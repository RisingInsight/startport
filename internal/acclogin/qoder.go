// qoder.go Qoder 双区 PKCE 设备授权（v1.7.0；规格 §2.1，
// 逐字依据 ref/qoder/oauth.rs:24-83 + endpoints.rs:87-157 + machine.rs:13-65）。
//
// 流程：授权 URL（challenge/nonce/machine_id，verifier 不出网关）→ 用户浏览器
// 完成 → 网关轮询 `GET {openapi}/api/v1/deviceToken/poll?nonce&verifier&…`
// （202/404 = 未完成）→ token + userinfo 补全 → blob → 入池。
package acclogin

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/RisingInsight/startport/internal/account"
)

// qoder 轮询/用户信息请求头（ref endpoints.rs:231-241 的 open_api_headers）。
func qoderOpenAPIHeaders(token string) [][2]string {
	headers := [][2]string{
		{"Cosy-Version", "1.0.1"},
		{"Cosy-ClientType", "5"},
		{"User-Agent", "qoder-local-proxy"},
	}
	if token != "" {
		headers = append(headers, [2]string{"Authorization", "Bearer " + token})
	}
	return headers
}

// startQoder 发起 PKCE 设备授权会话（ref oauth.rs:24-47）。
func (m *Manager) startQoder(ctx context.Context, platform string) (*StartResult, error) {
	intl := platform == account.PlatformQoderIntl
	machineID, err := m.machineID()
	if err != nil {
		return nil, fmt.Errorf("生成 qoder 设备标识失败: %w", err)
	}
	verifier := randomURLSafe(32) // 32 随机字节 → base64url 无填充
	challenge := base64.RawURLEncoding.EncodeToString([]byte(hashSHA256(verifier)))
	nonce := newUUIDv4NoDash()
	web := strings.TrimRight(m.cfg.QoderWeb[intl], "/")
	openAPI := strings.TrimRight(m.cfg.QoderOpenAPI[intl], "/")
	loginURL := fmt.Sprintf("%s/device/selectAccounts?challenge=%s&challenge_method=S256&machine_id=%s&nonce=%s",
		web, challenge, machineID, nonce)

	s := &Session{
		ID:        newSessionID(),
		Platform:  platform,
		CreatedAt: m.cfg.Now(),
		ExpiresAt: m.cfg.Now().Add(m.cfg.TTL),
		status:    StatusPending,
		loginURL:  loginURL,
		done:      make(chan struct{}),
	}
	if err := m.registerSession(s); err != nil {
		return nil, err
	}
	// 后台轮询任务（材料只在本 goroutine 与会话内——不出网关）。
	// ⚠ 生命周期（v1.7.0 发布修复）：**不得使用 Start 的请求 ctx**——start 响应
	// 一返回请求 ctx 即取消，轮询协程从未发出过请求（冒烟实锤：pending 永挂 +
	// mock 0 次命中）。改用独立生命周期：TTL+30s 兜底超时，显式 cancel 挂在
	// Session 上——四条收尾路径（用户取消 / TTL 清扫 / 同平台顶替 / Close）
	// 经 Session.cancel() 统一停掉轮询协程，不留泄漏。
	pollURL := fmt.Sprintf("%s/api/v1/deviceToken/poll?nonce=%s&verifier=%s&challenge_method=S256",
		openAPI, nonce, verifier)
	pollCtx, cancelPoll := context.WithTimeout(context.Background(), m.cfg.TTL+30*time.Second)
	s.mu.Lock()
	s.stopPoll = cancelPoll
	s.mu.Unlock()
	go m.pollQoder(pollCtx, s, pollURL, machineID)
	return &StartResult{
		SessionID: s.ID,
		Mode:      "auth_url",
		LoginURL:  loginURL,
		ExpiresAt: s.ExpiresAt,
	}, nil
}

// pollQoder 轮询任务（2 秒/次直至终态；202/404 继续；规格 §2.1-4）。
func (m *Manager) pollQoder(ctx context.Context, s *Session, pollURL, machineID string) {
	intl := s.Platform == account.PlatformQoderIntl
	for {
		m.mu.Lock()
		gone := m.sess[s.ID] != s
		m.mu.Unlock()
		if gone {
			return // 会话已被取消/顶替/清理
		}
		if m.cfg.Now().After(s.ExpiresAt) {
			s.finish(StatusExpired, "登录会话已超时，请重新发起", "") // 保留在表里供 Wait 观察
			return
		}
		blob, err := m.pollOnce(ctx, intl, pollURL, machineID)
		if err == nil {
			// 成功：入池（Store.Add 校验 + AfterPool 投影）。
			label := blobLabel(blob, "qoder")
			id, perr := m.poolAccount(s, blob, label, "设备授权登录")
			if perr != nil {
				s.finish(StatusFailed, "凭证已获取但入池失败: "+perr.Error(), "")
				return
			}
			s.finish(StatusSuccess, "", id)
			m.cfg.Logf("[acclogin:%s] 登录成功并入池（%s）", s.Platform, label)
			return
		}
		if !err.retryable {
			s.finish(StatusFailed, err.Message, "")
			return
		}
		// 可重试（202/404/429/网络）：等下一个节拍或被取消/超时。
		timer := time.NewTimer(m.cfg.PollEvery)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-s.done:
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// pollError 轮询失败（retryable = 继续轮；否则落 failed 终态）。
type pollError struct {
	Message   string
	retryable bool
}

// pollOnce 一次轮询 + 成功时的 userinfo 补全（ref oauth.rs:49-83）。
func (m *Manager) pollOnce(ctx context.Context, intl bool, pollURL, machineID string) (map[string]any, *pollError) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pollURL, nil)
	if err != nil {
		return nil, &pollError{Message: "构造轮询请求失败"}
	}
	for _, h := range qoderOpenAPIHeaders("") {
		req.Header.Set(h[0], h[1])
	}
	resp, err := m.cfg.Client.Do(req)
	if err != nil {
		return nil, &pollError{Message: "轮询请求失败（网络），请检查网络后重试", retryable: true}
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusAccepted, resp.StatusCode == http.StatusNotFound:
		return nil, &pollError{Message: "等待授权完成", retryable: true} // 202/404 = 未完成
	case resp.StatusCode == http.StatusTooManyRequests:
		return nil, &pollError{Message: "轮询被限流，稍后自动重试", retryable: true}
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return nil, &pollError{Message: "凭证无效或地区选择错误，请确认后重试"}
	case resp.StatusCode >= 400:
		return nil, &pollError{Message: fmt.Sprintf("轮询返回 HTTP %d", resp.StatusCode), retryable: true}
	}
	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, &pollError{Message: "上游响应畸形（不是 JSON）"}
	}
	token := jsonString(payload["token"])
	if token == "" {
		return nil, &pollError{Message: "Qoder 设备授权响应缺少 token"}
	}
	refreshToken := jsonString(payload["refresh_token"])
	if strings.Contains(refreshToken, "|") {
		return nil, &pollError{Message: "Qoder 设备授权响应的 refresh_token 格式无效"}
	}
	userID := jsonString(payload["user_id"])
	// userinfo 补全（失败但已有 user_id 时不阻塞——ref oauth.rs:76-80）。
	nickname, email := "", ""
	if profile := m.fetchProfile(ctx, intl, token); profile != nil {
		if id := jsonString(profile["id"]); id != "" && userID == "" {
			userID = id
		}
		if id := jsonString(profile["userId"]); id != "" && userID == "" {
			userID = id
		}
		nickname = jsonString(profile["name"])
		if nickname == "" {
			nickname = jsonString(profile["username"])
		}
		email = jsonString(profile["email"])
	}
	if userID == "" {
		return nil, &pollError{Message: "Qoder 设备授权未能取得用户标识（user_id）"}
	}
	mode := "cn"
	if intl {
		mode = "global"
	}
	blob := map[string]any{
		"access_token":  token,
		"user_id":       userID,
		"machine_id":    machineID,
		"mode":          mode,
		"refresh_token": refreshToken,
	}
	if refreshToken != "" {
		// 打包串（ref oauth.rs:70）：{refresh_token}|{user_id}|{machine_id}。
		blob["refresh_token"] = refreshToken + "|" + userID + "|" + machineID
	}
	if nickname != "" {
		blob["nickname"] = nickname
	}
	if email != "" {
		blob["email"] = email
	}
	return blob, nil
}

// fetchProfile GET {openapi}/api/v1/userinfo（失败返回 nil——不阻塞登录）。
func (m *Manager) fetchProfile(ctx context.Context, intl bool, token string) map[string]any {
	url := strings.TrimRight(m.cfg.QoderOpenAPI[intl], "/") + "/api/v1/userinfo"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil
	}
	for _, h := range qoderOpenAPIHeaders(token) {
		req.Header.Set(h[0], h[1])
	}
	resp, err := m.cfg.Client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	var profile map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&profile); err != nil {
		return nil
	}
	return profile
}

// blobLabel 入池 label 兜底（规格 §3.1：qoder 用 nickname/email）。
func blobLabel(blob map[string]any, fallbackPrefix string) string {
	if n := jsonString(blob["nickname"]); n != "" {
		return n
	}
	if e := jsonString(blob["email"]); e != "" {
		return e
	}
	return fallbackPrefix + "-登录账号"
}

// randReader 测试注入点占位（当前恒 crypto/rand；保留别名以免 Qoder/autoclaw
// 各自引一份 rand）。
var randReader = cryptoRand{}

type cryptoRand struct{}

func (cryptoRand) Read(p []byte) (int, error) { return rand.Read(p) }

// randomURLSafe n 随机字节 → base64url 无填充（PKCE verifier；ref machine.rs:13-18）。
func randomURLSafe(n int) string {
	buf := make([]byte, n)
	if _, err := io.ReadFull(randReader, buf); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return base64.RawURLEncoding.EncodeToString(buf)
}

// hashSHA256 字符串的 SHA-256 摘要（challenge = base64url(SHA-256(verifier))）。
func hashSHA256(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}
