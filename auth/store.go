package auth

import (
	"context"
	"fmt"
	"log"
	"math"
	"math/rand"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/codex2api/cache"
	"github.com/codex2api/database"
)

// AccountStatus 账号状态
type AccountStatus int

const (
	StatusReady    AccountStatus = iota // 可用
	StatusCooldown                      // 冷却中（被限速）
	StatusError                         // 不可用（RT 失效等）
)

const RateLimitedProbeInterval = 2 * time.Hour
const FullUsageProbeInterval = 4 * time.Hour
const rateLimitedWaitStep = 2 * time.Hour
const fullUsageWaitFallback = 12 * time.Hour

const (
	AutoCleanFullUsageModeOff    = "off"
	AutoCleanFullUsageModeDelete = "delete"
	AutoCleanFullUsageModeWait   = "wait"
)

// NormalizeAutoCleanFullUsageMode 规范化满用量处理模式
func NormalizeAutoCleanFullUsageMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case AutoCleanFullUsageModeDelete:
		return AutoCleanFullUsageModeDelete
	case AutoCleanFullUsageModeWait:
		return AutoCleanFullUsageModeWait
	default:
		return AutoCleanFullUsageModeOff
	}
}

// AccountHealthTier 账号健康层级（仅用于调度优先级，不直接暴露给外部 API）
type AccountHealthTier string

const (
	HealthTierHealthy AccountHealthTier = "healthy"
	HealthTierWarm    AccountHealthTier = "warm"
	HealthTierRisky   AccountHealthTier = "risky"
	HealthTierBanned  AccountHealthTier = "banned"
)

// Account 运行时账号状态
type Account struct {
	mu             sync.RWMutex
	DBID           int64 // 数据库 ID
	RefreshToken   string
	AccessToken    string
	ExpiresAt      time.Time
	AccountID      string
	Email          string
	PlanType       string
	ProxyURL       string
	PublicAPIKeyID int64
	Status         AccountStatus
	CooldownUtil   time.Time
	CooldownReason string // rate_limited / unauthorized / 空
	ErrorMsg       string

	// 用量进度（从 Codex 响应头被动解析）
	UsagePercent7d          float64 // 7d 窗口使用率 0-100+
	UsagePercent7dValid     bool
	Reset7dAt               time.Time // 7d 窗口重置时间
	UsagePercent5h          float64   // 5h 窗口使用率 0-100+
	UsagePercent5hValid     bool
	Reset5hAt               time.Time // 5h 窗口重置时间
	UsageUpdatedAt          time.Time
	usageProbeInFlight      bool
	imageQuotaProbeInFlight bool
	recoveryProbeInFlight   bool

	// 网页图片额度（ChatGPT Web）
	ImageWebRemaining      int
	ImageWebTotal          int
	ImageWebQuotaValid     bool
	ImageWebResetAt        time.Time
	ImageQuotaUpdatedAt    time.Time
	ImageOfficialAvailable int
	ImageOfficialKnown     bool

	// 调度健康信号
	HealthTier              AccountHealthTier
	SchedulerScore          float64
	DynamicConcurrencyLimit int64
	LatencyEWMA             float64
	SuccessStreak           int
	FailureStreak           int
	LastSuccessAt           time.Time
	LastFailureAt           time.Time
	LastUnauthorizedAt      time.Time
	LastRateLimitedAt       time.Time
	LastTimeoutAt           time.Time
	LastServerErrorAt       time.Time
	LastRecoveryProbeAt     time.Time
	LastRateLimitedProbeAt  time.Time
	LastFullUsageProbeAt    time.Time
	LastFailureStatusCode   int
	LastFailureCode         string
	LastFailureMessage      string

	// 滑动窗口成功率（最近 N 次请求）
	RecentResults    [20]uint8 // 1=成功, 0=失败
	RecentResultsIdx int       // 环形缓冲区写入位置
	RecentResultsCnt int       // 已记录数量（最大 20）

	// 高并发调度指标（原子操作，无需锁）
	ActiveRequests int64 // 当前并发请求数
	TotalRequests  int64 // 累计总请求数
	LastUsedAt     int64 // 最后使用时间（UnixNano）
	Disabled       int32 // 原子标志，1 = 立即不可调度（401 时瞬间置位，无需等锁）
	AddedAt        int64 // 加入号池的时间（UnixNano），用于过期清理
	Locked         int32 // 原子标志，1 = 锁定，自动清理跳过此账号

}

const (
	defaultBackgroundRefreshInterval = 2 * time.Minute
	defaultUsageProbeMaxAge          = 10 * time.Minute
	defaultImageQuotaProbeMaxAge     = 30 * time.Minute
	defaultRecoveryProbeInterval     = 30 * time.Minute
)

// SchedulerBreakdown 调度评分拆解
type SchedulerBreakdown struct {
	UnauthorizedPenalty float64
	RateLimitPenalty    float64
	TimeoutPenalty      float64
	ServerPenalty       float64
	FailurePenalty      float64
	SuccessBonus        float64
	ProvenBonus         float64 // 经过验证的账号（TotalRequests > 10）加分
	UsagePenalty7d      float64
	LatencyPenalty      float64
	SuccessRatePenalty  float64 // 滑动窗口成功率惩罚
}

// SchedulerDebugSnapshot 调度调试快照
type SchedulerDebugSnapshot struct {
	HealthTier              string
	SchedulerScore          float64
	DynamicConcurrencyLimit int64
	Breakdown               SchedulerBreakdown
	LastUnauthorizedAt      time.Time
	LastRateLimitedAt       time.Time
	LastTimeoutAt           time.Time
	LastServerErrorAt       time.Time
}

// ID 返回数据库 ID
func (a *Account) ID() int64 {
	return a.DBID
}

// Mu 返回读写锁（供外部包安全读取字段）
func (a *Account) Mu() *sync.RWMutex {
	return &a.mu
}

func clampInt(value, minValue, maxValue int) int {
	if value < minValue {
		return minValue
	}
	if value > maxValue {
		return maxValue
	}
	return value
}

// fastRandN 轻量级随机数（用于调度公平性，无需加密安全）
func fastRandN(n int) int {
	if n <= 0 {
		return 0
	}
	return rand.Intn(n)
}

func concurrencyLimitForTier(baseLimit int64, tier AccountHealthTier) int64 {
	if baseLimit <= 0 {
		baseLimit = 1
	}

	switch tier {
	case HealthTierHealthy:
		return baseLimit
	case HealthTierWarm:
		half := baseLimit / 2
		if half < 1 {
			return 1
		}
		return half
	case HealthTierRisky:
		return 1
	case HealthTierBanned:
		return 0
	default:
		if baseLimit >= 2 {
			return 2
		}
		return 1
	}
}

func tierPriority(tier AccountHealthTier) int {
	switch tier {
	case HealthTierHealthy:
		return 3
	case HealthTierWarm:
		return 2
	case HealthTierRisky:
		return 1
	default:
		return 0
	}
}

func (a *Account) healthTierLocked() AccountHealthTier {
	if a.HealthTier != "" {
		return a.HealthTier
	}
	if a.AccessToken != "" {
		return HealthTierHealthy
	}
	return HealthTierWarm
}

func (a *Account) recordLatencyLocked(latency time.Duration) {
	if latency <= 0 {
		return
	}

	latencyMs := float64(latency.Milliseconds())
	if latencyMs <= 0 {
		return
	}
	if a.LatencyEWMA == 0 {
		a.LatencyEWMA = latencyMs
		return
	}
	a.LatencyEWMA = a.LatencyEWMA*0.8 + latencyMs*0.2
}

// recordResultLocked 记录一次请求结果到滑动窗口（必须持有锁）
func (a *Account) recordResultLocked(success bool) {
	if success {
		a.RecentResults[a.RecentResultsIdx] = 1
	} else {
		a.RecentResults[a.RecentResultsIdx] = 0
	}
	a.RecentResultsIdx = (a.RecentResultsIdx + 1) % len(a.RecentResults)
	if a.RecentResultsCnt < len(a.RecentResults) {
		a.RecentResultsCnt++
	}
}

// recentSuccessRateLocked 计算滑动窗口成功率 (0.0 ~ 1.0)
func (a *Account) recentSuccessRateLocked() float64 {
	if a.RecentResultsCnt == 0 {
		return 1.0 // 无数据时返回 100%
	}
	var sum int
	for i := 0; i < a.RecentResultsCnt; i++ {
		sum += int(a.RecentResults[i])
	}
	return float64(sum) / float64(a.RecentResultsCnt)
}

func (a *Account) currentUsagePercentLocked() (float64, bool) {
	maxUsage := 0.0
	valid := false
	if a.UsagePercent7dValid {
		maxUsage = a.UsagePercent7d
		valid = true
	}
	if a.UsagePercent5hValid {
		if !valid || a.UsagePercent5h > maxUsage {
			maxUsage = a.UsagePercent5h
		}
		valid = true
	}
	return maxUsage, valid
}

func usageWindowFullLocked(valid bool, pct float64, resetAt time.Time, now time.Time) bool {
	if !valid || pct < 100.0 {
		return false
	}
	// 如果上游已经给出重置时间，且时间已过，则不要继续用旧快照把账号判定为满。
	if !resetAt.IsZero() && !resetAt.After(now) {
		return false
	}
	return true
}

func (a *Account) textQuotaFullLocked(now time.Time) bool {
	return usageWindowFullLocked(a.UsagePercent7dValid, a.UsagePercent7d, a.Reset7dAt, now) ||
		usageWindowFullLocked(a.UsagePercent5hValid, a.UsagePercent5h, a.Reset5hAt, now)
}

func (a *Account) imageQuotaExhaustedLocked(now time.Time) (exhausted bool, known bool) {
	webKnown := a.ImageWebQuotaValid
	webUsable := false
	if webKnown {
		if a.ImageWebTotal > 0 {
			if a.ImageWebRemaining > 0 {
				webUsable = true
			} else if !a.ImageWebResetAt.IsZero() && !a.ImageWebResetAt.After(now) {
				// 额度窗口已过期但后台探针尚未刷新时，允许图片链路重新探测。
				webUsable = true
			}
		}
	}

	officialKnown := a.ImageOfficialKnown
	officialAvailable := a.ImageOfficialAvailable
	if a.textQuotaFullLocked(now) {
		// 官方 API 图片生成消耗账号文本/Codex 总额度；文本额度已满时，官方图片也不可用。
		officialKnown = true
		officialAvailable = 0
	} else if NormalizePlanType(a.PlanType) == "free" {
		// free 官方图片次数按 0 处理；网页反代额度单独统计。
		officialKnown = true
		officialAvailable = 0
	}
	if officialKnown && officialAvailable < 0 {
		officialAvailable = 0
	}
	officialUsable := officialKnown && officialAvailable > 0

	if webUsable || officialUsable {
		return false, true
	}
	if !webKnown || !officialKnown {
		return false, false
	}
	return true, true
}

// usagePriorityBonusLocked 高用量优先调度：>80% 逐级加分，推动先消耗高进度账号。
func (a *Account) usagePriorityBonusLocked() float64 {
	usagePct, ok := a.currentUsagePercentLocked()
	if !ok {
		return 0
	}
	switch {
	case usagePct >= 98:
		return 50
	case usagePct >= 95:
		return 40
	case usagePct >= 90:
		return 30
	case usagePct >= 85:
		return 22
	case usagePct >= 80:
		return 15
	default:
		return 0
	}
}

// linearDecay 线性衰减：返回 base × max(0, 1 - elapsed/window)
func linearDecay(base float64, elapsed, window time.Duration) float64 {
	if elapsed >= window || window <= 0 {
		return 0
	}
	return base * (1.0 - float64(elapsed)/float64(window))
}

func (a *Account) schedulerBreakdownLocked() SchedulerBreakdown {
	now := time.Now()
	breakdown := SchedulerBreakdown{}

	// 线性衰减惩罚：随时间平滑更无突变
	if !a.LastUnauthorizedAt.IsZero() {
		elapsed := now.Sub(a.LastUnauthorizedAt)
		breakdown.UnauthorizedPenalty = linearDecay(50, elapsed, 24*time.Hour)
	}
	if !a.LastRateLimitedAt.IsZero() {
		elapsed := now.Sub(a.LastRateLimitedAt)
		breakdown.RateLimitPenalty = linearDecay(22, elapsed, time.Hour)
	}
	if !a.LastTimeoutAt.IsZero() {
		elapsed := now.Sub(a.LastTimeoutAt)
		breakdown.TimeoutPenalty = linearDecay(18, elapsed, 15*time.Minute)
	}
	if !a.LastServerErrorAt.IsZero() {
		elapsed := now.Sub(a.LastServerErrorAt)
		breakdown.ServerPenalty = linearDecay(12, elapsed, 15*time.Minute)
	}

	breakdown.FailurePenalty = float64(clampInt(a.FailureStreak*6, 0, 24))
	breakdown.SuccessBonus = float64(clampInt(a.SuccessStreak*2, 0, 12))

	// 经过验证的账号（累计请求 > 10 次）优先调度
	if atomic.LoadInt64(&a.TotalRequests) > 10 {
		breakdown.ProvenBonus = 20
	}

	// 滑动窗口成功率惩罚
	if a.RecentResultsCnt >= 5 { // 至少 5 次请求才统计
		rate := a.recentSuccessRateLocked()
		switch {
		case rate < 0.5:
			breakdown.SuccessRatePenalty = 15
		case rate < 0.75:
			breakdown.SuccessRatePenalty = 8
		}
	}

	usagePct, usageValid := a.currentUsagePercentLocked()
	if usageValid && usagePct >= 100 {
		breakdown.UsagePenalty7d = 40
	}

	switch {
	case a.LatencyEWMA >= 12000:
		breakdown.LatencyPenalty = 25
	case a.LatencyEWMA >= 8000:
		breakdown.LatencyPenalty = 18
	case a.LatencyEWMA >= 4000:
		breakdown.LatencyPenalty = 10
	case a.LatencyEWMA >= 2500:
		breakdown.LatencyPenalty = 5
	}

	return breakdown
}

func (a *Account) recomputeSchedulerLocked(baseLimit int64) {
	now := time.Now()
	breakdown := a.schedulerBreakdownLocked()
	usagePriorityBonus := a.usagePriorityBonusLocked()
	score := 100.0 -
		breakdown.UnauthorizedPenalty -
		breakdown.RateLimitPenalty -
		breakdown.TimeoutPenalty -
		breakdown.ServerPenalty -
		breakdown.FailurePenalty -
		breakdown.UsagePenalty7d -
		breakdown.LatencyPenalty -
		breakdown.SuccessRatePenalty +
		breakdown.SuccessBonus +
		breakdown.ProvenBonus +
		usagePriorityBonus

	tier := HealthTierHealthy
	switch {
	case score < 60:
		tier = HealthTierRisky
	case score < 85:
		tier = HealthTierWarm
	}

	if a.LastFailureAt.After(a.LastSuccessAt) && !a.LastFailureAt.IsZero() && tier == HealthTierHealthy {
		tier = HealthTierWarm
	}
	if !a.LastUnauthorizedAt.IsZero() && now.Sub(a.LastUnauthorizedAt) < 24*time.Hour && tier == HealthTierHealthy {
		tier = HealthTierWarm
	}
	if a.HealthTier == HealthTierBanned {
		tier = HealthTierBanned
	}

	a.HealthTier = tier
	a.SchedulerScore = score
	a.DynamicConcurrencyLimit = concurrencyLimitForTier(baseLimit, tier)
	// 高首包延迟账号自动收敛并发，避免把慢账号打得更慢。
	if a.LatencyEWMA >= 8000 && a.DynamicConcurrencyLimit > 1 {
		a.DynamicConcurrencyLimit = 1
	} else if a.LatencyEWMA >= 4000 && a.DynamicConcurrencyLimit > 2 {
		a.DynamicConcurrencyLimit = 2
	}
}

func (a *Account) schedulerSnapshot(baseLimit int64) (AccountHealthTier, float64, int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.recomputeSchedulerLocked(baseLimit)
	return a.HealthTier, a.SchedulerScore, a.DynamicConcurrencyLimit
}

// IsAvailable 检查账号是否可用
func (a *Account) IsAvailable() bool {
	// 原子标志优先：401 时瞬间置位，无需等锁即可拦截并发请求
	if atomic.LoadInt32(&a.Disabled) != 0 {
		return false
	}

	a.mu.RLock()
	defer a.mu.RUnlock()

	if a.Status == StatusError {
		return false
	}
	if a.healthTierLocked() == HealthTierBanned {
		return false
	}
	if a.AccessToken == "" {
		return false
	}
	now := time.Now()
	if a.Status == StatusCooldown {
		// rate_limited 必须等待测活成功后才放出，避免仅靠时间到点自动回池
		if a.CooldownReason == "rate_limited" {
			return false
		}
		if now.Before(a.CooldownUtil) {
			return false
		}
		// 非 rate_limited 冷却过期后自动恢复
		return true
	}
	return true
}

// IsTextAvailable 检查账号是否可用于文本/Codex 请求。
func (a *Account) IsTextAvailable() bool {
	if atomic.LoadInt32(&a.Disabled) != 0 {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	now := time.Now()
	if a.Status == StatusError || a.healthTierLocked() == HealthTierBanned || a.AccessToken == "" {
		return false
	}
	if a.Status == StatusCooldown {
		switch a.CooldownReason {
		case "rate_limited":
			return false
		default:
			if now.Before(a.CooldownUtil) {
				return false
			}
		}
	}
	return !a.textQuotaFullLocked(now)
}

// CanAttemptImageGeneration 检查账号是否可尝试图片链路。
func (a *Account) CanAttemptImageGeneration() bool {
	if atomic.LoadInt32(&a.Disabled) != 0 {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	now := time.Now()
	if a.Status == StatusError || a.healthTierLocked() == HealthTierBanned || a.AccessToken == "" {
		return false
	}
	if a.Status == StatusCooldown {
		switch a.CooldownReason {
		case "rate_limited", "unauthorized":
			return false
		case "full_usage":
			// full_usage 只代表文本额度等待；图片额度可用时仍允许调度。
		default:
			if now.Before(a.CooldownUtil) {
				return false
			}
		}
	}
	imageExhausted, imageKnown := a.imageQuotaExhaustedLocked(now)
	return !imageKnown || !imageExhausted
}

// ImageRuntimeStatus 返回独立图片状态：active / full_usage。
func (a *Account) ImageRuntimeStatus() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	exhausted, known := a.imageQuotaExhaustedLocked(time.Now())
	if known && exhausted {
		return "full_usage"
	}
	return "active"
}

// NeedsRefresh 检查 AT 是否需要刷新（过期前 5 分钟刷新）
func (a *Account) NeedsRefresh() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return time.Until(a.ExpiresAt) < 5*time.Minute
}

// SetCooldown 设置冷却时间
func (a *Account) SetCooldown(duration time.Duration) {
	a.SetCooldownUntil(time.Now().Add(duration), "")
}

// SetCooldownWithReason 设置冷却时间（带原因）
func (a *Account) SetCooldownWithReason(duration time.Duration, reason string) {
	a.SetCooldownUntil(time.Now().Add(duration), reason)
}

// SetCooldownUntil 设置冷却结束时间（带原因）
func (a *Account) SetCooldownUntil(until time.Time, reason string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.Status = StatusCooldown
	a.CooldownUtil = until
	a.CooldownReason = reason
	switch reason {
	case "unauthorized":
		a.HealthTier = HealthTierBanned
	case "rate_limited":
		if a.healthTierLocked() == HealthTierHealthy {
			a.HealthTier = HealthTierWarm
		} else {
			a.HealthTier = HealthTierRisky
		}
	default:
		if a.HealthTier == "" {
			a.HealthTier = HealthTierWarm
		}
	}
}

// GetCooldownReason 获取冷却原因
func (a *Account) GetCooldownReason() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.CooldownReason
}

// HasActiveCooldown 检查账号是否仍处于冷却期
func (a *Account) HasActiveCooldown() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.Status == StatusCooldown && time.Now().Before(a.CooldownUtil)
}

// GetCooldownSnapshot 获取冷却快照（时间、原因、是否处于有效冷却）
func (a *Account) GetCooldownSnapshot() (time.Time, string, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	active := a.Status == StatusCooldown && time.Now().Before(a.CooldownUtil)
	return a.CooldownUtil, a.CooldownReason, active
}

// GetProbeTimestamps 获取最近的等待态测活时间戳
func (a *Account) GetProbeTimestamps() (rateLimitedAt time.Time, fullUsageAt time.Time) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.LastRateLimitedProbeAt, a.LastFullUsageProbeAt
}

// SetLastFailureDetail 设置最近一次失败详情（用于前端展示）
func (a *Account) SetLastFailureDetail(statusCode int, code string, message string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.LastFailureStatusCode = statusCode
	a.LastFailureCode = strings.TrimSpace(code)
	a.LastFailureMessage = strings.TrimSpace(message)
}

// ClearLastFailureDetail 清空最近一次失败详情
func (a *Account) ClearLastFailureDetail() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.LastFailureStatusCode = 0
	a.LastFailureCode = ""
	a.LastFailureMessage = ""
}

// GetLastFailureDetail 获取最近一次失败详情
func (a *Account) GetLastFailureDetail() (statusCode int, code string, message string) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.LastFailureStatusCode, a.LastFailureCode, a.LastFailureMessage
}

// IsBanned 检查账号是否处于强隔离状态
func (a *Account) IsBanned() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.healthTierLocked() == HealthTierBanned
}

// RuntimeStatus 返回运行时状态字符串（供 admin API 使用）
func (a *Account) RuntimeStatus() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.healthTierLocked() == HealthTierBanned {
		return "unauthorized"
	}
	now := time.Now()
	switch a.Status {
	case StatusError:
		return "error"
	case StatusCooldown:
		if a.CooldownReason == "rate_limited" {
			return "rate_limited"
		}
		if now.Before(a.CooldownUtil) {
			if a.CooldownReason != "" {
				return a.CooldownReason
			}
			return "cooldown"
		}
		return "active" // 冷却过期，已恢复
	default:
		if a.AccessToken != "" {
			return "active"
		}
		return "error"
	}
}

// SetUsagePercent7d 更新 7d 用量百分比
func (a *Account) SetUsagePercent7d(pct float64) {
	a.SetUsageSnapshot(pct, time.Now())
}

// SetUsageSnapshot 更新用量快照及时间
func (a *Account) SetUsageSnapshot(pct float64, updatedAt time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.UsagePercent7d = pct
	a.UsagePercent7dValid = true
	a.UsageUpdatedAt = updatedAt
}

// GetUsagePercent7d 获取 7d 用量百分比
func (a *Account) GetUsagePercent7d() (float64, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.UsagePercent7d, a.UsagePercent7dValid
}

// SetUsageSnapshot5h 更新 5h 用量快照
func (a *Account) SetUsageSnapshot5h(pct float64, resetAt time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.UsagePercent5h = pct
	a.UsagePercent5hValid = true
	a.Reset5hAt = resetAt
}

// GetUsagePercent5h 获取 5h 用量百分比
func (a *Account) GetUsagePercent5h() (float64, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.UsagePercent5h, a.UsagePercent5hValid
}

// SetImageQuotaSnapshot 更新网页图片额度快照。
func (a *Account) SetImageQuotaSnapshot(remaining, total int, resetAt, updatedAt time.Time, officialAvailable int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.ImageWebRemaining = remaining
	a.ImageWebTotal = total
	a.ImageWebResetAt = resetAt
	a.ImageQuotaUpdatedAt = updatedAt
	a.ImageWebQuotaValid = remaining >= 0 && total >= 0
	a.ImageOfficialAvailable = officialAvailable
	a.ImageOfficialKnown = officialAvailable >= 0
}

// GetImageQuotaSnapshot 获取网页图片额度快照。
func (a *Account) GetImageQuotaSnapshot() (remaining, total int, resetAt time.Time, officialAvailable int, valid bool, updatedAt time.Time) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.ImageWebRemaining, a.ImageWebTotal, a.ImageWebResetAt, a.ImageOfficialAvailable, a.ImageWebQuotaValid, a.ImageQuotaUpdatedAt
}

// SetOfficialImageAvailability 更新官方图片链路剩余次数。
// known=false 表示当前没有任何可信的上游显式值。
func (a *Account) SetOfficialImageAvailability(available int, known bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !known {
		a.ImageOfficialAvailable = UnknownOfficialImageAvailability
		a.ImageOfficialKnown = false
		return
	}
	if available < 0 {
		available = 0
	}
	a.ImageOfficialAvailable = available
	a.ImageOfficialKnown = true
}

// GetOfficialImageAvailability 获取官方图片链路剩余次数。
// 仅当上游显式给出，或已知 free 不支持时才返回 known=true。
func (a *Account) GetOfficialImageAvailability() (available int, known bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.ImageOfficialKnown {
		return a.ImageOfficialAvailable, true
	}
	if NormalizePlanType(a.PlanType) == "free" {
		return 0, true
	}
	return 0, false
}

// DecrementImageWebQuota 成功生成后立刻扣减网页图片剩余额度。
func (a *Account) DecrementImageWebQuota(delta int) {
	if delta <= 0 {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.ImageWebQuotaValid {
		return
	}
	a.ImageWebRemaining -= delta
	if a.ImageWebRemaining < 0 {
		a.ImageWebRemaining = 0
	}
	a.ImageQuotaUpdatedAt = time.Now().UTC()
}

// ClearUsageCache 清除内存中的用量缓存，下次请求时从上游重新获取
func (a *Account) ClearUsageCache() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.UsagePercent7d = 0
	a.UsagePercent7dValid = false
	a.Reset7dAt = time.Time{}
	a.UsagePercent5h = 0
	a.UsagePercent5hValid = false
	a.Reset5hAt = time.Time{}
	a.UsageUpdatedAt = time.Time{}
}

// SetReset7dAt 设置 7d 窗口重置时间
func (a *Account) SetReset7dAt(t time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.Reset7dAt = t
}

// GetReset5hAt 获取 5h 窗口重置时间
func (a *Account) GetReset5hAt() time.Time {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.Reset5hAt
}

// GetReset7dAt 获取 7d 窗口重置时间
func (a *Account) GetReset7dAt() time.Time {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.Reset7dAt
}

// GetPlanType 获取账号套餐类型
func (a *Account) GetPlanType() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return NormalizePlanType(a.PlanType)
}

// GetHealthTier 获取当前健康层级
func (a *Account) GetHealthTier() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return string(a.HealthTier)
}

// GetSchedulerScore 获取当前调度分
func (a *Account) GetSchedulerScore() float64 {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.SchedulerScore
}

// GetDynamicConcurrencyLimit 获取当前动态并发上限
func (a *Account) GetDynamicConcurrencyLimit() int64 {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.DynamicConcurrencyLimit
}

// GetSchedulerDebugSnapshot 获取调度调试快照
func (a *Account) GetSchedulerDebugSnapshot(baseLimit int64) SchedulerDebugSnapshot {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.recomputeSchedulerLocked(baseLimit)
	return SchedulerDebugSnapshot{
		HealthTier:              string(a.HealthTier),
		SchedulerScore:          a.SchedulerScore,
		DynamicConcurrencyLimit: a.DynamicConcurrencyLimit,
		Breakdown:               a.schedulerBreakdownLocked(),
		LastUnauthorizedAt:      a.LastUnauthorizedAt,
		LastRateLimitedAt:       a.LastRateLimitedAt,
		LastTimeoutAt:           a.LastTimeoutAt,
		LastServerErrorAt:       a.LastServerErrorAt,
	}
}

// NeedsUsageProbe 判断是否需要主动探针刷新用量
func (a *Account) NeedsUsageProbe(maxAge time.Duration) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()

	if a.usageProbeInFlight || a.AccessToken == "" || a.Status == StatusError {
		return false
	}
	if a.Status == StatusCooldown && a.CooldownReason == "unauthorized" {
		return false
	}
	if a.Status == StatusCooldown && a.CooldownReason == "rate_limited" {
		if time.Now().Before(a.CooldownUtil) {
			if !a.LastRateLimitedProbeAt.IsZero() && time.Since(a.LastRateLimitedProbeAt) < RateLimitedProbeInterval {
				return false
			}
			return true
		}
		return true
	}
	if a.Status == StatusCooldown && a.CooldownReason == "full_usage" {
		if time.Now().Before(a.CooldownUtil) {
			if !a.LastFullUsageProbeAt.IsZero() && time.Since(a.LastFullUsageProbeAt) < FullUsageProbeInterval {
				return false
			}
			return true
		}
		return true
	}
	if !a.UsagePercent7dValid || a.UsageUpdatedAt.IsZero() {
		return true
	}
	return time.Since(a.UsageUpdatedAt) > maxAge
}

// TryBeginUsageProbe 尝试开始一次用量探针
func (a *Account) TryBeginUsageProbe() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.usageProbeInFlight {
		return false
	}
	a.usageProbeInFlight = true
	if a.Status == StatusCooldown {
		switch a.CooldownReason {
		case "rate_limited":
			a.LastRateLimitedProbeAt = time.Now()
		case "full_usage":
			a.LastFullUsageProbeAt = time.Now()
		}
	}
	return true
}

// FinishUsageProbe 结束一次用量探针
func (a *Account) FinishUsageProbe() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.usageProbeInFlight = false
}

// NeedsImageQuotaProbe 判断是否需要刷新网页图片额度。
func (a *Account) NeedsImageQuotaProbe(maxAge time.Duration) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()

	if a.imageQuotaProbeInFlight || a.AccessToken == "" || a.Status == StatusError {
		return false
	}
	if a.Status == StatusCooldown && a.CooldownReason == "unauthorized" {
		return false
	}
	if !a.ImageWebQuotaValid || a.ImageQuotaUpdatedAt.IsZero() {
		return true
	}
	return time.Since(a.ImageQuotaUpdatedAt) > maxAge
}

// TryBeginImageQuotaProbe 尝试开始一次网页图片额度探针。
func (a *Account) TryBeginImageQuotaProbe() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.imageQuotaProbeInFlight {
		return false
	}
	a.imageQuotaProbeInFlight = true
	return true
}

// FinishImageQuotaProbe 结束一次网页图片额度探针。
func (a *Account) FinishImageQuotaProbe() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.imageQuotaProbeInFlight = false
}

// NeedsRecoveryProbe 判断是否需要对被封禁账号做低频恢复探测
func (a *Account) NeedsRecoveryProbe(minInterval time.Duration) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()

	if a.recoveryProbeInFlight || a.healthTierLocked() != HealthTierBanned {
		return false
	}
	if a.RefreshToken == "" {
		return false
	}
	if a.Status == StatusCooldown && time.Now().Before(a.CooldownUtil) {
		return false
	}
	if !a.LastRecoveryProbeAt.IsZero() && time.Since(a.LastRecoveryProbeAt) < minInterval {
		return false
	}
	return true
}

// TryBeginRecoveryProbe 尝试开始一次恢复探测
func (a *Account) TryBeginRecoveryProbe() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.recoveryProbeInFlight {
		return false
	}
	a.recoveryProbeInFlight = true
	a.LastRecoveryProbeAt = time.Now()
	return true
}

// FinishRecoveryProbe 结束一次恢复探测
func (a *Account) FinishRecoveryProbe() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.recoveryProbeInFlight = false
}

// GetActiveRequests 获取当前并发数
func (a *Account) GetActiveRequests() int64 {
	return atomic.LoadInt64(&a.ActiveRequests)
}

// GetTotalRequests 获取累计请求数
func (a *Account) GetTotalRequests() int64 {
	return atomic.LoadInt64(&a.TotalRequests)
}

// GetLastUsedAt 获取最后使用时间
func (a *Account) GetLastUsedAt() time.Time {
	nano := atomic.LoadInt64(&a.LastUsedAt)
	if nano == 0 {
		return time.Time{}
	}
	return time.Unix(0, nano)
}

// Store 多账号管理器（数据库 + Token 缓存）
type Store struct {
	mu                        sync.RWMutex
	accounts                  []*Account
	globalProxy               string
	maxConcurrency            int64        // 每账号最大并发数
	testConcurrency           int64        // 批量测试并发数
	testModel                 atomic.Value // 测试连接使用的模型（string）
	db                        *database.DB
	tokenCache                cache.TokenCache
	usageProbeMu              sync.RWMutex
	usageProbe                func(context.Context, *Account) error
	usageProbeBatch           atomic.Bool
	imageQuotaProbeMu         sync.RWMutex
	imageQuotaProbe           func(context.Context, *Account) error
	imageQuotaProbeBatch      atomic.Bool
	recoveryProbeBatch        atomic.Bool
	autoCleanUnauthorized     atomic.Bool
	autoCleanRateLimited      atomic.Bool
	autoCleanFullUsageMode    atomic.Value // string: off / delete / wait
	autoCleanError            atomic.Bool
	autoCleanExpired          atomic.Bool
	autoCleanupBatch          atomic.Bool
	backgroundRefreshInterval int64 // 后台刷新/探针巡检间隔（ns）
	usageProbeMaxAge          int64 // 用量探针快照最大缓存时长（ns）
	imageQuotaProbeMaxAge     int64 // 网页图片额度探针快照最大缓存时长（ns）
	recoveryProbeInterval     int64 // 恢复探测最小间隔（ns）
	plusPortEnabled           atomic.Bool
	plusPortAccessFree        atomic.Bool
	imageRoutePriority        atomic.Value // string: official_first / web_first
	preferredPlanType         atomic.Value // string: free/plus/pro_5x/pro_20x/team/enterprise 或空
	preferredPlanBonus        int64        // 指定套餐额外调度加分
	modelMapping              atomic.Value // string: {"anthropic_model":"codex_model", ...}
	maxRetries                int64        // 请求失败最大重试次数（换号重试）
	publicInitialCreditX1e4   int64        // 公开上传账号初始入账金额（美元，放大 1e4）
	publicFullCreditX1e4      int64        // 公开上传账号满额度总金额（美元，放大 1e4）
	backgroundRefreshWakeCh   chan struct{}
	stopCh                    chan struct{}
	wg                        sync.WaitGroup

	// 代理池
	proxyPool        []string // 已启用的代理 URL 列表
	proxyPoolEnabled bool     // 代理池是否开启
	proxyRoundRobin  uint64   // 轮询计数器

	// Fast scheduler POC（默认关闭，通过环境变量启用）
	fastScheduler        atomic.Pointer[FastScheduler]
	fastSchedulerEnabled atomic.Bool

	// 智能刷新调度器
	refreshScheduler atomic.Pointer[RefreshSchedulerIntegration]

	allowRemoteMigration atomic.Bool // 是否允许远程迁移拉取账号
	sessionMu            sync.RWMutex
	sessionBindings      map[string]sessionAffinity
}

// AccountMatcher 用于在调度阶段对账号做额外过滤。
// 返回 true 表示该账号允许参与本次调度。
type AccountMatcher func(*Account) bool

type sessionAffinity struct {
	accountID int64
	proxyURL  string
	expiresAt time.Time
}

const sessionAffinityTTL = 30 * time.Minute

func fastSchedulerEnabledFromEnv() bool {
	for _, key := range []string{"FAST_SCHEDULER_ENABLED", "CODEX_FAST_SCHEDULER"} {
		if truthyEnv(os.Getenv(key)) {
			return true
		}
	}
	return false
}

func truthyEnv(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on", "enable", "enabled":
		return true
	default:
		return false
	}
}

func normalizePreferredPlanType(plan string) string {
	trimmed := strings.ToLower(strings.TrimSpace(plan))
	switch trimmed {
	case "prolite", "pro_lite", "pro-lite", "pro5x", "pro_5x", "pro-5x":
		return "pro_5x"
	case "pro", "pro20x", "pro_20x", "pro-20x":
		return "pro_20x"
	default:
		normalized := NormalizePlanType(trimmed)
		switch normalized {
		case "free", "plus", "team", "enterprise":
			return normalized
		default:
			return ""
		}
	}
}

const (
	ImageRoutePriorityOfficialFirst = "official_first"
	ImageRoutePriorityWebFirst      = "web_first"
)

func NormalizeImageRoutePriority(priority string) string {
	switch strings.ToLower(strings.TrimSpace(priority)) {
	case ImageRoutePriorityWebFirst, "web", "webimage", "web_image", "reverse", "reverse_proxy":
		return ImageRoutePriorityWebFirst
	case ImageRoutePriorityOfficialFirst, "official", "api", "official_api":
		return ImageRoutePriorityOfficialFirst
	default:
		return ImageRoutePriorityOfficialFirst
	}
}

func clampPreferredPlanBonus(bonus int) int {
	if bonus < 0 {
		return 0
	}
	if bonus > 200 {
		return 200
	}
	return bonus
}

func matchPreferredPlan(plan string, preferred string) bool {
	preferred = normalizePreferredPlanType(preferred)
	if preferred == "" {
		return false
	}
	switch preferred {
	case "pro_5x":
		return PlanVariant(plan) == "pro_5x"
	case "pro_20x":
		return PlanVariant(plan) == "pro_20x"
	}
	return NormalizePlanType(plan) == preferred
}

func moneyToScaled(value float64) int64 {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return 0
	}
	if value < 0 {
		value = 0
	}
	return int64(math.Round(value * 10000))
}

func scaledToMoney(value int64) float64 {
	return float64(value) / 10000
}

func normalizePublicCredit(initial, full float64) (float64, float64) {
	if initial <= 0 {
		initial = 0.1
	}
	if full <= 0 {
		full = 2
	}
	if full < initial {
		full = initial
	}
	return initial, full
}

// NewStore 创建账号管理器
func NewStore(db *database.DB, tc cache.TokenCache, settings *database.SystemSettings) *Store {
	if settings == nil {
		settings = &database.SystemSettings{
			MaxConcurrency:                   2,
			TestConcurrency:                  50,
			TestModel:                        "gpt-5.4",
			BackgroundRefreshIntervalMinutes: 2,
			UsageProbeMaxAgeMinutes:          10,
			RecoveryProbeIntervalMinutes:     30,
			ProxyURL:                         "",
			AutoCleanFullUsageMode:           AutoCleanFullUsageModeOff,
			PlusPortAccessFree:               true,
			ImageRoutePriority:               ImageRoutePriorityOfficialFirst,
		}
	}
	s := &Store{
		globalProxy:             settings.ProxyURL,
		maxConcurrency:          int64(settings.MaxConcurrency),
		testConcurrency:         int64(settings.TestConcurrency),
		db:                      db,
		tokenCache:              tc,
		backgroundRefreshWakeCh: make(chan struct{}, 1),
		stopCh:                  make(chan struct{}),
		proxyPoolEnabled:        settings.ProxyPoolEnabled,
		sessionBindings:         make(map[string]sessionAffinity),
	}
	s.testModel.Store(settings.TestModel)
	s.SetBackgroundRefreshInterval(time.Duration(settings.BackgroundRefreshIntervalMinutes) * time.Minute)
	s.SetUsageProbeMaxAge(time.Duration(settings.UsageProbeMaxAgeMinutes) * time.Minute)
	s.SetImageQuotaProbeMaxAge(defaultImageQuotaProbeMaxAge)
	s.SetRecoveryProbeInterval(time.Duration(settings.RecoveryProbeIntervalMinutes) * time.Minute)
	s.autoCleanUnauthorized.Store(settings.AutoCleanUnauthorized)
	s.autoCleanRateLimited.Store(settings.AutoCleanRateLimited)
	fullUsageMode := NormalizeAutoCleanFullUsageMode(settings.AutoCleanFullUsageMode)
	if fullUsageMode == AutoCleanFullUsageModeOff && settings.AutoCleanFullUsage {
		fullUsageMode = AutoCleanFullUsageModeDelete
	}
	s.autoCleanFullUsageMode.Store(fullUsageMode)
	s.autoCleanError.Store(settings.AutoCleanError)
	s.autoCleanExpired.Store(settings.AutoCleanExpired)
	s.plusPortEnabled.Store(settings.PlusPortEnabled)
	s.plusPortAccessFree.Store(settings.PlusPortAccessFree)
	s.imageRoutePriority.Store(NormalizeImageRoutePriority(settings.ImageRoutePriority))
	s.preferredPlanType.Store(normalizePreferredPlanType(settings.SchedulerPreferredPlan))
	atomic.StoreInt64(&s.preferredPlanBonus, int64(clampPreferredPlanBonus(settings.SchedulerPlanBonus)))
	s.modelMapping.Store(settings.ModelMapping)
	retries := int64(settings.MaxRetries)
	if retries <= 0 {
		retries = 2 // 默认重试 2 次
	}
	atomic.StoreInt64(&s.maxRetries, retries)
	initialCredit, fullCredit := normalizePublicCredit(settings.PublicInitialCreditUSD, settings.PublicFullCreditUSD)
	atomic.StoreInt64(&s.publicInitialCreditX1e4, moneyToScaled(initialCredit))
	atomic.StoreInt64(&s.publicFullCreditX1e4, moneyToScaled(fullCredit))
	s.allowRemoteMigration.Store(settings.AllowRemoteMigration)
	// 环境变量优先，否则读数据库设置
	fastEnabled := fastSchedulerEnabledFromEnv() || settings.FastSchedulerEnabled
	s.fastSchedulerEnabled.Store(fastEnabled)
	if fastEnabled {
		s.fastScheduler.Store(NewFastScheduler(
			int64(settings.MaxConcurrency),
			s.GetPreferredPlanType(),
			float64(s.GetPreferredPlanBonus()),
		))
		log.Printf("快速调度器已启用（请求热路径将优先走本地内存调度器）")
	}

	// 加载代理池
	if settings.ProxyPoolEnabled {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if proxies, err := db.ListEnabledProxies(ctx); err == nil {
			urls := make([]string, 0, len(proxies))
			for _, p := range proxies {
				urls = append(urls, p.URL)
			}
			s.proxyPool = urls
			log.Printf("代理池已加载: %d 个活跃代理", len(urls))
		}
	}

	return s
}

func (s *Store) getFastScheduler() *FastScheduler {
	if s == nil || !s.fastSchedulerEnabled.Load() {
		return nil
	}
	return s.fastScheduler.Load()
}

func (s *Store) rebuildFastScheduler() {
	if s == nil || !s.fastSchedulerEnabled.Load() {
		return
	}
	s.fastScheduler.Store(s.BuildFastScheduler())
}

func (s *Store) recomputeAllAccountSchedulerState() {
	if s == nil {
		return
	}
	baseLimit := atomic.LoadInt64(&s.maxConcurrency)
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, acc := range s.accounts {
		if acc == nil {
			continue
		}
		acc.mu.Lock()
		acc.recomputeSchedulerLocked(baseLimit)
		acc.mu.Unlock()
	}
}

func (s *Store) fastSchedulerUpdate(acc *Account) {
	if s == nil || acc == nil {
		return
	}
	scheduler := s.getFastScheduler()
	if scheduler == nil {
		return
	}
	scheduler.Update(acc)
}

func (s *Store) fastSchedulerRemove(dbID int64) {
	if s == nil || dbID == 0 {
		return
	}
	scheduler := s.getFastScheduler()
	if scheduler == nil {
		return
	}
	scheduler.Remove(dbID)
}

func (s *Store) SetFastSchedulerEnabled(enabled bool) {
	if s == nil {
		return
	}
	s.fastSchedulerEnabled.Store(enabled)
	if enabled {
		s.recomputeAllAccountSchedulerState()
		s.rebuildFastScheduler()
		return
	}
	s.fastScheduler.Store(nil)
}

func (s *Store) FastSchedulerEnabled() bool {
	if s == nil {
		return false
	}
	return s.fastSchedulerEnabled.Load()
}

// SetPlusPortEnabled 设置 plus 端口开关（需重启进程后生效监听）。
func (s *Store) SetPlusPortEnabled(enabled bool) {
	if s == nil {
		return
	}
	s.plusPortEnabled.Store(enabled)
}

// GetPlusPortEnabled 获取 plus 端口开关。
func (s *Store) GetPlusPortEnabled() bool {
	if s == nil {
		return false
	}
	return s.plusPortEnabled.Load()
}

// SetPlusPortAccessFree 设置 plus 端口是否可访问 free 套餐账号。
func (s *Store) SetPlusPortAccessFree(enabled bool) {
	if s == nil {
		return
	}
	s.plusPortAccessFree.Store(enabled)
}

// GetPlusPortAccessFree 获取 plus 端口是否可访问 free 套餐账号。
func (s *Store) GetPlusPortAccessFree() bool {
	if s == nil {
		return true
	}
	return s.plusPortAccessFree.Load()
}

// SetImageRoutePriority 设置 Plus 端口图片调度优先级。
func (s *Store) SetImageRoutePriority(priority string) {
	if s == nil {
		return
	}
	s.imageRoutePriority.Store(NormalizeImageRoutePriority(priority))
}

// GetImageRoutePriority 获取 Plus 端口图片调度优先级。
func (s *Store) GetImageRoutePriority() string {
	if s == nil {
		return ImageRoutePriorityOfficialFirst
	}
	v := s.imageRoutePriority.Load()
	priority, _ := v.(string)
	return NormalizeImageRoutePriority(priority)
}

// GetPreferredPlanType 获取指定套餐优先调度配置
func (s *Store) GetPreferredPlanType() string {
	if s == nil {
		return ""
	}
	v := s.preferredPlanType.Load()
	plan, _ := v.(string)
	return normalizePreferredPlanType(plan)
}

// GetPreferredPlanBonus 获取指定套餐额外调度加分
func (s *Store) GetPreferredPlanBonus() int {
	if s == nil {
		return 0
	}
	return int(atomic.LoadInt64(&s.preferredPlanBonus))
}

// SetPreferredPlanPriority 设置指定套餐额外调度加分
func (s *Store) SetPreferredPlanPriority(plan string, bonus int) {
	if s == nil {
		return
	}
	normalizedPlan := normalizePreferredPlanType(plan)
	clampedBonus := clampPreferredPlanBonus(bonus)
	s.preferredPlanType.Store(normalizedPlan)
	atomic.StoreInt64(&s.preferredPlanBonus, int64(clampedBonus))
	// 快速调度器需要按新的 score 重新排序。
	s.rebuildFastScheduler()
}

func (s *Store) schedulerPlanBonus(planType string) float64 {
	if s == nil {
		return 0
	}
	preferred := s.GetPreferredPlanType()
	bonus := s.GetPreferredPlanBonus()
	if preferred == "" || bonus <= 0 {
		return 0
	}
	if matchPreferredPlan(planType, preferred) {
		return float64(bonus)
	}
	return 0
}

func (s *Store) isPreferredPlanEnabled() bool {
	if s == nil {
		return false
	}
	return s.GetPreferredPlanType() != ""
}

func (s *Store) isPreferredPlan(planType string) bool {
	if s == nil {
		return false
	}
	preferred := s.GetPreferredPlanType()
	if preferred == "" {
		return false
	}
	return matchPreferredPlan(planType, preferred)
}

// GetProxyURL 获取全局代理地址
func (s *Store) GetProxyURL() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.globalProxy
}

// SetProxyURL 更新全局代理地址
func (s *Store) SetProxyURL(url string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.globalProxy = url
}

// NextProxy 轮询获取下一个代理 URL
func (s *Store) NextProxy() string {
	s.mu.RLock()
	enabled := s.proxyPoolEnabled
	pool := s.proxyPool
	s.mu.RUnlock()

	if !enabled || len(pool) == 0 {
		return s.GetProxyURL() // fallback 全局单代理
	}
	start := int(atomic.AddUint64(&s.proxyRoundRobin, 1)-1) % len(pool)
	for i := 0; i < len(pool); i++ {
		if proxy := strings.TrimSpace(pool[(start+i)%len(pool)]); proxy != "" {
			return proxy
		}
	}
	return s.GetProxyURL()
}

// ResolveProxyForAccount 返回账号内部调用应使用的有效代理。
// 优先级：账号自带代理 > 代理池粘性代理 > 全局代理 > 直连。
func (s *Store) ResolveProxyForAccount(acc *Account) string {
	if s == nil {
		return ""
	}

	var accountID int64
	if acc != nil {
		acc.mu.RLock()
		accountID = acc.DBID
		if proxy := strings.TrimSpace(acc.ProxyURL); proxy != "" {
			acc.mu.RUnlock()
			return proxy
		}
		acc.mu.RUnlock()
	}

	return s.resolveFallbackProxyForAccount(accountID)
}

func (s *Store) resolveFallbackProxyForAccount(accountID int64) string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.proxyPoolEnabled && len(s.proxyPool) > 0 {
		start := stickyProxyIndex(accountID, len(s.proxyPool))
		for i := 0; i < len(s.proxyPool); i++ {
			if proxy := strings.TrimSpace(s.proxyPool[(start+i)%len(s.proxyPool)]); proxy != "" {
				return proxy
			}
		}
	}

	return strings.TrimSpace(s.globalProxy)
}

func stickyProxyIndex(accountID int64, poolSize int) int {
	if poolSize <= 1 || accountID <= 0 {
		return 0
	}
	return int((accountID - 1) % int64(poolSize))
}

// GetProxyPoolEnabled 获取代理池开关状态
func (s *Store) GetProxyPoolEnabled() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.proxyPoolEnabled
}

// SetProxyPoolEnabled 设置代理池开关
func (s *Store) SetProxyPoolEnabled(enabled bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.proxyPoolEnabled = enabled
}

// ReloadProxyPool 从数据库重新加载代理池
func (s *Store) ReloadProxyPool() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	proxies, err := s.db.ListEnabledProxies(ctx)
	if err != nil {
		return err
	}
	urls := make([]string, 0, len(proxies))
	for _, p := range proxies {
		urls = append(urls, p.URL)
	}
	s.mu.Lock()
	s.proxyPool = urls
	s.mu.Unlock()
	log.Printf("代理池已重新加载: %d 个活跃代理", len(urls))
	return nil
}

// GetAutoCleanUnauthorized 获取是否自动清理 401 账号
func (s *Store) GetAutoCleanUnauthorized() bool {
	return s.autoCleanUnauthorized.Load()
}

// SetAutoCleanUnauthorized 设置是否自动清理 401 账号
func (s *Store) SetAutoCleanUnauthorized(enabled bool) {
	s.autoCleanUnauthorized.Store(enabled)
}

// GetAutoCleanRateLimited 获取是否自动清理 429 账号
func (s *Store) GetAutoCleanRateLimited() bool {
	return s.autoCleanRateLimited.Load()
}

// SetAutoCleanRateLimited 设置是否自动清理 429 账号
func (s *Store) SetAutoCleanRateLimited(enabled bool) {
	s.autoCleanRateLimited.Store(enabled)
}

// GetAutoCleanFullUsageMode 获取满用量账号处理模式（off / delete / wait）
func (s *Store) GetAutoCleanFullUsageMode() string {
	v := s.autoCleanFullUsageMode.Load()
	mode, ok := v.(string)
	if !ok {
		return AutoCleanFullUsageModeOff
	}
	return NormalizeAutoCleanFullUsageMode(mode)
}

// SetAutoCleanFullUsageMode 设置满用量账号处理模式
func (s *Store) SetAutoCleanFullUsageMode(mode string) {
	s.autoCleanFullUsageMode.Store(NormalizeAutoCleanFullUsageMode(mode))
}

// GetAutoCleanFullUsage 获取是否启用满用量账号处理（兼容旧版布尔开关）
func (s *Store) GetAutoCleanFullUsage() bool {
	return s.GetAutoCleanFullUsageMode() != AutoCleanFullUsageModeOff
}

// SetAutoCleanFullUsage 设置是否启用满用量账号处理（兼容旧版布尔开关）
func (s *Store) SetAutoCleanFullUsage(enabled bool) {
	if enabled {
		s.SetAutoCleanFullUsageMode(AutoCleanFullUsageModeDelete)
		return
	}
	s.SetAutoCleanFullUsageMode(AutoCleanFullUsageModeOff)
}

// GetAutoCleanError 获取是否自动清理 error 账号
func (s *Store) GetAutoCleanError() bool {
	return s.autoCleanError.Load()
}

// SetAutoCleanError 设置是否自动清理 error 账号
func (s *Store) SetAutoCleanError(enabled bool) {
	s.autoCleanError.Store(enabled)
}

// GetAutoCleanExpired 获取是否自动清理过期账号
func (s *Store) GetAutoCleanExpired() bool {
	return s.autoCleanExpired.Load()
}

// SetAutoCleanExpired 设置是否自动清理过期账号
func (s *Store) SetAutoCleanExpired(enabled bool) {
	s.autoCleanExpired.Store(enabled)
}

// SetBackgroundRefreshInterval 设置后台刷新/探针巡检间隔。
func (s *Store) SetBackgroundRefreshInterval(d time.Duration) {
	if d <= 0 {
		d = defaultBackgroundRefreshInterval
	}
	atomic.StoreInt64(&s.backgroundRefreshInterval, int64(d))
	select {
	case s.backgroundRefreshWakeCh <- struct{}{}:
	default:
	}
}

// GetBackgroundRefreshInterval 获取后台刷新/探针巡检间隔。
func (s *Store) GetBackgroundRefreshInterval() time.Duration {
	d := time.Duration(atomic.LoadInt64(&s.backgroundRefreshInterval))
	if d <= 0 {
		return defaultBackgroundRefreshInterval
	}
	return d
}

// SetUsageProbeMaxAge 设置用量探针最大缓存时长。
func (s *Store) SetUsageProbeMaxAge(d time.Duration) {
	if d <= 0 {
		d = defaultUsageProbeMaxAge
	}
	atomic.StoreInt64(&s.usageProbeMaxAge, int64(d))
}

// GetUsageProbeMaxAge 获取用量探针最大缓存时长。
func (s *Store) GetUsageProbeMaxAge() time.Duration {
	d := time.Duration(atomic.LoadInt64(&s.usageProbeMaxAge))
	if d <= 0 {
		return defaultUsageProbeMaxAge
	}
	return d
}

// SetImageQuotaProbeMaxAge 设置网页图片额度探针最大缓存时长。
func (s *Store) SetImageQuotaProbeMaxAge(d time.Duration) {
	if d <= 0 {
		d = defaultImageQuotaProbeMaxAge
	}
	atomic.StoreInt64(&s.imageQuotaProbeMaxAge, int64(d))
}

// GetImageQuotaProbeMaxAge 获取网页图片额度探针最大缓存时长。
func (s *Store) GetImageQuotaProbeMaxAge() time.Duration {
	d := time.Duration(atomic.LoadInt64(&s.imageQuotaProbeMaxAge))
	if d <= 0 {
		return defaultImageQuotaProbeMaxAge
	}
	return d
}

// SetRecoveryProbeInterval 设置恢复探测最小间隔。
func (s *Store) SetRecoveryProbeInterval(d time.Duration) {
	if d <= 0 {
		d = defaultRecoveryProbeInterval
	}
	atomic.StoreInt64(&s.recoveryProbeInterval, int64(d))
}

// GetRecoveryProbeInterval 获取恢复探测最小间隔。
func (s *Store) GetRecoveryProbeInterval() time.Duration {
	d := time.Duration(atomic.LoadInt64(&s.recoveryProbeInterval))
	if d <= 0 {
		return defaultRecoveryProbeInterval
	}
	return d
}

// CleanExpiredNow 立即执行一次过期清理，返回清理数量
func (s *Store) CleanExpiredNow() int {
	return s.CleanExpiredAccounts(context.Background(), 30*time.Minute)
}

// Init 初始化：从数据库加载账号
func (s *Store) Init(ctx context.Context) error {
	// 1. 从数据库加载账号到内存
	if err := s.loadFromDB(ctx); err != nil {
		return err
	}

	if len(s.accounts) == 0 {
		log.Println("⚠ 数据库中暂无账号，请通过管理后台添加")
		return nil
	}

	s.rebuildFastScheduler()

	// 2. 统计可用账号，RT 账号的刷新交给 StartBackgroundRefresh 处理
	available := 0
	for _, acc := range s.accounts {
		if acc.IsAvailable() {
			available++
		}
	}
	log.Printf("账号初始化完成: %d/%d 可用", available, len(s.accounts))
	return nil
}

// loadFromDB 从数据库加载账号
func (s *Store) loadFromDB(ctx context.Context) error {
	rows, err := s.db.ListActive(ctx)
	if err != nil {
		return fmt.Errorf("从数据库加载账号失败: %w", err)
	}

	for _, row := range rows {
		rt := row.GetCredential("refresh_token")
		at := row.GetCredential("access_token")
		if rt == "" && at == "" {
			log.Printf("[账号 %d] 缺少 refresh_token 和 access_token，跳过", row.ID)
			continue
		}

		account := &Account{
			DBID:         row.ID,
			RefreshToken: rt,
			ProxyURL:     strings.TrimSpace(row.ProxyURL),
			HealthTier:   HealthTierWarm,
			AddedAt:      row.CreatedAt.UnixNano(),
		}
		if row.PublicAPIKeyID.Valid {
			account.PublicAPIKeyID = row.PublicAPIKeyID.Int64
		}
		if row.Locked {
			atomic.StoreInt32(&account.Locked, 1)
		}

		// 尝试从 credentials 恢复已有的 AT
		if at != "" {
			account.AccessToken = at
			account.AccountID = row.GetCredential("account_id")
			account.Email = row.GetCredential("email")
			account.PlanType = strings.ToLower(strings.TrimSpace(row.GetCredential("plan_type")))
			account.HealthTier = HealthTierHealthy
			if expiresAt := row.GetCredential("expires_at"); expiresAt != "" {
				if parsed, err := time.Parse(time.RFC3339, expiresAt); err == nil {
					account.ExpiresAt = parsed
				} else {
					log.Printf("[账号 %d] 解析 expires_at 失败: %v", row.ID, err)
				}
			}
		}
		if row.CooldownUntil.Valid {
			now := time.Now()
			if now.Before(row.CooldownUntil.Time) {
				account.SetCooldownUntil(row.CooldownUntil.Time, row.CooldownReason)
			} else if row.CooldownReason == "rate_limited" {
				// rate_limited 需要测活成功后才放出，重启后也保持等待态
				account.SetCooldownUntil(row.CooldownUntil.Time, row.CooldownReason)
				account.LastRateLimitedProbeAt = now
			} else if row.CooldownReason != "" {
				if err := s.db.ClearCooldown(ctx, row.ID); err != nil {
					log.Printf("[账号 %d] 清理过期冷却状态失败: %v", row.ID, err)
				}
			}
		}
		if usagePct := row.GetCredential("codex_7d_used_percent"); usagePct != "" {
			if parsed, err := strconv.ParseFloat(usagePct, 64); err == nil {
				updatedAt := time.Time{}
				if usageUpdatedAt := row.GetCredential("codex_usage_updated_at"); usageUpdatedAt != "" {
					if parsedTime, err := time.Parse(time.RFC3339, usageUpdatedAt); err == nil {
						updatedAt = parsedTime
					} else {
						log.Printf("[账号 %d] 解析 codex_usage_updated_at 失败: %v", row.ID, err)
					}
				}
				account.SetUsageSnapshot(parsed, updatedAt)
				// 恢复 7d 重置时间
				if resetAt := row.GetCredential("codex_7d_reset_at"); resetAt != "" {
					if t, err := time.Parse(time.RFC3339, resetAt); err == nil {
						account.SetReset7dAt(t)
					}
				}
			} else {
				log.Printf("[账号 %d] 解析 codex_7d_used_percent 失败: %v", row.ID, err)
			}
		}
		// 恢复 5h 用量快照
		if usagePct5h := row.GetCredential("codex_5h_used_percent"); usagePct5h != "" {
			if parsed, err := strconv.ParseFloat(usagePct5h, 64); err == nil {
				resetAt := time.Time{}
				if r := row.GetCredential("codex_5h_reset_at"); r != "" {
					if t, err := time.Parse(time.RFC3339, r); err == nil {
						resetAt = t
					}
				}
				account.SetUsageSnapshot5h(parsed, resetAt)
			}
		}
		imageRemaining := -1
		imageTotal := -1
		if raw := row.GetCredential("image_web_remaining"); raw != "" {
			if parsed, err := strconv.Atoi(raw); err == nil {
				imageRemaining = parsed
			}
		}
		if raw := row.GetCredential("image_web_total"); raw != "" {
			if parsed, err := strconv.Atoi(raw); err == nil {
				imageTotal = parsed
			}
		}
		imageResetAt := time.Time{}
		if raw := row.GetCredential("image_web_reset_at"); raw != "" {
			if parsed, err := time.Parse(time.RFC3339, raw); err == nil {
				imageResetAt = parsed
			}
		}
		imageUpdatedAt := time.Time{}
		if raw := row.GetCredential("image_quota_updated_at"); raw != "" {
			if parsed, err := time.Parse(time.RFC3339, raw); err == nil {
				imageUpdatedAt = parsed
			}
		}
		officialAvailable := DefaultOfficialImageAvailabilityForPlan(account.PlanType)
		if strings.EqualFold(strings.TrimSpace(row.GetCredential("image_official_source")), "upstream") {
			if raw := row.GetCredential("image_official_available"); raw != "" {
				if parsed, err := strconv.Atoi(raw); err == nil {
					officialAvailable = parsed
				}
			}
		}
		account.SetImageQuotaSnapshot(imageRemaining, imageTotal, imageResetAt, imageUpdatedAt, officialAvailable)
		account.mu.Lock()
		account.recomputeSchedulerLocked(atomic.LoadInt64(&s.maxConcurrency))
		account.mu.Unlock()

		s.accounts = append(s.accounts, account)
	}

	log.Printf("从数据库加载了 %d 个账号", len(s.accounts))
	return nil
}

// StartBackgroundRefresh 启动后台定期刷新
func (s *Store) StartBackgroundRefresh() {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		refreshTimer := time.NewTimer(s.GetBackgroundRefreshInterval())
		autoCleanupTicker := time.NewTicker(30 * time.Second)
		fullUsageCleanupTicker := time.NewTicker(5 * time.Minute)
		expiredCleanupTicker := time.NewTicker(15 * time.Minute)
		// 添加定时重建 FastScheduler 以优化性能
		rebuildSchedulerTicker := time.NewTicker(10 * time.Minute)
		defer refreshTimer.Stop()
		defer autoCleanupTicker.Stop()
		defer fullUsageCleanupTicker.Stop()
		defer expiredCleanupTicker.Stop()
		defer rebuildSchedulerTicker.Stop()

		resetRefreshTimer := func() {
			if !refreshTimer.Stop() {
				select {
				case <-refreshTimer.C:
				default:
				}
			}
			refreshTimer.Reset(s.GetBackgroundRefreshInterval())
		}

		for {
			select {
			case <-refreshTimer.C:
				s.parallelRefreshAll(context.Background())
				s.TriggerUsageProbeAsync()
				s.TriggerImageQuotaProbeAsync()
				s.TriggerRecoveryProbeAsync()
				refreshTimer.Reset(s.GetBackgroundRefreshInterval())
			case <-s.backgroundRefreshWakeCh:
				resetRefreshTimer()
			case <-autoCleanupTicker.C:
				s.TriggerAutoCleanupAsync()
			case <-fullUsageCleanupTicker.C:
				switch s.GetAutoCleanFullUsageMode() {
				case AutoCleanFullUsageModeDelete:
					go s.CleanFullUsageAccounts(context.Background())
				case AutoCleanFullUsageModeWait:
					go s.WaitFullUsageAccounts(context.Background())
				}
			case <-expiredCleanupTicker.C:
				// 每 15 分钟清理加入超过 30 分钟的账号（需开启开关）
				if s.GetAutoCleanExpired() {
					go s.CleanExpiredAccounts(context.Background(), 30*time.Minute)
				}
			case <-rebuildSchedulerTicker.C:
				// 定期重建调度器以优化内存和性能
				if s.FastSchedulerEnabled() {
					s.rebuildFastScheduler()
				}
			case <-s.stopCh:
				return
			}
		}
	}()
}

// Stop 停止后台刷新
func (s *Store) Stop() {
	close(s.stopCh)
	s.wg.Wait()
}

// CleanByRuntimeStatus 按运行时状态清理账号
func (s *Store) CleanByRuntimeStatus(ctx context.Context, targetStatus string) int {
	accounts := s.Accounts()
	cleaned := 0

	for _, acc := range accounts {
		if acc == nil || acc.RuntimeStatus() != targetStatus {
			continue
		}

		// 锁定账号跳过自动清理
		if atomic.LoadInt32(&acc.Locked) == 1 {
			continue
		}

		if s.db != nil {
			if err := s.db.SoftDeleteAccount(ctx, acc.DBID); err != nil {
				log.Printf("[账号 %d] 清理 %s 状态失败: %v", acc.DBID, targetStatus, err)
				continue
			}
		}

		s.RemoveAccount(acc.DBID)
		cleaned++
		if s.db != nil {
			s.db.InsertAccountEventAsync(acc.DBID, "deleted", "auto_clean")
		}
	}

	return cleaned
}

// ==================== 最少连接调度 ====================

// Next 获取下一个可用账号（健康优先 + 低负载择优 + warm 公平调度）
func (s *Store) Next() *Account {
	return s.NextMatching(nil, nil)
}

// NextExcluding 获取下一个可用账号，排除指定的账号 ID 集合
// 用于重试时避免再次选到已失败（如 401）的账号
func (s *Store) NextExcluding(exclude map[int64]bool) *Account {
	return s.NextMatching(exclude, nil)
}

// NextMatching 获取下一个可用账号，并支持额外过滤条件。
func (s *Store) NextMatching(exclude map[int64]bool, matcher AccountMatcher) *Account {
	if scheduler := s.getFastScheduler(); scheduler != nil {
		return scheduler.AcquireMatching(exclude, matcher)
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	var best *Account
	var preferredBest *Account
	bestPriority := -1
	bestScore := -math.MaxFloat64
	var bestLoad int64 = math.MaxInt64
	preferredBestPriority := -1
	preferredBestScore := -math.MaxFloat64
	var preferredBestLoad int64 = math.MaxInt64
	maxConcurrency := atomic.LoadInt64(&s.maxConcurrency)

	// 收集所有可用候选（用于公平调度）
	var candidates []*Account
	var preferredCandidates []*Account
	preferredPlanEnabled := s.isPreferredPlanEnabled()

	for _, acc := range s.accounts {
		if exclude != nil && exclude[acc.DBID] {
			continue
		}
		if matcher != nil {
			if !matcher(acc) {
				continue
			}
		} else if !acc.IsAvailable() {
			continue
		}

		load := atomic.LoadInt64(&acc.ActiveRequests)
		tier, score, limit := acc.schedulerSnapshot(maxConcurrency)
		planType := acc.GetPlanType()
		score += s.schedulerPlanBonus(planType)
		if limit <= 0 || load >= limit {
			continue
		}

		candidates = append(candidates, acc)
		isPreferred := preferredPlanEnabled && s.isPreferredPlan(planType)
		if isPreferred {
			preferredCandidates = append(preferredCandidates, acc)
		}

		priority := tierPriority(tier)
		if priority > bestPriority ||
			(priority == bestPriority && (score > bestScore ||
				(score == bestScore && load < bestLoad) ||
				(score == bestScore && load == bestLoad && fastRandN(2) == 0))) {
			bestPriority = priority
			bestScore = score
			bestLoad = load
			best = acc
		}
		if isPreferred && (priority > preferredBestPriority ||
			(priority == preferredBestPriority && (score > preferredBestScore ||
				(score == preferredBestScore && load < preferredBestLoad) ||
				(score == preferredBestScore && load == preferredBestLoad && fastRandN(2) == 0)))) {
			preferredBestPriority = priority
			preferredBestScore = score
			preferredBestLoad = load
			preferredBest = acc
		}
	}

	if preferredBest != nil {
		best = preferredBest
		bestPriority = preferredBestPriority
		candidates = preferredCandidates
	}

	// Warm 公平调度：15% 概率随机选一个非 best 候选，避免 warm 饥饿
	if best != nil && len(candidates) > 1 && bestPriority >= tierPriority(HealthTierHealthy) {
		if fastRandN(100) < 15 {
			alt := candidates[fastRandN(len(candidates))]
			if alt != best {
				best = alt
			}
		}
	}

	if best != nil {
		atomic.AddInt64(&best.ActiveRequests, 1)
		atomic.AddInt64(&best.TotalRequests, 1)
		atomic.StoreInt64(&best.LastUsedAt, time.Now().UnixNano())
	}
	return best
}

// BindSessionAffinity 记录会话与账号/代理的亲和关系。
func (s *Store) BindSessionAffinity(key string, account *Account, proxyURL string) {
	if s == nil || account == nil {
		return
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return
	}

	s.sessionMu.Lock()
	if s.sessionBindings == nil {
		s.sessionBindings = make(map[string]sessionAffinity)
	}
	s.sessionBindings[key] = sessionAffinity{
		accountID: account.DBID,
		proxyURL:  strings.TrimSpace(proxyURL),
		expiresAt: time.Now().Add(sessionAffinityTTL),
	}
	s.sessionMu.Unlock()
}

// UnbindSessionAffinity 仅当绑定仍指向失败账号时清除会话亲和。
func (s *Store) UnbindSessionAffinity(key string, accountID int64) {
	if s == nil || accountID == 0 {
		return
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return
	}

	s.sessionMu.Lock()
	if binding, ok := s.sessionBindings[key]; ok && binding.accountID == accountID {
		delete(s.sessionBindings, key)
	}
	s.sessionMu.Unlock()
}

// NextForSession 优先复用已绑定的账号和代理，失败时回退到普通选号。
func (s *Store) NextForSession(key string, exclude map[int64]bool, matcher AccountMatcher) (*Account, string) {
	if s == nil {
		return nil, ""
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return s.NextMatching(exclude, matcher), ""
	}

	now := time.Now()
	s.sessionMu.RLock()
	binding, ok := s.sessionBindings[key]
	s.sessionMu.RUnlock()

	if ok {
		if !binding.expiresAt.After(now) {
			s.sessionMu.Lock()
			if current, exists := s.sessionBindings[key]; exists && !current.expiresAt.After(now) {
				delete(s.sessionBindings, key)
			}
			s.sessionMu.Unlock()
		} else if acc := s.takeByIDExcluding(binding.accountID, exclude, matcher); acc != nil {
			return acc, binding.proxyURL
		}
	}

	return s.NextMatching(exclude, matcher), ""
}

func (s *Store) takeByIDExcluding(id int64, exclude map[int64]bool, matcher AccountMatcher) *Account {
	if s == nil || id == 0 {
		return nil
	}
	if exclude != nil && exclude[id] {
		return nil
	}

	s.mu.RLock()
	var target *Account
	for _, acc := range s.accounts {
		if acc != nil && acc.DBID == id {
			target = acc
			break
		}
	}
	s.mu.RUnlock()

	if target == nil {
		return nil
	}
	if matcher != nil {
		if !matcher(target) {
			return nil
		}
	} else if !target.IsAvailable() {
		return nil
	}

	maxConcurrency := atomic.LoadInt64(&s.maxConcurrency)
	_, _, limit := target.schedulerSnapshot(maxConcurrency)
	if limit <= 0 {
		return nil
	}
	if !tryAcquireAccount(target, limit) {
		return nil
	}
	return target
}

// WaitForAvailable 等待可用账号（带超时的请求排队）
func (s *Store) WaitForAvailable(ctx context.Context, timeout time.Duration) *Account {
	return s.WaitForAvailableMatching(ctx, timeout, nil, nil)
}

// WaitForAvailableMatching 等待满足过滤条件的可用账号（带超时的请求排队）
func (s *Store) WaitForAvailableMatching(ctx context.Context, timeout time.Duration, exclude map[int64]bool, matcher AccountMatcher) *Account {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()

	backoff := 50 * time.Millisecond
	backoffTimer := time.NewTimer(backoff)
	defer backoffTimer.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-deadline.C:
			return nil
		default:
			acc := s.NextMatching(exclude, matcher)
			if acc != nil {
				return acc
			}
			// 等待一下再重试（指数退避，最大 500ms）
			backoffTimer.Reset(backoff)
			select {
			case <-backoffTimer.C:
				if backoff < 500*time.Millisecond {
					backoff *= 2
				}
			case <-ctx.Done():
				return nil
			case <-deadline.C:
				return nil
			}
		}
	}
}

// WaitForSessionAvailable 等待满足过滤条件的可用账号，并优先遵守会话亲和绑定。
func (s *Store) WaitForSessionAvailable(ctx context.Context, key string, timeout time.Duration, exclude map[int64]bool, matcher AccountMatcher) (*Account, string) {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()

	backoff := 50 * time.Millisecond
	backoffTimer := time.NewTimer(backoff)
	defer backoffTimer.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil, ""
		case <-deadline.C:
			return nil, ""
		default:
			acc, proxyURL := s.NextForSession(key, exclude, matcher)
			if acc != nil {
				return acc, proxyURL
			}
			backoffTimer.Reset(backoff)
			select {
			case <-backoffTimer.C:
				if backoff < 500*time.Millisecond {
					backoff *= 2
				}
			case <-ctx.Done():
				return nil, ""
			case <-deadline.C:
				return nil, ""
			}
		}
	}
}

// Release 释放账号（请求完成后调用，递减并发计数）
func (s *Store) Release(acc *Account) {
	if acc == nil {
		return
	}
	if scheduler := s.getFastScheduler(); scheduler != nil {
		scheduler.Release(acc)
		return
	}
	atomic.AddInt64(&acc.ActiveRequests, -1)
}

// SetMaxConcurrency 动态更新每账号并发上限
func (s *Store) SetMaxConcurrency(n int) {
	atomic.StoreInt64(&s.maxConcurrency, int64(n))
	s.recomputeAllAccountSchedulerState()
	s.rebuildFastScheduler()
}

// GetMaxConcurrency 获取当前每账号并发上限
func (s *Store) GetMaxConcurrency() int {
	return int(atomic.LoadInt64(&s.maxConcurrency))
}

// SetMaxRetries 动态更新最大重试次数
func (s *Store) SetMaxRetries(n int) {
	if n < 0 {
		n = 0
	}
	atomic.StoreInt64(&s.maxRetries, int64(n))
}

// GetMaxRetries 获取当前最大重试次数
func (s *Store) GetMaxRetries() int {
	return int(atomic.LoadInt64(&s.maxRetries))
}

// SetPublicCreditConfig 设置公开上传账号奖励配置（单位：美元）
func (s *Store) SetPublicCreditConfig(initialUSD, fullUSD float64) {
	initialUSD, fullUSD = normalizePublicCredit(initialUSD, fullUSD)
	atomic.StoreInt64(&s.publicInitialCreditX1e4, moneyToScaled(initialUSD))
	atomic.StoreInt64(&s.publicFullCreditX1e4, moneyToScaled(fullUSD))
}

// GetPublicInitialCreditUSD 获取公开上传账号初始入账金额（美元）
func (s *Store) GetPublicInitialCreditUSD() float64 {
	return scaledToMoney(atomic.LoadInt64(&s.publicInitialCreditX1e4))
}

// GetPublicFullCreditUSD 获取公开上传账号满额度总金额（美元）
func (s *Store) GetPublicFullCreditUSD() float64 {
	return scaledToMoney(atomic.LoadInt64(&s.publicFullCreditX1e4))
}

// GetAllowRemoteMigration 获取是否允许远程迁移
func (s *Store) GetAllowRemoteMigration() bool {
	return s.allowRemoteMigration.Load()
}

// SetAllowRemoteMigration 设置是否允许远程迁移
func (s *Store) SetAllowRemoteMigration(enabled bool) {
	s.allowRemoteMigration.Store(enabled)
}

// SetTestModel 动态更新测试连接模型
func (s *Store) SetTestModel(m string) {
	s.testModel.Store(m)
}

// GetTestModel 获取当前测试连接模型
func (s *Store) GetTestModel() string {
	if v, ok := s.testModel.Load().(string); ok && v != "" {
		return v
	}
	return "gpt-5.4"
}

// SetTestConcurrency 动态更新批量测试并发数
func (s *Store) SetTestConcurrency(n int) {
	atomic.StoreInt64(&s.testConcurrency, int64(n))
}

// GetTestConcurrency 获取当前批量测试并发数
func (s *Store) GetTestConcurrency() int {
	return int(atomic.LoadInt64(&s.testConcurrency))
}

// GetBackgroundRefreshIntervalMinutes 获取后台巡检间隔（分钟）。
func (s *Store) GetBackgroundRefreshIntervalMinutes() int {
	return int(s.GetBackgroundRefreshInterval() / time.Minute)
}

// GetUsageProbeMaxAgeMinutes 获取用量探针最大缓存时长（分钟）。
func (s *Store) GetUsageProbeMaxAgeMinutes() int {
	return int(s.GetUsageProbeMaxAge() / time.Minute)
}

// GetRecoveryProbeIntervalMinutes 获取恢复探测最小间隔（分钟）。
func (s *Store) GetRecoveryProbeIntervalMinutes() int {
	return int(s.GetRecoveryProbeInterval() / time.Minute)
}

// SetModelMapping 动态更新模型映射 JSON
func (s *Store) SetModelMapping(mapping string) {
	s.modelMapping.Store(mapping)
}

// GetModelMapping 获取当前模型映射 JSON
func (s *Store) GetModelMapping() string {
	if v, ok := s.modelMapping.Load().(string); ok && v != "" {
		return v
	}
	return "{}"
}

// AddAccount 热加载新账号到内存池（前端添加后即刻生效）
func (s *Store) AddAccount(acc *Account) {
	if acc == nil {
		return
	}
	// 记录加入时间（用于过期清理）
	if atomic.LoadInt64(&acc.AddedAt) == 0 {
		atomic.StoreInt64(&acc.AddedAt, time.Now().UnixNano())
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	acc.mu.Lock()
	acc.recomputeSchedulerLocked(atomic.LoadInt64(&s.maxConcurrency))
	acc.mu.Unlock()
	s.accounts = append(s.accounts, acc)
	s.fastSchedulerUpdate(acc)
}

// RemoveAccount 从内存池移除账号
func (s *Store) RemoveAccount(dbID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i, acc := range s.accounts {
		if acc.DBID == dbID {
			s.accounts = append(s.accounts[:i], s.accounts[i+1:]...)
			s.fastSchedulerRemove(dbID)
			// 清理 RefreshScheduler 中可能残留的任务
			if scheduler := s.GetRefreshScheduler(); scheduler != nil {
				scheduler.CancelTask(dbID)
			}
			return
		}
	}
}

// FindByID 通过数据库 ID 查找运行时账号
func (s *Store) FindByID(dbID int64) *Account {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, acc := range s.accounts {
		if acc.DBID == dbID {
			return acc
		}
	}
	return nil
}

// MarkCooldown 标记账号进入冷却，并持久化到数据库
func (s *Store) MarkCooldown(acc *Account, duration time.Duration, reason string) {
	if acc == nil {
		return
	}

	now := time.Now()
	acc.mu.Lock()
	switch reason {
	case "unauthorized":
		if !acc.LastUnauthorizedAt.IsZero() && now.Sub(acc.LastUnauthorizedAt) < 24*time.Hour {
			duration = 24 * time.Hour
		} else {
			duration = 6 * time.Hour
		}
		acc.LastUnauthorizedAt = now
		acc.LastFailureAt = now
		acc.FailureStreak++
		acc.SuccessStreak = 0
		acc.HealthTier = HealthTierBanned
	case "rate_limited":
		acc.LastRateLimitedAt = now
		acc.LastRateLimitedProbeAt = now
		acc.LastFailureAt = now
		acc.FailureStreak++
		acc.SuccessStreak = 0
		if acc.healthTierLocked() == HealthTierHealthy {
			acc.HealthTier = HealthTierWarm
		} else {
			acc.HealthTier = HealthTierRisky
		}
	case "full_usage":
		acc.LastFullUsageProbeAt = now
		acc.LastFailureAt = now
		acc.FailureStreak++
		acc.SuccessStreak = 0
		if acc.healthTierLocked() == HealthTierHealthy {
			acc.HealthTier = HealthTierWarm
		}
	}
	acc.recomputeSchedulerLocked(atomic.LoadInt64(&s.maxConcurrency))
	acc.mu.Unlock()

	until := now.Add(duration)
	acc.SetCooldownUntil(until, reason)
	s.fastSchedulerUpdate(acc)

	if s.db == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.db.SetCooldown(ctx, acc.DBID, reason, until); err != nil {
		log.Printf("[账号 %d] 持久化冷却状态失败: %v", acc.DBID, err)
	}
}

// ExtendRateLimitedCooldown 将限流等待时间在现有基础上追加（用于探针失败后延长等待）
func (s *Store) ExtendRateLimitedCooldown(acc *Account, step time.Duration) {
	if acc == nil {
		return
	}
	if step <= 0 {
		step = rateLimitedWaitStep
	}

	now := time.Now()
	base := now
	if until, reason, active := acc.GetCooldownSnapshot(); active && reason == "rate_limited" && until.After(base) {
		base = until
	}
	s.MarkCooldown(acc, base.Add(step).Sub(now), "rate_limited")
}

// ClearCooldown 清除账号冷却状态，并同步清理数据库
func (s *Store) ClearCooldown(acc *Account) {
	if acc == nil {
		return
	}

	atomic.StoreInt32(&acc.Disabled, 0) // 清除原子禁用标志
	acc.mu.Lock()
	wasCooling := acc.Status == StatusCooldown
	if acc.Status == StatusCooldown {
		acc.Status = StatusReady
	}
	acc.CooldownUtil = time.Time{}
	acc.CooldownReason = ""
	if wasCooling {
		acc.HealthTier = HealthTierWarm
	}
	acc.recomputeSchedulerLocked(atomic.LoadInt64(&s.maxConcurrency))
	acc.mu.Unlock()
	s.fastSchedulerUpdate(acc)

	if s.db == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.db.ClearCooldown(ctx, acc.DBID); err != nil {
		log.Printf("[账号 %d] 清理冷却状态失败: %v", acc.DBID, err)
	}
}

// ReportRequestSuccess 记录一次成功请求，用于动态调度评分
func (s *Store) ReportRequestSuccess(acc *Account, latency time.Duration) {
	if acc == nil {
		return
	}

	acc.mu.Lock()
	acc.recordLatencyLocked(latency)
	acc.recordResultLocked(true)
	acc.LastSuccessAt = time.Now()
	acc.SuccessStreak = clampInt(acc.SuccessStreak+1, 0, 20)
	acc.FailureStreak = 0
	acc.LastFailureStatusCode = 0
	acc.LastFailureCode = ""
	acc.LastFailureMessage = ""
	if acc.HealthTier == "" {
		acc.HealthTier = HealthTierHealthy
	}
	acc.recomputeSchedulerLocked(atomic.LoadInt64(&s.maxConcurrency))
	acc.mu.Unlock()
	s.fastSchedulerUpdate(acc)
}

// ReportFirstTokenLatency 在收到首个 token 时提前更新延迟画像。
// 仅更新延迟维度，不改变成功/失败统计。
func (s *Store) ReportFirstTokenLatency(acc *Account, latency time.Duration) {
	if acc == nil || latency <= 0 {
		return
	}

	acc.mu.Lock()
	acc.recordLatencyLocked(latency)
	acc.recomputeSchedulerLocked(atomic.LoadInt64(&s.maxConcurrency))
	acc.mu.Unlock()
	s.fastSchedulerUpdate(acc)
}

// ReportRequestFailure 记录一次失败请求，用于动态调度评分
func (s *Store) ReportRequestFailure(acc *Account, kind string, latency time.Duration) {
	if acc == nil {
		return
	}

	now := time.Now()
	acc.mu.Lock()
	acc.recordLatencyLocked(latency)
	acc.recordResultLocked(false)
	acc.LastFailureAt = now
	acc.FailureStreak = clampInt(acc.FailureStreak+1, 0, 20)
	acc.SuccessStreak = 0

	switch kind {
	case "unauthorized":
		acc.LastUnauthorizedAt = now
		acc.HealthTier = HealthTierBanned
	case "timeout":
		acc.LastTimeoutAt = now
		if acc.HealthTier == HealthTierHealthy {
			acc.HealthTier = HealthTierWarm
		} else {
			acc.HealthTier = HealthTierRisky
		}
	case "server":
		acc.LastServerErrorAt = now
		if acc.HealthTier == HealthTierHealthy {
			acc.HealthTier = HealthTierWarm
		} else {
			acc.HealthTier = HealthTierRisky
		}
	case "transport":
		if acc.HealthTier == HealthTierHealthy {
			acc.HealthTier = HealthTierWarm
		} else {
			acc.HealthTier = HealthTierRisky
		}
	case "client":
		if acc.HealthTier == HealthTierHealthy {
			acc.HealthTier = HealthTierWarm
		}
	}

	acc.recomputeSchedulerLocked(atomic.LoadInt64(&s.maxConcurrency))
	acc.mu.Unlock()
	s.fastSchedulerUpdate(acc)
}

// PersistUsageSnapshot 持久化账号用量快照（7d + 5h）
func (s *Store) PersistUsageSnapshot(acc *Account, pct7d float64) {
	if acc == nil {
		return
	}

	now := time.Now()
	acc.SetUsageSnapshot(pct7d, now)

	if s.db == nil {
		return
	}

	// 如果有 5h 数据，使用完整存储
	if pct5h, ok := acc.GetUsagePercent5h(); ok {
		reset5hAt := acc.GetReset5hAt()
		reset7dAt := acc.GetReset7dAt()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := s.db.UpdateUsageSnapshotFull(ctx, acc.DBID, pct7d, reset7dAt, pct5h, reset5hAt, now); err != nil {
			log.Printf("[账号 %d] 持久化用量快照失败: %v", acc.DBID, err)
		}
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.db.UpdateUsageSnapshot(ctx, acc.DBID, pct7d, now); err != nil {
		log.Printf("[账号 %d] 持久化用量快照失败: %v", acc.DBID, err)
	}
}

// PersistImageQuotaSnapshot 持久化网页图片额度快照。
func (s *Store) PersistImageQuotaSnapshot(acc *Account, remaining, total int, resetAt, updatedAt time.Time, officialAvailable int) {
	if acc == nil {
		return
	}
	_, prevTotal, prevResetAt, _, prevValid, _ := acc.GetImageQuotaSnapshot()
	if total < 0 {
		if prevValid && prevTotal > 0 {
			total = prevTotal
		} else if remaining >= 0 {
			total = remaining
		}
	}
	if remaining < 0 {
		remaining = 0
	}
	if total < 0 {
		total = 0
	}
	sameResetWindow := resetAt.IsZero() || prevResetAt.IsZero() || resetAt.Equal(prevResetAt)
	if prevValid && sameResetWindow && prevTotal > total && remaining <= prevTotal {
		total = prevTotal
	}
	if remaining > total {
		total = remaining
	}
	acc.SetImageQuotaSnapshot(remaining, total, resetAt, updatedAt, officialAvailable)
	if s.db == nil {
		return
	}
	fields := map[string]interface{}{
		"image_web_remaining":      remaining,
		"image_web_total":          total,
		"image_official_available": officialAvailable,
	}
	if !resetAt.IsZero() {
		fields["image_web_reset_at"] = resetAt.UTC().Format(time.RFC3339)
	}
	if !updatedAt.IsZero() {
		fields["image_quota_updated_at"] = updatedAt.UTC().Format(time.RFC3339)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.db.UpdateCredentials(ctx, acc.DBID, fields); err != nil {
		log.Printf("[账号 %d] 持久化网页图片额度失败: %v", acc.DBID, err)
	}
}

// PersistOfficialImageAvailability 持久化官方图片链路剩余次数。
// 仅在上游显式返回真实数值时调用；不会做任何本地推算。
func (s *Store) PersistOfficialImageAvailability(acc *Account, available int) {
	if acc == nil {
		return
	}
	if available < 0 {
		available = 0
	}
	acc.SetOfficialImageAvailability(available, true)
	if s.db == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.db.UpdateCredentials(ctx, acc.DBID, map[string]interface{}{
		"image_official_available": available,
		"image_official_source":    "upstream",
	}); err != nil {
		log.Printf("[账号 %d] 持久化官方图片额度失败: %v", acc.DBID, err)
	}
}

// DecrementImageQuotaAfterSuccess 成功生图后立刻扣减网页图片剩余额度。
func (s *Store) DecrementImageQuotaAfterSuccess(acc *Account, delta int) {
	if acc == nil || delta <= 0 {
		return
	}
	if _, _, _, _, valid, _ := acc.GetImageQuotaSnapshot(); !valid {
		total := DefaultWebImageQuotaForPlan(acc.GetPlanType())
		remaining := total - delta
		if remaining < 0 {
			remaining = 0
		}
		officialAvailable, officialKnown := acc.GetOfficialImageAvailability()
		if !officialKnown {
			officialAvailable = UnknownOfficialImageAvailability
		}
		acc.SetImageQuotaSnapshot(remaining, total, time.Time{}, time.Now().UTC(), officialAvailable)
	} else {
		acc.DecrementImageWebQuota(delta)
	}
	if s.db == nil {
		return
	}
	remaining, total, resetAt, officialAvailable, valid, updatedAt := acc.GetImageQuotaSnapshot()
	if !valid {
		return
	}
	fields := map[string]interface{}{
		"image_web_remaining":      remaining,
		"image_web_total":          total,
		"image_official_available": officialAvailable,
	}
	if !resetAt.IsZero() {
		fields["image_web_reset_at"] = resetAt.UTC().Format(time.RFC3339)
	}
	if !updatedAt.IsZero() {
		fields["image_quota_updated_at"] = updatedAt.UTC().Format(time.RFC3339)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.db.UpdateCredentials(ctx, acc.DBID, fields); err != nil {
		log.Printf("[账号 %d] 扣减网页图片额度失败: %v", acc.DBID, err)
	}
}

// SetUsageProbeFunc 注册主动探针回调
func (s *Store) SetUsageProbeFunc(fn func(context.Context, *Account) error) {
	s.usageProbeMu.Lock()
	defer s.usageProbeMu.Unlock()
	s.usageProbe = fn
}

// SetImageQuotaProbeFunc 注册网页图片额度探针回调。
func (s *Store) SetImageQuotaProbeFunc(fn func(context.Context, *Account) error) {
	s.imageQuotaProbeMu.Lock()
	defer s.imageQuotaProbeMu.Unlock()
	s.imageQuotaProbe = fn
}

// TriggerUsageProbeAsync 异步触发一次批量用量探针
func (s *Store) TriggerUsageProbeAsync() {
	if !s.usageProbeBatch.CompareAndSwap(false, true) {
		return
	}

	go func() {
		defer s.usageProbeBatch.Store(false)
		s.parallelProbeUsage(context.Background())
	}()
}

// TriggerImageQuotaProbeAsync 异步触发一次网页图片额度探针。
func (s *Store) TriggerImageQuotaProbeAsync() {
	if !s.imageQuotaProbeBatch.CompareAndSwap(false, true) {
		return
	}

	go func() {
		defer s.imageQuotaProbeBatch.Store(false)
		s.parallelProbeImageQuota(context.Background())
	}()
}

// TriggerRecoveryProbeAsync 异步触发一次封禁账号恢复探测
func (s *Store) TriggerRecoveryProbeAsync() {
	if !s.recoveryProbeBatch.CompareAndSwap(false, true) {
		return
	}

	go func() {
		defer s.recoveryProbeBatch.Store(false)
		s.parallelRecoveryProbe(context.Background())
	}()
}

// TriggerAutoCleanupAsync 异步触发一次自动清理巡检
func (s *Store) TriggerAutoCleanupAsync() {
	if !s.autoCleanupBatch.CompareAndSwap(false, true) {
		return
	}

	go func() {
		defer s.autoCleanupBatch.Store(false)
		s.runAutoCleanupSweep(context.Background())
	}()
}

func (s *Store) runAutoCleanupSweep(ctx context.Context) {
	if !s.GetAutoCleanUnauthorized() && !s.GetAutoCleanRateLimited() && !s.GetAutoCleanError() {
		return
	}

	cleanupCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	cleanedUnauthorized := 0
	waitedRateLimited := 0
	cleanedError := 0

	if s.GetAutoCleanUnauthorized() {
		cleanedUnauthorized = s.CleanByRuntimeStatus(cleanupCtx, "unauthorized")
	}
	if s.GetAutoCleanRateLimited() {
		// 429 自动处理改为“等待模式”：不删除账号，统一进入 2h 等待窗并排除调度
		waitedRateLimited = s.WaitRateLimitedAccounts(cleanupCtx, rateLimitedWaitStep)
	}
	if s.GetAutoCleanError() {
		cleanedError = s.CleanByRuntimeStatus(cleanupCtx, "error")
	}

	if cleanedUnauthorized > 0 || waitedRateLimited > 0 || cleanedError > 0 {
		log.Printf("自动清理完成: unauthorized=%d, rate_limited_wait=%d, error=%d", cleanedUnauthorized, waitedRateLimited, cleanedError)
	}
}

// WaitRateLimitedAccounts 对 rate_limited 账号应用等待模式（不删除）
func (s *Store) WaitRateLimitedAccounts(ctx context.Context, minWait time.Duration) int {
	accounts := s.Accounts()
	waited := 0

	for _, acc := range accounts {
		select {
		case <-ctx.Done():
			return waited
		default:
		}
		if acc == nil || acc.RuntimeStatus() != "rate_limited" {
			continue
		}

		// 跳过正在处理请求的账号，避免中断已有请求
		if atomic.LoadInt64(&acc.ActiveRequests) > 0 {
			continue
		}

		// 已在 rate_limited 等待态中的账号不重复打标，避免巡检重置探针节奏
		if _, reason, _ := acc.GetCooldownSnapshot(); reason == "rate_limited" {
			continue
		}

		waitDuration := minWait
		if waitDuration < time.Minute {
			waitDuration = time.Minute
		}

		s.MarkCooldown(acc, waitDuration, "rate_limited")
		if s.db != nil {
			s.db.InsertAccountEventAsync(acc.DBID, "cooldown", "auto_wait_429")
		}
		waited++
	}

	return waited
}

func (s *Store) resolveFullUsageWaitUntil(acc *Account, now time.Time) (time.Time, bool) {
	if acc == nil {
		return time.Time{}, false
	}

	var until time.Time
	appendUntil := func(candidate time.Time) {
		if candidate.IsZero() || !candidate.After(now) {
			return
		}
		if until.IsZero() || candidate.After(until) {
			until = candidate
		}
	}

	if pct7d, ok := acc.GetUsagePercent7d(); ok && pct7d >= 100.0 {
		appendUntil(acc.GetReset7dAt())
	}
	if pct5h, ok := acc.GetUsagePercent5h(); ok && pct5h >= 100.0 {
		appendUntil(acc.GetReset5hAt())
	}

	if until.IsZero() {
		return time.Time{}, false
	}
	return until, true
}

// MarkFullUsageCooldownFromSnapshot 若账号用量已满，则按额度恢复时间打 full_usage 等待
func (s *Store) MarkFullUsageCooldownFromSnapshot(acc *Account) bool {
	now := time.Now()
	if until, ok := s.resolveFullUsageWaitUntil(acc, now); ok {
		waitDuration := until.Sub(now)
		if waitDuration < time.Minute {
			waitDuration = time.Minute
		}
		s.MarkCooldown(acc, waitDuration, "full_usage")
		return true
	}
	return false
}

// WaitFullUsageAccounts 将用量 >= 100% 的账号切换为等待模式（冷却到重置时间）
func (s *Store) WaitFullUsageAccounts(ctx context.Context) int {
	accounts := s.Accounts()
	waited := 0
	now := time.Now()

	for _, acc := range accounts {
		select {
		case <-ctx.Done():
			return waited
		default:
		}
		if acc == nil {
			continue
		}
		runtimeStatus := acc.RuntimeStatus()
		if runtimeStatus == "error" || runtimeStatus == "unauthorized" {
			continue
		}

		// 跳过正在处理请求的账号
		if atomic.LoadInt64(&acc.ActiveRequests) > 0 {
			continue
		}

		pct7d, valid7d := acc.GetUsagePercent7d()
		pct5h, valid5h := acc.GetUsagePercent5h()
		if !(valid7d && pct7d >= 100.0) && !(valid5h && pct5h >= 100.0) {
			continue
		}

		// 已在冷却中则不重复打标，避免每 5 分钟重复覆盖
		if acc.HasActiveCooldown() {
			continue
		}

		waitUntil, ok := s.resolveFullUsageWaitUntil(acc, now)
		if !ok {
			waitUntil = now.Add(fullUsageWaitFallback)
		}
		s.MarkCooldown(acc, waitUntil.Sub(time.Now()), "full_usage")
		if s.db != nil {
			s.db.InsertAccountEventAsync(acc.DBID, "cooldown", "full_usage_wait")
		}
		log.Printf("[账号 %d] 用量已满 (7d=%.1f%%,5h=%.1f%%)，进入等待模式至 %s (email=%s)", acc.DBID, pct7d, pct5h, waitUntil.Format(time.RFC3339), acc.Email)
		waited++
	}

	if waited > 0 {
		log.Printf("满用量等待模式处理完成: 共标记 %d 个账号", waited)
	}
	return waited
}

// CleanFullUsageAccounts 清理用量达到 100% 的账号（跳过正在处理请求的账号）
func (s *Store) CleanFullUsageAccounts(ctx context.Context) int {
	accounts := s.Accounts()
	cleaned := 0

	for _, acc := range accounts {
		if acc == nil {
			continue
		}

		// 锁定账号跳过自动清理
		if atomic.LoadInt32(&acc.Locked) == 1 {
			continue
		}

		// 跳过正在处理请求的账号
		if atomic.LoadInt64(&acc.ActiveRequests) > 0 {
			continue
		}

		// 检查用量是否 >= 100%
		pct, valid := acc.GetUsagePercent7d()
		if !valid || pct < 100.0 {
			continue
		}

		if s.db != nil {
			if err := s.db.SoftDeleteAccount(ctx, acc.DBID); err != nil {
				log.Printf("[账号 %d] 清理用量满账号失败: %v", acc.DBID, err)
				continue
			}
		}

		s.RemoveAccount(acc.DBID)
		log.Printf("[账号 %d] 用量 %.1f%% 已满，已自动清理 (email=%s)", acc.DBID, pct, acc.Email)
		if s.db != nil {
			s.db.InsertAccountEventAsync(acc.DBID, "deleted", "clean_full_usage")
		}
		cleaned++
	}

	if cleaned > 0 {
		log.Printf("用量清理完成: 共清理 %d 个满用量账号", cleaned)
	}
	return cleaned
}

// CleanExpiredAccounts 清理加入号池超过指定时长的账号（不管是否被调用过）
// 批量操作优化：先收集所有过期 ID，再一次性完成数据库更新和内存移除
func (s *Store) CleanExpiredAccounts(ctx context.Context, maxAge time.Duration) int {
	accounts := s.Accounts()
	now := time.Now()
	cutoff := now.Add(-maxAge).UnixNano()

	// 1. 收集所有需要清理的账号 ID
	var expiredIDs []int64
	var skipNoAddedAt, skipNotExpired, skipActive, skipProven int
	for _, acc := range accounts {
		if acc == nil {
			continue
		}
		// 锁定账号跳过自动清理
		if atomic.LoadInt32(&acc.Locked) == 1 {
			continue
		}
		addedAt := atomic.LoadInt64(&acc.AddedAt)
		if addedAt == 0 {
			skipNoAddedAt++
			continue
		}
		if addedAt > cutoff {
			skipNotExpired++
			continue
		}
		if atomic.LoadInt64(&acc.ActiveRequests) > 0 {
			skipActive++
			continue
		}
		// 成功请求超过 10 次的账号保留，不做过期清理
		if atomic.LoadInt64(&acc.TotalRequests) > 10 {
			skipProven++
			continue
		}
		expiredIDs = append(expiredIDs, acc.DBID)
	}

	log.Printf("过期清理扫描: 总数=%d, 待清理=%d, 跳过(无时间=%d, 未过期=%d, 处理中=%d, 已验证=%d)",
		len(accounts), len(expiredIDs), skipNoAddedAt, skipNotExpired, skipActive, skipProven)

	if len(expiredIDs) == 0 {
		return 0
	}

	log.Printf("过期清理: 发现 %d 个超时账号，开始批量处理", len(expiredIDs))

	// 2. 批量更新数据库状态
	if s.db != nil {
		if err := s.db.BatchSoftDeleteAccounts(ctx, expiredIDs); err != nil {
			log.Printf("过期清理: 批量更新数据库失败: %v，回退逐条处理", err)
			return s.cleanExpiredFallback(ctx, expiredIDs)
		}
	}

	// 3. 批量从内存池移除
	s.RemoveAccounts(expiredIDs)

	// 4. 批量写入事件日志（异步）
	if s.db != nil {
		s.db.BatchInsertAccountEventsAsync(expiredIDs, "deleted", "clean_expired")
	}

	log.Printf("过期清理完成: 共清理 %d 个超时账号", len(expiredIDs))
	return len(expiredIDs)
}

// cleanExpiredFallback 批量操作失败时逐条回退处理
func (s *Store) cleanExpiredFallback(ctx context.Context, ids []int64) int {
	cleaned := 0
	for _, id := range ids {
		if err := s.db.SoftDeleteAccount(ctx, id); err != nil {
			log.Printf("[账号 %d] 过期清理失败: %v", id, err)
			continue
		}
		s.RemoveAccount(id)
		s.db.InsertAccountEventAsync(id, "deleted", "clean_expired")
		cleaned++
	}
	if cleaned > 0 {
		log.Printf("过期清理(回退): 共清理 %d 个超时账号", cleaned)
	}
	return cleaned
}

// RemoveAccounts 批量从内存池移除账号（一次加锁、一次遍历，避免 O(n²)）
func (s *Store) RemoveAccounts(dbIDs []int64) {
	if len(dbIDs) == 0 {
		return
	}

	removeSet := make(map[int64]struct{}, len(dbIDs))
	for _, id := range dbIDs {
		removeSet[id] = struct{}{}
	}

	s.mu.Lock()
	kept := s.accounts[:0]
	for _, acc := range s.accounts {
		if _, remove := removeSet[acc.DBID]; remove {
			s.fastSchedulerRemove(acc.DBID)
			if scheduler := s.GetRefreshScheduler(); scheduler != nil {
				scheduler.CancelTask(acc.DBID)
			}
		} else {
			kept = append(kept, acc)
		}
	}
	s.accounts = kept
	s.mu.Unlock()
}

func (s *Store) parallelProbeUsage(ctx context.Context) {
	s.usageProbeMu.RLock()
	probeFn := s.usageProbe
	s.usageProbeMu.RUnlock()
	if probeFn == nil {
		return
	}

	s.mu.RLock()
	accounts := make([]*Account, len(s.accounts))
	copy(accounts, s.accounts)
	s.mu.RUnlock()

	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup

	for _, acc := range accounts {
		if !acc.NeedsUsageProbe(s.GetUsageProbeMaxAge()) {
			continue
		}
		if !acc.TryBeginUsageProbe() {
			continue
		}

		wg.Add(1)
		sem <- struct{}{}
		go func(account *Account) {
			defer wg.Done()
			defer func() { <-sem }()
			defer account.FinishUsageProbe()

			probeCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
			defer cancel()
			if err := probeFn(probeCtx, account); err != nil {
				log.Printf("[账号 %d] 用量探针失败: %v", account.DBID, err)
			}
		}(acc)
	}

	wg.Wait()
}

func (s *Store) parallelProbeImageQuota(ctx context.Context) {
	s.imageQuotaProbeMu.RLock()
	probeFn := s.imageQuotaProbe
	s.imageQuotaProbeMu.RUnlock()
	if probeFn == nil {
		return
	}

	s.mu.RLock()
	accounts := make([]*Account, len(s.accounts))
	copy(accounts, s.accounts)
	s.mu.RUnlock()

	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup

	for _, acc := range accounts {
		if !acc.NeedsImageQuotaProbe(s.GetImageQuotaProbeMaxAge()) {
			continue
		}
		if !acc.TryBeginImageQuotaProbe() {
			continue
		}

		wg.Add(1)
		sem <- struct{}{}
		go func(account *Account) {
			defer wg.Done()
			defer func() { <-sem }()
			defer account.FinishImageQuotaProbe()

			probeCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
			defer cancel()
			if err := probeFn(probeCtx, account); err != nil {
				log.Printf("[账号 %d] 网页图片额度探针失败: %v", account.DBID, err)
			}
		}(acc)
	}

	wg.Wait()
}

func (s *Store) parallelRecoveryProbe(ctx context.Context) {
	s.usageProbeMu.RLock()
	probeFn := s.usageProbe
	s.usageProbeMu.RUnlock()
	if probeFn == nil {
		return
	}

	s.mu.RLock()
	accounts := make([]*Account, len(s.accounts))
	copy(accounts, s.accounts)
	s.mu.RUnlock()

	sem := make(chan struct{}, 2)
	var wg sync.WaitGroup

	for _, acc := range accounts {
		if !acc.NeedsRecoveryProbe(s.GetRecoveryProbeInterval()) {
			continue
		}
		if !acc.TryBeginRecoveryProbe() {
			continue
		}

		wg.Add(1)
		sem <- struct{}{}
		go func(account *Account) {
			defer wg.Done()
			defer func() { <-sem }()
			defer account.FinishRecoveryProbe()

			probeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()

			if account.NeedsRefresh() {
				if err := s.refreshAccount(probeCtx, account); err != nil {
					log.Printf("[账号 %d] 恢复探测前刷新失败: %v", account.DBID, err)
				}
			}

			if err := probeFn(probeCtx, account); err != nil {
				log.Printf("[账号 %d] 恢复探测失败: %v", account.DBID, err)
			} else {
				// 探测成功：将账号从 banned 升级到 warm，给予重新调度的机会
				atomic.StoreInt32(&account.Disabled, 0) // 清除原子禁用标志
				account.mu.Lock()
				if account.HealthTier == HealthTierBanned {
					account.HealthTier = HealthTierWarm
					account.SchedulerScore = 80
					account.FailureStreak = 0
					account.SuccessStreak = 1
					account.LastSuccessAt = time.Now()
					if account.Status == StatusCooldown {
						account.Status = StatusReady
						account.CooldownUtil = time.Time{}
						account.CooldownReason = ""
					}
					account.recomputeSchedulerLocked(atomic.LoadInt64(&s.maxConcurrency))
					log.Printf("[账号 %d] 恢复探测成功！已从 banned 升级到 warm", account.DBID)
				}
				account.mu.Unlock()
				// 清理数据库冷却状态
				if s.db != nil {
					_ = s.db.ClearCooldown(context.Background(), account.DBID)
				}
			}
		}(acc)
	}

	wg.Wait()
}

// RefreshSingle 刷新单个账号（供 admin handler 调用）
func (s *Store) RefreshSingle(ctx context.Context, dbID int64) error {
	s.mu.RLock()
	var target *Account
	for _, acc := range s.accounts {
		if acc.DBID == dbID {
			target = acc
			break
		}
	}
	s.mu.RUnlock()

	if target == nil {
		return fmt.Errorf("账号 %d 不存在", dbID)
	}
	if err := s.refreshAccount(ctx, target); err != nil {
		return err
	}

	// full_usage 等待态下，手动刷新后立即补一次探针。
	// 若额度已提前恢复，可立即退出等待模式；若仍满用量则保持等待。
	if _, reason, active := target.GetCooldownSnapshot(); !(active && reason == "full_usage") {
		return nil
	}

	s.usageProbeMu.RLock()
	probeFn := s.usageProbe
	s.usageProbeMu.RUnlock()
	if probeFn == nil {
		return nil
	}
	if !target.TryBeginUsageProbe() {
		return nil
	}
	defer target.FinishUsageProbe()

	probeCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	if err := probeFn(probeCtx, target); err != nil {
		log.Printf("[账号 %d] 刷新后用量探针失败: %v", target.DBID, err)
	}
	return nil
}

// AccountCount 返回账号数量
func (s *Store) AccountCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.accounts)
}

// AvailableCount 返回可用账号数量
func (s *Store) AvailableCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	count := 0
	for _, acc := range s.accounts {
		if acc.IsAvailable() {
			count++
		}
	}
	return count
}

// Accounts 返回所有账号（用于统计）
func (s *Store) Accounts() []*Account {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]*Account, len(s.accounts))
	copy(result, s.accounts)
	return result
}

// ==================== 并行刷新 ====================

// parallelRefreshAll 并行刷新所有需要刷新的账号（Worker Pool，并发度 10）
func (s *Store) parallelRefreshAll(ctx context.Context) {
	s.mu.RLock()
	accounts := make([]*Account, len(s.accounts))
	copy(accounts, s.accounts)
	s.mu.RUnlock()

	sem := make(chan struct{}, 10)
	var wg sync.WaitGroup

	for i, acc := range accounts {
		if acc.Status == StatusError {
			continue
		}
		if acc.IsBanned() {
			continue
		}
		if _, reason, active := acc.GetCooldownSnapshot(); active || reason == "rate_limited" {
			continue
		}
		// AT-only 账号无 RT，无法刷新
		acc.mu.RLock()
		hasRT := acc.RefreshToken != ""
		acc.mu.RUnlock()
		if !hasRT {
			continue
		}
		if !acc.NeedsRefresh() {
			continue
		}

		wg.Add(1)
		sem <- struct{}{}
		go func(idx int, account *Account) {
			defer wg.Done()
			defer func() { <-sem }()

			if err := s.refreshAccount(ctx, account); err != nil {
				log.Printf("[账号 %d] 刷新失败: %v", idx+1, err)
			} else {
				log.Printf("[账号 %d] 刷新成功: email=%s", idx+1, account.Email)
			}
		}(i, acc)
	}
	wg.Wait()
}

// refreshAccount 刷新单个账号的 AT（带缓存锁与 token 缓存）
func (s *Store) refreshAccount(ctx context.Context, acc *Account) error {
	acc.mu.RLock()
	rt := acc.RefreshToken
	dbID := acc.DBID
	cooldownUntil := acc.CooldownUtil
	cooldownReason := acc.CooldownReason
	rateLimitedHold := acc.Status == StatusCooldown && cooldownReason == "rate_limited"
	activeCooldown := acc.Status == StatusCooldown && (time.Now().Before(acc.CooldownUtil) || rateLimitedHold)
	expiredCooldown := acc.Status == StatusCooldown && !time.Now().Before(acc.CooldownUtil) && !rateLimitedHold
	acc.mu.RUnlock()

	proxy := s.ResolveProxyForAccount(acc)

	// 1. 尝试从缓存读取 AT
	cachedToken, err := s.tokenCache.GetAccessToken(ctx, dbID)
	if err == nil && cachedToken != "" {
		acc.mu.Lock()
		acc.AccessToken = cachedToken
		if acc.ExpiresAt.IsZero() || time.Until(acc.ExpiresAt) < 5*time.Minute {
			acc.ExpiresAt = time.Now().Add(30 * time.Minute)
		}
		if activeCooldown {
			acc.Status = StatusCooldown
			acc.CooldownUtil = cooldownUntil
			acc.CooldownReason = cooldownReason
		} else {
			acc.Status = StatusReady
			acc.CooldownUtil = time.Time{}
			acc.CooldownReason = ""
		}
		acc.recomputeSchedulerLocked(atomic.LoadInt64(&s.maxConcurrency))
		acc.mu.Unlock()
		s.fastSchedulerUpdate(acc)
		if expiredCooldown {
			_ = s.db.ClearCooldown(ctx, dbID)
		}
		return nil
	}

	// 2. 获取刷新锁
	acquired, lockErr := s.tokenCache.AcquireRefreshLock(ctx, dbID, 30*time.Second)
	if lockErr != nil {
		log.Printf("[账号 %d] 获取刷新锁失败: %v", dbID, lockErr)
	}
	if !acquired && lockErr == nil {
		// 另一个进程在刷新，等待它完成
		token, waitErr := s.tokenCache.WaitForRefreshComplete(ctx, dbID, 30*time.Second)
		if waitErr == nil && token != "" {
			acc.mu.Lock()
			acc.AccessToken = token
			acc.ExpiresAt = time.Now().Add(55 * time.Minute)
			if activeCooldown {
				acc.Status = StatusCooldown
				acc.CooldownUtil = cooldownUntil
				acc.CooldownReason = cooldownReason
			} else {
				acc.Status = StatusReady
				acc.CooldownUtil = time.Time{}
				acc.CooldownReason = ""
			}
			acc.recomputeSchedulerLocked(atomic.LoadInt64(&s.maxConcurrency))
			acc.mu.Unlock()
			s.fastSchedulerUpdate(acc)
			if expiredCooldown {
				_ = s.db.ClearCooldown(ctx, dbID)
			}
			return nil
		}
	} else if acquired {
		defer s.tokenCache.ReleaseRefreshLock(ctx, dbID)
	}

	// 3. 执行 RT 刷新
	td, info, err := RefreshWithRetry(ctx, rt, proxy)
	if err != nil {
		if isNonRetryable(err) {
			acc.mu.Lock()
			acc.Status = StatusError
			acc.ErrorMsg = err.Error()
			acc.mu.Unlock()
			s.fastSchedulerUpdate(acc)

			_ = s.db.SetError(ctx, dbID, err.Error())
		}
		return err
	}

	// 4. 更新内存状态
	acc.mu.Lock()
	acc.AccessToken = td.AccessToken
	acc.RefreshToken = td.RefreshToken
	acc.ExpiresAt = td.ExpiresAt
	acc.ErrorMsg = ""
	if info != nil {
		acc.AccountID = info.ChatGPTAccountID
		acc.Email = info.Email
	}
	if activeCooldown {
		acc.Status = StatusCooldown
		acc.CooldownUtil = cooldownUntil
		acc.CooldownReason = cooldownReason
	} else {
		acc.Status = StatusReady
		acc.CooldownUtil = time.Time{}
		acc.CooldownReason = ""
	}
	acc.recomputeSchedulerLocked(atomic.LoadInt64(&s.maxConcurrency))
	acc.mu.Unlock()
	s.fastSchedulerUpdate(acc)

	// 5. 写入缓存
	ttl := time.Until(td.ExpiresAt) - 5*time.Minute
	if ttl > 0 {
		_ = s.tokenCache.SetAccessToken(ctx, dbID, td.AccessToken, ttl)
	}

	// 6. 更新数据库 credentials
	credentials := map[string]interface{}{
		"refresh_token": td.RefreshToken,
		"access_token":  td.AccessToken,
		"id_token":      td.IDToken,
		"expires_at":    td.ExpiresAt.Format(time.RFC3339),
	}
	if info != nil {
		credentials["account_id"] = info.ChatGPTAccountID
		credentials["email"] = info.Email
	}
	if err := s.db.UpdateCredentials(ctx, dbID, credentials); err != nil {
		log.Printf("[账号 %d] 更新数据库失败: %v", dbID, err)
	}

	// 自动锁定 free 以上的账号（pro/plus/team/teamplus 等）
	if info != nil && atomic.LoadInt32(&acc.Locked) == 0 {
		plan := strings.ToLower(info.PlanType)
		if plan != "" && plan != "free" {
			atomic.StoreInt32(&acc.Locked, 1)
			_ = s.db.SetAccountLocked(ctx, dbID, true)
			log.Printf("[账号 %d] 检测到 %s 套餐，已自动锁定", dbID, info.PlanType)
		}
	}

	if expiredCooldown {
		if err := s.db.ClearCooldown(ctx, dbID); err != nil {
			log.Printf("[账号 %d] 清理过期冷却状态失败: %v", dbID, err)
		}
	}

	return nil
}
