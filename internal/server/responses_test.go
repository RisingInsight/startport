// responses_test.go /v1/responses 第三入站协议面的契约测试。
// 直调 handleResponses（路由注册在 handler.go，一行 mux 接线，与测试解耦）。
// 夹具复用 handler_test.go 的 testPoolWith / roundTripFunc / sseOK（同包）。
package server

import (
	"reflect"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/RisingInsight/startport/internal/auth"
	"github.com/RisingInsight/startport/internal/extchan"
	"github.com/RisingInsight/startport/internal/upstream"
)

// newResponsesHandler 构造主池回退用 handler（单账号池 + fake upstream）。
// 账号不设 token 字段（fake 上游不校验鉴权头）。
func newResponsesHandler(t *testing.T, fn func(r *http.Request) (int, string, bool)) *Handler {
	t.Helper()
	return NewHandler(Config{
		Pool: testPoolWith(&auth.Auth{UID: "u1", ExpiresAt: 9999999999}),
		Upstream: &upstream.Client{
			HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				status, body, isStream := fn(r)
				ct := "application/json"
				if isStream {
					ct = "text/event-stream"
				}
				return &http.Response{
					StatusCode: status,
					Header:     http.Header{"Content-Type": []string{ct}},
					Body:       io.NopCloser(strings.NewReader(body)),
				}, nil
			})},
			ChatBaseCN:    "https://fake.example",
			BillingBaseCN: "https://fake.example",
		},
	})
}

func responsesPost(h *Handler, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.handleResponses(rec, httptest.NewRequest("POST", "/v1/responses", strings.NewReader(body)))
	return rec
}

// 非流式：Responses 请求 → 主池（出站 SSE 聚合）→ Responses 对象折回。
func TestResponsesPoolFallbackSync(t *testing.T) {
	h := newResponsesHandler(t, func(r *http.Request) (int, string, bool) {
		var body struct {
			Model    string `json:"model"`
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Model != "glm-5.2" {
			t.Errorf("出站应为裸模型名 glm-5.2, got %q", body.Model)
		}
		if len(body.Messages) != 2 || body.Messages[0].Role != "system" || body.Messages[0].Content != "你是助手" {
			t.Errorf("instructions→system 转换异常: %+v", body.Messages)
		}
		return 200, sseOK, true
	})
	rec := responsesPost(h, `{"model":"glm-5.2","instructions":"你是助手","input":"hi"}`)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Object string `json:"object"`
		Status string `json:"status"`
		Model  string `json:"model"`
		Output []struct {
			Type   string `json:"type"`
			Role   string `json:"role"`
			Status string `json:"status"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
		Usage struct {
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if resp.Object != "response" || resp.Status != "completed" || resp.Model != "glm-5.2" {
		t.Errorf("Responses 骨架异常: %+v", resp)
	}
	if len(resp.Output) != 1 || resp.Output[0].Type != "message" ||
		len(resp.Output[0].Content) != 1 || resp.Output[0].Content[0].Text != "你好" {
		t.Errorf("output 折回异常: %+v", resp.Output)
	}
	if resp.Usage.InputTokens != 1 || resp.Usage.OutputTokens != 1 {
		t.Errorf("usage 折回异常: %+v", resp.Usage)
	}
}

// 流式：Responses 请求 → 主池 SSE → Responses 事件序列（created/delta/completed）。
func TestResponsesPoolFallbackStream(t *testing.T) {
	h := newResponsesHandler(t, func(r *http.Request) (int, string, bool) {
		return 200, sseOK, true
	})
	rec := responsesPost(h, `{"model":"cn:glm-5.2","stream":true,"input":"hi"}`)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	out := rec.Body.String()
	for _, want := range []string{
		"event: response.created", `"model":"cn:glm-5.2"`,
		"event: response.output_text.delta", `"delta":"你好"`,
		"event: response.output_text.done", "event: response.output_item.done",
		"event: response.completed", `"input_tokens":1`, `"output_tokens":1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("流缺事件片段 %s\n完整输出:\n%s", want, out)
		}
	}
	if strings.Contains(out, `"object":"chat.completion.chunk"`) {
		t.Error("OpenAI 原始 chunk 透传泄漏")
	}
	if strings.Contains(out, "data: [DONE]") {
		t.Error("[DONE] 不应透传（Responses 以 response.completed 收尾）")
	}
}

// 工具调用轮：扁平 function tools → chat 嵌套；流式 tool_calls → function_call item。
func TestResponsesToolCallStream(t *testing.T) {
	sse := "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"shell\",\"arguments\":\"{\\\"cmd\\\":\\\"ls\\\"}\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n" +
		"data: [DONE]\n\n"
	h := newResponsesHandler(t, func(r *http.Request) (int, string, bool) {
		var body struct {
			Tools []struct {
				Type     string `json:"type"`
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			} `json:"tools"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if len(body.Tools) != 1 || body.Tools[0].Type != "function" || body.Tools[0].Function.Name != "shell" {
			t.Errorf("扁平 tools 应转 chat 嵌套形态: %+v", body.Tools)
		}
		return 200, sse, true
	})
	rec := responsesPost(h, `{"model":"glm-5.2","stream":true,
		"tools":[{"type":"function","name":"shell","description":"跑命令","parameters":{"type":"object","properties":{"cmd":{"type":"string"}}}}],
		"input":"列目录"}`)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	out := rec.Body.String()
	for _, want := range []string{
		"event: response.output_item.added", `"type":"function_call"`,
		"event: response.function_call_arguments.delta", "ls",
		"event: response.function_call_arguments.done",
		"event: response.output_item.done", "event: response.completed",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("流缺事件片段 %s\n完整输出:\n%s", want, out)
		}
	}
	if !strings.Contains(out, `"call_id":"call_1"`) {
		t.Errorf("call_id 应保留: %s", out)
	}
}

// function_call / function_call_output 回传轮 → chat 的 assistant(tool_calls)+tool。
func TestResponsesToolResultRound(t *testing.T) {
	h := newResponsesHandler(t, func(r *http.Request) (int, string, bool) {
		var body struct {
			Messages []struct {
				Role      string `json:"role"`
				ToolCalls []struct {
					ID string `json:"id"`
				} `json:"tool_calls"`
				ToolCallID string `json:"tool_call_id"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if len(body.Messages) != 3 || body.Messages[1].Role != "assistant" ||
			len(body.Messages[1].ToolCalls) != 1 || body.Messages[1].ToolCalls[0].ID != "call_9" ||
			body.Messages[2].Role != "tool" || body.Messages[2].ToolCallID != "call_9" {
			b, _ := json.Marshal(body.Messages)
			t.Errorf("function_call/output 折算异常: %s", b)
		}
		return 200, sseOK, true
	})
	rec := responsesPost(h, `{"model":"glm-5.2","input":[
		{"type":"message","role":"user","content":"列目录"},
		{"type":"function_call","call_id":"call_9","name":"shell","arguments":"{\"cmd\":\"ls\"}"},
		{"type":"function_call_output","call_id":"call_9","output":"a.txt"}]}`)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
}

// 路由优先级：裸名唯一命中 ext 渠道时仍走 ext（与 /v1/messages 同规则）。
func TestResponsesExtRouted(t *testing.T) {
	extCalled := false
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		extCalled = true
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ext"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer up.Close()
	mgr := extchan.New(extchan.Config{TimeoutSeconds: 5})
	mgr.ReplaceAll([]extchan.ChannelDef{{
		Name:    "sf",
		BaseURL: up.URL,
		Models:  []string{"glm-5.2"},
	}})
	poolCalled := false
	h := NewHandler(Config{
		Pool: testPoolWith(&auth.Auth{UID: "u1", ExpiresAt: 9999999999}),
		Upstream: &upstream.Client{
			HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				poolCalled = true
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(sseOK))}, nil
			})},
			ChatBaseCN: "https://fake.example", BillingBaseCN: "https://fake.example",
		},
		Ext: mgr,
	})
	rec := responsesPost(h, `{"model":"glm-5.2","input":"hi"}`)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if !extCalled || poolCalled {
		t.Errorf("裸名唯一命中 ext 渠道应走 ext（ext=%v pool=%v）", extCalled, poolCalled)
	}
	var resp struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Model != "ext:sf/glm-5.2" {
		t.Errorf("响应 model 应带渠道前缀: %q", resp.Model)
	}
}

// 空池：错误折回 Responses 错误信封。
func TestResponsesPoolFallbackErrorShape(t *testing.T) {
	h := NewHandler(Config{
		Pool: testPoolWith(),
		Upstream: &upstream.Client{
			HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				t.Error("空池不应打出站请求")
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(sseOK))}, nil
			})},
			ChatBaseCN: "https://fake.example", BillingBaseCN: "https://fake.example",
		},
	})
	rec := responsesPost(h, `{"model":"glm-5.2","input":"hi"}`)
	if rec.Code < 400 {
		t.Fatalf("status %d, want >=400: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Error.Type == "" || resp.Error.Message == "" {
		t.Errorf("Responses 错误信封异常: %+v", resp)
	}
}

// 明确不支持面：previous_response_id / image part / 非 function 工具 → 400。
func TestResponsesUnsupportedRejected(t *testing.T) {
	h := newResponsesHandler(t, func(r *http.Request) (int, string, bool) {
		t.Error("不支持形态不应打出站请求")
		return 200, sseOK, true
	})
	cases := []string{
		`{"model":"glm-5.2","previous_response_id":"resp_x","input":"hi"}`,
		`{"model":"glm-5.2","input":[{"type":"message","role":"user","content":[{"type":"input_image","image_url":"https://x/img.png"}]}]}`,
		`{"model":"glm-5.2","tools":[{"type":"web_search"}],"input":"hi"}`,
	}
	for i, body := range cases {
		rec := responsesPost(h, body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("case %d: status %d, want 400: %s", i, rec.Code, rec.Body.String())
		}
	}
}

// output_item.added 事件里的 output_index 序列（按事件顺序提取）。
func responsesAddedIndexes(body string) []int {
	var out []int
	lines := strings.Split(body, "\n")
	for i, ln := range lines {
		if strings.TrimSpace(ln) != "event: response.output_item.added" || i+1 >= len(lines) {
			continue
		}
		var ev struct {
			OutputIndex int `json:"output_index"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(lines[i+1], "data: ")), &ev); err == nil {
			out = append(out, ev.OutputIndex)
		}
	}
	return out
}

// output_index 连续性回归：文本 item 与 function_call item 的 output_index 必须
// 连续不跳号。曾有双重递增 bug——closeTextItem 已把 outIdx 前进一次，开新工具
// 又预递增一次：文本后首个工具拿到 index 2（跳过 1），纯工具流则从 1 开始
// （跳过 0），违反 Responses 事件序列的连续索引语义。
func TestResponsesToolCallOutputIndexContiguous(t *testing.T) {
	// 场景一：先文本后工具 → 文本 0、工具 1。
	sse := "data: {\"choices\":[{\"delta\":{\"content\":\"你好\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"shell\",\"arguments\":\"{}\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n" +
		"data: [DONE]\n\n"
	h := newResponsesHandler(t, func(r *http.Request) (int, string, bool) { return 200, sse, true })
	rec := responsesPost(h, `{"model":"cn:glm-5.2","stream":true,"input":"hi"}`)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if got := responsesAddedIndexes(rec.Body.String()); !reflect.DeepEqual(got, []int{0, 1}) {
		t.Errorf("文本→工具的 output_index 序列 = %v, want [0 1]\n完整输出:\n%s", got, rec.Body.String())
	}

	// 场景二：纯工具流（无文本）→ 工具 0 起步。
	sse2 := "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"shell\",\"arguments\":\"{}\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":1,\"id\":\"call_2\",\"type\":\"function\",\"function\":{\"name\":\"edit\",\"arguments\":\"{}\"}}]}}]}\n\n" +
		"data: [DONE]\n\n"
	h2 := newResponsesHandler(t, func(r *http.Request) (int, string, bool) { return 200, sse2, true })
	rec2 := responsesPost(h2, `{"model":"cn:glm-5.2","stream":true,"input":"hi"}`)
	if rec2.Code != 200 {
		t.Fatalf("status %d: %s", rec2.Code, rec2.Body.String())
	}
	if got := responsesAddedIndexes(rec2.Body.String()); !reflect.DeepEqual(got, []int{0, 1}) {
		t.Errorf("纯工具流的 output_index 序列 = %v, want [0 1]\n完整输出:\n%s", got, rec2.Body.String())
	}
}
