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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Channel 单个外部渠道的定义与运行态。
type Channel struct {
	// Name 渠道名（路由用，"ext:<name>/<model>"）。小写字母/数字/连字符/下划线。
	Name string `json:"name"`
	// BaseURL 上游 OpenAI 兼容根地址（如 https://api.siliconflow.cn）。
	// 网关在其后拼 /v1/chat/completions 与 /v1/models；也接受已带 /v1 的形态。
	BaseURL string `json:"base_url"`
	// APIKey 上游 Bearer 密钥（仅落 config.json，接口不回显）。
	APIKey string `json:"api_key"`
	// Models 显式模型名单（优先）。空 = 启动后从 /v1/models 自动发现。
	// 显式名单是计费/白名单场景的推荐形态：自动发现可能带出付费模型。
	Models []string `json:"models,omitempty"`
	// Disabled 停用开关：true 时不参与路由与模型列表。
	Disabled bool `json:"disabled,omitempty"`
	// TimeoutSeconds 单次上游请求（连接到响应头）上限；<=0 回落 120。
	TimeoutSeconds int `json:"timeout_seconds,omitempty"`

	// ---- 运行态（不落盘；进程内状态机）----

	mu          sync.Mutex
	coolUntil   time.Time // 429/5xx 冷却截止
	coolStreak  int       // 连续限流次数（指数退避）
	failStreak  int       // 连续失败次数（熔断计数）
	modelsCache []string // 自动发现的模型名单缓存
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

// Status 面板观测快照。
type Status struct {
	Name       string    `json:"name"`
	BaseURL    string    `json:"base_url"`
	Disabled   bool      `json:"disabled"`
	Models     []string  `json:"models"`
	AutoModels []string  `json:"auto_models,omitempty"`
	Cooling    bool      `json:"cooling"`
	CoolRemain int64     `json:"cool_remaining_sec"`
	InFlight   int       `json:"in_flight"`
	LastErr    string    `json:"last_error,omitempty"`
	LastOK     time.Time `json:"last_ok,omitempty"`
	ModelsAt   time.Time `json:"models_at,omitempty"`
}

// Snapshot 渠道观测快照（不含 key——面板与 /status 都不回显）。
func (c *Channel) Snapshot() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := Status{
		Name:     c.Name,
		BaseURL:  c.BaseURL,
		Disabled: c.Disabled,
		Models:   append([]string(nil), c.Models...),
		InFlight: c.inFlight,
		LastErr:  c.lastErr,
		LastOK:   c.lastOK,
		ModelsAt: c.modelsAt,
	}
	if now := time.Now(); now.Before(c.coolUntil) {
		st.Cooling = true
		st.CoolRemain = int64(c.coolUntil.Sub(now).Seconds())
	}
	if len(c.modelsCache) > 0 {
		st.AutoModels = append([]string(nil), c.modelsCache...)
	}
	return st
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
		http:    &http.Client{Timeout: time.Duration(cfg.TimeoutSeconds) * time.Second},
		base:    cfg.RateBase,
		max:     cfg.RateMax,
		breaker: cfg.Breaker,
	}
}

// ReplaceAll 热更新渠道定义（面板保存配置时调用）：按名字复用既有 Channel 的
// 运行态（冷却/熔断/目录缓存），新渠道全新建，消失的渠道丢弃。
// 更新 base_url 时丢弃该渠道的模型自动发现缓存（旧目录对新上游已失效）。
func (m *Manager) ReplaceAll(defs []ChannelDef) {
	m.mu.Lock()
	defer m.mu.Unlock()
	old := map[string]*Channel{}
	for _, ch := range m.channels {
		old[ch.Name] = ch
	}
	out := make([]*Channel, 0, len(defs))
	for _, d := range defs {
		if d.Name == "" || d.BaseURL == "" {
			continue // 校验在 config 层做过；防御性跳过
		}
		ch, ok := old[d.Name]
		if ok && ch.BaseURL != d.BaseURL {
			ch.mu.Lock()
			ch.modelsCache, ch.modelsAt = nil, time.Time{}
			ch.mu.Unlock()
		}
		if !ok {
			ch = &Channel{Name: d.Name}
		}
		ch.BaseURL = d.BaseURL
		ch.APIKey = d.APIKey
		ch.Models = d.Models
		ch.Disabled = d.Disabled
		ch.TimeoutSeconds = d.TimeoutSeconds
		out = append(out, ch)
	}
	m.channels = out
}

// ChannelDef 渠道定义（config JSON 形态；与 Channel 分开是为了解耦配置校验
// 与运行态）。由 cmd/server 的 Config.ExtChannels []ExtChannel 映射而来。
type ChannelDef struct {
	Name           string
	BaseURL        string
	APIKey         string
	Models         []string
	Disabled       bool
	TimeoutSeconds int
	AllowPrivate   bool // SSRF 校验放行内网（自建聚合器）；校验在 cmd/server 层做
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

// HasPrefix 报告 model 是否以 "ext:<channel>/" 形式指向本 Manager 管辖的渠道。
// 返回渠道名与渠道内裸模型名。
func (m *Manager) HasPrefix(model string) (channel, bare string, ok bool) {
	const p = "ext:"
	if !strings.HasPrefix(model, p) {
		return "", "", false
	}
	rest := model[len(p):]
	idx := strings.IndexByte(rest, '/')
	if idx <= 0 || idx == len(rest)-1 {
		return "", "", false
	}
	return rest[:idx], rest[idx+1:], true
}

// RouteFor 解析请求模型名 → 路由目标。前缀 "ext:<channel>/<model>" 优先；
// 裸名时在各渠道可用模型中查唯一匹配（多渠道同名模型 → 不路由，让客户端用
// 前缀消歧——静默选一个会隐藏另一渠道的存在）。
func (m *Manager) RouteFor(model string) (*Channel, string, error) {
	if ch, bare, ok := m.HasPrefix(model); ok {
		ch2 := m.Get(ch)
		if ch2 == nil {
			return nil, "", fmt.Errorf("ext channel %q not found or disabled", ch)
		}
		return ch2, bare, nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	var found *Channel
	var bare string
	for _, ch := range m.channels {
		if ch.Disabled {
			continue
		}
		for _, mm := range ch.availableModels() {
			if mm == model {
				if found != nil {
					return nil, "", fmt.Errorf("model %q exists in multiple ext channels, use ext:<channel>/%s prefix", model, model)
				}
				found, bare = ch, mm
			}
		}
	}
	if found == nil {
		return nil, "", errNotExt
	}
	return found, bare, nil
}

// errNotExt 模型不属于任何外部渠道（调用方回落 CodeBuddy 池）。
var errNotExt = errors.New("not an ext-channel model")

// IsExtModel 报告模型是否可由外部渠道服务（裸名或前缀形态）。
func (m *Manager) IsExtModel(model string) bool {
	_, _, err := m.RouteFor(model)
	return err == nil
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

// endpoint 把 base_url 规范化为 API 根（去掉尾斜杠；补缺省 /v1——用户填
// https://api.siliconflow.cn 与 https://api.siliconflow.cn/v1 都可用）。
func endpoint(base, path string) string {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if !strings.HasSuffix(base, "/v1") {
		// 兼容 Azure 式无 /v1 部署：用户配 /openai/v1 时已带 /v1 不会被补。
		// 带 /v4、/api 等前缀的专有形态走显式 /v1 结尾配置。
		if path != "" && strings.Contains(base, "/v1") {
			// base 已含 /v1（如 /openai/v1）→ 不再补
		} else {
			base += "/v1"
		}
	}
	return base + path
}

// Chat 转发一次聊天请求到外部渠道。
//   - stream=true：返回打开的上游 SSE 流（调用方负责 Close）；首字节前超时由
//     http.Client.Timeout 兜底（长流不受其影响——Timeout 是整个响应体读取上限，
//     见下方 streamClient 说明）。
//   - stream=false：读完整 body 返回。
// status>=400 时返回 respBody（上游错误原文）+ nil error；传输层错误返回 err。
// 调用方按返回分类做渠道治理（noteRateLimited 等）。
func (m *Manager) Chat(ctx context.Context, ch *Channel, body []byte, stream bool) (rc io.ReadCloser, status int, respBody []byte, err error) {
	url := endpoint(ch.BaseURL, "/chat/completions")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	key := strings.TrimSpace(ch.APIKey)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	// 观测：在途计数。
	ch.mu.Lock()
	ch.inFlight++
	ch.mu.Unlock()
	defer func() {
		ch.mu.Lock()
		ch.inFlight--
		ch.mu.Unlock()
	}()

	// 渠道级超时：流式响应**不能**用 context.WithTimeout 包请求——WithTimeout 的
	// deadline 到点会取消 ctx 掐断长流，而「首帧后调 cancel 释放」也不行（cancel
	// 本身就是立即取消）。正确形态：WithCancel + 看门狗定时器——首帧前超时由看门狗
	// 关闭连接；首帧到达即 Stop 看门狗，流不再受任何网关侧限时（由客户端断连控制）。
	if stream {
		hc := *m.http
		hc.Timeout = 0
		hc.Transport = m.http.Transport // 共享连接池
		hdrTimeout := time.Duration(ch.TimeoutSeconds) * time.Second
		if hdrTimeout <= 0 {
			hdrTimeout = 120 * time.Second
		}
		reqCtx, cancel := context.WithCancel(ctx)
		req = req.WithContext(reqCtx)
		resp, err := hc.Do(req)
		if err != nil {
			cancel()
			return nil, 0, nil, err
		}
		if resp.StatusCode >= 400 {
			defer resp.Body.Close()
			raw, rerr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			cancel()
			if rerr != nil {
				return nil, resp.StatusCode, nil, rerr
			}
			return nil, resp.StatusCode, raw, nil
		}
		// 200：看门狗接管首字节限时（cancel 所有权移交 body 包装器，Close 时释放）。
		mon := newHeaderTimeoutCloser(resp.Body, cancel, hdrTimeout)
		return mon, resp.StatusCode, nil, nil
	}

	// 非流式：整体限时（默认 client.Timeout 已设，但用 ctx 显式超时更直观）。
	t := time.Duration(ch.TimeoutSeconds) * time.Second
	if t <= 0 {
		t = 120 * time.Second
	}
	// 非流式生成也可合法长达数分钟（大模型慢），超时给足 2 倍 channel timeout
	// 且封顶 600s；再长让客户端自己断。
	if t < 60*time.Second {
		t = 60 * time.Second
	}
	if t > 600*time.Second {
		t = 600 * time.Second
	}
	uctx, cancel := context.WithTimeout(ctx, t*2)
	defer cancel()
	req = req.WithContext(uctx)
	resp, err := m.http.Do(req)
	if err != nil {
		return nil, 0, nil, err
	}
	defer resp.Body.Close()
	raw, rerr := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if rerr != nil {
		return nil, resp.StatusCode, nil, rerr
	}
	return nil, resp.StatusCode, raw, nil
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
func (m *Manager) FetchModels(ctx context.Context, ch *Channel) ([]string, error) {
	url := endpoint(ch.BaseURL, "/models")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if key := strings.TrimSpace(ch.APIKey); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	hc := *m.http
	hc.Timeout = 30 * time.Second
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("upstream %d: %s", resp.StatusCode, truncateForLog(string(raw), 200))
	}
	var parsed struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("parse models: %w", err)
	}
	out := make([]string, 0, len(parsed.Data))
	for _, d := range parsed.Data {
		if d.ID != "" {
			out = append(out, d.ID)
		}
	}
	return out, nil
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

// ModelList 汇总全部启用渠道的模型（/v1/models 合并用）。含自动发现（懒触发）。
func (m *Manager) ModelList(ctx context.Context) []map[string]any {
	m.mu.RLock()
	chs := append([]*Channel(nil), m.channels...)
	m.mu.RUnlock()
	out := []map[string]any{}
	for _, ch := range chs {
		if ch.Disabled {
			continue
		}
		ids := ch.availableModels()
		if len(ch.Models) == 0 {
			ids = m.EnsureModels(ctx, ch)
		}
		for _, id := range ids {
			out = append(out, map[string]any{
				"id":       "ext:" + ch.Name + "/" + id,
				"object":   "model",
				"created":  1753600000,
				"owned_by": "ext:" + ch.Name,
			})
		}
	}
	return out
}

// ModelExists 报告某渠道是否声明了某模型（前缀路由用；裸名自动发现下放 RouteFor）。
func (m *Manager) ModelExists(channel, model string) bool {
	ch := m.Get(channel)
	if ch == nil {
		return false
	}
	ids := ch.availableModels()
	if len(ids) == 0 {
		return true // 目录未知（显式空 + 未发现成功）：放行，错误由上游报
	}
	for _, id := range ids {
		if id == model {
			return true
		}
	}
	return false
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
