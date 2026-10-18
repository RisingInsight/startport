package provider

import (
	"strings"
	"testing"
)

// 内置原生适配器必须已注册（装配期显式 RegisterNative）。
func TestNativeRegistryHasLoomy(t *testing.T) {
	na, ok := Native(LoomyAdapterName)
	if !ok {
		t.Fatalf("%q 未注册", LoomyAdapterName)
	}
	if na.Name() != LoomyAdapterName {
		t.Errorf("Name = %q", na.Name())
	}
	names := NativeNames()
	if len(names) == 0 || names[0] != LoomyAdapterName {
		t.Errorf("NativeNames = %v（应含并排序在前）", names)
	}
	for i := 1; i < len(names); i++ {
		if names[i-1] > names[i] {
			t.Errorf("NativeNames 未排序: %v", names)
		}
	}
}

// 名字归一：大小写与空白不敏感。
func TestNativeLookupNormalized(t *testing.T) {
	for _, in := range []string{"loomy", "Loomy", " LOOMY "} {
		if _, ok := Native(in); !ok {
			t.Errorf("Native(%q) 应命中", in)
		}
	}
	if _, ok := Native(""); ok {
		t.Error("空名不应命中")
	}
	if _, ok := Native("nope"); ok {
		t.Error("未注册名不应命中")
	}
}

// adapter 为空 → 按协议选（三协议基准行为不变）。
func TestAdapterForFallsBackToProtocol(t *testing.T) {
	cases := map[Protocol]string{
		ProtocolOpenAI:    "openai",
		ProtocolAnthropic: "anthropic",
		ProtocolGemini:    "gemini",
	}
	for proto := range cases {
		a, err := AdapterFor(proto, "")
		if err != nil {
			t.Fatalf("AdapterFor(%q, \"\"): %v", proto, err)
		}
		if a.Protocol() != proto {
			t.Errorf("AdapterFor(%q) 协议 = %q", proto, a.Protocol())
		}
	}
}

// 空协议按 openai（与 ParseProtocol 的老配置语义一致）。
func TestAdapterForEmptyProtocolDefaultsOpenAI(t *testing.T) {
	a, err := AdapterFor("", "")
	if err != nil {
		t.Fatalf("AdapterFor(\"\", \"\"): %v", err)
	}
	if a.Protocol() != ProtocolOpenAI {
		t.Errorf("协议 = %q", a.Protocol())
	}
	a, err = AdapterFor("", "loomy")
	if err != nil {
		t.Fatalf("AdapterFor(\"\", loomy): %v", err)
	}
	if na, ok := a.(NativeAdapter); !ok || na.Name() != LoomyAdapterName {
		t.Errorf("应选中 loomy 原生适配器, got %T", a)
	}
}

// native 非空 → 优先选中原生适配器（协议也匹配时）。
func TestAdapterForPrefersNative(t *testing.T) {
	a, err := AdapterFor(ProtocolOpenAI, "Loomy")
	if err != nil {
		t.Fatalf("AdapterFor: %v", err)
	}
	na, ok := a.(NativeAdapter)
	if !ok || na.Name() != LoomyAdapterName {
		t.Fatalf("应选中原生适配器, got %T", a)
	}
}

// 未注册的 adapter：必须报错，**不静默回落**到协议适配器。
func TestAdapterForUnknownNativeErrors(t *testing.T) {
	a, err := AdapterFor(ProtocolOpenAI, "nope")
	if err == nil {
		t.Fatalf("未注册 adapter 应报错, got adapter %T", a)
	}
	if !strings.Contains(err.Error(), "nope") {
		t.Errorf("错误应点名 adapter: %v", err)
	}
	if !strings.Contains(err.Error(), LoomyAdapterName) {
		t.Errorf("错误应给出可用清单: %v", err)
	}
}

// 原生适配器协议不匹配（loomy 只能是 openai）：报错而不是"凑合用"。
func TestAdapterForProtocolMismatch(t *testing.T) {
	_, err := AdapterFor(ProtocolAnthropic, LoomyAdapterName)
	if err == nil {
		t.Fatal("协议不匹配应报错")
	}
	if !strings.Contains(err.Error(), "openai") {
		t.Errorf("错误应说明支持哪个协议: %v", err)
	}
}

// 非法协议：报错并给出合法值清单。
func TestAdapterForInvalidProtocol(t *testing.T) {
	_, err := AdapterFor(Protocol("bogus"), "")
	if err == nil {
		t.Fatal("非法协议应报错")
	}
	if !strings.Contains(err.Error(), "openai") {
		t.Errorf("错误应给出合法值清单: %v", err)
	}
}

// 注册后同名覆盖、空名/空值忽略（注册表自身的边界）。
func TestRegisterNativeOverridesAndIgnoresEmpty(t *testing.T) {
	defer func() {
		// 恢复内置实现，避免污染其它用例。
		RegisterNative(NewLoomyAdapter())
	}()
	RegisterNative(nil)
	RegisterNative(&stubNative{name: ""})
	if _, ok := Native(""); ok {
		t.Error("空名不应注册")
	}
	RegisterNative(&stubNative{name: "stub"})
	if _, ok := Native("stub"); !ok {
		t.Error("stub 应已注册")
	}
	// 同名覆盖：后注册者生效。
	RegisterNative(&stubNative{name: "stub", tag: "second"})
	a, _ := Native("stub")
	if na, ok := a.(*stubNative); !ok || na.tag != "second" {
		t.Errorf("同名应后者覆盖, got %+v", a)
	}
	// 清理 stub，避免影响 NativeNames 的其它断言。
	nativeMu.Lock()
	delete(nativeRegistry, "stub")
	nativeMu.Unlock()
}

// stubNative 测试用的最小原生适配器（嵌入 openai 适配器满足接口）。
type stubNative struct {
	Adapter
	name string
	tag  string
}

func (s *stubNative) Name() string {
	if s.Adapter == nil {
		s.Adapter = NewOpenAIAdapter()
	}
	return s.name
}

func (s *stubNative) Protocol() Protocol { return ProtocolOpenAI }
