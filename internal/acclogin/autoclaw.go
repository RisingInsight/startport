// autoclaw.go AutoClaw 国内版手机验证码登录（v1.7.0；规格 §2.2，
// 逐字依据 ref/autoclaw/login.rs）。
//
// 三颗踩坑钉子（规格逐条照抄，别改回去）：
//  1. **code 必须是 JSON 数字**——字符串会得到 400001 参数错误而不是验证码
//     错误（login.rs:157-181）；
//  2. **device_id 两跳一致**——32 随机字节 → 64 位十六进制，由会话持有
//     （login.rs:183-207；缺它上游回 400001）；
//  3. **签名头复用 checkin.AutoClawSignedHeaders**——签名常量只允许一份实现，
//     第二份就是稳定 400002（架构纪律）。
package acclogin

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/RisingInsight/startport/internal/account"
	"github.com/RisingInsight/startport/internal/checkin"
)

const (
	autoclawSendCodePath = "/userapi/v1/agent-send-code"
	autoclawLoginPath    = "/userapi/v1/agent-login/" // **结尾斜杠是上游要求**（login.rs:108-109）
)

// startAutoClaw 发起短信登录会话（建会话 + 发码；60s 频控，规格 R3）。
func (m *Manager) startAutoClaw(ctx context.Context, phone string) (*StartResult, error) {
	normalized, err := normalizePhone(phone)
	if err != nil {
		return nil, err
	}
	deviceID := newDeviceID()
	s := &Session{
		ID:        newSessionID(),
		Platform:  account.PlatformAutoClaw,
		CreatedAt: m.cfg.Now(),
		ExpiresAt: m.cfg.Now().Add(m.cfg.TTL),
		status:    StatusPending,
		phone:     normalized,
		deviceID:  deviceID,
		done:      make(chan struct{}),
	}
	if err := m.registerSession(s); err != nil {
		return nil, err
	}
	// 发码（频控在 gateSMS；上游 630101 原样透出）。
	if err := m.sendSMSCode(ctx, s); err != nil {
		m.dropSession(s.ID)
		return nil, err
	}
	return &StartResult{
		SessionID: s.ID,
		Mode:      "sms",
		PhoneSent: true,
		ExpiresAt: s.ExpiresAt,
	}, nil
}

// ResendSMS 重发验证码（60s 频控；R3：网关侧无节制重发会放大上游频控）。
func (m *Manager) ResendSMS(ctx context.Context, sessionID string) error {
	s := m.getSession(sessionID)
	if s == nil {
		return fmt.Errorf("登录会话不存在（已取消、被顶替或已超时清理）")
	}
	if s.Platform != account.PlatformAutoClaw {
		return fmt.Errorf("该会话不是短信登录会话")
	}
	return m.sendSMSCode(ctx, s)
}

// gateSMS (device_id, phone) 60s 频控闸（R3 默认决策：后端每对 60s 内只放行
// 一次发码；上游 630101 原样透出作兜底）。
func (m *Manager) gateSMS(deviceID, phone string) error {
	key := deviceID + "|" + phone
	m.mu.Lock()
	defer m.mu.Unlock()
	if last, ok := m.smsLast[key]; ok {
		if elapsed := m.cfg.Now().Sub(last); elapsed < smsResendCooldown {
			return fmt.Errorf("获取验证码过于频繁，请 %d 秒后再试",
				int((smsResendCooldown-elapsed).Seconds())+1)
		}
	}
	m.smsLast[key] = m.cfg.Now()
	return nil
}

// sendSMSCode 发送验证码（ref login.rs:299-340）。
func (m *Manager) sendSMSCode(ctx context.Context, s *Session) error {
	if err := m.gateSMS(s.deviceID, s.phone); err != nil {
		return err
	}
	payload, err := m.userapiPost(ctx, "", autoclawSendCodePath, map[string]any{
		"phone":     s.phone,
		"source_id": "autoclaw",
		"device_id": s.deviceID,
	})
	if err != nil {
		return err
	}
	if code := jsonInt64(payload["code"]); code != 0 {
		return fmt.Errorf("%s", describeAutoClawLoginError(code, jsonString(payload["msg"])))
	}
	// 成功判定：code==0 **且** data.result==true（login.rs:316-332）。
	data, _ := payload["data"].(map[string]any)
	if !truthyAutoClawLogin(data["result"]) {
		return fmt.Errorf("验证码发送失败，请稍后重试")
	}
	m.cfg.Logf("[acclogin:autoclaw] 验证码已发送（%s）", maskPhone(s.phone))
	return nil
}

// VerifySMS 验证码验证 + 登录入池（ref login.rs:344-418）。
func (m *Manager) VerifySMS(ctx context.Context, sessionID, code string) (WaitStatus, error) {
	s := m.getSession(sessionID)
	if s == nil {
		return WaitStatus{}, fmt.Errorf("登录会话不存在（已取消、被顶替或已超时清理）")
	}
	if s.Platform != account.PlatformAutoClaw {
		return WaitStatus{}, fmt.Errorf("该会话不是短信登录会话")
	}
	numeric, err := normalizeSMSCode(code)
	if err != nil {
		return WaitStatus{}, err
	}
	// code 是 **JSON 数字**（login.rs:157-181 的钉子）——goes as number via map.
	payload, err := m.userapiPost(ctx, "", autoclawLoginPath, map[string]any{
		"phone":     s.phone,
		"code":      numeric,
		"platform":  "web",
		"source_id": "autoclaw",
		"device_id": s.deviceID,
	})
	if err != nil {
		return WaitStatus{}, err
	}
	if code := jsonInt64(payload["code"]); code != 0 {
		msg := describeAutoClawLoginError(code, jsonString(payload["msg"]))
		return WaitStatus{Status: StatusFailed, Error: msg},
			fmt.Errorf("%s", msg)
	}
	data, _ := payload["data"].(map[string]any)
	token := strings.TrimSpace(jsonString(data["access_token"]))
	if token == "" {
		return WaitStatus{Status: StatusFailed, Error: "登录成功但上游没有返回 access_token"},
			fmt.Errorf("登录成功但上游没有返回 access_token")
	}
	refreshToken := strings.TrimSpace(jsonString(data["refresh_token"]))
	// blob 按 validate.go schema（{token, refresh_token?, device_id}）。
	blob := map[string]any{"token": token, "device_id": s.deviceID}
	if refreshToken != "" {
		blob["refresh_token"] = refreshToken
	}
	label := maskPhone(s.phone) // label 兜底：脱敏手机号（login.rs:414-418）
	id, perr := m.poolAccount(s, blob, label, "短信登录")
	if perr != nil {
		return WaitStatus{Status: StatusFailed, Error: "凭证已获取但入池失败: " + perr.Error()},
			fmt.Errorf("凭证已获取但入池失败: %w", perr)
	}
	s.finish(StatusSuccess, "", id) // 保留在表里供 Wait 观察 success + account_id
	m.cfg.Logf("[acclogin:autoclaw] 短信登录成功并入池（%s）", label)
	return WaitStatus{Status: StatusSuccess, AccountID: id}, nil
}

// userapiPost userapi 域带签名 POST（匿名形态 token=""——不带 authorization；
// 签名复用 checkin.AutoClawSignedHeaders，只此一份实现）。
func (m *Manager) userapiPost(ctx context.Context, token, path string, body map[string]any) (map[string]any, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("序列化请求体失败")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(m.cfg.AutoClawUserAPI, "/")+path, bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("构造请求失败")
	}
	for _, h := range checkin.AutoClawSignedHeaders(token, m.cfg.Now()) {
		req.Header.Set(h[0], h[1])
	}
	resp, err := m.cfg.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求失败（网络），请检查网络后重试")
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("登录态已失效，请重新登录")
	}
	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("上游响应形状不符（平台接口可能已变更）")
	}
	return payload, nil
}

// normalizePhone 中国大陆手机号归一化（ref login.rs:136-153 逐字等价）：
// 容忍 +86/86 前缀与空格/连字符，结果必须 `1[2-9]` 开头的 11 位数字。
func normalizePhone(raw string) (string, error) {
	trimmed := strings.Map(func(r rune) rune {
		if r == ' ' || r == '-' || r == '\t' {
			return -1
		}
		return r
	}, raw)
	digits := strings.TrimPrefix(trimmed, "+86")
	if d2 := strings.TrimPrefix(digits, "86"); len(d2) == 11 {
		digits = d2
	}
	if len(digits) == 11 && strings.HasPrefix(digits, "1") &&
		digits[1] >= '2' && digits[1] <= '9' && allASCIIDigits(digits) {
		return digits, nil
	}
	return "", fmt.Errorf("请填写 11 位中国大陆手机号")
}

// normalizeSMSCode 6 位数字验证码 → **int**（JSON 数字的钉子；
// ref login.rs:157-181：字符串形态上游回 400001 参数错误而不是验证码错误）。
func normalizeSMSCode(raw string) (int, error) {
	trimmed := strings.TrimSpace(raw)
	if len(trimmed) == 6 && allASCIIDigits(trimmed) {
		n := 0
		for _, ch := range trimmed {
			n = n*10 + int(ch-'0')
		}
		return n, nil
	}
	return 0, fmt.Errorf("请填写 6 位数字验证码")
}

// describeAutoClawLoginError 业务码 → 人话（规格 §2.2 码表；
// 631002 的原文「版本已停止服务」是误导——真实原因是缺风控验证）。
func describeAutoClawLoginError(code int64, upstreamMsg string) string {
	switch code {
	case 400001:
		return "请求参数有误，请检查手机号与验证码格式后重试"
	case 630202:
		return "验证码不正确，请检查后重试"
	case 630101:
		return "获取验证码过于频繁，请稍后再试"
	case 400002:
		return "校准系统时间后重试"
	case 410000, 400000:
		return "登录态已失效，请重新登录"
	case 631002:
		return "上游要求过风控验证，本次登录无法在此完成；请改用填写凭证"
	case 630014:
		return "风控验证未通过，请稍后重试"
	}
	if upstreamMsg != "" {
		return "登录失败：" + upstreamMsg
	}
	return fmt.Sprintf("登录失败（业务码 %d）", code)
}

// newDeviceID 32 随机字节 → 64 位十六进制（ref login.rs:183-207；
// 两跳必须一致——由 Session 持有）。
func newDeviceID() string {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%064x", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}

func allASCIIDigits(s string) bool {
	for _, ch := range s {
		if ch < '0' || ch > '9' {
			return false
		}
	}
	return len(s) > 0
}

// jsonInt64 / jsonString 宽松取值助手（与 checkin 包同语义，本包独立一份
// 以避免 acclogin → checkin 的实现耦合——只共享签名头这一份契约）。
func jsonInt64(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	case int:
		return int64(n)
	}
	return 0
}

func jsonString(v any) string {
	if s, ok := v.(string); ok {
		return strings.TrimSpace(s)
	}
	return ""
}

func truthyAutoClawLogin(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case float64:
		return x != 0
	case string:
		return x != ""
	}
	return false
}
