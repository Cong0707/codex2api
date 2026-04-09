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
)

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
	last := h.planSyncAt[id]
	if !last.IsZero() && now.Sub(last) < interval {
		return false
	}
	// 先占位，避免并发探针重复请求 wham/usage。
	h.planSyncAt[id] = now
	return true
}

// tryAutoSyncPlanFromWhamUsage 自动用 wham/usage 纠正套餐识别（优先修正 free/空套餐）。
func (h *Handler) tryAutoSyncPlanFromWhamUsage(ctx context.Context, account *auth.Account) {
	if h == nil || h.db == nil || account == nil {
		return
	}
	now := time.Now()
	if !h.shouldAutoSyncPlan(account, now) {
		return
	}

	account.Mu().RLock()
	accessToken := strings.TrimSpace(account.AccessToken)
	accountID := strings.TrimSpace(account.AccountID)
	accountProxy := strings.TrimSpace(account.ProxyURL)
	currentPlan := auth.NormalizePlanType(account.PlanType)
	account.Mu().RUnlock()
	if accessToken == "" {
		return
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
		return
	}

	// 为了稳健，自动同步只做“升级修正”，避免偶发异常把高套餐降级。
	if currentPlan != "" && !isPlanBetter(currentPlan, bestPlan) {
		return
	}

	account.Mu().Lock()
	account.PlanType = bestPlan
	account.Mu().Unlock()

	dbCtx, dbCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer dbCancel()
	_ = h.db.UpdateCredentials(dbCtx, account.ID(), map[string]interface{}{
		"plan_type":             bestPlan,
		"raw_info_refreshed_at": time.Now().UTC().Format(time.RFC3339),
	})
	h.db.InsertAccountEventAsync(account.ID(), "plan_refreshed", "auto_wham_usage")
	log.Printf("[账号 %d] 自动套餐识别更新: %s -> %s (%s)", account.ID(), currentPlan, bestPlan, bestSource)
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

	_, _ = io.Copy(io.Discard, resp.Body)

	switch resp.StatusCode {
	case http.StatusOK:
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
	case http.StatusUnauthorized:
		account.SetLastFailureDetail(http.StatusUnauthorized, "unauthorized", "Unauthorized")
		h.store.ReportRequestFailure(account, "client", 0)
		h.store.MarkCooldown(account, 24*time.Hour, "unauthorized")
		return nil
	case http.StatusTooManyRequests:
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
