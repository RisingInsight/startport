// Package account 非 CodeBuddy 平台账号的模型与存储（账号中心 v1.6.0）。
//
// ── 账号是运行数据，凭证是配置数据（架构 §0-2）────────────────
// 账号主体（平台/备注/签到历史/余额快照）落 data/accounts.json（0600）；
// 凭证 blob **单向投影**进 config.json 对应的 managed 渠道 credentials
// （投影在 internal/acctchan）。两条安全红线都不破：秘密值只落 0600 文件、
// 任何接口不回显。
//
// ── 与 CodeBuddy 账号池的关系：并表不同存储（架构 §3.1）────────
// CodeBuddy 池有 token 生命周期/租约/粘性/加权选号；平台账号是静态凭证 +
// 引擎凭证池治理（extchan 的 pick=first(usable)）。两套语义混喂会互相污染
// 观测——账号中心只在 UX 层统一，不合并存储。
//
// ── 不变量（测试钉住）───────────────────────────────────────
//   - 同平台内 Label 唯一（重名报错，错误带 platform+label）；
//   - ID 永不复用（删除后 seq 继续走）；
//   - Cred 原样落盘（Store 不理解平台语义；平台校验在 ValidateCred）。
package account

import (
	"encoding/json"
	"fmt"
	"time"
)

// Platform 平台枚举（与 provider 的 NativeAdapter 名 + custom 对齐）。
const (
	PlatformLoomy        = "loomy"
	PlatformAutoClaw     = "autoclaw"
	PlatformAutoClawIntl = "autoclaw-intl"
	PlatformQoder        = "qoder"
	PlatformQoderIntl    = "qoder-intl"
	PlatformTrae         = "trae"
	PlatformCodex        = "codex"
	// PlatformKuku 库库AI（百度 GenFlow）——**账号 / 额度型平台，无对话能力**。
	// 它没有对外 API，凭证是网页版登录 Cookie；接进来的目的是把积分/额度纳入
	// 账号中心统一看板与签到调度，而不是转发对话（见 PlatformSpec.AccountOnly）。
	PlatformKuku   = "kuku"
	PlatformCustom = "custom"
)

// ValidPlatform 报告 platform 是否受支持（注册表见 platform.go）。
func ValidPlatform(p string) bool {
	_, ok := SpecOf(p)
	return ok
}

// NativePlatform 报告 platform 是否有原生适配器（custom 没有，投影为普通渠道）。
func NativePlatform(p string) bool {
	s, ok := SpecOf(p)
	return ok && s.Native
}

// Account 一个非 CodeBuddy 平台账号：账号主体 + 凭证 blob + 运行态。
type Account struct {
	ID       string `json:"id"`       // "acc-" + 8 hex（crypto/rand）；稳定且不涉密
	Platform string `json:"platform"` // 平台枚举（见 Platform* 常量）
	Label    string `json:"label"`    // 用户可读名；缺省 "账号-<seq>"（同平台内唯一）
	// Cred 平台凭证 blob（各平台独立最小 schema，架构 §3.1；仅落 accounts.json
	// 与 config.json（投影后），接口永不回显）。
	Cred json.RawMessage `json:"cred"`
	Note string          `json:"note,omitempty"`
	// CreatedAt / Disabled 配置面。
	CreatedAt time.Time `json:"created_at"`
	Disabled  bool      `json:"disabled,omitempty"`

	// ---- 签到（仅支持平台；环形保留 30 天）----
	Checkin []CheckinRecord `json:"checkin,omitempty"`

	// ---- 余额快照（P1-1；本期仅结构占位）----
	Balance *BalanceSnapshot `json:"balance,omitempty"`
}

// CheckinRecord 一次签到（对齐 internal/pool 的签到记录语义：按账号按次记，
// 成功/已签过/失败三态可辨，失败带原因）。
type CheckinRecord struct {
	At      time.Time `json:"at"`                // 触发时刻
	Status  string    `json:"status"`            // ok / already / failed
	Reward  string    `json:"reward,omitempty"`  // "100 credits" 等
	Message string    `json:"message,omitempty"` // 非失败结果的人话说明（今日已领取/签到成功）
	Error   string    `json:"error,omitempty"`   // 失败原因（已脱敏）
}

// Checkin 状态常量。
const (
	CheckinOK      = "ok"
	CheckinAlready = "already"
	CheckinFailed  = "failed"
)

// BalanceRow 余额桶级明细（套餐 / 个人资源包 / 组织资源包）。
type BalanceRow struct {
	Name      string  `json:"name"`
	Available float64 `json:"available,omitempty"`
	Total     float64 `json:"total,omitempty"`
	Unit      string  `json:"unit,omitempty"`
}

// BalanceSnapshot 钱包快照（P1-1 落数据，本期先定形状）。
// 口径对齐 ref：qoder 三桶（userQuota/addOnQuota/orgResourcePackage）合并为
// available + 明细行；autoclaw 积分钱包；loomy 积分。
type BalanceSnapshot struct {
	Available float64      `json:"available,omitempty"`
	Total     float64      `json:"total,omitempty"`
	Unit      string       `json:"unit,omitempty"`
	Rows      []BalanceRow `json:"rows,omitempty"`
	PlanName  string       `json:"plan_name,omitempty"`
	ExpiresAt time.Time    `json:"expires_at,omitempty"` // 零值 = 无/永不过期（哨兵过滤）
	FetchedAt time.Time    `json:"fetched_at"`
}

// CheckinHistoryWindow 签到历史的环形保留窗口（架构 §3.1：30 天 ≈ 每账号 30 条）。
const CheckinHistoryWindow = 30 * 24 * time.Hour

// TrimCheckinHistory 裁剪签到历史：只保留窗口内的记录（就地修改）。
func TrimCheckinHistory(records []CheckinRecord, now time.Time) []CheckinRecord {
	cutoff := now.Add(-CheckinHistoryWindow)
	kept := records[:0]
	for _, r := range records {
		if r.At.After(cutoff) {
			kept = append(kept, r)
		}
	}
	return kept
}

// String 调试/日志用（绝不含 Cred）。
func (a *Account) String() string {
	if a == nil {
		return "<nil account>"
	}
	return fmt.Sprintf("account{id=%s platform=%s label=%q disabled=%v}", a.ID, a.Platform, a.Label, a.Disabled)
}
