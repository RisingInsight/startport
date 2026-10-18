// Package extchan 外部渠道（openai-compatible provider）：把任意标准 OpenAI
// 兼容上游（硅基流动 / 阿里百炼 / OpenRouter / NVIDIA NIM / 火山 Ark / 自建
// one-api 等）接入 StarPort 的模型路由，与 CodeBuddy 账号池并存。
//
// 设计边界（阶段一，见 README「多平台路线」）：
//   - 外部渠道不走账号池（pool），不参与保号/签到/粘性——它们是「已持有的 key」，
//     没有 CodeBuddy 那种账号生命周期需要管理；
//   - 治理用进程内的渠道级状态机：429 短冷却（指数退避封顶）+ 连续失败熔断 +
//     5xx 计数；不喂 pool 熔断器（两套语义不同，混喂会互相污染观测）；
//   - 模型名协议沿用 realm 前缀惯例：请求 model 带前缀 "ext:<channel>/<model>"
//     时路由到该渠道；裸名时若与某渠道模型唯一匹配也路由（前缀优先）。
//   - key 只存 config.json（0600），GET /startport/api/config 不回显真实 key
//     （与 device_token/upstash.token 同一套哨兵脱敏）。
package extchan

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/RisingInsight/startport/internal/provider"
)

// Channel 单个外部渠道的定义与运行态。
type Channel struct {
	// Name 渠道名（路由用，"ext:<name>/<model>"）。小写字母/数字/连字符/下划线。
	Name string `json:"name"`
	// BaseURL 上游 OpenAI 兼容根地址（如 https://api.siliconflow.cn）。
	// 网关在其后拼 /v1/chat/completions 与 /v1/models；也接受已带 /v1 的形态。
	BaseURL string `json:"base_url"`
	// APIKey 上游 Bearer 密钥（仅落 config.json，接口不回显）。
	// 兼容字段：api_key 非空时 ReplaceAll 会合成为一条 label="default" 的凭证；
	// 运行期凭证池在 Creds 里，本字段只保留给老配置/观测读取。
	APIKey string `json:"api_key"`
	// Protocol 出站协议（openai / anthropic / gemini；空归一为 openai）。
	Protocol provider.Protocol `json:"protocol"`
	// Adapter 平台原生适配器名（空 = 按协议选；如 "loomy"）。
	// 非空时由 provider.AdapterFor 解析，未知名字是配置错（装配期即报，不静默回落）。
	Adapter string `json:"adapter,omitempty"`
	// Path 自定义路径覆盖（空 = 协议默认模板；支持 {model} 占位）。
	Path string `json:"path,omitempty"`
	// Auth 渠道级鉴权规格（凭证级非零时整体覆盖）。
	Auth provider.AuthSpec `json:"auth"`
	// Headers 渠道级静态头（校验层已拒绝传输层头）。
	Headers map[string]string `json:"headers,omitempty"`
	// Aliases 模型别名表（别名 → 上游模型名）。
	Aliases map[string]string `json:"aliases,omitempty"`
	// Note 渠道备注（情报草稿来源链接等；面板展示用，不参与路由）。
	Note string `json:"note,omitempty"`
	// Creds 凭证池（按配置顺序选证；api_key 兼容形态合成为单条 default）。
	Creds []*Credential `json:"-"`
	// Models 显式模型名单（优先）。空 = 启动后从 /v1/models 自动发现。
	// 显式名单是计费/白名单场景的推荐形态：自动发现可能带出付费模型。
	Models []string `json:"models,omitempty"`
	// Disabled 停用开关：true 时不参与路由与模型列表。
	Disabled bool `json:"disabled,omitempty"`
	// TimeoutSeconds 单次上游请求（连接到响应头）上限；<=0 回落 120。
	TimeoutSeconds int `json:"timeout_seconds,omitempty"`
	// AllowPrivate 显式放行内网/回环 base_url（SSRF 校验在 cmd/server 层做）。
	AllowPrivate bool `json:"allow_private,omitempty"`

	// ---- 运行态（不落盘；进程内状态机）----

	mu          sync.Mutex
	coolUntil   time.Time // 429/5xx 冷却截止
	coolStreak  int       // 连续限流次数（指数退避）
	failStreak  int       // 连续失败次数（熔断计数）
	modelsCache []string  // 自动发现的模型名单缓存
	modelsAt    time.Time // 模型目录发现时间
	lastErr     string    // 最近一次错误摘要（观测）
	lastOK      time.Time // 最近一次成功时间
	inFlight    int       // 在途请求（观测）
}

// Cooldown 报告渠道是否处于冷却期。
func (c *Channel) Cooldown() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Now().Before(c.coolUntil)
}

// InFlight 当前在途请求数（面板观测）。
func (c *Channel) InFlight() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.inFlight
}

// NoteSuccess 记录一次成功：清空所有失败计数与冷却。
func (c *Channel) NoteSuccess() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.coolUntil, c.coolStreak, c.failStreak, c.lastErr = time.Time{}, 0, 0, ""
	c.lastOK = time.Now()
}

// noteCooldown 进入冷却：第 1 次 base，之后每次翻倍（base·2^(streak-1)），封顶 max。
func (c *Channel) noteCooldown(base, max time.Duration, reason string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.coolStreak++
	d := base << (c.coolStreak - 1)
	if d > max {
		d = max
	}
	c.coolUntil = time.Now().Add(d)
	c.lastErr = reason
}

// NoteRateLimited 429 限流：短冷却指数退避（与 soft_rate 同哲学：渠道对同一
// key 有配额，连续触发说明窗口未滑过）。
func (c *Channel) NoteRateLimited(base, max time.Duration, reason string) {
	c.noteCooldown(base, max, reason)
}

// NoteServerUpstream 5xx：轻冷却（固定短窗防雪崩，不指数升级——5xx 多为上游
// 自身抖动，很快自愈）。
func (c *Channel) NoteServerUpstream(base time.Duration, reason string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.coolUntil = time.Now().Add(base)
	c.lastErr = reason
}

// NoteFailure 传输失败/连续失败：计数达阈值熔断（长冷却）。
func (c *Channel) NoteFailure(threshold int, breaker time.Duration, reason string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.failStreak++
	if c.failStreak < threshold {
		c.lastErr = reason
		return
	}
	c.coolUntil = time.Now().Add(breaker)
	c.failStreak = 0
	c.lastErr = reason + " (breaker)"
}

// 渠道派生状态（面板与 /status 用；不单独存储，按凭证池实时派生）。
const (
	// StateAvailable ≥1 条凭证可用。
	StateAvailable = "available"
	// StateCooling 渠道级冷却（429 全渠道打满 / 5xx 轻冷却 / 熔断）。
	StateCooling = "cooling"
	// StateDisabled 渠道配置 disabled=true。
	StateDisabled = "disabled"
	// StateAllInvalid 全部凭证失效（401/403 或已过期）。
	StateAllInvalid = "all_invalid"
	// StateAllCooling 全部可用凭证都在 429 冷却中。
	StateAllCooling = "all_cooling"
	// StateUnavailable 其它不可用形态（如全部凭证已过期）。
	StateUnavailable = "unavailable"
)

// Status 面板观测快照。
type Status struct {
	Name       string            `json:"name"`
	BaseURL    string            `json:"base_url"`
	Protocol   string            `json:"protocol"`
	Adapter    string            `json:"adapter,omitempty"`
	Disabled   bool              `json:"disabled"`
	State      string            `json:"state"`
	Models     []string          `json:"models"`
	AutoModels []string          `json:"auto_models,omitempty"`
	Aliases    map[string]string `json:"aliases,omitempty"`
	Note       string            `json:"note,omitempty"`
	Cooling    bool              `json:"cooling"`
	CoolRemain int64             `json:"cool_remaining_sec"`
	InFlight   int               `json:"in_flight"`
	LastErr    string            `json:"last_error,omitempty"`
	LastOK     time.Time         `json:"last_ok,omitempty"`
	ModelsAt   time.Time         `json:"models_at,omitempty"`
	// 凭证维度（P0-1 / P0-8）。
	CredentialsTotal        int            `json:"credentials_total"`
	CredentialsUsable       int            `json:"credentials_usable"`
	CredentialsInvalid      int            `json:"credentials_invalid"`
	CredentialsExpiringSoon int            `json:"credentials_expiring_soon"`
	Credentials             []CredSnapshot `json:"credentials"`
}

// Snapshot 渠道观测快照（不含 key——面板与 /status 都不回显）。
func (c *Channel) Snapshot() Status {
	now := time.Now()
	// 先取凭证快照（凭证锁内），再取渠道运行态（渠道锁内）——锁顺序恒为
	// channel.mu → credential.mu，与 pick/ReplaceAll 一致，不会反向死锁。
	creds := c.credentials()
	credSnaps := make([]CredSnapshot, 0, len(creds))
	usable, invalid, coolingCount, enabled, expiringSoon := 0, 0, 0, 0, 0
	for _, cred := range creds {
		snap := cred.Snapshot(now)
		credSnaps = append(credSnaps, snap)
		if snap.Disabled {
			continue
		}
		enabled++
		switch {
		case snap.State == CredInvalid:
			invalid++
		case snap.Cooling:
			coolingCount++ // 冷却到期自动恢复，不算"可用"也不算出局
		default:
			usable++
		}
		if snap.ExpiringSoon {
			expiringSoon++
		}
	}
	allCooling := enabled > 0 && coolingCount == enabled

	c.mu.Lock()
	st := Status{
		Name:                    c.Name,
		BaseURL:                 c.BaseURL,
		Protocol:                string(c.Protocol),
		Adapter:                 c.Adapter,
		Disabled:                c.Disabled,
		Models:                  append([]string(nil), c.Models...),
		Aliases:                 copyStringMap(c.Aliases),
		Note:                    c.Note,
		InFlight:                c.inFlight,
		LastErr:                 c.lastErr,
		LastOK:                  c.lastOK,
		ModelsAt:                c.modelsAt,
		CredentialsTotal:        len(creds),
		CredentialsUsable:       usable,
		CredentialsInvalid:      invalid,
		CredentialsExpiringSoon: expiringSoon,
		Credentials:             credSnaps,
	}
	if now.Before(c.coolUntil) {
		st.Cooling = true
		st.CoolRemain = int64(c.coolUntil.Sub(now).Seconds())
	}
	if len(c.modelsCache) > 0 {
		st.AutoModels = append([]string(nil), c.modelsCache...)
	}
	cooling := st.Cooling
	c.mu.Unlock()

	// 派生状态（配置优先 → 渠道冷却 → 凭证池形态）。
	switch {
	case c.Disabled:
		st.State = StateDisabled
	case cooling:
		st.State = StateCooling
	case len(creds) == 0:
		st.State = StateAvailable // 无凭证 = 不注入鉴权（自建内网场景）
	case usable > 0:
		st.State = StateAvailable
	case allCooling:
		st.State = StateAllCooling
	case enabled > 0 && invalid == enabled:
		st.State = StateAllInvalid
	default:
		st.State = StateUnavailable
	}
	return st
}

// copyStringMap 复制字符串 map（快照隔离；nil → nil）。
func copyStringMap(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// normalizeAdapterName 原生适配器名归一（小写 + 去首尾空白）；空串 = 按协议。
// 与 provider 侧归一规则一致：`Loomy` / ` loomy ` 都落在 "loomy"。
func normalizeAdapterName(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// availableModels 渠道当前可路由的模型名单：显式配置优先；否则用自动发现缓存。
func (c *Channel) availableModels() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.Models) > 0 {
		return append([]string(nil), c.Models...)
	}
	return append([]string(nil), c.modelsCache...)
}

// ---------------------------------------------------------------------------

// Manager 全部外部渠道的持有者与路由器。
type Manager struct {
	mu       sync.RWMutex
	channels []*Channel

	http    *http.Client
	base    time.Duration // 429 冷却基数（默认 60s）
	max     time.Duration // 429 冷却封顶（默认 30m）
	breaker time.Duration // 熔断时长（默认 10m）

	// probeSem 面板探测并发闸（容量 4）：单用户手抖连点不把上游打爆。
	// 探测绝不进 channels、不写任何状态/计数（见 probe.go）。
	probeSem chan struct{}

	// intel 额度情报（token-fbi data.json 定时拉取；见 intel.go）。
	intelMu   sync.Mutex
	intel     *IntelData
	intelAt   time.Time
	intelErr  string
	intelURL  string
	intelOn   bool
	intelStop chan struct{}
}

// Config Manager 构造参数。
type Config struct {
	TimeoutSeconds int // 渠道请求默认超时（<=0 回落 120）
	RateBase       time.Duration
	RateMax        time.Duration
	Breaker        time.Duration
}

// New 构造 Manager。
func New(cfg Config) *Manager {
	if cfg.TimeoutSeconds <= 0 {
		cfg.TimeoutSeconds = 120
	}
	if cfg.RateBase <= 0 {
		cfg.RateBase = 60 * time.Second
	}
	if cfg.RateMax <= 0 {
		cfg.RateMax = 30 * time.Minute
	}
	if cfg.Breaker <= 0 {
		cfg.Breaker = 10 * time.Minute
	}
	return &Manager{
		http:     &http.Client{Timeout: time.Duration(cfg.TimeoutSeconds) * time.Second},
		base:     cfg.RateBase,
		max:      cfg.RateMax,
		breaker:  cfg.Breaker,
		probeSem: make(chan struct{}, 4),
	}
}

// ReplaceAll 热更新渠道定义（面板保存配置时调用）。
//
// 两层运行态复用：
//   - 渠道级按 Name 复用（冷却/熔断/目录缓存）；
//   - 凭证级按 stateKey = label + "\x00" + sha256(secret)[:16] 复用冷却/失效
//     状态。**改 secret → stateKey 变化 → 状态重置为 healthy**（换 key 就是换
//     凭证，用户预期如此）；改 label 视为新凭证。
//
// 上游形态（base_url/protocol/path）变化时丢弃模型目录缓存（旧目录对新上游
// 已失效），但**保留**凭证状态（凭证与 base 无关）。
func (m *Manager) ReplaceAll(defs []ChannelDef) {
	m.mu.Lock()
	defer m.mu.Unlock()
	old := map[string]*Channel{}
	oldCreds := map[string]*Credential{}
	for _, ch := range m.channels {
		old[ch.Name] = ch
		for _, c := range ch.Creds {
			if k := c.StateKey(); k != "" {
				oldCreds[k] = c
			}
		}
	}
	out := make([]*Channel, 0, len(defs))
	for _, d := range defs {
		if d.Name == "" || d.BaseURL == "" {
			continue // 校验在 config 层做过；防御性跳过
		}
		proto, err := provider.ParseProtocol(d.Protocol)
		if err != nil {
			proto = provider.ProtocolOpenAI // 校验层已拒绝；防御性回落
		}
		ch, ok := old[d.Name]
		if !ok {
			ch = &Channel{Name: d.Name}
		}
		ch.mu.Lock()
		if ch.BaseURL != d.BaseURL || ch.Protocol != proto || ch.Path != d.Path ||
			ch.Adapter != normalizeAdapterName(d.Adapter) {
			ch.modelsCache, ch.modelsAt = nil, time.Time{}
		}
		ch.BaseURL = d.BaseURL
		ch.APIKey = d.APIKey
		ch.Protocol = proto
		ch.Adapter = normalizeAdapterName(d.Adapter)
		ch.Path = d.Path
		ch.Auth = d.Auth
		ch.Headers = copyStringMap(d.Headers)
		ch.Aliases = copyStringMap(d.Aliases)
		ch.Note = d.Note
		ch.Models = append([]string(nil), d.Models...)
		ch.Disabled = d.Disabled
		ch.TimeoutSeconds = d.TimeoutSeconds
		ch.AllowPrivate = d.AllowPrivate
		// 凭证池：按 stateKey 复用运行态，配置面就地更新。
		defsForCreds := NormalizeCredentialDefs(d)
		creds := make([]*Credential, 0, len(defsForCreds))
		for _, cd := range defsForCreds {
			key := stateKeyFor(cd.Label, cd.Secret)
			if prev, ok := oldCreds[key]; ok {
				exp, perr := ParseExpiresAt(cd.ExpiresAt)
				if perr != nil {
					exp = time.Time{}
				}
				prev.mu.Lock()
				prev.ExpiresAt = exp
				prev.Disabled = cd.Disabled
				prev.Auth = cd.Auth
				prev.mu.Unlock()
				creds = append(creds, prev)
				continue
			}
			cred, cerr := newCredential(cd)
			if cerr != nil {
				continue // 校验层已保证可解析；防御性跳过
			}
			creds = append(creds, cred)
		}
		ch.Creds = creds
		ch.mu.Unlock()
		out = append(out, ch)
	}
	m.channels = out
}

// ChannelDef 渠道定义（config JSON 形态；与 Channel 分开是为了解耦配置校验
// 与运行态）。由 cmd/server 的 Config.ExtChannels []ExtChannel 映射而来。
//
// 带 JSON tag 是为了让面板探测接口能直接把 `{channel:{...}}` 草稿解码进来
// （同一套 schema，不在 panel 侧复制一份配置结构）。
type ChannelDef struct {
	Name           string            `json:"name"`
	BaseURL        string            `json:"base_url"`
	APIKey         string            `json:"api_key,omitempty"`
	Protocol       string            `json:"protocol,omitempty"`
	Adapter        string            `json:"adapter,omitempty"` // 原生适配器名（空 = 按协议；如 "loomy"）
	Path           string            `json:"path,omitempty"`
	Auth           provider.AuthSpec `json:"auth,omitempty"`
	Headers        map[string]string `json:"headers,omitempty"`
	Models         []string          `json:"models,omitempty"`
	Aliases        map[string]string `json:"aliases,omitempty"`
	Note           string            `json:"note,omitempty"`
	Credentials    []CredentialDef   `json:"credentials,omitempty"`
	Disabled       bool              `json:"disabled,omitempty"`
	TimeoutSeconds int               `json:"timeout_seconds,omitempty"`
	AllowPrivate   bool              `json:"allow_private,omitempty"` // SSRF 校验放行内网（自建聚合器）；校验在 cmd/server 层做
}

// NormalizeCredentialDefs 把渠道定义归一成凭证清单：
//
//	credentials 非空          → 原样（label 缺省补 "default"）
//	credentials 空 + api_key  → 合成一条 {Label:"default", Secret:api_key}
//	两者皆空                  → 无凭证（自建内网 one-api 场景：不注入鉴权）
//
// api_key 与 credentials 互斥由 config 校验层保证（同时非空启动/保存失败）。
func NormalizeCredentialDefs(d ChannelDef) []CredentialDef {
	if len(d.Credentials) > 0 {
		out := make([]CredentialDef, 0, len(d.Credentials))
		for _, c := range d.Credentials {
			if c.Label == "" {
				c.Label = "default"
			}
			out = append(out, c)
		}
		return out
	}
	if d.APIKey != "" {
		return []CredentialDef{{Label: "default", Secret: d.APIKey, Auth: d.Auth}}
	}
	return nil
}

// Get 按名字取渠道（nil = 不存在或已停用）。
func (m *Manager) Get(name string) *Channel {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, ch := range m.channels {
		if ch.Name == name && !ch.Disabled {
			return ch
		}
	}
	return nil
}

// Status 全渠道观测快照（/status 与面板用）。
func (m *Manager) Status() []Status {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Status, 0, len(m.channels))
	for _, ch := range m.channels {
		out = append(out, ch.Snapshot())
	}
	return out
}

// ---------------------------------------------------------------------------
// 上游 HTTP：chat（流式/非流式）与模型发现
// ---------------------------------------------------------------------------

// endpoint 把 base_url 规范化为 OpenAI API 根（历史兼容 shim）。
//
// 实现已迁到 provider.LegacyOpenAIRoot（阶段三统一在适配层），这里保留薄包装
// 是因为阶段一的 TestEndpoint 用它钉住零回归；新代码请直接用适配器的
// BuildChatURL / BuildModelsURL。
func endpoint(base, path string) string {
	return provider.LegacyOpenAIRoot(base) + path
}

// Chat 转发一次聊天请求到外部渠道（阶段一兼容包装）。
//
// 内部走 Manager.Do（同一套协议适配、凭证选择与状态记账），因此行为与 Do 一致；
// 保留旧签名是为了让既有调用点/测试零改动。新代码请直接用 Do——它能拿到选中
// 凭证的 label 与换证过程（Attempts）。
func (m *Manager) Chat(ctx context.Context, ch *Channel, body []byte, stream bool) (io.ReadCloser, int, []byte, error) {
	res, err := m.Do(ctx, ch, &OutRequest{Body: body, Stream: stream})
	if err != nil {
		return nil, 0, nil, err
	}
	return res.Reader, res.Status, res.Body, nil
}

// headerTimeoutCloser 流式响应的 body 包装：首帧前的「响应头+首包」限时由看门狗
// 定时器执行（超时关闭连接，下次 Read 报错走失败治理）；首帧到达即停表——长流
// 合法时长可达数分钟，此后不再受任何网关侧限时。
type headerTimeoutCloser struct {
	closer io.ReadCloser
	cancel context.CancelFunc
	watch  *time.Timer
	fired  atomic.Bool // 首帧已到（true = 停表完成，看门狗不再动作）
}

// newHeaderTimeoutCloser 启动看门狗：hdrTimeout 内没有数据到达就关连接并取消 ctx。
func newHeaderTimeoutCloser(closer io.ReadCloser, cancel context.CancelFunc, hdrTimeout time.Duration) *headerTimeoutCloser {
	h := &headerTimeoutCloser{closer: closer, cancel: cancel}
	h.watch = time.AfterFunc(hdrTimeout, func() {
		if !h.fired.CompareAndSwap(false, true) {
			return // 首帧已到：不动作
		}
		_ = closer.Close()
		cancel()
	})
	return h
}

// release 首帧已到：停看门狗（幂等；与看门狗的 fired CAS 互斥，两边只会有一边赢）。
func (h *headerTimeoutCloser) release() {
	if h.fired.CompareAndSwap(false, true) {
		h.watch.Stop()
	}
}

func (h *headerTimeoutCloser) Read(p []byte) (int, error) {
	n, err := h.closer.Read(p)
	if n > 0 {
		// 有数据即视为首帧已到（SSE 帧/心跳皆算）。
		h.release()
	}
	return n, err
}

func (h *headerTimeoutCloser) Close() error {
	h.release()
	h.cancel()
	return h.closer.Close()
}

// FetchModels 拉取上游模型目录（自动发现；显式 Models 配置时不调用）。
//
// 走协议适配器的 BuildModels/DecodeModels：OpenAI 是 GET {root}/v1/models、
// Anthropic 是 GET {root}/v1/models、Gemini 是 GET {root}/v1beta/models。
// 凭证选择用第一条 usable；无可用时退回第一条（目录拉取失败不该罚凭证状态）。
func (m *Manager) FetchModels(ctx context.Context, ch *Channel) ([]string, error) {
	if ctx == nil {
		ctx = context.Background() // 防御：ModelList(nil) 等无请求上下文场景
	}
	cred := ch.pickCredential(nil)
	if cred == nil {
		creds := ch.credentials()
		if len(creds) > 0 {
			cred = creds[0]
		}
	}
	target := m.BuildTarget(ch, cred, "")
	adapter, aerr := adapterFor(ch)
	if aerr != nil {
		return nil, aerr
	}
	req, err := adapter.BuildModels(ctx, target)
	if err != nil {
		// 安全红线（P0-4）：query 鉴权下请求 URL 里带秘密值，错误文本必须脱敏
		// （ScrubSecrets 同时处理明文替换与 URL query 脱敏）。
		return nil, errors.New(provider.ScrubSecrets(err.Error(), target.Secret))
	}
	hc := *m.http
	hc.Timeout = 30 * time.Second
	resp, err := hc.Do(req)
	if err != nil {
		return nil, errors.New(provider.ScrubSecrets(err.Error(), target.Secret))
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, errors.New(provider.ScrubSecrets(err.Error(), target.Secret))
	}
	ids, derr := adapter.DecodeModels(target, resp.StatusCode, raw)
	if derr != nil {
		// 上游错误体可能回显请求 URL/参数：统一脱敏后再交出去（会被上层日志打印）。
		return nil, errors.New(provider.ScrubSecrets(derr.Error(), target.Secret))
	}
	return ids, nil
}

// EnsureModels 自动发现模型目录（显式配置时直通返回）。结果缓存 10 分钟；
// 失败进 5 分钟负缓存（不打上游风暴）。
func (m *Manager) EnsureModels(ctx context.Context, ch *Channel) []string {
	ch.mu.Lock()
	if len(ch.Models) > 0 {
		out := append([]string(nil), ch.Models...)
		ch.mu.Unlock()
		return out
	}
	if len(ch.modelsCache) > 0 && time.Since(ch.modelsAt) < 10*time.Minute {
		out := append([]string(nil), ch.modelsCache...)
		ch.mu.Unlock()
		return out
	}
	if !ch.modelsAt.IsZero() && time.Since(ch.modelsAt) < 5*time.Minute {
		// 5 分钟内失败过（modelsAt 驻留但 cache 空）→ 负缓存。
		ch.mu.Unlock()
		return nil
	}
	ch.modelsAt = time.Now() // 占位：并发请求只放一个进去发现
	ch.mu.Unlock()

	ids, err := m.FetchModels(ctx, ch)
	ch.mu.Lock()
	defer ch.mu.Unlock()
	if err != nil {
		log.Printf("WARN: [extchan] fetch models %s: %v", ch.Name, err)
		// 保留 modelsAt 作为负缓存时间戳；下次 5 分钟后再试。
		return nil
	}
	ch.modelsCache = ids
	ch.modelsAt = time.Now()
	return append([]string(nil), ids...)
}

func truncateForLog(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// ---------------------------------------------------------------------------
// 情报拉取（token-fbi data.json）
// ---------------------------------------------------------------------------

// StartIntel 启动额度情报定时拉取（url 为空或 enabled=false 时不启动）。
// 返回停止函数（优雅停机用）。
func (m *Manager) StartIntel(url string, interval time.Duration, enabled bool) func() {
	m.intelMu.Lock()
	m.intelURL, m.intelOn = url, enabled
	m.intelMu.Unlock()
	if url == "" || !enabled {
		return func() {}
	}
	if interval <= 0 {
		interval = 12 * time.Hour
	}
	stop := make(chan struct{})
	go func() {
		// 启动即拉一次（失败静默——情报是辅助信息，不阻塞主链路）。
		m.refreshIntel(url)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				m.refreshIntel(url)
			case <-stop:
				return
			}
		}
	}()
	return func() { close(stop) }
}

func (m *Manager) refreshIntel(url string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	data, err := fetchIntel(ctx, url)
	m.intelMu.Lock()
	defer m.intelMu.Unlock()
	if err != nil {
		m.intelErr = err.Error()
		log.Printf("WARN: [extchan] intel refresh: %v", err)
		return
	}
	m.intel = data
	m.intelAt = time.Now()
	m.intelErr = ""
	log.Printf("[extchan] intel refreshed: %d items (%s)", len(data.Items), url)
}

// Intel 当前情报快照（面板用；未拉取时 ok=false）。
func (m *Manager) Intel() (*IntelData, time.Time, string, bool) {
	m.intelMu.Lock()
	defer m.intelMu.Unlock()
	if m.intel == nil {
		return nil, time.Time{}, m.intelErr, false
	}
	return m.intel, m.intelAt, m.intelErr, true
}

// IntelStatus 面板观测（拉取时间/错误/条数）。
func (m *Manager) IntelStatus() map[string]any {
	m.intelMu.Lock()
	defer m.intelMu.Unlock()
	out := map[string]any{
		"enabled": m.intelOn && m.intelURL != "",
		"url":     m.intelURL,
	}
	if m.intel != nil {
		out["items"] = len(m.intel.Items)
		out["fetched_at"] = m.intelAt
	}
	if m.intelErr != "" {
		out["last_error"] = m.intelErr
	}
	return out
}
