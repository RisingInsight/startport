package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/RisingInsight/startport/internal/extchan"
)

// 六格矩阵集成测试：2 入站 × 3 出站 × （流式/非流式）。
// 断言三件事：上游收到的是**上游模型名**、客户端收到的是 **ext:<渠道>/<模型>**、
// usage 数值与上游一致。

const matrixSecret = "sk-matrix"

// matrixHandler 构造单渠道 handler（channel 名固定 "ch"）。
func matrixHandler(t *testing.T, protocol string, up http.HandlerFunc) (*Handler, *extchan.Manager) {
	t.Helper()
	srv := httptest.NewServer(up)
	t.Cleanup(srv.Close)
	mgr := extchan.New(extchan.Config{TimeoutSeconds: 5})
	mgr.ReplaceAll([]extchan.ChannelDef{{
		Name: "ch", BaseURL: srv.URL, Protocol: protocol,
		Models:      []string{"up-model"},
		Credentials: []extchan.CredentialDef{{Label: "p", Secret: matrixSecret}},
	}})
	return NewHandler(Config{Ext: mgr}), mgr
}

// openAIUpstream OpenAI 兼容上游（非流式/流式按 body.stream）。
func openAIUpstream(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/v1/chat/completions") {
			t.Errorf("OpenAI 上游路径异常: %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer "+matrixSecret {
			t.Errorf("OpenAI 上游鉴权异常: %q", r.Header.Get("Authorization"))
		}
		var body struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Model != "up-model" {
			t.Errorf("上游应收上游模型名 up-model, got %q", body.Model)
		}
		if body.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"你\"}}]}\n\n" +
				"data: {\"choices\":[{\"delta\":{\"content\":\"好\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":3,\"total_tokens\":13}}\n\n" +
				"data: [DONE]\n\n"))
			return
		}
		_, _ = w.Write([]byte(`{"id":"x","model":"up-model","choices":[{"message":{"role":"assistant","content":"你好"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":3,"total_tokens":13}}`))
	}
}

// anthropicUpstream Anthropic Messages 上游。
func anthropicUpstream(t *testing.T, wantSystem string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/v1/messages") {
			t.Errorf("Anthropic 上游路径异常: %s", r.URL.Path)
		}
		if r.Header.Get("x-api-key") != matrixSecret {
			t.Errorf("Anthropic 上游鉴权异常: %q", r.Header.Get("x-api-key"))
		}
		if r.Header.Get("anthropic-version") == "" {
			t.Error("Anthropic 上游缺 anthropic-version 头")
		}
		var body struct {
			Model     string `json:"model"`
			System    string `json:"system"`
			MaxTokens int    `json:"max_tokens"`
			Stream    bool   `json:"stream"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Model != "up-model" {
			t.Errorf("上游应收 up-model, got %q", body.Model)
		}
		if body.MaxTokens <= 0 {
			t.Error("Anthropic 请求 max_tokens 必填")
		}
		if wantSystem != "" && body.System != wantSystem {
			t.Errorf("system 抽取异常: %q, want %q", body.System, wantSystem)
		}
		if body.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(`event: message_start
data: {"type":"message_start","message":{"usage":{"input_tokens":10}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"你"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"好"}}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}

event: message_stop
data: {"type":"message_stop"}

`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant",
		  "content":[{"type":"text","text":"你好"}],"stop_reason":"end_turn",
		  "usage":{"input_tokens":10,"output_tokens":3}}`))
	}
}

// geminiUpstream Gemini 上游（流式必须 SSE）。
func geminiUpstream(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/v1beta/models/up-model:") {
			t.Errorf("Gemini 上游路径异常: %s", r.URL.Path)
		}
		if r.Header.Get("x-goog-api-key") != matrixSecret {
			t.Errorf("Gemini 上游鉴权异常: %q", r.Header.Get("x-goog-api-key"))
		}
		if strings.Contains(r.URL.Path, ":streamGenerateContent") {
			if r.URL.Query().Get("alt") != "sse" {
				t.Errorf("Gemini 流式必须带 alt=sse: %s", r.URL)
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"你\"}]}}]}\n\n" +
				"data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"好\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":10,\"candidatesTokenCount\":3,\"totalTokenCount\":13}}\n\n"))
			return
		}
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"你好"}]},"finishReason":"STOP"}],
		  "usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":3,"totalTokenCount":13}}`))
	}
}

// ── 入站 OpenAI ─────────────────────────────────────────────────────

func TestMatrixOpenAIToOpenAI(t *testing.T) {
	h, _ := matrixHandler(t, "openai", openAIUpstream(t))
	rec := postChat(t, h, `{"model":"ext:ch/up-model","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"content":"你好"`) {
		t.Fatalf("非流式失败 %d: %s", rec.Code, rec.Body.String())
	}
	rec = postChat(t, h, `{"model":"ext:ch/up-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"content":"你"`) ||
		!strings.Contains(rec.Body.String(), "data: [DONE]") {
		t.Fatalf("流式失败 %d: %s", rec.Code, rec.Body.String())
	}
}

func TestMatrixOpenAIToAnthropic(t *testing.T) {
	h, _ := matrixHandler(t, "anthropic", anthropicUpstream(t, ""))
	rec := postChat(t, h, `{"model":"ext:ch/up-model","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
			TotalTokens      int64 `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Model != "ext:ch/up-model" {
		t.Errorf("客户端可见 model = %q", resp.Model)
	}
	if len(resp.Choices) != 1 || resp.Choices[0].Message.Content != "你好" ||
		resp.Choices[0].FinishReason != "stop" {
		t.Errorf("转换响应异常: %+v", resp.Choices)
	}
	if resp.Usage.PromptTokens != 10 || resp.Usage.CompletionTokens != 3 || resp.Usage.TotalTokens != 13 {
		t.Errorf("usage 映射异常: %+v", resp.Usage)
	}

	rec = postChat(t, h, `{"model":"ext:ch/up-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	out := rec.Body.String()
	if rec.Code != 200 || !strings.Contains(out, `"content":"你"`) || !strings.Contains(out, "data: [DONE]") {
		t.Fatalf("流式失败 %d: %s", rec.Code, out)
	}
	// OpenAI 原始 Anthropic 事件不得泄漏给客户端。
	if strings.Contains(out, "content_block_delta") {
		t.Error("Anthropic 事件泄漏到 OpenAI 客户端")
	}
}

func TestMatrixOpenAIToGemini(t *testing.T) {
	h, _ := matrixHandler(t, "gemini", geminiUpstream(t))
	rec := postChat(t, h, `{"model":"ext:ch/up-model","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"content":"你好"`) {
		t.Fatalf("非流式失败 %d: %s", rec.Code, rec.Body.String())
	}
	rec = postChat(t, h, `{"model":"ext:ch/up-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	out := rec.Body.String()
	if rec.Code != 200 || !strings.Contains(out, `"content":"你"`) || !strings.Contains(out, `"content":"好"`) ||
		!strings.Contains(out, "data: [DONE]") {
		t.Fatalf("流式失败 %d: %s", rec.Code, out)
	}
}

// ── 入站 Anthropic ──────────────────────────────────────────────────

// postMessages 发一次 /v1/messages 请求。
func postMessages(t *testing.T, h *Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

const antInboundBody = `{"model":"ext:ch/up-model","max_tokens":100,"system":"sys",
  "messages":[{"role":"user","content":"hi"}]}`

// ANT → OAI（既有路径，零回归锚点）。
func TestMatrixAnthropicInboundToOpenAI(t *testing.T) {
	h, _ := matrixHandler(t, "openai", openAIUpstream(t))
	rec := postMessages(t, h, antInboundBody)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Type    string `json:"type"`
		Model   string `json:"model"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
		Usage      struct {
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Type != "message" || resp.Model != "ext:ch/up-model" {
		t.Errorf("Anthropic 响应骨架异常: %+v", resp)
	}
	if len(resp.Content) != 1 || resp.Content[0].Text != "你好" || resp.StopReason != "end_turn" {
		t.Errorf("content 折回异常: %+v", resp)
	}
	if resp.Usage.InputTokens != 10 || resp.Usage.OutputTokens != 3 {
		t.Errorf("usage 折回异常: %+v", resp.Usage)
	}
}

// ANT → ANT（双转换；/v1/messages 对 anthropic 渠道必须可用）。
func TestMatrixAnthropicInboundToAnthropic(t *testing.T) {
	h, _ := matrixHandler(t, "anthropic", anthropicUpstream(t, "sys"))
	rec := postMessages(t, h, antInboundBody)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"text":"你好"`) ||
		!strings.Contains(rec.Body.String(), `"stop_reason":"end_turn"`) {
		t.Errorf("Anthropic→Anthropic 转换异常: %s", rec.Body.String())
	}
	// 流式事件序。
	rec = postMessages(t, h, `{"model":"ext:ch/up-model","max_tokens":100,"stream":true,"system":"sys",
	  "messages":[{"role":"user","content":"hi"}]}`)
	out := rec.Body.String()
	for _, want := range []string{"event: message_start", `"text":"你"`, "event: message_stop"} {
		if !strings.Contains(out, want) {
			t.Errorf("ANT→ANT 流缺片段 %q\n%s", want, out)
		}
	}
}

// ANT → GEM。
func TestMatrixAnthropicInboundToGemini(t *testing.T) {
	h, _ := matrixHandler(t, "gemini", geminiUpstream(t))
	rec := postMessages(t, h, antInboundBody)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"text":"你好"`) {
		t.Errorf("ANT→GEM 转换异常: %s", rec.Body.String())
	}
	rec = postMessages(t, h, `{"model":"ext:ch/up-model","max_tokens":100,"stream":true,
	  "messages":[{"role":"user","content":"hi"}]}`)
	out := rec.Body.String()
	for _, want := range []string{"event: content_block_delta", `"text":"你"`, "event: message_stop"} {
		if !strings.Contains(out, want) {
			t.Errorf("ANT→GEM 流缺片段 %q\n%s", want, out)
		}
	}
}

// 别名在入站响应里回写（PRD P0-6）：请求 ext:ch/fast，上游收到真实模型名，
// 客户端看到的 model 仍是别名。
func TestMatrixAliasRewritesModel(t *testing.T) {
	var upstreamModel string
	up := func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		upstreamModel = body.Model
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}
	srv := httptest.NewServer(http.HandlerFunc(up))
	defer srv.Close()
	mgr := extchan.New(extchan.Config{TimeoutSeconds: 5})
	mgr.ReplaceAll([]extchan.ChannelDef{{
		Name: "ch", BaseURL: srv.URL, Models: []string{"up-model"},
		Aliases:     map[string]string{"fast": "up-model-mini"},
		Credentials: []extchan.CredentialDef{{Label: "p", Secret: "s"}},
	}})
	h := NewHandler(Config{Ext: mgr})
	rec := postChat(t, h, `{"model":"ext:ch/fast","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if upstreamModel != "up-model-mini" {
		t.Errorf("上游应收别名目标模型, got %q", upstreamModel)
	}
	// 非流式响应 model 回写别名（PRD P0-6）。
	if !strings.Contains(rec.Body.String(), `"model":"ext:ch/fast"`) {
		t.Errorf("响应 model 应回写别名: %s", rec.Body.String())
	}
	// /v1/models 暴露别名与 alias_of。
	models := mgr.ModelList(nil)
	found := false
	for _, m := range models {
		if m["id"] == "ext:ch/fast" && m["alias_of"] == "up-model-mini" {
			found = true
		}
	}
	if !found {
		t.Error("/v1/models 未暴露别名 alias_of")
	}
}
