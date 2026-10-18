// extchan.go 外部渠道面板接口。
//
// 管理面（增删改渠道）仍只有一条写路径：POST /startport/api/config 提交
// ext_channels 数组（main 侧热重载）。本文件提供**只读**接口：
//   - GET  /startport/api/ext_channels        渠道 + 凭证运行态（永不回显 secret）
//   - GET  /startport/api/ext_intel           额度情报（token-fbi data.json 快照）
//   - GET  /startport/api/ext_presets         平台预设目录（向导/草稿建议值）
//   - POST /startport/api/ext_channels/probe  只读连通性探测（草稿或已保存渠道）
//
// 探测不落盘、不改渠道状态；错误摘要已脱敏（不含 secret 与 URL query 值）。
package panel

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/RisingInsight/startport/internal/extchan"
	"github.com/RisingInsight/startport/internal/provider"
	"github.com/RisingInsight/startport/internal/upstream"
)

// extchanStatus 渠道运行态快照 + 汇总条。
func (p *Panel) extchanStatus(w http.ResponseWriter, r *http.Request) {
	if p.cfg.ExtChan == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "channels": []any{},
			"summary": map[string]int{"channels": 0, "credentials": 0, "usable": 0, "abnormal": 0},
		})
		return
	}
	channels := p.cfg.ExtChan.Status()
	summary := map[string]int{
		"channels":    len(channels),
		"credentials": 0,
		"usable":      0,
		"abnormal":    0,
	}
	for _, ch := range channels {
		summary["credentials"] += ch.CredentialsTotal
		summary["usable"] += ch.CredentialsUsable
		switch ch.State {
		case extchan.StateAvailable, extchan.StateDisabled:
			// 停用是用户意图，不算"异常"。
		default:
			summary["abnormal"]++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"channels": channels,
		"summary":  summary,
		"now":      time.Now(),
	})
}

// intelItemView 情报条目 + 预设匹配结果（P0-10：草稿入口需要的元数据）。
type intelItemView struct {
	extchan.IntelItem
	PresetKey      string `json:"preset_key,omitempty"`
	NativeRequired bool   `json:"native_required"`
	Configured     bool   `json:"configured"`
}

// extchanIntel 额度情报快照（未启用/未拉到时 ok:false + enabled:false，前端显示安静态）。
// 每条附带预设匹配（preset_key/native_required）与「已接入」标记，
// 供「生成草稿」按钮直接使用——纯展示，不参与任何路由决策。
func (p *Panel) extchanIntel(w http.ResponseWriter, r *http.Request) {
	if p.cfg.ExtChan == nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "enabled": false, "items": []any{}})
		return
	}
	data, at, lastErr, ok := p.cfg.ExtChan.Intel()
	status := p.cfg.ExtChan.IntelStatus()
	out := map[string]any{
		"ok":     true,
		"status": status,
		"now":    time.Now(),
	}
	if !ok {
		out["enabled"] = false
		out["items"] = []any{}
		if lastErr != "" {
			out["last_error"] = lastErr
		}
		writeJSON(w, http.StatusOK, out)
		return
	}
	// 已接入渠道名集合（规范化后比较，纯前端标记用；服务端不加状态）。
	configured := map[string]bool{}
	for _, ch := range p.cfg.ExtChan.Status() {
		configured[provider.NormalizeKey(ch.Name)] = true
	}
	items := make([]intelItemView, 0, len(data.Items))
	for _, it := range data.Items {
		view := intelItemView{IntelItem: it}
		if preset, hit := provider.MatchPreset(it.Name, it.Vendor); hit {
			view.PresetKey = preset.Key
			view.NativeRequired = preset.NativeRequired
			view.Configured = configured[provider.NormalizeKey(preset.Key)]
		} else {
			view.Configured = configured[provider.NormalizeKey(it.Name)]
		}
		items = append(items, view)
	}
	out["enabled"] = true
	out["fetched_at"] = at
	out["items"] = items
	if data.UpdatedAt != "" {
		out["source_updated_at"] = data.UpdatedAt
	}
	if lastErr != "" {
		out["last_error"] = lastErr
	}
	writeJSON(w, http.StatusOK, out)
}

// extPresets 平台预设目录（向导「从预设开始」与情报草稿的建议值来源）。
// base_url 一律为空——地址必须由用户按官方文档确认后填。
func (p *Panel) extPresets(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"presets": provider.Presets(),
	})
}

// extProbeRequest 探测请求体。三种形态：
//
//	{"channel": {...}}                     草稿（优先；未保存，秘密值可明文）
//	{"draft": {...}}                       同上（兼容架构文档的字段名）
//	{"name": "siliconflow", "credential_label": "primary"}   已保存渠道
type extProbeRequest struct {
	Channel         *extchan.ChannelDef `json:"channel"`
	Draft           *extchan.ChannelDef `json:"draft"`
	Name            string              `json:"name"`
	CredentialLabel string              `json:"credential_label"`
	Credential      string              `json:"credential"` // 兼容旧字段名
}

// extchanProbe 只读连通性探测：只发模型目录请求，不落盘、不改状态。
func (p *Panel) extchanProbe(w http.ResponseWriter, r *http.Request) {
	if p.cfg.ExtChan == nil {
		writeErr(w, http.StatusNotImplemented, "ext channels are not configured")
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	var req extProbeRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "parse body: "+err.Error())
		return
	}
	label := strings.TrimSpace(req.CredentialLabel)
	if label == "" {
		label = strings.TrimSpace(req.Credential)
	}

	draft := req.Channel
	if draft == nil {
		draft = req.Draft
	}
	if draft != nil {
		// SSRF 红线：与保存/启动同一套校验（元数据端点恒拒，内网需 allow_private）。
		if err := upstream.ValidateOutboundBaseAllowPrivate(draft.BaseURL, "channel.base_url", draft.AllowPrivate); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"ok":    false,
				"error": err.Error(),
				"result": extchan.ProbeResult{
					Protocol:  draft.Protocol,
					Probe:     "models",
					Error:     err.Error(),
					ErrorCode: "invalid_base_url",
				},
			})
			return
		}
		res := p.cfg.ExtChan.ProbeDraft(r.Context(), *draft, label)
		writeJSON(w, http.StatusOK, map[string]any{"ok": res.OK, "result": res})
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		writeErr(w, http.StatusBadRequest, "probe requires either channel (draft) or name (saved channel)")
		return
	}
	res := p.cfg.ExtChan.ProbeSaved(r.Context(), strings.TrimSpace(req.Name), label)
	writeJSON(w, http.StatusOK, map[string]any{"ok": res.OK, "result": res})
}

// ExtChanManager 面板对外部渠道管理器的只读依赖（避免 panel 直接 import 并
// 持有 *extchan.Manager 的写面；接口取观测快照/探测足够）。
type ExtChanManager interface {
	Status() []extchan.Status
	Intel() (*extchan.IntelData, time.Time, string, bool)
	IntelStatus() map[string]any
	// ProbeDraft 探测未保存草稿（不落盘、不改状态）。
	ProbeDraft(ctx context.Context, def extchan.ChannelDef, credentialLabel string) extchan.ProbeResult
	// ProbeSaved 探测已保存渠道（可选指定凭证 label）。
	ProbeSaved(ctx context.Context, name, credentialLabel string) extchan.ProbeResult
}
