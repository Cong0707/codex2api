package proxy

import (
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestBuildImagesResponsesRequestMatchesReferenceChain(t *testing.T) {
	tool := []byte(`{"type":"image_generation","action":"generate","model":"gpt-image-2","size":"1024x1024"}`)

	body := buildImagesResponsesRequest("draw a cat", nil, tool)

	if got := gjson.GetBytes(body, "model").String(); got != defaultImagesMainModel {
		t.Fatalf("responses model = %q, want %q", got, defaultImagesMainModel)
	}
	if got := gjson.GetBytes(body, "tool_choice.type").String(); got != "image_generation" {
		t.Fatalf("tool_choice.type = %q, want image_generation", got)
	}
	if got := gjson.GetBytes(body, "tools.0.type").String(); got != "image_generation" {
		t.Fatalf("tools.0.type = %q, want image_generation", got)
	}
	if got := gjson.GetBytes(body, "tools.0.model").String(); got != "gpt-image-2" {
		t.Fatalf("tools.0.model = %q, want gpt-image-2", got)
	}
	if got := gjson.GetBytes(body, "input.0.content.0.text").String(); got != "draw a cat" {
		t.Fatalf("prompt = %q, want draw a cat", got)
	}
}

func TestBuildImagesResponsesRequestIncludesEditImages(t *testing.T) {
	tool := []byte(`{"type":"image_generation","action":"edit","model":"gpt-image-2"}`)

	body := buildImagesResponsesRequest("replace background", []string{"https://example.com/source.png"}, tool)

	if got := gjson.GetBytes(body, "tools.0.action").String(); got != "edit" {
		t.Fatalf("tools.0.action = %q, want edit", got)
	}
	if got := gjson.GetBytes(body, "input.0.content.1.type").String(); got != "input_image" {
		t.Fatalf("input image type = %q, want input_image", got)
	}
	if got := gjson.GetBytes(body, "input.0.content.1.image_url").String(); got != "https://example.com/source.png" {
		t.Fatalf("input image URL = %q", got)
	}
}

func TestCollectImagesResponseBuildsOpenAIImagePayload(t *testing.T) {
	upstream := `data: {"type":"response.completed","response":{"created_at":1710000000,"usage":{"input_tokens":5,"output_tokens":9},"tool_usage":{"image_gen":{"images":1}},"tools":[{"type":"image_generation","model":"gpt-image-2","output_format":"png","quality":"high","size":"1024x1024"}],"output":[{"type":"image_generation_call","result":"aGVsbG8=","revised_prompt":"draw a cat","output_format":"png"}]}}` + "\n\n"

	out, usage, imageCount, officialAvailable, officialKnown, err := collectImagesResponse(strings.NewReader(upstream), "b64_json", "gpt-image-2")
	if err != nil {
		t.Fatalf("collectImagesResponse returned error: %v", err)
	}
	if imageCount != 1 {
		t.Fatalf("imageCount = %d, want 1", imageCount)
	}
	if usage == nil || usage.InputTokens != 5 || usage.OutputTokens != 9 {
		t.Fatalf("usage = %#v, want input=5 output=9", usage)
	}
	if got := gjson.GetBytes(out, "data.0.b64_json").String(); got != "aGVsbG8=" {
		t.Fatalf("b64_json = %q, want aGVsbG8=", got)
	}
	if got := gjson.GetBytes(out, "model").String(); got != "gpt-image-2" {
		t.Fatalf("model = %q, want gpt-image-2", got)
	}
	if got := gjson.GetBytes(out, "usage.images").Int(); got != 1 {
		t.Fatalf("usage.images = %d, want 1", got)
	}
	if officialKnown {
		t.Fatalf("official availability should stay unknown for images-only usage, got %d", officialAvailable)
	}
}

func TestNormalizeChatCompletionMessageShorthandRequest(t *testing.T) {
	raw := []byte(`{"model":"gpt-image-2","role":"user","content":[{"type":"text","text":"改成水彩画风"}]}`)

	normalized := normalizeChatCompletionMessageShorthandRequest(raw)

	if !gjson.GetBytes(normalized, "messages").Exists() {
		t.Fatalf("messages not created: %s", string(normalized))
	}
	if got := gjson.GetBytes(normalized, "messages.0.role").String(); got != "user" {
		t.Fatalf("messages.0.role = %q, want user", got)
	}
	if got := gjson.GetBytes(normalized, "messages.0.content.0.text").String(); got != "改成水彩画风" {
		t.Fatalf("messages.0.content.0.text = %q", got)
	}
	if gjson.GetBytes(normalized, "role").Exists() || gjson.GetBytes(normalized, "content").Exists() {
		t.Fatalf("top-level shorthand fields should be removed: %s", string(normalized))
	}
}

func TestBuildChatCompletionsImageResponsesRequestGenerate(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-image-2",
		"messages":[
			{"role":"system","content":"你是一个画图助手"},
			{"role":"user","content":[{"type":"text","text":"画一只猫"}]}
		],
		"size":"1024x1024",
		"reasoning_effort":"xhigh",
		"service_tier":"priority"
	}`)

	body, err := buildChatCompletionsImageResponsesRequest(raw, "gpt-image-2")
	if err != nil {
		t.Fatalf("buildChatCompletionsImageResponsesRequest returned error: %v", err)
	}
	if got := gjson.GetBytes(body, "tools.0.action").String(); got != "generate" {
		t.Fatalf("tools.0.action = %q, want generate", got)
	}
	if got := gjson.GetBytes(body, "tools.0.model").String(); got != "gpt-image-2" {
		t.Fatalf("tools.0.model = %q, want gpt-image-2", got)
	}
	if got := gjson.GetBytes(body, "tools.0.size").String(); got != "1024x1024" {
		t.Fatalf("tools.0.size = %q", got)
	}
	if got := gjson.GetBytes(body, "reasoning.effort").String(); got != "xhigh" {
		t.Fatalf("reasoning.effort = %q, want xhigh", got)
	}
	if got := gjson.GetBytes(body, "service_tier").String(); got != "priority" {
		t.Fatalf("service_tier = %q, want priority", got)
	}
	if got := gjson.GetBytes(body, "input.0.content.0.text").String(); !strings.Contains(got, "画一只猫") {
		t.Fatalf("prompt = %q, want contains 画一只猫", got)
	}
}

func TestBuildChatCompletionsImageResponsesRequestEdit(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-image-2",
		"messages":[
			{
				"role":"user",
				"content":[
					{"type":"text","text":"改成水彩画风"},
					{"type":"image_url","image_url":{"url":"data:image/png;base64,aGVsbG8="}}
				]
			}
		]
	}`)

	body, err := buildChatCompletionsImageResponsesRequest(raw, "gpt-image-2")
	if err != nil {
		t.Fatalf("buildChatCompletionsImageResponsesRequest returned error: %v", err)
	}
	if got := gjson.GetBytes(body, "tools.0.action").String(); got != "edit" {
		t.Fatalf("tools.0.action = %q, want edit", got)
	}
	if got := gjson.GetBytes(body, "input.0.content.1.image_url").String(); got != "data:image/png;base64,aGVsbG8=" {
		t.Fatalf("input.0.content.1.image_url = %q", got)
	}
}

func TestBuildChatCompletionsImageResponse(t *testing.T) {
	payload := []byte(`{
		"created":1714000000,
		"output_format":"png",
		"data":[{"b64_json":"aGVsbG8="}]
	}`)

	out, err := buildChatCompletionsImageResponse(payload, "gpt-image-2", "chatcmpl-test", 1714000000, nil)
	if err != nil {
		t.Fatalf("buildChatCompletionsImageResponse returned error: %v", err)
	}
	if got := gjson.GetBytes(out, "object").String(); got != "chat.completion" {
		t.Fatalf("object = %q, want chat.completion", got)
	}
	if got := gjson.GetBytes(out, "choices.0.message.role").String(); got != "assistant" {
		t.Fatalf("choices.0.message.role = %q, want assistant", got)
	}
	if got := gjson.GetBytes(out, "choices.0.message.content").String(); got != "![image_1](data:image/png;base64,aGVsbG8=)" {
		t.Fatalf("choices.0.message.content = %q", got)
	}
}
