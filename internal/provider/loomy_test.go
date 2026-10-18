package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// 内置样例值：够长（ScrubSecrets 对 <4 字符的值不替换）。
const (
	testLoomyBase   = "https://loomyad.xunfei.cn/api/v1"
	testLoomySecret = "loomy-session-abcdef123456"
)

// newLoomyTarget 构造一个 Loomy 出站目标。
func newLoomyTarget(t *testing.T) *Target {
	t.Helper()
	return &Target{
		Name:     "loomy",
		Protocol: ProtocolOpenAI,
		BaseURL:  testLoomyBase,
		Secret:   testLoomySecret,
	}
}

// 聊天：URL 是 {base}/chat/completions（**不补 /v1**，Loomy 的版本段是 /api/v1）。
func TestLoomyBuildRequestURLAndHeaders(t *testing.T) {
	a := NewLoomyAdapter()
	body := []byte(`{"model":"spark-x","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	req, err := a.BuildRequest(context.Background(), newLoomyTarget(t), body, true)
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	if got, want := req.URL.String(), testLoomyBase+"/chat/completions"; got != want {
		t.Errorf("URL = %q, want %q", got, want)
	}
	if req.Method != http.MethodPost {
		t.Errorf("Method = %s", req.Method)
	}
	// 双头：同一个 session 出现在 token 与 Authorization。
	if got := req.Header.Get("token"); got != testLoomySecret {
		t.Errorf("token 头 = %q", got)
	}
	if got, want := req.Header.Get("Authorization"), "Bearer "+testLoomySecret; got != want {
		t.Errorf("Authorization 头 = %q, want %q", got, want)
	}
	if got := req.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q", got)
	}
	if got := req.Header.Get("Accept"); got != "text/event-stream" {
		t.Errorf("流式 Accept = %q", got)
	}
	// body 原样透传（不重编码）。
	sent, _ := io.ReadAll(req.Body)
	if string(sent) != string(body) {
		t.Errorf("body 被改写:\n got %s\nwant %s", sent, body)
	}
	// 秘密值不得出现在 URL.
	if strings.Contains(req.URL.String(), testLoomySecret) {
		t.Errorf("秘密值出现在 URL: %s", req.URL)
	}
}

// 非流式 Accept 是 application/json。
func TestLoomyBuildRequestNonStreamAccept(t *testing.T) {
	a := NewLoomyAdapter()
	req, err := a.BuildRequest(context.Background(), newLoomyTarget(t), []byte(`{"model":"m"}`), false)
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	if got := req.Header.Get("Accept"); got != "application/json" {
		t.Errorf("非流式 Accept = %q", got)
	}
}

// base 末尾带斜杠也不产生双斜杠。
func TestLoomyBuildRequestTrailingSlash(t *testing.T) {
	a := NewLoomyAdapter()
	tgt := newLoomyTarget(t)
	tgt.BaseURL = testLoomyBase + "///"
	req, err := a.BuildRequest(context.Background(), tgt, []byte(`{"model":"m"}`), false)
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	if got, want := req.URL.String(), testLoomyBase+"/chat/completions"; got != want {
		t.Errorf("URL = %q, want %q", got, want)
	}
}

// Path 覆盖走 BuildChatURL 的规则（{model} 占位、URL 转义）。
func TestLoomyBuildRequestPathOverride(t *testing.T) {
	a := NewLoomyAdapter()
	tgt := newLoomyTarget(t)
	tgt.Path = "/openai/{model}/chat"
	req, err := a.BuildRequest(context.Background(), tgt, []byte(`{"model":"qwen/qwen3"}`), false)
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	want := testLoomyBase + "/openai/qwen%2Fqwen3/chat"
	if got := req.URL.String(); got != want {
		t.Errorf("URL = %q, want %q", got, want)
	}
}

// 静态头照常合并，但 token/Authorization 由适配器独占（用户写不进去）。
func TestLoomyStaticHeadersCannotOverrideAuth(t *testing.T) {
	a := NewLoomyAdapter()
	tgt := newLoomyTarget(t)
	tgt.Headers = map[string]string{
		"token":         "hijacked",
		"Authorization": "Bearer hijacked",
		"X-Title":       "StarPort",
		"Host":          "evil.example", // 传输层头：忽略
	}
	req, err := a.BuildRequest(context.Background(), tgt, []byte(`{"model":"m"}`), false)
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	if got := req.Header.Get("token"); got != testLoomySecret {
		t.Errorf("token 被静态头覆盖: %q", got)
	}
	if got, want := req.Header.Get("Authorization"), "Bearer "+testLoomySecret; got != want {
		t.Errorf("Authorization 被静态头覆盖: %q", got)
	}
	if got := req.Header.Get("X-Title"); got != "StarPort" {
		t.Errorf("普通静态头应生效, got %q", got)
	}
	if got := req.Host; got == "evil.example" {
		t.Errorf("Host 不得被静态头覆盖: %q", got)
	}
}

// 渠道级 auth 对 Loomy 无效（双头是平台契约，不随配置变化）。
func TestLoomyIgnoresAuthSpec(t *testing.T) {
	a := NewLoomyAdapter()
	tgt := newLoomyTarget(t)
	tgt.Auth = AuthSpec{Mode: AuthQuery, Param: "key"}
	req, err := a.BuildRequest(context.Background(), tgt, []byte(`{"model":"m"}`), false)
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	if req.URL.RawQuery != "" {
		t.Errorf("auth=query 不应生效, got query %q", req.URL.RawQuery)
	}
	if req.Header.Get("x-api-key") != "" {
		t.Errorf("不应注入 x-api-key")
	}
}

// 无凭证（探测草稿/自建场景）：不注入双头。
func TestLoomyNoSecretNoAuthHeaders(t *testing.T) {
	a := NewLoomyAdapter()
	tgt := newLoomyTarget(t)
	tgt.Secret = ""
	req, err := a.BuildRequest(context.Background(), tgt, []byte(`{"model":"m"}`), false)
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	if req.Header.Get("token") != "" || req.Header.Get("Authorization") != "" {
		t.Errorf("无凭证不应注入鉴权头: token=%q auth=%q",
			req.Header.Get("token"), req.Header.Get("Authorization"))
	}
}

// 正常响应原样透传（不重建 body）。
func TestLoomyConvertResponsePassthrough(t *testing.T) {
	a := NewLoomyAdapter()
	body := []byte(`{"id":"x","choices":[{"message":{"role":"assistant","content":"ok"}}],"code":"000000"}`)
	got := a.ConvertResponse(newLoomyTarget(t), http.StatusOK, body)
	if string(got) != string(body) {
		t.Errorf("正常响应应原样透传, got %s", got)
	}
}

// 目录响应带 code 字段也不能被误判成业务错误（data 优先）。
func TestLoomyConvertResponseDataNotMistakenAsError(t *testing.T) {
	a := NewLoomyAdapter()
	body := []byte(`{"code":"000000","data":[{"id":"spark-x"}]}`)
	got := a.ConvertResponse(newLoomyTarget(t), http.StatusOK, body)
	if string(got) != string(body) {
		t.Errorf("目录响应应原样透传, got %s", got)
	}
}

// 200 + 登录态失效业务码 → OpenAI 鉴权错误信封（客户端能解析，且提示重登）。
func TestLoomyConvertResponseSessionExpired(t *testing.T) {
	a := NewLoomyAdapter()
	for _, code := range []string{"020002", "100002"} {
		got := a.ConvertResponse(newLoomyTarget(t), http.StatusOK,
			[]byte(`{"code":"`+code+`","desc":"登录已失效"}`))
		var parsed struct {
			Error struct {
				Message string `json:"message"`
				Type    string `json:"type"`
				Code    string `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal(got, &parsed); err != nil {
			t.Fatalf("code=%s 信封不是合法 JSON: %s", code, got)
		}
		if parsed.Error.Type != "authentication_error" || parsed.Error.Code != "session_expired" {
			t.Errorf("code=%s 信封语义异常: %+v", code, parsed.Error)
		}
		if !strings.Contains(parsed.Error.Message, "14") {
			t.Errorf("消息应说明 14 天有效期: %q", parsed.Error.Message)
		}
		if !strings.Contains(parsed.Error.Message, "登录已失效") {
			t.Errorf("消息应带上游文案: %q", parsed.Error.Message)
		}
	}
}

// 数字形态的业务码同样识别（有些边缘节点回 20002 而不是 "020002"）。
func TestLoomyConvertResponseNumericAuthCode(t *testing.T) {
	a := NewLoomyAdapter()
	got := a.ConvertResponse(newLoomyTarget(t), http.StatusOK, []byte(`{"code":20002,"desc":"expired"}`))
	if !strings.Contains(string(got), `"session_expired"`) {
		t.Errorf("数字业务码未识别: %s", got)
	}
}

// 其它业务码 → upstream_error（带 code，便于排查）。
func TestLoomyConvertResponseOtherBizError(t *testing.T) {
	a := NewLoomyAdapter()
	got := a.ConvertResponse(newLoomyTarget(t), http.StatusOK, []byte(`{"code":"123456","desc":"参数不对"}`))
	if !strings.Contains(string(got), `"upstream_error"`) {
		t.Errorf("普通业务错误应包成 upstream_error: %s", got)
	}
	if !strings.Contains(string(got), "参数不对") {
		t.Errorf("应保留上游文案: %s", got)
	}
}

// 非信封的 4xx/5xx → OpenAI 错误信封（客户端能解析），且秘密值被脱敏。
func TestLoomyConvertResponseHTTPErrorScrubbed(t *testing.T) {
	a := NewLoomyAdapter()
	got := a.ConvertResponse(newLoomyTarget(t), http.StatusInternalServerError,
		[]byte("boom token="+testLoomySecret))
	if !strings.Contains(string(got), `"error"`) {
		t.Errorf("应包成错误信封: %s", got)
	}
	if strings.Contains(string(got), testLoomySecret) {
		t.Errorf("错误体泄露秘密值: %s", got)
	}
}

// 流式：原样透传（不解帧、不伪造）。
func TestLoomyConvertStreamPassthrough(t *testing.T) {
	a := NewLoomyAdapter()
	rc := io.NopCloser(strings.NewReader("data: {\"choices\":[]}\n\n"))
	out, err := a.ConvertStream(newLoomyTarget(t), "text/event-stream", rc)
	if err != nil {
		t.Fatalf("ConvertStream: %v", err)
	}
	if out != rc {
		t.Error("流式应原样返回同一个 reader")
	}
}

// 模型目录：GET {base}/models，双头同样存在。
func TestLoomyBuildModels(t *testing.T) {
	a := NewLoomyAdapter()
	req, err := a.BuildModels(context.Background(), newLoomyTarget(t))
	if err != nil {
		t.Fatalf("BuildModels: %v", err)
	}
	if got, want := req.URL.String(), testLoomyBase+"/models"; got != want {
		t.Errorf("URL = %q, want %q", got, want)
	}
	if req.Method != http.MethodGet {
		t.Errorf("Method = %s", req.Method)
	}
	if req.Header.Get("token") != testLoomySecret {
		t.Errorf("models 请求缺 token 头")
	}
	if req.Header.Get("Authorization") != "Bearer "+testLoomySecret {
		t.Errorf("models 请求缺 Authorization 头")
	}
}

// 目录 URL 不受 path 覆盖影响（path 是聊天路径的逃生口）。
func TestLoomyBuildModelsIgnoresPath(t *testing.T) {
	a := NewLoomyAdapter()
	tgt := newLoomyTarget(t)
	tgt.Path = "/custom/{model}"
	req, err := a.BuildModels(context.Background(), tgt)
	if err != nil {
		t.Fatalf("BuildModels: %v", err)
	}
	if got, want := req.URL.String(), testLoomyBase+"/models"; got != want {
		t.Errorf("URL = %q, want %q", got, want)
	}
}

// 目录解析：OpenAI {data:[{id}]} 形态。
func TestLoomyDecodeModels(t *testing.T) {
	a := NewLoomyAdapter()
	got, err := a.DecodeModels(newLoomyTarget(t), http.StatusOK,
		[]byte(`{"data":[{"id":"spark-x"},{"id":" glm-5.3 "},{"id":""}]}`))
	if err != nil {
		t.Fatalf("DecodeModels: %v", err)
	}
	want := []string{"spark-x", "glm-5.3"}
	if len(got) != len(want) {
		t.Fatalf("模型数 = %d (%v), want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("模型[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// 目录解析：兼容裸数组形态。
func TestLoomyDecodeModelsBareArray(t *testing.T) {
	a := NewLoomyAdapter()
	got, err := a.DecodeModels(newLoomyTarget(t), http.StatusOK, []byte(`[{"id":"a"},{"id":"b"}]`))
	if err != nil {
		t.Fatalf("DecodeModels: %v", err)
	}
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("裸数组解析异常: %v", got)
	}
}

// 目录接口回业务错误（HTTP 200）必须报错，不能变成"空目录"的假成功。
func TestLoomyDecodeModelsBizError(t *testing.T) {
	a := NewLoomyAdapter()
	_, err := a.DecodeModels(newLoomyTarget(t), http.StatusOK, []byte(`{"code":"020002","desc":"登录已失效"}`))
	if err == nil {
		t.Fatal("业务错误应返回 error")
	}
	he, ok := err.(*HTTPError)
	if !ok {
		t.Fatalf("错误类型 = %T, want *HTTPError", err)
	}
	if he.Code != "session_expired" {
		t.Errorf("错误码 = %q, want session_expired", he.Code)
	}
	if !strings.Contains(he.Message, "14") {
		t.Errorf("消息应说明 14 天有效期: %q", he.Message)
	}
}

// 目录接口非 200 → models_upstream_error。
func TestLoomyDecodeModelsHTTPError(t *testing.T) {
	a := NewLoomyAdapter()
	_, err := a.DecodeModels(newLoomyTarget(t), http.StatusNotFound, []byte(`nope`))
	he, ok := err.(*HTTPError)
	if !ok || he.Code != "models_upstream_error" {
		t.Fatalf("错误异常: %v (%T)", err, err)
	}
}

// 适配器声明：名字 loomy、协议 openai。
func TestLoomyAdapterMetadata(t *testing.T) {
	a := NewLoomyAdapter()
	if a.Name() != LoomyAdapterName {
		t.Errorf("Name = %q", a.Name())
	}
	if a.Protocol() != ProtocolOpenAI {
		t.Errorf("Protocol = %q", a.Protocol())
	}
}

// loomyStatus 取 Loomy 的 StatusNormalizer 面（可选能力不在 NativeAdapter 接口上）。
func loomyStatus(t *testing.T) StatusNormalizer {
	t.Helper()
	sn, ok := NewLoomyAdapter().(StatusNormalizer)
	if !ok {
		t.Fatal("loomy 应实现 StatusNormalizer（200 + 业务错误码需要改状态码）")
	}
	return sn
}

// NormalizeStatus：正常 200 + choices 原样（不改状态、不改 body）。
func TestLoomyNormalizeStatusKeepsNormalResponse(t *testing.T) {
	a := loomyStatus(t)
	body := []byte(`{"id":"x","choices":[{"message":{"content":"ok"}}]}`)
	st, out := a.NormalizeStatus(http.StatusOK, body)
	if st != http.StatusOK || string(out) != string(body) {
		t.Errorf("正常响应被改写: status=%d body=%s", st, out)
	}
}

// NormalizeStatus：200 + 登录失效码 → 401 + session_expired。
func TestLoomyNormalizeStatus200SessionExpired(t *testing.T) {
	a := loomyStatus(t)
	for _, code := range []string{"020002", "100002"} {
		st, out := a.NormalizeStatus(http.StatusOK, []byte(`{"code":"`+code+`","desc":"登录已失效"}`))
		if st != http.StatusUnauthorized {
			t.Errorf("code=%s status = %d, want 401", code, st)
		}
		s := string(out)
		if !strings.Contains(s, `"authentication_error"`) || !strings.Contains(s, `"session_expired"`) {
			t.Errorf("code=%s 信封异常: %s", code, s)
		}
		if !strings.Contains(s, "14 天") {
			t.Errorf("code=%s 消息应说明 14 天: %s", code, s)
		}
	}
}

// NormalizeStatus：200 + 其它业务码 → 400 + loomy_<code>（不用 5xx：那会触发
// 渠道 30s 冷却，而业务错误换证/冷却都解决不了）。
func TestLoomyNormalizeStatus200OtherBizError(t *testing.T) {
	a := loomyStatus(t)
	st, out := a.NormalizeStatus(http.StatusOK, []byte(`{"code":"123456","desc":"参数不对"}`))
	if st != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", st)
	}
	s := string(out)
	if !strings.Contains(s, `"upstream_error"`) || !strings.Contains(s, "loomy_123456") {
		t.Errorf("信封异常: %s", s)
	}
}

// NormalizeStatus：裸 401/403（含 OpenAI 信封与纯文本两种 body）→ 401 + session_expired。
func TestLoomyNormalizeStatusBareAuthErrors(t *testing.T) {
	a := loomyStatus(t)
	cases := []struct {
		status int
		body   string
	}{
		{http.StatusUnauthorized, `{"error":{"message":"unauthorized"}}`},
		{http.StatusForbidden, `{"error":{"message":"forbidden"}}`},
		{http.StatusUnauthorized, `plain unauthorized`},
	}
	for _, c := range cases {
		st, out := a.NormalizeStatus(c.status, []byte(c.body))
		if st != http.StatusUnauthorized {
			t.Errorf("%d(%s) status = %d, want 401", c.status, c.body, st)
		}
		s := string(out)
		if !strings.Contains(s, `"session_expired"`) || !strings.Contains(s, "14 天") {
			t.Errorf("%d(%s) 应表达 session 过期: %s", c.status, c.body, s)
		}
	}
}

// NormalizeStatus：4xx/5xx 自带的语义不能被业务码改写（429 必须还是 429）。
func TestLoomyNormalizeStatusKeepsUpstreamErrorStatus(t *testing.T) {
	a := loomyStatus(t)
	st, out := a.NormalizeStatus(http.StatusTooManyRequests, []byte(`{"code":"020002","desc":"x"}`))
	if st != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429（限流语义不能被业务码改写成业务错误码）", st)
	}
	if !strings.Contains(string(out), "upstream_429") {
		t.Errorf("429 body 应包成信封: %s", out)
	}
	// 5xx 同理：状态保持。
	st, _ = a.NormalizeStatus(http.StatusInternalServerError, []byte(`boom`))
	if st != http.StatusInternalServerError {
		t.Errorf("5xx status = %d, want 500", st)
	}
}

// ConvertResponse：裸 401 的 OpenAI 信封也要改写成 session_expired（不是原样透传）。
func TestLoomyConvertResponseBare401(t *testing.T) {
	a := NewLoomyAdapter()
	out := a.ConvertResponse(newLoomyTarget(t), http.StatusUnauthorized,
		[]byte(`{"error":{"message":"unauthorized"}}`))
	s := string(out)
	if !strings.Contains(s, `"authentication_error"`) || !strings.Contains(s, `"session_expired"`) {
		t.Errorf("裸 401 应归一为 session_expired: %s", s)
	}
	if !strings.Contains(s, "14 天") {
		t.Errorf("消息应说明 14 天: %s", s)
	}
}

// ConvertResponse：已是错误信封的 5xx 不二次包装（避免 message 里嵌整段 JSON）。
func TestLoomyConvertResponseDoesNotDoubleWrap(t *testing.T) {
	a := NewLoomyAdapter()
	body := []byte(`{"error":{"message":"boom","type":"server_error","code":"x"}}`)
	out := a.ConvertResponse(newLoomyTarget(t), http.StatusInternalServerError, body)
	if string(out) != string(body) {
		t.Errorf("已归一的信封不应二次包装: %s", out)
	}
}

// 预设：loomy 指向原生适配器，且不再需要"P2 原生适配器"的置灰标记；
// auth 必须留空（配置层禁止 adapter 与 auth 并存）。
func TestLoomyPresetUsesNativeAdapter(t *testing.T) {
	p, ok := MatchPreset("Loomy", "")
	if !ok {
		t.Fatal("应匹配到 loomy 预设")
	}
	if p.Key != "loomy" {
		t.Errorf("Key = %q", p.Key)
	}
	if p.Adapter != LoomyAdapterName {
		t.Errorf("Adapter = %q, want %q", p.Adapter, LoomyAdapterName)
	}
	if p.Protocol != ProtocolOpenAI {
		t.Errorf("Protocol = %q", p.Protocol)
	}
	if p.NativeRequired {
		t.Error("loomy 已有原生适配器，不应再标 native_required")
	}
	if !p.Auth.IsZero() {
		t.Errorf("loomy 预设不应声明 auth（由适配器注入双头）: %+v", p.Auth)
	}
	if p.BaseURL != "" {
		t.Errorf("预设 base_url 必须为空（由用户确认）: %q", p.BaseURL)
	}
	// note 要写清 14 天与 base 建议值（用户唯一的信息来源）。
	for _, want := range []string{"14 天", "loomyad.xunfei.cn"} {
		if !strings.Contains(p.Note, want) {
			t.Errorf("预设备注应包含 %q: %s", want, p.Note)
		}
	}
}
