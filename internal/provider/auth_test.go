package provider

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// 四形态鉴权注入位置 + 传输层头黑名单 + 脱敏。
func TestAuthApplyAndScrub(t *testing.T) {
	cases := []struct {
		name    string
		spec    AuthSpec
		secret  string
		wantHdr [2]string // name, value（空 = 不检查头）
		wantQry string    // 期望的 query 片段
	}{
		{"bearer", AuthSpec{Mode: AuthBearer}, "sk-1", [2]string{"Authorization", "Bearer sk-1"}, ""},
		{"x-api-key", AuthSpec{Mode: AuthXAPIKey}, "sk-2", [2]string{"x-api-key", "sk-2"}, ""},
		{"custom", AuthSpec{Mode: AuthCustom, Header: "x-goog-api-key"}, "sk-3", [2]string{"x-goog-api-key", "sk-3"}, ""},
		{"query", AuthSpec{Mode: AuthQuery, Param: "key"}, "sk-4", [2]string{}, "key=sk-4"},
	}
	for _, tc := range cases {
		u, _ := url.Parse("https://api.example.com/v1/chat")
		h := http.Header{}
		Apply(h, u, tc.spec, tc.secret)
		if tc.wantHdr[0] != "" && h.Get(tc.wantHdr[0]) != tc.wantHdr[1] {
			t.Errorf("%s: header %s=%q, want %q", tc.name, tc.wantHdr[0], h.Get(tc.wantHdr[0]), tc.wantHdr[1])
		}
		if tc.wantQry != "" && !strings.Contains(u.RawQuery, tc.wantQry) {
			t.Errorf("%s: query=%q, want 含 %q", tc.name, u.RawQuery, tc.wantQry)
		}
	}

	// 空 secret 不注入（自建内网 one-api 场景）。
	h := http.Header{}
	u, _ := url.Parse("https://x/y")
	Apply(h, u, AuthSpec{Mode: AuthBearer}, "")
	if h.Get("Authorization") != "" || u.RawQuery != "" {
		t.Error("空 secret 不应注入任何鉴权")
	}

	// 传输层头黑名单。
	for _, bad := range []string{"Host", "content-length", "Transfer-Encoding", "Connection", "Proxy-Authorization", "TE"} {
		if !HeaderForbidden(bad) {
			t.Errorf("%q 应在传输层黑名单", bad)
		}
	}
	for _, ok := range []string{"X-Title", "HTTP-Referer", "x-goog-api-key", "anthropic-version"} {
		if HeaderForbidden(ok) {
			t.Errorf("%q 不应在黑名单", ok)
		}
	}

	// query 方式脱敏：URL 里的秘密值必须变 ***。
	scrubbed := ScrubURL("https://api.x/v1beta/models/gemini:generateContent?key=AIza-secret&alt=sse")
	if strings.Contains(scrubbed, "AIza-secret") {
		t.Errorf("ScrubURL 未抹掉秘密值: %s", scrubbed)
	}
	if !strings.Contains(scrubbed, "alt=sse") {
		t.Errorf("ScrubURL 不应抹掉非敏感参数: %s", scrubbed)
	}
	// 文本里的 URL 片段也要脱敏（上游错误常回显完整 URL）。
	msg := ScrubSecrets("bad request at https://api.x/v1?key=sk-abcdef with key sk-abcdef", "sk-abcdef")
	if strings.Contains(msg, "sk-abcdef") {
		t.Errorf("ScrubSecrets 未抹掉秘密值: %s", msg)
	}
}

// 鉴权规格校验：非法模式/缺 header/黑名单 header。
func TestAuthSpecValidate(t *testing.T) {
	if err := (AuthSpec{}).Validate("auth"); err != nil {
		t.Errorf("空规格应合法（协议默认）: %v", err)
	}
	if err := (AuthSpec{Mode: "nope"}).Validate("auth"); err == nil || !strings.Contains(err.Error(), "openai") &&
		!strings.Contains(err.Error(), "bearer") {
		t.Error("未知 mode 应报错并给出合法值")
	}
	if err := (AuthSpec{Mode: AuthCustom}).Validate("auth"); err == nil {
		t.Error("custom 缺 header 应报错")
	}
	if err := (AuthSpec{Mode: AuthCustom, Header: "Host"}).Validate("auth"); err == nil {
		t.Error("custom 用 Host 应报错")
	}
	if err := (AuthSpec{Mode: AuthQuery, Param: "bad param!"}).Validate("auth"); err == nil {
		t.Error("query param 非法字符应报错")
	}
	if err := (AuthSpec{Mode: AuthQuery}).Validate("auth"); err != nil {
		t.Errorf("query 缺省 param 应合法: %v", err)
	}
}

// 协议默认鉴权：老配置（mode 为空）按协议解析。
func TestAuthSpecNormalized(t *testing.T) {
	if got := (AuthSpec{}).Normalized(ProtocolOpenAI); got.Mode != AuthBearer {
		t.Errorf("openai 默认应 bearer, got %+v", got)
	}
	if got := (AuthSpec{}).Normalized(ProtocolAnthropic); got.Mode != AuthCustom || got.Header != "x-api-key" {
		t.Errorf("anthropic 默认应 x-api-key, got %+v", got)
	}
	if got := (AuthSpec{}).Normalized(ProtocolGemini); got.Mode != AuthCustom || got.Header != "x-goog-api-key" {
		t.Errorf("gemini 默认应 x-goog-api-key, got %+v", got)
	}
	if got := (AuthSpec{Mode: AuthQuery}).Normalized(ProtocolGemini); got.Param != "key" {
		t.Errorf("query 缺省参数名应补 key, got %+v", got)
	}
}

// Query 方式下静态头不覆盖鉴权头；custom 头已有值时以静态头为准。
func TestAuthApplyDoesNotOverwriteStaticHeader(t *testing.T) {
	h := http.Header{}
	h.Set("Authorization", "Token raw-value")
	u, _ := url.Parse("https://x/y")
	Apply(h, u, AuthSpec{Mode: AuthBearer}, "sk-secret")
	if h.Get("Authorization") != "Token raw-value" {
		t.Error("已有 Authorization 时不应被 bearer 覆盖（支持无 Bearer 前缀的平台）")
	}
}
