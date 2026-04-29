package proxy

import (
	"strings"
	"testing"
)

func TestClientProfilesIncludeLatestCodexCLI(t *testing.T) {
	found := false
	for _, profile := range clientProfiles {
		if profile.Version != StableCodexVersion {
			continue
		}
		if !strings.Contains(profile.UserAgent, "codex_cli_rs/"+StableCodexVersion) {
			t.Fatalf("%s profile has mismatched User-Agent: %q", StableCodexVersion, profile.UserAgent)
		}
		found = true
	}
	if !found {
		t.Fatalf("clientProfiles should include codex_cli_rs/%s", StableCodexVersion)
	}
}

func TestDefaultClientProfileUsesStableCodexCLI(t *testing.T) {
	profile := ProfileForAccount(1)
	if profile.Version != StableCodexVersion {
		t.Fatalf("ProfileForAccount returned unexpected Codex version: %s", profile.Version)
	}
	if !strings.HasPrefix(profile.UserAgent, "codex_cli_rs/") {
		t.Fatalf("ProfileForAccount returned unexpected User-Agent: %s", profile.UserAgent)
	}
}
