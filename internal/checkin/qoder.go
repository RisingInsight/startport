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
	"github.com/RisingInsight/startport/internal/netdiag"
	"net/http"
	"sort"
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
	// 业务码先行（真实账号联调修复）：真实上游的信封带业务码字段时按业务错误
	// 报（含码值与 msg），**不要**误落到"形状已变更"。
	switch kind, code, msg := qoderBizVerdict(list); kind {
	case qoderBizAlready:
		// 已领取/已参与：上游在列表层就拒绝重复领取。目标已达成，不算失败。
		// 数据保留利用（team-lead 修复 2）：响应若仍带活动列表/奖励信息，
		// 解析用于 Message 丰富化（"今日已领取 100 credits"），不丢弃。
		if text := claimedRewardText(list); text != "" {
			return Result{Status: StatusAlready,
				Message: qoderAlreadyMessage(code) + "；" + text}, nil
		}
		return Result{Status: StatusAlready,
			Message: qoderAlreadyMessage(code)}, nil
	case qoderBizFailed:
		return Result{Status: StatusFailed, Message: "签到失败：" + msg}, nil
	}
	// 容错提取活动数组（真实形状：data 可能是对象嵌套 list 等）。
	items, ok := campaignsItems(list)
	if !ok {
		// 形状诊断（脱敏：只含键名与类型，不含值）——用户下次重试能把关键
		// 信息带回来（team-lead 修复 1，永久保留）。
		return Result{Status: StatusFailed,
			Message: "平台接口可能已变更（活动列表形状无法解析：" + shapeDiagnosis(list) + "）"}, nil
	}
	campaign, err := pickClaimableCampaign(items)
	if err != nil {
		return Result{Status: StatusFailed, Message: err.Error()}, nil
	}
	if campaign == nil {
		// 列表里没有可领取的 CLAIM_BENEFIT：区分"今日已领取"与"确实没有活动"。
		// 旧文案统一写"可能已领过"会让用户把 already 误读成失败（2026-10 反馈）。
		if reward := claimedBenefitReward(items); reward != "" {
			return Result{Status: StatusAlready, Reward: reward,
				Message: "今日已领取 " + reward + "，无需重复签到"}, nil
		}
		return Result{Status: StatusAlready,
			Message: "当前账号没有可领取的签到活动（Free 套餐或活动未开放）"}, nil
	}
	// 跳 2：领取。
	claim, err := c.sashPost(ctx, token,
		fmt.Sprintf("%s/%s/claim", qoderCampaignsPath, campaign.id))
	if err != nil {
		return Result{}, err
	}
	// claim 响应同样业务码先行（与 campaigns 同一口径）。
	switch kind, code, msg := qoderBizVerdict(claim); kind {
	case qoderBizAlready:
		// 重复领取：真实账号实测 claim 在已签过时返回 {"code":"CLAIMED"}
		// （v1.7.5）。这不是失败——签到目标已达成；判 failed 会让面板标红、
		// 并让调度器每 30 分钟无意义重试直到耗尽当日重试额度。
		// 数据保留利用：claim 响应若带奖励信息，一并进 Message。
		if text := claimedRewardText(claim); text != "" {
			return Result{Status: StatusAlready,
				Message: qoderAlreadyMessage(code) + "；" + text}, nil
		}
		return Result{Status: StatusAlready,
			Message: qoderAlreadyMessage(code)}, nil
	case qoderBizFailed:
		return Result{Status: StatusFailed, Message: "签到失败：" + msg}, nil
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
		// 部分上游不用 code 表达"已领取"，只在文案里说明（已领取/already）。
		if qoderAlreadyMessage(msg) != "" {
			return Result{Status: StatusAlready,
				Message: qoderAlreadyMessage(msg)}, nil
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

// pickClaimableCampaign 从活动条目数组里找可领取的 CLAIM_BENEFIT。
//   - 找到可领取的 → 返回条目；
//   - 有 CLAIM_BENEFIT 但全部已领取 → 返回 nil（上层走 already 文案）；
//   - 列表形状无法解析 → 返回错误（显式告警）。
func pickClaimableCampaign(items []any) (*qoderCampaign, error) {
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

// 业务码三态判定结果（qoderBizVerdict 返回值）。
const (
	qoderBizOK      = "ok"      // 成功口径（或缺字段）：继续走形状解析
	qoderBizAlready = "already" // 已领取/已参与：目标已达成，不计失败
	qoderBizFailed  = "failed"  // 真实业务错误
)

// qoderAlreadyCodes 已被拒为"已领取"的上游业务码（大小写不敏感）。
// CLAIMED 是真实账号实测见到的形态（重复 claim 时返回），其余为同族常见写法。
// 保守口径：只收窄到"已领取/已参与"语义，**不**把"活动结束/风控"等会吞掉
// 真实失败的码放进来——宁可按失败报，也别把失败洗成已签到。
var qoderAlreadyCodes = map[string]bool{
	"claimed":         true,
	"already_claimed": true,
	"alreadyclaimed":  true,
	"already":         true,
	"received":        true,
	"done":            true,
	"duplicate":       true,
	"claimed_today":   true, // team-lead 修复 1：CLAIMED 家族变体（真实联调补齐）
	"already_claim":   true,
}

// claimedBenefitReward 从活动列表里找已领取的 CLAIM_BENEFIT 奖励文案
// （真实形状：claimStatus=CLAIMED + benefit.amount/validity.days）。
func claimedBenefitReward(items []any) string {
	for _, item := range items {
		m, ok := item.(map[string]any)
		if !ok || jsonString(m["actionType"]) != qoderClaimAction {
			continue
		}
		switch strings.ToLower(jsonString(m["claimStatus"])) {
		case "claimed", "received", "done":
			if text := campaignBenefitText(m); text != "" {
				return text
			}
			return "100 credits"
		}
	}
	return ""
}

// campaignBenefitText 活动条目的奖励描述（benefit.amount + validity.days，
// 或 rewardName/reward 兜底）。
func campaignBenefitText(m map[string]any) string {
	if benefit, ok := m["benefit"].(map[string]any); ok {
		amount := jsonFloat(benefit["amount"])
		if amount > 0 {
			unit := strings.ToLower(jsonString(benefit["kind"]))
			if unit == "" {
				unit = "credits"
			}
			text := fmt.Sprintf("%.0f %s", amount, unit)
			if validity, ok := benefit["validity"].(map[string]any); ok {
				if days := int(jsonFloat(validity["days"])); days > 0 {
					text += fmt.Sprintf("（%d 天有效）", days)
				}
			}
			return text
		}
	}
	if name := jsonString(m["rewardName"]); name != "" {
		return name
	}
	return jsonString(m["reward"])
}

// claimedRewardText 从已领取响应里提取奖励信息（Message 丰富化用；无 → 空）。
// 来源两路：顶层/data 的 rewardPoints 数值（真实响应常见），活动条目里的
// rewardName/reward 文案。脱敏口径：只取奖励描述，不带用户信息。
func claimedRewardText(payload map[string]any) string {
	var parts []string
	if n := qoderRewardPoints(payload); n > 0 {
		parts = append(parts, fmt.Sprintf("今日已领取 %.0f credits", n))
	}
	if items, ok := campaignsItems(payload); ok {
		for _, item := range items {
			m, isMap := item.(map[string]any)
			if !isMap {
				continue
			}
			if jsonString(m["actionType"]) != qoderClaimAction {
				continue
			}
			if r := campaignBenefitText(m); r != "" {
				parts = append(parts, "活动奖励："+r)
				break
			}
		}
	}
	return strings.Join(parts, "；")
}

// qoderRewardPoints 顶层/data 的奖励点数（数字；无 → 0）。
func qoderRewardPoints(payload map[string]any) float64 {
	if v, ok := payload["rewardPoints"].(float64); ok {
		return v
	}
	if obj, ok := payload["data"].(map[string]any); ok {
		if v, ok2 := obj["reward_points"].(float64); ok2 {
			return v
		}
	}
	return 0
}

// qoderAlreadyCode 报告业务码是否表达"已领取"（幂等成功态）。
func qoderAlreadyCode(code string) bool {
	return qoderAlreadyCodes[strings.ToLower(strings.TrimSpace(code))]
}

// qoderAlreadyMessage 把上游的已领取证据翻成用户可读文案。hint 既可以是业务码
// （CLAIMED），也可以是普通文案（errorMessage）——后者只按关键词归一，避免把
// "活动已结束"这类真实失败洗成已签到。
// 返回空串 = 不是"已领取"。
func qoderAlreadyMessage(hint string) string {
	s := strings.TrimSpace(hint)
	if s == "" {
		return ""
	}
	if qoderAlreadyCode(s) {
		return "今天已领取过（上游业务码 " + strings.ToUpper(s) + "），无需重复签到"
	}
	low := strings.ToLower(s)
	switch {
	case strings.Contains(low, "claimed"),
		strings.Contains(s, "已领取"),
		strings.Contains(s, "已签到"),
		strings.Contains(low, "already"):
		return "今天已领取过，无需重复签到"
	}
	return ""
}

// qoderBizVerdict 业务码先行（真实账号联调修复 · team-lead 修复 2 的延伸）：
// campaigns/claim 信封若带业务码字段（code/status/statusCode），返回
// (已领取 / 失败 / ok) 三态——不再把"已领取"误判成失败。
// 成功值口径：缺省 / 0 / "0" / "000000" / "success" / "ok"（涵盖数字码与
// 字符串码两族——ref 信封与 loomy 同款字符串码都见过）。
func qoderBizVerdict(payload map[string]any) (kind, code, msg string) {
	for _, key := range []string{"code", "status", "statusCode"} {
		v, ok := payload[key]
		if !ok {
			continue
		}
		code := jsonString(v)
		if code == "" {
			if n := jsonInt64(v); n != 0 {
				code = fmt.Sprintf("%d", n)
			} else if _, isNum := v.(float64); isNum {
				code = "0"
			}
		}
		switch strings.ToLower(code) {
		case "", "0", "000000", "success", "ok":
			return qoderBizOK, code, "" // 成功口径：继续解析
		}
		if qoderAlreadyCode(code) {
			return qoderBizAlready, code, ""
		}
		msg := jsonString(payload["msg"])
		if msg == "" {
			msg = jsonString(payload["message"])
		}
		if msg == "" {
			msg = jsonString(payload["errorMessage"])
		}
		if msg == "" {
			msg = "上游未给原因"
		}
		// 码本身不认识，但文案说"已领取"时仍按已领取归一（部分上游只在
		// message 里说明，code 给的是通用错误码）。
		if m := qoderAlreadyMessage(msg); m != "" {
			return qoderBizAlready, code, m
		}
		return qoderBizFailed, code, fmt.Sprintf("业务码 %s：%s", code, msg)
	}
	return qoderBizOK, "", "" // 无业务码字段：交给形状解析
}

// campaignsItems 容错提取活动数组（真实账号联调修复 · team-lead 修复 3）：
//   - data 为数组（ref/实现原口径）；
//   - data 为对象 → 常见嵌套键 list / campaigns / items / records；
//   - 顶层 campaigns 键（数组或对象嵌套同上）；
//   - 顶层直接是数组（sashDo 解成 map 前的形态，由调用方处理——见 sashDo）。
//
// 找不到 → false（调用方报形状变更 + 诊断）。
func campaignsItems(payload map[string]any) ([]any, bool) {
	if arr, ok := payload["data"].([]any); ok {
		return arr, true
	}
	if obj, ok := payload["data"].(map[string]any); ok {
		for _, key := range []string{"list", "campaigns", "items", "records"} {
			if arr, ok := obj[key].([]any); ok {
				return arr, true
			}
		}
	}
	if arr, ok := payload["campaigns"].([]any); ok {
		return arr, true
	}
	if obj, ok := payload["campaigns"].(map[string]any); ok {
		for _, key := range []string{"list", "items", "records"} {
			if arr, ok := obj[key].([]any); ok {
				return arr, true
			}
		}
	}
	return nil, false
}

// ── token 刷新（账号中心刷新闭环）──────────────────────────────
//
// 设备流令牌（dt-/drt-）与作业令牌（jt-）走不同刷新路径，且**都不需要
// COSY 签名**（login-flows-spec §2.1「令牌续期」）。blob 里的 refresh_token
// 是打包串 `<refresh>|<user_id>|<machine_id>`（OAuth）或 PAT 的五段形态——
// 原样回传；若上游返回裸串，则按同一格式重新打包，保证下次刷新仍可用。
const (
	qoderDeviceRefreshPath = "/api/v1/deviceToken/refresh"
	qoderJobRefreshPath    = "/api/v1/jobToken/refresh"
)

// qoderOpenAPIBase 双区 openapi 基址（刷新/登录/用户信息同域）。
// var 而非 const：测试可指向 httptest（与 autoclawUserAPIBase 同款）。
var qoderOpenAPIBase = map[string]string{
	account.PlatformQoder:     "https://openapi.qoder.com.cn",
	account.PlatformQoderIntl: "https://openapi.qoder.sh",
}

// RefreshQoder 刷新 Qoder 双区 access token，返回更新后的 blob。
// 回写由调用方完成（scheduler.refreshAndWriteback 持有 Store）。
func RefreshQoder(ctx context.Context, a *account.Account) (map[string]any, error) {
	base := strings.TrimRight(qoderOpenAPIBase[a.Platform], "/")
	if base == "" {
		return nil, fmt.Errorf("平台 %s 未配置 Qoder 刷新端点", a.Platform)
	}
	return refreshQoderAt(ctx, base, a)
}

// refreshQoderAt 刷新请求本体（base 可注入 httptest）。
func refreshQoderAt(ctx context.Context, base string, a *account.Account) (map[string]any, error) {
	blob, err := decodeBlob(a)
	if err != nil {
		return nil, err
	}
	refresh := blobString(blob, "refresh_token", "refreshToken")
	if refresh == "" {
		return nil, fmt.Errorf("没有刷新凭证（账号 blob 缺 refresh_token）")
	}
	access := blobString(blob, "access_token", "accessToken")
	path := qoderDeviceRefreshPath
	if strings.HasPrefix(access, "jt-") {
		path = qoderJobRefreshPath
	}
	raw, err := json.Marshal(map[string]any{"refresh_token": refresh})
	if err != nil {
		return nil, fmt.Errorf("序列化刷新请求失败")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(base, "/")+path, strings.NewReader(string(raw)))
	if err != nil {
		return nil, fmt.Errorf("构造刷新请求失败")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "qoder-local-proxy")
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("刷新请求失败（网络）%s，请检查网络后重试", netdiag.ProxyHint(err))
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return nil, authExpiredf("刷新 HTTP %d", resp.StatusCode)
	case resp.StatusCode == http.StatusTooManyRequests:
		return nil, fmt.Errorf("Qoder token 刷新被限流，请稍后重试")
	case resp.StatusCode >= 400:
		return nil, fmt.Errorf("Qoder token 刷新失败（HTTP %d）", resp.StatusCode)
	}
	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("刷新响应形状不符（平台接口可能已变更）")
	}
	// 业务码字符串/数字双兼容（与 qoderBizVerdict 同口径）：上游可能回
	// {"code":105} 或 {"code":"105"}，只解析数字会漏判登录态失效。
	for _, key := range []string{"code", "status", "statusCode"} {
		raw, ok := payload[key]
		if !ok {
			continue
		}
		code := jsonString(raw)
		if code == "" {
			if n := jsonInt64(raw); n != 0 {
				code = fmt.Sprintf("%d", n)
			}
		}
		switch strings.ToLower(strings.TrimSpace(code)) {
		case "", "0", "000000", "success", "ok":
			continue
		}
		msg := jsonString(payload["msg"])
		if msg == "" {
			msg = jsonString(payload["message"])
		}
		switch code {
		case "105", "401", "410000":
			return nil, authExpiredf("刷新业务码 %s %s", code, msg)
		}
		return nil, fmt.Errorf("Qoder token 刷新失败（业务码 %s）%s", code, msg)
	}
	data, _ := payload["data"].(map[string]any)
	pick := func(keys ...string) string {
		for _, k := range keys {
			if s := jsonString(payload[k]); s != "" {
				return s
			}
			if data != nil {
				if s := jsonString(data[k]); s != "" {
					return s
				}
			}
		}
		return ""
	}
	newToken := pick("access_token", "accessToken", "token")
	if newToken == "" {
		return nil, fmt.Errorf("Qoder token 刷新响应缺少 token（平台接口可能已变更）")
	}
	blob["access_token"] = newToken
	if newRefresh := pick("refresh_token", "refreshToken"); newRefresh != "" {
		if !strings.Contains(newRefresh, "|") {
			uid := blobString(blob, "user_id", "userId")
			mid := blobString(blob, "machine_id", "machineId")
			if uid != "" && mid != "" {
				newRefresh = newRefresh + "|" + uid + "|" + mid
			}
		}
		blob["refresh_token"] = newRefresh
	}
	if exp := pick("expires_at", "expiresAt"); exp != "" {
		blob["expires_at"] = exp
	}
	return blob, nil
}

// shapeDiagnosis 形状诊断（真实账号联调修复 · team-lead 修复 1，永久保留）：
// 报**实际响应顶层键列表与 data 字段实际类型**——脱敏口径：只含键名/类型，
// 绝不含值（值里可能有 token/用户信息）。用户重试失败时把诊断带回即可定位。
func shapeDiagnosis(payload map[string]any) string {
	keys := make([]string, 0, len(payload))
	for k := range payload {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	diag := "顶层键=[" + strings.Join(keys, ",") + "]"
	v, ok := payload["data"]
	if !ok {
		return diag + "，data=缺失"
	}
	switch typed := v.(type) {
	case []any:
		diag += "，data=数组"
	case map[string]any:
		inner := make([]string, 0, len(typed))
		for k := range typed {
			inner = append(inner, k)
		}
		sort.Strings(inner)
		diag += "，data=对象（键=[" + strings.Join(inner, ",") + "]）"
	case string:
		diag += "，data=字符串"
	case float64:
		diag += "，data=数字"
	case bool:
		diag += "，data=布尔"
	case nil:
		diag += "，data=null"
	default:
		diag += "，data=未知"
	}
	return diag
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
	// 顶层可能直接是数组（无信封）——先探 raw，数组则包成 {"data":[…]} 交给
	// 同一条解析链（team-lead 修复 3 的顶层数组形态）。
	var raw json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("平台接口可能已变更（响应不是 JSON）")
	}
	trimmed := strings.TrimSpace(string(raw))
	if strings.HasPrefix(trimmed, "[") {
		var arr []any
		if err := json.Unmarshal(raw, &arr); err != nil {
			return nil, fmt.Errorf("平台接口可能已变更（响应数组无法解析）")
		}
		return map[string]any{"data": arr}, nil
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("平台接口可能已变更（响应不是 JSON 对象）")
	}
	return payload, nil
}
