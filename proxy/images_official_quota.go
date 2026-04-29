package proxy

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
)

var officialImageAvailabilityPaths = []string{
	"response.tool_usage.image_gen.remaining",
	"response.tool_usage.image_gen.available",
	"response.tool_usage.image_gen.remaining_images",
	"response.tool_usage.image_gen.available_images",
	"response.tool_usage.image_gen.images_remaining",
	"response.tool_usage.image_gen.quota_remaining",
	"response.tool_usage.image_gen.remaining_quota",
	"response.tool_usage.image_gen.quota.remaining",
	"response.tool_usage.image_gen.usage.remaining",
}

var officialImageAvailabilityKeys = map[string]struct{}{
	"remaining":        {},
	"available":        {},
	"remaining_images": {},
	"available_images": {},
	"images_remaining": {},
	"quota_remaining":  {},
	"remaining_quota":  {},
}

func detectOfficialImageAvailabilityFromHeaders(header http.Header) (int, bool) {
	if header == nil {
		return 0, false
	}
	for _, key := range []string{
		"x-openai-image-remaining",
		"x-openai-images-remaining",
		"x-codex-image-remaining",
		"x-codex-images-remaining",
		"x-image-remaining",
		"x-ratelimit-remaining-images",
	} {
		raw := strings.TrimSpace(header.Get(key))
		if raw == "" {
			continue
		}
		if parsed, err := strconv.Atoi(raw); err == nil && parsed >= 0 {
			return parsed, true
		}
	}
	return 0, false
}

func detectOfficialImageAvailabilityFromPayload(payload []byte) (int, bool) {
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return 0, false
	}
	for _, path := range officialImageAvailabilityPaths {
		if parsed, ok := parseOfficialImageAvailabilityResult(gjson.GetBytes(payload, path)); ok {
			return parsed, true
		}
	}
	node := gjson.GetBytes(payload, "response.tool_usage.image_gen")
	if !node.Exists() || !node.IsObject() {
		return 0, false
	}
	var decoded any
	if err := json.Unmarshal([]byte(node.Raw), &decoded); err != nil {
		return 0, false
	}
	return detectOfficialImageAvailabilityFromValue(decoded)
}

func parseOfficialImageAvailabilityResult(result gjson.Result) (int, bool) {
	if !result.Exists() {
		return 0, false
	}
	switch result.Type {
	case gjson.Number:
		value := int(result.Int())
		if value >= 0 {
			return value, true
		}
	case gjson.String:
		raw := strings.TrimSpace(result.String())
		if raw == "" {
			return 0, false
		}
		if parsed, err := strconv.Atoi(raw); err == nil && parsed >= 0 {
			return parsed, true
		}
	}
	return 0, false
}

func detectOfficialImageAvailabilityFromValue(value any) (int, bool) {
	switch typed := value.(type) {
	case map[string]any:
		for key, candidate := range typed {
			if _, ok := officialImageAvailabilityKeys[normalizeOfficialImageQuotaKey(key)]; ok {
				if parsed, ok := parseOfficialImageAvailabilityAny(candidate); ok {
					return parsed, true
				}
			}
		}
		for _, candidate := range typed {
			if parsed, ok := detectOfficialImageAvailabilityFromValue(candidate); ok {
				return parsed, true
			}
		}
	case []any:
		for _, candidate := range typed {
			if parsed, ok := detectOfficialImageAvailabilityFromValue(candidate); ok {
				return parsed, true
			}
		}
	}
	return 0, false
}

func parseOfficialImageAvailabilityAny(value any) (int, bool) {
	switch typed := value.(type) {
	case float64:
		if typed >= 0 {
			return int(typed), true
		}
	case int:
		if typed >= 0 {
			return typed, true
		}
	case int64:
		if typed >= 0 {
			return int(typed), true
		}
	case json.Number:
		if parsed, err := typed.Int64(); err == nil && parsed >= 0 {
			return int(parsed), true
		}
	case string:
		raw := strings.TrimSpace(typed)
		if raw == "" {
			return 0, false
		}
		if parsed, err := strconv.Atoi(raw); err == nil && parsed >= 0 {
			return parsed, true
		}
	}
	return 0, false
}

func normalizeOfficialImageQuotaKey(key string) string {
	key = strings.TrimSpace(strings.ToLower(key))
	key = strings.ReplaceAll(key, "-", "_")
	key = strings.ReplaceAll(key, " ", "_")
	return key
}
