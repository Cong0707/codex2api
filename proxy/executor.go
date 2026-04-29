package proxy

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptrace"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/codex2api/auth"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ==================== HTTP 连接池（按账号隔离 + TTL 淘汰） ====================
//
// 设计要点：
//   - 按账号隔离：避免同一 TCP 连接被不同 token 复用（会被服务端检测）
//   - TTL 淘汰：只有活跃账号持有连接，不活跃的自动清理，几万账号也不爆内存
//   - 空闲连接极简：每账号只保留 1 条空闲连接，空闲 30s 后自动关闭

// poolEntry 包装 http.Client，追踪最后使用时间用于 TTL 淘汰
type poolEntry struct {
	client   *http.Client
	lastUsed atomic.Int64 // UnixNano 时间戳
}

func (e *poolEntry) touch() {
	e.lastUsed.Store(time.Now().UnixNano())
}

var clientPool sync.Map // map[string]*poolEntry, key = accountID|proxyURL

// clientPoolTTL 未使用超过此时间的 Client 将被淘汰
const clientPoolTTL = 5 * time.Minute

// clientPoolCleanupInterval 清理协程执行间隔
const clientPoolCleanupInterval = 60 * time.Second

func init() {
	// 后台清理：每 60 秒扫描一次，淘汰过期的 Client
	go func() {
		ticker := time.NewTicker(clientPoolCleanupInterval)
		defer ticker.Stop()
		for range ticker.C {
			evictExpiredClients()
		}
	}()
}

func evictExpiredClients() {
	cutoff := time.Now().Add(-clientPoolTTL).UnixNano()
	clientPool.Range(func(key, value any) bool {
		entry := value.(*poolEntry)
		if entry.lastUsed.Load() < cutoff {
			clientPool.Delete(key)
			entry.client.CloseIdleConnections()
		}
		return true
	})
}

func clientPoolKey(account *auth.Account, proxyURL string) string {
	return fmt.Sprintf("%d|%s", account.ID(), proxyURL)
}

func shouldRecyclePooledClient(err error) bool {
	if err == nil {
		return false
	}

	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "connection is shutting down") ||
		strings.Contains(msg, "connection reset") ||
		strings.Contains(msg, "broken pipe")
}

func recyclePooledClient(account *auth.Account, proxyURL string) {
	key := clientPoolKey(account, proxyURL)
	if v, ok := clientPool.LoadAndDelete(key); ok {
		v.(*poolEntry).client.CloseIdleConnections()
	}
}

func recyclePooledClientForAccount(account *auth.Account) {
	if account == nil {
		return
	}

	account.Mu().RLock()
	proxyURL := account.ProxyURL
	account.Mu().RUnlock()
	recyclePooledClient(account, proxyURL)
}

// getPooledClient 获取或创建连接池中的 HTTP Client（按账号隔离，TTL 自动淘汰）
func getPooledClient(account *auth.Account, proxyURL string) *http.Client {
	key := clientPoolKey(account, proxyURL)
	if v, ok := clientPool.Load(key); ok {
		entry := v.(*poolEntry)
		entry.touch()
		return entry.client
	}

	transport := newProxyAwareTransport(proxyURL)

	entry := &poolEntry{
		client: &http.Client{
			Transport: transport,
			Timeout:   0,
		},
	}
	entry.touch()

	if v, loaded := clientPool.LoadOrStore(key, entry); loaded {
		e := v.(*poolEntry)
		e.touch()
		return e.client
	}
	return entry.client
}

func cloneDefaultTransport() *http.Transport {
	if base, ok := http.DefaultTransport.(*http.Transport); ok && base != nil {
		return base.Clone()
	}
	return &http.Transport{}
}

// newProxyAwareTransport 使用与 CLIProxyAPI 一致的 Go 原生 HTTP transport。
// 这样请求侧 TLS/HTTP 行为更接近官方 CLI 的常规客户端栈，减少额外指纹差异。
func newProxyAwareTransport(proxyURL string) *http.Transport {
	transport := cloneDefaultTransport()
	// 默认禁用环境变量代理，避免与账号/代理池配置冲突。
	transport.Proxy = nil
	transport.ForceAttemptHTTP2 = true
	if err := auth.ConfigureTransportProxy(transport, proxyURL, nil); err != nil {
		log.Printf("配置上游代理失败，回退直连: %v", err)
		transport.Proxy = nil
	}
	return transport
}

// Codex 上游常量
const (
	CodexBaseURL = "https://chatgpt.com/backend-api/codex"
	Originator   = StableCodexOriginator
)

var WebsocketExecuteFunc func(
	ctx context.Context,
	account *auth.Account,
	requestBody []byte,
	sessionID string,
	proxyOverride string,
	apiKey string,
	deviceCfg *DeviceProfileConfig,
	headers http.Header,
) (*http.Response, error)

type UpstreamAttemptTrace struct {
	StartedAt           time.Time
	ProxyURL            string
	Transport           string
	WebsocketFallback   bool
	ReusedConn          bool
	WasIdleConn         bool
	DNSStartAt          time.Time
	DNSDoneAt           time.Time
	ConnectStartAt      time.Time
	ConnectDoneAt       time.Time
	TLSStartAt          time.Time
	TLSDoneAt           time.Time
	FirstResponseByteAt time.Time
	HeaderAt            time.Time
	ErrorAt             time.Time
}

func newUpstreamAttemptTrace(proxyURL string) *UpstreamAttemptTrace {
	return &UpstreamAttemptTrace{
		StartedAt: time.Now(),
		ProxyURL:  proxyURL,
		Transport: "http",
	}
}

func setFirstTimestamp(dst *time.Time, t time.Time) {
	if dst == nil || t.IsZero() {
		return
	}
	if dst.IsZero() {
		*dst = t
	}
}

func elapsedTraceMs(start, end time.Time) int {
	if start.IsZero() || end.IsZero() || end.Before(start) {
		return 0
	}
	return int(end.Sub(start).Milliseconds())
}

func (t *UpstreamAttemptTrace) HeaderMs() int {
	if t == nil {
		return 0
	}
	return elapsedTraceMs(t.StartedAt, t.HeaderAt)
}

func (t *UpstreamAttemptTrace) FirstResponseByteMs() int {
	if t == nil {
		return 0
	}
	return elapsedTraceMs(t.StartedAt, t.FirstResponseByteAt)
}

func (t *UpstreamAttemptTrace) ConnectMs() int {
	if t == nil {
		return 0
	}
	return elapsedTraceMs(t.ConnectStartAt, t.ConnectDoneAt)
}

func (t *UpstreamAttemptTrace) DNSMs() int {
	if t == nil {
		return 0
	}
	return elapsedTraceMs(t.DNSStartAt, t.DNSDoneAt)
}

func (t *UpstreamAttemptTrace) TLSMs() int {
	if t == nil {
		return 0
	}
	return elapsedTraceMs(t.TLSStartAt, t.TLSDoneAt)
}

// ExecuteRequest 向 Codex 上游发送请求
// sessionID 可选，用于 prompt cache 会话绑定
// useWebsocket 可选，如果为 true 则使用 WebSocket 连接
// headers 下游请求头，用于设备指纹学习
func ExecuteRequest(ctx context.Context, account *auth.Account, requestBody []byte, sessionID string, proxyOverride string, apiKey string, deviceCfg *DeviceProfileConfig, headers http.Header, useWebsocket ...bool) (*http.Response, error) {
	resp, _, err := ExecuteRequestTraced(ctx, account, requestBody, sessionID, proxyOverride, apiKey, deviceCfg, headers, useWebsocket...)
	return resp, err
}

func ExecuteRequestTraced(ctx context.Context, account *auth.Account, requestBody []byte, sessionID string, proxyOverride string, apiKey string, deviceCfg *DeviceProfileConfig, headers http.Header, useWebsocket ...bool) (*http.Response, *UpstreamAttemptTrace, error) {
	// 检查是否使用 WebSocket
	if ctx == nil {
		ctx = context.Background()
	}

	account.Mu().RLock()
	accessToken := account.AccessToken
	accountID := account.AccountID
	proxyURL := account.ProxyURL
	account.Mu().RUnlock()

	// 代理池优先级: proxyOverride (来自 NextProxy) > account.ProxyURL
	if proxyOverride != "" {
		proxyURL = proxyOverride
	}

	if accessToken == "" {
		return nil, nil, ErrNoAvailableAccount()
	}

	trace := newUpstreamAttemptTrace(proxyURL)
	if len(useWebsocket) > 0 && useWebsocket[0] && WebsocketExecuteFunc != nil {
		trace.Transport = "websocket"
		wsResp, wsErr := WebsocketExecuteFunc(ctx, account, requestBody, sessionID, proxyOverride, apiKey, deviceCfg, headers)
		if wsErr == nil {
			trace.HeaderAt = time.Now()
			return wsResp, trace, nil
		}
		// 对齐 CLIProxyAPI 的稳态策略：WebSocket 失败自动回退 HTTP 链路，避免单点失败。
		log.Printf("WebSocket 上游失败，回退 HTTP: account=%d err=%v", account.ID(), wsErr)
		trace = newUpstreamAttemptTrace(proxyURL)
		trace.WebsocketFallback = true
	}

	// ==================== Codex 请求体优化 ====================
	// 参考 CLIProxyAPI/codex_executor.go + sub2api 的实现

	// 1. 确保 instructions 字段存在（Codex 后端要求）
	if !gjson.GetBytes(requestBody, "instructions").Exists() {
		requestBody, _ = sjson.SetBytes(requestBody, "instructions", "")
	}

	// 2. 清理可能导致上游报错的多余字段
	requestBody, _ = sjson.DeleteBytes(requestBody, "previous_response_id")
	requestBody, _ = sjson.DeleteBytes(requestBody, "prompt_cache_retention")
	requestBody, _ = sjson.DeleteBytes(requestBody, "safety_identifier")
	requestBody, _ = sjson.DeleteBytes(requestBody, "disable_response_storage")

	// 3. 注入 prompt_cache_key（如果请求体中没有，且 sessionID 不为空）
	existingCacheKey := strings.TrimSpace(gjson.GetBytes(requestBody, "prompt_cache_key").String())
	cacheKey := existingCacheKey
	if cacheKey == "" && sessionID != "" {
		cacheKey = sessionID
		requestBody, _ = sjson.SetBytes(requestBody, "prompt_cache_key", cacheKey)
	}

	endpoint := CodexBaseURL + "/responses"

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(requestBody))
	if err != nil {
		trace.ErrorAt = time.Now()
		return nil, trace, ErrInternalError("创建请求失败", err)
	}
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
		DNSStart: func(httptrace.DNSStartInfo) {
			setFirstTimestamp(&trace.DNSStartAt, time.Now())
		},
		DNSDone: func(httptrace.DNSDoneInfo) {
			setFirstTimestamp(&trace.DNSDoneAt, time.Now())
		},
		ConnectStart: func(_, _ string) {
			setFirstTimestamp(&trace.ConnectStartAt, time.Now())
		},
		ConnectDone: func(_, _ string, _ error) {
			setFirstTimestamp(&trace.ConnectDoneAt, time.Now())
		},
		TLSHandshakeStart: func() {
			setFirstTimestamp(&trace.TLSStartAt, time.Now())
		},
		TLSHandshakeDone: func(_ tls.ConnectionState, _ error) {
			setFirstTimestamp(&trace.TLSDoneAt, time.Now())
		},
		GotConn: func(info httptrace.GotConnInfo) {
			trace.ReusedConn = info.Reused
			trace.WasIdleConn = info.WasIdle
		},
		GotFirstResponseByte: func() {
			setFirstTimestamp(&trace.FirstResponseByteAt, time.Now())
		},
	}))

	// ==================== 请求头（对齐 CLIProxyAPI 的稳定策略） ====================
	identity := resolveCodexRequestIdentity(account, apiKey, headers, deviceCfg)
	applyCodexRequestHeaders(req, accessToken, accountID, cacheKey, identity, true, headers)

	// 获取连接池 HTTP 客户端（账号级隔离，复用 TCP/TLS 连接）
	client := getPooledClient(account, proxyURL)

	resp, err := client.Do(req)
	if err != nil {
		trace.ErrorAt = time.Now()
		if shouldRecyclePooledClient(err) {
			recyclePooledClient(account, proxyURL)
		}
		return nil, trace, ErrUpstream(0, "请求上游失败", err)
	}
	trace.HeaderAt = time.Now()

	return resp, trace, nil
}

// ExecuteCompactRequest 向 Codex 上游发送 /responses/compact 请求（非流式压缩接口）
func ExecuteCompactRequest(ctx context.Context, account *auth.Account, requestBody []byte, sessionID string, proxyOverride string, apiKey string, deviceCfg *DeviceProfileConfig, headers http.Header) (*http.Response, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	account.Mu().RLock()
	accessToken := account.AccessToken
	accountID := account.AccountID
	proxyURL := account.ProxyURL
	account.Mu().RUnlock()

	if proxyOverride != "" {
		proxyURL = proxyOverride
	}
	if accessToken == "" {
		return nil, ErrNoAvailableAccount()
	}

	if !gjson.GetBytes(requestBody, "instructions").Exists() {
		requestBody, _ = sjson.SetBytes(requestBody, "instructions", "")
	}
	requestBody, _ = sjson.DeleteBytes(requestBody, "previous_response_id")
	requestBody, _ = sjson.DeleteBytes(requestBody, "prompt_cache_retention")
	requestBody, _ = sjson.DeleteBytes(requestBody, "safety_identifier")
	requestBody, _ = sjson.DeleteBytes(requestBody, "disable_response_storage")

	existingCacheKey := strings.TrimSpace(gjson.GetBytes(requestBody, "prompt_cache_key").String())
	cacheKey := existingCacheKey
	if cacheKey == "" && sessionID != "" {
		cacheKey = sessionID
		requestBody, _ = sjson.SetBytes(requestBody, "prompt_cache_key", cacheKey)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, CodexBaseURL+"/responses/compact", bytes.NewReader(requestBody))
	if err != nil {
		return nil, ErrInternalError("创建请求失败", err)
	}

	identity := resolveCodexRequestIdentity(account, apiKey, headers, deviceCfg)
	applyCodexRequestHeaders(req, accessToken, accountID, cacheKey, identity, false, headers)

	client := getPooledClient(account, proxyURL)
	resp, err := client.Do(req)
	if err != nil {
		if shouldRecyclePooledClient(err) {
			recyclePooledClient(account, proxyURL)
		}
		return nil, ErrUpstream(0, "请求上游失败", err)
	}
	return resp, nil
}

func requestAPIKeyFromHeaders(headers http.Header) string {
	if headers == nil {
		return ""
	}
	authHeader := strings.TrimSpace(headers.Get("Authorization"))
	if strings.HasPrefix(strings.ToLower(authHeader), "bearer ") {
		if key := strings.TrimSpace(authHeader[7:]); key != "" {
			return key
		}
	}
	for _, header := range []string{"x-api-key", "anthropic-auth-token"} {
		if key := strings.TrimSpace(headers.Get(header)); key != "" {
			return key
		}
	}
	return ""
}

// ResolveSessionID 从下游请求提取或生成 session ID
// 优先级：
//  1. Header: Session_id
//  2. Header: Conversation_id
//  3. Header: Idempotency-Key
//  4. Body:   prompt_cache_key
//  5. 基于 API Key 的确定性 UUID
func ResolveSessionID(headers http.Header, body []byte) string {
	if headers != nil {
		if v := strings.TrimSpace(headers.Get("Session_id")); v != "" {
			return v
		}
		if v := strings.TrimSpace(headers.Get("Conversation_id")); v != "" {
			return v
		}
		if v := strings.TrimSpace(headers.Get("Idempotency-Key")); v != "" {
			return v
		}
	}
	// 优先从 body 的 prompt_cache_key 提取
	if v := strings.TrimSpace(gjson.GetBytes(body, "prompt_cache_key").String()); v != "" {
		return v
	}

	// 基于下游用户的 API Key 生成确定性 cache key（参考 CLIProxyAPI codex_executor.go:621）
	apiKey := requestAPIKeyFromHeaders(headers)
	if apiKey != "" {
		return uuid.NewSHA1(uuid.NameSpaceOID, []byte("codex2api:prompt-cache:"+apiKey)).String()
	}

	// 最后兜底：生成随机 UUID
	return uuid.New().String()
}

// ReadSSEStream 从上游 SSE 响应读取事件流
// callback 返回 true 表示继续读取，false 表示停止
func ReadSSEStream(body io.Reader, callback func(data []byte) bool) error {
	// 使用 sync.Pool 复用缓冲区，减少 GC 压力
	buf := sseBufferPool.Get().([]byte)
	defer sseBufferPool.Put(buf)

	var lineBuf []byte
	var dataLines [][]byte

	emitEvent := func() bool {
		if len(dataLines) == 0 {
			return true
		}

		data := bytes.Join(dataLines, []byte("\n"))
		dataLines = dataLines[:0]
		if bytes.Equal(data, []byte("[DONE]")) {
			return false
		}
		return callback(data)
	}

	for {
		n, err := body.Read(buf)
		if n > 0 {
			lineBuf = append(lineBuf, buf[:n]...)

			// 按行处理
			for {
				idx := bytes.IndexByte(lineBuf, '\n')
				if idx < 0 {
					break
				}

				line := bytes.TrimRight(lineBuf[:idx], "\r")
				lineBuf = lineBuf[idx+1:]

				if len(line) == 0 {
					if !emitEvent() {
						return nil
					}
					continue
				}

				if bytes.HasPrefix(line, []byte(":")) {
					continue
				}

				// 解析 SSE data: 前缀，支持标准多行 data 聚合
				if bytes.HasPrefix(line, []byte("data:")) {
					data := bytes.TrimPrefix(line, []byte("data:"))
					data = bytes.TrimPrefix(data, []byte(" "))
					// 使用 copy 避免底层数组共享导致的内存泄漏
					dataCopy := make([]byte, len(data))
					copy(dataCopy, data)
					dataLines = append(dataLines, dataCopy)
				}
			}
		}

		if err != nil {
			if err == io.EOF {
				if len(lineBuf) > 0 {
					line := bytes.TrimRight(lineBuf, "\r")
					if bytes.HasPrefix(line, []byte("data:")) {
						data := bytes.TrimPrefix(line, []byte("data:"))
						data = bytes.TrimPrefix(data, []byte(" "))
						dataCopy := make([]byte, len(data))
						copy(dataCopy, data)
						dataLines = append(dataLines, dataCopy)
					}
				}
				if !emitEvent() {
					return nil
				}
				return nil
			}
			return err
		}
	}
}

// sseBufferPool 用于复用 SSE 读取缓冲区
var sseBufferPool = sync.Pool{
	New: func() interface{} {
		return make([]byte, 8192) // 增加到 8KB 提高读取效率
	},
}
