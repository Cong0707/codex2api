package admin

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/security"
	"github.com/gin-gonic/gin"
)

const publicAPIKeyContextKey = "public_api_key_auth"

// RegisterPublicRoutes 注册公开接口（不走管理员鉴权）
func (h *Handler) RegisterPublicRoutes(r *gin.Engine) {
	group := r.Group("/public")
	group.POST("/generate", h.PublicGenerateKey)
	group.POST("/redeem", h.publicAPIKeyAuthMiddleware(), h.PublicRedeem)
	group.GET("/key-info", h.publicAPIKeyAuthMiddleware(), h.PublicKeyInfo)
	group.POST("/key-info", h.publicAPIKeyAuthMiddleware(), h.PublicKeyInfo)
	group.GET("/quota-stats", h.PublicQuotaStats)
	group.POST("/quota-stats", h.PublicQuotaStats)
}

func extractPublicKeyFromRequest(c *gin.Context) string {
	if c == nil {
		return ""
	}
	candidates := []string{
		c.GetHeader("X-Public-Key"),
	}
	for _, raw := range candidates {
		key := strings.TrimSpace(security.SanitizeInput(raw))
		if key != "" {
			return key
		}
	}
	authHeader := strings.TrimSpace(c.GetHeader("Authorization"))
	if strings.HasPrefix(authHeader, "Bearer ") {
		return strings.TrimSpace(security.SanitizeInput(strings.TrimPrefix(authHeader, "Bearer ")))
	}
	return ""
}

func (h *Handler) publicAPIKeyAuthMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		readCtx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
		defer cancel()

		publicKeys, err := h.db.GetAllPublicAPIKeyValues(readCtx)
		if err != nil {
			writeError(c, http.StatusInternalServerError, "读取公开密钥失败")
			c.Abort()
			return
		}
		if len(publicKeys) == 0 {
			writeError(c, http.StatusForbidden, "未配置公开上传密钥")
			c.Abort()
			return
		}

		key := extractPublicKeyFromRequest(c)
		if key == "" || !strings.HasPrefix(key, "pk-") {
			writeError(c, http.StatusUnauthorized, "公开上传密钥无效或缺失")
			c.Abort()
			return
		}

		keyAuth, err := h.db.GetPublicAPIKeyByValue(readCtx, key)
		if err != nil {
			if err == sql.ErrNoRows {
				writeError(c, http.StatusUnauthorized, "公开上传密钥无效或缺失")
			} else {
				writeError(c, http.StatusInternalServerError, "读取公开密钥失败")
			}
			c.Abort()
			return
		}

		c.Set(publicAPIKeyContextKey, keyAuth)
		_ = h.db.TouchPublicAPIKeyUsage(readCtx, keyAuth.ID, c.ClientIP())
		c.Next()
	}
}

func (h *Handler) getPublicAPIKeyFromContext(c *gin.Context) (*database.PublicAPIKeyAuth, bool) {
	if c == nil {
		return nil, false
	}
	value, ok := c.Get(publicAPIKeyContextKey)
	if !ok {
		return nil, false
	}
	authKey, ok := value.(*database.PublicAPIKeyAuth)
	return authKey, ok && authKey != nil
}

type publicGenerateReq struct {
	Name string `json:"name"`
}

// PublicGenerateKey 公开创建上传 key（同 IP 每小时最多 3 个）
func (h *Handler) PublicGenerateKey(c *gin.Context) {
	var req publicGenerateReq
	_ = c.ShouldBindJSON(&req)

	name := strings.TrimSpace(security.SanitizeInput(req.Name))
	if name == "" {
		name = "public-upload"
	}
	if len(name) > 100 {
		writeError(c, http.StatusBadRequest, "名称长度不能超过100字符")
		return
	}

	clientIP := strings.TrimSpace(c.ClientIP())
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()

	count, err := h.db.CountRecentGeneratedPublicKeysByIP(ctx, clientIP, time.Now().Add(-1*time.Hour))
	if err != nil {
		writeInternalError(c, err)
		return
	}
	if count >= 3 {
		c.JSON(http.StatusTooManyRequests, gin.H{
			"error": "同一 IP 在 1 小时内最多生成 3 个公开密钥",
			"code":  "public_generate_rate_limited",
		})
		return
	}

	var (
		id  int64
		key string
	)
	for i := 0; i < 3; i++ {
		key = generatePublicKey()
		id, err = h.db.InsertPublicAPIKeyWithMeta(ctx, name, key, "public_generate", clientIP, 0)
		if err == nil {
			break
		}
	}
	if err != nil {
		writeError(c, http.StatusInternalServerError, "生成公开密钥失败: "+err.Error())
		return
	}

	security.SecurityAuditLog("PUBLIC_KEY_GENERATED", fmt.Sprintf("id=%d ip=%s", id, c.ClientIP()))
	c.JSON(http.StatusOK, gin.H{
		"id":          id,
		"name":        name,
		"key":         key,
		"balance_usd": 0,
		"created_at":  time.Now().Format(time.RFC3339),
	})
}

type publicRedeemReq struct {
	AmountUSD *float64 `json:"amount_usd"`
	Amount    *float64 `json:"amount"`
}

// PublicRedeem 使用公开 key 进行兑换
func (h *Handler) PublicRedeem(c *gin.Context) {
	keyAuth, ok := h.getPublicAPIKeyFromContext(c)
	if !ok || keyAuth.ID == 0 {
		writeError(c, http.StatusUnauthorized, "公开上传密钥无效或缺失")
		return
	}

	var req publicRedeemReq
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "请求格式错误")
		return
	}

	amount := 0.0
	if req.AmountUSD != nil {
		amount = *req.AmountUSD
	} else if req.Amount != nil {
		amount = *req.Amount
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	result, err := h.db.RedeemByPublicKey(ctx, keyAuth.ID, amount, c.ClientIP())
	if err != nil {
		if failure, ok := err.(*database.RedeemFailure); ok {
			status := http.StatusBadRequest
			switch failure.Kind {
			case "invalid_key":
				status = http.StatusUnauthorized
			case "no_code":
				status = http.StatusNotFound
			case "insufficient_balance":
				status = http.StatusPaymentRequired
			case "code_conflict":
				status = http.StatusConflict
			}
			c.JSON(status, gin.H{
				"error":                failure.Message,
				"code":                 failure.Kind,
				"requested_amount_usd": failure.RequestedAmount,
				"matched_amount_usd":   failure.MatchedAmount,
				"balance_usd":          failure.BalanceUSD,
			})
			return
		}
		writeInternalError(c, err)
		return
	}

	c.JSON(http.StatusOK, result)
}

// PublicKeyInfo 返回当前公开 key 的概览信息
func (h *Handler) PublicKeyInfo(c *gin.Context) {
	keyAuth, ok := h.getPublicAPIKeyFromContext(c)
	if !ok || keyAuth.ID == 0 {
		writeError(c, http.StatusUnauthorized, "公开上传密钥无效或缺失")
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()

	info, err := h.db.GetPublicAPIKeyOverview(ctx, keyAuth.ID)
	if err != nil {
		if err == sql.ErrNoRows {
			writeError(c, http.StatusUnauthorized, "公开上传密钥无效或缺失")
			return
		}
		writeInternalError(c, err)
		return
	}

	lastUsedAt := ""
	if info.LastUsedAt.Valid {
		lastUsedAt = info.LastUsedAt.Time.Format(time.RFC3339)
	}

	c.JSON(http.StatusOK, gin.H{
		"id":                  info.ID,
		"name":                info.Name,
		"source":              info.Source,
		"balance_usd":         info.BalanceUSD,
		"created_ip":          info.CreatedIP,
		"created_at":          info.CreatedAt.Format(time.RFC3339),
		"last_used_ip":        info.LastUsedIP,
		"last_used_at":        lastUsedAt,
		"bound_account_count": info.BoundAccountCount,
		"settled_amount_usd":  info.SettledAmountUSD,
	})
}

func parsePublicUsagePercent(raw string) float64 {
	value, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
		return 0
	}
	return clampPublicUsagePercent(value)
}

func clampPublicUsagePercent(value float64) float64 {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return 0
	}
	if value < 0 {
		return 0
	}
	if value > 100 {
		return 100
	}
	return value
}

func roundTo2(value float64) float64 {
	return math.Round(value*100) / 100
}

func deriveQuotaStatus(row *database.AccountRow, now time.Time) string {
	if row == nil {
		return ""
	}
	if strings.EqualFold(strings.TrimSpace(row.ErrorMessage), "deleted") {
		return "deleted"
	}
	if row.CooldownReason == "unauthorized" && row.CooldownUntil.Valid && row.CooldownUntil.Time.After(now) {
		return "unauthorized"
	}
	return row.Status
}

type publicQuotaRateConfig struct {
	plus   float64
	pro5x  float64
	pro20x float64
	team   float64
}

func (h *Handler) loadPublicQuotaRates(ctx context.Context) publicQuotaRateConfig {
	plus, pro5x, pro20x, team := defaultQuotaRates()
	if h == nil || h.db == nil {
		return publicQuotaRateConfig{plus: plus, pro5x: pro5x, pro20x: pro20x, team: team}
	}
	if settings, err := h.db.GetSystemSettings(ctx); err == nil && settings != nil {
		if normalized, ok := normalizeQuotaRate(settings.QuotaRatePlus, plus); ok {
			plus = normalized
		}
		if normalized, ok := normalizeQuotaRate(settings.QuotaRatePro5x, pro5x); ok {
			pro5x = normalized
		}
		if normalized, ok := normalizeQuotaRate(settings.QuotaRatePro20x, pro20x); ok {
			pro20x = normalized
		} else if normalized, ok := normalizeQuotaRate(settings.QuotaRatePro, pro20x); ok {
			pro20x = normalized
		}
		if normalized, ok := normalizeQuotaRate(settings.QuotaRateTeam, team); ok {
			team = normalized
		}
	}
	return publicQuotaRateConfig{plus: plus, pro5x: pro5x, pro20x: pro20x, team: team}
}

func publicQuotaWeight(plan string, rates publicQuotaRateConfig) float64 {
	switch auth.PlanVariant(plan) {
	case "pro_5x":
		return rates.pro5x
	case "pro_20x", "pro":
		return rates.pro20x
	}
	switch auth.NormalizePlanType(plan) {
	case "plus":
		return rates.plus
	case "team", "enterprise":
		return rates.team
	case "free":
		return 1
	default:
		return 1
	}
}

// PublicQuotaStats 返回账号页额度统计（口径与账号管理一致，排除 unauthorized 和 deleted）
func (h *Handler) PublicQuotaStats(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()

	rows, err := h.db.ListActive(ctx)
	if err != nil {
		writeInternalError(c, err)
		return
	}

	now := time.Now()
	rates := h.loadPublicQuotaRates(ctx)
	quotaAccountCount := 0
	quotaTotalFloat := 0.0
	usageSum := 0.0

	for _, row := range rows {
		status := deriveQuotaStatus(row, now)
		planType := row.GetCredential("plan_type")
		var runtimeUsage float64
		runtimeUsageValid := false
		if h.store != nil {
			if acc := h.store.FindByID(row.ID); acc != nil {
				status = acc.RuntimeStatus()
				planType = acc.GetPlanType()
				if usage, ok := acc.GetUsagePercent7d(); ok {
					runtimeUsage = clampPublicUsagePercent(usage)
					runtimeUsageValid = true
				}
			}
		}

		if status == "unauthorized" || status == "deleted" {
			continue
		}

		quotaAccountCount++

		weight := publicQuotaWeight(planType, rates)
		quotaTotalFloat += weight * 100
		usage := parsePublicUsagePercent(row.GetCredential("codex_7d_used_percent"))
		if runtimeUsageValid {
			usage = runtimeUsage
		}
		usageSum += weight * usage
	}

	quotaTotal := int(math.Round(quotaTotalFloat))
	quotaUsed := int(math.Round(usageSum))
	quotaRemaining := quotaTotal - quotaUsed
	if quotaRemaining < 0 {
		quotaRemaining = 0
	}

	quotaUsedPercent := 0.0
	quotaRemainingPercent := 0.0
	if quotaTotal > 0 {
		quotaUsedPercent = roundTo2((float64(quotaUsed) / float64(quotaTotal)) * 100)
		quotaRemainingPercent = roundTo2((float64(quotaRemaining) / float64(quotaTotal)) * 100)
	}

	c.JSON(http.StatusOK, gin.H{
		"quota_account_count":      quotaAccountCount,
		"quota_total":              quotaTotal,
		"quota_used":               quotaUsed,
		"quota_remaining":          quotaRemaining,
		"quota_used_percent":       quotaUsedPercent,
		"quota_remaining_percent":  quotaRemainingPercent,
		"quota_remaining_accounts": roundTo2(float64(quotaRemaining) / 100),
	})
}

type importRedeemCodesReq struct {
	AmountUSD *float64 `json:"amount_usd"`
	Amount    *float64 `json:"amount"`
	Codes     string   `json:"codes"`
	CodesText string   `json:"codes_text"`
}

// ImportRedeemCodes 管理员批量导入兑换码
func (h *Handler) ImportRedeemCodes(c *gin.Context) {
	var req importRedeemCodesReq
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "请求格式错误")
		return
	}

	amount := 0.0
	if req.AmountUSD != nil {
		amount = *req.AmountUSD
	} else if req.Amount != nil {
		amount = *req.Amount
	}
	if amount <= 0 {
		writeError(c, http.StatusBadRequest, "amount_usd 必须大于 0")
		return
	}

	raw := req.Codes
	if strings.TrimSpace(raw) == "" {
		raw = req.CodesText
	}
	lines := strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n")
	codes := database.NormalizeRedeemCodes(lines)
	if len(codes) == 0 {
		writeError(c, http.StatusBadRequest, "未解析到有效兑换码")
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	inserted, duplicates, err := h.db.ImportRedeemCodes(ctx, amount, codes)
	if err != nil {
		writeInternalError(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"amount_usd": amount,
		"inserted":   inserted,
		"duplicates": duplicates,
		"total":      len(codes),
	})
}

// ListRedeemCodeSummaries 返回兑换码面额汇总
func (h *Handler) ListRedeemCodeSummaries(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()

	items, err := h.db.ListRedeemCodeSummaries(ctx)
	if err != nil {
		writeInternalError(c, err)
		return
	}
	if items == nil {
		items = []database.RedeemCodeSummary{}
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}
