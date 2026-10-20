// trae_test.go TRAE 日常养护（第二批）的契约测试：签到/刷新/余额。
// 全部走 httptest 注入（Base / traeExchangeBases / traeRefreshInvoke），不打真实上游。
package checkin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/RisingInsight/startport/internal/account"
)

// traeTestBlob 组测试 blob（map → JSON，避免在源码里出现凭据形态字面量）。
func traeTestBlob(extra map[string]any) *account.Account {
	m := map[string]any{"accessToken": "at", "deviceId": "dev-1", "machineId": "m-1", "refreshToken": "rt"}
	for k, v := range extra {
		m[k] = v
	}
	raw, _ := json.Marshal(m)
	return &account.Account{ID: "acc-test", Platform: account.PlatformTrae, Cred: raw}
}

// 签到：成功（回查积分明细进 Reward）+ 头束契约。
func TestTraeCheckinClaim(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == traeClaimPath && r.Method == http.MethodPost:
			if r.Header.Get("Authorization") != "Bearer at" {
				t.Errorf("claim 应带 Bearer token, got %q", r.Header.Get("Authorization"))
			}
			if r.Header.Get("x-device-id") != "dev-1" || r.Header.Get("x-user-region") != "CN" || r.Header.Get("x-app-type") != "trae" {
				t.Errorf("claim 头束契约缺失: %v", r.Header)
			}
			_, _ = w.Write([]byte(`{"code":0,"message":"签到成功"}`))
		case r.URL.Path == traeStatusPath && r.Method == http.MethodGet:
			if r.URL.Query().Get("did") != "dev-1" {
				t.Errorf("status 应带 did 查询参数, got %q", r.URL.RawQuery)
			}
			_, _ = w.Write([]byte(`{"code":0,"checked_in":true,"credits":30,"consecutive_days":2,"checkin_date":"2026-10-20"}`))
		default:
			t.Errorf("意外请求: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	c := &traeChecker{Base: srv.URL, Client: srv.Client()}
	res, err := c.CheckIn(context.Background(), traeTestBlob(nil))
	if err != nil {
		t.Fatalf("CheckIn: %v", err)
	}
	if res.Status != StatusOK || !strings.Contains(res.Reward, "30") {
		t.Errorf("签到结果异常: %+v", res)
	}
}

// 签到：官方 code 1001 = 服务未对该账号开放 → 记 already（不重试不标红）。
func TestTraeCheckinUnavailable1001(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":1001,"message":"not open"}`))
	}))
	defer srv.Close()
	c := &traeChecker{Base: srv.URL, Client: srv.Client()}
	res, err := c.CheckIn(context.Background(), traeTestBlob(nil))
	if err != nil {
		t.Fatalf("CheckIn: %v", err)
	}
	if res.Status != StatusAlready || !strings.Contains(res.Message, "1001") {
		t.Errorf("1001 应记 already: %+v", res)
	}
}

// 签到：「已签到」文案 = 幂等成功；其余非 0 码 = 失败。
func TestTraeCheckinAlreadyAndFail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":1002,"message":"今日已签到"}`))
	}))
	defer srv.Close()
	c := &traeChecker{Base: srv.URL, Client: srv.Client()}
	res, err := c.CheckIn(context.Background(), traeTestBlob(nil))
	if err != nil || res.Status != StatusAlready {
		t.Errorf("已签到文案应记 already: %+v err=%v", res, err)
	}

	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":500,"message":"系统繁忙"}`))
	}))
	defer srv2.Close()
	c2 := &traeChecker{Base: srv2.URL, Client: srv2.Client()}
	res2, err := c2.CheckIn(context.Background(), traeTestBlob(nil))
	if err != nil || res2.Status != StatusFailed || !strings.Contains(res2.Message, "系统繁忙") {
		t.Errorf("非 0 码应记 failed: %+v err=%v", res2, err)
	}
}

// 刷新：RefreshTrae 重写 blob（token/refreshToken/expiresAt），其余字段保留；
// 缺 refreshToken 报错不盲试。
func TestTraeRefresh(t *testing.T) {
	old := traeRefreshInvoke
	defer func() { traeRefreshInvoke = old }()
	traeRefreshInvoke = func(ctx context.Context, client *http.Client, rt string) (string, string, int64, error) {
		if rt != "rt" {
			t.Errorf("应透传旧 refreshToken, got %q", rt)
		}
		return "at-new", "rt-new", time.Now().Add(time.Hour).Unix(), nil
	}
	a := traeTestBlob(nil)
	blob, err := RefreshTrae(context.Background(), a)
	if err != nil {
		t.Fatalf("RefreshTrae: %v", err)
	}
	if blob["accessToken"] != "at-new" || blob["refreshToken"] != "rt-new" {
		t.Errorf("blob 未回写新值: %v", blob)
	}
	if blob["machineId"] != "m-1" || blob["deviceId"] != "dev-1" {
		t.Errorf("设备字段应保留: %v", blob)
	}

	_, err = RefreshTrae(context.Background(), traeTestBlob(map[string]any{"refreshToken": ""}))
	if err == nil || !strings.Contains(err.Error(), "refreshToken") {
		t.Errorf("缺 refreshToken 应报错: %v", err)
	}
}

// ExchangeToken 契约：路径/头/body 四字段 + 嵌套 Result 解析 + Cloud-IDE-JWT
// 前缀剥离 + 单副本轮换（未回新值沿用旧值）+ 多域兜底。
func TestTraeExchangeRefreshContract(t *testing.T) {
	var sawBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != traeExchangePath {
			t.Errorf("path = %q", r.URL.Path)
		}
		if _, ok := r.Header["X-Cloudide-Token"]; !ok {
			t.Error("刷新请求应带 x-cloudide-token 头（参考契约：空串）")
		}
		_ = json.NewDecoder(r.Body).Decode(&sawBody)
		_, _ = w.Write([]byte(`{"Result":{"Token":"Cloud-IDE-JWT abc.def","TokenExpireAt":1790000000000}}`))
	}))
	defer srv.Close()

	oldBases := traeExchangeBases
	defer func() { traeExchangeBases = oldBases }()
	traeExchangeBases = []string{srv.URL}

	access, refresh, exp, err := traeExchangeRefresh(context.Background(), srv.Client(), nil, "rt-old")
	if err != nil {
		t.Fatalf("traeExchangeRefresh: %v", err)
	}
	if access != "abc.def" {
		t.Errorf("Cloud-IDE-JWT 前缀应剥离: %q", access)
	}
	if refresh != "rt-old" {
		t.Errorf("未回新 refreshToken 应沿用旧值: %q", refresh)
	}
	if exp == 0 {
		t.Error("毫秒级 TokenExpireAt 应解析出 expiresAt")
	}
	if sawBody["ClientID"] != traeSoloClientID || sawBody["RefreshToken"] != "rt-old" ||
		sawBody["ClientSecret"] != "-" || sawBody["UserID"] != "" {
		t.Errorf("body 契约缺失: %v", sawBody)
	}

	// 轮换：响应带新 refreshToken → 采用新值。
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"Result":{"Token":"t2","RefreshToken":"rt-new"}}`))
	}))
	defer srv2.Close()
	traeExchangeBases = []string{srv2.URL}
	access2, refresh2, _, err := traeExchangeRefresh(context.Background(), srv2.Client(), nil, "rt-old")
	if err != nil || access2 != "t2" || refresh2 != "rt-new" {
		t.Errorf("轮换语义异常: %q %q err=%v", access2, refresh2, err)
	}

	// 错误信封：volcengine ResponseMetadata.Error.Message 进错误消息。
	srv3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ResponseMetadata":{"Error":{"Message":"invalid refresh token"}}}`))
	}))
	defer srv3.Close()
	traeExchangeBases = []string{srv3.URL}
	_, _, _, err = traeExchangeRefresh(context.Background(), srv3.Client(), nil, "rt-old")
	if err == nil || !strings.Contains(err.Error(), "invalid refresh token") {
		t.Errorf("错误信封应透出消息: %v", err)
	}
}

// 余额：订阅包求和（limit-used + remain 型）+ 最早到期；1001 与 401 分流。
func TestTraeBalance(t *testing.T) {
	future := time.Now().Add(72 * time.Hour).Unix()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != traeEntUsagePathV2 || r.Method != http.MethodPost {
			t.Errorf("path/method = %s %s", r.Method, r.URL.Path)
		}
		fmt.Fprintf(w, `{"code":0,"data":{"user_entitlement_pack_list":[`+
			`{"entitlement_pack_id":"p1","credits_limit":100,"usage":{"credits_amount":30},"end_time":%d},`+
			`{"entitlement_pack_id":"p2","remain":50}]}}`, future)
	}))
	defer srv.Close()

	snap, err := queryTraeBalanceAt(context.Background(), srv.URL, "at", "dev-1")
	if err != nil {
		t.Fatalf("queryTraeBalanceAt: %v", err)
	}
	if snap.Available != 120 {
		t.Errorf("余额应 = (100-30)+50 = 120, got %v", snap.Available)
	}
	if len(snap.Rows) != 2 || snap.ExpiresAt.IsZero() {
		t.Errorf("行数/到期异常: %+v", snap)
	}

	// 1001：积分服务未开放（不是登录态失效）。
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"code":1001}`))
	}))
	defer srv2.Close()
	_, err = queryTraeBalanceAt(context.Background(), srv2.URL, "at", "dev-1")
	if err == nil || !strings.Contains(err.Error(), "1001") || errors.Is(err, ErrAuthExpired) {
		t.Errorf("1001 不应判登录态失效: %v", err)
	}

	// 无 1001 的 401：登录态失效。
	srv3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"code":40101}`))
	}))
	defer srv3.Close()
	_, err = queryTraeBalanceAt(context.Background(), srv3.URL, "at", "dev-1")
	if !errors.Is(err, ErrAuthExpired) {
		t.Errorf("纯 401 应判登录态失效: %v", err)
	}
}
