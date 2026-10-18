// accounts_test.go 账号中心的托管规则与单一写路径（v1.6.0 · T1 验收 ③④、
// 架构 §4.5 冲突规则逐条锚点）。
//
// 走**真实** saveConfig 链（ParseConfig 校验 / 哨兵回填 / 深合并 / 原子写 /
// 热重载），不用假闭包——"单一写路径"的断言只有打到同一份实现上才有意义。
package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RisingInsight/startport/internal/account"
	"github.com/RisingInsight/startport/internal/acctchan"
	"github.com/RisingInsight/startport/internal/extchan"
	"github.com/RisingInsight/startport/internal/livecfg"
	"github.com/RisingInsight/startport/internal/pool"
	"github.com/RisingInsight/startport/internal/scheduler"
	"github.com/RisingInsight/startport/internal/upstream"
	"github.com/RisingInsight/startport/internal/usage"
)

// acctTestEnv 可变配置落盘链的完整环境（真实依赖，临时目录）。
type acctTestEnv struct {
	t      *testing.T
	dir    string
	path   string
	live   *livecfg.Holder
	pool   *pool.Pool
	up     *upstream.Client
	sch    *scheduler.Scheduler
	extMgr *extchan.Manager
}

func newAcctTestEnv(t *testing.T) *acctTestEnv {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	base := `{
	  "listen": "127.0.0.1:0",
	  "ext_channels": []
	}`
	if err := os.WriteFile(path, []byte(base), 0o600); err != nil {
		t.Fatal(err)
	}
	env := &acctTestEnv{
		t:      t,
		dir:    dir,
		path:   path,
		live:   livecfg.New(livecfg.Snapshot{}),
		pool:   pool.New(filepath.Join(dir, "state.json")),
		up:     upstream.New(),
		sch:    scheduler.New(scheduler.Config{}),
		extMgr: extchan.New(extchan.Config{TimeoutSeconds: 5}),
	}
	// 初始热应用（与启动同链）。
	if _, err := env.save([]byte(base)); err != nil {
		t.Fatalf("初始保存失败: %v", err)
	}
	return env
}

// save 走真实 saveConfig（面板 POST config 的同一条入口）。
func (e *acctTestEnv) save(raw []byte) ([]string, error) {
	return saveConfig(raw, e.path, e.live, e.pool, e.up, e.sch, e.extMgr)
}

// saveChannels 走账号中心投影入口（与 saveConfig 共用 applyConfig）。
func (e *acctTestEnv) saveChannels(defs []extchan.ChannelDef) error {
	return saveChannelsAccounts(defs, e.path, e.live, e.pool, e.up, e.sch, e.extMgr)
}

// loadDefs 读当前盘上的渠道定义（acctchan.Loader 的真实实现）。
func (e *acctTestEnv) loadDefs() []extchan.ChannelDef {
	raw, err := os.ReadFile(e.path)
	if err != nil {
		return nil
	}
	cfg, err := ParseConfig(raw)
	if err != nil {
		return nil
	}
	return extChannelDefs(cfg.ExtChannels)
}

// onDisk 断言用：读盘上 ext_channels 的原始 JSON。
func (e *acctTestEnv) onDisk(t *testing.T) []map[string]any {
	t.Helper()
	raw, err := os.ReadFile(e.path)
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		ExtChannels []map[string]any `json:"ext_channels"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m.ExtChannels
}

// 验收 ①：老配置（无任何新键）走一遍保存 → 不出现 managed/origin/accounts 键。
func TestOldConfigStaysByteCompatible(t *testing.T) {
	e := newAcctTestEnv(t)
	raw, _ := os.ReadFile(e.path)
	if strings.Contains(string(raw), `"managed"`) || strings.Contains(string(raw), `"origin"`) ||
		strings.Contains(string(raw), `"accounts"`) {
		t.Errorf("老配置不应出现新键: %s", raw)
	}
	// 一次常规保存后仍无新键（omitempty）。
	_, err := e.save([]byte(`{"listen":"127.0.0.1:0","ext_channels":[
	  {"name":"manual","base_url":"https://api.example.com","api_key":"sk-x"}
	]}`))
	if err != nil {
		t.Fatal(err)
	}
	chans := e.onDisk(t)
	if len(chans) != 1 {
		t.Fatalf("渠道数 = %d", len(chans))
	}
	for _, key := range []string{"managed", "origin", "accounts"} {
		if _, exists := chans[0][key]; exists {
			t.Errorf("普通渠道不应出现 %q 键", key)
		}
	}
}

// 验收 ②：账号 → Sync → config 出现 managed 渠道，内容与手写等价（可转发）。
func TestProjectionCreatesManagedChannel(t *testing.T) {
	e := newAcctTestEnv(t)
	store, err := account.Load(filepath.Join(e.dir, "accounts.json"))
	if err != nil {
		t.Fatal(err)
	}
	store.SetLogf(func(string, ...any) {})
	t.Cleanup(store.Close)
	a, err := store.Add(account.PlatformLoomy,
		json.RawMessage(`{"session":"sess-0123456789abcdef"}`), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := acctchan.Sync(store, e.loadDefs, e.saveChannels, func(string, ...any) {}); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	// 引擎热重载已被 applyConfig 触发：managed 渠道在运行态可见。
	found := false
	for _, st := range e.extMgr.Status() {
		if st.Name == account.PlatformLoomy {
			found = true
			if !st.Managed {
				t.Error("运行态渠道缺 managed 标记")
			}
			if len(st.Credentials) != 1 || st.Credentials[0].Label != a.ID {
				t.Errorf("运行态凭证异常: %+v", st.Credentials)
			}
		}
	}
	if !found {
		t.Fatal("managed 渠道未进引擎（热重载断链）")
	}
	// 盘上形态：managed/origin/accounts 三键齐全，secret 是主凭证本体。
	chans := e.onDisk(t)
	for _, ch := range chans {
		if ch["name"] == account.PlatformLoomy {
			if ch["managed"] != true || ch["origin"] != "account-center" {
				t.Errorf("盘上托管标记缺失: %+v", ch)
			}
			creds, _ := ch["credentials"].([]any)
			if len(creds) != 1 {
				t.Fatalf("盘上凭证数 = %d", len(creds))
			}
			c0, _ := creds[0].(map[string]any)
			if c0["label"] != a.ID || c0["secret"] != "sess-0123456789abcdef" {
				t.Errorf("盘上凭证异常: %+v", c0)
			}
			return
		}
	}
	t.Fatal("盘上缺 managed 渠道")
}

// 验收 ③ / §4.5 规则 2：用户经 POST config 改 managed 渠道 credentials → 拒绝。
func TestSaveConfigRejectsManagedCredentialChange(t *testing.T) {
	e := newAcctTestEnv(t)
	store, _ := account.Load(filepath.Join(e.dir, "accounts.json"))
	store.SetLogf(func(string, ...any) {})
	t.Cleanup(store.Close)
	if _, err := store.Add(account.PlatformLoomy,
		json.RawMessage(`{"session":"sess-0123456789abcdef"}`), "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := acctchan.Sync(store, e.loadDefs, e.saveChannels, func(string, ...any) {}); err != nil {
		t.Fatal(err)
	}
	// 用户改 secret。
	_, err := e.save([]byte(`{"ext_channels":[
	  {"name":"loomy","base_url":"https://loomyad.xunfei.cn/api/v1","adapter":"loomy",
	   "managed":true,"origin":"account-center",
	   "credentials":[{"label":"acc-x","secret":"hijacked"}]}
	]}`))
	if err == nil {
		t.Fatal("改 managed 渠道 credentials 应被拒绝")
	}
	if !strings.Contains(err.Error(), "账号中心托管") {
		t.Errorf("报错应指名托管: %v", err)
	}
	// 用户**新增**一条凭证同样拒绝（两处事实来源打架的另一种形态）。
	_, err = e.save([]byte(`{"ext_channels":[
	  {"name":"loomy","base_url":"https://loomyad.xunfei.cn/api/v1","adapter":"loomy",
	   "managed":true,
	   "credentials":[{"label":"acc-y","secret":"new-one"}]}
	]}`))
	if err == nil {
		t.Fatal("给 managed 渠道新增凭证也应被拒绝")
	}
	// 盘上凭证未被破坏（拒绝 = 不落盘）。
	defs := e.loadDefs()
	for _, def := range defs {
		if def.Name == account.PlatformLoomy && (len(def.Credentials) != 1 ||
			def.Credentials[0].Secret != "sess-0123456789abcdef") {
			t.Errorf("被拒的保存不应落盘: %+v", def.Credentials)
		}
	}
}

// §4.5 规则 1：managed 渠道的渠道级字段（base_url/models/note）允许修改。
func TestSaveConfigAllowsManagedChannelFields(t *testing.T) {
	e := newAcctTestEnv(t)
	store, _ := account.Load(filepath.Join(e.dir, "accounts.json"))
	store.SetLogf(func(string, ...any) {})
	t.Cleanup(store.Close)
	a, err := store.Add(account.PlatformLoomy,
		json.RawMessage(`{"session":"sess-0123456789abcdef"}`), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := acctchan.Sync(store, e.loadDefs, e.saveChannels, func(string, ...any) {}); err != nil {
		t.Fatal(err)
	}
	body := `{"ext_channels":[
	  {"name":"loomy","base_url":"https://my-own-proxy.example/api/v1","adapter":"loomy",
	   "managed":true,"origin":"account-center","models":["glm-x"],"note":"我改的",
	   "credentials":[{"label":"` + a.ID + `","secret":"sess-0123456789abcdef"}]}
	]}`
	_, err = e.save([]byte(body))
	if err != nil {
		t.Fatalf("渠道级字段修改应放行: %v", err)
	}
	// 热重载后引擎里的 base_url 已更新。
	for _, st := range e.extMgr.Status() {
		if st.Name == account.PlatformLoomy && st.BaseURL != "https://my-own-proxy.example/api/v1" {
			t.Errorf("base_url 未生效: %q", st.BaseURL)
		}
	}
}

// §4.5 规则 4：managed 渠道 disabled:true 允许（停用整个平台渠道）。
func TestSaveConfigAllowsManagedDisable(t *testing.T) {
	e := newAcctTestEnv(t)
	store, _ := account.Load(filepath.Join(e.dir, "accounts.json"))
	store.SetLogf(func(string, ...any) {})
	t.Cleanup(store.Close)
	a, err := store.Add(account.PlatformLoomy,
		json.RawMessage(`{"session":"sess-0123456789abcdef"}`), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := acctchan.Sync(store, e.loadDefs, e.saveChannels, func(string, ...any) {}); err != nil {
		t.Fatal(err)
	}
	body := `{"ext_channels":[
	  {"name":"loomy","base_url":"https://loomyad.xunfei.cn/api/v1","adapter":"loomy",
	   "managed":true,"disabled":true,
	   "credentials":[{"label":"` + a.ID + `","secret":"sess-0123456789abcdef"}]}
	]}`
	if _, err := e.save([]byte(body)); err != nil {
		t.Fatalf("停用 managed 渠道应放行: %v", err)
	}
}

// 验收 ④ / §4.5 规则 3：删 managed 渠道允许；Reconcile（再跑一次 Sync）重建。
func TestReconcileRebuildsDeletedManagedChannel(t *testing.T) {
	e := newAcctTestEnv(t)
	store, err := account.Load(filepath.Join(e.dir, "accounts.json"))
	if err != nil {
		t.Fatal(err)
	}
	store.SetLogf(func(string, ...any) {})
	t.Cleanup(store.Close)
	a, err := store.Add(account.PlatformLoomy,
		json.RawMessage(`{"session":"sess-0123456789abcdef"}`), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := acctchan.Sync(store, e.loadDefs, e.saveChannels, func(string, ...any) {}); err != nil {
		t.Fatal(err)
	}
	// 用户删掉渠道（保存允许——尊重用户）。
	if _, err := e.save([]byte(`{"ext_channels":[]}`)); err != nil {
		t.Fatal(err)
	}
	if got := len(e.loadDefs()); got != 0 {
		t.Fatalf("删除后渠道数 = %d", got)
	}
	// 下次账号操作 / 启动：Reconcile 以 accounts.json 为准重建。
	if _, err := acctchan.Sync(store, e.loadDefs, e.saveChannels, func(string, ...any) {}); err != nil {
		t.Fatal(err)
	}
	defs := e.loadDefs()
	if len(defs) != 1 || defs[0].Name != account.PlatformLoomy || !defs[0].Managed {
		t.Fatalf("Reconcile 未重建: %+v", defs)
	}
	if len(defs[0].Credentials) != 1 || defs[0].Credentials[0].Label != a.ID {
		t.Errorf("重建的 credentials 异常: %+v", defs[0].Credentials)
	}
}

// 验收 ⑤ / T2 单一写路径：SaveChannels 与 POST config 对同一份内容落盘，
// 产生的凭证数据一致（字节级同构——两者都过 applyConfig 的同一序列化）。
func TestSaveChannelsAndSaveConfigSameBytes(t *testing.T) {
	e := newAcctTestEnv(t)
	projected := []extchan.ChannelDef{{
		Name: "loomy", BaseURL: "https://loomyad.xunfei.cn/api/v1", Protocol: "openai",
		Adapter: "loomy", Managed: true, Origin: "account-center",
		Accounts:    map[string]string{"acc-1": "acc-1"},
		Credentials: []extchan.CredentialDef{{Label: "acc-1", Secret: "sess-0123456789abcdef"}},
	}}
	if err := e.saveChannels(projected); err != nil {
		t.Fatalf("saveChannels: %v", err)
	}
	viaSaveChannels, err := os.ReadFile(e.path)
	if err != nil {
		t.Fatal(err)
	}
	// 同内容走 POST config 入口（手写用户形态，凭证为真实值——没有哨兵可回填）。
	if err := os.WriteFile(e.path, []byte(`{"listen":"127.0.0.1:0","ext_channels":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = e.save([]byte(`{"ext_channels":[
	  {"name":"loomy","base_url":"https://loomyad.xunfei.cn/api/v1","protocol":"openai","adapter":"loomy",
	   "managed":true,"origin":"account-center","accounts":{"acc-1":"acc-1"},
	   "credentials":[{"label":"acc-1","secret":"sess-0123456789abcdef"}]}
	]}`))
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	viaSaveConfig, err := os.ReadFile(e.path)
	if err != nil {
		t.Fatal(err)
	}
	// 两份内容的 ext_channels 语义一致（JSON 层比较，键序无关）。
	var a, b struct {
		ExtChannels []map[string]any `json:"ext_channels"`
	}
	if err := json.Unmarshal(viaSaveChannels, &a); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(viaSaveConfig, &b); err != nil {
		t.Fatal(err)
	}
	aj, _ := json.Marshal(a.ExtChannels)
	bj, _ := json.Marshal(b.ExtChannels)
	if string(aj) != string(bj) {
		t.Errorf("两条写路径的渠道内容不一致:\n saveChannels: %s\n saveConfig:   %s", aj, bj)
	}
}

// usage 记录器（编译期占位：applyConfig 热应用不触碰它，导入仅为对齐 main 的
// 依赖形态——防未来有人把 usage 热应用塞进 applyConfig 时无测试覆盖）。
var _ = usage.Recorder{}
