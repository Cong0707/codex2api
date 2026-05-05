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
// 注意：该函数仅用于调度、限流、额度等行为判断。
// OpenAI/CPA 目前会直接返回更细的原始 plan_type（例如 prolite / pro），
// 存储和前端展示应保留原始值，不要调用该函数覆盖掉细分套餐。
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
	case text == "prolite", text == "pro5x", text == "pro20x", strings.Contains(text, "pro"):
		return "pro"
	case strings.Contains(text, "plus"):
		return "plus"
	case strings.Contains(text, "free"):
		return "free"
	default:
		return strings.TrimSpace(strings.ToLower(plan))
	}
}

// PlanVariant 返回用于展示/精细过滤的套餐分支。
// 行为上 prolite/pro5x/pro20x 都属于 pro；这里专门保留 Pro 5x / 20x 差异。
func PlanVariant(plan string) string {
	text := compactPlanText(plan)
	switch {
	case text == "":
		return ""
	case text == "prolite", strings.Contains(text, "pro5x"), strings.Contains(text, "5x"):
		return "pro_5x"
	case text == "pro", strings.Contains(text, "pro20x"), strings.Contains(text, "20x"):
		return "pro_20x"
	default:
		return NormalizePlanType(plan)
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

const UnknownOfficialImageAvailability = -1

// SupportsOfficialImageGeneration 表示该套餐是否允许走官方图片链路。
// 这里只判断“是否支持”，不推算次数；真实次数只信任上游显式返回字段。
func SupportsOfficialImageGeneration(plan string) bool {
	switch NormalizePlanType(plan) {
	case "", "free":
		return false
	default:
		return true
	}
}

// DefaultOfficialImageAvailabilityForPlan 返回在没有任何上游显式额度字段时的默认值。
// free 已知无法走官方图片链路，因此固定为 0；其它套餐一律未知（-1），不能猜。
func DefaultOfficialImageAvailabilityForPlan(plan string) int {
	if NormalizePlanType(plan) == "free" {
		return 0
	}
	return UnknownOfficialImageAvailability
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
