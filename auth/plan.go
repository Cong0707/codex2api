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
