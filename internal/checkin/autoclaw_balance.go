// autoclaw_balance.go AutoClaw 双区积分查询。
//
// 主链路：GET {userapi}/agent-assetmgr/api/v2/wallets?biz_app_id=autoclaw
//
//	→ {code:0, data:{total_balance, wallets:[{public_wallet_type, display_name,
//	   balance, display, priority, …}]}}
//
// userapi 域需要 X-Auth-Sign 签名头（与登录/签到同一份实现）。余额只读展示，
// 不触发 refresh；401 由调用方决定是否刷新后重试。
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
	autoclawWalletsPath      = "/agent-assetmgr/api/v2/wallets?biz_app_id=autoclaw"
	autoclawWalletLegacyPath = "/agent-assetmgr/api/v1/wallet-instances?wallet_type=all&wallet_scope=all"
)

func queryAutoClawBalance(ctx context.Context, a *account.Account) (*account.BalanceSnapshot, error) {
	blob, err := decodeBlob(a)
	if err != nil {
		return nil, err
	}
	token := blobString(blob, "token")
	if token == "" {
		return nil, authExpiredf("账号 blob 缺少 token")
	}
	intl := a.Platform == account.PlatformAutoClawIntl
	base := strings.TrimRight(autoclawUserAPIBase[intl], "/")
	return queryAutoClawBalanceAt(ctx, base, intl, token)
}

func queryAutoClawBalanceAt(ctx context.Context, base string, intl bool, token string) (*account.BalanceSnapshot, error) {
	checker := newAutoClawChecker(intl)
	checker.Base = base
	payload, err := autoclawBalanceGet(ctx, checker, token, autoclawWalletsPath)
	if err != nil {
		return nil, err
	}
	data, _ := payload["data"].(map[string]any)
	wallets, _ := data["wallets"].([]any)
	if len(wallets) == 0 {
		// v2 拿不到钱包时降级 v1（与参考实现同口径）。
		legacy, lerr := autoclawBalanceGet(ctx, checker, token, autoclawWalletLegacyPath)
		if lerr != nil {
			return nil, lerr
		}
		ldata, _ := legacy["data"].(map[string]any)
		wallets, _ = ldata["wallet_instances"].([]any)
		if len(wallets) == 0 {
			wallets, _ = ldata["wallets"].([]any)
		}
		data = ldata
		if len(wallets) == 0 {
			return nil, fmt.Errorf("积分查询失败：上游未返回钱包")
		}
	}
	rows := make([]struct {
		priority int64
		row      account.BalanceRow
	}, 0, len(wallets))
	sum := 0.0
	for _, raw := range wallets {
		w, ok := raw.(map[string]any)
		if !ok || !autoclawTruthy(w["display"]) {
			continue
		}
		balance := jsonFloat(w["balance"])
		sum += balance
		name := jsonString(w["display_name"])
		if name == "" {
			name = jsonString(w["public_wallet_type"])
		}
		if name == "" {
			name = "积分"
		}
		priority := int64(jsonFloat(w["priority"]))
		rows = append(rows, struct {
			priority int64
			row      account.BalanceRow
		}{priority, account.BalanceRow{Name: name, Available: balance, Unit: "积分"}})
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].priority < rows[j].priority })
	out := make([]account.BalanceRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.row)
	}
	total := sum
	if v, ok := data["total_balance"]; ok && v != nil {
		total = jsonFloat(v)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("积分查询失败：上游钱包为空")
	}
	return &account.BalanceSnapshot{Available: total, Unit: "积分", Rows: out, FetchedAt: time.Now()}, nil
}

func autoclawBalanceGet(ctx context.Context, checker *autoclawChecker, token, path string) (map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(checker.Base, "/")+path, nil)
	if err != nil {
		return nil, fmt.Errorf("构造积分请求失败")
	}
	for _, h := range checker.signedAuthHeaders(token) {
		req.Header.Set(h[0], h[1])
	}
	resp, err := checker.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("积分请求失败（网络）%s", netdiag.ProxyHint(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, authExpiredf("HTTP %d", resp.StatusCode)
	}
	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("积分响应形状不符（平台接口可能已变更）")
	}
	code := int64(jsonFloat(payload["code"]))
	if code == 410000 || code == 400000 {
		return nil, authExpiredf("业务码 %d", code)
	}
	if code != 0 {
		msg := jsonString(payload["msg"])
		if msg == "" {
			msg = "上游未给原因"
		}
		return nil, fmt.Errorf("积分查询失败（业务码 %d）：%s", code, msg)
	}
	return payload, nil
}

func autoclawTruthy(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case float64:
		return x != 0
	case string:
		return strings.TrimSpace(x) != ""
	case nil:
		return false
	default:
		return true
	}
}
