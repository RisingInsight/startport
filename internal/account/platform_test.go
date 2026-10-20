package account_test

import (
	"encoding/json"
	"testing"

	"github.com/RisingInsight/startport/internal/acclogin"
	"github.com/RisingInsight/startport/internal/account"
	"github.com/RisingInsight/startport/internal/checkin"
	"github.com/RisingInsight/startport/internal/provider"
)

// TestPlatformSpecsDerivedAccessors 注册表必须驱动所有派生访问器
// （ValidPlatform / NativePlatform / AdapterOf / BaseURLOf），避免出现
// "注册表里有、访问器不认"的分叉。
func TestPlatformSpecsDerivedAccessors(t *testing.T) {
	specs := account.Specs()
	if len(specs) == 0 {
		t.Fatal("平台注册表为空")
	}
	seen := map[string]bool{}
	for _, s := range specs {
		if s.Key == "" {
			t.Error("平台 key 不能为空")
			continue
		}
		if seen[s.Key] {
			t.Errorf("平台 key 重复: %q", s.Key)
		}
		seen[s.Key] = true
		if !account.ValidPlatform(s.Key) {
			t.Errorf("%s: ValidPlatform=false", s.Key)
		}
		if got := account.NativePlatform(s.Key); got != s.Native {
			t.Errorf("%s: NativePlatform=%v want %v", s.Key, got, s.Native)
		}
		if got := account.AdapterOf(s.Key); got != s.Adapter {
			t.Errorf("%s: AdapterOf=%q want %q", s.Key, got, s.Adapter)
		}
		if s.Native && s.Adapter == "" {
			t.Errorf("%s: 原生平台必须声明 adapter", s.Key)
		}
		if s.Refresh && !s.Native {
			t.Errorf("%s: 非原生平台不应声明刷新能力", s.Key)
		}
		if s.Balance && !s.Native {
			t.Errorf("%s: 非原生平台不应声明余额能力", s.Key)
		}
		base, err := account.BaseURLOf(s.Key, json.RawMessage(`{}`))
		if s.DefaultBaseURL == "" {
			if err == nil {
				t.Errorf("%s: 无默认基址时应报错，实际返回 %q", s.Key, base)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: BaseURLOf 失败: %v", s.Key, err)
			continue
		}
		if base != s.DefaultBaseURL {
			t.Errorf("%s: BaseURLOf=%q want %q", s.Key, base, s.DefaultBaseURL)
		}
	}
	if account.ValidPlatform("no-such-platform") {
		t.Error("未知平台不应被 ValidPlatform 接受")
	}
}

// TestPlatformSpecsCopy 返回的清单必须是副本（调用方修改不污染注册表）。
func TestPlatformSpecsCopy(t *testing.T) {
	first := account.Specs()
	first[0].Key = "mutated"
	second := account.Specs()
	if second[0].Key == "mutated" {
		t.Fatal("Specs() 返回了内部切片，调用方可篡改注册表")
	}
}

// TestPlatformSpecParityWithImplementations 单一事实来源的核心约束：
// 注册表声明的能力必须与各包实际注册的实现一一对应。
func TestPlatformSpecParityWithImplementations(t *testing.T) {
	// native 平台集合 == provider 已注册原生适配器集合。
	nativeSpecs := map[string]bool{}
	for _, s := range account.Specs() {
		if s.Native {
			nativeSpecs[s.Adapter] = true
		}
	}
	nativeImpls := map[string]bool{}
	for _, n := range provider.NativeNames() {
		nativeImpls[n] = true
	}
	assertSameSet(t, "native 平台", nativeSpecs, nativeImpls)

	// login 平台集合 == acclogin 支持集合。
	loginSpecs := map[string]bool{}
	for _, p := range account.LoginPlatforms() {
		loginSpecs[p] = true
	}
	loginImpls := map[string]bool{}
	for _, p := range acclogin.SupportedPlatforms() {
		loginImpls[p] = true
	}
	assertSameSet(t, "登录平台", loginSpecs, loginImpls)

	// checkin 能力双向一致：注册表 flag ↔ 实际 Checker。
	for _, s := range account.Specs() {
		if got := checkin.Supported(s.Key); got != s.Checkin {
			t.Errorf("%s: checkin.Supported=%v want %v", s.Key, got, s.Checkin)
		}
	}
	for _, p := range checkin.Platforms() {
		s, ok := account.SpecOf(p)
		if !ok || !s.Checkin {
			t.Errorf("checkin 已注册 %q，但注册表未声明该平台可签到", p)
		}
	}

	// 预设里的原生适配器必须能在注册表里找到同名的原生平台，反之亦然。
	presetAdapters := map[string]bool{}
	for _, p := range provider.Presets() {
		if p.Adapter != "" {
			presetAdapters[p.Adapter] = true
		}
	}
	for _, s := range account.Specs() {
		if s.Native && !presetAdapters[s.Adapter] {
			t.Errorf("%s: 预设目录缺少 adapter=%q 的条目", s.Key, s.Adapter)
		}
	}
	for a := range presetAdapters {
		s, ok := account.SpecOf(a)
		if !ok || s.Adapter != a {
			t.Errorf("预设 adapter=%q 没有对应的原生平台注册", a)
		}
	}
}

func assertSameSet(t *testing.T, label string, want, got map[string]bool) {
	t.Helper()
	if len(want) != len(got) {
		t.Errorf("%s 集合大小不一致: 注册表=%v 实现=%v", label, keys(want), keys(got))
		return
	}
	for k := range want {
		if !got[k] {
			t.Errorf("%s: 注册表有 %q，实现未注册（注册表=%v 实现=%v）", label, k, keys(want), keys(got))
		}
	}
	for k := range got {
		if !want[k] {
			t.Errorf("%s: 实现注册了 %q，注册表未声明（注册表=%v 实现=%v）", label, k, keys(want), keys(got))
		}
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
