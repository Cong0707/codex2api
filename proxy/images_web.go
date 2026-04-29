package proxy

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strings"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy/webimage"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

type webImageForwardSpec struct {
	InboundEndpoint string
	RequestModel    string
	Prompt          string
	References      []webimage.ReferenceImage
	ResponseFormat  string
	StreamPrefix    string
	Stream          bool
	N               int
	ResponsesBody   []byte
	ReasoningEffort string
	ServiceTier     string
}

type webImageFallbackState struct {
	LastStatusCode int
	LastBody       []byte
	LastErr        error
}

type webImageAttemptPayload struct {
	ImagePayload []byte
	Image        imageCallResult
	CreatedAt    int64
	ImageCount   int
}

const (
	defaultWebImageAttemptTimeout = 180 * time.Second
	defaultWebImagePollMaxWait    = 90 * time.Second
)

func (h *Handler) writeWebFallbackState(c *gin.Context, state *webImageFallbackState) bool {
	if state == nil {
		return false
	}
	if state.LastStatusCode > 0 && len(state.LastBody) > 0 {
		h.sendFinalUpstreamError(c, state.LastStatusCode, state.LastBody)
		return true
	}
	if state.LastErr != nil {
		c.JSON(http.StatusBadGateway, gin.H{
			"error": gin.H{"message": "上游请求失败: " + state.LastErr.Error(), "type": "upstream_error"},
		})
		return true
	}
	return false
}

func officialImageAccountMatcher(acc *auth.Account) bool {
	if acc == nil || !acc.IsAvailable() {
		return false
	}
	if !auth.SupportsOfficialImageGeneration(acc.GetPlanType()) {
		return false
	}
	if available, known := acc.GetOfficialImageAvailability(); known && available <= 0 {
		return false
	}
	return true
}

func webImageAccountMatcher(acc *auth.Account) bool {
	if acc == nil {
		return false
	}
	remaining, total, resetAt, _, valid, _ := acc.GetImageQuotaSnapshot()
	if !valid {
		return true
	}
	if total <= 0 {
		return false
	}
	if remaining > 0 {
		return true
	}
	return !resetAt.IsZero() && time.Now().After(resetAt)
}

func (h *Handler) hasAvailableAccountForMatcher(c *gin.Context, extraMatcher auth.AccountMatcher) bool {
	matcher := mergeAccountMatchers(h.accountMatcherForCurrentPort(c), extraMatcher)
	for _, acc := range h.store.Accounts() {
		if acc == nil || !acc.IsAvailable() {
			continue
		}
		if matcher != nil && !matcher(acc) {
			continue
		}
		return true
	}
	return false
}

func extractWebImageUpstreamError(err error) (int, []byte) {
	var upstreamErr *webimage.UpstreamError
	if errors.As(err, &upstreamErr) {
		return upstreamErr.Status, []byte(strings.TrimSpace(upstreamErr.Body))
	}
	return 0, nil
}

func shouldRetryWebImageFailure(statusCode int, err error) bool {
	switch {
	case statusCode == http.StatusUnauthorized || statusCode == http.StatusForbidden:
		return true
	case statusCode == http.StatusTooManyRequests:
		return true
	case statusCode >= 500:
		return true
	case statusCode >= 400:
		return false
	}
	return classifyTransportFailure(err) != ""
}

func buildWebImageAttemptPayload(result *webimage.GenerateResult, prompt, requestModel, responseFormat string) (*webImageAttemptPayload, error) {
	if result == nil || len(result.Images) == 0 {
		return nil, fmt.Errorf("web image result is empty")
	}
	img := result.Images[0]
	if len(img.Data) == 0 {
		return nil, fmt.Errorf("web image bytes are empty")
	}
	encoded := base64.StdEncoding.EncodeToString(img.Data)
	outputFormat := inferOutputFormatFromContentType(img.ContentType)
	createdAt := time.Now().Unix()
	image := imageCallResult{
		Result:        encoded,
		RevisedPrompt: strings.TrimSpace(prompt),
		OutputFormat:  outputFormat,
		Model:         strings.TrimSpace(requestModel),
	}
	payload, err := buildImagesAPIResponse([]imageCallResult{image}, createdAt, nil, image, responseFormat)
	if err != nil {
		return nil, err
	}
	return &webImageAttemptPayload{
		ImagePayload: payload,
		Image:        image,
		CreatedAt:    createdAt,
		ImageCount:   1,
	}, nil
}

func inferOutputFormatFromContentType(contentType string) string {
	contentType = strings.TrimSpace(contentType)
	if contentType == "" {
		return "png"
	}
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err == nil {
		contentType = mediaType
	}
	switch strings.ToLower(contentType) {
	case "image/jpeg":
		return "jpeg"
	case "image/webp":
		return "webp"
	case "image/png":
		fallthrough
	default:
		return "png"
	}
}

func (h *Handler) executeWebImageAttempt(c *gin.Context, account *auth.Account, proxyURL string, spec webImageForwardSpec) (*webImageAttemptPayload, error) {
	client, err := NewWebImageClient(account, proxyURL)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), defaultWebImageAttemptTimeout)
	defer cancel()
	result, err := client.Generate(ctx, webimage.GenerateRequest{
		Prompt:          spec.Prompt,
		ReferenceImages: spec.References,
		UpstreamModel:   "auto",
		PollMaxWait:     defaultWebImagePollMaxWait,
	})
	if err != nil {
		return nil, err
	}
	return buildWebImageAttemptPayload(result, spec.Prompt, spec.RequestModel, spec.ResponseFormat)
}

func writeWebImagesStream(c *gin.Context, streamPrefix, responseFormat string, payload *webImageAttemptPayload) (int, error) {
	if payload == nil {
		return 0, fmt.Errorf("web image payload is nil")
	}
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")

	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		return 0, fmt.Errorf("streaming not supported")
	}
	firstTokenMs := int(time.Since(requestStartTime(c)).Milliseconds())
	eventName := streamPrefix + ".completed"
	if _, err := fmt.Fprintf(c.Writer, "event: %s\n", eventName); err != nil {
		return 0, err
	}
	if _, err := fmt.Fprintf(c.Writer, "data: %s\n\n", buildImagesStreamCompletedPayload(eventName, payload.Image, responseFormat, payload.CreatedAt, nil)); err != nil {
		return 0, err
	}
	flusher.Flush()
	return firstTokenMs, nil
}

func writeWebImagesBatchStream(c *gin.Context, streamPrefix, responseFormat string, payloads []*webImageAttemptPayload) (int, error) {
	if len(payloads) == 0 {
		return 0, fmt.Errorf("web image payload is empty")
	}
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")

	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		return 0, fmt.Errorf("streaming not supported")
	}
	firstTokenMs := int(time.Since(requestStartTime(c)).Milliseconds())
	eventName := streamPrefix + ".completed"
	for _, payload := range payloads {
		if payload == nil {
			continue
		}
		if _, err := fmt.Fprintf(c.Writer, "event: %s\n", eventName); err != nil {
			return firstTokenMs, err
		}
		if _, err := fmt.Fprintf(c.Writer, "data: %s\n\n", buildImagesStreamCompletedPayload(eventName, payload.Image, responseFormat, payload.CreatedAt, nil)); err != nil {
			return firstTokenMs, err
		}
		flusher.Flush()
	}
	return firstTokenMs, nil
}

func buildWebImagesBatchPayload(payloads []*webImageAttemptPayload, responseFormat string) ([]byte, int, error) {
	results := make([]imageCallResult, 0, len(payloads))
	createdAt := time.Now().Unix()
	firstMeta := imageCallResult{}
	for _, payload := range payloads {
		if payload == nil {
			continue
		}
		if createdAt <= 0 || payload.CreatedAt < createdAt {
			createdAt = payload.CreatedAt
		}
		if len(results) == 0 {
			firstMeta = payload.Image
		}
		results = append(results, payload.Image)
	}
	if len(results) == 0 {
		return nil, 0, fmt.Errorf("web image batch result is empty")
	}
	out, err := buildImagesAPIResponse(results, createdAt, nil, firstMeta, responseFormat)
	if err != nil {
		return nil, 0, err
	}
	return out, len(results), nil
}

func (h *Handler) runWebImageOne(c *gin.Context, spec webImageForwardSpec, requestStartedAt time.Time, affinityKey string, excludeAccounts map[int64]bool, attemptOffset int) (*webImageAttemptPayload, *webImageFallbackState, bool) {
	maxRetries := h.getMaxRetries()
	state := &webImageFallbackState{}

	for attempt := 0; attempt <= maxRetries; attempt++ {
		attemptAcquireStartedAt := time.Now()
		account, stickyProxyURL := h.acquireAccountForRequestWithMatcherAndAffinity(c, affinityKey, excludeAccounts, webImageAccountMatcher)
		attemptAcquireMs := int(time.Since(attemptAcquireStartedAt).Milliseconds())
		if account == nil {
			return nil, state, false
		}

		proxyURL := strings.TrimSpace(stickyProxyURL)
		if proxyURL == "" {
			proxyURL = h.store.ResolveProxyForAccount(account)
		}
		logRequestDispatch(c, spec.InboundEndpoint, attemptOffset+attempt+1, account, proxyURL, spec.RequestModel, spec.ReasoningEffort, attemptAcquireMs)

		attemptStartedAt := time.Now()
		payload, err := h.executeWebImageAttempt(c, account, proxyURL, spec)
		attemptDurationMs := int(time.Since(attemptStartedAt).Milliseconds())
		if err != nil {
			statusCode, body := extractWebImageUpstreamError(err)
			state.LastStatusCode = statusCode
			state.LastBody = body
			state.LastErr = err
			if kind := classifyHTTPFailure(statusCode, body); kind != "" {
				h.store.ReportRequestFailure(account, kind, time.Duration(attemptDurationMs)*time.Millisecond)
			} else if kind := classifyTransportFailure(err); kind != "" {
				h.store.ReportRequestFailure(account, kind, time.Duration(attemptDurationMs)*time.Millisecond)
			}
			if statusCode > 0 {
				h.applyCooldown(account, statusCode, body, nil)
			}
			logUpstreamAttemptResult(c, spec.InboundEndpoint, attemptOffset+attempt+1, account, proxyURL, statusCode, attemptDurationMs, requestStartedAt, err.Error())
			h.store.Release(account)
			h.store.UnbindSessionAffinity(affinityKey, account.ID())
			excludeAccounts[account.ID()] = true
			if shouldRetryWebImageFailure(statusCode, err) && attempt < maxRetries {
				continue
			}
			return nil, state, false
		}

		h.store.BindSessionAffinity(affinityKey, account, proxyURL)
		account.Mu().RLock()
		c.Set("x-account-email", account.Email)
		account.Mu().RUnlock()
		c.Set("x-account-proxy", proxyURL)
		c.Set("x-model", spec.RequestModel)
		if spec.ReasoningEffort != "" {
			c.Set("x-reasoning-effort", spec.ReasoningEffort)
		}

		resolvedServiceTier := resolveServiceTier("", spec.ServiceTier)
		if resolvedServiceTier != "" {
			c.Set("x-service-tier", resolvedServiceTier)
		}
		logInput := &database.UsageLogInput{
			AccountID:        account.ID(),
			Endpoint:         spec.InboundEndpoint,
			Model:            spec.RequestModel,
			StatusCode:       http.StatusOK,
			DurationMs:       int(time.Since(requestStartedAt).Milliseconds()),
			FirstTokenMs:     0,
			ReasoningEffort:  spec.ReasoningEffort,
			InboundEndpoint:  spec.InboundEndpoint,
			UpstreamEndpoint: "/backend-api/f/conversation",
			Stream:           spec.Stream,
			ServiceTier:      resolvedServiceTier,
			CompletionTokens: payload.ImageCount,
			OutputTokens:     payload.ImageCount,
			TotalTokens:      payload.ImageCount,
		}
		h.logUsageForRequest(c, logInput)
		logUpstreamAttemptResult(c, spec.InboundEndpoint, attemptOffset+attempt+1, account, proxyURL, http.StatusOK, attemptDurationMs, requestStartedAt, "")
		h.store.DecrementImageQuotaAfterSuccess(account, payload.ImageCount)
		h.store.ReportRequestSuccess(account, time.Duration(logInput.DurationMs)*time.Millisecond)
		h.store.Release(account)
		return payload, state, true
	}

	return nil, state, false
}

func (h *Handler) tryHandleWebImagesBatchRequest(c *gin.Context, spec webImageForwardSpec) (bool, *webImageFallbackState) {
	if spec.N <= 1 {
		return false, nil
	}
	logRequestLifecycleStartOnce(c, spec.InboundEndpoint, spec.RequestModel, spec.Stream, spec.ReasoningEffort)
	requestStartedAt := requestStartTime(c)
	apiKey := requestAPIKeyFromHeaders(c.Request.Header)
	sessionID := ResolveSessionID(c.Request.Header, spec.ResponsesBody)
	affinityKey := sessionAffinityKey(sessionID, apiKey)
	excludeAccounts := make(map[int64]bool)
	var lastState *webImageFallbackState
	payloads := make([]*webImageAttemptPayload, 0, spec.N)

	for i := 0; i < spec.N; i++ {
		payload, state, ok := h.runWebImageOne(c, spec, requestStartedAt, affinityKey, excludeAccounts, i*(h.getMaxRetries()+1))
		if !ok {
			lastState = state
			break
		}
		payloads = append(payloads, payload)
	}
	if len(payloads) == 0 {
		return false, lastState
	}

	firstTokenMs := 0
	var err error
	if spec.Stream {
		firstTokenMs, err = writeWebImagesBatchStream(c, spec.StreamPrefix, spec.ResponseFormat, payloads)
	} else {
		var out []byte
		out, _, err = buildWebImagesBatchPayload(payloads, spec.ResponseFormat)
		if err == nil {
			c.Data(http.StatusOK, "application/json", out)
		}
	}
	if err != nil {
		return false, &webImageFallbackState{LastErr: err}
	}
	if firstTokenMs > 0 {
		c.Set("x-first-token-ms", firstTokenMs)
	}
	return true, lastState
}

func (h *Handler) tryHandleWebImagesRequest(c *gin.Context, spec webImageForwardSpec) (bool, *webImageFallbackState) {
	if h == nil || h.store == nil {
		return false, nil
	}
	if spec.N <= 0 {
		spec.N = 1
	}
	if spec.N > 1 {
		return h.tryHandleWebImagesBatchRequest(c, spec)
	}
	logRequestLifecycleStartOnce(c, spec.InboundEndpoint, spec.RequestModel, spec.Stream, spec.ReasoningEffort)
	requestStartedAt := requestStartTime(c)
	apiKey := requestAPIKeyFromHeaders(c.Request.Header)
	sessionID := ResolveSessionID(c.Request.Header, spec.ResponsesBody)
	affinityKey := sessionAffinityKey(sessionID, apiKey)
	maxRetries := h.getMaxRetries()
	excludeAccounts := make(map[int64]bool)
	state := &webImageFallbackState{}

	for attempt := 0; attempt <= maxRetries; attempt++ {
		attemptAcquireStartedAt := time.Now()
		account, stickyProxyURL := h.acquireAccountForRequestWithMatcherAndAffinity(c, affinityKey, excludeAccounts, webImageAccountMatcher)
		attemptAcquireMs := int(time.Since(attemptAcquireStartedAt).Milliseconds())
		if account == nil {
			return false, state
		}

		proxyURL := strings.TrimSpace(stickyProxyURL)
		if proxyURL == "" {
			proxyURL = h.store.ResolveProxyForAccount(account)
		}
		logRequestDispatch(c, spec.InboundEndpoint, attempt+1, account, proxyURL, spec.RequestModel, spec.ReasoningEffort, attemptAcquireMs)

		attemptStartedAt := time.Now()
		payload, err := h.executeWebImageAttempt(c, account, proxyURL, spec)
		attemptDurationMs := int(time.Since(attemptStartedAt).Milliseconds())
		if err != nil {
			statusCode, body := extractWebImageUpstreamError(err)
			state.LastStatusCode = statusCode
			state.LastBody = body
			state.LastErr = err
			if kind := classifyHTTPFailure(statusCode, body); kind != "" {
				h.store.ReportRequestFailure(account, kind, time.Duration(attemptDurationMs)*time.Millisecond)
			} else if kind := classifyTransportFailure(err); kind != "" {
				h.store.ReportRequestFailure(account, kind, time.Duration(attemptDurationMs)*time.Millisecond)
			}
			if statusCode > 0 {
				h.applyCooldown(account, statusCode, body, nil)
			}
			logUpstreamAttemptResult(c, spec.InboundEndpoint, attempt+1, account, proxyURL, statusCode, attemptDurationMs, requestStartedAt, err.Error())
			h.store.Release(account)
			h.store.UnbindSessionAffinity(affinityKey, account.ID())
			excludeAccounts[account.ID()] = true
			if shouldRetryWebImageFailure(statusCode, err) && attempt < maxRetries {
				continue
			}
			return false, state
		}

		h.store.BindSessionAffinity(affinityKey, account, proxyURL)
		account.Mu().RLock()
		c.Set("x-account-email", account.Email)
		account.Mu().RUnlock()
		c.Set("x-account-proxy", proxyURL)
		c.Set("x-model", spec.RequestModel)
		if spec.ReasoningEffort != "" {
			c.Set("x-reasoning-effort", spec.ReasoningEffort)
		}
		firstTokenMs := 0
		if spec.Stream {
			firstTokenMs, err = writeWebImagesStream(c, spec.StreamPrefix, spec.ResponseFormat, payload)
		} else {
			c.Data(http.StatusOK, "application/json", payload.ImagePayload)
		}
		if err != nil {
			logUpstreamAttemptResult(c, spec.InboundEndpoint, attempt+1, account, proxyURL, http.StatusBadGateway, attemptDurationMs, requestStartedAt, err.Error())
			h.store.UnbindSessionAffinity(affinityKey, account.ID())
			h.store.ReportRequestFailure(account, "transport", time.Duration(attemptDurationMs)*time.Millisecond)
			h.store.Release(account)
			state.LastErr = err
			return false, state
		}

		c.Set("x-first-token-ms", firstTokenMs)
		resolvedServiceTier := resolveServiceTier("", spec.ServiceTier)
		if resolvedServiceTier != "" {
			c.Set("x-service-tier", resolvedServiceTier)
		}
		logInput := &database.UsageLogInput{
			AccountID:        account.ID(),
			Endpoint:         spec.InboundEndpoint,
			Model:            spec.RequestModel,
			StatusCode:       http.StatusOK,
			DurationMs:       int(time.Since(requestStartedAt).Milliseconds()),
			FirstTokenMs:     firstTokenMs,
			ReasoningEffort:  spec.ReasoningEffort,
			InboundEndpoint:  spec.InboundEndpoint,
			UpstreamEndpoint: "/backend-api/f/conversation",
			Stream:           spec.Stream,
			ServiceTier:      resolvedServiceTier,
			CompletionTokens: payload.ImageCount,
			OutputTokens:     payload.ImageCount,
			TotalTokens:      payload.ImageCount,
		}
		h.logUsageForRequest(c, logInput)
		logUpstreamAttemptResult(c, spec.InboundEndpoint, attempt+1, account, proxyURL, http.StatusOK, attemptDurationMs, requestStartedAt, "")
		h.store.DecrementImageQuotaAfterSuccess(account, payload.ImageCount)
		h.store.ReportRequestSuccess(account, time.Duration(logInput.DurationMs)*time.Millisecond)
		h.store.Release(account)
		return true, state
	}

	return false, state
}

func (h *Handler) tryHandleWebChatCompletionsImageRequest(c *gin.Context, rawBody []byte, model string) (bool, *webImageFallbackState) {
	rawBody = normalizeServiceTierField(rawBody)
	if gjson.GetBytes(rawBody, "mask").Exists() {
		return false, nil
	}
	if n := parseIntField(gjson.GetBytes(rawBody, "n").String(), 1); n > 1 {
		return false, nil
	}

	prompt, imageURLs, err := extractPromptAndImagesFromChatMessages(gjson.GetBytes(rawBody, "messages"))
	if err != nil {
		return false, nil
	}
	refs, err := webReferencesFromImageURLs(imageURLs)
	if err != nil {
		return false, nil
	}
	responsesBody, err := buildChatCompletionsImageResponsesRequest(rawBody, model)
	if err != nil {
		return false, nil
	}

	isStream := gjson.GetBytes(rawBody, "stream").Bool()
	reasoningEffort := extractReasoningEffort(rawBody)
	serviceTier := extractServiceTier(rawBody)
	if serviceTier != "" {
		c.Set("x-service-tier", serviceTier)
	}

	logRequestLifecycleStartOnce(c, "/v1/chat/completions", model, isStream, reasoningEffort)
	requestStartedAt := requestStartTime(c)
	apiKey := requestAPIKeyFromHeaders(c.Request.Header)
	sessionID := ResolveSessionID(c.Request.Header, responsesBody)
	affinityKey := sessionAffinityKey(sessionID, apiKey)
	maxRetries := h.getMaxRetries()
	excludeAccounts := make(map[int64]bool)
	state := &webImageFallbackState{}

	for attempt := 0; attempt <= maxRetries; attempt++ {
		attemptAcquireStartedAt := time.Now()
		account, stickyProxyURL := h.acquireAccountForRequestWithMatcherAndAffinity(c, affinityKey, excludeAccounts, webImageAccountMatcher)
		attemptAcquireMs := int(time.Since(attemptAcquireStartedAt).Milliseconds())
		if account == nil {
			return false, state
		}

		proxyURL := strings.TrimSpace(stickyProxyURL)
		if proxyURL == "" {
			proxyURL = h.store.ResolveProxyForAccount(account)
		}
		logRequestDispatch(c, "/v1/chat/completions", attempt+1, account, proxyURL, model, reasoningEffort, attemptAcquireMs)

		attemptStartedAt := time.Now()
		payload, err := h.executeWebImageAttempt(c, account, proxyURL, webImageForwardSpec{
			InboundEndpoint: "/v1/chat/completions",
			RequestModel:    model,
			Prompt:          prompt,
			References:      refs,
			ResponseFormat:  "b64_json",
			ResponsesBody:   responsesBody,
			ReasoningEffort: reasoningEffort,
			ServiceTier:     serviceTier,
			Stream:          isStream,
		})
		attemptDurationMs := int(time.Since(attemptStartedAt).Milliseconds())
		if err != nil {
			statusCode, body := extractWebImageUpstreamError(err)
			state.LastStatusCode = statusCode
			state.LastBody = body
			state.LastErr = err
			if kind := classifyHTTPFailure(statusCode, body); kind != "" {
				h.store.ReportRequestFailure(account, kind, time.Duration(attemptDurationMs)*time.Millisecond)
			} else if kind := classifyTransportFailure(err); kind != "" {
				h.store.ReportRequestFailure(account, kind, time.Duration(attemptDurationMs)*time.Millisecond)
			}
			if statusCode > 0 {
				h.applyCooldown(account, statusCode, body, nil)
			}
			logUpstreamAttemptResult(c, "/v1/chat/completions", attempt+1, account, proxyURL, statusCode, attemptDurationMs, requestStartedAt, err.Error())
			h.store.Release(account)
			h.store.UnbindSessionAffinity(affinityKey, account.ID())
			excludeAccounts[account.ID()] = true
			if shouldRetryWebImageFailure(statusCode, err) && attempt < maxRetries {
				continue
			}
			return false, state
		}

		h.store.BindSessionAffinity(affinityKey, account, proxyURL)
		account.Mu().RLock()
		c.Set("x-account-email", account.Email)
		account.Mu().RUnlock()
		c.Set("x-account-proxy", proxyURL)
		c.Set("x-model", model)
		c.Set("x-reasoning-effort", reasoningEffort)

		chunkID := "chatcmpl-" + fmt.Sprint(time.Now().UnixNano())
		firstTokenMs := 0
		if isStream {
			firstTokenMs, err = writeChatCompletionsImageStream(c, model, chunkID, payload.CreatedAt, payload.ImagePayload, nil)
		} else {
			var out []byte
			out, err = buildChatCompletionsImageResponse(payload.ImagePayload, model, chunkID, payload.CreatedAt, nil)
			if err == nil {
				c.Data(http.StatusOK, "application/json", out)
			}
		}
		if err != nil {
			logUpstreamAttemptResult(c, "/v1/chat/completions", attempt+1, account, proxyURL, http.StatusBadGateway, attemptDurationMs, requestStartedAt, err.Error())
			h.store.UnbindSessionAffinity(affinityKey, account.ID())
			h.store.ReportRequestFailure(account, "transport", time.Duration(attemptDurationMs)*time.Millisecond)
			h.store.Release(account)
			state.LastErr = err
			return false, state
		}

		c.Set("x-first-token-ms", firstTokenMs)
		resolvedServiceTier := resolveServiceTier("", serviceTier)
		if resolvedServiceTier != "" {
			c.Set("x-service-tier", resolvedServiceTier)
		}
		logInput := &database.UsageLogInput{
			AccountID:        account.ID(),
			Endpoint:         "/v1/chat/completions",
			Model:            model,
			StatusCode:       http.StatusOK,
			DurationMs:       int(time.Since(requestStartedAt).Milliseconds()),
			FirstTokenMs:     firstTokenMs,
			ReasoningEffort:  reasoningEffort,
			InboundEndpoint:  "/v1/chat/completions",
			UpstreamEndpoint: "/backend-api/f/conversation",
			Stream:           isStream,
			ServiceTier:      resolvedServiceTier,
			CompletionTokens: payload.ImageCount,
			OutputTokens:     payload.ImageCount,
			TotalTokens:      payload.ImageCount,
		}
		h.logUsageForRequest(c, logInput)
		logUpstreamAttemptResult(c, "/v1/chat/completions", attempt+1, account, proxyURL, http.StatusOK, attemptDurationMs, requestStartedAt, "")
		h.store.DecrementImageQuotaAfterSuccess(account, payload.ImageCount)
		h.store.ReportRequestSuccess(account, time.Duration(logInput.DurationMs)*time.Millisecond)
		h.store.Release(account)
		return true, state
	}

	return false, state
}

func multipartFileHeaderToReferenceImage(fileHeader *multipart.FileHeader) (webimage.ReferenceImage, error) {
	if fileHeader == nil {
		return webimage.ReferenceImage{}, fmt.Errorf("upload file is nil")
	}
	file, err := fileHeader.Open()
	if err != nil {
		return webimage.ReferenceImage{}, fmt.Errorf("open upload file failed: %w", err)
	}
	defer file.Close()
	data, err := io.ReadAll(file)
	if err != nil {
		return webimage.ReferenceImage{}, fmt.Errorf("read upload file failed: %w", err)
	}
	if len(data) == 0 {
		return webimage.ReferenceImage{}, fmt.Errorf("upload %q is empty", strings.TrimSpace(fileHeader.Filename))
	}
	name := strings.TrimSpace(fileHeader.Filename)
	if name == "" {
		name = "image.png"
	}
	return webimage.ReferenceImage{Data: data, FileName: name}, nil
}

func dataURLToReferenceImage(raw string, index int) (webimage.ReferenceImage, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return webimage.ReferenceImage{}, fmt.Errorf("image_url is empty")
	}
	if !strings.HasPrefix(strings.ToLower(raw), "data:") {
		return webimage.ReferenceImage{}, fmt.Errorf("only data URL images are supported by web image proxy")
	}
	comma := strings.Index(raw, ",")
	if comma <= 0 {
		return webimage.ReferenceImage{}, fmt.Errorf("invalid data URL image")
	}
	header := raw[:comma]
	encoded := raw[comma+1:]
	if !strings.Contains(strings.ToLower(header), ";base64") {
		return webimage.ReferenceImage{}, fmt.Errorf("only base64 data URL images are supported by web image proxy")
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return webimage.ReferenceImage{}, fmt.Errorf("decode data URL image failed: %w", err)
	}
	mediaType := strings.TrimPrefix(strings.SplitN(header, ";", 2)[0], "data:")
	ext := ".png"
	switch strings.ToLower(mediaType) {
	case "image/jpeg":
		ext = ".jpg"
	case "image/webp":
		ext = ".webp"
	}
	return webimage.ReferenceImage{Data: data, FileName: fmt.Sprintf("image_%d%s", index+1, ext)}, nil
}

func webReferencesFromImageURLs(urls []string) ([]webimage.ReferenceImage, error) {
	refs := make([]webimage.ReferenceImage, 0, len(urls))
	for idx, raw := range urls {
		ref, err := dataURLToReferenceImage(raw, idx)
		if err != nil {
			return nil, err
		}
		refs = append(refs, ref)
	}
	return refs, nil
}
