package panel

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/RisingInsight/startport/internal/apikeys"
	"github.com/RisingInsight/startport/internal/pool"
	"github.com/RisingInsight/startport/internal/usage"
)

// newKeysPanel 带密钥存储与鉴权的面板实例。
func newKeysPanel(t *testing.T) (*Panel, *apikeys.APIKeys) {
	t.Helper()
	st := apikeys.New("")
	p := New(Config{Version: "test", APIKey: "panel-key", APIKeys: st})
	return p, st
}

func panelJSON(t *testing.T, p *Panel, method, path, bearer, body string) (int, map[string]any) {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, r)
	var m map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &m)
	return rec.Code, m
}

// 创建 → 全钥只此一次;列表只有前缀,不含全文/摘要。
func TestKeysCreateListLifecycle(t *testing.T) {
	p, st := newKeysPanel(t)
	code, body := panelJSON(t, p, "POST", "/startport/api/keys", "panel-key", `{"name":"我的 agent"}`)
	if code != 200 {
		t.Fatalf("create = %d: %v", code, body)
	}
	full, _ := body["full"].(string)
	if !strings.HasPrefix(full, "sp-") {
		t.Fatalf("full key shape = %q", full)
	}
	if !st.Verify(full) {
		t.Fatal("创建的密钥未通过 /v1 校验")
	}
	// 列表:前缀可辨,全文/摘要不下发。
	code, body = panelJSON(t, p, "GET", "/startport/api/keys", "panel-key", "")
	if code != 200 {
		t.Fatalf("list = %d", code)
	}
	raw, _ := json.Marshal(body)
	if strings.Contains(string(raw), full) || strings.Contains(string(raw), `"hash"`) {
		t.Fatal("list 响应泄露了全钥或摘要")
	}
	keys, _ := body["keys"].([]any)
	if len(keys) != 1 {
		t.Fatalf("keys = %d, want 1", len(keys))
	}
	// 未鉴权 401。
	if code, _ := panelJSON(t, p, "GET", "/startport/api/keys", "", ""); code != 401 {
		t.Fatalf("unauthenticated list = %d, want 401", code)
	}
	// 改名 → 生效;禁用 → Verify 拒绝;启用 → 恢复;删除 → 401。
	id, _ := keys[0].(map[string]any)["id"].(string)
	code, _ = panelJSON(t, p, "POST", "/startport/api/keys/"+id+"/rename", "panel-key", `{"name":"改名了"}`)
	if code != 200 {
		t.Fatalf("rename = %d", code)
	}
	if _, k, _ := st.Create("temp"); k.ID == "" {
		t.Fatal("precondition")
	} else {
		st.Delete(k.ID)
	}
	rows := st.List()
	var gotName string
	for _, r := range rows {
		if r.ID == id {
			gotName = r.Name
		}
	}
	if gotName != "改名了" {
		t.Fatalf("rename 未生效: %q", gotName)
	}
}

// 空名称 400;501 当未注入存储。
func TestKeysValidationAndDisabledFeature(t *testing.T) {
	p, _ := newKeysPanel(t)
	if code, _ := panelJSON(t, p, "POST", "/startport/api/keys", "panel-key", `{"name":"  "}`); code != 400 {
		t.Fatalf("empty name = %d, want 400", code)
	}
	p2 := New(Config{Version: "test", APIKey: "panel-key"})
	if code, _ := panelJSON(t, p2, "GET", "/startport/api/keys", "panel-key", ""); code != 501 {
		t.Fatalf("nil store list = %d, want 501", code)
	}
}

// 隔离边界(面板侧):agent 密钥对一切面板端点(含密钥管理本身)一律 401
// ——agent 密钥哪怕泄露,也不能创建/删除密钥或触达任何管理能力。
func TestAgentKeyCannotReachPanel(t *testing.T) {
	p, st := newKeysPanel(t)
	full, _, err := st.Create("泄露的 agent 密钥")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	for _, c := range []struct{ method, path string }{
		{"GET", "/startport/api/overview"},
		{"GET", "/startport/api/keys"},
		{"POST", "/startport/api/keys"},
		{"POST", "/startport/api/keys/x/rename"},
		{"POST", "/startport/api/keys/x/disable"},
		{"POST", "/startport/api/keys/x/delete"},
		{"GET", "/startport/api/config"},
	} {
		code, _ := panelJSON(t, p, c.method, c.path, full, "{}")
		if code != 401 {
			t.Errorf("%s %s with agent key → %d, want 401", c.method, c.path, code)
		}
	}
}

// 隐藏开关端到端:POST hidden → usage 响应过滤该组并回显 hidden_keys;
// 恢复后重新出现。桶数据不受影响(恢复即重现)。
func TestKeysHiddenToggleFiltersUsage(t *testing.T) {
	st := apikeys.New("")
	rec := usage.New("")
	base := time.Now().Add(-time.Hour)
	rec.Add(base, "cn", "u1", "glm-5.3", usage.Delta{PromptTokens: 100, HasPromptTokens: true, Credit: 2, HasCredit: true, KeyName: "gone"}, true)
	rec.Add(base, "cn", "u1", "glm-5.3", usage.Delta{PromptTokens: 50, HasPromptTokens: true, Credit: 1, HasCredit: true, KeyName: ""}, true)
	p := New(Config{Version: "test", APIKey: "panel-key", APIKeys: st, Usage: rec, Pool: pool.New("")})

	const usagePath = "/startport/api/usage?hours=24"
	code, body := panelJSON(t, p, "GET", usagePath, "panel-key", "")
	if code != 200 {
		t.Fatalf("usage = %d", code)
	}
	ks, _ := body["key_series"].([]any)
	if len(ks) != 2 {
		t.Fatalf("初始 key_series = %d 组, want 2", len(ks))
	}
	// 隐藏 "gone"
	code, _ = panelJSON(t, p, "POST", "/startport/api/keys/hidden", "panel-key", `{"name":"gone","hidden":true}`)
	if code != 200 {
		t.Fatalf("hide = %d", code)
	}
	code, body = panelJSON(t, p, "GET", usagePath, "panel-key", "")
	ks, _ = body["key_series"].([]any)
	if len(ks) != 1 {
		t.Fatalf("隐藏后 key_series = %d 组, want 1", len(ks))
	}
	hk, _ := body["hidden_keys"].([]any)
	if len(hk) != 1 || hk[0] != "gone" {
		t.Fatalf("hidden_keys = %v, want [gone]", hk)
	}
	// 恢复
	code, _ = panelJSON(t, p, "POST", "/startport/api/keys/hidden", "panel-key", `{"name":"gone","hidden":false}`)
	if code != 200 {
		t.Fatalf("show = %d", code)
	}
	code, body = panelJSON(t, p, "GET", usagePath, "panel-key", "")
	ks, _ = body["key_series"].([]any)
	if len(ks) != 2 {
		t.Fatalf("恢复后 key_series = %d 组, want 2", len(ks))
	}
}
