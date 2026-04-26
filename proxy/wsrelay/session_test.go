package wsrelay

import (
	"testing"
	"time"
)

// TestPendingRequestCloseIdempotent 测试 PendingRequest.Close 幂等性
func TestPendingRequestCloseIdempotent(t *testing.T) {
	pr := NewPendingRequest("test-session")

	// 第一次关闭应该成功
	pr.Close()

	// 验证 closed 标志已设置
	pr.closeMu.Lock()
	closed := pr.closed
	pr.closeMu.Unlock()

	if !closed {
		t.Error("expected closed flag to be true after first Close()")
	}

	// 第二次关闭不应该 panic
	pr.Close()

	// 第三次关闭也不应该 panic
	pr.Close()
}

// TestPendingRequestConcurrentClose 测试并发 Close 安全性
func TestPendingRequestConcurrentClose(t *testing.T) {
	pr := NewPendingRequest("test-session")

	done := make(chan bool, 10)

	// 启动多个 goroutine 同时 Close
	for i := 0; i < 10; i++ {
		go func() {
			pr.Close()
			done <- true
		}()
	}

	// 等待所有 goroutine 完成
	for i := 0; i < 10; i++ {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("timeout waiting for concurrent Close")
		}
	}

	// 验证最终状态是 closed
	pr.closeMu.Lock()
	closed := pr.closed
	pr.closeMu.Unlock()

	if !closed {
		t.Error("expected closed flag to be true after concurrent Close")
	}
}

// TestSessionAddAndRemovePendingRequest 测试会话的请求管理
func TestSessionAddAndRemovePendingRequest(t *testing.T) {
	session := NewSession(12345, nil)

	// 添加请求
	pr := session.AddPendingRequest("test-session")
	if pr == nil {
		t.Fatal("expected PendingRequest to be created")
	}

	// 获取请求
	foundPr, ok := session.GetPendingRequest(pr.RequestID)
	if !ok {
		t.Fatal("expected to find PendingRequest")
	}
	if foundPr.RequestID != pr.RequestID {
		t.Errorf("expected RequestID %s, got %s", pr.RequestID, foundPr.RequestID)
	}

	// 移除请求
	session.RemovePendingRequest(pr.RequestID)

	// 验证已移除
	_, ok = session.GetPendingRequest(pr.RequestID)
	if ok {
		t.Error("expected PendingRequest to be removed")
	}
}

func TestSessionPendingCount(t *testing.T) {
	session := NewSession(12345, nil)
	if got := session.PendingCount(); got != 0 {
		t.Fatalf("PendingCount() = %d, want 0", got)
	}

	pr1 := session.AddPendingRequest("a")
	pr2 := session.AddPendingRequest("b")
	if got := session.PendingCount(); got != 2 {
		t.Fatalf("PendingCount() = %d, want 2", got)
	}

	session.RemovePendingRequest(pr1.RequestID)
	if got := session.PendingCount(); got != 1 {
		t.Fatalf("PendingCount() = %d, want 1", got)
	}
	session.RemovePendingRequest(pr2.RequestID)
}

// TestSessionExpiration 测试会话过期判断
func TestSessionExpiration(t *testing.T) {
	session := NewSession(12345, nil)

	// 新创建的会话不应该过期
	if session.IsExpired() {
		t.Error("newly created session should not be expired")
	}

	// 手动设置 LastActiveAt 为很久以前
	session.mu.Lock()
	session.LastActiveAt = time.Now().Add(-10 * time.Minute)
	session.mu.Unlock()

	// 现在应该过期（IdleTimeout = 5分钟）
	if !session.IsExpired() {
		t.Error("session should be expired after 10 minutes of inactivity")
	}
}

// TestSessionConnectedState 测试会话连接状态
func TestSessionConnectedState(t *testing.T) {
	session := NewSession(12345, nil)

	// 初始状态为未连接
	if session.IsConnected() {
		t.Error("newly created session should not be connected")
	}

	// 设置为已连接
	session.SetConnected(true)
	if !session.IsConnected() {
		t.Error("session should be connected after SetConnected(true)")
	}

	// 设置为未连接
	session.SetConnected(false)
	if session.IsConnected() {
		t.Error("session should not be connected after SetConnected(false)")
	}
}
