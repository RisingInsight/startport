// Package alert 异常告警推送（运维闭环的最后一环）：关键事件（账号判死/签到
// 连续失败/池无可服务账号）经 webhook 推到 IM，人不用盯着面板。
//
// 设计边界：
//   - 渠道是**预置 payload 模板**的 webhook（飞书 / 钉钉 / Server酱 / 通用 JSON），
//     不做通用集成框架——个人运维场景四个模板覆盖主流 IM，新增渠道=加一个 builder；
//   - 每个事件键有冷却窗口（默认 30 分钟）：同一事件风暴只发第一条，恢复通知
//     用独立键不受同一冷却压制；
//   - 推送绝不阻塞主流程：10s 超时、失败只记日志不重试（下一轮事件或恢复通知
//     自然补上状态），调用方永远不需要 await 结果。
package alert

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// WebhookType 支持的渠道（payload 模板）。
const (
	TypeFeishu     = "feishu"
	TypeDingtalk   = "dingtalk"
	TypeServerChan = "serverchan"
	TypeGeneric    = "generic"
)

// ValidType 报告渠道类型是否受支持（"" 视为未配置，由校验层处理）。
func ValidType(t string) bool {
	switch t {
	case TypeFeishu, TypeDingtalk, TypeServerChan, TypeGeneric:
		return true
	}
	return false
}

// Config 运行配置（来自 config.json 的 alert 节；经 Reconfigure 热生效）。
type Config struct {
	Enabled         bool
	WebhookType     string
	WebhookURL      string
	CooldownMinutes int // 同一事件键的最小重发间隔；<=0 = 30
}

// Manager 告警管理器。并发安全；零配置时是静默 no-op。
type Manager struct {
	mu   sync.RWMutex
	cfg  Config
	last map[string]time.Time // 事件键 → 上次实际发送时刻（运行态，重启归零无害）
	logf func(format string, args ...any)
}

// New 构建 Manager（cfg 零值 = 全部静默）。
func New(cfg Config, logf func(format string, args ...any)) *Manager {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Manager{cfg: cfg, last: map[string]time.Time{}, logf: logf}
}

// Reconfigure 热更新配置（面板保存后调用）；已发送计时保留。
func (m *Manager) Reconfigure(cfg Config) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cfg = cfg
}

// Active 报告当前配置下告警是否在工作（面板与测试端点用）。
func (m *Manager) Active() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.cfg.Enabled && m.cfg.WebhookURL != "" && ValidType(m.cfg.WebhookType)
}

// Send 按键发送一条告警（同键冷却内静默跳过）。title 一行摘要，body 细节可多行。
// 永不返回错误：失败记日志（面板运行日志可见），调用方无需处理。
func (m *Manager) Send(key, title, body string) {
	if err := m.send(key, title, body, false); err != nil {
		m.logf("[alert] 推送失败 key=%s: %v", key, err)
	}
}

// SendTest 绕过冷却发一条测试告警（面板「发送测试」按钮），把结果交回调用方。
func (m *Manager) SendTest(title, body string) error {
	return m.send("test", title, body, true)
}

func (m *Manager) send(key, title, body string, bypassCooldown bool) error {
	m.mu.RLock()
	cfg := m.cfg
	last, sentBefore := m.last[key]
	m.mu.RUnlock()
	if !cfg.Enabled || cfg.WebhookURL == "" || !ValidType(cfg.WebhookType) {
		if bypassCooldown {
			return fmt.Errorf("告警未启用（config.alert 未配置完整）")
		}
		return nil
	}
	cooldown := time.Duration(cfg.CooldownMinutes) * time.Minute
	if cooldown <= 0 {
		cooldown = 30 * time.Minute
	}
	if !bypassCooldown && sentBefore && time.Since(last) < cooldown {
		return nil
	}

	payload, contentType, err := BuildPayload(cfg.WebhookType, title, body)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Post(cfg.WebhookURL, contentType, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("webhook 返回 HTTP %d", resp.StatusCode)
	}
	m.mu.Lock()
	m.last[key] = time.Now()
	m.mu.Unlock()
	m.logf("[alert] 已推送 key=%s title=%s", key, title)
	return nil
}

// BuildPayload 渠道 payload 模板（纯函数，测试锚点）。返回 (请求体, Content-Type)。
// 文案统一 "title\nbody"——IM 消息一行摘要 + 详情。
func BuildPayload(webhookType, title, body string) ([]byte, string, error) {
	text := strings.TrimSpace(title)
	if body != "" {
		text += "\n" + strings.TrimSpace(body)
	}
	switch webhookType {
	case TypeFeishu:
		return marshal(map[string]any{
			"msg_type": "text",
			"content":  map[string]any{"text": text},
		}), "application/json", nil
	case TypeDingtalk:
		return marshal(map[string]any{
			"msgtype": "text",
			"text":    map[string]any{"content": text},
		}), "application/json", nil
	case TypeServerChan:
		// Server酱 Turbo：form 表单 title + desp（markdown）。
		form := url.Values{}
		form.Set("title", title)
		form.Set("desp", body)
		return []byte(form.Encode()), "application/x-www-form-urlencoded", nil
	case TypeGeneric:
		return marshal(map[string]any{
			"title": title,
			"body":  body,
			"time":  time.Now().Format(time.RFC3339),
		}), "application/json", nil
	default:
		return nil, "", fmt.Errorf("未知告警渠道 %q", webhookType)
	}
}

func marshal(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte("{}") // 模板均为纯 map，理论不可达
	}
	return b
}
