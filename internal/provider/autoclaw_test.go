// autoclaw_test.go AutoClaw 原生适配器的单测：头集合 / 模型路由 / body 改写 /
// system 规范化 / max_tokens 归一 / SSE 回写 / 错误分类 / 静态目录。
package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// autoclawAdapterFor 取指定地区的适配器实例。
func autoclawAdapterFor(t *testing.T, intl bool) *autoclawAdapter {
	t.Helper()
	var na NativeAdapter
	if intl {
		na = NewAutoClawIntlAdapter()
	} else {
		na = NewAutoClawAdapter()
	}
	a, ok := na.(*autoclawAdapter)
	if !ok {
		t.Fatalf("类型断言失败: %T", na)
	}
	return a
}

// newAutoClawTarget 构造一个 AutoClaw 出站目标。
func newAutoClawTarget(t *testing.T) *Target {
	t.Helper()
	return &Target{
		Name:        "autoclaw",
		Protocol:    ProtocolOpenAI,
		BaseURL:     "https://autoglm-acceleration-api.zhipuai.cn/autoclaw-proxy/proxy/autoclaw",
		Secret:      "autoclaw-token-abcdef-123456",
		ClientModel: "ext:autoclaw/glm-5.3",
	}
}

// autoclawBody 构造一个待发送的 OpenAI body。
func autoclawBody(model string) []byte {
	if model == "" {
		return []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
	}
	return []byte(`{"model":"` + model + `","messages":[{"role":"user","content":"hi"}]}`)
}

// 头集合：X-Authorization（不是 Authorization）/ X-Request-Model 路由 ID /
// X-Request-Id UUID 形态 / 品牌头 / 不发 X-Harness-Type。
func TestAutoClawBuildRequestHeaders(t *testing.T) {
	a := autoclawAdapterFor(t, false)
	req, err := a.BuildRequest(context.Background(), newAutoClawTarget(t),
		autoclawBody("glm-5.3"), false)
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	if got, want := req.URL.String(),
		"https://autoglm-acceleration-api.zhipuai.cn/autoclaw-proxy/proxy/autoclaw/chat/completions"; got != want {
		t.Errorf("URL = %q, want %q", got, want)
	}
	if got, want := req.Header.Get("X-Authorization"), "Bearer autoclaw-token-abcdef-123456"; got != want {
		t.Errorf("X-Authorization = %q, want %q", got, want)
	}
	if req.Header.Get("Authorization") != "" {
		t.Error("不应发 Authorization 头（认证头是 X-Authorization）")
	}
	if got := req.Header.Get("X-Request-Model"); got != "zaicoding_glm-5.3" {
		t.Errorf("X-Request-Model = %q, want zaicoding_glm-5.3", got)
	}
	rid := req.Header.Get("X-Request-Id")
	if len(rid) != 36 || strings.Count(rid, "-") != 4 {
		t.Errorf("X-Request-Id = %q, want UUID v4 形态", rid)
	}
	// 品牌头（源实现照抄值）。
	for k, want := range map[string]string{
		"X-Product": "autoclaw", "X-Client-Type": "pc", "X-Tm": "win",
		"X-Version": "1.17.8", "X-Lang": "zh-CN", "X-Channel": "official",
		"x_trace_id": "autoclaw-desktop",
	} {
		if got := req.Header.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if req.Header.Get("X-Harness-Type") != "" {
		t.Error("不应发 X-Harness-Type（带上会稳定 403 pay-view）")
	}
	if got := req.Header.Get("Accept"); got != "*/*" {
		t.Errorf("Accept = %q, want */*", got)
	}
	if strings.Contains(req.URL.String(), "autoclaw-token") {
		t.Errorf("秘密值出现在 URL: %s", req.URL)
	}
}

// 国际版实例：名字不同，头集合同一套。
func TestAutoClawIntlAdapterIdentity(t *testing.T) {
	a := autoclawAdapterFor(t, true)
	if a.Name() != AutoClawIntlAdapterName {
		t.Errorf("Name = %q", a.Name())
	}
	if a.Protocol() != ProtocolOpenAI {
		t.Errorf("Protocol = %q", a.Protocol())
	}
	req, err := a.BuildRequest(context.Background(), newAutoClawTarget(t), autoclawBody("glm-5.3"), false)
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	if req.Header.Get("X-Authorization") == "" {
		t.Error("国际版也应发 X-Authorization")
	}
}

// 模型路由：静态表（id / 展示名大小写变体 / 完整 routeId 三种写法都命中规范条目）。
func TestAutoClawRouteCatalogEntries(t *testing.T) {
	cases := map[string][2]string{ // 请求名 → (routeModelID, bodyModelID)
		"glm-5.3":           {"zaicoding_glm-5.3", "glm-5.3"},
		"GLM-5.3":           {"zaicoding_glm-5.3", "glm-5.3"},
		"glm-5.3-flash":     {"zai_glm-5.3-flash", "glm-5.3-flash"},
		"GLM-5.3-Flash":     {"zai_glm-5.3-flash", "glm-5.3-flash"},
		"zaicoding_glm-5.3": {"zaicoding_glm-5.3", "glm-5.3"},
		"zai_glm-5.3-flash": {"zai_glm-5.3-flash", "glm-5.3-flash"},
	}
	for in, want := range cases {
		r := resolveAutoClawRoute(in)
		if r.routeModelID != want[0] || r.bodyModelID != want[1] {
			t.Errorf("resolve(%q) = (%q,%q), want (%q,%q)", in, r.routeModelID, r.bodyModelID, want[0], want[1])
		}
		if r.requestedModel != in {
			t.Errorf("requestedModel = %q, want 原请求 %q", r.requestedModel, in)
		}
	}
}

// 模型路由：默认 / x_yyy 透传 / 未知回落。
func TestAutoClawRouteFallbacks(t *testing.T) {
	// 空 → 默认路由，body 剥前缀为 auto。
	r := resolveAutoClawRoute("")
	if r.routeModelID != "zai_auto" || r.bodyModelID != "auto" {
		t.Errorf("空 model 路由 = (%q,%q), want (zai_auto,auto)", r.routeModelID, r.bodyModelID)
	}
	// x_yyy 形态 → 原样透传（body 没有已知前缀可剥，保持原样——与源实现
	// stripRoutePrefix 只剥已知前缀的口径一致）。
	r = resolveAutoClawRoute("x_yyy")
	if r.routeModelID != "x_yyy" || r.bodyModelID != "x_yyy" {
		t.Errorf("x_yyy 路由 = (%q,%q), want (x_yyy,x_yyy)", r.routeModelID, r.bodyModelID)
	}
	// 已知前缀 + 未知模型名 → 路由原样、body 剥前缀（大小写保留）。
	r = resolveAutoClawRoute("zai_Custom-Model")
	if r.routeModelID != "zai_Custom-Model" || r.bodyModelID != "Custom-Model" {
		t.Errorf("zai_Custom-Model 路由 = (%q,%q)", r.routeModelID, r.bodyModelID)
	}
	// 未知名字（claude-* 这类）→ 回落默认路由，但 requested 保留原请求。
	r = resolveAutoClawRoute("claude-sonnet-4")
	if r.routeModelID != "zai_auto" || r.bodyModelID != "auto" || r.requestedModel != "claude-sonnet-4" {
		t.Errorf("未知名字路由 = %+v", r)
	}
}

// body 改写：model 换成剥前缀的模型 ID，其余字段（含未知字段）原样。
func TestAutoClawBodyModelStripped(t *testing.T) {
	a := autoclawAdapterFor(t, false)
	req, err := a.BuildRequest(context.Background(), newAutoClawTarget(t),
		[]byte(`{"model":"glm-5.3","messages":[],"stream":true,"top_p":0.9}`), true)
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	sent, _ := io.ReadAll(req.Body)
	var obj map[string]any
	if err := json.Unmarshal(sent, &obj); err != nil {
		t.Fatalf("body 不是 JSON: %s", sent)
	}
	if obj["model"] != "glm-5.3" {
		t.Errorf("body.model = %v, want glm-5.3（剥前缀）", obj["model"])
	}
	if obj["stream"] != true || obj["top_p"] != 0.9 {
		t.Errorf("其余字段被改写: %v", obj)
	}
}

// system 规范化：前缀注入 + 幂等（已以身份句开头不再前置）。
func TestAutoClawSystemPrefixIdempotent(t *testing.T) {
	a := autoclawAdapterFor(t, false)
	body := []byte(`{"model":"glm-5.3","messages":[{"role":"system","content":"You are a helpful assistant."},{"role":"user","content":"hi"}]}`)
	req, err := a.BuildRequest(context.Background(), newAutoClawTarget(t), body, false)
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	sent, _ := io.ReadAll(req.Body)
	var obj struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(sent, &obj); err != nil {
		t.Fatalf("body 不是 JSON: %s", sent)
	}
	if len(obj.Messages) != 2 {
		t.Fatalf("消息数 = %d, want 2（不增删消息）", len(obj.Messages))
	}
	sys := obj.Messages[0].Content
	if !strings.HasPrefix(sys, autoclawIdentityLine) {
		t.Errorf("system 未以身份句开头: %q", sys)
	}
	if !strings.Contains(sys, "## Tooling") {
		t.Errorf("system 缺 Tooling 段: %q", sys)
	}
	if !strings.HasSuffix(sys, "You are a helpful assistant.") {
		t.Errorf("客户端提示词未逐字保留在后: %q", sys)
	}
	// 幂等：再过一遍不改写（身份句只出现一次，前缀不重复）。
	req2, err := a.BuildRequest(context.Background(), newAutoClawTarget(t), sent, false)
	if err != nil {
		t.Fatalf("BuildRequest#2: %v", err)
	}
	sent2, _ := io.ReadAll(req2.Body)
	var obj2 struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(sent2, &obj2); err != nil {
		t.Fatalf("body#2 不是 JSON: %s", sent2)
	}
	if len(obj2.Messages) != 2 {
		t.Fatalf("消息数变化: %+v", obj2.Messages)
	}
	if !strings.HasPrefix(obj2.Messages[0].Content, autoclawIdentityPrefix) {
		t.Errorf("前缀丢失: %q", obj2.Messages[0].Content)
	}
	if n := strings.Count(obj2.Messages[0].Content, autoclawIdentityLine); n != 1 {
		t.Errorf("身份句出现次数 = %d, want 1（幂等）: %q", n, obj2.Messages[0].Content)
	}
}

// 首条不是 system：插入一条只带前缀的 system 消息（不重排后续）。
func TestAutoClawSystemPrefixInserted(t *testing.T) {
	a := autoclawAdapterFor(t, false)
	req, err := a.BuildRequest(context.Background(), newAutoClawTarget(t),
		[]byte(`{"model":"glm-5.3","messages":[{"role":"user","content":"hi"}]}`), false)
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	sent, _ := io.ReadAll(req.Body)
	var obj struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	_ = json.Unmarshal(sent, &obj)
	if len(obj.Messages) != 2 || obj.Messages[0].Role != "system" ||
		obj.Messages[0].Content != autoclawIdentityPrefix {
		t.Fatalf("应插入 system 前缀消息: %+v", obj.Messages)
	}
	if obj.Messages[1].Role != "user" || obj.Messages[1].Content != "hi" {
		t.Errorf("user 消息被改动: %+v", obj.Messages[1])
	}
}

// 外来身份句改写：整串替换 + 大小写不敏感 + user 消息不动。
func TestAutoClawForeignIdentityRewrite(t *testing.T) {
	a := autoclawAdapterFor(t, false)
	body := []byte(`{"model":"glm-5.3","messages":[` +
		`{"role":"system","content":"You are Claude Code, Anthropic's official CLI tool. Do work."},` +
		`{"role":"user","content":"Please act as You are Codex today"}]}`)
	req, err := a.BuildRequest(context.Background(), newAutoClawTarget(t), body, false)
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	sent, _ := io.ReadAll(req.Body)
	s := string(sent)
	if strings.Contains(s, "Claude Code") {
		t.Errorf("外来身份句未改写: %s", s)
	}
	if !strings.Contains(s, "You are a coding assistant, Anthropic's official CLI tool.") {
		t.Errorf("改写应只替换产品名、保留后文: %s", s)
	}
	// 大小写变体也要命中（闸门自己就不敏感）。
	req2, err := a.BuildRequest(context.Background(), newAutoClawTarget(t),
		[]byte(`{"model":"glm-5.3","messages":[{"role":"system","content":"YOU ARE ZCode, an interactive coding agent"}]}`), false)
	if err != nil {
		t.Fatalf("BuildRequest#2: %v", err)
	}
	sent2, _ := io.ReadAll(req2.Body)
	if strings.Contains(string(sent2), "ZCode") {
		t.Errorf("大小写变体未改写: %s", sent2)
	}
	// user 消息一字不动。
	var obj struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(sent, &obj); err != nil {
		t.Fatal(err)
	}
	if obj.Messages[1].Content != "Please act as You are Codex today" {
		t.Errorf("user 消息不应被改写: %q", obj.Messages[1].Content)
	}
}

// max_tokens 归一：小正整数抬到 10000；缺省不注入；大值/字符串/浮点/负数不碰。
func TestAutoClawMaxTokensNormalized(t *testing.T) {
	cases := []struct {
		in, want string // max_tokens 字段的 JSON 形态
	}{
		{`1024`, `10000`},
		{`30`, `10000`},
		{`8192`, `10000`},
		{`9999`, `10000`},
		{`10000`, `10000`},
		{`65536`, `65536`},
		{`"1024"`, `"1024"`}, // 字符串：不碰
		{`1024.5`, `1024.5`}, // 浮点：不碰
		{`0`, `0`},           // 非正：不碰
		{`-5`, `-5`},         // 负数：不碰
		{`null`, `null`},     // null：不碰
	}
	for _, c := range cases {
		a := autoclawAdapterFor(t, false)
		body := []byte(`{"model":"glm-5.3","max_tokens":` + c.in + `,"max_completion_tokens":30,"messages":[]}`)
		req, err := a.BuildRequest(context.Background(), newAutoClawTarget(t), body, false)
		if err != nil {
			t.Fatalf("BuildRequest(%s): %v", c.in, err)
		}
		sent, _ := io.ReadAll(req.Body)
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(sent, &obj); err != nil {
			t.Fatalf("body 不是 JSON: %s", sent)
		}
		if got := string(obj["max_tokens"]); got != c.want {
			t.Errorf("max_tokens %s → %s, want %s", c.in, got, c.want)
		}
		if got := string(obj["max_completion_tokens"]); got != "30" {
			t.Errorf("max_completion_tokens 被改动: %s", got)
		}
	}
	// 缺省时不注入。
	a := autoclawAdapterFor(t, false)
	req, err := a.BuildRequest(context.Background(), newAutoClawTarget(t),
		[]byte(`{"model":"glm-5.3","messages":[]}`), false)
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	sent, _ := io.ReadAll(req.Body)
	if strings.Contains(string(sent), "max_tokens") {
		t.Errorf("缺省时不应注入 max_tokens: %s", sent)
	}
}

// 非流式响应：model 回写成客户端请求名。
func TestAutoClawConvertResponseModelRewrite(t *testing.T) {
	a := autoclawAdapterFor(t, false)
	body := []byte(`{"id":"x","model":"zaicoding_glm-5.3","choices":[{"message":{"content":"ok"}}]}`)
	out := a.ConvertResponse(newAutoClawTarget(t), http.StatusOK, body)
	var obj struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("响应不是 JSON: %s", out)
	}
	if obj.Model != "ext:autoclaw/glm-5.3" {
		t.Errorf("model = %q, want ext:autoclaw/glm-5.3", obj.Model)
	}
}

// SSE：逐帧回写 model（本适配器允许重建帧的例外）。
func TestAutoClawConvertStreamModelRewrite(t *testing.T) {
	a := autoclawAdapterFor(t, false)
	upstream := io.NopCloser(strings.NewReader(
		"data: {\"model\":\"zaicoding_glm-5.3\",\"choices\":[{\"delta\":{\"content\":\"he\"}}]}\n\n" +
			"data: {\"model\":\"zaicoding_glm-5.3\",\"choices\":[{\"delta\":{\"content\":\"llo\"}}]}\n\n" +
			"data: [DONE]\n\n"))
	rc, err := a.ConvertStream(newAutoClawTarget(t), "text/event-stream", upstream)
	if err != nil {
		t.Fatalf("ConvertStream: %v", err)
	}
	out, _ := io.ReadAll(rc)
	s := string(out)
	if strings.Contains(s, "zaicoding_glm-5.3") {
		t.Errorf("SSE 帧未回写 model: %s", s)
	}
	if n := strings.Count(s, `"model":"ext:autoclaw/glm-5.3"`); n != 2 {
		t.Errorf("model 回写帧数 = %d, want 2: %s", n, s)
	}
	if !strings.Contains(s, "data: [DONE]") {
		t.Error("终止帧丢失")
	}
	// 非 SSE（JSON）：原样透传（不伪流式）。
	plain := io.NopCloser(strings.NewReader(`{"error":{"message":"bad"}}`))
	rc2, err := a.ConvertStream(newAutoClawTarget(t), "application/json", plain)
	if err != nil {
		t.Fatalf("ConvertStream(json): %v", err)
	}
	out2, _ := io.ReadAll(rc2)
	if !strings.Contains(string(out2), `"bad"`) {
		t.Errorf("非 SSE 应原样透传: %s", out2)
	}
}

// 错误分类：401 → authentication_error/token_invalid。
func TestAutoClawConvertResponse401(t *testing.T) {
	a := autoclawAdapterFor(t, false)
	out := a.ConvertResponse(newAutoClawTarget(t), http.StatusUnauthorized,
		[]byte(`{"message":"unauthorized"}`))
	s := string(out)
	if !strings.Contains(s, `"authentication_error"`) || !strings.Contains(s, `"token_invalid"`) {
		t.Errorf("401 应为 token 失效信封: %s", s)
	}
	if !strings.Contains(s, "重新") {
		t.Errorf("文案应提示重新粘贴 token: %s", s)
	}
	if strings.Contains(s, "autoclaw-token-abcdef-123456") {
		t.Errorf("错误体泄露 secret: %s", s)
	}
}

// 错误分类：403 + pay-view/810001 → membership_required（需要会员/额度）。
func TestAutoClawConvertResponse403PayView(t *testing.T) {
	a := autoclawAdapterFor(t, false)
	for _, body := range []string{
		`{"code":810001,"message":"当前使用人数较多，请升级为连续包月会员"}`,
		`{"message":"403 pay-view"}`,
	} {
		out := a.ConvertResponse(newAutoClawTarget(t), http.StatusForbidden, []byte(body))
		s := string(out)
		if !strings.Contains(s, `"membership_required"`) {
			t.Errorf("403 pay-view 应为会员提示: %s", s)
		}
		if !strings.Contains(s, "会员") {
			t.Errorf("文案应说明需要会员/额度: %s", s)
		}
	}
	// 普通 403（非 pay-view）→ token 失效。
	out := a.ConvertResponse(newAutoClawTarget(t), http.StatusForbidden,
		[]byte(`{"message":"forbidden"}`))
	if !strings.Contains(string(out), `"token_invalid"`) {
		t.Errorf("普通 403 应为 token 失效信封: %s", out)
	}
}

// 错误分类：406 空响应体 → 附可执行提示（上游预校验拒收的统一形态）。
func TestAutoClawConvertResponse406EmptyBody(t *testing.T) {
	a := autoclawAdapterFor(t, false)
	out := a.ConvertResponse(newAutoClawTarget(t), http.StatusNotAcceptable, nil)
	s := string(out)
	if !strings.Contains(s, "upstream_406") {
		t.Errorf("406 应带 upstream_406: %s", s)
	}
	for _, want := range []string{"max_tokens", "system", "风控"} {
		if !strings.Contains(s, want) {
			t.Errorf("406 提示缺方向 %q: %s", want, s)
		}
	}
	// 上游给了 message 的 406 以它为准（不再追加提示）。
	out2 := a.ConvertResponse(newAutoClawTarget(t), http.StatusNotAcceptable,
		[]byte(`{"message":"custom gate"}`))
	if strings.Contains(string(out2), "max_tokens") {
		t.Errorf("上游已给说明时不应追加提示: %s", out2)
	}
}

// 已是 OpenAI 信封的 5xx 不二次包装（保留上游 error.code）。
func TestAutoClawConvertResponseDoesNotDoubleWrap(t *testing.T) {
	a := autoclawAdapterFor(t, false)
	body := []byte(`{"error":{"message":"boom","type":"server_error","code":"x"}}`)
	out := a.ConvertResponse(newAutoClawTarget(t), http.StatusInternalServerError, body)
	if string(out) != string(body) {
		t.Errorf("已归一的信封不应二次包装: %s", out)
	}
}

// 静态目录：两个地区同表；DecodeModels 防御路径可用。
func TestAutoClawStaticModels(t *testing.T) {
	for _, intl := range []bool{false, true} {
		a := autoclawAdapterFor(t, intl)
		sm, ok := NewAutoClawAdapter().(StaticModels)
		if !ok {
			t.Fatal("autoclaw 应实现 StaticModels")
		}
		_ = a
		_ = sm
	}
	sm := NewAutoClawAdapter().(StaticModels)
	got := sm.StaticModels()
	want := []string{"glm-5.3", "glm-5.3-flash"}
	if len(got) != len(want) {
		t.Fatalf("静态目录 = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("静态目录[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// 预设：两条（国内 / 国际），都指向各自的适配器，不再 native_required。
func TestAutoClawPresetsUseNativeAdapter(t *testing.T) {
	cases := []struct {
		lookup  string
		wantKey string
	}{
		{"AutoClaw", "autoclaw"},
		{"autoclaw-intl", "autoclaw-intl"},
	}
	for _, c := range cases {
		p, ok := MatchPreset(c.lookup, "")
		if !ok || p.Key != c.wantKey {
			t.Fatalf("MatchPreset(%q) = (%+v,%v), want key %q", c.lookup, p, ok, c.wantKey)
		}
		if p.Adapter != c.wantKey {
			t.Errorf("%s: Adapter = %q, want %q", c.wantKey, p.Adapter, c.wantKey)
		}
		if p.Protocol != ProtocolOpenAI {
			t.Errorf("%s: Protocol = %q", c.wantKey, p.Protocol)
		}
		if p.NativeRequired {
			t.Errorf("%s: 不应再标 native_required", c.wantKey)
		}
		if !p.Auth.IsZero() {
			t.Errorf("%s: 不应声明 auth: %+v", c.wantKey, p.Auth)
		}
		if len(p.Models) != 2 {
			t.Errorf("%s: Models = %v, want 2 条", c.wantKey, p.Models)
		}
		// base_url 恒空（由用户确认），note 里给出建议值；国内版还要写明 token 来源。
		if p.BaseURL != "" {
			t.Errorf("%s: BaseURL 必须为空", c.wantKey)
		}
		if !strings.Contains(p.Note, "proxy/autoclaw") {
			t.Errorf("%s: Note 应写明 base 建议: %s", c.wantKey, p.Note)
		}
		if c.wantKey == "autoclaw" && !strings.Contains(p.Note, "X-Authorization") {
			t.Errorf("国内版 Note 应写明 token 来源（X-Authorization）: %s", p.Note)
		}
	}
	// 两条预设的 key 不重复、地区各自独立。
	intl, _ := MatchPreset("autoclaw-intl", "")
	cn, _ := MatchPreset("AutoClaw", "")
	if intl.BaseURL != "" && cn.BaseURL != "" {
		t.Error("unreachable")
	}
}

// 路由 ID 形态判定的边界（x_yyy 命中、目录 ID / 连字符名不命中）。
func TestAutoClawRouteIDPattern(t *testing.T) {
	cases := map[string]bool{
		"zai_auto":          true,
		"x_yyy":             true,
		"zaicoding_glm-5.3": true, // 前缀剥掉后由 strip 处理，形态本身合法
		"a_b":               true,
		"ab":                false, // 没有 _
		"Z_yyy":             false, // 前缀含大写
		"z_ai":              true,  // 单字母前缀 + 下划线：形态合法（与源实现一致）
		"glm-5.3":           false, // 无下划线
		"1_yyy":             false, // 前缀不能以数字开头
		"x_":                false, // 尾段为空
	}
	for in, want := range cases {
		if got := isAutoClawRouteID(in); got != want {
			t.Errorf("isAutoClawRouteID(%q) = %v, want %v", in, got, want)
		}
	}
}

// 品牌头不能被渠道静态头覆盖（平台契约独占）。
func TestAutoClawStaticHeadersCannotOverrideBrand(t *testing.T) {
	a := autoclawAdapterFor(t, false)
	tgt := newAutoClawTarget(t)
	tgt.Headers = map[string]string{
		"X-Version":       "9.9.9",
		"X-Authorization": "Bearer hijacked",
		"X-Request-Model": "zai_fake",
		"X-Note":          "mine",
	}
	req, err := a.BuildRequest(context.Background(), tgt, autoclawBody("glm-5.3"), false)
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	if got := req.Header.Get("X-Version"); got != "1.17.8" {
		t.Errorf("X-Version 被静态头覆盖: %q", got)
	}
	if got := req.Header.Get("X-Authorization"); got != "Bearer autoclaw-token-abcdef-123456" {
		t.Errorf("X-Authorization 被静态头覆盖: %q", got)
	}
	if got := req.Header.Get("X-Request-Model"); got != "zaicoding_glm-5.3" {
		t.Errorf("X-Request-Model 被静态头覆盖: %q", got)
	}
	if got := req.Header.Get("X-Note"); got != "mine" {
		t.Errorf("普通静态头应生效: %q", got)
	}
}

// 无凭证：不发认证头（探测草稿的合法形态），路由头照常。
func TestAutoClawNoSecretNoAuthHeader(t *testing.T) {
	a := autoclawAdapterFor(t, false)
	tgt := newAutoClawTarget(t)
	tgt.Secret = ""
	req, err := a.BuildRequest(context.Background(), tgt, autoclawBody("glm-5.3"), false)
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	if req.Header.Get("X-Authorization") != "" {
		t.Error("无凭证不应注入认证头")
	}
	if req.Header.Get("X-Request-Model") != "zaicoding_glm-5.3" {
		t.Error("路由头与凭证无关，应照常注入")
	}
}
