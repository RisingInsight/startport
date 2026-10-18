// migrate_fix_test.go 发布阻塞缺陷修复回归（v1.6.0 发布前；.smoke5 场景）：
// D1 同平台第二渠道迁入拒绝、D2 回退兜底清理兄弟渠道死引用。
package panel

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/RisingInsight/startport/internal/account"
	"github.com/RisingInsight/startport/internal/extchan"
)

// D1：账号中心已有 loomy 账号 main（托管渠道 loomy）→ 迁入同平台第二条渠道
// loomy2 → 400 指路，两边数据零变化。
func TestMigrateRejectsSecondSamePlatformChannel(t *testing.T) {
	e := newMigrateEnv(t, extchan.ChannelDef{
		Name: "loomy", BaseURL: "https://loomyad.xunfei.cn/api/v1", Protocol: "openai",
		Adapter: "loomy", Managed: true, Origin: "account-center",
		Accounts:    map[string]string{"acc-main01": "acc-main01"},
		Credentials: []extchan.CredentialDef{{Label: "acc-main01", Secret: "sess-MAIN-12345678"}},
	})
	main, _ := e.store.Add(account.PlatformLoomy,
		json.RawMessage(`{"session":"sess-MAIN-12345678"}`), "main", "")
	// 让投影引用与账号对齐（main 的 ID 是随机生成的）。
	e.defs[0].Accounts = map[string]string{main.ID: main.ID}
	e.defs[0].Credentials = []extchan.CredentialDef{{Label: main.ID, Secret: "sess-MAIN-12345678"}}

	// 同平台第二条渠道（非托管）。
	loomy2 := extchan.ChannelDef{
		Name: "loomy2", BaseURL: "https://loomyad.xunfei.cn/api/v1", Protocol: "openai",
		Adapter:     "loomy",
		Credentials: []extchan.CredentialDef{{Label: "second", Secret: "sess-SECOND-987654"}},
	}
	e.defs = append(e.defs, loomy2)

	rec := e.doChannel("POST", "/startport/api/channels/loomy2/migrate")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"loomy", "只支持一条托管渠道", "账号中心"} {
		if !strings.Contains(body, want) {
			t.Errorf("报错缺指路信息 %q: %s", want, body)
		}
	}
	// 两边数据零变化：账号仍在、两条渠道形态不变。
	if len(e.store.List(account.PlatformLoomy)) != 1 {
		t.Error("已有账号被改动")
	}
	if !e.defs[0].Managed || e.defs[1].Managed {
		t.Errorf("渠道形态被改动: %+v", e.defs)
	}
	if len(e.defs[1].Credentials) != 1 || e.defs[1].Credentials[0].Secret != "sess-SECOND-987654" {
		t.Errorf("第二条渠道凭证被改动: %+v", e.defs[1].Credentials)
	}
	if strings.Contains(body, "sess-SECOND-987654") {
		t.Error("400 响应泄露 secret")
	}
}

// D1 变体：账号中心无账号，但同平台已存在 managed 渠道（历史脏数据）→ 同样 400。
func TestMigrateRejectsWhenManagedChannelExistsWithoutAccounts(t *testing.T) {
	e := newMigrateEnv(t, extchan.ChannelDef{
		Name: "loomy", BaseURL: "https://loomyad.xunfei.cn/api/v1", Protocol: "openai",
		Adapter: "loomy", Managed: true, Origin: "account-center",
		Accounts:    map[string]string{"acc-dead01": "acc-dead01"},
		Credentials: []extchan.CredentialDef{{Label: "acc-dead01", Secret: "sess-OLD-123456789"}},
	})
	e.defs = append(e.defs, extchan.ChannelDef{
		Name: "loomy2", BaseURL: "https://loomyad.xunfei.cn/api/v1", Protocol: "openai",
		Adapter:     "loomy",
		Credentials: []extchan.CredentialDef{{Label: "second", Secret: "sess-SECOND-987654"}},
	})
	rec := e.doChannel("POST", "/startport/api/channels/loomy2/migrate")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "托管渠道") {
		t.Errorf("报错应指明已存在托管渠道: %s", rec.Body.String())
	}
}

// D2：历史脏数据（同平台两条 managed、accounts.json 只有 main）→ 回退 loomy2
// → loomy2 解除托管；兄弟渠道 loomy 的 managed 标记随账号清空被兜底清理；
// accounts.json 与 config 无死引用。
func TestRollbackCleansSiblingDeadRefs(t *testing.T) {
	e := newMigrateEnv(t, extchan.ChannelDef{
		Name: "loomy", BaseURL: "https://loomyad.xunfei.cn/api/v1", Protocol: "openai",
		Adapter: "loomy", Managed: true, Origin: "account-center",
		Accounts:    map[string]string{"acc-main01": "acc-main01"},
		Credentials: []extchan.CredentialDef{{Label: "acc-main01", Secret: "sess-MAIN-12345678"}},
	})
	e.defs = append(e.defs, extchan.ChannelDef{
		Name: "loomy2", BaseURL: "https://loomyad.xunfei.cn/api/v1", Protocol: "openai",
		Adapter: "loomy", Managed: true, Origin: "account-center",
		Accounts: map[string]string{
			"acc-main01": "acc-main01",
			"acc-dead02": "acc-dead02", // 死引用（历史遗留）
		},
		Credentials: []extchan.CredentialDef{
			{Label: "acc-main01", Secret: "sess-MAIN-12345678"},
			{Label: "acc-dead02", Secret: "sess-DEAD-87654321"},
		},
	})
	// 账号事实：只有 main 一个（dead02 已不存在）。
	main, _ := e.store.Add(account.PlatformLoomy,
		json.RawMessage(`{"session":"sess-MAIN-12345678"}`), "main", "")
	e.defs[0].Accounts = map[string]string{main.ID: main.ID}
	e.defs[0].Credentials = []extchan.CredentialDef{{Label: main.ID, Secret: "sess-MAIN-12345678"}}
	e.defs[1].Accounts = map[string]string{main.ID: main.ID, "acc-dead02": "acc-dead02"}
	e.defs[1].Credentials = []extchan.CredentialDef{
		{Label: main.ID, Secret: "sess-MAIN-12345678"},
		{Label: "acc-dead02", Secret: "sess-DEAD-87654321"},
	}

	rec := e.doChannel("POST", "/startport/api/channels/loomy2/rollback")
	if rec.Code != http.StatusOK {
		t.Fatalf("回退 status = %d: %s", rec.Code, rec.Body.String())
	}
	// 账号事实：main 已随回退删除（回退 = 账号移除）。
	if len(e.store.List(account.PlatformLoomy)) != 0 {
		t.Error("回退后账号应清空")
	}
	// 兜底清理：两条渠道都不再是 managed（该平台已无账号），凭证原样保留。
	var loomy, loomy2 *extchan.ChannelDef
	for i := range e.defs {
		switch e.defs[i].Name {
		case "loomy":
			loomy = &e.defs[i]
		case "loomy2":
			loomy2 = &e.defs[i]
		}
	}
	if loomy == nil || loomy2 == nil {
		t.Fatal("渠道不应被删除（只清标记）")
	}
	if loomy.Managed || loomy.Accounts != nil {
		t.Errorf("兄弟渠道 loomy 的死引用未清理: managed=%v accounts=%v", loomy.Managed, loomy.Accounts)
	}
	if loomy2.Managed || loomy2.Accounts != nil {
		t.Errorf("回退渠道 loomy2 的死引用未清理: managed=%v accounts=%v", loomy2.Managed, loomy2.Accounts)
	}
	// 无死引用：任何渠道的 accounts 映射都不再指向不存在的账号（标记已清 → 恒真，
	// 再验凭证数据未丢）。
	if len(loomy.Credentials) != 1 || loomy.Credentials[0].Secret != "sess-MAIN-12345678" {
		t.Errorf("兄弟渠道凭证被改动: %+v", loomy.Credentials)
	}
	if len(loomy2.Credentials) != 2 {
		t.Errorf("回退渠道凭证应原样保留: %+v", loomy2.Credentials)
	}
	// 响应零 secret。
	if strings.Contains(rec.Body.String(), "sess-MAIN-12345678") {
		t.Error("回退响应泄露 secret")
	}
}

// D2 对照：兄弟渠道的平台**仍有**账号时不清标记——由 Sync 以账号为准重建。
func TestRollbackKeepsSiblingWhenPlatformStillHasAccounts(t *testing.T) {
	e := newMigrateEnv(t, extchan.ChannelDef{
		Name: "loomy", BaseURL: "https://loomyad.xunfei.cn/api/v1", Protocol: "openai",
		Adapter: "loomy", Managed: true, Origin: "account-center",
		Accounts:    map[string]string{"acc-keep01": "acc-keep01"},
		Credentials: []extchan.CredentialDef{{Label: "acc-keep01", Secret: "sess-KEEP-12345678"}},
	})
	// 平台仍有账号（keep）。
	keep, _ := e.store.Add(account.PlatformLoomy,
		json.RawMessage(`{"session":"sess-KEEP-12345678"}`), "keep", "")
	e.defs[0].Accounts = map[string]string{keep.ID: keep.ID}
	e.defs[0].Credentials = []extchan.CredentialDef{{Label: keep.ID, Secret: "sess-KEEP-12345678"}}
	// 脏数据：另一条 managed 的 custom 渠道（c-<accID>）账号已删——不属于原生
	// 平台兜底范围，仅确认不被误清。
	e.defs = append(e.defs, extchan.ChannelDef{
		Name: "c-acc-custom", BaseURL: "https://relay.example.com", Protocol: "openai",
		Managed: true, Origin: "account-center",
		Accounts:    map[string]string{"acc-custom": "acc-custom"},
		Credentials: []extchan.CredentialDef{{Label: "acc-custom", Secret: "sk-x"}},
	})

	// 对 loomy 渠道做一次"无实际账号删除"的回退前清理路径——直接调用兜底函数验证语义。
	if cleaned := cleanupStaleManagedChannels(e.defs, e.store); len(cleaned) != 0 {
		t.Errorf("平台仍有账号的兄弟渠道不应被清理: %v", cleaned)
	}
	if !e.defs[0].Managed {
		t.Error("有账号平台的 managed 渠道被误清")
	}
}
