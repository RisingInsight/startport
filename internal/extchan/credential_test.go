package extchan

import (
	"testing"
	"time"
)

// 选证顺序与配置优先级：按数组顺序取第一条 usable；disabled 跳过。
func TestCredentialPickOrder(t *testing.T) {
	m := newTestManager()
	m.ReplaceAll([]ChannelDef{{
		Name: "a", BaseURL: "https://a",
		Credentials: []CredentialDef{
			{Label: "primary", Secret: "s1"},
			{Label: "backup", Secret: "s2"},
		},
	}})
	ch := m.Get("a")
	if got := ch.pickCredential(nil); got == nil || got.Label != "primary" {
		t.Fatalf("应选 primary, got %+v", got)
	}
	if !ch.Creds[0].Disabled {
		ch.Creds[0].mu.Lock()
		ch.Creds[0].Disabled = true
		ch.Creds[0].mu.Unlock()
	}
	if got := ch.pickCredential(nil); got == nil || got.Label != "backup" {
		t.Fatalf("primary 停用后应选 backup, got %+v", got)
	}
	if !ch.hasUsableCredential() {
		t.Error("backup 仍可用")
	}
}

// 429：只冷却该凭证（指数退避封顶）；成功清零；到期恢复可用。
func TestCredentialRateLimitedCooling(t *testing.T) {
	c, err := newCredential(CredentialDef{Label: "primary", Secret: "s"})
	if err != nil {
		t.Fatal(err)
	}
	c.NoteRateLimited(10*time.Millisecond, 50*time.Millisecond, "429")
	if c.State() != CredCooling {
		t.Fatalf("429 后状态应 cooling, got %s", c.State())
	}
	if c.usableAt(time.Now()) {
		t.Error("冷却中不应可用")
	}
	snap := c.Snapshot(time.Now())
	if !snap.Cooling {
		t.Errorf("快照缺冷却信息: %+v", snap)
	}
	// 连续 429 快照冷却时长不应超过封顶。
	for i := 0; i < 10; i++ {
		c.NoteRateLimited(10*time.Millisecond, 50*time.Millisecond, "429")
	}
	if d := time.Until(c.coolUntil); d > 60*time.Millisecond {
		t.Errorf("冷却未封顶: %v", d)
	}
	// 冷却到期自动恢复。
	time.Sleep(60 * time.Millisecond)
	if !c.usableAt(time.Now()) {
		t.Error("冷却到期应恢复可用")
	}
	c.NoteRateLimited(time.Second, time.Second, "429")
	c.NoteSuccess()
	if c.State() != CredHealthy || c.Snapshot(time.Now()).Cooling {
		t.Error("成功后应清零冷却")
	}
}

// 401/403：invalid 退出轮换，且不因时间自动恢复。
func TestCredentialInvalid(t *testing.T) {
	c, _ := newCredential(CredentialDef{Label: "p", Secret: "s"})
	c.NoteInvalid("401 authentication failed")
	if c.State() != CredInvalid {
		t.Fatalf("状态应 invalid, got %s", c.State())
	}
	if c.usableAt(time.Now()) {
		t.Error("invalid 不应可用")
	}
	snap := c.Snapshot(time.Now())
	if snap.State != CredInvalid || snap.LastError == "" {
		t.Errorf("快照缺失效信息: %+v", snap)
	}
	// 改 secret（ReplaceAll）才恢复：状态键变化 → 新凭证 healthy。
	m := newTestManager()
	m.ReplaceAll([]ChannelDef{{Name: "a", BaseURL: "https://a",
		Credentials: []CredentialDef{{Label: "p", Secret: "old"}}}})
	ch := m.Get("a")
	ch.Creds[0].NoteInvalid("401")
	m.ReplaceAll([]ChannelDef{{Name: "a", BaseURL: "https://a",
		Credentials: []CredentialDef{{Label: "p", Secret: "new"}}}})
	if got := m.Get("a").Creds[0].State(); got != CredHealthy {
		t.Errorf("改 secret 应重置为 healthy, got %s", got)
	}
}

// 过期凭证不参与轮换，但面板仍展示（has_expiry / expiring_soon）。
func TestCredentialExpiry(t *testing.T) {
	soon := time.Now().Add(24 * time.Hour).Format("2006-01-02")
	c, err := newCredential(CredentialDef{Label: "p", Secret: "s", ExpiresAt: soon})
	if err != nil {
		t.Fatal(err)
	}
	if !c.usableAt(time.Now()) {
		t.Error("未过期应可用")
	}
	snap := c.Snapshot(time.Now())
	if !snap.HasExpiry || !snap.ExpiringSoon || snap.ExpiresDays > 3 {
		t.Errorf("即将过期标注异常: %+v", snap)
	}
	past, err := newCredential(CredentialDef{Label: "old", Secret: "s", ExpiresAt: "2020-01-01"})
	if err != nil {
		t.Fatal(err)
	}
	if past.usableAt(time.Now()) {
		t.Error("已过期不应可用")
	}
	if _, err := newCredential(CredentialDef{Label: "bad", Secret: "s", ExpiresAt: "不是日期"}); err == nil {
		t.Error("非法 expires_at 应报错")
	}
}

// 进程内用量计数（P0-9 降级方案 B）。
func TestCredentialUsageCounters(t *testing.T) {
	c, _ := newCredential(CredentialDef{Label: "p", Secret: "s"})
	c.NoteUsage(10, 3)
	c.NoteUsage(5, 2)
	snap := c.Snapshot(time.Now())
	if snap.Requests != 2 || snap.PromptTokens != 15 || snap.CompletionTokens != 5 || snap.TotalTokens != 20 {
		t.Errorf("用量计数异常: %+v", snap)
	}
	if !snap.Masked {
		t.Error("凭证快照必须标记 Masked（无秘密值）")
	}
}

// 凭证级运行态复用：同 label+secret 保留冷却；只改 secret → 重置 healthy。
func TestReplaceAllCredentialStateReuse(t *testing.T) {
	defs := []ChannelDef{{Name: "a", BaseURL: "https://a", Credentials: []CredentialDef{
		{Label: "primary", Secret: "s1"},
		{Label: "backup", Secret: "s2"},
	}}}
	m := newTestManager()
	m.ReplaceAll(defs)
	ch := m.Get("a")
	ch.Creds[0].NoteRateLimited(time.Minute, time.Minute, "429")

	m.ReplaceAll(defs) // 完全相同的定义
	ch2 := m.Get("a")
	if ch2.Creds[0].State() != CredCooling {
		t.Error("同 label+secret 热重载应保留冷却状态")
	}
	// 只改 base_url：凭证状态保留（凭证与 base 无关）。
	m.ReplaceAll([]ChannelDef{{Name: "a", BaseURL: "https://b", Credentials: defs[0].Credentials}})
	if m.Get("a").Creds[0].State() != CredCooling {
		t.Error("改 base_url 不应重置凭证状态")
	}
	// 改 primary 的 secret：该凭证重置，backup 状态仍保留。
	defs2 := []ChannelDef{{Name: "a", BaseURL: "https://b", Credentials: []CredentialDef{
		{Label: "primary", Secret: "s1-new"},
		{Label: "backup", Secret: "s2"},
	}}}
	m.Get("a").Creds[1].NoteInvalid("401")
	m.ReplaceAll(defs2)
	ch3 := m.Get("a")
	if ch3.Creds[0].State() != CredHealthy {
		t.Error("改 secret 应重置该凭证状态")
	}
	if ch3.Creds[1].State() != CredInvalid {
		t.Error("未改动的 backup 应保留 invalid 状态")
	}
	// 删除凭证：状态丢弃。
	m.ReplaceAll([]ChannelDef{{Name: "a", BaseURL: "https://b", Credentials: []CredentialDef{
		{Label: "primary", Secret: "s1-new"},
	}}})
	if len(m.Get("a").Creds) != 1 {
		t.Error("删除的凭证应被丢弃")
	}
}

// api_key 兼容形态：合成一条 label=default 凭证；Status 计数正确。
func TestAPIKeySynthesizedCredential(t *testing.T) {
	m := newTestManager()
	m.ReplaceAll([]ChannelDef{{Name: "a", BaseURL: "https://a", APIKey: "sk-legacy"}})
	ch := m.Get("a")
	if len(ch.Creds) != 1 || ch.Creds[0].Label != "default" || ch.Creds[0].Secret != "sk-legacy" {
		t.Fatalf("api_key 应合成 default 凭证: %+v", ch.Creds)
	}
	st := m.Status()
	if len(st) != 1 || st[0].CredentialsTotal != 1 || st[0].CredentialsUsable != 1 ||
		st[0].State != StateAvailable {
		t.Errorf("Status 凭证维度异常: %+v", st)
	}
	// 无凭证（自建内网场景）：状态仍 available（不注入鉴权）。
	m.ReplaceAll([]ChannelDef{{Name: "b", BaseURL: "https://b"}})
	if st := m.Status(); st[0].State != StateAvailable || st[0].CredentialsTotal != 0 {
		t.Errorf("无凭证渠道状态异常: %+v", st[0])
	}
	// 渠道停用 → disabled。
	m.ReplaceAll([]ChannelDef{{Name: "b", BaseURL: "https://b", Disabled: true}})
	if st := m.Status(); st[0].State != StateDisabled {
		t.Errorf("停用渠道状态应为 disabled: %+v", st[0])
	}
}

// 渠道派生状态：全部失效 / 全部冷却。
func TestChannelDerivedStates(t *testing.T) {
	m := newTestManager()
	m.ReplaceAll([]ChannelDef{{Name: "a", BaseURL: "https://a", Credentials: []CredentialDef{
		{Label: "p", Secret: "1"}, {Label: "b", Secret: "2"},
	}}})
	ch := m.Get("a")
	ch.Creds[0].NoteInvalid("401")
	ch.Creds[1].NoteInvalid("403")
	if st := ch.Snapshot(); st.State != StateAllInvalid || st.CredentialsInvalid != 2 {
		t.Errorf("全失效状态异常: %+v", st)
	}
	m.ReplaceAll([]ChannelDef{{Name: "c", BaseURL: "https://c", Credentials: []CredentialDef{
		{Label: "p", Secret: "1"},
	}}})
	c := m.Get("c")
	c.Creds[0].NoteRateLimited(time.Minute, time.Minute, "429")
	if st := c.Snapshot(); st.State != StateAllCooling {
		t.Errorf("全冷却状态异常: %+v", st)
	}
}
