// refresh.go 平台登录态刷新的统一入口（账号中心刷新闭环）。
//
// 各平台刷新协议不同（AutoClaw userapi 签名链 / Qoder openapi 匿名刷新），
// 但调度器的编排完全一致：登录态失效 → 刷新 → 回写 accounts.json → 重试一次。
// 本文件只做平台分发，协议实现留在各平台文件里。
package checkin

import (
	"context"
	"fmt"

	"github.com/RisingInsight/startport/internal/account"
)

// RefreshAccount 刷新一个平台账号的登录态，返回更新后的 blob map。
// 回写由调用方完成（scheduler 持有 Store）。
func RefreshAccount(ctx context.Context, a *account.Account) (map[string]any, error) {
	if a == nil {
		return nil, fmt.Errorf("账号为空")
	}
	switch a.Platform {
	case account.PlatformAutoClaw, account.PlatformAutoClawIntl:
		newToken, err := RefreshAutoClaw(ctx, a)
		if err != nil {
			return nil, err
		}
		m, err := decodeBlob(a)
		if err != nil {
			return nil, err
		}
		m["token"] = newToken
		return m, nil
	case account.PlatformQoder, account.PlatformQoderIntl:
		return RefreshQoder(ctx, a)
	// TRAE：RefreshTrae 已实现（trae.go）暂不接线，随 platform.go 能力开关一并打开。
	default:
		return nil, ErrPlatformNotSupported
	}
}
