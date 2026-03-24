package auth

import (
	"context"
	"fmt"
	"log"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/codex2api/cache"
	"github.com/codex2api/config"
	"github.com/codex2api/database"
)

// AccountStatus 账号状态
type AccountStatus int

const (
	StatusReady    AccountStatus = iota // 可用
	StatusCooldown                      // 冷却中（被限速）
	StatusError                         // 不可用（RT 失效等）
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
	UsagePercent7d float64 // 7d 窗口使用率 0-100+

	// 高并发调度指标（原子操作，无需锁）
	ActiveRequests int64 // 当前并发请求数
	TotalRequests  int64 // 累计总请求数
	LastUsedAt     int64 // 最后使用时间（UnixNano）
}

// ID 返回数据库 ID
func (a *Account) ID() int64 {
	return a.DBID
}

// Mu 返回读写锁（供外部包安全读取字段）
func (a *Account) Mu() *sync.RWMutex {
	return &a.mu
}

// IsAvailable 检查账号是否可用
func (a *Account) IsAvailable() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()

	if a.Status == StatusError {
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
}

// GetCooldownReason 获取冷却原因
func (a *Account) GetCooldownReason() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.CooldownReason
}

// RuntimeStatus 返回运行时状态字符串（供 admin API 使用）
func (a *Account) RuntimeStatus() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
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
	a.mu.Lock()
	defer a.mu.Unlock()
	a.UsagePercent7d = pct
}

// GetUsagePercent7d 获取 7d 用量百分比
func (a *Account) GetUsagePercent7d() float64 {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.UsagePercent7d
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
	mu              sync.RWMutex
	accounts        []*Account
	globalProxy     string
	maxConcurrency  int64        // 每账号最大并发数
	testConcurrency int64        // 批量测试并发数
	testModel       atomic.Value // 测试连接使用的模型（string）
	db              *database.DB
	tokenCache      *cache.TokenCache
	stopCh          chan struct{}
	wg              sync.WaitGroup
}

// NewStore 创建账号管理器
func NewStore(cfg *config.Config, db *database.DB, tc *cache.TokenCache) *Store {
	s := &Store{
		globalProxy:     cfg.ProxyURL,
		maxConcurrency:  int64(cfg.MaxConcurrency),
		testConcurrency: int64(cfg.TestConcurrency),
		db:              db,
		tokenCache:      tc,
		stopCh:          make(chan struct{}),
	}
	s.testModel.Store(cfg.TestModel)
	return s
}

// Init 初始化：从 PG 加载账号
func (s *Store) Init(ctx context.Context, cfg *config.Config) error {
	// 1. 把配置文件/环境变量中的 RT 导入数据库（首次启动用）
	if len(cfg.Accounts) > 0 {
		count, _ := s.db.CountAll(ctx)
		if count == 0 {
			log.Println("数据库为空，导入配置中的账号...")
			for i, ac := range cfg.Accounts {
				name := fmt.Sprintf("account-%d", i+1)
				id, err := s.db.InsertAccount(ctx, name, ac.RefreshToken, ac.ProxyURL)
				if err != nil {
					log.Printf("[账号 %d] 导入失败: %v", i+1, err)
				} else {
					log.Printf("[账号 %d] 导入成功: id=%d", i+1, id)
				}
			}
		}
	}

	// 2. 从 PG 加载账号
	if err := s.loadFromDB(ctx); err != nil {
		return err
	}

	if len(s.accounts) == 0 {
		log.Println("⚠ 数据库中暂无账号，请通过管理后台添加")
		return nil
	}

	// 3. 并行刷新所有账号的 AT
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
		}

		// 尝试从 credentials 恢复已有的 AT
		if at := row.GetCredential("access_token"); at != "" {
			account.AccessToken = at
			account.AccountID = row.GetCredential("account_id")
			account.Email = row.GetCredential("email")
			account.PlanType = row.GetCredential("plan_type")
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
		ticker := time.NewTicker(2 * time.Minute)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				s.parallelRefreshAll(context.Background())
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

// ==================== 最少连接调度 ====================

// Next 获取下一个可用账号（最少连接调度 + 并发上限）
// 选择 ActiveRequests 最小的可用账号，可用账号的并发不能超过 maxConcurrency
func (s *Store) Next() *Account {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var best *Account
	var bestLoad int64 = math.MaxInt64

	for _, acc := range s.accounts {
		if !acc.IsAvailable() {
			continue
		}
		load := atomic.LoadInt64(&acc.ActiveRequests)
		if load >= s.maxConcurrency {
			continue // 超过并发上限，跳过
		}
		if load < bestLoad {
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

	until := time.Now().Add(duration)
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
	if acc.Status == StatusCooldown {
		acc.Status = StatusReady
	}
	acc.CooldownUtil = time.Time{}
	acc.CooldownReason = ""
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
	acc.mu.RUnlock()

	// 1. 尝试从 Redis 缓存读取 AT
	cachedToken, err := s.tokenCache.GetAccessToken(ctx, dbID)
	if err == nil && cachedToken != "" {
		acc.mu.Lock()
		acc.AccessToken = cachedToken
		if acc.ExpiresAt.IsZero() || time.Until(acc.ExpiresAt) < 5*time.Minute {
			acc.ExpiresAt = time.Now().Add(30 * time.Minute)
		}
		acc.Status = StatusReady
		acc.mu.Unlock()
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
			acc.Status = StatusReady
			acc.mu.Unlock()
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
	acc.Status = StatusReady
	acc.ErrorMsg = ""
	if info != nil {
		acc.AccountID = info.ChatGPTAccountID
		acc.Email = info.Email
		acc.PlanType = info.PlanType
	}
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

	return nil
}
