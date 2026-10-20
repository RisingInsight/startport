// qoder_balance.go Qoder 双区余额查询（账号中心 Hero 的「积分 / 余额」列）。
//
// 上游 `GET {openapi}/api/v2/quota/usage` 返回三个桶：
//   - userQuota：订阅套餐额度（total/used/remaining/unit）；
//   - addOnQuota：个人资源包（签到领的 credits 在这里；上游没有时整键缺失）；
//   - orgResourcePackage：组织资源包（cap/remaining/available；空桶也会返回，需看 available）。
//
// available = 三个桶的 remaining 之和（组织包仅 available=true 时计入）。
// 套餐名走 `GET /api/v2/user/plan` 的 plan_tier_name，失败不影响余额。
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
	qoderUsagePath = "/api/v2/quota/usage"
	qoderPlanPath  = "/api/v2/user/plan"
)

// BalanceSupported 平台是否有余额查询实现（能力事实来自 account.PlatformSpec）。
func BalanceSupported(platform string) bool {
	s, ok := account.SpecOf(platform)
	return ok && s.Balance
}

// QueryBalance 平台余额统一入口。
func QueryBalance(ctx context.Context, a *account.Account) (*account.BalanceSnapshot, error) {
	if a == nil {
		return nil, fmt.Errorf("账号为空")
	}
	switch a.Platform {
	case account.PlatformQoder, account.PlatformQoderIntl:
		return queryQoderBalance(ctx, a)
	case account.PlatformLoomy:
		return queryLoomyBalance(ctx, a)
	case account.PlatformAutoClaw, account.PlatformAutoClawIntl:
		return queryAutoClawBalance(ctx, a)
	default:
		return nil, ErrPlatformNotSupported
	}
}

func queryQoderBalance(ctx context.Context, a *account.Account) (*account.BalanceSnapshot, error) {
	blob, err := decodeBlob(a)
	if err != nil {
		return nil, err
	}
	token := blobString(blob, "access_token", "accessToken")
	if token == "" {
		return nil, authExpiredf("账号 blob 缺少 access_token")
	}
	base := strings.TrimRight(qoderOpenAPIBase[a.Platform], "/")
	if base == "" {
		return nil, fmt.Errorf("平台 %s 未配置 Qoder 余额端点", a.Platform)
	}
	payload, err := qoderOpenAPIGet(ctx, base+qoderUsagePath, token)
	if err != nil {
		return nil, err
	}
	snap := parseQoderBalance(payload)
	// 套餐名是补充信息：失败静默，不影响余额主链路。
	if plan, perr := qoderOpenAPIGet(ctx, base+qoderPlanPath, token); perr == nil {
		if name := jsonString(plan["plan_tier_name"]); name != "" {
			snap.PlanName = name
		}
	}
	return snap, nil
}

func qoderOpenAPIGet(ctx context.Context, url, token string) (map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("构造余额请求失败")
	}
	for _, h := range [][2]string{
		{"Cosy-Version", "1.0.1"},
		{"Cosy-ClientType", "5"},
		{"User-Agent", "qoder-local-proxy"},
		{"Accept", "application/json"},
		{"Authorization", "Bearer " + token},
	} {
		req.Header.Set(h[0], h[1])
	}
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("余额请求失败（网络）")
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return nil, authExpiredf("HTTP %d", resp.StatusCode)
	case resp.StatusCode == http.StatusTooManyRequests:
		return nil, fmt.Errorf("余额查询被限流，请稍后重试")
	case resp.StatusCode >= 400:
		return nil, fmt.Errorf("余额查询失败（HTTP %d）", resp.StatusCode)
	}
	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("余额响应形状不符（平台接口可能已变更）")
	}
	return payload, nil
}

// parseQoderBalance 三桶合成 BalanceSnapshot（纯函数，测试直接喂 JSON）。
func parseQoderBalance(payload map[string]any) *account.BalanceSnapshot {
	snap := &account.BalanceSnapshot{FetchedAt: time.Now()}
	unit := "credits"
	if user, ok := payload["userQuota"].(map[string]any); ok {
		if u := jsonString(user["unit"]); u != "" {
			unit = u
		}
		remaining := jsonFloat(user["remaining"])
		total := jsonFloat(user["total"])
		snap.Available += remaining
		snap.Total += total
		snap.Rows = append(snap.Rows, account.BalanceRow{Name: "个人套餐", Available: remaining, Total: total, Unit: unit})
	}
	if addon, ok := payload["addOnQuota"].(map[string]any); ok {
		remaining := jsonFloat(addon["remaining"])
		total := jsonFloat(addon["total"])
		used := jsonFloat(addon["used"])
		if remaining > 0 || total > 0 || used > 0 {
			snap.Available += remaining
			snap.Total += total
			snap.Rows = append(snap.Rows, account.BalanceRow{Name: "个人资源包", Available: remaining, Total: total, Unit: unit})
		}
	}
	if org, ok := payload["orgResourcePackage"].(map[string]any); ok {
		if available, _ := org["available"].(bool); available {
			remaining := jsonFloat(org["remaining"])
			cap := jsonFloat(org["cap"])
			snap.Available += remaining
			snap.Total += cap
			snap.Rows = append(snap.Rows, account.BalanceRow{Name: "组织资源包", Available: remaining, Total: cap, Unit: unit})
		}
	}
	snap.Unit = unit
	// expiresAt 是毫秒时间戳；Qoder 国际 Free 会返回 9999 年哨兵，不能当成真实到期。
	if ms := jsonFloat(payload["expiresAt"]); ms > 0 {
		if exp := time.UnixMilli(int64(ms)); exp.Year() < 9000 {
			snap.ExpiresAt = exp
		}
	}
	return snap
}

// jsonFloat 宽松数字取值（数字/数字字符串都认）。
func jsonFloat(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case json.Number:
		f, _ := n.Float64()
		return f
	case string:
		var f float64
		if _, err := fmt.Sscanf(strings.TrimSpace(n), "%f", &f); err == nil {
			return f
		}
	}
	return 0
}
