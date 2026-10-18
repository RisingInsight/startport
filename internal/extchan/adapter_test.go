// adapter_test.go 原生适配器在 extchan 侧的接线：解析、报错、观测与探测。
package extchan

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/RisingInsight/startport/internal/provider"
)

// 未知 adapter：Do 返回 typed error（ext_adapter_unknown），绝不静默回落到协议适配器。
func TestDoUnknownAdapterIsTypedError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("未知 adapter 不应发起任何上游请求")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	m := New(Config{TimeoutSeconds: 5})
	m.ReplaceAll([]ChannelDef{{
		Name: "x", BaseURL: srv.URL, Adapter: "nope",
		Credentials: []CredentialDef{{Label: "p", Secret: "sk-abcdef"}},
	}})
	_, err := m.Do(context.Background(), m.Get("x"), &OutRequest{Body: []byte(`{"model":"m"}`)})
	var he *Error
	if !errors.As(err, &he) {
		t.Fatalf("应为 typed error, got %v", err)
	}
	if he.Code != "ext_adapter_unknown" {
		t.Errorf("错误码 = %q, want ext_adapter_unknown", he.Code)
	}
	if !strings.Contains(he.Message, "nope") {
		t.Errorf("错误应点名 adapter: %q", he.Message)
	}
}

// 原生适配器端到端：双头注入、URL 形态、状态快照带 adapter。
func TestDoLoomyAdapterInjectsDualHeaders(t *testing.T) {
	var sawToken, sawAuth, sawPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawToken = r.Header.Get("token")
		sawAuth = r.Header.Get("Authorization")
		sawPath = r.URL.Path
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer srv.Close()

	m := New(Config{TimeoutSeconds: 5})
	m.ReplaceAll([]ChannelDef{{
		Name: "loomy", BaseURL: srv.URL + "/api/v1", Protocol: "openai", Adapter: "loomy",
		Credentials: []CredentialDef{{Label: "primary", Secret: "loomy-session-xyz"}},
	}})
	ch := m.Get("loomy")
	res, err := m.Do(context.Background(), ch, &OutRequest{Body: []byte(`{"model":"spark-x"}`)})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if res.Status != http.StatusOK {
		t.Errorf("status = %d", res.Status)
	}
	if got, want := sawPath, "/api/v1/chat/completions"; got != want {
		t.Errorf("上游路径 = %q, want %q（不能补 /v1）", got, want)
	}
	if sawToken != "loomy-session-xyz" {
		t.Errorf("token 头 = %q", sawToken)
	}
	if sawAuth != "Bearer loomy-session-xyz" {
		t.Errorf("Authorization 头 = %q", sawAuth)
	}
	// 观测面：Status 必须带 adapter，面板据此显示徽标。
	st := ch.Snapshot()
	if st.Adapter != "loomy" {
		t.Errorf("Status.Adapter = %q", st.Adapter)
	}
}

// 原生适配器的模型目录发现同样可用（FetchModels 走 adapterFor）。
func TestFetchModelsUsesAdapter(t *testing.T) {
	var sawPath, sawToken string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawPath = r.URL.Path
		sawToken = r.Header.Get("token")
		_, _ = w.Write([]byte(`{"data":[{"id":"spark-x"},{"id":"glm-5.3"}]}`))
	}))
	defer srv.Close()

	m := New(Config{TimeoutSeconds: 5})
	m.ReplaceAll([]ChannelDef{{
		Name: "loomy", BaseURL: srv.URL + "/api/v1", Adapter: "loomy",
		Credentials: []CredentialDef{{Label: "p", Secret: "sess-1"}},
	}})
	ids, err := m.FetchModels(context.Background(), m.Get("loomy"))
	if err != nil {
		t.Fatalf("FetchModels: %v", err)
	}
	if got, want := sawPath, "/api/v1/models"; got != want {
		t.Errorf("目录路径 = %q, want %q", got, want)
	}
	if sawToken != "sess-1" {
		t.Errorf("目录请求缺 token 头: %q", sawToken)
	}
	if len(ids) != 2 || ids[0] != "spark-x" {
		t.Errorf("目录解析异常: %v", ids)
	}
}

// 探测：草稿带 adapter 时也走原生适配器（双头 + /models），且结果带 adapter。
func TestProbeDraftWithAdapter(t *testing.T) {
	var sawToken, sawAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawToken = r.Header.Get("token")
		sawAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"data":[{"id":"spark-x"}]}`))
	}))
	defer srv.Close()

	m := New(Config{TimeoutSeconds: 5})
	res := m.ProbeDraft(context.Background(), ChannelDef{
		Name: "loomy", BaseURL: srv.URL + "/api/v1", Adapter: "loomy",
		Credentials: []CredentialDef{{Label: "p", Secret: "sess-probe"}},
	}, "")
	if !res.OK {
		t.Fatalf("探测失败: %+v", res)
	}
	if res.ModelsCount != 1 {
		t.Errorf("模型数 = %d", res.ModelsCount)
	}
	if res.Adapter != "loomy" {
		t.Errorf("ProbeResult.Adapter = %q", res.Adapter)
	}
	if sawToken != "sess-probe" || sawAuth != "Bearer sess-probe" {
		t.Errorf("探测应带双头: token=%q auth=%q", sawToken, sawAuth)
	}
}

// 探测：未知 adapter 明确报 invalid_adapter，且不发上游请求。
func TestProbeDraftUnknownAdapter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("未知 adapter 不应发起请求")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	m := New(Config{TimeoutSeconds: 5})
	res := m.ProbeDraft(context.Background(), ChannelDef{
		Name: "x", BaseURL: srv.URL, Adapter: "nope",
		Credentials: []CredentialDef{{Label: "p", Secret: "s3cret"}},
	}, "")
	if res.OK {
		t.Error("未知 adapter 不应成功")
	}
	if res.ErrorCode != "invalid_adapter" {
		t.Errorf("error_code = %q, want invalid_adapter", res.ErrorCode)
	}
	if !strings.Contains(res.Error, "nope") {
		t.Errorf("错误应点名 adapter: %q", res.Error)
	}
}

// adapter 变化要丢掉模型目录缓存（旧目录对新适配器无效）。
func TestReplaceAllDropsModelCacheOnAdapterChange(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"m1"}]}`))
	}))
	defer srv.Close()

	m := New(Config{TimeoutSeconds: 5})
	def := ChannelDef{
		Name: "c", BaseURL: srv.URL, Protocol: "openai",
		Credentials: []CredentialDef{{Label: "p", Secret: "s3cret"}},
	}
	m.ReplaceAll([]ChannelDef{def})
	models := m.EnsureModels(context.Background(), m.Get("c"))
	if len(models) != 1 {
		t.Fatalf("首次发现应有 1 条: %v", models)
	}
	// 仅改 adapter：目录缓存必须失效（重新发现），凭证状态保留。
	def.Adapter = provider.LoomyAdapterName
	m.ReplaceAll([]ChannelDef{def})
	ch := m.Get("c")
	if got := ch.Snapshot().AutoModels; len(got) != 0 {
		t.Errorf("改 adapter 后应丢弃目录缓存, got %v", got)
	}
	if ch.CredentialByLabel("p") == nil {
		t.Error("凭证明细应保留")
	}
}

// 原生适配器：4xx/5xx 的错误体也要归一成 OpenAI 信封（Loomy 的 {code,desc}
// 客户端解析不了），但**状态码保持上游语义**——429 必须是 429（限流退避），
// 不能被业务码改写掉（那会把"限流"当"请求错误"，冷却策略跑偏）。
func TestDoNativeAdapterKeepsUpstreamStatusOn4xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"code":"020002","desc":"登录已失效"}`))
	}))
	defer srv.Close()

	m := New(Config{TimeoutSeconds: 5})
	m.ReplaceAll([]ChannelDef{{
		Name: "loomy", BaseURL: srv.URL + "/api/v1", Adapter: "loomy",
		Credentials: []CredentialDef{{Label: "p", Secret: "sess-x"}},
	}})
	res, err := m.Do(context.Background(), m.Get("loomy"), &OutRequest{Body: []byte(`{"model":"m"}`)})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if res.Status != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429（4xx/5xx 不应被业务码改写）", res.Status)
	}
	body := string(res.Body)
	if !strings.Contains(body, `"error"`) || !strings.Contains(body, "upstream_429") {
		t.Errorf("错误体未归一成 OpenAI 信封: %s", body)
	}
}

// 原生适配器：HTTP 200 + 登录失效业务码 → **状态码也要变 401**（否则按状态码
// 判定成功的 SDK 会把 session 过期当正常响应）。
func TestDoNativeAdapterNormalizes200BizErrorTo401(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":"020002","desc":"登录已失效"}`))
	}))
	defer srv.Close()

	m := New(Config{TimeoutSeconds: 5})
	m.ReplaceAll([]ChannelDef{{
		Name: "loomy", BaseURL: srv.URL + "/api/v1", Adapter: "loomy",
		Credentials: []CredentialDef{{Label: "p", Secret: "sess-x"}},
	}})
	res, err := m.Do(context.Background(), m.Get("loomy"), &OutRequest{Body: []byte(`{"model":"m"}`)})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if res.Status != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", res.Status)
	}
	body := string(res.Body)
	if !strings.Contains(body, "session_expired") || !strings.Contains(body, "authentication_error") {
		t.Errorf("应为 session_expired 鉴权信封: %s", body)
	}
	if strings.Contains(body, "sess-x") {
		t.Errorf("错误体泄露 secret: %s", body)
	}
}

// 原生适配器：HTTP 200 + 其它业务码 → 400（业务失败不是"成功响应"，也不能用
// 5xx——那会给渠道加 30s 冷却，而审核/参数这类错误冷却解决不了）。
func TestDoNativeAdapterNormalizes200BizErrorTo400(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":"123456","desc":"参数不对"}`))
	}))
	defer srv.Close()

	m := New(Config{TimeoutSeconds: 5})
	m.ReplaceAll([]ChannelDef{{
		Name: "loomy", BaseURL: srv.URL + "/api/v1", Adapter: "loomy",
		Credentials: []CredentialDef{{Label: "p", Secret: "sess-x"}},
	}})
	res, err := m.Do(context.Background(), m.Get("loomy"), &OutRequest{Body: []byte(`{"model":"m"}`)})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if res.Status != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", res.Status)
	}
	if !strings.Contains(string(res.Body), "loomy_123456") {
		t.Errorf("错误码应带 loomy_<code>: %s", res.Body)
	}
}

// 原生适配器：裸 HTTP 401（body 是 OpenAI 信封）→ 401 + session_expired。
func TestDoNativeAdapterNormalizesBare401(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"unauthorized"}}`))
	}))
	defer srv.Close()

	m := New(Config{TimeoutSeconds: 5})
	m.ReplaceAll([]ChannelDef{{
		Name: "loomy", BaseURL: srv.URL + "/api/v1", Adapter: "loomy",
		Credentials: []CredentialDef{{Label: "p", Secret: "sess-x"}},
	}})
	res, err := m.Do(context.Background(), m.Get("loomy"), &OutRequest{Body: []byte(`{"model":"m"}`)})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if res.Status != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", res.Status)
	}
	body := string(res.Body)
	if !strings.Contains(body, "session_expired") || !strings.Contains(body, "14 天") {
		t.Errorf("裸 401 应表达 session 过期: %s", body)
	}
}

// 正常 200 + choices 不被归一误判（状态与 body 原样）。
func TestDoNativeAdapterLeavesNormalResponseAlone(t *testing.T) {
	raw := `{"id":"x","choices":[{"message":{"role":"assistant","content":"ok"}}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(raw))
	}))
	defer srv.Close()

	m := New(Config{TimeoutSeconds: 5})
	m.ReplaceAll([]ChannelDef{{
		Name: "loomy", BaseURL: srv.URL + "/api/v1", Adapter: "loomy",
		Credentials: []CredentialDef{{Label: "p", Secret: "sess-x"}},
	}})
	res, err := m.Do(context.Background(), m.Get("loomy"), &OutRequest{Body: []byte(`{"model":"m"}`)})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if res.Status != http.StatusOK || string(res.Body) != raw {
		t.Errorf("正常响应被改写: status=%d body=%s", res.Status, res.Body)
	}
}

// 零回归：没挂 adapter 的 openai 渠道，429 错误体仍逐字节透传（保留上游 error.code）。
func TestDoPlainOpenAIKeepsUpstreamErrorBody(t *testing.T) {
	raw := `{"error":{"message":"rate limited","type":"rate_limit","code":"upstream_rate"}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(raw))
	}))
	defer srv.Close()

	m := New(Config{TimeoutSeconds: 5})
	m.ReplaceAll([]ChannelDef{{
		Name: "plain", BaseURL: srv.URL, Protocol: "openai",
		Credentials: []CredentialDef{{Label: "p", Secret: "sk-abcdef"}},
	}})
	res, err := m.Do(context.Background(), m.Get("plain"), &OutRequest{Body: []byte(`{"model":"m"}`)})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if string(res.Body) != raw {
		t.Errorf("普通 openai 渠道错误体应逐字节透传:\n got %s\nwant %s", res.Body, raw)
	}
}

// AutoClaw 端到端：X-Authorization 认证头、X-Request-Model 路由 ID、
// body.model 剥前缀、system 前缀注入、品牌头、不发 X-Harness-Type。
func TestDoAutoClawAdapterEndToEnd(t *testing.T) {
	var sawAuth, sawRoute, sawHarness, sawVersion, sawBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("X-Authorization")
		sawRoute = r.Header.Get("X-Request-Model")
		sawHarness = r.Header.Get("X-Harness-Type")
		sawVersion = r.Header.Get("X-Version")
		raw, _ := io.ReadAll(r.Body)
		sawBody = string(raw)
		_, _ = w.Write([]byte(`{"model":"zaicoding_glm-5.3","choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer srv.Close()

	m := New(Config{TimeoutSeconds: 5})
	m.ReplaceAll([]ChannelDef{{
		Name: "autoclaw", BaseURL: srv.URL, Protocol: "openai", Adapter: "autoclaw",
		Models:      []string{"glm-5.3", "glm-5.3-flash"},
		Credentials: []CredentialDef{{Label: "p", Secret: "ac-token-xyz"}},
	}})
	res, err := m.Do(context.Background(), m.Get("autoclaw"), &OutRequest{
		Body:         []byte(`{"model":"glm-5.3","messages":[{"role":"system","content":"You are a helpful assistant."},{"role":"user","content":"hi"}]}`),
		DisplayModel: "ext:autoclaw/glm-5.3",
	})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if res.Status != http.StatusOK {
		t.Errorf("status = %d: %s", res.Status, res.Body)
	}
	if got, want := sawAuth, "Bearer ac-token-xyz"; got != want {
		t.Errorf("X-Authorization = %q, want %q", got, want)
	}
	if sawRoute != "zaicoding_glm-5.3" {
		t.Errorf("X-Request-Model = %q, want zaicoding_glm-5.3", sawRoute)
	}
	if sawHarness != "" {
		t.Errorf("不应发 X-Harness-Type, got %q", sawHarness)
	}
	if sawVersion != "1.17.8" {
		t.Errorf("X-Version = %q", sawVersion)
	}
	var body struct {
		Model    string `json:"model"`
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(sawBody), &body); err != nil {
		t.Fatalf("出站 body 不是 JSON: %s", sawBody)
	}
	if body.Model != "glm-5.3" {
		t.Errorf("body.model = %q, want glm-5.3（剥前缀）", body.Model)
	}
	if !strings.HasPrefix(body.Messages[0].Content, "You are a personal assistant running inside OpenClaw.") {
		t.Errorf("system 未包身份前缀: %q", body.Messages[0].Content)
	}
	if !strings.Contains(string(res.Body), `"model":"ext:autoclaw/glm-5.3"`) {
		t.Errorf("响应 model 未回写: %s", res.Body)
	}
}

// AutoClaw 401 → 客户端 401 + token_invalid（token 失效，提示重新粘贴）。
func TestDoAutoClawAdapter401TokenInvalid(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"unauthorized"}`))
	}))
	defer srv.Close()

	m := New(Config{TimeoutSeconds: 5})
	m.ReplaceAll([]ChannelDef{{
		Name: "autoclaw", BaseURL: srv.URL, Adapter: "autoclaw",
		Models:      []string{"glm-5.3"},
		Credentials: []CredentialDef{{Label: "p", Secret: "ac-token-xyz"}},
	}})
	res, err := m.Do(context.Background(), m.Get("autoclaw"), &OutRequest{
		Body:         []byte(`{"model":"glm-5.3","messages":[]}`),
		DisplayModel: "ext:autoclaw/glm-5.3",
	})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if res.Status != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", res.Status)
	}
	s := string(res.Body)
	if !strings.Contains(s, `"authentication_error"`) || !strings.Contains(s, `"token_invalid"`) {
		t.Errorf("应为 token 失效信封: %s", s)
	}
	if strings.Contains(s, "ac-token-xyz") {
		t.Errorf("错误体泄露 secret: %s", s)
	}
	// 401 走 Do 的失效分支：凭证进 invalid（退出轮换）。
	if ch2 := m.Get("autoclaw"); ch2.CredentialByLabel("p").State() != CredInvalid {
		t.Error("401 应把凭证标为 invalid")
	}
}

// AutoClaw 403 pay-view → 会员/额度提示（不是 token 失效文案）。
func TestDoAutoClawAdapter403PayView(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"code":810001,"message":"请升级为连续包月会员"}`))
	}))
	defer srv.Close()

	m := New(Config{TimeoutSeconds: 5})
	m.ReplaceAll([]ChannelDef{{
		Name: "autoclaw", BaseURL: srv.URL, Adapter: "autoclaw",
		Models:      []string{"glm-5.3"},
		Credentials: []CredentialDef{{Label: "p", Secret: "ac-token-xyz"}},
	}})
	res, err := m.Do(context.Background(), m.Get("autoclaw"), &OutRequest{
		Body: []byte(`{"model":"glm-5.3","messages":[]}`), DisplayModel: "ext:autoclaw/glm-5.3",
	})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if res.Status != http.StatusForbidden {
		t.Errorf("status = %d, want 403", res.Status)
	}
	if !strings.Contains(string(res.Body), `"membership_required"`) {
		t.Errorf("应为会员提示信封: %s", res.Body)
	}
}

// 静态目录：FetchModels 直接返回适配器清单，**不发任何 HTTP**。
func TestFetchModelsStaticAdapterNoHTTP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("静态目录适配器不应发起 HTTP 请求")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	m := New(Config{TimeoutSeconds: 5})
	m.ReplaceAll([]ChannelDef{{
		Name: "autoclaw", BaseURL: srv.URL, Adapter: "autoclaw",
		Credentials: []CredentialDef{{Label: "p", Secret: "ac-token-xyz"}},
	}})
	ids, err := m.FetchModels(context.Background(), m.Get("autoclaw"))
	if err != nil {
		t.Fatalf("FetchModels: %v", err)
	}
	if len(ids) != 2 || ids[0] != "glm-5.3" || ids[1] != "glm-5.3-flash" {
		t.Errorf("静态目录 = %v", ids)
	}
	// EnsureModels 语义不变：结果进运行时缓存。
	got := m.EnsureModels(context.Background(), m.Get("autoclaw"))
	if len(got) != 2 {
		t.Errorf("EnsureModels = %v", got)
	}
}

// 探测：静态目录适配器不打上游，返回静态清单即成功。
func TestProbeDraftStaticAdapter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("静态目录适配器探测不应发起 HTTP 请求")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	m := New(Config{TimeoutSeconds: 5})
	res := m.ProbeDraft(context.Background(), ChannelDef{
		Name: "autoclaw", BaseURL: srv.URL, Adapter: "autoclaw",
		Credentials: []CredentialDef{{Label: "p", Secret: "ac-token-xyz"}},
	}, "")
	if !res.OK {
		t.Fatalf("探测失败: %+v", res)
	}
	if res.ModelsCount != 2 || len(res.ModelsSample) != 2 {
		t.Errorf("探测结果异常: %+v", res)
	}
	if res.Adapter != "autoclaw" {
		t.Errorf("ProbeResult.Adapter = %q", res.Adapter)
	}
}

// 探测归一（v1.4.0）：Loomy 裸 401 → error_code=session_expired（不是 upstream_401）。
func TestProbeDraftLoomy401Normalized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"unauthorized"}}`))
	}))
	defer srv.Close()

	m := New(Config{TimeoutSeconds: 5})
	res := m.ProbeDraft(context.Background(), ChannelDef{
		Name: "loomy", BaseURL: srv.URL + "/api/v1", Adapter: "loomy",
		Credentials: []CredentialDef{{Label: "p", Secret: "sess-probe"}},
	}, "")
	if res.OK {
		t.Fatal("探测应失败")
	}
	if res.Status != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", res.Status)
	}
	if res.ErrorCode != "session_expired" {
		t.Errorf("error_code = %q, want session_expired（让用户知道该换 session）", res.ErrorCode)
	}
}

// 探测归一：Loomy 200 + 登录失效业务码 → 状态归一为 401 + session_expired。
func TestProbeDraftLoomy200BizErrorNormalized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":"020002","desc":"登录已失效"}`))
	}))
	defer srv.Close()

	m := New(Config{TimeoutSeconds: 5})
	res := m.ProbeDraft(context.Background(), ChannelDef{
		Name: "loomy", BaseURL: srv.URL + "/api/v1", Adapter: "loomy",
		Credentials: []CredentialDef{{Label: "p", Secret: "sess-probe"}},
	}, "")
	if res.OK {
		t.Fatal("业务错误不应判为连通")
	}
	if res.Status != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401（状态码归一）", res.Status)
	}
	if res.ErrorCode != "session_expired" {
		t.Errorf("error_code = %q, want session_expired", res.ErrorCode)
	}
}

// 说明：AutoClaw 的探测**不会**走到 401 归一路径——它实现 StaticModels，
// probeWith 在发 HTTP 之前就短路返回静态清单（见 TestProbeDraftStaticAdapter）。
// 它的 StatusNormalizer 只在 Do 的错误路径生效（token_invalid 等信封由
// TestDoAutoClawAdapter401TokenInvalid 锚定）。

// 零回归锚点：普通 openai 探测 401 仍是 upstream_401（三协议基准适配器
// 不实现 StatusNormalizer，探测行为逐字节不变）。
func TestProbeDraftPlainOpenAI401Unchanged(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid key"}}`))
	}))
	defer srv.Close()

	m := New(Config{TimeoutSeconds: 5})
	res := m.ProbeDraft(context.Background(), ChannelDef{
		Name: "plain", BaseURL: srv.URL, Protocol: "openai",
		Credentials: []CredentialDef{{Label: "p", Secret: "sk-abcdef"}},
	}, "")
	if res.ErrorCode != "upstream_401" {
		t.Errorf("error_code = %q, want upstream_401（零回归）", res.ErrorCode)
	}
}

// Qoder 端到端：COSY 头齐全、请求体已编码（不是裸 JSON）、信封解包成
// OpenAI chunk、首帧预读在字节下发前转 typed error。
func TestDoQoderAdapterEndToEnd(t *testing.T) {
	var sawAuth, sawModelKey, sawSigPath, sawBody, sawCT string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("Authorization")
		sawModelKey = r.Header.Get("X-Model-Key")
		sawSigPath = r.Header.Get("Cosy-Sigpath")
		sawCT = r.Header.Get("Content-Type")
		raw, _ := io.ReadAll(r.Body)
		sawBody = string(raw)
		// 上游恒流式 + 信封。
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"statusCodeValue\":200,\"body\":\"{\\\"choices\\\":[{\\\"delta\\\":{\\\"role\\\":\\\"assistant\\\",\\\"content\\\":\\\"pong\\\"}}]}\"}\n\n" +
			"data: {\"statusCodeValue\":200,\"body\":\"{\\\"choices\\\":[{\\\"delta\\\":{},\\\"finish_reason\\\":\\\"stop\\\"}]}\"}\n\n" +
			"data: [DONE]\n\n"))
	}))
	defer upstream.Close()

	m := New(Config{TimeoutSeconds: 5})
	m.ReplaceAll([]ChannelDef{{
		Name: "qoder", BaseURL: "https://gateway.qoder.com.cn.invalid", Protocol: "openai",
		Adapter:     "qoder",
		Models:      []string{"Qwen3.8-Flash"},
		Credentials: []CredentialDef{{Label: "p", Secret: `{"mode":"cn","accessToken":"jt-t","userId":"u-1","machineId":"m-1"}`}},
	}})
	// base 指向 mock：适配器把 /algo/... 拼在 base 后（ SigPath 剥掉 /algo 前缀）。
	chDef := m.Get("qoder")
	chDef.mu.Lock()
	chDef.BaseURL = upstream.URL
	chDef.mu.Unlock()

	res, err := m.Do(context.Background(), ch2(m, "qoder"), &OutRequest{
		Body:         []byte(`{"model":"Qwen3.8-Flash","messages":[{"role":"user","content":"hi"}]}`),
		DisplayModel: "ext:qoder/Qwen3.8-Flash",
	})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if res.Status != http.StatusOK {
		t.Fatalf("status = %d: %s", res.Status, res.Body)
	}
	if !strings.HasPrefix(sawAuth, "Bearer COSY.") {
		t.Errorf("Authorization = %q, want Bearer COSY.<payload>.<sig>", sawAuth)
	}
	if sawModelKey != "qfmodel" {
		t.Errorf("X-Model-Key = %q, want qfmodel", sawModelKey)
	}
	if sawSigPath != "/api/v2/service/pro/sse/agent_chat_generation" {
		t.Errorf("Cosy-Sigpath = %q（剥 /algo 前缀）", sawSigPath)
	}
	if sawCT != "application/json" {
		t.Errorf("Content-Type = %q", sawCT)
	}
	if strings.HasPrefix(sawBody, "{") {
		t.Errorf("请求体应是编码后形态（不是裸 JSON）: %.60s", sawBody)
	}
	// 客户端拿到的是解包后的 OpenAI 帧（非流式聚合）。
	var resp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(res.Body, &resp); err != nil {
		t.Fatalf("响应不是 JSON: %s", res.Body)
	}
	if len(resp.Choices) != 1 || resp.Choices[0].Message.Content != "pong" {
		t.Errorf("信封解包/聚合异常: %s", res.Body)
	}
	_ = ch2
	_ = chDef
}

// ch2 取渠道（测试辅助，避免与局部变量名冲突）。
func ch2(m *Manager, name string) *Channel { return m.Get(name) }

// 首帧预读：HTTP 200 + 排队信封 → 在字节下发前转 typed error（503
// qoder_model_busy），且不罚渠道（冷却不生效，紧跟着的请求还能正常走）。
func TestDoQoderStreamQueuedNoChannelPenalty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"statusCodeValue\":403,\"body\":\"{\\\"code\\\":\\\"10605\\\",\\\"message\\\":\\\"排队\\\",\\\"retryAfterSeconds\\\":9}\"}\n\n"))
	}))
	defer srv.Close()

	m := New(Config{TimeoutSeconds: 5})
	m.ReplaceAll([]ChannelDef{{
		Name: "qoder", BaseURL: srv.URL, Adapter: "qoder",
		Models:      []string{"Qwen3.8-Flash"},
		Credentials: []CredentialDef{{Label: "p", Secret: `{"mode":"cn","accessToken":"jt-t","userId":"u-1"}`}},
	}})
	ch := m.Get("qoder")
	_, err := m.Do(context.Background(), ch, &OutRequest{
		Body:         []byte(`{"model":"Qwen3.8-Flash","messages":[]}`),
		Stream:       true,
		DisplayModel: "ext:qoder/Qwen3.8-Flash",
	})
	var he *Error
	if !errors.As(err, &he) {
		t.Fatalf("应为 typed error, got %v", err)
	}
	if he.Status != http.StatusServiceUnavailable || he.Code != "qoder_model_busy" {
		t.Errorf("排队语义异常: %+v", he)
	}
	if ch.Cooldown() {
		t.Error("排队不应触发渠道冷却（既不是凭证问题也不是账号问题）")
	}
}

// 401（业务码 105）→ 鉴权语义：凭证标 invalid + typed error token_invalid。
func TestDoQoderStreamAuthError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"statusCodeValue\":401,\"body\":\"{\\\"code\\\":\\\"105\\\",\\\"message\\\":\\\"expired\\\"}\"}\n\n"))
	}))
	defer srv.Close()

	m := New(Config{TimeoutSeconds: 5})
	m.ReplaceAll([]ChannelDef{{
		Name: "qoder", BaseURL: srv.URL, Adapter: "qoder",
		Models:      []string{"Qwen3.8-Flash"},
		Credentials: []CredentialDef{{Label: "p", Secret: `{"mode":"cn","accessToken":"jt-t","userId":"u-1"}`}},
	}})
	ch := m.Get("qoder")
	_, err := m.Do(context.Background(), ch, &OutRequest{
		Body:         []byte(`{"model":"Qwen3.8-Flash","messages":[]}`),
		Stream:       true,
		DisplayModel: "ext:qoder/Qwen3.8-Flash",
	})
	var he *Error
	if !errors.As(err, &he) {
		t.Fatalf("应为 typed error, got %v", err)
	}
	if he.Status != http.StatusUnauthorized || he.Code != "token_invalid" {
		t.Errorf("401 语义异常: %+v", he)
	}
	if ch.CredentialByLabel("p").State() != CredInvalid {
		t.Error("鉴权失败应把凭证标为 invalid")
	}
}

// 未知名模型：硬失败 model_not_found（不静默回落）。
func TestDoQoderUnknownModelHardFail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("模型未知名不应发起上游请求")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	m := New(Config{TimeoutSeconds: 5})
	m.ReplaceAll([]ChannelDef{{
		Name: "qoder", BaseURL: srv.URL, Adapter: "qoder",
		Models:      []string{"Qwen3.8-Flash"},
		Credentials: []CredentialDef{{Label: "p", Secret: `{"mode":"cn","accessToken":"jt-t","userId":"u-1"}`}},
	}})
	_, err := m.Do(context.Background(), m.Get("qoder"), &OutRequest{
		Body:         []byte(`{"model":"claude-sonnet-4","messages":[]}`),
		DisplayModel: "ext:qoder/claude-sonnet-4",
	})
	var he *Error
	if !errors.As(err, &he) {
		t.Fatalf("应为 typed error, got %v", err)
	}
	if he.Code != "model_not_found" || he.Status != http.StatusBadRequest {
		t.Errorf("错误语义异常: %+v", he)
	}
}

// 静态目录：FetchModels / 探测都不发 HTTP（零上游请求）。
func TestQoderStaticModelsNoHTTP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("静态目录适配器不应发起 HTTP 请求")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	m := New(Config{TimeoutSeconds: 5})
	m.ReplaceAll([]ChannelDef{{
		Name: "qoder", BaseURL: srv.URL, Adapter: "qoder",
		Credentials: []CredentialDef{{Label: "p", Secret: `{"mode":"cn","accessToken":"jt-t","userId":"u-1"}`}},
	}})
	ids, err := m.FetchModels(context.Background(), m.Get("qoder"))
	if err != nil {
		t.Fatalf("FetchModels: %v", err)
	}
	if len(ids) == 0 || ids[0] != "Qwen3.8-Flash" {
		t.Errorf("静态目录 = %v", ids)
	}
	res := m.ProbeDraft(context.Background(), ChannelDef{
		Name: "qoder", BaseURL: srv.URL, Adapter: "qoder",
		Credentials: []CredentialDef{{Label: "p", Secret: `{"mode":"cn","accessToken":"jt-t","userId":"u-1"}`}},
	}, "")
	if !res.OK || res.ModelsCount == 0 {
		t.Errorf("探测异常: %+v", res)
	}
}

// 名字归一：配置里写 "Loomy" 也能用（大小写不敏感）。
func TestAdapterNameNormalized(t *testing.T) {
	m := New(Config{TimeoutSeconds: 5})
	m.ReplaceAll([]ChannelDef{{
		Name: "c", BaseURL: "https://example.com", Adapter: "  Loomy ",
	}})
	if got := m.Get("c").Snapshot().Adapter; got != "loomy" {
		t.Errorf("adapter 归一化失败: %q", got)
	}
}
