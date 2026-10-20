// loomy_balance.go Loomy 积分查询。
//
// Loomy 没有独立余额接口：客户端读 V2 流水接口的摘要字段：
//
//	GET {integration}/api/v2/points/records?pageNo=1&pageSize=1&direction=debit&granularity=chat
//	→ {code:"000000", data:{balance, dailyBalance, dailyRemainingPoints, …}}
//
// balance=永久积分，dailyBalance=每日赠送积分；账户页把两者相加作为可用总量。
package checkin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/RisingInsight/startport/internal/account"
	"github.com/RisingInsight/startport/internal/provider"
)

const loomyRecordsPath = "/api/v2/points/records?pageNo=1&pageSize=1&direction=debit&granularity=chat"

func queryLoomyBalance(ctx context.Context, a *account.Account) (*account.BalanceSnapshot, error) {
	blob, err := decodeBlob(a)
	if err != nil {
		return nil, err
	}
	session := blobString(blob, "session")
	if session == "" {
		return nil, authExpiredf("账号 blob 缺少 session")
	}
	return queryLoomyBalanceAt(ctx, loomyBase, session)
}

func queryLoomyBalanceAt(ctx context.Context, base, session string) (*account.BalanceSnapshot, error) {
	url := strings.TrimRight(base, "/") + loomyRecordsPath
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("构造积分请求失败")
	}
	req.Header.Set("Accept", "application/json")
	for _, h := range provider.LoomySessionHeaders(session) {
		req.Header.Set(h[0], h[1])
	}
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("积分请求失败（网络）")
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, authExpiredf("HTTP %d", resp.StatusCode)
	}
	var payload struct {
		Code string          `json:"code"`
		Desc string          `json:"desc"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("积分响应形状不符（平台接口可能已变更）")
	}
	if isLoomyAuthCode(payload.Code) {
		return nil, authExpiredf("业务码 %s", payload.Code)
	}
	if payload.Code != loomySuccessCode {
		desc := strings.TrimSpace(payload.Desc)
		if desc == "" {
			desc = "上游未给原因"
		}
		return nil, fmt.Errorf("积分查询失败：%s", desc)
	}
	var data map[string]any
	if err := json.Unmarshal(payload.Data, &data); err != nil {
		return nil, fmt.Errorf("积分数据形状不符（平台接口可能已变更）")
	}
	// 摘要可能在 data 或 data.data 两层（不同版本出现过两种）。
	if inner, ok := data["data"].(map[string]any); ok {
		data = inner
	}
	permanent := jsonFloat(data["balance"])
	daily := jsonFloat(data["dailyBalance"])
	return &account.BalanceSnapshot{
		Available: permanent + daily,
		Unit:      "积分",
		Rows: []account.BalanceRow{
			{Name: "永久积分", Available: permanent, Unit: "积分"},
			{Name: "每日赠送积分", Available: daily, Unit: "积分"},
		},
		FetchedAt: time.Now(),
	}, nil
}
