package proxy

import (
	"net/http"
	"testing"
)

func TestDetectOfficialImageAvailabilityFromPayload(t *testing.T) {
	payload := []byte(`{"type":"response.completed","response":{"tool_usage":{"image_gen":{"images":1,"quota":{"remaining":17}}}}}`)
	got, ok := detectOfficialImageAvailabilityFromPayload(payload)
	if !ok {
		t.Fatalf("expected official availability to be detected")
	}
	if got != 17 {
		t.Fatalf("availability = %d, want 17", got)
	}
}

func TestDetectOfficialImageAvailabilityFromPayloadIgnoresImageCount(t *testing.T) {
	payload := []byte(`{"type":"response.completed","response":{"tool_usage":{"image_gen":{"images":1}}}}`)
	if got, ok := detectOfficialImageAvailabilityFromPayload(payload); ok {
		t.Fatalf("unexpected availability detected: %d", got)
	}
}

func TestDetectOfficialImageAvailabilityFromHeaders(t *testing.T) {
	header := http.Header{}
	header.Set("X-OpenAI-Images-Remaining", "23")
	got, ok := detectOfficialImageAvailabilityFromHeaders(header)
	if !ok {
		t.Fatalf("expected header availability to be detected")
	}
	if got != 23 {
		t.Fatalf("availability = %d, want 23", got)
	}
}
