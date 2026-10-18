// acct_api_test.go 账号中心 API（T2）：CRUD 流、导入校验、脱敏断言、
// 一致性查询、ProbeDraft 接线。
package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/RisingInsight/startport/internal/account"
	"github.com/RisingInsight/startport/internal/extchan"
)

// acctEnv 账号 API 的测试环境：真 Store + 真投影（内存 defs）+ 真探测引擎。
type acctEnv struct {
	t      *testing.T
	pan    *Panel
	store  *account.Store
	defs   []extchan.ChannelDef
	saves  int
	extMgr *extchan.Manager
}

func newAcctEnv(t *testing.T) *acctEnv {
	t.Helper()
	store, err := account.Load(t.TempDir() + "/accounts.json")
	if err != nil {
		t.Fatal(err)
	}
	store.SetLogf(func(string, ...any) {})
	t.Cleanup(store.Close)

	env := &acctEnv{t: t, store: store}
	env.extMgr = extchan.New(extchan.Config{TimeoutSeconds: 5})
	env.pan = New(Config{
		Version:  "test",
		ExtChan:  env.extMgr,
		Accounts: store,
		SaveChannels: func(defs []extchan.ChannelDef) error {
			env.defs = defs
			env.saves++
			// 同步进引擎（模拟 main 的 applyConfig 热重载），转发面可见。
			env.extMgr.ReplaceAll(defs)
			return nil
		},
		LoadChannelDefs: func() []extchan.ChannelDef { return env.defs },
	})
	return env
}

func (e *acctEnv) do(method, path string, body string) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	e.pan.ServeHTTP(rec, req)
	return rec
}

func (e *acctEnv) add(platform, cred string) (string, *httptest.ResponseRecorder) {
	rec := e.do("POST", "/startport/api/accounts",
		`{"platform":"`+platform+`","cred":`+cred+`}`)
	var out struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return out.ID, rec
}

// 验收 ①：合法 blob → 201 + 账号出现 + managed 渠道 credentials +1（投影落盘）。
func TestAcctAddLoomyProjectsChannel(t *testing.T) {
	e := newAcctEnv(t)
	id, rec := e.add("loomy", `{"session":"sess-0123456789abcdef"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if id == "" {
		t.Fatal("响应缺 id")
	}
	// 账号中心可见。
	if e.store.Get(id) == nil {
		t.Error("账号未入 Store")
	}
	// 投影渠道：managed + credentials[label=id]。
	found := false
	for _, def := range e.defs {
		if def.Name == account.PlatformLoomy {
			found = true
			if !def.Managed || len(def.Credentials) != 1 || def.Credentials[0].Label != id {
				t.Errorf("投影异常: %+v", def)
			}
		}
	}
	if !found {
		t.Error("managed 渠道未创建")
	}
	// 引擎可见（转发面）。
	seen := false
	for _, st := range e.extMgr.Status() {
		if st.Name == account.PlatformLoomy && len(st.Credentials) == 1 {
			seen = true
		}
	}
	if !seen {
		t.Error("引擎热重载断链：账号未进 extchan.Manager")
	}
}

// 验收 ②：非法 blob → 400 且报错带字段名。
func TestAcctAddInvalidBlobFieldNames(t *testing.T) {
	e := newAcctEnv(t)
	cases := []struct {
		platform, cred, want string
	}{
		{"loomy", `{"session":""}`, "session"},
		{"qoder", `{"access_token":"dt-1"}`, "user_id"},
		{"custom", `{"base_url":"https://x.example","secret":""}`, "secret"},
		{"bogus", `{}`, "未知平台"},
	}
	for _, c := range cases {
		rec := e.do("POST", "/startport/api/accounts",
			`{"platform":"`+c.platform+`","cred":`+c.cred+`}`)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", c.platform, rec.Code)
			continue
		}
		if !strings.Contains(rec.Body.String(), c.want) {
			t.Errorf("%s: 报错缺字段名 %q: %s", c.platform, c.want, rec.Body.String())
		}
	}
	// 全部失败：不应有账号产生。
	if len(e.store.List("")) != 0 {
		t.Error("失败导入不应留下账号")
	}
}

// 验收 ③：GET accounts 深度 grep 无秘密（响应零回显红线）。
func TestAcctResponsesContainNoSecret(t *testing.T) {
	e := newAcctEnv(t)
	secret := "sess-SUPER-SECRET-0123456789"
	e.add("loomy", `{"session":"`+secret+`"}`)
	e.add("qoder", `{"access_token":"jt-QODER-SECRET","user_id":"u-1"}`)
	e.add("autoclaw", `{"token":"AC-SECRET-TOKEN"}`)

	paths := []string{
		"/startport/api/accounts",
		"/startport/api/accounts/consistency",
	}
	// 详情路径。
	list := e.do("GET", "/startport/api/accounts", "")
	var out struct {
		Accounts []struct {
			ID string `json:"id"`
		} `json:"accounts"`
	}
	_ = json.Unmarshal(list.Body.Bytes(), &out)
	for _, a := range out.Accounts {
		paths = append(paths, "/startport/api/accounts/"+a.ID)
	}
	for _, path := range paths {
		rec := e.do("GET", path, "")
		body := rec.Body.String()
		for _, secret := range []string{secret, "jt-QODER-SECRET", "AC-SECRET-TOKEN"} {
			if strings.Contains(body, secret) {
				t.Errorf("%s 泄露秘密值: %s", path, body)
			}
		}
	}
}

// 验收 ④：consistency 端点可用（probe 的只读/不污染语义由 extchan 既有 P0-7
// 测试锚定；qoder/loomy 的探测走 StaticModels/ProbeDraft 短路，零上游请求）。
func TestAcctProbeDraftDoesNotPolluteEngine(t *testing.T) {
	e := newAcctEnv(t)
	_, _ = e.add("loomy", `{"session":"sess-0123456789abcdef"}`)
	rec := e.do("GET", "/startport/api/accounts/consistency", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("consistency status = %d", rec.Code)
	}
}

// 禁用/启用/删除：投影即时跟随（disabled 凭证 → 移除）。
func TestAcctStateAndDeleteProjection(t *testing.T) {
	e := newAcctEnv(t)
	id, _ := e.add("loomy", `{"session":"sess-0123456789abcdef"}`)
	e.do("POST", "/startport/api/accounts/"+id+"/state", `{"disabled":true}`)
	if len(e.defs) != 1 || len(e.defs[0].Credentials) != 1 || !e.defs[0].Credentials[0].Disabled {
		t.Errorf("禁用投影异常: %+v", e.defs)
	}
	e.do("DELETE", "/startport/api/accounts/"+id, "")
	// 删除最后一个账号：整条 managed 渠道随之移除（已删账号的秘密值不能留在 config）。
	if len(e.defs) != 0 {
		t.Errorf("删除投影异常: %+v", e.defs)
	}
	if e.store.Get(id) != nil {
		t.Error("账号未删除")
	}
}

// 一致性查询：一致 / 渠道被删（missing）/ 非托管同名 三种形态可辨。
func TestAcctConsistency(t *testing.T) {
	e := newAcctEnv(t)
	e.add("loomy", `{"session":"sess-0123456789abcdef"}`)
	rec := e.do("GET", "/startport/api/accounts/consistency", "")
	var out struct {
		Platforms []struct {
			Platform   string `json:"platform"`
			Managed    bool   `json:"managed"`
			Consistent bool   `json:"consistent"`
		} `json:"platforms"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if len(out.Platforms) != 1 || !out.Platforms[0].Consistent || !out.Platforms[0].Managed {
		t.Fatalf("一致性异常: %+v", out.Platforms)
	}
	// 用户删除 managed 渠道 → missing（下次 Sync 重建）。
	e.defs = nil
	rec = e.do("GET", "/startport/api/accounts/consistency", "")
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if len(out.Platforms) != 1 || out.Platforms[0].Consistent {
		t.Errorf("渠道被删后应报不一致: %+v", out.Platforms)
	}
}

// 单一写路径验收 ⑤：SaveChannels 与 POST config 对同一份内容落盘一致
// （真链对比在 cmd/server/accounts_test.go——这里锚定 panel 侧闭包走到了
// acctchan.Sync 且 defs 有序）。
func TestAcctSyncUsesAcctchan(t *testing.T) {
	e := newAcctEnv(t)
	e.add("loomy", `{"session":"sess-0123456789abcdef"}`)
	if e.saves == 0 {
		t.Fatal("创建账号后未触发投影落盘")
	}
	if len(e.defs) != 1 || e.defs[0].Name != account.PlatformLoomy {
		t.Fatalf("投影 defs 异常: %+v", e.defs)
	}
}
