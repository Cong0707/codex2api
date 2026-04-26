package wsrelay

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/codex2api/auth"
	"github.com/gorilla/websocket"
	xproxy "golang.org/x/net/proxy"
)

// ==================== 连接池管理器 ====================

// ConnectionState 连接状态
type ConnectionState int32

const (
	StateDisconnected ConnectionState = 0
	StateConnecting   ConnectionState = 1
	StateConnected    ConnectionState = 2
	StateClosing      ConnectionState = 3
)

// WsConnection WebSocket 连接包装
type WsConnection struct {
	// WebSocket 连接
	conn *websocket.Conn

	// 会话
	session *Session

	// 连接 URL
	URL string

	// 连接池键
	PoolKey string

	// 连接状态
	state atomic.Int32

	// 最后使用时间
	lastUsed atomic.Int64

	// 写操作锁
	writeMu sync.Mutex

	// HTTP 握手响应
	httpResp *http.Response

	// 连接关闭回调
	onDisconnected func(accountID int64)
}

// NewWsConnection 创建 WebSocket 连接
func NewWsConnection(conn *websocket.Conn, session *Session, wsURL string) *WsConnection {
	wc := &WsConnection{
		conn:    conn,
		session: session,
		URL:     wsURL,
	}
	wc.lastUsed.Store(time.Now().UnixNano())
	wc.state.Store(int32(StateConnected))
	return wc
}

// Touch 更新最后使用时间
func (wc *WsConnection) Touch() {
	wc.lastUsed.Store(time.Now().UnixNano())
}

// IsExpired 检查连接是否过期
func (wc *WsConnection) IsExpired() bool {
	lastUsed := time.Unix(0, wc.lastUsed.Load())
	return time.Since(lastUsed) > IdleTimeout
}

// IsConnected 检查是否已连接
func (wc *WsConnection) IsConnected() bool {
	return wc.state.Load() == int32(StateConnected)
}

// Close 安全关闭连接
func (wc *WsConnection) Close() error {
	if wc.state.CompareAndSwap(int32(StateConnected), int32(StateClosing)) ||
		wc.state.CompareAndSwap(int32(StateConnecting), int32(StateClosing)) {
		// 调用断开回调
		if wc.onDisconnected != nil && wc.session != nil {
			wc.onDisconnected(wc.session.AccountID)
		}
		if wc.conn != nil {
			return wc.conn.Close()
		}
		return nil
	}
	return nil
}

// SetState 设置连接状态
func (wc *WsConnection) SetState(state ConnectionState) {
	wc.state.Store(int32(state))
}

// WriteMessage 安全写入消息
func (wc *WsConnection) WriteMessage(messageType int, data []byte) error {
	wc.writeMu.Lock()
	defer wc.writeMu.Unlock()

	if !wc.IsConnected() {
		return fmt.Errorf("websocket connection is not connected")
	}

	wc.conn.SetWriteDeadline(time.Now().Add(WriteTimeout))
	defer wc.conn.SetWriteDeadline(time.Time{})

	return wc.conn.WriteMessage(messageType, data)
}

// ReadMessage 读取消息（带超时）
func (wc *WsConnection) ReadMessage() (int, []byte, error) {
	wc.conn.SetReadDeadline(time.Now().Add(ReadTimeout))
	defer wc.conn.SetReadDeadline(time.Time{})

	msgType, data, err := wc.conn.ReadMessage()
	if err == nil {
		wc.Touch()
	}
	return msgType, data, err
}

// HTTPResponse 返回 HTTP 握手响应
func (wc *WsConnection) HTTPResponse() *http.Response {
	return wc.httpResp
}

// ==================== 连接池管理器 ====================

// Manager WebSocket 连接池管理器
type Manager struct {
	// 连接池（accountID -> *WsConnection）
	connections sync.Map

	// 会话池（accountID -> *Session）
	sessions sync.Map

	// 拨号器配置
	dialer *websocket.Dialer

	// 清理定时器
	cleanupTicker *time.Ticker
	stopCleanup   chan struct{}

	// 连接回调
	onConnected    func(accountID int64, session *Session)
	onDisconnected func(accountID int64)

	// 读写锁保护回调设置
	mu sync.RWMutex

	// pool key 级别串行化，避免同一逻辑 session 并发复用同一条连接
	keyLocks sync.Map

	// 可选的探活函数（测试替换）
	probeFunc func(wc *WsConnection) bool
}

// NewManager 创建连接池管理器
func NewManager() *Manager {
	m := &Manager{
		dialer: &websocket.Dialer{
			Proxy:             http.ProxyFromEnvironment,
			HandshakeTimeout:  HandshakeTimeout,
			EnableCompression: true,
			NetDialContext: (&net.Dialer{
				Timeout:   30 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
		},
		stopCleanup: make(chan struct{}),
	}

	// 启动后台清理
	m.cleanupTicker = time.NewTicker(30 * time.Second)
	go m.cleanupLoop()

	return m
}

func effectiveProxyURL(account *auth.Account, proxyOverride string) string {
	proxyURL := ""
	if account != nil {
		account.Mu().RLock()
		proxyURL = account.ProxyURL
		account.Mu().RUnlock()
	}
	if strings.TrimSpace(proxyOverride) != "" {
		proxyURL = proxyOverride
	}
	return strings.TrimSpace(proxyURL)
}

func applyProxyToWebsocketDialer(dialer *websocket.Dialer, rawProxy string) error {
	if dialer == nil {
		return nil
	}
	rawProxy = strings.TrimSpace(rawProxy)
	if rawProxy == "" {
		return nil
	}

	switch strings.ToLower(rawProxy) {
	case "direct", "none":
		dialer.Proxy = nil
		return nil
	}

	parsed, err := url.Parse(rawProxy)
	if err != nil {
		return fmt.Errorf("parse proxy URL failed: %w", err)
	}

	switch strings.ToLower(parsed.Scheme) {
	case "http", "https":
		dialer.Proxy = http.ProxyURL(parsed)
		return nil
	case "socks5", "socks5h":
		var authInfo *xproxy.Auth
		if parsed.User != nil {
			password, _ := parsed.User.Password()
			authInfo = &xproxy.Auth{
				User:     parsed.User.Username(),
				Password: password,
			}
		}
		socksDialer, err := xproxy.SOCKS5("tcp", parsed.Host, authInfo, xproxy.Direct)
		if err != nil {
			return fmt.Errorf("build socks5 dialer failed: %w", err)
		}
		dialer.Proxy = nil
		dialer.NetDialContext = func(_ context.Context, network, addr string) (net.Conn, error) {
			return socksDialer.Dial(network, addr)
		}
		return nil
	default:
		return fmt.Errorf("unsupported proxy scheme: %s", parsed.Scheme)
	}
}

// cleanupLoop 定期清理过期连接
func (m *Manager) cleanupLoop() {
	for {
		select {
		case <-m.cleanupTicker.C:
			m.evictExpired()
		case <-m.stopCleanup:
			m.cleanupTicker.Stop()
			return
		}
	}
}

// evictExpired 清理过期连接、会话和对应 key 锁
func (m *Manager) evictExpired() {
	activeKeys := make(map[string]struct{})

	m.connections.Range(func(key, value any) bool {
		wc := value.(*WsConnection)
		if wc.IsExpired() || !wc.IsConnected() {
			m.connections.Delete(key)
			wc.Close()
		} else if sk, ok := key.(string); ok {
			activeKeys[sk] = struct{}{}
		}
		return true
	})

	m.sessions.Range(func(key, value any) bool {
		s := value.(*Session)
		if s.IsExpired() || !s.IsConnected() {
			m.sessions.Delete(key)
			s.Close()
		} else if sk, ok := key.(string); ok {
			activeKeys[sk] = struct{}{}
		}
		return true
	})

	m.keyLocks.Range(func(key, _ any) bool {
		if sk, ok := key.(string); ok {
			if _, alive := activeKeys[sk]; !alive {
				m.keyLocks.Delete(key)
			}
		}
		return true
	})
}

// Stop 停止管理器
func (m *Manager) Stop() {
	close(m.stopCleanup)
	m.closeAll()
}

// closeAll 关闭所有连接
func (m *Manager) closeAll() {
	m.connections.Range(func(key, value any) bool {
		wc := value.(*WsConnection)
		m.connections.Delete(key)
		wc.Close()
		return true
	})

	m.sessions.Range(func(key, value any) bool {
		s := value.(*Session)
		m.sessions.Delete(key)
		s.Close()
		return true
	})
}

// SetOnConnected 设置连接回调
func (m *Manager) SetOnConnected(fn func(accountID int64, session *Session)) {
	m.mu.Lock()
	m.onConnected = fn
	m.mu.Unlock()
}

// SetOnDisconnected 设置断开回调
func (m *Manager) SetOnDisconnected(fn func(accountID int64)) {
	m.mu.Lock()
	m.onDisconnected = fn
	m.mu.Unlock()
}

// getOnDisconnected 获取断开回调
func (m *Manager) getOnDisconnected() func(accountID int64) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.onDisconnected
}

// getOnConnected 获取连接回调
func (m *Manager) getOnConnected() func(accountID int64, session *Session) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.onConnected
}

func (m *Manager) keyLock(key string) *sync.Mutex {
	if v, ok := m.keyLocks.Load(key); ok {
		return v.(*sync.Mutex)
	}
	mu := &sync.Mutex{}
	if actual, loaded := m.keyLocks.LoadOrStore(key, mu); loaded {
		return actual.(*sync.Mutex)
	}
	return mu
}

// AcquireConnection 获取或创建连接
// 仅在同一逻辑 session 且连接空闲时复用，避免不同会话/代理复用同一条连接。
func (m *Manager) AcquireConnection(
	ctx context.Context,
	account *auth.Account,
	wsURL string,
	sessionKey string,
	headers http.Header,
	proxyOverride string,
) (*WsConnection, *PendingRequest, error) {
	key := m.poolKey(account.ID(), wsURL, sessionKey, effectiveProxyURL(account, proxyOverride))
	lock := m.keyLock(key)
	wait := 10 * time.Millisecond

	for {
		lock.Lock()
		if v, ok := m.connections.Load(key); ok {
			wc := v.(*WsConnection)
			if canReuseConnection(wc) {
				if m.probe(wc) {
					pr := wc.session.AddPendingRequest(sessionKey)
					wc.Touch()
					lock.Unlock()
					return wc, pr, nil
				}
				m.connections.Delete(key)
				m.sessions.Delete(key)
				m.keyLocks.Delete(key)
				wc.Close()
				lock.Unlock()
				lock = m.keyLock(key)
				continue
			}
			if wc.IsConnected() && !wc.IsExpired() && wc.session != nil {
				lock.Unlock()
				select {
				case <-ctx.Done():
					return nil, nil, ctx.Err()
				case <-time.After(wait):
				}
				continue
			}
			m.connections.Delete(key)
			m.sessions.Delete(key)
			wc.Close()
		}

		wc, err := m.createConnection(ctx, account, wsURL, sessionKey, headers, proxyOverride)
		if err != nil {
			lock.Unlock()
			return nil, nil, err
		}

		m.connections.Store(key, wc)
		pr := wc.session.AddPendingRequest(sessionKey)
		lock.Unlock()

		if fn := m.getOnConnected(); fn != nil {
			fn(account.ID(), wc.session)
		}
		return wc, pr, nil
	}
}

func canReuseConnection(wc *WsConnection) bool {
	if wc == nil {
		return false
	}
	if !wc.IsConnected() || wc.IsExpired() || wc.session == nil {
		return false
	}
	return wc.session.PendingCount() == 0
}

func probeConnection(wc *WsConnection) bool {
	wc.writeMu.Lock()
	defer wc.writeMu.Unlock()

	if !wc.IsConnected() || wc.conn == nil {
		return false
	}
	deadline := time.Now().Add(5 * time.Second)
	return wc.conn.WriteControl(websocket.PingMessage, []byte{}, deadline) == nil
}

func (m *Manager) probe(wc *WsConnection) bool {
	m.mu.RLock()
	fn := m.probeFunc
	m.mu.RUnlock()
	if fn != nil {
		return fn(wc)
	}
	return probeConnection(wc)
}

// createConnection 创建新 WebSocket 连接
func (m *Manager) createConnection(
	ctx context.Context,
	account *auth.Account,
	wsURL string,
	sessionKey string,
	headers http.Header,
	proxyOverride string,
) (*WsConnection, error) {
	// 创建拨号器副本（避免修改共享 dialer）
	dialer := &websocket.Dialer{
		Proxy:             m.dialer.Proxy,
		HandshakeTimeout:  m.dialer.HandshakeTimeout,
		EnableCompression: m.dialer.EnableCompression,
		NetDialContext:    m.dialer.NetDialContext,
	}

	// 配置代理
	proxyURL := effectiveProxyURL(account, proxyOverride)

	if proxyURL != "" {
		if err := applyProxyToWebsocketDialer(dialer, proxyURL); err != nil {
			return nil, err
		}
	}

	// 创建会话（先关闭旧 session 避免泄漏）
	poolKey := m.poolKey(account.ID(), wsURL, sessionKey, proxyURL)
	if oldSessionVal, ok := m.sessions.Load(poolKey); ok {
		oldSession := oldSessionVal.(*Session)
		oldSession.Close()
	}
	session := NewSession(account.ID(), m)
	if trimmed := strings.TrimSpace(sessionKey); trimmed != "" {
		session.ID = trimmed
	}
	m.sessions.Store(poolKey, session)

	// 拨号连接
	conn, resp, err := dialer.DialContext(ctx, wsURL, headers)
	if err != nil {
		m.sessions.Delete(poolKey)
		session.Close()
		return nil, fmt.Errorf("websocket handshake failed: %w", err)
	}

	// 创建连接包装
	wc := NewWsConnection(conn, session, wsURL)
	wc.PoolKey = poolKey
	wc.httpResp = resp
	wc.onDisconnected = m.getOnDisconnected()
	session.SetConnected(true)

	// 设置 Pong 处理器
	conn.SetPongHandler(func(appData string) error {
		session.HandlePong()
		wc.Touch()
		return nil
	})

	return wc, nil
}

// ReleaseConnection 释放连接（归还池）
func (m *Manager) ReleaseConnection(wc *WsConnection) {
	if wc == nil {
		return
	}
	wc.Touch()
}

// RemoveConnection 移除连接
func (m *Manager) RemoveConnection(accountID int64, wsURL string, sessionKey string, proxyURL string) {
	key := m.poolKey(accountID, wsURL, sessionKey, proxyURL)
	if v, ok := m.connections.LoadAndDelete(key); ok {
		wc := v.(*WsConnection)
		wc.Close()
	}
	m.sessions.Delete(key)
	m.keyLocks.Delete(key)
}

// poolKey 生成连接池键
func (m *Manager) poolKey(accountID int64, wsURL string, sessionKey string, proxyURL string) string {
	return fmt.Sprintf("%d|%s|%s|%s", accountID, wsURL, strings.TrimSpace(sessionKey), strings.TrimSpace(proxyURL))
}

// GetSession 获取会话
func (m *Manager) GetSession(accountID int64, wsURL string, sessionKey string, proxyURL string) (*Session, bool) {
	if v, ok := m.sessions.Load(m.poolKey(accountID, wsURL, sessionKey, proxyURL)); ok {
		return v.(*Session), true
	}
	return nil, false
}

// ConnectionCount 获取连接数量
func (m *Manager) ConnectionCount() int {
	count := 0
	m.connections.Range(func(key, value any) bool {
		count++
		return true
	})
	return count
}

// SessionCount 获取会话数量
func (m *Manager) SessionCount() int {
	count := 0
	m.sessions.Range(func(key, value any) bool {
		count++
		return true
	})
	return count
}

// ReplaceConnection 替换连接（用于重连）
func (m *Manager) ReplaceConnection(
	ctx context.Context,
	account *auth.Account,
	wsURL string,
	sessionKey string,
	headers http.Header,
	proxyOverride string,
) (*WsConnection, *PendingRequest, error) {
	// 先移除旧连接
	m.RemoveConnection(account.ID(), wsURL, sessionKey, effectiveProxyURL(account, proxyOverride))

	// 创建新连接
	return m.AcquireConnection(ctx, account, wsURL, sessionKey, headers, proxyOverride)
}

// SendHeartbeat 发送心跳 Ping
func (m *Manager) SendHeartbeat(wc *WsConnection) error {
	wc.writeMu.Lock()
	defer wc.writeMu.Unlock()

	if !wc.IsConnected() {
		return fmt.Errorf("connection is not connected")
	}

	deadline := time.Now().Add(10 * time.Second)
	err := wc.conn.WriteControl(websocket.PingMessage, []byte{}, deadline)
	if err != nil {
		log.Printf("WebSocket Ping 失败 (account %d): %v", wc.session.AccountID, err)
		wc.Close()
		if wc.PoolKey != "" {
			m.connections.Delete(wc.PoolKey)
			m.sessions.Delete(wc.PoolKey)
		}
		return err
	}
	return nil
}

// StartHeartbeat 启动连接心跳
func (m *Manager) StartHeartbeat(wc *WsConnection) {
	wc.session.StartHeartbeat(func() error {
		return m.SendHeartbeat(wc)
	})
}

// 全局管理器实例
var globalManager *Manager
var managerOnce sync.Once

// GetManager 获取全局管理器实例
func GetManager() *Manager {
	managerOnce.Do(func() {
		globalManager = NewManager()
	})
	return globalManager
}

// ShutdownManager 关闭全局管理器
func ShutdownManager() {
	if globalManager != nil {
		globalManager.Stop()
	}
}
