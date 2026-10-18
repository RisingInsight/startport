// adapter_test.go 原生适配器在 extchan 侧的接线：解析、报错、观测与探测。
package extchan

import (
	"context"
	"errors"
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
