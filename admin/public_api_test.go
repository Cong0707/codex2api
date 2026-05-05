package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func TestPublicGenerateRateLimitByIP(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newSQLiteDBForAdminTest(t)
	h := &Handler{db: db}

	const ip = "10.0.0.1"
	for i := 0; i < 3; i++ {
		_, err := db.InsertPublicAPIKeyWithMeta(context.Background(), "k", generatePublicKey(), "public_generate", ip, 0)
		if err != nil {
			t.Fatalf("insert key %d: %v", i, err)
		}
	}

	r := gin.New()
	h.RegisterPublicRoutes(r)

	req := httptest.NewRequest(http.MethodPost, "/public/generate", bytes.NewBufferString(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forwarded-For", ip)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status=%d, want=%d, body=%s", rec.Code, http.StatusTooManyRequests, rec.Body.String())
	}
}

func TestPublicRedeemSuccess(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newSQLiteDBForAdminTest(t)
	h := &Handler{db: db}

	key := "pk-redeem-route-test-1234567890"
	keyID, err := db.InsertPublicAPIKeyWithMeta(context.Background(), "k", key, "public_generate", "10.0.0.2", 1.0)
	if err != nil {
		t.Fatalf("insert key: %v", err)
	}
	if _, _, err = db.ImportRedeemCodes(context.Background(), 0.5, []string{"R-500"}); err != nil {
		t.Fatalf("import redeem codes: %v", err)
	}

	r := gin.New()
	h.RegisterPublicRoutes(r)

	body, _ := json.Marshal(map[string]float64{"amount_usd": 0.8})
	req := httptest.NewRequest(http.MethodPost, "/public/redeem", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want=%d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var payload struct {
		PublicAPIKeyID   int64   `json:"public_api_key_id"`
		Code             string  `json:"code"`
		RedeemedAmount   float64 `json:"redeemed_amount_usd"`
		RemainingBalance float64 `json:"remaining_balance_usd"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if payload.PublicAPIKeyID != keyID {
		t.Fatalf("public_api_key_id=%d, want=%d", payload.PublicAPIKeyID, keyID)
	}
	if payload.Code != "R-500" {
		t.Fatalf("code=%q, want=%q", payload.Code, "R-500")
	}
	if payload.RedeemedAmount != 0.5 {
		t.Fatalf("redeemed_amount=%.4f, want=0.5", payload.RedeemedAmount)
	}
	if payload.RemainingBalance != 0.5 {
		t.Fatalf("remaining_balance=%.4f, want=0.5", payload.RemainingBalance)
	}
}

func TestPublicKeyInfoRequiresPublicKey(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newSQLiteDBForAdminTest(t)
	h := &Handler{db: db}

	r := gin.New()
	h.RegisterPublicRoutes(r)

	req := httptest.NewRequest(http.MethodGet, "/public/key-info", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden && rec.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d, want=%d or %d, body=%s", rec.Code, http.StatusForbidden, http.StatusUnauthorized, rec.Body.String())
	}
}

func TestPublicKeyInfoSuccess(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newSQLiteDBForAdminTest(t)
	h := &Handler{db: db}

	ctx := context.Background()
	key := "pk-info-test-1234567890"
	keyID, err := db.InsertPublicAPIKeyWithMeta(ctx, "pub-info", key, "public_generate", "10.0.0.10", 0)
	if err != nil {
		t.Fatalf("insert public key: %v", err)
	}

	accountID, err := db.InsertATAccount(ctx, "acc-info", "at-token-info", "")
	if err != nil {
		t.Fatalf("insert account: %v", err)
	}

	if err := db.BindAccountToPublicKey(ctx, database.PublicSettlementBindInput{
		AccountID:            accountID,
		PublicAPIKeyID:       keyID,
		BaselineUsagePercent: 0,
		InitialAmountUSD:     0.4,
		FullAmountUSD:        2.0,
	}); err != nil {
		t.Fatalf("bind account to key: %v", err)
	}

	r := gin.New()
	h.RegisterPublicRoutes(r)

	req := httptest.NewRequest(http.MethodGet, "/public/key-info", nil)
	req.Header.Set("X-Public-Key", key)
	req.Header.Set("X-Forwarded-For", "10.0.0.11")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want=%d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var payload struct {
		ID                int64   `json:"id"`
		Source            string  `json:"source"`
		BalanceUSD        float64 `json:"balance_usd"`
		BoundAccountCount int64   `json:"bound_account_count"`
		SettledAmountUSD  float64 `json:"settled_amount_usd"`
		LastUsedIP        string  `json:"last_used_ip"`
		LastUsedAt        string  `json:"last_used_at"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	if payload.ID != keyID {
		t.Fatalf("id=%d, want=%d", payload.ID, keyID)
	}
	if payload.Source != "public_generate" {
		t.Fatalf("source=%q, want=%q", payload.Source, "public_generate")
	}
	if payload.BoundAccountCount != 1 {
		t.Fatalf("bound_account_count=%d, want=1", payload.BoundAccountCount)
	}
	if payload.SettledAmountUSD != 0.4 {
		t.Fatalf("settled_amount_usd=%.4f, want=0.4", payload.SettledAmountUSD)
	}
	if payload.BalanceUSD != 0.4 {
		t.Fatalf("balance_usd=%.4f, want=0.4", payload.BalanceUSD)
	}
	if payload.LastUsedIP != "10.0.0.11" {
		t.Fatalf("last_used_ip=%q, want=%q", payload.LastUsedIP, "10.0.0.11")
	}
	if payload.LastUsedAt == "" {
		t.Fatalf("last_used_at should not be empty")
	}
}

func TestPublicQuotaStatsExcludeUnauthorizedAndDeleted(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newSQLiteDBForAdminTest(t)
	h := &Handler{db: db}

	ctx := context.Background()
	activeID, err := db.InsertATAccount(ctx, "active", "at-token-active", "")
	if err != nil {
		t.Fatalf("insert active account: %v", err)
	}
	if err := db.UpdateCredentials(ctx, activeID, map[string]interface{}{
		"codex_7d_used_percent": 80.0,
	}); err != nil {
		t.Fatalf("update active usage: %v", err)
	}

	unauthorizedID, err := db.InsertATAccount(ctx, "unauthorized", "at-token-unauth", "")
	if err != nil {
		t.Fatalf("insert unauthorized account: %v", err)
	}
	if err := db.UpdateCredentials(ctx, unauthorizedID, map[string]interface{}{
		"codex_7d_used_percent": 55.0,
	}); err != nil {
		t.Fatalf("update unauthorized usage: %v", err)
	}
	if err := db.SetCooldown(ctx, unauthorizedID, "unauthorized", time.Now().Add(2*time.Hour)); err != nil {
		t.Fatalf("set unauthorized cooldown: %v", err)
	}

	deletedID, err := db.InsertATAccount(ctx, "deleted", "at-token-deleted", "")
	if err != nil {
		t.Fatalf("insert deleted account: %v", err)
	}
	if err := db.SetError(ctx, deletedID, "deleted"); err != nil {
		t.Fatalf("set deleted error: %v", err)
	}

	r := gin.New()
	h.RegisterPublicRoutes(r)

	req := httptest.NewRequest(http.MethodGet, "/public/quota-stats", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want=%d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var payload struct {
		QuotaAccountCount      int     `json:"quota_account_count"`
		QuotaTotal             int     `json:"quota_total"`
		QuotaUsed              int     `json:"quota_used"`
		QuotaRemaining         int     `json:"quota_remaining"`
		QuotaUsedPercent       float64 `json:"quota_used_percent"`
		QuotaRemainingPercent  float64 `json:"quota_remaining_percent"`
		QuotaRemainingAccounts float64 `json:"quota_remaining_accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	if payload.QuotaAccountCount != 1 {
		t.Fatalf("quota_account_count=%d, want=1", payload.QuotaAccountCount)
	}
	if payload.QuotaTotal != 100 {
		t.Fatalf("quota_total=%d, want=100", payload.QuotaTotal)
	}
	if payload.QuotaUsed != 80 {
		t.Fatalf("quota_used=%d, want=80", payload.QuotaUsed)
	}
	if payload.QuotaRemaining != 20 {
		t.Fatalf("quota_remaining=%d, want=20", payload.QuotaRemaining)
	}
	if payload.QuotaUsedPercent != 80 {
		t.Fatalf("quota_used_percent=%.2f, want=80", payload.QuotaUsedPercent)
	}
	if payload.QuotaRemainingPercent != 20 {
		t.Fatalf("quota_remaining_percent=%.2f, want=20", payload.QuotaRemainingPercent)
	}
	if payload.QuotaRemainingAccounts != 0.2 {
		t.Fatalf("quota_remaining_accounts=%.2f, want=0.2", payload.QuotaRemainingAccounts)
	}
}

func TestPublicQuotaStatsUsePlanVariantRates(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newSQLiteDBForAdminTest(t)
	h := &Handler{db: db}

	ctx := context.Background()
	if err := db.UpdateSystemSettings(ctx, &database.SystemSettings{
		QuotaRatePlus:   10,
		QuotaRatePro:    100,
		QuotaRatePro5x:  25,
		QuotaRatePro20x: 100,
		QuotaRateTeam:   10,
	}); err != nil {
		t.Fatalf("update settings: %v", err)
	}

	pro5xID, err := db.InsertATAccount(ctx, "pro5x", "at-token-pro5x", "")
	if err != nil {
		t.Fatalf("insert pro5x account: %v", err)
	}
	if err := db.UpdateCredentials(ctx, pro5xID, map[string]interface{}{
		"plan_type":             "prolite",
		"codex_7d_used_percent": 50.0,
	}); err != nil {
		t.Fatalf("update pro5x usage: %v", err)
	}

	pro20xID, err := db.InsertATAccount(ctx, "pro20x", "at-token-pro20x", "")
	if err != nil {
		t.Fatalf("insert pro20x account: %v", err)
	}
	if err := db.UpdateCredentials(ctx, pro20xID, map[string]interface{}{
		"plan_type":             "pro",
		"codex_7d_used_percent": 25.0,
	}); err != nil {
		t.Fatalf("update pro20x usage: %v", err)
	}

	r := gin.New()
	h.RegisterPublicRoutes(r)

	req := httptest.NewRequest(http.MethodGet, "/public/quota-stats", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want=%d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var payload struct {
		QuotaAccountCount      int     `json:"quota_account_count"`
		QuotaTotal             int     `json:"quota_total"`
		QuotaUsed              int     `json:"quota_used"`
		QuotaRemaining         int     `json:"quota_remaining"`
		QuotaUsedPercent       float64 `json:"quota_used_percent"`
		QuotaRemainingPercent  float64 `json:"quota_remaining_percent"`
		QuotaRemainingAccounts float64 `json:"quota_remaining_accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	if payload.QuotaAccountCount != 2 {
		t.Fatalf("quota_account_count=%d, want=2", payload.QuotaAccountCount)
	}
	if payload.QuotaTotal != 12500 {
		t.Fatalf("quota_total=%d, want=12500", payload.QuotaTotal)
	}
	if payload.QuotaUsed != 3750 {
		t.Fatalf("quota_used=%d, want=3750", payload.QuotaUsed)
	}
	if payload.QuotaRemaining != 8750 {
		t.Fatalf("quota_remaining=%d, want=8750", payload.QuotaRemaining)
	}
	if payload.QuotaUsedPercent != 30 {
		t.Fatalf("quota_used_percent=%.2f, want=30", payload.QuotaUsedPercent)
	}
	if payload.QuotaRemainingPercent != 70 {
		t.Fatalf("quota_remaining_percent=%.2f, want=70", payload.QuotaRemainingPercent)
	}
	if payload.QuotaRemainingAccounts != 87.5 {
		t.Fatalf("quota_remaining_accounts=%.2f, want=87.5", payload.QuotaRemainingAccounts)
	}
}

func TestPublicKeyInfoPostAlias(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newSQLiteDBForAdminTest(t)
	h := &Handler{db: db}

	ctx := context.Background()
	key := "pk-info-post-test-1234567890"
	keyID, err := db.InsertPublicAPIKeyWithMeta(ctx, "pub-info-post", key, "public_generate", "10.0.0.20", 0)
	if err != nil {
		t.Fatalf("insert public key: %v", err)
	}

	r := gin.New()
	h.RegisterPublicRoutes(r)

	req := httptest.NewRequest(http.MethodPost, "/public/key-info", nil)
	req.Header.Set("X-Public-Key", key)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want=%d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var payload struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if payload.ID != keyID {
		t.Fatalf("id=%d, want=%d", payload.ID, keyID)
	}
}

func TestPublicQuotaStatsPostAlias(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newSQLiteDBForAdminTest(t)
	h := &Handler{db: db}

	r := gin.New()
	h.RegisterPublicRoutes(r)

	req := httptest.NewRequest(http.MethodPost, "/public/quota-stats", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want=%d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
}
