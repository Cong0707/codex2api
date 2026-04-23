package proxy

import (
	"strings"
	"testing"
)

func TestClientProfilesIncludeLatestCodexCLI(t *testing.T) {
	found := false
	for _, profile := range clientProfiles {
		if profile.Version != "0.124.0" {
			continue
		}
		if !strings.Contains(profile.UserAgent, "codex_cli_rs/0.124.0") {
			t.Fatalf("0.124.0 profile has mismatched User-Agent: %q", profile.UserAgent)
		}
		found = true
	}
	if !found {
		t.Fatal("clientProfiles should include codex_cli_rs/0.124.0")
	}
}

func TestDefaultClientProfileUsesLatestCodexCLI(t *testing.T) {
	profile := ProfileForAccount(1)
	if profile.Version == "0.117.0" || profile.Version == "0.116.0" {
		t.Fatalf("ProfileForAccount returned outdated Codex CLI version: %s", profile.Version)
	}
}
