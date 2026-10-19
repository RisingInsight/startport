// autoclaw.go AutoClaw 平台签到（架构 §5.2；移植依据 ref `autoclaw/checkin.rs`
// + `refresh.rs` 的 userapi 域签名头）。
//
// ── 上游是什么（checkin.rs:1-25 的逆向结论）─────────────────
// 老项目没有签到；接口从桌面端 app.asar 挖出并实测：每日签到走**通用任务接口**
// `POST {userapi}/autoclaw-proxy/proxy/autoclaw-task-complete`，body 只有
// `{"task_id":"daily_signin"}`。**HTTP 200 + body 里的成功标记**——当天已签时
// 返回 `success:false` + `already_completed:true`，判"本次领到了"要看
// `reward_points > 0`（或 `success === true`），不能只看 HTTP 状态码。
//
// ── userapi 域签名头（refresh.rs:241-289 逐字移植）───────────
// `X-Auth-Sign = MD5("{appId}&{ts秒}&{appKey}")`（appId/appKey 是客户端内置
// 指纹常量）、`X-Version: 1.18.5`（版本门控）、品牌头束、`X-Harness-Type:
// zcode`——**这条链路保留它**（userapi 域实测带着照常 200，且与签名一起构成
// 官方客户端指纹；与转发那条路刻意不一致，别统一，refresh.rs:255-265）。
//
// ── 活动福利（checkin.rs:39-63）本期不做 ─────────────────────
// user-info 触发 + 积分流水复核是附加收益，依赖 promotion.rs 的流水读取链，
// 不在本次交付口径内（已在汇报中列为降级项；漏掉不影响签到本身的成败）。
package checkin

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/RisingInsight/startport/internal/account"
)

// autoclaw 常量（refresh.rs:59-74 + checkin.rs:79-98 逐字照抄）。
const (
	autoclawAuthAppID  = "100003"
	autoclawAuthAppKey = "38d2391985e2369a5fb8227d8e6cd5e5"
	autoclawVersion    = "1.18.5" // X-Version 版本门控：过时的症状是目录/数据回落而非报错

	autoclawTaskComplete = "/autoclaw-proxy/proxy/autoclaw-task-complete"
	autoclawTaskList     = "/autoclaw-proxy/proxy/autoclaw-task-list"
	autoclawRefreshPath  = "/userapi/v1/refresh"
	autoclawAgentRefresh = "/userapi/v1/agent-refresh"
	autoclawUserInfo     = "/userapi/v1/user-info"

	autoclawDailySigninTaskID = "daily_signin"
)

// userapi 域基址（ref autoclaw/region.rs:138 的 userapi_base_url）。
var autoclawUserAPIBase = map[bool]string{
	false: "https://autoglm-acceleration-api.zhipuai.cn", // 国内
	true:  "https://autoglm-api.autoglm.ai",              // 国际
}

// autoclaw 登录态失效业务码（balance.rs:86）。
var autoclawAuthExpiredCodes = []int64{410000, 400000}

// autoclaw 刷新签名校验失败的降级码（refresh.rs:344-350：400002 → agent-refresh）。
const autoclawRefreshFallbackCode = 400002

type autoclawChecker struct {
	intl   bool
	Base   string // userapi 域官方缺省；测试注入 httptest
	Client *http.Client
	Now    func() time.Time // 签名时间戳注入（测试）
}

func newAutoClawChecker(intl bool) *autoclawChecker {
	return &autoclawChecker{intl: intl, Base: autoclawUserAPIBase[intl],
		Client: &http.Client{Timeout: 20 * time.Second}}
}

// Platform 实现 Checker。
func (c *autoclawChecker) Platform() string {
	if c.intl {
		return account.PlatformAutoClawIntl
	}
	return account.PlatformAutoClaw
}

// signedAuthHeaders userapi 域签名头（refresh.rs:241-289 逐字移植；
// 时间戳从 c.now() 取——测试注入固定秒）。
func (c *autoclawChecker) signedAuthHeaders(token string) [][2]string {
	ts := fmt.Sprintf("%d", c.now().Unix())
	sum := md5.Sum([]byte(autoclawAuthAppID + "&" + ts + "&" + autoclawAuthAppKey))
	sign := hex.EncodeToString(sum[:])
	headers := [][2]string{
		{"Content-Type", "application/json"},
		{"Accept", "*/*"},
		{"X-Version", autoclawVersion}, // 版本门控（refresh.rs:62-74）
		{"X-Product", "autoclaw"},
		{"X-Client-Type", "pc"},
		// ── 这里**保留** X-Harness-Type（refresh.rs:255-265：userapi 域实测
		// 带着照常 200，且是官方客户端指纹的一部分——与转发那条路刻意不一致）──
		{"X-Harness-Type", "zcode"},
		{"X-Tm", "win"}, // 恒 win（linux 会被部分账号 403，与转发域同一结论）
		{"X-Lang", "zh-CN"},
		{"X-Channel", "official"},
		{"X-Auth-Appid", autoclawAuthAppID},
		{"X-Auth-TimeStamp", ts},
		{"X-Auth-Sign", sign},
		{"X-Trace-Id", newCheckinUUID()},
	}
	if token != "" {
		headers = append(headers, [2]string{"authorization", "Bearer " + token})
	}
	return headers
}

// now 当前时间（测试注入点）。
func (c *autoclawChecker) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// CheckIn 每日签到（checkin.rs:210-273 的 run_daily_signin 主体）。
// 登录态失效时的刷新重试由调度器编排（refreshAndWriteback——它持有 Store
// 才能完成"自管存储回写"，见 scheduler.go；本函数保持纯函数语义）。
//
// 活动福利的 user-info 触发与流水复核本期不做（附加收益，失败/缺失不影响
// 签到本身的成败——已在交付汇报列为降级项）。
func (c *autoclawChecker) CheckIn(ctx context.Context, a *account.Account) (Result, error) {
	blob, err := decodeBlob(a)
	if err != nil {
		return Result{}, err
	}
	token := blobString(blob, "token")
	if token == "" {
		return Result{}, authExpiredf("账号 blob 缺少 token")
	}
	return c.postTaskComplete(ctx, token)
}

// RefreshAutoClaw 强制刷新一个 AutoClaw 账号的 access token（架构 §4.7 最小
// 闭环的刷新半边）：400002 → 降级 agent-refresh。**回写由调用方完成**——
// scheduler.refreshAndWriteback 持有 Store（网关自管存储的刷新结果回写是
// 安全且必要的，否则重启即丢，账号必死）。
// 走注册表里对应地区的 Checker 实例（测试可注入 httptest Base）。
func RefreshAutoClaw(ctx context.Context, a *account.Account) (string, error) {
	c, ok := For(a.Platform)
	if !ok {
		return "", ErrPlatformNotSupported
	}
	ac, isAC := c.(*autoclawChecker)
	if !isAC {
		return "", fmt.Errorf("平台 %s 的签到实现不是 AutoClaw", a.Platform)
	}
	blob, err := decodeBlob(a)
	if err != nil {
		return "", err
	}
	token := blobString(blob, "token")
	if token == "" {
		return "", authExpiredf("账号 blob 缺少 token")
	}
	return ac.refreshToken(ctx, token, blob)
}

// postTaskComplete 签到任务本身（checkin.rs:210-273）。
func (c *autoclawChecker) postTaskComplete(ctx context.Context, token string) (Result, error) {
	payload, err := c.userapiPost(ctx, token, autoclawTaskComplete,
		map[string]any{"task_id": autoclawDailySigninTaskID})
	if err != nil {
		return Result{}, err
	}
	// 业务码判定：登录态失效同样以业务码表达（balance.rs 的 AUTH_EXPIRED_CODES）。
	if code := jsonInt64(payload["code"]); code != 0 {
		if autoclawIsAuthExpired(code) {
			return Result{}, authExpiredf("业务码 %d", code)
		}
		msg := jsonString(payload["msg"])
		if msg == "" {
			msg = "签到失败"
		}
		return Result{Status: StatusFailed, Message: msg}, nil
	}
	data, _ := payload["data"].(map[string]any)
	already := truthyAutoClaw(data["already_completed"])
	reportedSuccess := truthyAutoClaw(data["success"])
	rewardPoints := jsonInt64(data["reward_points"])

	// 「本次领到了」的判据是 reward_points > 0 或显式 success；已领过不算成功
	// （checkin.rs:240-243：把"今天已签到"算成成功会让界面与实际入账不符）。
	claimed := rewardPoints > 0 || (reportedSuccess && !already)
	if claimed {
		reward := "签到成功"
		if rewardPoints > 0 {
			reward = fmt.Sprintf("%d 积分", rewardPoints)
		}
		return Result{Status: StatusOK, Reward: reward,
			Message: "签到成功，获得 " + reward}, nil
	}
	if already {
		return Result{Status: StatusAlready, Message: "今天已签到"}, nil
	}
	// 未领取且非"已签过"：拉任务列表归因（只读，失败不升级；checkin.rs:275-299）。
	if hint := c.describeTaskState(ctx, token); hint != "" {
		return Result{Status: StatusFailed, Message: "签到未领取（" + hint + "）"}, nil
	}
	return Result{Status: StatusFailed, Message: "签到未领取"}, nil
}

// describeTaskState 签到未领取时的归因（checkin.rs:279-299：能拿到列表而
// daily_signin 已完成 → 是"今天已签到"而不是"接口坏了"；拿不到 → None）。
func (c *autoclawChecker) describeTaskState(ctx context.Context, token string) string {
	payload, err := c.userapiGet(ctx, token, autoclawTaskList)
	if err != nil {
		return ""
	}
	if code := jsonInt64(payload["code"]); code != 0 {
		return ""
	}
	tasks, _ := payload["data"].([]any)
	for _, item := range tasks {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if jsonString(m["task_id"]) != autoclawDailySigninTaskID {
			continue
		}
		if truthyAutoClaw(m["completed"]) {
			return "任务列表显示今天已完成"
		}
		return ""
	}
	return ""
}

// userapiPost / userapiGet userapi 域薄封装（签名头 + JSON 解码 + 错误翻译；
// 消息已脱敏——只带动作名与状态码，不带 URL）。
func (c *autoclawChecker) userapiPost(ctx context.Context, token, path string, body any) (map[string]any, error) {
	return c.userapiDo(ctx, http.MethodPost, token, path, body)
}

func (c *autoclawChecker) userapiGet(ctx context.Context, token, path string) (map[string]any, error) {
	return c.userapiDo(ctx, http.MethodGet, token, path, nil)
}

func (c *autoclawChecker) userapiDo(ctx context.Context, method, token, path string, body any) (map[string]any, error) {
	var rdr *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("序列化请求体失败")
		}
		rdr = bytes.NewReader(raw)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.Base, "/")+path, rdr)
	if err != nil {
		return nil, fmt.Errorf("构造 %s 请求失败", actionName(method))
	}
	for _, h := range c.signedAuthHeaders(token) {
		req.Header.Set(h[0], h[1])
	}
	resp, err := c.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s请求失败（网络）", actionName(method))
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, authExpiredf("HTTP %d", resp.StatusCode)
	}
	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("%s响应形状不符（平台接口可能已变更）", actionName(method))
	}
	return payload, nil
}

// refreshToken 强制刷新 access token（refresh.rs:299-407 的最小闭环：
// 400002 → 降级 agent-refresh；**回写由调用方完成**——本函数只返回新 token）。
func (c *autoclawChecker) refreshToken(ctx context.Context, oldToken string, blob map[string]any) (string, error) {
	refresh := blobString(blob, "refresh_token")
	if refresh == "" {
		return "", fmt.Errorf("没有刷新凭证")
	}
	body := map[string]any{"refresh_token": refresh, "source_id": "autoclaw"}
	if device := blobString(blob, "device_id"); device != "" {
		body["device_id"] = device
	}
	payload, err := c.userapiDo(ctx, http.MethodPost, oldToken, autoclawRefreshPath, body)
	if err != nil {
		return "", err
	}
	if code := jsonInt64(payload["code"]); code == autoclawRefreshFallbackCode {
		// 400002：签名校验失败 → 降级 agent-refresh（refresh.rs:344-350）。
		payload, err = c.userapiDo(ctx, http.MethodPost, oldToken, autoclawAgentRefresh, body)
		if err != nil {
			return "", err
		}
	}
	if code := jsonInt64(payload["code"]); code != 0 {
		unauthorized := code == 401 || code == 410000
		msg := jsonString(payload["msg"])
		if msg == "" {
			msg = jsonString(payload["message"])
		}
		if msg == "" {
			msg = "服务器未返回新 token"
		}
		if unauthorized {
			return "", authExpiredf("刷新失败 code=%d msg=%s", code, msg)
		}
		return "", fmt.Errorf("AutoClaw token 刷新失败 code=%d msg=%s", code, msg)
	}
	data, _ := payload["data"].(map[string]any)
	newToken := strings.TrimPrefix(jsonString(data["access_token"]), "Bearer ")
	if newToken == "" {
		return "", fmt.Errorf("AutoClaw token 刷新失败：服务器未返回新 token")
	}
	return newToken, nil
}

// updateAutoClawToken 把新 access token 写进 blob 的 map 形态（序列化由
// 调用方完成——scheduler.refreshAndWriteback 经 Store.UpdateCred 落盘）。
func updateAutoClawToken(m map[string]any, newToken string) {
	m["token"] = newToken
}

func autoclawIsAuthExpired(code int64) bool {
	for _, c := range autoclawAuthExpiredCodes {
		if c == code {
			return true
		}
	}
	return false
}

func isAuthExpiredErr(err error) bool {
	return err != nil && errors.Is(err, ErrAuthExpired)
}

// jsonInt64 / jsonString / truthyAutoClaw 宽松取值助手。
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

func truthyAutoClaw(v any) bool {
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

// newCheckinUUID UUID v4（X-Trace-Id 用；crypto/rand，无熵源退化纳秒）。
func newCheckinUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("trace-%016x", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func actionName(method string) string {
	if method == http.MethodGet {
		return "查询"
	}
	return "签到"
}

// AutoClawSignedHeaders userapi 域签名头的导出形态（账号中心登录流程
// internal/acclogin 复用——签名常量与头束**只允许一份实现**，第二份就是
// 稳定 400002）。now 注入时间戳（测试复算；生产传 time.Now()）。
func AutoClawSignedHeaders(token string, now time.Time) [][2]string {
	c := newAutoClawChecker(false)
	c.Now = func() time.Time { return now }
	return c.signedAuthHeaders(token)
}
