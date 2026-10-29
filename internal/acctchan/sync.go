// Package acctchan 账号 → config.json managed 渠道的**单向投影**（账号中心
// v1.6.0 · T1；架构 §4）。
//
// ── 投影模型（架构 §4.1，规则逐条）──────────────────────────
//   - 原生适配平台：**每平台一条渠道**（渠道名 = 平台 key），凭证数组即账号
//     数组（label = account.ID）；
//   - custom 平台：**每账号一条渠道**（name = "c-<accountID>"，base_url/
//     protocol 属于账号）；
//   - credentials[].secret = 账号 blob 的主凭证字段（account.PrimarySecret；
//     refresh_token 等生命周期字段不进渠道）；
//   - 投影单向：accounts.json → config.json；用户手改 managed 渠道凭证会被
//     配置层拒绝（cmd/server 的 saveConfig 托管校验）；
//   - 新增 JSON 键全部 omitempty：老配置零修改兼容（架构 §4.1 规则 6）。
//
// ── 写路径（架构 §4.2）──────────────────────────────────────
// 本包不碰盘：Saver 闭包由 main 注入（= saveConfig 同一条链：校验/合并/原子
// 写/热重载各只有一份实现）。
package acctchan

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"

	"github.com/RisingInsight/startport/internal/account"
	"github.com/RisingInsight/startport/internal/extchan"
)

// 托管渠道的标记键（ExtChannel 上的 omitempty 三键；架构 §4.1 规则 6）。
const (
	OriginAccountCenter = "account-center"
	// ManagedChannelPrefix custom 平台渠道名前缀（设计 §4.1：每账号一条渠道；
	// 用 account.ID 而非 label slug——ID 稳定且满足渠道名字符集，label 可能含中文）。
	ManagedChannelPrefix = "c-"
)

// Saver 把投影出的渠道数组落盘并热应用（main 注入 = saveConfig 同一条链）。
type Saver func(defs []extchan.ChannelDef) error

// Loader 读当前盘上的渠道定义（main 注入：读 config → ParseConfig → 映射）。
type Loader func() []extchan.ChannelDef

// Loggerf 日志注入（测试静音；生产 log.Printf）。
type Loggerf func(format string, args ...any)

// syncMu 串行化投影读改写（登录入池 / 签到刷新 / 面板账号操作可能并发触发；
// Sync 是"读 config → 改 ext_channels → 原子写"的复合操作，不加锁会丢更新）。
var syncMu sync.Mutex

// Sync 把账号投影进 config 的 ext_channels 并经 Saver 落盘（架构 §4.4）。
//
//   - 每个有账号的原生平台：渠道不存在 → 生成 managed 渠道；存在且 managed →
//     以 accounts.json 为准重建 credentials（渠道级字段不动）；存在但非
//     managed → **不接管**（WARN，绝不静默改写用户手写的渠道）；
//   - custom 平台：每账号一条 managed 渠道；
//   - orphan（managed 但无账号）保留不动（面板提示"可删除或回退"）；
//   - 无任何变更时不落盘。
//
// 返回本次同步的动作摘要（测试断言用）；警告走 logf。
func Sync(store *account.Store, loader Loader, save Saver, logf Loggerf) (string, error) {
	syncMu.Lock()
	defer syncMu.Unlock()
	if logf == nil {
		logf = func(format string, args ...any) { log.Printf("[acctchan] "+format, args...) }
	}
	accounts := store.List("")
	byPlatform := map[string][]*account.Account{}
	for _, a := range accounts {
		byPlatform[a.Platform] = append(byPlatform[a.Platform], a)
	}

	defs := loader()
	byName := map[string]int{}
	for i := range defs {
		byName[defs[i].Name] = i
	}
	changed := false
	var actions []string

	// ── 原生平台：每平台一条渠道 ────────────────────────────────
	for platform, accts := range byPlatform {
		if !account.NativePlatform(platform) {
			continue
		}
		// 账号型平台（库库AI 一类）：没有对话 API，投影只会造出一条永远打不通的
		// managed 渠道，污染路由表与模型探测。跳过即不投影——账号仍留在
		// accounts.json，账号中心的额度/签到照常工作。
		if s, ok := account.SpecOf(platform); ok && s.AccountOnly {
			continue
		}
		idx, found := byName[platform]
		switch {
		case !found:
			def, err := newManagedChannel(platform, accts)
			if err != nil {
				return "", err
			}
			defs = append(defs, def)
			byName[platform] = len(defs) - 1
			changed = true
			actions = append(actions, "created channel "+platform)
		case defs[idx].Managed:
			// 幂等：投影结果与现盘一致时不落盘（重复 Sync 不产生写流量）。
			if def, changedNow := rebuildCredentials(&defs[idx], platform, accts); changedNow {
				_ = def
				changed = true
				actions = append(actions, fmt.Sprintf("updated credentials of %s (%d accounts)", platform, len(accts)))
			}
		default:
			// 架构 §4.4-1c：同名普通渠道不接管。
			logf("渠道 %s 与账号中心平台同名但非托管，账号未挂载；请改名或迁入", platform)
		}
	}

	// ── custom 平台：每账号一条渠道 ─────────────────────────────
	for _, a := range accounts {
		if a.Platform != account.PlatformCustom {
			continue
		}
		name := CustomChannelName(a)
		idx, found := byName[name]
		switch {
		case !found:
			def, err := customChannelFor(a)
			if err != nil {
				return "", fmt.Errorf("账号 %s: %w", a.ID, err)
			}
			defs = append(defs, def)
			byName[name] = len(defs) - 1
			changed = true
			actions = append(actions, "created channel "+name)
		case defs[idx].Managed:
			if _, changedNow := rebuildCredentials(&defs[idx], account.PlatformCustom, []*account.Account{a}); changedNow {
				changed = true
				actions = append(actions, "updated credentials of "+name)
			}
		default:
			logf("渠道 %s 与账号中心同名但非托管，账号未挂载；请改名或迁入", name)
		}
	}

	if !changed {
		return "no changes", nil
	}
	if err := save(defs); err != nil {
		return "", err
	}
	return strings.Join(actions, "; "), nil
}

// newManagedChannel 生成一个原生平台的 managed 渠道（首次/用户误删后的重建）。
func newManagedChannel(platform string, accts []*account.Account) (extchan.ChannelDef, error) {
	// base_url：第一 enabled 账号 blob 的覆盖值优先，否则平台官方默认。
	base := ""
	for _, a := range accts {
		if b, err := account.BaseURLOf(platform, a.Cred); err == nil && b != "" {
			base = b
			break
		}
	}
	if base == "" {
		return extchan.ChannelDef{}, fmt.Errorf("平台 %s: 无法确定 base_url", platform)
	}
	def := extchan.ChannelDef{
		Name:     platform,
		BaseURL:  base,
		Protocol: "openai",
		Adapter:  platform,
		Note:     "账号中心托管渠道（managed）——credentials 由账号中心投影，渠道级字段可在此调整",
		Managed:  true,
		Origin:   OriginAccountCenter,
		Accounts: map[string]string{},
	}
	for _, a := range accts {
		if account.AllowPrivateOf(platform, a.Cred) {
			def.AllowPrivate = true
			break
		}
	}
	rebuildCredentials(&def, platform, accts)
	return def, nil
}

// customChannelFor 为一个 custom 账号生成 managed 渠道
// （base_url/protocol 属于账号——架构 §4.1 规则 1）。
func customChannelFor(a *account.Account) (extchan.ChannelDef, error) {
	base, err := account.BaseURLOf(account.PlatformCustom, a.Cred)
	if err != nil {
		return extchan.ChannelDef{}, err
	}
	secret, err := account.PrimarySecret(account.PlatformCustom, a.Cred)
	if err != nil {
		return extchan.ChannelDef{}, err
	}
	protocol := "openai"
	if m, merr := decodeBlobFor(a.Cred); merr == nil {
		if p, ok := m["protocol"].(string); ok && strings.TrimSpace(p) != "" {
			protocol = strings.TrimSpace(p)
		}
	}
	def := extchan.ChannelDef{
		Name:     CustomChannelName(a),
		BaseURL:  base,
		Protocol: protocol,
		Note:     "账号中心托管渠道（managed，custom）——credentials 由账号中心投影",
		Managed:  true,
		Origin:   OriginAccountCenter,
		Accounts: map[string]string{a.ID: a.ID},
	}
	def.Credentials = append(def.Credentials, extchan.CredentialDef{
		Label:    a.ID,
		Secret:   secret,
		Disabled: a.Disabled,
	})
	return def, nil
}

// CustomChannelName custom 账号的托管渠道名（"c-" + accountID：稳定、唯一、
// 满足渠道名字符集；与设计稿 "c-<label slug>" 的差异已记录——label 可含中文
// 且可改，ID 不会）。面板一致性查询与投影共用。
func CustomChannelName(a *account.Account) string {
	return ManagedChannelPrefix + a.ID
}

// rebuildCredentials 以 accounts.json 为准重建渠道的 credentials 数组
// （架构 §4.4-1b：渠道级字段不动，只重建凭证）。disabled 账号投影为
// disabled 凭证（停用账号保留在渠道里、退出轮换——面板仍可见）。
//
// 返回值：def（就地更新后的渠道）与 changedNow（本次是否真的改了内容——
// 幂等判定用：与现盘逐字段一致时返回 false，调用方不落盘）。
func rebuildCredentials(def *extchan.ChannelDef, platform string, accts []*account.Account) (*extchan.ChannelDef, bool) {
	// 先在副本上投影，与现盘对比后再落（避免"内容相同也置脏"）。
	projected := extchan.ChannelDef{
		Credentials: make([]extchan.CredentialDef, 0, len(accts)),
		Accounts:    map[string]string{},
		Managed:     true,
		Origin:      OriginAccountCenter,
	}
	skip := false
	for _, a := range accts {
		secret, err := account.PrimarySecret(platform, a.Cred)
		if err != nil {
			// 单账号 blob 坏了不拖垮整条渠道：跳过并留痕（面板一致性查询可见）。
			log.Printf("[acctchan] 账号 %s 的凭证不可投影: %v", a.ID, err)
			skip = true
			continue
		}
		projected.Accounts[a.ID] = a.ID
		projected.Credentials = append(projected.Credentials, extchan.CredentialDef{
			Label:    a.ID, // 架构 §4.1 规则 3：config 里存 ID 不存人读名
			Secret:   secret,
			Disabled: a.Disabled,
		})
	}
	changed := def.Managed != projected.Managed || def.Origin != projected.Origin ||
		!sameAccounts(def.Accounts, projected.Accounts) ||
		!sameCredentials(def.Credentials, projected.Credentials)
	if changed {
		def.Credentials = projected.Credentials
		def.Accounts = projected.Accounts
		def.Managed = projected.Managed
		def.Origin = projected.Origin
	}
	return def, changed || skip
}

// sameAccounts 账号映射对比（键集合一致即同；值恒为 ID 自身）。
func sameAccounts(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if _, ok := b[k]; !ok {
			return false
		}
	}
	return true
}

// sameCredentials 凭证数组对比（顺序敏感：投影顺序 = 账号添加顺序）。
func sameCredentials(a, b []extchan.CredentialDef) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Label != b[i].Label || a[i].Secret != b[i].Secret ||
			a[i].ExpiresAt != b[i].ExpiresAt || a[i].Disabled != b[i].Disabled {
			return false
		}
	}
	return true
}

// ChannelDefFor 单账号的探测用渠道定义（T2 面板 ProbeDraft 接线；
// 复用 extchan.ProbeDraft 的只读语义：不落盘、不污染状态）。
func ChannelDefFor(a *account.Account) (extchan.ChannelDef, error) {
	if a == nil {
		return extchan.ChannelDef{}, fmt.Errorf("账号为空")
	}
	if !account.NativePlatform(a.Platform) {
		return customChannelFor(a)
	}
	base, err := account.BaseURLOf(a.Platform, a.Cred)
	if err != nil {
		return extchan.ChannelDef{}, err
	}
	secret, err := account.PrimarySecret(a.Platform, a.Cred)
	if err != nil {
		return extchan.ChannelDef{}, err
	}
	def := extchan.ChannelDef{
		Name:     a.Platform,
		BaseURL:  base,
		Protocol: "openai",
		Adapter:  a.Platform,
		Managed:  true,
		Origin:   OriginAccountCenter,
		Accounts: map[string]string{a.ID: a.ID},
	}
	def.Credentials = append(def.Credentials, extchan.CredentialDef{
		Label:    a.ID,
		Secret:   secret,
		Disabled: a.Disabled,
	})
	return def, nil
}

// decodeBlobFor 局部解码（custom protocol 字段读取用）——复用 account 包的
// 宽松解码，避免重复实现。
func decodeBlobFor(cred json.RawMessage) (map[string]any, error) {
	return account.DecodeBlob(cred)
}
