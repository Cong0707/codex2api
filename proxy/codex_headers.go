package proxy

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/codex2api/auth"
	"github.com/google/uuid"
)

type codexRequestIdentity struct {
	UserAgent string
	Version   string
}

// ResolveCodexHeaderIdentity 解析上游请求需要使用的 UA/Version。
// 该函数用于在 HTTP 与 WebSocket 两条链路上统一请求身份策略。
func ResolveCodexHeaderIdentity(account *auth.Account, apiKey string, downstreamHeaders http.Header, deviceCfg *DeviceProfileConfig) (userAgent string, version string) {
	identity := resolveCodexRequestIdentity(account, apiKey, downstreamHeaders, deviceCfg)
	return identity.UserAgent, identity.Version
}

func resolveCodexRequestIdentity(account *auth.Account, apiKey string, downstreamHeaders http.Header, deviceCfg *DeviceProfileConfig) codexRequestIdentity {
	// 设备稳定化模式：优先使用稳定 profile（并支持从下游 Codex CLI 学习更高版本）。
	if IsDeviceProfileStabilizationEnabled(deviceCfg) {
		profile := ResolveDeviceProfile(account, apiKey, downstreamHeaders, deviceCfg)
		identity := codexRequestIdentity{
			UserAgent: strings.TrimSpace(profile.UserAgent),
			Version:   strings.TrimSpace(profile.RuntimeVersion),
		}
		if profile.HasVersion {
			identity.Version = fmt.Sprintf("%d.%d.%d", profile.Version.major, profile.Version.minor, profile.Version.patch)
		}
		if identity.UserAgent == "" {
			stable := StableCodexClientProfile()
			identity.UserAgent = stable.UserAgent
			if identity.Version == "" {
				identity.Version = stable.Version
			}
		}
		if identity.Version == "" {
			identity.Version = StableCodexVersion
		}
		return identity
	}

	// 非稳定化模式：仅在下游已经是足够新的 codex-tui 指纹时才透传，
	// 避免旧 codex_cli_rs / 旧版本导致 gpt-5.5 被上游拒绝。
	if ua := strings.TrimSpace(downstreamHeaders.Get("User-Agent")); shouldPassThroughCodexUserAgent(ua) {
		if v, ok := parseCodexCLIVersion(ua); ok {
			return codexRequestIdentity{
				UserAgent: ua,
				Version:   fmt.Sprintf("%d.%d.%d", v.major, v.minor, v.patch),
			}
		}
	}

	stable := StableCodexClientProfile()
	return codexRequestIdentity{
		UserAgent: stable.UserAgent,
		Version:   stable.Version,
	}
}

func ensureHeader(target http.Header, source http.Header, key, fallback string) {
	if target == nil {
		return
	}
	if strings.TrimSpace(target.Get(key)) != "" {
		return
	}
	if source != nil {
		if value := strings.TrimSpace(source.Get(key)); value != "" {
			target.Set(key, value)
			return
		}
	}
	if value := strings.TrimSpace(fallback); value != "" {
		target.Set(key, value)
	}
}

func applyCodexRequestHeaders(req *http.Request, accessToken, accountID, sessionID string, identity codexRequestIdentity, stream bool, downstreamHeaders http.Header) {
	if req == nil {
		return
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+accessToken)
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	} else {
		req.Header.Set("Accept", "application/json")
	}
	req.Header.Set("Connection", "Keep-Alive")

	if identity.UserAgent != "" {
		req.Header.Set("User-Agent", identity.UserAgent)
	}
	if identity.Version != "" {
		req.Header.Set("Version", identity.Version)
	}

	ensureHeader(req.Header, downstreamHeaders, "X-Codex-Turn-Metadata", "")
	ensureHeader(req.Header, downstreamHeaders, "X-Codex-Turn-State", "")
	ensureHeader(req.Header, downstreamHeaders, "X-Responsesapi-Include-Timing-Metrics", "")
	ensureHeader(req.Header, downstreamHeaders, "X-Client-Request-Id", uuid.NewString())

	originator := Originator
	if downstreamHeaders != nil {
		if incoming := strings.TrimSpace(downstreamHeaders.Get("Originator")); incoming != "" {
			originator = incoming
		}
	}
	req.Header.Set("Originator", originator)

	if strings.TrimSpace(accountID) != "" {
		req.Header.Set("Chatgpt-Account-Id", strings.TrimSpace(accountID))
	}

	if strings.TrimSpace(sessionID) != "" {
		req.Header.Set("Session_id", strings.TrimSpace(sessionID))
		req.Header.Set("Conversation_id", strings.TrimSpace(sessionID))
	} else {
		ensureHeader(req.Header, downstreamHeaders, "Session_id", uuid.NewString())
	}
}
