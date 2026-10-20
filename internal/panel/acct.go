// acct.go 账号中心 API（v1.6.0 · T2）：平台账号 CRUD、导入校验、探测接线、
// 账号 ↔ 投影渠道一致性查询。
//
// ── 安全红线（架构 §6）───────────────────────────────────────
//   - accounts.json 是 0600 数据文件；本文件的所有响应**只出状态不出 blob**：
//     id/platform/label/note/签到/余额/凭证运行态——深度断言测试钉住"零 secret"；
//   - 秘密值的唯一出口是投影（SaveChannels → config.json，0600），那是
//     ext_channels 的既有哨兵体系覆盖的面。
//
// ── 单一写路径（架构 §4.2）───────────────────────────────────
// 本文件**不碰盘**：账号变更落 account.Store（事实来源），渠道投影经
// p.cfg.SaveChannels 闭包（= saveConfig 同一条链：校验/SSRF/原子写/热重载）。
package panel

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/RisingInsight/startport/internal/account"
	"github.com/RisingInsight/startport/internal/acctchan"
	"github.com/RisingInsight/startport/internal/checkin"
	"github.com/RisingInsight/startport/internal/extchan"
	"github.com/RisingInsight/startport/internal/pool"
	"github.com/RisingInsight/startport/internal/usage"
)

// platformAccountView 账号列表/详情的响应形态（**绝不含 Cred**）。
type platformAccountView struct {
	ID           string `json:"id"`
	Platform     string `json:"platform"`
	PlatformName string `json:"platform_name,omitempty"`
	Label        string `json:"label"`
	Note         string `json:"note,omitempty"`
	CreatedAt    string `json:"created_at"`
	Disabled     bool   `json:"disabled"`
	// Source 账号来源：platform（账号中心）/ codebuddy（CodeBuddy 池，统一清单只读加入）。
	Source string `json:"source,omitempty"`
	// RefreshSupported 平台有刷新实现且 blob 带 refresh_token（面板显示「刷新」按钮）。
	RefreshSupported bool                 `json:"refresh_supported,omitempty"`
	Capabilities     platformCapabilities `json:"capabilities"`
	// HealthReason 人话健康原因（正常为空）：停用 / 未投影 / 待重登 / 额度耗尽 / 冷却中。
	HealthReason string               `json:"health_reason,omitempty"`
	Checkin      platformCheckinVo    `json:"checkin"`
	Balance      *platformBalanceView `json:"balance,omitempty"`
	// Channel 投影渠道侧的运行态（账号五态判据，架构 §4.6）。
	Channel *platformChannelState `json:"channel,omitempty"`
	// Stats 近 7 天逐账号用量（外部渠道按账号 ID 归因；CodeBuddy 按池 UID）。
	Stats *platformAccountStats `json:"stats,omitempty"`
}

// platformBalanceView 余额快照的面板形态（不含逐包明细）。
type platformBalanceView struct {
	Available float64 `json:"available"`
	Total     float64 `json:"total,omitempty"`
	Unit      string  `json:"unit,omitempty"`
	PlanName  string  `json:"plan_name,omitempty"`
	ExpiresAt string  `json:"expires_at,omitempty"`
}

// platformAccountStats 近 7 天逐账号用量（外部渠道按账号 ID 归因；CodeBuddy 按 UID）。
type platformAccountStats struct {
	Requests         int64   `json:"requests"`
	Errors           int64   `json:"errors"`
	PromptTokens     int64   `json:"prompt_tokens,omitempty"`
	CompletionTokens int64   `json:"completion_tokens,omitempty"`
	TotalTokens      int64   `json:"total_tokens,omitempty"`
	Credits          float64 `json:"credits,omitempty"`
	CreditSamples    int64   `json:"credit_samples,omitempty"`
}

type platformCheckinVo struct {
	Supported   bool                    `json:"supported"`             // 平台是否有签到（qoder-intl/custom = false）
	Total       int                     `json:"total"`                 // 窗口内记录数（ok/already/failed 合计）
	Failed      int                     `json:"failed"`                // 窗口内失败记录数（失败态下单独展示，避免与成功合计混读）
	LastStatus  string                  `json:"last_status,omitempty"` // ok / already / failed
	LastAt      string                  `json:"last_at,omitempty"`
	LastMessage string                  `json:"last_message,omitempty"` // 非失败结果的说明（今日已领取/签到成功）
	LastError   string                  `json:"last_error,omitempty"`   // 最近一次失败原因（已脱敏）
	History     []account.CheckinRecord `json:"history,omitempty"`      // 详情才带；列表只给摘要
}

type platformChannelState struct {
	Name            string `json:"name"`
	Exists          bool   `json:"exists"`
	Managed         bool   `json:"managed"`
	State           string `json:"state,omitempty"`          // 凭证运行态（healthy/cooling/invalid/disabled）
	InvalidReason   string `json:"invalid_reason,omitempty"` // auth=待重登 / quota=耗尽
	CoolRemaining   int64  `json:"cool_remaining_sec,omitempty"`
	CredentialsInCh int    `json:"credentials_in_channel"`
	LastUsed        string `json:"last_used,omitempty"`
	LastOK          string `json:"last_ok,omitempty"`
	InFlight        int    `json:"in_flight,omitempty"`
	Requests        int64  `json:"requests,omitempty"`
	ExpiresAt       string `json:"expires_at,omitempty"`
	ExpiringSoon    bool   `json:"expiring_soon,omitempty"`
}

// platformCapabilities 面板按能力显隐操作（平台事实来自 account.PlatformSpec）。
type platformCapabilities struct {
	Login   bool `json:"login"`
	Checkin bool `json:"checkin"`
	Refresh bool `json:"refresh"`
	Probe   bool `json:"probe"`
	Balance bool `json:"balance"`
}

// acctDeps 账号端点的依赖快捷访问。
func (p *Panel) acctReady() bool { return p.cfg.Accounts != nil }

// syncProjection 账号变更后的投影落盘（SaveChannels 闭包 = saveConfig 同一条链）。
func (p *Panel) syncProjection() error {
	if p.cfg.SaveChannels == nil {
		return errors.New("账号投影未启用（SaveChannels 未注入）")
	}
	loader := p.cfg.LoadChannelDefs
	if loader == nil {
		loader = func() []extchan.ChannelDef { return nil }
	}
	_, err := acctchan.Sync(p.cfg.Accounts, loader, p.cfg.SaveChannels, log.Printf)
	return err
}

// platformAccountsList GET /startport/api/accounts?platform=<p>
// 统一清单：platform 为空/全部时并入 CodeBuddy 池账号（只读行，操作跳账号池）；
// platform=codebuddy 只返回池账号；其它平台只返回账号中心账号。
func (p *Panel) platformAccountsList(w http.ResponseWriter, r *http.Request) {
	if !p.acctReady() {
		writeErr(w, http.StatusNotImplemented, "账号中心未启用")
		return
	}
	filter := strings.TrimSpace(r.URL.Query().Get("platform"))
	accounts := p.cfg.Accounts.List(filter)
	statuses := p.extChannelStatuses()
	out := make([]platformAccountView, 0, len(accounts)+4)
	if filter == "" || filter == "codebuddy" {
		if p.cfg.Pool != nil {
			for _, st := range p.cfg.Pool.List() {
				out = append(out, codeBuddyAccountView(st))
			}
		}
	}
	for _, a := range accounts {
		out = append(out, p.accountView(a, statuses, false))
	}
	p.attachAccountStats(out)
	writeJSON(w, http.StatusOK, map[string]any{"accounts": out, "total": len(out)})
}

// attachAccountStats 给统一账号列表补近 7 天逐账号用量。
// 外部渠道新的 usage UID 是账号 ID；CodeBuddy 池沿用池 UID，两者都能按 ID 命中。
func (p *Panel) attachAccountStats(rows []platformAccountView) {
	if p.cfg.Usage == nil || len(rows) == 0 {
		return
	}
	snap := p.cfg.Usage.SnapshotWindow(usage.Window{Hours: 168}, map[string]string{}, nil)
	if len(snap.ByAccount) == 0 {
		return
	}
	byID := make(map[string]usage.KeyedAgg, len(snap.ByAccount))
	for _, x := range snap.ByAccount {
		byID[x.Key] = x
	}
	for i := range rows {
		x, ok := byID[rows[i].ID]
		if !ok {
			continue
		}
		rows[i].Stats = &platformAccountStats{
			Requests:         x.Requests,
			Errors:           x.Errors,
			PromptTokens:     x.PromptTokens,
			CompletionTokens: x.CompletionTok,
			TotalTokens:      x.TotalTokens,
			Credits:          x.Credits,
			CreditSamples:    x.CreditSamples,
		}
	}
}

// codeBuddyAccountView CodeBuddy 池账号 → 统一清单的只读行。
// 只做展示（积分/状态/签到/最近成功）；操作由前端跳回 CodeBuddy 账号池。
func codeBuddyAccountView(st pool.Status) platformAccountView {
	state := "healthy"
	reason := ""
	switch {
	case st.Disabled:
		state, reason = "disabled", st.DisabledReason
		if reason == "" {
			reason = "账号已禁用"
		}
	case st.Paused:
		state, reason = "paused", "暂停选号"
	case st.Cooling:
		state, reason = "cooling", st.Reason
		if reason == "" {
			reason = "冷却中"
		}
	}
	ch := &platformChannelState{
		Name: "codebuddy", Exists: true, State: state,
		CoolRemaining: st.CoolRemaining, CredentialsInCh: 1,
		InFlight: st.InFlight, Requests: st.SuccessCount + st.ErrTotal,
	}
	if !st.LastSuccessTime.IsZero() {
		ch.LastUsed = st.LastSuccessTime.UTC().Format(timeRFC3339)
		ch.LastOK = ch.LastUsed
	}
	if !st.CreditsEarliestExpiry.IsZero() {
		ch.ExpiresAt = st.CreditsEarliestExpiry.Format("2006-01-02")
	}
	label := strings.TrimSpace(st.Nickname)
	if label == "" {
		label = st.UID
	}
	platformName := "CodeBuddy"
	note := ""
	switch st.Realm {
	case "cn":
		platformName += " 国内版"
	case "global":
		platformName += " 国际版"
	default:
		note = st.Realm
	}
	if st.Enterprise {
		if note != "" {
			note += " · "
		}
		note += "企业版"
	}
	v := platformAccountView{
		ID: st.UID, Platform: "codebuddy", PlatformName: platformName,
		Label: label, Note: note, Source: "codebuddy", Disabled: st.Disabled,
		HealthReason: reason,
		Capabilities: platformCapabilities{Checkin: true},
		Balance:      &platformBalanceView{Available: float64(st.Credits), Total: float64(st.CreditsTotal), Unit: "积分"},
		Channel:      ch,
	}
	v.Checkin.Supported = true
	if st.CheckinDone {
		v.Checkin.LastStatus = checkin.StatusOK
		v.Checkin.LastAt = time.Now().UTC().Format(timeRFC3339)
		v.Checkin.LastMessage = "今日已签到（账号池）"
	}
	return v
}

// platformAccountDetail GET /startport/api/accounts/{id}（详情含签到历史）。
func (p *Panel) platformAccountDetail(w http.ResponseWriter, r *http.Request) {
	if !p.acctReady() {
		writeErr(w, http.StatusNotImplemented, "账号中心未启用")
		return
	}
	a := p.cfg.Accounts.Get(r.PathValue("id"))
	if a == nil {
		writeErr(w, http.StatusNotFound, "账号不存在")
		return
	}
	writeJSON(w, http.StatusOK, p.accountView(a, p.extChannelStatuses(), true))
}

// platformAccountsCreate POST /startport/api/accounts
// body: {"platform":"loomy","cred":{...},"label":"可选","note":"可选"}
func (p *Panel) platformAccountsCreate(w http.ResponseWriter, r *http.Request) {
	if !p.acctReady() {
		writeErr(w, http.StatusNotImplemented, "账号中心未启用")
		return
	}
	if p.cfg.SaveChannels == nil {
		writeErr(w, http.StatusNotImplemented, "账号投影未启用（SaveChannels 未注入）")
		return
	}
	var body struct {
		Platform string          `json:"platform"`
		Cred     json.RawMessage `json:"cred"`
		Label    string          `json:"label"`
		Note     string          `json:"note"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "解析请求体失败: "+err.Error())
		return
	}
	a, err := p.cfg.Accounts.Add(body.Platform, body.Cred, strings.TrimSpace(body.Label), strings.TrimSpace(body.Note))
	if err != nil {
		// 导入校验失败：400 且报错带字段名（架构 §3.1 fail fast）。
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if serr := p.syncProjection(); serr != nil {
		// 投影失败：回滚账号（保持两份数据一致——事实来源不能领先投影太多）。
		_ = p.cfg.Accounts.Delete(a.ID)
		writeErr(w, http.StatusInternalServerError, "账号已校验但投影失败，已回滚: "+serr.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"id": a.ID, "platform": a.Platform, "label": a.Label,
	})
}

// platformAccountState POST /startport/api/accounts/{id}/state
// body: {"disabled":true|false}
func (p *Panel) platformAccountState(w http.ResponseWriter, r *http.Request) {
	if !p.acctReady() {
		writeErr(w, http.StatusNotImplemented, "账号中心未启用")
		return
	}
	var body struct {
		Disabled *bool `json:"disabled"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil || body.Disabled == nil {
		writeErr(w, http.StatusBadRequest, "body 需为 {\"disabled\":true|false}")
		return
	}
	if err := p.cfg.Accounts.SetDisabled(r.PathValue("id"), *body.Disabled); err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	if serr := p.syncProjection(); serr != nil {
		writeErr(w, http.StatusInternalServerError, "状态已记录但投影失败: "+serr.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// platformAccountDelete DELETE /startport/api/accounts/{id}
// 删除账号 + 投影：从 managed 渠道里**移除该凭证**（已删账号的秘密值不能继续
// 留在 config.json——Sync 的孤儿语义是"保留渠道等面板处置"，所以这里主动摘除；
// 最后一个账号被删时整条渠道随之移除）。
func (p *Panel) platformAccountDelete(w http.ResponseWriter, r *http.Request) {
	if !p.acctReady() {
		writeErr(w, http.StatusNotImplemented, "账号中心未启用")
		return
	}
	id := r.PathValue("id")
	a := p.cfg.Accounts.Get(id)
	if a == nil {
		writeErr(w, http.StatusNotFound, "账号不存在")
		return
	}
	if err := p.cfg.Accounts.Delete(id); err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	if serr := p.removeAccountFromProjection(a); serr != nil {
		writeErr(w, http.StatusInternalServerError, "账号已删除但投影失败: "+serr.Error())
		return
	}
	if serr := p.syncProjection(); serr != nil {
		writeErr(w, http.StatusInternalServerError, "账号已删除但热重载失败: "+serr.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// removeAccountFromProjection 从渠道投影里摘除一个已删除账号的凭证；
// 渠道凭证清空后整条渠道移除（managed 渠道没有凭证就没有存在意义）。
func (p *Panel) removeAccountFromProjection(a *account.Account) error {
	if p.cfg.SaveChannels == nil {
		return errors.New("账号投影未启用")
	}
	loader := p.cfg.LoadChannelDefs
	if loader == nil {
		loader = func() []extchan.ChannelDef { return nil }
	}
	defs := loader()
	channel := a.Platform
	if a.Platform == account.PlatformCustom {
		channel = acctchan.CustomChannelName(a)
	}
	for i := range defs {
		if defs[i].Name != channel {
			continue
		}
		var kept []extchan.CredentialDef
		for _, c := range defs[i].Credentials {
			if c.Label != a.ID {
				kept = append(kept, c)
			}
		}
		if len(kept) == 0 {
			// 最后一个账号：整条渠道移除。
			defs = append(defs[:i], defs[i+1:]...)
		} else {
			defs[i].Credentials = kept
			delete(defs[i].Accounts, a.ID)
		}
		break
	}
	return p.cfg.SaveChannels(defs)
}

// platformAccountProbe POST /startport/api/accounts/{id}/probe
// 用账号构造单账号渠道草稿走 extchan.ProbeDraft（只读：不落盘、不污染状态，
// PRD P0-7 语义沿用）。
func (p *Panel) platformAccountProbe(w http.ResponseWriter, r *http.Request) {
	if !p.acctReady() {
		writeErr(w, http.StatusNotImplemented, "账号中心未启用")
		return
	}
	if p.cfg.ExtChan == nil {
		writeErr(w, http.StatusNotImplemented, "外部渠道引擎未启用")
		return
	}
	a := p.cfg.Accounts.Get(r.PathValue("id"))
	if a == nil {
		writeErr(w, http.StatusNotFound, "账号不存在")
		return
	}
	def, err := acctchan.ChannelDefFor(a)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	res := p.cfg.ExtChan.ProbeDraft(r.Context(), def, a.ID)
	writeJSON(w, http.StatusOK, map[string]any{"ok": res.OK, "result": res})
}

// platformAccountRefresh POST /startport/api/accounts/{id}/refresh
// 手动刷新登录态（AutoClaw / Qoder）：刷新 → 回写 accounts.json → 立即投影。
// 与签到失败触发的自动刷新同一条链，用于真实账号验收与令牌临期自救。
func (p *Panel) platformAccountRefresh(w http.ResponseWriter, r *http.Request) {
	if !p.acctReady() {
		writeErr(w, http.StatusNotImplemented, "账号中心未启用")
		return
	}
	a := p.cfg.Accounts.Get(r.PathValue("id"))
	if a == nil {
		writeErr(w, http.StatusNotFound, "账号不存在")
		return
	}
	if !account.CanRefresh(a) {
		writeErr(w, http.StatusBadRequest,
			"该账号没有可用的刷新凭证（平台不支持自动刷新或 blob 缺 refresh_token）")
		return
	}
	refresh := p.cfg.RefreshAccount
	if refresh == nil {
		refresh = checkin.RefreshAccount
	}
	updated, err := refresh(r.Context(), a)
	if err != nil {
		status := http.StatusBadGateway
		if errors.Is(err, checkin.ErrAuthExpired) || errors.Is(err, checkin.ErrPlatformNotSupported) {
			status = http.StatusBadRequest
		}
		writeErr(w, status, "刷新失败："+err.Error())
		return
	}
	raw, err := json.Marshal(updated)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "序列化刷新结果失败: "+err.Error())
		return
	}
	if err := p.cfg.Accounts.UpdateCred(a.ID, raw); err != nil {
		writeErr(w, http.StatusInternalServerError, "回写账号失败: "+err.Error())
		return
	}
	if err := p.syncProjection(); err != nil {
		writeErr(w, http.StatusInternalServerError,
			"登录态已刷新但投影失败（accounts.json 已更新，下次 Reconcile 修齐）: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// platformAccountBalance POST /startport/api/accounts/{id}/balance
// 查询平台余额并写入 accounts.json 的 balance 快照（Hero 积分列的数据来源）。
func (p *Panel) platformAccountBalance(w http.ResponseWriter, r *http.Request) {
	if !p.acctReady() {
		writeErr(w, http.StatusNotImplemented, "账号中心未启用")
		return
	}
	a := p.cfg.Accounts.Get(r.PathValue("id"))
	if a == nil {
		writeErr(w, http.StatusNotFound, "账号不存在")
		return
	}
	if !checkin.BalanceSupported(a.Platform) {
		writeErr(w, http.StatusBadRequest, "该平台暂不支持余额查询")
		return
	}
	query := p.cfg.BalanceAccount
	if query == nil {
		query = checkin.QueryBalance
	}
	snap, err := query(r.Context(), a)
	if err != nil {
		status := http.StatusBadGateway
		if errors.Is(err, checkin.ErrAuthExpired) || errors.Is(err, checkin.ErrPlatformNotSupported) {
			status = http.StatusBadRequest
		}
		writeErr(w, status, "余额查询失败："+err.Error())
		return
	}
	if err := p.cfg.Accounts.SetBalance(a.ID, snap); err != nil {
		writeErr(w, http.StatusInternalServerError, "余额已查询但回写失败: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "balance": snap})
}

// platformAccountsConsistency GET /startport/api/accounts/consistency
// 账号 ↔ 投影渠道的一致性查询（架构 T2 验收 ④）：每平台渠道是否存在/是否
// managed/凭证数 vs 账号数/孤儿与缺失清单。
func (p *Panel) platformAccountsConsistency(w http.ResponseWriter, r *http.Request) {
	if !p.acctReady() {
		writeErr(w, http.StatusNotImplemented, "账号中心未启用")
		return
	}
	if p.cfg.LoadChannelDefs == nil {
		writeErr(w, http.StatusNotImplemented, "一致性查询未启用（LoadChannelDefs 未注入）")
		return
	}
	defs := p.cfg.LoadChannelDefs()
	byName := map[string]extchan.ChannelDef{}
	for _, def := range defs {
		byName[def.Name] = def
	}
	accounts := p.cfg.Accounts.List("")
	byPlatform := map[string][]*account.Account{}
	for _, a := range accounts {
		byPlatform[a.Platform] = append(byPlatform[a.Platform], a)
	}
	out := make([]map[string]any, 0, len(byPlatform))
	for platform, accts := range byPlatform {
		expected := map[string]bool{}
		for _, a := range accts {
			expected[a.ID] = true
		}
		item := map[string]any{
			"platform":       platform,
			"accounts_total": len(accts),
		}
		def, found := byName[platform]
		if !found && platform == account.PlatformCustom {
			// custom 每账号一条渠道：聚合成"逐账号"报告。
			var missing, orphans []string
			for _, a := range accts {
				cdef, ok := byName[acctchan.CustomChannelName(a)]
				if !ok {
					missing = append(missing, a.ID)
					continue
				}
				if !credLabelExists(cdef, a.ID) {
					missing = append(missing, a.ID)
				}
			}
			for name, def := range byName {
				if !strings.HasPrefix(name, acctchan.ManagedChannelPrefix) || !def.Managed {
					continue
				}
				for _, c := range def.Credentials {
					if !expected[c.Label] {
						orphans = append(orphans, name+"/"+c.Label)
					}
				}
			}
			item["kind"] = "custom"
			item["missing"] = missing
			item["orphans"] = orphans
			item["consistent"] = len(missing) == 0 && len(orphans) == 0
			out = append(out, item)
			continue
		}
		if !found {
			item["channel_exists"] = false
			item["managed"] = false
			item["consistent"] = false
			out = append(out, item)
			continue
		}
		item["channel_exists"] = true
		item["managed"] = def.Managed
		item["credentials_total"] = len(def.Credentials)
		var missing, orphans []string
		for _, c := range def.Credentials {
			if !expected[c.Label] {
				orphans = append(orphans, c.Label)
			}
		}
		for id := range expected {
			if !credLabelExists(def, id) {
				missing = append(missing, id)
			}
		}
		item["missing"] = missing
		item["orphans"] = orphans
		item["consistent"] = def.Managed && len(missing) == 0 && len(orphans) == 0
		out = append(out, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{"platforms": out})
}

// credLabelExists 渠道凭证里是否有指定 label。
func credLabelExists(def extchan.ChannelDef, label string) bool {
	for _, c := range def.Credentials {
		if c.Label == label {
			return true
		}
	}
	return false
}

// accountHealthReason 把账号/渠道状态翻成一句人话（统一视图的「状态/原因」列）。
func accountHealthReason(a *account.Account, ch *platformChannelState) string {
	if a == nil {
		return ""
	}
	if a.Disabled {
		return "账号已停用"
	}
	if ch == nil || !ch.Exists {
		return "托管渠道缺失（等待投影）"
	}
	switch ch.State {
	case "invalid":
		if ch.InvalidReason == "quota" {
			return "额度耗尽（可签到复活）"
		}
		return "待重新登录"
	case "cooling":
		return "凭证冷却中（限流/额度）"
	case "disabled":
		return "账号已停用"
	}
	return ""
}

// accountView 账号 → 响应视图（**绝不含 Cred**；withCheckin=true 带历史）。
func (p *Panel) accountView(a *account.Account, statuses map[string]extchan.Status, withCheckin bool) platformAccountView {
	v := platformAccountView{
		ID:               a.ID,
		Platform:         a.Platform,
		Label:            a.Label,
		Note:             a.Note,
		CreatedAt:        a.CreatedAt.UTC().Format(timeRFC3339),
		Disabled:         a.Disabled,
		Source:           "platform",
		RefreshSupported: account.CanRefresh(a),
	}
	if spec, ok := account.SpecOf(a.Platform); ok {
		v.PlatformName = spec.Name
		v.Capabilities.Login = spec.Login != account.LoginNone
	}
	v.Capabilities.Checkin = checkinSupported(a.Platform)
	v.Capabilities.Refresh = account.CanRefresh(a)
	v.Capabilities.Probe = true
	v.Capabilities.Balance = checkin.BalanceSupported(a.Platform)
	v.Checkin.Supported = v.Capabilities.Checkin
	for _, r := range a.Checkin {
		v.Checkin.Total++
		if r.Status == checkin.StatusFailed {
			v.Checkin.Failed++
		}
		if withCheckin {
			v.Checkin.History = append(v.Checkin.History, r)
		}
	}
	// RecordCheckin 前插新记录：切片首条即最近一次。
	if len(a.Checkin) > 0 {
		last := a.Checkin[0]
		v.Checkin.LastStatus = last.Status
		v.Checkin.LastAt = last.At.UTC().Format(timeRFC3339)
		v.Checkin.LastMessage = last.Message
		if last.Status == checkin.StatusFailed {
			v.Checkin.LastError = last.Error
		} else if v.Checkin.LastMessage == "" {
			// 旧记录（v1.7.6 之前）把 already 的说明写进了 Error：兼容展示。
			v.Checkin.LastMessage = last.Error
		}
	}
	if a.Balance != nil {
		v.Balance = &platformBalanceView{
			Available: a.Balance.Available,
			Total:     a.Balance.Total,
			Unit:      a.Balance.Unit,
			PlanName:  a.Balance.PlanName,
		}
		if !a.Balance.ExpiresAt.IsZero() {
			v.Balance.ExpiresAt = a.Balance.ExpiresAt.Format("2006-01-02")
		}
	}
	// 投影渠道侧运行态（五态判据的数据源）。
	name := a.Platform
	if a.Platform == account.PlatformCustom {
		name = acctchan.CustomChannelName(a)
	}
	if st, ok := statuses[name]; ok {
		cs := &platformChannelState{
			Name:            name,
			Exists:          true,
			Managed:         st.Managed,
			CredentialsInCh: st.CredentialsTotal,
		}
		for _, c := range st.Credentials {
			if c.Label != a.ID {
				continue
			}
			cs.State = string(c.State)
			cs.InvalidReason = c.InvalidReason
			cs.CoolRemaining = c.CoolRemain
			if !c.LastUsed.IsZero() {
				cs.LastUsed = c.LastUsed.UTC().Format(timeRFC3339)
			}
			if !c.LastOK.IsZero() {
				cs.LastOK = c.LastOK.UTC().Format(timeRFC3339)
			}
			cs.InFlight = c.InFlight
			cs.Requests = c.Requests
			cs.ExpiresAt = c.ExpiresAt
			cs.ExpiringSoon = c.ExpiringSoon
			if a.Disabled || c.Disabled {
				cs.State = "disabled"
			}
		}
		v.Channel = cs
	}
	v.HealthReason = accountHealthReason(a, v.Channel)
	return v
}

// extChannelStatuses 渠道快照按名索引（ExtChan 为 nil 时返回空表）。
func (p *Panel) extChannelStatuses() map[string]extchan.Status {
	if p.cfg.ExtChan == nil {
		return nil
	}
	out := map[string]extchan.Status{}
	for _, st := range p.cfg.ExtChan.Status() {
		out[st.Name] = st
	}
	return out
}

// checkinSupported 平台是否有签到活动（internal/checkin 注册表驱动：loomy /
// autoclaw / autoclaw-intl / qoder 支持；qoder-intl 与 custom 无——架构 §5 Q4/Q5，
// For() 返回 false 时 UI 显示「平台无此活动」）。
func checkinSupported(platform string) bool {
	return checkin.Supported(platform)
}

// timeRFC3339 响应时间戳格式。
const timeRFC3339 = "2006-01-02T15:04:05Z07:00"
