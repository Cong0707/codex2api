package webimage

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	utls "github.com/refraction-networking/utls"
	"golang.org/x/net/http2"
	xproxy "golang.org/x/net/proxy"
)

// NewUTLSTransport 返回一个带 uTLS 浏览器指纹伪装的 http.RoundTripper。
//
// chatgpt.com 前置的 Cloudflare 会按 TLS ClientHello 的 JA3/JA4 指纹识别客户端;
// Go 标准 crypto/tls 的指纹会被直接拒绝(返回 403 HTML 拦截页)。这里用
// refraction-networking/utls 把 ClientHello 换成 Chrome 120 模板,让 Cloudflare
// 认为是真 Chrome。
//
// 同时支持通过 HTTP(S) 代理走 CONNECT 隧道。
//
// 行为要点:
//   - ALPN 协商到 h2 时走内部 http2.Transport,http/1.1 时走标准 http.Transport
//   - 首次 h2 失败且为协议级错误(例如 ALPN 回退到 h1)时自动切 h1 重试
//   - 连接不做跨请求复用的特殊处理,依赖各子 transport 自身的空闲池
//
// proxyURL 只支持 http/https,socks5 需要走另一条路径(当前账号池均为 HTTP 代理)。
func NewUTLSTransport(proxyURL string, idleTimeout time.Duration) (http.RoundTripper, error) {
	if idleTimeout <= 0 {
		idleTimeout = 30 * time.Second
	}
	rt := &utlsRoundTripper{
		dialer:      &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second},
		idleTimeout: idleTimeout,
	}
	if strings.TrimSpace(proxyURL) != "" {
		u, err := url.Parse(proxyURL)
		if err != nil {
			return nil, fmt.Errorf("invalid proxy url: %w", err)
		}
		switch strings.ToLower(u.Scheme) {
		case "http", "https":
			rt.proxyURL = u
		case "socks5", "socks5h":
			var auth *xproxy.Auth
			if u.User != nil {
				password, _ := u.User.Password()
				auth = &xproxy.Auth{User: u.User.Username(), Password: password}
			}
			d, err := xproxy.SOCKS5("tcp", u.Host, auth, rt.dialer)
			if err != nil {
				return nil, fmt.Errorf("build socks5 dialer: %w", err)
			}
			rt.socksDialer = d
		default:
			return nil, fmt.Errorf("unsupported proxy scheme %q", u.Scheme)
		}
	}
	rt.h1 = &http.Transport{
		DialTLSContext:        rt.dialTLS,
		MaxIdleConnsPerHost:   4,
		IdleConnTimeout:       idleTimeout,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ForceAttemptHTTP2:     false,
	}
	rt.h2 = &http2.Transport{
		DialTLSContext: func(ctx context.Context, network, addr string, cfg *tls.Config) (net.Conn, error) {
			return rt.dialTLS(ctx, network, addr)
		},
		ReadIdleTimeout: idleTimeout,
		AllowHTTP:       false,
	}
	return rt, nil
}

// forceH1 如果为 true,ALPN 只请求 http/1.1 且 RoundTrip 完全跳过 h2。
//
// 为什么要这个开关:
// chatgpt.com + Cloudflare 近期加强了 JA4H(HTTP/2 SETTINGS frame 指纹)识别。
// Go 的 golang.org/x/net/http2.Transport 发出的 SETTINGS frame 顺序/参数跟
// Chrome 不同,即使 TLS ClientHello 用 uTLS 伪装成 Chrome,h2 层依然会被识别
// 为自动化客户端,触发"Unusual activity has been detected"硬风控。
//
// 参考实现 gen_image.py 用 curl-cffi(impersonate chrome131),那个库同时伪装
// TLS + HTTP/2 指纹。本 transport 暂未接 tls-client,
// 作为权宜之计:强制 http/1.1,上游对 h1 的指纹检测更宽松。
var forceH1 = true

type utlsRoundTripper struct {
	proxyURL    *url.URL
	socksDialer xproxy.Dialer
	dialer      *net.Dialer
	idleTimeout time.Duration

	mu sync.Mutex
	h1 *http.Transport
	h2 *http2.Transport
}

// RoundTrip 先尝试 h2(chatgpt.com 默认),仅在明确是协议级错误时回退到 h1。
//
// 当 forceH1 = true 时,完全跳过 h2(避免 JA4H 识别),直接走 h1 transport。
func (rt *utlsRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if forceH1 {
		return rt.h1.RoundTrip(req)
	}
	// SSE / chunked body 场景:不能让 h2 transport 吃掉 Connection: close 之类的 h1 头
	// 这里 chatgpt.com 都能用 h2 处理 SSE,优先 h2.
	resp, err := rt.h2.RoundTrip(req)
	if err == nil {
		return resp, nil
	}
	if isH2Retryable(err) {
		return rt.h1.RoundTrip(req)
	}
	return nil, err
}

// CloseIdleConnections 关闭两个子 transport 的空闲连接(http.Client 会调用)。
func (rt *utlsRoundTripper) CloseIdleConnections() {
	rt.mu.Lock()
	h1, h2 := rt.h1, rt.h2
	rt.mu.Unlock()
	if h1 != nil {
		h1.CloseIdleConnections()
	}
	if h2 != nil {
		h2.CloseIdleConnections()
	}
}

// dialTLS 建立到 addr 的 TCP(可能走代理 CONNECT),再用 utls 做 Chrome 指纹的 TLS 握手。
// 返回的 net.Conn 实际是 *utls.UConn,ALPN 结果由握手自动确定。
func (rt *utlsRoundTripper) dialTLS(ctx context.Context, network, addr string) (net.Conn, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	raw, err := rt.dialRaw(ctx, addr)
	if err != nil {
		return nil, err
	}
	// forceH1:ALPN 只请求 http/1.1,避免 HTTP/2 SETTINGS 指纹被识别。
	// 否则按常规先 h2 再 h1 fallback。
	alpn := []string{"h2", "http/1.1"}
	if forceH1 {
		alpn = []string{"http/1.1"}
	}
	// InsecureSkipVerify 说明:
	//   1. 部分商业 HTTP 代理会做 SSL Inspection(MITM):对目标域名重签一张自签名证书
	//      转发给客户端。这张证书不在系统 CA 列表里,导致 uTLS 证书验证失败。
	//   2. 我们的安全边界在代理本身:流量已经走 CONNECT 隧道通过已信任的代理;
	//      对 chatgpt.com 再做证书 pin 没有额外意义。
	//   3. 即使不配置代理,直连场景下 chatgpt.com 用的是正规 DigiCert 证书,
	//      实际不会触发此标志,行为与之前相同。
	uconn := utls.UClient(raw, &utls.Config{
		ServerName:         host,
		NextProtos:         alpn,
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: true, //nolint:gosec
	}, utls.HelloChrome_131)

	// 关键:utls 的预设 HelloID(HelloChrome_131 等)里 ALPNExtension 的值
	// 是按 Chrome 原样硬编码的 ["h2", "http/1.1"],Config.NextProtos 对预设
	// HelloID 不生效。forceH1 场景下必须显式覆盖 Extension 里的值,
	// 否则服务器仍按 h2 协商,后续 h1.Transport 会读到 HTTP/2 SETTINGS frame
	// 报 `malformed HTTP response "\x00\x00\x12\x04..."`。
	if forceH1 {
		if err := uconn.BuildHandshakeState(); err != nil {
			_ = raw.Close()
			return nil, fmt.Errorf("utls build state: %w", err)
		}
		for _, ext := range uconn.Extensions {
			if alpnExt, ok := ext.(*utls.ALPNExtension); ok {
				alpnExt.AlpnProtocols = []string{"http/1.1"}
			}
		}
	}

	if err := uconn.HandshakeContext(ctx); err != nil {
		_ = raw.Close()
		return nil, fmt.Errorf("utls handshake %s: %w", host, err)
	}

	// 二次保险:如果服务器忽略我们的 ALPN(比如某些商业代理)还是协商到 h2,
	// 直接断开,避免 h1.Transport 读到 SETTINGS frame 的脏字节。
	if forceH1 {
		np := uconn.ConnectionState().NegotiatedProtocol
		if np != "" && np != "http/1.1" {
			_ = uconn.Close()
			return nil, fmt.Errorf("alpn negotiated %q, expected http/1.1", np)
		}
	}
	return uconn, nil
}

// dialRaw 返回一个已经"到对端 host:port"的 TCP 通道。若配置了 HTTP 代理,先
// 连到代理再发 CONNECT。
func (rt *utlsRoundTripper) dialRaw(ctx context.Context, addr string) (net.Conn, error) {
	if rt.socksDialer != nil {
		return rt.socksDialer.Dial("tcp", addr)
	}
	if rt.proxyURL == nil {
		return rt.dialer.DialContext(ctx, "tcp", addr)
	}
	proxyAddr := rt.proxyURL.Host
	if !strings.Contains(proxyAddr, ":") {
		if strings.EqualFold(rt.proxyURL.Scheme, "https") {
			proxyAddr += ":443"
		} else {
			proxyAddr += ":80"
		}
	}
	conn, err := rt.dialer.DialContext(ctx, "tcp", proxyAddr)
	if err != nil {
		return nil, fmt.Errorf("dial proxy %s: %w", proxyAddr, err)
	}
	// HTTPS 代理本身要先 TLS 握手(走标准 tls,不需伪装指纹,代理一般不卡 JA3)。
	// 部分商业代理使用私有 CA 或自签名证书,标准 CA 库无法验证;
	// InsecureSkipVerify 只作用于「到代理服务器」这一段,不影响后续 uTLS 握手
	// 对 chatgpt.com 的证书验证(两段是独立的 TLS 连接)。
	if strings.EqualFold(rt.proxyURL.Scheme, "https") {
		tlsConn := tls.Client(conn, &tls.Config{
			ServerName:         rt.proxyURL.Hostname(),
			InsecureSkipVerify: true, //nolint:gosec
		})
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("tls handshake to https proxy: %w", err)
		}
		conn = tlsConn
	}
	// CONNECT addr
	connectReq := &http.Request{
		Method: http.MethodConnect,
		URL:    &url.URL{Opaque: addr},
		Host:   addr,
		Header: make(http.Header),
	}
	connectReq.Header.Set("User-Agent", DefaultUserAgent)
	if u := rt.proxyURL.User; u != nil {
		pw, _ := u.Password()
		connectReq.Header.Set("Proxy-Authorization", "Basic "+
			base64.StdEncoding.EncodeToString([]byte(u.Username()+":"+pw)))
	}
	if err := connectReq.Write(conn); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("write CONNECT: %w", err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, connectReq)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("read CONNECT response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = conn.Close()
		return nil, fmt.Errorf("proxy CONNECT %s → %s", addr, resp.Status)
	}
	// br 里可能已经预读了握手后的第一批字节,必须把它包进 conn 返回,否则 TLS 握手会少字节
	if n := br.Buffered(); n > 0 {
		peeked, _ := br.Peek(n)
		return &bufConn{Conn: conn, rd: bufio.NewReaderSize(io.MultiReader(peeked2Reader(peeked), conn), 4096)}, nil
	}
	return conn, nil
}

// isH2Retryable 判断 h2 错误是否可以降级到 h1 重试。
func isH2Retryable(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	// 常见 h2 → h1 协商失败或 HTTP_1_1_REQUIRED
	return strings.Contains(s, "HTTP_1_1_REQUIRED") ||
		strings.Contains(s, "http2: unsupported scheme") ||
		strings.Contains(s, "bad protocol") ||
		strings.Contains(s, "remote error: tls: no application protocol") ||
		strings.Contains(s, "http2: server sent GOAWAY") ||
		errors.Is(err, http2.ErrNoCachedConn)
}

// bufConn 让预读过的字节流继续可读,同时保留 net.Conn 的其他方法(Close/LocalAddr 等)。
type bufConn struct {
	net.Conn
	rd *bufio.Reader
}

func (b *bufConn) Read(p []byte) (int, error) { return b.rd.Read(p) }

// peeked2Reader 把一段已 Peek 的字节封装成 io.Reader。独立函数是为了避免
// 直接把 bufio.Reader 塞进 io.MultiReader 时,它内部还能继续读原 conn 的字节
// 而造成重复读。
func peeked2Reader(peeked []byte) io.Reader {
	return &readOnceBuf{buf: peeked}
}

type readOnceBuf struct {
	buf []byte
	off int
}

func (r *readOnceBuf) Read(p []byte) (int, error) {
	if r.off >= len(r.buf) {
		return 0, io.EOF
	}
	n := copy(p, r.buf[r.off:])
	r.off += n
	return n, nil
}
