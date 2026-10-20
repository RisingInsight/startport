// trae_balance.go TRAE（SOLO 国内）余额查询：订阅包口径（第二批）。
//
// 契约来源（⚠ 未真实账号验收，参考实现逆向）：POST /trae/api/v2/pay/ide_user_ent_usage
// （v1 兜底，空体）；余额 = 全部订阅包（credits_limit - usage.credits_amount）求和
// （AgentHub 实测：老实现只取单一档位包口径是错的）。401 + code 1001 = 「积分服务
// 未对该账号开放」——可用性缺失而非凭证失效（同一 token 在对话域完全正常），不判
// 登录态失效；无 1001 的 401 才是登录态失效。
package checkin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/RisingInsight/startport/internal/account"
)

const (
	traeEntUsagePathV2 = "/trae/api/v2/pay/ide_user_ent_usage"
	traeEntUsagePathV1 = "/trae/api/v1/pay/ide_user_ent_usage"
	traePacksKey       = "user_entitlement_pack_list"
)

// queryTraeBalance TRAE 余额统一入口（QueryBalance 分发）。
func queryTraeBalance(ctx context.Context, a *account.Account) (*account.BalanceSnapshot, error) {
	blob, err := decodeBlob(a)
	if err != nil {
		return nil, err
	}
	token := blobString(blob, "accessToken")
	if token == "" {
		return nil, fmt.Errorf("blob 缺 accessToken（请重新登录）")
	}
	return queryTraeBalanceAt(ctx, traeCheckinBase, token, blobString(blob, "deviceId"))
}

// queryTraeBalanceAt 余额查询（base 可注入 httptest）。
func queryTraeBalanceAt(ctx context.Context, base, token, deviceID string) (*account.BalanceSnapshot, error) {
	client := &http.Client{Timeout: traeClientTimeout}
	var lastErr string
	for _, path := range []string{traeEntUsagePathV2, traeEntUsagePathV1} {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			strings.TrimRight(base, "/")+path, strings.NewReader("{}"))
		if err != nil {
			return nil, err
		}
		for k, v := range traeHeaders(token, deviceID) {
			req.Header.Set(k, v)
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()

		var env struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal(raw, &env)
		if (resp.StatusCode == 401 || resp.StatusCode == 403) && env.Code == 1001 {
			return nil, fmt.Errorf("积分服务未对该账号开放（官方接口 code 1001）")
		}
		if resp.StatusCode == 401 {
			return nil, authExpiredf("TRAE 登录态失效（HTTP 401）")
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 || (env.Code != 0 && env.Code != 200) {
			lastErr = traeFirst(env.Message, fmt.Sprintf("HTTP %d", resp.StatusCode))
			continue
		}
		packs := traeFindPacks(raw)
		if len(packs) == 0 {
			lastErr = "上游未返回订阅包"
			continue
		}
		snap := traePacksToSnapshot(packs)
		if snap != nil {
			return snap, nil
		}
		lastErr = "订阅包解析为空"
	}
	return nil, fmt.Errorf("余额查询失败: %s", traeFirst(lastErr, "上游无可用响应"))
}

// traeFindPacks 在顶层与 data 层找 user_entitlement_pack_list（两形态兼容）。
func traeFindPacks(raw []byte) []any {
	var env map[string]any
	if json.Unmarshal(raw, &env) != nil {
		return nil
	}
	if list, ok := env[traePacksKey].([]any); ok {
		return list
	}
	if data, ok := env["data"].(map[string]any); ok {
		if list, ok := data[traePacksKey].([]any); ok {
			return list
		}
	}
	return nil
}

// traePacksToSnapshot 订阅包 → 余额快照：全部包（limit - used）求和，
// ExpiresAt 取最早的到期时刻（面板「最早到期」口径）。
func traePacksToSnapshot(packs []any) *account.BalanceSnapshot {
	snap := &account.BalanceSnapshot{Unit: "credits", FetchedAt: time.Now()}
	for i, p := range packs {
		pack, ok := p.(map[string]any)
		if !ok {
			continue
		}
		limit, hasLimit := traeNum(pack, "credits_limit")
		if !hasLimit {
			limit, hasLimit = traeNum(pack, "total_credit")
		}
		var used int64
		if usage, ok := pack["usage"].(map[string]any); ok {
			used, _ = traeNum(usage, "credits_amount")
		}
		var credits int64
		switch {
		case hasLimit && limit > 0:
			credits = limit - used
			if credits < 0 {
				credits = 0
			}
		default:
			// 新版字段缺失：退化到余量型字段（remain/balance/left/available/credits/quota）。
			for _, k := range []string{"remain", "balance", "left", "available", "credits", "quota"} {
				if n, ok := traeNum(pack, k); ok && n > 0 {
					credits = n
					break
				}
			}
		}
		label := traeFirst(
			traeStr(pack, "entitlement_pack_id", "pack_id", "id"),
			fmt.Sprintf("pack-%d", i+1))
		row := account.BalanceRow{Name: label, Available: float64(credits), Unit: "credits"}
		if end, ok := traeNum(pack, "end_time"); ok && end > 0 {
			row.Total = float64(credits)
			if t := traeToTime(end); !t.IsZero() && (snap.ExpiresAt.IsZero() || t.Before(snap.ExpiresAt)) {
				snap.ExpiresAt = t
			}
		}
		snap.Rows = append(snap.Rows, row)
		snap.Available += float64(credits)
	}
	if len(snap.Rows) == 0 {
		return nil
	}
	return snap
}

// traeToTime 秒/毫秒双形态时间戳 → time（毫秒判定同 traeExpiresAt）。
func traeToTime(n int64) time.Time {
	if n <= 0 {
		return time.Time{}
	}
	if n > 1<<36 {
		return time.Unix(n/1000, 0)
	}
	return time.Unix(n, 0)
}
