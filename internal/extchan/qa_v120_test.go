// qa_v120_test.go QA 独立验收（v1.2.0 原生适配器框架 + Loomy）。
//
// 覆盖：双头/URL/流式 Accept、静态头不可覆盖双头、auth 被忽略、session 过期的
// 两种上游形态、模型目录（含裸数组与 200 业务错误）、未知 adapter 的请求期行为、
// 缓存失效、三协议错误体零回归。
package extchan

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/RisingInsight/startport/internal/provider"
)

const qaLoomySecret = "qa-loomy-session-abcdef-987654"

// qaLoomyUpstream 记录 Loomy 上游收到的请求形态。
type qaLoomyUpstream struct {
	paths   []string
	tokens  []string
	auths   []string
	accepts []string
	queries []string
	hits    int
}

func (u *qaLoomyUpstream) handler(respond func(w http.ResponseWriter, r *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u.hits++
		u.paths = append(u.paths, r.URL.Path)
		u.tokens = append(u.tokens, r.Header.Get("token"))
		u.auths = append(u.auths, r.Header.Get("Authorization"))
		u.accepts = append(u.accepts, r.Header.Get("Accept"))
		u.queries = append(u.queries, r.URL.RawQuery)
		respond(w, r)
	}
}

// qaLoomyDo 用单凭证 Loomy 渠道跑一次 Do。
func qaLoomyDo(t *testing.T, baseURL string, body string, stream bool) (*DoResult, *Channel, *Manager) {
	t.Helper()
	m := New(Config{TimeoutSeconds: 5})
	m.ReplaceAll([]ChannelDef{{
		Name: "loomy", BaseURL: baseURL + "/api/v1", Protocol: "openai", Adapter: "loomy",
		Credentials: []CredentialDef{{Label: "primary", Secret: qaLoomySecret}},
	}})
	ch := m.Get("loomy")
	res, err := m.Do(context.Background(), ch, &OutRequest{Body: []byte(body), Stream: stream})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	return res, ch, m
}

func qaJSONOK(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"id":"x","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
}

// 必测 1：双头 + URL 形态 + 流式 Accept + 秘密值不进 URL。
func TestQALoomyDualHeadersURLAndAccept(t *testing.T) {
	up := &qaLoomyUpstream{}
	srv := httptest.NewServer(up.handler(qaJSONOK))
	defer srv.Close()

	res, ch, _ := qaLoomyDo(t, srv.URL, `{"model":"spark-x","messages":[{"role":"user","content":"hi"}]}`, false)
	if res.Status != http.StatusOK {
		t.Fatalf("status = %d", res.Status)
	}
	if got, want := up.paths[0], "/api/v1/chat/completions"; got != want {
		t.Errorf("上游路径 = %q, want %q", got, want)
	}
	if up.tokens[0] != qaLoomySecret {
		t.Errorf("token 头 = %q", up.tokens[0])
	}
	if up.auths[0] != "Bearer "+qaLoomySecret {
		t.Errorf("Authorization 头 = %q", up.auths[0])
	}
	if up.queries[0] != "" {
		t.Errorf("URL 不应带 query: %q", up.queries[0])
	}
	if strings.Contains(up.paths[0]+up.queries[0], qaLoomySecret) {
		t.Error("秘密值出现在 URL")
	}
	if up.accepts[0] != "application/json" {
		t.Errorf("非流式 Accept = %q", up.accepts[0])
	}
	if st := ch.Snapshot(); st.Adapter != "loomy" || st.Protocol != "openai" {
		t.Errorf("Status.Adapter/Protocol = %q/%q", st.Adapter, st.Protocol)
	}

	// 流式：Accept: text/event-stream。
	up2 := &qaLoomyUpstream{}
	srv2 := httptest.NewServer(up2.handler(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n"))
	}))
	defer srv2.Close()
	res, _, _ = qaLoomyDo(t, srv2.URL, `{"model":"spark-x","stream":true}`, true)
	if res.Reader == nil {
		t.Fatal("流式应返回 reader")
	}
	defer res.Reader.Close()
	if up2.accepts[0] != "text/event-stream" {
		t.Errorf("流式 Accept = %q", up2.accepts[0])
	}
}

// 必测 1：静态头不能覆盖双头；auth 配置被忽略（双头是平台契约）。
func TestQALoomyStaticHeadersAndAuthIgnored(t *testing.T) {
	var sawToken, sawAuth, sawTitle, sawKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawToken = r.Header.Get("token")
		sawAuth = r.Header.Get("Authorization")
		sawTitle = r.Header.Get("X-Title")
		sawKey = r.Header.Get("x-api-key")
		qaJSONOK(w, r)
	}))
	defer srv.Close()

	m := New(Config{TimeoutSeconds: 5})
	// 直接构造（绕过 config 校验）：模拟"运行时仍持有 auth 配置"，适配器必须忽略。
	m.ReplaceAll([]ChannelDef{{
		Name: "loomy", BaseURL: srv.URL + "/api/v1", Adapter: "loomy",
		Auth:    provider.AuthSpec{Mode: provider.AuthBearer},
		Headers: map[string]string{"token": "hijack", "Authorization": "Bearer hijack", "X-Title": "StarPort"},
		Credentials: []CredentialDef{{
			Label: "p", Secret: qaLoomySecret,
			Auth: provider.AuthSpec{Mode: provider.AuthXAPIKey},
		}},
	}})
	if _, err := m.Do(context.Background(), m.Get("loomy"), &OutRequest{Body: []byte(`{"model":"m"}`)}); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if sawToken != qaLoomySecret || sawAuth != "Bearer "+qaLoomySecret {
		t.Errorf("双头被静态头/auth 覆盖: token=%q auth=%q", sawToken, sawAuth)
	}
	if sawTitle != "StarPort" {
		t.Errorf("普通静态头应生效, got %q", sawTitle)
	}
	if sawKey != "" {
		t.Errorf("凭证级 auth=x-api-key 应被忽略, got %q", sawKey)
	}
}

// 必测 1：credentials 声明 auth 时配置层会拒绝；运行时以"忽略"兜底（不写 x-api-key）。
// 上面 TestQALoomyStaticHeadersAndAuthIgnored 已覆盖。此处补渠道级 auth=bearer 的情形。
func TestQALoomyAuthConfigIgnoredChannelLevel(t *testing.T) {
	var sawAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("Authorization")
		qaJSONOK(w, r)
	}))
	defer srv.Close()
	m := New(Config{TimeoutSeconds: 5})
	m.ReplaceAll([]ChannelDef{{
		Name: "loomy", BaseURL: srv.URL + "/api/v1", Adapter: "loomy",
		Auth:        provider.AuthSpec{Mode: provider.AuthQuery, Param: "key"},
		Credentials: []CredentialDef{{Label: "p", Secret: qaLoomySecret}},
	}})
	if _, err := m.Do(context.Background(), m.Get("loomy"), &OutRequest{Body: []byte(`{"model":"m"}`)}); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if sawAuth != "Bearer "+qaLoomySecret {
		t.Errorf("Authorization = %q（应为适配器写入的 Bearer session）", sawAuth)
	}
}

// 必测 4：session 过期的两种形态都必须让客户端拿到
// error.type=authentication_error + error.code=session_expired。
func TestQALoomySessionExpiredForms(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"HTTP200+业务码020002", http.StatusOK, `{"code":"020002","desc":"登录已失效"}`},
		{"HTTP200+业务码100002", http.StatusOK, `{"code":"100002","desc":"session expired"}`},
		{"HTTP401+业务码020002", http.StatusUnauthorized, `{"code":"020002","desc":"登录已失效"}`},
		{"HTTP401+裸错误体", http.StatusUnauthorized, `{"error":{"message":"unauthorized"}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			res, _, _ := qaLoomyDo(t, srv.URL, `{"model":"m"}`, false)
			var env struct {
				Error struct {
					Type string `json:"type"`
					Code string `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal(res.Body, &env); err != nil {
				t.Fatalf("错误体不是 OpenAI 信封: %s", res.Body)
			}
			if env.Error.Type != "authentication_error" || env.Error.Code != "session_expired" {
				t.Errorf("错误语义 = type:%q code:%q（期望 authentication_error / session_expired）\nbody=%s",
					env.Error.Type, env.Error.Code, res.Body)
			}
		})
	}
}

// 必测 4：正常 200（带 choices）不得被误判成业务错误，即使 body 里有 code 字段。
func TestQALoomyNormalResponseNotMistaken(t *testing.T) {
	raw := `{"id":"x","code":"000000","choices":[{"message":{"role":"assistant","content":"ok"}}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(raw))
	}))
	defer srv.Close()
	res, _, _ := qaLoomyDo(t, srv.URL, `{"model":"m"}`, false)
	if string(res.Body) != raw {
		t.Errorf("正常响应被改写:\n got %s\nwant %s", res.Body, raw)
	}
}

// 必测 5：模型目录：GET {base}/models + 双头；{data:[{id}]} 与裸数组都可解析。
func TestQALoomyModelsShapes(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int
	}{
		{"openai 形态", `{"data":[{"id":"spark-x"},{"id":"glm-5.3"}]}`, 2},
		{"裸数组", `[{"id":"a"},{"id":"b"},{"id":"c"}]`, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up := &qaLoomyUpstream{}
			srv := httptest.NewServer(up.handler(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			m := New(Config{TimeoutSeconds: 5})
			m.ReplaceAll([]ChannelDef{{
				Name: "loomy", BaseURL: srv.URL + "/api/v1", Adapter: "loomy",
				Credentials: []CredentialDef{{Label: "p", Secret: qaLoomySecret}},
			}})
			ids, err := m.FetchModels(context.Background(), m.Get("loomy"))
			if err != nil {
				t.Fatalf("FetchModels: %v", err)
			}
			if len(ids) != tc.want {
				t.Errorf("模型数 = %d (%v), want %d", len(ids), ids, tc.want)
			}
			if got, want := up.paths[0], "/api/v1/models"; got != want {
				t.Errorf("目录路径 = %q, want %q", got, want)
			}
			if up.tokens[0] != qaLoomySecret || up.auths[0] != "Bearer "+qaLoomySecret {
				t.Errorf("目录请求缺双头: token=%q auth=%q", up.tokens[0], up.auths[0])
			}
		})
	}
}

// 必测 5：目录 200 + 业务错误 → 如实报 session_expired，绝不返回空目录。
func TestQALoomyModelsBizErrorNotEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":"020002","desc":"登录已失效"}`))
	}))
	defer srv.Close()
	m := New(Config{TimeoutSeconds: 5})
	m.ReplaceAll([]ChannelDef{{
		Name: "loomy", BaseURL: srv.URL + "/api/v1", Adapter: "loomy",
		Credentials: []CredentialDef{{Label: "p", Secret: qaLoomySecret}},
	}})
	ids, err := m.FetchModels(context.Background(), m.Get("loomy"))
	if err == nil {
		t.Fatalf("200+业务错误应报 error, 实际返回空目录 ids=%v", ids)
	}
	if len(ids) != 0 {
		t.Errorf("业务错误不得产出模型清单: %v", ids)
	}
	// 错误文案必须能让用户直接看懂"该重登了"（含 session_expired 与 14 天提示）。
	msg := strings.ToLower(err.Error())
	if !strings.Contains(msg, "session_expired") {
		t.Errorf("错误应如实报 session_expired: %q", err.Error())
	}
	if !strings.Contains(msg, "14") {
		t.Errorf("错误消息应说明 14 天有效期: %q", err.Error())
	}
	// 注：FetchModels 用 errors.New(ScrubSecrets(...)) 包装后 *provider.HTTPError 类型
	// 丢失，调用方只能读文案（见报告 Minor 项 O1）。
}

// 必测 3：直接构造 Channel{Adapter:"lomy"}（绕过 config）→ 请求期 typed error，
// 且**不向上游发请求**。
func TestQAUnknownAdapterAtRequestTime(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		qaJSONOK(w, r)
	}))
	defer srv.Close()
	m := New(Config{TimeoutSeconds: 5})
	m.ReplaceAll([]ChannelDef{{
		Name: "x", BaseURL: srv.URL, Adapter: "lomy",
		Credentials: []CredentialDef{{Label: "p", Secret: "sk-abcdef"}},
	}})
	_, err := m.Do(context.Background(), m.Get("x"), &OutRequest{Body: []byte(`{"model":"m"}`)})
	var he *Error
	if !errors.As(err, &he) {
		t.Fatalf("应为 typed *extchan.Error, got %T %v", err, err)
	}
	if he.Code != "ext_adapter_unknown" || he.Status != http.StatusBadRequest {
		t.Errorf("typed error = %+v", he)
	}
	if !strings.Contains(he.Message, "lomy") || !strings.Contains(he.Message, "loomy") {
		t.Errorf("错误应点名错误值并给出可用清单: %q", he.Message)
	}
	if hits != 0 {
		t.Errorf("未知 adapter 不应发上游请求, hits=%d", hits)
	}
}

// 必测 8：adapter 从空改为 loomy 后，模型目录缓存必须失效（重新发现）。
func TestQAAdapterChangeInvalidatesModelCache(t *testing.T) {
	var sawPaths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawPaths = append(sawPaths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"m1"}]}`))
	}))
	defer srv.Close()

	m := New(Config{TimeoutSeconds: 5})
	def := ChannelDef{
		Name: "c", BaseURL: srv.URL, Protocol: "openai",
		Credentials: []CredentialDef{{Label: "p", Secret: "s3cret-abc"}},
	}
	m.ReplaceAll([]ChannelDef{def})
	if got := m.EnsureModels(context.Background(), m.Get("c")); len(got) != 1 {
		t.Fatalf("首次发现异常: %v", got)
	}
	firstPath := sawPaths[len(sawPaths)-1]

	def.Adapter = "loomy"
	m.ReplaceAll([]ChannelDef{def})
	ch := m.Get("c")
	if got := ch.Snapshot().AutoModels; len(got) != 0 {
		t.Fatalf("改 adapter 后目录缓存应失效, got %v", got)
	}
	if got := m.EnsureModels(context.Background(), ch); len(got) != 1 {
		t.Fatalf("重新发现失败: %v", got)
	}
	secondPath := sawPaths[len(sawPaths)-1]
	if firstPath == secondPath {
		t.Errorf("重新发现应换用 Loomy 目录路径: 两次都是 %q", firstPath)
	}
	if !strings.Contains(secondPath, "/models") {
		t.Errorf("Loomy 目录路径异常: %q", secondPath)
	}
}

// 必测 6（零回归）：普通 openai 渠道 429/500 错误体逐字节透传（保留上游 error.code）。
func TestQAPlainOpenAIErrorBodyByteIdentical(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
	}{
		{429, `{"error":{"message":"rate limited","type":"rate_limit","code":"upstream_rate"}}`},
		{500, `{"error":{"message":"boom","code":"internal"}}`},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(tc.body))
		}))
		m := New(Config{TimeoutSeconds: 5})
		m.ReplaceAll([]ChannelDef{{
			Name: "plain", BaseURL: srv.URL, Protocol: "openai",
			Credentials: []CredentialDef{{Label: "p", Secret: "sk-abcdef"}},
		}})
		res, err := m.Do(context.Background(), m.Get("plain"), &OutRequest{Body: []byte(`{"model":"m"}`)})
		if err != nil {
			t.Fatalf("status=%d Do: %v", tc.status, err)
		}
		if res.Status != tc.status {
			t.Errorf("status = %d, want %d", res.Status, tc.status)
		}
		if string(res.Body) != tc.body {
			t.Errorf("status=%d 错误体应逐字节透传:\n got %s\nwant %s", tc.status, res.Body, tc.body)
		}
		srv.Close()
	}
}

// 必测 7：探测 Loomy 草稿 → 打 {base}/models 带双头；失败不污染状态。
func TestQAProbeLoomyDraftAndNoPollution(t *testing.T) {
	up := &qaLoomyUpstream{}
	srv := httptest.NewServer(up.handler(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"spark-x"}]}`))
	}))
	defer srv.Close()

	m := New(Config{TimeoutSeconds: 5})
	res := m.ProbeDraft(context.Background(), ChannelDef{
		Name: "loomy", BaseURL: srv.URL + "/api/v1", Adapter: "loomy",
		Credentials: []CredentialDef{{Label: "p", Secret: qaLoomySecret}},
	}, "")
	if !res.OK || res.ModelsCount != 1 {
		t.Fatalf("草稿探测应成功: %+v", res)
	}
	if res.Adapter != "loomy" {
		t.Errorf("ProbeResult.Adapter = %q", res.Adapter)
	}
	if up.paths[0] != "/api/v1/models" || up.tokens[0] != qaLoomySecret {
		t.Errorf("探测形态异常: path=%q token=%q", up.paths[0], up.tokens[0])
	}
	// 失败探测（401）不得改动任何已保存渠道状态。
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"code":"020002","desc":"expired"}`))
	}))
	defer bad.Close()
	before := m.Status()
	fail := m.ProbeDraft(context.Background(), ChannelDef{
		Name: "loomy2", BaseURL: bad.URL + "/api/v1", Adapter: "loomy",
		Credentials: []CredentialDef{{Label: "p", Secret: qaLoomySecret}},
	}, "")
	if fail.OK {
		t.Error("401 探测应失败")
	}
	after := m.Status()
	if len(after) != len(before) {
		t.Errorf("探测不应新增渠道: %d → %d", len(before), len(after))
	}
}

// 附加：双 session 渠道里首证过期（200+业务码）→ 自动换次证，客户端拿到 200。
// 证明"状态归一"与"凭证轮换"两个机制能正确叠加（Loomy 无续期，靠多 session 兜底）。
func TestQALoomyExpiredSessionSwitchesCredential(t *testing.T) {
	var used []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := r.Header.Get("token")
		used = append(used, token)
		w.Header().Set("Content-Type", "application/json")
		if token == "qa-loomy-expired-aaa" {
			_, _ = w.Write([]byte(`{"code":"020002","desc":"登录已失效"}`))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer srv.Close()

	m := New(Config{TimeoutSeconds: 5})
	m.ReplaceAll([]ChannelDef{{
		Name: "loomy", BaseURL: srv.URL + "/api/v1", Adapter: "loomy",
		Credentials: []CredentialDef{
			{Label: "old", Secret: "qa-loomy-expired-aaa"},
			{Label: "new", Secret: "qa-loomy-valid-bbb"},
		},
	}})
	ch := m.Get("loomy")
	res, err := m.Do(context.Background(), ch, &OutRequest{Body: []byte(`{"model":"m"}`)})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if res.Status != http.StatusOK || res.Attempts != 2 || res.CredentialLabel != "new" {
		t.Fatalf("换证结果异常: status=%d attempts=%d cred=%q", res.Status, res.Attempts, res.CredentialLabel)
	}
	if len(used) != 2 || used[0] != "qa-loomy-expired-aaa" || used[1] != "qa-loomy-valid-bbb" {
		t.Errorf("上游调用序列异常: %v", used)
	}
	if ch.CredentialByLabel("old").State() != CredInvalid {
		t.Error("过期 session 应标记 invalid")
	}
	if ch.CredentialByLabel("new").State() != CredHealthy {
		t.Error("可用 session 应保持 healthy")
	}
}
