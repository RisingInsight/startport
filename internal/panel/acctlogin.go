// acctlogin.go 登录式添加的面板 API（v1.7.0；规格 §3.1 契约，**已冻结**——
// 前端 acct.js 登录页签并行开发按此对接）。
//
// 路由（本文件注册，panel.go 一行 registerLoginRoutes；字面段 `login` 优先于
// `{uid}` 通配，与 CodeBuddy 账号路由同树共存；与 CodeBuddy 的 /api/login/*
// 前缀不同树，互不干扰）：
//
//	POST /startport/api/accounts/login/start   {platform, region?, phone?}
//	  → {session_id, mode: "auth_url"|"sms", login_url?, phone_sent?, expires_at}
//	POST /startport/api/accounts/login/wait    {session_id} → {status: pending|
//	  success|failed|expired + account_id?/error?}（长轮询 ≤25s；会话不存在 404）
//	POST /startport/api/accounts/login/cancel  {session_id} → 200/404
//	POST /startport/api/accounts/login/sms/send   {session_id} → 重发验证码（60s 频控）
//	POST /startport/api/accounts/login/sms/verify {session_id, code} → 200（凭证已入池）
//
// 每个操作另有**路径参数等价形态**（acct.js 采用的 `{sid}/...`）：
//
//	GET  /startport/api/accounts/login/{sid}/wait
//	POST /startport/api/accounts/login/{sid}/cancel
//	POST /startport/api/accounts/login/{sid}/sms_verify   {code}
//
// 另有 `GET /startport/api/accounts/login/capabilities` 下发支持平台清单
// （规格 §3.4：前端不硬编码）。入池走既有 Accounts.Add + 投影（Manager.AfterPool
// → acctchan.Sync），不新开写路径。全链路脱敏：手机号前 3 后 4；token/verifier
// 不回显不落日志。
package panel

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/RisingInsight/startport/internal/acclogin"
)

// registerLoginRoutes 注册登录式添加端点（panel.go 调用一次）。
func (p *Panel) registerLoginRoutes() {
	p.mux.HandleFunc("POST /startport/api/accounts/login/start", p.withAuth(p.platformLoginStart))
	// wait / cancel / sms_verify：body 形态与路径参数形态等价（两态测试双覆盖）。
	p.mux.HandleFunc("POST /startport/api/accounts/login/wait", p.withAuth(p.platformLoginWait))
	p.mux.HandleFunc("GET /startport/api/accounts/login/{sid}/wait", p.withAuth(p.platformLoginWaitPath))
	p.mux.HandleFunc("POST /startport/api/accounts/login/cancel", p.withAuth(p.platformLoginCancel))
	p.mux.HandleFunc("POST /startport/api/accounts/login/{sid}/cancel", p.withAuth(p.platformLoginCancelPath))
	p.mux.HandleFunc("POST /startport/api/accounts/login/sms/send", p.withAuth(p.platformLoginSMSSend))
	p.mux.HandleFunc("POST /startport/api/accounts/login/sms/verify", p.withAuth(p.platformLoginSMSVerify))
	p.mux.HandleFunc("POST /startport/api/accounts/login/{sid}/sms_verify", p.withAuth(p.platformLoginSMSVerifyPath))
	p.mux.HandleFunc("GET /startport/api/accounts/login/capabilities", p.withAuth(p.platformLoginCapabilities))
}

// loginReady Manager 是否已注入。
func (p *Panel) loginReady() bool { return p.cfg.Login != nil }

// platformLoginStart POST /startport/api/accounts/login/start
func (p *Panel) platformLoginStart(w http.ResponseWriter, r *http.Request) {
	if !p.loginReady() {
		writeErr(w, http.StatusNotImplemented, "登录式添加未启用")
		return
	}
	var body struct {
		Platform string `json:"platform"`
		Region   string `json:"region"`
		Phone    string `json:"phone"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "解析请求体失败: "+err.Error())
		return
	}
	res, err := p.cfg.Login.Start(r.Context(), strings.TrimSpace(body.Platform),
		strings.TrimSpace(body.Region), strings.TrimSpace(body.Phone))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	out := map[string]any{
		"session_id": res.SessionID,
		"mode":       res.Mode,
		"expires_at": res.ExpiresAt.UTC().Format(timeRFC3339),
	}
	if res.LoginURL != "" {
		out["login_url"] = res.LoginURL
	}
	if res.PhoneSent {
		out["phone_sent"] = true
	}
	writeJSON(w, http.StatusOK, out)
}

// platformLoginWait POST /startport/api/accounts/login/wait（长轮询 ≤25s；body 形态）
func (p *Panel) platformLoginWait(w http.ResponseWriter, r *http.Request) {
	var body struct {
		SessionID string `json:"session_id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "解析请求体失败: "+err.Error())
		return
	}
	p.waitByID(w, strings.TrimSpace(body.SessionID))
}

// platformLoginWaitPath GET /startport/api/accounts/login/{sid}/wait（路径形态）
func (p *Panel) platformLoginWaitPath(w http.ResponseWriter, r *http.Request) {
	p.waitByID(w, r.PathValue("sid"))
}

// waitByID wait 的共用实现（会话不存在 → 404，规格 §3.2 取消语义）。
func (p *Panel) waitByID(w http.ResponseWriter, sessionID string) {
	if !p.loginReady() {
		writeErr(w, http.StatusNotImplemented, "登录式添加未启用")
		return
	}
	st, err := p.cfg.Login.Wait(strings.TrimSpace(sessionID))
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// platformLoginCancel POST /startport/api/accounts/login/cancel（body 形态）
func (p *Panel) platformLoginCancel(w http.ResponseWriter, r *http.Request) {
	var body struct {
		SessionID string `json:"session_id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "解析请求体失败: "+err.Error())
		return
	}
	p.cancelByID(w, strings.TrimSpace(body.SessionID))
}

// platformLoginCancelPath POST /startport/api/accounts/login/{sid}/cancel（路径形态）
func (p *Panel) platformLoginCancelPath(w http.ResponseWriter, r *http.Request) {
	p.cancelByID(w, r.PathValue("sid"))
}

// cancelByID cancel 的共用实现。
func (p *Panel) cancelByID(w http.ResponseWriter, sessionID string) {
	if !p.loginReady() {
		writeErr(w, http.StatusNotImplemented, "登录式添加未启用")
		return
	}
	if err := p.cfg.Login.Cancel(strings.TrimSpace(sessionID)); err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// platformLoginSMSSend POST /startport/api/accounts/login/sms/send（重发验证码）
func (p *Panel) platformLoginSMSSend(w http.ResponseWriter, r *http.Request) {
	if !p.loginReady() {
		writeErr(w, http.StatusNotImplemented, "登录式添加未启用")
		return
	}
	var body struct {
		SessionID string `json:"session_id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "解析请求体失败: "+err.Error())
		return
	}
	if err := p.cfg.Login.ResendSMS(r.Context(), strings.TrimSpace(body.SessionID)); err != nil {
		// 频控（本地 60s / 上游 630101）与其它失败都按 400 + 原文——前端
		// 倒计时逻辑按文案里的"秒"提示即可。
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "phone_sent": true})
}

// platformLoginSMSVerify POST /startport/api/accounts/login/sms/verify（body 形态）
func (p *Panel) platformLoginSMSVerify(w http.ResponseWriter, r *http.Request) {
	var body struct {
		SessionID string   `json:"session_id"`
		Code      flexCode `json:"code"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "解析请求体失败: "+err.Error())
		return
	}
	p.verifyByID(w, r.Context(), strings.TrimSpace(body.SessionID), body.Code)
}

// platformLoginSMSVerifyPath POST /startport/api/accounts/login/{sid}/sms_verify（路径形态）
func (p *Panel) platformLoginSMSVerifyPath(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Code flexCode `json:"code"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "解析请求体失败: "+err.Error())
		return
	}
	p.verifyByID(w, r.Context(), r.PathValue("sid"), body.Code)
}

// flexCode 验证码字段的**数字/字符串双兼容**形态（v1.7.0 冒烟修复）：
// 冻结契约规定前端传 JSON 数字（`parseInt(code,10)` 后的 number），早期/
// 手工调用可能传字符串——此处统一接下，合法性（6 位数字）由 verifyByID 的
// digits() 判定。UnmarshalJSON 永不返回 error（畸形形态交给 digits 报
// "验证码格式错误"，而不是笼统的"解析请求体失败"）。
type flexCode struct {
	raw string
	ok  bool
}

// UnmarshalJSON 实现 json.Unmarshaler：数字 → 取整字面量；字符串 → 去引号；
// 其余（null/对象/数组/浮点）→ ok=false。
func (f *flexCode) UnmarshalJSON(b []byte) error {
	trimmed := strings.TrimSpace(string(b))
	if trimmed == "null" || trimmed == "" {
		return nil // 保持零值（ok=false）——空码由 digits() 报格式错误
	}
	if len(trimmed) >= 2 && trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return nil // 引号内非法转义：保持 ok=false
		}
		f.raw = strings.TrimSpace(s)
		f.ok = allDigitsJSON(f.raw)
		return nil
	}
	// JSON 数字：必须是整数字面量（浮点/负数 → 格式错误）。
	if allDigitsJSON(trimmed) {
		f.raw = trimmed
		f.ok = true
		return nil
	}
	f.ok = false // 浮点（"1.5"）/负数等数字形态
	return nil
}

// digits 取归一后的 6 位数字验证码（不合法 → false）。
func (f *flexCode) digits() (string, bool) {
	if !f.ok || !allDigitsJSON(f.raw) {
		return "", false
	}
	return f.raw, true
}

// allDigitsJSON 非空全数字判定。
func allDigitsJSON(s string) bool {
	if s == "" {
		return false
	}
	for _, ch := range s {
		if ch < '0' || ch > '9' {
			return false
		}
	}
	return true
}

// verifyByID verify 的共用实现（验证码错误/频控/参数错误：400 + 人话；成功已入池回 200）。
func (p *Panel) verifyByID(w http.ResponseWriter, ctx context.Context, sessionID string, code flexCode) {
	if !p.loginReady() {
		writeErr(w, http.StatusNotImplemented, "登录式添加未启用")
		return
	}
	codeStr, ok := code.digits()
	if !ok {
		writeErr(w, http.StatusBadRequest, "验证码格式错误：请填写 6 位数字验证码")
		return
	}
	st, err := p.cfg.Login.VerifySMS(ctx, strings.TrimSpace(sessionID), codeStr)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// platformLoginCapabilities GET /startport/api/accounts/login/capabilities
// 能力开关下发（规格 §3.4：前端不硬编码支持平台——acct.js 的
// ACCT_BROWSER_PLATFORMS 常量可用本端点替换）。
func (p *Panel) platformLoginCapabilities(w http.ResponseWriter, r *http.Request) {
	if !p.loginReady() {
		writeErr(w, http.StatusNotImplemented, "登录式添加未启用")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"platforms": acclogin.SupportedPlatforms()})
}
