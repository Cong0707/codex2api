package proxy

import (
	"net/http"
	"testing"
)

func TestResolveCodexRequestIdentityStableFallback(t *testing.T) {
	identity := resolveCodexRequestIdentity(nil, "", nil, nil)
	stable := StableCodexClientProfile()
	if identity.UserAgent != stable.UserAgent {
		t.Fatalf("User-Agent = %q, want %q", identity.UserAgent, stable.UserAgent)
	}
	if identity.Version != stable.Version {
		t.Fatalf("Version = %q, want %q", identity.Version, stable.Version)
	}
}

func TestResolveCodexRequestIdentityUsesDownstreamCodexTUI(t *testing.T) {
	headers := http.Header{}
	headers.Set("User-Agent", "codex-tui/0.120.0 (Mac OS 26.3.1; arm64) iTerm.app/3.6.9 (codex-tui; 0.120.0)")

	identity := resolveCodexRequestIdentity(nil, "", headers, &DeviceProfileConfig{StabilizeDeviceProfile: false})
	if identity.UserAgent != headers.Get("User-Agent") {
		t.Fatalf("User-Agent = %q, want downstream %q", identity.UserAgent, headers.Get("User-Agent"))
	}
	if identity.Version != "0.120.0" {
		t.Fatalf("Version = %q, want %q", identity.Version, "0.120.0")
	}
}

func TestResolveCodexRequestIdentityIgnoresLegacyCodexCLI(t *testing.T) {
	headers := http.Header{}
	headers.Set("User-Agent", "codex_cli_rs/0.124.0 (Mac OS 15.5.0; arm64) Apple_Terminal/464")

	identity := resolveCodexRequestIdentity(nil, "", headers, &DeviceProfileConfig{StabilizeDeviceProfile: false})
	stable := StableCodexClientProfile()
	if identity.UserAgent != stable.UserAgent {
		t.Fatalf("User-Agent = %q, want stable %q", identity.UserAgent, stable.UserAgent)
	}
	if identity.Version != stable.Version {
		t.Fatalf("Version = %q, want stable %q", identity.Version, stable.Version)
	}
}

func TestApplyCodexRequestHeaders(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://example.com", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}

	identity := codexRequestIdentity{
		UserAgent: "codex-tui/0.120.0 (Mac OS 26.3.1; arm64) iTerm.app/3.6.9 (codex-tui; 0.120.0)",
		Version:   "0.120.0",
	}
	applyCodexRequestHeaders(req, "token", "acc-id", "session-123", identity, true, nil)

	if got := req.Header.Get("Authorization"); got != "Bearer token" {
		t.Fatalf("Authorization = %q", got)
	}
	if got := req.Header.Get("User-Agent"); got != identity.UserAgent {
		t.Fatalf("User-Agent = %q", got)
	}
	if got := req.Header.Get("Version"); got != identity.Version {
		t.Fatalf("Version = %q", got)
	}
	if got := req.Header.Get("Session_id"); got != "session-123" {
		t.Fatalf("Session_id = %q", got)
	}
	if got := req.Header.Get("Conversation_id"); got != "session-123" {
		t.Fatalf("Conversation_id = %q", got)
	}
	if got := req.Header.Get("Chatgpt-Account-Id"); got != "acc-id" {
		t.Fatalf("Chatgpt-Account-Id = %q", got)
	}
	if got := req.Header.Get("Originator"); got != Originator {
		t.Fatalf("Originator = %q", got)
	}
	if got := req.Header.Get("Accept"); got != "text/event-stream" {
		t.Fatalf("Accept = %q", got)
	}
}
