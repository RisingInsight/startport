package account_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/RisingInsight/startport/internal/account"
)

func TestTraeCredentialShape(t *testing.T) {
	flat := json.RawMessage(`{"accessToken":"jwt-1","uid":"u-1","machineId":"m-1","deviceId":"d-1","variant":"solo"}`)
	if err := account.ValidateCred(account.PlatformTrae, flat); err != nil {
		t.Fatalf("平铺凭证应通过: %v", err)
	}
	nested := json.RawMessage(`{"type":"trae","provider":"trae",
		"auth":{"accessToken":"jwt-2","refreshToken":"rt-2","machineId":"m-2","deviceId":"d-2"},
		"account":{"uid":"u-2","nickname":"小明"}}`)
	if err := account.ValidateCred(account.PlatformTrae, nested); err != nil {
		t.Fatalf("嵌套 CPA 形状应通过: %v", err)
	}
	if err := account.ValidateCred(account.PlatformTrae, json.RawMessage(`{"uid":"u"}`)); err == nil ||
		!strings.Contains(err.Error(), "accessToken") {
		t.Errorf("缺 accessToken 应报字段名: %v", err)
	}
	if err := account.ValidateCred(account.PlatformTrae, json.RawMessage(`{"accessToken":"jwt","variant":"intl"}`)); err == nil ||
		!strings.Contains(err.Error(), "国际版") {
		t.Errorf("intl 应拒绝: %v", err)
	}

	secret, err := account.PrimarySecret(account.PlatformTrae, nested)
	if err != nil {
		t.Fatalf("PrimarySecret: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(secret), &m); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{
		"accessToken": "jwt-2", "refreshToken": "rt-2", "uid": "u-2",
		"nickname": "小明", "machineId": "m-2", "deviceId": "d-2", "variant": "solo",
	} {
		if got, _ := m[key].(string); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}

	base, err := account.BaseURLOf(account.PlatformTrae, nested)
	if err != nil || base != "https://trae-api-cn.mchost.guru" {
		t.Errorf("BaseURLOf = %q, %v", base, err)
	}
	override := json.RawMessage(`{"accessToken":"jwt","base_url":"https://trae.example"}`)
	base, err = account.BaseURLOf(account.PlatformTrae, override)
	if err != nil || base != "https://trae.example" {
		t.Errorf("base_url 覆盖 = %q, %v", base, err)
	}

	if !account.NativePlatform(account.PlatformTrae) || account.AdapterOf(account.PlatformTrae) != "trae" {
		t.Error("TRAE 应注册为原生平台 adapter=trae")
	}
	// 刷新能力已接线，但仍按账号凭证动态判定：blob 缺 refreshToken 的粘贴
	// 凭证不广告可刷新；带 refreshToken 的浏览器登录产物才广告。
	a := &account.Account{Platform: account.PlatformTrae, Cred: flat}
	if account.CanRefresh(a) {
		t.Error("blob 缺 refreshToken 时不应广告可刷新")
	}
	withRefresh := json.RawMessage(`{"accessToken":"jwt-1","refreshToken":"rt-1","deviceId":"d-1"}`)
	b := &account.Account{Platform: account.PlatformTrae, Cred: withRefresh}
	if !account.CanRefresh(b) {
		t.Error("TRAE 刷新能力已接线：blob 带 refreshToken 时应广告可刷新")
	}
	// traeField 补读 account{} 后，CPA 形状（accessToken 只在 account{} 内）
	// 也应通过校验——traePrimarySecret 的 pick 早已把 accountPart 列为合法
	// 来源，校验层与投影层 / 执行层口径必须一致。
	cpa := json.RawMessage(`{"auth":{},"account":{"access_token":"jwt-3","refresh_token":"rt-3"}}`)
	if err := account.ValidateCred(account.PlatformTrae, cpa); err != nil {
		t.Errorf("CPA 形状 account{} 内的 access_token 应通过校验: %v", err)
	}
	if secret, err := account.PrimarySecret(account.PlatformTrae, cpa); err != nil || !strings.Contains(secret, "jwt-3") {
		t.Errorf("CPA 形状投影应能读出 account{} 的 accessToken（err=%v）", err)
	}
	c := &account.Account{Platform: account.PlatformTrae, Cred: cpa}
	if !account.CanRefresh(c) {
		t.Error("CPA 形状 account{} 内的 refresh_token 应广告可刷新")
	}
}
