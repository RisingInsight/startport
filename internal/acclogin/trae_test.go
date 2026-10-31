// trae_test.go TRAE 浏览器登录的 mock 用例：guidance → 授权 URL 形态 →
// 回环回调 → ExchangeToken（设备公钥绑定）→ GetUserInfo → 入池 → Wait 终态。
// 全部 httptest mock，零真实网络。
package acclogin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RisingInsight/startport/internal/account"
)

func TestTraeBrowserLoginFlow(t *testing.T) {
	var exchangeBody, userInfoAuth map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/cloudide/api/v3/trae/GetLoginGuidance":
			_, _ = io.WriteString(w, `{"Result":{"LoginHost":"www.trae.cn"}}`)
		case "/trae/api/v3/oauth/ExchangeToken":
			_ = json.NewDecoder(r.Body).Decode(&exchangeBody)
			_, _ = io.WriteString(w, `{"Result":{"AccessToken":"at-1","RefreshToken":"rt-1","TokenExpireAt":1893456000000}}`)
		case "/cloudide/api/v3/trae/GetUserInfo":
			userInfoAuth = map[string]any{"token": r.Header.Get("X-Cloudide-Token")}
			_, _ = io.WriteString(w, `{"Result":{"UserID":"u-1","ScreenName":"tester"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	store, err := account.Load(filepath.Join(t.TempDir(), "accounts.json"))
	if err != nil {
		t.Fatal(err)
	}
	identityFile := filepath.Join(t.TempDir(), "trae-device-identity.json")
	m, err := NewManager(Config{
		Store:            store,
		TraeIdentityFile: identityFile,
		TraeGuidanceURLs: []string{srv.URL + "/cloudide/api/v3/trae/GetLoginGuidance"},
		TraeAPIOrigins:   []string{srv.URL},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	res, err := m.Start(context.Background(), account.PlatformTrae, "", "")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res.Mode != "auth_url" || res.SessionID == "" {
		t.Fatalf("start result = %+v", res)
	}
	u, err := url.Parse(res.LoginURL)
	if err != nil {
		t.Fatalf("login url: %v", err)
	}
	// 授权页形态：官方 host + /authorization；auth_callback_url 不编码（裸 127.0.0.1）。
	if !strings.HasPrefix(res.LoginURL, "https://www.trae.cn/authorization?") {
		t.Errorf("授权页 host = %s", res.LoginURL)
	}
	q := u.Query()
	if q.Get("client_id") != traeClientID || q.Get("auth_from") != "solo" ||
		q.Get("hide_saas_login") != "true" || q.Get("code_challenge_method") != "S256" ||
		q.Get("code_challenge") == "" || q.Get("login_trace_id") == "" {
		t.Errorf("授权 URL 参数缺失: %v", q)
	}
	cbURL := q.Get("auth_callback_url")
	if !strings.HasPrefix(cbURL, "http://127.0.0.1:") || !strings.HasSuffix(cbURL, "/authorize") {
		t.Errorf("auth_callback_url = %q", cbURL)
	}

	// 浏览器噪音：无参数 → 404 且会话仍 pending。
	noiseHost := strings.TrimPrefix(strings.TrimSuffix(cbURL, "/authorize"), "http://")
	nresp, err := http.Get("http://" + noiseHost + "/authorize")
	if err != nil {
		t.Fatalf("noise callback: %v", err)
	}
	_, _ = io.Copy(io.Discard, nresp.Body)
	nresp.Body.Close()
	if nresp.StatusCode != http.StatusNotFound {
		t.Errorf("噪音请求应 404，got %d", nresp.StatusCode)
	}

	// 真实回调：authCode + userInfo 回显。
	cbURL2 := cbURL + "?authCode=ac-1&login_trace_id=" + url.QueryEscape(q.Get("login_trace_id")) +
		"&userInfo=" + url.QueryEscape(`{"UserID":"u-echo","ScreenName":"echo"}`)
	cresp, err := http.Get(cbURL2)
	if err != nil {
		t.Fatalf("callback: %v", err)
	}
	_, _ = io.Copy(io.Discard, cresp.Body)
	cresp.Body.Close()
	if cresp.StatusCode != http.StatusOK {
		t.Fatalf("callback status = %d", cresp.StatusCode)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		st, werr := m.Wait(res.SessionID)
		if werr == nil && st.Status != StatusPending {
			if st.Status != StatusSuccess || st.AccountID == "" {
				t.Fatalf("wait = %+v", st)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("登录未在限时内完成（exchange/入池卡住）")
		}
		time.Sleep(50 * time.Millisecond)
	}

	if exchangeBody["ClientID"] != traeClientID || exchangeBody["AuthCode"] != "ac-1" {
		t.Errorf("exchange body = %+v", exchangeBody)
	}
	if v, ok := exchangeBody["CodeVerifier"].(string); !ok || len(v) < 40 {
		t.Errorf("CodeVerifier 缺失/过短: %+v", exchangeBody)
	}
	di, _ := exchangeBody["DeviceInfo"].(map[string]any)
	if di == nil || !strings.Contains(fmt.Sprint(di["DevicePublicKey"]), "BEGIN PUBLIC KEY") {
		t.Errorf("DeviceInfo 设备公钥缺失: %+v", di)
	}
	if userInfoAuth["token"] != "at-1" {
		t.Errorf("GetUserInfo 未带 access token: %+v", userInfoAuth)
	}

	list := store.List(account.PlatformTrae)
	if len(list) != 1 {
		t.Fatalf("trae 账号数 = %d", len(list))
	}
	a := list[0]
	if a.Label != "tester" {
		t.Errorf("label = %q（GetUserInfo 结果优先于回调回显）", a.Label)
	}
	var blob map[string]any
	if err := json.Unmarshal(a.Cred, &blob); err != nil {
		t.Fatalf("blob: %v", err)
	}
	for _, k := range []string{"accessToken", "refreshToken", "machineId", "deviceId",
		"devicePublicKey", "devicePrivateKey", "variant", "uid", "authClientId"} {
		if s, _ := blob[k].(string); s == "" {
			t.Errorf("blob 缺 %s", k)
		}
	}
	if blob["variant"] != "solo" {
		t.Errorf("variant = %v", blob["variant"])
	}
}

// ── 回调形态回归（v2.0.1）：authCodeInfo / 噪音容忍 / 登录身份回显无码 /
// 粘贴回调兜底。每种形态都用 mock 上游，零真实网络。 ───────────────────

type traeMockEnv struct {
	m        *Manager
	store    *account.Store
	srv      *httptest.Server
	exchange map[string]any
}

func newTraeMock(t *testing.T) *traeMockEnv {
	t.Helper()
	env := &traeMockEnv{exchange: map[string]any{}}
	env.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/cloudide/api/v3/trae/GetLoginGuidance":
			_, _ = io.WriteString(w, `{"Result":{"LoginHost":"www.trae.cn"}}`)
		case "/trae/api/v3/oauth/ExchangeToken":
			body := map[string]any{}
			_ = json.NewDecoder(r.Body).Decode(&body)
			for k, v := range body {
				env.exchange[k] = v
			}
			_, _ = io.WriteString(w, `{"Result":{"AccessToken":"at-1","RefreshToken":"rt-1"}}`)
		case "/cloudide/api/v3/trae/GetUserInfo":
			_, _ = io.WriteString(w, `{"Result":{"UserID":"u-1","ScreenName":"tester"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(env.srv.Close)
	store, err := account.Load(filepath.Join(t.TempDir(), "accounts.json"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	m, err := NewManager(Config{
		Store:            store,
		TraeIdentityFile: filepath.Join(t.TempDir(), "trae-device-identity.json"),
		TraeGuidanceURLs: []string{env.srv.URL + "/cloudide/api/v3/trae/GetLoginGuidance"},
		TraeAPIOrigins:   []string{env.srv.URL},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	env.m, env.store = m, store
	return env
}

// startLogin 发起一轮登录并取出回调地址（auth_callback_url 刻意不编码，走 Query 解析）。
func (e *traeMockEnv) startLogin(t *testing.T) (*StartResult, string) {
	t.Helper()
	res, err := e.m.Start(context.Background(), account.PlatformTrae, "", "")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	u, err := url.Parse(res.LoginURL)
	if err != nil {
		t.Fatalf("login url: %v", err)
	}
	cb := u.Query().Get("auth_callback_url")
	if cb == "" {
		t.Fatalf("login url 无 auth_callback_url: %s", res.LoginURL)
	}
	return res, cb
}

func waitTrae(t *testing.T, m *Manager, sid string) WaitStatus {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		st, err := m.Wait(sid)
		if err == nil && st.Status != StatusPending {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("登录未在限时内完成（status=%s err=%v）", st.Status, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// authCodeInfo（值是 URL 编码 JSON）是上游第二种回调形态，必须与 authCode 等价。
func TestTraeCallbackAuthCodeInfo(t *testing.T) {
	env := newTraeMock(t)
	res, cb := env.startLogin(t)
	info := url.QueryEscape(`{"authCode":"ac-info","scene":"sso"}`)
	resp, err := http.Get(cb + "?authCodeInfo=" + info + "&loginHost=" + url.QueryEscape("www.trae.cn"))
	if err != nil {
		t.Fatalf("callback: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	st := waitTrae(t, env.m, res.SessionID)
	if st.Status != StatusSuccess || st.AccountID == "" {
		t.Fatalf("wait = %+v", st)
	}
	if env.exchange["AuthCode"] != "ac-info" {
		t.Errorf("Exchange AuthCode = %v（authCodeInfo 未提取）", env.exchange["AuthCode"])
	}
}

// 真实授权页（www.trae.cn/authorization 的 TraeGetPCAuthCode 分支）回调形态：
// isRedirect=true + authCodeInfo=JSON.stringify(Result) + host + loginTraceID + userInfo。
func TestTraeParseCallbackLivePageShape(t *testing.T) {
	query := "isRedirect=true&scope=trae&authCodeInfo=" +
		url.QueryEscape(`{"AuthCode":"ac-live","AuthCodeToken":"tok"}`) +
		"&loginTraceID=95aa9ad1-1c6c-4971-98b6-60ac5a5dcc1e&host=" +
		url.QueryEscape("www.trae.cn") + "&userRegion=cn&userInfo=" +
		url.QueryEscape(`{"UserID":"u-live","ScreenName":"live-nick"}`)
	q, err := url.ParseQuery(query)
	if err != nil {
		t.Fatalf("ParseQuery: %v", err)
	}
	m := traeParseCallback(q)
	if m.AuthCode != "ac-live" || m.AuthCodeFrom != "authCodeInfo" {
		t.Errorf("AuthCode = %q（来自 %q），应取自 authCodeInfo", m.AuthCode, m.AuthCodeFrom)
	}
	if m.LoginHost != "www.trae.cn" {
		t.Errorf("LoginHost = %q，应取回调里的 host", m.LoginHost)
	}
	if m.UID != "u-live" || m.Nickname != "live-nick" {
		t.Errorf("身份回显 = %q/%q", m.UID, m.Nickname)
	}
	if m.ErrMsg != "" {
		t.Errorf("isRedirect=true 不应判失败: %q", m.ErrMsg)
	}
}

func TestTraeExtractAuthCodeInfo(t *testing.T) {
	cases := []struct{ raw, want string }{
		{`{"authCode":"a"}`, "a"},
		{`{"data":{"authCode":"b"}}`, "b"},
		{`[{"code":"c"}]`, "c"},
		{`"ac-str"`, "ac-str"},                     // Result 直接是字符串
		{url.QueryEscape(`{"authCode":"d"}`), "d"}, // Query() 只还原一层，这里再兜底一层
		{`not-json`, ""},
		{`{"other":"x"}`, ""},
	}
	for _, c := range cases {
		if got := traeExtractAuthCodeInfo(c.raw); got != c.want {
			t.Errorf("extract(%q) = %q, want %q", c.raw, got, c.want)
		}
	}
}

// 无法识别的探测请求（favicon/preconnect）回 404 且会话继续等待，不能把真回调挡在门外。
func TestTraeCallbackNoiseKeepsWaiting(t *testing.T) {
	env := newTraeMock(t)
	res, cb := env.startLogin(t)
	resp, err := http.Get(cb + "?favicon=1")
	if err != nil {
		t.Fatalf("noise callback: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("噪音请求应 404，got %d", resp.StatusCode)
	}
	s := env.m.getSession(res.SessionID)
	if s == nil {
		t.Fatal("噪音请求不应结束会话")
	}
	s.mu.Lock()
	st := s.status
	s.mu.Unlock()
	if st != StatusPending {
		t.Errorf("会话状态 = %s，应保持 pending", st)
	}
}

// 登录页真回调但没带授权材料（userInfo/loginHost 回显）：回中间页、会话继续
// pending（可能是上游中间跳转），页面点名参数但不得回显回调里的身份值。
func TestTraeCallbackIdentityWithoutCodeKeepsWaiting(t *testing.T) {
	env := newTraeMock(t)
	res, cb := env.startLogin(t)
	cb2 := cb + "?userInfo=" + url.QueryEscape(`{"UserID":"u-echo"}`) + "&loginHost=" + url.QueryEscape("www.trae.cn")
	resp, err := http.Get(cb2)
	if err != nil {
		t.Fatalf("callback: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("中间页应 200，got %d", resp.StatusCode)
	}
	page := string(body)
	if !strings.Contains(page, "授权码") || !strings.Contains(page, "userInfo") {
		t.Errorf("中间页应说明缺授权码并点名参数: %q", page)
	}
	if strings.Contains(page, "u-echo") {
		t.Errorf("中间页泄露了回调身份值: %q", page)
	}
	s := env.m.getSession(res.SessionID)
	if s == nil {
		t.Fatal("无授权材料的回调不应结束会话")
	}
	s.mu.Lock()
	st := s.status
	s.mu.Unlock()
	if st != StatusPending {
		t.Errorf("会话状态 = %s，应保持 pending（等后续回调或粘贴兜底）", st)
	}
}

// 粘贴回调兜底：非法粘贴不消耗会话，合法粘贴等价于真实浏览器回调。
func TestTraeSubmitCallbackPaste(t *testing.T) {
	env := newTraeMock(t)
	res, cb := env.startLogin(t)

	if err := env.m.SubmitCallback(res.SessionID, "http://127.0.0.1:1/authorize"); err == nil ||
		!strings.Contains(err.Error(), "查询参数") {
		t.Fatalf("无 query 的粘贴应报错，got %v", err)
	}
	if err := env.m.SubmitCallback(res.SessionID, "?foo=1"); err == nil ||
		!strings.Contains(err.Error(), "授权信息") {
		t.Fatalf("无材料的粘贴应报错，got %v", err)
	}
	if err := env.m.SubmitCallback(res.SessionID, cb+"?authCode=ac-paste"); err != nil {
		t.Fatalf("合法粘贴被拒: %v", err)
	}
	st := waitTrae(t, env.m, res.SessionID)
	if st.Status != StatusSuccess {
		t.Fatalf("wait = %+v", st)
	}
	if env.exchange["AuthCode"] != "ac-paste" {
		t.Errorf("Exchange AuthCode = %v", env.exchange["AuthCode"])
	}
}

// 回调带回的 loginHost 优先（www → api 派生候选必须出现在换证源里）。
func TestTraeAuthCodeURLsFromCallbackHost(t *testing.T) {
	env := newTraeMock(t)
	urls := env.m.traeAuthCodeURLs("https://www.example.com")
	if !strings.Contains(strings.Join(urls, " "), "https://api.example.com/trae/api/v3/oauth/ExchangeToken") {
		t.Errorf("loginHost 派生换证源缺失: %v", urls)
	}
}

// 回调带 error 参数 → 会话 failed + 页面 200（成败由面板轮询展示）。
func TestTraeCallbackError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"Result":{"LoginHost":"www.trae.cn"}}`)
	}))
	defer srv.Close()
	store, err := account.Load(filepath.Join(t.TempDir(), "accounts.json"))
	if err != nil {
		t.Fatal(err)
	}
	m, err := NewManager(Config{
		Store:            store,
		TraeIdentityFile: filepath.Join(t.TempDir(), "id.json"),
		TraeGuidanceURLs: []string{srv.URL},
		TraeAPIOrigins:   []string{srv.URL},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	res, err := m.Start(context.Background(), account.PlatformTrae, "", "")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	// 从授权 URL 取回调端口。
	i := strings.Index(res.LoginURL, "auth_callback_url=http://127.0.0.1:")
	if i < 0 {
		t.Fatalf("login url 无 callback: %s", res.LoginURL)
	}
	rest := res.LoginURL[i+len("auth_callback_url=http://127.0.0.1:"):]
	port := rest[:strings.Index(rest, "/authorize")]
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%s/authorize?error=denied", port))
	if err != nil {
		t.Fatalf("callback: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	st, err := m.Wait(res.SessionID)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if st.Status != StatusFailed || !strings.Contains(st.Error, "denied") {
		t.Fatalf("wait = %+v", st)
	}
	if len(store.List(account.PlatformTrae)) != 0 {
		t.Error("失败登录不应入池")
	}
}
