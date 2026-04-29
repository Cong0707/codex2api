package auth

import "strings"

var planPriority = map[string]int{
	"":           -1,
	"free":       0,
	"plus":       1,
	"pro":        2,
	"team":       3,
	"enterprise": 4,
}

func planScore(plan string) int {
	if score, ok := planPriority[plan]; ok {
		return score
	}
	return -1
}

func compactPlanText(plan string) string {
	text := strings.TrimSpace(strings.ToLower(plan))
	if text == "" {
		return ""
	}
	text = strings.ReplaceAll(text, "_", "")
	text = strings.ReplaceAll(text, "-", "")
	text = strings.ReplaceAll(text, " ", "")
	return text
}

// NormalizePlanType 将上游/导入中的套餐字符串标准化为内部统一值。
func NormalizePlanType(plan string) string {
	text := compactPlanText(plan)
	if text == "" {
		return ""
	}

	switch {
	case strings.Contains(text, "enterprise"):
		return "enterprise"
	case strings.Contains(text, "team"), strings.Contains(text, "business"), text == "go":
		return "team"
	case strings.Contains(text, "pro"):
		return "pro"
	case strings.Contains(text, "plus"):
		return "plus"
	case strings.Contains(text, "free"):
		return "free"
	default:
		return strings.TrimSpace(strings.ToLower(plan))
	}
}

// PreferPlanType 选择更可信的套餐值（优先级：enterprise > team > pro > plus > free）。
func PreferPlanType(a, b string) string {
	pa := NormalizePlanType(a)
	pb := NormalizePlanType(b)
	if planScore(pa) >= planScore(pb) {
		return pa
	}
	return pb
}

// OfficialImageQuotaForPlan 返回官方图片链路可用计数。
// 当前仅用于前端展示与是否允许走官方兜底的快速判断。
func OfficialImageQuotaForPlan(plan string) int {
	switch NormalizePlanType(plan) {
	case "plus", "pro", "team", "enterprise":
		return 1
	default:
		return 0
	}
}

// OfficialImageQuotaForAccount 返回当前账号在官方图片兜底链路上的动态可用次数。
// 这里的“次数”不是上游显式图片额度，而是对 Codex 官方工具链路的可调度性判断：
// 套餐不支持、账号冷却/封禁、5h/7d 用量已满时均显示/调度为 0。
func OfficialImageQuotaForAccount(acc *Account) int {
	if acc == nil {
		return 0
	}
	capacity := OfficialImageQuotaForPlan(acc.GetPlanType())
	if capacity <= 0 || !acc.IsAvailable() {
		return 0
	}
	if pct7d, ok := acc.GetUsagePercent7d(); ok && pct7d >= 100 {
		return 0
	}
	if pct5h, ok := acc.GetUsagePercent5h(); ok && pct5h >= 100 {
		return 0
	}
	return capacity
}

// DefaultWebImageQuotaForPlan 是网页反代额度无法从上游读取、但请求已成功时的兜底容量。
// free 网页端通常有独立图片额度；付费号没有明确上限字段时按 gpt2api 默认值兜底。
func DefaultWebImageQuotaForPlan(plan string) int {
	switch NormalizePlanType(plan) {
	case "free":
		return 25
	case "plus", "pro", "team", "enterprise":
		return 100
	default:
		return 25
	}
}
