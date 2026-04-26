package wsrelay

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/codex2api/auth"
)

func TestAcquireConnectionReusesIdleConnectedConnection(t *testing.T) {
	manager := NewManager()
	t.Cleanup(manager.Stop)
	manager.probeFunc = func(wc *WsConnection) bool { return true }

	account := &auth.Account{DBID: 42}
	wsURL := "wss://example.test/responses"
	key := manager.poolKey(account.ID(), wsURL, "session-1", "")

	session := NewSession(account.ID(), manager)
	session.SetConnected(true)
	conn := &WsConnection{
		session:  session,
		URL:      wsURL,
		PoolKey:  key,
		httpResp: &http.Response{StatusCode: http.StatusSwitchingProtocols},
	}
	conn.SetState(StateConnected)
	conn.Touch()
	manager.connections.Store(key, conn)
	manager.sessions.Store(key, session)

	got, pr, err := manager.AcquireConnection(context.Background(), account, wsURL, "session-1", http.Header{}, "")
	if err != nil {
		t.Fatalf("AcquireConnection() error = %v", err)
	}
	if got != conn {
		t.Fatal("expected existing connection to be reused")
	}
	if pr == nil {
		t.Fatal("expected pending request reservation")
	}
	if session.PendingCount() != 1 {
		t.Fatalf("PendingCount = %d, want 1", session.PendingCount())
	}
	session.RemovePendingRequest(pr.RequestID)
}

func TestPoolKeyIncludesSessionAndProxyScope(t *testing.T) {
	manager := NewManager()
	t.Cleanup(manager.Stop)

	keyA := manager.poolKey(42, "wss://example.test/responses", "session-a", "http://proxy-a")
	keyB := manager.poolKey(42, "wss://example.test/responses", "session-b", "http://proxy-a")
	keyC := manager.poolKey(42, "wss://example.test/responses", "session-a", "http://proxy-b")
	if keyA == keyB {
		t.Fatal("expected different session keys to produce different pool keys")
	}
	if keyA == keyC {
		t.Fatal("expected different proxies to produce different pool keys")
	}
}

func TestCanReuseConnection(t *testing.T) {
	manager := NewManager()
	t.Cleanup(manager.Stop)

	t.Run("idle connected session can be reused", func(t *testing.T) {
		session := NewSession(42, manager)
		session.SetConnected(true)
		conn := &WsConnection{session: session}
		conn.SetState(StateConnected)
		conn.Touch()

		if !canReuseConnection(conn) {
			t.Fatal("expected connection to be reusable")
		}
	})

	t.Run("pending request blocks reuse", func(t *testing.T) {
		session := NewSession(42, manager)
		session.SetConnected(true)
		pending := session.AddPendingRequest("session-a")
		t.Cleanup(func() { session.RemovePendingRequest(pending.RequestID) })

		conn := &WsConnection{session: session}
		conn.SetState(StateConnected)
		conn.Touch()

		if canReuseConnection(conn) {
			t.Fatal("expected connection with pending request to be non-reusable")
		}
	})

	t.Run("expired connection cannot be reused", func(t *testing.T) {
		session := NewSession(42, manager)
		session.SetConnected(true)
		conn := &WsConnection{session: session}
		conn.SetState(StateConnected)
		conn.lastUsed.Store(time.Now().Add(-IdleTimeout - time.Second).UnixNano())

		if canReuseConnection(conn) {
			t.Fatal("expected expired connection to be non-reusable")
		}
	})
}

func TestAcquireConnectionWaitsWhileSessionHasPendingRequest(t *testing.T) {
	manager := NewManager()
	t.Cleanup(manager.Stop)
	manager.probeFunc = func(wc *WsConnection) bool { return true }

	account := &auth.Account{DBID: 42}
	wsURL := "wss://example.test/responses"
	key := manager.poolKey(account.ID(), wsURL, "session-1", "")

	session := NewSession(account.ID(), manager)
	session.SetConnected(true)
	blocking := session.AddPendingRequest("session-1")
	t.Cleanup(func() { session.RemovePendingRequest(blocking.RequestID) })

	conn := &WsConnection{
		session:  session,
		URL:      wsURL,
		PoolKey:  key,
		httpResp: &http.Response{StatusCode: http.StatusSwitchingProtocols},
	}
	conn.SetState(StateConnected)
	conn.Touch()
	manager.connections.Store(key, conn)
	manager.sessions.Store(key, session)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	_, _, err := manager.AcquireConnection(ctx, account, wsURL, "session-1", http.Header{}, "")
	if err == nil {
		t.Fatal("expected acquire to stop when session stays busy until context timeout")
	}
}
