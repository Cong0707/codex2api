package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/auth"
	"github.com/codex2api/config"
	"github.com/codex2api/database"
	"github.com/codex2api/security"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Handler API 路由处理器
type Handler struct {
	store      *auth.Store
	configKeys map[string]bool // 配置文件中的静态 key
	db         *database.DB
	cfg        *config.Config       // 全局配置
	deviceCfg  *DeviceProfileConfig // 设备指纹配置
	// plus 端口是否在当前进程启动时实际开启（端口监听变更需重启）
	plusPortListening bool

	// 动态 key 缓存
	dbKeysMu    sync.RWMutex
	dbKeys      map[string]bool
	dbKeysUntil time.Time
}

type usageLimitDetails struct {
	message         string
	planType        string
	resetsAt        int64
	resetsInSeconds int64
}

// NewHandler 创建处理器
func NewHandler(store *auth.Store, db *database.DB, cfg *config.Config, deviceCfg *DeviceProfileConfig) *Handler {
	plusPortListening := false
	if store != nil {
		plusPortListening = store.GetPlusPortEnabled()
	}
	return &Handler{
		store:             store,
		configKeys:        make(map[string]bool), // 不再使用硬编码，但保留结构以向后兼容逻辑
		db:                db,
		cfg:               cfg,
		deviceCfg:         deviceCfg,
		plusPortListening: plusPortListening,
	}
}

// NewHandlerWithDeviceProfile 创建处理器（带设备指纹配置）
func NewHandlerWithDeviceProfile(store *auth.Store, db *database.DB, deviceCfg *DeviceProfileConfig) *Handler {
	return NewHandler(store, db, nil, deviceCfg)
}

// refreshDBKeys 从数据库刷新密钥缓存（5 分钟）
func (h *Handler) refreshDBKeys() map[string]bool {
	h.dbKeysMu.RLock()
	if time.Now().Before(h.dbKeysUntil) {
		keys := h.dbKeys
		h.dbKeysMu.RUnlock()
		return keys
	}
	h.dbKeysMu.RUnlock()

	h.dbKeysMu.Lock()
	defer h.dbKeysMu.Unlock()

	// double check
	if time.Now().Before(h.dbKeysUntil) {
		return h.dbKeys
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	vals, err := h.db.GetAllAPIKeyValues(ctx)
	if err != nil {
		log.Printf("刷新 API Keys 缓存失败: %v", err)
		return h.dbKeys
	}

	newMap := make(map[string]bool, len(vals))
	for _, v := range vals {
		newMap[v] = true
	}
	h.dbKeys = newMap
	h.dbKeysUntil = time.Now().Add(5 * time.Minute)
	return newMap
}

// isValidKey 检查 key 是否有效（配置文件 + DB）
func (h *Handler) isValidKey(key string) bool {
	if h.configKeys[key] {
		return true
	}
	dbKeys := h.refreshDBKeys()
	return dbKeys[key]
}

// hasAnyKeys 检查是否配置了任何密钥
func (h *Handler) hasAnyKeys() bool {
	if len(h.configKeys) > 0 {
		return true
	}
	dbKeys := h.refreshDBKeys()
	return len(dbKeys) > 0
}

// logUsage 记录请求日志（非阻塞，写入内存缓冲由后台批量 flush）
func (h *Handler) logUsage(input *database.UsageLogInput) {
	if h.db == nil || input == nil {
		return
	}
	_ = h.db.InsertUsageLog(context.Background(), input)
}

func (h *Handler) settlePublicAccountUsage(account *auth.Account, usageBefore float64, usageBeforeValid bool, usageAfter float64) {
	if h == nil || h.db == nil || account == nil {
		return
	}
	account.Mu().RLock()
	publicKeyID := account.PublicAPIKeyID
	account.Mu().RUnlock()
	if publicKeyID == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := h.db.SettlePublicAccountUsageWithRequestDelta(ctx, account.ID(), usageBefore, usageBeforeValid, usageAfter); err != nil {
		log.Printf("[账号 %d] 公开上传结算失败: %v", account.ID(), err)
	}
}

func (h *Handler) persistUsageAndSettleFromResponse(account *auth.Account, resp *http.Response) {
	if account == nil || resp == nil {
		return
	}
	usageBefore, usageBeforeValid := account.GetUsagePercent7d()
	if usageAfter, ok := parseCodexUsageHeaders(resp, account); ok {
		h.store.PersistUsageSnapshot(account, usageAfter)
		h.settlePublicAccountUsage(account, usageBefore, usageBeforeValid, usageAfter)
	}
}

// extractReasoningEffort 从请求体提取推理强度
// 支持 reasoning.effort（Responses API）和 reasoning_effort（Chat Completions API）
func extractReasoningEffort(body []byte) string {
	// Responses API: reasoning.effort
	if effort := gjson.GetBytes(body, "reasoning.effort").String(); effort != "" {
		return effort
	}
	// Chat Completions API: reasoning_effort
	if effort := gjson.GetBytes(body, "reasoning_effort").String(); effort != "" {
		return effort
	}
	return ""
}

// extractServiceTier 从请求体提取服务等级
func extractServiceTier(body []byte) string {
	if tier := gjson.GetBytes(body, "service_tier").String(); tier != "" {
		return tier
	}
	return gjson.GetBytes(body, "serviceTier").String()
}

func classifyTransportFailure(err error) string {
	if err == nil {
		return ""
	}

	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "timeout") || strings.Contains(msg, "deadline exceeded") {
		return "timeout"
	}
	return "transport"
}

func classifyHTTPFailure(statusCode int, body []byte) string {
	switch {
	case isUnauthorizedLikeStatus(statusCode, body):
		return "unauthorized"
	case statusCode == http.StatusTooManyRequests:
		return "" // 429 由 applyCooldown 单独处理
	case statusCode >= 500:
		return "server"
	case statusCode >= 400:
		return "client"
	default:
		return ""
	}
}

type streamOutcome struct {
	logStatusCode  int
	failureKind    string
	failureMessage string
	penalize       bool
}

func classifyStreamOutcome(ctxErr, readErr, writeErr error, gotTerminal bool) streamOutcome {
	if gotTerminal {
		return streamOutcome{logStatusCode: http.StatusOK}
	}

	if ctxErr != nil || writeErr != nil {
		msg := "下游客户端提前断开"
		switch {
		case errors.Is(ctxErr, context.DeadlineExceeded):
			msg = "下游请求上下文超时"
		case writeErr != nil:
			msg = fmt.Sprintf("写回下游失败: %v", writeErr)
		case ctxErr != nil:
			msg = fmt.Sprintf("下游请求提前取消: %v", ctxErr)
		}
		return streamOutcome{
			logStatusCode:  logStatusClientClosed,
			failureMessage: msg,
		}
	}

	if readErr != nil {
		kind := classifyTransportFailure(readErr)
		if kind == "" {
			kind = "transport"
		}
		return streamOutcome{
			logStatusCode:  logStatusUpstreamStreamBreak,
			failureKind:    kind,
			failureMessage: fmt.Sprintf("上游流读取失败: %v", readErr),
			penalize:       true,
		}
	}

	return streamOutcome{
		logStatusCode:  logStatusUpstreamStreamBreak,
		failureKind:    "transport",
		failureMessage: "上游流提前结束，未收到终止事件",
		penalize:       true,
	}
}

func shouldTransparentRetryStream(outcome streamOutcome, attempt int, maxRetries int, wroteAnyBody bool, ctxErr, writeErr error) bool {
	if attempt >= maxRetries {
		return false
	}
	if !outcome.penalize {
		return false
	}
	if wroteAnyBody || ctxErr != nil || writeErr != nil {
		return false
	}
	return true
}

func isTerminalUpstreamEvent(eventType string) bool {
	return eventType == "response.completed" || eventType == "response.failed"
}

// isUserVisibleDeltaEvent 判断事件是否已经产生对下游可见的正文增量。
// 仅在首个可见增量前断流时才允许透明重试，避免重复输出。
func isUserVisibleDeltaEvent(eventType string, data []byte) bool {
	if !strings.HasSuffix(eventType, ".delta") {
		return false
	}
	delta := gjson.GetBytes(data, "delta")
	if !delta.Exists() {
		// 没有 delta 字段时，保守视为可见，避免无限缓冲。
		return true
	}
	if delta.Type == gjson.String {
		return strings.TrimSpace(delta.String()) != ""
	}
	return strings.TrimSpace(delta.Raw) != "" && delta.Raw != "null"
}

func schedulerLatency(totalDurationMs, firstTokenMs int) time.Duration {
	if firstTokenMs > 0 {
		return time.Duration(firstTokenMs) * time.Millisecond
	}
	if totalDurationMs > 0 {
		return time.Duration(totalDurationMs) * time.Millisecond
	}
	return 0
}

func isWebsocketUpgrade(headers http.Header) bool {
	if headers == nil {
		return false
	}
	connection := strings.ToLower(strings.TrimSpace(headers.Get("Connection")))
	upgrade := strings.ToLower(strings.TrimSpace(headers.Get("Upgrade")))
	return strings.Contains(connection, "upgrade") && upgrade == "websocket"
}

func shouldUseWebsocketTransport(cfg *config.Config, req *http.Request) bool {
	if cfg == nil || !cfg.UseWebsocket || req == nil {
		return false
	}
	// 对齐 CPA：仅在下游就是 websocket 传输时才走 websocket 上游链路。
	return isWebsocketUpgrade(req.Header)
}

func parsePortString(raw string) int {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0
	}
	p, err := strconv.Atoi(raw)
	if err != nil || p <= 0 {
		return 0
	}
	return p
}

func parsePortFromAddr(addr string) int {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return 0
	}
	// 优先 split host:port（支持 IPv6）
	if _, port, err := net.SplitHostPort(addr); err == nil {
		return parsePortString(port)
	}
	// 兜底：按最后一个冒号取端口
	if idx := strings.LastIndex(addr, ":"); idx >= 0 && idx+1 < len(addr) {
		return parsePortString(addr[idx+1:])
	}
	return 0
}

func resolveRequestPort(req *http.Request) int {
	if req == nil {
		return 0
	}
	if local := req.Context().Value(http.LocalAddrContextKey); local != nil {
		if addr, ok := local.(net.Addr); ok && addr != nil {
			if p := parsePortFromAddr(addr.String()); p > 0 {
				return p
			}
		}
	}
	if p := parsePortFromAddr(req.Host); p > 0 {
		return p
	}
	if p := parsePortFromAddr(req.URL.Host); p > 0 {
		return p
	}
	return 0
}

func requestStartTime(c *gin.Context) time.Time {
	if c != nil {
		if v, ok := c.Get("x-request-start-time"); ok {
			if t, ok := v.(time.Time); ok && !t.IsZero() {
				return t
			}
		}
	}
	return time.Now()
}

func (h *Handler) shouldApplyPlusPortPolicy() bool {
	if h == nil || h.store == nil || h.cfg == nil {
		return false
	}
	return h.plusPortListening
}

func (h *Handler) allowAccountForCurrentPort(c *gin.Context, acc *auth.Account) bool {
	if acc == nil {
		return false
	}
	if matcher := h.accountMatcherForCurrentPort(c); matcher != nil {
		return matcher(acc)
	}
	if !h.shouldApplyPlusPortPolicy() {
		return true
	}
	reqPort := resolveRequestPort(c.Request)
	if reqPort <= 0 {
		// 无法识别端口时不强行拦截，避免反代链路误杀。
		return true
	}
	mainPort := h.cfg.Port
	plusPort := h.cfg.Port + 1
	planType := auth.NormalizePlanType(acc.GetPlanType())
	isFree := planType == "free"

	switch reqPort {
	case mainPort:
		// plus 端口开启后，主端口仅允许 free。
		return isFree
	case plusPort:
		// plus 端口允许全部套餐；是否包含 free 受配置控制。
		if isFree && !h.store.GetPlusPortAccessFree() {
			return false
		}
		return true
	default:
		// 其他端口保持历史行为，避免影响非预期端口场景。
		return true
	}
}

func (h *Handler) accountMatcherForCurrentPort(c *gin.Context) auth.AccountMatcher {
	if h == nil || h.store == nil || h.cfg == nil || c == nil || c.Request == nil {
		return nil
	}
	if !h.shouldApplyPlusPortPolicy() {
		return nil
	}

	reqPort := resolveRequestPort(c.Request)
	if reqPort <= 0 {
		return nil
	}

	mainPort := h.cfg.Port
	plusPort := h.cfg.Port + 1
	switch reqPort {
	case mainPort:
		return func(acc *auth.Account) bool {
			return acc != nil && auth.NormalizePlanType(acc.GetPlanType()) == "free"
		}
	case plusPort:
		if h.store.GetPlusPortAccessFree() {
			return nil
		}
		return func(acc *auth.Account) bool {
			return acc != nil && auth.NormalizePlanType(acc.GetPlanType()) != "free"
		}
	default:
		return nil
	}
}

func sessionAffinityKey(sessionID string, apiKey string) string {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return ""
	}
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return sessionID
	}
	scope := uuid.NewSHA1(uuid.NameSpaceOID, []byte("codex2api:affinity:"+apiKey)).String()
	return sessionID + "|" + scope
}

func (h *Handler) acquireAccountForRequest(c *gin.Context, exclude map[int64]bool) *auth.Account {
	acc, _ := h.acquireAccountForRequestWithAffinity(c, "", exclude)
	return acc
}

func (h *Handler) acquireAccountForRequestWithAffinity(c *gin.Context, affinityKey string, exclude map[int64]bool) (*auth.Account, string) {
	if h == nil || h.store == nil {
		return nil, ""
	}
	if exclude == nil {
		exclude = make(map[int64]bool)
	}
	matcher := h.accountMatcherForCurrentPort(c)
	startedAt := requestStartTime(c)
	waitRounds := 0

	tryPick := func() (*auth.Account, string) {
		return h.store.NextForSession(affinityKey, exclude, matcher)
	}

	logSlowAcquire := func(acc *auth.Account) {
		elapsed := time.Since(startedAt)
		if c != nil {
			c.Set("x-scheduler-acquire-ms", elapsed.Milliseconds())
			c.Set("x-scheduler-wait-rounds", waitRounds)
		}
		if elapsed < 200*time.Millisecond {
			return
		}
		planType := ""
		accountID := int64(0)
		if acc != nil {
			accountID = acc.ID()
			planType = acc.GetPlanType()
		}
		log.Printf("调度耗时较高: elapsed=%s wait_rounds=%d request_port=%d exclude=%d picked_account=%d picked_plan=%s",
			elapsed.Round(time.Millisecond),
			waitRounds,
			resolveRequestPort(c.Request),
			len(exclude),
			accountID,
			planType,
		)
	}

	if acc, proxyURL := tryPick(); acc != nil {
		logSlowAcquire(acc)
		return acc, proxyURL
	}

	deadline := time.Now().Add(30 * time.Second)
	for {
		if c.Request.Context().Err() != nil || time.Now().After(deadline) {
			return nil, ""
		}
		waitFor := time.Until(deadline)
		if waitFor > 3*time.Second {
			waitFor = 3 * time.Second
		}
		if waitFor <= 0 {
			return nil, ""
		}
		waitRounds++
		acc, proxyURL := h.store.WaitForSessionAvailable(c.Request.Context(), affinityKey, waitFor, exclude, matcher)
		if acc == nil {
			if c != nil {
				c.Set("x-scheduler-acquire-ms", time.Since(startedAt).Milliseconds())
				c.Set("x-scheduler-wait-rounds", waitRounds)
			}
			continue
		}
		logSlowAcquire(acc)
		return acc, proxyURL
	}
}

// RegisterRoutes 注册路由
func (h *Handler) RegisterRoutes(r *gin.Engine) {
	registerOpenAIRoutes := func(group *gin.RouterGroup) {
		group.POST("/chat/completions", h.ChatCompletions)
		group.POST("/responses", h.Responses)
		// 兼容部分客户端将 compact 请求打到 /responses/compact。
		group.POST("/responses/compact", h.Responses)
		group.POST("/messages", h.Messages)
		group.GET("/models", h.ListModels)
	}

	authMW := h.authMiddleware()

	v1 := r.Group("/v1")
	v1.Use(authMW)
	registerOpenAIRoutes(v1)

	// 兼容无版本前缀路径（如 base_url 不含 /v1 的客户端配置）。
	compat := r.Group("")
	compat.Use(authMW)
	registerOpenAIRoutes(compat)

	codexDirect := r.Group("/backend-api/codex")
	codexDirect.Use(authMW)
	codexDirect.POST("/responses", h.Responses)
	codexDirect.POST("/responses/*subpath", func(c *gin.Context) {
		subpath := strings.TrimSpace(c.Param("subpath"))
		if subpath == "/compact" || strings.HasPrefix(subpath, "/compact/") {
			h.Responses(c)
			return
		}
		h.Responses(c)
	})
}

// authMiddleware API Key 鉴权中间件（增强版，带安全日志）
func (h *Handler) authMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		// 如果没有配置任何密钥，跳过鉴权
		if !h.hasAnyKeys() {
			c.Next()
			return
		}

		key := requestAPIKeyFromHeaders(c.Request.Header)
		if key == "" {
			// Use standardized error format from api package
			api.SendError(c, api.ErrMissingAPIKey)
			c.Abort()
			return
		}
		key = security.SanitizeInput(key)
		if !h.isValidKey(key) {
			// 记录安全审计日志（脱敏）
			maskedKey := security.MaskAPIKey(key)
			security.SecurityAuditLog("AUTH_FAILED", fmt.Sprintf("path=%s ip=%s key=%s", c.Request.URL.Path, c.ClientIP(), maskedKey))
			// Use standardized error format from api package
			api.SendError(c, api.ErrInvalidAPIKey)
			c.Abort()
			return
		}
		c.Set("apiKey", key)
		c.Next()
	}
}

// ==================== /v1/responses ====================

// getMaxRetries 从 store 读取可配置的最大重试次数
func (h *Handler) getMaxRetries() int {
	return h.store.GetMaxRetries()
}

const (
	logStatusClientClosed        = 499
	logStatusUpstreamStreamBreak = 598
)

func isDeactivatedWorkspaceError(code int, body []byte) bool {
	if code != http.StatusPaymentRequired || len(body) == 0 {
		return false
	}
	errCode, _ := parseUpstreamErrorBrief(body)
	return strings.EqualFold(strings.TrimSpace(errCode), "deactivated_workspace")
}

func isUnauthorizedLikeStatus(code int, body []byte) bool {
	return code == http.StatusUnauthorized || isDeactivatedWorkspaceError(code, body)
}

// IsUnauthorizedLikeStatus 返回该上游状态是否应按封禁/401 处理。
func IsUnauthorizedLikeStatus(code int, body []byte) bool {
	return isUnauthorizedLikeStatus(code, body)
}

func normalizeUpstreamStatusCode(code int, body []byte) int {
	if isUnauthorizedLikeStatus(code, body) {
		return http.StatusUnauthorized
	}
	return code
}

// NormalizeUpstreamStatusCode 将需要按 401 对待的上游错误归一化为 401。
func NormalizeUpstreamStatusCode(code int, body []byte) int {
	return normalizeUpstreamStatusCode(code, body)
}

// isRetryableStatus 检查是否可重试的上游状态码
func isRetryableStatus(code int, body []byte) bool {
	if isUnauthorizedLikeStatus(code, body) || code == http.StatusTooManyRequests {
		return true
	}
	return code >= 500 && code <= 599
}

func isPermanentUnauthorizedError(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	code := strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, "error.code").String()))
	msg := strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, "error.message").String()))
	if code == "no_organization" || code == "organization_required" {
		return true
	}
	return strings.Contains(msg, "member of an organization")
}

func (h *Handler) forceDeleteAccount(accountID int64, source string) {
	if accountID == 0 {
		return
	}
	if h.db != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_ = h.db.SetError(ctx, accountID, "deleted")
		cancel()
		h.db.InsertAccountEventAsync(accountID, "deleted", source)
	}
	if h.store != nil {
		h.store.RemoveAccount(accountID)
	}
}

func parseUsageLimitDetails(body []byte) (usageLimitDetails, bool) {
	if len(body) == 0 {
		return usageLimitDetails{}, false
	}
	if gjson.GetBytes(body, "error.type").String() != "usage_limit_reached" {
		return usageLimitDetails{}, false
	}
	return usageLimitDetails{
		message:         gjson.GetBytes(body, "error.message").String(),
		planType:        gjson.GetBytes(body, "error.plan_type").String(),
		resetsAt:        gjson.GetBytes(body, "error.resets_at").Int(),
		resetsInSeconds: gjson.GetBytes(body, "error.resets_in_seconds").Int(),
	}, true
}

func parseUpstreamErrorBrief(body []byte) (code string, message string) {
	if len(body) == 0 {
		return "", ""
	}
	code = strings.TrimSpace(gjson.GetBytes(body, "error.code").String())
	if code == "" {
		code = strings.TrimSpace(gjson.GetBytes(body, "detail.code").String())
	}
	if code == "" {
		code = strings.TrimSpace(gjson.GetBytes(body, "code").String())
	}

	message = strings.TrimSpace(gjson.GetBytes(body, "error.message").String())
	if message == "" {
		message = strings.TrimSpace(gjson.GetBytes(body, "detail.message").String())
	}
	if message == "" {
		message = strings.TrimSpace(gjson.GetBytes(body, "message").String())
	}
	return code, message
}

// ParseUpstreamErrorBrief 提取统一的上游错误 code/message，兼容 error/detail 两种结构。
func ParseUpstreamErrorBrief(body []byte) (code string, message string) {
	return parseUpstreamErrorBrief(body)
}

func (h *Handler) applyUnauthorizedCooldown(account *auth.Account, upstreamStatusCode int, body []byte) {
	errCode, errMsg := parseUpstreamErrorBrief(body)
	if errCode == "" && upstreamStatusCode == http.StatusPaymentRequired {
		errCode = "deactivated_workspace"
	}
	if errMsg == "" {
		errMsg = http.StatusText(http.StatusUnauthorized)
	}
	account.SetLastFailureDetail(http.StatusUnauthorized, errCode, errMsg)
	// 原子标志瞬间置位，阻止其他并发请求再选到该账号
	atomic.StoreInt32(&account.Disabled, 1)

	if h.store.GetAutoCleanUnauthorized() {
		log.Printf("账号 %d 收到封禁类错误(上游 %d/%s)，立即清理", account.ID(), upstreamStatusCode, errCode)
		if h.db != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			_ = h.db.SetError(ctx, account.ID(), "deleted")
			cancel()
			h.db.InsertAccountEventAsync(account.ID(), "deleted", "auto_clean_unauthorized")
		}
		h.store.RemoveAccount(account.ID())
		return
	}

	log.Printf("账号 %d 收到封禁类错误(上游 %d/%s)，按 401 封禁处理", account.ID(), upstreamStatusCode, errCode)
	h.store.MarkCooldown(account, 5*time.Minute, "unauthorized")
}

// Responses 处理 /v1/responses 请求（原生透传，增强输入验证）
func (h *Handler) Responses(c *gin.Context) {
	// 1. 读取请求体
	rawBody, err := io.ReadAll(c.Request.Body)
	if err != nil {
		api.SendError(c, api.NewAPIError(api.ErrCodeInvalidRequest, "Failed to read request body", api.ErrorTypeInvalidRequest))
		return
	}

	// Validate request
	validator := api.NewValidator(rawBody)
	rules := api.ResponsesAPIValidationRules()
	rules["model"] = append(rules["model"], api.ModelValidator(SupportedModels))
	result := validator.ValidateRequest(rules)
	if !result.Valid {
		api.SendError(c, validator.ToAPIError())
		return
	}

	// 检查请求体大小
	if len(rawBody) > security.MaxRequestBodySize {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{
			"error": gin.H{"message": "请求体过大", "type": "invalid_request_error"},
		})
		return
	}

	model := gjson.GetBytes(rawBody, "model").String()
	requestStartedAt := requestStartTime(c)

	// 验证 model 参数
	if err := security.ValidateModelName(model); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": gin.H{"message": "model 参数无效", "type": "invalid_request_error"},
		})
		return
	}

	if model == "" {
		api.SendMissingFieldError(c, "model")
		return
	}

	rawBody = normalizeServiceTierField(rawBody)
	isStream := gjson.GetBytes(rawBody, "stream").Bool()
	downstreamHeaders := c.Request.Header.Clone()
	apiKey := requestAPIKeyFromHeaders(downstreamHeaders)
	sessionID := ResolveSessionID(downstreamHeaders, rawBody)
	affinityKey := sessionAffinityKey(sessionID, apiKey)
	reasoningEffort := extractReasoningEffort(rawBody)
	serviceTier := extractServiceTier(rawBody)
	if serviceTier != "" {
		c.Set("x-service-tier", serviceTier)
	}
	logRequestLifecycleStart(c, "/v1/responses", model, isStream, reasoningEffort)

	// 2. 注入/修正 Codex 必需字段
	codexBody := rawBody
	codexBody, _ = sjson.SetBytes(codexBody, "stream", true)
	codexBody, _ = sjson.SetBytes(codexBody, "store", false)
	if !gjson.GetBytes(codexBody, "include").Exists() {
		codexBody, _ = sjson.SetBytes(codexBody, "include", []string{"reasoning.encrypted_content"})
	}

	// 自动将字符串 input 包装为数组格式（Codex 要求 input 为 list）
	inputResult := gjson.GetBytes(codexBody, "input")
	if inputResult.Exists() && inputResult.Type == gjson.String {
		codexBody, _ = sjson.SetBytes(codexBody, "input", []map[string]string{
			{"role": "user", "content": inputResult.String()},
		})
	}

	// 将 Chat Completions 风格的 reasoning_effort 自动转换为 Responses API 的 reasoning.effort
	if re := gjson.GetBytes(codexBody, "reasoning_effort"); re.Exists() && !gjson.GetBytes(codexBody, "reasoning.effort").Exists() {
		codexBody, _ = sjson.SetBytes(codexBody, "reasoning.effort", re.String())
	}
	codexBody = clampReasoningEffort(codexBody)
	codexBody = sanitizeServiceTierForUpstream(codexBody)

	// 为缺少 description 的客户端执行工具补充默认描述（如 tool_search）
	codexBody = ensureToolDescriptions(codexBody)
	// 清理 function tool parameters 中上游不支持的 JSON Schema 关键字
	codexBody = sanitizeToolSchemas(codexBody)

	// 展开 previous_response_id（将缓存的历史对话上下文注入 input）
	codexBody, _ = expandPreviousResponse(codexBody)
	// 保存展开后的 input，用于在 response.completed 时缓存完整上下文
	expandedInputRaw := gjson.GetBytes(codexBody, "input").Raw

	// 删除 Codex 不支持的参数
	unsupportedFields := []string{
		"max_output_tokens", "max_tokens", "max_completion_tokens",
		"temperature", "top_p", "frequency_penalty", "presence_penalty",
		"logprobs", "top_logprobs", "n", "seed", "stop", "user",
		"logit_bias", "response_format", "serviceTier",
		"stream_options", "reasoning_effort", "truncation", "context_management",
		"disable_response_storage", "verbosity",
	}
	for _, field := range unsupportedFields {
		codexBody, _ = sjson.DeleteBytes(codexBody, field)
	}

	// 3. 带重试的上游请求
	maxRetries := h.getMaxRetries()
	var lastErr error
	var lastStatusCode int
	var lastBody []byte
	var failedAttempts []string
	var upstreamStageMs int64
	excludeAccounts := make(map[int64]bool) // 重试时排除已失败的账号
	deviceCfg := h.deviceCfg
	if deviceCfg == nil {
		deviceCfg = DefaultDeviceProfileConfig()
	}

	for attempt := 0; attempt <= maxRetries; attempt++ {
		acquireStartedAt := time.Now()
		account, stickyProxyURL := h.acquireAccountForRequestWithAffinity(c, affinityKey, excludeAccounts)
		attemptAcquireMs := int(time.Since(acquireStartedAt).Milliseconds())
		if account == nil {
			if lastStatusCode != 0 && len(lastBody) > 0 {
				h.sendFinalUpstreamError(c, lastStatusCode, lastBody)
				return
			}
			c.JSON(http.StatusServiceUnavailable, gin.H{
				"error": gin.H{"message": "无可用账号，请稍后重试", "type": "server_error"},
			})
			return
		}

		start := time.Now()
		proxyURL := strings.TrimSpace(stickyProxyURL)
		if proxyURL == "" {
			proxyURL = h.store.ResolveProxyForAccount(account)
		}
		useWebsocket := shouldUseWebsocketTransport(h.cfg, c.Request)
		logRequestDispatch(c, "/v1/responses", attempt+1, account, proxyURL, model, reasoningEffort, attemptAcquireMs)

		resp, attemptTrace, reqErr := ExecuteRequestTraced(c.Request.Context(), account, codexBody, sessionID, proxyURL, apiKey, deviceCfg, downstreamHeaders, useWebsocket)
		durationMs := int(time.Since(start).Milliseconds())
		upstreamStageMs += int64(durationMs)
		c.Set("x-upstream-stage-ms", upstreamStageMs)
		statusCode := 0
		if resp != nil {
			statusCode = resp.StatusCode
		}
		if attemptTrace != nil && !attemptTrace.HeaderAt.IsZero() {
			logUpstreamAttemptHeaders(c, "/v1/responses", attempt+1, account, statusCode, attemptTrace, requestStartedAt)
		}

		if reqErr != nil {
			if kind := classifyTransportFailure(reqErr); kind != "" {
				failedAttempts = append(failedAttempts, kind)
				h.store.ReportRequestFailure(account, kind, time.Duration(durationMs)*time.Millisecond)
			}
			logUpstreamAttemptResult(c, "/v1/responses", attempt+1, account, proxyURL, 0, durationMs, requestStartedAt, reqErr.Error())
			h.store.Release(account)
			h.store.UnbindSessionAffinity(affinityKey, account.ID())
			excludeAccounts[account.ID()] = true

			// 不可重试的结构化错误直接返回
			if !IsRetryableError(reqErr) && classifyTransportFailure(reqErr) == "" {
				ErrorToGinResponse(c, reqErr)
				return
			}

			log.Printf("上游请求失败 (attempt %d): %v", attempt+1, reqErr)
			lastErr = reqErr
			continue
		}

		if resp.StatusCode != http.StatusOK {
			h.persistUsageAndSettleFromResponse(account, resp)
			errBody, _ := io.ReadAll(resp.Body)
			if kind := classifyHTTPFailure(resp.StatusCode, errBody); kind != "" {
				h.store.ReportRequestFailure(account, kind, time.Duration(durationMs)*time.Millisecond)
			}
			resp.Body.Close()
			logUpstreamAttemptResult(c, "/v1/responses", attempt+1, account, proxyURL, resp.StatusCode, durationMs, requestStartedAt, fmt.Sprintf("http_%d", resp.StatusCode))
			h.store.Release(account)
			h.store.UnbindSessionAffinity(affinityKey, account.ID())
			excludeAccounts[account.ID()] = true

			log.Printf("上游返回错误 (attempt %d, status %d): %s", attempt+1, resp.StatusCode, string(errBody))
			failedAttempts = append(failedAttempts, strconv.Itoa(resp.StatusCode))
			logUpstreamError("/v1/responses", resp.StatusCode, model, account.ID(), errBody)
			h.logUsage(&database.UsageLogInput{
				AccountID:        account.ID(),
				Endpoint:         "/v1/responses",
				Model:            model,
				StatusCode:       resp.StatusCode,
				DurationMs:       durationMs,
				ReasoningEffort:  reasoningEffort,
				InboundEndpoint:  "/v1/responses",
				UpstreamEndpoint: "/v1/responses",
				Stream:           isStream,
				ServiceTier:      serviceTier,
			})
			h.applyCooldown(account, resp.StatusCode, errBody, resp)
			lastStatusCode = resp.StatusCode
			lastBody = errBody
			if resp.StatusCode == http.StatusUnauthorized && isPermanentUnauthorizedError(errBody) {
				log.Printf("账号 %d 返回永久 401(no_organization)，已从账号池剔除", account.ID())
				h.forceDeleteAccount(account.ID(), "auto_clean_no_organization")
			}

			if isRetryableStatus(resp.StatusCode, errBody) && attempt < maxRetries {
				continue
			}

			h.sendFinalUpstreamError(c, resp.StatusCode, errBody)
			return
		}

		// 成功！透传响应并跟踪 TTFT / usage
		if attempt > 0 {
			c.Set("x-upstream-attempts", attempt+1)
			c.Set("x-upstream-failed-attempts", strings.Join(failedAttempts, ","))
			log.Printf("请求重试成功: endpoint=/v1/responses attempts=%d failed=%s account=%d", attempt+1, strings.Join(failedAttempts, ","), account.ID())
		} else {
			c.Set("x-upstream-attempts", 1)
		}
		account.Mu().RLock()
		c.Set("x-account-email", account.Email)
		account.Mu().RUnlock()
		c.Set("x-account-proxy", proxyURL)
		c.Set("x-model", model)
		c.Set("x-reasoning-effort", reasoningEffort)
		var firstTokenMs int
		var attemptFirstTokenMs int
		firstFrameRecorded := false
		var usage *UsageInfo
		var actualServiceTier string
		ttftRecorded := false
		gotTerminal := false // 是否收到 response.completed 或 response.failed
		deltaCharCount := 0  // 累计 delta 字符数（用于断流时估算 token）
		var readErr error
		var writeErr error
		wroteAnyBytes := false
		wroteVisibleBody := false
		pendingFrames := make([]string, 0, 8)
		var responseJSON []byte

		if isStream {
			// 流式透传 + TTFT 跟踪
			c.Header("Content-Type", "text/event-stream")
			c.Header("Cache-Control", "no-cache")
			c.Header("Connection", "keep-alive")
			c.Header("X-Accel-Buffering", "no")

			flusher, ok := c.Writer.(http.Flusher)
			if !ok {
				c.JSON(http.StatusInternalServerError, gin.H{
					"error": gin.H{"message": "streaming not supported", "type": "server_error"},
				})
				resp.Body.Close()
				h.store.Release(account)
				return
			}
			// 先发一个 SSE 注释帧，避免前置代理/网关等待首包超时（如 524）。
			if _, err := fmt.Fprint(c.Writer, ": keep-alive\n\n"); err != nil {
				c.JSON(http.StatusBadGateway, gin.H{
					"error": gin.H{"message": "下游连接写入失败", "type": "upstream_error"},
				})
				resp.Body.Close()
				h.store.Release(account)
				return
			}
			wroteAnyBytes = true
			flusher.Flush()

			readErr = ReadSSEStream(resp.Body, func(data []byte) bool {
				eventType := gjson.GetBytes(data, "type").String()
				visibleEvent := isUserVisibleDeltaEvent(eventType, data)
				if !firstFrameRecorded {
					firstFrameRecorded = true
					logUpstreamFirstFrame(c, "/v1/responses", attempt+1, eventType, requestStartedAt, start)
				}

				// TTFT: 记录第一个 output_text.delta 事件的时间
				if !ttftRecorded && eventType == "response.output_text.delta" {
					attemptFirstTokenMs = int(time.Since(start).Milliseconds())
					firstTokenMs = int(time.Since(requestStartedAt).Milliseconds())
					ttftRecorded = true
					logUpstreamFirstVisible(c, "/v1/responses", attempt+1, eventType, requestStartedAt, start)
					h.store.ReportFirstTokenLatency(account, time.Duration(attemptFirstTokenMs)*time.Millisecond)
				}

				// 累计 delta 字符数
				if eventType == "response.output_text.delta" {
					deltaCharCount += len(gjson.GetBytes(data, "delta").String())
				}

				// 提取 usage + service_tier
				if eventType == "response.completed" {
					usage = extractUsage(data)
					if tier := gjson.GetBytes(data, "response.service_tier").String(); tier != "" {
						actualServiceTier = tier
					}
					// 缓存响应上下文，供后续 previous_response_id 展开使用
					cacheCompletedResponse([]byte(expandedInputRaw), data)
					gotTerminal = true
				}
				if eventType == "response.failed" {
					gotTerminal = true
				}

				frame := fmt.Sprintf("data: %s\n\n", data)
				if !wroteVisibleBody {
					// 首个可见正文前先缓冲，若上游在此阶段断流可透明重试。
					if !visibleEvent && !isTerminalUpstreamEvent(eventType) {
						pendingFrames = append(pendingFrames, frame)
						return true
					}
					for _, pending := range pendingFrames {
						if _, err := fmt.Fprint(c.Writer, pending); err != nil {
							writeErr = err
							return false
						}
					}
					pendingFrames = pendingFrames[:0]
				}

				if _, err := fmt.Fprint(c.Writer, frame); err != nil {
					writeErr = err
					return false
				}
				wroteAnyBytes = true
				if visibleEvent {
					wroteVisibleBody = true
				}
				flusher.Flush()
				return !isTerminalUpstreamEvent(eventType)
			})
		} else {
			// 非流式收集
			var lastResponseData []byte
			var collectedOutputText strings.Builder
			readErr = ReadSSEStream(resp.Body, func(data []byte) bool {
				eventType := gjson.GetBytes(data, "type").String()
				if !firstFrameRecorded {
					firstFrameRecorded = true
					logUpstreamFirstFrame(c, "/v1/responses", attempt+1, eventType, requestStartedAt, start)
				}
				if !ttftRecorded && eventType == "response.output_text.delta" {
					attemptFirstTokenMs = int(time.Since(start).Milliseconds())
					firstTokenMs = int(time.Since(requestStartedAt).Milliseconds())
					ttftRecorded = true
					logUpstreamFirstVisible(c, "/v1/responses", attempt+1, eventType, requestStartedAt, start)
					h.store.ReportFirstTokenLatency(account, time.Duration(attemptFirstTokenMs)*time.Millisecond)
				}
				// 累计 delta 字符数
				if eventType == "response.output_text.delta" {
					delta := gjson.GetBytes(data, "delta").String()
					collectedOutputText.WriteString(delta)
					deltaCharCount += len(delta)
				}
				if eventType == "response.completed" {
					usage = extractUsage(data)
					if tier := gjson.GetBytes(data, "response.service_tier").String(); tier != "" {
						actualServiceTier = tier
					}
					// 缓存响应上下文，供后续 previous_response_id 展开使用
					cacheCompletedResponse([]byte(expandedInputRaw), data)
					gotTerminal = true
					lastResponseData = data
					return false
				}
				if eventType == "response.failed" {
					gotTerminal = true
					lastResponseData = data
					return false
				}
				return true
			})

			if lastResponseData != nil {
				responseObj := gjson.GetBytes(lastResponseData, "response")
				if responseObj.Exists() {
					responseJSON = []byte(responseObj.Raw)
					responseJSON = ensureResponseOutputFromDelta(responseJSON, collectedOutputText.String())
				}
			}
		}

		// 断流检测 + token 估算
		attemptDuration := int(time.Since(start).Milliseconds())
		totalDuration := int(time.Since(requestStartedAt).Milliseconds())
		c.Set("x-first-token-ms", firstTokenMs)
		outcome := classifyStreamOutcome(c.Request.Context().Err(), readErr, writeErr, gotTerminal)
		if shouldTransparentRetryStream(outcome, attempt, maxRetries, wroteVisibleBody, c.Request.Context().Err(), writeErr) {
			log.Printf("上游流在首包前断开，重置连接并重试 (attempt %d/%d, account %d, /v1/responses): %s", attempt+1, maxRetries+1, account.ID(), outcome.failureMessage)
			logUpstreamAttemptResult(c, "/v1/responses", attempt+1, account, proxyURL, outcome.logStatusCode, attemptDuration, requestStartedAt, "retry:"+outcome.failureMessage)
			recyclePooledClientForAccount(account)
			h.persistUsageAndSettleFromResponse(account, resp)
			h.store.ReportRequestFailure(account, outcome.failureKind, time.Duration(attemptDuration)*time.Millisecond)
			resp.Body.Close()
			h.store.Release(account)
			h.store.UnbindSessionAffinity(affinityKey, account.ID())
			excludeAccounts[account.ID()] = true
			lastErr = readErr
			if lastErr == nil {
				lastErr = errors.New(outcome.failureMessage)
			}
			continue
		}
		h.store.BindSessionAffinity(affinityKey, account, proxyURL)
		logStatusCode := outcome.logStatusCode
		if outcome.logStatusCode != http.StatusOK {
			log.Printf("流异常结束 (account %d, /v1/responses, status %d): %s，已转发约 %d 字符", account.ID(), outcome.logStatusCode, outcome.failureMessage, deltaCharCount)
			if deltaCharCount > 0 {
				estOutputTokens := deltaCharCount / 3 // 粗略估算: 约 3 字符 = 1 token
				if estOutputTokens < 1 {
					estOutputTokens = 1
				}
				usage = &UsageInfo{
					OutputTokens:     estOutputTokens,
					CompletionTokens: estOutputTokens,
					TotalTokens:      estOutputTokens,
				}
			}
		}
		if isStream && outcome.logStatusCode != http.StatusOK && !wroteAnyBytes {
			c.JSON(http.StatusBadGateway, gin.H{
				"error": gin.H{
					"message": "上游流在首包前中断: " + outcome.failureMessage,
					"type":    "upstream_error",
					"code":    "upstream_stream_break",
				},
			})
		}
		if !isStream {
			if responseJSON != nil {
				c.Data(http.StatusOK, "application/json", responseJSON)
			} else {
				c.JSON(http.StatusBadGateway, gin.H{
					"error": gin.H{"message": "未收到完整的上游响应", "type": "upstream_error"},
				})
			}
		}

		resolvedServiceTier := resolveServiceTier(actualServiceTier, serviceTier)
		c.Set("x-service-tier", resolvedServiceTier)

		logInput := &database.UsageLogInput{
			AccountID:        account.ID(),
			Endpoint:         "/v1/responses",
			Model:            model,
			StatusCode:       logStatusCode,
			DurationMs:       totalDuration,
			FirstTokenMs:     firstTokenMs,
			ReasoningEffort:  reasoningEffort,
			InboundEndpoint:  "/v1/responses",
			UpstreamEndpoint: "/v1/responses",
			Stream:           isStream,
			ServiceTier:      resolvedServiceTier,
		}
		if usage != nil {
			logInput.PromptTokens = usage.PromptTokens
			logInput.CompletionTokens = usage.CompletionTokens
			logInput.TotalTokens = usage.TotalTokens
			logInput.InputTokens = usage.InputTokens
			logInput.OutputTokens = usage.OutputTokens
			logInput.ReasoningTokens = usage.ReasoningTokens
			logInput.CachedTokens = usage.CachedTokens
		}
		h.logUsage(logInput)

		resp.Body.Close()
		h.persistUsageAndSettleFromResponse(account, resp)
		if outcome.penalize {
			recyclePooledClientForAccount(account)
			h.store.ReportRequestFailure(account, outcome.failureKind, time.Duration(attemptDuration)*time.Millisecond)
		} else if outcome.logStatusCode == http.StatusOK {
			h.store.ReportRequestSuccess(account, schedulerLatency(attemptDuration, attemptFirstTokenMs))
		}
		logUpstreamAttemptResult(c, "/v1/responses", attempt+1, account, proxyURL, outcome.logStatusCode, attemptDuration, requestStartedAt, "")
		h.store.Release(account)
		return
	}

	// 所有重试都失败
	if lastErr != nil {
		c.JSON(http.StatusBadGateway, gin.H{
			"error": gin.H{"message": "上游请求失败: " + lastErr.Error(), "type": "upstream_error"},
		})
	} else if lastStatusCode != 0 {
		h.sendFinalUpstreamError(c, lastStatusCode, lastBody)
	}
}

func (h *Handler) ChatCompletions(c *gin.Context) {
	// 1. 读取请求体
	rawBody, err := io.ReadAll(c.Request.Body)
	if err != nil {
		api.SendError(c, api.NewAPIError(api.ErrCodeInvalidRequest, "Failed to read request body", api.ErrorTypeInvalidRequest))
		return
	}

	// Validate request
	validator := api.NewValidator(rawBody)
	rules := api.ChatCompletionValidationRules()
	rules["model"] = append(rules["model"], api.ModelValidator(SupportedModels))
	result := validator.ValidateRequest(rules)
	if !result.Valid {
		api.SendError(c, validator.ToAPIError())
		return
	}

	// 检查请求体大小
	if len(rawBody) > security.MaxRequestBodySize {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{
			"error": gin.H{"message": "请求体过大", "type": "invalid_request_error"},
		})
		return
	}

	model := gjson.GetBytes(rawBody, "model").String()
	if model == "" {
		model = "gpt-5.4"
	}
	requestStartedAt := requestStartTime(c)

	// 验证 model 参数
	if err := security.ValidateModelName(model); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": gin.H{"message": "model 参数无效", "type": "invalid_request_error"},
		})
		return
	}

	isStream := gjson.GetBytes(rawBody, "stream").Bool()
	reasoningEffort := extractReasoningEffort(rawBody)
	serviceTier := extractServiceTier(rawBody)
	if serviceTier != "" {
		c.Set("x-service-tier", serviceTier)
	}
	logRequestLifecycleStart(c, "/v1/chat/completions", model, isStream, reasoningEffort)

	// 2. 翻译请求：OpenAI Chat → Codex Responses
	codexBody, err := TranslateRequest(rawBody)
	if err != nil {
		api.SendError(c, api.NewAPIError(api.ErrCodeInvalidRequest, "Request translation failed: "+err.Error(), api.ErrorTypeInvalidRequest))
		return
	}

	downstreamHeaders := c.Request.Header.Clone()
	apiKey := requestAPIKeyFromHeaders(downstreamHeaders)
	sessionID := ResolveSessionID(downstreamHeaders, codexBody)
	affinityKey := sessionAffinityKey(sessionID, apiKey)

	// 3. 带重试的上游请求
	maxRetries := h.getMaxRetries()
	var lastErr error
	var lastStatusCode int
	var lastBody []byte
	var failedAttempts []string
	var upstreamStageMs int64
	excludeAccounts := make(map[int64]bool) // 重试时排除已失败的账号
	deviceCfg := h.deviceCfg
	if deviceCfg == nil {
		deviceCfg = DefaultDeviceProfileConfig()
	}

	for attempt := 0; attempt <= maxRetries; attempt++ {
		acquireStartedAt := time.Now()
		account, stickyProxyURL := h.acquireAccountForRequestWithAffinity(c, affinityKey, excludeAccounts)
		attemptAcquireMs := int(time.Since(acquireStartedAt).Milliseconds())
		if account == nil {
			if lastStatusCode != 0 && len(lastBody) > 0 {
				h.sendFinalUpstreamError(c, lastStatusCode, lastBody)
				return
			}
			c.JSON(http.StatusServiceUnavailable, gin.H{
				"error": gin.H{"message": "无可用账号，请稍后重试", "type": "server_error"},
			})
			return
		}

		start := time.Now()
		proxyURL := strings.TrimSpace(stickyProxyURL)
		if proxyURL == "" {
			proxyURL = h.store.ResolveProxyForAccount(account)
		}
		useWebsocket := shouldUseWebsocketTransport(h.cfg, c.Request)
		logRequestDispatch(c, "/v1/chat/completions", attempt+1, account, proxyURL, model, reasoningEffort, attemptAcquireMs)

		resp, attemptTrace, reqErr := ExecuteRequestTraced(c.Request.Context(), account, codexBody, sessionID, proxyURL, apiKey, deviceCfg, downstreamHeaders, useWebsocket)
		durationMs := int(time.Since(start).Milliseconds())
		upstreamStageMs += int64(durationMs)
		c.Set("x-upstream-stage-ms", upstreamStageMs)
		statusCode := 0
		if resp != nil {
			statusCode = resp.StatusCode
		}
		if attemptTrace != nil && !attemptTrace.HeaderAt.IsZero() {
			logUpstreamAttemptHeaders(c, "/v1/chat/completions", attempt+1, account, statusCode, attemptTrace, requestStartedAt)
		}

		if reqErr != nil {
			if kind := classifyTransportFailure(reqErr); kind != "" {
				failedAttempts = append(failedAttempts, kind)
				h.store.ReportRequestFailure(account, kind, time.Duration(durationMs)*time.Millisecond)
			}
			logUpstreamAttemptResult(c, "/v1/chat/completions", attempt+1, account, proxyURL, 0, durationMs, requestStartedAt, reqErr.Error())
			h.store.Release(account)
			h.store.UnbindSessionAffinity(affinityKey, account.ID())
			excludeAccounts[account.ID()] = true

			// 不可重试的结构化错误直接返回
			if !IsRetryableError(reqErr) && classifyTransportFailure(reqErr) == "" {
				ErrorToGinResponse(c, reqErr)
				return
			}

			log.Printf("上游请求失败 (attempt %d): %v", attempt+1, reqErr)
			lastErr = reqErr
			continue
		}

		if resp.StatusCode != http.StatusOK {
			h.persistUsageAndSettleFromResponse(account, resp)
			errBody, _ := io.ReadAll(resp.Body)
			if kind := classifyHTTPFailure(resp.StatusCode, errBody); kind != "" {
				h.store.ReportRequestFailure(account, kind, time.Duration(durationMs)*time.Millisecond)
			}
			resp.Body.Close()
			logUpstreamAttemptResult(c, "/v1/chat/completions", attempt+1, account, proxyURL, resp.StatusCode, durationMs, requestStartedAt, fmt.Sprintf("http_%d", resp.StatusCode))
			h.store.Release(account)
			h.store.UnbindSessionAffinity(affinityKey, account.ID())
			excludeAccounts[account.ID()] = true

			log.Printf("上游返回错误 (attempt %d, status %d): %s", attempt+1, resp.StatusCode, string(errBody))
			failedAttempts = append(failedAttempts, strconv.Itoa(resp.StatusCode))
			logUpstreamError("/v1/chat/completions", resp.StatusCode, model, account.ID(), errBody)
			h.logUsage(&database.UsageLogInput{
				AccountID:        account.ID(),
				Endpoint:         "/v1/chat/completions",
				Model:            model,
				StatusCode:       resp.StatusCode,
				DurationMs:       durationMs,
				ReasoningEffort:  reasoningEffort,
				InboundEndpoint:  "/v1/chat/completions",
				UpstreamEndpoint: "/v1/responses",
				Stream:           isStream,
				ServiceTier:      serviceTier,
			})
			h.applyCooldown(account, resp.StatusCode, errBody, resp)
			lastStatusCode = resp.StatusCode
			lastBody = errBody
			if resp.StatusCode == http.StatusUnauthorized && isPermanentUnauthorizedError(errBody) {
				log.Printf("账号 %d 返回永久 401(no_organization)，已从账号池剔除", account.ID())
				h.forceDeleteAccount(account.ID(), "auto_clean_no_organization")
			}

			if isRetryableStatus(resp.StatusCode, errBody) && attempt < maxRetries {
				continue
			}

			h.sendFinalUpstreamError(c, resp.StatusCode, errBody)
			return
		}

		// 成功！翻译响应 + TTFT 跟踪
		if attempt > 0 {
			c.Set("x-upstream-attempts", attempt+1)
			c.Set("x-upstream-failed-attempts", strings.Join(failedAttempts, ","))
			log.Printf("请求重试成功: endpoint=/v1/chat/completions attempts=%d failed=%s account=%d", attempt+1, strings.Join(failedAttempts, ","), account.ID())
		} else {
			c.Set("x-upstream-attempts", 1)
		}
		account.Mu().RLock()
		c.Set("x-account-email", account.Email)
		account.Mu().RUnlock()
		c.Set("x-account-proxy", proxyURL)
		c.Set("x-model", model)
		c.Set("x-reasoning-effort", reasoningEffort)
		var firstTokenMs int
		var attemptFirstTokenMs int
		firstFrameRecorded := false
		var usage *UsageInfo
		var actualServiceTier string
		ttftRecorded := false
		gotTerminal := false // 是否收到 response.completed 或 response.failed
		deltaCharCount := 0  // 累计 delta 字符数（用于断流时估算 token）
		var readErr error
		var writeErr error
		wroteAnyBytes := false
		wroteVisibleBody := false
		pendingFrames := make([]string, 0, 8)
		var compactResult []byte

		chunkID := "chatcmpl-" + uuid.New().String()[:8]
		created := time.Now().Unix()

		if isStream {
			streamTranslator := NewStreamTranslator(chunkID, model)
			c.Header("Content-Type", "text/event-stream")
			c.Header("Cache-Control", "no-cache")
			c.Header("Connection", "keep-alive")
			c.Header("X-Accel-Buffering", "no")

			flusher, ok := c.Writer.(http.Flusher)
			if !ok {
				c.JSON(http.StatusInternalServerError, gin.H{
					"error": gin.H{"message": "streaming not supported", "type": "server_error"},
				})
				resp.Body.Close()
				h.store.Release(account)
				return
			}
			// 先发一个 SSE 注释帧，避免前置代理/网关等待首包超时（如 524）。
			if _, err := fmt.Fprint(c.Writer, ": keep-alive\n\n"); err != nil {
				c.JSON(http.StatusBadGateway, gin.H{
					"error": gin.H{"message": "下游连接写入失败", "type": "upstream_error"},
				})
				resp.Body.Close()
				h.store.Release(account)
				return
			}
			wroteAnyBytes = true
			flusher.Flush()

			readErr = ReadSSEStream(resp.Body, func(data []byte) bool {
				chunk, done := streamTranslator.Translate(data)

				eventType := gjson.GetBytes(data, "type").String()
				visibleEvent := isUserVisibleDeltaEvent(eventType, data)
				if !firstFrameRecorded {
					firstFrameRecorded = true
					logUpstreamFirstFrame(c, "/v1/chat/completions", attempt+1, eventType, requestStartedAt, start)
				}
				if !ttftRecorded && strings.Contains(eventType, ".delta") {
					attemptFirstTokenMs = int(time.Since(start).Milliseconds())
					firstTokenMs = int(time.Since(requestStartedAt).Milliseconds())
					ttftRecorded = true
					logUpstreamFirstVisible(c, "/v1/chat/completions", attempt+1, eventType, requestStartedAt, start)
					h.store.ReportFirstTokenLatency(account, time.Duration(attemptFirstTokenMs)*time.Millisecond)
				}
				// 累计 delta 字符数（文本 + function call 参数）
				if eventType == "response.output_text.delta" || eventType == "response.function_call_arguments.delta" {
					deltaCharCount += len(gjson.GetBytes(data, "delta").String())
				}
				if eventType == "response.completed" {
					usage = extractUsage(data)
					if tier := gjson.GetBytes(data, "response.service_tier").String(); tier != "" {
						actualServiceTier = tier
					}
					gotTerminal = true
				}
				if eventType == "response.failed" {
					gotTerminal = true
				}

				if chunk != nil {
					chunk, _ = sjson.SetBytes(chunk, "created", created)
					frame := fmt.Sprintf("data: %s\n\n", chunk)
					if !wroteVisibleBody {
						// 首个可见正文前先缓冲，若上游在此阶段断流可透明重试。
						if !visibleEvent && !done {
							pendingFrames = append(pendingFrames, frame)
						} else {
							for _, pending := range pendingFrames {
								if _, err := fmt.Fprint(c.Writer, pending); err != nil {
									writeErr = err
									return false
								}
							}
							pendingFrames = pendingFrames[:0]
							if _, err := fmt.Fprint(c.Writer, frame); err != nil {
								writeErr = err
								return false
							}
							wroteAnyBytes = true
							if visibleEvent {
								wroteVisibleBody = true
							}
							flusher.Flush()
						}
					} else {
						if _, err := fmt.Fprint(c.Writer, frame); err != nil {
							writeErr = err
							return false
						}
						wroteAnyBytes = true
						if visibleEvent {
							wroteVisibleBody = true
						}
						flusher.Flush()
					}
				}
				if done {
					if !wroteVisibleBody {
						for _, pending := range pendingFrames {
							if _, err := fmt.Fprint(c.Writer, pending); err != nil {
								writeErr = err
								return false
							}
						}
						pendingFrames = pendingFrames[:0]
					}
					if _, err := fmt.Fprint(c.Writer, "data: [DONE]\n\n"); err != nil {
						writeErr = err
						return false
					}
					wroteAnyBytes = true
					flusher.Flush()
					return false
				}
				return true
			})
		} else {
			var fullContent strings.Builder
			var toolCalls []ToolCallResult
			var lastResponseData []byte

			readErr = ReadSSEStream(resp.Body, func(data []byte) bool {
				eventType := gjson.GetBytes(data, "type").String()
				if !firstFrameRecorded {
					firstFrameRecorded = true
					logUpstreamFirstFrame(c, "/v1/chat/completions", attempt+1, eventType, requestStartedAt, start)
				}
				if !ttftRecorded && strings.Contains(eventType, ".delta") {
					attemptFirstTokenMs = int(time.Since(start).Milliseconds())
					firstTokenMs = int(time.Since(requestStartedAt).Milliseconds())
					ttftRecorded = true
					logUpstreamFirstVisible(c, "/v1/chat/completions", attempt+1, eventType, requestStartedAt, start)
					h.store.ReportFirstTokenLatency(account, time.Duration(attemptFirstTokenMs)*time.Millisecond)
				}
				switch eventType {
				case "response.output_text.delta":
					deltaCharCount += len(gjson.GetBytes(data, "delta").String())
					fullContent.WriteString(gjson.GetBytes(data, "delta").String())
				case "response.function_call_arguments.delta":
					deltaCharCount += len(gjson.GetBytes(data, "delta").String())
				case "response.completed":
					usage = extractUsage(data)
					if tier := gjson.GetBytes(data, "response.service_tier").String(); tier != "" {
						actualServiceTier = tier
					}
					// 从 response.output 提取 function_call 项
					toolCalls = ExtractToolCallsFromOutput(data)
					lastResponseData = data
					gotTerminal = true
					return false
				case "response.failed":
					gotTerminal = true
					return false
				}
				return true
			})

			result := []byte(`{}`)
			result, _ = sjson.SetBytes(result, "id", chunkID)
			result, _ = sjson.SetBytes(result, "object", "chat.completion")
			result, _ = sjson.SetBytes(result, "created", created)
			result, _ = sjson.SetBytes(result, "model", model)
			result, _ = sjson.SetBytes(result, "choices.0.index", 0)
			result, _ = sjson.SetBytes(result, "choices.0.message.role", "assistant")

			if fullContent.Len() == 0 && len(toolCalls) == 0 && lastResponseData != nil {
				if outputText := ExtractOutputTextFromOutput(lastResponseData); outputText != "" {
					fullContent.WriteString(outputText)
				}
			}

			if len(toolCalls) > 0 {
				// 有工具调用: 设置 tool_calls 和对应的 finish_reason
				contentStr := fullContent.String()
				if contentStr != "" {
					result, _ = sjson.SetBytes(result, "choices.0.message.content", contentStr)
				} else {
					result, _ = sjson.SetRawBytes(result, "choices.0.message.content", []byte("null"))
				}
				for i, tc := range toolCalls {
					prefix := fmt.Sprintf("choices.0.message.tool_calls.%d", i)
					result, _ = sjson.SetBytes(result, prefix+".id", tc.ID)
					result, _ = sjson.SetBytes(result, prefix+".type", "function")
					result, _ = sjson.SetBytes(result, prefix+".function.name", tc.Name)
					result, _ = sjson.SetBytes(result, prefix+".function.arguments", tc.Arguments)
				}
				result, _ = sjson.SetBytes(result, "choices.0.finish_reason", "tool_calls")
			} else {
				result, _ = sjson.SetBytes(result, "choices.0.message.content", fullContent.String())
				result, _ = sjson.SetBytes(result, "choices.0.finish_reason", "stop")
			}

			if usage != nil {
				result, _ = sjson.SetBytes(result, "usage.prompt_tokens", usage.PromptTokens)
				result, _ = sjson.SetBytes(result, "usage.completion_tokens", usage.CompletionTokens)
				result, _ = sjson.SetBytes(result, "usage.total_tokens", usage.TotalTokens)
			}

			compactResult = result
		}

		// 断流检测 + token 估算
		attemptDuration := int(time.Since(start).Milliseconds())
		totalDuration := int(time.Since(requestStartedAt).Milliseconds())
		c.Set("x-first-token-ms", firstTokenMs)
		outcome := classifyStreamOutcome(c.Request.Context().Err(), readErr, writeErr, gotTerminal)
		if shouldTransparentRetryStream(outcome, attempt, maxRetries, wroteVisibleBody, c.Request.Context().Err(), writeErr) {
			log.Printf("上游流在首包前断开，重置连接并重试 (attempt %d/%d, account %d, /v1/chat/completions): %s", attempt+1, maxRetries+1, account.ID(), outcome.failureMessage)
			logUpstreamAttemptResult(c, "/v1/chat/completions", attempt+1, account, proxyURL, outcome.logStatusCode, attemptDuration, requestStartedAt, "retry:"+outcome.failureMessage)
			recyclePooledClientForAccount(account)
			h.persistUsageAndSettleFromResponse(account, resp)
			h.store.ReportRequestFailure(account, outcome.failureKind, time.Duration(attemptDuration)*time.Millisecond)
			resp.Body.Close()
			h.store.Release(account)
			h.store.UnbindSessionAffinity(affinityKey, account.ID())
			excludeAccounts[account.ID()] = true
			lastErr = readErr
			if lastErr == nil {
				lastErr = errors.New(outcome.failureMessage)
			}
			continue
		}
		h.store.BindSessionAffinity(affinityKey, account, proxyURL)
		logStatusCode := outcome.logStatusCode
		if outcome.logStatusCode != http.StatusOK {
			log.Printf("流异常结束 (account %d, /v1/chat/completions, status %d): %s，已转发约 %d 字符", account.ID(), outcome.logStatusCode, outcome.failureMessage, deltaCharCount)
			if deltaCharCount > 0 {
				estOutputTokens := deltaCharCount / 3
				if estOutputTokens < 1 {
					estOutputTokens = 1
				}
				usage = &UsageInfo{
					OutputTokens:     estOutputTokens,
					CompletionTokens: estOutputTokens,
					TotalTokens:      estOutputTokens,
				}
			}
		}
		if isStream && outcome.logStatusCode != http.StatusOK && !wroteAnyBytes {
			c.JSON(http.StatusBadGateway, gin.H{
				"error": gin.H{
					"message": "上游流在首包前中断: " + outcome.failureMessage,
					"type":    "upstream_error",
					"code":    "upstream_stream_break",
				},
			})
		}
		if !isStream {
			if compactResult != nil {
				c.Data(http.StatusOK, "application/json", compactResult)
			} else {
				c.JSON(http.StatusBadGateway, gin.H{
					"error": gin.H{"message": "未收到完整的上游响应", "type": "upstream_error"},
				})
			}
		}

		resolvedServiceTier := resolveServiceTier(actualServiceTier, serviceTier)
		c.Set("x-service-tier", resolvedServiceTier)

		logInput := &database.UsageLogInput{
			AccountID:        account.ID(),
			Endpoint:         "/v1/chat/completions",
			Model:            model,
			StatusCode:       logStatusCode,
			DurationMs:       totalDuration,
			FirstTokenMs:     firstTokenMs,
			ReasoningEffort:  reasoningEffort,
			InboundEndpoint:  "/v1/chat/completions",
			UpstreamEndpoint: "/v1/responses",
			Stream:           isStream,
			ServiceTier:      resolvedServiceTier,
		}
		if usage != nil {
			logInput.PromptTokens = usage.PromptTokens
			logInput.CompletionTokens = usage.CompletionTokens
			logInput.TotalTokens = usage.TotalTokens
			logInput.InputTokens = usage.InputTokens
			logInput.OutputTokens = usage.OutputTokens
			logInput.ReasoningTokens = usage.ReasoningTokens
			logInput.CachedTokens = usage.CachedTokens
		}
		h.logUsage(logInput)

		resp.Body.Close()
		h.persistUsageAndSettleFromResponse(account, resp)
		if outcome.penalize {
			recyclePooledClientForAccount(account)
			h.store.ReportRequestFailure(account, outcome.failureKind, time.Duration(attemptDuration)*time.Millisecond)
		} else if outcome.logStatusCode == http.StatusOK {
			h.store.ReportRequestSuccess(account, schedulerLatency(attemptDuration, attemptFirstTokenMs))
		}
		logUpstreamAttemptResult(c, "/v1/chat/completions", attempt+1, account, proxyURL, outcome.logStatusCode, attemptDuration, requestStartedAt, "")
		h.store.Release(account)
		return
	}

	// 所有重试都失败
	if lastErr != nil {
		c.JSON(http.StatusBadGateway, gin.H{
			"error": gin.H{"message": "上游请求失败: " + lastErr.Error(), "type": "upstream_error"},
		})
	} else if lastStatusCode != 0 {
		h.sendFinalUpstreamError(c, lastStatusCode, lastBody)
	}
}

// handleStreamResponse 处理流式响应（翻译 Codex → OpenAI）
func (h *Handler) handleStreamResponse(c *gin.Context, body io.Reader, model, chunkID string, created int64) {
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")

	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": gin.H{"message": "streaming not supported", "type": "server_error"},
		})
		return
	}

	err := ReadSSEStream(body, func(data []byte) bool {
		chunk, done := TranslateStreamChunk(data, model, chunkID)
		if chunk != nil {
			chunk, _ = sjson.SetBytes(chunk, "created", created)
			fmt.Fprintf(c.Writer, "data: %s\n\n", chunk)
			flusher.Flush()
		}
		if done {
			fmt.Fprintf(c.Writer, "data: [DONE]\n\n")
			flusher.Flush()
			return false
		}
		return true
	})

	if err != nil {
		log.Printf("读取上游流失败: %v", err)
	}
}

// handleCompactResponse 处理非流式响应
func (h *Handler) handleCompactResponse(c *gin.Context, body io.Reader, model, chunkID string, created int64) {
	var fullContent strings.Builder
	var usage *UsageInfo

	_ = ReadSSEStream(body, func(data []byte) bool {
		eventType := gjson.GetBytes(data, "type").String()
		switch eventType {
		case "response.output_text.delta":
			delta := gjson.GetBytes(data, "delta").String()
			fullContent.WriteString(delta)
		case "response.completed":
			usage = extractUsage(data)
			return false
		case "response.failed":
			return false
		}
		return true
	})

	result := []byte(`{}`)
	result, _ = sjson.SetBytes(result, "id", chunkID)
	result, _ = sjson.SetBytes(result, "object", "chat.completion")
	result, _ = sjson.SetBytes(result, "created", created)
	result, _ = sjson.SetBytes(result, "model", model)
	result, _ = sjson.SetBytes(result, "choices.0.index", 0)
	result, _ = sjson.SetBytes(result, "choices.0.message.role", "assistant")
	result, _ = sjson.SetBytes(result, "choices.0.message.content", fullContent.String())
	result, _ = sjson.SetBytes(result, "choices.0.finish_reason", "stop")

	if usage != nil {
		result, _ = sjson.SetBytes(result, "usage.prompt_tokens", usage.PromptTokens)
		result, _ = sjson.SetBytes(result, "usage.completion_tokens", usage.CompletionTokens)
		result, _ = sjson.SetBytes(result, "usage.total_tokens", usage.TotalTokens)
	}

	c.Data(http.StatusOK, "application/json", result)
}

// ==================== 通用辅助 ====================

// parseRetryAfter 解析上游 429 响应中的重试时间（参考 CLIProxyAPI codex_executor.go:689-708）
func parseRetryAfter(body []byte) time.Duration {
	if len(body) == 0 {
		return 2 * time.Minute
	}

	// 解析 error.resets_at (Unix timestamp)
	if resetsAt := gjson.GetBytes(body, "error.resets_at").Int(); resetsAt > 0 {
		resetTime := time.Unix(resetsAt, 0)
		if resetTime.After(time.Now()) {
			d := time.Until(resetTime)
			if d > 0 {
				return d
			}
		}
	}

	// 解析 error.resets_in_seconds
	if secs := gjson.GetBytes(body, "error.resets_in_seconds").Int(); secs > 0 {
		return time.Duration(secs) * time.Second
	}

	// 默认 2 分钟
	return 2 * time.Minute
}

// applyCooldown 根据上游状态码设置智能冷却
func (h *Handler) applyCooldown(account *auth.Account, statusCode int, body []byte, resp *http.Response) {
	switch statusCode {
	case http.StatusTooManyRequests:
		errCode, errMsg := parseUpstreamErrorBrief(body)
		account.SetLastFailureDetail(statusCode, errCode, errMsg)
		now := time.Now()
		if details, ok := parseUsageLimitDetails(body); ok {
			var until time.Time
			if details.resetsAt > 0 {
				until = time.Unix(details.resetsAt, 0)
			} else if details.resetsInSeconds > 0 {
				until = now.Add(time.Duration(details.resetsInSeconds) * time.Second)
			}
			if until.After(now) {
				h.store.MarkCooldown(account, until.Sub(now), "full_usage")
				log.Printf("账号 %d 命中 usage_limit_reached，进入满额度等待至 %s", account.ID(), until.Format(time.RFC3339))
				return
			}
		}
		if h.store.MarkFullUsageCooldownFromSnapshot(account) {
			if until, _, active := account.GetCooldownSnapshot(); active {
				log.Printf("账号 %d 用量已满，进入等待模式至 %s", account.ID(), until.Format(time.RFC3339))
			} else {
				log.Printf("账号 %d 用量已满，进入等待模式", account.ID())
			}
			return
		}
		cooldown := auth.RateLimitedProbeInterval
		log.Printf("账号 %d 被限速 (plan=%s)，进入等待模式 %v（2小时测活一次）", account.ID(), account.GetPlanType(), cooldown)
		h.store.MarkCooldown(account, cooldown, "rate_limited")
	case http.StatusUnauthorized:
		h.applyUnauthorizedCooldown(account, statusCode, body)
	case http.StatusPaymentRequired:
		if !isDeactivatedWorkspaceError(statusCode, body) {
			return
		}
		h.applyUnauthorizedCooldown(account, statusCode, body)
	}
}

// compute429Cooldown 根据计划类型和 Codex 响应精确计算 429 冷却时间
func (h *Handler) compute429Cooldown(account *auth.Account, body []byte, resp *http.Response) time.Duration {
	// 1. 优先使用 Codex 响应体中的精确重置时间
	if resetDuration := parseRetryAfter(body); resetDuration > 2*time.Minute {
		// parseRetryAfter 默认返回 2min（无数据），超过 2min 说明解析到了真实的 resets_at/resets_in_seconds
		if resetDuration > 7*24*time.Hour {
			resetDuration = 7 * 24 * time.Hour // 最多 7 天
		}
		return resetDuration
	}

	// 2. 没有精确重置时间，根据套餐类型 + 用量窗口推断
	planType := strings.ToLower(account.GetPlanType())

	switch planType {
	case "free":
		// Free 只有 7d 窗口，429 = 额度耗尽，冷却 7 天
		return 7 * 24 * time.Hour

	case "plus", "team", "pro", "enterprise":
		// Team/Pro 有 5h + 7d 双窗口，需要判断是哪个窗口触发了限制
		return h.detectTeamCooldownWindow(resp)

	default:
		// 未知套餐，保守默认 5 小时
		return 5 * time.Hour
	}
}

// detectTeamCooldownWindow 通过响应头判断 Team/Pro 账号是哪个窗口触发的限制
func (h *Handler) detectTeamCooldownWindow(resp *http.Response) time.Duration {
	if resp == nil {
		return 5 * time.Hour // 保守默认
	}

	// Codex 返回两组窗口头：primary 和 secondary
	// x-codex-primary-window-minutes / x-codex-primary-used-percent
	// x-codex-secondary-window-minutes / x-codex-secondary-used-percent
	// 用量 >= 100% 的窗口就是触发限制的窗口

	primaryUsed := parseFloat(resp.Header.Get("x-codex-primary-used-percent"))
	primaryWindowMin := parseFloat(resp.Header.Get("x-codex-primary-window-minutes"))
	secondaryUsed := parseFloat(resp.Header.Get("x-codex-secondary-used-percent"))
	secondaryWindowMin := parseFloat(resp.Header.Get("x-codex-secondary-window-minutes"))

	// 找到 used >= 100% 的窗口
	primaryExhausted := primaryUsed >= 100
	secondaryExhausted := secondaryUsed >= 100

	switch {
	case primaryExhausted && secondaryExhausted:
		// 两个窗口都满了，取较大窗口的冷却时间
		return windowMinutesToCooldown(max(primaryWindowMin, secondaryWindowMin))
	case primaryExhausted:
		return windowMinutesToCooldown(primaryWindowMin)
	case secondaryExhausted:
		return windowMinutesToCooldown(secondaryWindowMin)
	default:
		// 都没满但还是 429，可能是短时 burst 限制
		return 5 * time.Hour
	}
}

// windowMinutesToCooldown 根据窗口分钟数决定冷却时长
func windowMinutesToCooldown(windowMinutes float64) time.Duration {
	switch {
	case windowMinutes >= 1440: // >= 1 天 → 7d 窗口
		return 7 * 24 * time.Hour
	case windowMinutes >= 60: // >= 1 小时 → 5h 窗口
		return 5 * time.Hour
	default:
		return 30 * time.Minute // 短窗口
	}
}

// parseCodexUsageHeaders 从 Codex 响应头解析 5h/7d 用量百分比
func parseCodexUsageHeaders(resp *http.Response, account *auth.Account) (float64, bool) {
	if resp == nil {
		return 0, false
	}

	// 解析 primary 和 secondary 窗口
	primaryUsedStr := resp.Header.Get("x-codex-primary-used-percent")
	primaryWindowStr := resp.Header.Get("x-codex-primary-window-minutes")
	primaryResetStr := resp.Header.Get("x-codex-primary-reset-after-seconds")
	secondaryUsedStr := resp.Header.Get("x-codex-secondary-used-percent")
	secondaryWindowStr := resp.Header.Get("x-codex-secondary-window-minutes")
	secondaryResetStr := resp.Header.Get("x-codex-secondary-reset-after-seconds")

	type windowData struct {
		usedPct   float64
		resetSec  float64
		windowMin float64
		valid     bool
	}

	parseWindow := func(usedStr, windowStr, resetStr string) windowData {
		if usedStr == "" {
			return windowData{}
		}
		return windowData{
			usedPct:   parseFloat(usedStr),
			windowMin: parseFloat(windowStr),
			resetSec:  parseFloat(resetStr),
			valid:     true,
		}
	}

	primary := parseWindow(primaryUsedStr, primaryWindowStr, primaryResetStr)
	secondary := parseWindow(secondaryUsedStr, secondaryWindowStr, secondaryResetStr)

	// 归一化：小窗口 (≤360min) → 5h，大窗口 (>360min) → 7d
	var w5h, w7d windowData
	now := time.Now()

	if primary.valid && secondary.valid {
		if primary.windowMin >= secondary.windowMin {
			w7d, w5h = primary, secondary
		} else {
			w7d, w5h = secondary, primary
		}
	} else if primary.valid {
		if primary.windowMin <= 360 && primary.windowMin > 0 {
			w5h = primary
		} else {
			w7d = primary
		}
	} else if secondary.valid {
		if secondary.windowMin <= 360 && secondary.windowMin > 0 {
			w5h = secondary
		} else {
			w7d = secondary
		}
	}

	// 写入 5h
	if w5h.valid {
		resetAt := now.Add(time.Duration(w5h.resetSec) * time.Second)
		account.SetUsageSnapshot5h(w5h.usedPct, resetAt)
	}

	// 写入 7d
	if w7d.valid {
		resetAt := now.Add(time.Duration(w7d.resetSec) * time.Second)
		account.SetReset7dAt(resetAt)
		account.SetUsagePercent7d(w7d.usedPct)
		return w7d.usedPct, true
	}

	return 0, false
}

// ParseCodexUsageHeaders 从响应头提取并更新账号用量信息
func ParseCodexUsageHeaders(resp *http.Response, account *auth.Account) (float64, bool) {
	return parseCodexUsageHeaders(resp, account)
}

func parseFloat(s string) float64 {
	if s == "" {
		return 0
	}
	v := 0.0
	fmt.Sscanf(s, "%f", &v)
	return v
}

func ensureResponseOutputFromDelta(responseJSON []byte, collectedText string) []byte {
	text := strings.TrimSpace(collectedText)
	if len(responseJSON) == 0 || text == "" {
		return responseJSON
	}
	output := gjson.GetBytes(responseJSON, "output")
	if output.Exists() && output.IsArray() && len(output.Array()) > 0 {
		return responseJSON
	}

	patched := responseJSON
	patched, _ = sjson.SetBytes(patched, "output.0.type", "message")
	patched, _ = sjson.SetBytes(patched, "output.0.role", "assistant")
	patched, _ = sjson.SetBytes(patched, "output.0.status", "completed")
	patched, _ = sjson.SetBytes(patched, "output.0.content.0.type", "output_text")
	patched, _ = sjson.SetBytes(patched, "output.0.content.0.text", text)
	return patched
}

// sendUpstreamError 发送上游错误响应给客户端
func (h *Handler) sendUpstreamError(c *gin.Context, statusCode int, body []byte) {
	statusCode = normalizeUpstreamStatusCode(statusCode, body)
	c.JSON(statusCode, gin.H{
		"error": gin.H{
			"message": fmt.Sprintf("上游返回错误 (status %d): %s", statusCode, string(body)),
			"type":    "upstream_error",
			"code":    fmt.Sprintf("upstream_%d", statusCode),
		},
	})
}

// sendFinalUpstreamError 重试用尽后的最终错误响应：识别 usage_limit_reached 改写为 503，其余透传
func (h *Handler) sendFinalUpstreamError(c *gin.Context, statusCode int, body []byte) {
	if statusCode == http.StatusTooManyRequests {
		if details, ok := parseUsageLimitDetails(body); ok {
			if details.resetsInSeconds > 0 {
				c.Header("Retry-After", fmt.Sprintf("%d", details.resetsInSeconds))
			}

			message := "账号池额度已耗尽，请稍后重试"
			if details.message != "" {
				message = fmt.Sprintf("%s：%s", message, details.message)
			}

			errInfo := gin.H{
				"message": message,
				"type":    "server_error",
				"code":    "account_pool_usage_limit_reached",
			}
			if details.planType != "" {
				errInfo["plan_type"] = details.planType
			}
			if details.resetsAt != 0 {
				errInfo["resets_at"] = details.resetsAt
			}
			if details.resetsInSeconds != 0 {
				errInfo["resets_in_seconds"] = details.resetsInSeconds
			}
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": errInfo})
			return
		}
	}

	h.sendUpstreamError(c, normalizeUpstreamStatusCode(statusCode, body), body)
}

// handleUpstreamError 统一处理上游错误（兼容旧调用）
func (h *Handler) handleUpstreamError(c *gin.Context, account *auth.Account, statusCode int, body []byte) {
	h.applyCooldown(account, statusCode, body, nil)
	h.sendUpstreamError(c, statusCode, body)
}

// SupportedModels 支持的模型列表（全局共享）
var SupportedModels = []string{
	"gpt-5.4", "gpt-5.4-mini", "gpt-5", "gpt-5-codex", "gpt-5-codex-mini",
	"gpt-5.1", "gpt-5.1-codex", "gpt-5.1-codex-mini", "gpt-5.1-codex-max",
	"gpt-5.2", "gpt-5.2-codex", "gpt-5.3-codex", "gpt-5.5-pro", "gpt-5.5",
}

// ListModels 列出可用模型
func (h *Handler) ListModels(c *gin.Context) {
	models := make([]api.Model, 0, len(SupportedModels))
	now := time.Now().Unix()
	for _, id := range SupportedModels {
		models = append(models, api.Model{
			ID:      id,
			Object:  "model",
			Created: now,
			OwnedBy: "openai",
		})
	}
	api.SendList(c, "list", models)
}
