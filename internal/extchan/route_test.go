package extchan

import (
	"strings"
	"testing"
)

// 前缀路由 + 别名解析：display 保留别名，upstream 是真实模型名。
func TestRouteAlias(t *testing.T) {
	m := newTestManager()
	m.ReplaceAll([]ChannelDef{{
		Name: "sf", BaseURL: "https://sf",
		Models:  []string{"Qwen/Qwen3-235B-A22B"},
		Aliases: map[string]string{"fast": "Qwen/Qwen3-8B"},
	}})
	ch, upstream, display, err := m.RouteModel("ext:sf/fast")
	if err != nil {
		t.Fatal(err)
	}
	if ch.Name != "sf" || upstream != "Qwen/Qwen3-8B" || display != "ext:sf/fast" {
		t.Errorf("别名路由异常: ch=%s upstream=%q display=%q", ch.Name, upstream, display)
	}
	// 兼容包装：RouteFor 只回上游名。
	if _, bare, err := m.RouteFor("ext:sf/fast"); err != nil || bare != "Qwen/Qwen3-8B" {
		t.Errorf("RouteFor 别名解析异常: %q %v", bare, err)
	}
	// 非别名原样。
	if _, bare, _, err := m.RouteModel("ext:sf/Qwen/Qwen3-235B-A22B"); err != nil || bare != "Qwen/Qwen3-235B-A22B" {
		t.Errorf("普通模型路由异常: %q %v", bare, err)
	}
	// 未知渠道。
	if _, _, _, err := m.RouteModel("ext:ghost/m"); err == nil {
		t.Error("未知渠道应报错")
	}
}

// 裸名路由的候选集 = models ∪ aliases；多渠道冲突要求前缀消歧。
func TestRouteBareAliasAndConflict(t *testing.T) {
	m := newTestManager()
	m.ReplaceAll([]ChannelDef{
		{Name: "a", BaseURL: "https://a", Aliases: map[string]string{"fast": "up-a"}},
		{Name: "b", BaseURL: "https://b", Aliases: map[string]string{"fast": "up-b"}},
	})
	if _, _, _, err := m.RouteModel("fast"); err == nil || !strings.Contains(err.Error(), "multiple ext channels") {
		t.Errorf("多渠道同名别名应要求前缀消歧, got %v", err)
	}
	ch, upstream, display, err := m.RouteModel("ext:b/fast")
	if err != nil || ch.Name != "b" || upstream != "up-b" || display != "ext:b/fast" {
		t.Errorf("前缀应消歧: %v %q %q", err, upstream, display)
	}
	// 单渠道裸名别名可直达。
	m.ReplaceAll([]ChannelDef{{Name: "a", BaseURL: "https://a", Aliases: map[string]string{"only": "up"}}})
	if _, upstream, display, err := m.RouteModel("only"); err != nil || upstream != "up" || display != "ext:a/only" {
		t.Errorf("裸名别名路由异常: %v %q %q", err, upstream, display)
	}
}

// /v1/models 合并：模型 + 别名（带 alias_of）；停用渠道不出现。
func TestModelListWithAliases(t *testing.T) {
	m := newTestManager()
	m.ReplaceAll([]ChannelDef{
		{Name: "sf", BaseURL: "https://sf", Models: []string{"m1"}, Aliases: map[string]string{"fast": "m1-mini"}},
		{Name: "off", BaseURL: "https://off", Models: []string{"m2"}, Disabled: true},
	})
	list := m.ModelList(nil)
	ids := map[string]map[string]any{}
	for _, e := range list {
		ids[e["id"].(string)] = e
	}
	if _, ok := ids["ext:sf/m1"]; !ok {
		t.Error("缺模型条目 ext:sf/m1")
	}
	alias, ok := ids["ext:sf/fast"]
	if !ok {
		t.Fatal("缺别名条目 ext:sf/fast")
	}
	if alias["alias_of"] != "m1-mini" {
		t.Errorf("别名条目缺 alias_of: %v", alias)
	}
	if alias["owned_by"] != "ext:sf" {
		t.Errorf("owned_by 口径应保持 ext:<渠道>: %v", alias["owned_by"])
	}
	if _, ok := ids["ext:off/m2"]; ok {
		t.Error("停用渠道不应出现在 /v1/models")
	}
	// ModelExists 认别名。
	if !m.ModelExists("sf", "fast") || !m.ModelExists("sf", "m1") {
		t.Error("ModelExists 应同时认模型与别名")
	}
}
