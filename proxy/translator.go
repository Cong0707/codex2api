package proxy

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const maxTools = 128

// ==================== 请求翻译: OpenAI Chat Completions → Codex Responses ====================

// TranslateRequest 将 OpenAI Chat Completions 请求转换为 Codex Responses 格式
func TranslateRequest(rawJSON []byte) ([]byte, error) {
	result := rawJSON

	// 1. 转换 messages → input
	messages := gjson.GetBytes(result, "messages")
	if messages.Exists() && messages.IsArray() {
		input := convertMessagesToInput(messages)
		result, _ = sjson.SetRawBytes(result, "input", input)
		result, _ = sjson.DeleteBytes(result, "messages")
	}

	// 2. 强制设置 Codex 必需字段
	result, _ = sjson.SetBytes(result, "stream", true)
	result, _ = sjson.SetBytes(result, "store", false)

	// 3. 将 reasoning_effort 转换为 Codex 的 reasoning.effort
	if re := gjson.GetBytes(result, "reasoning_effort"); re.Exists() && !gjson.GetBytes(result, "reasoning.effort").Exists() {
		result, _ = sjson.SetBytes(result, "reasoning.effort", re.String())
	}
	result = clampReasoningEffort(result)

	// 4. 统一 service tier 字段命名，保留给上游用于 fast 调度
	result = normalizeServiceTierField(result)
	result = sanitizeServiceTierForUpstream(result)

	// 5. 删除 Codex 不支持的字段
	unsupportedFields := []string{
		"max_tokens", "max_completion_tokens", "temperature", "top_p",
		"frequency_penalty", "presence_penalty", "logprobs", "top_logprobs",
		"n", "seed", "stop", "user", "logit_bias", "response_format",
		"serviceTier", "stream_options", "truncation",
		"context_management", "disable_response_storage", "verbosity",
		"reasoning_effort",
	}
	for _, field := range unsupportedFields {
		result, _ = sjson.DeleteBytes(result, field)
	}

	// 6. 转换 tools 格式: OpenAI Chat {type, function:{name,description,parameters}} → Codex {type, name, description, parameters}
	result = convertToolsFormat(result)
	// 清理 function tool parameters 中上游不支持的 JSON Schema 关键字
	result = sanitizeToolSchemas(result)

	// 7. 删除 Codex 不支持的 tool 相关字段
	result, _ = sjson.DeleteBytes(result, "tool_choice")

	// 8. system → developer 角色转换
	result = convertSystemRoleToDeveloper(result)

	// 9. 添加 include
	result, _ = sjson.SetBytes(result, "include", []string{"reasoning.encrypted_content"})

	return result, nil
}

// clampReasoningEffort 将 reasoning.effort 钳位到上游支持的值（low/medium/high/xhigh）
// 不支持的值映射到最接近的合法值
func clampReasoningEffort(body []byte) []byte {
	effort := gjson.GetBytes(body, "reasoning.effort").String()
	if effort == "" {
		return body
	}
	switch strings.ToLower(effort) {
	case "low", "medium", "high", "xhigh":
		return body // 合法值，不修改
	default:
		// very_high 等超出范围的值 → 钳位到 high
		body, _ = sjson.SetBytes(body, "reasoning.effort", "high")
		return body
	}
}

func normalizeServiceTierField(body []byte) []byte {
	tier := strings.TrimSpace(gjson.GetBytes(body, "service_tier").String())
	if tier == "" {
		tier = strings.TrimSpace(gjson.GetBytes(body, "serviceTier").String())
	}
	if tier == "" {
		return body
	}

	body, _ = sjson.SetBytes(body, "service_tier", tier)
	body, _ = sjson.DeleteBytes(body, "serviceTier")
	return body
}

func sanitizeServiceTierForUpstream(body []byte) []byte {
	tier := strings.TrimSpace(gjson.GetBytes(body, "service_tier").String())
	if tier == "" {
		body, _ = sjson.DeleteBytes(body, "serviceTier")
		return body
	}

	switch tier {
	case "auto", "default", "flex", "priority", "scale", "fast":
		if tier == "fast" {
			body, _ = sjson.SetBytes(body, "service_tier", "priority")
		}
		body, _ = sjson.DeleteBytes(body, "serviceTier")
		return body
	default:
		body, _ = sjson.DeleteBytes(body, "service_tier")
		body, _ = sjson.DeleteBytes(body, "serviceTier")
		return body
	}
}

func resolveServiceTier(actualTier, requestedTier string) string {
	requestedTier = strings.TrimSpace(requestedTier)
	if requestedTier == "fast" {
		return requestedTier
	}

	actualTier = strings.TrimSpace(actualTier)
	if actualTier != "" {
		return actualTier
	}
	return requestedTier
}

// convertMessagesToInput 将 OpenAI messages 格式转换为 Codex input 格式
func convertMessagesToInput(messages gjson.Result) []byte {
	var items []string

	messages.ForEach(func(_, msg gjson.Result) bool {
		role := msg.Get("role").String()
		content := msg.Get("content")

		// tool 角色: 转换为 function_call_output
		if role == "tool" {
			callID := msg.Get("tool_call_id").String()
			output := content.String()
			item := fmt.Sprintf(`{"type":"function_call_output","call_id":%s,"output":%s}`,
				escapeJSON(callID), escapeJSON(output))
			items = append(items, item)
			return true
		}

		// assistant 消息带 tool_calls: 转换为 function_call 项
		if role == "assistant" {
			toolCalls := msg.Get("tool_calls")
			if toolCalls.Exists() && toolCalls.IsArray() {
				// 如有非空文本内容，先输出 assistant message
				if content.Type == gjson.String && content.String() != "" {
					item := fmt.Sprintf(`{"type":"message","role":"assistant","content":[{"type":"output_text","text":%s}]}`,
						escapeJSON(content.String()))
					items = append(items, item)
				}
				// 每个 tool_call 生成一个 function_call 项
				toolCalls.ForEach(func(_, tc gjson.Result) bool {
					callID := tc.Get("id").String()
					name := tc.Get("function.name").String()
					arguments := tc.Get("function.arguments").String()
					item := fmt.Sprintf(`{"type":"function_call","call_id":%s,"name":%s,"arguments":%s}`,
						escapeJSON(callID), escapeJSON(name), escapeJSON(arguments))
					items = append(items, item)
					return true
				})
				return true
			}
		}

		// 角色映射
		switch role {
		case "system":
			role = "developer"
		case "assistant":
			role = "assistant"
		default:
			role = "user"
		}

		// assistant 用 output_text，其他角色用 input_text
		contentType := "input_text"
		if role == "assistant" {
			contentType = "output_text"
		}

		if content.Type == gjson.String {
			// 简单文本内容
			item := fmt.Sprintf(`{"type":"message","role":"%s","content":[{"type":"%s","text":%s}]}`,
				role, contentType, escapeJSON(content.String()))
			items = append(items, item)
		} else if content.IsArray() {
			// 多部分内容（text / image_url 等）
			var parts []string
			content.ForEach(func(_, part gjson.Result) bool {
				partType := part.Get("type").String()
				switch partType {
				case "text":
					text := part.Get("text").String()
					parts = append(parts, fmt.Sprintf(`{"type":"%s","text":%s}`, contentType, escapeJSON(text)))
				case "image_url":
					imgURL := part.Get("image_url.url").String()
					parts = append(parts, fmt.Sprintf(`{"type":"input_image","image_url":"%s"}`, imgURL))
				}
				return true
			})
			if len(parts) > 0 {
				item := fmt.Sprintf(`{"type":"message","role":"%s","content":[%s]}`,
					role, strings.Join(parts, ","))
				items = append(items, item)
			}
		}
		return true
	})

	return []byte("[" + strings.Join(items, ",") + "]")
}

// convertSystemRoleToDeveloper 将 input 中的 system 角色转为 developer
func convertSystemRoleToDeveloper(rawJSON []byte) []byte {
	inputResult := gjson.GetBytes(rawJSON, "input")
	if !inputResult.IsArray() {
		return rawJSON
	}

	result := rawJSON
	for i := 0; i < int(inputResult.Get("#").Int()); i++ {
		rolePath := fmt.Sprintf("input.%d.role", i)
		if gjson.GetBytes(result, rolePath).String() == "system" {
			result, _ = sjson.SetBytes(result, rolePath, "developer")
		}
	}
	return result
}

// convertToolsFormat 将 OpenAI Chat 格式的 tools 转换为 Codex Responses 格式
// OpenAI: {type:"function", function:{name, description, parameters}}
// Codex:  {type:"function", name, description, parameters}
func convertToolsFormat(rawJSON []byte) []byte {
	tools := gjson.GetBytes(rawJSON, "tools")
	if !tools.Exists() || !tools.IsArray() {
		return rawJSON
	}

	result := rawJSON
	for i := 0; i < int(tools.Get("#").Int()); i++ {
		funcObj := gjson.GetBytes(result, fmt.Sprintf("tools.%d.function", i))
		if !funcObj.Exists() {
			continue
		}

		// 提升 function 下的字段到顶层
		if name := funcObj.Get("name"); name.Exists() {
			result, _ = sjson.SetBytes(result, fmt.Sprintf("tools.%d.name", i), name.String())
		}
		if desc := funcObj.Get("description"); desc.Exists() {
			result, _ = sjson.SetBytes(result, fmt.Sprintf("tools.%d.description", i), desc.String())
		}
		if params := funcObj.Get("parameters"); params.Exists() {
			result, _ = sjson.SetRawBytes(result, fmt.Sprintf("tools.%d.parameters", i), []byte(params.Raw))
		}
		if strict := funcObj.Get("strict"); strict.Exists() {
			result, _ = sjson.SetBytes(result, fmt.Sprintf("tools.%d.strict", i), strict.Bool())
		}

		// 删除嵌套的 function 对象
		result, _ = sjson.DeleteBytes(result, fmt.Sprintf("tools.%d.function", i))
	}

	return result
}

// ensureToolDescriptions 为缺少 description 的客户端执行工具补充默认描述
// 例如 tool_search 类型要求必须有 description，否则上游会返回 400
func ensureToolDescriptions(rawJSON []byte) []byte {
	tools := gjson.GetBytes(rawJSON, "tools")
	if !tools.Exists() || !tools.IsArray() {
		return rawJSON
	}

	// 客户端执行工具类型 → 默认描述
	defaults := map[string]string{
		"tool_search": "Search through available tools to find the most relevant one for the task.",
	}

	result := rawJSON
	for i := 0; i < int(tools.Get("#").Int()); i++ {
		toolType := gjson.GetBytes(result, fmt.Sprintf("tools.%d.type", i)).String()
		desc := gjson.GetBytes(result, fmt.Sprintf("tools.%d.description", i))
		if defaultDesc, ok := defaults[toolType]; ok && (!desc.Exists() || desc.String() == "") {
			result, _ = sjson.SetBytes(result, fmt.Sprintf("tools.%d.description", i), defaultDesc)
		}
	}

	return result
}

// sanitizeToolSchemas 递归清理 function tool parameters 中上游不支持的 JSON Schema 关键字
// 例如 uniqueItems、minItems、maxItems、pattern 等验证约束会导致 Codex API 返回 400
func sanitizeToolSchemas(rawJSON []byte) []byte {
	tools := gjson.GetBytes(rawJSON, "tools")
	if !tools.Exists() || !tools.IsArray() {
		return rawJSON
	}

	result := rawJSON
	if toolCount := int(tools.Get("#").Int()); toolCount > maxTools {
		result, _ = sjson.SetRawBytes(result, "tools", []byte(tools.Raw))
		var rawTools []json.RawMessage
		if err := json.Unmarshal([]byte(tools.Raw), &rawTools); err == nil && len(rawTools) > maxTools {
			rawTools = rawTools[:maxTools]
			if limited, err := json.Marshal(rawTools); err == nil {
				result, _ = sjson.SetRawBytes(result, "tools", limited)
			}
		}
	}
	tools = gjson.GetBytes(result, "tools")
	for i := 0; i < int(tools.Get("#").Int()); i++ {
		params := gjson.GetBytes(result, fmt.Sprintf("tools.%d.parameters", i))
		if !params.Exists() || params.Type != gjson.JSON {
			continue
		}

		var schema map[string]interface{}
		if err := json.Unmarshal([]byte(params.Raw), &schema); err != nil {
			continue
		}

		sanitizeSchemaForUpstream(schema)

		sanitized, err := json.Marshal(schema)
		if err != nil {
			continue
		}
		result, _ = sjson.SetRawBytes(result, fmt.Sprintf("tools.%d.parameters", i), sanitized)
	}

	return result
}

// 上游不支持的 JSON Schema 验证约束关键字（仅影响校验，不影响结构）
var unsupportedSchemaKeys = map[string]bool{
	"uniqueItems":      true,
	"minItems":         true,
	"maxItems":         true,
	"minimum":          true,
	"maximum":          true,
	"exclusiveMinimum": true,
	"exclusiveMaximum": true,
	"multipleOf":       true,
	"pattern":          true,
	"minLength":        true,
	"maxLength":        true,
	"format":           true,
	"minProperties":    true,
	"maxProperties":    true,
}

// stripUnsupportedSchemaKeys 递归删除 schema 中上游不支持的关键字
func stripUnsupportedSchemaKeys(schema map[string]interface{}) {
	for key := range unsupportedSchemaKeys {
		delete(schema, key)
	}

	// 递归处理 properties
	if props, ok := schema["properties"].(map[string]interface{}); ok {
		for _, v := range props {
			if sub, ok := v.(map[string]interface{}); ok {
				stripUnsupportedSchemaKeys(sub)
			}
		}
	}

	// 递归处理 items
	if items, ok := schema["items"].(map[string]interface{}); ok {
		stripUnsupportedSchemaKeys(items)
	}

	// 递归处理 allOf / anyOf / oneOf
	for _, key := range []string{"allOf", "anyOf", "oneOf"} {
		if arr, ok := schema[key].([]interface{}); ok {
			for _, item := range arr {
				if sub, ok := item.(map[string]interface{}); ok {
					stripUnsupportedSchemaKeys(sub)
				}
			}
		}
	}

	// 递归处理 additionalProperties（为 object 时）
	if addProps, ok := schema["additionalProperties"].(map[string]interface{}); ok {
		stripUnsupportedSchemaKeys(addProps)
	}

	// 递归处理 $defs
	if defs, ok := schema["$defs"].(map[string]interface{}); ok {
		for _, v := range defs {
			if sub, ok := v.(map[string]interface{}); ok {
				stripUnsupportedSchemaKeys(sub)
			}
		}
	}
}

func sanitizeSchemaForUpstream(schema map[string]interface{}) {
	stripUnsupportedSchemaKeys(schema)
	ensureArrayItems(schema)
}

func ensureArrayItems(schema map[string]interface{}) {
	if schemaDeclaresArray(schema) {
		if _, ok := schema["items"]; !ok {
			schema["items"] = map[string]interface{}{}
		}
	}
	if props, ok := schema["properties"].(map[string]interface{}); ok {
		for _, v := range props {
			if sub, ok := v.(map[string]interface{}); ok {
				ensureArrayItems(sub)
			}
		}
	}
	if items, ok := schema["items"].(map[string]interface{}); ok {
		ensureArrayItems(items)
	}
	for _, key := range []string{"allOf", "anyOf", "oneOf"} {
		if arr, ok := schema[key].([]interface{}); ok {
			for _, item := range arr {
				if sub, ok := item.(map[string]interface{}); ok {
					ensureArrayItems(sub)
				}
			}
		}
	}
	if addProps, ok := schema["additionalProperties"].(map[string]interface{}); ok {
		ensureArrayItems(addProps)
	}
	if defs, ok := schema["$defs"].(map[string]interface{}); ok {
		for _, v := range defs {
			if sub, ok := v.(map[string]interface{}); ok {
				ensureArrayItems(sub)
			}
		}
	}
}

func schemaDeclaresArray(schema map[string]interface{}) bool {
	switch t := schema["type"].(type) {
	case string:
		return t == "array"
	case []interface{}:
		for _, item := range t {
			if s, ok := item.(string); ok && s == "array" {
				return true
			}
		}
	}
	return false
}

// ==================== 响应翻译: Codex SSE → OpenAI SSE ====================

// TranslateStreamChunk 将 Codex SSE 数据块翻译为 OpenAI Chat Completions 流式格式
func TranslateStreamChunk(eventData []byte, model string, chunkID string) ([]byte, bool) {
	eventType := gjson.GetBytes(eventData, "type").String()

	switch eventType {
	case "response.output_text.delta":
		delta := gjson.GetBytes(eventData, "delta").String()
		return buildOpenAIChunk(chunkID, model, delta, "", ""), false

	case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		delta := gjson.GetBytes(eventData, "delta").String()
		return buildOpenAIChunk(chunkID, model, "", delta, ""), false

	case "response.content_part.done":
		// 内容部分完成，不需要翻译
		return nil, false

	case "response.output_item.done":
		// 输出项完成
		return nil, false

	case "response.completed":
		// 生成完成，发送 [DONE]
		usage := extractUsage(eventData)
		chunk := buildOpenAIFinalChunk(chunkID, model, usage)
		return chunk, true

	case "response.failed":
		errMsg := gjson.GetBytes(eventData, "response.error.message").String()
		if errMsg == "" {
			errMsg = "Codex upstream error"
		}
		return buildOpenAIError(errMsg), true

	case "response.created", "response.in_progress",
		"response.output_item.added", "response.content_part.added",
		"response.reasoning_summary_text.done",
		"response.reasoning.encrypted_content.delta", "response.reasoning.encrypted_content.done",
		"response.reasoning_summary_part.added", "response.reasoning_summary_part.done":
		// 这些事件不需要转发给下游
		return nil, false

	default:
		// 未知事件类型，尝试提取 delta
		if delta := gjson.GetBytes(eventData, "delta"); delta.Exists() && delta.Type == gjson.String {
			return buildOpenAIChunk(chunkID, model, delta.String(), "", ""), false
		}
		return nil, false
	}
}

// UsageInfo token 使用统计
type UsageInfo struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
	InputTokens      int `json:"input_tokens"`
	OutputTokens     int `json:"output_tokens"`
	ReasoningTokens  int `json:"reasoning_tokens"`
	CachedTokens     int `json:"cached_tokens"`
}

// extractUsage 从 response.completed 事件提取 usage
func extractUsage(eventData []byte) *UsageInfo {
	usage := gjson.GetBytes(eventData, "response.usage")
	if !usage.Exists() {
		return nil
	}
	return extractUsageFromResult(usage)
}

func extractUsageFromResult(usage gjson.Result) *UsageInfo {
	if !usage.Exists() {
		return nil
	}
	inputTokens := int(usage.Get("input_tokens").Int())
	outputTokens := int(usage.Get("output_tokens").Int())
	reasoningTokens := int(usage.Get("output_tokens_details.reasoning_tokens").Int())
	cachedTokens := int(usage.Get("input_tokens_details.cached_tokens").Int())
	return &UsageInfo{
		PromptTokens:     inputTokens,
		CompletionTokens: outputTokens,
		TotalTokens:      inputTokens + outputTokens,
		InputTokens:      inputTokens,
		OutputTokens:     outputTokens,
		ReasoningTokens:  reasoningTokens,
		CachedTokens:     cachedTokens,
	}
}

// buildOpenAIChunk 构建 OpenAI 流式响应块
func buildOpenAIChunk(id, model, content, reasoningContent, finishReason string) []byte {
	chunk := []byte(`{}`)
	chunk, _ = sjson.SetBytes(chunk, "id", id)
	chunk, _ = sjson.SetBytes(chunk, "object", "chat.completion.chunk")
	chunk, _ = sjson.SetBytes(chunk, "created", 0) // 由调用方填充
	chunk, _ = sjson.SetBytes(chunk, "model", model)

	if content != "" || reasoningContent != "" {
		chunk, _ = sjson.SetBytes(chunk, "choices.0.index", 0)
		if content != "" {
			chunk, _ = sjson.SetBytes(chunk, "choices.0.delta.content", content)
		}
		if reasoningContent != "" {
			chunk, _ = sjson.SetBytes(chunk, "choices.0.delta.reasoning_content", reasoningContent)
		}
	} else if finishReason == "" {
		// 确保存在 delta 对象（即便是空的），符合 OpenAI 规范
		chunk, _ = sjson.SetBytes(chunk, "choices.0.index", 0)
		chunk, _ = sjson.SetRawBytes(chunk, "choices.0.delta", []byte(`{}`))
	}

	if finishReason != "" {
		chunk, _ = sjson.SetBytes(chunk, "choices.0.index", 0)
		chunk, _ = sjson.SetBytes(chunk, "choices.0.finish_reason", finishReason)
	} else {
		chunk, _ = sjson.SetRawBytes(chunk, "choices.0.finish_reason", []byte("null"))
	}

	return chunk
}

// buildOpenAIFinalChunk 构建最终的 OpenAI 流式响应块（包含 usage）
func buildOpenAIFinalChunk(id, model string, usage *UsageInfo) []byte {
	chunk := buildOpenAIChunk(id, model, "", "", "stop")
	if usage != nil {
		chunk, _ = sjson.SetBytes(chunk, "usage.prompt_tokens", usage.PromptTokens)
		chunk, _ = sjson.SetBytes(chunk, "usage.completion_tokens", usage.CompletionTokens)
		chunk, _ = sjson.SetBytes(chunk, "usage.total_tokens", usage.TotalTokens)
	}
	return chunk
}

// buildOpenAIError 构建错误响应
func buildOpenAIError(message string) []byte {
	result := []byte(`{}`)
	result, _ = sjson.SetBytes(result, "error.message", message)
	result, _ = sjson.SetBytes(result, "error.type", "upstream_error")
	return result
}

// TranslateCompactResponse 将 Codex 非流式响应转换为 OpenAI 格式
func TranslateCompactResponse(responseData []byte, model string, id string) []byte {
	// 提取输出文本
	var outputText string
	output := gjson.GetBytes(responseData, "output")
	if output.IsArray() {
		output.ForEach(func(_, item gjson.Result) bool {
			if item.Get("type").String() == "message" {
				content := item.Get("content")
				if content.IsArray() {
					content.ForEach(func(_, part gjson.Result) bool {
						if part.Get("type").String() == "output_text" {
							outputText += part.Get("text").String()
						}
						return true
					})
				}
			}
			return true
		})
	}

	// 构建 OpenAI 非流式响应
	result := []byte(`{}`)
	result, _ = sjson.SetBytes(result, "id", id)
	result, _ = sjson.SetBytes(result, "object", "chat.completion")
	result, _ = sjson.SetBytes(result, "model", model)
	result, _ = sjson.SetBytes(result, "choices.0.index", 0)
	result, _ = sjson.SetBytes(result, "choices.0.message.role", "assistant")
	result, _ = sjson.SetBytes(result, "choices.0.message.content", outputText)
	result, _ = sjson.SetBytes(result, "choices.0.finish_reason", "stop")

	// 提取 usage
	usage := extractUsage(responseData)
	if usage == nil {
		usage = gjson.GetBytes(responseData, "usage").Value().(*UsageInfo)
	}
	if usage != nil {
		result, _ = sjson.SetBytes(result, "usage.prompt_tokens", usage.PromptTokens)
		result, _ = sjson.SetBytes(result, "usage.completion_tokens", usage.CompletionTokens)
		result, _ = sjson.SetBytes(result, "usage.total_tokens", usage.TotalTokens)
	}

	return result
}

// ==================== 有状态流式转换器（支持 Function Calling） ====================

// ToolCallResult 表示一个完整的工具调用结果（用于非流式收集）
type ToolCallResult struct {
	ID        string
	Name      string
	Arguments string
}

// StreamTranslator 有状态的流式响应翻译器，跟踪 function_call 索引映射
type StreamTranslator struct {
	Model        string
	ChunkID      string
	HasToolCalls bool
	toolCallMap  map[string]int // Codex item.id → OpenAI tool_calls index
	nextIdx      int
}

// NewStreamTranslator 创建流式翻译器实例
func NewStreamTranslator(chunkID, model string) *StreamTranslator {
	return &StreamTranslator{
		Model:       model,
		ChunkID:     chunkID,
		toolCallMap: make(map[string]int),
	}
}

// Translate 将单个 Codex SSE 事件翻译为 OpenAI Chat Completions 流式格式
func (st *StreamTranslator) Translate(eventData []byte) ([]byte, bool) {
	eventType := gjson.GetBytes(eventData, "type").String()

	switch eventType {
	case "response.output_text.delta":
		delta := gjson.GetBytes(eventData, "delta").String()
		return buildOpenAIChunk(st.ChunkID, st.Model, delta, "", ""), false

	case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		delta := gjson.GetBytes(eventData, "delta").String()
		return buildOpenAIChunk(st.ChunkID, st.Model, "", delta, ""), false

	case "response.output_item.added":
		// 检查是否为 function_call 类型的输出项
		itemType := gjson.GetBytes(eventData, "item.type").String()
		if itemType == "function_call" {
			itemID := gjson.GetBytes(eventData, "item.id").String()
			callID := gjson.GetBytes(eventData, "item.call_id").String()
			name := gjson.GetBytes(eventData, "item.name").String()

			tcIdx := st.nextIdx
			st.toolCallMap[itemID] = tcIdx
			st.nextIdx++
			st.HasToolCalls = true

			return buildOpenAIToolCallChunk(st.ChunkID, st.Model, tcIdx, callID, name), false
		}
		return nil, false

	case "response.function_call_arguments.delta":
		itemID := gjson.GetBytes(eventData, "item_id").String()
		tcIdx, ok := st.toolCallMap[itemID]
		if !ok {
			return nil, false
		}
		delta := gjson.GetBytes(eventData, "delta").String()
		return buildOpenAIToolCallDeltaChunk(st.ChunkID, st.Model, tcIdx, delta), false

	case "response.function_call_arguments.done":
		// 参数已通过 delta 发送完毕，忽略
		return nil, false

	case "response.content_part.done":
		return nil, false

	case "response.output_item.done":
		return nil, false

	case "response.completed":
		usage := extractUsage(eventData)
		finishReason := "stop"
		if st.HasToolCalls {
			finishReason = "tool_calls"
		}
		chunk := buildOpenAIFinalChunkWithReason(st.ChunkID, st.Model, usage, finishReason)
		return chunk, true

	case "response.failed":
		errMsg := gjson.GetBytes(eventData, "response.error.message").String()
		if errMsg == "" {
			errMsg = "Codex upstream error"
		}
		return buildOpenAIError(errMsg), true

	case "response.created", "response.in_progress",
		"response.content_part.added",
		"response.reasoning_summary_text.done",
		"response.reasoning.encrypted_content.delta", "response.reasoning.encrypted_content.done",
		"response.reasoning_summary_part.added", "response.reasoning_summary_part.done":
		return nil, false

	default:
		if delta := gjson.GetBytes(eventData, "delta"); delta.Exists() && delta.Type == gjson.String {
			return buildOpenAIChunk(st.ChunkID, st.Model, delta.String(), "", ""), false
		}
		return nil, false
	}
}

// buildOpenAIToolCallChunk 构建 tool call 首块（含 id、type、function.name）
func buildOpenAIToolCallChunk(id, model string, tcIndex int, callID, funcName string) []byte {
	chunk := []byte(`{}`)
	chunk, _ = sjson.SetBytes(chunk, "id", id)
	chunk, _ = sjson.SetBytes(chunk, "object", "chat.completion.chunk")
	chunk, _ = sjson.SetBytes(chunk, "created", 0)
	chunk, _ = sjson.SetBytes(chunk, "model", model)
	chunk, _ = sjson.SetBytes(chunk, "choices.0.index", 0)
	chunk, _ = sjson.SetBytes(chunk, "choices.0.delta.role", "assistant")
	chunk, _ = sjson.SetBytes(chunk, "choices.0.delta.tool_calls.0.index", tcIndex)
	chunk, _ = sjson.SetBytes(chunk, "choices.0.delta.tool_calls.0.id", callID)
	chunk, _ = sjson.SetBytes(chunk, "choices.0.delta.tool_calls.0.type", "function")
	chunk, _ = sjson.SetBytes(chunk, "choices.0.delta.tool_calls.0.function.name", funcName)
	chunk, _ = sjson.SetBytes(chunk, "choices.0.delta.tool_calls.0.function.arguments", "")
	chunk, _ = sjson.SetRawBytes(chunk, "choices.0.finish_reason", []byte("null"))
	return chunk
}

// buildOpenAIToolCallDeltaChunk 构建 tool call 参数增量块
func buildOpenAIToolCallDeltaChunk(id, model string, tcIndex int, argsDelta string) []byte {
	chunk := []byte(`{}`)
	chunk, _ = sjson.SetBytes(chunk, "id", id)
	chunk, _ = sjson.SetBytes(chunk, "object", "chat.completion.chunk")
	chunk, _ = sjson.SetBytes(chunk, "created", 0)
	chunk, _ = sjson.SetBytes(chunk, "model", model)
	chunk, _ = sjson.SetBytes(chunk, "choices.0.index", 0)
	chunk, _ = sjson.SetBytes(chunk, "choices.0.delta.tool_calls.0.index", tcIndex)
	chunk, _ = sjson.SetBytes(chunk, "choices.0.delta.tool_calls.0.function.arguments", argsDelta)
	chunk, _ = sjson.SetRawBytes(chunk, "choices.0.finish_reason", []byte("null"))
	return chunk
}

// buildOpenAIFinalChunkWithReason 构建带自定义 finish_reason 的最终流式块
func buildOpenAIFinalChunkWithReason(id, model string, usage *UsageInfo, finishReason string) []byte {
	chunk := buildOpenAIChunk(id, model, "", "", finishReason)
	if usage != nil {
		chunk, _ = sjson.SetBytes(chunk, "usage.prompt_tokens", usage.PromptTokens)
		chunk, _ = sjson.SetBytes(chunk, "usage.completion_tokens", usage.CompletionTokens)
		chunk, _ = sjson.SetBytes(chunk, "usage.total_tokens", usage.TotalTokens)
	}
	return chunk
}

// ExtractToolCallsFromOutput 从 response.completed 事件的 output 数组中提取 function_call 项
func ExtractToolCallsFromOutput(eventData []byte) []ToolCallResult {
	var toolCalls []ToolCallResult
	output := gjson.GetBytes(eventData, "response.output")
	if !output.IsArray() {
		return nil
	}
	output.ForEach(func(_, item gjson.Result) bool {
		if item.Get("type").String() == "function_call" {
			toolCalls = append(toolCalls, ToolCallResult{
				ID:        item.Get("call_id").String(),
				Name:      item.Get("name").String(),
				Arguments: item.Get("arguments").String(),
			})
		}
		return true
	})
	return toolCalls
}

// ExtractOutputTextFromOutput 从 response.completed 事件的 output 数组中提取输出文本（message/output_text）。
func ExtractOutputTextFromOutput(eventData []byte) string {
	output := gjson.GetBytes(eventData, "response.output")
	if !output.IsArray() {
		return ""
	}
	var builder strings.Builder
	output.ForEach(func(_, item gjson.Result) bool {
		if item.Get("type").String() != "message" {
			return true
		}
		content := item.Get("content")
		if !content.IsArray() {
			return true
		}
		content.ForEach(func(_, part gjson.Result) bool {
			if part.Get("type").String() == "output_text" {
				builder.WriteString(part.Get("text").String())
			}
			return true
		})
		return true
	})
	return builder.String()
}

// escapeJSON 安全转义 JSON 字符串
func escapeJSON(s string) string {
	b, _ := sjson.SetBytes([]byte(`{"v":""}`), "v", s)
	return gjson.GetBytes(b, "v").Raw
}
