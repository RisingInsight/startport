// exthropic_pool_test.go /v1/messages 主池回退（v1.7.6）的端到端测试。
//
// 与 exthropic_test.go（ext 渠道折回）分文件：本文件只覆盖「非 ext 模型 →
// CodeBuddy 池」的新链路——路由优先级、请求转换、流式逐帧翻译、工具调用折回、
// 错误信封形态。主池 fake 复用 handler_test.go 的 testPoolWith / roundTripFunc /
// sseOK 夹具（同包）。
package server

import (
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

// newPoolMsgUpstream 构造主池路径的 fake upstream（可检查完整出站请求）。
// 与 newFakeUpstream 同构，但把完整 *http.Request 交给用例，断言转换后的出站形态。
func newPoolMsgUpstream(t *testing.T, fn func(r *http.Request) (int, string, bool)) *upstream.Client {
	t.Helper()
	return &upstream.Client{
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
	}
}

// newPoolMsgHandler 构造带单账号池 + fake upstream 的 handler（主池回退用）。
// 账号不设 token 字段（fake 上游不校验鉴权头，与 handler_test 的空 token 用法一致）。
func newPoolMsgHandler(t *testing.T, fn func(r *http.Request) (int, string, bool)) *Handler {
	t.Helper()
	return NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", ExpiresAt: 9999999999}),
		Upstream: newPoolMsgUpstream(t, fn),
	})
}

// 裸名模型经 /v1/messages 落主池：非流式（出站强制 SSE → 网关聚合 → 折回）。
func TestMessagesPoolFallbackSync(t *testing.T) {
	h := newPoolMsgHandler(t, func(r *http.Request) (int, string, bool) {
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
			t.Errorf("system 转换异常: %+v", body.Messages)
		}
		return 200, sseOK, true
	})
	reqBody := `{"model":"glm-5.2","max_tokens":100,"system":"你是助手","messages":[{"role":"user","content":"hi"}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(reqBody)))
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Type       string `json:"type"`
		Model      string `json:"model"`
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Usage struct {
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if resp.Type != "message" || resp.Model != "glm-5.2" || resp.StopReason != "end_turn" {
		t.Errorf("Anthropic 骨架异常: %+v", resp)
	}
	if len(resp.Content) != 1 || resp.Content[0].Text != "你好" {
		t.Errorf("content 折回异常: %+v", resp.Content)
	}
	if resp.Usage.InputTokens != 1 || resp.Usage.OutputTokens != 1 {
		t.Errorf("usage 折回异常: %+v", resp.Usage)
	}
}

// 流式：Anthropic 请求 → 主池 SSE → 逐帧翻译成 Anthropic 事件序列。
func TestMessagesPoolFallbackStream(t *testing.T) {
	h := newPoolMsgHandler(t, func(r *http.Request) (int, string, bool) {
		return 200, sseOK, true
	})
	reqBody := `{"model":"cn:glm-5.2","max_tokens":100,"stream":true,"messages":[{"role":"user","content":"hi"}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(reqBody)))
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	out := rec.Body.String()
	for _, want := range []string{
		"event: message_start", `"model":"cn:glm-5.2"`, "event: ping",
		"event: content_block_start", `"text":"你好"`, "event: content_block_stop",
		`"stop_reason":"end_turn"`, "event: message_stop",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("流缺事件片段 %q\n完整输出:\n%s", want, out)
		}
	}
	if strings.Contains(out, `"object":"chat.completion.chunk"`) {
		t.Error("OpenAI 原始 chunk 透传泄漏")
	}
	if strings.Contains(out, "data: [DONE]") {
		t.Error("[DONE] 不应透传（Anthropic 以 message_stop 收尾）")
	}
}

// 工具调用轮：Anthropic tools → OpenAI tools；流式 tool_calls → tool_use 块。
func TestMessagesPoolFallbackToolUseStream(t *testing.T) {
	sse := "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"get_weather\",\"arguments\":\"{\\\"city\\\":\\\"北京\\\"}\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n" +
		"data: [DONE]\n\n"
	h := newPoolMsgHandler(t, func(r *http.Request) (int, string, bool) {
		var body struct {
			Tools []struct {
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			} `json:"tools"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if len(body.Tools) != 1 || body.Tools[0].Function.Name != "get_weather" {
			t.Errorf("tools 转换异常: %+v", body.Tools)
		}
		return 200, sse, true
	})
	reqBody := `{"model":"glm-5.2","max_tokens":100,"stream":true,
		"tools":[{"name":"get_weather","description":"查天气","input_schema":{"type":"object","properties":{"city":{"type":"string"}}}}],
		"messages":[{"role":"user","content":"北京天气"}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(reqBody)))
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	out := rec.Body.String()
	for _, want := range []string{
		`"type":"tool_use"`, `"name":"get_weather"`, `"id":"call_1"`,
		`"type":"input_json_delta"`, "北京", `"stop_reason":"tool_use"`, "event: message_stop",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("流缺事件片段 %q\n完整输出:\n%s", want, out)
		}
	}
}

// 路由优先级回归：裸名唯一命中 ext 渠道时仍走 ext（回退只兜底非 ext 模型）。
// 渠道不设鉴权（本测试只验证路由归属，不验鉴权头）。
func TestMessagesBareNameStillPrefersExt(t *testing.T) {
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
		Upstream: newPoolMsgUpstream(t, func(r *http.Request) (int, string, bool) {
			poolCalled = true
			return 200, sseOK, true
		}),
		Ext: mgr,
	})
	reqBody := `{"model":"glm-5.2","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(reqBody)))
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if !extCalled {
		t.Fatal("应路由到 ext 渠道")
	}
	if poolCalled {
		t.Error("裸名唯一命中 ext 渠道时不应落到主池")
	}
	var resp struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Model != "ext:sf/glm-5.2" {
		t.Errorf("响应 model 应带渠道前缀: %q", resp.Model)
	}
}

// 主池不可用（空池）：错误折回 Anthropic 信封，type 与状态码映射一致。
func TestMessagesPoolFallbackErrorShape(t *testing.T) {
	h := NewHandler(Config{
		Pool: testPoolWith(), // 空池
		Upstream: newPoolMsgUpstream(t, func(r *http.Request) (int, string, bool) {
			t.Error("空池不应打出站请求")
			return 200, sseOK, true
		}),
	})
	reqBody := `{"model":"glm-5.2","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(reqBody)))
	if rec.Code < 400 {
		t.Fatalf("status %d, want >=400: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Type != "error" || resp.Error.Type == "" || resp.Error.Message == "" {
		t.Errorf("Anthropic 错误信封异常: %+v", resp)
	}
	if resp.Error.Type != anthropicErrTypeFor(rec.Code) {
		t.Errorf("错误类型映射不一致: got %q, want %q", resp.Error.Type, anthropicErrTypeFor(rec.Code))
	}
}

// ext: 前缀指向不存在的渠道：仍 400（路由协议错误不回主池）。
// 注意 handler 必须配置了 Ext 管理器（哪怕为空）——Ext==nil 时前缀路由根本
// 不发生，按设计直接落主池。
func TestMessagesExtPrefixUnknownChannel(t *testing.T) {
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", ExpiresAt: 9999999999}),
		Upstream: newPoolMsgUpstream(t, func(r *http.Request) (int, string, bool) { return 200, sseOK, true }),
		Ext:      extchan.New(extchan.Config{TimeoutSeconds: 5}),
	})
	reqBody := `{"model":"ext:nope/glm-5.2","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(reqBody)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400: %s", rec.Code, rec.Body.String())
	}
}

// 流中错误帧：error 事件透出后不再补收尾骨架（与 ext 路径同语义，
// error 之后接 message_delta/message_stop 是畸形流）。
func TestMessagesPoolFallbackStreamErrorFrame(t *testing.T) {
	sse := "data: {\"error\":{\"message\":\"quota exceeded\"}}\n\n" +
		"data: [DONE]\n\n"
	h := newPoolMsgHandler(t, func(r *http.Request) (int, string, bool) {
		return 200, sse, true
	})
	reqBody := `{"model":"glm-5.2","max_tokens":100,"stream":true,"messages":[{"role":"user","content":"hi"}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(reqBody)))
	out := rec.Body.String()
	if !strings.Contains(out, "event: error") || !strings.Contains(out, "quota exceeded") {
		t.Errorf("流中错误帧应透出 error 事件: %s", out)
	}
	if strings.Contains(out, "event: message_stop") || strings.Contains(out, "event: message_delta") {
		t.Errorf("error 事件之后不应再补收尾骨架: %s", out)
	}
}

// 桥接器半帧拼接：一帧分两次 Write 到达也能正确翻译（钉住跨 Write 缓冲语义，
// 防止退回 [:0] 复用底层数组覆盖半帧前缀的回归）。
func TestMessagesPoolBridgeSplitFrame(t *testing.T) {
	rec := httptest.NewRecorder()
	bridge := &poolAnthropicBridge{ResponseWriter: rec, display: "glm-5.2"}
	bridge.mode = 1 // 直接置 SSE 模式（模拟首写检测已完成后的纯翻译路径）
	bridge.x = newAnthropicStreamXlator("glm-5.2", anthropicEventWriter(rec, rec))
	frame := "data: {\"choices\":[{\"delta\":{\"content\":\"你\"}}]}\n\n"
	half := len(frame) / 2
	if _, err := bridge.Write([]byte(frame[:half])); err != nil {
		t.Fatal(err)
	}
	if _, err := bridge.Write([]byte(frame[half:])); err != nil {
		t.Fatal(err)
	}
	bridge.finish()
	out := rec.Body.String()
	if !strings.Contains(out, `"text":"你"`) || !strings.Contains(out, "event: message_stop") {
		t.Errorf("半帧拼接翻译失败: %s", out)
	}
}
