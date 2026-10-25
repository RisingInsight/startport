// autoclaw_intl.go AutoClaw 国际版 OAuth 登录（阿里云滑块 + 本机回环三跳）。
//
// 上游协议（ref AgentHub autoclaw/login.cjs + 2026-10-20 实测）：
//
//	① POST {intl userapi}/userapi/overseasv1/oauth-captcha-config → scene/region/prefix
//	② POST {acceleration}/userapi/overseasv1/zai-oauth-url
//	   body {source_id, device_id, navigate_uri, ali_captcha_verify_param}
//	   → oauth_url；浏览器登录后回跳 http://localhost:<port>/auth/callback-zai
//	③ POST {acceleration}/userapi/overseasv1/zai-oauth-login
//	   body {source_id, device_id, code, state, navigate_uri} → access/refresh token
//
// 注意 base 分流：国际站 userapi 的 zai-oauth-url 当前返回 405，而加速站能正常
// 返回业务码；captcha-config 则只在国际站返回 enabled=true。
//
// 回调端口必须是官方客户端登记过的四个之一（localhost，不是 127.0.0.1）。
// 滑块参数由面板前端阿里云 SDK 生成，后端只透传，不解析、不记录。
package acclogin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/RisingInsight/startport/internal/netdiag"
	"html"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/RisingInsight/startport/internal/account"
	"github.com/RisingInsight/startport/internal/checkin"
)

type autoClawIntlPaths struct {
	url, login, callback, captcha string
}

func autoClawIntlPathsFor(vendor string) autoClawIntlPaths {
	if strings.EqualFold(strings.TrimSpace(vendor), "google") {
		return autoClawIntlPaths{
			url:      "/userapi/overseasv1/google-oauth-url",
			login:    "/userapi/overseasv1/google-oauth-login",
			callback: "/auth/callback-google",
			captcha:  "/userapi/overseasv1/oauth-captcha-config",
		}
	}
	return autoClawIntlPaths{
		url:      "/userapi/overseasv1/zai-oauth-url",
		login:    "/userapi/overseasv1/zai-oauth-login",
		callback: "/auth/callback-zai",
		captcha:  "/userapi/overseasv1/oauth-captcha-config",
	}
}

// StartOAuth 发起需要风控验证的浏览器 OAuth 登录（当前仅 autoclaw-intl）。
func (m *Manager) StartOAuth(ctx context.Context, platform, captcha, vendor string) (*StartResult, error) {
	if platform != account.PlatformAutoClawIntl {
		return nil, fmt.Errorf("平台 %q 暂不支持 OAuth 登录", platform)
	}
	if strings.TrimSpace(captcha) == "" {
		return nil, fmt.Errorf("请先完成滑块验证")
	}
	return m.startAutoClawIntl(ctx, captcha, vendor)
}

// AutoClawCaptchaConfig 拉取国际版风控配置（前端阿里云 SDK 初始化用）。
func (m *Manager) AutoClawCaptchaConfig(ctx context.Context) (map[string]any, error) {
	paths := autoClawIntlPathsFor("zai")
	payload, err := m.userapiIntlPost(ctx, "", paths.captcha, map[string]any{})
	if err != nil {
		return nil, err
	}
	if code := jsonInt64(payload["code"]); code != 0 {
		return nil, fmt.Errorf("%s", describeAutoClawLoginError(code, jsonString(payload["msg"])))
	}
	data, _ := payload["data"].(map[string]any)
	return map[string]any{
		"enabled":  truthyAutoClawLogin(data["enabled"]),
		"region":   jsonString(data["region"]),
		"prefix":   jsonString(data["prefix"]),
		"scene_id": jsonString(data["scene_id"]),
		"supplier": jsonString(data["captcha_supplier"]),
	}, nil
}

func (m *Manager) startAutoClawIntl(ctx context.Context, captcha, vendor string) (*StartResult, error) {
	if strings.EqualFold(strings.TrimSpace(vendor), "") {
		vendor = "zai"
	}
	vendor = strings.ToLower(strings.TrimSpace(vendor))
	if vendor != "google" {
		vendor = "zai"
	}
	paths := autoClawIntlPathsFor(vendor)

	var ln net.Listener
	var port int
	for _, p := range m.cfg.AutoClawIntlPorts {
		l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
		if err == nil {
			ln, port = l, p
			break
		}
	}
	if ln == nil {
		return nil, fmt.Errorf("AutoClaw 国际版回调端口全部被占用（可能客户端正在登录），请关闭后重试")
	}

	s := &Session{
		ID:        newSessionID(),
		Platform:  account.PlatformAutoClawIntl,
		CreatedAt: m.cfg.Now(),
		ExpiresAt: m.cfg.Now().Add(m.cfg.TTL),
		status:    StatusPending,
		deviceID:  newDeviceID(),
		vendor:    vendor,
		done:      make(chan struct{}),
	}
	s.navigateURI = fmt.Sprintf("http://localhost:%d%s", port, paths.callback)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.handleAutoClawIntlCallback(s, paths, w, r)
	})}
	s.httpSrv = srv
	go func() { _ = srv.Serve(ln) }()

	if err := m.registerSession(s); err != nil {
		s.closeHTTP()
		return nil, err
	}

	payload, err := m.oauthPost(ctx, "", paths.url, map[string]any{
		"source_id":                "autoclaw",
		"device_id":                s.deviceID,
		"navigate_uri":             s.navigateURI,
		"ali_captcha_verify_param": strings.TrimSpace(captcha),
	})
	if err != nil {
		m.dropSession(s.ID)
		return nil, err
	}
	if code := jsonInt64(payload["code"]); code != 0 {
		// 业务码落日志（630014=风控校验未过 等）：前端只看得到文案，
		// 排查"滑块一直失败"必须知道上游真实业务码。captcha 参数本身不落日志。
		m.cfg.Logf("[acclogin:autoclaw-intl] oauth-url 业务码 %d: %s", code, jsonString(payload["msg"]))
		m.dropSession(s.ID)
		return nil, fmt.Errorf("%s", describeAutoClawLoginError(code, jsonString(payload["msg"])))
	}
	data, _ := payload["data"].(map[string]any)
	oauthURL := strings.TrimSpace(jsonString(data["oauth_url"]))
	if oauthURL == "" {
		m.dropSession(s.ID)
		return nil, fmt.Errorf("上游未返回授权地址，请重试")
	}
	s.mu.Lock()
	s.loginURL = oauthURL
	s.mu.Unlock()
	m.cfg.Logf("[acclogin:autoclaw-intl] OAuth 会话已创建（%s，port %d）", vendor, port)
	return &StartResult{
		SessionID: s.ID,
		Mode:      "auth_url",
		LoginURL:  oauthURL,
		ExpiresAt: s.ExpiresAt,
	}, nil
}

func (m *Manager) handleAutoClawIntlCallback(s *Session, paths autoClawIntlPaths, w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != paths.callback {
		http.NotFound(w, r)
		return
	}
	s.mu.Lock()
	ready := s.loginURL != ""
	status := s.status
	s.mu.Unlock()
	if !ready {
		writeAutoClawLoginPage(w, "正在初始化", "授权页尚未就绪，请回到面板重试。", false)
		return
	}
	if status != StatusPending {
		writeAutoClawLoginPage(w, "登录会话已结束", "请回到面板重新发起登录。", false)
		return
	}
	q := r.URL.Query()
	msg := strings.TrimSpace(q.Get("error_description"))
	if msg == "" {
		msg = strings.TrimSpace(q.Get("error"))
	}
	if msg != "" {
		msg = "授权失败：" + msg
		s.finish(StatusFailed, msg, "")
		writeAutoClawLoginPage(w, "登录失败", msg, false)
		time.AfterFunc(2*time.Second, s.closeHTTP)
		return
	}
	code := strings.TrimSpace(q.Get("code"))
	if code == "" {
		writeAutoClawLoginPage(w, "等待授权完成", "请在浏览器完成登录；本页可保持打开。", true)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	id, err := m.exchangeAutoClawIntl(ctx, s, paths, code, strings.TrimSpace(q.Get("state")))
	if err != nil {
		s.finish(StatusFailed, err.Error(), "")
		writeAutoClawLoginPage(w, "登录失败", err.Error(), false)
	} else {
		s.finish(StatusSuccess, "", id)
		writeAutoClawLoginPage(w, "登录成功", "账号已加入 AutoClaw 账号池，可关闭本页。", true)
	}
	time.AfterFunc(2*time.Second, s.closeHTTP)
}

func (m *Manager) exchangeAutoClawIntl(ctx context.Context, s *Session, paths autoClawIntlPaths, code, state string) (string, error) {
	payload, err := m.oauthPost(ctx, "", paths.login, map[string]any{
		"source_id":    "autoclaw",
		"device_id":    s.deviceID,
		"code":         code,
		"state":        state,
		"navigate_uri": s.navigateURI,
	})
	if err != nil {
		return "", err
	}
	if c := jsonInt64(payload["code"]); c != 0 {
		return "", fmt.Errorf("%s", describeAutoClawLoginError(c, jsonString(payload["msg"])))
	}
	data, _ := payload["data"].(map[string]any)
	token := strings.TrimSpace(jsonString(data["access_token"]))
	if token == "" {
		return "", fmt.Errorf("换码成功但上游未返回 access_token")
	}
	refresh := strings.TrimSpace(jsonString(data["refresh_token"]))
	userID := strings.TrimSpace(jsonString(data["user_id"]))
	userName := strings.TrimSpace(jsonString(data["user_name"]))
	email := strings.TrimSpace(jsonString(data["email"]))
	if email == "" {
		email = m.autoClawIntlEmail(ctx, token, s.deviceID)
	}
	label := userName
	if label == "" {
		label = email
	}
	if label == "" {
		label = userID
	}
	if label == "" {
		label = "AutoClaw 国际"
	}
	blob := map[string]any{"token": token, "device_id": s.deviceID}
	if refresh != "" {
		blob["refresh_token"] = refresh
	}
	id, perr := m.poolAccount(s, blob, label, "OAuth 登录")
	if perr != nil {
		return "", fmt.Errorf("凭证已获取但入池失败: %w", perr)
	}
	m.cfg.Logf("[acclogin:autoclaw-intl] OAuth 登录成功并入池（%s）", label)
	return id, nil
}

func (m *Manager) autoClawIntlEmail(ctx context.Context, token, deviceID string) string {
	payload, err := m.userapiIntlPost(ctx, token, "/userapi/v1/user-profile", map[string]any{
		"source_id": "autotyper",
		"device_id": deviceID,
	})
	if err != nil || jsonInt64(payload["code"]) != 0 {
		return ""
	}
	data, _ := payload["data"].(map[string]any)
	return strings.TrimSpace(jsonString(data["email"]))
}

func (m *Manager) userapiIntlPost(ctx context.Context, token, path string, body map[string]any) (map[string]any, error) {
	return m.userapiPostBase(ctx, m.cfg.AutoClawIntlUserAPI, token, path, body)
}

// oauthPost OAuth 三跳专用：上游实际在加速站（国际站 overseasv1 = 405）。
func (m *Manager) oauthPost(ctx context.Context, token, path string, body map[string]any) (map[string]any, error) {
	return m.userapiPostBase(ctx, m.cfg.AutoClawOAuthAPI, token, path, body)
}

func (m *Manager) userapiPostBase(ctx context.Context, base, token, path string, body map[string]any) (map[string]any, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("序列化请求体失败")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(base, "/")+path, bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("构造请求失败")
	}
	for _, h := range checkin.AutoClawSignedHeaders(token, m.cfg.Now()) {
		req.Header.Set(h[0], h[1])
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := m.cfg.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求失败（网络）%s，请检查网络后重试", netdiag.ProxyHint(err))
	}
	defer resp.Body.Close()
	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("上游响应形状不符（HTTP %d）", resp.StatusCode)
	}
	if resp.StatusCode >= 400 && jsonInt64(payload["code"]) == 0 {
		return payload, fmt.Errorf("上游 HTTP %d", resp.StatusCode)
	}
	return payload, nil
}

func writeAutoClawLoginPage(w http.ResponseWriter, title, detail string, ok bool) {
	color := "#16a34a"
	if !ok {
		color = "#dc2626"
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprintf(w, `<!doctype html><html lang="zh-CN"><meta charset="utf-8"><title>%s</title>
<body style="margin:0;font:16px/1.6 system-ui,sans-serif;background:#0b1020;color:#e5e7eb;display:flex;min-height:100vh;align-items:center;justify-content:center">
<div style="max-width:520px;padding:28px 32px;border:1px solid #1f2937;border-radius:14px;background:#111827">
<h2 style="margin:0 0 10px;color:%s">%s</h2><p style="margin:0;color:#9ca3af">%s</p></div></body></html>`,
		html.EscapeString(title), color, html.EscapeString(title), html.EscapeString(detail))
}
