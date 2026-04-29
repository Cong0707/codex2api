package admin

import (
	"context"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy"
)

func (h *Handler) ProbeImageQuotaSnapshot(ctx context.Context, account *auth.Account) error {
	if h == nil || h.store == nil || account == nil {
		return nil
	}

	officialAvailable := auth.OfficialImageQuotaForAccount(account)
	proxyURL := h.store.ResolveProxyForAccount(account)
	client, err := proxy.NewWebImageClient(account, proxyURL)
	if err != nil {
		return err
	}

	quota, err := client.ProbeImageQuota(ctx)
	if err != nil {
		return err
	}
	if quota == nil {
		return nil
	}

	updatedAt := quota.UpdatedAt
	if updatedAt.IsZero() {
		updatedAt = time.Now().UTC()
	}
	h.store.PersistImageQuotaSnapshot(account, quota.Remaining, quota.Total, quota.ResetAt, updatedAt, officialAvailable)
	return nil
}
