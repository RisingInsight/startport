package provider

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
)

// OpenAI wire → Anthropic wire：system 抽离、tools、tool_calls → tool_use、
// role=tool → user 轮 tool_result（相邻同角色合并）。
func TestOpenAIToAnthropicRequest(t *testing.T) {
	in := &oaiChatRequest{
		Model: "claude-3",
		Messages: []oaiMessage{
			{Role: "system", Content: json.RawMessage(`"you are helpful"`)},
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
		}{Name: "get_weather", Description: "查天气", Parameters: json.RawMessage(`{"type":"object"}`)}}},
		MaxTokens: 100,
	}
	raw, err := openAIToAnthropic(in, false)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if out["system"] != "you are helpful" {
		t.Errorf("system 抽离失败: %v", out["system"])
	}
	if out["max_tokens"].(float64) != 100 {
		t.Errorf("max_tokens = %v", out["max_tokens"])
	}
	msgs := out["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("消息数 = %d, want 3（user / assistant(tool_use) / user(tool_result)）: %s", len(msgs), raw)
	}
	assistant := msgs[1].(map[string]any)
	blocks := assistant["content"].([]any)
	found := false
	for _, b := range blocks {
		bm := b.(map[string]any)
		if bm["type"] == "tool_use" && bm["name"] == "get_weather" {
			found = true
			if bm["input"].(map[string]any)["city"] != "北京" {
				t.Errorf("tool_use input = %v", bm["input"])
			}
		}
	}
	if !found {
		t.Error("assistant 缺 tool_use 块")
	}
	toolResult := msgs[2].(map[string]any)
	if toolResult["role"] != "user" {
		t.Errorf("tool_result 应在 user 轮: %v", toolResult["role"])
	}
	trBlocks := toolResult["content"].([]any)
	if len(trBlocks) != 1 || trBlocks[0].(map[string]any)["type"] != "tool_result" {
		t.Errorf("tool_result 块异常: %v", trBlocks)
	}
	tools := out["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools 数 = %d", len(tools))
	}
	if tools[0].(map[string]any)["name"] != "get_weather" {
		t.Errorf("tools 映射异常: %v", tools[0])
	}

	// max_tokens 缺省补 4096。
	if got := openAIMaxTokens(&oaiChatRequest{}); got != 0 {
		t.Errorf("openAIMaxTokens 空请求应 0, got %d", got)
	}
	raw2, _ := openAIToAnthropic(&oaiChatRequest{Messages: []oaiMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}}}, false)
	var out2 map[string]any
	_ = json.Unmarshal(raw2, &out2)
	if out2["max_tokens"].(float64) != anthropicDefaultMaxTokens {
		t.Errorf("缺省 max_tokens 应 %d, got %v", anthropicDefaultMaxTokens, out2["max_tokens"])
	}
}

// Anthropic 响应 → OpenAI 响应（content/tool_use/stop_reason/usage）。
func TestAnthropicConvertResponse(t *testing.T) {
	a := NewAnthropicAdapter()
	target := &Target{Name: "ant", Protocol: ProtocolAnthropic, ClientModel: "ext:ant/claude"}
	body := []byte(`{
	  "id":"msg_1","type":"message","role":"assistant","model":"claude-3",
	  "content":[{"type":"text","text":"我来查"},{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"city":"北京"}}],
	  "stop_reason":"tool_use",
	  "usage":{"input_tokens":12,"output_tokens":5,"cache_read_input_tokens":3}
	}`)
	var out map[string]any
	if err := json.Unmarshal(a.ConvertResponse(target, 200, body), &out); err != nil {
		t.Fatal(err)
	}
	if out["model"] != "ext:ant/claude" {
		t.Errorf("model 应回写客户端模型名: %v", out["model"])
	}
	choice := out["choices"].([]any)[0].(map[string]any)
	if choice["finish_reason"] != "tool_calls" {
		t.Errorf("finish_reason = %v", choice["finish_reason"])
	}
	msg := choice["message"].(map[string]any)
	if msg["content"] != "我来查" {
		t.Errorf("content = %v", msg["content"])
	}
	calls := msg["tool_calls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("tool_calls = %v", msg["tool_calls"])
	}
	fn := calls[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "get_weather" || !strings.Contains(fn["arguments"].(string), "北京") {
		t.Errorf("tool_call 映射异常: %v", fn)
	}
	usage := out["usage"].(map[string]any)
	if usage["prompt_tokens"].(float64) != 12 || usage["completion_tokens"].(float64) != 5 ||
		usage["total_tokens"].(float64) != 17 {
		t.Errorf("usage 映射异常: %v", usage)
	}

	// 错误体 → OpenAI 错误信封（message 保留原文）。
	errBody := a.ConvertResponse(target, 401, []byte(`{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`))
	var e map[string]any
	_ = json.Unmarshal(errBody, &e)
	em := e["error"].(map[string]any)
	if !strings.Contains(em["message"].(string), "invalid x-api-key") {
		t.Errorf("错误 message 未保留原文: %v", em)
	}
}

// Anthropic SSE → OpenAI chunk SSE：文本增量 + tool_calls 分片 + finish + usage + [DONE]。
func TestAnthropicConvertStream(t *testing.T) {
	sse := `event: message_start
data: {"type":"message_start","message":{"usage":{"input_tokens":12}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"你"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"好"}}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"read"}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"p\":"}}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":5}}

event: message_stop
data: {"type":"message_stop"}

`
	a := NewAnthropicAdapter()
	target := &Target{Name: "ant", Protocol: ProtocolAnthropic, ClientModel: "ext:ant/claude"}
	rc, err := a.ConvertStream(target, "text/event-stream", io.NopCloser(strings.NewReader(sse)))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rc)
	out := string(b)
	for _, want := range []string{
		`"object":"chat.completion.chunk"`, `"content":"你"`, `"content":"好"`,
		`"id":"toolu_1"`, `"name":"read"`, `"arguments":"{\"p\":"`,
		`"finish_reason":"tool_calls"`, `"prompt_tokens":12`, `"completion_tokens":5`,
		"data: [DONE]",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("转换流缺片段 %q\n%s", want, out)
		}
	}
}

// Anthropic 请求 URL/版本头。
func TestAnthropicBuildRequest(t *testing.T) {
	a := NewAnthropicAdapter()
	target := &Target{
		Name: "ant", Protocol: ProtocolAnthropic, BaseURL: "https://api.anthropic.com",
		Auth: AuthSpec{Mode: AuthXAPIKey}, Secret: "sk-ant",
	}
	req, err := a.BuildRequest(context.Background(), target, []byte(`{"model":"claude","messages":[{"role":"user","content":"hi"}]}`), false)
	if err != nil {
		t.Fatal(err)
	}
	if req.URL.String() != "https://api.anthropic.com/v1/messages" {
		t.Errorf("url = %q", req.URL)
	}
	if req.Header.Get("x-api-key") != "sk-ant" {
		t.Errorf("x-api-key = %q", req.Header.Get("x-api-key"))
	}
	if req.Header.Get("anthropic-version") != anthropicAPIVersion {
		t.Errorf("anthropic-version = %q", req.Header.Get("anthropic-version"))
	}
}
