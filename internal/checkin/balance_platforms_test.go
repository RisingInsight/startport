package checkin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/RisingInsight/startport/internal/account"
)

func TestBalanceSupported(t *testing.T) {
	for _, p := range []string{account.PlatformLoomy, account.PlatformAutoClaw,
		account.PlatformAutoClawIntl, account.PlatformQoder, account.PlatformQoderIntl} {
		if !BalanceSupported(p) {
			t.Errorf("%s 应支持余额查询", p)
		}
	}
	for _, p := range []string{account.PlatformTrae, account.PlatformCustom} {
		if BalanceSupported(p) {
			t.Errorf("%s 不应支持余额查询", p)
		}
	}
}

func TestLoomyBalance(t *testing.T) {
	var sawToken, sawBearer string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawToken = r.Header.Get("token")
		sawBearer = r.Header.Get("Authorization")
		if r.URL.Path != "/api/v2/points/records" {
			t.Errorf("path = %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"code":"000000","data":{"balance":5000,"dailyBalance":3000}}`))
	}))
	defer srv.Close()
	snap, err := queryLoomyBalanceAt(context.Background(), srv.URL, "sess-0123456789abcdef")
	if err != nil {
		t.Fatalf("queryLoomyBalanceAt: %v", err)
	}
	if sawToken != "sess-0123456789abcdef" || sawBearer != "Bearer sess-0123456789abcdef" {
		t.Errorf("双头异常: token=%q bearer=%q", sawToken, sawBearer)
	}
	if snap.Available != 8000 || snap.Unit != "积分" || len(snap.Rows) != 2 {
		t.Errorf("snapshot = %+v", snap)
	}
}

func TestAutoClawBalance(t *testing.T) {
	var sawPath, sawAuth, sawSign string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawPath = r.URL.Path
		sawAuth = r.Header.Get("authorization")
		sawSign = r.Header.Get("X-Auth-Sign")
		_, _ = w.Write([]byte(`{"code":0,"data":{"total_balance":450,"wallets":[
			{"public_wallet_type":"points","display_name":"积分","balance":450,"display":true,"priority":1},
			{"public_wallet_type":"promo","display_name":"赠送","balance":50,"display":false,"priority":2}
		]}}`))
	}))
	defer srv.Close()
	snap, err := queryAutoClawBalanceAt(context.Background(), srv.URL, false, "ac-token")
	if err != nil {
		t.Fatalf("queryAutoClawBalanceAt: %v", err)
	}
	if sawPath != "/agent-assetmgr/api/v2/wallets" || sawAuth != "Bearer ac-token" || sawSign == "" {
		t.Errorf("请求异常: path=%q auth=%q sign=%q", sawPath, sawAuth, sawSign)
	}
	if snap.Available != 450 || snap.Unit != "积分" || len(snap.Rows) != 1 ||
		snap.Rows[0].Name != "积分" || snap.Rows[0].Available != 450 {
		t.Errorf("snapshot = %+v", snap)
	}
}

func TestLoomyBalanceAuthExpired(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":"020002","desc":"登录已失效"}`))
	}))
	defer srv.Close()
	_, err := queryLoomyBalanceAt(context.Background(), srv.URL, strings.Repeat("s", 20))
	if err == nil || !strings.Contains(err.Error(), "登录态已失效") {
		t.Fatalf("业务码 020002 应报登录失效: %v", err)
	}
}
