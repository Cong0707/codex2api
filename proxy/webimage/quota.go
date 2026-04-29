package webimage

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type ImageQuota struct {
	Remaining int
	Total     int
	ResetAt   time.Time
	UpdatedAt time.Time
}

func (c *Client) ProbeImageQuota(ctx context.Context) (*ImageQuota, error) {
	if c == nil {
		return nil, fmt.Errorf("client is nil")
	}
	_ = c.Bootstrap(ctx)

	body, _ := json.Marshal(map[string]any{
		"gizmo_id":                nil,
		"requested_default_model": nil,
		"conversation_id":         nil,
		"timezone_offset_min":     -480,
		"system_hints":            []string{"picture_v2"},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.opts.BaseURL+"/backend-api/conversation/init", strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	c.commonHeaders(req)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "*/*")

	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, &UpstreamError{Status: resp.StatusCode, Message: "conversation/init failed", Body: string(raw)}
	}

	var payload struct {
		LimitsProgress []struct {
			FeatureName string `json:"feature_name"`
			Remaining   *int   `json:"remaining"`
			ResetAfter  string `json:"reset_after"`
			MaxValue    *int   `json:"max_value"`
			Cap         *int   `json:"cap"`
			Total       *int   `json:"total"`
			Limit       *int   `json:"limit"`
		} `json:"limits_progress"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("decode conversation/init: %w", err)
	}

	quota := &ImageQuota{Remaining: -1, Total: -1, UpdatedAt: time.Now().UTC()}
	for _, item := range payload.LimitsProgress {
		if !isImageFeature(item.FeatureName) {
			continue
		}
		if item.Remaining != nil {
			if quota.Remaining < 0 || *item.Remaining < quota.Remaining {
				quota.Remaining = *item.Remaining
			}
		}
		if value := firstInt(item.MaxValue, item.Cap, item.Total, item.Limit); value != nil {
			if quota.Total < 0 || *value > quota.Total {
				quota.Total = *value
			}
		}
		if item.ResetAfter != "" {
			if parsed, parseErr := time.Parse(time.RFC3339, item.ResetAfter); parseErr == nil {
				if quota.ResetAt.IsZero() || parsed.Before(quota.ResetAt) {
					quota.ResetAt = parsed
				}
			}
		}
	}

	// 2026-04 起部分账号的 conversation/init 只返回 image_gen.remaining，
	// 不再携带 max_value/cap/total/limit。此时用 remaining 作为首次容量下界，
	// 调用方会用已有快照继续保留更大的历史 total。
	if quota.Remaining >= 0 && quota.Total < 0 {
		quota.Total = quota.Remaining
	}
	if quota.Total >= 0 && quota.Remaining > quota.Total {
		quota.Total = quota.Remaining
	}
	// 200 响应但没有 image_gen 条目时，视作当前不可用而不是探针失败，
	// 避免后台对同一批账号持续刷屏重试。
	if quota.Remaining < 0 && quota.Total < 0 {
		quota.Remaining = 0
		quota.Total = 0
	}
	if quota.Remaining < 0 || quota.Total < 0 {
		return nil, fmt.Errorf("image quota not found in conversation/init")
	}
	return quota, nil
}

func firstInt(values ...*int) *int {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}

func isImageFeature(name string) bool {
	lower := strings.ToLower(strings.TrimSpace(name))
	switch lower {
	case "image_gen", "image_generation", "image_edit", "img_gen":
		return true
	}
	return strings.Contains(lower, "image_gen") || strings.Contains(lower, "img_gen")
}
