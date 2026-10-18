// extchan.go 外部渠道面板接口：渠道观测快照 + 额度情报展示。
//
// 管理面（增删改渠道）走既有配置接口（POST /startport/api/config 提交
// ext_channels 数组，main 侧热重载）——本文件只提供**只读观测**：
//   - GET /startport/api/ext_channels  渠道运行态（冷却/在途/目录/最近错误）
//   - GET /startport/api/ext_intel     额度情报（token-fbi data.json 快照）
//
// 两个接口都不回显 api_key（Snapshot 结构体天然不含）。
package panel

import (
	"net/http"
	"time"

	"github.com/RisingInsight/startport/internal/extchan"
)

// extchanStatus 渠道运行态快照。
func (p *Panel) extchanStatus(w http.ResponseWriter, r *http.Request) {
	if p.cfg.ExtChan == nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "channels": []any{}})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"channels": p.cfg.ExtChan.Status(),
		"now":      time.Now(),
	})
}

// extchanIntel 额度情报快照（未启用/未拉到时 ok:false + enabled:false，前端显示安静态）。
func (p *Panel) extchanIntel(w http.ResponseWriter, r *http.Request) {
	if p.cfg.ExtChan == nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "enabled": false, "items": []any{}})
		return
	}
	data, at, lastErr, ok := p.cfg.ExtChan.Intel()
	status := p.cfg.ExtChan.IntelStatus()
	out := map[string]any{
		"ok":      true,
		"status":  status,
		"now":     time.Now(),
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
	out["enabled"] = true
	out["fetched_at"] = at
	out["items"] = data.Items
	if data.UpdatedAt != "" {
		out["source_updated_at"] = data.UpdatedAt
	}
	if lastErr != "" {
		out["last_error"] = lastErr
	}
	writeJSON(w, http.StatusOK, out)
}

// ExtChanManager 面板对外部渠道管理器的只读依赖（避免 panel 直接 import 并
// 持有 *extchan.Manager 的写面；接口取观测快照足够）。
type ExtChanManager interface {
	Status() []extchan.Status
	Intel() (*extchan.IntelData, time.Time, string, bool)
	IntelStatus() map[string]any
}
