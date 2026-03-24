package auth

import (
	"context"
	"fmt"
	"log"
	"math"
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
	Status         AccountStatus
	CooldownUtil   time.Time
	CooldownReason string // rate_limited / unauthorized / 空
	ErrorMsg       string

	// 用量进度（从 Codex 响应头被动解析）
	UsagePercent7d        float64 // 7d 窗口使用率 0-100+
	UsagePercent7dValid   bool
	UsageUpdatedAt        time.Time
	usageProbeInFlight    bool
	recoveryProbeInFlight bool

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

	// 高并发调度指标（原子操作，无需锁）
	ActiveRequests int64 // 当前并发请求数
	TotalRequests  int64 // 累计总请求数
	LastUsedAt     int64 // 最后使用时间（UnixNano）
}

// SchedulerBreakdown 调度评分拆解
type SchedulerBreakdown struct {
	UnauthorizedPenalty float64
	RateLimitPenalty    float64
	TimeoutPenalty      float64
	ServerPenalty       float64
	FailurePenalty      float64
	SuccessBonus        float64
	UsagePenalty7d      float64
	LatencyPenalty      float64
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

func concurrencyLimitForTier(baseLimit int64, tier AccountHealthTier) int64 {
	if baseLimit <= 0 {
		baseLimit = 1
	}

	switch tier {
	case HealthTierHealthy:
		if baseLimit >= 3 {
			return 3
		}
		return baseLimit
	case HealthTierWarm:
		return 1
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

func (a *Account) schedulerBreakdownLocked() SchedulerBreakdown {
	now := time.Now()
	breakdown := SchedulerBreakdown{}

	if !a.LastUnauthorizedAt.IsZero() {
		since := now.Sub(a.LastUnauthorizedAt)
		switch {
		case since < time.Hour:
			breakdown.UnauthorizedPenalty = 50
		case since < 6*time.Hour:
			breakdown.UnauthorizedPenalty = 30
		case since < 24*time.Hour:
			breakdown.UnauthorizedPenalty = 15
		}
	}
	if !a.LastRateLimitedAt.IsZero() {
		since := now.Sub(a.LastRateLimitedAt)
		switch {
		case since < 10*time.Minute:
			breakdown.RateLimitPenalty = 22
		case since < time.Hour:
			breakdown.RateLimitPenalty = 10
		}
	}
	if !a.LastTimeoutAt.IsZero() && now.Sub(a.LastTimeoutAt) < 15*time.Minute {
		breakdown.TimeoutPenalty = 18
	}
	if !a.LastServerErrorAt.IsZero() && now.Sub(a.LastServerErrorAt) < 15*time.Minute {
		breakdown.ServerPenalty = 12
	}

	breakdown.FailurePenalty = float64(clampInt(a.FailureStreak*6, 0, 24))
	breakdown.SuccessBonus = float64(clampInt(a.SuccessStreak*2, 0, 12))

	if a.UsagePercent7dValid && strings.EqualFold(a.PlanType, "free") {
		switch {
		case a.UsagePercent7d >= 100:
			breakdown.UsagePenalty7d = 40
		case a.UsagePercent7d >= 95:
			breakdown.UsagePenalty7d = 30
		case a.UsagePercent7d >= 85:
			breakdown.UsagePenalty7d = 18
		case a.UsagePercent7d >= 70:
			breakdown.UsagePenalty7d = 8
		}
	}

	switch {
	case a.LatencyEWMA >= 20000:
		breakdown.LatencyPenalty = 15
	case a.LatencyEWMA >= 10000:
		breakdown.LatencyPenalty = 8
	case a.LatencyEWMA >= 5000:
		breakdown.LatencyPenalty = 4
	}

	return breakdown
}

func (a *Account) recomputeSchedulerLocked(baseLimit int64) {
	now := time.Now()
	breakdown := a.schedulerBreakdownLocked()
	score := 100.0 -
		breakdown.UnauthorizedPenalty -
		breakdown.RateLimitPenalty -
		breakdown.TimeoutPenalty -
		breakdown.ServerPenalty -
		breakdown.FailurePenalty -
		breakdown.UsagePenalty7d -
		breakdown.LatencyPenalty +
		breakdown.SuccessBonus

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
	if a.UsagePercent7dValid && strings.EqualFold(a.PlanType, "free") {
		switch {
		case a.UsagePercent7d >= 95:
			tier = HealthTierRisky
		case a.UsagePercent7d >= 85 && tier == HealthTierHealthy:
			tier = HealthTierWarm
		}
	}
	if a.HealthTier == HealthTierBanned {
		tier = HealthTierBanned
	}

	a.HealthTier = tier
	a.SchedulerScore = score
	a.DynamicConcurrencyLimit = concurrencyLimitForTier(baseLimit, tier)
}

func (a *Account) schedulerSnapshot(baseLimit int64) (AccountHealthTier, float64, int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.recomputeSchedulerLocked(baseLimit)
	return a.HealthTier, a.SchedulerScore, a.DynamicConcurrencyLimit
}

// IsAvailable 检查账号是否可用
func (a *Account) IsAvailable() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()

	if a.Status == StatusError {
		return false
	}
	if a.healthTierLocked() == HealthTierBanned {
		return false
	}
	if a.Status == StatusCooldown && time.Now().Before(a.CooldownUtil) {
		return false
	}
	// 冷却期过了自动恢复
	if a.Status == StatusCooldown && !time.Now().Before(a.CooldownUtil) {
		return a.AccessToken != ""
	}
	return a.AccessToken != ""
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
	switch a.Status {
	case StatusError:
		return "error"
	case StatusCooldown:
		if time.Now().Before(a.CooldownUtil) {
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
	return true
}

// FinishUsageProbe 结束一次用量探针
func (a *Account) FinishUsageProbe() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.usageProbeInFlight = false
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

// Store 多账号管理器（PG + Redis）
type Store struct {
	mu                    sync.RWMutex
	accounts              []*Account
	globalProxy           string
	maxConcurrency        int64        // 每账号最大并发数
	testConcurrency       int64        // 批量测试并发数
	testModel             atomic.Value // 测试连接使用的模型（string）
	db                    *database.DB
	tokenCache            *cache.TokenCache
	usageProbeMu          sync.RWMutex
	usageProbe            func(context.Context, *Account) error
	usageProbeBatch       atomic.Bool
	recoveryProbeBatch    atomic.Bool
	autoCleanUnauthorized atomic.Bool
	autoCleanRateLimited  atomic.Bool
	autoCleanupBatch      atomic.Bool
	stopCh                chan struct{}
	wg                    sync.WaitGroup
}

// NewStore 创建账号管理器
func NewStore(db *database.DB, tc *cache.TokenCache, settings *database.SystemSettings) *Store {
	if settings == nil {
		settings = &database.SystemSettings{
			MaxConcurrency:  2,
			TestConcurrency: 50,
			TestModel:       "gpt-5.4",
			ProxyURL:        "",
		}
	}
	s := &Store{
		globalProxy:     settings.ProxyURL,
		maxConcurrency:  int64(settings.MaxConcurrency),
		testConcurrency: int64(settings.TestConcurrency),
		db:              db,
		tokenCache:      tc,
		stopCh:          make(chan struct{}),
	}
	s.testModel.Store(settings.TestModel)
	s.autoCleanUnauthorized.Store(settings.AutoCleanUnauthorized)
	s.autoCleanRateLimited.Store(settings.AutoCleanRateLimited)
	return s
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

// Init 初始化：从 PG 加载账号
func (s *Store) Init(ctx context.Context) error {
	// 1. 从 PG 加载账号
	if err := s.loadFromDB(ctx); err != nil {
		return err
	}

	if len(s.accounts) == 0 {
		log.Println("⚠ 数据库中暂无账号，请通过管理后台添加")
		return nil
	}

	// 2. 并行刷新所有账号的 AT
	s.parallelRefreshAll(ctx)

	successCount := 0
	for _, acc := range s.accounts {
		if acc.IsAvailable() {
			successCount++
		}
	}

	if successCount == 0 {
		log.Println("⚠ 所有账号刷新失败，服务仍将启动")
		return nil
	}

	log.Printf("账号初始化完成: %d/%d 成功", successCount, len(s.accounts))
	return nil
}

// loadFromDB 从 PostgreSQL 加载账号
func (s *Store) loadFromDB(ctx context.Context) error {
	rows, err := s.db.ListActive(ctx)
	if err != nil {
		return fmt.Errorf("从数据库加载账号失败: %w", err)
	}

	for _, row := range rows {
		rt := row.GetCredential("refresh_token")
		if rt == "" {
			log.Printf("[账号 %d] 缺少 refresh_token，跳过", row.ID)
			continue
		}

		proxy := row.ProxyURL
		if proxy == "" {
			proxy = s.globalProxy
		}

		account := &Account{
			DBID:         row.ID,
			RefreshToken: rt,
			ProxyURL:     proxy,
			HealthTier:   HealthTierWarm,
		}

		// 尝试从 credentials 恢复已有的 AT
		if at := row.GetCredential("access_token"); at != "" {
			account.AccessToken = at
			account.AccountID = row.GetCredential("account_id")
			account.Email = row.GetCredential("email")
			account.PlanType = row.GetCredential("plan_type")
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
			if time.Now().Before(row.CooldownUntil.Time) {
				account.SetCooldownUntil(row.CooldownUntil.Time, row.CooldownReason)
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
			} else {
				log.Printf("[账号 %d] 解析 codex_7d_used_percent 失败: %v", row.ID, err)
			}
		}
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
		refreshTicker := time.NewTicker(2 * time.Minute)
		autoCleanupTicker := time.NewTicker(30 * time.Second)
		defer refreshTicker.Stop()
		defer autoCleanupTicker.Stop()

		for {
			select {
			case <-refreshTicker.C:
				s.parallelRefreshAll(context.Background())
				s.TriggerUsageProbeAsync()
				s.TriggerRecoveryProbeAsync()
			case <-autoCleanupTicker.C:
				s.TriggerAutoCleanupAsync()
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

		if s.db != nil {
			if err := s.db.SetError(ctx, acc.DBID, "deleted"); err != nil {
				log.Printf("[账号 %d] 清理 %s 状态失败: %v", acc.DBID, targetStatus, err)
				continue
			}
		}

		s.RemoveAccount(acc.DBID)
		cleaned++
	}

	return cleaned
}

// ==================== 最少连接调度 ====================

// Next 获取下一个可用账号（健康优先 + 低负载择优）
func (s *Store) Next() *Account {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var best *Account
	bestPriority := -1
	bestScore := -math.MaxFloat64
	var bestLoad int64 = math.MaxInt64
	maxConcurrency := atomic.LoadInt64(&s.maxConcurrency)

	for _, acc := range s.accounts {
		if !acc.IsAvailable() {
			continue
		}

		load := atomic.LoadInt64(&acc.ActiveRequests)
		tier, score, limit := acc.schedulerSnapshot(maxConcurrency)
		if limit <= 0 || load >= limit {
			continue
		}

		priority := tierPriority(tier)
		if priority > bestPriority ||
			(priority == bestPriority && (score > bestScore ||
				(score == bestScore && load < bestLoad))) {
			bestPriority = priority
			bestScore = score
			bestLoad = load
			best = acc
		}
	}

	if best != nil {
		atomic.AddInt64(&best.ActiveRequests, 1)
		atomic.AddInt64(&best.TotalRequests, 1)
		atomic.StoreInt64(&best.LastUsedAt, time.Now().UnixNano())
	}
	return best
}

// WaitForAvailable 等待可用账号（带超时的请求排队）
func (s *Store) WaitForAvailable(ctx context.Context, timeout time.Duration) *Account {
	deadline := time.After(timeout)
	backoff := 50 * time.Millisecond

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-deadline:
			return nil
		default:
			acc := s.Next()
			if acc != nil {
				return acc
			}
			// 等待一下再重试（指数退避，最大 500ms）
			select {
			case <-time.After(backoff):
				if backoff < 500*time.Millisecond {
					backoff *= 2
				}
			case <-ctx.Done():
				return nil
			case <-deadline:
				return nil
			}
		}
	}
}

// Release 释放账号（请求完成后调用，递减并发计数）
func (s *Store) Release(acc *Account) {
	if acc == nil {
		return
	}
	atomic.AddInt64(&acc.ActiveRequests, -1)
}

// SetMaxConcurrency 动态更新每账号并发上限
func (s *Store) SetMaxConcurrency(n int) {
	atomic.StoreInt64(&s.maxConcurrency, int64(n))
}

// GetMaxConcurrency 获取当前每账号并发上限
func (s *Store) GetMaxConcurrency() int {
	return int(atomic.LoadInt64(&s.maxConcurrency))
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

// AddAccount 热加载新账号到内存池（前端添加后即刻生效）
func (s *Store) AddAccount(acc *Account) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.accounts = append(s.accounts, acc)
}

// RemoveAccount 从内存池移除账号
func (s *Store) RemoveAccount(dbID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i, acc := range s.accounts {
		if acc.DBID == dbID {
			s.accounts = append(s.accounts[:i], s.accounts[i+1:]...)
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
		acc.LastFailureAt = now
		acc.FailureStreak++
		acc.SuccessStreak = 0
		if acc.healthTierLocked() == HealthTierHealthy {
			acc.HealthTier = HealthTierWarm
		} else {
			acc.HealthTier = HealthTierRisky
		}
	}
	acc.recomputeSchedulerLocked(atomic.LoadInt64(&s.maxConcurrency))
	acc.mu.Unlock()

	until := now.Add(duration)
	acc.SetCooldownUntil(until, reason)

	if s.db == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.db.SetCooldown(ctx, acc.DBID, reason, until); err != nil {
		log.Printf("[账号 %d] 持久化冷却状态失败: %v", acc.DBID, err)
	}
}

// ClearCooldown 清除账号冷却状态，并同步清理数据库
func (s *Store) ClearCooldown(acc *Account) {
	if acc == nil {
		return
	}

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
	defer acc.mu.Unlock()

	acc.recordLatencyLocked(latency)
	acc.LastSuccessAt = time.Now()
	acc.SuccessStreak = clampInt(acc.SuccessStreak+1, 0, 20)
	acc.FailureStreak = 0
	if acc.HealthTier == "" {
		acc.HealthTier = HealthTierHealthy
	}
	acc.recomputeSchedulerLocked(atomic.LoadInt64(&s.maxConcurrency))
}

// ReportRequestFailure 记录一次失败请求，用于动态调度评分
func (s *Store) ReportRequestFailure(acc *Account, kind string, latency time.Duration) {
	if acc == nil {
		return
	}

	now := time.Now()
	acc.mu.Lock()
	defer acc.mu.Unlock()

	acc.recordLatencyLocked(latency)
	acc.LastFailureAt = now
	acc.FailureStreak = clampInt(acc.FailureStreak+1, 0, 20)
	acc.SuccessStreak = 0

	switch kind {
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
}

// PersistUsageSnapshot 持久化账号 7d 用量快照
func (s *Store) PersistUsageSnapshot(acc *Account, pct7d float64) {
	if acc == nil {
		return
	}

	now := time.Now()
	acc.SetUsageSnapshot(pct7d, now)

	if s.db == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.db.UpdateUsageSnapshot(ctx, acc.DBID, pct7d, now); err != nil {
		log.Printf("[账号 %d] 持久化用量快照失败: %v", acc.DBID, err)
	}
}

// SetUsageProbeFunc 注册主动探针回调
func (s *Store) SetUsageProbeFunc(fn func(context.Context, *Account) error) {
	s.usageProbeMu.Lock()
	defer s.usageProbeMu.Unlock()
	s.usageProbe = fn
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
	if !s.GetAutoCleanUnauthorized() && !s.GetAutoCleanRateLimited() {
		return
	}

	cleanupCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	cleanedUnauthorized := 0
	cleanedRateLimited := 0

	if s.GetAutoCleanUnauthorized() {
		cleanedUnauthorized = s.CleanByRuntimeStatus(cleanupCtx, "unauthorized")
	}
	if s.GetAutoCleanRateLimited() {
		cleanedRateLimited = s.CleanByRuntimeStatus(cleanupCtx, "rate_limited")
	}

	if cleanedUnauthorized > 0 || cleanedRateLimited > 0 {
		log.Printf("自动清理完成: unauthorized=%d, rate_limited=%d", cleanedUnauthorized, cleanedRateLimited)
	}
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
		if !acc.NeedsUsageProbe(10 * time.Minute) {
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
		if !acc.NeedsRecoveryProbe(30 * time.Minute) {
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
	return s.refreshAccount(ctx, target)
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
		if acc.HasActiveCooldown() {
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

// refreshAccount 刷新单个账号的 AT（带 Redis 锁和缓存）
func (s *Store) refreshAccount(ctx context.Context, acc *Account) error {
	acc.mu.RLock()
	rt := acc.RefreshToken
	proxy := acc.ProxyURL
	dbID := acc.DBID
	cooldownUntil := acc.CooldownUtil
	cooldownReason := acc.CooldownReason
	activeCooldown := acc.Status == StatusCooldown && time.Now().Before(acc.CooldownUtil)
	expiredCooldown := acc.Status == StatusCooldown && !time.Now().Before(acc.CooldownUtil)
	acc.mu.RUnlock()

	// 1. 尝试从 Redis 缓存读取 AT
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
		if expiredCooldown {
			_ = s.db.ClearCooldown(ctx, dbID)
		}
		return nil
	}

	// 2. 获取分布式刷新锁
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
		acc.PlanType = info.PlanType
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

	// 5. 写入 Redis 缓存
	ttl := time.Until(td.ExpiresAt) - 5*time.Minute
	if ttl > 0 {
		_ = s.tokenCache.SetAccessToken(ctx, dbID, td.AccessToken, ttl)
	}

	// 6. 更新 PG credentials
	credentials := map[string]interface{}{
		"refresh_token": td.RefreshToken,
		"access_token":  td.AccessToken,
		"id_token":      td.IDToken,
		"expires_at":    td.ExpiresAt.Format(time.RFC3339),
	}
	if info != nil {
		credentials["account_id"] = info.ChatGPTAccountID
		credentials["email"] = info.Email
		credentials["plan_type"] = info.PlanType
	}
	if err := s.db.UpdateCredentials(ctx, dbID, credentials); err != nil {
		log.Printf("[账号 %d] 更新数据库失败: %v", dbID, err)
	}
	if expiredCooldown {
		if err := s.db.ClearCooldown(ctx, dbID); err != nil {
			log.Printf("[账号 %d] 清理过期冷却状态失败: %v", dbID, err)
		}
	}

	return nil
}
