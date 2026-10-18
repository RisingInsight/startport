package provider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
)

// OpenAI wire → Gemini wire：systemInstruction / role:model / functionResponse /
// generationConfig / functionDeclarations。
func TestOpenAIToGeminiRequest(t *testing.T) {
	temp := 0.2
	in := &oaiChatRequest{
		Model: "gemini-2.0-flash",
		Messages: []oaiMessage{
			{Role: "system", Content: json.RawMessage(`"你是助手"`)},
			{Role: "user", Content: json.RawMessage(`"北京天气"`)},
			{Role: "assistant", Content: json.RawMessage(`null`), ToolCalls: []oaiToolCall{
				{ID: "call_1", Type: "function", Function: struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				}{Name: "get_weather", Arguments: `{"city":"北京"}`}},
			}},
			{Role: "tool", ToolCallID: "call_1", Content: json.RawMessage(`"晴 25 度"`)},
		},
		Tools: []oaiTool{{Type: "function", Function: struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			Parameters  json.RawMessage `json:"parameters"`
		}{Name: "get_weather", Parameters: json.RawMessage(`{"type":"object"}`)}}},
		MaxTokens:   256,
		Temperature: &temp,
	}
	raw, err := openAIToGemini(in)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	sys := out["systemInstruction"].(map[string]any)
	if sys["parts"].([]any)[0].(map[string]any)["text"] != "你是助手" {
		t.Errorf("systemInstruction 异常: %v", sys)
	}
	contents := out["contents"].([]any)
	if len(contents) != 3 {
		t.Fatalf("contents = %d, want 3（user / model / user(functionResponse)）: %s", len(contents), raw)
	}
	if contents[1].(map[string]any)["role"] != "model" {
		t.Errorf("assistant 应映射 role=model: %v", contents[1])
	}
	parts := contents[1].(map[string]any)["parts"].([]any)
	fc := parts[0].(map[string]any)["functionCall"].(map[string]any)
	if fc["name"] != "get_weather" {
		t.Errorf("functionCall 异常: %v", fc)
	}
	last := contents[2].(map[string]any)
	fr := last["parts"].([]any)[0].(map[string]any)["functionResponse"].(map[string]any)
	if fr["name"] != "get_weather" {
		t.Errorf("functionResponse 缺函数名（Gemini 用名字关联）: %v", fr)
	}
	tools := out["tools"].([]any)[0].(map[string]any)
	decls := tools["functionDeclarations"].([]any)
	if len(decls) != 1 || decls[0].(map[string]any)["name"] != "get_weather" {
		t.Errorf("functionDeclarations 异常: %v", decls)
	}
	gen := out["generationConfig"].(map[string]any)
	if gen["maxOutputTokens"].(float64) != 256 || gen["temperature"].(float64) != 0.2 {
		t.Errorf("generationConfig 异常: %v", gen)
	}
}

// Gemini 响应 → OpenAI 响应（text/functionCall/finishReason/usageMetadata）。
func TestGeminiConvertResponse(t *testing.T) {
	a := NewGeminiAdapter()
	target := &Target{Name: "gem", Protocol: ProtocolGemini, ClientModel: "ext:gem/gemini"}
	body := []byte(`{
	  "candidates":[{"content":{"role":"model","parts":[
	    {"text":"结果："},{"functionCall":{"name":"read","args":{"p":"/x"}}}]},
	    "finishReason":"STOP"}],
	  "usageMetadata":{"promptTokenCount":7,"candidatesTokenCount":3,"totalTokenCount":10}
	}`)
	var out map[string]any
	if err := json.Unmarshal(a.ConvertResponse(target, 200, body), &out); err != nil {
		t.Fatal(err)
	}
	choice := out["choices"].([]any)[0].(map[string]any)
	if choice["finish_reason"] != "tool_calls" {
		t.Errorf("有 functionCall 时 finish_reason 应 tool_calls: %v", choice["finish_reason"])
	}
	msg := choice["message"].(map[string]any)
	if msg["content"] != "结果：" {
		t.Errorf("content = %v", msg["content"])
	}
	usage := out["usage"].(map[string]any)
	if usage["prompt_tokens"].(float64) != 7 || usage["completion_tokens"].(float64) != 3 ||
		usage["total_tokens"].(float64) != 10 {
		t.Errorf("usage = %v", usage)
	}

	// 安全拦截：finishReason=SAFETY → content_filter。
	blocked := []byte(`{"candidates":[{"content":{"parts":[]},"finishReason":"SAFETY"}]}`)
	var out2 map[string]any
	_ = json.Unmarshal(a.ConvertResponse(target, 200, blocked), &out2)
	c2 := out2["choices"].([]any)[0].(map[string]any)
	if c2["finish_reason"] != "content_filter" {
		t.Errorf("SAFETY 应映射 content_filter: %v", c2["finish_reason"])
	}
}

// Gemini SSE → OpenAI chunk SSE；上游非 SSE 时返回 ErrStreamUnsupported。
func TestGeminiConvertStream(t *testing.T) {
	sse := `data: {"candidates":[{"content":{"role":"model","parts":[{"text":"he"}]}}]}

data: {"candidates":[{"content":{"role":"model","parts":[{"text":"llo"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":7,"candidatesTokenCount":3,"totalTokenCount":10}}

`
	a := NewGeminiAdapter()
	target := &Target{Name: "gem", Protocol: ProtocolGemini, ClientModel: "ext:gem/gemini"}
	rc, err := a.ConvertStream(target, "text/event-stream; charset=utf-8", io.NopCloser(strings.NewReader(sse)))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rc)
	out := string(b)
	for _, want := range []string{`"content":"he"`, `"content":"llo"`, `"finish_reason":"stop"`,
		`"prompt_tokens":7`, `"completion_tokens":3`, "data: [DONE]"} {
		if !strings.Contains(out, want) {
			t.Errorf("Gemini 流转换缺片段 %q\n%s", want, out)
		}
	}

	// 上游返回 application/json：明确不支持，绝不伪流式。
	_, err = a.ConvertStream(target, "application/json", io.NopCloser(strings.NewReader(`{"candidates":[]}`)))
	if !errors.Is(err, ErrStreamUnsupported) {
		t.Errorf("非 SSE 应返回 ErrStreamUnsupported, got %v", err)
	}
}

// Gemini 模型目录：剥离 models/ 前缀 + 过滤非 generateContent 条目。
func TestGeminiDecodeModels(t *testing.T) {
	a := NewGeminiAdapter()
	target := &Target{Name: "gem", Protocol: ProtocolGemini}
	body := []byte(`{"models":[
	  {"name":"models/gemini-2.0-flash","supportedGenerationMethods":["generateContent"]},
	  {"name":"models/text-embedding-004","supportedGenerationMethods":["embedContent"]},
	  {"name":"models/gemini-1.5-pro"}
	]}`)
	models, err := a.DecodeModels(target, 200, body)
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 || models[0] != "gemini-2.0-flash" || models[1] != "gemini-1.5-pro" {
		t.Errorf("DecodeModels = %v", models)
	}
}

// Gemini 请求 URL/鉴权（x-goog-api-key 走 header，不落 URL）。
func TestGeminiBuildRequest(t *testing.T) {
	a := NewGeminiAdapter()
	target := &Target{
		Name: "gem", Protocol: ProtocolGemini, BaseURL: "https://generativelanguage.googleapis.com",
		Auth: AuthSpec{Mode: AuthCustom, Header: "x-goog-api-key"}, Secret: "AIza-secret",
	}
	req, err := a.BuildRequest(context.Background(), target,
		[]byte(`{"model":"gemini-2.0-flash","messages":[{"role":"user","content":"hi"}]}`), true)
	if err != nil {
		t.Fatal(err)
	}
	want := "https://generativelanguage.googleapis.com/v1beta/models/gemini-2.0-flash:streamGenerateContent?alt=sse"
	if req.URL.String() != want {
		t.Errorf("url = %q, want %q", req.URL, want)
	}
	if req.Header.Get("x-goog-api-key") != "AIza-secret" {
		t.Errorf("x-goog-api-key = %q", req.Header.Get("x-goog-api-key"))
	}
	if strings.Contains(req.URL.RawQuery, "AIza-secret") {
		t.Error("秘密值不应出现在 URL（默认 header 形态）")
	}
}
