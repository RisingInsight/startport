// qa_p0_verify_test.go QA 独立验收（不依赖工程师既有用例的断言）。
//
// 覆盖 PRD P0-2 / P0-4 / P0-5 / P0-8 / P0-9 与安全红线（query 鉴权秘密值
// 不出现在日志里）。命名统一 TestQA* / qa* 前缀，避免与既有用例冲突。
package extchan

import (
	"bytes"
	"context"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RisingInsight/startport/internal/provider"
)

// qaSecret 统一用足够长的秘密值（ScrubSecrets 会跳过 <4 字节的值）。
const qaSecret = "QA-SUPER-SECRET-0123456789"

// qaUpstream 记录上游收到的请求形态，并按 respond 返回。
type qaUpstream struct {
	mu       sync.Mutex
	paths    []string
	headers  []http.Header
	queries  []string
	methods  []string
	respond  func(w http.ResponseWriter, r *http.Request)
	requests int
}

func (u *qaUpstream) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		u.requests++
		u.paths = append(u.paths, r.URL.Path)
		u.headers = append(u.headers, r.Header.Clone())
		u.queries = append(u.queries, r.URL.RawQuery)
		u.methods = append(u.methods, r.Method)
		respond := u.respond
		u.mu.Unlock()
		if respond != nil {
			respond(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],` +
			`"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`))
	}
}

// last 返回最后一次上游请求的观测值。
func (u *qaUpstream) last() (string, http.Header, string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.paths) == 0 {
		return "", nil, ""
	}
	i := len(u.paths) - 1
	return u.paths[i], u.headers[i], u.queries[i]
}

// qaDo 用一条单凭证渠道跑一次 Do（非流式）。
func qaDo(t *testing.T, def ChannelDef) *DoResult {
	t.Helper()
	m := New(Config{TimeoutSeconds: 5})
	m.ReplaceAll([]ChannelDef{def})
	ch := m.Get(def.Name)
	if ch == nil {
		t.Fatalf("渠道 %q 未注册", def.Name)
	}
	res, err := m.Do(context.Background(), ch, &OutRequest{
		Body: []byte(`{"model":"m1","messages":[{"role":"user","content":"hi"}]}`),
	})
	if err != nil {
		t.Fatalf("Do 失败: %v", err)
	}
	if res.Status != http.StatusOK {
		t.Fatalf("Do status=%d body=%s", res.Status, res.Body)
	}
	return res
}

// P0-4：四种鉴权方式都必须按声明形态落到上游（渠道级声明）。
func TestQAAuthModesReachUpstream(t *testing.T) {
	cases := []struct {
		name    string
		auth    provider.AuthSpec
		want    func(t *testing.T, h http.Header, rawQuery string)
		comment string
	}{
		{
			name: "bearer",
			auth: provider.AuthSpec{Mode: provider.AuthBearer},
			want: func(t *testing.T, h http.Header, _ string) {
				if got := h.Get("Authorization"); got != "Bearer "+qaSecret {
					t.Errorf("Authorization = %q", got)
				}
			},
		},
		{
			name: "x-api-key",
			auth: provider.AuthSpec{Mode: provider.AuthXAPIKey},
			want: func(t *testing.T, h http.Header, _ string) {
				if got := h.Get("x-api-key"); got != qaSecret {
					t.Errorf("x-api-key = %q", got)
				}
			},
		},
		{
			name: "query",
			auth: provider.AuthSpec{Mode: provider.AuthQuery, Param: "api_key"},
			want: func(t *testing.T, h http.Header, rawQuery string) {
				if !strings.Contains(rawQuery, "api_key="+qaSecret) {
					t.Errorf("query 未注入 api_key: %q", rawQuery)
				}
				if h.Get("Authorization") != "" {
					t.Errorf("query 方式不应同时写 Authorization: %q", h.Get("Authorization"))
				}
			},
		},
		{
			name: "custom",
			auth: provider.AuthSpec{Mode: provider.AuthCustom, Header: "X-Custom-Token"},
			want: func(t *testing.T, h http.Header, _ string) {
				if got := h.Get("X-Custom-Token"); got != qaSecret {
					t.Errorf("X-Custom-Token = %q", got)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up := &qaUpstream{}
			srv := httptest.NewServer(up.handler())
			defer srv.Close()
			qaDo(t, ChannelDef{
				Name: "qa-auth", BaseURL: srv.URL, Models: []string{"m1"},
				Auth:        tc.auth,
				Credentials: []CredentialDef{{Label: "p", Secret: qaSecret}},
			})
			_, h, rawQuery := up.last()
			tc.want(t, h, rawQuery)
		})
	}
}

// Q1 默认假设：凭证级 auth 整体覆盖渠道级 auth。
func TestQACredentialAuthOverridesChannel(t *testing.T) {
	up := &qaUpstream{}
	srv := httptest.NewServer(up.handler())
	defer srv.Close()
	qaDo(t, ChannelDef{
		Name: "qa-override", BaseURL: srv.URL, Models: []string{"m1"},
		Auth: provider.AuthSpec{Mode: provider.AuthBearer},
		Credentials: []CredentialDef{{
			Label: "p", Secret: qaSecret,
			Auth: provider.AuthSpec{Mode: provider.AuthXAPIKey},
		}},
	})
	_, h, _ := up.last()
	if h.Get("x-api-key") != qaSecret {
		t.Errorf("凭证级 auth 未覆盖渠道级: x-api-key=%q", h.Get("x-api-key"))
	}
	if h.Get("Authorization") != "" {
		t.Errorf("覆盖后不应再注入渠道级 Authorization: %q", h.Get("Authorization"))
	}
}

// P0-5：自定义 path + {model} 占位 + 渠道级静态头都必须生效。
func TestQACustomPathModelPlaceholderAndHeaders(t *testing.T) {
	up := &qaUpstream{}
	srv := httptest.NewServer(up.handler())
	defer srv.Close()
	qaDo(t, ChannelDef{
		Name: "qa-path", BaseURL: srv.URL + "/ark", Models: []string{"m1"},
		Path:        "/api/v3/chat/{model}",
		Headers:     map[string]string{"X-Title": "StarPort"},
		Credentials: []CredentialDef{{Label: "p", Secret: qaSecret}},
	})
	path, h, _ := up.last()
	if path != "/ark/api/v3/chat/m1" {
		t.Errorf("自定义 path + {model} 未生效: %q", path)
	}
	if h.Get("X-Title") != "StarPort" {
		t.Errorf("渠道级静态头未生效: %q", h.Get("X-Title"))
	}
}

// P0-2：429 换证后冷却剩余秒数可读（面板据此显示"冷却 Ns"）。
func TestQACoolRemainingReadableAfter429(t *testing.T) {
	up := &qaUpstream{}
	up.respond = func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.Header.Get("Authorization"), "qa-primary") {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"rate limited"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}
	srv := httptest.NewServer(up.handler())
	defer srv.Close()

	m := New(Config{TimeoutSeconds: 5})
	m.ReplaceAll([]ChannelDef{{
		Name: "qa-cool", BaseURL: srv.URL, Models: []string{"m1"},
		Credentials: []CredentialDef{
			{Label: "primary", Secret: "qa-primary-secret"},
			{Label: "backup", Secret: "qa-backup-secret"},
		},
	}})
	ch := m.Get("qa-cool")
	res, err := m.Do(context.Background(), ch, &OutRequest{Body: []byte(`{"model":"m1"}`)})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if res.Attempts != 2 || res.CredentialLabel != "backup" {
		t.Fatalf("换证语义异常: attempts=%d cred=%q", res.Attempts, res.CredentialLabel)
	}
	snap := ch.CredentialByLabel("primary").Snapshot(time.Now())
	if snap.State != CredCooling || !snap.Cooling {
		t.Fatalf("primary 应 cooling: %+v", snap)
	}
	if snap.CoolRemain <= 0 || snap.CoolRemain > 3600 {
		t.Errorf("冷却剩余秒数不合理: %d", snap.CoolRemain)
	}
	if snap.Masked != true {
		t.Error("凭证快照必须标记 masked=true")
	}
}

// P0-8：3 天内到期 → expiring_soon=true 且计入渠道计数；401 → invalid 计数。
func TestQAExpiringSoonAndInvalidCounts(t *testing.T) {
	m := New(Config{})
	soon := time.Now().Add(48 * time.Hour).Format("2006-01-02")
	far := time.Now().Add(90 * 24 * time.Hour).Format("2006-01-02")
	m.ReplaceAll([]ChannelDef{{
		Name: "qa-alert", BaseURL: "https://api.example.com", Models: []string{"m1"},
		Credentials: []CredentialDef{
			{Label: "soon", Secret: "s1", ExpiresAt: soon},
			{Label: "far", Secret: "s2", ExpiresAt: far},
			{Label: "dead", Secret: "s3"},
		},
	}})
	ch := m.Get("qa-alert")
	ch.CredentialByLabel("dead").NoteInvalid("401 authentication failed")

	st := m.Status()[0]
	if st.CredentialsTotal != 3 || st.CredentialsInvalid != 1 || st.CredentialsExpiringSoon != 1 {
		t.Fatalf("计数异常: total=%d invalid=%d expiring=%d",
			st.CredentialsTotal, st.CredentialsInvalid, st.CredentialsExpiringSoon)
	}
	byLabel := map[string]CredSnapshot{}
	for _, c := range st.Credentials {
		byLabel[c.Label] = c
	}
	if !byLabel["soon"].ExpiringSoon {
		t.Errorf("2 天后到期应 expiring_soon: %+v", byLabel["soon"])
	}
	if byLabel["far"].ExpiringSoon {
		t.Errorf("90 天后到期不应 expiring_soon: %+v", byLabel["far"])
	}
	if byLabel["dead"].State != CredInvalid {
		t.Errorf("dead 应 invalid: %+v", byLabel["dead"])
	}
}

// P0-9：凭证维度用量递增，且 total = prompt + completion。
func TestQACredentialUsageCounters(t *testing.T) {
	up := &qaUpstream{}
	srv := httptest.NewServer(up.handler())
	defer srv.Close()
	m := New(Config{TimeoutSeconds: 5})
	m.ReplaceAll([]ChannelDef{{
		Name: "qa-usage", BaseURL: srv.URL, Models: []string{"m1"},
		Credentials: []CredentialDef{
			{Label: "primary", Secret: "qa-primary-secret"},
			{Label: "backup", Secret: "qa-backup-secret"},
		},
	}})
	ch := m.Get("qa-usage")
	cred := ch.CredentialByLabel("primary")
	for i := 0; i < 2; i++ {
		if _, err := m.Do(context.Background(), ch, &OutRequest{Body: []byte(`{"model":"m1"}`)}); err != nil {
			t.Fatalf("Do: %v", err)
		}
	}
	cred.NoteUsage(7, 3) // 与 Do 内的记账口径一致（Do 不记账，server 层记）

	snap := cred.Snapshot(time.Now())
	if snap.Requests != 1 || snap.PromptTokens != 7 || snap.CompletionTokens != 3 || snap.TotalTokens != 10 {
		t.Errorf("凭证用量计数异常: %+v", snap)
	}
}

// 安全红线（P0-4）：query 鉴权下，模型目录发现失败（传输错误/上游回显 URL）
// 的日志里不得出现秘密值。
func TestQAModelDiscoveryLogsNoSecret(t *testing.T) {
	// 先占一个端口再关掉 → 稳定的"连接被拒"，不产生任何外网请求。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	m := New(Config{TimeoutSeconds: 2})
	m.ReplaceAll([]ChannelDef{{
		Name: "qa-log", BaseURL: "http://" + addr, // Models 为空 → 触发自动发现
		Auth:        provider.AuthSpec{Mode: provider.AuthQuery},
		Credentials: []CredentialDef{{Label: "p", Secret: qaSecret}},
	}})

	var buf bytes.Buffer
	prevOut := log.Writer()
	log.SetOutput(&buf)
	_ = m.ModelList(context.Background()) // EnsureModels → FetchModels → 传输失败 → 打日志
	log.SetOutput(prevOut)

	logged := buf.String()
	if strings.Contains(logged, qaSecret) {
		t.Errorf("日志泄露 query 鉴权秘密值:\n%s", logged)
	}
}

// 同样的路径：FetchModels 的 error 值本身（可能被上层日志/接口带出）不得含秘密值。
func TestQAFetchModelsErrorScrubbed(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	m := New(Config{TimeoutSeconds: 2})
	m.ReplaceAll([]ChannelDef{{
		Name: "qa-err", BaseURL: "http://" + addr,
		Auth:        provider.AuthSpec{Mode: provider.AuthQuery},
		Credentials: []CredentialDef{{Label: "p", Secret: qaSecret}},
	}})
	_, err = m.FetchModels(context.Background(), m.Get("qa-err"))
	if err == nil {
		t.Fatal("预期传输失败")
	}
	if strings.Contains(err.Error(), qaSecret) {
		t.Errorf("FetchModels error 泄露秘密值: %v", err)
	}
}
