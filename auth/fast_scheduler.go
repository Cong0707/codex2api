package auth

import (
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

var fastSchedulerTierOrder = []AccountHealthTier{
	HealthTierHealthy,
	HealthTierWarm,
	HealthTierRisky,
}

type fastSchedulerEntry struct {
	acc       *Account
	dbID      int64
	score     float64
	preferred bool
}

type fastSchedulerPosition struct {
	tier  AccountHealthTier
	index int
}

// FastScheduler 是一个仅使用本地内存的调度器 POC。
// 它不在请求热路径内重算全量 score，而是直接复用 Account 上已缓存的
// HealthTier / SchedulerScore / DynamicConcurrencyLimit。
//
// 调度策略：两阶段扫描
// 1. 优先在验证过的账号（score > 100，排在桶前部）中 round-robin
// 2. 验证账号全忙时，回退到全量 round-robin
type FastScheduler struct {
	mu                 sync.RWMutex
	baseLimit          int64
	preferredPlan      string
	preferredPlanBonus float64
	buckets            map[AccountHealthTier][]fastSchedulerEntry
	positions          map[int64]fastSchedulerPosition
	cursors            [3]atomic.Uint64
	preferredCursors   [3]atomic.Uint64
	provenBounds       [3]int           // 每个 tier 桶中验证过的账号数量（排在前面）
	provenCurs         [3]atomic.Uint64 // 验证账号专用 round-robin 游标
}

func NewFastScheduler(baseLimit int64, preferredPlan string, preferredPlanBonus float64) *FastScheduler {
	if baseLimit <= 0 {
		baseLimit = 1
	}
	if preferredPlanBonus < 0 {
		preferredPlanBonus = 0
	}
	return &FastScheduler{
		baseLimit:          baseLimit,
		preferredPlan:      normalizePreferredPlanType(preferredPlan),
		preferredPlanBonus: preferredPlanBonus,
		buckets: map[AccountHealthTier][]fastSchedulerEntry{
			HealthTierHealthy: nil,
			HealthTierWarm:    nil,
			HealthTierRisky:   nil,
		},
		positions: make(map[int64]fastSchedulerPosition),
	}
}

// BuildFastScheduler 用当前 Store 快照构建一个独立 scheduler。
// 该方法不会影响现有生产流量路径，只用于 POC/benchmark/灰度验证。
func (s *Store) BuildFastScheduler() *FastScheduler {
	if s == nil {
		return NewFastScheduler(1, "", 0)
	}
	scheduler := NewFastScheduler(
		atomic.LoadInt64(&s.maxConcurrency),
		s.GetPreferredPlanType(),
		float64(s.GetPreferredPlanBonus()),
	)

	s.mu.RLock()
	accounts := make([]*Account, len(s.accounts))
	copy(accounts, s.accounts)
	s.mu.RUnlock()

	scheduler.Rebuild(accounts)
	return scheduler
}

func (s *FastScheduler) Rebuild(accounts []*Account) {
	if s == nil {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.buckets = map[AccountHealthTier][]fastSchedulerEntry{
		HealthTierHealthy: nil,
		HealthTierWarm:    nil,
		HealthTierRisky:   nil,
	}
	s.positions = make(map[int64]fastSchedulerPosition, len(accounts))

	// 批量插入：先全部放入桶中，不逐条排序
	now := time.Now()
	for _, acc := range accounts {
		if acc == nil || acc.DBID == 0 {
			continue
		}
		tier, score, limit, available := acc.fastSchedulerSnapshot(s.baseLimit, now)
		if !available || limit <= 0 {
			continue
		}
		if tier != HealthTierHealthy && tier != HealthTierWarm && tier != HealthTierRisky {
			continue
		}
		score += s.planBonusForAccount(acc)
		preferred := s.isPreferredAccount(acc)
		s.buckets[tier] = append(s.buckets[tier], fastSchedulerEntry{
			acc:       acc,
			dbID:      acc.DBID,
			score:     score,
			preferred: preferred,
		})
	}

	// 每个桶只排序一次 + 重建位置索引 + 计算验证账号边界
	for tierIdx, tier := range fastSchedulerTierOrder {
		entries := s.buckets[tier]
		if len(entries) == 0 {
			s.provenBounds[tierIdx] = 0
			continue
		}
		sort.SliceStable(entries, func(i, j int) bool {
			if entries[i].score == entries[j].score {
				return entries[i].dbID < entries[j].dbID
			}
			return entries[i].score > entries[j].score
		})
		s.buckets[tier] = entries
		s.rebuildPositionsLocked(tier)
		s.provenBounds[tierIdx] = countProvenEntries(entries)
	}
}

func (s *FastScheduler) Update(acc *Account) {
	if s == nil || acc == nil {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.removeLocked(acc.DBID)
	s.insertLocked(acc, time.Now())
}

func (s *FastScheduler) Remove(dbID int64) {
	if s == nil || dbID == 0 {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.removeLocked(dbID)
}

func (s *FastScheduler) SetBaseLimit(baseLimit int64) {
	if s == nil {
		return
	}
	if baseLimit <= 0 {
		baseLimit = 1
	}
	s.mu.Lock()
	s.baseLimit = baseLimit
	s.mu.Unlock()
}

func (s *FastScheduler) Acquire() *Account {
	return s.AcquireMatching(nil, nil)
}

// AcquireExcluding 获取下一个可用账号，排除指定的账号 ID 集合
// 两阶段调度：优先在验证过的账号中选取，全忙时回退到全量扫描
func (s *FastScheduler) AcquireExcluding(exclude map[int64]bool) *Account {
	return s.AcquireMatching(exclude, nil)
}

// AcquireMatching 获取下一个满足过滤条件的可用账号。
// matcher 为 nil 时表示不过滤。
func (s *FastScheduler) AcquireMatching(exclude map[int64]bool, matcher AccountMatcher) *Account {
	if s == nil {
		return nil
	}

	now := time.Now()

	s.mu.RLock()
	baseLimit := s.baseLimit
	if s.hasPreferredPlan() {
		for tierIdx, tier := range fastSchedulerTierOrder {
			bucket := s.buckets[tier]
			if len(bucket) == 0 {
				continue
			}
			if acc := s.scanPreferred(bucket, &s.preferredCursors[tierIdx], baseLimit, now, exclude, matcher); acc != nil {
				s.mu.RUnlock()
				return acc
			}
		}
	}
	for tierIdx, tier := range fastSchedulerTierOrder {
		bucket := s.buckets[tier]
		if len(bucket) == 0 {
			continue
		}

		// 阶段 1：优先在验证过的账号（桶前部 provenBound 个）中 round-robin
		provenBound := s.provenBounds[tierIdx]
		if provenBound > 0 {
			if acc := s.scanRange(bucket, 0, provenBound, &s.provenCurs[tierIdx], baseLimit, now, exclude, matcher); acc != nil {
				s.mu.RUnlock()
				return acc
			}
		}

		// 阶段 2：回退到全量 round-robin
		if acc := s.scanRange(bucket, 0, len(bucket), &s.cursors[tierIdx], baseLimit, now, exclude, matcher); acc != nil {
			s.mu.RUnlock()
			return acc
		}
	}
	s.mu.RUnlock()
	return nil
}

// scanRange 在 bucket[start:end) 范围内 round-robin 扫描可用账号
func (s *FastScheduler) scanRange(bucket []fastSchedulerEntry, rangeStart, rangeEnd int, cursor *atomic.Uint64, baseLimit int64, now time.Time, exclude map[int64]bool, matcher AccountMatcher) *Account {
	rangeLen := rangeEnd - rangeStart
	if rangeLen <= 0 {
		return nil
	}
	start := int(cursor.Add(1)-1) % rangeLen
	for offset := 0; offset < rangeLen; offset++ {
		entry := bucket[rangeStart+(start+offset)%rangeLen]
		if entry.acc == nil {
			continue
		}
		if exclude != nil && exclude[entry.dbID] {
			continue
		}
		if matcher != nil && !matcher(entry.acc) {
			continue
		}
		_, _, limit, available := entry.acc.fastSchedulerSnapshot(baseLimit, now)
		if !available || limit <= 0 {
			continue
		}
		if !tryAcquireAccount(entry.acc, limit) {
			continue
		}
		return entry.acc
	}
	return nil
}

func (s *FastScheduler) scanPreferred(bucket []fastSchedulerEntry, cursor *atomic.Uint64, baseLimit int64, now time.Time, exclude map[int64]bool, matcher AccountMatcher) *Account {
	bucketLen := len(bucket)
	if bucketLen == 0 {
		return nil
	}
	start := int(cursor.Add(1)-1) % bucketLen
	for offset := 0; offset < bucketLen; offset++ {
		entry := bucket[(start+offset)%bucketLen]
		if !entry.preferred || entry.acc == nil {
			continue
		}
		if exclude != nil && exclude[entry.dbID] {
			continue
		}
		if matcher != nil && !matcher(entry.acc) {
			continue
		}
		_, _, limit, available := entry.acc.fastSchedulerSnapshot(baseLimit, now)
		if !available || limit <= 0 {
			continue
		}
		if !tryAcquireAccount(entry.acc, limit) {
			continue
		}
		return entry.acc
	}
	return nil
}

func (s *FastScheduler) Release(acc *Account) {
	if acc == nil {
		return
	}
	atomic.AddInt64(&acc.ActiveRequests, -1)
}

func (s *FastScheduler) BucketSizes() map[AccountHealthTier]int {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	return map[AccountHealthTier]int{
		HealthTierHealthy: len(s.buckets[HealthTierHealthy]),
		HealthTierWarm:    len(s.buckets[HealthTierWarm]),
		HealthTierRisky:   len(s.buckets[HealthTierRisky]),
	}
}

func (s *FastScheduler) insertLocked(acc *Account, now time.Time) {
	if acc == nil || acc.DBID == 0 {
		return
	}

	tier, score, limit, available := acc.fastSchedulerSnapshot(s.baseLimit, now)
	if !available || limit <= 0 {
		return
	}
	if tier != HealthTierHealthy && tier != HealthTierWarm && tier != HealthTierRisky {
		return
	}
	score += s.planBonusForAccount(acc)

	entries := append(s.buckets[tier], fastSchedulerEntry{
		acc:       acc,
		dbID:      acc.DBID,
		score:     score,
		preferred: s.isPreferredAccount(acc),
	})
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].score == entries[j].score {
			return entries[i].dbID < entries[j].dbID
		}
		return entries[i].score > entries[j].score
	})
	s.buckets[tier] = entries
	s.rebuildPositionsLocked(tier)
	// 更新该 tier 的验证账号边界
	for tierIdx, t := range fastSchedulerTierOrder {
		if t == tier {
			s.provenBounds[tierIdx] = countProvenEntries(entries)
			break
		}
	}
}

func (s *FastScheduler) removeLocked(dbID int64) {
	pos, ok := s.positions[dbID]
	if !ok {
		return
	}

	entries := s.buckets[pos.tier]
	if pos.index < 0 || pos.index >= len(entries) {
		delete(s.positions, dbID)
		return
	}

	copy(entries[pos.index:], entries[pos.index+1:])
	entries = entries[:len(entries)-1]
	s.buckets[pos.tier] = entries
	delete(s.positions, dbID)
	s.rebuildPositionsLocked(pos.tier)
	// 更新该 tier 的验证账号边界
	for tierIdx, t := range fastSchedulerTierOrder {
		if t == pos.tier {
			s.provenBounds[tierIdx] = countProvenEntries(entries)
			break
		}
	}
}

// countProvenEntries 统计桶中验证过的账号数量（score > 100，排在前面）
// 桶已按 score 降序排列，找到第一个 score <= 100 的位置即为边界
func countProvenEntries(entries []fastSchedulerEntry) int {
	for i, e := range entries {
		if e.score <= 100 {
			return i
		}
	}
	return len(entries) // 全部都是验证过的
}

func (s *FastScheduler) rebuildPositionsLocked(tier AccountHealthTier) {
	for idx, entry := range s.buckets[tier] {
		s.positions[entry.dbID] = fastSchedulerPosition{
			tier:  tier,
			index: idx,
		}
	}
}

func (s *FastScheduler) planBonusForAccount(acc *Account) float64 {
	if s == nil || acc == nil {
		return 0
	}
	return s.planBonusForPlan(acc.GetPlanType())
}

func (s *FastScheduler) hasPreferredPlan() bool {
	return s != nil && s.preferredPlan != ""
}

func (s *FastScheduler) isPreferredAccount(acc *Account) bool {
	if s == nil || acc == nil {
		return false
	}
	return s.isPreferredPlan(acc.GetPlanType())
}

func (s *FastScheduler) isPreferredPlan(plan string) bool {
	if s == nil || s.preferredPlan == "" {
		return false
	}
	return matchPreferredPlan(plan, s.preferredPlan)
}

func (s *FastScheduler) planBonusForPlan(plan string) float64 {
	if s == nil {
		return 0
	}
	if s.preferredPlanBonus <= 0 || s.preferredPlan == "" {
		return 0
	}
	if matchPreferredPlan(plan, s.preferredPlan) {
		return s.preferredPlanBonus
	}
	return 0
}

func (a *Account) fastSchedulerSnapshot(baseLimit int64, now time.Time) (AccountHealthTier, float64, int64, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()

	tier := a.healthTierLocked()
	score := a.SchedulerScore
	limit := a.DynamicConcurrencyLimit

	if score == 0 && tier != HealthTierBanned && a.AccessToken != "" && a.Status != StatusError {
		score = 100
	}
	if limit <= 0 {
		limit = concurrencyLimitForTier(baseLimit, tier)
	}

	available := a.Status != StatusError && tier != HealthTierBanned && a.AccessToken != ""
	if a.Status == StatusCooldown {
		if a.CooldownReason == "rate_limited" || a.CooldownReason == "unauthorized" {
			available = false
		} else if a.CooldownReason == "full_usage" {
			imageExhausted, imageKnown := a.imageQuotaExhaustedLocked(now)
			available = !(a.textQuotaFullLocked(now) && imageKnown && imageExhausted)
		} else if now.Before(a.CooldownUtil) {
			available = false
		}
	}
	if available {
		imageExhausted, imageKnown := a.imageQuotaExhaustedLocked(now)
		if a.textQuotaFullLocked(now) && imageKnown && imageExhausted {
			available = false
		}
	}

	return tier, score, limit, available
}

func tryAcquireAccount(acc *Account, limit int64) bool {
	if acc == nil {
		return false
	}

	if limit <= 0 {
		return false
	}

	for {
		current := atomic.LoadInt64(&acc.ActiveRequests)
		if current >= limit {
			return false
		}
		if atomic.CompareAndSwapInt64(&acc.ActiveRequests, current, current+1) {
			atomic.AddInt64(&acc.TotalRequests, 1)
			atomic.StoreInt64(&acc.LastUsedAt, time.Now().UnixNano())
			return true
		}
	}
}
