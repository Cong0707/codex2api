package proxy

import (
	"fmt"
	"hash/fnv"
)

// ==================== 动态 User-Agent 生成 ====================
//
// 当前 Codex 官方链路已经切到 codex-tui 指纹，gpt-5.5 等新模型会校验
// 客户端版本。这里默认对齐 CLIProxyAPI 最新上游的稳定指纹，避免继续
// 透传过旧的 codex_cli_rs 指纹导致 “requires a newer version of Codex”。

// ClientProfile 表示一个模拟客户端的完整身份
type ClientProfile struct {
	UserAgent string // 完整的 User-Agent 字符串
	Version   string // codex CLI 版本（需与 UA 中的版本一致）
}

const (
	// StableCodexUserAgent 对齐 CLIProxyAPI 最新 Codex 默认指纹。
	StableCodexUserAgent  = "codex-tui/0.118.0 (Mac OS 26.3.1; arm64) iTerm.app/3.6.9 (codex-tui; 0.118.0)"
	StableCodexVersion    = "0.118.0"
	StableCodexOriginator = "codex-tui"
)

// StableCodexClientProfile 返回稳定的 Codex 客户端画像。
func StableCodexClientProfile() ClientProfile {
	return ClientProfile{
		UserAgent: StableCodexUserAgent,
		Version:   StableCodexVersion,
	}
}

// 预定义客户端画像池。
// 目前 Codex 请求链路统一固定到稳定 codex-tui 指纹，避免账号池内混入旧
// codex_cli_rs 标识造成新版模型校验失败。
var clientProfiles = []ClientProfile{
	StableCodexClientProfile(),
}

// ProfileForAccount 根据账号 ID 确定性地选择一个 ClientProfile
// 同一个账号永远返回相同的 profile，不同账号大概率返回不同的 profile
func ProfileForAccount(accountID int64) ClientProfile {
	if len(clientProfiles) == 0 {
		return ClientProfile{
			UserAgent: StableCodexUserAgent,
			Version:   StableCodexVersion,
		}
	}

	// 用 FNV hash 将 accountID 映射到 profile 池，确保分布均匀
	h := fnv.New32a()
	fmt.Fprintf(h, "codex2api:ua-profile:%d", accountID)
	idx := int(h.Sum32()) % len(clientProfiles)
	if idx < 0 {
		idx = -idx
	}

	return clientProfiles[idx]
}
