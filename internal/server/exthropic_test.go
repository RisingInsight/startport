package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/RisingInsight/startport/internal/extchan"
)

// newExtHandler 构造带 mock ext 渠道的 handler（直连 httptest 上游）。
func newExtHandler(t *testing.T, upstreamHandler http.HandlerFunc) (*Handler, *httptest.Server) {
	t.Helper()
	up := httptest.NewServer(upstreamHandler)
	t.Cleanup(up.Close)
	mgr := extchan.New(extchan.Config{TimeoutSeconds: 5})
	mgr.ReplaceAll([]extchan.ChannelDef{{
		Name:    "sf",
		BaseURL: up.URL,
		APIKey:  "sk-ext-test",
		Models:  []string{"glm-5.2", "deepseek-v4"},
	}})
	h := NewHandler(Config{Ext: mgr})
	return h, up
}

// anthropicSync 非流式请求 → Anthropic 响应结构折回。
func TestExtMessagesSync(t *testing.T) {
	h, _ := newExtHandler(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sk-ext-test" {
			w.WriteHeader(401)
			_, _ = w.Write([]byte(`{"error":{"message":"bad key"}}`))
			return
		}
		var body struct {
			Model    string `json:"model"`
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
			Stream bool `json:"stream"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Model != "glm-5.2" {
			t.Errorf("上游应收裸模型名 glm-5.2, got %q", body.Model)
		}
		if len(body.Messages) != 2 || body.Messages[0].Role != "system" {
			t.Errorf("system 消息应转为首条 system: %+v", body.Messages)
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"你好"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":3,"total_tokens":13}}`))
	})
	body := `{"model":"ext:sf/glm-5.2","max_tokens":100,"system":"你是助手","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Type       string `json:"type"`
		Role       string `json:"role"`
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
	if resp.Type != "message" || resp.Role != "assistant" || resp.StopReason != "end_turn" {
		t.Errorf("Anthropic 骨架字段异常: %+v", resp)
	}
	if len(resp.Content) != 1 || resp.Content[0].Text != "你好" {
		t.Errorf("content 折回异常: %+v", resp.Content)
	}
	if resp.Usage.InputTokens != 10 || resp.Usage.OutputTokens != 3 {
		t.Errorf("usage 折回异常: %+v", resp.Usage)
	}
	if !strings.HasPrefix(resp.Model, "ext:sf/") {
		t.Errorf("响应 model 应带渠道前缀: %q", resp.Model)
	}
}

// tool_use 折回：OpenAI tool_calls → Anthropic tool_use 块；finish_reason 映射。
func TestExtMessagesToolUse(t *testing.T) {
	h, _ := newExtHandler(t, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Tools []struct {
				Function struct {
					Name       string          `json:"name"`
					Parameters json.RawMessage `json:"parameters"`
				} `json:"function"`
			} `json:"tools"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if len(body.Tools) != 1 || body.Tools[0].Function.Name != "get_weather" {
			t.Errorf("tools 转换异常: %+v", body.Tools)
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"北京\"}"}}]},"finish_reason":"tool_calls"}]}`))
	})
	body := `{"model":"ext:sf/glm-5.2","max_tokens":100,
		"tools":[{"name":"get_weather","description":"查天气","input_schema":{"type":"object","properties":{"city":{"type":"string"}}}}],
		"messages":[{"role":"user","content":"北京天气"}]}`
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type  string          `json:"type"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.StopReason != "tool_use" {
		t.Errorf("finish_reason=tool_calls 应映射 tool_use, got %q", resp.StopReason)
	}
	if len(resp.Content) != 1 || resp.Content[0].Type != "tool_use" ||
		resp.Content[0].Name != "get_weather" || !strings.Contains(string(resp.Content[0].Input), "北京") {
		t.Errorf("tool_use 块折回异常: %+v", resp.Content)
	}
}

// 流式：OpenAI SSE → Anthropic 事件序列（message_start → deltas → message_stop）。
func TestExtMessagesStreamEvents(t *testing.T) {
	h, _ := newExtHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"你\"}}]}\n\n" +
			"data: {\"choices\":[{\"delta\":{\"content\":\"好\"}}]}\n\n" +
			"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":2,\"total_tokens\":7}}\n\n" +
			"data: [DONE]\n\n"))
	})
	body := `{"model":"ext:sf/glm-5.2","max_tokens":100,"stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	out := rec.Body.String()
	for _, want := range []string{
		"event: message_start", "event: ping", "event: content_block_start",
		`"text":"你"`, `"text":"好"`, `"type":"text_delta"`,
		"event: content_block_stop", `"stop_reason":"end_turn"`, "event: message_stop",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("流缺事件片段 %q\n完整输出:\n%s", want, out)
		}
	}
	// 非法事件不应出现（OpenAI 原始帧不得透传）。
	if strings.Contains(out, `"object":"chat.completion.chunk"`) {
		t.Error("OpenAI 原始 chunk 透传泄漏")
	}
}

// 上游错误折回 Anthropic 形态：429 → rate_limit_error。
func TestExtMessagesErrorShape(t *testing.T) {
	h, _ := newExtHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"error":{"message":"too many requests"}}`))
	})
	body := `{"model":"ext:sf/glm-5.2","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 429 {
		t.Fatalf("status %d", rec.Code)
	}
	var resp struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Type != "error" || resp.Error.Type != "rate_limit_error" ||
		resp.Error.Message != "too many requests" {
		t.Errorf("Anthropic 错误形态异常: %+v", resp)
	}
}

// 未配置 ext 渠道：/v1/messages 501（明确语义而非误路由 CodeBuddy）。
func TestExtMessagesNoExt(t *testing.T) {
	h := NewHandler(Config{})
	body := `{"model":"whatever","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status %d, want 501", rec.Code)
	}
}

// 工具结果轮：Anthropic tool_result → OpenAI role=tool（顺序约束）。
func TestExtMessagesToolResultRound(t *testing.T) {
	h, _ := newExtHandler(t, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []struct {
				Role    string `json:"role"`
				Content any    `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		// 期望：user → assistant(tool_calls) → tool
		if len(body.Messages) != 3 || body.Messages[2].Role != "tool" {
			b, _ := json.Marshal(body.Messages)
			t.Errorf("消息序列异常: %s", b)
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}]}`))
	})
	body := `{"model":"ext:sf/glm-5.2","max_tokens":100,"messages":[
		{"role":"user","content":"北京天气"},
		{"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"get_weather","input":{"city":"北京"}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":"晴 25 度"}]}
	]}`
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
}
