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
