package proxy

import (
	"encoding/json"
	"testing"
)

func TestTrimRawItemsTail_MaxItems(t *testing.T) {
	items := []json.RawMessage{
		json.RawMessage(`{"i":1}`),
		json.RawMessage(`{"i":2}`),
		json.RawMessage(`{"i":3}`),
		json.RawMessage(`{"i":4}`),
	}

	trimmed := trimRawItemsTail(items, 2, 0)
	if len(trimmed) != 2 {
		t.Fatalf("len(trimmed) = %d, want 2", len(trimmed))
	}
	if string(trimmed[0]) != `{"i":3}` || string(trimmed[1]) != `{"i":4}` {
		t.Fatalf("unexpected tail result: %s, %s", string(trimmed[0]), string(trimmed[1]))
	}
}

func TestTrimRawItemsTail_MaxBytes(t *testing.T) {
	items := []json.RawMessage{
		json.RawMessage(`{"v":"aaaaaaaaaa"}`),
		json.RawMessage(`{"v":"bbbbbbbbbb"}`),
		json.RawMessage(`{"v":"cccccccccc"}`),
	}

	// 每项约 18 字节，限制 25 字节时应至少保留最后一项，最多两项。
	trimmed := trimRawItemsTail(items, 10, 25)
	if len(trimmed) < 1 || len(trimmed) > 2 {
		t.Fatalf("len(trimmed) = %d, want 1..2", len(trimmed))
	}
	if string(trimmed[len(trimmed)-1]) != `{"v":"cccccccccc"}` {
		t.Fatalf("tail item mismatch: %s", string(trimmed[len(trimmed)-1]))
	}
}
