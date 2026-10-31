package modelgroup

import "testing"

func def(name string, routing string, members ...string) Def {
	return Def{Name: name, Members: members, Routing: routing}
}

// order：首个可用成员优先；不可用成员被跳过；全员不可用回落首个（让请求走
// 既有错误路径拿精确原因，组层不吞错误）。
func TestPickOrderSkipsUnservable(t *testing.T) {
	m := New()
	m.ReplaceAll([]Def{def("g", "order", "cn:a", "ext:q/b", "global:c")})

	servable := map[string]bool{"cn:a": false, "ext:q/b": true, "global:c": true}
	member, got, ok := m.Pick("g", func(s string) bool { return servable[s] })
	if !ok || member != "ext:q/b" {
		t.Fatalf("pick = %q ok=%v, want ext:q/b（跳过不可用的首个）", member, ok)
	}
	// 首个恢复可用后回到首位。
	servable["cn:a"] = true
	if member, _, _ = m.Pick("g", func(s string) bool { return servable[s] }); member != "cn:a" {
		t.Fatalf("pick = %q, want cn:a（恢复后回首员）", member)
	}
	// 全员不可用：仍选首个。
	member, got, ok = m.Pick("g", func(string) bool { return false })
	if !ok || member != "cn:a" || got.Name != "g" {
		t.Fatalf("all-down pick = %q ok=%v, want cn:a（不吞错误）", member, ok)
	}
	// nil 判定 = 全员可用。
	if member, _, _ = m.Pick("g", nil); member != "cn:a" {
		t.Fatalf("nil servable pick = %q, want cn:a", member)
	}
}

// rotate：可用成员间轮转；不可用成员跳过且不消耗轮转位；全员不可用按轮转位返回。
func TestPickRotateSequence(t *testing.T) {
	m := New()
	m.ReplaceAll([]Def{def("r", "rotate", "cn:a", "cn:b", "cn:c")})
	pick := func(serv map[string]bool) string {
		member, _, _ := m.Pick("r", func(s string) bool { return serv[s] })
		return member
	}
	if got := pick(map[string]bool{"cn:a": true, "cn:b": true, "cn:c": true}); got != "cn:a" {
		t.Fatalf("1st = %q, want cn:a", got)
	}
	if got := pick(map[string]bool{"cn:a": true, "cn:b": true, "cn:c": true}); got != "cn:b" {
		t.Fatalf("2nd = %q, want cn:b", got)
	}
	// cn:c 本轮不可用 → 顺延到 cn:a。轮转指针按请求推进（c 也占一格），
	// 语义 = 每请求轮一格、格上不可用就顺延到下一个可用者。
	if got := pick(map[string]bool{"cn:a": true, "cn:b": true, "cn:c": false}); got != "cn:a" {
		t.Fatalf("3rd (c down) = %q, want cn:a", got)
	}
	// 第 4 次请求：指针回到格 0 → cn:a。
	if got := pick(map[string]bool{"cn:a": true, "cn:b": true, "cn:c": false}); got != "cn:a" {
		t.Fatalf("4th = %q, want cn:a", got)
	}
	// 全员不可用：按轮转位兜底（第 5 次 → 格 1 → cn:b）。
	if got := pick(map[string]bool{}); got != "cn:b" {
		t.Fatalf("all-down = %q, want cn:b（轮转位兜底）", got)
	}
}

// 未知/停用/空成员：ok=false，交由调用方回 400。
func TestPickUnknownAndDisabled(t *testing.T) {
	m := New()
	m.ReplaceAll([]Def{
		def("ok", "order", "cn:a"),
		def("off", "order", "cn:b"),
		def("empty", "order"),
	})
	if _, _, ok := m.Pick("nope", nil); ok {
		t.Fatal("unknown group should not resolve")
	}
	m.ReplaceAll([]Def{{Name: "off", Members: []string{"cn:b"}, Disabled: true}})
	if _, _, ok := m.Pick("off", nil); ok {
		t.Fatal("disabled group should not resolve")
	}
	if _, _, ok := m.Pick("empty", nil); ok {
		t.Fatal("empty group should not resolve")
	}
}

// ReplaceAll 后 Get/List 反映新表；轮转计数沿用（重建同名组不从头开始）。
func TestReplaceAllKeepsRotateCounter(t *testing.T) {
	m := New()
	m.ReplaceAll([]Def{def("r", "rotate", "cn:a", "cn:b")})
	m.Pick("r", nil) // 消耗一次（cn:a）
	m.Pick("r", nil) // cn:b
	m.ReplaceAll([]Def{def("r", "rotate", "cn:a", "cn:b")})
	if member, _, _ := m.Pick("r", nil); member != "cn:a" {
		t.Fatalf("after replace pick = %q, want cn:a（计数沿用）", member)
	}
}

func TestNormalizeRouting(t *testing.T) {
	for in, want := range map[string]string{
		"": "order", "order": "order", "rotate": "rotate", "bogus": "order",
	} {
		if got := NormalizeRouting(in); got != want {
			t.Errorf("NormalizeRouting(%q) = %q, want %q", in, got, want)
		}
	}
}
