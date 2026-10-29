// sync_test.go 账号 → managed 渠道单向投影的投影规则与冲突边界（架构 §4.1/§4.4）。
package acctchan

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/RisingInsight/startport/internal/account"
	"github.com/RisingInsight/startport/internal/extchan"
)

func loomyCred() json.RawMessage {
	return json.RawMessage(`{"session":"sess-0123456789abcdef"}`)
}

func qoderCred() json.RawMessage {
	return json.RawMessage(`{"access_token":"jt-1","user_id":"u-1","machine_id":"m-1"}`)
}

func customCred() json.RawMessage {
	return json.RawMessage(`{"base_url":"https://relay.example.com","secret":"sk-custom","protocol":"openai"}`)
}

// testSync 组装 store + 可变 loader/saver 的同步环境。
type testSync struct {
	store *account.Store
	defs  []extchan.ChannelDef
	saved int
	logs  []string
}

func newTestSync(t *testing.T) *testSync {
	t.Helper()
	store, err := account.Load(t.TempDir() + "/accounts.json")
	if err != nil {
		t.Fatal(err)
	}
	store.SetLogf(func(string, ...any) {})
	t.Cleanup(store.Close)
	return &testSync{store: store}
}

func (ts *testSync) loader() []extchan.ChannelDef { return ts.defs }

func (ts *testSync) saver(defs []extchan.ChannelDef) error {
	ts.defs = defs
	ts.saved++
	return nil
}

func (ts *testSync) run(t *testing.T) string {
	t.Helper()
	ts.logs = nil
	summary, err := Sync(ts.store, ts.loader, ts.saver, func(f string, a ...any) {
		ts.logs = append(ts.logs, f)
	})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	return summary
}

func (ts *testSync) byName(name string) (extchan.ChannelDef, bool) {
	for _, def := range ts.defs {
		if def.Name == name {
			return def, true
		}
	}
	return extchan.ChannelDef{}, false
}

// 首次投影：原生平台渠道被创建（managed + 官方默认基址 + credentials=label=ID）。
func TestSyncCreatesManagedChannel(t *testing.T) {
	ts := newTestSync(t)
	a, err := ts.store.Add(account.PlatformLoomy, loomyCred(), "", "")
	if err != nil {
		t.Fatal(err)
	}
	ts.run(t)
	def, ok := ts.byName(account.PlatformLoomy)
	if !ok {
		t.Fatal("loomy 渠道未创建")
	}
	if !def.Managed || def.Origin != OriginAccountCenter {
		t.Errorf("托管标记缺失: managed=%v origin=%q", def.Managed, def.Origin)
	}
	if def.BaseURL != "https://loomyad.xunfei.cn/api/v1" {
		t.Errorf("官方默认基址未生效: %q", def.BaseURL)
	}
	if len(def.Credentials) != 1 || def.Credentials[0].Label != a.ID {
		t.Errorf("credentials 投影异常: %+v", def.Credentials)
	}
	if def.Credentials[0].Secret != "sess-0123456789abcdef" {
		t.Errorf("主凭证提取异常: %q", def.Credentials[0].Secret)
	}
	if def.Adapter != "loomy" || def.Protocol != "openai" {
		t.Errorf("adapter/protocol 异常: %+v", def)
	}
	if !strings.Contains(def.Note, "账号中心托管") {
		t.Errorf("托管渠道应有说明性 note: %q", def.Note)
	}
}

// 追加账号 → credentials 重建（渠道级字段不动：用户改过的 base_url 保留）。
func TestSyncRebuildKeepsChannelFields(t *testing.T) {
	ts := newTestSync(t)
	ts.store.Add(account.PlatformLoomy, loomyCred(), "a", "")
	ts.run(t)
	// 用户手改渠道级字段（config 为准）。
	for i := range ts.defs {
		if ts.defs[i].Name == account.PlatformLoomy {
			ts.defs[i].BaseURL = "https://user-chosen.example/api"
			ts.defs[i].Models = []string{"m-1"}
			ts.defs[i].Aliases = map[string]string{"fast": "m-1"}
		}
	}
	a2, _ := ts.store.Add(account.PlatformLoomy, loomyCred(), "b", "")
	ts.run(t)
	def, _ := ts.byName(account.PlatformLoomy)
	if def.BaseURL != "https://user-chosen.example/api" || len(def.Models) != 1 || len(def.Aliases) != 1 {
		t.Errorf("渠道级字段被投影改写: %+v", def)
	}
	if len(def.Credentials) != 2 {
		t.Fatalf("credentials = %d, want 2", len(def.Credentials))
	}
	for _, c := range def.Credentials {
		if c.Label != "a" && c.Label != "b" && !strings.HasPrefix(c.Label, "acc-") {
			t.Errorf("credentials label 应为 account.ID: %+v", def.Credentials)
		}
	}
	_ = a2
}

// 用户手建的同名非托管渠道：WARN 不接管（绝不静默改写）。
func TestSyncSkipsUnmanagedSameName(t *testing.T) {
	ts := newTestSync(t)
	ts.defs = []extchan.ChannelDef{{
		Name: account.PlatformLoomy, BaseURL: "https://manual.example", Protocol: "openai",
		Credentials: []extchan.CredentialDef{{Label: "default", Secret: "sk-manual"}},
	}}
	ts.store.Add(account.PlatformLoomy, loomyCred(), "", "")
	ts.run(t)
	def, _ := ts.byName(account.PlatformLoomy)
	if def.Managed {
		t.Error("非托管渠道不应被接管")
	}
	if len(def.Credentials) != 1 || def.Credentials[0].Label != "default" {
		t.Errorf("非托管渠道的凭证被改写: %+v", def.Credentials)
	}
	if len(ts.logs) == 0 || !strings.Contains(strings.Join(ts.logs, "\n"), "非托管") {
		t.Errorf("应有 WARN 日志: %v", ts.logs)
	}
}

// 删除全部账号 → 该 managed 渠道成为**孤儿**：按架构 §4.4-2 保留不动
// （面板提示"已无账号，可删除或回退"），不自动删除——自动删除会连带清掉
// rollback 所需的凭证数据，与"回退零数据损失"冲突。
func TestSyncOrphanAfterDeleteKept(t *testing.T) {
	ts := newTestSync(t)
	a, _ := ts.store.Add(account.PlatformLoomy, loomyCred(), "", "")
	ts.run(t)
	ts.store.Delete(a.ID)
	summary := ts.run(t)
	def, ok := ts.byName(account.PlatformLoomy)
	if !ok {
		t.Fatal("孤儿渠道被删除（架构 §4.4-2 要求保留）")
	}
	if !def.Managed || len(def.Credentials) != 1 {
		t.Errorf("孤儿渠道内容不应被改写: managed=%v creds=%d", def.Managed, len(def.Credentials))
	}
	if strings.Contains(summary, account.PlatformLoomy) {
		t.Errorf("孤儿渠道不应出现在本次动作里: %q", summary)
	}
}

// custom 平台：每账号一条渠道（name=c-<accID>），blob.base_url/protocol 生效。
func TestSyncCustomPlatformPerAccountChannels(t *testing.T) {
	ts := newTestSync(t)
	a1, _ := ts.store.Add(account.PlatformCustom, customCred(), "r1", "")
	a2, _ := ts.store.Add(account.PlatformCustom,
		json.RawMessage(`{"base_url":"https://r2.example.com","secret":"sk2","protocol":"anthropic"}`), "r2", "")
	ts.run(t)
	d1, ok1 := ts.byName(CustomChannelName(a1))
	d2, ok2 := ts.byName(CustomChannelName(a2))
	if !ok1 || !ok2 {
		t.Fatalf("custom 渠道未按账号创建: %v %v", ok1, ok2)
	}
	if d1.BaseURL != "https://relay.example.com" || d1.Protocol != "openai" {
		t.Errorf("custom#1 异常: %+v", d1)
	}
	if d2.Protocol != "anthropic" {
		t.Errorf("custom#2 protocol = %q", d2.Protocol)
	}
	if d1.Credentials[0].Secret != "sk-custom" {
		t.Errorf("custom secret = %q", d1.Credentials[0].Secret)
	}
}

// qoder：主凭证是适配器形态的账号 JSON（PrimarySecret 的产出原样进 secret）。
func TestSyncQoderPrimarySecretIsAdapterJSON(t *testing.T) {
	ts := newTestSync(t)
	ts.store.Add(account.PlatformQoder, qoderCred(), "", "")
	ts.run(t)
	def, _ := ts.byName(account.PlatformQoder)
	if def.BaseURL != "https://gateway.qoder.com.cn" {
		t.Errorf("qoder 缺省基址 = %q", def.BaseURL)
	}
	secret := def.Credentials[0].Secret
	if !strings.Contains(secret, `"accessToken":"jt-1"`) || !strings.Contains(secret, `"mode":"cn"`) {
		t.Errorf("qoder 主凭证不是适配器 JSON: %s", secret)
	}
}

// 无账号：无变更、不落盘（老配置零变化——v1.5.1 行为逐字节保留）。
func TestSyncNoAccountsNoChanges(t *testing.T) {
	ts := newTestSync(t)
	ts.defs = []extchan.ChannelDef{{Name: "manual", BaseURL: "https://x", Protocol: "openai"}}
	summary := ts.run(t)
	if summary != "no changes" || ts.saved != 0 {
		t.Errorf("无账号应 no changes 且不落盘: summary=%q saved=%d", summary, ts.saved)
	}
	if len(ts.defs) != 1 {
		t.Error("无账号时不应创建渠道")
	}
}

// managed 渠道凭证与账号一致（无变更）时不重复落盘。
func TestSyncIdempotentWhenInSync(t *testing.T) {
	ts := newTestSync(t)
	ts.store.Add(account.PlatformLoomy, loomyCred(), "", "")
	ts.run(t)
	saved := ts.saved
	ts.run(t)
	if ts.saved != saved {
		t.Errorf("已同步状态下重复落盘: %d → %d", saved, ts.saved)
	}
}

// 孤儿：managed 渠道保留不动（无账号的平台不再生成，也不删除渠道）。
func TestSyncOrphanChannelKept(t *testing.T) {
	ts := newTestSync(t)
	ts.defs = []extchan.ChannelDef{{
		Name: account.PlatformLoomy, BaseURL: "https://loomyad.xunfei.cn/api/v1",
		Protocol: "openai", Adapter: "loomy",
		Managed: true, Origin: OriginAccountCenter,
		Accounts:    map[string]string{"acc-old": "acc-old"},
		Credentials: []extchan.CredentialDef{{Label: "acc-old", Secret: "old"}},
	}}
	ts.run(t)
	if _, ok := ts.byName(account.PlatformLoomy); !ok {
		t.Fatal("孤儿渠道被删除")
	}
	if ts.saved != 0 {
		t.Errorf("孤儿渠道应保持原样（面板提示删除/回退）: saved=%d", ts.saved)
	}
}

// 停用账号投影为 disabled 凭证（退出轮换但保留在渠道里，面板仍可见）。
func TestSyncDisabledAccountProjectedDisabled(t *testing.T) {
	ts := newTestSync(t)
	a, _ := ts.store.Add(account.PlatformLoomy, loomyCred(), "", "")
	_ = ts.store.SetDisabled(a.ID, true)
	ts.run(t)
	def, _ := ts.byName(account.PlatformLoomy)
	if len(def.Credentials) != 1 || !def.Credentials[0].Disabled {
		t.Errorf("停用账号应投影为 disabled 凭证: %+v", def.Credentials)
	}
}

// 停用与启用的状态迁移即时反映到投影。
func TestSyncReEnableAccount(t *testing.T) {
	ts := newTestSync(t)
	a, _ := ts.store.Add(account.PlatformLoomy, loomyCred(), "", "")
	_ = ts.store.SetDisabled(a.ID, true)
	ts.run(t)
	_ = ts.store.SetDisabled(a.ID, false)
	ts.run(t)
	def, _ := ts.byName(account.PlatformLoomy)
	if def.Credentials[0].Disabled {
		t.Error("启用后凭证仍为 disabled")
	}
}

// ChannelDefFor 单账号探测草稿：与投影同形（managed + 单凭证）。
func TestChannelDefFor(t *testing.T) {
	a := &account.Account{ID: "acc-abc12345", Platform: account.PlatformQoder,
		Cred: json.RawMessage(`{"access_token":"dt-9","user_id":"u-9"}`)}
	def, err := ChannelDefFor(a)
	if err != nil {
		t.Fatalf("ChannelDefFor: %v", err)
	}
	if def.Adapter != "qoder" || def.BaseURL != "https://gateway.qoder.com.cn" {
		t.Errorf("探测草稿异常: %+v", def)
	}
	if len(def.Credentials) != 1 || def.Credentials[0].Label != a.ID {
		t.Errorf("探测草稿凭证异常: %+v", def.Credentials)
	}
	// custom 账号。
	a2 := &account.Account{ID: "acc-def67890", Platform: account.PlatformCustom,
		Cred: json.RawMessage(`{"base_url":"https://c.example","secret":"s"}`)}
	def2, err := ChannelDefFor(a2)
	if err != nil {
		t.Fatalf("ChannelDefFor(custom): %v", err)
	}
	if def2.Name != CustomChannelName(a2) || def2.Adapter != "" {
		t.Errorf("custom 探测草稿异常: %+v", def2)
	}
}

// 账号型平台（库库AI）**不投影**：它没有对话 API，投影只会造出一条永远打不通的
// managed 渠道（污染路由表与模型探测）。账号仍留在 accounts.json，额度照常可查。
func TestSyncSkipsAccountOnlyPlatform(t *testing.T) {
	ts := newTestSync(t)
	if _, err := ts.store.Add(account.PlatformKuku,
		json.RawMessage(`{"cookie":"BDUSS=abcdefghijklmnop; STOKEN=xyz"}`), "", ""); err != nil {
		t.Fatal(err)
	}
	// 同一批里放一个正常平台，确认"跳过"只作用于账号型平台、不影响其它投影。
	if _, err := ts.store.Add(account.PlatformLoomy, loomyCred(), "", ""); err != nil {
		t.Fatal(err)
	}
	ts.run(t)

	if _, ok := ts.byName(account.PlatformKuku); ok {
		t.Fatal("kuku 是账号型平台，不应生成托管渠道")
	}
	if _, ok := ts.byName(account.PlatformLoomy); !ok {
		t.Fatal("loomy 应照常投影（跳过逻辑不能误伤普通平台）")
	}
}
