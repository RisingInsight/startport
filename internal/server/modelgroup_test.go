package server

import (
	"testing"
	"time"

	"github.com/RisingInsight/startport/internal/auth"
	"github.com/RisingInsight/startport/internal/modelgroup"
	"github.com/RisingInsight/startport/internal/pool"
	"github.com/RisingInsight/startport/internal/upstream"
)

type testGroup = modelgroup.Def

func newTestGroupManager(t *testing.T, defs ...testGroup) *modelgroup.Manager {
	t.Helper()
	m := modelgroup.New()
	m.ReplaceAll(defs)
	return m
}

// postChatModel 用既有 postChat 助手发 model=group 形态的最小请求。
func postChatModel(t *testing.T, h *Handler, model string) int {
	t.Helper()
	body := `{"model":"` + model + `","messages":[{"role":"user","content":"hi"}]}`
	return postChat(t, h, body).Code
}

// 路由组解析：/v1/models 暴露组条目；group/<name> 请求被改写进既有链路
// （池成员 → 选号路径，空池 503 证明到达选号；未知/停用组 → 400）。
func TestModelGroupResolution(t *testing.T) {
	mg := newTestGroupManager(t,
		testGroup{Name: "deepseek-any", Members: []string{"cn:deepseek-v4.1-flash", "global:glm-5.3"}},
		testGroup{Name: "ext-any", Members: []string{"ext:nope/m1"}},
		testGroup{Name: "off", Members: []string{"cn:x"}, Disabled: true},
	)
	h := NewHandler(Config{
		Pool:        pool.New(""),
		Upstream:    upstream.New(), // 全量套件下动态模型缓存非空，避免 nil Upstream
		ModelGroups: mg,             // APIKey 空 = 关闭鉴权（与既有 postChat 助手同口径）
	})

	// /v1/models：组条目在最前，禁用组不出现，元数据带成员。
	code, body := getJSON(t, h, "/v1/models", "")
	if code != 200 {
		t.Fatalf("/v1/models = %d, want 200", code)
	}
	models, _ := body["data"].([]any)
	if len(models) == 0 {
		t.Fatal("/v1/models empty")
	}
	first, _ := models[0].(map[string]any)
	if first["id"] != "group/deepseek-any" || first["owned_by"] != "starport-group" {
		t.Fatalf("first model = %v, want group/deepseek-any", first["id"])
	}
	if members, _ := first["x_members"].([]any); len(members) != 2 {
		t.Fatalf("x_members = %v, want 2 entries", first["x_members"])
	}
	ids := map[string]bool{}
	for _, m := range models {
		if mm, ok := m.(map[string]any); ok {
			ids[mm["id"].(string)] = true
		}
	}
	if ids["group/off"] {
		t.Error("disabled group should not be listed")
	}
	if !ids["group/ext-any"] {
		t.Error("enabled ext-member group missing from list")
	}

	// 未定义的组名：400（而不是稀里糊涂当裸名进池）。
	if code := postChatModel(t, h, "group/nope"); code != 400 {
		t.Fatalf("unknown group = %d, want 400", code)
	}
	// 已停用的组：同样 400。
	if code := postChatModel(t, h, "group/off"); code != 400 {
		t.Fatalf("disabled group = %d, want 400", code)
	}
	// 非法组名形态（多段路径）：400。
	if code := postChatModel(t, h, "group/a/b"); code != 400 {
		t.Fatalf("malformed group = %d, want 400", code)
	}

	// 定义的组：解析到池成员 → 空池选号失败 503（证明改写已发生并到达选号路径）。
	if code := postChatModel(t, h, "group/deepseek-any"); code != 503 {
		t.Fatalf("defined group with empty pool = %d, want 503", code)
	}

	// ModelGroups 为 nil：group/ 前缀 400（协议保留段不放行成裸名）。
	h2 := NewHandler(Config{Pool: pool.New(""), Upstream: upstream.New()})
	if code := postChatModel(t, h2, "group/whatever"); code != 400 {
		t.Fatalf("nil manager group request = %d, want 400", code)
	}
}

// memberServable 池成员口径：模型级冷却挡住时不可用，realm 可服务即可用。
func TestMemberServablePool(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1"})
	h := NewHandler(Config{Pool: p, Upstream: upstream.New()})
	if !h.memberServable("cn:m1") {
		t.Fatal("healthy pool member should be servable")
	}
	// 全池冷却该模型 → 不可用。
	p.CooldownSoftForModel("u1", 1*time.Minute, time.Now().Add(1*time.Minute), "m2", "6004 test")
	if h.memberServable("cn:m2") {
		t.Fatal("model-cooled member should not be servable")
	}
	// 池成员冷却（账号级）→ realm 无可服务 → 不可用。
	p.Cooldown("u1", pool.CoolSoft, time.Minute, "test")
	if h.memberServable("cn:m1") {
		t.Fatal("all-cooled realm should not be servable")
	}
}

func TestIsGroupModelBoundaries(t *testing.T) {
	cases := map[string]bool{
		"group/x": true, "group/deepseek-any": true,
		"group/": false, "group/a/b": false, "": false,
		"cn:group/x": false, "ext:ch/group/x": false, "groupx": false,
	}
	for in, want := range cases {
		if got := isGroupModel(in); got != want {
			t.Errorf("isGroupModel(%q) = %v, want %v", in, got, want)
		}
	}
}
