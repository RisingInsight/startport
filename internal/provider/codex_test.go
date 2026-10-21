// codex_test.go Codex 原生适配器契约测试（凭证解析/头组/聚合路径）。
package provider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// idToken 构造带嵌套 chatgpt_account_id 的 JWT 形态（payload 段真实编码）。
func makeIDToken(accountID string) string {
	payload, _ := json.Marshal(map[string]any{
		"email":                       "user@example.com",
		"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": accountID},
	})
	return "eyJhbGciOiJFUzI1NiJ9." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

// readBodyBytes 读取请求体（本分支专用 helper；zcode 分支另有同名 helper，
// 合并时二者取一即可）。
func readBodyBytes(r *http.Request) []byte {
	if r.Body == nil {
		return nil
	}
	defer r.Body.Close()
	buf := make([]byte, 1<<20)
	n, _ := r.Body.Read(buf)
	return buf[:n]
}

// codexAuthFixture auth.json 夹具（类型化构造，序列化产出真实形状）。
type codexAuthFixture struct {
	AuthMode string `json:"auth_mode"`
	Tokens   struct {
		Access  string `json:"access_token"`
		Refresh string `json:"refresh_token"`
		ID      string `json:"id_token"`
		Account string `json:"account_id"`
	} `json:"tokens"`
}

func codexAuthDoc(mode, access, refresh, id string) []byte {
	var f codexAuthFixture
	f.AuthMode = mode
	f.Tokens.Access = access
	f.Tokens.Refresh = refresh
	f.Tokens.ID = id
	raw, _ := json.Marshal(f)
	return raw
}

func TestParseCodexSecretPlain(t *testing.T) {
	cred, err := parseCodexSecret("sk-dummy")
	if err != nil {
		t.Fatalf("plain: %v", err)
	}
	if cred.Access != "sk-dummy" {
		t.Errorf("Access = %q", cred.Access)
	}
}

func TestParseCodexSecretAuthJSON(t *testing.T) {
	cred, err := parseCodexSecret(string(codexAuthDoc("chatgpt", "at-1", "rt-1", makeIDToken("acc-42"))))
	if err != nil {
		t.Fatalf("auth.json: %v", err)
	}
	if cred.Access != "at-1" || cred.Refresh != "rt-1" {
		t.Errorf("tokens = %+v", cred)
	}
	if cred.Account != "acc-42" {
		t.Errorf("account_id 应从 id_token 提取: %q", cred.Account)
	}
}

func TestParseCodexSecretRejects(t *testing.T) {
	if _, err := parseCodexSecret(""); err == nil {
		t.Error("空凭证应报错")
	}
	if _, err := parseCodexSecret(string(codexAuthDoc("api_key", "x", "", ""))); err == nil {
		t.Error("非 chatgpt 模式应显式拒绝")
	}
	if _, err := parseCodexSecret(string(codexAuthDoc("chatgpt", "", "", ""))); err == nil {
		t.Error("缺 access_token 应报错")
	}
}

func TestCodexBuildRequest(t *testing.T) {
	a := newCodexAdapter()
	tgt := &Target{
		"codex", ProtocolResponses, "https://chatgpt.com/backend-api/codex", "",
		nil, AuthSpec{}, string(codexAuthDoc("chatgpt", "at-1", "", makeIDToken("acc-42"))), 0, "",
	}
	body := []byte(`{"model":"gpt-5.6-luna","messages":[{"role":"system","content":"sys"},{"role":"user","content":"hi"}],"reasoning_effort":"high","temperature":0.5}`)
	req, err := a.BuildRequest(context.Background(), tgt, body, false)
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	if req.URL.String() != "https://chatgpt.com/backend-api/codex/responses" {
		t.Errorf("URL = %q", req.URL.String())
	}
	wants := [][2]string{
		{"Authorization", "Bearer at-1"},
		{"originator", "codex_cli_rs"},
		{"OpenAI-Beta", "responses=experimental"},
		{"Accept", "text/event-stream"},
		{"chatgpt-account-id", "acc-42"},
	}
	for _, w := range wants {
		if got := req.Header.Get(w[0]); got != w[1] {
			t.Errorf("%s = %q, want %q", w[0], got, w[1])
		}
	}
	if req.Header.Get("session_id") == "" {
		t.Error("session_id 头缺失")
	}
	var payload map[string]any
	if err := json.Unmarshal(readBodyBytes(req), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["stream"] != true || payload["store"] != false {
		t.Errorf("stream/store 应强制 true/false: %v/%v", payload["stream"], payload["store"])
	}
	if payload["instructions"] != "sys" {
		t.Errorf("instructions = %v", payload["instructions"])
	}
	if _, has := payload["temperature"]; has {
		t.Error("采样参数不应上抛")
	}
	if _, ok := payload["reasoning"].(map[string]any); !ok {
		t.Error("reasoning.effort 应映射")
	}
}

// 非流式客户端路径：上游恒 SSE → ConvertResponse 聚合成 chat envelope。
func TestCodexConvertResponseAggregates(t *testing.T) {
	a := newCodexAdapter()
	sse := []byte("event: response.completed\ndata: {\"response\":{\"id\":\"resp_1\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"答案\"}]}],\"usage\":{\"input_tokens\":3,\"output_tokens\":7,\"total_tokens\":10}}}\n\n")
	out := a.ConvertResponse(&Target{ClientModel: "gpt-5.6-luna"}, http.StatusOK, sse)
	var chat struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens int `json:"prompt_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(out, &chat); err != nil {
		t.Fatalf("aggregate envelope: %v", err)
	}
	if chat.Choices[0].Message.Content != "答案" || chat.Usage.PromptTokens != 3 {
		t.Errorf("聚合结果 = %+v", chat)
	}
}

// 流式路径：Responses SSE → OpenAI chunks 读取器。
func TestCodexConvertStream(t *testing.T) {
	a := newCodexAdapter()
	sse := strings.Join([]string{
		"event: response.output_item.added",
		"data: {\"item\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\"}}",
		"",
		"event: response.output_text.delta",
		"data: {\"delta\":\"ok\"}",
		"",
		"event: response.completed",
		"data: {\"response\":{\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}",
		"",
	}, "\n")
	rc, err := a.ConvertStream(&Target{ClientModel: "gpt-5.6-luna"}, "text/event-stream", io.NopCloser(strings.NewReader(sse)))
	if err != nil {
		t.Fatalf("ConvertStream: %v", err)
	}
	defer rc.Close()
	var text strings.Builder
	buf := make([]byte, 8192)
	for {
		n, rerr := rc.Read(buf)
		if n > 0 {
			text.Write(buf[:n])
		}
		if rerr != nil {
			break
		}
	}
	for _, want := range []string{`"role":"assistant"`, `"content":"ok"`, `"finish_reason":"stop"`, "data: [DONE]"} {
		if !strings.Contains(text.String(), want) {
			t.Errorf("流输出缺 %q: %s", want, text.String())
		}
	}
}

func TestCodexStaticModels(t *testing.T) {
	a := newCodexAdapter()
	models := a.StaticModels()
	if len(models) < 10 || models[0] != "gpt-5.1" {
		t.Errorf("StaticModels = %v", models)
	}
}

// httptest 冒烟：目录端点解码（BuildModels/DecodeModels 对）。
func TestCodexModelsDecode(t *testing.T) {
	a := newCodexAdapter()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/models") {
			t.Errorf("路径 = %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"gpt-5.6-luna"},{"id":"gpt-5.2"}]}`))
	}))
	defer srv.Close()
	tgt := &Target{
		"codex", ProtocolResponses, srv.URL, "",
		nil, AuthSpec{}, "sk-dummy", 0, "",
	}
	req, err := a.BuildModels(context.Background(), tgt)
	if err != nil {
		t.Fatalf("BuildModels: %v", err)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer sk-dummy" {
		t.Errorf("目录鉴权 = %q", got)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	ids, err := a.DecodeModels(tgt, resp.StatusCode, raw)
	if err != nil {
		t.Fatalf("DecodeModels: %v", err)
	}
	if len(ids) != 2 || ids[0] != "gpt-5.6-luna" {
		t.Errorf("ids = %v", ids)
	}
}
