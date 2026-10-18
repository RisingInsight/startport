// migrate.go 迁入 / 回退（账号中心 v1.6.0 · T5；架构 §7.3）。
//
// ── 迁入（Q6：仅按钮不自动）─────────────────────────────────
// 把一条**非托管**渠道的凭证逐条转为账号中心账号（blob 按平台 schema 重组），
// 成功后渠道转 managed（Reconcile 接管）。**不产生半迁移态**：先建全部账号、
// 任一失败则回滚已建账号并整体报错；全部成功才转 managed + 落盘 + 热重载。
// 渠道级字段（base_url/models/aliases/note）原样保留——架构 §4.5 规则 1。
//
// ── 回退（零数据损失）───────────────────────────────────────
// managed 渠道解除托管（只动 managed/origin/accounts 三个标志键），账号从
// 账号中心删除，凭证**原样留在渠道**（本来就是投影产出的合法渠道凭证——
// 架构 §7.3："credentials 数据原样保留"）。前端做二次确认，后端一个端点做
// 完整还原。
//
// 两条端点的响应**零 secret**（秘密值只存在于 0600 两文件，接口只回 label/id）。
package panel

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/RisingInsight/startport/internal/account"
	"github.com/RisingInsight/startport/internal/acctchan"
	"github.com/RisingInsight/startport/internal/extchan"
)

// migrateReport 迁移结果（响应零 secret：只有 label 与错误文案）。
type migrateReport struct {
	Migrated []map[string]any `json:"migrated,omitempty"` // {account_id, label}
	Failed   []map[string]any `json:"failed,omitempty"`   // {label, error}
	Total    int              `json:"total"`
}

// channelMigrate POST /startport/api/channels/{name}/migrate
func (p *Panel) channelMigrate(w http.ResponseWriter, r *http.Request) {
	if !p.acctReady() || p.cfg.SaveChannels == nil || p.cfg.LoadChannelDefs == nil {
		writeErr(w, http.StatusNotImplemented, "账号中心未启用")
		return
	}
	name := r.PathValue("name")
	defs := p.cfg.LoadChannelDefs()
	idx := -1
	for i := range defs {
		if defs[i].Name == name {
			idx = i
			break
		}
	}
	if idx < 0 {
		writeErr(w, http.StatusNotFound, "渠道不存在: "+name)
		return
	}
	def := defs[idx]
	if def.Managed {
		writeErr(w, http.StatusConflict, "该渠道已由账号中心托管，无需迁入")
		return
	}
	platform := migratePlatformOf(def)
	if platform == "" {
		writeErr(w, http.StatusBadRequest,
			fmt.Sprintf("渠道 %s 的适配器 %q 无法对应账号平台（支持：%s 及通用平台）",
				name, def.Adapter, strings.Join([]string{
					account.PlatformLoomy, account.PlatformAutoClaw, account.PlatformAutoClawIntl,
					account.PlatformQoder, account.PlatformQoderIntl,
				}, "/")))
		return
	}
	if len(def.Credentials) == 0 {
		writeErr(w, http.StatusBadRequest, "渠道没有可迁移的凭证")
		return
	}

	// ── D1 前置校验（发布修复）：原生平台只支持一条托管渠道 ─────────
	// 投影模型是"每平台一条渠道"——同平台第二条渠道迁入会让两条渠道同时
	// managed、accounts 互相重叠，且回退时会误删兄弟渠道的账号（账号级数据
	// 损失）。所以：该平台在账号中心已有账号，或已存在同平台 managed 渠道
	// （历史脏数据形态）→ 400 指路。
	if account.NativePlatform(platform) {
		if existing := p.cfg.Accounts.List(platform); len(existing) > 0 {
			writeErr(w, http.StatusBadRequest, fmt.Sprintf(
				"平台 %s 已有账号中心账号（%d 个）——同一平台只支持一条托管渠道；"+
					"请先在账号中心回退，或直接在账号中心继续添加该平台账号",
				platform, len(existing)))
			return
		}
		for i := range defs {
			if i != idx && defs[i].Name == platform && defs[i].Managed {
				writeErr(w, http.StatusBadRequest, fmt.Sprintf(
					"平台 %s 已存在托管渠道 %q——同一平台只支持一条托管渠道；"+
						"请先回退，或直接在账号中心继续添加该平台账号", platform, defs[i].Name))
				return
			}
		}
	}

	// ── 先建全部账号（任一失败 → 回滚已建，不产生半迁移态）────────
	report := migrateReport{Total: len(def.Credentials)}
	var added []*account.Account
	for i, cred := range def.Credentials {
		label := strings.TrimSpace(cred.Label)
		if label == "" {
			label = fmt.Sprintf("%s-%d", name, i+1)
		}
		blob, err := migrateBlob(platform, def, cred.Secret)
		if err != nil {
			report.Failed = append(report.Failed, map[string]any{"label": label, "error": err.Error()})
			continue
		}
		a, aerr := p.cfg.Accounts.Add(platform, blob, label,
			"迁移自渠道 "+name)
		if aerr != nil {
			report.Failed = append(report.Failed, map[string]any{"label": label, "error": aerr.Error()})
			continue
		}
		added = append(added, a)
		report.Migrated = append(report.Migrated, map[string]any{
			"account_id": a.ID, "label": a.Label,
		})
	}
	if len(report.Failed) > 0 {
		for _, a := range added {
			_ = p.cfg.Accounts.Delete(a.ID)
		}
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error":    "部分凭证无法迁移，已回滚全部账号（未产生半迁移态）",
			"migrated": report.Migrated,
			"failed":   report.Failed,
			"total":    report.Total,
		})
		return
	}
	if len(added) == 0 {
		writeErr(w, http.StatusBadRequest, "没有可迁移的凭证")
		return
	}

	// ── 全部成功：渠道转 managed（三键），credentials 由投影重建 ────
	for i := range defs {
		if defs[i].Name == name {
			defs[i].Managed = true
			defs[i].Origin = acctchan.OriginAccountCenter
			if defs[i].Accounts == nil {
				defs[i].Accounts = map[string]string{}
			}
		}
	}
	if err := p.cfg.SaveChannels(defs); err != nil {
		// 落盘失败：回滚账号（两份数据必须一致——事实来源不能领先投影）。
		for _, a := range added {
			_ = p.cfg.Accounts.Delete(a.ID)
		}
		writeErr(w, http.StatusInternalServerError, "迁移已回滚（配置落盘失败）: "+err.Error())
		return
	}
	if serr := p.syncProjection(); serr != nil {
		writeErr(w, http.StatusInternalServerError, "迁移已落盘但热重载失败: "+serr.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"migrated": report.Migrated,
		"total":    report.Total,
	})
}

// channelRollback POST /startport/api/channels/{name}/rollback
// managed 渠道 → 普通渠道：账号从账号中心删除、托管三键清除、凭证原样保留
// （架构 §7.3：回退零数据损失）。二次确认由前端负责。
func (p *Panel) channelRollback(w http.ResponseWriter, r *http.Request) {
	if !p.acctReady() || p.cfg.SaveChannels == nil || p.cfg.LoadChannelDefs == nil {
		writeErr(w, http.StatusNotImplemented, "账号中心未启用")
		return
	}
	name := r.PathValue("name")
	defs := p.cfg.LoadChannelDefs()
	idx := -1
	for i := range defs {
		if defs[i].Name == name {
			idx = i
			break
		}
	}
	if idx < 0 {
		writeErr(w, http.StatusNotFound, "渠道不存在: "+name)
		return
	}
	def := defs[idx]
	if !def.Managed {
		writeErr(w, http.StatusConflict, "该渠道不是账号中心托管渠道，无需回退")
		return
	}
	// 删除对应账号（架构 §7.3 默认：回退 = 账号移除、凭证留在渠道）。
	removed := 0
	if def.Adapter != "" && account.NativePlatform(def.Adapter) {
		for _, a := range p.cfg.Accounts.List(def.Adapter) {
			if err := p.cfg.Accounts.Delete(a.ID); err == nil {
				removed++
			}
		}
	} else {
		// custom：渠道名 = "c-<accountID>"。
		if id := strings.TrimPrefix(name, acctchan.ManagedChannelPrefix); id != name {
			if err := p.cfg.Accounts.Delete(id); err == nil {
				removed++
			}
		}
	}
	// 解除托管：只动三个标志键，credentials 原样保留（零数据损失）。
	for i := range defs {
		if defs[i].Name == name {
			defs[i].Managed = false
			defs[i].Origin = ""
			defs[i].Accounts = nil
		}
	}
	// ── D2 兜底清理（发布修复）：消灭兄弟渠道的"managed 标记 + 死账号引用"──
	// 历史脏数据（同平台多条 managed 渠道）里，本渠道删除账号后，兄弟渠道的
	// accounts 映射/凭证引用可能指向已删账号。扫一遍全部 managed 原生平台渠道：
	// 该平台在账号中心已无账号 → 清除托管标记（credentials 原样保留）；仍有
	// 账号 → 交由下面的 syncProjection 以 accounts 为准重建。
	if cleaned := cleanupStaleManagedChannels(defs, p.cfg.Accounts); len(cleaned) > 0 {
		log.Printf("[migrate] 回退兜底清理：以下渠道的托管标记已随账号移除而清除（凭证保留）: %s",
			strings.Join(cleaned, ", "))
	}
	if err := p.cfg.SaveChannels(defs); err != nil {
		writeErr(w, http.StatusInternalServerError, "回退落盘失败: "+err.Error())
		return
	}
	if serr := p.syncProjection(); serr != nil {
		writeErr(w, http.StatusInternalServerError, "回退已落盘但热重载失败: "+serr.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "removed_accounts": removed,
		"message": "已回退为普通渠道；凭证原样保留在渠道里",
	})
}

// cleanupStaleManagedChannels D2 兜底清理：对每个 managed 的**原生平台**渠道，
// 若账号中心该平台已无账号 → 清除托管三键（credentials 原样保留，数据零损失），
// 消灭"managed 标记 + 死账号引用"。平台仍有账号的 managed 渠道不动——它们由
// syncProjection 的重建路径以 accounts 为准修齐（有账号平台 Sync 必然重建凭证）。
// 返回被清理的渠道名（日志用）。
func cleanupStaleManagedChannels(defs []extchan.ChannelDef, store *account.Store) []string {
	platformAccounts := map[string]int{}
	for _, a := range store.List("") {
		platformAccounts[a.Platform]++
	}
	var cleaned []string
	for i := range defs {
		def := &defs[i]
		if !def.Managed || def.Adapter == "" || !account.NativePlatform(def.Adapter) {
			continue
		}
		if platformAccounts[def.Adapter] == 0 {
			def.Managed = false
			def.Origin = ""
			def.Accounts = nil
			cleaned = append(cleaned, def.Name)
		}
	}
	return cleaned
}

// migratePlatformOf 渠道 → 账号平台（adapter 即平台；无 adapter = custom）。
func migratePlatformOf(def extchan.ChannelDef) string {
	if def.Adapter != "" && account.ValidPlatform(def.Adapter) {
		return def.Adapter
	}
	if def.Adapter == "" {
		return account.PlatformCustom
	}
	return ""
}

// migrateBlob 渠道凭证 → 平台账号 blob（架构 §3.1 各平台 schema 重组）。
// 迁移失败必须报错带原因（如 qoder 缺 user_id——COSY 签名需要，无法凭空生成）。
func migrateBlob(platform string, def extchan.ChannelDef, secret string) (json.RawMessage, error) {
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return nil, fmt.Errorf("凭证 secret 为空")
	}
	switch platform {
	case account.PlatformLoomy:
		return mustJSON(map[string]any{"session": secret}), nil
	case account.PlatformAutoClaw, account.PlatformAutoClawIntl:
		return mustJSON(map[string]any{"token": secret}), nil
	case account.PlatformQoder, account.PlatformQoderIntl:
		// 渠道 secret 可能已是适配器形态的账号 JSON（此前经账号中心投影后手工
		// 复制过），也可能是裸 token——后者缺 user_id（COSY 签名需要），必须
		// 让用户手工补（无法凭空生成用户标识）。
		var probe map[string]any
		if err := json.Unmarshal([]byte(secret), &probe); err == nil {
			if _, hasAT := probe["accessToken"]; hasAT {
				if uid, ok := probe["userId"].(string); ok && strings.TrimSpace(uid) != "" {
					return json.RawMessage(secret), nil // 已是完整适配器 JSON
				}
			}
		}
		return nil, fmt.Errorf(
			"qoder 凭证需要 user_id（COSY 签名需要用户标识），裸 token 无法自动迁移；" +
				"请到账号中心手工添加该账号（blob：access_token + user_id）")
	case account.PlatformCustom:
		blob := map[string]any{
			"base_url": def.BaseURL,
			"secret":   secret,
		}
		if def.Protocol != "" {
			blob["protocol"] = def.Protocol
		}
		if def.AllowPrivate {
			blob["allow_private"] = true
		}
		return mustJSON(blob), nil
	default:
		return nil, fmt.Errorf("未知平台 %q", platform)
	}
}

func mustJSON(v any) json.RawMessage {
	raw, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return raw
}
