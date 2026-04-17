package admin

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	neturl "net/url"
	"strings"
	"sync"
	"time"

	"github.com/codex2api/auth"
	"github.com/gin-gonic/gin"
)

// ==================== OAuth 常量 ====================

const (
	oauthAuthorizeURL       = "https://auth.openai.com/oauth/authorize"
	oauthTokenURL           = "https://auth.openai.com/oauth/token"
	oauthClientID           = "app_EMoamEEZ73f0CkXaXp7hrann"
	oauthDefaultRedirectURI = "http://localhost:1455/auth/callback"
	oauthDefaultScopes      = "openid profile email offline_access"
	oauthSessionTTL         = 30 * time.Minute
)

// ==================== 内存 Session 存储 ====================

type oauthSession struct {
	State        string
	CodeVerifier string
	RedirectURI  string
	ProxyURL     string
	CreatedAt    time.Time
}

type oauthSessionStore struct {
	mu       sync.Mutex
	sessions map[string]*oauthSession
}

var globalOAuthStore = &oauthSessionStore{sessions: make(map[string]*oauthSession)}

func init() {
	go globalOAuthStore.cleanupLoop()
}

func (s *oauthSessionStore) set(id string, sess *oauthSession) {
	s.mu.Lock()
	s.sessions[id] = sess
	s.mu.Unlock()
}

func (s *oauthSessionStore) get(id string) (*oauthSession, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[id]
	if !ok || time.Since(sess.CreatedAt) > oauthSessionTTL {
		return nil, false
	}
	return sess, true
}

func (s *oauthSessionStore) delete(id string) {
	s.mu.Lock()
	delete(s.sessions, id)
	s.mu.Unlock()
}

func (s *oauthSessionStore) cleanupLoop() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		s.mu.Lock()
		for id, sess := range s.sessions {
			if time.Since(sess.CreatedAt) > oauthSessionTTL {
				delete(s.sessions, id)
			}
		}
		s.mu.Unlock()
	}
}

// ==================== PKCE 工具函数 ====================

func oauthRandomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func oauthCodeChallenge(verifier string) string {
	h := sha256.Sum256([]byte(verifier))
	return strings.TrimRight(base64.URLEncoding.EncodeToString(h[:]), "=")
}

// ==================== Handlers ====================

// GenerateOAuthURL 生成 Codex CLI PKCE OAuth 授权 URL
// POST /api/admin/oauth/generate-auth-url
func (h *Handler) GenerateOAuthURL(c *gin.Context) {
	var req struct {
		ProxyURL    string `json:"proxy_url"`
		RedirectURI string `json:"redirect_uri"`
	}
	_ = c.ShouldBindJSON(&req)

	redirectURI := strings.TrimSpace(req.RedirectURI)
	if redirectURI == "" {
		redirectURI = oauthDefaultRedirectURI
	}

	state, err := oauthRandomHex(32)
	if err != nil {
		writeError(c, http.StatusInternalServerError, "生成 state 失败")
		return
	}
	codeVerifier, err := oauthRandomHex(64)
	if err != nil {
		writeError(c, http.StatusInternalServerError, "生成 code_verifier 失败")
		return
	}
	sessionID, err := oauthRandomHex(16)
	if err != nil {
		writeError(c, http.StatusInternalServerError, "生成 session_id 失败")
		return
	}

	globalOAuthStore.set(sessionID, &oauthSession{
		State:        state,
		CodeVerifier: codeVerifier,
		RedirectURI:  redirectURI,
		ProxyURL:     strings.TrimSpace(req.ProxyURL),
		CreatedAt:    time.Now(),
	})

	params := neturl.Values{}
	params.Set("response_type", "code")
	params.Set("client_id", oauthClientID)
	params.Set("redirect_uri", redirectURI)
	params.Set("scope", oauthDefaultScopes)
	params.Set("state", state)
	params.Set("code_challenge", oauthCodeChallenge(codeVerifier))
	params.Set("code_challenge_method", "S256")
	params.Set("id_token_add_organizations", "true")
	params.Set("codex_cli_simplified_flow", "true")

	c.JSON(http.StatusOK, gin.H{
		"auth_url":   oauthAuthorizeURL + "?" + params.Encode(),
		"session_id": sessionID,
	})
}

// ExchangeOAuthCode 用授权码兑换 token，并写入新账号
// POST /api/admin/oauth/exchange-code
func (h *Handler) ExchangeOAuthCode(c *gin.Context) {
	var req struct {
		SessionID string `json:"session_id"`
		Code      string `json:"code"`
		State     string `json:"state"`
		Name      string `json:"name"`
		ProxyURL  string `json:"proxy_url"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "请求格式错误")
		return
	}
	if req.SessionID == "" || req.Code == "" || req.State == "" {
		writeError(c, http.StatusBadRequest, "session_id、code 和 state 均为必填")
		return
	}

	sess, ok := globalOAuthStore.get(req.SessionID)
	if !ok {
		writeError(c, http.StatusBadRequest, "OAuth 会话不存在或已过期（有效期 30 分钟）")
		return
	}
	if req.State != sess.State {
		writeError(c, http.StatusBadRequest, "state 不匹配，请重新发起授权")
		return
	}

	proxyURL := sess.ProxyURL
	if trimmed := strings.TrimSpace(req.ProxyURL); trimmed != "" {
		proxyURL = trimmed
	}

	tokenResp, accountInfo, err := doOAuthCodeExchange(c.Request.Context(), req.Code, sess.CodeVerifier, sess.RedirectURI, proxyURL)
	if err != nil {
		writeError(c, http.StatusBadGateway, "授权码兑换失败: "+err.Error())
		return
	}
	globalOAuthStore.delete(req.SessionID)

	if tokenResp.RefreshToken == "" {
		writeError(c, http.StatusBadGateway, "授权服务器未返回 refresh_token，请确认已开启 offline_access scope")
		return
	}

	name := strings.TrimSpace(req.Name)
	if name == "" && accountInfo != nil && accountInfo.Email != "" {
		name = accountInfo.Email
	}
	if name == "" {
		name = "oauth-account"
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()

	id, err := h.db.InsertAccount(ctx, name, tokenResp.RefreshToken, proxyURL)
	if err != nil {
		writeError(c, http.StatusInternalServerError, "账号写入数据库失败: "+err.Error())
		return
	}
	h.db.InsertAccountEventAsync(id, "added", "oauth")

	newAcc := &auth.Account{
		DBID:         id,
		RefreshToken: tokenResp.RefreshToken,
		ProxyURL:     proxyURL,
	}
	h.store.AddAccount(newAcc)

	go func(accountID int64) {
		refreshCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := h.store.RefreshSingle(refreshCtx, accountID); err != nil {
			log.Printf("OAuth 账号 %d AT 刷新失败: %v", accountID, err)
		} else {
			log.Printf("OAuth 账号 %d 已加入号池", accountID)
		}
		h.triggerForcedPlanSync(accountID, "oauth_add")
	}(id)

	email := ""
	planType := ""
	if accountInfo != nil {
		email = accountInfo.Email
		planType = accountInfo.PlanType
	}

	c.JSON(http.StatusOK, gin.H{
		"message":   fmt.Sprintf("OAuth 账号 %s 添加成功", name),
		"id":        id,
		"email":     email,
		"plan_type": planType,
	})
}

// ==================== 内部 HTTP 调用 ====================

type rawOAuthTokenResp struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	ExpiresIn    int64  `json:"expires_in"`
}

func doOAuthCodeExchange(ctx context.Context, code, codeVerifier, redirectURI, proxyURL string) (*rawOAuthTokenResp, *auth.AccountInfo, error) {
	form := neturl.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("client_id", oauthClientID)
	form.Set("code", code)
	form.Set("redirect_uri", redirectURI)
	form.Set("code_verifier", codeVerifier)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, oauthTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, nil, fmt.Errorf("创建请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "codex-cli/0.91.0")

	client := auth.BuildHTTPClient(proxyURL)
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("请求失败: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("token 兑换失败 (HTTP %d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var tokenResp rawOAuthTokenResp
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return nil, nil, fmt.Errorf("解析响应失败: %w", err)
	}

	info := auth.ParseIDToken(tokenResp.IDToken)
	return &tokenResp, info, nil
}
