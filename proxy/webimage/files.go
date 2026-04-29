// files.go —— chatgpt.com 文件上传协议,图生图/图像编辑的前置步骤。
//
// 三步协议(对齐 chatgpt.com 浏览器真实抓包):
//
//  1. POST /backend-api/files
//     body: {file_name, file_size, use_case: "multimodal"}
//     resp: {file_id, upload_url, status: "success"}
//
//  2. PUT <upload_url>                 (Azure Blob SAS URL)
//     headers: Content-Type / x-ms-blob-type: BlockBlob / x-ms-version: 2020-04-08 / Origin
//     body: 原始字节
//
//  3. POST /backend-api/files/{file_id}/uploaded
//     body: {}
//     resp: {status: "success", download_url, ...}
//
// 上传完成后,在 f/conversation.messages 里:
//   - content 从 text 变 multimodal_text;parts 前面加上
//     {"asset_pointer": "file-service://<file_id>", "height":.., "width":.., "size_bytes":..}
//   - metadata.attachments 加一项 {id, mimeType, name, size, height?, width?}
//
// 注意:upload_url 指向 Azure,不要走同一个 chatgpt.com 代理/utls transport,
// 这里用单独的一个 http.Client(沿用 Client 内部的 Transport 走代理,但不带 Auth/Oai-* 头)。

package webimage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/gif"  // register decoders
	_ "image/jpeg" //
	_ "image/png"  //
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// UploadedFile 是三步上传后沉淀的"可 attach 给 messages"的元数据。
// 字段命名对齐 chatgpt.com 的 attachment payload,序列化时直接当 map 用。
type UploadedFile struct {
	FileID      string `json:"file_id"`
	FileName    string `json:"file_name"`
	FileSize    int    `json:"file_size"`
	MimeType    string `json:"mime_type"`
	UseCase     string `json:"use_case"`         // 图片: multimodal, 文件: my_files
	Width       int    `json:"width,omitempty"`  // 仅图片
	Height      int    `json:"height,omitempty"` // 仅图片
	DownloadURL string `json:"download_url"`     // POST /uploaded 返回,通常不直接用
}

// UploadFile 执行完整三步上传。调用方传入原始字节 + 建议的文件名即可。
// 识别到 image/* 时会尝试 Decode 拿到宽高(Decode 失败不致命,按 0 处理)。
//
// 实践经验:步骤 1、3 走 chatgpt.com(uTLS / 代理 / auth 头),步骤 2 走 Azure,
// 用同一条 http.Client 但是请求头手动裁剪;Azure 的 SAS URL 本身带鉴权。
//
// 重试策略(transient retry):
//   - step1 / step3:走 chatgpt.com,偶发 cloudflare 5xx / utls EOF,重试 3 次;
//   - step2:走 Azure Blob,SAS URL PUT 是幂等覆盖语义,瞬时握手中断重试 3 次最稳;
//   - 退避 0.5s / 1.5s / 3s;命中 4xx(401/403/413 等业务错误)立刻终止不浪费 token;
//   - context 取消立即返回,不再继续等待退避。
func (c *Client) UploadFile(ctx context.Context, data []byte, fileName string) (*UploadedFile, error) {
	if len(data) == 0 {
		return nil, errors.New("empty file data")
	}
	mime, ext := sniffMime(data)
	useCase := "multimodal"
	if !strings.HasPrefix(mime, "image/") {
		useCase = "my_files"
	}
	if fileName == "" {
		fileName = fmt.Sprintf("file-%d%s", len(data), ext)
	}

	out := &UploadedFile{
		FileName: fileName,
		FileSize: len(data),
		MimeType: mime,
		UseCase:  useCase,
	}
	if strings.HasPrefix(mime, "image/") {
		if img, _, err := image.DecodeConfig(bytes.NewReader(data)); err == nil {
			out.Width = img.Width
			out.Height = img.Height
		}
	}

	// ---- Step 1: POST /backend-api/files ----
	step1Body := map[string]interface{}{
		"file_name": fileName,
		"file_size": len(data),
		"use_case":  useCase,
	}
	if out.Width > 0 && out.Height > 0 {
		step1Body["height"] = out.Height
		step1Body["width"] = out.Width
	}
	b1, _ := json.Marshal(step1Body)
	buf1, status1, err := c.doUploadStep(ctx, "create file", func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			c.opts.BaseURL+"/backend-api/files", bytes.NewReader(b1))
		if err != nil {
			return nil, err
		}
		c.commonHeaders(req)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		return req, nil
	})
	if err != nil {
		return nil, err
	}
	if status1 >= 400 {
		return nil, &UpstreamError{Status: status1, Message: "create file failed", Body: string(buf1)}
	}
	var step1Resp struct {
		FileID    string `json:"file_id"`
		UploadURL string `json:"upload_url"`
		Status    string `json:"status"`
	}
	if err := json.Unmarshal(buf1, &step1Resp); err != nil {
		return nil, fmt.Errorf("decode create-file resp: %w (body=%s)", err, truncateStr(string(buf1), 200))
	}
	if step1Resp.FileID == "" || step1Resp.UploadURL == "" {
		return nil, fmt.Errorf("create-file empty: %s", truncateStr(string(buf1), 200))
	}
	out.FileID = step1Resp.FileID

	// chatgpt 浏览器行为:step1 和 step2 之间会 sleep 一小会儿,避免 Azure 那边
	// 还没完成 SAS 分发。参考实现是 1s,这里保守点给 500ms。
	select {
	case <-time.After(500 * time.Millisecond):
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	// ---- Step 2: PUT upload_url (Azure Blob) ----
	// SAS URL 的 PUT 是覆盖语义,重试任意次都安全。
	_, status2, err := c.doUploadStep(ctx, "upload PUT", func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPut,
			step1Resp.UploadURL, bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", mime)
		req.Header.Set("x-ms-blob-type", "BlockBlob")
		req.Header.Set("x-ms-version", "2020-04-08")
		req.Header.Set("Origin", c.opts.BaseURL)
		req.Header.Set("User-Agent", c.opts.UserAgent)
		req.Header.Set("Accept", "application/json, text/plain, */*")
		req.Header.Set("Accept-Language", "en-US,en;q=0.8")
		req.Header.Set("Referer", c.opts.BaseURL+"/")
		return req, nil
	})
	if err != nil {
		return nil, err
	}
	if status2 >= 400 {
		// 这里 body 已经被 helper 读过,但 4xx 不重试时返回的 buf 含错误体。
		// 重新发起一次拿 body 太麻烦,用 status 文案就够定位了。
		return nil, &UpstreamError{Status: status2, Message: "upload PUT failed"}
	}

	// ---- Step 3: POST /backend-api/files/{file_id}/uploaded ----
	buf3, status3, err := c.doUploadStep(ctx, "register uploaded", func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			c.opts.BaseURL+"/backend-api/files/"+step1Resp.FileID+"/uploaded",
			strings.NewReader("{}"))
		if err != nil {
			return nil, err
		}
		c.commonHeaders(req)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		return req, nil
	})
	if err != nil {
		return nil, err
	}
	if status3 >= 400 {
		return nil, &UpstreamError{Status: status3, Message: "register uploaded failed", Body: string(buf3)}
	}
	var step3Resp struct {
		Status      string `json:"status"`
		DownloadURL string `json:"download_url"`
	}
	_ = json.Unmarshal(buf3, &step3Resp)
	out.DownloadURL = step3Resp.DownloadURL

	return out, nil
}

// doUploadStep 执行一次带重试的 HTTP 调用。
//
// 设计:
//   - 把请求构造放到回调里,每次重试都 build 一个新 *http.Request(避免 body 被消费过)。
//   - 仅对"瞬时网络错误"和 5xx 重试,4xx 业务错误立刻退出。
//   - 退避序列 0 / 0.5s / 1.5s / 3s,共 4 次尝试;ctx 取消则不再等待。
//
// 返回 (response_body, status_code, err)。当且仅当请求建立失败时才返回 err。
// 业务错误(4xx)返回 status>=400 + 空 err,由调用方决定如何包成 UpstreamError。
func (c *Client) doUploadStep(
	ctx context.Context,
	label string,
	build func() (*http.Request, error),
) ([]byte, int, error) {
	const maxAttempts = 4
	backoffs := []time.Duration{0, 500 * time.Millisecond, 1500 * time.Millisecond, 3 * time.Second}

	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(backoffs[attempt]):
			case <-ctx.Done():
				return nil, 0, ctx.Err()
			}
		}

		req, err := build()
		if err != nil {
			return nil, 0, err
		}

		resp, doErr := c.hc.Do(req)
		if doErr != nil {
			lastErr = fmt.Errorf("%s: %w", label, doErr)
			if !isTransientNetErr(doErr) || ctx.Err() != nil {
				return nil, 0, lastErr
			}
			continue
		}

		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()

		if resp.StatusCode >= 500 && attempt < maxAttempts-1 {
			lastErr = fmt.Errorf("%s http=%d", label, resp.StatusCode)
			continue
		}
		return body, resp.StatusCode, nil
	}
	return nil, 0, lastErr
}

// isTransientNetErr 判断错误是不是值得重试的"瞬时网络故障"。
//
// 命中场景(基于线上常见症状):
//   - utls handshake xxx: EOF        ← 当前 Azure 偶发关连接最常见的形态
//   - read/write tcp ...: connection reset by peer
//   - tls: handshake failure
//   - i/o timeout / deadline exceeded(底层超时,非 ctx 取消)
//   - net.Error 实现且 Timeout()==true
//
// 不命中:context.Canceled / context.DeadlineExceeded — 这是上层主动取消,
// 强行重试只会刷请求量。
func isTransientNetErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	s := err.Error()
	for _, kw := range []string{
		"EOF",
		"connection reset",
		"connection refused",
		"broken pipe",
		"no route to host",
		"network is unreachable",
		"TLS handshake",
		"tls: handshake",
		"utls handshake",
		"i/o timeout",
		"unexpected EOF",
		"server closed connection",
		"use of closed network connection",
	} {
		if strings.Contains(s, kw) {
			return true
		}
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	return false
}

// Attachment 是 messages[*].metadata.attachments[*] 的序列化对象。
type Attachment struct {
	ID       string `json:"id"`
	MimeType string `json:"mimeType"`
	Name     string `json:"name"`
	Size     int    `json:"size"`
	Width    int    `json:"width,omitempty"`
	Height   int    `json:"height,omitempty"`
}

// ToAttachment 把一个已上传的 file 转成 messages.metadata.attachments 里的条目。
func (u *UploadedFile) ToAttachment() Attachment {
	a := Attachment{ID: u.FileID, MimeType: u.MimeType, Name: u.FileName, Size: u.FileSize}
	if u.UseCase == "multimodal" {
		a.Width = u.Width
		a.Height = u.Height
	}
	return a
}

// AssetPointerPart 是 messages[*].content.parts 里的一项(图片),
// 用于把 file-service:// 挂到多模态消息最前面。
type AssetPointerPart struct {
	ContentType  string `json:"content_type,omitempty"` // "image_asset_pointer"
	AssetPointer string `json:"asset_pointer"`
	Width        int    `json:"width,omitempty"`
	Height       int    `json:"height,omitempty"`
	SizeBytes    int    `json:"size_bytes,omitempty"`
}

// ToAssetPointerPart 返回 multimodal_text.parts 里 insert 在 prompt 前的那一项。
func (u *UploadedFile) ToAssetPointerPart() AssetPointerPart {
	return AssetPointerPart{
		ContentType:  "image_asset_pointer",
		AssetPointer: "file-service://" + u.FileID,
		Width:        u.Width,
		Height:       u.Height,
		SizeBytes:    u.FileSize,
	}
}

// sniffMime 用前 512 字节识别 mime 和推荐扩展名。
// net/http 的 DetectContentType 已足够覆盖 png/jpg/gif/webp 的主流场景。
func sniffMime(data []byte) (mime, ext string) {
	n := 512
	if len(data) < n {
		n = len(data)
	}
	mime = http.DetectContentType(data[:n])
	// DetectContentType 可能附带 charset,去掉
	if i := strings.Index(mime, ";"); i >= 0 {
		mime = strings.TrimSpace(mime[:i])
	}
	switch {
	case strings.EqualFold(mime, "image/jpeg"):
		ext = ".jpg"
	case strings.EqualFold(mime, "image/png"):
		ext = ".png"
	case strings.EqualFold(mime, "image/gif"):
		ext = ".gif"
	case strings.EqualFold(mime, "image/webp"):
		ext = ".webp"
	case strings.EqualFold(mime, "application/pdf"):
		ext = ".pdf"
	default:
		ext = ""
	}
	return
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
