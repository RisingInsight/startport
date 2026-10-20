// trae.go TRAE（SOLO 国内）浏览器登录：PKCE + 本机回环回调（spec §2.4）。
//
// 编排（ref cockpit-tools trae_oauth.rs / cpa-multi-plugins trae 插件，2026-10 对齐）：
//
//	① POST {guidance}/cloudide/api/v3/trae/GetLoginGuidance → LoginHost（失败兜底 api.trae.cn）
//	② 浏览器打开 {loginHost}/authorization?…（auth_callback_url 刻意不编码——
//	   官方前端正则硬校验 ^http://127\.0\.0\.1:<port>/authorize$，其余形态直接渲染登录失败）
//	③ 回调 GET http://127.0.0.1:<port>/authorize?authCode|refreshToken|loginHost|userInfo…
//	④ POST {api}/trae/api/v3/oauth/ExchangeToken（P-256 SPKI 公钥进 DeviceInfo；
//	   空公钥 = HTTP 401 业务码 20405）→ token → GetUserInfo 补身份 → blob 入池
//
// 设备身份（machine/device/P-256 密钥对）网关级持久化：同安装复用同一套指纹，
// 重复登录不再耗尽上游设备位（20401 Device limit reached）。
package acclogin

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/RisingInsight/startport/internal/account"
)

// TRAE SOLO 国内的登录常量（实测抓包值，勿改；ref trae_oauth.rs:33-67）。
const (
	traeClientID      = "en1oxy7wnw8j9n" // solo 谱系 ClientID（trae/IDE 用 ono9krqynydwx5）
	traeAuthFrom      = "solo"
	traePlatformCode  = "SOLO_PC"
	traePluginVersion = "1.0.0"
	traeDeviceName    = "DESKTOP-CPASOLO"
	traeDeviceType    = "windows"
	traeDeviceBrand   = "83DG"
	traeOSVersion     = "Windows 11 Pro"
	traeEnv           = "prod"
	traeAppType       = "trae"
	traeIdeVersion    = "0.1.61"
	traeDomain        = "trae.cn"
	traeDefaultHost   = "https://api.trae.cn"
	traeCallbackPath  = "/authorize"
	traeUserAgent     = "Trae/" + traePluginVersion + " antigravity-cockpit-tools"
	traeLoginTTL      = 15 * time.Minute // 一轮登录 TTL（oauth.rs:41）
)

// traeGuidanceURLs GetLoginGuidance 候选（依序，每个 5s 超时；CN 全挂兜底默认 host）。
var traeGuidanceURLs = []string{
	"https://api.trae.cn/cloudide/api/v3/trae/GetLoginGuidance",
	"https://api.trae.com.cn/cloudide/api/v3/trae/GetLoginGuidance",
	"https://www.trae.cn/cloudide/api/v3/trae/GetLoginGuidance",
}

// traeDeviceIdentity 网关级设备身份（跨登录复用；落 0600 JSON）。
type traeDeviceIdentity struct {
	MachineID        string `json:"machine_id"`
	DeviceID         string `json:"device_id"`
	DevicePublicKey  string `json:"device_public_key"`
	DevicePrivateKey string `json:"device_private_key"`
}

// traeIdentity 读取（或现生成并落盘）网关级设备身份。落盘失败不阻断登录
// （身份只影响设备位复用，不影响本轮换证）。
func (m *Manager) traeIdentity() (traeDeviceIdentity, error) {
	path := m.cfg.TraeIdentityFile
	if raw, err := os.ReadFile(path); err == nil {
		var id traeDeviceIdentity
		if json.Unmarshal(raw, &id) == nil &&
			id.MachineID != "" && id.DeviceID != "" && id.DevicePublicKey != "" && id.DevicePrivateKey != "" {
			return id, nil
		}
	}
	id, err := newTraeDeviceIdentity()
	if err != nil {
		return traeDeviceIdentity{}, err
	}
	raw, err := json.MarshalIndent(id, "", "  ")
	if err != nil {
		return traeDeviceIdentity{}, err
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		_ = os.MkdirAll(dir, 0o755)
	}
	if werr := os.WriteFile(path, raw, 0o600); werr != nil {
		m.cfg.Logf("[acclogin:trae] 设备身份落盘失败（不影响本次登录）: %v", werr)
	}
	return id, nil
}

// newTraeDeviceIdentity 现生成一套设备身份：UUID v4 machine、16 位纯数字 device
// （上游 is_numeric_id(8,24)；字母形态会被拒）、EC P-256 SPKI/PKCS#8 PEM 密钥对。
func newTraeDeviceIdentity() (traeDeviceIdentity, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return traeDeviceIdentity{}, fmt.Errorf("生成 P-256 设备密钥: %w", err)
	}
	privDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return traeDeviceIdentity{}, fmt.Errorf("编码 PKCS#8 私钥: %w", err)
	}
	pubDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return traeDeviceIdentity{}, fmt.Errorf("编码 SPKI 公钥: %w", err)
	}
	pubPEM := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER}))
	privPEM := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privDER}))
	return traeDeviceIdentity{
		MachineID:        traeUUIDv4(),
		DeviceID:         traeRandomDigits(16),
		DevicePublicKey:  pubPEM,
		DevicePrivateKey: privPEM,
	}, nil
}

func traeRandomDigits(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = '0' + b[i]%10
	}
	return string(b)
}

func traeUUIDv4() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// traePKCEPair 48 随机字节 base64url → verifier（64 字符）；SHA-256 → challenge。
func traePKCEPair() (verifier, challenge string) {
	var b [48]byte
	_, _ = rand.Read(b[:])
	verifier = base64.RawURLEncoding.EncodeToString(b[:])
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return verifier, challenge
}

// traeEnsureHTTPS 裸域名补 https://（GetLoginGuidance 常回 "www.trae.cn"）。
func traeEnsureHTTPS(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	if strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") {
		return s
	}
	return "https://" + strings.TrimLeft(s, "/")
}

// traeExtractLoginHost 从 guidance 响应里找 LoginHost（顶层 / Result / data 多路径）。
func traeExtractLoginHost(raw []byte) string {
	var top map[string]any
	if json.Unmarshal(raw, &top) != nil {
		return ""
	}
	keys := []string{"LoginHost", "loginHost", "LoginURL", "loginUrl", "login_url"}
	pick := func(m map[string]any) string {
		for _, k := range keys {
			if s, ok := m[k].(string); ok && strings.TrimSpace(s) != "" {
				return strings.TrimSpace(s)
			}
		}
		return ""
	}
	if s := pick(top); s != "" {
		return s
	}
	for _, rk := range []string{"Result", "result"} {
		if sub, ok := top[rk].(map[string]any); ok {
			if s := pick(sub); s != "" {
				return s
			}
		}
	}
	if data, ok := top["data"].(map[string]any); ok {
		if s := pick(data); s != "" {
			return s
		}
		for _, rk := range []string{"Result", "result"} {
			if sub, ok := data[rk].(map[string]any); ok {
				if s := pick(sub); s != "" {
					return s
				}
			}
		}
	}
	return ""
}

// traeGuidanceHost 拉登录 host（5s/候选；CN 全挂兜底默认 host，不阻断登录）。
func (m *Manager) traeGuidanceHost(ctx context.Context, loginTraceID string) string {
	endpoints := m.cfg.TraeGuidanceURLs
	if len(endpoints) == 0 {
		endpoints = traeGuidanceURLs
	}
	body, _ := json.Marshal(map[string]any{"loginTraceID": loginTraceID, "login_trace_id": loginTraceID})
	for _, endpoint := range endpoints {
		gctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		req, err := http.NewRequestWithContext(gctx, http.MethodPost, endpoint, bytes.NewReader(body))
		if err == nil {
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json")
			req.Header.Set("User-Agent", traeUserAgent)
			resp, derr := m.cfg.Client.Do(req)
			if derr == nil {
				raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
				resp.Body.Close()
				if resp.StatusCode < 400 {
					if host := traeExtractLoginHost(raw); host != "" {
						cancel()
						return host
					}
				}
			}
		}
		cancel()
	}
	return traeDefaultHost
}

// traeBuildVerificationURI 拼授权 URL：参数顺序与编码逐字对齐官方客户端；
// auth_callback_url **不编码**（官方前端正则硬校验，编码反而匹配失败）。
func traeBuildVerificationURI(loginHost, cbURL, machineID, deviceID, codeChallenge, loginTraceID string) string {
	type kv struct {
		k, v   string
		encode bool
	}
	params := []kv{
		{"login_version", "1", false},
		{"auth_from", traeAuthFrom, false},
		{"login_channel", "native_ide", false},
		{"plugin_version", traePluginVersion, true},
		{"auth_type", "local", false},
		{"client_id", traeClientID, false},
		{"redirect", "0", false},
		{"login_trace_id", loginTraceID, true},
		{"auth_callback_url", cbURL, false},
		{"machine_id", machineID, true},
		{"device_id", deviceID, true},
		{"x_device_id", deviceID, true},
		{"x_machine_id", machineID, true},
		{"x_device_brand", traeDeviceBrand, true},
		{"x_device_type", traeDeviceType, true},
		{"x_os_version", traeOSVersion, true},
		{"x_env", traeEnv, true},
		{"x_app_version", traeIdeVersion, true},
		{"x_app_type", traeAppType, true},
		{"code_challenge", codeChallenge, true},
		{"code_challenge_method", "S256", false},
		{"hide_saas_login", "true", false},
	}
	parts := make([]string, 0, len(params))
	for _, e := range params {
		v := e.v
		if e.encode {
			v = url.QueryEscape(v)
		}
		parts = append(parts, e.k+"="+v)
	}
	base := strings.TrimRight(traeEnsureHTTPS(loginHost), "/")
	return base + "/authorization?" + strings.Join(parts, "&")
}

// traeDeviceInfo ExchangeToken 的 DeviceInfo（公钥必填；空 = 20405 设备绑定被拒）。
func traeDeviceInfo(ident traeDeviceIdentity) map[string]any {
	return map[string]any{
		"DeviceID":        ident.DeviceID,
		"MachineID":       ident.MachineID,
		"PlatformCode":    traePlatformCode,
		"DeviceType":      "PC",
		"DeviceName":      traeDeviceName,
		"DeviceModel":     traeDeviceBrand,
		"ClientVersion":   traeIdeVersion,
		"DevicePublicKey": ident.DevicePublicKey,
		"DeviceBrand":     "Microsoft",
		"DeviceCPU":       "",
		"OSInfo":          traeDeviceType,
		"OSVersion":       traeOSVersion,
	}
}

// traeAuthCodeURLs 授权码换证候选：CN 账号 API 官方源在前，loginHost 派生（www→api）
// 兜底（ref candidate_account_api_origins）。TraeAPIOrigins 注入时以注入源为准。
func (m *Manager) traeAuthCodeURLs(loginHost string) []string {
	var origins []string
	if len(m.cfg.TraeAPIOrigins) > 0 {
		origins = append(origins, m.cfg.TraeAPIOrigins...)
	} else {
		origins = append(origins, traeDefaultHost, "https://api.trae.com.cn")
	}
	urls := make([]string, 0, len(origins)+2)
	for _, o := range origins {
		urls = append(urls, strings.TrimRight(o, "/")+"/trae/api/v3/oauth/ExchangeToken")
	}
	if u := traeEnsureHTTPS(loginHost); u != "" {
		rest := u
		if i := strings.Index(rest, "://"); i >= 0 {
			rest = rest[i+3:]
		}
		if i := strings.IndexAny(rest, "/?#"); i >= 0 {
			rest = rest[:i]
		}
		if stripped := strings.TrimPrefix(rest, "www."); stripped != rest {
			urls = append(urls, "https://api."+stripped+"/trae/api/v3/oauth/ExchangeToken")
		} else {
			urls = append(urls, "https://"+rest+"/trae/api/v3/oauth/ExchangeToken")
		}
	}
	return dedupeStrings(urls)
}

func dedupeStrings(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, v := range in {
		if v == "" {
			continue
		}
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}

// traeExchangePost 逐候选 POST 换证；带 Volcengine 业务码的 4xx 立即短路
// （与候选源无关，20401 设备位满不该再烧其余源）；404/HTML/5xx 继续下一候选。
func traeExchangePost(ctx context.Context, client *http.Client, urls []string, body map[string]any) ([]byte, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("序列化换证请求失败")
	}
	var errs []string
	for _, u := range urls {
		ectx, cancel := context.WithTimeout(ctx, 30*time.Second)
		req, rerr := http.NewRequestWithContext(ectx, http.MethodPost, u, bytes.NewReader(raw))
		if rerr == nil {
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json")
			req.Header.Set("x-cloudide-token", "")
			req.Header.Set("User-Agent", traeUserAgent)
			resp, derr := client.Do(req)
			if derr == nil {
				data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
				resp.Body.Close()
				if resp.StatusCode >= 400 {
					if code, msg := traeVolcError(data); code != "" {
						cancel()
						return nil, fmt.Errorf("换证被上游拒绝（业务码 %s：%s）", code, traeExchangeHint(code, msg))
					}
					errs = append(errs, fmt.Sprintf("%s => HTTP %d", u, resp.StatusCode))
					cancel()
					continue
				}
				if at := traeExtractExchangeToken(data); at != "" {
					cancel()
					return data, nil
				}
				errs = append(errs, fmt.Sprintf("%s => 响应无 access token", u))
			} else {
				errs = append(errs, fmt.Sprintf("%s => %v", u, derr))
			}
		} else {
			errs = append(errs, fmt.Sprintf("%s => %v", u, rerr))
		}
		cancel()
	}
	return nil, fmt.Errorf("换证失败：%s", strings.Join(errs, " | "))
}

// traeVolcError 解 Volcengine 风格 ResponseMetadata.Error {Code, Message}。
func traeVolcError(data []byte) (code, msg string) {
	var env struct {
		ResponseMetadata struct {
			Error struct {
				Code    string `json:"Code"`
				Message string `json:"Message"`
			} `json:"Error"`
		} `json:"ResponseMetadata"`
	}
	if json.Unmarshal(data, &env) != nil {
		return "", ""
	}
	return strings.TrimSpace(env.ResponseMetadata.Error.Code), strings.TrimSpace(env.ResponseMetadata.Error.Message)
}

// traeExchangeHint 业务码 → 用户可执行的恢复指引。
func traeExchangeHint(code, msg string) string {
	hint := ""
	if code == "20401" {
		hint = "；设备数已达上限——请在 Trae 官方客户端/网页端解绑旧设备后重试（本安装复用同一设备身份，重试不再新增设备位）"
	}
	if msg != "" {
		return msg + hint
	}
	return hint
}

// traeParseExchange 解析换证响应（Result.{AccessToken|accessToken|Token|token} 多键名）。
func traeParseExchange(raw []byte) (accessToken, refreshToken string, expiresAt int64) {
	var env struct {
		Result map[string]any `json:"Result"`
	}
	if json.Unmarshal(raw, &env) != nil || env.Result == nil {
		return "", "", 0
	}
	r := env.Result
	for _, k := range []string{"AccessToken", "accessToken", "Token", "token"} {
		if s, ok := r[k].(string); ok && strings.TrimSpace(s) != "" {
			accessToken = strings.TrimSpace(s)
			break
		}
	}
	for _, k := range []string{"RefreshToken", "refreshToken"} {
		if s, ok := r[k].(string); ok && strings.TrimSpace(s) != "" {
			refreshToken = strings.TrimSpace(s)
			break
		}
	}
	// TokenExpireAt 毫秒优先，TokenExpireDuration 秒兜底。
	if n, ok := traeJSONInt64(r["TokenExpireAt"]); ok && n > 0 {
		if n > 1<<36 { // 毫秒级时间戳（>2^36 ≈ 1980s 纪元的毫秒数）
			expiresAt = n / 1000
		} else {
			expiresAt = n
		}
	}
	if expiresAt == 0 {
		if n, ok := traeJSONInt64(r["TokenExpireDuration"]); ok && n > 0 {
			expiresAt = time.Now().Add(time.Duration(n) * time.Second).Unix()
		}
	}
	return accessToken, refreshToken, expiresAt
}

func traeJSONInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case float64:
		return int64(n), true
	case int64:
		return n, true
	case int:
		return int64(n), true
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return i, true
		}
	}
	return 0, false
}

// traeExtractExchangeToken 换证响应是否携带 token（候选源可用性判据）。
func traeExtractExchangeToken(raw []byte) string {
	at, _, _ := traeParseExchange(raw)
	return at
}

// traeUserInfo GetUserInfo 补身份（失败不阻塞——回调 userInfo 回显兜底）。
func (m *Manager) traeUserInfo(accessToken string) (uid, nickname string) {
	if accessToken == "" {
		return "", ""
	}
	origins := m.cfg.TraeAPIOrigins
	if len(origins) == 0 {
		origins = []string{traeDefaultHost, "https://api.trae.com.cn"}
	}
	body, _ := json.Marshal(map[string]any{"ReqSource": "IDE", "IDEVersion": traeIdeVersion})
	for _, origin := range origins {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, origin+"/cloudide/api/v3/trae/GetUserInfo", bytes.NewReader(body))
		if err == nil {
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json")
			req.Header.Set("x-cloudide-token", "")
			req.Header.Set("X-Cloudide-Token", accessToken)
			req.Header.Set("User-Agent", traeUserAgent)
			resp, derr := m.cfg.Client.Do(req)
			if derr == nil {
				raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
				resp.Body.Close()
				var env struct {
					Result struct {
						UserID     string `json:"UserID"`
						ScreenName string `json:"ScreenName"`
					} `json:"Result"`
				}
				if json.Unmarshal(raw, &env) == nil {
					cancel()
					return strings.TrimSpace(env.Result.UserID), strings.TrimSpace(env.Result.ScreenName)
				}
			}
		}
		cancel()
	}
	return "", ""
}

// traeParseCallbackIdentity 回调 userInfo 回显（JSON 或二次 JSON 引号包裹；尽力而为）。
func traeParseCallbackIdentity(q url.Values) (uid, nickname string) {
	for _, k := range []string{"userInfo", "user_info", "UserInfo", "userinfo"} {
		raw := strings.TrimSpace(q.Get(k))
		if raw == "" {
			continue
		}
		uid, nickname = traeIdentityFromJSON(raw, 0)
		if uid != "" || nickname != "" {
			return uid, nickname
		}
	}
	return "", ""
}

func traeIdentityFromJSON(raw string, depth int) (uid, nickname string) {
	if depth > 2 || raw == "" {
		return "", ""
	}
	trimmed := strings.TrimSpace(raw)
	if strings.HasPrefix(trimmed, "\"") {
		var inner string
		if json.Unmarshal([]byte(trimmed), &inner) == nil && inner != "" {
			return traeIdentityFromJSON(inner, depth+1)
		}
		return "", ""
	}
	if !strings.HasPrefix(trimmed, "{") {
		return "", ""
	}
	var m map[string]any
	if json.Unmarshal([]byte(trimmed), &m) != nil {
		return "", ""
	}
	pick := func(keys []string) string {
		for _, k := range keys {
			if s, ok := m[k].(string); ok && strings.TrimSpace(s) != "" {
				return strings.TrimSpace(s)
			}
		}
		return ""
	}
	return pick([]string{"UserID", "userId", "uid", "UID", "user_id"}),
		pick([]string{"ScreenName", "screenName", "Nickname", "nickname", "Name", "name", "displayName"})
}

// startTrae 发起 TRAE 浏览器登录：回环监听 + 授权 URL；回调协程完成换证入池。
func (m *Manager) startTrae(ctx context.Context) (*StartResult, error) {
	ident, err := m.traeIdentity()
	if err != nil {
		return nil, fmt.Errorf("生成 TRAE 设备身份失败: %w", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("分配 TRAE 回调端口失败: %w", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	cbURL := fmt.Sprintf("http://127.0.0.1:%d%s", port, traeCallbackPath)
	loginTraceID := traeUUIDv4()
	verifier, challenge := traePKCEPair()
	loginHost := m.traeGuidanceHost(ctx, loginTraceID)
	verificationURI := traeBuildVerificationURI(loginHost, cbURL, ident.MachineID, ident.DeviceID, challenge, loginTraceID)

	s := &Session{
		ID:        newSessionID(),
		Platform:  account.PlatformTrae,
		CreatedAt: m.cfg.Now(),
		ExpiresAt: m.cfg.Now().Add(traeLoginTTL),
		status:    StatusPending,
		loginURL:  verificationURI,
		done:      make(chan struct{}),
	}
	if err := m.registerSession(s); err != nil {
		_ = ln.Close()
		return nil, err
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.handleTraeCallback(s, ident, verifier, loginHost, w, r)
	})}
	s.httpSrv = srv
	go func() { _ = srv.Serve(ln) }()
	m.cfg.Logf("[acclogin:trae] 登录会话已创建（callback %s）", cbURL)
	return &StartResult{
		SessionID: s.ID,
		Mode:      "auth_url",
		LoginURL:  verificationURI,
		ExpiresAt: s.ExpiresAt,
	}, nil
}

// traeCallbackPage 回调落地页（状态码恒 200；成败由面板轮询展示）。
func traeCallbackPage(w http.ResponseWriter, ok bool, msg string) {
	title, body := "Login failed", msg
	if ok {
		title, body = "Login successful", "You can close this window now."
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprintf(w, "<html><head><meta charset=\"utf-8\"><title>%s</title></head>"+
		"<body style=\"font-family:sans-serif;text-align:center;padding-top:60px\">"+
		"<h2>%s</h2><p>%s</p></body></html>",
		htmlEscapeStr(title), htmlEscapeStr(title), htmlEscapeStr(body))
}

func htmlEscapeStr(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&#34;")
	return r.Replace(s)
}

// handleTraeCallback GET /authorize?…：浏览器噪音（无参数）回 404 不投递；
// error/isRedirect=false 直接失败；authCode/refreshToken → 后台换证入池。
func (m *Manager) handleTraeCallback(s *Session, ident traeDeviceIdentity, codeVerifier, loginHost string, w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != traeCallbackPath {
		http.NotFound(w, r)
		return
	}
	q := r.URL.Query()
	if len(q) == 0 {
		// favicon / preconnect / 手工粘贴裸链接：保持等待，不终结会话。
		w.WriteHeader(http.StatusNotFound)
		return
	}
	s.mu.Lock()
	ready := s.loginURL != ""
	status := s.status
	s.mu.Unlock()
	if !ready || status != StatusPending {
		traeCallbackPage(w, false, "登录会话已结束，请回到面板重新发起。")
		return
	}
	fail := func(msg string) {
		traeCallbackPage(w, false, msg)
		s.finish(StatusFailed, msg, "")
		time.AfterFunc(2*time.Second, s.closeHTTP)
	}
	for _, k := range []string{"error", "error_code", "err", "errorCode"} {
		if v := strings.TrimSpace(q.Get(k)); v != "" {
			fail("授权失败：" + v)
			return
		}
	}
	if q.Get("isRedirect") == "false" {
		fail("授权失败：登录页未完成跳转（isRedirect=false）")
		return
	}
	cbUID, cbNickname := traeParseCallbackIdentity(q)
	authCode := ""
	for _, k := range []string{"authCode", "auth_code", "AuthCode", "authorization_code", "code"} {
		if v := strings.TrimSpace(q.Get(k)); v != "" {
			authCode = v
			break
		}
	}
	refreshToken := ""
	for _, k := range []string{"refreshToken", "refresh_token", "RefreshToken", "refresh-token"} {
		if v := strings.TrimSpace(q.Get(k)); v != "" {
			refreshToken = v
			break
		}
	}
	if authCode == "" && refreshToken == "" {
		fail("回调链接中没有授权码参数——请回到面板重新发起登录（若浏览器无法回连，可改用粘贴凭证）")
		return
	}
	traeCallbackPage(w, true, "")
	go func() {
		blob, xerr := m.traeExchange(ident, codeVerifier, loginHost, authCode, refreshToken, cbUID, cbNickname)
		if xerr != nil {
			s.finish(StatusFailed, xerr.Error(), "")
			time.AfterFunc(2*time.Second, s.closeHTTP)
			return
		}
		label := blobLabel(blob, "trae")
		id, perr := m.poolAccount(s, blob, label, "浏览器登录")
		if perr != nil {
			s.finish(StatusFailed, "凭证已获取但入池失败: "+perr.Error(), "")
		} else {
			s.finish(StatusSuccess, "", id)
			m.cfg.Logf("[acclogin:trae] 登录成功并入池（%s）", label)
		}
		time.AfterFunc(2*time.Second, s.closeHTTP)
	}()
}

// traeExchange 回调产物 → token → blob。refreshToken 分支优先（授权码一次性；
// 两者都有时先走 refreshToken——ref login.rs:101-105），失败回落 refreshToken
// 直接当 access token 用。
func (m *Manager) traeExchange(ident traeDeviceIdentity, codeVerifier, loginHost, authCode, refreshToken, cbUID, cbNickname string) (map[string]any, error) {
	var accessToken, refreshTokenOut string
	var expiresAt int64
	switch {
	case refreshToken != "":
		// 旧版裸续期（ClientID+RefreshToken，无设备绑定）：失败兜底把回调的
		// refreshToken 直接当 access token 用（ref login.rs:101-105 同款）。
		body := map[string]any{
			"ClientID":     traeClientID,
			"RefreshToken": refreshToken,
		}
		// refresh 换证走 cloudide 家族路径（与授权码路径不同源）。
		urls := []string{
			traeDefaultHost + "/cloudide/api/v3/trae/oauth/ExchangeToken",
			"https://api.trae.com.cn/cloudide/api/v3/trae/oauth/ExchangeToken",
		}
		raw, err := traeExchangePost(context.Background(), m.cfg.Client, urls, body)
		if err != nil {
			// 兜底：回调的 refreshToken 直接当 access token（参考实现同款）。
			m.cfg.Logf("[acclogin:trae] refreshToken 换证失败，按直用兜底: %v", err)
			accessToken, refreshTokenOut = refreshToken, refreshToken
		} else {
			accessToken, refreshTokenOut, expiresAt = traeParseExchange(raw)
			if accessToken == "" {
				accessToken, refreshTokenOut = refreshToken, refreshToken
			}
		}
	default:
		if authCode == "" {
			return nil, fmt.Errorf("回调缺少授权码（authCode）")
		}
		body := map[string]any{
			"ClientID":     traeClientID,
			"AuthCode":     authCode,
			"CodeVerifier": codeVerifier,
			"DeviceInfo":   traeDeviceInfo(ident),
			"IDEVersion":   traeIdeVersion,
		}
		raw, err := traeExchangePost(context.Background(), m.cfg.Client, m.traeAuthCodeURLs(loginHost), body)
		if err != nil {
			return nil, err
		}
		accessToken, refreshTokenOut, expiresAt = traeParseExchange(raw)
		if accessToken == "" && refreshTokenOut == "" {
			return nil, fmt.Errorf("换证响应没有 token")
		}
		if accessToken == "" {
			accessToken = refreshTokenOut
		}
	}
	uid, nickname := m.traeUserInfo(accessToken)
	if uid == "" {
		uid = cbUID
	}
	if nickname == "" {
		nickname = cbNickname
	}
	if uid == "" {
		uid = "solo-unknown" // 稳定兜底（同名复用，不铸造逐次垃圾名）
	}
	blob := map[string]any{
		"accessToken":      accessToken,
		"domain":           traeDomain,
		"apiHost":          traeDefaultHost,
		"machineId":        ident.MachineID,
		"deviceId":         ident.DeviceID,
		"devicePublicKey":  ident.DevicePublicKey,
		"devicePrivateKey": ident.DevicePrivateKey,
		"variant":          traeAuthFrom,
		"uid":              uid,
		"authClientId":     traeClientID,
	}
	if refreshTokenOut != "" {
		blob["refreshToken"] = refreshTokenOut
	}
	if expiresAt > 0 {
		blob["expiresAt"] = expiresAt
	}
	if nickname != "" {
		blob["nickname"] = nickname
	}
	return blob, nil
}
