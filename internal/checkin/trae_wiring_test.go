package checkin

// trae_wiring_test.go 本次 BugFix 的回归护栏。
//
// 现象：TRAE 账号（浏览器登录导入、凭证有效）在面板里签到 / 余额 / 模型 /
// 请求全空，而顶部仍报 100% 可用。根因是 platformSpecs 的 TRAE 条目把
// Checkin/Refresh/Balance 三项能力位留成 false，checkin 注册表 / 刷新分发 /
// 余额分发也随之未接线——实现与契约单测其实早已就位。
//
// 本文件钉住「注册表声明 ↔ 注册表实现 ↔ 分发入口」整条链，防止再次回退；
// 全部走 httptest / 函数注入，不打真实上游，也不在源码里编造上游响应。

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/RisingInsight/startport/internal/account"
)

// traeWiringBlob 组测试 blob（map → JSON；值均为测试占位串，非真实凭据）。
func traeWiringBlob(extra map[string]any) *account.Account {
	m := map[string]any{"accessToken": "at-old", "deviceId": "dev-1"}
	for k, v := range extra {
		m[k] = v
	}
	raw, _ := json.Marshal(m)
	return &account.Account{
		ID:       "acc-trae-wiring",
		Platform: account.PlatformTrae,
		Cred:     json.RawMessage(raw),
	}
}

// TestTraeCapabilityWiring 平台能力位、checkin 注册表与账号级能力位三者一致。
func TestTraeCapabilityWiring(t *testing.T) {
	spec, ok := account.SpecOf(account.PlatformTrae)
	if !ok {
		t.Fatal("TRAE 不在平台注册表")
	}
	if !spec.Checkin || !spec.Refresh || !spec.Balance {
		t.Fatalf("TRAE 能力位未打开: %+v", spec)
	}
	if !Supported(account.PlatformTrae) {
		t.Error("checkin 注册表未注册 TRAE Checker（签到会被判「平台无此活动」）")
	}
	if !BalanceSupported(account.PlatformTrae) {
		t.Error("TRAE 余额能力未接通（BalanceSupported=false）")
	}

	// 能力位按账号凭证动态判定：无 refreshToken 的粘贴账号不广告刷新。
	if account.CanRefresh(traeWiringBlob(nil)) {
		t.Error("blob 缺 refreshToken 时不应广告可刷新")
	}
	logged := traeWiringBlob(map[string]any{"refreshToken": "rt-old"})
	if !account.CanRefresh(logged) {
		t.Error("浏览器登录产物（带 refreshToken）应广告可刷新")
	}
	// 嵌套 auth{} + 下划线键名也要被 CanRefresh 认出（与执行口径一致，
	// 否则会出现「能力位可用、执行时却报缺 refreshToken」的分叉）。
	nested := &account.Account{Platform: account.PlatformTrae,
		Cred: json.RawMessage(`{"auth":{"accessToken":"at","refresh_token":"rt"}}`)}
	if !account.CanRefresh(nested) {
		t.Error("嵌套 auth{} + refresh_token 应被 CanRefresh 认出")
	}
	// account{} 内的 refresh_token 同样是合法来源（QA Round 1 回归点：
	// traeField 曾漏读这一层，导致可刷新的 CPA 形状账号被低估）。
	accountPart := &account.Account{Platform: account.PlatformTrae,
		Cred: json.RawMessage(`{"account":{"access_token":"at","refresh_token":"rt"}}`)}
	if !account.CanRefresh(accountPart) {
		t.Error("account{} 内的 refresh_token 应被 CanRefresh 认出")
	}
	// 混合形状：auth{} 只给 accessToken、refresh_token 落在 account{}。
	// 修复前 traeField 查完 auth{}（命中 accessToken 但没有 refresh_token）
	// 就直接回落平铺，整层 account{} 被跳过 —— 这条与上面一条在旧实现下
	// 都会失败，正是护栏要证明的「修复前会红」。
	mixed := &account.Account{Platform: account.PlatformTrae,
		Cred: json.RawMessage(`{"auth":{"accessToken":"at"},"account":{"refresh_token":"rt-mixed"}}`)}
	if !account.CanRefresh(mixed) {
		t.Error("auth{} + account{} 混合形状的 refresh_token 应被 CanRefresh 认出")
	}
}

// TestTraeRefreshGateParity 能力位（account.CanRefresh）与执行层
// （checkin.RefreshTrae / traeBlobString）必须对同一组 blob 形状给出一致判定：
// 既不能「说可刷新但执行报缺字段」，也不能「能刷新却不广告」。
//
// QA Round 1 发现 traeField 漏读 account{}，使 CPA 形状粘贴凭证
// （refresh_token 只在 account{} 内）被低估：RefreshTrae 明明能读到 rt，
// CanRefresh 却返回 false，手动刷新端点 / 自动刷新调度 / 能力位展示三处
// 一起被挡。此用例把「双向一致」钉死，覆盖所有字段层与两种键名。
func TestTraeRefreshGateParity(t *testing.T) {
	shapes := []struct {
		name   string
		cred   string
		wantRT string // 期望执行层读到的 refreshToken；空 = 不应可刷新
	}{
		{"flat camelCase", `{"accessToken":"at","refreshToken":"rt-flat"}`, "rt-flat"},
		{"flat snake_case", `{"access_token":"at","refresh_token":"rt-snake"}`, "rt-snake"},
		{"nested auth", `{"auth":{"accessToken":"at","refreshToken":"rt-auth"}}`, "rt-auth"},
		{"nested account", `{"account":{"access_token":"at","refresh_token":"rt-account"}}`, "rt-account"},
		{"auth token + account rt", `{"auth":{"accessToken":"at"},"account":{"refresh_token":"rt-mixed"}}`, "rt-mixed"},
		{"account snake_case only", `{"account":{"access_token":"at","refresh_token":"RT-ACCT"}}`, "RT-ACCT"},
		{"no refresh token", `{"accessToken":"at"}`, ""},
	}
	for _, tc := range shapes {
		t.Run(tc.name, func(t *testing.T) {
			a := &account.Account{Platform: account.PlatformTrae, Cred: json.RawMessage(tc.cred)}
			blob, err := account.DecodeBlob(a.Cred)
			if err != nil {
				t.Fatalf("DecodeBlob: %v", err)
			}
			// 执行层读取口径（RefreshTrae 用的就是它）。
			gotRT := traeBlobString(blob, "refreshToken", "refresh_token")
			if gotRT != tc.wantRT {
				t.Errorf("执行层 rt = %q, want %q", gotRT, tc.wantRT)
			}
			// 能力位必须与执行层一致：wantRT 非空 ⇔ CanRefresh=true。
			gate := account.CanRefresh(a)
			if want := tc.wantRT != ""; gate != want {
				t.Errorf("CanRefresh 与执行层分叉: CanRefresh=%v, want %v (执行层 rt=%q)",
					gate, want, gotRT)
			}
		})
	}
}

// TestTraeRefreshAccountDispatch RefreshAccount 必须分发到 RefreshTrae，
// 不能落到 default 的 ErrPlatformNotSupported。
func TestTraeRefreshAccountDispatch(t *testing.T) {
	old := traeRefreshInvoke
	defer func() { traeRefreshInvoke = old }()
	var gotRT string
	traeRefreshInvoke = func(_ context.Context, _ *http.Client, rt string) (string, string, int64, error) {
		gotRT = rt
		return "at-new", "rt-new", 0, nil
	}
	// 用下划线键名，顺带验证 RefreshTrae 与 CanRefresh 的读取口径一致。
	a := &account.Account{Platform: account.PlatformTrae,
		Cred: json.RawMessage(`{"access_token":"at-old","refresh_token":"rt-old","device_id":"dev-1"}`)}
	updated, err := RefreshAccount(context.Background(), a)
	if err != nil {
		if errors.Is(err, ErrPlatformNotSupported) {
			t.Fatal("刷新分发未接线：返回 ErrPlatformNotSupported")
		}
		t.Fatalf("RefreshAccount(trae) 失败: %v", err)
	}
	if gotRT != "rt-old" {
		t.Errorf("refreshToken 透传 = %q, want rt-old", gotRT)
	}
	if updated["accessToken"] != "at-new" || updated["refreshToken"] != "rt-new" {
		t.Errorf("刷新结果未回写 blob: %+v", updated)
	}
}

// TestTraeQueryBalanceDispatch QueryBalance 必须分发到 queryTraeBalance。
// 用 httptest 注入基址，不打真实上游；响应是测试桩，不是生产代码编造的数字。
func TestTraeQueryBalanceDispatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != traeEntUsagePathV2 {
			t.Errorf("path = %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"code":0,"data":{"user_entitlement_pack_list":` +
			`[{"entitlement_pack_id":"p1","remain":42}]}}`))
	}))
	defer srv.Close()
	old := traeBalanceBase
	defer func() { traeBalanceBase = old }()
	traeBalanceBase = srv.URL

	snap, err := QueryBalance(context.Background(), traeWiringBlob(nil))
	if err != nil {
		if errors.Is(err, ErrPlatformNotSupported) {
			t.Fatal("余额分发未接线：返回 ErrPlatformNotSupported")
		}
		t.Fatalf("QueryBalance(trae) 失败: %v", err)
	}
	if snap == nil || snap.Available != 42 {
		t.Errorf("余额快照异常: %+v", snap)
	}
}

// TestTraeCheckinBlobCompat CheckIn 要能读嵌套 auth{} + snake_case 的粘贴凭证，
// 否则能力位打开后这类账号签到会误报「blob 缺 accessToken」。
func TestTraeCheckinBlobCompat(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case traeClaimPath:
			_, _ = w.Write([]byte(`{"code":0,"message":"ok"}`))
		case traeStatusPath:
			_, _ = w.Write([]byte(`{"code":0,"data":{}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	c := &traeChecker{Base: srv.URL, Client: srv.Client()}
	a := &account.Account{Platform: account.PlatformTrae,
		Cred: json.RawMessage(`{"auth":{"access_token":"at","device_id":"dev-1"}}`)}
	res, err := c.CheckIn(context.Background(), a)
	if err != nil {
		t.Fatalf("嵌套/下划线 blob 签到失败: %v", err)
	}
	if res.Status != StatusOK {
		t.Errorf("签到状态 = %+v", res)
	}
}
