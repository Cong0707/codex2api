package wsrelay

import (
	"net/http"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy"
)

func TestPrepareWebsocketHeadersUsesConfiguredDefaultsAndBetaFeatures(t *testing.T) {
	exec := NewExecutor()
	cfg := &proxy.DeviceProfileConfig{
		UserAgent:              "codex_cli_rs/0.120.0 (Mac OS 15.5.0; arm64) Apple_Terminal/464",
		PackageVersion:         "0.120.0",
		RuntimeVersion:         "0.120.0",
		OS:                     "MacOS",
		Arch:                   "arm64",
		StabilizeDeviceProfile: true,
		BetaFeatures:           "multi_agent",
	}
	ginHeaders := http.Header{
		"Originator": []string{"custom-originator"},
	}
	account := &auth.Account{DBID: 42}

	headers := exec.prepareWebsocketHeaders("token-123", account, "42", "session-123", "api-key-1", cfg, ginHeaders)

	if got := headers.Get("Authorization"); got != "Bearer token-123" {
		t.Fatalf("Authorization = %q", got)
	}
	if got := headers.Get("OpenAI-Beta"); got != responsesWebsocketBetaHeader {
		t.Fatalf("OpenAI-Beta = %q", got)
	}
	if got := headers.Get("X-Codex-Beta-Features"); got != "multi_agent" {
		t.Fatalf("X-Codex-Beta-Features = %q", got)
	}
	if got := headers.Get("User-Agent"); got != cfg.UserAgent {
		t.Fatalf("User-Agent = %q", got)
	}
	if got := headers.Get("Version"); got != "0.120.0" {
		t.Fatalf("Version = %q", got)
	}
	if got := headers.Get("Originator"); got != "custom-originator" {
		t.Fatalf("Originator = %q", got)
	}
	if got := headers.Get("Chatgpt-Account-Id"); got != "42" {
		t.Fatalf("Chatgpt-Account-Id = %q", got)
	}
	if got := headers.Get("Conversation_id"); got != "session-123" {
		t.Fatalf("Conversation_id = %q", got)
	}
}

func TestPrepareWebsocketHeadersUsesAccountProfileByDefault(t *testing.T) {
	exec := NewExecutor()
	account := &auth.Account{DBID: 42}
	profile := proxy.StableCodexClientProfile()

	headers := exec.prepareWebsocketHeaders("token-123", account, "42", "session-123", "api-key-1", nil, http.Header{})

	if got := headers.Get("User-Agent"); got != profile.UserAgent {
		t.Fatalf("User-Agent = %q, want %q", got, profile.UserAgent)
	}
	if got := headers.Get("Version"); got != profile.Version {
		t.Fatalf("Version = %q, want %q", got, profile.Version)
	}
}
