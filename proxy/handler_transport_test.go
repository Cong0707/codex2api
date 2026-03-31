package proxy

import (
	"net/http"
	"testing"

	"github.com/codex2api/config"
)

func TestShouldUseWebsocketTransport(t *testing.T) {
	cfg := &config.Config{UseWebsocket: true}

	req, err := http.NewRequest(http.MethodGet, "http://localhost/v1/responses", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")

	if !shouldUseWebsocketTransport(cfg, req) {
		t.Fatal("expected websocket transport to be enabled for websocket upgrade request")
	}
}

func TestShouldUseWebsocketTransport_HTTPRequest(t *testing.T) {
	cfg := &config.Config{UseWebsocket: true}

	req, err := http.NewRequest(http.MethodPost, "http://localhost/v1/responses", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Connection", "keep-alive")

	if shouldUseWebsocketTransport(cfg, req) {
		t.Fatal("expected normal HTTP request to stay on HTTP upstream path")
	}
}
