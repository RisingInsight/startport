package checkin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/RisingInsight/startport/internal/account"
)

// qoderRefreshCred 带打包刷新串的设备流凭证（OAuth 三段形态）。
func qoderRefreshCred() json.RawMessage {
	return json.RawMessage(`{
		"access_token":"dt-old",
		"user_id":"u-1",
		"machine_id":"m-1",
		"refresh_token":"rt-old|u-1|m-1"
	}`)
}

// 设备流令牌：路径、头、打包刷新串原样回传，新裸 refresh 重新打包。
func TestQoderRefreshDeviceToken(t *testing.T) {
	var sawPath, sawMethod, sawUA, sawAuth, sawRefresh string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawPath, sawMethod = r.URL.Path, r.Method
		sawUA = r.Header.Get("User-Agent")
		sawAuth = r.Header.Get("Authorization")
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		sawRefresh, _ = body["refresh_token"].(string)
		_, _ = w.Write([]byte(`{"token":"dt-new","refresh_token":"rt-new","expires_at":"2026-12-01T00:00:00Z"}`))
	}))
	defer srv.Close()

	a := &account.Account{ID: "acc-q1", Platform: account.PlatformQoder, Cred: qoderRefreshCred()}
	blob, err := refreshQoderAt(context.Background(), srv.URL, a)
	if err != nil {
		t.Fatalf("refreshQoderAt: %v", err)
	}
	if sawMethod != http.MethodPost || sawPath != qoderDeviceRefreshPath {
		t.Errorf("请求形态 = %s %s, want POST %s", sawMethod, sawPath, qoderDeviceRefreshPath)
	}
	if sawUA != "qoder-local-proxy" {
		t.Errorf("User-Agent = %q, want qoder-local-proxy", sawUA)
	}
	if sawAuth != "" {
		t.Errorf("刷新不应带 Authorization（无签名刷新链），实际 %q", sawAuth)
	}
	if sawRefresh != "rt-old|u-1|m-1" {
		t.Errorf("refresh_token = %q, want 原样打包串", sawRefresh)
	}
	if got := blobString(blob, "access_token"); got != "dt-new" {
		t.Errorf("access_token = %q, want dt-new", got)
	}
	if got := blobString(blob, "refresh_token"); got != "rt-new|u-1|m-1" {
		t.Errorf("refresh_token = %q, want 新串按三段格式重新打包", got)
	}
	if got := blobString(blob, "expires_at"); got != "2026-12-01T00:00:00Z" {
		t.Errorf("expires_at = %q", got)
	}
}

// 作业令牌（jt-）走 jobToken 刷新路径，且兼容 data 嵌套响应。
func TestQoderRefreshJobTokenPath(t *testing.T) {
	var sawPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawPath = r.URL.Path
		_, _ = w.Write([]byte(`{"data":{"access_token":"jt-new"}}`))
	}))
	defer srv.Close()

	a := &account.Account{ID: "acc-q2", Platform: account.PlatformQoderIntl,
		Cred: json.RawMessage(`{"access_token":"jt-old","user_id":"u-2","machine_id":"m-2","refresh_token":"rt-jt|u-2|m-2"}`)}
	blob, err := refreshQoderAt(context.Background(), srv.URL, a)
	if err != nil {
		t.Fatalf("refreshQoderAt: %v", err)
	}
	if sawPath != qoderJobRefreshPath {
		t.Errorf("path = %q, want %q", sawPath, qoderJobRefreshPath)
	}
	if got := blobString(blob, "access_token"); got != "jt-new" {
		t.Errorf("access_token = %q, want jt-new", got)
	}
	if got := blobString(blob, "refresh_token"); got != "rt-jt|u-2|m-2" {
		t.Errorf("未返回新 refresh 时应保留原值，实际 %q", got)
	}
}

// 刷新失败语义：401 → ErrAuthExpired；缺 token → 明确的接口变更告警。
func TestQoderRefreshFailureModes(t *testing.T) {
	a := &account.Account{ID: "acc-q3", Platform: account.PlatformQoder, Cred: qoderRefreshCred()}

	srv401 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	_, err := refreshQoderAt(context.Background(), srv401.URL, a)
	srv401.Close()
	if !errors.Is(err, ErrAuthExpired) {
		t.Errorf("401 应返回 ErrAuthExpired，实际 %v", err)
	}

	srvEmpty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	_, err = refreshQoderAt(context.Background(), srvEmpty.URL, a)
	srvEmpty.Close()
	if err == nil || !strings.Contains(err.Error(), "缺少 token") {
		t.Errorf("缺 token 应显式告警，实际 %v", err)
	}

	// 字符串业务码 105 也必须判成登录态失效（数字/字符串双兼容）。
	srvCode := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":"105","msg":"token expired"}`))
	}))
	_, err = refreshQoderAt(context.Background(), srvCode.URL, a)
	srvCode.Close()
	if !errors.Is(err, ErrAuthExpired) {
		t.Errorf("字符串业务码 105 应返回 ErrAuthExpired，实际 %v", err)
	}
}

// fakeQoderExpiredChecker 模拟旧 token 失效、刷新后成功。
type fakeQoderExpiredChecker struct{}

func (fakeQoderExpiredChecker) Platform() string { return account.PlatformQoder }

func (fakeQoderExpiredChecker) CheckIn(_ context.Context, a *account.Account) (Result, error) {
	blob, err := decodeBlob(a)
	if err != nil {
		return Result{}, err
	}
	if blobString(blob, "access_token") == "dt-NEW" {
		return Result{Status: StatusOK, Reward: "100 credits"}, nil
	}
	return Result{}, authExpiredf("token 过期")
}

// 调度器闭环：签到 401 → 刷新 → 回写 → 用新 token 重试成功。
func TestQoderCheckinRefreshWritebackRetry(t *testing.T) {
	refreshCalls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refreshCalls++
		if r.URL.Path != qoderDeviceRefreshPath {
			t.Errorf("刷新 path = %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"token":"dt-NEW","refresh_token":"rt-NEW"}`))
	}))
	defer srv.Close()

	oldBase := qoderOpenAPIBase[account.PlatformQoder]
	qoderOpenAPIBase[account.PlatformQoder] = srv.URL
	defer func() { qoderOpenAPIBase[account.PlatformQoder] = oldBase }()

	store, _ := account.Load(t.TempDir() + "/accounts.json")
	store.SetLogf(func(string, ...any) {})
	defer store.Close()
	a, err := store.Add(account.PlatformQoder, qoderRefreshCred(), "", "")
	if err != nil {
		t.Fatal(err)
	}

	Register(fakeQoderExpiredChecker{})
	defer Register(newQoderChecker())

	refreshed := false
	s := NewScheduler(SchedulerConfig{
		Store: store, Logf: func(string, ...any) {},
		OnRefresh: func(platform, accountID string) {
			refreshed = true
			// 钩子必须在 UpdateCred 之后：否则投影会读到旧 secret。
			got := store.Get(accountID)
			var blob map[string]any
			_ = json.Unmarshal(got.Cred, &blob)
			if blob["access_token"] != "dt-NEW" {
				t.Errorf("OnRefresh 时 blob 仍是旧 token: %v", blob["access_token"])
			}
		},
	})
	defer s.Close()

	res, err := s.CheckinAccount(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("CheckinAccount: %v", err)
	}
	if res.Status != StatusOK || !strings.Contains(res.Reward, "100") {
		t.Fatalf("刷新后重试应成功: %+v %v", res, err)
	}
	if refreshCalls != 1 {
		t.Errorf("refreshCalls = %d, want 1", refreshCalls)
	}
	if !refreshed {
		t.Error("刷新回写后必须触发 OnRefresh（main 用它重投影 managed 渠道）")
	}
	got := store.Get(a.ID)
	var blob map[string]any
	if err := json.Unmarshal(got.Cred, &blob); err != nil {
		t.Fatal(err)
	}
	if blob["access_token"] != "dt-NEW" {
		t.Errorf("回写 access_token = %v, want dt-NEW", blob["access_token"])
	}
	if blob["refresh_token"] != "rt-NEW|u-1|m-1" {
		t.Errorf("回写 refresh_token = %v, want 重新打包", blob["refresh_token"])
	}
	if len(got.Checkin) == 0 || got.Checkin[0].Status != StatusOK {
		t.Errorf("签到历史未落账: %+v", got.Checkin)
	}
}
