package proxy

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	defaultImagesMainModel = "gpt-5.4-mini"
	defaultImagesToolModel = "gpt-image-2"
)

type imageCallResult struct {
	Result        string
	RevisedPrompt string
	OutputFormat  string
	Size          string
	Background    string
	Quality       string
	Model         string
}

func isImageOnlyModel(model string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(model)), "gpt-image-")
}

func validateImagesModel(model string) error {
	if !isImageOnlyModel(model) {
		return fmt.Errorf("images endpoint requires an image model, got %q", strings.TrimSpace(model))
	}
	return nil
}

func sendImageOnlyModelError(c *gin.Context, model string) {
	c.JSON(http.StatusServiceUnavailable, gin.H{
		"error": gin.H{
			"message": fmt.Sprintf("model %s is only supported on /v1/images/generations and /v1/images/edits", strings.TrimSpace(model)),
			"type":    "server_error",
		},
	})
}

func mimeTypeFromOutputFormat(outputFormat string) string {
	outputFormat = strings.TrimSpace(outputFormat)
	if outputFormat == "" {
		return "image/png"
	}
	if strings.Contains(outputFormat, "/") {
		return outputFormat
	}
	switch strings.ToLower(outputFormat) {
	case "png":
		return "image/png"
	case "jpg", "jpeg":
		return "image/jpeg"
	case "webp":
		return "image/webp"
	default:
		return "image/png"
	}
}

func parseIntField(raw string, fallback int64) int64 {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fallback
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return fallback
	}
	return value
}

func parseBoolField(raw string, fallback bool) bool {
	raw = strings.TrimSpace(strings.ToLower(raw))
	if raw == "" {
		return fallback
	}
	switch raw {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return fallback
	}
}

func multipartFileToDataURL(fileHeader *multipart.FileHeader) (string, error) {
	if fileHeader == nil {
		return "", fmt.Errorf("upload file is nil")
	}
	file, err := fileHeader.Open()
	if err != nil {
		return "", fmt.Errorf("open upload file failed: %w", err)
	}
	defer file.Close()

	data, err := io.ReadAll(file)
	if err != nil {
		return "", fmt.Errorf("read upload file failed: %w", err)
	}
	if len(data) == 0 {
		return "", fmt.Errorf("upload %q is empty", strings.TrimSpace(fileHeader.Filename))
	}

	mediaType := strings.TrimSpace(fileHeader.Header.Get("Content-Type"))
	if mediaType == "" {
		mediaType = http.DetectContentType(data)
	}
	return "data:" + mediaType + ";base64," + base64.StdEncoding.EncodeToString(data), nil
}

func (h *Handler) ImagesGenerations(c *gin.Context) {
	rawBody, err := io.ReadAll(c.Request.Body)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "Invalid request: " + err.Error(), "type": "invalid_request_error"}})
		return
	}
	if !json.Valid(rawBody) {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "Invalid request: body must be valid JSON", "type": "invalid_request_error"}})
		return
	}

	prompt := strings.TrimSpace(gjson.GetBytes(rawBody, "prompt").String())
	if prompt == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "Invalid request: prompt is required", "type": "invalid_request_error"}})
		return
	}

	imageModel := strings.TrimSpace(gjson.GetBytes(rawBody, "model").String())
	if imageModel == "" {
		imageModel = defaultImagesToolModel
	}
	if err := validateImagesModel(imageModel); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "Invalid request: " + err.Error(), "type": "invalid_request_error"}})
		return
	}

	responseFormat := strings.TrimSpace(gjson.GetBytes(rawBody, "response_format").String())
	if responseFormat == "" {
		responseFormat = "b64_json"
	}
	stream := gjson.GetBytes(rawBody, "stream").Bool()

	tool := []byte(`{"type":"image_generation","action":"generate","model":""}`)
	tool, _ = sjson.SetBytes(tool, "model", imageModel)
	for _, field := range []string{"size", "quality", "background", "output_format", "moderation", "style"} {
		if value := strings.TrimSpace(gjson.GetBytes(rawBody, field).String()); value != "" {
			tool, _ = sjson.SetBytes(tool, field, value)
		}
	}
	for _, field := range []string{"output_compression", "partial_images"} {
		if value := gjson.GetBytes(rawBody, field); value.Exists() && value.Type == gjson.Number {
			tool, _ = sjson.SetBytes(tool, field, value.Int())
		}
	}

	responsesBody := buildImagesResponsesRequest(prompt, nil, tool)
	h.forwardImagesRequest(c, "/v1/images/generations", imageModel, responsesBody, responseFormat, "image_generation", stream)
}

func (h *Handler) ImagesEdits(c *gin.Context) {
	contentType := strings.ToLower(strings.TrimSpace(c.GetHeader("Content-Type")))
	if strings.HasPrefix(contentType, "application/json") {
		h.imagesEditsFromJSON(c)
		return
	}
	if strings.HasPrefix(contentType, "multipart/form-data") || contentType == "" {
		h.imagesEditsFromMultipart(c)
		return
	}

	c.JSON(http.StatusBadRequest, gin.H{
		"error": gin.H{"message": fmt.Sprintf("Invalid request: unsupported Content-Type %q", contentType), "type": "invalid_request_error"},
	})
}

func (h *Handler) imagesEditsFromMultipart(c *gin.Context) {
	form, err := c.MultipartForm()
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "Invalid request: " + err.Error(), "type": "invalid_request_error"}})
		return
	}

	prompt := strings.TrimSpace(c.PostForm("prompt"))
	if prompt == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "Invalid request: prompt is required", "type": "invalid_request_error"}})
		return
	}

	var imageFiles []*multipart.FileHeader
	if files := form.File["image[]"]; len(files) > 0 {
		imageFiles = files
	} else if files := form.File["image"]; len(files) > 0 {
		imageFiles = files
	}
	if len(imageFiles) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "Invalid request: image is required", "type": "invalid_request_error"}})
		return
	}

	images := make([]string, 0, len(imageFiles))
	for _, fileHeader := range imageFiles {
		dataURL, err := multipartFileToDataURL(fileHeader)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "Invalid request: " + err.Error(), "type": "invalid_request_error"}})
			return
		}
		images = append(images, dataURL)
	}

	var maskDataURL string
	if maskFiles := form.File["mask"]; len(maskFiles) > 0 && maskFiles[0] != nil {
		dataURL, err := multipartFileToDataURL(maskFiles[0])
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "Invalid request: " + err.Error(), "type": "invalid_request_error"}})
			return
		}
		maskDataURL = dataURL
	}

	imageModel := strings.TrimSpace(c.PostForm("model"))
	if imageModel == "" {
		imageModel = defaultImagesToolModel
	}
	if err := validateImagesModel(imageModel); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "Invalid request: " + err.Error(), "type": "invalid_request_error"}})
		return
	}

	responseFormat := strings.TrimSpace(c.PostForm("response_format"))
	if responseFormat == "" {
		responseFormat = "b64_json"
	}
	stream := parseBoolField(c.PostForm("stream"), false)

	tool := buildImagesEditToolFromForm(c, imageModel, maskDataURL)
	responsesBody := buildImagesResponsesRequest(prompt, images, tool)
	h.forwardImagesRequest(c, "/v1/images/edits", imageModel, responsesBody, responseFormat, "image_edit", stream)
}

func buildImagesEditToolFromForm(c *gin.Context, imageModel, maskDataURL string) []byte {
	tool := []byte(`{"type":"image_generation","action":"edit","model":""}`)
	tool, _ = sjson.SetBytes(tool, "model", imageModel)
	for _, field := range []string{"size", "quality", "background", "output_format", "input_fidelity", "moderation", "style"} {
		if value := strings.TrimSpace(c.PostForm(field)); value != "" {
			tool, _ = sjson.SetBytes(tool, field, value)
		}
	}
	for _, field := range []string{"output_compression", "partial_images"} {
		if value := strings.TrimSpace(c.PostForm(field)); value != "" {
			tool, _ = sjson.SetBytes(tool, field, parseIntField(value, 0))
		}
	}
	if strings.TrimSpace(maskDataURL) != "" {
		tool, _ = sjson.SetBytes(tool, "input_image_mask.image_url", strings.TrimSpace(maskDataURL))
	}
	return tool
}

func (h *Handler) imagesEditsFromJSON(c *gin.Context) {
	rawBody, err := io.ReadAll(c.Request.Body)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "Invalid request: " + err.Error(), "type": "invalid_request_error"}})
		return
	}
	if !json.Valid(rawBody) {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "Invalid request: body must be valid JSON", "type": "invalid_request_error"}})
		return
	}

	prompt := strings.TrimSpace(gjson.GetBytes(rawBody, "prompt").String())
	if prompt == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "Invalid request: prompt is required", "type": "invalid_request_error"}})
		return
	}

	images := make([]string, 0)
	imagesResult := gjson.GetBytes(rawBody, "images")
	if imagesResult.Exists() && !imagesResult.IsArray() {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "Invalid request: invalid images field type", "type": "invalid_request_error"}})
		return
	}
	for _, image := range imagesResult.Array() {
		if imageURL := strings.TrimSpace(image.Get("image_url").String()); imageURL != "" {
			images = append(images, imageURL)
			continue
		}
		if image.Get("file_id").Exists() {
			c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "Invalid request: images[].file_id is not supported (use images[].image_url instead)", "type": "invalid_request_error"}})
			return
		}
	}
	if len(images) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "Invalid request: images[].image_url is required", "type": "invalid_request_error"}})
		return
	}

	maskDataURL := strings.TrimSpace(gjson.GetBytes(rawBody, "mask.image_url").String())
	if maskDataURL == "" && gjson.GetBytes(rawBody, "mask.file_id").Exists() {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "Invalid request: mask.file_id is not supported (use mask.image_url instead)", "type": "invalid_request_error"}})
		return
	}

	imageModel := strings.TrimSpace(gjson.GetBytes(rawBody, "model").String())
	if imageModel == "" {
		imageModel = defaultImagesToolModel
	}
	if err := validateImagesModel(imageModel); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "Invalid request: " + err.Error(), "type": "invalid_request_error"}})
		return
	}

	responseFormat := strings.TrimSpace(gjson.GetBytes(rawBody, "response_format").String())
	if responseFormat == "" {
		responseFormat = "b64_json"
	}
	stream := gjson.GetBytes(rawBody, "stream").Bool()

	tool := []byte(`{"type":"image_generation","action":"edit","model":""}`)
	tool, _ = sjson.SetBytes(tool, "model", imageModel)
	for _, field := range []string{"size", "quality", "background", "output_format", "input_fidelity", "moderation", "style"} {
		if value := strings.TrimSpace(gjson.GetBytes(rawBody, field).String()); value != "" {
			tool, _ = sjson.SetBytes(tool, field, value)
		}
	}
	for _, field := range []string{"output_compression", "partial_images"} {
		if value := gjson.GetBytes(rawBody, field); value.Exists() && value.Type == gjson.Number {
			tool, _ = sjson.SetBytes(tool, field, value.Int())
		}
	}
	if maskDataURL != "" {
		tool, _ = sjson.SetBytes(tool, "input_image_mask.image_url", maskDataURL)
	}

	responsesBody := buildImagesResponsesRequest(prompt, images, tool)
	h.forwardImagesRequest(c, "/v1/images/edits", imageModel, responsesBody, responseFormat, "image_edit", stream)
}

func buildImagesResponsesRequest(prompt string, images []string, toolJSON []byte) []byte {
	req := []byte(`{"instructions":"","stream":true,"parallel_tool_calls":true,"include":["reasoning.encrypted_content"],"model":"","store":false,"tool_choice":{"type":"image_generation"}}`)
	req, _ = sjson.SetBytes(req, "model", defaultImagesMainModel)

	input := []byte(`[{"type":"message","role":"user","content":[{"type":"input_text","text":""}]}]`)
	input, _ = sjson.SetBytes(input, "0.content.0.text", prompt)
	contentIndex := 1
	for _, imageURL := range images {
		if strings.TrimSpace(imageURL) == "" {
			continue
		}
		part := []byte(`{"type":"input_image","image_url":""}`)
		part, _ = sjson.SetBytes(part, "image_url", imageURL)
		input, _ = sjson.SetRawBytes(input, fmt.Sprintf("0.content.%d", contentIndex), part)
		contentIndex++
	}
	req, _ = sjson.SetRawBytes(req, "input", input)

	req, _ = sjson.SetRawBytes(req, "tools", []byte(`[]`))
	if len(toolJSON) > 0 && json.Valid(toolJSON) {
		req, _ = sjson.SetRawBytes(req, "tools.-1", toolJSON)
	}
	return req
}

func normalizeChatCompletionMessageShorthandRequest(rawBody []byte) []byte {
	if len(rawBody) == 0 || !json.Valid(rawBody) {
		return rawBody
	}
	if gjson.GetBytes(rawBody, "messages").Exists() {
		return rawBody
	}

	content := gjson.GetBytes(rawBody, "content")
	if !content.Exists() {
		return rawBody
	}

	role := strings.TrimSpace(gjson.GetBytes(rawBody, "role").String())
	if role == "" {
		role = "user"
	}

	message := []byte(`{}`)
	message, _ = sjson.SetBytes(message, "role", role)
	message, _ = sjson.SetRawBytes(message, "content", []byte(content.Raw))
	for _, field := range []string{"name", "tool_call_id"} {
		if value := gjson.GetBytes(rawBody, field); value.Exists() {
			message, _ = sjson.SetRawBytes(message, field, []byte(value.Raw))
		}
	}
	if toolCalls := gjson.GetBytes(rawBody, "tool_calls"); toolCalls.Exists() {
		message, _ = sjson.SetRawBytes(message, "tool_calls", []byte(toolCalls.Raw))
	}

	normalized, err := sjson.SetRawBytes(rawBody, "messages", []byte("["+string(message)+"]"))
	if err != nil {
		return rawBody
	}
	for _, field := range []string{"role", "content", "name", "tool_call_id", "tool_calls"} {
		normalized, _ = sjson.DeleteBytes(normalized, field)
	}
	return normalized
}

func extractPromptAndImagesFromChatMessages(messages gjson.Result) (string, []string, error) {
	if !messages.Exists() || !messages.IsArray() {
		return "", nil, fmt.Errorf("messages is required")
	}

	lines := make([]string, 0, len(messages.Array()))
	images := make([]string, 0, 4)
	messages.ForEach(func(_, msg gjson.Result) bool {
		role := strings.ToLower(strings.TrimSpace(msg.Get("role").String()))
		content := msg.Get("content")

		texts := make([]string, 0, 4)
		switch {
		case content.Type == gjson.String:
			if text := strings.TrimSpace(content.String()); text != "" {
				texts = append(texts, text)
			}
		case content.IsArray():
			content.ForEach(func(_, part gjson.Result) bool {
				partType := strings.ToLower(strings.TrimSpace(part.Get("type").String()))
				switch partType {
				case "text", "input_text", "output_text":
					if text := strings.TrimSpace(part.Get("text").String()); text != "" {
						texts = append(texts, text)
					}
				case "image_url":
					if imageURL := strings.TrimSpace(part.Get("image_url.url").String()); imageURL != "" {
						images = append(images, imageURL)
					}
				case "input_image", "image":
					imageURL := strings.TrimSpace(part.Get("image_url").String())
					if imageURL == "" {
						imageURL = strings.TrimSpace(part.Get("url").String())
					}
					if imageURL != "" {
						images = append(images, imageURL)
					}
				}
				return true
			})
		}

		text := strings.TrimSpace(strings.Join(texts, "\n"))
		if text == "" {
			return true
		}
		label := "User"
		switch role {
		case "system":
			label = "System"
		case "developer":
			label = "Developer"
		case "assistant":
			label = "Assistant"
		case "tool":
			label = "Tool"
		}
		lines = append(lines, label+": "+text)
		return true
	})

	prompt := strings.TrimSpace(strings.Join(lines, "\n\n"))
	if prompt == "" && len(images) > 0 {
		prompt = "Edit the provided image."
	}
	if prompt == "" {
		return "", images, fmt.Errorf("at least one text prompt is required")
	}
	return prompt, images, nil
}

func buildChatCompletionsImageTool(rawBody []byte, imageModel string, hasImages bool) []byte {
	action := "generate"
	if hasImages {
		action = "edit"
	}
	tool := []byte(`{"type":"image_generation","action":"","model":""}`)
	tool, _ = sjson.SetBytes(tool, "action", action)
	tool, _ = sjson.SetBytes(tool, "model", imageModel)

	stringFields := []string{"size", "quality", "background", "output_format", "moderation", "style"}
	if hasImages {
		stringFields = append(stringFields, "input_fidelity")
	}
	for _, field := range stringFields {
		if value := strings.TrimSpace(gjson.GetBytes(rawBody, field).String()); value != "" {
			tool, _ = sjson.SetBytes(tool, field, value)
		}
	}
	for _, field := range []string{"output_compression", "partial_images"} {
		if value := gjson.GetBytes(rawBody, field); value.Exists() && value.Type == gjson.Number {
			tool, _ = sjson.SetBytes(tool, field, value.Int())
		}
	}
	if hasImages {
		if maskURL := strings.TrimSpace(gjson.GetBytes(rawBody, "mask.image_url").String()); maskURL != "" {
			tool, _ = sjson.SetBytes(tool, "input_image_mask.image_url", maskURL)
		}
	}
	return tool
}

func buildChatCompletionsImageResponsesRequest(rawBody []byte, imageModel string) ([]byte, error) {
	rawBody = normalizeServiceTierField(rawBody)
	prompt, images, err := extractPromptAndImagesFromChatMessages(gjson.GetBytes(rawBody, "messages"))
	if err != nil {
		return nil, err
	}

	tool := buildChatCompletionsImageTool(rawBody, imageModel, len(images) > 0)
	responsesBody := buildImagesResponsesRequest(prompt, images, tool)

	if reasoning := gjson.GetBytes(rawBody, "reasoning.effort"); reasoning.Exists() && strings.TrimSpace(reasoning.String()) != "" {
		responsesBody, _ = sjson.SetBytes(responsesBody, "reasoning.effort", reasoning.String())
	} else if reasoning := gjson.GetBytes(rawBody, "reasoning_effort"); reasoning.Exists() && strings.TrimSpace(reasoning.String()) != "" {
		responsesBody, _ = sjson.SetBytes(responsesBody, "reasoning.effort", reasoning.String())
	}
	if serviceTier := extractServiceTier(rawBody); serviceTier != "" {
		responsesBody, _ = sjson.SetBytes(responsesBody, "service_tier", serviceTier)
	}
	return sanitizeServiceTierForUpstream(responsesBody), nil
}

func renderChatMarkdownFromImagePayload(payload []byte) (string, error) {
	data := gjson.GetBytes(payload, "data")
	if !data.Exists() || !data.IsArray() || len(data.Array()) == 0 {
		return "", fmt.Errorf("image payload is empty")
	}

	mimeType := mimeTypeFromOutputFormat(gjson.GetBytes(payload, "output_format").String())
	parts := make([]string, 0, len(data.Array()))
	for idx, item := range data.Array() {
		imageURL := strings.TrimSpace(item.Get("url").String())
		if imageURL == "" {
			b64 := strings.TrimSpace(item.Get("b64_json").String())
			if b64 == "" {
				continue
			}
			imageURL = "data:" + mimeType + ";base64," + b64
		}
		parts = append(parts, fmt.Sprintf("![image_%d](%s)", idx+1, imageURL))
	}
	if len(parts) == 0 {
		return "", fmt.Errorf("image payload does not contain usable images")
	}
	return strings.Join(parts, "\n\n"), nil
}

func buildChatCompletionsImageResponse(imagePayload []byte, model, chunkID string, created int64, usage *UsageInfo) ([]byte, error) {
	content, err := renderChatMarkdownFromImagePayload(imagePayload)
	if err != nil {
		return nil, err
	}
	if created <= 0 {
		created = time.Now().Unix()
	}

	result := []byte(`{}`)
	result, _ = sjson.SetBytes(result, "id", chunkID)
	result, _ = sjson.SetBytes(result, "object", "chat.completion")
	result, _ = sjson.SetBytes(result, "created", created)
	result, _ = sjson.SetBytes(result, "model", model)
	result, _ = sjson.SetBytes(result, "choices.0.index", 0)
	result, _ = sjson.SetBytes(result, "choices.0.message.role", "assistant")
	result, _ = sjson.SetBytes(result, "choices.0.message.content", content)
	result, _ = sjson.SetBytes(result, "choices.0.finish_reason", "stop")

	if usage == nil {
		usage = extractUsageFromResult(gjson.GetBytes(imagePayload, "usage"))
	}
	if usage != nil {
		result, _ = sjson.SetBytes(result, "usage.prompt_tokens", usage.PromptTokens)
		result, _ = sjson.SetBytes(result, "usage.completion_tokens", usage.CompletionTokens)
		result, _ = sjson.SetBytes(result, "usage.total_tokens", usage.TotalTokens)
	}
	return result, nil
}

func writeChatCompletionsImageStream(c *gin.Context, model, chunkID string, created int64, imagePayload []byte, usage *UsageInfo) (int, error) {
	content, err := renderChatMarkdownFromImagePayload(imagePayload)
	if err != nil {
		return 0, err
	}
	if created <= 0 {
		created = time.Now().Unix()
	}
	if usage == nil {
		usage = extractUsageFromResult(gjson.GetBytes(imagePayload, "usage"))
	}

	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")

	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		return 0, fmt.Errorf("streaming not supported")
	}

	if _, err := fmt.Fprint(c.Writer, ": keep-alive\n\n"); err != nil {
		return 0, err
	}
	flusher.Flush()

	firstTokenMs := int(time.Since(requestStartTime(c)).Milliseconds())

	roleChunk := buildOpenAIChunk(chunkID, model, "", "", "")
	roleChunk, _ = sjson.SetBytes(roleChunk, "created", created)
	roleChunk, _ = sjson.SetBytes(roleChunk, "choices.0.delta.role", "assistant")
	if _, err := fmt.Fprintf(c.Writer, "data: %s\n\n", roleChunk); err != nil {
		return 0, err
	}
	flusher.Flush()

	contentChunk := buildOpenAIChunk(chunkID, model, content, "", "")
	contentChunk, _ = sjson.SetBytes(contentChunk, "created", created)
	if _, err := fmt.Fprintf(c.Writer, "data: %s\n\n", contentChunk); err != nil {
		return 0, err
	}
	flusher.Flush()

	finalChunk := buildOpenAIFinalChunk(chunkID, model, usage)
	finalChunk, _ = sjson.SetBytes(finalChunk, "created", created)
	if _, err := fmt.Fprintf(c.Writer, "data: %s\n\n", finalChunk); err != nil {
		return 0, err
	}
	if _, err := fmt.Fprint(c.Writer, "data: [DONE]\n\n"); err != nil {
		return 0, err
	}
	flusher.Flush()
	return firstTokenMs, nil
}

func (h *Handler) handleChatCompletionsImageFallback(c *gin.Context, rawBody []byte, model string) {
	rawBody = normalizeServiceTierField(rawBody)
	isStream := gjson.GetBytes(rawBody, "stream").Bool()
	reasoningEffort := extractReasoningEffort(rawBody)
	serviceTier := extractServiceTier(rawBody)
	if serviceTier != "" {
		c.Set("x-service-tier", serviceTier)
	}
	logRequestLifecycleStart(c, "/v1/chat/completions", model, isStream, reasoningEffort)

	responsesBody, err := buildChatCompletionsImageResponsesRequest(rawBody, model)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": gin.H{"message": "Invalid request: " + err.Error(), "type": "invalid_request_error"},
		})
		return
	}

	requestStartedAt := requestStartTime(c)
	apiKey := requestAPIKeyFromHeaders(c.Request.Header)
	sessionID := ResolveSessionID(c.Request.Header, responsesBody)
	affinityKey := sessionAffinityKey(sessionID, apiKey)
	maxRetries := h.getMaxRetries()
	var lastErr error
	var lastStatusCode int
	var lastBody []byte
	excludeAccounts := make(map[int64]bool)

	for attempt := 0; attempt <= maxRetries; attempt++ {
		attemptAcquireStartedAt := time.Now()
		account, stickyProxyURL := h.acquireAccountForRequestWithAffinity(c, affinityKey, excludeAccounts)
		attemptAcquireMs := int(time.Since(attemptAcquireStartedAt).Milliseconds())
		if account == nil {
			if lastStatusCode != 0 && len(lastBody) > 0 {
				h.sendFinalUpstreamError(c, lastStatusCode, lastBody)
				return
			}
			c.JSON(http.StatusServiceUnavailable, gin.H{
				"error": gin.H{"message": "无可用账号，请稍后重试", "type": "server_error"},
			})
			return
		}

		start := time.Now()
		proxyURL := strings.TrimSpace(stickyProxyURL)
		if proxyURL == "" {
			proxyURL = h.store.ResolveProxyForAccount(account)
		}
		deviceCfg := h.deviceCfg
		if deviceCfg == nil {
			deviceCfg = &DeviceProfileConfig{StabilizeDeviceProfile: false}
		}
		downstreamHeaders := c.Request.Header.Clone()
		useWebsocket := shouldUseWebsocketTransport(h.cfg, c.Request)
		logRequestDispatch(c, "/v1/chat/completions", attempt+1, account, proxyURL, model, reasoningEffort, attemptAcquireMs)

		resp, attemptTrace, reqErr := ExecuteRequestTraced(c.Request.Context(), account, responsesBody, sessionID, proxyURL, apiKey, deviceCfg, downstreamHeaders, useWebsocket)
		durationMs := int(time.Since(start).Milliseconds())
		statusCode := 0
		if resp != nil {
			statusCode = resp.StatusCode
		}
		if attemptTrace != nil && !attemptTrace.HeaderAt.IsZero() {
			logUpstreamAttemptHeaders(c, "/v1/chat/completions", attempt+1, account, statusCode, attemptTrace, requestStartedAt)
		}
		if reqErr != nil {
			if kind := classifyTransportFailure(reqErr); kind != "" {
				h.store.ReportRequestFailure(account, kind, time.Duration(durationMs)*time.Millisecond)
			}
			logUpstreamAttemptResult(c, "/v1/chat/completions", attempt+1, account, proxyURL, 0, durationMs, requestStartedAt, reqErr.Error())
			h.store.Release(account)
			h.store.UnbindSessionAffinity(affinityKey, account.ID())
			excludeAccounts[account.ID()] = true
			if !IsRetryableError(reqErr) && classifyTransportFailure(reqErr) == "" {
				ErrorToGinResponse(c, reqErr)
				return
			}
			lastErr = reqErr
			continue
		}

		if resp.StatusCode != http.StatusOK {
			errBody, _ := io.ReadAll(resp.Body)
			if kind := classifyHTTPFailure(resp.StatusCode, errBody); kind != "" {
				h.store.ReportRequestFailure(account, kind, time.Duration(durationMs)*time.Millisecond)
			}
			h.persistUsageAndSettleFromResponse(account, resp)
			resp.Body.Close()
			logUpstreamAttemptResult(c, "/v1/chat/completions", attempt+1, account, proxyURL, resp.StatusCode, durationMs, requestStartedAt, fmt.Sprintf("http_%d", resp.StatusCode))
			h.store.Release(account)
			h.store.UnbindSessionAffinity(affinityKey, account.ID())
			excludeAccounts[account.ID()] = true
			h.applyCooldown(account, resp.StatusCode, errBody, resp)
			lastStatusCode = resp.StatusCode
			lastBody = errBody
			if isRetryableStatus(resp.StatusCode, errBody) && attempt < maxRetries {
				continue
			}
			h.sendFinalUpstreamError(c, resp.StatusCode, errBody)
			return
		}

		h.store.BindSessionAffinity(affinityKey, account, proxyURL)
		account.Mu().RLock()
		c.Set("x-account-email", account.Email)
		account.Mu().RUnlock()
		c.Set("x-account-proxy", proxyURL)
		c.Set("x-model", model)
		c.Set("x-reasoning-effort", reasoningEffort)

		imagePayload, usage, imageCount, readErr := collectImagesResponse(resp.Body, "b64_json", model)
		logStatusCode := http.StatusOK
		firstTokenMs := 0
		if readErr != nil {
			logStatusCode = http.StatusBadGateway
			c.JSON(http.StatusBadGateway, gin.H{
				"error": gin.H{"message": readErr.Error(), "type": "upstream_error"},
			})
		} else {
			chunkID := "chatcmpl-" + strconv.FormatInt(time.Now().UnixNano(), 36)
			created := gjson.GetBytes(imagePayload, "created").Int()
			if isStream {
				firstTokenMs, readErr = writeChatCompletionsImageStream(c, model, chunkID, created, imagePayload, usage)
				if readErr != nil {
					logStatusCode = http.StatusBadGateway
				}
			} else {
				var out []byte
				out, readErr = buildChatCompletionsImageResponse(imagePayload, model, chunkID, created, usage)
				if readErr != nil {
					logStatusCode = http.StatusBadGateway
					c.JSON(http.StatusBadGateway, gin.H{
						"error": gin.H{"message": readErr.Error(), "type": "upstream_error"},
					})
				} else {
					c.Data(http.StatusOK, "application/json", out)
				}
			}
		}

		if readErr != nil && isStream {
			log.Printf("chat 图像兜底流写入失败: %v", readErr)
		}

		totalDurationMs := int(time.Since(requestStartedAt).Milliseconds())
		c.Set("x-first-token-ms", firstTokenMs)
		resolvedServiceTier := resolveServiceTier("", serviceTier)
		c.Set("x-service-tier", resolvedServiceTier)
		logInput := &database.UsageLogInput{
			AccountID:        account.ID(),
			Endpoint:         "/v1/chat/completions",
			Model:            model,
			StatusCode:       logStatusCode,
			DurationMs:       totalDurationMs,
			FirstTokenMs:     firstTokenMs,
			ReasoningEffort:  reasoningEffort,
			InboundEndpoint:  "/v1/chat/completions",
			UpstreamEndpoint: "/v1/responses",
			Stream:           isStream,
			ServiceTier:      resolvedServiceTier,
		}
		if usage != nil {
			logInput.PromptTokens = usage.PromptTokens
			logInput.CompletionTokens = usage.CompletionTokens
			logInput.TotalTokens = usage.TotalTokens
			logInput.InputTokens = usage.InputTokens
			logInput.OutputTokens = usage.OutputTokens
			logInput.ReasoningTokens = usage.ReasoningTokens
			logInput.CachedTokens = usage.CachedTokens
		}
		if imageCount > 0 && logInput.CompletionTokens == 0 {
			logInput.CompletionTokens = imageCount
			logInput.OutputTokens = imageCount
			logInput.TotalTokens = logInput.PromptTokens + imageCount
		}
		h.logUsageForRequest(c, logInput)

		failureDetail := ""
		if readErr != nil {
			failureDetail = readErr.Error()
		}
		logUpstreamAttemptResult(c, "/v1/chat/completions", attempt+1, account, proxyURL, logStatusCode, durationMs, requestStartedAt, failureDetail)
		h.persistUsageAndSettleFromResponse(account, resp)
		resp.Body.Close()
		if readErr != nil {
			h.store.UnbindSessionAffinity(affinityKey, account.ID())
			h.store.ReportRequestFailure(account, "transport", time.Duration(logInput.DurationMs)*time.Millisecond)
		} else {
			h.store.ReportRequestSuccess(account, time.Duration(logInput.DurationMs)*time.Millisecond)
		}
		h.store.Release(account)
		return
	}

	if lastErr != nil {
		c.JSON(http.StatusBadGateway, gin.H{
			"error": gin.H{"message": "上游请求失败: " + lastErr.Error(), "type": "upstream_error"},
		})
	} else if lastStatusCode != 0 {
		h.sendFinalUpstreamError(c, lastStatusCode, lastBody)
	}
}

func (h *Handler) forwardImagesRequest(c *gin.Context, inboundEndpoint, requestModel string, responsesBody []byte, responseFormat, streamPrefix string, stream bool) {
	requestStartedAt := requestStartTime(c)
	apiKey := requestAPIKeyFromHeaders(c.Request.Header)
	sessionID := ResolveSessionID(c.Request.Header, responsesBody)
	affinityKey := sessionAffinityKey(sessionID, apiKey)
	logRequestLifecycleStart(c, inboundEndpoint, requestModel, stream, "")
	maxRetries := h.getMaxRetries()
	var lastErr error
	var lastStatusCode int
	var lastBody []byte
	excludeAccounts := make(map[int64]bool)

	for attempt := 0; attempt <= maxRetries; attempt++ {
		attemptAcquireStartedAt := time.Now()
		account, stickyProxyURL := h.acquireAccountForRequestWithAffinity(c, affinityKey, excludeAccounts)
		attemptAcquireMs := int(time.Since(attemptAcquireStartedAt).Milliseconds())
		if account == nil {
			if lastStatusCode == http.StatusTooManyRequests && len(lastBody) > 0 {
				h.sendFinalUpstreamError(c, lastStatusCode, lastBody)
				return
			}
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": gin.H{"message": "无可用账号，请稍后重试", "type": "server_error"}})
			return
		}

		start := time.Now()
		proxyURL := strings.TrimSpace(stickyProxyURL)
		if proxyURL == "" {
			proxyURL = h.store.ResolveProxyForAccount(account)
		}
		deviceCfg := h.deviceCfg
		if deviceCfg == nil {
			deviceCfg = &DeviceProfileConfig{StabilizeDeviceProfile: false}
		}
		downstreamHeaders := c.Request.Header.Clone()
		useWebsocket := shouldUseWebsocketTransport(h.cfg, c.Request)
		logRequestDispatch(c, inboundEndpoint, attempt+1, account, proxyURL, requestModel, "", attemptAcquireMs)

		resp, attemptTrace, reqErr := ExecuteRequestTraced(c.Request.Context(), account, responsesBody, sessionID, proxyURL, apiKey, deviceCfg, downstreamHeaders, useWebsocket)
		durationMs := int(time.Since(start).Milliseconds())
		statusCode := 0
		if resp != nil {
			statusCode = resp.StatusCode
		}
		if attemptTrace != nil && !attemptTrace.HeaderAt.IsZero() {
			logUpstreamAttemptHeaders(c, inboundEndpoint, attempt+1, account, statusCode, attemptTrace, requestStartedAt)
		}
		if reqErr != nil {
			if kind := classifyTransportFailure(reqErr); kind != "" {
				h.store.ReportRequestFailure(account, kind, time.Duration(durationMs)*time.Millisecond)
			}
			logUpstreamAttemptResult(c, inboundEndpoint, attempt+1, account, proxyURL, 0, durationMs, requestStartedAt, reqErr.Error())
			h.store.Release(account)
			h.store.UnbindSessionAffinity(affinityKey, account.ID())
			excludeAccounts[account.ID()] = true
			if !IsRetryableError(reqErr) && classifyTransportFailure(reqErr) == "" {
				ErrorToGinResponse(c, reqErr)
				return
			}
			lastErr = reqErr
			continue
		}

		if resp.StatusCode != http.StatusOK {
			errBody, _ := io.ReadAll(resp.Body)
			if kind := classifyHTTPFailure(resp.StatusCode, errBody); kind != "" {
				h.store.ReportRequestFailure(account, kind, time.Duration(durationMs)*time.Millisecond)
			}
			h.persistUsageAndSettleFromResponse(account, resp)
			resp.Body.Close()
			logUpstreamAttemptResult(c, inboundEndpoint, attempt+1, account, proxyURL, resp.StatusCode, durationMs, requestStartedAt, fmt.Sprintf("http_%d", resp.StatusCode))
			h.store.Release(account)
			h.store.UnbindSessionAffinity(affinityKey, account.ID())
			excludeAccounts[account.ID()] = true
			h.applyCooldown(account, resp.StatusCode, errBody, resp)
			lastStatusCode = resp.StatusCode
			lastBody = errBody
			if isRetryableStatus(resp.StatusCode, errBody) && attempt < maxRetries {
				continue
			}
			h.sendFinalUpstreamError(c, resp.StatusCode, errBody)
			return
		}

		h.store.BindSessionAffinity(affinityKey, account, proxyURL)
		account.Mu().RLock()
		c.Set("x-account-email", account.Email)
		account.Mu().RUnlock()
		c.Set("x-account-proxy", proxyURL)
		c.Set("x-model", requestModel)

		var usage *UsageInfo
		var firstTokenMs int
		var imageCount int
		var readErr error
		if stream {
			usage, imageCount, firstTokenMs, readErr = h.streamImagesResponse(c, resp.Body, responseFormat, streamPrefix, requestModel, start)
		} else {
			var out []byte
			out, usage, imageCount, readErr = collectImagesResponse(resp.Body, responseFormat, requestModel)
			if readErr == nil {
				c.Data(http.StatusOK, "application/json", out)
			} else {
				c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{"message": readErr.Error(), "type": "upstream_error"}})
			}
		}

		logStatusCode := http.StatusOK
		if readErr != nil {
			logStatusCode = http.StatusBadGateway
		}
		totalDurationMs := int(time.Since(requestStartedAt).Milliseconds())
		logInput := &database.UsageLogInput{
			AccountID:        account.ID(),
			Endpoint:         inboundEndpoint,
			Model:            requestModel,
			StatusCode:       logStatusCode,
			DurationMs:       totalDurationMs,
			FirstTokenMs:     firstTokenMs,
			InboundEndpoint:  inboundEndpoint,
			UpstreamEndpoint: "/v1/responses",
			Stream:           stream,
		}
		if usage != nil {
			logInput.PromptTokens = usage.PromptTokens
			logInput.CompletionTokens = usage.CompletionTokens
			logInput.TotalTokens = usage.TotalTokens
			logInput.InputTokens = usage.InputTokens
			logInput.OutputTokens = usage.OutputTokens
			logInput.ReasoningTokens = usage.ReasoningTokens
			logInput.CachedTokens = usage.CachedTokens
		}
		if imageCount > 0 && logInput.CompletionTokens == 0 {
			logInput.CompletionTokens = imageCount
			logInput.OutputTokens = imageCount
			logInput.TotalTokens = logInput.PromptTokens + imageCount
		}
		h.logUsageForRequest(c, logInput)

		failureDetail := ""
		if readErr != nil {
			failureDetail = readErr.Error()
		}
		logUpstreamAttemptResult(c, inboundEndpoint, attempt+1, account, proxyURL, logStatusCode, durationMs, requestStartedAt, failureDetail)
		h.persistUsageAndSettleFromResponse(account, resp)
		resp.Body.Close()
		if readErr != nil {
			h.store.UnbindSessionAffinity(affinityKey, account.ID())
			h.store.ReportRequestFailure(account, "transport", time.Duration(logInput.DurationMs)*time.Millisecond)
		} else {
			h.store.ReportRequestSuccess(account, time.Duration(logInput.DurationMs)*time.Millisecond)
		}
		h.store.Release(account)
		return
	}

	if lastErr != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{"message": "上游请求失败: " + lastErr.Error(), "type": "upstream_error"}})
	} else if lastStatusCode != 0 {
		h.sendFinalUpstreamError(c, lastStatusCode, lastBody)
	}
}

func collectImagesResponse(body io.Reader, responseFormat, fallbackModel string) ([]byte, *UsageInfo, int, error) {
	var (
		out            []byte
		usage          *UsageInfo
		pendingResults []imageCallResult
		createdAt      int64
		firstMeta      = imageCallResult{Model: fallbackModel}
		readErr        error
	)
	err := ReadSSEStream(body, func(data []byte) bool {
		if meta, eventCreatedAt, ok := extractImageMetaFromLifecycleEvent(data); ok {
			mergeImageMeta(&firstMeta, meta)
			if eventCreatedAt > 0 {
				createdAt = eventCreatedAt
			}
		}
		switch gjson.GetBytes(data, "type").String() {
		case "response.output_item.done":
			if image, ok := extractImageFromOutputItemDone(data, fallbackModel); ok {
				mergeImageMeta(&image, firstMeta)
				pendingResults = append(pendingResults, image)
			}
		case "response.completed":
			results, completedAt, usageRaw, completedMeta, completedUsage, err := extractImagesFromResponsesCompleted(data, fallbackModel)
			if err != nil {
				readErr = err
				return false
			}
			if completedAt > 0 {
				createdAt = completedAt
			}
			mergeImageMeta(&firstMeta, completedMeta)
			if completedUsage != nil {
				usage = completedUsage
			}
			if len(results) == 0 {
				results = pendingResults
				if len(results) > 0 {
					firstMeta = results[0]
				}
			}
			if len(results) == 0 {
				readErr = fmt.Errorf("upstream did not return image output")
				return false
			}
			out, readErr = buildImagesAPIResponse(results, createdAt, usageRaw, firstMeta, responseFormat)
			return false
		case "response.failed":
			readErr = fmt.Errorf("upstream image generation failed")
			return false
		}
		return true
	})
	if err != nil {
		return nil, usage, 0, err
	}
	if readErr != nil {
		return nil, usage, 0, readErr
	}
	if len(out) == 0 {
		if len(pendingResults) > 0 {
			for i := range pendingResults {
				mergeImageMeta(&pendingResults[i], firstMeta)
			}
			out, readErr = buildImagesAPIResponse(pendingResults, createdAt, nil, firstMeta, responseFormat)
			if readErr != nil {
				return nil, usage, 0, readErr
			}
			return out, usage, len(gjson.GetBytes(out, "data").Array()), nil
		}
		return nil, usage, 0, fmt.Errorf("stream disconnected before image generation completed")
	}
	return out, usage, len(gjson.GetBytes(out, "data").Array()), nil
}

func (h *Handler) streamImagesResponse(c *gin.Context, body io.Reader, responseFormat, streamPrefix, fallbackModel string, start time.Time) (*UsageInfo, int, int, error) {
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")

	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		return nil, 0, 0, fmt.Errorf("streaming not supported")
	}

	var (
		usage          *UsageInfo
		firstTokenMs   int
		createdAt      int64
		streamMeta     = imageCallResult{Model: fallbackModel}
		pendingResults []imageCallResult
		imageCount     int
		readErr        error
	)
	writeEvent := func(eventName string, payload []byte) {
		if strings.TrimSpace(eventName) != "" {
			fmt.Fprintf(c.Writer, "event: %s\n", eventName)
		}
		fmt.Fprintf(c.Writer, "data: %s\n\n", payload)
		flusher.Flush()
	}

	err := ReadSSEStream(body, func(data []byte) bool {
		if firstTokenMs == 0 {
			firstTokenMs = int(time.Since(start).Milliseconds())
		}
		if meta, eventCreatedAt, ok := extractImageMetaFromLifecycleEvent(data); ok {
			mergeImageMeta(&streamMeta, meta)
			if eventCreatedAt > 0 {
				createdAt = eventCreatedAt
			}
		}
		switch gjson.GetBytes(data, "type").String() {
		case "response.image_generation_call.partial_image":
			b64 := strings.TrimSpace(gjson.GetBytes(data, "partial_image_b64").String())
			if b64 == "" {
				return true
			}
			partialMeta := streamMeta
			mergeImageMeta(&partialMeta, imageCallResult{
				OutputFormat: strings.TrimSpace(gjson.GetBytes(data, "output_format").String()),
				Background:   strings.TrimSpace(gjson.GetBytes(data, "background").String()),
			})
			eventName := streamPrefix + ".partial_image"
			writeEvent(eventName, buildImagesStreamPartialPayload(eventName, b64, gjson.GetBytes(data, "partial_image_index").Int(), responseFormat, createdAt, partialMeta))
		case "response.output_item.done":
			if image, ok := extractImageFromOutputItemDone(data, fallbackModel); ok {
				mergeImageMeta(&image, streamMeta)
				pendingResults = append(pendingResults, image)
			}
		case "response.completed":
			results, completedAt, usageRaw, firstMeta, completedUsage, err := extractImagesFromResponsesCompleted(data, fallbackModel)
			if err != nil {
				readErr = err
				writeEvent("error", buildImagesStreamErrorPayload(err.Error()))
				return false
			}
			if completedUsage != nil {
				usage = completedUsage
			}
			if completedAt > 0 {
				createdAt = completedAt
			}
			mergeImageMeta(&streamMeta, firstMeta)
			if len(results) == 0 {
				results = pendingResults
			}
			if len(results) == 0 {
				readErr = fmt.Errorf("upstream did not return image output")
				writeEvent("error", buildImagesStreamErrorPayload(readErr.Error()))
				return false
			}
			eventName := streamPrefix + ".completed"
			for _, image := range results {
				mergeImageMeta(&image, streamMeta)
				writeEvent(eventName, buildImagesStreamCompletedPayload(eventName, image, responseFormat, createdAt, usageRaw))
				imageCount++
			}
			return false
		case "response.failed":
			readErr = fmt.Errorf("upstream image generation failed")
			writeEvent("error", buildImagesStreamErrorPayload(readErr.Error()))
			return false
		}
		return true
	})
	if err != nil {
		return usage, imageCount, firstTokenMs, err
	}
	if imageCount == 0 && len(pendingResults) > 0 && readErr == nil {
		eventName := streamPrefix + ".completed"
		for _, image := range pendingResults {
			mergeImageMeta(&image, streamMeta)
			writeEvent(eventName, buildImagesStreamCompletedPayload(eventName, image, responseFormat, createdAt, nil))
			imageCount++
		}
	}
	if imageCount == 0 && readErr == nil {
		readErr = fmt.Errorf("stream disconnected before image generation completed")
		writeEvent("error", buildImagesStreamErrorPayload(readErr.Error()))
	}
	return usage, imageCount, firstTokenMs, readErr
}

func extractImagesFromResponsesCompleted(payload []byte, fallbackModel string) ([]imageCallResult, int64, []byte, imageCallResult, *UsageInfo, error) {
	if gjson.GetBytes(payload, "type").String() != "response.completed" {
		return nil, 0, nil, imageCallResult{}, nil, fmt.Errorf("unexpected event type")
	}

	createdAt := gjson.GetBytes(payload, "response.created_at").Int()
	if createdAt <= 0 {
		createdAt = time.Now().Unix()
	}

	results := make([]imageCallResult, 0)
	firstMeta := imageCallResult{Model: fallbackModel}
	if meta, _, ok := extractImageMetaFromLifecycleEvent(payload); ok {
		mergeImageMeta(&firstMeta, meta)
	}
	if output := gjson.GetBytes(payload, "response.output"); output.IsArray() {
		for _, item := range output.Array() {
			if item.Get("type").String() != "image_generation_call" {
				continue
			}
			result := strings.TrimSpace(item.Get("result").String())
			if result == "" {
				continue
			}
			image := imageCallResult{
				Result:        result,
				RevisedPrompt: strings.TrimSpace(item.Get("revised_prompt").String()),
				OutputFormat:  strings.TrimSpace(item.Get("output_format").String()),
				Size:          strings.TrimSpace(item.Get("size").String()),
				Background:    strings.TrimSpace(item.Get("background").String()),
				Quality:       strings.TrimSpace(item.Get("quality").String()),
				Model:         fallbackModel,
			}
			mergeImageMeta(&image, firstMeta)
			if len(results) == 0 {
				firstMeta = image
			}
			results = append(results, image)
		}
	}

	var usageRaw []byte
	if usage := gjson.GetBytes(payload, "response.tool_usage.image_gen"); usage.Exists() && usage.IsObject() {
		usageRaw = []byte(usage.Raw)
	}
	return results, createdAt, usageRaw, firstMeta, extractUsageFromResult(gjson.GetBytes(payload, "response.usage")), nil
}

func extractImageFromOutputItemDone(payload []byte, fallbackModel string) (imageCallResult, bool) {
	if gjson.GetBytes(payload, "type").String() != "response.output_item.done" {
		return imageCallResult{}, false
	}
	item := gjson.GetBytes(payload, "item")
	if !item.Exists() || item.Get("type").String() != "image_generation_call" {
		return imageCallResult{}, false
	}
	result := strings.TrimSpace(item.Get("result").String())
	if result == "" {
		return imageCallResult{}, false
	}
	return imageCallResult{
		Result:        result,
		RevisedPrompt: strings.TrimSpace(item.Get("revised_prompt").String()),
		OutputFormat:  strings.TrimSpace(item.Get("output_format").String()),
		Size:          strings.TrimSpace(item.Get("size").String()),
		Background:    strings.TrimSpace(item.Get("background").String()),
		Quality:       strings.TrimSpace(item.Get("quality").String()),
		Model:         fallbackModel,
	}, true
}

func extractImageMetaFromLifecycleEvent(payload []byte) (imageCallResult, int64, bool) {
	response := gjson.GetBytes(payload, "response")
	if !response.Exists() {
		return imageCallResult{}, 0, false
	}
	meta := imageCallResult{
		OutputFormat: strings.TrimSpace(response.Get("tools.0.output_format").String()),
		Size:         strings.TrimSpace(response.Get("tools.0.size").String()),
		Background:   strings.TrimSpace(response.Get("tools.0.background").String()),
		Quality:      strings.TrimSpace(response.Get("tools.0.quality").String()),
		Model:        strings.TrimSpace(response.Get("tools.0.model").String()),
	}
	return meta, response.Get("created_at").Int(), true
}

func mergeImageMeta(target *imageCallResult, source imageCallResult) {
	if target == nil {
		return
	}
	if target.OutputFormat == "" {
		target.OutputFormat = source.OutputFormat
	}
	if target.Size == "" {
		target.Size = source.Size
	}
	if target.Background == "" {
		target.Background = source.Background
	}
	if target.Quality == "" {
		target.Quality = source.Quality
	}
	if target.Model == "" {
		target.Model = source.Model
	}
}

func buildImagesAPIResponse(results []imageCallResult, createdAt int64, usageRaw []byte, firstMeta imageCallResult, responseFormat string) ([]byte, error) {
	if createdAt <= 0 {
		createdAt = time.Now().Unix()
	}
	out := []byte(`{"created":0,"data":[]}`)
	out, _ = sjson.SetBytes(out, "created", createdAt)

	format := strings.ToLower(strings.TrimSpace(responseFormat))
	if format == "" {
		format = "b64_json"
	}
	for _, image := range results {
		item := []byte(`{}`)
		if format == "url" {
			item, _ = sjson.SetBytes(item, "url", "data:"+mimeTypeFromOutputFormat(image.OutputFormat)+";base64,"+image.Result)
		} else {
			item, _ = sjson.SetBytes(item, "b64_json", image.Result)
		}
		if image.RevisedPrompt != "" {
			item, _ = sjson.SetBytes(item, "revised_prompt", image.RevisedPrompt)
		}
		out, _ = sjson.SetRawBytes(out, "data.-1", item)
	}
	if firstMeta.Background != "" {
		out, _ = sjson.SetBytes(out, "background", firstMeta.Background)
	}
	if firstMeta.OutputFormat != "" {
		out, _ = sjson.SetBytes(out, "output_format", firstMeta.OutputFormat)
	}
	if firstMeta.Quality != "" {
		out, _ = sjson.SetBytes(out, "quality", firstMeta.Quality)
	}
	if firstMeta.Size != "" {
		out, _ = sjson.SetBytes(out, "size", firstMeta.Size)
	}
	if firstMeta.Model != "" {
		out, _ = sjson.SetBytes(out, "model", firstMeta.Model)
	}
	if len(usageRaw) > 0 && json.Valid(usageRaw) {
		out, _ = sjson.SetRawBytes(out, "usage", usageRaw)
	}
	return out, nil
}

func buildImagesStreamPartialPayload(eventType, b64 string, partialImageIndex int64, responseFormat string, createdAt int64, meta imageCallResult) []byte {
	if createdAt <= 0 {
		createdAt = time.Now().Unix()
	}
	payload := []byte(`{"type":"","created_at":0,"partial_image_index":0,"b64_json":""}`)
	payload, _ = sjson.SetBytes(payload, "type", eventType)
	payload, _ = sjson.SetBytes(payload, "created_at", createdAt)
	payload, _ = sjson.SetBytes(payload, "partial_image_index", partialImageIndex)
	payload, _ = sjson.SetBytes(payload, "b64_json", b64)
	if strings.EqualFold(strings.TrimSpace(responseFormat), "url") {
		payload, _ = sjson.SetBytes(payload, "url", "data:"+mimeTypeFromOutputFormat(meta.OutputFormat)+";base64,"+b64)
	}
	return addImageMetaToPayload(payload, meta)
}

func buildImagesStreamCompletedPayload(eventType string, image imageCallResult, responseFormat string, createdAt int64, usageRaw []byte) []byte {
	if createdAt <= 0 {
		createdAt = time.Now().Unix()
	}
	payload := []byte(`{"type":"","created_at":0,"b64_json":""}`)
	payload, _ = sjson.SetBytes(payload, "type", eventType)
	payload, _ = sjson.SetBytes(payload, "created_at", createdAt)
	payload, _ = sjson.SetBytes(payload, "b64_json", image.Result)
	if strings.EqualFold(strings.TrimSpace(responseFormat), "url") {
		payload, _ = sjson.SetBytes(payload, "url", "data:"+mimeTypeFromOutputFormat(image.OutputFormat)+";base64,"+image.Result)
	}
	payload = addImageMetaToPayload(payload, image)
	if len(usageRaw) > 0 && json.Valid(usageRaw) {
		payload, _ = sjson.SetRawBytes(payload, "usage", usageRaw)
	}
	return payload
}

func addImageMetaToPayload(payload []byte, meta imageCallResult) []byte {
	if meta.Background != "" {
		payload, _ = sjson.SetBytes(payload, "background", meta.Background)
	}
	if meta.OutputFormat != "" {
		payload, _ = sjson.SetBytes(payload, "output_format", meta.OutputFormat)
	}
	if meta.Quality != "" {
		payload, _ = sjson.SetBytes(payload, "quality", meta.Quality)
	}
	if meta.Size != "" {
		payload, _ = sjson.SetBytes(payload, "size", meta.Size)
	}
	if meta.Model != "" {
		payload, _ = sjson.SetBytes(payload, "model", meta.Model)
	}
	return payload
}

func buildImagesStreamErrorPayload(message string) []byte {
	payload := []byte(`{"error":{"message":"","type":"upstream_error"}}`)
	payload, _ = sjson.SetBytes(payload, "error.message", message)
	return payload
}
