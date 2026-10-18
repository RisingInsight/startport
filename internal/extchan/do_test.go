package extchan

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RisingInsight/startport/internal/provider"
)

// authRecorder 记录每次上游请求的鉴权头（验证换证是否真的换了 key）。
type authRecorder struct {
	mu    sync.Mutex
	calls []string
}

func (a *authRecorder) add(v string) {
	a.mu.Lock()
	a.calls = append(a.calls, v)
	a.mu.Unlock()
}

func (a *authRecorder) snapshot() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.calls...)
}

// newDoManager 构造带双凭证渠道的 Manager（primary/backup）。
func newDoManager(t *testing.T, baseURL string) (*Manager, *Channel) {
	t.Helper()
	m := New(Config{TimeoutSeconds: 5})
	m.ReplaceAll([]ChannelDef{{
		Name: "sf", BaseURL: baseURL,
		Credentials: []CredentialDef{
			{Label: "primary", Secret: "primary-secret"},
			{Label: "backup", Secret: "backup-secret"},
		},
	}})
	ch := m.Get("sf")
	if ch == nil {
		t.Fatal("渠道未注册")
	}
	return m, ch
}

// serveAccounting 模拟 server 侧的最终记账（recordExtAttempt → NoteUsage）。
//
// 单测里没有 server 层；生产口径是「Do 记被换证淘汰的尝试（NoteAttempt）、
// server 记最终服务/透传的那次（NoteUsage）」。这里用同一调用补齐生产口径，
// 让断言与真实链路一致而不是与"缺了半截记账的单测"一致。
func serveAccounting(t *testing.T, ch *Channel, res *DoResult) {
	t.Helper()
	if ch == nil || res == nil {
		return
	}
	if cred := ch.CredentialByLabel(res.CredentialLabel); cred != nil {
		cred.NoteUsage(0, 0)
	}
}

// assertNoInFlightLeak 断言渠道与其下所有凭证的在途计数都已归零。
//
// 回归锚点：Do 只在「传输错误」和「status<400」两个分支调用 cred.end()，
// 429 / 401 / 403 / 5xx / 4xx 全部漏掉 → /status 与面板里的 in_flight
// 永久停在 1，且后续每次同类失败继续累加（冒烟实测）。现在 end() 紧跟在
// m.exec 返回之后统一调用，任何分支都不再可能漏。
func assertNoInFlightLeak(t *testing.T, ch *Channel) {
	t.Helper()
	if got := ch.InFlight(); got != 0 {
		t.Errorf("渠道在途计数泄漏: %d", got)
	}
	for _, c := range ch.credentials() {
		if got := c.InFlight(); got != 0 {
			t.Errorf("凭证 %q 在途计数泄漏: %d", c.Label, got)
		}
	}
	// 快照是面板与 /status 的真实数据源，必须同样是 0。
	for _, snap := range ch.Snapshot().Credentials {
		if snap.InFlight != 0 {
			t.Errorf("快照里凭证 %q in_flight 泄漏: %d", snap.Label, snap.InFlight)
		}
	}
}

// 回归：所有错误路径都必须释放凭证 in_flight。
// 覆盖换证（429/401/403）与不换证（5xx/400/404）两类，逐类断言归零。
func TestDoInFlightReleasedOnErrorPaths(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		wantCall int // 上游往返次数（2 = 换证一次）
	}{
		{"429 换证", http.StatusTooManyRequests, 2},
		{"401 换证", http.StatusUnauthorized, 2},
		{"403 换证", http.StatusForbidden, 2},
		{"500 不换证", http.StatusInternalServerError, 1},
		{"400 不换证", http.StatusBadRequest, 1},
		{"404 不换证", http.StatusNotFound, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				calls++
				mu.Unlock()
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(`{"error":{"message":"boom"}}`))
			}))
			defer srv.Close()

			m, ch := newDoManager(t, srv.URL)
			res, err := m.Do(context.Background(), ch, &OutRequest{Body: []byte(`{"model":"m"}`)})
			if err != nil {
				t.Fatalf("上游错误应作为结果透传, got %v", err)
			}
			if res.Status != tc.status {
				t.Errorf("状态码 = %d, want %d", res.Status, tc.status)
			}
			if res.Attempts != tc.wantCall {
				t.Errorf("往返次数 = %d, want %d", res.Attempts, tc.wantCall)
			}
			mu.Lock()
			n := calls
			mu.Unlock()
			if n != tc.wantCall {
				t.Errorf("上游实际调用 %d 次, want %d", n, tc.wantCall)
			}
			assertNoInFlightLeak(t, ch)
		})
	}
}

// 回归：连续多次失败不得让 in_flight 累积（实测每轮 +1 且永不归零）。
func TestDoInFlightNoAccumulationAcrossRequests(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"rate limited"}}`))
	}))
	defer srv.Close()

	m, ch := newDoManager(t, srv.URL)
	// 第 1 次：两条凭证都 429（Attempts=2）→ 凭证与渠道双双冷却；
	// 之后每次请求在选证期即失败（503），一次都不该打上游。
	for i := 0; i < 3; i++ {
		_, _ = m.Do(context.Background(), ch, &OutRequest{Body: []byte(`{"model":"m"}`)})
		assertNoInFlightLeak(t, ch)
	}
}

// 回归：成功流式在拿到响应头后归零（reader 交给调用方后不再计入在途）。
func TestDoInFlightReleasedOnStreamSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	m, ch := newDoManager(t, srv.URL)
	res, err := m.Do(context.Background(), ch, &OutRequest{
		Body: []byte(`{"model":"m","stream":true}`), Stream: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != http.StatusOK || res.Reader == nil {
		t.Fatalf("流式结果异常: %+v", res)
	}
	if _, err := io.Copy(io.Discard, res.Reader); err != nil {
		t.Errorf("读取流失败: %v", err)
	}
	_ = res.Reader.Close()
	assertNoInFlightLeak(t, ch)
}

// 429 只冷却该凭证并换下一条重试一次（单请求最多 2 次往返）。
func TestDoSwitchesOn429(t *testing.T) {
	rec := &authRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("Authorization")
		rec.add(key)
		if strings.Contains(key, "primary-secret") {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"rate limited"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12}}`))
	}))
	defer srv.Close()

	m, ch := newDoManager(t, srv.URL)
	res, err := m.Do(context.Background(), ch, &OutRequest{
		Body: []byte(`{"model":"m","stream":false}`), DisplayModel: "ext:sf/m",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != 200 || res.Attempts != 2 || res.CredentialLabel != "backup" {
		t.Fatalf("换证结果异常: %+v", res)
	}
	if !strings.Contains(string(res.Body), `"content":"ok"`) {
		t.Errorf("响应体异常: %s", res.Body)
	}
	calls := rec.snapshot()
	if len(calls) != 2 {
		t.Fatalf("上游调用次数 = %d, want 2", len(calls))
	}
	if ch.CredentialByLabel("primary").State() != CredCooling {
		t.Error("primary 应进入 cooling（只罚该凭证）")
	}
	if ch.CredentialByLabel("backup").State() != CredHealthy {
		t.Error("backup 应保持 healthy")
	}
	// 冷却中的凭证不再被选中：下一次请求只打 backup。
	before := len(calls)
	if _, err := m.Do(context.Background(), ch, &OutRequest{Body: []byte(`{"model":"m"}`)}); err != nil {
		t.Fatal(err)
	}
	if got := rec.snapshot()[before:]; len(got) != 1 || !strings.Contains(got[0], "backup-secret") {
		t.Errorf("冷却凭证应被跳过: %v", got)
	}
	// 换证路径（primary 429）与成功路径（backup 200）都必须归零。
	if got := ch.CredentialByLabel("primary").InFlight(); got != 0 {
		t.Errorf("primary in_flight = %d, want 0", got)
	}
	if got := ch.CredentialByLabel("backup").InFlight(); got != 0 {
		t.Errorf("backup in_flight = %d, want 0", got)
	}
	assertNoInFlightLeak(t, ch)
	// v1.4.0 口径：primary 被 429 淘汰 → Do 记 1 次尝试；backup 最终服务 →
	// server 侧记 1 次。总上游往返 2 次，两边各 1、无双计。
	serveAccounting(t, ch, res)
	if got := ch.CredentialByLabel("primary").Snapshot(time.Now()).Requests; got != 1 {
		t.Errorf("primary requests = %d, want 1（被换证淘汰的尝试也计）", got)
	}
	if got := ch.CredentialByLabel("backup").Snapshot(time.Now()).Requests; got != 1 {
		t.Errorf("backup requests = %d, want 1（最终服务）", got)
	}
}

// 401 标记 invalid 并换证；后续请求不再尝试失效凭证。
func TestDoMarksInvalidOn401(t *testing.T) {
	rec := &authRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("Authorization")
		rec.add(key)
		if strings.Contains(key, "primary-secret") {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"message":"invalid key"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer srv.Close()

	m, ch := newDoManager(t, srv.URL)
	res, err := m.Do(context.Background(), ch, &OutRequest{Body: []byte(`{"model":"m"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != 200 || res.CredentialLabel != "backup" {
		t.Fatalf("401 换证异常: %+v", res)
	}
	if ch.CredentialByLabel("primary").State() != CredInvalid {
		t.Error("401 应标记 invalid")
	}
	before := len(rec.snapshot())
	if _, err := m.Do(context.Background(), ch, &OutRequest{Body: []byte(`{"model":"m"}`)}); err != nil {
		t.Fatal(err)
	}
	for _, key := range rec.snapshot()[before:] {
		if strings.Contains(key, "primary-secret") {
			t.Error("invalid 凭证不应再被尝试")
		}
	}
	assertNoInFlightLeak(t, ch)
	// v1.4.0 口径：401 淘汰的 primary 记 1 次尝试；backup 服务 1 次。
	serveAccounting(t, ch, res)
	if got := ch.CredentialByLabel("primary").Snapshot(time.Now()).Requests; got != 1 {
		t.Errorf("primary requests = %d, want 1（401 淘汰也计）", got)
	}
	if got := ch.CredentialByLabel("backup").Snapshot(time.Now()).Requests; got != 1 {
		t.Errorf("backup requests = %d, want 1（最终服务）", got)
	}
}

// 两条凭证都 429：首次返回上游 429，随后无可用凭证 → 503 all_credentials_unavailable。
func TestDoAllCredentialsUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"rate limited"}}`))
	}))
	defer srv.Close()

	m, ch := newDoManager(t, srv.URL)
	res, err := m.Do(context.Background(), ch, &OutRequest{Body: []byte(`{"model":"m"}`)})
	if err != nil {
		t.Fatalf("首次应返回上游结果（不吞状态码）: %v", err)
	}
	if res.Status != http.StatusTooManyRequests || res.Attempts != 2 {
		t.Errorf("首次结果异常: %+v", res)
	}
	_, err = m.Do(context.Background(), ch, &OutRequest{Body: []byte(`{"model":"m"}`)})
	var he *Error
	if !errors.As(err, &he) {
		t.Fatalf("第二次应为 typed error, got %v", err)
	}
	if he.Status != http.StatusServiceUnavailable || he.Code != "all_credentials_unavailable" {
		t.Errorf("错误语义异常: %+v", he)
	}
	assertNoInFlightLeak(t, ch)
	// v1.4.0 口径：两条凭证各被打了一次 429——primary 换证淘汰（Do 记）、
	// backup 是最终透传结果（server 记）。总尝试 2 次、两凭证各 1、无双计。
	serveAccounting(t, ch, res)
	total := 0
	for _, snap := range ch.Snapshot().Credentials {
		total += int(snap.Requests)
		if snap.Requests != 1 {
			t.Errorf("凭证 %q requests = %d, want 1", snap.Label, snap.Requests)
		}
	}
	if total != 2 {
		t.Errorf("requests 总和 = %d, want 2（等于上游往返数）", total)
	}
}

// 5xx：渠道短冷却、**不换证**；冷却期但凭证健康 → channel_cooling。
func TestDoServerErrorDoesNotSwitch(t *testing.T) {
	rec := &authRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.add(r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"upstream boom"}}`))
	}))
	defer srv.Close()

	m, ch := newDoManager(t, srv.URL)
	res, err := m.Do(context.Background(), ch, &OutRequest{Body: []byte(`{"model":"m"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != 500 || res.Attempts != 1 {
		t.Errorf("5xx 不应换证: %+v", res)
	}
	if len(rec.snapshot()) != 1 {
		t.Errorf("5xx 上游应只被调用 1 次: %v", rec.snapshot())
	}
	if !ch.Cooldown() {
		t.Error("5xx 应让渠道短冷却")
	}
	_, err = m.Do(context.Background(), ch, &OutRequest{Body: []byte(`{"model":"m"}`)})
	var he *Error
	if !errors.As(err, &he) || he.Code != "channel_cooling" {
		t.Errorf("冷却期应返回 channel_cooling, got %v", err)
	}
	assertNoInFlightLeak(t, ch)
	// v1.4.0 口径（无双计锚点）：5xx 不换证 → 该尝试就是最终结果，只由 server
	// 计 1 次；Do 的 NoteAttempt 不应再给它 +1。
	serveAccounting(t, ch, res)
	for _, snap := range ch.Snapshot().Credentials {
		want := int64(0)
		if snap.Label == res.CredentialLabel {
			want = 1
		}
		if snap.Requests != want {
			t.Errorf("凭证 %q requests = %d, want %d（5xx 不换证无双计）", snap.Label, snap.Requests, want)
		}
	}
}

// Gemini 上游在流式请求下返回 JSON（非 SSE）：502 gemini_stream_unsupported。
func TestDoGeminiStreamUnsupported(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, ":streamGenerateContent") {
			t.Errorf("流式请求 URL 形态异常: %s", r.URL)
		}
		if r.Header.Get("x-goog-api-key") != "g-secret" {
			t.Errorf("Gemini 默认鉴权应为 x-goog-api-key, got %q", r.Header.Get("x-goog-api-key"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"hi"}]}}]}`))
	}))
	defer srv.Close()

	m := New(Config{TimeoutSeconds: 5})
	m.ReplaceAll([]ChannelDef{{
		Name: "gem", BaseURL: srv.URL, Protocol: "gemini",
		Credentials: []CredentialDef{{Label: "p", Secret: "g-secret"}},
	}})
	_, err := m.Do(context.Background(), m.Get("gem"), &OutRequest{
		Body: []byte(`{"model":"gemini-2.0-flash","messages":[{"role":"user","content":"hi"}]}`), Stream: true,
	})
	var he *Error
	if !errors.As(err, &he) {
		t.Fatalf("应为 typed error, got %v", err)
	}
	if he.Status != http.StatusBadGateway || he.Code != "gemini_stream_unsupported" {
		t.Errorf("错误语义异常: %+v", he)
	}
	if !strings.Contains(he.Message, "stream") {
		t.Errorf("错误消息应提示改用非流式: %q", he.Message)
	}
	assertNoInFlightLeak(t, m.Get("gem"))
}

// 无凭证渠道：不注入鉴权（自建内网场景）。
func TestDoNoCredentialNoAuth(t *testing.T) {
	var sawAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer srv.Close()
	m := New(Config{TimeoutSeconds: 5})
	m.ReplaceAll([]ChannelDef{{Name: "local", BaseURL: srv.URL}})
	res, err := m.Do(context.Background(), m.Get("local"), &OutRequest{Body: []byte(`{"model":"m"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != 200 {
		t.Errorf("status = %d", res.Status)
	}
	if sawAuth != "" {
		t.Errorf("无凭证渠道不应注入 Authorization, got %q", sawAuth)
	}
	assertNoInFlightLeak(t, m.Get("local"))
}

// 错误体里不得出现秘密值（query 鉴权 + 上游回显 URL）。
func TestDoErrorBodyScrubbed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		body, _ := json.Marshal(map[string]any{"error": map[string]any{
			"message": "bad request at " + r.URL.String(),
		}})
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	m := New(Config{TimeoutSeconds: 5})
	m.ReplaceAll([]ChannelDef{{
		Name: "q", BaseURL: srv.URL, Protocol: "gemini",
		Auth:        provider.AuthSpec{Mode: provider.AuthQuery},
		Credentials: []CredentialDef{{Label: "p", Secret: "AIza-super-secret"}},
	}})
	res, err := m.Do(context.Background(), m.Get("q"), &OutRequest{Body: []byte(`{"model":"m"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(res.Body), "AIza-super-secret") {
		t.Errorf("错误体泄露了秘密值: %s", res.Body)
	}
	assertNoInFlightLeak(t, m.Get("q"))
}
