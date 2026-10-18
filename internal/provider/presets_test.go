package provider

import (
	"strings"
	"testing"
)

// 预设目录：数量、key 唯一、base_url 恒空、native_required 标注。
func TestPresetsCatalog(t *testing.T) {
	list := Presets()
	if len(list) < 21 {
		t.Fatalf("预设条目 = %d, want ≥21", len(list))
	}
	keys := map[string]bool{}
	for _, p := range list {
		if p.Key == "" {
			t.Errorf("预设 %q 缺 key", p.Name)
		}
		if keys[p.Key] {
			t.Errorf("预设 key 重复: %q", p.Key)
		}
		keys[p.Key] = true
		if p.BaseURL != "" {
			t.Errorf("预设 %q 的 base_url 必须为空（地址由用户确认）: %q", p.Key, p.BaseURL)
		}
		if !p.Protocol.Valid() {
			t.Errorf("预设 %q 协议非法: %q", p.Key, p.Protocol)
		}
		if err := p.Auth.Validate("auth"); err != nil {
			// 空 auth 合法（协议默认）；非空则必须通过校验。
			if !p.Auth.IsZero() {
				t.Errorf("预设 %q 鉴权规格非法: %v", p.Key, err)
			}
		}
		if p.Note == "" {
			t.Errorf("预设 %q 缺备注", p.Key)
		}
	}
	// 三个协议基准必须在。
	for _, k := range []string{"siliconflow", "google-gemini", "anthropic-official", "volc-ark", "openrouter"} {
		if !keys[k] {
			t.Errorf("缺关键预设 %q", k)
		}
	}
	// native_required 至少覆盖 Qoder/TRAE/CatPaw/Loomy/华为云码道。
	for _, k := range []string{"qoder-cn", "trae", "catpaw", "loomy", "huawei-codearts"} {
		found := false
		for _, p := range list {
			if p.Key == k {
				found = true
				if !p.NativeRequired {
					t.Errorf("%q 应标注 native_required", k)
				}
			}
		}
		if !found {
			t.Errorf("缺预设 %q", k)
		}
	}
}

// 展示名 → 匹配键：大小写/空格/连字符/后缀不敏感。
func TestNormalizeKey(t *testing.T) {
	cases := map[string]string{
		"Qoder CN":          "qoder",
		"硅基流动":              "硅基流动",
		"NVIDIA NIM":        "nvidianim",
		"Google Gemini API": "googlegemini",
		"OPENROUTER":        "openrouter",
		"官方 Anthropic":      "anthropic",
	}
	for in, want := range cases {
		if got := NormalizeKey(in); got != want {
			t.Errorf("NormalizeKey(%q) = %q, want %q", in, got, want)
		}
	}
}

// 情报条目 → 预设：name 精确 → alias → vendor 三级降级。
func TestMatchPreset(t *testing.T) {
	if p, ok := MatchPreset("硅基流动", ""); !ok || p.Key != "siliconflow" {
		t.Errorf("中文名匹配失败: %v %v", p.Key, ok)
	}
	if p, ok := MatchPreset("TRAE CN", ""); !ok || p.Key != "trae" {
		t.Errorf("别名/规范化匹配失败: %v %v", p.Key, ok)
	}
	if p, ok := MatchPreset("unknown-platform", "字节跳动"); !ok || p.Key != "trae" {
		t.Errorf("vendor 降级匹配失败（字节跳动应命中 trae）: %v %v", p.Key, ok)
	}
	if _, ok := MatchPreset("完全没听过的平台", ""); ok {
		t.Error("未收录平台不应匹配")
	}
	// 协议建议值抽查（Gemini/Anthropic 基准）。
	p, _ := MatchPreset("Google Gemini", "")
	if p.Protocol != ProtocolGemini || p.Auth.Header != "x-goog-api-key" {
		t.Errorf("gemini 预设建议值异常: %+v", p)
	}
	if !strings.Contains(strings.ToLower(p.Note), "gemini") {
		t.Errorf("gemini 预设备注应可读: %q", p.Note)
	}
}
