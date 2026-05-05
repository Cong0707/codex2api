package admin

import "testing"

func TestExtractCredentialUpdatesFromRawInfo(t *testing.T) {
	raw := []byte(`{
		"account": {
			"email": "demo@example.com",
			"chatgpt_account_id": "acc_123",
			"plan_type": "Plus"
		}
	}`)

	refreshed, updates := extractCredentialUpdatesFromRawInfo(raw)

	if got := refreshed["email"]; got != "demo@example.com" {
		t.Fatalf("email = %q, want demo@example.com", got)
	}
	if got := refreshed["account_id"]; got != "acc_123" {
		t.Fatalf("account_id = %q, want acc_123", got)
	}
	if got := refreshed["plan_type"]; got != "plus" {
		t.Fatalf("plan_type = %q, want plus", got)
	}

	if got, _ := updates["email"].(string); got != "demo@example.com" {
		t.Fatalf("updates[email] = %q, want demo@example.com", got)
	}
	if got, _ := updates["account_id"].(string); got != "acc_123" {
		t.Fatalf("updates[account_id] = %q, want acc_123", got)
	}
	if got, _ := updates["plan_type"].(string); got != "plus" {
		t.Fatalf("updates[plan_type] = %q, want plus", got)
	}
}

func TestExtractCredentialUpdatesFromRawInfo_AccountsArray(t *testing.T) {
	raw := []byte(`{
		"account_ordering": ["b68fdf8a-5017-485a-9b94-362323c9fff2"],
		"accounts": [
			{
				"id": "b68fdf8a-5017-485a-9b94-362323c9fff2",
				"plan_type": "free"
			}
		],
		"default_account_id": "b68fdf8a-5017-485a-9b94-362323c9fff2"
	}`)

	refreshed, _ := extractCredentialUpdatesFromRawInfo(raw)

	if got := refreshed["account_id"]; got != "b68fdf8a-5017-485a-9b94-362323c9fff2" {
		t.Fatalf("account_id = %q, want default_account_id", got)
	}
	if got := refreshed["plan_type"]; got != "free" {
		t.Fatalf("plan_type = %q, want free", got)
	}
}

func TestMergeCredentialRefresh_PlanFromQuotaData(t *testing.T) {
	profile := cliproxyProfile{
		Email:      "from_cpa@example.com",
		AccountID:  "acc_from_cpa",
		PlanType:   "plus",
		PlanSource: "id_token.chatgpt_plan_type",
	}
	upstream := map[string]string{
		"plan_type": "free",
	}

	refreshed, updates := mergeCredentialRefresh(profile, upstream, "plus", "free")

	if got := refreshed["plan_type"]; got != "free" {
		t.Fatalf("refreshed plan_type = %q, want free", got)
	}
	if got, _ := updates["plan_type"].(string); got != "free" {
		t.Fatalf("updates plan_type = %q, want free", got)
	}
	if got := refreshed["email"]; got != "from_cpa@example.com" {
		t.Fatalf("email = %q, want from_cpa@example.com", got)
	}
}

func TestMergeAuthCredentialRefresh_NoPlanWrite(t *testing.T) {
	profile := cliproxyProfile{
		Email:     "auth@example.com",
		AccountID: "acc_auth",
		PlanType:  "team",
	}
	upstream := map[string]string{
		"plan_type": "free",
	}
	refreshed, updates := mergeAuthCredentialRefresh(profile, upstream)

	if _, ok := refreshed["plan_type"]; ok {
		t.Fatalf("auth refresh should not include plan_type, got %v", refreshed["plan_type"])
	}
	if _, ok := updates["plan_type"]; ok {
		t.Fatalf("auth refresh should not update plan_type")
	}
	if got := refreshed["email"]; got != "auth@example.com" {
		t.Fatalf("email = %q, want auth@example.com", got)
	}
}

func TestDetectPlanFromPayload_MeSubscriptionFlag(t *testing.T) {
	raw := []byte(`{
		"has_paid_subscription": true
	}`)

	plan, source := detectPlanFromPayload(openAIMeURL, raw)
	if plan != "plus" {
		t.Fatalf("plan = %q, want plus", plan)
	}
	if source == "" {
		t.Fatal("source should not be empty")
	}
}

func TestDetectPlanFromPayload_PreservesProlitePlan(t *testing.T) {
	raw := []byte(`{"plan_type":"prolite"}`)

	plan, source := detectPlanFromPayload(openAIWhamUsageURL, raw)
	if plan != "prolite" {
		t.Fatalf("plan = %q, want prolite", plan)
	}
	if source == "" {
		t.Fatal("source should not be empty")
	}
}

func TestMergeCredentialRefresh_PreservesProliteOverGenericPro(t *testing.T) {
	refreshed, updates := mergeCredentialRefresh(cliproxyProfile{}, map[string]string{"plan_type": "prolite"}, "pro", "")

	if got := refreshed["plan_type"]; got != "prolite" {
		t.Fatalf("refreshed plan_type = %q, want prolite", got)
	}
	if got, _ := updates["plan_type"].(string); got != "prolite" {
		t.Fatalf("updates plan_type = %q, want prolite", got)
	}
}

func TestPickBestPlanSnapshot_FallbackToErrorBody(t *testing.T) {
	snapshots := []openAISnapshot{
		{
			Endpoint: openAIWhamAccountCheckURL,
			Raw:      []byte(`{"error":{"message":"forbidden"}}`),
			Err:      assertError("upstream 403"),
		},
	}

	plan, source, raw, endpoint := pickBestPlanSnapshot(snapshots)
	if plan != "" {
		t.Fatalf("plan = %q, want empty", plan)
	}
	if source != "" {
		t.Fatalf("source = %q, want empty", source)
	}
	if endpoint != openAIWhamAccountCheckURL {
		t.Fatalf("endpoint = %q, want %q", endpoint, openAIWhamAccountCheckURL)
	}
	if string(raw) != `{"error":{"message":"forbidden"}}` {
		t.Fatalf("raw = %q, want fallback error body", string(raw))
	}
}

func assertError(msg string) error { return errString(msg) }

type errString string

func (e errString) Error() string { return string(e) }
