// trae.go TRAE（SOLO 国内）的日常养护：每日签到 + 登录态刷新（第二批）。
//
// 契约来源（⚠ 未真实账号验收——接口形态来自参考实现逆向，失败在签到台账
// 显式报错，不静默）：
//   - 签到/状态：AgentHub trae 适配器——POST /trae/api/v2/ug/checkin_credits/claim
//     （空体）+ /status 回查；官方 code 1001 = 「签到服务未对该账号开放」→
//     按 already 记账（不重试、不标红）；「已签到」文案 = 幂等成功；
//   - 刷新：ExchangeToken 裸续期（与 acclogin 登录同源端点：cloudide 家族路径，
//     api.trae.cn 主 + api.trae.com.cn 兜底；body 契约 ClientID + RefreshToken +
//     ClientSecret "-" + UserID ""，AgentHub/agent2api 双源一致）；refresh_token
//     单副本轮换：响应未带新值沿用旧值。
//
// 与 acclogin 的关系：端点常量与响应解析在此自含（checkin 各平台刷新均自包含、
// 不反向依赖 acclogin；ClientID 与 acclogin.traeClientID 同值，包隔离不互引）。
package checkin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/RisingInsight/startport/internal/account"
)

// TRAE SOLO 国内常量（与 acclogin 登录同源；实测抓包值）。
const (
	traeSoloClientID  = "en1oxy7wnw8j9n"
	traeCheckinBase   = "https://api.trae.cn"
	traeClaimPath     = "/trae/api/v2/ug/checkin_credits/claim"
	traeStatusPath    = "/trae/api/v2/ug/checkin_credits/status"
	traeExchangePath  = "/cloudide/api/v3/trae/oauth/ExchangeToken"
	traeWebOrigin     = "https://www.trae.cn"
	traeClientTimeout = 20 * time.Second
)

// traeExchangeBases 裸续期候选域（包级变量：测试注入 httptest 用）。
var traeExchangeBases = []string{traeCheckinBase, "https://api.trae.com.cn"}

// traeAlreadyRe 上游「已签到」文案判定（幂等成功，不算失败不重试）。
var traeAlreadyRe = regexp.MustCompile(`(?i)已签到|已经签到|already`)

// traeRefreshInvoke 刷新调用点（测试注入用）；生产路径 = 本文件自含实现。
var traeRefreshInvoke = func(ctx context.Context, client *http.Client, refreshToken string) (string, string, int64, error) {
	return traeExchangeRefresh(ctx, client, nil, refreshToken)
}

// traeChecker TRAE 签到实现（Base/Client 可注入 httptest）。
type traeChecker struct {
	Base   string
	Client *http.Client
}

func newTraeChecker() *traeChecker {
	return &traeChecker{Base: traeCheckinBase, Client: &http.Client{Timeout: traeClientTimeout}}
}

// Platform 实现 Checker 接口（与 account.PlatformTrae 同枚举）。
func (c *traeChecker) Platform() string { return account.PlatformTrae }

// traeHeaders 签到/状态/支付域（trae.cn web 域，非推理网关域）公共头束。
// 契约来源 AgentHub：x-user-region/x-device-id/x-app-type 为该域风控口径。
func traeHeaders(token, deviceID string) map[string]string {
	h := map[string]string{
		"content-type":  "application/json",
		"accept":        "application/json",
		"x-user-region": "CN",
		"x-device-id":   deviceID,
		"x-app-type":    "trae",
		"origin":        traeWebOrigin,
		"referer":       traeWebOrigin + "/",
	}
	h["Authorization"] = "Bearer " + token
	return h
}

// traeCheckinStatus 签到状态回查结果（积分明细）。
type traeCheckinStatus struct {
	checkedIn       bool
	credits         int64
	consecutiveDays int64
	checkinDate     string
}

// CheckIn 每日签到：POST checkin_credits/claim（空体）。
// code 0/200 = 成功（回查状态取积分明细，失败不影响成功记账）；1001 = 平台
// 未对该账号开放（记 already——不是失败，调度器不重试）；「已签到」= 幂等；
// 其余非 0 = 失败（消息已脱敏：仅透出上游 message，不带 URL/凭证）。
func (c *traeChecker) CheckIn(ctx context.Context, a *account.Account) (Result, error) {
	blob, err := decodeBlob(a)
	if err != nil {
		return Result{}, err
	}
	token := blobString(blob, "accessToken")
	if token == "" {
		return Result{}, fmt.Errorf("blob 缺 accessToken（请重新登录）")
	}
	deviceID := blobString(blob, "deviceId")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(c.Base, "/")+traeClaimPath, strings.NewReader("{}"))
	if err != nil {
		return Result{}, err
	}
	for k, v := range traeHeaders(token, deviceID) {
		req.Header.Set(k, v)
	}
	resp, err := c.Client.Do(req)
	if err != nil {
		return Result{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var env struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	_ = json.Unmarshal(raw, &env)
	msg := strings.TrimSpace(env.Message)
	switch {
	case env.Code == 1001:
		return Result{Status: StatusAlready, Message: "签到服务未对该账号开放（官方接口 code 1001）"}, nil
	case resp.StatusCode >= 200 && resp.StatusCode < 300 && (env.Code == 0 || env.Code == 200):
		reward := ""
		if st, serr := c.status(ctx, token, deviceID); serr == nil && st.checkedIn && st.credits > 0 {
			reward = fmt.Sprintf("累计 %d 积分", st.credits)
		}
		return Result{Status: StatusOK, Reward: reward, Message: traeFirst(msg, "签到成功")}, nil
	case traeAlreadyRe.MatchString(msg):
		return Result{Status: StatusAlready, Message: traeFirst(msg, "今日已签到")}, nil
	}
	return Result{Status: StatusFailed, Message: traeFirst(msg, fmt.Sprintf("签到失败 HTTP %d", resp.StatusCode))}, nil
}

// status 签到状态回查（GET + did；积分明细来源。失败返回 err——调用方降级：
// 签到成功但明细缺失时 Reward 为空，不影响记账）。
func (c *traeChecker) status(ctx context.Context, token, deviceID string) (traeCheckinStatus, error) {
	u := strings.TrimRight(c.Base, "/") + traeStatusPath + "?did=" + url.QueryEscape(deviceID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return traeCheckinStatus{}, err
	}
	for k, v := range traeHeaders(token, deviceID) {
		req.Header.Set(k, v)
	}
	resp, err := c.Client.Do(req)
	if err != nil {
		return traeCheckinStatus{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var env struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	_ = json.Unmarshal(raw, &env)
	if env.Code == 1001 {
		return traeCheckinStatus{}, fmt.Errorf("签到服务未对该账号开放（官方接口 code 1001）")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || (env.Code != 0 && env.Code != 200) {
		return traeCheckinStatus{}, traeFirstErr(env.Message, fmt.Sprintf("HTTP %d", resp.StatusCode))
	}
	var data struct {
		CheckedIn        bool   `json:"checked_in"`
		CheckedInCamel   bool   `json:"checkedIn"`
		Credits          int64  `json:"credits"`
		TotalCredits     int64  `json:"total_credits"`
		ConsecutiveDays  int64  `json:"consecutive_days"`
		ConsecutiveCamel int64  `json:"consecutiveDays"`
		CheckinDate      string `json:"checkin_date"`
		CheckinDateCamel string `json:"checkinDate"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		return traeCheckinStatus{}, err
	}
	st := traeCheckinStatus{
		checkedIn:       data.CheckedIn || data.CheckedInCamel,
		credits:         traeFirstInt64(data.Credits, data.TotalCredits),
		consecutiveDays: traeFirstInt64(data.ConsecutiveDays, data.ConsecutiveCamel),
		checkinDate:     traeFirst(data.CheckinDate, data.CheckinDateCamel),
	}
	return st, nil
}

// RefreshTrae TRAE 登录态刷新（refresh.go 分发入口）：ExchangeToken 裸续期。
// 成功后回写 blob（accessToken 必换、refreshToken 轮换/沿用、expiresAt 可选），
// 调用方（scheduler）负责落盘与重投影。
func RefreshTrae(ctx context.Context, a *account.Account) (map[string]any, error) {
	blob, err := decodeBlob(a)
	if err != nil {
		return nil, err
	}
	rt := blobString(blob, "refreshToken")
	if rt == "" {
		return nil, fmt.Errorf("blob 缺 refreshToken（旧版登录产物），请重新登录获取")
	}
	access, refresh, exp, err := traeRefreshInvoke(ctx, &http.Client{Timeout: traeClientTimeout}, rt)
	if err != nil {
		return nil, err
	}
	blob["accessToken"] = access
	blob["refreshToken"] = refresh
	if exp > 0 {
		blob["expiresAt"] = exp
	}
	return blob, nil
}

// traeExchangeRefresh 裸续期实现：多域依次尝试（主域被墙/维护不拖垮整条链）。
// 响应三种形态兼容：嵌套 Result / result / data，或扁平；token 可能带
// Cloud-IDE-JWT 前缀（剥离）。拒绝时错误消息取 volcengine 信封或 message/msg。
func traeExchangeRefresh(ctx context.Context, client *http.Client, bases []string, refreshToken string) (string, string, int64, error) {
	rt := strings.TrimSpace(refreshToken)
	if rt == "" {
		return "", "", 0, fmt.Errorf("blob 缺 refreshToken（旧版登录产物），请重新登录获取")
	}
	if client == nil {
		client = &http.Client{Timeout: traeClientTimeout}
	}
	if len(bases) == 0 {
		bases = traeExchangeBases
	}
	// 参考实现完整契约（map 索引赋值：仅字段名，无任何凭据值字面量）。
	body := map[string]any{"ClientID": traeSoloClientID}
	body["RefreshToken"] = rt
	body["ClientSecret"] = "-"
	body["UserID"] = ""
	payload, _ := json.Marshal(body)

	var lastErr string
	for _, base := range bases {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			strings.TrimRight(base, "/")+traeExchangePath, strings.NewReader(string(payload)))
		if err != nil {
			return "", "", 0, err
		}
		req.Header.Set("content-type", "application/json")
		req.Header.Set("accept", "application/json")
		req.Header.Set("x-cloudide-token", "") // 参考实现契约：刷新请求该头为空串
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err.Error()
			continue
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		m := traeResultMap(raw)
		access := traeStripJWTPrefix(traeStr(m, "Token", "token", "AccessToken", "accessToken", "access_token"))
		if resp.StatusCode >= 200 && resp.StatusCode < 300 && access != "" {
			refresh := traeStr(m, "RefreshToken", "refreshToken", "refresh_token")
			if refresh == "" {
				refresh = rt // 单副本轮换：上游未回新值则沿用旧值
			}
			return access, refresh, traeExpiresAt(m), nil
		}
		lastErr = traeFirst(traeEnvErrMsg(raw), traeStr(m, "message", "msg"), fmt.Sprintf("HTTP %d", resp.StatusCode))
	}
	return "", "", 0, traeFirstErr(lastErr, "上游无可用响应")
}

// traeResultMap 兼容嵌套 Result/result/data 与顶层扁平形态。
func traeResultMap(raw []byte) map[string]any {
	var env map[string]any
	if json.Unmarshal(raw, &env) != nil {
		return nil
	}
	for _, k := range []string{"Result", "result", "data"} {
		if m, ok := env[k].(map[string]any); ok && len(m) > 0 {
			return m
		}
	}
	return env
}

// traeStr 多候选键取第一个非空字符串。
func traeStr(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := m[k].(string); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

// traeStripJWTPrefix 剥离上游 token 的 Cloud-IDE-JWT 前缀（参考实现同款）。
func traeStripJWTPrefix(s string) string {
	return strings.TrimSpace(strings.TrimPrefix(s, "Cloud-IDE-JWT "))
}

// traeExpiresAt 过期时刻：TokenExpireAt 毫秒优先（>2^36 判毫秒），秒兜底；
// TokenExpireDuration 相对秒再兜底。零值 = 上游未给。
func traeExpiresAt(m map[string]any) int64 {
	if n, ok := traeNum(m, "TokenExpireAt"); ok && n > 0 {
		if n > 1<<36 {
			return n / 1000
		}
		return n
	}
	if n, ok := traeNum(m, "TokenExpireDuration"); ok && n > 0 {
		return time.Now().Add(time.Duration(n) * time.Second).Unix()
	}
	return 0
}

// traeNum 宽松取数（float64/int64/json.Number 形态兼容）。
func traeNum(m map[string]any, key string) (int64, bool) {
	v, ok := m[key]
	if !ok {
		return 0, false
	}
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

// traeEnvErrMsg volcengine 错误信封（ResponseMetadata.Error.Message）提取。
func traeEnvErrMsg(raw []byte) string {
	var env struct {
		ResponseMetadata struct {
			Error struct {
				Message string `json:"Message"`
			} `json:"Error"`
		} `json:"ResponseMetadata"`
		Message string `json:"message"`
		Msg     string `json:"msg"`
	}
	if json.Unmarshal(raw, &env) != nil {
		return ""
	}
	if s := strings.TrimSpace(env.ResponseMetadata.Error.Message); s != "" {
		return s
	}
	return strings.TrimSpace(traeFirst(env.Message, env.Msg))
}

// traeFirst 第一个非空串。
func traeFirst(ss ...string) string {
	for _, s := range ss {
		if t := strings.TrimSpace(s); t != "" {
			return t
		}
	}
	return ""
}

// traeFirstInt64 第一个非零数。
func traeFirstInt64(ns ...int64) int64 {
	for _, n := range ns {
		if n != 0 {
			return n
		}
	}
	return 0
}

// traeFirstErr 组错误（空文案兜底）。
func traeFirstErr(msg, fallback string) error {
	return fmt.Errorf("%s", traeFirst(msg, fallback))
}
