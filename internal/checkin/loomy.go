// loomy.go Loomy 平台签到（架构 §5.2：每日登录积分，三平台里最薄的一个）。
//
// 移植依据：ref `loomy/client.rs`（token 请求薄封装：`token` + `Authorization:
// Bearer` **双头**；HTTP 401/403 → "登录态已失效"）+ `loomy/endpoints.rs:31`
// （积分域 `https://loomyad.xunfei.cn`）+ 架构文档 §5.2（每日登录刷新 =
// `GET /api/v1/points/first-login`）。
//
// 业务码口径（client.rs:7-12）：HTTP 200 + `{code, desc, data}`；
// `000000` 成功 / `020002`·`100002` 登录失效 / 其余业务失败。
package checkin

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/RisingInsight/startport/internal/netdiag"
	"net/http"
	"strings"
	"time"

	"github.com/RisingInsight/startport/internal/account"
	"github.com/RisingInsight/startport/internal/provider"
)

// loomy 集成网关域与签到路径（ref loomy/endpoints.rs:31 + 架构 §5.2）。
const (
	loomyBase          = "https://loomyad.xunfei.cn"
	loomyFirstLoginRef = "/api/v1/points/first-login"
)

// Loomy 成功 / 登录失效业务码（ref client.rs:29-32）。
const (
	loomySuccessCode = "000000"
)

var loomyAuthCodes = []string{"020002", "100002"}

type loomyChecker struct {
	Base   string       // 官方缺省；测试注入 httptest
	Client *http.Client // 超时 ≤20s（client.rs:26 口径）
	Now    func() time.Time
}

func newLoomyChecker() *loomyChecker {
	return &loomyChecker{Base: loomyBase, Client: &http.Client{Timeout: 20 * time.Second}}
}

// Platform 实现 Checker。
func (c *loomyChecker) Platform() string { return account.PlatformLoomy }

// CheckIn 每日登录积分：GET {base}/api/v1/points/first-login，双头会话。
func (c *loomyChecker) CheckIn(ctx context.Context, a *account.Account) (Result, error) {
	blob, err := decodeBlob(a)
	if err != nil {
		return Result{}, err
	}
	session := blobString(blob, "session")
	if session == "" {
		return Result{}, authExpiredf("账号 blob 缺少 session")
	}
	url := strings.TrimRight(c.Base, "/") + loomyFirstLoginRef
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Result{}, fmt.Errorf("构造签到请求失败")
	}
	req.Header.Set("Accept", "application/json")
	for _, h := range provider.LoomySessionHeaders(session) {
		req.Header.Set(h[0], h[1])
	}
	resp, err := c.Client.Do(req)
	if err != nil {
		return Result{}, fmt.Errorf("签到请求失败（网络）%s", netdiag.ProxyHint(err))
	}
	defer resp.Body.Close()
	// HTTP 401/403 → 登录态失效（client.rs:122-127 同一口径）。
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return Result{}, authExpiredf("HTTP %d", resp.StatusCode)
	}
	var payload struct {
		Code string          `json:"code"`
		Desc string          `json:"desc"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		// 响应形状不符：显式告警（P0-4——不静默）。
		return Result{Status: StatusFailed,
			Message: "平台接口可能已变更（响应不是 Loomy 业务信封）"}, nil
	}
	if isLoomyAuthCode(payload.Code) {
		return Result{}, authExpiredf("业务码 %s", payload.Code)
	}
	if payload.Code != loomySuccessCode {
		desc := strings.TrimSpace(payload.Desc)
		if desc == "" {
			desc = "上游未给原因"
		}
		return Result{Status: StatusFailed, Message: "签到失败：" + desc}, nil
	}
	reward := loomyReward(payload.Data)
	return Result{Status: StatusOK, Reward: reward,
		Message: "签到成功" + rewardSuffix(reward)}, nil
}

// loomyReward 从 data 里提取积分描述（字段名未在 ref 中固化——防御性提取，
// 取不到给通用文案，绝不因此判失败）。
func loomyReward(data json.RawMessage) string {
	if len(data) == 0 {
		return "每日登录积分"
	}
	var probe struct {
		Points *json.Number `json:"points"`
		Credit *json.Number `json:"credits"`
		Reward string       `json:"reward"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return "每日登录积分"
	}
	if probe.Points != nil {
		return "每日登录积分 +" + probe.Points.String()
	}
	if probe.Credit != nil {
		return "每日登录积分 +" + probe.Credit.String()
	}
	if probe.Reward != "" {
		return probe.Reward
	}
	return "每日登录积分"
}

func rewardSuffix(reward string) string {
	if reward == "" {
		return ""
	}
	return "，获得 " + reward
}

func isLoomyAuthCode(code string) bool {
	for _, c := range loomyAuthCodes {
		if code == c {
			return true
		}
	}
	return false
}
