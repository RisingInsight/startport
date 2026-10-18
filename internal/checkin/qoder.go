// qoder.go Qoder（中国版）平台签到（架构 §5.2；移植依据 ref `qoder/endpoints.rs:
// 164-215` 的 sash 域 + 架构文档的两跳口径）。
//
// ── sash 域的关键结论（endpoints.rs:164-198）─────────────────
//   - `GET {openapi}/sash/api/v1/me/campaigns?forceRefresh=true` 找
//     `actionType == "CLAIM_BENEFIT"` 的活动，再 `POST .../{campaignId}/claim`；
//   - 头是**最小束、无 COSY 签名**：`authorization: Bearer <token>` +
//     `cosy-clienttype: 10`（**门控值**：用 5 会返回空活动列表）+
//     `accept / accept-language / user-agent: Qoder`；
//   - 领到的 100 credits 落 addOnQuota 桶（balance.rs：漏判此桶 = issue #26
//     根因——签到结果文案固定写明 credits）。
//
// ── 中国版 only（endpoints.rs:167 + 架构 Q5）─────────────────
// 国际版没有签到计划（ref qoder/mod.rs:46）——不注册 Checker，
// For("qoder-intl") 返回 false，面板显示「平台无此活动」。
package checkin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/RisingInsight/startport/internal/account"
)

const (
	qoderBase          = "https://openapi.qoder.com.cn" // sash 与 openapi 同主机（endpoints.rs:165）
	qoderCampaignsPath = "/sash/api/v1/me/campaigns"
	qoderClaimAction   = "CLAIM_BENEFIT"
	qoderSashClient    = "10"    // 门控值：5 会返回空活动列表（endpoints.rs:178-185）
	qoderSashUA        = "Qoder" // 抓包确认的客户端标识（endpoints.rs:189-191）
)

type qoderChecker struct {
	Base   string // 官方缺省；测试注入 httptest
	Client *http.Client
}

func newQoderChecker() *qoderChecker {
	return &qoderChecker{Base: qoderBase, Client: &http.Client{Timeout: 20 * time.Second}}
}

// Platform 实现 Checker。
func (c *qoderChecker) Platform() string { return account.PlatformQoder }

// sashHeaders 最小头束（endpoints.rs:190-199 逐字照抄，小写头名保持对拍一致）。
func qoderSashHeaders(token string) [][2]string {
	return [][2]string{
		{"authorization", "Bearer " + token},
		{"cosy-clienttype", qoderSashClient},
		{"accept", "application/json"},
		{"accept-language", "zh-CN"},
		{"user-agent", qoderSashUA},
	}
}

// CheckIn 两跳：列表 → 过滤 → claim（架构 §5.2）。
func (c *qoderChecker) CheckIn(ctx context.Context, a *account.Account) (Result, error) {
	blob, err := decodeBlob(a)
	if err != nil {
		return Result{}, err
	}
	token := blobString(blob, "access_token", "accessToken")
	if token == "" {
		return Result{}, authExpiredf("账号 blob 缺少 access_token")
	}
	// 跳 1：活动列表（forceRefresh 让上游跳过缓存给实时 claimStatus）。
	list, err := c.sashGet(ctx, token, qoderCampaignsPath+"?forceRefresh=true")
	if err != nil {
		return Result{}, err
	}
	campaign, err := pickClaimableCampaign(list)
	if err != nil {
		return Result{Status: StatusFailed, Message: err.Error()}, nil
	}
	if campaign == nil {
		// 列表里没有可领取的 CLAIM_BENEFIT：可能是已领过，也可能活动未开放。
		// 上游不区分这两种（列表口径），如实回报"没有可领取的活动"。
		return Result{Status: StatusAlready,
			Message: "今天没有可领取的签到活动（可能已领过或活动未开放）"}, nil
	}
	// 跳 2：领取。
	claim, err := c.sashPost(ctx, token,
		fmt.Sprintf("%s/%s/claim", qoderCampaignsPath, campaign.id))
	if err != nil {
		return Result{}, err
	}
	// claim 响应形状未在 ref 固化：只做保守判读——顶层 success/errorMessage。
	if success, ok := claim["success"].(bool); ok && !success {
		msg := jsonString(claim["errorMessage"])
		if msg == "" {
			msg = jsonString(claim["message"])
		}
		if msg == "" {
			msg = "上游未给原因"
		}
		return Result{Status: StatusFailed, Message: "签到失败：" + msg}, nil
	}
	reward := campaign.reward
	if reward == "" {
		reward = "100 credits" // ref：每日签到每签一次发一个 100 credits、30 天有效的包
	}
	return Result{Status: StatusOK, Reward: reward,
		Message: "签到成功，获得 " + reward + "（落入个人资源包，30 天有效）"}, nil
}

// qoderCampaign 活动列表条目里签到需要的字段（形状防御性提取——ref 未固化
// 列表条目结构，形状不符时显式报"接口可能已变更"，P0-4）。
type qoderCampaign struct {
	id     string
	reward string
}

// pickClaimableCampaign 从活动列表里找可领取的 CLAIM_BENEFIT。
//   - 找到可领取的 → 返回条目；
//   - 有 CLAIM_BENEFIT 但全部已领取 → 返回 nil（上层走 already 文案）；
//   - 列表形状无法解析 → 返回错误（显式告警）。
func pickClaimableCampaign(list map[string]any) (*qoderCampaign, error) {
	items, ok := list["data"].([]any)
	if !ok {
		// 上游可能直接给数组（无 data 信封）。
		if arr, isArr := list["data"]; isArr {
			_ = arr
		}
		return nil, fmt.Errorf("平台接口可能已变更（活动列表缺少 data 数组）")
	}
	var claimable *qoderCampaign
	sawBenefit := false
	for _, item := range items {
		m, isMap := item.(map[string]any)
		if !isMap {
			continue
		}
		if actionType := jsonString(m["actionType"]); actionType != qoderClaimAction {
			continue
		}
		sawBenefit = true
		id := jsonString(m["id"])
		if id == "" {
			id = jsonString(m["campaignId"])
		}
		if id == "" {
			continue
		}
		status := strings.ToLower(jsonString(m["claimStatus"]))
		switch status {
		case "claimed", "received", "done":
			continue // 已领取
		case "", "claimable", "pending":
			// 可领取（status 缺失 = 上游没给口径，按可领取尝试——claim 接口
			// 自身幂等，重复领取会被拒并给原因）。
			reward := jsonString(m["rewardName"])
			if reward == "" {
				reward = jsonString(m["reward"])
			}
			claimable = &qoderCampaign{id: id, reward: reward}
			break
		}
	}
	if !sawBenefit && claimable == nil {
		// 列表里连 CLAIM_BENEFIT 都没有：形状可疑，但"活动未开放"也是合法态。
		return nil, nil
	}
	return claimable, nil
}

// sashGet / sashPost sash 域薄封装（最小头束、无签名；消息已脱敏）。
func (c *qoderChecker) sashGet(ctx context.Context, token, path string) (map[string]any, error) {
	return c.sashDo(ctx, http.MethodGet, token, path, nil)
}

func (c *qoderChecker) sashPost(ctx context.Context, token, path string) (map[string]any, error) {
	return c.sashDo(ctx, http.MethodPost, token, path, map[string]any{})
}

func (c *qoderChecker) sashDo(ctx context.Context, method, token, path string, body any) (map[string]any, error) {
	var rdr *strings.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("序列化请求体失败")
		}
		rdr = strings.NewReader(string(raw))
	} else {
		rdr = strings.NewReader("")
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.Base, "/")+path, rdr)
	if err != nil {
		return nil, fmt.Errorf("构造签到请求失败")
	}
	for _, h := range qoderSashHeaders(token) {
		req.Header.Set(h[0], h[1])
	}
	resp, err := c.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("签到请求失败（网络）")
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, authExpiredf("HTTP %d", resp.StatusCode)
	}
	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("平台接口可能已变更（响应不是 JSON）")
	}
	return payload, nil
}
