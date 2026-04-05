package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
)

const (
	openAIMeURL               = "https://chatgpt.com/backend-api/me"
	openAIWhamUsageURL        = "https://chatgpt.com/backend-api/wham/usage"
	openAIWhamAccountCheckURL = "https://chatgpt.com/backend-api/wham/accounts/check"
)

type cliproxyProfile struct {
	Email      string
	AccountID  string
	PlanType   string
	PlanSource string
}

type openAISnapshot struct {
	Endpoint   string
	Raw        []byte
	StatusCode int
	Err        error
	PlanType   string
	PlanSource string
}

// GetAccountRawInfo 获取账号一手信息：以 CPA/CLIProxyAPI 口径为主，原始数据返回 OpenAI 原生响应。
// GET /api/admin/accounts/:id/raw-info
func (h *Handler) GetAccountRawInfo(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		writeError(c, http.StatusBadRequest, "无效的账号 ID")
		return
	}

	account := h.store.FindByID(id)
	if account == nil {
		writeError(c, http.StatusNotFound, "账号不在运行时池中")
		return
	}

	refreshFn := h.refreshAccount
	if refreshFn == nil {
		refreshFn = h.refreshSingleAccount
	}

	account.Mu().RLock()
	hasAccessToken := strings.TrimSpace(account.AccessToken) != ""
	hasRefreshToken := strings.TrimSpace(account.RefreshToken) != ""
	currentPlan := strings.TrimSpace(account.PlanType)
	account.Mu().RUnlock()
	needsRefresh := account.NeedsRefresh()

	// 对齐 CPA：先尽量刷新到最新 token，再拉取账号信息。
	if (!hasAccessToken || needsRefresh) && hasRefreshToken {
		refreshCtx, cancel := context.WithTimeout(c.Request.Context(), 45*time.Second)
		defer cancel()
		if err := refreshFn(refreshCtx, id); err != nil {
			writeError(c, http.StatusInternalServerError, "刷新 Access Token 失败: "+err.Error())
			return
		}
	} else if !hasAccessToken {
		writeError(c, http.StatusBadRequest, "账号没有可用的 Access Token，且缺少 Refresh Token")
		return
	}

	account.Mu().RLock()
	accessToken := strings.TrimSpace(account.AccessToken)
	accountID := strings.TrimSpace(account.AccountID)
	accountProxy := strings.TrimSpace(account.ProxyURL)
	account.Mu().RUnlock()

	if accessToken == "" {
		writeError(c, http.StatusBadRequest, "账号没有可用的 Access Token，请先刷新")
		return
	}

	proxyURL := strings.TrimSpace(h.store.NextProxy())
	if proxyURL == "" {
		proxyURL = accountProxy
	}

	rowCtx, rowCancel := context.WithTimeout(c.Request.Context(), 8*time.Second)
	defer rowCancel()
	row, err := h.db.GetAccountByID(rowCtx, id)
	if err != nil {
		writeInternalError(c, fmt.Errorf("读取账号凭据失败: %w", err))
		return
	}

	profile := resolveCliproxyProfile(row)
	if !hasCliproxyProfile(profile) {
		profile = resolveRuntimeProfile(account)
	}

	reqCtx, reqCancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer reqCancel()
	snapshots := fetchOpenAISnapshots(reqCtx, accessToken, accountID, proxyURL)

	bestPlan, bestSource, bestRaw, bestEndpoint := pickBestPlanSnapshot(snapshots)
	if len(bestRaw) == 0 {
		if !hasCliproxyProfile(profile) {
			writeError(c, http.StatusBadGateway, firstSnapshotError(snapshots, "拉取 OpenAI 原始信息失败"))
			return
		}
	}

	snapshotFields, _ := extractCredentialUpdatesFromRawInfo(bestRaw)
	refreshedFields, credentialUpdates := mergeCredentialRefresh(profile, snapshotFields, currentPlan, bestPlan)
	credentialUpdates["raw_info_refreshed_at"] = time.Now().UTC().Format(time.RFC3339)

	dbCtx, dbCancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer dbCancel()
	if err := h.db.UpdateCredentials(dbCtx, id, credentialUpdates); err != nil {
		writeInternalError(c, fmt.Errorf("写入账号原始信息失败: %w", err))
		return
	}

	applyRawInfoToRuntimeAccount(account, refreshedFields)
	h.db.InsertAccountEventAsync(id, "raw_info_refreshed", "manual")

	if bestSource == "" {
		bestSource = profile.PlanSource
	}

	rawPayload := json.RawMessage([]byte(`{}`))
	if len(bestRaw) > 0 {
		rawPayload = json.RawMessage(bestRaw)
	}

	c.JSON(http.StatusOK, gin.H{
		"message":          "账号原始信息获取成功",
		"source":           "openai",
		"fetched_at":       time.Now().UTC().Format(time.RFC3339),
		"refreshed_fields": refreshedFields,
		"plan_source":      bestSource,
		"raw_endpoint":     bestEndpoint,
		"raw":              rawPayload,
	})
}

func fetchOpenAISnapshots(ctx context.Context, accessToken, accountID, proxyURL string) []openAISnapshot {
	steps := []struct {
		endpoint          string
		withAccountHeader bool
	}{
		{endpoint: openAIMeURL, withAccountHeader: true},
		{endpoint: openAIMeURL, withAccountHeader: false},
		{endpoint: openAIWhamUsageURL, withAccountHeader: true},
		{endpoint: openAIWhamAccountCheckURL, withAccountHeader: true},
		{endpoint: openAIWhamUsageURL, withAccountHeader: false},
		{endpoint: openAIWhamAccountCheckURL, withAccountHeader: false},
	}

	snapshots := make([]openAISnapshot, 0, len(steps))
	bestPlan := ""

	for _, step := range steps {
		if !step.withAccountHeader && strings.TrimSpace(accountID) == "" {
			continue
		}

		raw, statusCode, err := requestOpenAIEndpoint(ctx, step.endpoint, accessToken, accountID, proxyURL, step.withAccountHeader)
		snapshot := openAISnapshot{
			Endpoint:   step.endpoint,
			Raw:        raw,
			StatusCode: statusCode,
			Err:        err,
		}
		if err == nil {
			snapshot.PlanType, snapshot.PlanSource = detectPlanFromPayload(step.endpoint, raw)
			if isPlanBetter(bestPlan, snapshot.PlanType) {
				bestPlan = snapshot.PlanType
			}
		}
		snapshots = append(snapshots, snapshot)

		// 已拿到付费套餐信号时可以提前结束，减少额外上游请求。
		if strings.TrimSpace(bestPlan) != "" && auth.NormalizePlanType(bestPlan) != "free" {
			break
		}
	}

	return snapshots
}

func requestOpenAIEndpoint(ctx context.Context, endpoint, accessToken, accountID, proxyURL string, withAccountHeader bool) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("创建上游请求失败: %w", err)
	}

	profile := proxy.StableCodexClientProfile()
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(accessToken))
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Originator", proxy.Originator)
	req.Header.Set("X-Client-Request-Id", uuid.NewString())
	if strings.TrimSpace(profile.UserAgent) != "" {
		req.Header.Set("User-Agent", profile.UserAgent)
	}
	if strings.TrimSpace(profile.Version) != "" {
		req.Header.Set("Version", profile.Version)
	}
	if withAccountHeader && strings.TrimSpace(accountID) != "" {
		req.Header.Set("ChatGPT-Account-Id", strings.TrimSpace(accountID))
	}

	client := newWhamClient(proxyURL)
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("请求上游失败: %w", err)
	}
	defer resp.Body.Close()

	rawBody, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("读取上游响应失败: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return rawBody, resp.StatusCode, fmt.Errorf("上游返回 %d: %s", resp.StatusCode, truncate(string(rawBody), 500))
	}
	return rawBody, resp.StatusCode, nil
}

func newWhamClient(proxyURL string) *http.Client {
	transport := cloneHTTPTransport()
	transport.Proxy = nil
	transport.ForceAttemptHTTP2 = true

	baseDialer := &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
	}
	transport.DialContext = baseDialer.DialContext

	if strings.TrimSpace(proxyURL) != "" {
		if err := auth.ConfigureTransportProxy(transport, proxyURL, baseDialer); err != nil {
			log.Printf("配置账号原始信息请求代理失败，回退直连: %v", err)
			transport.Proxy = nil
			transport.DialContext = baseDialer.DialContext
		}
	}

	return &http.Client{
		Transport: transport,
		Timeout:   30 * time.Second,
	}
}

func cloneHTTPTransport() *http.Transport {
	if base, ok := http.DefaultTransport.(*http.Transport); ok && base != nil {
		return base.Clone()
	}
	return &http.Transport{}
}

func resolveCliproxyProfile(row *database.AccountRow) cliproxyProfile {
	if row == nil {
		return cliproxyProfile{}
	}

	result := cliproxyProfile{}
	credEmail := strings.TrimSpace(row.GetCredential("email"))
	credAccountID := strings.TrimSpace(row.GetCredential("account_id"))

	if credEmail != "" {
		result.Email = credEmail
	}
	if credAccountID != "" {
		result.AccountID = credAccountID
	}

	plan := ""
	planSource := ""
	applyPlan := func(candidate string, source string) {
		candidate = auth.NormalizePlanType(candidate)
		if candidate == "" {
			return
		}
		if isPlanBetter(plan, candidate) {
			plan = candidate
			planSource = source
		}
	}

	if credPlan := strings.TrimSpace(row.GetCredential("plan_type")); credPlan != "" {
		applyPlan(credPlan, "credentials.plan_type")
	}
	if credPlan := strings.TrimSpace(row.GetCredential("chatgpt_plan_type")); credPlan != "" {
		applyPlan(credPlan, "credentials.chatgpt_plan_type")
	}

	if accessToken := strings.TrimSpace(row.GetCredential("access_token")); accessToken != "" {
		if info := auth.ParseAccessToken(accessToken); info != nil {
			if result.Email == "" && strings.TrimSpace(info.Email) != "" {
				result.Email = strings.TrimSpace(info.Email)
			}
			if result.AccountID == "" && strings.TrimSpace(info.ChatGPTAccountID) != "" {
				result.AccountID = strings.TrimSpace(info.ChatGPTAccountID)
			}
			applyPlan(info.PlanType, "access_token.chatgpt_plan_type")
		}
	}

	if idToken := strings.TrimSpace(row.GetCredential("id_token")); idToken != "" {
		if info := auth.ParseIDToken(idToken); info != nil {
			if strings.TrimSpace(info.Email) != "" {
				result.Email = strings.TrimSpace(info.Email)
			}
			if strings.TrimSpace(info.ChatGPTAccountID) != "" {
				result.AccountID = strings.TrimSpace(info.ChatGPTAccountID)
			}
			applyPlan(info.PlanType, "id_token.chatgpt_plan_type")
		}
	}

	result.PlanType = plan
	result.PlanSource = planSource
	return result
}

func resolveRuntimeProfile(account *auth.Account) cliproxyProfile {
	if account == nil {
		return cliproxyProfile{}
	}
	account.Mu().RLock()
	defer account.Mu().RUnlock()
	return cliproxyProfile{
		Email:      strings.TrimSpace(account.Email),
		AccountID:  strings.TrimSpace(account.AccountID),
		PlanType:   auth.NormalizePlanType(account.PlanType),
		PlanSource: "runtime.account",
	}
}

func hasCliproxyProfile(profile cliproxyProfile) bool {
	return strings.TrimSpace(profile.Email) != "" ||
		strings.TrimSpace(profile.AccountID) != "" ||
		strings.TrimSpace(profile.PlanType) != ""
}

func pickBestPlanSnapshot(snapshots []openAISnapshot) (plan string, source string, raw []byte, endpoint string) {
	for _, snapshot := range snapshots {
		// 优先记录首个有响应体的数据（即使是上游错误，也要给前端原样展示，避免出现空 {}）。
		if len(snapshot.Raw) > 0 && len(raw) == 0 {
			raw = snapshot.Raw
			endpoint = snapshot.Endpoint
			source = snapshot.PlanSource
		}
		if snapshot.Err != nil || len(snapshot.Raw) == 0 {
			continue
		}
		if isPlanBetter(plan, snapshot.PlanType) {
			plan = snapshot.PlanType
			source = snapshot.PlanSource
			raw = snapshot.Raw
			endpoint = snapshot.Endpoint
		}
	}
	return plan, source, raw, endpoint
}

func firstSnapshotError(snapshots []openAISnapshot, fallback string) string {
	for _, snapshot := range snapshots {
		if snapshot.Err != nil {
			return snapshot.Err.Error()
		}
	}
	return fallback
}

func detectPlanFromPayload(endpoint string, raw []byte) (string, string) {
	if len(raw) == 0 {
		return "", ""
	}
	var payload any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return "", ""
	}

	endpointLabel := openAIEndpointLabel(endpoint)
	bestPlan := ""
	bestSource := ""

	apply := func(candidatePlan string, suffix string) {
		candidatePlan = auth.NormalizePlanType(candidatePlan)
		if candidatePlan == "" {
			return
		}
		if isPlanBetter(bestPlan, candidatePlan) {
			bestPlan = candidatePlan
			if suffix == "" {
				bestSource = endpointLabel
			} else {
				bestSource = endpointLabel + "." + suffix
			}
		}
	}

	for _, candidate := range collectPlanCandidates(payload) {
		apply(candidate, "plan")
	}

	// 对齐 CPA：me 接口额外参考 org/workspace 与订阅布尔信号。
	if endpoint == openAIMeURL {
		if plan, ok := detectPlanFromMeOrgSettings(payload); ok {
			apply(plan, "org.workspace_plan_type")
		}
		if hasPaidSubscriptionSignal(payload) {
			apply("plus", "subscription_flag")
		}
	}

	return bestPlan, bestSource
}

func detectPlanFromMeOrgSettings(value any) (string, bool) {
	root, ok := value.(map[string]any)
	if !ok {
		return "", false
	}
	orgs, ok := root["orgs"].(map[string]any)
	if !ok {
		return "", false
	}
	items, ok := orgs["data"].([]any)
	if !ok {
		return "", false
	}

	best := ""
	for _, item := range items {
		org, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if settings, ok := org["settings"].(map[string]any); ok {
			if workspacePlan, ok := settings["workspace_plan_type"].(string); ok {
				if isPlanBetter(best, workspacePlan) {
					best = auth.NormalizePlanType(workspacePlan)
				}
			}
		}
		if orgPlan, ok := org["plan_type"].(string); ok {
			if isPlanBetter(best, orgPlan) {
				best = auth.NormalizePlanType(orgPlan)
			}
		}
	}
	if best == "" {
		return "", false
	}
	return best, true
}

func hasPaidSubscriptionSignal(value any) bool {
	root, ok := value.(map[string]any)
	if !ok {
		return false
	}
	for _, key := range []string{"has_paid_subscription", "has_active_subscription", "is_paid", "is_subscribed"} {
		if v, exists := root[key]; exists {
			if b, ok := v.(bool); ok && b {
				return true
			}
		}
	}
	return false
}

func collectPlanCandidates(value any) []string {
	keys := map[string]struct{}{
		"plan_type":           {},
		"plan":                {},
		"subscription_plan":   {},
		"subscription_tier":   {},
		"chatgpt_plan_type":   {},
		"tier":                {},
		"workspace_plan_type": {},
		"product":             {},
	}

	out := make([]string, 0, 8)
	var walk func(node any)
	walk = func(node any) {
		switch v := node.(type) {
		case map[string]any:
			for key, val := range v {
				if _, ok := keys[strings.ToLower(strings.TrimSpace(key))]; ok {
					if s, ok := val.(string); ok {
						s = strings.TrimSpace(s)
						if s != "" {
							out = append(out, s)
						}
					}
				}
				walk(val)
			}
		case []any:
			for _, item := range v {
				walk(item)
			}
		}
	}
	walk(value)
	return out
}

func openAIEndpointLabel(endpoint string) string {
	switch endpoint {
	case openAIMeURL:
		return "me"
	case openAIWhamUsageURL:
		return "wham_usage"
	case openAIWhamAccountCheckURL:
		return "wham_accounts_check"
	default:
		return endpoint
	}
}

func isPlanBetter(current, candidate string) bool {
	currentNorm := auth.NormalizePlanType(current)
	candidateNorm := auth.NormalizePlanType(candidate)
	if candidateNorm == "" {
		return false
	}
	if currentNorm == "" {
		return true
	}
	return auth.PreferPlanType(currentNorm, candidateNorm) == candidateNorm && currentNorm != candidateNorm
}

func mergeCredentialRefresh(profile cliproxyProfile, upstream map[string]string, currentPlan string, detectedPlan string) (map[string]string, map[string]interface{}) {
	refreshed := make(map[string]string, 3)
	updates := make(map[string]interface{}, 3)

	email := strings.TrimSpace(profile.Email)
	if email == "" {
		email = strings.TrimSpace(upstream["email"])
	}
	if email != "" {
		refreshed["email"] = email
		updates["email"] = email
	}

	accountID := strings.TrimSpace(profile.AccountID)
	if accountID == "" {
		accountID = strings.TrimSpace(upstream["account_id"])
	}
	if accountID != "" {
		refreshed["account_id"] = accountID
		updates["account_id"] = accountID
	}

	selectedPlan := auth.NormalizePlanType(strings.TrimSpace(currentPlan))
	if candidate := strings.TrimSpace(profile.PlanType); candidate != "" {
		selectedPlan = auth.PreferPlanType(selectedPlan, candidate)
	}
	if candidate := strings.TrimSpace(detectedPlan); candidate != "" {
		selectedPlan = auth.PreferPlanType(selectedPlan, candidate)
	}
	if candidate := strings.TrimSpace(upstream["plan_type"]); candidate != "" {
		selectedPlan = auth.PreferPlanType(selectedPlan, candidate)
	}
	if selectedPlan != "" {
		refreshed["plan_type"] = selectedPlan
		updates["plan_type"] = selectedPlan
	}

	return refreshed, updates
}

func extractCredentialUpdatesFromRawInfo(rawBody []byte) (map[string]string, map[string]interface{}) {
	if len(rawBody) == 0 {
		return map[string]string{}, map[string]interface{}{}
	}

	email := firstNonEmptyJSONValue(rawBody,
		"email",
		"user.email",
		"profile.email",
		"account.email",
		"data.email",
	)
	defaultAccountID := firstNonEmptyJSONValue(rawBody, "default_account_id")
	accountID := firstNonEmptyJSONValue(rawBody,
		"chatgpt_account_id",
		"account_id",
		"account.account_id",
		"account.chatgpt_account_id",
		"data.account_id",
	)
	if accountID == "" {
		accountID = defaultAccountID
	}
	if accountID == "" {
		accountID = firstNonEmptyJSONValue(rawBody, "accounts.0.id")
	}

	planTypeRaw := firstNonEmptyJSONValue(rawBody,
		"plan_type",
		"chatgpt_plan_type",
		"planType",
		"account.plan_type",
		"account.chatgpt_plan_type",
		"account.planType",
		"subscription.plan_type",
		"subscription.chatgpt_plan_type",
		"data.plan_type",
	)

	if planTypeRaw == "" && defaultAccountID != "" {
		escaped := escapeForGJSONLiteral(defaultAccountID)
		path := fmt.Sprintf(`accounts.#(id=="%s").plan_type`, escaped)
		planTypeRaw = strings.TrimSpace(gjson.GetBytes(rawBody, path).String())
	}
	if planTypeRaw == "" {
		planTypeRaw = firstNonEmptyJSONValue(rawBody, "accounts.0.plan_type")
	}

	refreshed := make(map[string]string, 3)
	updates := make(map[string]interface{}, 3)

	if strings.TrimSpace(email) != "" {
		refreshed["email"] = strings.TrimSpace(email)
		updates["email"] = strings.TrimSpace(email)
	}
	if strings.TrimSpace(accountID) != "" {
		refreshed["account_id"] = strings.TrimSpace(accountID)
		updates["account_id"] = strings.TrimSpace(accountID)
	}
	if strings.TrimSpace(planTypeRaw) != "" {
		normalizedPlan := auth.NormalizePlanType(planTypeRaw)
		refreshed["plan_type"] = normalizedPlan
		updates["plan_type"] = normalizedPlan
	}

	return refreshed, updates
}

func firstNonEmptyJSONValue(rawBody []byte, paths ...string) string {
	for _, path := range paths {
		value := strings.TrimSpace(gjson.GetBytes(rawBody, path).String())
		if value != "" {
			return value
		}
	}
	return ""
}

func escapeForGJSONLiteral(value string) string {
	replacer := strings.NewReplacer(`\\`, `\\\\`, `"`, `\\"`)
	return replacer.Replace(value)
}

func applyRawInfoToRuntimeAccount(account *auth.Account, refreshed map[string]string) {
	account.Mu().Lock()
	defer account.Mu().Unlock()

	if email := strings.TrimSpace(refreshed["email"]); email != "" {
		account.Email = email
	}
	if accountID := strings.TrimSpace(refreshed["account_id"]); accountID != "" {
		account.AccountID = accountID
	}
	if planType := strings.TrimSpace(refreshed["plan_type"]); planType != "" {
		account.PlanType = auth.NormalizePlanType(planType)
	}
}
