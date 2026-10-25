// protocol_matrix_test.go 协议一致性矩阵的补全行：三入站 × 四出站共 12 格，
// 其中 6 格（chat/messages 入站 × openai/anthropic/gemini 出站）已在
// extmatrix_test.go；本文件补齐剩余 6 格：
//
//	Responses 入站 × {openai, anthropic, gemini} 出站   —— Codex CLI 直连任意标准上游
//	responses 出站（codex 适配器承载）× {chat, messages, responses} 入站
//	                                                     —— 任意客户端直连 Codex 订阅
//
// 断言口径与 extmatrix 完全一致：上游收到的是**上游模型名**、客户端看到的是
// ext:<渠道>/<模型>、usage 数值跨协议不漂移。表驱动收口的目的（README 路线图
// 「协议一致性矩阵」）：新增协议面/平台时漏掉某个组合，CI 直接红——output_index
// 跳号（2026-10 修复）正属这一族回归。
package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/RisingInsight/startport/internal/extchan"
)

// postResponses 发一次 /v1/responses 请求。
func postResponses(t *testing.T, h *Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// ── 入站 Responses × {openai, anthropic, gemini} 出站 ────────────────

// Responses 入站通用断言：status=completed、文本折叠、usage 不漂移、model 回写。
func assertResponsesShape(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Status string `json:"status"`
		Model  string `json:"model"`
		Output []struct {
			Type    string `json:"type"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
		Usage *struct {
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
			TotalTokens  int64 `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse responses: %v\n%s", err, rec.Body.String())
	}
	if resp.Status != "completed" {
		t.Errorf("status = %q want completed", resp.Status)
	}
	if resp.Model != "ext:ch/up-model" {
		t.Errorf("客户端可见 model = %q", resp.Model)
	}
	if len(resp.Output) != 1 || resp.Output[0].Type != "message" ||
		len(resp.Output[0].Content) != 1 || resp.Output[0].Content[0].Text != "你好" {
		t.Errorf("文本折叠异常: %+v", resp.Output)
	}
	if resp.Usage == nil || resp.Usage.InputTokens != 10 || resp.Usage.OutputTokens != 3 ||
		resp.Usage.TotalTokens != 13 {
		t.Errorf("usage 跨协议漂移: %+v", resp.Usage)
	}
}

// RESP → OAI：非流式整包 + 流式事件骨架（created → delta → completed）。
func TestMatrixResponsesInToOpenAI(t *testing.T) {
	h, _ := matrixHandler(t, "openai", openAIUpstream(t))
	rec := postResponses(t, h, `{"model":"ext:ch/up-model","input":"hi"}`)
	assertResponsesShape(t, rec)

	rec = postResponses(t, h, `{"model":"ext:ch/up-model","stream":true,"input":"hi"}`)
	out := rec.Body.String()
	for _, want := range []string{
		"event: response.created", "event: response.output_text.delta", `"delta":"你"`,
		`"delta":"好"`, "event: response.completed", `"input_tokens":10`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("流缺片段 %s\n完整输出:\n%s", want, out)
		}
	}
	if strings.Contains(out, `"object":"chat.completion.chunk"`) {
		t.Error("OpenAI 原始 chunk 泄漏到 Responses 客户端")
	}
}

// RESP → ANT：工具声明与工具调用双向折叠（扁平 function → anthropic tools 声明；
// anthropic tool_use 块 → function_call 输出项）。
func TestMatrixResponsesInToAnthropicTools(t *testing.T) {
	up := func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Model != "up-model" {
			t.Errorf("上游应收 up-model, got %q", body.Model)
		}
		if len(body.Tools) != 1 || body.Tools[0].Name != "shell" {
			t.Errorf("tools 声明应转 Anthropic 形态: %+v", body.Tools)
		}
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant",
		  "content":[{"type":"text","text":"跑一下"},{"type":"tool_use","id":"toolu_1","name":"shell","input":{"cmd":"ls"}}],
		  "stop_reason":"tool_use","usage":{"input_tokens":10,"output_tokens":3}}`))
	}
	h, _ := matrixHandler(t, "anthropic", up)
	rec := postResponses(t, h, `{"model":"ext:ch/up-model","input":"列目录",
	  "tools":[{"type":"function","name":"shell","description":"跑命令","parameters":{"type":"object"}}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Status string `json:"status"`
		Output []struct {
			Type    string `json:"type"`
			CallID  string `json:"call_id"`
			Name    string `json:"name"`
			Args    string `json:"arguments"` // Responses 协议：arguments 是 JSON 编码后的字符串
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
		Usage *struct {
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
			TotalTokens  int64 `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Output) != 2 {
		t.Fatalf("应折叠 text + function_call 两个输出项: %+v", resp.Output)
	}
	if resp.Output[0].Type != "message" || resp.Output[0].Content[0].Text != "跑一下" {
		t.Errorf("文本项异常: %+v", resp.Output[0])
	}
	fc := resp.Output[1]
	if fc.Type != "function_call" || fc.CallID != "toolu_1" || fc.Name != "shell" ||
		strings.TrimSpace(fc.Args) != `{"cmd":"ls"}` {
		t.Errorf("tool_use → function_call 折叠异常: %+v args=%q", fc, fc.Args)
	}
	if resp.Usage == nil || resp.Usage.InputTokens != 10 || resp.Usage.OutputTokens != 3 {
		t.Errorf("usage 漂移: %+v", resp.Usage)
	}
}

// RESP → GEM：文本与 usage 跨两套语义体系不漂移。
func TestMatrixResponsesInToGemini(t *testing.T) {
	h, _ := matrixHandler(t, "gemini", geminiUpstream(t))
	rec := postResponses(t, h, `{"model":"ext:ch/up-model","input":"hi"}`)
	assertResponsesShape(t, rec)
}

// ── responses 出站（codex 适配器）× 三入站 ───────────────────────────

// matrixCodexHandler 构造 responses 出站渠道（codex 适配器承载；secret 用裸
// access_token 形态——parseCodexSecret 的两种凭证形态之一）。
func matrixCodexHandler(t *testing.T, up http.HandlerFunc) *Handler {
	t.Helper()
	srv := httptest.NewServer(up)
	t.Cleanup(srv.Close)
	mgr := extchan.New(extchan.Config{TimeoutSeconds: 5})
	mgr.ReplaceAll([]extchan.ChannelDef{{
		Name: "ch", BaseURL: srv.URL, Protocol: "responses", Adapter: "codex",
		Models:      []string{"up-model"},
		Credentials: []extchan.CredentialDef{{Label: "p", Secret: matrixSecret}},
	}})
	return NewHandler(Config{Ext: mgr})
}

// responsesOutUpstream 伪造 Codex 后端：校验 chat→Responses 请求形态（恒 SSE、
// 不落库、system → instructions、user → input_text、模型名改写），回一段标准
// Responses 事件流（文本增量 + usage）。
func responsesOutUpstream(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/responses") {
			t.Errorf("Codex 上游路径异常: %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer "+matrixSecret {
			t.Errorf("Codex 上游鉴权异常: %q", r.Header.Get("Authorization"))
		}
		if r.Header.Get("originator") == "" {
			t.Error("Codex 上游缺 originator 头")
		}
		var body struct {
			Model        string `json:"model"`
			Stream       bool   `json:"stream"`
			Store        bool   `json:"store"`
			Instructions string `json:"instructions"`
			Input        []struct {
				Type    string `json:"type"`
				Role    string `json:"role"`
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Model != "up-model" {
			t.Errorf("上游应收 up-model, got %q", body.Model)
		}
		if !body.Stream {
			t.Error("Codex 后端请求必须恒为 SSE（stream=true）")
		}
		if body.Store {
			t.Error("Codex 后端请求不得落库（store=false）")
		}
		if !strings.Contains(body.Instructions, "sys") {
			t.Errorf("system 应抽取为 instructions: %q", body.Instructions)
		}
		if len(body.Input) != 1 || body.Input[0].Role != "user" ||
			len(body.Input[0].Content) != 1 || body.Input[0].Content[0].Text != "hi" {
			t.Errorf("user 消息应转 input 项: %+v", body.Input)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		// completed 事件携带**完整最终 response**（aggregateResponsesSSE 取该对象
		// 聚合，不累加 delta）；delta 事件仅供流式客户端消费。
		_, _ = w.Write([]byte(
			"event: response.output_item.added\ndata: {\"item\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\"}}\n\n" +
				"event: response.output_text.delta\ndata: {\"delta\":\"你\"}\n\n" +
				"event: response.output_text.delta\ndata: {\"delta\":\"好\"}\n\n" +
				"event: response.completed\ndata: {\"response\":{\"id\":\"resp_1\",\"status\":\"completed\"," +
				"\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"你好\"}]}]," +
				"\"usage\":{\"input_tokens\":10,\"output_tokens\":3,\"total_tokens\":13}}}\n\n"))
	}
}

// CHAT → RESP（出站）：非流式客户端由适配器聚合 SSE，usage 与 model 不漂移。
func TestMatrixChatInToResponsesOut(t *testing.T) {
	h := matrixCodexHandler(t, responsesOutUpstream(t))
	rec := postChat(t, h, `{"model":"ext:ch/up-model",
	  "messages":[{"role":"system","content":"sys"},{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
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
	if len(resp.Choices) != 1 || resp.Choices[0].Message.Content != "你好" {
		t.Errorf("聚合文本异常: %+v", resp.Choices)
	}
	if resp.Usage.PromptTokens != 10 || resp.Usage.CompletionTokens != 3 || resp.Usage.TotalTokens != 13 {
		t.Errorf("usage 漂移: %+v", resp.Usage)
	}

	// 流式：Responses 事件 → chat chunks，末尾 [DONE]。
	rec = postChat(t, h, `{"model":"ext:ch/up-model","stream":true,
	  "messages":[{"role":"system","content":"sys"},{"role":"user","content":"hi"}]}`)
	out := rec.Body.String()
	if rec.Code != 200 || !strings.Contains(out, `"content":"你"`) || !strings.Contains(out, `"content":"好"`) ||
		!strings.Contains(out, "data: [DONE]") {
		t.Fatalf("流式失败 %d: %s", rec.Code, out)
	}
	if strings.Contains(out, "response.output_text.delta") {
		t.Error("Responses 原始事件泄漏到 chat 客户端")
	}
}

// MESSAGES → RESP（出站）：Responses 聚合结果折回 Anthropic message。
func TestMatrixMessagesInToResponsesOut(t *testing.T) {
	h := matrixCodexHandler(t, responsesOutUpstream(t))
	rec := postMessages(t, h, antInboundBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Model  string `json:"model"`
		Role   string `json:"role"`
		Stop   string `json:"stop_reason"`
		Blocks []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Usage *struct {
			Input  int64 `json:"input_tokens"`
			Output int64 `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Model != "ext:ch/up-model" {
		t.Errorf("客户端可见 model = %q", resp.Model)
	}
	if len(resp.Blocks) != 1 || resp.Blocks[0].Type != "text" || resp.Blocks[0].Text != "你好" {
		t.Errorf("文本块异常: %+v", resp.Blocks)
	}
	if resp.Stop != "end_turn" {
		t.Errorf("stop_reason = %q", resp.Stop)
	}
	if resp.Usage == nil || resp.Usage.Input != 10 || resp.Usage.Output != 3 {
		t.Errorf("usage 漂移: %+v", resp.Usage)
	}
}

// RESP → RESP（出站）：双向转换互为镜像，客户端拿回的仍是合法 Responses 对象。
func TestMatrixResponsesInToResponsesOut(t *testing.T) {
	h := matrixCodexHandler(t, responsesOutUpstream(t))
	rec := postResponses(t, h, `{"model":"ext:ch/up-model","instructions":"sys","input":"hi"}`)
	assertResponsesShape(t, rec)
}
