package admin

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/codex2api/security"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// Handler 管理后台 API 处理器
type Handler struct {
	store          *auth.Store
	cache          cache.TokenCache
	db             *database.DB
	rateLimiter    *proxy.RateLimiter
	refreshAccount func(context.Context, int64) error
	cpuSampler     *cpuSampler
	startedAt      time.Time
	pgMaxConns     int
	redisPoolSize  int
	databaseDriver string
	databaseLabel  string
	cacheDriver    string
	cacheLabel     string
	adminSecretEnv string

	// 图表聚合内存缓存（10秒 TTL）
	chartCacheMu   sync.RWMutex
	chartCacheData map[string]*chartCacheEntry

	// 自动套餐识别：记录最近一次 wham/usage 同步时间，避免高频请求
	planSyncMu sync.Mutex
	planSyncAt map[int64]time.Time
}

type chartCacheEntry struct {
	data      *database.ChartAggregation
	expiresAt time.Time
}

// NewHandler 创建管理后台处理器
func NewHandler(store *auth.Store, db *database.DB, tc cache.TokenCache, rl *proxy.RateLimiter, adminSecretEnv string) *Handler {
	handler := &Handler{
		store:          store,
		cache:          tc,
		db:             db,
		rateLimiter:    rl,
		cpuSampler:     newCPUSampler(),
		startedAt:      time.Now(),
		databaseDriver: db.Driver(),
		databaseLabel:  db.Label(),
		cacheDriver:    tc.Driver(),
		cacheLabel:     tc.Label(),
		adminSecretEnv: adminSecretEnv,
		chartCacheData: make(map[string]*chartCacheEntry),
		planSyncAt:     make(map[int64]time.Time),
	}
	handler.refreshAccount = handler.refreshSingleAccount
	return handler
}

// SetPoolSizes 设置连接池大小跟踪值（由 main.go 在启动时调用）
func (h *Handler) SetPoolSizes(pgMaxConns, redisPoolSize int) {
	h.pgMaxConns = pgMaxConns
	h.redisPoolSize = redisPoolSize
}

// RegisterRoutes 注册管理 API 路由
func (h *Handler) RegisterRoutes(r *gin.Engine) {
	api := r.Group("/api/admin")
	api.Use(h.adminAuthMiddleware())
	api.GET("/stats", h.GetStats)
	api.GET("/accounts", h.ListAccounts)
	api.POST("/accounts", h.AddAccount)
	api.POST("/accounts/at", h.AddATAccount)
	api.POST("/accounts/import", h.ImportAccounts)
	api.DELETE("/accounts/:id", h.DeleteAccount)
	api.POST("/accounts/:id/refresh", h.RefreshAccount)
	api.GET("/accounts/:id/auth-info", h.GetAccountAuthInfo)
	api.GET("/accounts/:id/quota-info", h.GetAccountQuotaInfo)
	api.GET("/accounts/:id/raw-info", h.GetAccountRawInfo)
	api.POST("/accounts/:id/lock", h.ToggleAccountLock)
	api.GET("/accounts/:id/test", h.TestConnection)
	api.GET("/accounts/:id/usage", h.GetAccountUsage)
	api.POST("/accounts/batch-test", h.BatchTest)
	api.POST("/accounts/batch-refresh", h.BatchRefresh)
	api.POST("/accounts/clean-banned", h.CleanBanned)
	api.POST("/accounts/clean-rate-limited", h.CleanRateLimited)
	api.POST("/accounts/clean-error", h.CleanError)
	api.GET("/accounts/export", h.ExportAccounts)
	api.POST("/accounts/migrate", h.MigrateAccounts)
	api.GET("/accounts/event-trend", h.GetAccountEventTrend)
	api.GET("/usage/stats", h.GetUsageStats)
	api.GET("/usage/logs", h.GetUsageLogs)
	api.GET("/usage/chart-data", h.GetChartData)
	api.DELETE("/usage/logs", h.ClearUsageLogs)
	api.GET("/keys", h.ListAPIKeys)
	api.POST("/keys", h.CreateAPIKey)
	api.DELETE("/keys/:id", h.DeleteAPIKey)
	api.GET("/pubkeys", h.ListPublicAPIKeys)
	api.POST("/pubkeys", h.CreatePublicAPIKey)
	api.DELETE("/pubkeys/:id", h.DeletePublicAPIKey)
	api.GET("/redeem-codes", h.ListRedeemCodeSummaries)
	api.POST("/redeem-codes/import", h.ImportRedeemCodes)
	api.GET("/health", h.GetHealth)
	api.GET("/ops/overview", h.GetOpsOverview)
	api.GET("/settings", h.GetSettings)
	api.PUT("/settings", h.UpdateSettings)
	api.GET("/models", h.ListModels)
	api.GET("/proxies", h.ListProxies)
	api.POST("/proxies", h.AddProxies)
	api.DELETE("/proxies/:id", h.DeleteProxy)
	api.PATCH("/proxies/:id", h.UpdateProxy)
	api.POST("/proxies/batch-delete", h.BatchDeleteProxies)
	api.POST("/proxies/test", h.TestProxy)

	// OAuth 授权流程
	api.POST("/oauth/generate-auth-url", h.GenerateOAuthURL)
	api.POST("/oauth/exchange-code", h.ExchangeOAuthCode)
}

// adminAuthMiddleware 管理接口鉴权中间件（增强版，增加安全审计日志）
func (h *Handler) adminAuthMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		adminSecret, source := h.resolveAdminSecret(c.Request.Context())
		if adminSecret == "" {
			// 未配置管理密钥，跳过鉴权
			c.Next()
			return
		}

		adminKey := c.GetHeader("X-Admin-Key")
		if adminKey == "" {
			// 兼容 Authorization: Bearer 方式
			authHeader := c.GetHeader("Authorization")
			if strings.HasPrefix(authHeader, "Bearer ") {
				adminKey = strings.TrimPrefix(authHeader, "Bearer ")
			}
		}

		// 清理输入
		adminKey = security.SanitizeInput(adminKey)

		// 使用安全比较防止时序攻击
		if !security.SecureCompare(adminKey, adminSecret) {
			// 记录安全审计日志
			security.SecurityAuditLog("ADMIN_AUTH_FAILED", fmt.Sprintf("path=%s ip=%s source=%s", c.Request.URL.Path, c.ClientIP(), source))
			c.JSON(http.StatusUnauthorized, gin.H{
				"error": "管理密钥无效或缺失",
			})
			c.Abort()
			return
		}

		// 成功认证，记录审计日志
		if security.IsSensitiveEndpoint(c.Request.URL.Path) {
			security.SecurityAuditLog("ADMIN_ACCESS", fmt.Sprintf("path=%s ip=%s method=%s", c.Request.URL.Path, c.ClientIP(), c.Request.Method))
		}

		c.Next()
	}
}

func (h *Handler) resolveAdminSecret(ctx context.Context) (string, string) {
	if h.adminSecretEnv != "" {
		return h.adminSecretEnv, "env"
	}

	readCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	settings, err := h.db.GetSystemSettings(readCtx)
	if err != nil || settings == nil || settings.AdminSecret == "" {
		return "", "disabled"
	}
	return settings.AdminSecret, "database"
}

func (h *Handler) hasConfiguredAdminSecret(ctx context.Context) bool {
	adminSecret, _ := h.resolveAdminSecret(ctx)
	return strings.TrimSpace(adminSecret) != ""
}

// ==================== Stats ====================

// GetStats 获取仪表盘统计
func (h *Handler) GetStats(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()

	accounts, err := h.db.ListActive(ctx)
	if err != nil {
		writeInternalError(c, err)
		return
	}

	total := len(accounts)
	available := h.store.AvailableCount()
	errCount := 0
	for _, acc := range accounts {
		if acc.Status == "error" {
			errCount++
		}
	}

	usageStats, _ := h.db.GetUsageStats(ctx)
	todayReqs := int64(0)
	if usageStats != nil {
		todayReqs = usageStats.TodayRequests
	}

	c.JSON(http.StatusOK, statsResponse{
		Total:         total,
		Available:     available,
		Error:         errCount,
		TodayRequests: todayReqs,
	})
}

// ==================== Accounts ====================

type accountResponse struct {
	ID                  int64                      `json:"id"`
	Name                string                     `json:"name"`
	Email               string                     `json:"email"`
	PlanType            string                     `json:"plan_type"`
	UploaderID          *int64                     `json:"uploader_id,omitempty"`
	SettlementAmountUSD float64                    `json:"settlement_amount_usd"`
	Status              string                     `json:"status"`
	WaitMode            bool                       `json:"wait_mode"`
	WaitReason          string                     `json:"wait_reason,omitempty"`
	WaitUntil           string                     `json:"wait_until,omitempty"`
	WaitRemainingSec    int64                      `json:"wait_remaining_seconds,omitempty"`
	WaitProbeAt         string                     `json:"wait_probe_at,omitempty"`
	WaitProbeRemaining  int64                      `json:"wait_probe_remaining_seconds,omitempty"`
	LastFailureStatus   int                        `json:"last_failure_status,omitempty"`
	LastFailureCode     string                     `json:"last_failure_code,omitempty"`
	LastFailureMessage  string                     `json:"last_failure_message,omitempty"`
	ATOnly              bool                       `json:"at_only"`
	HealthTier          string                     `json:"health_tier"`
	SchedulerScore      float64                    `json:"scheduler_score"`
	ConcurrencyCap      int64                      `json:"dynamic_concurrency_limit"`
	ProxyURL            string                     `json:"proxy_url"`
	CreatedAt           string                     `json:"created_at"`
	UpdatedAt           string                     `json:"updated_at"`
	ActiveRequests      int64                      `json:"active_requests"`
	TotalRequests       int64                      `json:"total_requests"`
	LastUsedAt          string                     `json:"last_used_at"`
	SuccessRequests     int64                      `json:"success_requests"`
	ErrorRequests       int64                      `json:"error_requests"`
	UsagePercent7d      *float64                   `json:"usage_percent_7d"`
	UsagePercent5h      *float64                   `json:"usage_percent_5h"`
	Reset5hAt           string                     `json:"reset_5h_at,omitempty"`
	Reset7dAt           string                     `json:"reset_7d_at,omitempty"`
	ScoreBreakdown      schedulerBreakdownResponse `json:"scheduler_breakdown"`
	LastUnauthorizedAt  string                     `json:"last_unauthorized_at,omitempty"`
	LastRateLimitedAt   string                     `json:"last_rate_limited_at,omitempty"`
	LastTimeoutAt       string                     `json:"last_timeout_at,omitempty"`
	LastServerErrorAt   string                     `json:"last_server_error_at,omitempty"`
	Locked              bool                       `json:"locked"`
}

type schedulerBreakdownResponse struct {
	UnauthorizedPenalty float64 `json:"unauthorized_penalty"`
	RateLimitPenalty    float64 `json:"rate_limit_penalty"`
	TimeoutPenalty      float64 `json:"timeout_penalty"`
	ServerPenalty       float64 `json:"server_penalty"`
	FailurePenalty      float64 `json:"failure_penalty"`
	SuccessBonus        float64 `json:"success_bonus"`
	UsagePenalty7d      float64 `json:"usage_penalty_7d"`
	LatencyPenalty      float64 `json:"latency_penalty"`
	SuccessRatePenalty  float64 `json:"success_rate_penalty"`
}

// ListAccounts 获取账号列表
func (h *Handler) ListAccounts(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 20*time.Second)
	defer cancel()
	now := time.Now()

	h.store.TriggerUsageProbeAsync()
	h.store.TriggerRecoveryProbeAsync()

	rows, err := h.db.ListActiveForAdmin(ctx)
	if err != nil {
		writeInternalError(c, err)
		return
	}

	// 合并内存中的调度指标
	accountMap := make(map[int64]*auth.Account)
	for _, acc := range h.store.Accounts() {
		accountMap[acc.DBID] = acc
	}

	// 获取每账号的请求统计（单独超时，避免被前面的查询挤占时间）
	reqCountCtx, reqCountCancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer reqCountCancel()
	reqCounts, err := h.db.GetAccountRequestCounts(reqCountCtx)
	if err != nil {
		log.Printf("获取账号请求统计失败: %v", err)
		reqCounts = make(map[int64]*database.AccountRequestCount)
	}

	accounts := make([]accountResponse, 0, len(rows))
	for _, row := range rows {
		hasRT := row.GetCredential("refresh_token_present") != "" || row.GetCredential("refresh_token") != ""
		hasAT := row.GetCredential("access_token_present") != "" || row.GetCredential("access_token") != ""
		resp := accountResponse{
			ID:                  row.ID,
			Name:                row.Name,
			Email:               row.GetCredential("email"),
			PlanType:            auth.NormalizePlanType(row.GetCredential("plan_type")),
			SettlementAmountUSD: row.SettledAmount,
			Status:              row.Status,
			ATOnly:              !hasRT && hasAT,
			ProxyURL:            row.ProxyURL,
			Locked:              row.Locked,
			CreatedAt:           row.CreatedAt.Format(time.RFC3339),
			UpdatedAt:           row.UpdatedAt.Format(time.RFC3339),
		}
		if row.PublicAPIKeyID.Valid {
			uploaderID := row.PublicAPIKeyID.Int64
			resp.UploaderID = &uploaderID
		}
		if acc, ok := accountMap[row.ID]; ok {
			if plan := strings.TrimSpace(acc.GetPlanType()); plan != "" {
				resp.PlanType = plan
			}
			resp.ActiveRequests = acc.GetActiveRequests()
			resp.TotalRequests = acc.GetTotalRequests()
			debug := acc.GetSchedulerDebugSnapshot(int64(h.store.GetMaxConcurrency()))
			resp.HealthTier = debug.HealthTier
			resp.SchedulerScore = debug.SchedulerScore
			resp.ConcurrencyCap = debug.DynamicConcurrencyLimit
			resp.ScoreBreakdown = schedulerBreakdownResponse{
				UnauthorizedPenalty: debug.Breakdown.UnauthorizedPenalty,
				RateLimitPenalty:    debug.Breakdown.RateLimitPenalty,
				TimeoutPenalty:      debug.Breakdown.TimeoutPenalty,
				ServerPenalty:       debug.Breakdown.ServerPenalty,
				FailurePenalty:      debug.Breakdown.FailurePenalty,
				SuccessBonus:        debug.Breakdown.SuccessBonus,
				UsagePenalty7d:      debug.Breakdown.UsagePenalty7d,
				LatencyPenalty:      debug.Breakdown.LatencyPenalty,
				SuccessRatePenalty:  debug.Breakdown.SuccessRatePenalty,
			}
			if usagePct, ok := acc.GetUsagePercent7d(); ok {
				resp.UsagePercent7d = &usagePct
			}
			if usagePct5h, ok := acc.GetUsagePercent5h(); ok {
				resp.UsagePercent5h = &usagePct5h
			}
			if t := acc.GetReset5hAt(); !t.IsZero() {
				resp.Reset5hAt = t.Format(time.RFC3339)
			}
			if t := acc.GetReset7dAt(); !t.IsZero() {
				resp.Reset7dAt = t.Format(time.RFC3339)
			}
			if t := acc.GetLastUsedAt(); !t.IsZero() {
				resp.LastUsedAt = t.Format(time.RFC3339)
			}
			if !debug.LastUnauthorizedAt.IsZero() {
				resp.LastUnauthorizedAt = debug.LastUnauthorizedAt.Format(time.RFC3339)
			}
			if !debug.LastRateLimitedAt.IsZero() {
				resp.LastRateLimitedAt = debug.LastRateLimitedAt.Format(time.RFC3339)
			}
			if !debug.LastTimeoutAt.IsZero() {
				resp.LastTimeoutAt = debug.LastTimeoutAt.Format(time.RFC3339)
			}
			if !debug.LastServerErrorAt.IsZero() {
				resp.LastServerErrorAt = debug.LastServerErrorAt.Format(time.RFC3339)
			}
			if statusCode, code, message := acc.GetLastFailureDetail(); statusCode > 0 || code != "" || message != "" {
				resp.LastFailureStatus = statusCode
				resp.LastFailureCode = code
				resp.LastFailureMessage = message
			}
			// 使用运行时状态（优先于 DB 状态）
			resp.Status = acc.RuntimeStatus()
			if cooldownUntil, cooldownReason, activeCooldown := acc.GetCooldownSnapshot(); resp.Status != "unauthorized" && (activeCooldown || cooldownReason == "rate_limited") {
				resp.WaitMode = true
				reason := strings.TrimSpace(cooldownReason)
				if reason == "" {
					reason = resp.Status
				}
				if reason == "" || reason == "active" || reason == "ready" {
					reason = "cooldown"
				}
				resp.WaitReason = reason
				waitUntil := cooldownUntil
				if reason == "rate_limited" && !waitUntil.After(now) {
					// rate_limited 冷却过期但尚未测活成功时，退出时间视为“当前待测活”
					waitUntil = now
				}
				resp.WaitUntil = waitUntil.Format(time.RFC3339)
				if waitUntil.After(now) {
					resp.WaitRemainingSec = int64(waitUntil.Sub(now).Seconds())
				}

				rateProbeAt, fullUsageProbeAt := acc.GetProbeTimestamps()
				var nextProbeAt time.Time
				switch reason {
				case "rate_limited":
					// 限流等待：测活时间与等待退出时间保持一致（2 小时窗口）
					nextProbeAt = waitUntil
				case "full_usage":
					if !fullUsageProbeAt.IsZero() {
						nextProbeAt = fullUsageProbeAt.Add(auth.FullUsageProbeInterval)
					} else {
						nextProbeAt = now.Add(auth.FullUsageProbeInterval)
					}
					// 满额度等待不应晚于退出时间
					if !cooldownUntil.IsZero() && nextProbeAt.After(cooldownUntil) {
						nextProbeAt = cooldownUntil
					}
				default:
					if !rateProbeAt.IsZero() {
						nextProbeAt = rateProbeAt.Add(auth.RateLimitedProbeInterval)
					}
					if nextProbeAt.IsZero() {
						nextProbeAt = cooldownUntil
					}
				}
				if !nextProbeAt.IsZero() {
					resp.WaitProbeAt = nextProbeAt.Format(time.RFC3339)
					if nextProbeAt.After(now) {
						resp.WaitProbeRemaining = int64(nextProbeAt.Sub(now).Seconds())
					}
				}
			}
		} else if row.CooldownReason != "unauthorized" && ((row.CooldownReason == "rate_limited" && row.CooldownUntil.Valid) || (row.CooldownUntil.Valid && row.CooldownUntil.Time.After(now))) {
			// 兜底：账号不在运行时内存池时，使用数据库冷却信息回填等待态
			resp.WaitMode = true
			reason := strings.TrimSpace(row.CooldownReason)
			if reason == "" {
				reason = "cooldown"
			}
			resp.WaitReason = reason
			waitUntil := row.CooldownUntil.Time
			if reason == "rate_limited" && !waitUntil.After(now) {
				waitUntil = now
			}
			resp.WaitUntil = waitUntil.Format(time.RFC3339)
			if waitUntil.After(now) {
				resp.WaitRemainingSec = int64(waitUntil.Sub(now).Seconds())
			}
			resp.WaitProbeAt = waitUntil.Format(time.RFC3339)
			resp.WaitProbeRemaining = resp.WaitRemainingSec
			if resp.Status == "active" || resp.Status == "ready" {
				resp.Status = reason
			}
		}
		if rc, ok := reqCounts[row.ID]; ok {
			resp.SuccessRequests = rc.SuccessCount
			resp.ErrorRequests = rc.ErrorCount
		}
		accounts = append(accounts, resp)
	}

	c.JSON(http.StatusOK, accountsResponse{Accounts: accounts})
}

type addAccountReq struct {
	Name         string `json:"name"`
	RefreshToken string `json:"refresh_token"`
	ProxyURL     string `json:"proxy_url"`
}

// AddAccount 添加新账号（支持批量：refresh_token 按行分割）
func (h *Handler) AddAccount(c *gin.Context) {
	var req addAccountReq
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "请求格式错误")
		return
	}

	// 输入验证和清理
	req.Name = security.SanitizeInput(req.Name)
	req.ProxyURL = security.SanitizeInput(req.ProxyURL)

	if req.RefreshToken == "" {
		writeError(c, http.StatusBadRequest, "refresh_token 是必填字段")
		return
	}

	// 检查XSS和SQL注入
	if security.ContainsXSS(req.Name) || security.ContainsSQLInjection(req.Name) {
		writeError(c, http.StatusBadRequest, "名称包含非法字符")
		return
	}

	// 验证名称长度
	if utf8.RuneCountInString(req.Name) > 100 {
		writeError(c, http.StatusBadRequest, "名称长度不能超过100字符")
		return
	}

	// 验证代理URL
	if err := security.ValidateProxyURL(req.ProxyURL); err != nil {
		writeError(c, http.StatusBadRequest, "代理URL无效")
		return
	}

	// 按行分割，支持批量添加
	lines := strings.Split(req.RefreshToken, "\n")
	var tokens []string
	for _, line := range lines {
		t := strings.TrimSpace(security.SanitizeInput(line))
		if t != "" {
			tokens = append(tokens, t)
		}
	}

	if len(tokens) == 0 {
		writeError(c, http.StatusBadRequest, "未找到有效的 Refresh Token")
		return
	}

	// 限制批量添加数量
	if len(tokens) > 100 {
		writeError(c, http.StatusBadRequest, "单次最多添加100个账号")
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()

	successCount := 0
	failCount := 0

	for i, rt := range tokens {
		name := req.Name
		if name == "" {
			name = fmt.Sprintf("account-%d", i+1)
		} else if len(tokens) > 1 {
			name = fmt.Sprintf("%s-%d", req.Name, i+1)
		}

		id, err := h.db.InsertAccount(ctx, name, rt, req.ProxyURL)
		if err != nil {
			log.Printf("批量添加账号 %d 失败: %v", i+1, err)
			failCount++
			continue
		}

		successCount++
		h.db.InsertAccountEventAsync(id, "added", "manual")

		// 热加载：直接加入内存池
		newAcc := &auth.Account{
			DBID:         id,
			RefreshToken: rt,
			ProxyURL:     req.ProxyURL,
		}
		h.store.AddAccount(newAcc)

		// 异步刷新 AT
		go func(accountID int64) {
			refreshCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := h.store.RefreshSingle(refreshCtx, accountID); err != nil {
				log.Printf("新账号 %d 刷新失败: %v", accountID, err)
			} else {
				log.Printf("新账号 %d 刷新成功，已加入号池", accountID)
			}
			h.triggerForcedPlanSync(accountID, "manual_add")
		}(id)
	}

	// 记录安全审计日志
	security.SecurityAuditLog("ACCOUNTS_ADDED", fmt.Sprintf("success=%d failed=%d ip=%s", successCount, failCount, c.ClientIP()))

	msg := fmt.Sprintf("成功添加 %d 个账号", successCount)
	if failCount > 0 {
		msg += fmt.Sprintf("，%d 个失败", failCount)
	}

	c.JSON(http.StatusOK, gin.H{
		"message": msg,
		"success": successCount,
		"failed":  failCount,
	})
}

// addATAccountReq AT 模式添加账号请求
type addATAccountReq struct {
	Name        string `json:"name"`
	AccessToken string `json:"access_token"`
	ProxyURL    string `json:"proxy_url"`
}

// AddATAccount 添加 AT-only 账号（支持批量：access_token 按行分割）
func (h *Handler) AddATAccount(c *gin.Context) {
	var req addATAccountReq
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "请求格式错误")
		return
	}

	req.Name = security.SanitizeInput(req.Name)
	req.ProxyURL = security.SanitizeInput(req.ProxyURL)

	if req.AccessToken == "" {
		writeError(c, http.StatusBadRequest, "access_token 是必填字段")
		return
	}

	if security.ContainsXSS(req.Name) || security.ContainsSQLInjection(req.Name) {
		writeError(c, http.StatusBadRequest, "名称包含非法字符")
		return
	}

	if utf8.RuneCountInString(req.Name) > 100 {
		writeError(c, http.StatusBadRequest, "名称长度不能超过100字符")
		return
	}

	if err := security.ValidateProxyURL(req.ProxyURL); err != nil {
		writeError(c, http.StatusBadRequest, "代理URL无效")
		return
	}

	// 按行分割，支持批量添加
	lines := strings.Split(req.AccessToken, "\n")
	var tokens []string
	for _, line := range lines {
		t := strings.TrimSpace(line)
		if t != "" {
			tokens = append(tokens, t)
		}
	}

	if len(tokens) == 0 {
		writeError(c, http.StatusBadRequest, "未找到有效的 Access Token")
		return
	}

	if len(tokens) > 100 {
		writeError(c, http.StatusBadRequest, "单次最多添加100个账号")
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()

	successCount := 0
	failCount := 0

	for i, at := range tokens {
		name := req.Name
		if name == "" {
			name = fmt.Sprintf("at-account-%d", i+1)
		} else if len(tokens) > 1 {
			name = fmt.Sprintf("%s-%d", req.Name, i+1)
		}

		id, err := h.db.InsertATAccount(ctx, name, at, req.ProxyURL)
		if err != nil {
			log.Printf("添加 AT 账号 %d 失败: %v", i+1, err)
			failCount++
			continue
		}

		successCount++
		h.db.InsertAccountEventAsync(id, "added", "manual_at")

		// 解析 AT JWT 提取账号信息（email、account_id、过期时间）
		atInfo := auth.ParseAccessToken(at)

		// 热加载到内存池（AT-only，无 RT）
		newAcc := &auth.Account{
			DBID:        id,
			AccessToken: at,
			ExpiresAt:   time.Now().Add(1 * time.Hour),
			ProxyURL:    req.ProxyURL,
		}
		if atInfo != nil {
			newAcc.Email = atInfo.Email
			newAcc.AccountID = atInfo.ChatGPTAccountID
			if !atInfo.ExpiresAt.IsZero() {
				newAcc.ExpiresAt = atInfo.ExpiresAt
			}
		}
		h.store.AddAccount(newAcc)

		// 将解析到的信息持久化到数据库
		if atInfo != nil {
			creds := map[string]interface{}{
				"email":      atInfo.Email,
				"account_id": atInfo.ChatGPTAccountID,
				"expires_at": newAcc.ExpiresAt.Format(time.RFC3339),
			}
			if err := h.db.UpdateCredentials(ctx, id, creds); err != nil {
				log.Printf("AT 账号 %d 更新 credentials 失败: %v", id, err)
			}
		}
		h.triggerForcedPlanSync(id, "manual_at")
		log.Printf("AT 账号 %d 已加入号池 (id=%d, email=%s)", i+1, id, newAcc.Email)
	}

	security.SecurityAuditLog("AT_ACCOUNTS_ADDED", fmt.Sprintf("success=%d failed=%d ip=%s", successCount, failCount, c.ClientIP()))

	msg := fmt.Sprintf("成功添加 %d 个 AT 账号", successCount)
	if failCount > 0 {
		msg += fmt.Sprintf("，%d 个失败", failCount)
	}

	c.JSON(http.StatusOK, gin.H{
		"message": msg,
		"success": successCount,
		"failed":  failCount,
	})
}

// importToken 导入时的统一 token 载体
type importToken struct {
	refreshToken string
	accessToken  string
	name         string
	email        string
	idToken      string
	accountID    string
	planType     string
	expiresAt    string
}

type importAccountItem struct {
	name  string
	entry compatEntry
}

// jsonAccountEntry CLIProxyAPI 凭证 JSON 条目
type jsonAccountEntry struct {
	RefreshToken string `json:"refresh_token"`
	AccessToken  string `json:"access_token"`
	IDToken      string `json:"id_token"`
	AccountID    string `json:"account_id"`
	Email        string `json:"email"`
	Name         string `json:"name"`
	PlanType     string `json:"plan_type"`
	Expired      string `json:"expired"`
	ExpiresAt    string `json:"expires_at"`
}

type sub2apiImportPayload struct {
	Accounts []sub2apiAccountEntry `json:"accounts"`
}

type sub2apiAccountEntry struct {
	Name        string                    `json:"name"`
	Credentials sub2apiAccountCredentials `json:"credentials"`
}

type sub2apiAccountCredentials struct {
	RefreshToken string `json:"refresh_token"`
	AccessToken  string `json:"access_token"`
	IDToken      string `json:"id_token"`
	AccountID    string `json:"account_id"`
	Email        string `json:"email"`
	PlanType     string `json:"plan_type"`
	ExpiresAt    string `json:"expires_at"`
	Expired      string `json:"expired"`
}

var utf8BOM = []byte{0xef, 0xbb, 0xbf}

func trimUTF8BOM(data []byte) []byte {
	return bytes.TrimPrefix(data, utf8BOM)
}

// parseImportJSONTokens 同时兼容现有扁平 JSON 和 Sub2Api 顶层对象。
func parseImportJSONTokens(data []byte) ([]importToken, error) {
	data = trimUTF8BOM(data)
	if !json.Valid(data) {
		return nil, fmt.Errorf("invalid import json")
	}

	if tokens := parseFlatJSONImportTokens(data); len(tokens) > 0 {
		return tokens, nil
	}

	if tokens := parseSub2APIJSONImportTokens(data); len(tokens) > 0 {
		return tokens, nil
	}

	return nil, nil
}

func parseFlatJSONImportTokens(data []byte) []importToken {
	var entries []jsonAccountEntry
	if err := json.Unmarshal(data, &entries); err == nil {
		return jsonAccountEntriesToTokens(entries)
	}

	var single jsonAccountEntry
	if err := json.Unmarshal(data, &single); err == nil {
		return jsonAccountEntriesToTokens([]jsonAccountEntry{single})
	}

	return nil
}

func jsonAccountEntriesToTokens(entries []jsonAccountEntry) []importToken {
	tokens := make([]importToken, 0, len(entries))
	for _, entry := range entries {
		rt := strings.TrimSpace(entry.RefreshToken)
		at := strings.TrimSpace(entry.AccessToken)
		email := strings.TrimSpace(entry.Email)
		name := firstNonEmpty(entry.Name, email)

		if rt != "" || at != "" {
			tokens = append(tokens, importToken{
				refreshToken: rt,
				accessToken:  at,
				name:         name,
				email:        email,
				idToken:      strings.TrimSpace(entry.IDToken),
				accountID:    strings.TrimSpace(entry.AccountID),
				planType:     strings.TrimSpace(entry.PlanType),
				expiresAt:    firstNonEmpty(entry.ExpiresAt, entry.Expired),
			})
		}
	}
	return tokens
}

func parseSub2APIJSONImportTokens(data []byte) []importToken {
	var payload sub2apiImportPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil
	}

	tokens := make([]importToken, 0, len(payload.Accounts))
	for _, account := range payload.Accounts {
		rt := strings.TrimSpace(account.Credentials.RefreshToken)
		at := strings.TrimSpace(account.Credentials.AccessToken)
		name := strings.TrimSpace(account.Name)
		email := strings.TrimSpace(account.Credentials.Email)

		if name == "" {
			name = email
		}

		if rt != "" || at != "" {
			tokens = append(tokens, importToken{
				refreshToken: rt,
				accessToken:  at,
				name:         name,
				email:        email,
				idToken:      strings.TrimSpace(account.Credentials.IDToken),
				accountID:    strings.TrimSpace(account.Credentials.AccountID),
				planType:     strings.TrimSpace(account.Credentials.PlanType),
				expiresAt:    firstNonEmpty(account.Credentials.ExpiresAt, account.Credentials.Expired),
			})
		}
	}

	return tokens
}

// ImportAccounts 批量导入账号（支持 TXT / JSON）
func (h *Handler) ImportAccounts(c *gin.Context) {
	format := c.DefaultPostForm("format", "txt")
	proxyURL := c.PostForm("proxy_url")

	switch format {
	case "json":
		h.importAccountsJSON(c, proxyURL)
	case "at_txt":
		h.importAccountsATTXT(c, proxyURL)
	default:
		h.importAccountsTXT(c, proxyURL)
	}
}

// importAccountsTXT 通过 TXT 文件导入（每行一个 RT）
func (h *Handler) importAccountsTXT(c *gin.Context, proxyURL string) {
	file, header, err := c.Request.FormFile("file")
	if err != nil {
		writeError(c, http.StatusBadRequest, "请上传文件（字段名: file）")
		return
	}
	defer file.Close()

	if header.Size > 2*1024*1024 {
		writeError(c, http.StatusBadRequest, "文件大小不能超过 2MB")
		return
	}

	data, err := io.ReadAll(file)
	if err != nil {
		writeError(c, http.StatusBadRequest, "读取文件失败")
		return
	}

	// 按行分割，去重
	lines := strings.Split(string(data), "\n")
	seen := make(map[string]bool)
	var tokens []importToken
	for _, line := range lines {
		t := strings.TrimSpace(line)
		t = strings.TrimPrefix(t, "\xef\xbb\xbf") // 去除 UTF-8 BOM
		if t != "" && !seen[t] {
			seen[t] = true
			tokens = append(tokens, importToken{refreshToken: t})
		}
	}

	if len(tokens) == 0 {
		writeError(c, http.StatusBadRequest, "文件中未找到有效的 Refresh Token")
		return
	}

	h.importAccountsCommon(c, tokens, proxyURL)
}

// importAccountsJSON 通过 JSON 文件导入（兼容 CLIProxyAPI 凭证格式）
func (h *Handler) importAccountsJSON(c *gin.Context, proxyURL string) {
	if err := c.Request.ParseMultipartForm(32 << 20); err != nil {
		writeError(c, http.StatusBadRequest, "解析表单失败")
		return
	}

	files := c.Request.MultipartForm.File["file"]
	if len(files) == 0 {
		writeError(c, http.StatusBadRequest, "请上传至少一个 JSON 文件")
		return
	}

	var items []importAccountItem

	for _, fh := range files {
		if fh.Size > 2*1024*1024 {
			writeError(c, http.StatusBadRequest, fmt.Sprintf("文件 %s 大小超过 2MB", fh.Filename))
			return
		}

		f, err := fh.Open()
		if err != nil {
			writeError(c, http.StatusBadRequest, fmt.Sprintf("打开文件 %s 失败", fh.Filename))
			return
		}
		data, err := io.ReadAll(f)
		f.Close()
		if err != nil {
			writeError(c, http.StatusBadRequest, fmt.Sprintf("读取文件 %s 失败", fh.Filename))
			return
		}

		// 去除 UTF-8 BOM
		data = []byte(strings.TrimPrefix(string(data), "\xef\xbb\xbf"))

		entries, err := parseCompatEntries(data)
		if err != nil {
			writeError(c, http.StatusBadRequest, fmt.Sprintf("文件 %s 不是有效的 JSON 格式", fh.Filename))
			return
		}

		for idx, entry := range entries {
			parsed := parseCompatEntry(entry)
			if parsed.refreshToken == "" && parsed.accessToken == "" {
				continue
			}
			if proxyURL != "" {
				parsed.proxyURL = proxyURL
			}
			name := buildImportName(fh.Filename, parsed.email, idx, len(entries))
			items = append(items, importAccountItem{name: name, entry: parsed})
		}
	}

	if len(items) == 0 {
		writeError(c, http.StatusBadRequest, "JSON 文件中未找到有效的 refresh_token 或 access_token")
		return
	}

	h.importMixedAccountsCommon(c, items, "import")
}

func buildImportName(fileName, email string, idx, total int) string {
	name := strings.TrimSpace(security.SanitizeInput(email))
	if name == "" {
		clean := strings.TrimSpace(security.SanitizeInput(fileName))
		if clean != "" {
			if dot := strings.LastIndex(clean, "."); dot > 0 {
				clean = clean[:dot]
			}
			name = clean
		}
	}
	if name == "" {
		name = fmt.Sprintf("import-%d", time.Now().UnixNano())
	}
	if total > 1 {
		name = fmt.Sprintf("%s-%d", name, idx+1)
	}
	return name
}

// importEvent SSE 导入进度事件
type importEvent struct {
	Type      string `json:"type"` // progress | complete
	Current   int    `json:"current"`
	Total     int    `json:"total"`
	Success   int    `json:"success"`
	Duplicate int    `json:"duplicate"`
	Failed    int    `json:"failed"`
}

func sendImportEvent(c *gin.Context, e importEvent) {
	data, _ := json.Marshal(e)
	fmt.Fprintf(c.Writer, "data: %s\n\n", data)
	c.Writer.Flush()
}

func setupSSE(c *gin.Context) {
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")
	c.Writer.Flush()
}

// importAccountsCommon 公共的去重、并发插入、SSE 进度推送逻辑
func (h *Handler) importAccountsCommon(c *gin.Context, tokens []importToken, proxyURL string) {
	seenRT := make(map[string]bool)
	seenAT := make(map[string]bool)
	var unique []importToken
	for _, t := range tokens {
		if t.refreshToken != "" {
			if seenRT[t.refreshToken] {
				continue
			}
			seenRT[t.refreshToken] = true
			if t.accessToken != "" {
				seenAT[t.accessToken] = true
			}
			unique = append(unique, t)
			continue
		}
		if t.accessToken != "" {
			if seenAT[t.accessToken] {
				continue
			}
			seenAT[t.accessToken] = true
			unique = append(unique, t)
		}
	}

	dedupeCtx, dedupeCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer dedupeCancel()

	existingRTs, err := h.db.GetAllRefreshTokens(dedupeCtx)
	if err != nil {
		log.Printf("查询已有 RT 失败: %v", err)
		existingRTs = make(map[string]bool)
	}

	existingATs := make(map[string]bool)
	if len(seenAT) > 0 {
		existingATs, err = h.db.GetAllAccessTokens(dedupeCtx)
		if err != nil {
			log.Printf("查询已有 AT 失败: %v", err)
			existingATs = make(map[string]bool)
		}
	}

	var newTokens []importToken
	duplicateCount := 0
	for _, t := range unique {
		switch {
		case t.refreshToken != "":
			if existingRTs[t.refreshToken] || (t.accessToken != "" && existingATs[t.accessToken]) {
				duplicateCount++
			} else {
				newTokens = append(newTokens, t)
			}
		case t.accessToken != "":
			if existingATs[t.accessToken] {
				duplicateCount++
			} else {
				newTokens = append(newTokens, t)
			}
		}
	}

	total := len(unique)
	if len(newTokens) == 0 {
		c.JSON(http.StatusOK, gin.H{
			"message":   fmt.Sprintf("所有 %d 个 Token 已存在，无需导入", total),
			"success":   0,
			"duplicate": duplicateCount,
			"failed":    0,
			"total":     total,
		})
		return
	}

	setupSSE(c)

	var successCount int64
	var failCount int64
	var current int64
	sem := make(chan struct{}, 20)
	var wg sync.WaitGroup

	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				cur := int(atomic.LoadInt64(&current))
				suc := int(atomic.LoadInt64(&successCount))
				fai := int(atomic.LoadInt64(&failCount))
				sendImportEvent(c, importEvent{
					Type: "progress", Current: cur + duplicateCount, Total: total,
					Success: suc, Duplicate: duplicateCount, Failed: fai,
				})
			case <-done:
				return
			}
		}
	}()

	for i, t := range newTokens {
		sem <- struct{}{}
		wg.Add(1)
		go func(idx int, tok importToken) {
			defer wg.Done()
			defer func() { <-sem }()

			name := strings.TrimSpace(tok.name)
			if tok.accessToken != "" && tok.refreshToken == "" {
				if name == "" {
					name = fmt.Sprintf("at-import-%d", idx+1)
				}

				insertCtx, insertCancel := context.WithTimeout(context.Background(), 5*time.Second)
				id, err := h.db.InsertATAccount(insertCtx, name, tok.accessToken, proxyURL)
				insertCancel()
				if err != nil {
					log.Printf("导入 AT 账号 %d/%d 失败: %v", idx+1, len(newTokens), err)
					atomic.AddInt64(&failCount, 1)
					atomic.AddInt64(&current, 1)
					return
				}

				seed := normalizeTokenCredentialSeed(tokenCredentialSeed{
					accessToken:  tok.accessToken,
					idToken:      tok.idToken,
					accountID:    tok.accountID,
					email:        tok.email,
					planType:     tok.planType,
					expiresAtRaw: tok.expiresAt,
				})
				if len(tokenCredentialMap(seed)) > 0 {
					credCtx, credCancel := context.WithTimeout(context.Background(), 3*time.Second)
					if err := h.db.UpdateCredentials(credCtx, id, tokenCredentialMap(seed)); err != nil {
						log.Printf("导入 AT 账号 %d 更新 token 信息失败: %v", id, err)
					}
					credCancel()
				}

				h.store.AddAccount(accountFromCredentialSeed(id, proxyURL, seed))
				h.db.InsertAccountEventAsync(id, "added", "import_at")
				atomic.AddInt64(&successCount, 1)
				atomic.AddInt64(&current, 1)
				h.triggerForcedPlanSync(id, "import_at")
				return
			}

			if name == "" {
				name = fmt.Sprintf("import-%d", idx+1)
			}

			insertCtx, insertCancel := context.WithTimeout(context.Background(), 5*time.Second)
			id, err := h.db.InsertAccount(insertCtx, name, tok.refreshToken, proxyURL)
			insertCancel()
			if err != nil {
				log.Printf("导入账号 %d/%d 失败: %v", idx+1, len(newTokens), err)
				atomic.AddInt64(&failCount, 1)
				atomic.AddInt64(&current, 1)
				return
			}

			seed := normalizeTokenCredentialSeed(tokenCredentialSeed{
				refreshToken: tok.refreshToken,
				accessToken:  tok.accessToken,
				idToken:      tok.idToken,
				accountID:    tok.accountID,
				email:        tok.email,
				planType:     tok.planType,
				expiresAtRaw: tok.expiresAt,
			})
			if len(tokenCredentialMap(seed)) > 0 {
				credCtx, credCancel := context.WithTimeout(context.Background(), 3*time.Second)
				if err := h.db.UpdateCredentials(credCtx, id, tokenCredentialMap(seed)); err != nil {
					log.Printf("导入账号 %d 更新 token 信息失败: %v", id, err)
				}
				credCancel()
			}

			h.store.AddAccount(accountFromCredentialSeed(id, proxyURL, seed))
			h.db.InsertAccountEventAsync(id, "added", "import")
			atomic.AddInt64(&successCount, 1)
			atomic.AddInt64(&current, 1)

			if tok.accessToken == "" {
				go func(accountID int64) {
					refreshCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
					defer cancel()
					if err := h.store.RefreshSingle(refreshCtx, accountID); err != nil {
						log.Printf("导入账号 %d 刷新失败: %v", accountID, err)
					} else {
						log.Printf("导入账号 %d 刷新成功", accountID)
					}
					h.triggerForcedPlanSync(accountID, "import_rt")
				}(id)
			} else {
				h.triggerForcedPlanSync(id, "import")
			}
		}(i, t)
	}

	wg.Wait()
	close(done)

	suc := int(atomic.LoadInt64(&successCount))
	fai := int(atomic.LoadInt64(&failCount))
	sendImportEvent(c, importEvent{
		Type: "complete", Current: total, Total: total,
		Success: suc, Duplicate: duplicateCount, Failed: fai,
	})

	log.Printf("导入完成: success=%d, duplicate=%d, failed=%d, total=%d", suc, duplicateCount, fai, total)
}

func (h *Handler) importMixedAccountsCommon(c *gin.Context, items []importAccountItem, source string) {
	seenRT := make(map[string]bool)
	seenAT := make(map[string]bool)
	var unique []importAccountItem
	for _, item := range items {
		rt := strings.TrimSpace(item.entry.refreshToken)
		at := strings.TrimSpace(item.entry.accessToken)
		if rt == "" && at == "" {
			continue
		}
		if rt != "" {
			if seenRT[rt] {
				continue
			}
			seenRT[rt] = true
		} else if at != "" {
			if seenAT[at] {
				continue
			}
			seenAT[at] = true
		}
		unique = append(unique, item)
	}

	total := len(unique)
	if total == 0 {
		c.JSON(http.StatusOK, gin.H{
			"message":   "没有可导入账号",
			"success":   0,
			"duplicate": 0,
			"failed":    0,
			"total":     0,
		})
		return
	}

	dedupeCtx, dedupeCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer dedupeCancel()
	existingRTs, err := h.db.GetAllRefreshTokens(dedupeCtx)
	if err != nil {
		log.Printf("查询已有 RT 失败: %v", err)
		existingRTs = make(map[string]bool)
	}
	existingATs, err := h.db.GetAllAccessTokens(dedupeCtx)
	if err != nil {
		log.Printf("查询已有 AT 失败: %v", err)
		existingATs = make(map[string]bool)
	}

	var newItems []importAccountItem
	duplicateCount := 0
	for _, item := range unique {
		rt := strings.TrimSpace(item.entry.refreshToken)
		at := strings.TrimSpace(item.entry.accessToken)
		if rt != "" {
			if existingRTs[rt] {
				duplicateCount++
				continue
			}
		} else if at != "" {
			if existingATs[at] {
				duplicateCount++
				continue
			}
		}
		newItems = append(newItems, item)
	}

	if len(newItems) == 0 {
		c.JSON(http.StatusOK, gin.H{
			"message":   fmt.Sprintf("所有 %d 个账号已存在，无需导入", total),
			"success":   0,
			"duplicate": duplicateCount,
			"failed":    0,
			"total":     total,
		})
		return
	}

	setupSSE(c)

	var successCount int64
	var failCount int64
	var current int64
	sem := make(chan struct{}, 20)
	var wg sync.WaitGroup

	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				cur := int(atomic.LoadInt64(&current))
				suc := int(atomic.LoadInt64(&successCount))
				fai := int(atomic.LoadInt64(&failCount))
				sendImportEvent(c, importEvent{
					Type: "progress", Current: cur + duplicateCount, Total: total,
					Success: suc, Duplicate: duplicateCount, Failed: fai,
				})
			case <-done:
				return
			}
		}
	}()

	for i, item := range newItems {
		sem <- struct{}{}
		wg.Add(1)
		go func(idx int, it importAccountItem) {
			defer wg.Done()
			defer func() { <-sem }()

			name := strings.TrimSpace(it.name)
			if name == "" {
				name = fmt.Sprintf("import-%d", idx+1)
			}

			rt := strings.TrimSpace(it.entry.refreshToken)
			at := strings.TrimSpace(it.entry.accessToken)
			seed := normalizeTokenCredentialSeed(tokenCredentialSeed{
				refreshToken: rt,
				accessToken:  at,
				idToken:      it.entry.idToken,
				accountID:    it.entry.accountID,
				email:        it.entry.email,
				planType:     it.entry.planType,
				expiresAt:    it.entry.expiresAt,
				expiresAtRaw: it.entry.expiresRaw,
			})

			insertCtx, insertCancel := context.WithTimeout(context.Background(), 10*time.Second)
			var id int64
			var err error
			if rt != "" {
				id, err = h.db.InsertAccount(insertCtx, name, rt, it.entry.proxyURL)
			} else {
				id, err = h.db.InsertATAccount(insertCtx, name, at, it.entry.proxyURL)
			}
			if err != nil {
				insertCancel()
				log.Printf("导入账号 %d/%d 失败: %v", idx+1, len(newItems), err)
				atomic.AddInt64(&failCount, 1)
				atomic.AddInt64(&current, 1)
				return
			}

			if len(tokenCredentialMap(seed)) > 0 {
				if err := h.db.UpdateCredentials(insertCtx, id, tokenCredentialMap(seed)); err != nil {
					log.Printf("导入账号 %d 更新 token 信息失败: %v", id, err)
				}
			}
			insertCancel()

			h.store.AddAccount(accountFromCredentialSeed(id, it.entry.proxyURL, seed))
			h.db.InsertAccountEventAsync(id, "added", source)
			atomic.AddInt64(&successCount, 1)
			atomic.AddInt64(&current, 1)

			if rt != "" && at == "" {
				go func(accountID int64) {
					refreshCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
					defer cancel()
					if err := h.store.RefreshSingle(refreshCtx, accountID); err != nil {
						log.Printf("导入账号 %d 刷新失败: %v", accountID, err)
					} else {
						log.Printf("导入账号 %d 刷新成功", accountID)
					}
					h.triggerForcedPlanSync(accountID, "import_rt")
				}(id)
			} else if at != "" {
				h.triggerForcedPlanSync(id, "import")
			}
		}(i, item)
	}

	wg.Wait()
	close(done)

	suc := int(atomic.LoadInt64(&successCount))
	fai := int(atomic.LoadInt64(&failCount))
	sendImportEvent(c, importEvent{
		Type: "complete", Current: total, Total: total,
		Success: suc, Duplicate: duplicateCount, Failed: fai,
	})

	log.Printf("导入完成: success=%d, duplicate=%d, failed=%d, total=%d", suc, duplicateCount, fai, total)
}

// importAccountsATTXT 通过 TXT 文件导入 AT-only 账号（每行一个 Access Token）
func (h *Handler) importAccountsATTXT(c *gin.Context, proxyURL string) {
	file, header, err := c.Request.FormFile("file")
	if err != nil {
		writeError(c, http.StatusBadRequest, "请上传文件（字段名: file）")
		return
	}
	defer file.Close()

	if header.Size > 2*1024*1024 {
		writeError(c, http.StatusBadRequest, "文件大小不能超过 2MB")
		return
	}

	data, err := io.ReadAll(file)
	if err != nil {
		writeError(c, http.StatusBadRequest, "读取文件失败")
		return
	}

	// 按行分割，文件内去重
	lines := strings.Split(string(data), "\n")
	seen := make(map[string]bool)
	var atTokens []string
	for _, line := range lines {
		t := strings.TrimSpace(line)
		t = strings.TrimPrefix(t, "\xef\xbb\xbf")
		if t != "" && !seen[t] {
			seen[t] = true
			atTokens = append(atTokens, t)
		}
	}

	if len(atTokens) == 0 {
		writeError(c, http.StatusBadRequest, "文件中未找到有效的 Access Token")
		return
	}

	// 数据库去重
	dedupeCtx, dedupeCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer dedupeCancel()
	existingATs, err := h.db.GetAllAccessTokens(dedupeCtx)
	if err != nil {
		log.Printf("查询已有 AT 失败: %v", err)
		existingATs = make(map[string]bool)
	}

	var newTokens []string
	duplicateCount := 0
	for _, at := range atTokens {
		if existingATs[at] {
			duplicateCount++
		} else {
			newTokens = append(newTokens, at)
		}
	}

	total := len(atTokens)

	if len(newTokens) == 0 {
		c.JSON(http.StatusOK, gin.H{
			"message":   fmt.Sprintf("所有 %d 个 AT 已存在，无需导入", total),
			"success":   0,
			"duplicate": duplicateCount,
			"failed":    0,
			"total":     total,
		})
		return
	}

	// SSE 流式响应
	setupSSE(c)

	var successCount int64
	var failCount int64
	var current int64
	sem := make(chan struct{}, 20)
	var wg sync.WaitGroup

	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				cur := int(atomic.LoadInt64(&current))
				suc := int(atomic.LoadInt64(&successCount))
				fai := int(atomic.LoadInt64(&failCount))
				sendImportEvent(c, importEvent{
					Type: "progress", Current: cur + duplicateCount, Total: total,
					Success: suc, Duplicate: duplicateCount, Failed: fai,
				})
			case <-done:
				return
			}
		}
	}()

	for i, at := range newTokens {
		sem <- struct{}{}
		wg.Add(1)
		go func(idx int, accessToken string) {
			defer wg.Done()
			defer func() { <-sem }()

			name := fmt.Sprintf("at-import-%d", idx+1)

			insertCtx, insertCancel := context.WithTimeout(context.Background(), 5*time.Second)
			id, err := h.db.InsertATAccount(insertCtx, name, accessToken, proxyURL)
			insertCancel()

			if err != nil {
				log.Printf("导入 AT 账号 %d/%d 失败: %v", idx+1, len(newTokens), err)
				atomic.AddInt64(&failCount, 1)
				atomic.AddInt64(&current, 1)
				return
			}

			atomic.AddInt64(&successCount, 1)
			atomic.AddInt64(&current, 1)
			h.db.InsertAccountEventAsync(id, "added", "import_at")

			// 解析 AT JWT 提取账号信息
			atInfo := auth.ParseAccessToken(accessToken)

			newAcc := &auth.Account{
				DBID:        id,
				AccessToken: accessToken,
				ExpiresAt:   time.Now().Add(1 * time.Hour),
				ProxyURL:    proxyURL,
			}
			if atInfo != nil {
				newAcc.Email = atInfo.Email
				newAcc.AccountID = atInfo.ChatGPTAccountID
				if !atInfo.ExpiresAt.IsZero() {
					newAcc.ExpiresAt = atInfo.ExpiresAt
				}
				// 持久化解析到的账号信息
				credCtx, credCancel := context.WithTimeout(context.Background(), 3*time.Second)
				_ = h.db.UpdateCredentials(credCtx, id, map[string]interface{}{
					"email":      atInfo.Email,
					"account_id": atInfo.ChatGPTAccountID,
					"expires_at": newAcc.ExpiresAt.Format(time.RFC3339),
				})
				credCancel()

				// 如果解析到邮箱，用邮箱替换默认名称
				if atInfo.Email != "" {
					name = atInfo.Email
				}
			}
			h.store.AddAccount(newAcc)
			h.triggerForcedPlanSync(id, "import_at")
		}(i, at)
	}

	wg.Wait()
	close(done)

	suc := int(atomic.LoadInt64(&successCount))
	fai := int(atomic.LoadInt64(&failCount))
	sendImportEvent(c, importEvent{
		Type: "complete", Current: total, Total: total,
		Success: suc, Duplicate: duplicateCount, Failed: fai,
	})

	log.Printf("AT 导入完成: success=%d, duplicate=%d, failed=%d, total=%d", suc, duplicateCount, fai, total)
}

// GetAccountUsage 查询单个账号的用量统计
func (h *Handler) GetAccountUsage(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		writeError(c, http.StatusBadRequest, "无效的账号 ID")
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	detail, err := h.db.GetAccountUsageStats(ctx, id)
	if err != nil {
		writeInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, detail)
}

// DeleteAccount 删除账号
func (h *Handler) DeleteAccount(c *gin.Context) {
	idStr := c.Param("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeError(c, http.StatusBadRequest, "无效的账号 ID")
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()

	// 软删除：保留账号数据与事件记录，但从运行时池和 active 列表中移除。
	if err := h.db.SoftDeleteAccount(ctx, id); err != nil {
		writeError(c, http.StatusInternalServerError, "删除失败: "+err.Error())
		return
	}

	// 从内存池移除
	h.store.RemoveAccount(id)
	h.db.InsertAccountEventAsync(id, "deleted", "manual")

	writeMessage(c, http.StatusOK, "账号已删除")
}

// RefreshAccount 手动刷新账号 AT
func (h *Handler) RefreshAccount(c *gin.Context) {
	idStr := c.Param("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeError(c, http.StatusBadRequest, "无效的账号 ID")
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()

	refreshFn := h.refreshAccount
	if refreshFn == nil {
		refreshFn = h.refreshSingleAccount
	}
	if err := refreshFn(ctx, id); err != nil {
		if strings.Contains(err.Error(), "不存在") {
			writeError(c, http.StatusNotFound, err.Error())
			return
		}
		writeError(c, http.StatusInternalServerError, "刷新失败: "+err.Error())
		return
	}
	syncCtx, syncCancel := context.WithTimeout(c.Request.Context(), 25*time.Second)
	defer syncCancel()
	if err := h.forceSyncPlanFromWhamUsageByID(syncCtx, id); err != nil {
		log.Printf("账号 %d 刷新后套餐同步失败: %v", id, err)
		h.triggerForcedPlanSync(id, "manual_refresh_fallback")
	}

	writeMessage(c, http.StatusOK, "账号刷新成功")
}

// ToggleAccountLock 切换账号的锁定状态
func (h *Handler) ToggleAccountLock(c *gin.Context) {
	idStr := c.Param("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeError(c, http.StatusBadRequest, "无效的账号 ID")
		return
	}

	var req struct {
		Locked bool `json:"locked"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "请求格式错误")
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
	defer cancel()

	if err := h.db.SetAccountLocked(ctx, id, req.Locked); err != nil {
		writeError(c, http.StatusInternalServerError, "更新锁定状态失败: "+err.Error())
		return
	}

	// 同步更新内存中的状态
	if acc := h.store.FindByID(id); acc != nil {
		if req.Locked {
			atomic.StoreInt32(&acc.Locked, 1)
		} else {
			atomic.StoreInt32(&acc.Locked, 0)
		}
	}

	if req.Locked {
		writeMessage(c, http.StatusOK, "账号已锁定")
	} else {
		writeMessage(c, http.StatusOK, "账号已解锁")
	}
}

// ResetAccountStatus 重置单个账号状态为正常
func (h *Handler) ResetAccountStatus(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		writeError(c, http.StatusBadRequest, "无效的账号 ID")
		return
	}

	acc := h.store.FindByID(id)
	if acc == nil {
		writeError(c, http.StatusNotFound, "账号不在运行时池中")
		return
	}

	h.store.ClearCooldown(acc)
	acc.ClearUsageCache()
	writeMessage(c, http.StatusOK, "账号状态已重置")
}

// BatchResetStatus 批量重置账号状态为正常
func (h *Handler) BatchResetStatus(c *gin.Context) {
	var req struct {
		IDs []int64 `json:"ids"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || len(req.IDs) == 0 {
		writeError(c, http.StatusBadRequest, "请提供要重置的账号 ID 列表")
		return
	}

	success := 0
	fail := 0
	for _, id := range req.IDs {
		acc := h.store.FindByID(id)
		if acc == nil {
			fail++
			continue
		}
		h.store.ClearCooldown(acc)
		acc.ClearUsageCache()
		success++
	}

	c.JSON(http.StatusOK, gin.H{
		"message": fmt.Sprintf("已重置 %d 个账号状态", success),
		"success": success,
		"failed":  fail,
	})
}
func (h *Handler) refreshSingleAccount(ctx context.Context, id int64) error {
	if h == nil || h.store == nil {
		return fmt.Errorf("账号池未初始化")
	}
	return h.store.RefreshSingle(ctx, id)
}

// ==================== Health ====================

// GetHealth 系统健康检查（扩展版）
func (h *Handler) GetHealth(c *gin.Context) {
	c.JSON(http.StatusOK, healthResponse{
		Status:    "ok",
		Available: h.store.AvailableCount(),
		Total:     h.store.AccountCount(),
	})
}

// ==================== Usage ====================

// GetUsageStats 获取使用统计
func (h *Handler) GetUsageStats(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()

	stats, err := h.db.GetUsageStats(ctx)
	if err != nil {
		writeInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, stats)
}

// GetChartData 返回图表聚合数据（服务端分桶 + 内存缓存）
func (h *Handler) GetChartData(c *gin.Context) {
	startStr := c.Query("start")
	endStr := c.Query("end")
	bucketStr := c.DefaultQuery("bucket_minutes", "5")

	startTime, e1 := time.Parse(time.RFC3339, startStr)
	endTime, e2 := time.Parse(time.RFC3339, endStr)
	if e1 != nil || e2 != nil {
		writeError(c, http.StatusBadRequest, "start/end 参数格式错误，需要 RFC3339 格式")
		return
	}
	bucketMinutes, _ := strconv.Atoi(bucketStr)
	if bucketMinutes < 1 {
		bucketMinutes = 5
	}

	// 检查内存缓存（10秒 TTL）
	cacheKey := fmt.Sprintf("%s|%s|%d", startStr, endStr, bucketMinutes)
	h.chartCacheMu.RLock()
	if entry, ok := h.chartCacheData[cacheKey]; ok && time.Now().Before(entry.expiresAt) {
		h.chartCacheMu.RUnlock()
		c.JSON(http.StatusOK, entry.data)
		return
	}
	h.chartCacheMu.RUnlock()

	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	result, err := h.db.GetChartAggregation(ctx, startTime, endTime, bucketMinutes)
	if err != nil {
		writeInternalError(c, err)
		return
	}

	// 写入缓存
	h.chartCacheMu.Lock()
	h.chartCacheData[cacheKey] = &chartCacheEntry{
		data:      result,
		expiresAt: time.Now().Add(10 * time.Second),
	}
	// 清理过期条目（延迟清理，避免内存泄漏）
	for k, v := range h.chartCacheData {
		if time.Now().After(v.expiresAt) {
			delete(h.chartCacheData, k)
		}
	}
	h.chartCacheMu.Unlock()

	c.JSON(http.StatusOK, result)
}

// GetUsageLogs 获取使用日志
func (h *Handler) GetUsageLogs(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()

	startStr := c.Query("start")
	endStr := c.Query("end")

	if startStr != "" && endStr != "" {
		startTime, e1 := time.Parse(time.RFC3339, startStr)
		endTime, e2 := time.Parse(time.RFC3339, endStr)
		if e1 != nil || e2 != nil {
			writeError(c, http.StatusBadRequest, "start/end 参数格式错误，需要 RFC3339 格式")
			return
		}

		// 有 page 参数 → 服务端分页（Usage 页面表格）
		if pageStr := c.Query("page"); pageStr != "" {
			page, _ := strconv.Atoi(pageStr)
			pageSize := 20
			if ps := c.Query("page_size"); ps != "" {
				if n, err := strconv.Atoi(ps); err == nil && n > 0 && n <= 200 {
					pageSize = n
				}
			}
			var apiKeyID *int64
			if apiKeyIDStr := c.Query("api_key_id"); apiKeyIDStr != "" {
				parsed, err := strconv.ParseInt(apiKeyIDStr, 10, 64)
				if err != nil || parsed <= 0 {
					writeError(c, http.StatusBadRequest, "api_key_id 参数无效，需要正整数")
					return
				}
				apiKeyID = &parsed
			}

			filter := database.UsageLogFilter{
				Start:    startTime,
				End:      endTime,
				Page:     page,
				PageSize: pageSize,
				Email:    c.Query("email"),
				Model:    c.Query("model"),
				Endpoint: c.Query("endpoint"),
				APIKeyID: apiKeyID,
			}
			if fastStr := c.Query("fast"); fastStr != "" {
				v := fastStr == "true"
				filter.FastOnly = &v
			}
			if streamStr := c.Query("stream"); streamStr != "" {
				v := streamStr == "true"
				filter.StreamOnly = &v
			}

			result, err := h.db.ListUsageLogsByTimeRangePaged(ctx, filter)
			if err != nil {
				writeInternalError(c, err)
				return
			}
			c.JSON(http.StatusOK, result)
			return
		}

		// 无 page 参数 → 返回全量（Dashboard 图表聚合）
		logs, err := h.db.ListUsageLogsByTimeRange(ctx, startTime, endTime)
		if err != nil {
			writeInternalError(c, err)
			return
		}
		if logs == nil {
			logs = []*database.UsageLog{}
		}
		c.JSON(http.StatusOK, usageLogsResponse{Logs: logs})
		return
	}

	// 回退：limit 模式
	limit := 50
	if l := c.Query("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 {
			limit = n
		}
	}
	logs, err := h.db.ListRecentUsageLogs(ctx, limit)
	if err != nil {
		writeInternalError(c, err)
		return
	}
	if logs == nil {
		logs = []*database.UsageLog{}
	}
	c.JSON(http.StatusOK, usageLogsResponse{Logs: logs})
}

// ClearUsageLogs 清空所有使用日志
func (h *Handler) ClearUsageLogs(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	if err := h.db.ClearUsageLogs(ctx); err != nil {
		writeInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "日志已清空"})
}

// ==================== API Keys ====================

// ListAPIKeys 获取所有 API 密钥（脱敏版本）
func (h *Handler) ListAPIKeys(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()

	keys, err := h.db.ListAPIKeys(ctx)
	if err != nil {
		writeInternalError(c, err)
		return
	}

	// 转换为脱敏响应
	maskedKeys := make([]*MaskedAPIKeyRow, 0, len(keys))
	for _, k := range keys {
		maskedKeys = append(maskedKeys, NewMaskedAPIKeyRow(k))
	}

	c.JSON(http.StatusOK, apiKeysResponse{Keys: maskedKeys})
}

type createKeyReq struct {
	Name string `json:"name"`
	Key  string `json:"key"`
}

func generateKeyWithPrefix(prefix string) string {
	b := make([]byte, 24)
	rand.Read(b)
	return prefix + hex.EncodeToString(b)
}

// generateKey 生成随机 API Key
func generateKey() string {
	return generateKeyWithPrefix("sk-")
}

// generatePublicKey 生成公开上传 API Key
func generatePublicKey() string {
	return generateKeyWithPrefix("pk-")
}

// CreateAPIKey 创建新 API 密钥（增强版，带输入验证）
func (h *Handler) CreateAPIKey(c *gin.Context) {
	var req createKeyReq
	if err := c.ShouldBindJSON(&req); err != nil {
		req.Name = ""
	}

	// 输入验证和清理
	req.Name = security.SanitizeInput(req.Name)
	if req.Name == "" {
		req.Name = "default"
	}

	// 验证名称长度
	if utf8.RuneCountInString(req.Name) > 100 {
		writeError(c, http.StatusBadRequest, "名称长度不能超过100字符")
		return
	}

	// 检查XSS
	if security.ContainsXSS(req.Name) {
		writeError(c, http.StatusBadRequest, "名称包含非法字符")
		return
	}

	key := req.Key
	if key == "" {
		key = generateKey()
	} else {
		// 验证用户提供的key格式
		key = security.SanitizeInput(key)
		if !strings.HasPrefix(key, "sk-") || len(key) < 20 {
			writeError(c, http.StatusBadRequest, "API Key格式无效")
			return
		}
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()

	id, err := h.db.InsertAPIKey(ctx, req.Name, key)
	if err != nil {
		writeError(c, http.StatusInternalServerError, "创建失败: "+err.Error())
		return
	}

	// 记录安全审计日志
	security.SecurityAuditLog("API_KEY_CREATED", fmt.Sprintf("id=%d name=%s ip=%s", id, security.SanitizeLog(req.Name), c.ClientIP()))

	c.JSON(http.StatusOK, createAPIKeyResponse{
		ID:   id,
		Key:  key,
		Name: req.Name,
	})
}

// DeleteAPIKey 删除 API 密钥
func (h *Handler) DeleteAPIKey(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		writeError(c, http.StatusBadRequest, "无效 ID")
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()

	if err := h.db.DeleteAPIKey(ctx, id); err != nil {
		writeError(c, http.StatusInternalServerError, "删除失败: "+err.Error())
		return
	}
	writeMessage(c, http.StatusOK, "已删除")
}

// ListPublicAPIKeys 获取所有公开上传 API 密钥（脱敏版本）
func (h *Handler) ListPublicAPIKeys(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()

	keys, err := h.db.ListPublicAPIKeys(ctx)
	if err != nil {
		writeInternalError(c, err)
		return
	}

	maskedKeys := make([]*MaskedAPIKeyRow, 0, len(keys))
	for _, k := range keys {
		maskedKeys = append(maskedKeys, NewMaskedAPIKeyRow(k))
	}

	c.JSON(http.StatusOK, apiKeysResponse{Keys: maskedKeys})
}

// CreatePublicAPIKey 创建公开上传 API 密钥
func (h *Handler) CreatePublicAPIKey(c *gin.Context) {
	var req createKeyReq
	if err := c.ShouldBindJSON(&req); err != nil {
		req.Name = ""
	}

	req.Name = security.SanitizeInput(req.Name)
	if req.Name == "" {
		req.Name = "public-upload"
	}
	if utf8.RuneCountInString(req.Name) > 100 {
		writeError(c, http.StatusBadRequest, "名称长度不能超过100字符")
		return
	}
	if security.ContainsXSS(req.Name) {
		writeError(c, http.StatusBadRequest, "名称包含非法字符")
		return
	}

	key := req.Key
	if key == "" {
		key = generatePublicKey()
	} else {
		key = security.SanitizeInput(key)
		if !strings.HasPrefix(key, "pk-") || len(key) < 20 {
			writeError(c, http.StatusBadRequest, "API Key格式无效")
			return
		}
	}
	if adminSecret, _ := h.resolveAdminSecret(c.Request.Context()); adminSecret != "" && security.SecureCompare(key, adminSecret) {
		writeError(c, http.StatusBadRequest, "公开上传密钥不能与管理密钥相同")
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()

	id, err := h.db.InsertPublicAPIKeyWithMeta(ctx, req.Name, key, "admin", c.ClientIP(), 0)
	if err != nil {
		writeError(c, http.StatusInternalServerError, "创建失败: "+err.Error())
		return
	}

	security.SecurityAuditLog("PUBLIC_API_KEY_CREATED", fmt.Sprintf("id=%d name=%s ip=%s", id, security.SanitizeLog(req.Name), c.ClientIP()))

	c.JSON(http.StatusOK, createAPIKeyResponse{
		ID:   id,
		Key:  key,
		Name: req.Name,
	})
}

// DeletePublicAPIKey 删除公开上传 API 密钥
func (h *Handler) DeletePublicAPIKey(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		writeError(c, http.StatusBadRequest, "无效 ID")
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()

	if err := h.db.DeletePublicAPIKey(ctx, id); err != nil {
		writeError(c, http.StatusInternalServerError, "删除失败: "+err.Error())
		return
	}
	writeMessage(c, http.StatusOK, "已删除")
}

// ==================== Settings ====================

type settingsResponse struct {
	MaxConcurrency                   int     `json:"max_concurrency"`
	GlobalRPM                        int     `json:"global_rpm"`
	TestModel                        string  `json:"test_model"`
	TestConcurrency                  int     `json:"test_concurrency"`
	BackgroundRefreshIntervalMinutes int     `json:"background_refresh_interval_minutes"`
	UsageProbeMaxAgeMinutes          int     `json:"usage_probe_max_age_minutes"`
	RecoveryProbeIntervalMinutes     int     `json:"recovery_probe_interval_minutes"`
	ProxyURL                         string  `json:"proxy_url"`
	PgMaxConns                       int     `json:"pg_max_conns"`
	RedisPoolSize                    int     `json:"redis_pool_size"`
	AutoCleanUnauthorized            bool    `json:"auto_clean_unauthorized"`
	AutoCleanRateLimited             bool    `json:"auto_clean_rate_limited"`
	AdminSecret                      string  `json:"admin_secret"`
	AdminAuthSource                  string  `json:"admin_auth_source"`
	AutoCleanFullUsage               bool    `json:"auto_clean_full_usage"`
	AutoCleanFullUsageMode           string  `json:"auto_clean_full_usage_mode"`
	AutoCleanError                   bool    `json:"auto_clean_error"`
	AutoCleanExpired                 bool    `json:"auto_clean_expired"`
	ProxyPoolEnabled                 bool    `json:"proxy_pool_enabled"`
	FastSchedulerEnabled             bool    `json:"fast_scheduler_enabled"`
	PlusPortEnabled                  bool    `json:"plus_port_enabled"`
	PlusPortAccessFree               bool    `json:"plus_port_access_free"`
	SchedulerPreferredPlan           string  `json:"scheduler_preferred_plan"`
	SchedulerPlanBonus               int     `json:"scheduler_plan_bonus"`
	QuotaRatePlus                    float64 `json:"quota_rate_plus"`
	QuotaRatePro                     float64 `json:"quota_rate_pro"`
	QuotaRateTeam                    float64 `json:"quota_rate_team"`
	MaxRetries                       int     `json:"max_retries"`
	AllowRemoteMigration             bool    `json:"allow_remote_migration"`
	PublicInitialCreditUSD           float64 `json:"public_initial_credit_usd"`
	PublicFullCreditUSD              float64 `json:"public_full_credit_usd"`
	DatabaseDriver                   string  `json:"database_driver"`
	DatabaseLabel                    string  `json:"database_label"`
	CacheDriver                      string  `json:"cache_driver"`
	CacheLabel                       string  `json:"cache_label"`
	ExpiredCleaned                   int     `json:"expired_cleaned,omitempty"`
	ModelMapping                     string  `json:"model_mapping"`
}

type updateSettingsReq struct {
	MaxConcurrency                   *int     `json:"max_concurrency"`
	GlobalRPM                        *int     `json:"global_rpm"`
	TestModel                        *string  `json:"test_model"`
	TestConcurrency                  *int     `json:"test_concurrency"`
	BackgroundRefreshIntervalMinutes *int     `json:"background_refresh_interval_minutes"`
	UsageProbeMaxAgeMinutes          *int     `json:"usage_probe_max_age_minutes"`
	RecoveryProbeIntervalMinutes     *int     `json:"recovery_probe_interval_minutes"`
	ProxyURL                         *string  `json:"proxy_url"`
	PgMaxConns                       *int     `json:"pg_max_conns"`
	RedisPoolSize                    *int     `json:"redis_pool_size"`
	AutoCleanUnauthorized            *bool    `json:"auto_clean_unauthorized"`
	AutoCleanRateLimited             *bool    `json:"auto_clean_rate_limited"`
	AdminSecret                      *string  `json:"admin_secret"`
	AutoCleanFullUsage               *bool    `json:"auto_clean_full_usage"`
	AutoCleanFullUsageMode           *string  `json:"auto_clean_full_usage_mode"`
	AutoCleanError                   *bool    `json:"auto_clean_error"`
	AutoCleanExpired                 *bool    `json:"auto_clean_expired"`
	ProxyPoolEnabled                 *bool    `json:"proxy_pool_enabled"`
	FastSchedulerEnabled             *bool    `json:"fast_scheduler_enabled"`
	PlusPortEnabled                  *bool    `json:"plus_port_enabled"`
	PlusPortAccessFree               *bool    `json:"plus_port_access_free"`
	SchedulerPreferredPlan           *string  `json:"scheduler_preferred_plan"`
	SchedulerPlanBonus               *int     `json:"scheduler_plan_bonus"`
	QuotaRatePlus                    *float64 `json:"quota_rate_plus"`
	QuotaRatePro                     *float64 `json:"quota_rate_pro"`
	QuotaRateTeam                    *float64 `json:"quota_rate_team"`
	MaxRetries                       *int     `json:"max_retries"`
	AllowRemoteMigration             *bool    `json:"allow_remote_migration"`
	PublicInitialCreditUSD           *float64 `json:"public_initial_credit_usd"`
	PublicFullCreditUSD              *float64 `json:"public_full_credit_usd"`
	ModelMapping                     *string  `json:"model_mapping"`
}

func normalizeSchedulerPreferredPlan(raw string) (string, bool) {
	trimmed := strings.ToLower(strings.TrimSpace(raw))
	switch trimmed {
	case "", "off", "none", "disabled":
		return "", true
	}
	normalized := auth.NormalizePlanType(trimmed)
	switch normalized {
	case "free", "plus", "pro", "team", "enterprise":
		return normalized, true
	default:
		return "", false
	}
}

func defaultQuotaRates() (plus float64, pro float64, team float64) {
	return 10, 100, 10
}

func normalizeQuotaRate(value float64, fallback float64) (float64, bool) {
	if value <= 0 {
		return fallback, false
	}
	if value > 100000 {
		return fallback, false
	}
	return value, true
}

// GetSettings 获取当前系统设置
func (h *Handler) GetSettings(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
	defer cancel()
	dbSettings, _ := h.db.GetSystemSettings(ctx)
	_, adminAuthSource := h.resolveAdminSecret(c.Request.Context())
	adminSecret := ""
	if dbSettings != nil && adminAuthSource != "env" {
		adminSecret = dbSettings.AdminSecret
	}
	quotaRatePlus, quotaRatePro, quotaRateTeam := defaultQuotaRates()
	if dbSettings != nil {
		if normalized, ok := normalizeQuotaRate(dbSettings.QuotaRatePlus, quotaRatePlus); ok {
			quotaRatePlus = normalized
		}
		if normalized, ok := normalizeQuotaRate(dbSettings.QuotaRatePro, quotaRatePro); ok {
			quotaRatePro = normalized
		}
		if normalized, ok := normalizeQuotaRate(dbSettings.QuotaRateTeam, quotaRateTeam); ok {
			quotaRateTeam = normalized
		}
	}
	c.JSON(http.StatusOK, settingsResponse{
		MaxConcurrency:                   h.store.GetMaxConcurrency(),
		GlobalRPM:                        h.rateLimiter.GetRPM(),
		TestModel:                        h.store.GetTestModel(),
		TestConcurrency:                  h.store.GetTestConcurrency(),
		BackgroundRefreshIntervalMinutes: h.store.GetBackgroundRefreshIntervalMinutes(),
		UsageProbeMaxAgeMinutes:          h.store.GetUsageProbeMaxAgeMinutes(),
		RecoveryProbeIntervalMinutes:     h.store.GetRecoveryProbeIntervalMinutes(),
		ProxyURL:                         h.store.GetProxyURL(),
		PgMaxConns:                       h.pgMaxConns,
		RedisPoolSize:                    h.redisPoolSize,
		AutoCleanUnauthorized:            h.store.GetAutoCleanUnauthorized(),
		AutoCleanRateLimited:             h.store.GetAutoCleanRateLimited(),
		AdminSecret:                      adminSecret,
		AdminAuthSource:                  adminAuthSource,
		AutoCleanFullUsage:               h.store.GetAutoCleanFullUsage(),
		AutoCleanFullUsageMode:           h.store.GetAutoCleanFullUsageMode(),
		AutoCleanError:                   h.store.GetAutoCleanError(),
		AutoCleanExpired:                 h.store.GetAutoCleanExpired(),
		ProxyPoolEnabled:                 h.store.GetProxyPoolEnabled(),
		FastSchedulerEnabled:             h.store.FastSchedulerEnabled(),
		PlusPortEnabled:                  h.store.GetPlusPortEnabled(),
		PlusPortAccessFree:               h.store.GetPlusPortAccessFree(),
		SchedulerPreferredPlan:           h.store.GetPreferredPlanType(),
		SchedulerPlanBonus:               h.store.GetPreferredPlanBonus(),
		QuotaRatePlus:                    quotaRatePlus,
		QuotaRatePro:                     quotaRatePro,
		QuotaRateTeam:                    quotaRateTeam,
		MaxRetries:                       h.store.GetMaxRetries(),
		AllowRemoteMigration:             h.store.GetAllowRemoteMigration() && adminAuthSource != "disabled",
		PublicInitialCreditUSD:           h.store.GetPublicInitialCreditUSD(),
		PublicFullCreditUSD:              h.store.GetPublicFullCreditUSD(),
		DatabaseDriver:                   h.databaseDriver,
		DatabaseLabel:                    h.databaseLabel,
		CacheDriver:                      h.cacheDriver,
		CacheLabel:                       h.cacheLabel,
		ModelMapping:                     h.store.GetModelMapping(),
	})
}

// UpdateSettings 更新系统设置（实时生效）
func (h *Handler) UpdateSettings(c *gin.Context) {
	var req updateSettingsReq
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "请求格式错误")
		return
	}

	currentAdminSecret := ""
	quotaRatePlus, quotaRatePro, quotaRateTeam := defaultQuotaRates()
	if dbSettings, err := h.db.GetSystemSettings(c.Request.Context()); err == nil && dbSettings != nil {
		currentAdminSecret = dbSettings.AdminSecret
		if normalized, ok := normalizeQuotaRate(dbSettings.QuotaRatePlus, quotaRatePlus); ok {
			quotaRatePlus = normalized
		}
		if normalized, ok := normalizeQuotaRate(dbSettings.QuotaRatePro, quotaRatePro); ok {
			quotaRatePro = normalized
		}
		if normalized, ok := normalizeQuotaRate(dbSettings.QuotaRateTeam, quotaRateTeam); ok {
			quotaRateTeam = normalized
		}
	}
	if req.AdminSecret != nil {
		if h.adminSecretEnv == "" {
			currentAdminSecret = *req.AdminSecret
			log.Printf("设置已更新: admin_secret (长度=%d)", len(currentAdminSecret))
		} else {
			log.Printf("检测到环境变量 ADMIN_SECRET，忽略前端提交的 admin_secret")
		}
	}
	hasAdminSecret := strings.TrimSpace(currentAdminSecret) != "" || strings.TrimSpace(h.adminSecretEnv) != ""

	if req.MaxConcurrency != nil {
		v := *req.MaxConcurrency
		if v < 1 {
			v = 1
		}
		if v > 50 {
			v = 50
		}
		h.store.SetMaxConcurrency(v)
		log.Printf("设置已更新: max_concurrency = %d", v)
	}

	if req.GlobalRPM != nil {
		v := *req.GlobalRPM
		if v < 0 {
			v = 0
		}
		h.rateLimiter.UpdateRPM(v)
		log.Printf("设置已更新: global_rpm = %d", v)
	}

	if req.TestModel != nil && *req.TestModel != "" {
		h.store.SetTestModel(*req.TestModel)
		log.Printf("设置已更新: test_model = %s", *req.TestModel)
	}

	if req.TestConcurrency != nil {
		v := *req.TestConcurrency
		if v < 1 {
			v = 1
		}
		if v > 200 {
			v = 200
		}
		h.store.SetTestConcurrency(v)
		log.Printf("设置已更新: test_concurrency = %d", v)
	}

	if req.BackgroundRefreshIntervalMinutes != nil {
		v := *req.BackgroundRefreshIntervalMinutes
		if v < 1 {
			v = 1
		}
		if v > 1440 {
			v = 1440
		}
		h.store.SetBackgroundRefreshInterval(time.Duration(v) * time.Minute)
		log.Printf("设置已更新: background_refresh_interval_minutes = %d", v)
	}

	if req.UsageProbeMaxAgeMinutes != nil {
		v := *req.UsageProbeMaxAgeMinutes
		if v < 1 {
			v = 1
		}
		if v > 10080 {
			v = 10080
		}
		h.store.SetUsageProbeMaxAge(time.Duration(v) * time.Minute)
		log.Printf("设置已更新: usage_probe_max_age_minutes = %d", v)
	}

	if req.RecoveryProbeIntervalMinutes != nil {
		v := *req.RecoveryProbeIntervalMinutes
		if v < 1 {
			v = 1
		}
		if v > 10080 {
			v = 10080
		}
		h.store.SetRecoveryProbeInterval(time.Duration(v) * time.Minute)
		log.Printf("设置已更新: recovery_probe_interval_minutes = %d", v)
	}

	if req.ProxyURL != nil {
		h.store.SetProxyURL(*req.ProxyURL)
		log.Printf("设置已更新: proxy_url = %s", *req.ProxyURL)
	}

	if req.PgMaxConns != nil {
		v := *req.PgMaxConns
		if v < 5 {
			v = 5
		}
		if v > 500 {
			v = 500
		}
		h.db.SetMaxOpenConns(v)
		h.pgMaxConns = v
		log.Printf("设置已更新: pg_max_conns = %d", v)
	}

	if req.RedisPoolSize != nil {
		v := *req.RedisPoolSize
		if v < 5 {
			v = 5
		}
		if v > 500 {
			v = 500
		}
		h.cache.SetPoolSize(v)
		h.redisPoolSize = v
		log.Printf("设置已更新: redis_pool_size = %d", v)
	}

	if req.AutoCleanUnauthorized != nil {
		h.store.SetAutoCleanUnauthorized(*req.AutoCleanUnauthorized)
		log.Printf("设置已更新: auto_clean_unauthorized = %t", *req.AutoCleanUnauthorized)
	}

	if req.AutoCleanRateLimited != nil {
		h.store.SetAutoCleanRateLimited(*req.AutoCleanRateLimited)
		log.Printf("设置已更新: auto_clean_rate_limited = %t", *req.AutoCleanRateLimited)
	}

	if req.AutoCleanFullUsageMode != nil {
		rawMode := strings.ToLower(strings.TrimSpace(*req.AutoCleanFullUsageMode))
		switch rawMode {
		case auth.AutoCleanFullUsageModeOff, auth.AutoCleanFullUsageModeDelete, auth.AutoCleanFullUsageModeWait:
		default:
			writeError(c, http.StatusBadRequest, "auto_clean_full_usage_mode 仅支持 off/delete/wait")
			return
		}
		h.store.SetAutoCleanFullUsageMode(rawMode)
		log.Printf("设置已更新: auto_clean_full_usage_mode = %s", rawMode)
	} else if req.AutoCleanFullUsage != nil {
		// 兼容旧版布尔开关：true=delete，false=off
		h.store.SetAutoCleanFullUsage(*req.AutoCleanFullUsage)
		log.Printf("设置已更新: auto_clean_full_usage(legacy) = %t -> mode=%s", *req.AutoCleanFullUsage, h.store.GetAutoCleanFullUsageMode())
	}

	if req.AutoCleanError != nil {
		h.store.SetAutoCleanError(*req.AutoCleanError)
		log.Printf("设置已更新: auto_clean_error = %t", *req.AutoCleanError)
	}

	var expiredCleaned int
	if req.AutoCleanExpired != nil {
		h.store.SetAutoCleanExpired(*req.AutoCleanExpired)
		log.Printf("设置已更新: auto_clean_expired = %t", *req.AutoCleanExpired)
		// 开启时立即同步执行一次清理
		if *req.AutoCleanExpired {
			expiredCleaned = h.store.CleanExpiredNow()
		}
	}

	if req.ProxyPoolEnabled != nil {
		h.store.SetProxyPoolEnabled(*req.ProxyPoolEnabled)
		if *req.ProxyPoolEnabled {
			_ = h.store.ReloadProxyPool()
		}
		log.Printf("设置已更新: proxy_pool_enabled = %t", *req.ProxyPoolEnabled)
	}

	if req.FastSchedulerEnabled != nil {
		h.store.SetFastSchedulerEnabled(*req.FastSchedulerEnabled)
		log.Printf("设置已更新: fast_scheduler_enabled = %t", *req.FastSchedulerEnabled)
	}
	if req.PlusPortEnabled != nil {
		h.store.SetPlusPortEnabled(*req.PlusPortEnabled)
		log.Printf("设置已更新: plus_port_enabled = %t", *req.PlusPortEnabled)
	}
	if req.PlusPortAccessFree != nil {
		h.store.SetPlusPortAccessFree(*req.PlusPortAccessFree)
		log.Printf("设置已更新: plus_port_access_free = %t", *req.PlusPortAccessFree)
	}

	preferredPlan := h.store.GetPreferredPlanType()
	preferredBonus := h.store.GetPreferredPlanBonus()
	needApplyPreferredPlan := false
	if req.SchedulerPreferredPlan != nil {
		normalizedPlan, ok := normalizeSchedulerPreferredPlan(*req.SchedulerPreferredPlan)
		if !ok {
			writeError(c, http.StatusBadRequest, "scheduler_preferred_plan 仅支持 off/free/plus/pro/team/enterprise")
			return
		}
		preferredPlan = normalizedPlan
		needApplyPreferredPlan = true
	}
	if req.SchedulerPlanBonus != nil {
		v := *req.SchedulerPlanBonus
		if v < 0 {
			v = 0
		}
		if v > 200 {
			v = 200
		}
		preferredBonus = v
		needApplyPreferredPlan = true
	}
	if needApplyPreferredPlan {
		h.store.SetPreferredPlanPriority(preferredPlan, preferredBonus)
		log.Printf("设置已更新: scheduler_preferred_plan = %s, scheduler_plan_bonus = %d", h.store.GetPreferredPlanType(), h.store.GetPreferredPlanBonus())
	}

	if req.QuotaRatePlus != nil {
		normalized, ok := normalizeQuotaRate(*req.QuotaRatePlus, quotaRatePlus)
		if !ok {
			writeError(c, http.StatusBadRequest, "quota_rate_plus 必须在 (0, 100000] 范围内")
			return
		}
		quotaRatePlus = normalized
	}
	if req.QuotaRatePro != nil {
		normalized, ok := normalizeQuotaRate(*req.QuotaRatePro, quotaRatePro)
		if !ok {
			writeError(c, http.StatusBadRequest, "quota_rate_pro 必须在 (0, 100000] 范围内")
			return
		}
		quotaRatePro = normalized
	}
	if req.QuotaRateTeam != nil {
		normalized, ok := normalizeQuotaRate(*req.QuotaRateTeam, quotaRateTeam)
		if !ok {
			writeError(c, http.StatusBadRequest, "quota_rate_team 必须在 (0, 100000] 范围内")
			return
		}
		quotaRateTeam = normalized
	}

	if req.MaxRetries != nil {
		v := *req.MaxRetries
		if v < 0 {
			v = 0
		}
		if v > 10 {
			v = 10
		}
		h.store.SetMaxRetries(v)
		log.Printf("设置已更新: max_retries = %d", v)
	}

	initialCredit := h.store.GetPublicInitialCreditUSD()
	fullCredit := h.store.GetPublicFullCreditUSD()
	if req.PublicInitialCreditUSD != nil {
		initialCredit = *req.PublicInitialCreditUSD
	}
	if req.PublicFullCreditUSD != nil {
		fullCredit = *req.PublicFullCreditUSD
	}
	if req.PublicInitialCreditUSD != nil || req.PublicFullCreditUSD != nil {
		if initialCredit < 0 {
			initialCredit = 0
		}
		if fullCredit < 0 {
			fullCredit = 0
		}
		if fullCredit < initialCredit {
			writeError(c, http.StatusBadRequest, "full_credit 必须大于或等于 initial_credit")
			return
		}
		h.store.SetPublicCreditConfig(initialCredit, fullCredit)
		log.Printf("设置已更新: public_credit initial=%.4f full=%.4f", h.store.GetPublicInitialCreditUSD(), h.store.GetPublicFullCreditUSD())
	}

	if req.AllowRemoteMigration != nil {
		if *req.AllowRemoteMigration && !hasAdminSecret {
			writeError(c, http.StatusBadRequest, "请先设置管理密钥，再启用远程迁移")
			return
		}
		h.store.SetAllowRemoteMigration(*req.AllowRemoteMigration)
		log.Printf("设置已更新: allow_remote_migration = %t", *req.AllowRemoteMigration)
	} else if !hasAdminSecret {
		h.store.SetAllowRemoteMigration(false)
	}

	if req.ModelMapping != nil {
		h.store.SetModelMapping(*req.ModelMapping)
		log.Printf("设置已更新: model_mapping")
	}

	// 持久化保存到数据库
	err := h.db.UpdateSystemSettings(c.Request.Context(), &database.SystemSettings{
		MaxConcurrency:                   h.store.GetMaxConcurrency(),
		GlobalRPM:                        h.rateLimiter.GetRPM(),
		TestModel:                        h.store.GetTestModel(),
		TestConcurrency:                  h.store.GetTestConcurrency(),
		BackgroundRefreshIntervalMinutes: h.store.GetBackgroundRefreshIntervalMinutes(),
		UsageProbeMaxAgeMinutes:          h.store.GetUsageProbeMaxAgeMinutes(),
		RecoveryProbeIntervalMinutes:     h.store.GetRecoveryProbeIntervalMinutes(),
		ProxyURL:                         h.store.GetProxyURL(),
		PgMaxConns:                       h.pgMaxConns,
		RedisPoolSize:                    h.redisPoolSize,
		AutoCleanUnauthorized:            h.store.GetAutoCleanUnauthorized(),
		AutoCleanRateLimited:             h.store.GetAutoCleanRateLimited(),
		AdminSecret:                      currentAdminSecret,
		AutoCleanFullUsage:               h.store.GetAutoCleanFullUsage(),
		AutoCleanFullUsageMode:           h.store.GetAutoCleanFullUsageMode(),
		AutoCleanError:                   h.store.GetAutoCleanError(),
		AutoCleanExpired:                 h.store.GetAutoCleanExpired(),
		ProxyPoolEnabled:                 h.store.GetProxyPoolEnabled(),
		FastSchedulerEnabled:             h.store.FastSchedulerEnabled(),
		PlusPortEnabled:                  h.store.GetPlusPortEnabled(),
		PlusPortAccessFree:               h.store.GetPlusPortAccessFree(),
		SchedulerPreferredPlan:           h.store.GetPreferredPlanType(),
		SchedulerPlanBonus:               h.store.GetPreferredPlanBonus(),
		QuotaRatePlus:                    quotaRatePlus,
		QuotaRatePro:                     quotaRatePro,
		QuotaRateTeam:                    quotaRateTeam,
		MaxRetries:                       h.store.GetMaxRetries(),
		AllowRemoteMigration:             h.store.GetAllowRemoteMigration() && hasAdminSecret,
		PublicInitialCreditUSD:           h.store.GetPublicInitialCreditUSD(),
		PublicFullCreditUSD:              h.store.GetPublicFullCreditUSD(),
		ModelMapping:                     h.store.GetModelMapping(),
	})
	if err != nil {
		log.Printf("无法持久化保存设置: %v", err)
	}

	if h.store.GetAutoCleanUnauthorized() || h.store.GetAutoCleanRateLimited() || h.store.GetAutoCleanError() {
		h.store.TriggerAutoCleanupAsync()
	}

	adminSecretForDisplay := currentAdminSecret
	adminAuthSource := func() string {
		_, source := h.resolveAdminSecret(c.Request.Context())
		return source
	}()
	if adminAuthSource == "env" {
		adminSecretForDisplay = ""
	}

	c.JSON(http.StatusOK, settingsResponse{
		MaxConcurrency:                   h.store.GetMaxConcurrency(),
		GlobalRPM:                        h.rateLimiter.GetRPM(),
		TestModel:                        h.store.GetTestModel(),
		TestConcurrency:                  h.store.GetTestConcurrency(),
		BackgroundRefreshIntervalMinutes: h.store.GetBackgroundRefreshIntervalMinutes(),
		UsageProbeMaxAgeMinutes:          h.store.GetUsageProbeMaxAgeMinutes(),
		RecoveryProbeIntervalMinutes:     h.store.GetRecoveryProbeIntervalMinutes(),
		ProxyURL:                         h.store.GetProxyURL(),
		PgMaxConns:                       h.pgMaxConns,
		RedisPoolSize:                    h.redisPoolSize,
		AutoCleanUnauthorized:            h.store.GetAutoCleanUnauthorized(),
		AutoCleanRateLimited:             h.store.GetAutoCleanRateLimited(),
		AdminSecret:                      adminSecretForDisplay,
		AdminAuthSource:                  adminAuthSource,
		AutoCleanFullUsage:               h.store.GetAutoCleanFullUsage(),
		AutoCleanFullUsageMode:           h.store.GetAutoCleanFullUsageMode(),
		AutoCleanError:                   h.store.GetAutoCleanError(),
		AutoCleanExpired:                 h.store.GetAutoCleanExpired(),
		ProxyPoolEnabled:                 h.store.GetProxyPoolEnabled(),
		FastSchedulerEnabled:             h.store.FastSchedulerEnabled(),
		PlusPortEnabled:                  h.store.GetPlusPortEnabled(),
		PlusPortAccessFree:               h.store.GetPlusPortAccessFree(),
		SchedulerPreferredPlan:           h.store.GetPreferredPlanType(),
		SchedulerPlanBonus:               h.store.GetPreferredPlanBonus(),
		QuotaRatePlus:                    quotaRatePlus,
		QuotaRatePro:                     quotaRatePro,
		QuotaRateTeam:                    quotaRateTeam,
		MaxRetries:                       h.store.GetMaxRetries(),
		AllowRemoteMigration:             h.store.GetAllowRemoteMigration() && adminAuthSource != "disabled",
		PublicInitialCreditUSD:           h.store.GetPublicInitialCreditUSD(),
		PublicFullCreditUSD:              h.store.GetPublicFullCreditUSD(),
		DatabaseDriver:                   h.databaseDriver,
		DatabaseLabel:                    h.databaseLabel,
		CacheDriver:                      h.cacheDriver,
		CacheLabel:                       h.cacheLabel,
		ExpiredCleaned:                   expiredCleaned,
		ModelMapping:                     h.store.GetModelMapping(),
	})
}

// ==================== 导出 & 迁移 ====================

type cpaExportEntry struct {
	Type         string `json:"type"`
	Email        string `json:"email"`
	Expired      string `json:"expired"`
	IDToken      string `json:"id_token"`
	AccountID    string `json:"account_id"`
	AccessToken  string `json:"access_token"`
	LastRefresh  string `json:"last_refresh"`
	RefreshToken string `json:"refresh_token"`
}

// ExportAccounts 导出账号（CPA JSON 格式）
func (h *Handler) ExportAccounts(c *gin.Context) {
	filter := c.DefaultQuery("filter", "healthy")
	idsParam := c.Query("ids")
	remote := c.Query("remote")

	// 远程调用需检查 allow_remote_migration
	if remote == "true" {
		if !h.hasConfiguredAdminSecret(c.Request.Context()) {
			writeError(c, http.StatusForbidden, "请先设置管理密钥，再启用远程迁移")
			return
		}
		if !h.store.GetAllowRemoteMigration() {
			writeError(c, http.StatusForbidden, "远程迁移未启用，请在系统设置中开启")
			return
		}
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	var rows []*database.AccountRow
	var err error
	if filter == "healthy" {
		rows, err = h.db.ListActive(ctx)
	} else {
		rows, err = h.db.ListAll(ctx)
	}
	if err != nil {
		writeError(c, http.StatusInternalServerError, "查询账号失败: "+err.Error())
		return
	}

	// 按指定 ID 过滤
	var idSet map[int64]bool
	if idsParam != "" {
		idSet = make(map[int64]bool)
		for _, s := range strings.Split(idsParam, ",") {
			if id, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64); err == nil {
				idSet[id] = true
			}
		}
	}

	// 构建运行时状态映射（用于健康过滤）
	runtimeMap := make(map[int64]*auth.Account)
	if filter == "healthy" {
		for _, acc := range h.store.Accounts() {
			runtimeMap[acc.DBID] = acc
		}
	}

	var entries []cpaExportEntry
	for _, row := range rows {
		if idSet != nil && !idSet[row.ID] {
			continue
		}
		if filter == "healthy" {
			acc, ok := runtimeMap[row.ID]
			if !ok || acc.RuntimeStatus() != "active" {
				continue
			}
		}
		rt := row.GetCredential("refresh_token")
		at := row.GetCredential("access_token")
		if rt == "" && at == "" {
			continue
		}
		entries = append(entries, cpaExportEntry{
			Type:         "codex",
			Email:        row.GetCredential("email"),
			Expired:      row.GetCredential("expires_at"),
			IDToken:      row.GetCredential("id_token"),
			AccountID:    row.GetCredential("account_id"),
			AccessToken:  at,
			LastRefresh:  row.UpdatedAt.Format(time.RFC3339),
			RefreshToken: rt,
		})
	}

	if entries == nil {
		entries = []cpaExportEntry{}
	}
	c.JSON(http.StatusOK, entries)
}

type migrateReq struct {
	URL      string `json:"url"`
	AdminKey string `json:"admin_key"`
}

// MigrateAccounts 从远程 codex2api 实例迁移健康账号（SSE 流式进度）
func (h *Handler) MigrateAccounts(c *gin.Context) {
	if !h.hasConfiguredAdminSecret(c.Request.Context()) {
		writeError(c, http.StatusForbidden, "请先设置管理密钥，再使用远程迁移")
		return
	}

	var req migrateReq
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "请求格式错误")
		return
	}
	if req.URL == "" || req.AdminKey == "" {
		writeError(c, http.StatusBadRequest, "url 和 admin_key 是必填字段")
		return
	}

	remoteURL := strings.TrimRight(req.URL, "/")
	exportURL := remoteURL + "/api/admin/accounts/export?filter=healthy&remote=true"

	fetchCtx, fetchCancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer fetchCancel()

	httpReq, err := http.NewRequestWithContext(fetchCtx, http.MethodGet, exportURL, nil)
	if err != nil {
		writeError(c, http.StatusBadRequest, "构建请求失败: "+err.Error())
		return
	}
	httpReq.Header.Set("X-Admin-Key", req.AdminKey)
	httpReq.Header.Set("Authorization", "Bearer "+req.AdminKey)

	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(httpReq)
	if err != nil {
		writeError(c, http.StatusBadGateway, "连接远程实例失败: "+err.Error())
		return
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		if ok, errClip := h.tryCliproxyMigration(c, remoteURL, req.AdminKey); ok {
			if errClip == nil {
				return
			}
			writeError(c, http.StatusBadGateway, fmt.Sprintf("远程实例返回错误 (%d): %s; cliproxy 迁移失败: %v", resp.StatusCode, string(body), errClip))
			return
		}
		writeError(c, http.StatusBadGateway, fmt.Sprintf("远程实例返回错误 (%d): %s", resp.StatusCode, string(body)))
		return
	}

	var remoteAccounts []cpaExportEntry
	if err := json.Unmarshal(body, &remoteAccounts); err != nil {
		writeError(c, http.StatusBadGateway, "解析远程数据失败: "+err.Error())
		return
	}

	if len(remoteAccounts) == 0 {
		c.JSON(http.StatusOK, gin.H{"message": "远程实例没有可迁移的健康账号", "total": 0, "imported": 0, "duplicate": 0, "failed": 0})
		return
	}

	// 转换为 importToken 格式，复用 importAccountsCommon
	var tokens []importToken
	for _, entry := range remoteAccounts {
		rt := strings.TrimSpace(entry.RefreshToken)
		if rt == "" {
			continue
		}
		name := entry.Email
		if name == "" {
			name = "migrate"
		}
		tokens = append(tokens, importToken{
			refreshToken: rt,
			accessToken:  strings.TrimSpace(entry.AccessToken),
			name:         name,
			email:        strings.TrimSpace(entry.Email),
			idToken:      strings.TrimSpace(entry.IDToken),
			accountID:    strings.TrimSpace(entry.AccountID),
			expiresAt:    strings.TrimSpace(entry.Expired),
		})
	}

	log.Printf("远程迁移: 从 %s 拉取到 %d 个账号，开始导入", remoteURL, len(tokens))
	h.importAccountsCommon(c, tokens, "")
}

type compatImportItem struct {
	name  string
	entry compatEntry
}

func (h *Handler) tryCliproxyMigration(c *gin.Context, remoteURL, adminKey string) (bool, error) {
	listURL := remoteURL + "/v0/management/auth-files"
	fetchCtx, fetchCancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer fetchCancel()

	listReq, err := http.NewRequestWithContext(fetchCtx, http.MethodGet, listURL, nil)
	if err != nil {
		return true, fmt.Errorf("构建 cliproxy 请求失败: %w", err)
	}
	setCliproxyHeaders(listReq, adminKey)

	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(listReq)
	if err != nil {
		return true, fmt.Errorf("连接 cliproxy 失败: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return true, fmt.Errorf("cliproxy 返回错误 (%d): %s", resp.StatusCode, string(body))
	}

	var listResp struct {
		Files []map[string]interface{} `json:"files"`
	}
	if err := json.Unmarshal(body, &listResp); err != nil {
		return true, fmt.Errorf("解析 cliproxy 列表失败: %w", err)
	}
	if len(listResp.Files) == 0 {
		c.JSON(http.StatusOK, gin.H{"message": "cliproxy 没有可迁移账号", "total": 0, "imported": 0, "duplicate": 0, "failed": 0})
		return true, nil
	}

	items := make([]compatImportItem, 0)
	skipped := 0
	for _, file := range listResp.Files {
		if !isCliproxyCodexEntry(file) {
			continue
		}
		name := cliproxyFileName(file)
		if name == "" {
			skipped++
			continue
		}

		downloadURL := remoteURL + "/v0/management/auth-files/download?name=" + url.QueryEscape(name)
		fileReq, err := http.NewRequestWithContext(fetchCtx, http.MethodGet, downloadURL, nil)
		if err != nil {
			skipped++
			continue
		}
		setCliproxyHeaders(fileReq, adminKey)

		fileResp, err := (&http.Client{Timeout: 60 * time.Second}).Do(fileReq)
		if err != nil {
			skipped++
			continue
		}
		data, _ := io.ReadAll(fileResp.Body)
		fileResp.Body.Close()

		if fileResp.StatusCode != http.StatusOK {
			skipped++
			continue
		}

		entries, err := parseCompatEntries(data)
		if err != nil {
			skipped++
			continue
		}
		for idx, entry := range entries {
			parsed := parseCompatEntry(entry)
			if parsed.refreshToken == "" && parsed.accessToken == "" {
				continue
			}
			itemName := buildCompatName(name, parsed.email, idx, len(entries))
			items = append(items, compatImportItem{name: itemName, entry: parsed})
		}
	}

	if len(items) == 0 {
		return true, fmt.Errorf("cliproxy 没有可迁移账号 (已跳过 %d 个文件)", skipped)
	}

	log.Printf("远程迁移(cliproxy): 从 %s 拉取到 %d 个账号，开始导入", remoteURL, len(items))
	h.importCompatAccountsCommon(c, items)
	return true, nil
}

func setCliproxyHeaders(req *http.Request, adminKey string) {
	if req == nil {
		return
	}
	if adminKey != "" {
		req.Header.Set("Authorization", "Bearer "+adminKey)
		req.Header.Set("X-Management-Key", adminKey)
	}
}

func cliproxyFileName(entry map[string]interface{}) string {
	name := strings.TrimSpace(compatString(entry, "name", "label", "account", "id"))
	if name == "" {
		return ""
	}
	lower := strings.ToLower(name)
	if !strings.HasSuffix(lower, ".json") {
		name = name + ".json"
	}
	return name
}

func isCliproxyCodexEntry(entry map[string]interface{}) bool {
	provider := strings.ToLower(strings.TrimSpace(compatString(entry, "provider")))
	typ := strings.ToLower(strings.TrimSpace(compatString(entry, "type")))
	if provider != "" && !strings.Contains(provider, "codex") {
		return false
	}
	if typ != "" && !strings.Contains(typ, "codex") {
		return false
	}
	return true
}

func (h *Handler) importCompatAccountsCommon(c *gin.Context, items []compatImportItem) {
	seenRT := make(map[string]bool)
	seenAT := make(map[string]bool)
	var unique []compatImportItem
	for _, item := range items {
		rt := strings.TrimSpace(item.entry.refreshToken)
		at := strings.TrimSpace(item.entry.accessToken)
		if rt == "" && at == "" {
			continue
		}
		if rt != "" {
			if seenRT[rt] {
				continue
			}
			seenRT[rt] = true
		} else if at != "" {
			if seenAT[at] {
				continue
			}
			seenAT[at] = true
		}
		unique = append(unique, item)
	}

	total := len(unique)
	if total == 0 {
		c.JSON(http.StatusOK, gin.H{
			"message":   "没有可迁移账号",
			"success":   0,
			"duplicate": 0,
			"failed":    0,
			"total":     0,
		})
		return
	}

	dedupeCtx, dedupeCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer dedupeCancel()
	existingRTs, err := h.db.GetAllRefreshTokens(dedupeCtx)
	if err != nil {
		log.Printf("查询已有 RT 失败: %v", err)
		existingRTs = make(map[string]bool)
	}
	existingATs, err := h.db.GetAllAccessTokens(dedupeCtx)
	if err != nil {
		log.Printf("查询已有 AT 失败: %v", err)
		existingATs = make(map[string]bool)
	}

	var newItems []compatImportItem
	duplicateCount := 0
	for _, item := range unique {
		rt := strings.TrimSpace(item.entry.refreshToken)
		at := strings.TrimSpace(item.entry.accessToken)
		if rt != "" {
			if existingRTs[rt] {
				duplicateCount++
				continue
			}
		} else if at != "" {
			if existingATs[at] {
				duplicateCount++
				continue
			}
		}
		newItems = append(newItems, item)
	}

	if len(newItems) == 0 {
		c.JSON(http.StatusOK, gin.H{
			"message":   fmt.Sprintf("所有 %d 个账号已存在，无需导入", total),
			"success":   0,
			"duplicate": duplicateCount,
			"failed":    0,
			"total":     total,
		})
		return
	}

	setupSSE(c)

	var successCount int64
	var failCount int64
	var current int64
	sem := make(chan struct{}, 20)
	var wg sync.WaitGroup

	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				cur := int(atomic.LoadInt64(&current))
				suc := int(atomic.LoadInt64(&successCount))
				fai := int(atomic.LoadInt64(&failCount))
				sendImportEvent(c, importEvent{
					Type: "progress", Current: cur + duplicateCount, Total: total,
					Success: suc, Duplicate: duplicateCount, Failed: fai,
				})
			case <-done:
				return
			}
		}
	}()

	for i, item := range newItems {
		sem <- struct{}{}
		wg.Add(1)
		go func(idx int, it compatImportItem) {
			defer wg.Done()
			defer func() { <-sem }()

			name := it.name
			if name == "" {
				name = fmt.Sprintf("migrate-%d", idx+1)
			}

			insertCtx, insertCancel := context.WithTimeout(context.Background(), 10*time.Second)
			_, err := h.insertCompatAccount(insertCtx, name, it.entry, nil)
			insertCancel()

			if err != nil {
				log.Printf("迁移账号 %d/%d 失败: %v", idx+1, len(newItems), err)
				atomic.AddInt64(&failCount, 1)
				atomic.AddInt64(&current, 1)
				return
			}

			atomic.AddInt64(&successCount, 1)
			atomic.AddInt64(&current, 1)
		}(i, item)
	}

	wg.Wait()
	close(done)

	suc := int(atomic.LoadInt64(&successCount))
	fai := int(atomic.LoadInt64(&failCount))
	sendImportEvent(c, importEvent{
		Type: "complete", Current: total, Total: total,
		Success: suc, Duplicate: duplicateCount, Failed: fai,
	})

	log.Printf("迁移完成(cliproxy): success=%d, duplicate=%d, failed=%d, total=%d", suc, duplicateCount, fai, total)
}

// ==================== Models ====================

// ListModels 返回支持的模型列表（供前端设置页使用）
func (h *Handler) ListModels(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"models": proxy.SupportedModels})
}

// ==================== 账号趋势 ====================

// GetAccountEventTrend 获取账号增删趋势聚合数据
func (h *Handler) GetAccountEventTrend(c *gin.Context) {
	startStr := c.Query("start")
	endStr := c.Query("end")
	if startStr == "" || endStr == "" {
		writeError(c, http.StatusBadRequest, "start 和 end 参数为必填")
		return
	}

	start, err := time.Parse(time.RFC3339, startStr)
	if err != nil {
		writeError(c, http.StatusBadRequest, "start 时间格式无效（需 RFC3339）")
		return
	}
	end, err := time.Parse(time.RFC3339, endStr)
	if err != nil {
		writeError(c, http.StatusBadRequest, "end 时间格式无效（需 RFC3339）")
		return
	}

	bucketMinutes := 60
	if bStr := c.Query("bucket_minutes"); bStr != "" {
		if b, err := strconv.Atoi(bStr); err == nil && b > 0 {
			bucketMinutes = b
		}
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	trend, err := h.db.GetAccountEventTrend(ctx, start, end, bucketMinutes)
	if err != nil {
		writeInternalError(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"trend": trend})
}

// ==================== 清理 ====================

// CleanBanned 清理封禁（unauthorized）账号
func (h *Handler) CleanBanned(c *gin.Context) {
	h.cleanByStatus(c, "unauthorized")
}

// CleanRateLimited 清理限流（rate_limited）账号
func (h *Handler) CleanRateLimited(c *gin.Context) {
	h.cleanByStatus(c, "rate_limited")
}

// CleanError 清理错误（error）账号
func (h *Handler) CleanError(c *gin.Context) {
	h.cleanByStatus(c, "error")
}

// cleanByStatus 按运行时状态清理账号
func (h *Handler) cleanByStatus(c *gin.Context, targetStatus string) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()

	cleaned := h.store.CleanByRuntimeStatus(ctx, targetStatus)

	c.JSON(http.StatusOK, gin.H{"message": fmt.Sprintf("已清理 %d 个账号", cleaned), "cleaned": cleaned})
}

// ==================== Proxies ====================

// ListProxies 获取代理列表
func (h *Handler) ListProxies(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()

	proxies, err := h.db.ListProxies(ctx)
	if err != nil {
		writeError(c, http.StatusInternalServerError, "获取代理列表失败")
		return
	}
	if proxies == nil {
		proxies = []*database.ProxyRow{}
	}
	c.JSON(http.StatusOK, gin.H{"proxies": proxies})
}

// AddProxies 添加代理（支持批量）
func (h *Handler) AddProxies(c *gin.Context) {
	var req struct {
		URLs  []string `json:"urls"`
		URL   string   `json:"url"`
		Label string   `json:"label"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "请求格式错误")
		return
	}

	// 合并单条和批量
	urls := req.URLs
	if req.URL != "" {
		urls = append(urls, req.URL)
	}
	if len(urls) == 0 {
		writeError(c, http.StatusBadRequest, "请提供至少一个代理 URL")
		return
	}

	// 过滤空行
	cleaned := make([]string, 0, len(urls))
	for _, u := range urls {
		u = strings.TrimSpace(u)
		if u != "" {
			cleaned = append(cleaned, u)
		}
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	inserted, err := h.db.InsertProxies(ctx, cleaned, req.Label)
	if err != nil {
		writeError(c, http.StatusInternalServerError, "添加代理失败")
		return
	}

	// 刷新代理池
	_ = h.store.ReloadProxyPool()

	c.JSON(http.StatusOK, gin.H{
		"message":  fmt.Sprintf("成功添加 %d 个代理", inserted),
		"inserted": inserted,
		"total":    len(cleaned),
	})
}

// DeleteProxy 删除单个代理
func (h *Handler) DeleteProxy(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		writeError(c, http.StatusBadRequest, "无效的代理 ID")
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()

	if err := h.db.DeleteProxy(ctx, id); err != nil {
		writeError(c, http.StatusInternalServerError, "删除代理失败")
		return
	}

	_ = h.store.ReloadProxyPool()
	c.JSON(http.StatusOK, gin.H{"message": "代理已删除"})
}

// UpdateProxy 更新代理（启用/禁用/改标签）
func (h *Handler) UpdateProxy(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		writeError(c, http.StatusBadRequest, "无效的代理 ID")
		return
	}

	var req struct {
		Label   *string `json:"label"`
		Enabled *bool   `json:"enabled"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "请求格式错误")
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()

	if err := h.db.UpdateProxy(ctx, id, req.Label, req.Enabled); err != nil {
		writeError(c, http.StatusInternalServerError, "更新代理失败")
		return
	}

	_ = h.store.ReloadProxyPool()
	c.JSON(http.StatusOK, gin.H{"message": "代理已更新"})
}

// BatchDeleteProxies 批量删除代理
func (h *Handler) BatchDeleteProxies(c *gin.Context) {
	var req struct {
		IDs []int64 `json:"ids"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || len(req.IDs) == 0 {
		writeError(c, http.StatusBadRequest, "请提供要删除的代理 ID 列表")
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	deleted, err := h.db.DeleteProxies(ctx, req.IDs)
	if err != nil {
		writeError(c, http.StatusInternalServerError, "批量删除失败")
		return
	}

	_ = h.store.ReloadProxyPool()
	c.JSON(http.StatusOK, gin.H{"message": fmt.Sprintf("已删除 %d 个代理", deleted), "deleted": deleted})
}

// TestProxy 测试代理连通性与出口 IP 位置
func (h *Handler) TestProxy(c *gin.Context) {
	var req struct {
		URL  string `json:"url"`
		ID   int64  `json:"id"`
		Lang string `json:"lang"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.URL == "" {
		writeError(c, http.StatusBadRequest, "请提供代理 URL")
		return
	}

	// 创建使用指定代理的 HTTP client
	transport := &http.Transport{}
	baseDialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	transport.DialContext = baseDialer.DialContext
	if err := auth.ConfigureTransportProxy(transport, req.URL, baseDialer); err != nil {
		c.JSON(http.StatusOK, gin.H{"success": false, "error": fmt.Sprintf("代理 URL 格式错误: %v", err)})
		return
	}
	client := &http.Client{Transport: transport, Timeout: 15 * time.Second}

	apiLang := req.Lang
	if apiLang == "" {
		apiLang = "en"
	}
	start := time.Now()
	resp, err := client.Get(fmt.Sprintf("http://ip-api.com/json/?lang=%s&fields=status,message,country,regionName,city,isp,query", apiLang))
	latencyMs := int(time.Since(start).Milliseconds())

	if err != nil {
		c.JSON(http.StatusOK, gin.H{"success": false, "error": fmt.Sprintf("连接失败: %v", err), "latency_ms": latencyMs})
		return
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	result := gjson.ParseBytes(body)

	if result.Get("status").String() != "success" {
		c.JSON(http.StatusOK, gin.H{"success": false, "error": result.Get("message").String(), "latency_ms": latencyMs})
		return
	}

	ip := result.Get("query").String()
	country := result.Get("country").String()
	region := result.Get("regionName").String()
	city := result.Get("city").String()
	isp := result.Get("isp").String()
	location := country + "·" + region + "·" + city

	// 持久化测试结果
	if req.ID > 0 {
		ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
		defer cancel()
		_ = h.db.UpdateProxyTestResult(ctx, req.ID, ip, location, latencyMs)
	}

	c.JSON(http.StatusOK, gin.H{
		"success":    true,
		"ip":         ip,
		"country":    country,
		"region":     region,
		"city":       city,
		"isp":        isp,
		"latency_ms": latencyMs,
		"location":   location,
	})
}
