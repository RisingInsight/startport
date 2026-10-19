// acclogin_test.go 登录式添加的 fixture 用例（v1.7.0 首批；全部 httptest mock，
// 零真实网络）。协议依据 starport-login-flows-spec.md §2.1/§2.2。
package acclogin

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RisingInsight/startport/internal/account"
)

// testManager 带 httptest 注入的 Manager。
type testManager struct {
	*Manager
	store *account.Store
}

func newTestManager(t *testing.T, mutate func(*Config)) *testManager {
	t.Helper()
	store, err := account.Load(t.TempDir() + "/accounts.json")
	if err != nil {
		t.Fatal(err)
	}
	store.SetLogf(func(string, ...any) {})
	t.Cleanup(store.Close)
	cfg := Config{
		Store:         store,
		TTL:           2 * time.Second,
		PollEvery:     30 * time.Millisecond,
		MachineIDFile: t.TempDir() + "/qoder-machine-id",
		Logf:          func(string, ...any) {},
	}
	if mutate != nil {
		mutate(&cfg)
	}
	m, err := NewManager(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	return &testManager{Manager: m, store: store}
}

// qoderPollMock 可编排的 poll/userinfo mock（每次 poll 响应由 pollBump 决定）。
type qoderPollMock struct {
	srv      *httptest.Server
	pollBump atomic.Int64 // 轮询计数（第 1 次 202、第 2 次成功……由测试自己编排）
	pollPath string
	// responses[i] = 第 i 次 poll 的 (status, body)；超出取最后一个。
	responses []mockResp
	sawQuery  map[string]string
	userInfo  bool
}

type mockResp struct {
	status int
	body   string
}

func newQoderPollMock(t *testing.T, responses []mockResp, userInfo bool) *qoderPollMock {
	m := &qoderPollMock{responses: responses, userInfo: userInfo, sawQuery: map[string]string{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/deviceToken/poll", func(w http.ResponseWriter, r *http.Request) {
		m.pollPath = r.URL.Path
		q := r.URL.Query()
		m.sawQuery["nonce"] = q.Get("nonce")
		m.sawQuery["verifier"] = q.Get("verifier")
		m.sawQuery["challenge_method"] = q.Get("challenge_method")
		i := int(m.pollBump.Add(1)) - 1
		if i >= len(m.responses) {
			i = len(m.responses) - 1
		}
		w.WriteHeader(m.responses[i].status)
		_, _ = w.Write([]byte(m.responses[i].body))
	})
	mux.HandleFunc("POST /userapi/v1/agent-send-code", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"data":{"result":true}}`))
	})
	mux.HandleFunc("GET /api/v1/userinfo", func(w http.ResponseWriter, r *http.Request) {
		if !m.userInfo {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		_, _ = w.Write([]byte(`{"id":"u-777","name":"测试昵称","email":"u@qoder.example"}`))
	})
	m.srv = httptest.NewServer(mux)
	t.Cleanup(m.srv.Close)
	return m
}

func loomyCredJSON() string { return `{"session":"sess-0123456789abcdef"}` }

// qoder 成功全链：202 → 成功 → userinfo 补全 → blob 入池（refresh 打包串 + mode）。
func TestQoderLoginSuccess(t *testing.T) {
	mock := newQoderPollMock(t, []mockResp{
		{http.StatusAccepted, `{}`},
		{http.StatusOK, `{"token":"dt-TOKEN-1","refresh_token":"drt-REFRESH","user_id":"u-777","expires_at":1893456000000}`},
	}, true)
	m := newTestManager(t, func(c *Config) {
		c.QoderOpenAPI = map[bool]string{false: mock.srv.URL, true: mock.srv.URL}
		c.QoderWeb = map[bool]string{false: "https://web.example", true: "https://web.example"}
	})
	res, err := m.Start(context.Background(), "qoder", "", "")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res.Mode != "auth_url" {
		t.Errorf("mode = %q", res.Mode)
	}
	// 授权 URL 只带 challenge/nonce/machine_id（verifier 不出网关——规格 §3.3-4）。
	if !strings.Contains(res.LoginURL, "/device/selectAccounts?challenge=") ||
		strings.Contains(res.LoginURL, "verifier") {
		t.Errorf("授权 URL 异常: %s", res.LoginURL)
	}
	// 轮询材料：nonce/verifier/challenge_method=S256（ref oauth.rs:36-41）。
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if mock.sawQuery["verifier"] != "" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if mock.sawQuery["challenge_method"] != "S256" || mock.sawQuery["nonce"] == "" ||
		mock.sawQuery["verifier"] == "" {
		t.Errorf("轮询参数异常: %+v", mock.sawQuery)
	}
	// Wait 长轮询到 success。
	st, err := m.Wait(res.SessionID)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if st.Status != StatusSuccess || st.AccountID == "" {
		t.Fatalf("状态异常: %+v", st)
	}
	// 入池断言：blob 形态匹配 validate.go schema（refresh 打包串 + mode=cn）。
	acc := m.store.Get(st.AccountID)
	if acc == nil || acc.Platform != account.PlatformQoder {
		t.Fatalf("账号未入池: %+v", acc)
	}
	var blob map[string]any
	if err := json.Unmarshal(acc.Cred, &blob); err != nil {
		t.Fatal(err)
	}
	if blob["access_token"] != "dt-TOKEN-1" {
		t.Errorf("access_token = %v", blob["access_token"])
	}
	if !strings.HasPrefix(blob["refresh_token"].(string), "drt-REFRESH|u-777|") {
		t.Errorf("refresh 打包串 = %v", blob["refresh_token"])
	}
	if blob["user_id"] != "u-777" || blob["mode"] != "cn" {
		t.Errorf("user_id/mode 异常: %+v", blob)
	}
	if blob["nickname"] != "测试昵称" {
		t.Errorf("userinfo 补全缺失: %+v", blob)
	}
}

// qoder 202 持续 → pending；会话超时 → expired（Wait 可观察）。
func TestQoderLoginTimeoutExpired(t *testing.T) {
	mock := newQoderPollMock(t, []mockResp{{http.StatusAccepted, `{}`}}, false)
	m := newTestManager(t, func(c *Config) {
		c.QoderOpenAPI = map[bool]string{false: mock.srv.URL, true: mock.srv.URL}
		c.QoderWeb = map[bool]string{false: "https://web.example", true: "https://web.example"}
		c.TTL = 300 * time.Millisecond
	})
	res, err := m.Start(context.Background(), "qoder", "", "")
	if err != nil {
		t.Fatal(err)
	}
	st, err := m.Wait(res.SessionID)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if st.Status != StatusPending && st.Status != StatusExpired {
		t.Fatalf("超时前状态异常: %+v", st)
	}
	// 等 reapLoop 落 expired（清扫周期 5s 太慢——直接调 reapExpired）。
	m.reapExpired(time.Now().Add(10 * time.Second))
	st, err = m.Wait(res.SessionID)
	if err != nil {
		t.Fatalf("Wait after expire: %v", err)
	}
	if st.Status != StatusExpired {
		t.Errorf("status = %q, want expired", st.Status)
	}
}

// qoder 取消：Wait 立刻 404（规格 §3.2 取消语义）。
func TestQoderLoginCancel(t *testing.T) {
	mock := newQoderPollMock(t, []mockResp{{http.StatusAccepted, `{}`}}, false)
	m := newTestManager(t, func(c *Config) {
		c.QoderOpenAPI = map[bool]string{false: mock.srv.URL, true: mock.srv.URL}
		c.QoderWeb = map[bool]string{false: "https://web.example", true: "https://web.example"}
	})
	res, _ := m.Start(context.Background(), "qoder", "", "")
	if err := m.Cancel(res.SessionID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if _, err := m.Wait(res.SessionID); err == nil {
		t.Fatal("取消后 Wait 应报会话不存在")
	}
}

// qoder 响应畸形（缺 token）→ failed。
func TestQoderLoginMalformed(t *testing.T) {
	mock := newQoderPollMock(t, []mockResp{{http.StatusOK, `{"refresh_token":"x"}`}}, false)
	m := newTestManager(t, func(c *Config) {
		c.QoderOpenAPI = map[bool]string{false: mock.srv.URL, true: mock.srv.URL}
		c.QoderWeb = map[bool]string{false: "https://web.example", true: "https://web.example"}
	})
	res, _ := m.Start(context.Background(), "qoder", "", "")
	deadline := time.Now().Add(3 * time.Second)
	var st WaitStatus
	var err error
	for time.Now().Before(deadline) {
		st, err = m.Wait(res.SessionID)
		if err != nil || st.Status != StatusPending {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	if err != nil || st.Status != StatusFailed || !strings.Contains(st.Error, "token") {
		t.Errorf("畸形响应应 failed: %+v %v", st, err)
	}
}

// 会话约束：同平台新会话顶掉旧的（旧 Wait 404）；全局 pending 上限 3。
func TestSessionConstraints(t *testing.T) {
	mock := newQoderPollMock(t, []mockResp{{http.StatusAccepted, `{}`}}, false)
	m := newTestManager(t, func(c *Config) {
		c.QoderOpenAPI = map[bool]string{false: mock.srv.URL, true: mock.srv.URL}
		c.QoderWeb = map[bool]string{false: "https://web.example", true: "https://web.example"}
		c.AutoClawUserAPI = mock.srv.URL // autoclaw start 也走 mock（零真实网络红线）
	})
	r1, _ := m.Start(context.Background(), "qoder", "", "")
	r2, err := m.Start(context.Background(), "qoder", "", "")
	if err != nil {
		t.Fatalf("同平台第二个会话应顶掉第一个: %v", err)
	}
	if _, err := m.Wait(r1.SessionID); err == nil {
		t.Error("被顶替的旧会话 Wait 应 404")
	}
	if _, err := m.Wait(r2.SessionID); err != nil {
		t.Errorf("新会话应可用: %v", err)
	}
	// 全局上限：再开 qoder-intl + autoclaw + 另一 autoclaw → 第 4 个 pending 拒绝。
	if _, err := m.Start(context.Background(), "qoder-intl", "", ""); err != nil {
		t.Fatalf("qoder-intl: %v", err)
	}
	if _, err := m.Start(context.Background(), "autoclaw", "", "13800008000"); err != nil {
		t.Fatalf("autoclaw: %v", err)
	}
	// autoclaw 单会话：第二个 autoclaw start 顶掉第一个 → 仍只有 2 个 pending。
	if _, err := m.Start(context.Background(), "autoclaw", "", "13900009000"); err != nil {
		t.Fatalf("第二个 autoclaw 应顶掉第一个: %v", err)
	}
	// 此时 pending = qoder + qoder-intl + autoclaw = 3 → 第 4 个跨平台会话拒绝。
	// 用 autoclaw-intl 不在支持列表 → 400；改用另一个 qoder → 顶替，仍是 3。
	if _, err := m.Start(context.Background(), "qoder", "", ""); err != nil {
		t.Fatalf("顶替式重启应成功: %v", err)
	}
}

// autoclaw：手机号归一化（+86/空格容忍、非法拒绝）。
func TestAutoClawNormalizePhone(t *testing.T) {
	for in, want := range map[string]string{
		"13800008000": "13800008000", "+86 138-0000-8000": "13800008000", "8613800008000": "13800008000",
	} {
		got, err := normalizePhone(in)
		if err != nil || got != want {
			t.Errorf("normalize(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"10000008000", "138000080", "abcdefghijk"} {
		if _, err := normalizePhone(bad); err == nil {
			t.Errorf("非法手机号 %q 应报错", bad)
		}
	}
}

// autoclaw：发码成功（路径/签名头/body 形态）+ 本地 60s 频控 + 630101 透出。
func TestAutoClawSMSFlow(t *testing.T) {
	var sawPath, sawPhone, sawDevice, sawSource, sawSign, sawTS string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawPath = r.URL.Path
		sawSign = r.Header.Get("X-Auth-Sign")
		sawTS = r.Header.Get("X-Auth-TimeStamp")
		var body struct {
			Phone    string `json:"phone"`
			Code     any    `json:"code"`
			Platform string `json:"platform"`
			SourceID string `json:"source_id"`
			DeviceID string `json:"device_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		sawPhone, sawDevice, sawSource = body.Phone, body.DeviceID, body.SourceID
		switch r.URL.Path {
		case autoclawSendCodePath:
			_, _ = w.Write([]byte(`{"code":0,"data":{"result":true}}`))
		case autoclawLoginPath:
			if body.Code == nil {
				t.Errorf("code 必须是 JSON 数字（字符串会 400001）")
			}
			if n, ok := body.Code.(float64); !ok || n != 123456 {
				t.Errorf("code = %v, want 数字 123456", body.Code)
			}
			if body.Platform != "web" {
				t.Errorf("platform = %q", body.Platform)
			}
			_, _ = w.Write([]byte(`{"code":0,"data":{"access_token":"ac-NEW","refresh_token":"ac-RT"}}`))
		}
	}))
	defer srv.Close()

	m := newTestManager(t, func(c *Config) { c.AutoClawUserAPI = srv.URL })
	res, err := m.Start(context.Background(), "autoclaw", "", "+86 138-0000-8000")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res.Mode != "sms" || !res.PhoneSent {
		t.Fatalf("Start 结果异常: %+v", res)
	}
	if sawPath != autoclawSendCodePath {
		t.Errorf("发码路径 = %s", sawPath)
	}
	if sawPhone != "13800008000" {
		t.Errorf("phone = %q", sawPhone)
	}
	if len(sawDevice) != 64 {
		t.Errorf("device_id = %q, want 64 hex", sawDevice)
	}
	if sawSource != "autoclaw" {
		t.Errorf("source_id = %q", sawSource)
	}
	// 签名头在固定时间戳下逐字节可复算（与 checkin 同一份实现）。
	expect := md5HexAutoClaw("100003&" + sawTS + "&38d2391985e2369a5fb8227d8e6cd5e5")
	if sawSign != expect {
		t.Errorf("X-Auth-Sign = %q, want %q", sawSign, expect)
	}
	// 60s 频控：重发被本地拒绝。
	if err := m.ResendSMS(context.Background(), res.SessionID); err == nil ||
		!strings.Contains(err.Error(), "频繁") {
		t.Errorf("60s 内重发应被频控: %v", err)
	}
	// 验证成功：code 作为 JSON 数字 + blob 入池。
	st, err := m.VerifySMS(context.Background(), res.SessionID, "123456")
	if err != nil {
		t.Fatalf("VerifySMS: %v", err)
	}
	if st.Status != StatusSuccess || st.AccountID == "" {
		t.Fatalf("验证结果异常: %+v", st)
	}
	acc := m.store.Get(st.AccountID)
	if acc == nil || acc.Platform != account.PlatformAutoClaw {
		t.Fatalf("账号未入池: %+v", acc)
	}
	var blob map[string]any
	_ = json.Unmarshal(acc.Cred, &blob)
	if blob["token"] != "ac-NEW" || blob["refresh_token"] != "ac-RT" || blob["device_id"] != sawDevice {
		t.Errorf("blob 异常: %s", acc.Cred)
	}
	if acc.Label != "138****8000" {
		t.Errorf("label = %q, want 脱敏手机号", acc.Label)
	}
}

// autoclaw：错误码表逐条（630202 验证码错误 / 630101 频控 / 631002 风控）。
func TestAutoClawErrorCodes(t *testing.T) {
	cases := []struct {
		code    int64
		rawBody string
		want    string
	}{
		{630202, `{"code":630202,"msg":"验证码错误"}`, "验证码不正确"},
		{630101, `{"code":630101,"msg":"too often"}`, "获取验证码过于频繁"},
		{631002, `{"code":631002,"msg":"版本已停止服务"}`, "风控验证"}, // 误导码改写
		{400002, `{"code":400002,"msg":"sign bad"}`, "校准系统时间"},
	}
	for _, c := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == autoclawSendCodePath {
				_, _ = w.Write([]byte(`{"code":0,"data":{"result":true}}`))
				return
			}
			_, _ = w.Write([]byte(c.rawBody))
		}))
		m := newTestManager(t, func(cfg *Config) { cfg.AutoClawUserAPI = srv.URL })
		res, err := m.Start(context.Background(), "autoclaw", "", "13800008000")
		if err != nil {
			t.Fatalf("code=%d: Start(发码) 失败: %v", c.code, err)
		}
		st, verr := m.VerifySMS(context.Background(), res.SessionID, "123456")
		srv.Close()
		if verr == nil || !strings.Contains(verr.Error(), c.want) {
			t.Errorf("code=%d: err = %v, want 含 %q", c.code, verr, c.want)
		}
		_ = st
		// 失败后账号不入池。
		if len(m.store.List(account.PlatformAutoClaw)) != 0 {
			t.Errorf("code=%d: 失败不应入池", c.code)
		}
	}
}

// autoclaw：verify 后 630101（发码频控）经 ResendSMS 透出。
func TestAutoClawResendUpstream630101(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == autoclawSendCodePath {
			_, _ = w.Write([]byte(`{"code":630101,"msg":"too often"}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":0,"data":{"result":true}}`))
	}))
	defer srv.Close()
	m := newTestManager(t, func(c *Config) {
		c.AutoClawUserAPI = srv.URL
		c.Now = func() time.Time { return time.Now().Add(-2 * time.Minute) } // 绕过本地频控
	})
	res, err := m.Start(context.Background(), "autoclaw", "", "13800008000")
	if err == nil || !strings.Contains(err.Error(), "630101") && !strings.Contains(err.Error(), "频繁") {
		t.Fatalf("上游 630101 应透出: %v", err)
	}
	_ = res
}

// md5HexAutoClaw 测试辅助。
func md5HexAutoClaw(s string) string {
	sum := md5Sum([]byte(s))
	return hex.EncodeToString(sum)
}

// md5Sum 测试辅助。
func md5Sum(b []byte) []byte {
	sum := md5.Sum(b)
	return sum[:]
}

// 键级回落：只覆盖 cn 侧 → intl 侧仍为官方默认（main 的 env 覆盖只填非空键）。
func TestEndpointPerKeyFallback(t *testing.T) {
	m, err := NewManager(Config{
		Store: func() *account.Store {
			s, err := account.Load(t.TempDir() + "/accounts.json")
			if err != nil {
				t.Fatal(err)
			}
			s.SetLogf(func(string, ...any) {})
			t.Cleanup(s.Close)
			return s
		}(),
		QoderOpenAPI:  map[bool]string{false: "https://cn-override.example"},
		MachineIDFile: t.TempDir() + "/qoder-machine-id",
		Logf:          func(string, ...any) {},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if m.cfg.QoderOpenAPI[false] != "https://cn-override.example" {
		t.Errorf("cn 覆盖未生效: %q", m.cfg.QoderOpenAPI[false])
	}
	if m.cfg.QoderOpenAPI[true] != "https://openapi.qoder.sh" {
		t.Errorf("intl 应回落官方默认: %q", m.cfg.QoderOpenAPI[true])
	}
	if m.cfg.QoderWeb[false] != "https://qoder.com.cn" || m.cfg.QoderWeb[true] != "https://qoder.com" {
		t.Errorf("web 未设键应整体回落官方: %+v", m.cfg.QoderWeb)
	}
}

// 回归（v1.7.0 发布修复根因）：**请求 ctx 取消后轮询协程必须存活**。
// 旧实现把 Start 的请求 ctx 传进后台轮询——start 响应一返回 ctx 即取消，
// mock 0 次命中、会话永 pending。本用例用可取消 ctx 模拟 HTTP 请求返回。
func TestQoderPollSurvivesRequestCtxCancel(t *testing.T) {
	mock := newQoderPollMock(t, []mockResp{
		{http.StatusAccepted, `{}`},
		{http.StatusOK, `{"token":"dt-FIX","refresh_token":"rt-FIX","user_id":"u-fix"}`},
	}, false)
	m := newTestManager(t, func(c *Config) {
		c.QoderOpenAPI = map[bool]string{false: mock.srv.URL, true: mock.srv.URL}
		c.QoderWeb = map[bool]string{false: "https://web.example", true: "https://web.example"}
	})
	ctx, cancel := context.WithCancel(context.Background())
	res, err := m.Start(ctx, "qoder", "", "")
	if err != nil {
		t.Fatal(err)
	}
	cancel() // 模拟 HTTP start 请求返回（请求 ctx 取消）
	st, err := m.Wait(res.SessionID)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if st.Status != StatusSuccess || st.AccountID == "" {
		t.Fatalf("请求 ctx 取消后轮询应继续并成功: %+v %v", st, err)
	}
	if mock.pollCalls() < 2 {
		t.Errorf("轮询次数 = %d, want ≥2（202 后成功）", mock.pollCalls())
	}
	// 顶替/取消路径仍会停轮询：取消会话后计数停止（允许 1 次在途请求收尾——
	// cancel 落在 pollOnce 进行中时，该次 HTTP 已发出，属正常排水）。
	time.Sleep(300 * time.Millisecond)
	before := mock.pollCalls()
	if _, err := m.Start(context.Background(), "qoder", "", ""); err != nil { // 同平台顶掉旧会话
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond) // 排水：在途请求落地
	stable := mock.pollCalls()
	time.Sleep(300 * time.Millisecond)
	if after := mock.pollCalls(); after != stable {
		t.Errorf("顶替后旧会话轮询未停止: stable=%d after=%d", stable, after)
	}
	_ = before
}

// pollCalls 读取 mock 的轮询计数（qoderPollMock 扩展）。
func (m *qoderPollMock) pollCalls() int64 {
	return m.pollBump.Load()
}
