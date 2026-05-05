package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/codex2api/security"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// RegisterCliproxyRoutes registers minimal CLIProxyAPI-compatible management routes.
func (h *Handler) RegisterCliproxyRoutes(r *gin.Engine) {
	group := r.Group("/v0/management")
	group.GET("/auth-files", h.adminAuthMiddleware(), h.ListAuthFilesCompat)
	group.POST("/auth-files", h.cliproxyUploadAuthMiddleware(), h.UploadAuthFilesCompat)
	group.DELETE("/auth-files", h.adminAuthMiddleware(), h.DeleteAuthFilesCompat)
	group.POST("/api-call", h.adminAuthMiddleware(), h.ManagementAPICallCompat)
}

// cliproxyUploadAuthMiddleware:
// 1) 兼容管理员旧链路（X-Admin-Key / X-Management-Key / Authorization Bearer）
// 2) 兼容公开上传密钥（pk-...）
// 其它 /public 接口仍然只接受公开密钥，不接受管理员密钥。
func (h *Handler) cliproxyUploadAuthMiddleware() gin.HandlerFunc {
	publicKeyAuth := h.publicAPIKeyAuthMiddleware()
	return func(c *gin.Context) {
		adminSecret, _ := h.resolveAdminSecret(c.Request.Context())
		if adminSecret != "" {
			adminKey := strings.TrimSpace(c.GetHeader("X-Admin-Key"))
			if adminKey == "" {
				adminKey = strings.TrimSpace(c.GetHeader("X-Management-Key"))
			}
			if adminKey == "" {
				authHeader := strings.TrimSpace(c.GetHeader("Authorization"))
				if strings.HasPrefix(authHeader, "Bearer ") {
					adminKey = strings.TrimSpace(strings.TrimPrefix(authHeader, "Bearer "))
				}
			}
			adminKey = security.SanitizeInput(adminKey)
			if adminKey != "" && security.SecureCompare(adminKey, adminSecret) {
				c.Next()
				return
			}
		}
		// 管理员密钥未通过时，回退到公开上传密钥鉴权
		publicKeyAuth(c)
	}
}

// ListAuthFilesCompat returns a CLIProxyAPI-style auth files list backed by codex2api accounts.
func (h *Handler) ListAuthFilesCompat(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()

	rows, err := h.db.ListAll(ctx)
	if err != nil {
		writeError(c, http.StatusInternalServerError, "查询账号失败: "+err.Error())
		return
	}

	files := make([]gin.H, 0, len(rows))
	for _, row := range rows {
		if row == nil {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(row.ErrorMessage), "deleted") {
			continue
		}

		email := strings.TrimSpace(row.GetCredential("email"))
		accountID := strings.TrimSpace(firstNonEmpty(
			row.GetCredential("account_id"),
			row.GetCredential("chatgpt_account_id"),
		))

		name := normalizeCompatFileName(row.Name)
		if name == "" {
			if email != "" {
				name = normalizeCompatFileName(email + ".json")
			} else {
				name = fmt.Sprintf("account-%d.json", row.ID)
			}
		}

		entry := gin.H{
			"auth_index": fmt.Sprintf("%d", row.ID),
			"name":       name,
			"type":       "codex",
			"provider":   "codex",
		}

		if email != "" {
			entry["email"] = email
			entry["label"] = email
		}
		if accountID != "" {
			entry["account_id"] = accountID
			entry["account"] = accountID
		} else if email != "" {
			entry["account"] = email
		} else if row.Name != "" {
			entry["account"] = row.Name
			entry["label"] = row.Name
		}

		if !row.CreatedAt.IsZero() {
			entry["created_at"] = row.CreatedAt.Format(time.RFC3339)
		}
		if !row.UpdatedAt.IsZero() {
			entry["updated_at"] = row.UpdatedAt.Format(time.RFC3339)
			entry["modtime"] = row.UpdatedAt.Format(time.RFC3339)
		}

		files = append(files, entry)
	}

	sort.Slice(files, func(i, j int) bool {
		return strings.ToLower(files[i]["name"].(string)) < strings.ToLower(files[j]["name"].(string))
	})

	c.JSON(http.StatusOK, gin.H{"files": files})
}

// UploadAuthFilesCompat accepts CLIProxyAPI-style auth JSON (multipart or raw JSON).
func (h *Handler) UploadAuthFilesCompat(c *gin.Context) {
	ctx := c.Request.Context()
	var sourcePublicKeyID *int64
	if keyAuth, ok := h.getPublicAPIKeyFromContext(c); ok && keyAuth != nil && keyAuth.ID > 0 {
		keyID := keyAuth.ID
		sourcePublicKeyID = &keyID
	}

	var uploaded []string
	var failed []gin.H

	if strings.HasPrefix(c.ContentType(), "multipart/form-data") {
		form, err := c.MultipartForm()
		if err != nil {
			writeError(c, http.StatusBadRequest, "解析上传失败: "+err.Error())
			return
		}

		files := form.File["file"]
		if len(files) == 0 {
			writeError(c, http.StatusBadRequest, "未找到上传文件")
			return
		}

		for _, fh := range files {
			if fh == nil {
				continue
			}
			fileName := normalizeCompatFileName(fh.Filename)
			data, errRead := readMultipartFile(fh)
			if errRead != nil {
				failed = append(failed, gin.H{"name": fileName, "error": errRead.Error()})
				continue
			}
			names, errUpload := h.importCompatPayload(ctx, fileName, data, sourcePublicKeyID)
			if errUpload != nil {
				failed = append(failed, gin.H{"name": fileName, "error": errUpload.Error()})
				continue
			}
			uploaded = append(uploaded, names...)
		}
	} else {
		name := normalizeCompatFileName(c.Query("name"))
		data, err := io.ReadAll(c.Request.Body)
		if err != nil {
			writeError(c, http.StatusBadRequest, "读取请求体失败")
			return
		}
		names, errUpload := h.importCompatPayload(ctx, name, data, sourcePublicKeyID)
		if errUpload != nil {
			writeError(c, http.StatusBadRequest, errUpload.Error())
			return
		}
		uploaded = append(uploaded, names...)
	}

	if len(failed) > 0 {
		c.JSON(http.StatusOK, gin.H{
			"status":   "partial",
			"uploaded": len(uploaded),
			"files":    uploaded,
			"failed":   failed,
		})
		return
	}

	if len(uploaded) == 0 {
		writeError(c, http.StatusBadRequest, "未解析到有效账号")
		return
	}

	if len(uploaded) == 1 {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"status": "ok", "uploaded": len(uploaded), "files": uploaded})
}

// DeleteAuthFilesCompat deletes accounts by CLIProxyAPI-style name parameter.
func (h *Handler) DeleteAuthFilesCompat(c *gin.Context) {
	names := c.QueryArray("name")
	if len(names) == 0 {
		if single := strings.TrimSpace(c.Query("name")); single != "" {
			names = []string{single}
		}
	}
	if len(names) == 0 {
		writeError(c, http.StatusBadRequest, "name 参数缺失")
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	idsToDelete := make(map[int64]struct{})
	for _, rawName := range names {
		name := normalizeCompatFileName(rawName)
		if name == "" {
			continue
		}
		if id, ok := parseIDFromCompatName(name); ok {
			idsToDelete[id] = struct{}{}
			continue
		}
		if id, ok := parseIDFromCompatName(strings.TrimSuffix(name, ".json")); ok {
			idsToDelete[id] = struct{}{}
			continue
		}
	}

	if len(idsToDelete) == 0 {
		// Fallback: scan accounts by name/email
		rows, err := h.db.ListAll(ctx)
		if err != nil {
			writeError(c, http.StatusInternalServerError, "查询账号失败: "+err.Error())
			return
		}
		nameSet := make(map[string]struct{})
		for _, raw := range names {
			nameSet[strings.ToLower(strings.TrimSpace(raw))] = struct{}{}
			nameSet[strings.ToLower(normalizeCompatFileName(raw))] = struct{}{}
		}
		for _, row := range rows {
			if row == nil {
				continue
			}
			email := strings.TrimSpace(row.GetCredential("email"))
			candidates := []string{
				strings.ToLower(strings.TrimSpace(row.Name)),
				strings.ToLower(normalizeCompatFileName(row.Name)),
			}
			if email != "" {
				candidates = append(candidates, strings.ToLower(email), strings.ToLower(email+".json"))
			}
			for _, candidate := range candidates {
				if candidate == "" {
					continue
				}
				if _, ok := nameSet[candidate]; ok {
					idsToDelete[row.ID] = struct{}{}
					break
				}
			}
		}
	}

	if len(idsToDelete) == 0 {
		writeError(c, http.StatusNotFound, "未找到匹配账号")
		return
	}

	deleted := 0
	for id := range idsToDelete {
		if err := h.db.SetError(ctx, id, "deleted"); err != nil {
			log.Printf("删除账号 %d 失败: %v", id, err)
			continue
		}
		h.store.RemoveAccount(id)
		h.db.InsertAccountEventAsync(id, "deleted", "cliproxy")
		deleted++
	}

	c.JSON(http.StatusOK, gin.H{"status": "ok", "deleted": deleted})
}

// ManagementAPICallCompat executes a generic HTTP call with optional token substitution.
func (h *Handler) ManagementAPICallCompat(c *gin.Context) {
	var req compatAPICallRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "请求格式错误")
		return
	}

	method := strings.ToUpper(strings.TrimSpace(req.Method))
	if method == "" {
		writeError(c, http.StatusBadRequest, "缺少 method")
		return
	}

	rawURL := strings.TrimSpace(req.URL)
	if rawURL == "" {
		writeError(c, http.StatusBadRequest, "缺少 url")
		return
	}
	parsedURL, err := url.Parse(rawURL)
	if err != nil || parsedURL.Scheme == "" || parsedURL.Host == "" {
		writeError(c, http.StatusBadRequest, "url 无效")
		return
	}

	authIndex := firstNonEmptyPtr(req.AuthIndexSnake, req.AuthIndexCamel, req.AuthIndexPascal)
	var token string
	var account *auth.Account
	if authIndex != "" {
		if id, errParse := strconv.ParseInt(authIndex, 10, 64); errParse == nil {
			account = h.store.FindByID(id)
			token = resolveCompatToken(c.Request.Context(), h.store, id, account)
		}
	}

	headers := req.Header
	if headers == nil {
		headers = map[string]string{}
	}

	var hostOverride string
	for key, value := range headers {
		if strings.Contains(value, "$TOKEN$") {
			if token == "" {
				writeError(c, http.StatusBadRequest, "auth token not found")
				return
			}
			headers[key] = strings.ReplaceAll(value, "$TOKEN$", token)
		}
	}

	var bodyReader io.Reader
	if req.Data != "" {
		bodyReader = strings.NewReader(req.Data)
	}

	callCtx, cancel := context.WithTimeout(c.Request.Context(), 60*time.Second)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(callCtx, method, rawURL, bodyReader)
	if err != nil {
		writeError(c, http.StatusBadRequest, "构建请求失败")
		return
	}

	for key, value := range headers {
		if strings.EqualFold(key, "host") {
			hostOverride = strings.TrimSpace(value)
			continue
		}
		httpReq.Header.Set(key, value)
	}
	if hostOverride != "" {
		httpReq.Host = hostOverride
	}

	client := proxy.NewUTLSHttpClient("")
	if account != nil {
		account.Mu().RLock()
		proxyURL := account.ProxyURL
		account.Mu().RUnlock()
		if strings.TrimSpace(proxyURL) != "" {
			client = proxy.NewUTLSHttpClient(proxyURL)
		}
	}

	resp, err := client.Do(httpReq)
	if err != nil {
		writeError(c, http.StatusBadGateway, "请求失败")
		return
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		writeError(c, http.StatusBadGateway, "读取响应失败")
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"status_code": resp.StatusCode,
		"header":      resp.Header,
		"body":        string(bodyBytes),
	})
}

type compatAPICallRequest struct {
	AuthIndexSnake  *string           `json:"auth_index"`
	AuthIndexCamel  *string           `json:"authIndex"`
	AuthIndexPascal *string           `json:"AuthIndex"`
	Method          string            `json:"method"`
	URL             string            `json:"url"`
	Header          map[string]string `json:"header"`
	Data            string            `json:"data"`
}

func resolveCompatToken(ctx context.Context, store *auth.Store, id int64, account *auth.Account) string {
	if store == nil || id == 0 {
		return ""
	}
	if account == nil {
		account = store.FindByID(id)
	}
	if account == nil {
		return ""
	}

	account.Mu().RLock()
	token := strings.TrimSpace(account.AccessToken)
	hasRT := strings.TrimSpace(account.RefreshToken) != ""
	account.Mu().RUnlock()

	if token != "" || !hasRT {
		return token
	}

	refreshCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := store.RefreshSingle(refreshCtx, id); err != nil {
		return ""
	}

	account = store.FindByID(id)
	if account == nil {
		return ""
	}
	account.Mu().RLock()
	defer account.Mu().RUnlock()
	return strings.TrimSpace(account.AccessToken)
}

func readMultipartFile(header *multipart.FileHeader) ([]byte, error) {
	if header == nil {
		return nil, errors.New("empty file")
	}
	file, err := header.Open()
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return io.ReadAll(file)
}

func (h *Handler) importCompatPayload(ctx context.Context, nameHint string, data []byte, sourcePublicKeyID *int64) ([]string, error) {
	entries, err := parseCompatEntries(data)
	if err != nil {
		return nil, err
	}

	if len(entries) == 0 {
		return nil, errors.New("未解析到有效账号")
	}

	dedupeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	existingRTs, err := h.db.GetAllRefreshTokens(dedupeCtx)
	if err != nil {
		return nil, fmt.Errorf("读取已有 refresh_token 失败: %w", err)
	}
	existingATs, err := h.db.GetAllAccessTokens(dedupeCtx)
	if err != nil {
		return nil, fmt.Errorf("读取已有 access_token 失败: %w", err)
	}

	var uploaded []string
	duplicateCount := 0
	for idx, entry := range entries {
		parsed := parseCompatEntry(entry)
		refreshToken := strings.TrimSpace(parsed.refreshToken)
		accessToken := strings.TrimSpace(parsed.accessToken)
		if refreshToken == "" && accessToken == "" {
			continue
		}
		if isCompatTokenDuplicate(existingRTs, existingATs, refreshToken, accessToken) {
			duplicateCount++
			continue
		}
		markCompatTokenSeen(existingRTs, existingATs, refreshToken, accessToken)
		parsed.refreshToken = refreshToken
		parsed.accessToken = accessToken

		name := buildCompatName(nameHint, parsed.email, idx, len(entries))
		id, errInsert := h.insertCompatAccount(ctx, name, parsed, sourcePublicKeyID)
		if errInsert != nil {
			return uploaded, errInsert
		}
		uploaded = append(uploaded, name)
		log.Printf("[cliproxy] 账号已导入: id=%d name=%s email=%s", id, name, parsed.email)
	}

	if len(uploaded) == 0 {
		if duplicateCount > 0 {
			return nil, errors.New("上传账号均已存在（重复 token）")
		}
		return nil, errors.New("未解析到有效账号")
	}
	return uploaded, nil
}

func isCompatTokenDuplicate(existingRTs, existingATs map[string]bool, refreshToken, accessToken string) bool {
	if refreshToken != "" && existingRTs[refreshToken] {
		return true
	}
	if accessToken != "" && existingATs[accessToken] {
		return true
	}
	return false
}

func markCompatTokenSeen(existingRTs, existingATs map[string]bool, refreshToken, accessToken string) {
	if refreshToken != "" {
		existingRTs[refreshToken] = true
	}
	if accessToken != "" {
		existingATs[accessToken] = true
	}
}

type compatEntry struct {
	refreshToken string
	accessToken  string
	idToken      string
	email        string
	accountID    string
	planType     string
	expiresAt    time.Time
	expiresRaw   string
	proxyURL     string
}

func parseCompatEntry(entry map[string]interface{}) compatEntry {
	parsed := compatEntry{}
	parsed.refreshToken = compatString(entry, "refresh_token", "refreshToken")
	parsed.accessToken = compatString(entry, "access_token", "accessToken", "token")
	parsed.idToken = compatString(entry, "id_token", "idToken")
	parsed.email = compatString(entry, "email", "account_email", "accountEmail")
	parsed.accountID = compatString(entry, "account_id", "accountId", "chatgpt_account_id", "chatgptAccountId")
	parsed.planType = compatString(entry, "plan_type", "planType")
	parsed.proxyURL = compatString(entry, "proxy_url", "proxyUrl", "proxy")

	parsed.expiresRaw = compatString(entry, "expires_at", "expiresAt", "expired")
	if ts, ok := parseCompatTime(parsed.expiresRaw); ok {
		parsed.expiresAt = ts
	}

	if parsed.email == "" || parsed.accountID == "" || parsed.planType == "" || parsed.expiresAt.IsZero() {
		if parsed.accessToken != "" {
			if info := auth.ParseAccessToken(parsed.accessToken); info != nil {
				if parsed.email == "" {
					parsed.email = info.Email
				}
				if parsed.accountID == "" {
					parsed.accountID = info.ChatGPTAccountID
				}
				if parsed.planType == "" {
					parsed.planType = info.PlanType
				}
				if parsed.expiresAt.IsZero() {
					parsed.expiresAt = info.ExpiresAt
				}
			}
		}
	}

	if parsed.email == "" || parsed.accountID == "" || parsed.planType == "" {
		if parsed.idToken != "" {
			if info := auth.ParseIDToken(parsed.idToken); info != nil {
				if parsed.email == "" {
					parsed.email = info.Email
				}
				if parsed.accountID == "" {
					parsed.accountID = info.ChatGPTAccountID
				}
				if parsed.planType == "" {
					parsed.planType = info.PlanType
				}
			}
		}
	}
	parsed.planType = normalizeStoredPlanType(parsed.planType)

	return parsed
}

func (h *Handler) insertCompatAccount(ctx context.Context, name string, entry compatEntry, sourcePublicKeyID *int64) (int64, error) {
	if entry.refreshToken == "" && entry.accessToken == "" {
		return 0, errors.New("缺少 refresh_token 或 access_token")
	}

	cleanName := normalizeCompatFileName(name)
	if cleanName == "" {
		cleanName = fmt.Sprintf("account-%d.json", time.Now().UnixNano())
	}

	var id int64
	var err error
	if entry.refreshToken != "" {
		id, err = h.db.InsertAccount(ctx, cleanName, entry.refreshToken, entry.proxyURL)
	} else {
		id, err = h.db.InsertATAccount(ctx, cleanName, entry.accessToken, entry.proxyURL)
	}
	if err != nil {
		return 0, err
	}

	creds := map[string]interface{}{}
	if entry.refreshToken != "" {
		creds["refresh_token"] = entry.refreshToken
	}
	if entry.accessToken != "" {
		creds["access_token"] = entry.accessToken
	}
	if entry.idToken != "" {
		creds["id_token"] = entry.idToken
	}
	if entry.email != "" {
		creds["email"] = entry.email
	}
	if entry.accountID != "" {
		creds["account_id"] = entry.accountID
	}
	if !entry.expiresAt.IsZero() {
		creds["expires_at"] = entry.expiresAt.Format(time.RFC3339)
	} else if entry.expiresRaw != "" {
		creds["expires_at"] = entry.expiresRaw
	}
	if len(creds) > 0 {
		_ = h.db.UpdateCredentials(ctx, id, creds)
	}

	acc := &auth.Account{
		DBID:         id,
		RefreshToken: entry.refreshToken,
		AccessToken:  entry.accessToken,
		AccountID:    entry.accountID,
		Email:        entry.email,
		ProxyURL:     entry.proxyURL,
	}
	if sourcePublicKeyID != nil && *sourcePublicKeyID > 0 {
		// 公开上传账号先不绑定、不入账：必须先通过一次真实连通测试才算有效上传。
		acc.PublicAPIKeyID = 0
	}
	if !entry.expiresAt.IsZero() {
		acc.ExpiresAt = entry.expiresAt
	} else if entry.accessToken != "" {
		acc.ExpiresAt = time.Now().Add(1 * time.Hour)
	}

	h.store.AddAccount(acc)
	h.db.InsertAccountEventAsync(id, "added", "cliproxy")

	if sourcePublicKeyID != nil && *sourcePublicKeyID > 0 {
		validateCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
		defer cancel()
		if err := h.validatePublicUploadedAccount(validateCtx, id); err != nil {
			_ = h.db.SetError(context.Background(), id, "deleted")
			h.store.RemoveAccount(id)
			h.db.InsertAccountEventAsync(id, "upload_invalid", "cliproxy")
			return 0, fmt.Errorf("上传后测试失败，账号无效: %w", err)
		}

		initialCredit := 0.1
		fullCredit := 2.0
		if h.store != nil {
			initialCredit = h.store.GetPublicInitialCreditUSD()
			fullCredit = h.store.GetPublicFullCreditUSD()
		}
		if err := h.db.BindAccountToPublicKey(ctx, database.PublicSettlementBindInput{
			AccountID:            id,
			PublicAPIKeyID:       *sourcePublicKeyID,
			BaselineUsagePercent: 0,
			InitialAmountUSD:     initialCredit,
			FullAmountUSD:        fullCredit,
		}); err != nil {
			_ = h.db.SetError(context.Background(), id, "deleted")
			h.store.RemoveAccount(id)
			return 0, fmt.Errorf("账号绑定公开 key 失败: %w", err)
		}

		if bound := h.store.FindByID(id); bound != nil {
			bound.Mu().Lock()
			bound.PublicAPIKeyID = *sourcePublicKeyID
			bound.Mu().Unlock()
		}
		h.triggerForcedPlanSync(id, "cliproxy_public_upload")
		h.db.InsertAccountEventAsync(id, "upload_valid", "cliproxy")
	} else if entry.refreshToken != "" && entry.accessToken == "" {
		go func(accountID int64) {
			refreshCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := h.store.RefreshSingle(refreshCtx, accountID); err != nil {
				log.Printf("[cliproxy] 账号 %d 刷新失败: %v", accountID, err)
				return
			}
			h.triggerForcedPlanSync(accountID, "cliproxy_rt_upload")
		}(id)
	} else if entry.accessToken != "" {
		h.triggerForcedPlanSync(id, "cliproxy_upload")
	}

	return id, nil
}

func (h *Handler) validatePublicUploadedAccount(ctx context.Context, accountID int64) error {
	if h == nil || h.store == nil {
		return errors.New("服务未就绪")
	}
	account := h.store.FindByID(accountID)
	if account == nil {
		return errors.New("账号不在运行时池中")
	}

	account.Mu().RLock()
	hasToken := strings.TrimSpace(account.AccessToken) != ""
	needsRefresh := strings.TrimSpace(account.RefreshToken) != "" && !hasToken
	account.Mu().RUnlock()

	if needsRefresh {
		refreshCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
		defer cancel()
		if err := h.store.RefreshSingle(refreshCtx, accountID); err != nil {
			return fmt.Errorf("refresh 失败: %w", err)
		}
		account = h.store.FindByID(accountID)
		if account == nil {
			return errors.New("刷新后账号不在运行时池中")
		}
	}

	account.Mu().RLock()
	hasToken = strings.TrimSpace(account.AccessToken) != ""
	account.Mu().RUnlock()
	if !hasToken {
		return errors.New("账号没有可用的 Access Token")
	}

	testCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	testModel := h.store.GetTestModel()
	payload := buildTestPayload(testModel)
	proxyURL := h.store.ResolveProxyForAccount(account)
	resp, reqErr := proxy.ExecuteRequest(testCtx, account, payload, "", proxyURL, "", nil, nil)
	if reqErr != nil {
		return fmt.Errorf("测试请求失败: %w", reqErr)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		if usagePct, ok := proxy.ParseCodexUsageHeaders(resp, account); ok {
			h.store.PersistUsageSnapshot(account, usagePct)
		}
		errBody, _ := io.ReadAll(resp.Body)
		errCode, errMsg := proxy.ParseUpstreamErrorBrief(errBody)
		displayStatus := proxy.NormalizeUpstreamStatusCode(resp.StatusCode, errBody)
		account.SetLastFailureDetail(displayStatus, errCode, errMsg)
		switch {
		case proxy.IsUnauthorizedLikeStatus(resp.StatusCode, errBody):
			h.store.MarkCooldown(account, 24*time.Hour, "unauthorized")
		case resp.StatusCode == http.StatusTooManyRequests:
			if !h.store.MarkFullUsageCooldownFromSnapshot(account) {
				h.store.MarkCooldown(account, auth.RateLimitedProbeInterval, "rate_limited")
			}
		}
		return fmt.Errorf("上游返回 %d: %s", displayStatus, truncate(string(errBody), 500))
	}

	if usagePct, ok := proxy.ParseCodexUsageHeaders(resp, account); ok {
		h.store.PersistUsageSnapshot(account, usagePct)
	}

	hasContent := false
	completed := false
	streamErr := proxy.ReadSSEStream(resp.Body, func(data []byte) bool {
		eventType := gjson.GetBytes(data, "type").String()
		switch eventType {
		case "response.output_text.delta":
			if strings.TrimSpace(gjson.GetBytes(data, "delta").String()) != "" {
				hasContent = true
			}
		case "response.completed":
			completed = true
			return false
		case "response.failed":
			return false
		}
		return true
	})
	if streamErr != nil {
		return fmt.Errorf("读取测试响应失败: %w", streamErr)
	}
	if !completed {
		return errors.New("测试未完成（未收到 response.completed）")
	}
	if !hasContent {
		return errors.New("测试未收到模型输出")
	}

	account.ClearLastFailureDetail()
	if cooldownUntil, cooldownReason, active := account.GetCooldownSnapshot(); !(active && cooldownReason == "full_usage" && time.Now().Before(cooldownUntil)) {
		h.store.ClearCooldown(account)
	}
	return nil
}

func parseCompatEntries(data []byte) ([]map[string]interface{}, error) {
	if len(data) == 0 {
		return nil, errors.New("内容为空")
	}
	var raw interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, errors.New("JSON 解析失败")
	}
	switch v := raw.(type) {
	case []interface{}:
		result := make([]map[string]interface{}, 0, len(v))
		for _, item := range v {
			m, ok := item.(map[string]interface{})
			if ok {
				result = append(result, m)
			}
		}
		return result, nil
	case map[string]interface{}:
		return []map[string]interface{}{v}, nil
	default:
		return nil, errors.New("JSON 格式错误")
	}
}

func buildCompatName(hint, email string, idx, total int) string {
	name := normalizeCompatFileName(hint)
	if name == "" && email != "" {
		name = normalizeCompatFileName(email + ".json")
	}
	if name == "" {
		name = fmt.Sprintf("account-%d.json", time.Now().UnixNano())
	}
	if total > 1 {
		base := strings.TrimSuffix(name, ".json")
		if base == "" {
			base = "account"
		}
		name = fmt.Sprintf("%s-%d.json", base, idx+1)
	}
	return name
}

func compatString(entry map[string]interface{}, keys ...string) string {
	for _, key := range keys {
		if key == "" {
			continue
		}
		if v, ok := entry[key]; ok {
			switch val := v.(type) {
			case string:
				if s := strings.TrimSpace(val); s != "" {
					return s
				}
			case json.Number:
				if s := strings.TrimSpace(val.String()); s != "" {
					return s
				}
			case float64:
				return strconv.FormatInt(int64(val), 10)
			case int64:
				return strconv.FormatInt(val, 10)
			case int:
				return strconv.Itoa(val)
			}
		}
	}
	return ""
}

func parseCompatTime(value string) (time.Time, bool) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return time.Time{}, false
	}
	layouts := []string{
		time.RFC3339,
		time.RFC3339Nano,
		"2006-01-02 15:04:05",
		"2006-01-02T15:04:05Z07:00",
	}
	for _, layout := range layouts {
		if ts, err := time.Parse(layout, trimmed); err == nil {
			return ts, true
		}
	}
	if unix, err := strconv.ParseInt(trimmed, 10, 64); err == nil && unix > 0 {
		return time.Unix(unix, 0), true
	}
	return time.Time{}, false
}

func normalizeCompatFileName(name string) string {
	clean := strings.TrimSpace(security.SanitizeInput(name))
	if clean == "" {
		return ""
	}
	clean = filepath.Base(clean)
	clean = strings.ReplaceAll(clean, "\\", "")
	clean = strings.ReplaceAll(clean, "/", "")
	if !strings.HasSuffix(strings.ToLower(clean), ".json") {
		clean += ".json"
	}
	return security.SafeTruncate(clean, 200)
}

func parseIDFromCompatName(name string) (int64, bool) {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return 0, false
	}
	base := strings.TrimSuffix(trimmed, ".json")
	if id, err := strconv.ParseInt(base, 10, 64); err == nil {
		return id, true
	}
	lastDash := strings.LastIndex(base, "-")
	if lastDash == -1 {
		return 0, false
	}
	idPart := base[lastDash+1:]
	if idPart == "" {
		return 0, false
	}
	id, err := strconv.ParseInt(idPart, 10, 64)
	if err != nil {
		return 0, false
	}
	return id, true
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func firstNonEmptyPtr(values ...*string) string {
	for _, v := range values {
		if v == nil {
			continue
		}
		if s := strings.TrimSpace(*v); s != "" {
			return s
		}
	}
	return ""
}
