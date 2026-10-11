// Package update 版本检查：从固定可信源读取最新发布版本，供面板与桌面端提示升级。
//
// 安全边界（安全官 F-003 红线，务必保持）：
//   - 仅「检查 + 提示」，**绝不自动下载或执行**任何产物；
//   - 只访问编译期硬编码的可信源主机白名单，强制 HTTPS，禁止重定向到白名单外主机；
//   - 版本单调：只提示比当前更高的版本，且拒绝采用低于「已见最高版本」的响应
//     （防降级/回放；最高版本持久化到 state 文件，跨重启仍生效）；
//   - 失败静默降级：网络不可达 / 限流 / 解析失败都不报错、不阻塞，保留上次结果，
//     仅在 Info.Error 里留一条观测信息；
//   - 提示里的 release 链接由网关**自行拼装**（不采用响应体里的 URL），这样即使
//     响应被篡改也无法把用户导向站外。
//
// 本包只做「通知」。产物的签名校验（ed25519）与下载替换属后续阶段，需发布流水线
// 配套（见 deliverables 报告），不在本包范围内。
package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// defaultRepo 官方仓库（编译期固定，不接受配置——固定可信源是安全红线）。
const defaultRepo = "RisingInsight/startport"

// allowedHosts 可信源主机白名单（编译期硬编码）。请求与重定向目标都必须命中其一。
var allowedHosts = map[string]bool{
	"api.github.com":            true,
	"github.com":                true,
	"raw.githubusercontent.com": true,
	"cdn.jsdelivr.net":          true,
}

// defaultInterval 默认检查间隔（6h）。
const defaultInterval = 6 * time.Hour

// initialDelay 启动后首次检查的延迟：避开启动期（预热模型倍率等）的网络竞争。
const initialDelay = 10 * time.Second

// retryInterval 失败后的快速重试间隔。若沿用 interval（默认 6h），一次瞬时网络
// 抖动就会让用户 6 小时内看不到更新提示——首查失败（无 latest）或出错时改用它重试。
const retryInterval = 15 * time.Minute

// Info 一次版本检查的结果（JSON 直接透出给面板/桌面端）。
type Info struct {
	Current     string `json:"current"`               // 当前版本
	Latest      string `json:"latest,omitempty"`      // 最新版本（未检查到时为空）
	Available   bool   `json:"update_available"`      // 是否有更新
	ReleaseURL  string `json:"release_url,omitempty"` // 发布页链接（网关拼装）
	PublishedAt string `json:"published_at,omitempty"`
	CheckedAt   string `json:"checked_at,omitempty"`
	Source      string `json:"source,omitempty"` // "github" / "jsdelivr"
	Enabled     bool   `json:"enabled"`          // 是否启用了更新检查
	Error       string `json:"error,omitempty"`  // 最近一次失败（静默降级时带出）
}

// Config 检查器配置。
type Config struct {
	Current   string        // 当前版本（appVersion）
	Interval  time.Duration // 检查间隔；<=0 回落 defaultInterval
	Enabled   bool          // 是否启用
	StatePath string        // 单调性状态文件路径；空 = 不持久化（进程内单调）
}

// Checker 更新检查器（并发安全；后台周期刷新 + 缓存只读读取）。
type Checker struct {
	current   string
	interval  time.Duration
	enabled   bool
	statePath string
	client    *http.Client

	mu        sync.Mutex
	cached    Info
	lastCheck time.Time
	highest   string // 已见最高版本（单调性；持久化）

	// 源地址。生产恒为下方默认值；仅同包测试可覆盖（unexported，配置无法改动，
	// 固定可信源是安全红线）。空 = 用默认。
	baseGitHub   string
	baseJsDelivr string
}

// New 构造检查器。Enabled=false 时所有读取都返回 enabled=false（不发起任何网络请求）。
func New(cfg Config) *Checker {
	interval := cfg.Interval
	if interval <= 0 {
		interval = defaultInterval
	}
	c := &Checker{
		current:   strings.TrimSpace(cfg.Current),
		interval:  interval,
		enabled:   cfg.Enabled,
		statePath: cfg.StatePath,
		client:    newClient(),
	}
	c.loadHighest()
	return c
}

// newClient 构造带白名单重定向策略的 HTTP client。超时覆盖整次请求（含重定向）。
func newClient() *http.Client {
	return &http.Client{
		Timeout: 10 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return errors.New("too many redirects")
			}
			if !allowedHosts[strings.ToLower(req.URL.Hostname())] {
				return fmt.Errorf("redirect to disallowed host %q", req.URL.Hostname())
			}
			return nil
		},
	}
}

// Cached 返回最近一次检查结果（**不做任何网络 I/O**，供 overview 等高频路径使用）。
func (c *Checker) Cached() Info {
	if c == nil {
		return Info{Enabled: false}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.enabled {
		return Info{Current: c.current, Enabled: false}
	}
	if c.cached.Current == "" {
		c.cached.Current = c.current
		c.cached.Enabled = true
	}
	return c.cached
}

// Refresh 立即执行一次检查（可能阻塞至 HTTP 超时；仅手动「检查更新」与后台循环调用）。
func (c *Checker) Refresh(ctx context.Context) Info {
	if c == nil || !c.enabled {
		return c.Cached()
	}
	latest, published, source, err := c.fetch(ctx)

	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	c.lastCheck = now
	if err != nil {
		// 静默降级：保留上次结果，只补一条 error 供观测。
		info := c.cached
		if info.Current == "" {
			info.Current = c.current
		}
		info.Enabled = true
		info.Error = err.Error()
		info.CheckedAt = now.Format(time.RFC3339)
		c.cached = info
		return info
	}
	// 单调性：采用「已见最高版本」，拒绝回放更低版本（防降级）。
	if c.highest == "" || newer(latest, c.highest) {
		c.highest = latest
		c.saveHighest()
	} else if newer(c.highest, latest) {
		latest = c.highest
	}
	info := Info{
		Current:     c.current,
		Latest:      latest,
		Available:   newer(latest, c.current),
		ReleaseURL:  c.releaseURL(latest),
		PublishedAt: published,
		CheckedAt:   now.Format(time.RFC3339),
		Source:      source,
		Enabled:     true,
	}
	c.cached = info
	return info
}

// Start 启动后台周期检查（ctx 取消即停）。首查延迟 initialDelay；之后按 interval，
// 但若上一次检查失败（带 error 或未取到 latest），改按 retryInterval 快速重试——
// 避免一次瞬时网络抖动导致 6 小时内都不再尝试。
func (c *Checker) Start(ctx context.Context) {
	if c == nil || !c.enabled {
		return
	}
	go func() {
		select {
		case <-ctx.Done():
			return
		case <-time.After(initialDelay):
		}
		for {
			info := c.Refresh(ctx)
			delay := c.interval
			if info.Error != "" || info.Latest == "" {
				delay = retryInterval
			}
			t := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				t.Stop()
				return
			case <-t.C:
			}
		}
	}()
}

// fetch 依次尝试主源（GitHub Releases API）与兜底源（jsDelivr version.json）。
// 任一成功即返回；全失败返回**两个源的错误**（便于诊断，避免只看到兜底源报错）。
func (c *Checker) fetch(ctx context.Context) (version, published, source string, err error) {
	v, p, ghErr := c.fetchGitHub(ctx)
	if ghErr == nil {
		return v, p, "github", nil
	}
	v2, jsErr := c.fetchJsDelivr(ctx)
	if jsErr == nil {
		return v2, "", "jsdelivr", nil
	}
	return "", "", "", fmt.Errorf("github: %v; jsdelivr: %v", ghErr, jsErr)
}

func (c *Checker) fetchGitHub(ctx context.Context) (version, published string, err error) {
	u := c.baseGitHub
	if u == "" {
		u = "https://api.github.com/repos/" + defaultRepo + "/releases/latest"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "starport-update-check")
	resp, err := c.client.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("github api status %d", resp.StatusCode)
	}
	var r struct {
		TagName     string `json:"tag_name"`
		PublishedAt string `json:"published_at"`
		Draft       bool   `json:"draft"`
		Prerelease  bool   `json:"prerelease"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return "", "", err
	}
	if r.TagName == "" {
		return "", "", errors.New("github api: empty tag_name")
	}
	if r.Draft || r.Prerelease {
		// 草稿/预发布不作为「最新正式版」提示（避免把演练 tag 推给用户）。
		return "", "", errors.New("github api: latest is draft/prerelease")
	}
	return r.TagName, r.PublishedAt, nil
}

func (c *Checker) fetchJsDelivr(ctx context.Context) (version string, err error) {
	u := c.baseJsDelivr
	if u == "" {
		u = "https://cdn.jsdelivr.net/gh/" + defaultRepo + "@main/version.json"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "starport-update-check")
	resp, err := c.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("jsdelivr status %d", resp.StatusCode)
	}
	var r struct {
		Version string `json:"version"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return "", err
	}
	if strings.TrimSpace(r.Version) == "" {
		return "", errors.New("jsdelivr: empty version")
	}
	return r.Version, nil
}

// releaseURL 由网关自行拼装发布页链接（不采用响应体里的 URL）。
func (c *Checker) releaseURL(version string) string {
	v := strings.TrimPrefix(strings.TrimSpace(version), "v")
	if v == "" {
		return ""
	}
	return "https://github.com/" + defaultRepo + "/releases/tag/v" + v
}

// ---------------------------------------------------------------- 版本比较 ----

// versionRe 匹配形如 "v1.13.0" / "1.13.0-panel" / "1.13" 的版本串，取前导数字三元组。
var versionRe = regexp.MustCompile(`^v?(\d+)\.(\d+)(?:\.(\d+))?`)

// parseVersion 解析版本串为 [major, minor, patch]。解析失败返回 ok=false。
func parseVersion(s string) ([3]int, bool) {
	m := versionRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return [3]int{}, false
	}
	var out [3]int
	for i := 0; i < 3; i++ {
		if i+1 < len(m) && m[i+1] != "" {
			n, err := strconv.Atoi(m[i+1])
			if err != nil {
				return [3]int{}, false
			}
			out[i] = n
		}
	}
	return out, true
}

// newer 报告 a 是否比 b 更新。任一侧无法解析时返回 false（宁可漏报也不误报）。
func newer(a, b string) bool {
	va, oka := parseVersion(a)
	vb, okb := parseVersion(b)
	if !oka || !okb {
		return false
	}
	for i := 0; i < 3; i++ {
		if va[i] != vb[i] {
			return va[i] > vb[i]
		}
	}
	return false
}

// ---------------------------------------------------------------- 状态持久化 ----

// loadHighest 从 state 文件恢复「已见最高版本」（缺失/损坏静默忽略）。
func (c *Checker) loadHighest() {
	if c.statePath == "" {
		return
	}
	raw, err := os.ReadFile(c.statePath)
	if err != nil {
		return
	}
	var f struct {
		Highest string `json:"highest"`
	}
	if json.Unmarshal(raw, &f) == nil {
		c.highest = f.Highest
	}
}

// saveHighest 原子写回「已见最高版本」（失败静默：单调性降级为进程内有效）。
func (c *Checker) saveHighest() {
	if c.statePath == "" {
		return
	}
	raw, err := json.Marshal(struct {
		Highest string `json:"highest"`
	}{c.highest})
	if err != nil {
		return
	}
	if dir := filepath.Dir(c.statePath); dir != "" && dir != "." {
		_ = os.MkdirAll(dir, 0o755)
	}
	tmp := c.statePath + ".tmp"
	if os.WriteFile(tmp, raw, 0o600) != nil {
		return
	}
	_ = os.Rename(tmp, c.statePath)
}
