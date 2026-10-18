package provider

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// LegacyOpenAIRoot 必须逐字节等价于历史 endpoint()（老配置零回归）。
func TestLegacyOpenAIRootEquivalence(t *testing.T) {
	// 与 internal/extchan 阶段一的 TestEndpoint 同一组样本 + 额外边界。
	cases := map[string]string{
		"https://api.x.cn":           "https://api.x.cn/v1",
		"https://api.x.cn/":          "https://api.x.cn/v1",
		"https://api.x.cn/v1":        "https://api.x.cn/v1",
		"https://api.x.cn/v1/":       "https://api.x.cn/v1",
		"https://api.x.cn/openai/v1": "https://api.x.cn/openai/v1",
		"  https://api.x.cn  ":       "https://api.x.cn/v1",
		"https://ark/api/v3":         "https://ark/api/v3/v1", // 历史怪规则：没有 /v1 就补
		"https://h/v1beta":           "https://h/v1beta",      // 含 /v1 子串即不补（历史规则）
		"http://127.0.0.1:8080/v1":   "http://127.0.0.1:8080/v1",
		"https://h/a/v1/b":           "https://h/a/v1/b", // 含 /v1 即不补
	}
	for in, want := range cases {
		if got := LegacyOpenAIRoot(in); got != want {
			t.Errorf("LegacyOpenAIRoot(%q) = %q, want %q", in, got, want)
		}
	}
	for in := range cases {
		if got, want := LegacyOpenAIRoot(in)+"/chat/completions", LegacyOpenAIRoot(in)+"/chat/completions"; got != want {
			t.Errorf("chat url mismatch: %q != %q", got, want)
		}
	}
}

// BuildChatURL：三协议默认模板 + path 覆盖 + 流式形态。
func TestBuildChatURL(t *testing.T) {
	openai := &Target{Protocol: ProtocolOpenAI, BaseURL: "https://api.sf.cn"}
	if got := BuildChatURL(openai, "m", false); got != "https://api.sf.cn/v1/chat/completions" {
		t.Errorf("openai url = %q", got)
	}
	ant := &Target{Protocol: ProtocolAnthropic, BaseURL: "https://api.anthropic.com"}
	if got := BuildChatURL(ant, "claude", false); got != "https://api.anthropic.com/v1/messages" {
		t.Errorf("anthropic url = %q", got)
	}
	gem := &Target{Protocol: ProtocolGemini, BaseURL: "https://generativelanguage.googleapis.com"}
	if got := BuildChatURL(gem, "gemini-2.0-flash", false); got != "https://generativelanguage.googleapis.com/v1beta/models/gemini-2.0-flash:generateContent" {
		t.Errorf("gemini url = %q", got)
	}
	if got := BuildChatURL(gem, "gemini-2.0-flash", true); !strings.HasSuffix(got, ":streamGenerateContent?alt=sse") {
		t.Errorf("gemini stream url = %q", got)
	}
	// path 覆盖：不再追加版本段（火山 Ark 类形态）。
	ark := &Target{Protocol: ProtocolOpenAI, BaseURL: "https://ark/api/v3", Path: "/chat/completions"}
	if got := BuildChatURL(ark, "m", false); got != "https://ark/api/v3/chat/completions" {
		t.Errorf("ark url = %q", got)
	}
	// {model} 占位。
	tpl := &Target{Protocol: ProtocolOpenAI, BaseURL: "https://h", Path: "/custom/{model}:gen"}
	if got := BuildChatURL(tpl, "a/b", false); !strings.Contains(got, "a%2Fb") {
		t.Errorf("{model} 未做 URL 转义: %q", got)
	}
}

// OpenAI 适配器：请求字节透传 + 鉴权注入 + 模型目录解析。
func TestOpenAIAdapter(t *testing.T) {
	a := NewOpenAIAdapter()
	body := []byte(`{"model":"m1","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	target := &Target{
		Name: "sf", Protocol: ProtocolOpenAI, BaseURL: "https://api.sf.cn",
		Auth: AuthSpec{Mode: AuthBearer}, Secret: "sk-1", Timeout: time.Second,
	}
	req, err := a.BuildRequest(context.Background(), target, body, true)
	if err != nil {
		t.Fatal(err)
	}
	if req.URL.String() != "https://api.sf.cn/v1/chat/completions" {
		t.Errorf("url = %q", req.URL)
	}
	if req.Header.Get("Authorization") != "Bearer sk-1" {
		t.Errorf("auth header = %q", req.Header.Get("Authorization"))
	}
	if req.Header.Get("Accept") != "text/event-stream" {
		t.Errorf("Accept = %q", req.Header.Get("Accept"))
	}
	gotBody, _ := io.ReadAll(req.Body)
	if string(gotBody) != string(body) {
		t.Errorf("body 应逐字节透传")
	}

	// 静态头注入。
	target.Headers = map[string]string{"X-Title": "StarPort"}
	req2, _ := a.BuildRequest(context.Background(), target, body, false)
	if req2.Header.Get("X-Title") != "StarPort" {
		t.Error("静态头未注入")
	}

	// 模型目录解析。
	models, err := a.DecodeModels(target, 200, []byte(`{"data":[{"id":"m1"},{"id":"m2"}]}`))
	if err != nil || len(models) != 2 || models[0] != "m1" {
		t.Errorf("DecodeModels = %v, %v", models, err)
	}
	if _, err := a.DecodeModels(target, http.StatusUnauthorized, []byte(`{"error":{"message":"bad"}}`)); err == nil {
		t.Error("非 200 应返回错误")
	}
}

// 响应原样透传（含流）：不重建帧，保住第三方私有字段。
func TestOpenAIConvertPassthrough(t *testing.T) {
	a := NewOpenAIAdapter()
	raw := []byte("data: {\"x\":1}\n\ndata: [DONE]\n\n")
	if got := a.ConvertResponse(nil, 200, raw); string(got) != string(raw) {
		t.Error("ConvertResponse 应原样返回")
	}
	rc := io.NopCloser(strings.NewReader(string(raw)))
	out, err := a.ConvertStream(nil, "text/event-stream", rc)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(out)
	if string(b) != string(raw) {
		t.Error("ConvertStream 应原样透传")
	}
}
