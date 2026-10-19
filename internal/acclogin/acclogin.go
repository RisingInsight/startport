// Package acclogin 账号中心的**登录式添加**（v1.7.0 首批）：qoder 双区 PKCE
// 设备授权 + autoclaw 国内版手机验证码。规格：`starport-login-flows-spec.md`
// §2.1/§2.2/§3（协议细节以它 + C:/tmp/ref/ 为唯一事实来源）。
//
// ── 会话模型（规格 §3.2）─────────────────────────────────────
//   - 每平台同时只允许 1 个进行中会话（新会话顶掉旧的）；
//   - 全局上限 3，超出报错；
//   - 统一超时（默认 5 分钟，可注入）；四条收尾路径（成功/超时/取消/顶替）
//     都显式清理；
//   - 取消后 Wait 立刻报"会话不存在"（不能等超时）。
//
// ── 入池通路（不新开写路径）─────────────────────────────────
// 登录产出 → 组平台 blob → account.Store.Add（既有校验）→ AfterPool 钩子
// （main 注入 = acctchan.Sync 投影）→ 转发即时生效。与 T5 的迁入同一纪律：
// 两份数据必须一致。
//
// ── 安全边界（规格 §3.3）─────────────────────────────────────
//   - verifier / poll 材料 / refresh_token 只存服务端内存，不进日志、不回显；
//   - 授权 URL 只带 challenge/nonce/machine_id（verifier 只在网关发起的轮询
//     URL 里，不出网关）；
//   - 手机号日志只留前 3 后 4（138****8000）；
//   - 失败清理：未入池的半成品凭证丢弃。
package acclogin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/RisingInsight/startport/internal/account"
)

// 会话状态常量（team-lead 契约：pending|success|failed|expired）。
const (
	StatusPending   = "pending"
	StatusSuccess   = "success"
	StatusFailed    = "failed"
	StatusExpired   = "expired"
	StatusCancelled = "cancelled"
)

// 默认参数（规格 §3.2 / §2.1）。
const (
	defaultSessionTTL = 5 * time.Minute  // 会话统一超时
	defaultPollEvery  = 2 * time.Second  // qoder 轮询节奏（建议 2 秒/次）
	defaultWaitMax    = 25 * time.Second // 面板长轮询上限
	maxGlobalSessions = 3                // 全局并发会话上限
	smsResendCooldown = 60 * time.Second // R3：同 (device_id, phone) 发码频控
)

// Config Manager 装配参数（零值 = 官方端点 + 默认参数；测试注入 httptest）。
type Config struct {
	Store *account.Store // 入池落点（必填）
	// AfterPool 账号入池后的钩子（main 注入 = acctchan.Sync 投影；缺省不投影）。
	AfterPool func(platform, accountID string)

	TTL       time.Duration    // 会话超时（默认 5 分钟）
	PollEvery time.Duration    // qoder 轮询间隔（默认 2s）
	WaitMax   time.Duration    // Wait 长轮询上限（默认 25s）
	Now       func() time.Time // 时钟注入
	Logf      func(format string, args ...any)

	// 端点注入（测试；零值 = 官方）。
	QoderOpenAPI    map[bool]string // false=cn / true=intl
	QoderWeb        map[bool]string
	AutoClawUserAPI string // 国内版（短信登录只在国内）
	// MachineIDFile qoder machine id 持久化路径（main 注入 state 同目录文件）。
	MachineIDFile string
	Client        *http.Client
}

// Manager 登录会话管理器（账号中心 v1.7.0）。
type Manager struct {
	cfg  Config
	mu   sync.Mutex
	sess map[string]*Session
	// smsLast (deviceID|phone) → 上次发码时刻（R3：60s 内只放行一次）。
	smsLast map[string]time.Time
	stop    chan struct{}
	stopped bool
}

// NewManager 构造并启动会话超时清扫循环。
func NewManager(cfg Config) (*Manager, error) {
	if cfg.Store == nil {
		return nil, fmt.Errorf("acclogin: Config.Store 必填")
	}
	if cfg.TTL <= 0 {
		cfg.TTL = defaultSessionTTL
	}
	if cfg.PollEvery <= 0 {
		cfg.PollEvery = defaultPollEvery
	}
	if cfg.WaitMax <= 0 {
		cfg.WaitMax = defaultWaitMax
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	// 端点默认值按**键级**回落（env override 只覆盖单侧地区时，另一侧仍用
	// 官方默认——main.go 的 accloginEndpointsFromEnv 只填非空 env）。
	if cfg.QoderOpenAPI == nil {
		cfg.QoderOpenAPI = map[bool]string{}
	}
	if cfg.QoderOpenAPI[false] == "" {
		cfg.QoderOpenAPI[false] = "https://openapi.qoder.com.cn"
	}
	if cfg.QoderOpenAPI[true] == "" {
		cfg.QoderOpenAPI[true] = "https://openapi.qoder.sh"
	}
	if cfg.QoderWeb == nil {
		cfg.QoderWeb = map[bool]string{}
	}
	if cfg.QoderWeb[false] == "" {
		cfg.QoderWeb[false] = "https://qoder.com.cn"
	}
	if cfg.QoderWeb[true] == "" {
		cfg.QoderWeb[true] = "https://qoder.com"
	}
	if cfg.AutoClawUserAPI == "" {
		cfg.AutoClawUserAPI = "https://autoglm-acceleration-api.zhipuai.cn"
	}
	if cfg.Client == nil {
		cfg.Client = &http.Client{Timeout: 15 * time.Second} // ref auth.rs:13 口径
	}
	if cfg.MachineIDFile == "" {
		cfg.MachineIDFile = filepath.Join(os.TempDir(), "starport-qoder-machine-id")
	}
	m := &Manager{
		cfg:     cfg,
		sess:    map[string]*Session{},
		smsLast: map[string]time.Time{},
		stop:    make(chan struct{}),
	}
	go m.reapLoop()
	return m, nil
}

// SetLogf 注入日志（测试静音）。
func (m *Manager) SetLogf(f func(format string, args ...any)) { m.cfg.Logf = f }

// Close 停止清扫循环并作废全部会话。
func (m *Manager) Close() {
	m.mu.Lock()
	if m.stopped {
		m.mu.Unlock()
		return
	}
	m.stopped = true
	sessions := make([]*Session, 0, len(m.sess))
	for _, s := range m.sess {
		sessions = append(sessions, s)
	}
	m.sess = map[string]*Session{}
	m.mu.Unlock()
	for _, s := range sessions {
		s.cancel()
	}
	close(m.stop)
}

// Session 一个进行中的登录会话（材料只存服务端内存——规格 §3.3-1）。
type Session struct {
	ID        string
	Platform  string // qoder / qoder-intl / autoclaw
	CreatedAt time.Time
	ExpiresAt time.Time

	mu         sync.Mutex
	status     string
	errMsg     string
	accountID  string
	loginURL   string     // qoder：给用户浏览器打开
	phone      string     // autoclaw：归一化后的手机号
	deviceID   string     // autoclaw：两跳一致
	blob       jsonRawMap // 登录产出（入池前驻留；Never logged）
	label      string
	cancelled  bool
	terminalAt time.Time          // 终态时刻（reapLoop 延迟清理，Wait 可观察 expired）
	stopPoll   context.CancelFunc // qoder 轮询协程的独立生命周期（v1.7.0 修复）
	done       chan struct{}
}

// jsonRawMap blob 的宽松 map 形态（编码由入池时统一做）。
type jsonRawMap = map[string]any

// cancel 标记取消并唤醒等待者（幂等）。
// 同时停掉 qoder 的后台轮询协程（v1.7.0 修复：四条收尾路径——用户取消 /
// TTL 清扫 / 同平台顶替 / Close——都经这里统一停轮询，不留泄漏；
// stopPoll 由 startQoder 挂上，幂等）。
func (s *Session) cancel() {
	s.mu.Lock()
	if s.status == StatusPending && !s.cancelled {
		s.cancelled = true
		s.status = StatusCancelled
	}
	stop := s.stopPoll
	s.mu.Unlock()
	if stop != nil {
		stop()
	}
	select {
	case <-s.done:
	default:
		close(s.done)
	}
}

// finish 落定终态并唤醒等待者（幂等——只落第一次）。
func (s *Session) finish(status, errMsg, accountID string) {
	s.mu.Lock()
	if s.status == StatusPending {
		s.status = status
		s.errMsg = errMsg
		s.accountID = accountID
		s.terminalAt = time.Now()
	}
	s.mu.Unlock()
	select {
	case <-s.done:
	default:
		close(s.done)
	}
}

// machineID 网关级持久化的 qoder machine id（规格 §2.1-1 的 Go 建议：
// 避免每账号漂移；ref machine.rs:36-65 的持久化语义等价实现——客户端路径
// 探测略去，网关自管一份）。
func (m *Manager) machineID() (string, error) {
	if raw, err := os.ReadFile(m.cfg.MachineIDFile); err == nil {
		if id := strings.TrimSpace(string(raw)); id != "" {
			return id, nil
		}
	}
	id := newUUIDv4NoDash()
	if err := os.MkdirAll(filepath.Dir(m.cfg.MachineIDFile), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(m.cfg.MachineIDFile, []byte(id), 0o600); err != nil {
		return "", err
	}
	return id, nil
}

// SupportedPlatforms 支持登录式添加的平台清单（面板能力开关下发用；
// 规格 §3.4：前端不硬编码）。
func SupportedPlatforms() []string {
	return []string{account.PlatformQoder, account.PlatformQoderIntl, account.PlatformAutoClaw}
}

// newSessionID "lgn-" + 8 hex。
func newSessionID() string {
	buf := make([]byte, 4)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("lgn-%016x", time.Now().UnixNano())
	}
	return "lgn-" + hex.EncodeToString(buf)
}

// newUUIDv4NoDash UUID v4 去横线（qoder nonce 用；ref machine.rs:20-28）。
func newUUIDv4NoDash() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%032x", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// Start 发起登录会话。
//
//   - qoder / qoder-intl：PKCE 设备授权 → 返回授权 URL（用户浏览器打开），
//     后台轮询任务自动完成入池；
//   - autoclaw（国内版）：phone 必填 → 建会话 + 发送短信验证码（60s 频控）；
//     验证码经 VerifySMS 完成入池。
//
// 每平台单会话：同平台新会话顶掉旧的（旧的 Wait 立即报"会话不存在"）。
// 全局上限 3，超出报错（规格 §3.2）。
func (m *Manager) Start(ctx context.Context, platform, region, phone string) (*StartResult, error) {
	// region 归一：qoder + region=intl → qoder-intl（规格 §3.1 的 region 参数）。
	if platform == account.PlatformQoder && strings.EqualFold(region, "intl") {
		platform = account.PlatformQoderIntl
	}
	switch platform {
	case account.PlatformQoder, account.PlatformQoderIntl:
		return m.startQoder(ctx, platform)
	case account.PlatformAutoClaw:
		if strings.TrimSpace(phone) == "" {
			return nil, fmt.Errorf("autoclaw 登录需要手机号")
		}
		return m.startAutoClaw(ctx, phone)
	default:
		return nil, fmt.Errorf("平台 %q 暂不支持登录式添加（支持：qoder / qoder-intl / autoclaw）", platform)
	}
}

// registerSession 平台单会话 + 全局上限检查；被顶替的旧会话显式收尾。
func (m *Manager) registerSession(s *Session) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	var superseded []*Session
	for id, old := range m.sess {
		if old.Platform == s.Platform {
			superseded = append(superseded, old)
			delete(m.sess, id)
		}
	}
	pending := 0
	for _, old := range m.sess {
		old.mu.Lock()
		isPending := old.status == StatusPending
		old.mu.Unlock()
		if isPending {
			pending++
		}
	}
	if pending >= maxGlobalSessions {
		return fmt.Errorf("进行中的登录会话已达上限（%d），请先完成或取消", maxGlobalSessions)
	}
	m.sess[s.ID] = s
	for _, old := range superseded {
		old.finish(StatusCancelled, "已被同平台的新登录会话取代", "")
		old.cancel()
	}
	return nil
}

// StartResult Start 的响应（规格 §3.1 契约）。
type StartResult struct {
	SessionID string
	Mode      string // auth_url | sms
	LoginURL  string // qoder
	PhoneSent bool   // autoclaw
	ExpiresAt time.Time
}

// getSession 取会话（不存在 = 已取消/顶替/超时——Wait 的 404 语义）。
func (m *Manager) getSession(id string) *Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sess[id]
}

// dropSession 移除会话（收尾路径统一走这里）。
func (m *Manager) dropSession(id string) {
	m.mu.Lock()
	s := m.sess[id]
	delete(m.sess, id)
	m.mu.Unlock()
	if s != nil {
		s.cancel()
	}
}

// reapExpired 超时清扫：pending 过期 → 落 expired 终态（**保留在表里**，
// 面板 Wait 要能观察到 expired）；终态超过 1 分钟 → 从表里清掉（Wait 之后
// 报 404，与取消语义一致）。
func (m *Manager) reapExpired(now time.Time) {
	m.mu.Lock()
	var toExpire []*Session
	var toPurge []string
	for id, s := range m.sess {
		s.mu.Lock()
		st, at := s.status, s.terminalAt
		s.mu.Unlock()
		if st == StatusPending && now.After(s.ExpiresAt) {
			toExpire = append(toExpire, s)
		} else if st != StatusPending && !at.IsZero() && now.After(at.Add(time.Minute)) {
			toPurge = append(toPurge, id)
		}
	}
	m.mu.Unlock()
	for _, s := range toExpire {
		s.finish(StatusExpired, "登录会话已超时，请重新发起", "")
	}
	for _, id := range toPurge {
		m.mu.Lock()
		delete(m.sess, id)
		m.mu.Unlock()
	}
}

// reapLoop 超时清扫循环（每 5 秒；幂等——过期会话落 expired 终态）。
func (m *Manager) reapLoop() {
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-m.stop:
			return
		case <-tick.C:
			m.reapExpired(m.cfg.Now())
		}
	}
}

// poolAccount 登录产出 → Store.Add（既有校验）→ AfterPool 钩子（投影）。
// 返回 account ID。
func (m *Manager) poolAccount(a *Session, blob map[string]any, label, note string) (string, error) {
	raw, err := json.Marshal(blob)
	if err != nil {
		return "", fmt.Errorf("序列化登录凭证失败")
	}
	acc, err := m.cfg.Store.Add(a.Platform, raw, label, note)
	if err != nil {
		return "", err
	}
	if m.cfg.AfterPool != nil {
		m.cfg.AfterPool(a.Platform, acc.ID)
	}
	return acc.ID, nil
}

// Wait 面板长轮询（≤WaitMax；500ms 步进检查状态）。
// 会话不存在（已取消/顶替/超时清理）→ 错误（HTTP 层映射 404）。
type WaitStatus struct {
	Status    string `json:"status"`
	AccountID string `json:"account_id,omitempty"`
	Error     string `json:"error,omitempty"`
}

func (m *Manager) Wait(sessionID string) (WaitStatus, error) {
	deadline := m.cfg.Now().Add(m.cfg.WaitMax)
	for {
		s := m.getSession(sessionID)
		if s == nil {
			return WaitStatus{}, fmt.Errorf("登录会话不存在（已取消、被顶替或已超时清理）")
		}
		s.mu.Lock()
		status, accountID, errMsg := s.status, s.accountID, s.errMsg
		s.mu.Unlock()
		if status != StatusPending {
			return WaitStatus{Status: status, AccountID: accountID, Error: errMsg}, nil
		}
		if !m.cfg.Now().Before(deadline) {
			return WaitStatus{Status: StatusPending}, nil
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// Cancel 取消会话（收尾路径之一：立刻从任务表移除并唤醒等待者）。
func (m *Manager) Cancel(sessionID string) error {
	s := m.getSession(sessionID)
	if s == nil {
		return fmt.Errorf("登录会话不存在（已取消、被顶替或已超时清理）")
	}
	m.dropSession(sessionID)
	m.cfg.Logf("[acclogin:%s] 会话 %s 已取消", s.Platform, sessionID)
	return nil
}

// maskPhone 手机号脱敏（前 3 后 4；ref autoclaw/login.rs:429-437）。
func maskPhone(phone string) string {
	chars := []rune(phone)
	if len(chars) < 7 {
		return phone
	}
	return string(chars[:3]) + "****" + string(chars[len(chars)-4:])
}
