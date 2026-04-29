package proxy

import (
	"strings"
	"testing"
)

func TestClientProfilesIncludeLatestCodexTUI(t *testing.T) {
	found := false
	for _, profile := range clientProfiles {
		if profile.Version != StableCodexVersion {
			continue
		}
		if !strings.Contains(profile.UserAgent, "codex-tui/"+StableCodexVersion) {
			t.Fatalf("%s profile has mismatched User-Agent: %q", StableCodexVersion, profile.UserAgent)
		}
		found = true
	}
	if !found {
		t.Fatalf("clientProfiles should include codex-tui/%s", StableCodexVersion)
	}
}

func TestDefaultClientProfileUsesStableCodexTUI(t *testing.T) {
	profile := ProfileForAccount(1)
	if profile.Version != StableCodexVersion {
		t.Fatalf("ProfileForAccount returned unexpected Codex version: %s", profile.Version)
	}
	if !strings.HasPrefix(profile.UserAgent, "codex-tui/") {
		t.Fatalf("ProfileForAccount returned unexpected User-Agent: %s", profile.UserAgent)
	}
}
