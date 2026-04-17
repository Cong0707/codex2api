package admin

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy"
)

const (
	autoPlanSyncIntervalDefault = 2 * time.Hour
	autoPlanSyncIntervalFree    = 5 * time.Minute
	forcedPlanSyncMaxAttempts   = 8
)

var forcedPlanSyncRetryDelays = []time.Duration{
	0,
	10 * time.Second,
	20 * time.Second,
	40 * time.Second,
	1 * time.Minute,
	2 * time.Minute,
	3 * time.Minute,
	5 * time.Minute,
}

func forcedPlanSyncDelayForAttempt(attempt int) time.Duration {
	if attempt <= 1 {
		return 0
	}
	idx := attempt - 1
	if idx >= 0 && idx < len(forcedPlanSyncRetryDelays) {
		return forcedPlanSyncRetryDelays[idx]
	}
	return 5 * time.Minute
}

func shouldRetryForcedPlanSync(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "账号不在运行时池中") || strings.Contains(msg, "not in runtime pool") {
		return false
	}
	return true
}

func (h *Handler) shouldAutoSyncPlan(account *auth.Account, now time.Time) bool {
	if h == nil || account == nil {
		return false
	}
	plan := auth.NormalizePlanType(account.GetPlanType())
	interval := autoPlanSyncIntervalDefault
	if plan == "" || plan == "free" {
		interval = autoPlanSyncIntervalFree
	}

	id := account.ID()
	h.planSyncMu.Lock()
	defer h.planSyncMu.Unlock()
	if h.planSyncAt == nil {
		h.planSyncAt = make(map[int64]time.Time)
	}
	last := h.planSyncAt[id]
	if !last.IsZero() && now.Sub(last) < interval {
		return false
	}
	// 先占位，避免并发探针重复请求 wham/usage。
	h.planSyncAt[id] = now
	return true
}

func (h *Handler) markPlanSyncAt(accountID int64, at time.Time) {
	if h == nil || accountID <= 0 {
		return
	}
	h.planSyncMu.Lock()
	if h.planSyncAt == nil {
		h.planSyncAt = make(map[int64]time.Time)
	}
	h.planSyncAt[accountID] = at
	h.planSyncMu.Unlock()
}

// syncPlanFromWhamUsage 通过 wham/usage 同步套餐。
// force=true 时忽略间隔限制，且按 wham/usage 结果强制覆盖当前套餐。
func (h *Handler) syncPlanFromWhamUsage(ctx context.Context, account *auth.Account, force bool) (string, error) {
	if h == nil || h.db == nil || account == nil {
		return "", fmt.Errorf("handler 未初始化")
	}
	now := time.Now()
	if !force && !h.shouldAutoSyncPlan(account, now) {
		return "", nil
	}
	if force {
		h.markPlanSyncAt(account.ID(), now)
	}

	account.Mu().RLock()
	accessToken := strings.TrimSpace(account.AccessToken)
	accountID := strings.TrimSpace(account.AccountID)
	accountProxy := strings.TrimSpace(account.ProxyURL)
	currentPlan := auth.NormalizePlanType(account.PlanType)
	account.Mu().RUnlock()
	if accessToken == "" {
		return "", fmt.Errorf("账号缺少 access_token")
	}

	proxyURL := accountProxy
	if proxyURL == "" {
		proxyURL = strings.TrimSpace(h.store.NextProxy())
	}

	planCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	snapshots := fetchEndpointSnapshots(planCtx, openAIWhamUsageURL, accessToken, accountID, proxyURL)
	bestPlan, bestSource, _, _ := pickBestPlanSnapshot(snapshots)
	bestPlan = auth.NormalizePlanType(bestPlan)
	if bestPlan == "" {
		return "", fmt.Errorf("wham/usage 未识别到套餐")
	}

	account.Mu().Lock()
	account.PlanType = bestPlan
	account.Mu().Unlock()

	dbCtx, dbCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer dbCancel()
	if err := h.db.UpdateCredentials(dbCtx, account.ID(), map[string]interface{}{
		"plan_type":             bestPlan,
		"raw_info_refreshed_at": time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		return "", fmt.Errorf("写入套餐失败: %w", err)
	}
	h.db.InsertAccountEventAsync(account.ID(), "plan_refreshed", "auto_wham_usage")
	log.Printf("[账号 %d] 自动套餐识别更新: %s -> %s (%s)", account.ID(), currentPlan, bestPlan, bestSource)
	h.markPlanSyncAt(account.ID(), now)
	return bestPlan, nil
}

// tryAutoSyncPlanFromWhamUsage 自动用 wham/usage 纠正套餐识别。
func (h *Handler) tryAutoSyncPlanFromWhamUsage(ctx context.Context, account *auth.Account) {
	if _, err := h.syncPlanFromWhamUsage(ctx, account, false); err != nil {
		accountID := int64(0)
		if account != nil {
			accountID = account.ID()
		}
		log.Printf("[账号 %d] 自动套餐同步失败: %v", accountID, err)
	}
}

// forceSyncPlanFromWhamUsageByID 强制从 wham/usage 同步套餐（忽略间隔限制）。
func (h *Handler) forceSyncPlanFromWhamUsageByID(ctx context.Context, accountID int64) error {
	if h == nil || h.store == nil {
		return fmt.Errorf("handler/store 未初始化")
	}
	account := h.store.FindByID(accountID)
	if account == nil {
		return fmt.Errorf("账号不在运行时池中")
	}
	_, err := h.syncPlanFromWhamUsage(ctx, account, true)
	return err
}

func (h *Handler) triggerForcedPlanSync(accountID int64, source string) {
	if h == nil || accountID <= 0 {
		return
	}
	go func() {
		var lastErr error
		for attempt := 1; attempt <= forcedPlanSyncMaxAttempts; attempt++ {
			if delay := forcedPlanSyncDelayForAttempt(attempt); delay > 0 {
				time.Sleep(delay)
			}

			syncCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			err := h.forceSyncPlanFromWhamUsageByID(syncCtx, accountID)
			cancel()
			if err == nil {
				if attempt > 1 {
					log.Printf("[账号 %d] %s 套餐同步重试成功 (attempt %d)", accountID, source, attempt)
				}
				return
			}

			lastErr = err
			if !shouldRetryForcedPlanSync(err) {
				log.Printf("[账号 %d] %s 套餐同步终止: %v", accountID, source, err)
				return
			}
			log.Printf("[账号 %d] %s 套餐同步失败 (attempt %d/%d): %v", accountID, source, attempt, forcedPlanSyncMaxAttempts, err)
		}
		if lastErr != nil {
			log.Printf("[账号 %d] %s 套餐同步最终失败: %v", accountID, source, lastErr)
		}
	}()
}

// ProbeUsageSnapshot 主动发送最小探针请求刷新账号用量
func (h *Handler) ProbeUsageSnapshot(ctx context.Context, account *auth.Account) error {
	if account == nil {
		return nil
	}

	account.Mu().RLock()
	hasToken := account.AccessToken != ""
	account.Mu().RUnlock()
	if !hasToken {
		return nil
	}

	payload := buildTestPayload(h.store.GetTestModel())
	proxyURL := h.store.NextProxy()
	resp, err := proxy.ExecuteRequest(ctx, account, payload, "", proxyURL, "", nil, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if usagePct, ok := proxy.ParseCodexUsageHeaders(resp, account); ok {
		h.store.PersistUsageSnapshot(account, usagePct)
	}

	body, _ := io.ReadAll(resp.Body)
	displayStatus := proxy.NormalizeUpstreamStatusCode(resp.StatusCode, body)
	errCode, errMsg := proxy.ParseUpstreamErrorBrief(body)

	switch {
	case resp.StatusCode == http.StatusOK:
		h.store.ReportRequestSuccess(account, 0)
		h.tryAutoSyncPlanFromWhamUsage(ctx, account)
		if _, cooldownReason, active := account.GetCooldownSnapshot(); active && cooldownReason == "full_usage" {
			// 允许提前恢复：探针成功后按最新用量快照重判；
			// 仍满用量则继续等待，不满用量则立即退出等待模式。
			if h.store.MarkFullUsageCooldownFromSnapshot(account) {
				return nil
			}
		}
		h.store.ClearCooldown(account)
		return nil
	case proxy.IsUnauthorizedLikeStatus(resp.StatusCode, body):
		if errCode == "" {
			errCode = "unauthorized"
		}
		if errMsg == "" {
			errMsg = "Unauthorized"
		}
		account.SetLastFailureDetail(displayStatus, errCode, errMsg)
		h.store.ReportRequestFailure(account, "unauthorized", 0)
		h.store.MarkCooldown(account, 24*time.Hour, "unauthorized")
		return nil
	case resp.StatusCode == http.StatusTooManyRequests:
		account.SetLastFailureDetail(http.StatusTooManyRequests, "rate_limited", "Rate limited")
		h.store.ReportRequestFailure(account, "client", 0)
		if _, cooldownReason, _ := account.GetCooldownSnapshot(); cooldownReason == "full_usage" {
			if h.store.MarkFullUsageCooldownFromSnapshot(account) {
				return nil
			}
			// 没有可用 reset 时间时，至少再等待一个测活周期
			h.store.MarkCooldown(account, auth.FullUsageProbeInterval, "full_usage")
			return nil
		}
		if h.store.MarkFullUsageCooldownFromSnapshot(account) {
			return nil
		}
		h.store.ExtendRateLimitedCooldown(account, auth.RateLimitedProbeInterval)
		return nil
	default:
		if resp.StatusCode >= 500 {
			h.store.ReportRequestFailure(account, "server", 0)
		} else if resp.StatusCode >= 400 {
			h.store.ReportRequestFailure(account, "client", 0)
		}
		return fmt.Errorf("探针返回状态 %d", resp.StatusCode)
	}
}
