// diagnose.go 一键诊断（GET /startport/api/diagnose）：把排障要看的东西一把抓——
// 池健康、各平台上游的 DNS/连通性、外部渠道状态、近期风控拦截计数——一次返回。
//
// 为什么需要它：出问题时用户先看面板，但"哪里坏了"分散在日志/账号页/渠道页——
// 诊断把最常见的问题收敛成一次点击：DNS 没解析（开机断网）、443 连不上（代理故障）、
// 账号全冷却、渠道被 11-128 拦截。
//
// 网络检查是**实时探测**（DNS 解析 + TCP 443 连接，各 4 秒超时，并发执行），
// 不是缓存快照——诊断按钮就是"现在通不通"的权威答案。整体上限约 10 秒。
package server

import (
	"fmt"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/RisingInsight/startport/internal/account"
	"github.com/RisingInsight/startport/internal/logfmt"
	"github.com/RisingInsight/startport/internal/reqlog"
)

// diagPlatform 单个平台上游的网络诊断结果。
type diagPlatform struct {
	Platform string `json:"platform"`
	Host     string `json:"host"`
	DNS      string `json:"dns"` // 解析出的 IP；失败 = "失败: 原因"
	TCP      string `json:"tcp"` // "ok (45ms)" 或 "失败: 原因"
	OK       bool   `json:"ok"`  // DNS + TCP 全过
}

// diagChannel 外部渠道状态摘要。
type diagChannel struct {
	Name  string `json:"name"`
	State string `json:"state"`
	OK    bool   `json:"ok"`
}

// diagResult 诊断总报告。
type diagResult struct {
	Version      string         `json:"version"`
	Pool         map[string]int `json:"pool"`
	Platforms    []diagPlatform `json:"platforms"`
	ExtChannels  []diagChannel  `json:"ext_channels"`
	RecentBlocks int            `json:"recent_blocks_24h"`
	RecentReqs   int            `json:"recent_requests_24h"`
}

// diagNetCheck 单主机网络探测（DNS 解析 + TCP 443 连接），测试注入点。
var diagNetCheck = func(host string) (dns, tcp string, ok bool) {
	ips, err := net.LookupHost(host)
	if err != nil {
		return "失败: " + logfmt.Truncate(err.Error(), 60), "未测试", false
	}
	start := time.Now()
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(ips[0], "443"), 4*time.Second)
	if err != nil {
		return strings.Join(ips, ","), "失败: " + logfmt.Truncate(err.Error(), 60), false
	}
	conn.Close()
	return strings.Join(ips, ","), fmt.Sprintf("ok (%dms)", time.Since(start).Milliseconds()), true
}

// diagnose GET /startport/api/diagnose。单主机探测 4 秒超时 × 并发，
// 总时长由最慢的主机决定（正常 <5 秒全回）。
func (h *Handler) diagnose(w http.ResponseWriter, r *http.Request) {
	res := diagResult{Platforms: []diagPlatform{}, ExtChannels: []diagChannel{}}

	// ① 池健康
	total, healthy, cooling, disabled, full := h.cfg.Pool.CountsDetailed()
	res.Pool = map[string]int{
		"total": total, "healthy": healthy, "cooling": cooling,
		"disabled": disabled, "in_flight_full": full,
	}

	// ② 平台上游网络检查（并发；hosts 去重）
	hosts := h.diagHosts()
	var mu sync.Mutex
	var wg sync.WaitGroup
	for plat, host := range hosts {
		wg.Add(1)
		go func(plat, host string) {
			defer wg.Done()
			dns, tcp, ok := diagNetCheck(host)
			mu.Lock()
			res.Platforms = append(res.Platforms, diagPlatform{Platform: plat, Host: host, DNS: dns, TCP: tcp, OK: ok})
			mu.Unlock()
		}(plat, host)
	}
	wg.Wait()
	sort.Slice(res.Platforms, func(i, j int) bool { return res.Platforms[i].Platform < res.Platforms[j].Platform })

	// ③ 外部渠道状态
	if h.cfg.Ext != nil {
		for _, st := range h.cfg.Ext.Status() {
			res.ExtChannels = append(res.ExtChannels, diagChannel{Name: st.Name, State: st.State, OK: st.State == "available"})
		}
		sort.Slice(res.ExtChannels, func(i, j int) bool { return res.ExtChannels[i].Name < res.ExtChannels[j].Name })
	}

	// ④ 近 24h 风控拦截与请求量（reqlog 归档；未启用则 0）
	res.RecentBlocks, res.RecentReqs = h.recentRiskStats()

	writeJSON(w, http.StatusOK, res)
}

// diagHosts 汇总需要探测的上游主机：平台注册表的默认地址 + CodeBuddy 池上游。
func (h *Handler) diagHosts() map[string]string {
	hosts := map[string]string{}
	for _, spec := range account.Specs() {
		if host := hostOf(spec.DefaultBaseURL); host != "" {
			hosts[spec.Name] = host
		}
	}
	if h.cfg.Upstream != nil {
		if host := hostOf(h.cfg.Upstream.ChatBaseCN); host != "" {
			hosts["CodeBuddy 池上游"] = host
		}
	}
	return hosts
}

// recentRiskStats 从请求归档统计近 24h 的风控拦截（11-128）与总请求量。
// 归档未启用 / RequestLog 未接线时返回 0（诊断不因此失败）。
func (h *Handler) recentRiskStats() (blocks, total int) {
	if h.cfg.RequestLog == nil {
		return 0, 0
	}
	rows, err := h.cfg.RequestLog.ReadArchive(500, reqlog.Filter{From: time.Now().Add(-24 * time.Hour)})
	if err != nil {
		return 0, 0
	}
	for _, e := range rows {
		total++
		if strings.Contains(e.ErrorCode, "11-128") {
			blocks++
		}
	}
	return blocks, total
}

// hostOf 从 URL 提取主机名；解析失败返回空串。
func hostOf(rawURL string) string {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return ""
	}
	if i := strings.Index(rawURL, "://"); i >= 0 {
		rawURL = rawURL[i+3:]
	}
	if i := strings.IndexAny(rawURL, "/:?#"); i >= 0 {
		rawURL = rawURL[:i]
	}
	return rawURL
}
