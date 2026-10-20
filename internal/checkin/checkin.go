// Package checkin 平台账号的「日常养护」子系统（账号中心 v1.6.0 · T3；架构 §5）。
//
// ── 是什么 ──────────────────────────────────────────────────
// 平台可选的每日签到能力：loomy（每日登录积分）/ autoclaw（task-complete 通
// 用任务接口，国内/国际两地区）/ qoder 中国版（sash 活动领取）。CodeBuddy 的
// 签到在 internal/scheduler，两套语义**命名强区分**：本包日志前缀
// `[checkin:<platform>]`，面板文案「平台签到」（架构 §11 风险 2 的对策）。
//
// ── 两域区分（架构 §2 的关键结论）────────────────────────────
// 签到/积分走**用户域**（loomy 积分路径、autoclaw userapi、qoder openapi+sash），
// 与聊天的推理网关域不同主机、不同头、不同鉴权口径——平台实现**不经过**
// provider 的聊天适配器，只复用其中的头构造契约（Loomy 双头）或自实现
// userapi/sash 的最小头束（这些头 provider 从不需要，"一份实现"自然落在本包）。
//
// ── 契约 ────────────────────────────────────────────────────
//   - 实现必须幂等：平台侧"已签过"返回 Status=already，不算失败；
//   - error.Message 必须已脱敏（不含 token/完整 URL）；
//   - 响应形状不符 → 显式"平台接口可能已变更"告警文案（P0-4 风险对策：
//     失效必须可观测，不静默）。
package checkin

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/RisingInsight/startport/internal/account"
)

// 签到结果状态（对齐 internal/pool 的签到记录语义 + 账号中心三态）。
const (
	StatusOK      = "ok"
	StatusAlready = "already"
	StatusFailed  = "failed"
)

// ErrAuthExpired 登录态失效（401/业务码）。调度器据此记录失败原因；
// 凭证侧的"待重登"状态由 extchan 的转发链路自身维护，这里不重复标注。
var ErrAuthExpired = errors.New("登录态已失效，请重新登录")

// ErrPlatformNotSupported 平台没有签到活动（qoder-intl 无签到计划——ref
// qoder/mod.rs:46；custom 无平台语义）。架构 §5.1 Q5：For() 对这些平台返回
// false，UI 显示「平台无此活动」；本哨兵供调用方需要显式文案时使用。
var ErrPlatformNotSupported = errors.New("平台无此活动")

// Result 一次签到的业务结果。
type Result struct {
	Status  string // ok / already / failed
	Reward  string // 领到的额度描述（"100 credits"）；already/failed 为空
	Message string // 人话（失败原因 / 已签过说明）
}

// Checker 平台可选的签到能力（init 装配进注册表，运行期只读）。
type Checker interface {
	Platform() string // 与 account.Platform 同枚举
	// CheckIn 对一个账号做每日签到。实现内部自带超时（≤20s）、错误翻译与
	// 脱敏；幂等（已签过 → Status=already）。
	CheckIn(ctx context.Context, a *account.Account) (Result, error)
}

var (
	registryMu sync.RWMutex
	registry   = map[string]Checker{}
)

// Register 注册平台 Checker（重复注册覆盖——测试注入用）。
func Register(c Checker) {
	registryMu.Lock()
	defer registryMu.Unlock()
	registry[c.Platform()] = c
}

// For 取平台 Checker（未实现平台返回 false——UI 显示「平台无此活动」）。
func For(platform string) (Checker, bool) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	c, ok := registry[platform]
	return c, ok
}

// Supported 平台是否有签到能力。
func Supported(platform string) bool {
	_, ok := For(platform)
	return ok
}

// Platforms 返回已注册签到能力的平台清单（排序；注册表一致性测试与面板用）。
func Platforms() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	out := make([]string, 0, len(registry))
	for k := range registry {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// decodeBlob 宽松解码平台凭证 blob（account 包的同一实现）。
func decodeBlob(a *account.Account) (map[string]any, error) {
	return account.DecodeBlob(a.Cred)
}

// blobString 取 blob 里的第一个非空字符串字段。
func blobString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := m[k].(string); ok {
			if v := strings.TrimSpace(s); v != "" {
				return v
			}
		}
	}
	return ""
}

// authExpiredf 组一个带哨兵的登录失效错误（消息已脱敏——调用方只拼平台文案）。
func authExpiredf(format string, args ...any) error {
	return fmt.Errorf("%w：%s", ErrAuthExpired, fmt.Sprintf(format, args...))
}

func init() {
	Register(newLoomyChecker())
	Register(newAutoClawChecker(false)) // 国内版
	Register(newAutoClawChecker(true))  // 国际版（任务体系两地独立，域不同）
	Register(newQoderChecker())
	// TRAE Checker（newTraeChecker）已实现但暂不注册：能力接线随 platform.go
	// TRAE 条目（Checkin/Refresh/Balance 翻 true）一次性打开，见该处注释。
}
