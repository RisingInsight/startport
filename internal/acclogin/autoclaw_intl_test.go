package acclogin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/RisingInsight/startport/internal/account"
)

// TestAutoClawIntlOAuthFlow mock 上游三跳 + 本机回环回调，验证 captcha 透传、
// code 换 token、账号入池与 Wait 终态。
func TestAutoClawIntlOAuthFlow(t *testing.T) {
	var startBody, loginBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/userapi/overseasv1/oauth-captcha-config":
			_, _ = io.WriteString(w, `{"code":0,"data":{"enabled":true,"region":"ga","prefix":"sq51tr","scene_id":"scene-1","captcha_supplier":"aliyun"}}`)
		case "/userapi/overseasv1/zai-oauth-url":
			startBody = body
			_, _ = io.WriteString(w, `{"code":0,"data":{"oauth_url":"https://example.test/auth"}}`)
		case "/userapi/overseasv1/zai-oauth-login":
			loginBody = body
			_, _ = io.WriteString(w, `{"code":0,"data":{"access_token":"tok-1","refresh_token":"rt-1","user_id":"u-1","user_name":"tester"}}`)
		case "/userapi/v1/user-profile":
			_, _ = io.WriteString(w, `{"code":0,"data":{"email":"tester@example.com"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	store, err := account.Load(filepath.Join(t.TempDir(), "accounts.json"))
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	m, err := NewManager(Config{
		Store:               store,
		AutoClawIntlUserAPI: srv.URL,
		AutoClawOAuthAPI:    srv.URL,
		AutoClawIntlPorts:   []int{port},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	cfg, err := m.AutoClawCaptchaConfig(context.Background())
	if err != nil {
		t.Fatalf("captcha config: %v", err)
	}
	if cfg["scene_id"] != "scene-1" || cfg["enabled"] != true || cfg["region"] != "ga" {
		t.Fatalf("captcha config = %+v", cfg)
	}

	res, err := m.StartOAuth(context.Background(), account.PlatformAutoClawIntl, "captcha-param", "zai")
	if err != nil {
		t.Fatalf("StartOAuth: %v", err)
	}
	if res.Mode != "auth_url" || res.LoginURL != "https://example.test/auth" || res.SessionID == "" {
		t.Fatalf("start result = %+v", res)
	}
	if startBody["ali_captcha_verify_param"] != "captcha-param" {
		t.Errorf("captcha 未透传: %+v", startBody)
	}
	nav, _ := startBody["navigate_uri"].(string)
	if nav != fmt.Sprintf("http://localhost:%d/auth/callback-zai", port) {
		t.Errorf("navigate_uri = %q", nav)
	}

	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/auth/callback-zai?code=code-1&state=st-1", port))
	if err != nil {
		t.Fatalf("callback: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("callback status = %d", resp.StatusCode)
	}
	if loginBody["code"] != "code-1" || loginBody["state"] != "st-1" || loginBody["navigate_uri"] != nav {
		t.Errorf("exchange body = %+v", loginBody)
	}

	st, err := m.Wait(res.SessionID)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if st.Status != StatusSuccess || st.AccountID == "" {
		t.Fatalf("wait = %+v", st)
	}
	list := store.List(account.PlatformAutoClawIntl)
	if len(list) != 1 || list[0].Label != "tester" || list[0].ID != st.AccountID {
		t.Fatalf("accounts = %+v", list)
	}
}
