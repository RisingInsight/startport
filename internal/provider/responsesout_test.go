// responsesout_test.go Responses 出站转换的契约测试（请求/响应/流三方向）。
package provider

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestChatToResponsesRequest(t *testing.T) {
	body := []byte(`{
		"model": "gpt-5.6-luna",
		"messages": [
			{"role": "system", "content": "你是编码助手"},
			{"role": "user", "content": "查天气"},
			{"role": "assistant", "content": "好"},
			{"role": "assistant", "content": "", "tool_calls": [{"id": "call_1", "type": "function", "function": {"name": "get_weather", "arguments": "{\"city\":\"杭州\"}"}}]},
			{"role": "tool", "tool_call_id": "call_1", "content": "晴 25 度"}
		],
		"tools": [{"type": "function", "function": {"name": "get_weather", "description": "查天气", "parameters": {"type": "object"}}}],
		"tool_choice": {"type": "function", "function": {"name": "get_weather"}},
		"max_tokens": 4096,
		"reasoning_effort": "low",
		"temperature": 0.7,
		"top_p": 0.9
	}`)
	req, err := chatToResponsesRequest(body, true)
	if err != nil {
		t.Fatalf("chatToResponsesRequest: %v", err)
	}
	if req["instructions"] != "你是编码助手" {
		t.Errorf("instructions = %v", req["instructions"])
	}
	if req["stream"] != true || req["store"] != false {
		t.Errorf("stream/store 应强制: %v / %v", req["stream"], req["store"])
	}
	if req["max_output_tokens"] != 4096 {
		t.Errorf("max_output_tokens = %v", req["max_output_tokens"])
	}
	if req["reasoning"] == nil {
		t.Error("reasoning.effort 应映射")
	}
	if _, has := req["temperature"]; has {
		t.Error("采样参数不应上抛（Codex 后端拒绝）")
	}
	input, _ := req["input"].([]map[string]any)
	if len(input) != 4 {
		t.Fatalf("input 应 4 项（user/assistant/function_call/function_call_output）: %d", len(input))
	}
	if input[0]["role"] != "user" || input[1]["type"] != "message" {
		t.Errorf("前两项形状错: %+v / %+v", input[0], input[1])
	}
	if input[2]["type"] != "function_call" || input[2]["call_id"] != "call_1" || input[2]["name"] != "get_weather" {
		t.Errorf("function_call 项错: %+v", input[2])
	}
	if input[3]["type"] != "function_call_output" || input[3]["call_id"] != "call_1" {
		t.Errorf("function_call_output 项错: %+v", input[3])
	}
	tools, _ := req["tools"].([]map[string]any)
	if len(tools) != 1 || tools[0]["type"] != "function" || tools[0]["name"] != "get_weather" {
		t.Errorf("tools 应扁平化: %+v", tools)
	}
	tc, _ := req["tool_choice"].(map[string]any)
	if tc == nil || tc["type"] != "function" || tc["name"] != "get_weather" {
		t.Errorf("tool_choice 应改写为命名形态: %+v", req["tool_choice"])
	}
}

func TestResponsesResponseToChat(t *testing.T) {
	body := []byte(`{
		"id": "resp_1", "model": "gpt-5.6-luna", "status": "completed",
		"output": [
			{"type": "message", "role": "assistant", "content": [{"type": "output_text", "text": "你好"}]},
			{"type": "function_call", "call_id": "call_9", "name": "get_weather", "arguments": "{\"city\":\"北京\"}"}
		],
		"usage": {"input_tokens": 12, "output_tokens": 34, "total_tokens": 46}
	}`)
	out := responsesResponseToChat(body, "gpt-5.6-luna")
	var chat struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content   string `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(out, &chat); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if chat.ID != "resp_1" || chat.Model != "gpt-5.6-luna" {
		t.Errorf("id/model = %q/%q", chat.ID, chat.Model)
	}
	if chat.Choices[0].Message.Content != "你好" {
		t.Errorf("content = %q", chat.Choices[0].Message.Content)
	}
	if chat.Choices[0].FinishReason != "tool_calls" {
		t.Errorf("finish_reason = %q", chat.Choices[0].FinishReason)
	}
	if len(chat.Choices[0].Message.ToolCalls) != 1 || chat.Choices[0].Message.ToolCalls[0].ID != "call_9" {
		t.Errorf("tool_calls = %+v", chat.Choices[0].Message.ToolCalls)
	}
	if chat.Usage.PromptTokens != 12 || chat.Usage.CompletionTokens != 34 {
		t.Errorf("usage = %+v", chat.Usage)
	}
}

// 流方向：role → 文本增量 → function_call 声明 → 参数增量 → usage/finish/[DONE]。
func TestResponsesOutStream(t *testing.T) {
	st := newResponsesOutStream("gpt-5.6-luna")
	frames := []sseFrame{
		{Event: "response.output_item.added", Data: `{"item":{"id":"msg_1","type":"message","role":"assistant"}}`},
		{Event: "response.output_text.delta", Data: `{"delta":"你好"}`},
		{Event: "response.output_item.added", Data: `{"item":{"id":"fc_1","type":"function_call","call_id":"call_x","name":"get_weather"}}`},
		{Event: "response.function_call_arguments.delta", Data: `{"item_id":"fc_1","delta":"{\"city\":"}`},
		{Event: "response.completed", Data: `{"response":{"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}}`},
	}
	var out strings.Builder
	done := false
	for _, f := range frames {
		outs, d, err := st.handle(f)
		if err != nil {
			t.Fatalf("handle(%s): %v", f.Event, err)
		}
		for _, o := range outs {
			out.Write(o)
		}
		if d {
			done = true
		}
	}
	if !done {
		t.Fatal("completed 后流应终止")
	}
	text := out.String()
	for _, want := range []string{
		`"role":"assistant"`,
		`"content":"你好"`,
		`"id":"call_x"`,
		`"name":"get_weather"`,
		`"{\"city\":"`,
		`"finish_reason":"tool_calls"`,
		`"prompt_tokens":10`,
		"data: [DONE]",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("流输出缺 %q", want)
		}
	}
}

// 上游失败事件：流终止并带错误（不静默吞掉）。
func TestResponsesOutStreamFailed(t *testing.T) {
	st := newResponsesOutStream("gpt-5.6-luna")
	_, _, err := st.handle(sseFrame{Event: "response.failed", Data: `{"response":{"error":{"message":"quota"}}}`})
	if err == nil {
		t.Fatal("response.failed 应返回错误")
	}
}

func TestAggregateResponsesSSE(t *testing.T) {
	sse := "event: response.created\ndata: {\"response\":{}}\n\n" +
		"event: response.completed\ndata: {\"response\":{\"id\":\"resp_9\",\"output\":[]}}\n\n"
	final, err := aggregateResponsesSSE([]byte(sse))
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	if !strings.Contains(string(final), `"id":"resp_9"`) {
		t.Errorf("最终 response = %s", final)
	}
	if _, err := aggregateResponsesSSE([]byte("event: response.failed\ndata: {}\n\n")); err == nil {
		t.Error("failed 流应报错")
	}
}
