// checkin.go 平台签到的面板 API（账号中心 v1.6.0 · T3）。
//
// 路由**只在本文件注册**（panel.go 里一行 registerCheckinRoutes）——与
// CodeBuddy 的 /api/accounts/{uid}/checkin 同树共存：平台签到用
// `platform_checkin` 字面段（Go 1.22 mux 字面段优先于通配段，互不干扰）。
//
// 调度器（internal/checkin.Scheduler）自带账号级 TryLock 与环形历史落账；
// 本文件只做 HTTP 编排与错误翻译。全链路脱敏由 Checker 契约保证
// （internal/checkin 包注释）。
package panel

import (
	"errors"
	"net/http"
	"strings"

	"github.com/RisingInsight/startport/internal/checkin"
)

// registerCheckinRoutes 注册平台签到端点（panel.go 在路由装配尾部调用一次）。
func (p *Panel) registerCheckinRoutes() {
	p.mux.HandleFunc("POST /startport/api/accounts/{id}/platform_checkin", p.withAuth(p.platformCheckinOne))
	p.mux.HandleFunc("POST /startport/api/accounts/platform_checkin_all", p.withAuth(p.platformCheckinAll))
}

// platformCheckinOne POST /startport/api/accounts/{id}/platform_checkin
// 手动触发单账号的平台签到（绕过每日小时窗口；TryLock 防与定时双跑）。
func (p *Panel) platformCheckinOne(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Checkin == nil {
		writeErr(w, http.StatusNotImplemented, "平台签到未启用")
		return
	}
	res, err := p.cfg.Checkin.CheckinAccount(r.Context(), r.PathValue("id"))
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": res.Status != checkin.StatusFailed,
			"result": map[string]any{
				"status":  res.Status,
				"reward":  res.Reward,
				"message": res.Message,
			},
		})
	case errors.Is(err, checkin.ErrPlatformNotSupported):
		writeErr(w, http.StatusBadRequest, err.Error())
	case strings.Contains(err.Error(), "不存在"):
		writeErr(w, http.StatusNotFound, err.Error())
	default:
		// 签到失败也回 200 + result（业务失败不是 HTTP 层错误——面板按
		// result.status 标红），只有编排层错误（如账号不存在）才走非 2xx。
		writeErr(w, http.StatusBadGateway, err.Error())
	}
}

// platformCheckinAll POST /startport/api/accounts/platform_checkin_all
// 手动触发全平台签到（串行逐账号；停用账号与无签到平台自动跳过）。
func (p *Panel) platformCheckinAll(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Checkin == nil {
		writeErr(w, http.StatusNotImplemented, "平台签到未启用")
		return
	}
	p.cfg.Checkin.RunAll(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"message": "全平台签到已执行；逐账号结果见账号列表的签到状态",
	})
}
