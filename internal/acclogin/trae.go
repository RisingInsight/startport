// trae.go TRAE（SOLO 国内）浏览器登录：PKCE + 本机回环回调（spec §2.4）。
//
// 编排（ref cockpit-tools trae_oauth.rs / cpa-multi-plugins trae 插件，2026-10 对齐）：
//
//	① POST {guidance}/cloudide/api/v3/trae/GetLoginGuidance → LoginHost（失败兜底 api.trae.cn）
//	② 浏览器打开 {loginHost}/authorization?…（auth_callback_url 刻意不编码——
//	   官方前端正则硬校验 ^http://127\.0\.0\.1:<port>/authorize$，其余形态直接渲染登录失败）
//	③ 回调 GET http://127.0.0.1:<port>/authorize?authCode|authCodeInfo|refreshToken|loginHost|userInfo…
//	   浏览器回连失败时可把地址栏整段粘回（SubmitCallback，规格 §2.4 submit_callback）
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

// traeSessionState 一轮 TRAE 登录的服务端材料（HTTP 回调与「粘贴回调」共用）。
// 只存内存：verifier/密钥对不落日志、不回显。
type traeSessionState struct {
	ident      traeDeviceIdentity
	verifier   string
	loginHost  string
	dispatched bool // 已开始换证（两种投递方式只认第一次）
}

// traeCallbackMaterial 一次回调里可用的材料（键名候选顺序即优先级，
// 与参考实现 oauth.rs:349-389 对齐）。
type traeCallbackMaterial struct {
	AuthCode     string
	AuthCodeFrom string // 命中的键名（诊断用；只记键不记值）
	RefreshToken string
	LoginHost    string
	UID          string
	Nickname     string
	ErrMsg       string
}

// traeParseCallback 解析回调 query。error 分支先返回；authCode 直接键优先，
// `authCodeInfo`（值是 URL 编码过的 JSON）作兜底递归提取——2026-10 实测上游
// 有两种回调形态，只认 authCode 会把后一种误判成「没有授权码」。
func traeParseCallback(q url.Values) traeCallbackMaterial {
	var m traeCallbackMaterial
	for _, k := range []string{"error", "error_code", "err", "errorCode"} {
		if v := strings.TrimSpace(q.Get(k)); v != "" {
			m.ErrMsg = "授权失败：" + v
			return m
		}
	}
	if q.Get("isRedirect") == "false" {
		m.ErrMsg = "授权失败：登录页未完成跳转（isRedirect=false）"
		return m
	}
	m.LoginHost = traeFirstValue(q, "loginHost", "login_host", "LoginHost", "host", "consoleHost")
	for _, k := range []string{"authCode", "auth_code", "AuthCode", "authorization_code", "code"} {
		if v := strings.TrimSpace(q.Get(k)); v != "" {
			m.AuthCode, m.AuthCodeFrom = v, k
			break
		}
	}
	if m.AuthCode == "" {
		for _, k := range []string{"authCodeInfo", "auth_code_info", "AuthCodeInfo"} {
			if raw := strings.TrimSpace(q.Get(k)); raw != "" {
				if code := traeExtractAuthCodeInfo(raw); code != "" {
					m.AuthCode, m.AuthCodeFrom = code, k
					break
				}
			}
		}
	}
	m.RefreshToken = traeFirstValue(q, "refreshToken", "refresh_token", "RefreshToken", "refresh-token")
	m.UID, m.Nickname = traeParseCallbackIdentity(q)
	return m
}

func traeFirstValue(q url.Values, keys ...string) string {
	for _, k := range keys {
		if v := strings.TrimSpace(q.Get(k)); v != "" {
			return v
		}
	}
	return ""
}

// traeExtractAuthCodeInfo 解 `authCodeInfo`：值是 URL 编码过的 JSON（Go 的
// Query() 已还原一层；这里再兜底一次，兼容双重编码），递归找内部 authCode。
func traeExtractAuthCodeInfo(raw string) string {
	candidates := []string{raw}
	if s, err := url.QueryUnescape(raw); err == nil && s != raw {
		candidates = append(candidates, s)
	}
	for _, c := range candidates {
		var v any
		if json.Unmarshal([]byte(c), &v) != nil {
			continue
		}
		if code := traeFindAuthCodeJSON(v, 0); code != "" {
			return code
		}
	}
	return ""
}

func traeFindAuthCodeJSON(v any, depth int) string {
	if depth > 6 {
		return ""
	}
	switch node := v.(type) {
	case string:
		// Result 直接是授权码字符串的形态（JSON.stringify("…")）也要接。
		return strings.TrimSpace(node)
	case map[string]any:
		for _, k := range []string{"authCode", "auth_code", "AuthCode", "AuthCodeToken", "code"} {
			if s, ok := node[k].(string); ok && strings.TrimSpace(s) != "" {
				return strings.TrimSpace(s)
			}
		}
		for _, child := range node {
			switch child.(type) {
			case map[string]any, []any:
				if code := traeFindAuthCodeJSON(child, depth+1); code != "" {
					return code
				}
			}
		}
	case []any:
		for _, item := range node {
			if code := traeFindAuthCodeJSON(item, depth+1); code != "" {
				return code
			}
		}
	}
	return ""
}

// traeQueryKeys 取回调参数名（诊断用：只记键不记值——授权码/续期串都可能出现）。
func traeQueryKeys(rawQuery string) []string {
	if rawQuery == "" {
		return nil
	}
	parts := strings.Split(rawQuery, "&")
	keys := make([]string, 0, len(parts))
	for _, p := range parts {
		if p == "" {
			continue
		}
		k := p
		if i := strings.IndexByte(k, '='); i >= 0 {
			k = k[:i]
		}
		if d, err := url.QueryUnescape(k); err == nil {
			k = d
		}
		if k != "" {
			keys = append(keys, k)
		}
	}
	return keys
}

// traeLooksLikeLoginCallback 带身份/宿主回显说明这是登录页发来的真回调，只是
// 没带授权材料——要落失败并把参数名报出来，不能当浏览器噪音 404 吞掉。其余
// （favicon / preconnect / 随机探测）继续等下一个请求。
func traeLooksLikeLoginCallback(q url.Values) bool {
	for _, k := range []string{"userInfo", "user_info", "UserInfo", "userinfo",
		"loginHost", "login_host", "LoginHost", "login_trace_id", "loginTraceID",
		"userTag", "user_tag", "UserTag"} {
		if strings.TrimSpace(q.Get(k)) != "" {
			return true
		}
	}
	return false
}

// traePastedQuery 把「粘贴的回调地址」归一成 query 串：整段 URL / 带 ? 的片段 /
// 裸 query 都接；空内容直接报人话。
func traePastedQuery(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", fmt.Errorf("粘贴内容为空：请把浏览器地址栏里的完整回调地址粘进来")
	}
	if strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") {
		u, err := url.Parse(s)
		if err != nil || u.RawQuery == "" {
			return "", fmt.Errorf("粘贴的地址没有查询参数（应形如 http://127.0.0.1:<端口>/authorize?authCode=…）")
		}
		return u.RawQuery, nil
	}
	if i := strings.IndexByte(s, '?'); i >= 0 {
		s = s[i+1:]
	}
	s = strings.TrimPrefix(s, "?")
	if s == "" {
		return "", fmt.Errorf("粘贴内容没有查询参数（应形如 …/authorize?authCode=…）")
	}
	return s, nil
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
		trae: &traeSessionState{
			ident:     ident,
			verifier:  verifier,
			loginHost: loginHost,
		},
	}
	if err := m.registerSession(s); err != nil {
		_ = ln.Close()
		return nil, err
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.handleTraeCallback(s, w, r)
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

// traeWaitingPage 尚未收到授权材料的中间页（状态码 200）：会话保持 pending，
// 面板继续轮询/可粘贴回调。标题刻意不是 Login failed——这一轮还没定论。
func traeWaitingPage(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprintf(w, "<html><head><meta charset=\"utf-8\"><title>%s</title></head>"+
		"<body style=\"font-family:sans-serif;text-align:center;padding-top:60px\">"+
		"<h2>%s</h2><p>%s</p></body></html>",
		htmlEscapeStr("Waiting for authorization"), htmlEscapeStr("Waiting for authorization"), htmlEscapeStr(msg))
}

// handleTraeCallback GET /authorize?…：无参数/纯噪音回 404 不投递；error 落失败；
// 「有身份回显但没授权材料」回中间页并继续等（不误杀可能存在的下一次回调）；
// authCode|authCodeInfo|refreshToken → 换证。
func (m *Manager) handleTraeCallback(s *Session, w http.ResponseWriter, r *http.Request) {
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
	mat := traeParseCallback(q)
	hasMaterial := mat.ErrMsg != "" || mat.AuthCode != "" || mat.RefreshToken != ""
	if !hasMaterial && !traeLooksLikeLoginCallback(q) {
		// 认不出登录材料的探测请求：回 404 继续等下一个请求（ref callback_server.rs:158-180）。
		m.cfg.Logf("[acclogin:trae] 忽略回调噪音（参数 %v）", traeQueryKeys(r.URL.RawQuery))
		w.WriteHeader(http.StatusNotFound)
		return
	}
	accepted, msg := m.traeDispatch(s, r.URL.RawQuery, mat)
	if !hasMaterial {
		// 有身份回显但没授权材料：不终结会话（可能是上游的中间跳转），
		// 面板继续等；用户也可把地址栏整段粘回「提交回调」。
		traeWaitingPage(w, msg)
		return
	}
	traeCallbackPage(w, accepted && mat.ErrMsg == "", msg)
}

// traeDispatch 投递一次回调（HTTP 回调与粘贴回调共用同一套解析与幂等）。
// 返回 (是否已投递, 页面要显示的文案)：成功投递 = 后台换证已启动（文案为空）。
func (m *Manager) traeDispatch(s *Session, rawQuery string, mat traeCallbackMaterial) (bool, string) {
	hasMaterial := mat.ErrMsg == "" && (mat.AuthCode != "" || mat.RefreshToken != "")
	s.mu.Lock()
	pending := s.status == StatusPending && s.trae != nil
	already, started := false, false
	if pending && hasMaterial {
		if s.trae.dispatched {
			already = true
		} else {
			s.trae.dispatched = true
			started = true
		}
	}
	st := s.trae
	s.mu.Unlock()

	if !pending {
		return false, "登录会话已结束，请回到面板重新发起。"
	}
	if mat.ErrMsg != "" {
		m.cfg.Logf("[acclogin:trae] 回调失败：%s（参数 %v）", mat.ErrMsg, traeQueryKeys(rawQuery))
		s.finish(StatusFailed, mat.ErrMsg, "")
		time.AfterFunc(2*time.Second, s.closeHTTP)
		return true, mat.ErrMsg
	}
	if !hasMaterial {
		return false, "还没有收到授权码（收到的参数：" + strings.Join(traeQueryKeys(rawQuery), "、") +
			"）。请继续在浏览器完成登录；若页面已停在这里，请回到 StarPort 面板把地址栏整段粘贴到「提交回调」"
	}
	if already {
		return true, "已收到授权，正在换证，请回到面板查看结果。"
	}
	if !started || st == nil {
		return false, "登录会话已结束，请回到面板重新发起。"
	}
	loginHost := mat.LoginHost
	if loginHost == "" {
		loginHost = st.loginHost
	}
	m.cfg.Logf("[acclogin:trae] 回调命中授权材料（键 %s；参数 %v）", mat.AuthCodeFrom, traeQueryKeys(rawQuery))
	go func() {
		blob, xerr := m.traeExchange(st.ident, st.verifier, loginHost, mat.AuthCode, mat.RefreshToken, mat.UID, mat.Nickname)
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
	return true, ""
}

// SubmitCallback 粘贴回调兜底（规格 §2.4 submit_callback）：浏览器无法回连
// 127.0.0.1 时，把登录后地址栏里的整段回调地址粘回面板。
func (m *Manager) SubmitCallback(sessionID, raw string) error {
	s := m.getSession(strings.TrimSpace(sessionID))
	if s == nil {
		return fmt.Errorf("登录会话不存在（已取消、被顶替或已超时清理）")
	}
	if s.Platform != account.PlatformTrae {
		return fmt.Errorf("该平台不支持粘贴回调地址")
	}
	rawQuery, err := traePastedQuery(raw)
	if err != nil {
		return err
	}
	q, perr := url.ParseQuery(rawQuery)
	if perr != nil || len(q) == 0 {
		return fmt.Errorf("粘贴内容里没有可解析的回调参数")
	}
	mat := traeParseCallback(q)
	if mat.ErrMsg == "" && mat.AuthCode == "" && mat.RefreshToken == "" {
		return fmt.Errorf("回调地址没有携带可用的授权信息（收到的参数：%s）", strings.Join(traeQueryKeys(rawQuery), "、"))
	}
	accepted, msg := m.traeDispatch(s, rawQuery, mat)
	if !accepted {
		if msg == "" {
			msg = "登录会话已结束，请回到面板重新发起。"
		}
		return fmt.Errorf("%s", msg)
	}
	m.cfg.Logf("[acclogin:trae] 已接受粘贴回调（%s）", s.ID)
	return nil
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
