// credential.go 渠道下的「凭证池」：一条凭证 = 配置面（label/secret/过期/
// 停用/auth）+ 运行态（健康状态、冷却、最近使用、进程内用量计数）。
//
// 状态机（PRD P0-2）：
//
//	healthy → cooling  429（base·2^(n-1)，封顶 max；成功清零）
//	healthy → invalid  401/403（退出轮换，不再被选中）
//	invalid → healthy  改 secret（stateKey 变化）或删除重建
//	disabled 是配置面派生状态（Disabled=true）；配置优先级高于运行态
//
// 稳定 ID（运行态复用键，PRD §10 Q1）：
//
//	stateKey = label + "\x00" + sha256(secret)[:16]
//
// 只在进程内做热重载复用匹配，不进任何日志/接口/Status()。用它而不是明文
// 比较，是为了不把秘密值当 map key 长期驻留（也降低 core dump 泄露面）。
package extchan

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"
	"time"

	"github.com/RisingInsight/startport/internal/provider"
)

// CredState 凭证运行态。
type CredState string

const (
	// CredHealthy 可用（含"从未使用过"）。
	CredHealthy CredState = "healthy"
	// CredCooling 429 冷却中（到期自动恢复，成功也会清零）。
	CredCooling CredState = "cooling"
	// CredInvalid 401/403 鉴权失败（退出轮换；改 secret 才恢复）。
	CredInvalid CredState = "invalid"
	// CredDisabled 配置面停用（派生，不落运行态）。
	CredDisabled CredState = "disabled"
)

// credExpiringWindow 「即将过期」窗口（PRD P0-8：3 天内到期）。
const credExpiringWindow = 72 * time.Hour

// Credential 一条凭证：配置面 + 运行态。
type Credential struct {
	// ---- 配置面（来自 ChannelDef，热重载就地更新）----
	// Label 用户可读标识，渠道内唯一（路由/观测/日志都用它，永不用 Secret）。
	Label string
	// Secret 秘密值（仅 config.json 0600；接口/日志永不回显）。
	Secret string
	// ExpiresAt 过期时间（零值 = 无过期）。
	ExpiresAt time.Time
	// Disabled 配置面停用（优先级高于运行态）。
	Disabled bool
	// Auth 凭证级鉴权（零值 = 继承渠道级；非零值整体覆盖，不做字段级合并）。
	Auth provider.AuthSpec

	// ---- 运行态（不落盘）----
	mu         sync.Mutex
	state      CredState
	coolUntil  time.Time
	coolStreak int
	lastOK     time.Time
	lastUsed   time.Time
	lastErr    string
	inFlight   int
	// 进程内用量计数（P0-9 降级方案 B：不碰 internal/usage 桶格式）。
	requests         int64
	promptTokens     int64
	completionTokens int64

	stateKey string // label + "\x00" + sha256(secret)[:16]
}

// CredentialDef 凭证的配置形态（config JSON 与面板探测共用）。
// ExpiresAt 用字符串表达：支持 YYYY-MM-DD 与 RFC3339（解析见 ParseExpiresAt）。
type CredentialDef struct {
	Label     string            `json:"label"`
	Secret    string            `json:"secret"`
	ExpiresAt string            `json:"expires_at,omitempty"`
	Disabled  bool              `json:"disabled,omitempty"`
	Auth      provider.AuthSpec `json:"auth,omitempty"`
}

// ParseExpiresAt 解析 expires_at：空串 = 无过期；YYYY-MM-DD 或 RFC3339。
func ParseExpiresAt(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, nil
	}
	for _, layout := range []string{"2006-01-02", time.RFC3339, "2006-01-02T15:04:05"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	// 带时区的日期形态（2006-01-02T15:04:05Z07:00 已由 RFC3339 覆盖）。
	_, err := time.Parse(time.RFC3339Nano, s)
	return time.Time{}, err
}

// stateKeyFor 计算运行态复用键（label + 秘密值指纹）。
func stateKeyFor(label, secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return label + "\x00" + hex.EncodeToString(sum[:])[:16]
}

// newCredential 由配置构造凭证（运行态全新 healthy）。
func newCredential(def CredentialDef) (*Credential, error) {
	exp, err := ParseExpiresAt(def.ExpiresAt)
	if err != nil {
		return nil, err
	}
	return &Credential{
		Label:     def.Label,
		Secret:    def.Secret,
		ExpiresAt: exp,
		Disabled:  def.Disabled,
		Auth:      def.Auth,
		state:     CredHealthy,
		stateKey:  stateKeyFor(def.Label, def.Secret),
	}, nil
}

// StateKey 返回运行态复用键（内部用；不导出到 JSON）。
func (c *Credential) StateKey() string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stateKey
}

// State 当前运行态（配置面 Disabled 优先）。
func (c *Credential) State() CredState {
	if c == nil {
		return CredInvalid
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stateLocked()
}

// stateLocked 状态归一（零值视为 healthy；停用优先）。
func (c *Credential) stateLocked() CredState {
	if c.Disabled {
		return CredDisabled
	}
	switch c.state {
	case CredCooling:
		if time.Now().Before(c.coolUntil) {
			return CredCooling
		}
		return CredHealthy // 冷却到期自动恢复
	case CredInvalid:
		return CredInvalid
	default:
		return CredHealthy
	}
}

// usableAt 选择策略的唯一判据（PRD P0-2 / 架构 §3.4）：
// 未停用 ∧ 非 invalid ∧ 不在冷却 ∧ 未过期。
func (c *Credential) usableAt(now time.Time) bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.usableLocked(now)
}

// usableLocked 与 usableAt 同语义（持锁版本，供 pick 在渠道锁内调用）。
func (c *Credential) usableLocked(now time.Time) bool {
	if c.Disabled {
		return false
	}
	if c.state == CredInvalid {
		return false
	}
	if c.state == CredCooling && now.Before(c.coolUntil) {
		return false
	}
	if !c.ExpiresAt.IsZero() && !now.Before(c.ExpiresAt) {
		return false // 过期凭证不再参与轮换（面板仍展示，提示换 key）
	}
	return true
}

// NoteSuccess 成功：清空冷却/失效标记与连续计流，记录最近成功时间。
func (c *Credential) NoteSuccess() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.state = CredHealthy
	c.coolUntil, c.coolStreak, c.lastErr = time.Time{}, 0, ""
	now := time.Now()
	c.lastOK = now
	c.lastUsed = now
}

// NoteRateLimited 429：短冷却指数退避（第 1 次 base，之后翻倍，封顶 max）。
func (c *Credential) NoteRateLimited(base, max time.Duration, reason string) {
	if c == nil {
		return
	}
	if base <= 0 {
		base = time.Minute
	}
	if max <= 0 {
		max = 30 * time.Minute
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.coolStreak++
	d := base << (c.coolStreak - 1)
	if c.coolStreak > 1 && d <= 0 { // 位移溢出保护
		d = max
	}
	if d > max {
		d = max
	}
	c.state = CredCooling
	c.coolUntil = time.Now().Add(d)
	c.lastErr = reason
	c.lastUsed = time.Now()
}

// NoteInvalid 401/403：退出轮换（无冷却时长；改 secret 或删重建才恢复）。
func (c *Credential) NoteInvalid(reason string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.state = CredInvalid
	c.coolUntil = time.Time{}
	c.lastErr = reason
	c.lastUsed = time.Now()
}

// NoteFailure 只记录最近错误，不影响可用性（400/404/422 是请求/配置问题，
// 换凭证也解决不了，不应把凭证或渠道罚下线）。
func (c *Credential) NoteFailure(reason string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastErr = reason
	c.lastUsed = time.Now()
}

// NoteUsage 累计进程内用量（P0-9 降级：不写 usage 桶）。
// requests 每次尝试成功计 1；token 数按上游返回累加。
func (c *Credential) NoteUsage(promptTokens, completionTokens int64) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.requests++
	if promptTokens > 0 {
		c.promptTokens += promptTokens
	}
	if completionTokens > 0 {
		c.completionTokens += completionTokens
	}
	c.lastUsed = time.Now()
}

// NoteAttempt 记一次"被换证淘汰"的上游尝试（v1.4.0 口径）。
//
// 只累计 `requests` 与 `lastUsed`，**不动 token 与状态**——状态由
// NoteRateLimited / NoteInvalid / NoteFailure 负责。调用时机在 Do 的
// 429 / 401 / 403 分支：本次尝试已真实发出上游请求、且马上要换下一条凭证，
// 若不记它，面板上的"尝试数"就看不到这条凭证出过力（v1.3 前的口径只计
// 最终服务那次，被淘汰的凭证请求恒为 0）。
//
// 最终**服务成功/把结果透传给客户端**的那次仍由 server 侧 recordExtAttempt
// → NoteUsage 计（含 tokens），两套记账互不重复：任一凭证的 `requests` +
// 最终服务的 1 次之和 == 该请求的上游往返总数。
func (c *Credential) NoteAttempt() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.requests++
	c.lastUsed = time.Now()
}

// begin 在途计数 +1（观测）。
func (c *Credential) begin() {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.inFlight++
	c.lastUsed = time.Now()
	c.mu.Unlock()
}

// end 在途计数 -1（观测）。
func (c *Credential) end() {
	if c == nil {
		return
	}
	c.mu.Lock()
	if c.inFlight > 0 {
		c.inFlight--
	}
	c.mu.Unlock()
}

// InFlight 当前在途请求数（观测/测试用；与 Channel.InFlight 同语义：
// 计数区间是"一次上游往返"，由 Do 的 begin/end 配对维护）。
func (c *Credential) InFlight() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.inFlight
}

// AuthFor 生效鉴权：凭证级非零则整体覆盖，否则继承渠道级。
func (c *Credential) AuthFor(channelAuth provider.AuthSpec) provider.AuthSpec {
	if c == nil {
		return channelAuth
	}
	c.mu.Lock()
	a := c.Auth
	c.mu.Unlock()
	if a.IsZero() {
		return channelAuth
	}
	return a
}

// CredSnapshot 凭证观测快照（绝不含 Secret）。
type CredSnapshot struct {
	Label        string    `json:"label"`
	State        CredState `json:"state"`
	Disabled     bool      `json:"disabled"`
	Cooling      bool      `json:"cooling"`
	CoolRemain   int64     `json:"cool_remaining_sec"`
	HasExpiry    bool      `json:"has_expiry"`
	ExpiresAt    string    `json:"expires_at,omitempty"`
	ExpiresDays  int       `json:"expires_in_days,omitempty"`
	ExpiringSoon bool      `json:"expiring_soon"`
	LastOK       time.Time `json:"last_ok,omitempty"`
	LastUsed     time.Time `json:"last_used,omitempty"`
	LastError    string    `json:"last_error,omitempty"`
	AuthMode     string    `json:"auth_mode,omitempty"`
	InFlight     int       `json:"in_flight"`
	// 进程内累计（重启清零；面板明确标注该口径）。
	Requests         int64 `json:"requests"`
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
	// Masked 永远为 true（前端据此确认"这里没有秘密值"）。
	Masked bool `json:"masked"`
}

// Snapshot 凭证观测快照。
func (c *Credential) Snapshot(now time.Time) CredSnapshot {
	if c == nil {
		return CredSnapshot{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	state := c.stateLocked()
	out := CredSnapshot{
		Label:            c.Label,
		State:            state,
		Disabled:         c.Disabled,
		LastOK:           c.lastOK,
		LastUsed:         c.lastUsed,
		LastError:        c.lastErr,
		AuthMode:         c.Auth.Mode,
		InFlight:         c.inFlight,
		Requests:         c.requests,
		PromptTokens:     c.promptTokens,
		CompletionTokens: c.completionTokens,
		TotalTokens:      c.promptTokens + c.completionTokens,
		Masked:           true,
	}
	if state == CredCooling && now.Before(c.coolUntil) {
		out.Cooling = true
		out.CoolRemain = int64(c.coolUntil.Sub(now).Seconds())
	}
	if !c.ExpiresAt.IsZero() {
		out.HasExpiry = true
		out.ExpiresAt = c.ExpiresAt.Format("2006-01-02")
		days := int(c.ExpiresAt.Sub(now).Hours() / 24)
		out.ExpiresDays = days
		out.ExpiringSoon = c.ExpiresAt.Sub(now) <= credExpiringWindow
	}
	return out
}

// ---------------------------------------------------------------------------
// 渠道侧：选证与派生状态
// ---------------------------------------------------------------------------

// pickCredential 按配置数组顺序取第一条 usable 且本轮没试过的凭证。
// 返回 nil = 无可用凭证（渠道应报 all_credentials_unavailable）。
func (c *Channel) pickCredential(tried map[string]bool) *Credential {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	creds := append([]*Credential(nil), c.Creds...)
	c.mu.Unlock()
	now := time.Now()
	for _, cred := range creds {
		if cred == nil {
			continue
		}
		if tried != nil && tried[cred.Label] {
			continue
		}
		if cred.usableAt(now) {
			return cred
		}
	}
	return nil
}

// hasUsableCredential 报告是否还有可用凭证（面板/状态派生用）。
func (c *Channel) hasUsableCredential() bool {
	return c.pickCredential(nil) != nil
}

// CredentialByLabel 按 label 取凭证（记账把 usage 归到具体凭证用）。
func (c *Channel) CredentialByLabel(label string) *Credential {
	if c == nil || label == "" {
		return nil
	}
	for _, cred := range c.credentials() {
		if cred != nil && cred.Label == label {
			return cred
		}
	}
	return nil
}

// credentials 返回凭证切片副本（避免调用方在无锁状态下遍历可变切片）。
func (c *Channel) credentials() []*Credential {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]*Credential(nil), c.Creds...)
}
