// probe.go 面板「测试连通」的只读探测。
//
// 硬约束（PRD P0-7）：
//   - **只读**：只发模型目录请求（openai: GET .../v1/models、anthropic:
//     GET .../v1/models、gemini: GET .../v1beta/models），绝不发 chat
//     （会消耗额度，也违反"只读"语义）；
//   - **不落盘**：草稿只在本次请求内构造临时 Channel/Target，不进 Manager.channels；
//   - **不污染状态**：不调用 NoteSuccess/NoteRateLimited/NoteInvalid，不计
//     in_flight、不写 lastErr——探测失败不能把渠道标成"坏的"；
//   - **独立超时体系**：独立 http.Client{Timeout:10s}（不复用渠道 client，避免
//     连接池/超时互相干扰），但共享 Transport 以复用 TLS 连接；
//   - **响应上限 8MiB**；
//   - **脱敏**：错误摘要经 provider.ScrubSecrets（抹掉 secret 字面量与 URL query）。
package extchan

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/RisingInsight/startport/internal/provider"
)

// probeTimeout 单次探测上限（连接 + 响应体读取）。
const probeTimeout = 10 * time.Second

// probeModelsSample 探测返回的模型样例条数（面板展示用，避免大目录塞满响应）。
const probeModelsSample = 10

// ProbeResult 探测结果（面板直接渲染）。
type ProbeResult struct {
	OK           bool     `json:"ok"`
	Status       int      `json:"status"`
	LatencyMs    int64    `json:"latency_ms"`
	ModelsCount  int      `json:"models_count"`
	ModelsSample []string `json:"models_sample,omitempty"`
	Protocol     string   `json:"protocol"`
	Probe        string   `json:"probe"`
	Error        string   `json:"error,omitempty"`
	ErrorCode    string   `json:"error_code,omitempty"`
}

// probeTransport 探测专用 Transport（共享连接池/默认拨号；不复用渠道 client）。
var probeTransport = &http.Transport{
	Proxy:                 http.ProxyFromEnvironment,
	MaxIdleConns:          8,
	IdleConnTimeout:       60 * time.Second,
	TLSHandshakeTimeout:   10 * time.Second,
	ExpectContinueTimeout: 1 * time.Second,
}

// ProbeDraft 探测一份未保存的渠道草稿。credentialLabel 为空时取第一条凭证。
// 草稿秘密值只在本次请求内存里使用，不落盘、不进管理器。
func (m *Manager) ProbeDraft(ctx context.Context, def ChannelDef, credentialLabel string) ProbeResult {
	proto, err := provider.ParseProtocol(def.Protocol)
	if err != nil {
		return ProbeResult{Protocol: def.Protocol, Probe: "models", Error: err.Error(), ErrorCode: "invalid_protocol"}
	}
	ch := &Channel{
		Name:           def.Name,
		BaseURL:        def.BaseURL,
		Protocol:       proto,
		Path:           def.Path,
		Auth:           def.Auth,
		Headers:        def.Headers,
		Aliases:        def.Aliases,
		Disabled:       def.Disabled,
		TimeoutSeconds: def.TimeoutSeconds,
		AllowPrivate:   def.AllowPrivate,
	}
	var creds []*Credential
	for _, cd := range NormalizeCredentialDefs(def) {
		cred, cerr := newCredential(cd)
		if cerr != nil {
			return ProbeResult{Protocol: string(proto), Probe: "models", Error: cerr.Error(), ErrorCode: "invalid_credential"}
		}
		creds = append(creds, cred)
	}
	return m.probeWith(ctx, ch, creds, credentialLabel, def.BaseURL)
}

// ProbeSaved 探测一条已保存渠道（可选指定凭证 label；空 = 第一条可用，
// 无可用时退回第一条）。
func (m *Manager) ProbeSaved(ctx context.Context, name, credentialLabel string) ProbeResult {
	m.mu.RLock()
	var ch *Channel
	for _, c := range m.channels {
		if c.Name == name {
			ch = c
			break
		}
	}
	m.mu.RUnlock()
	if ch == nil {
		return ProbeResult{Probe: "models", Error: fmt.Sprintf("ext channel %q not found", name), ErrorCode: "channel_not_found"}
	}
	creds := ch.credentials()
	return m.probeWith(ctx, ch, creds, credentialLabel, ch.BaseURL)
}

// probeWith 探测主流程（草稿与已保存共用）。
func (m *Manager) probeWith(ctx context.Context, ch *Channel, creds []*Credential, label, baseURL string) ProbeResult {
	proto := ch.Protocol
	if !proto.Valid() {
		proto = provider.ProtocolOpenAI
	}
	result := ProbeResult{Protocol: string(proto), Probe: "models"}

	// 并发闸：容量 4（探测是面板单用户操作，4 足够；防手抖连点打爆上游）。
	if m.probeSem != nil {
		select {
		case m.probeSem <- struct{}{}:
			defer func() { <-m.probeSem }()
		case <-ctx.Done():
			result.Error = "probe canceled"
			result.ErrorCode = "canceled"
			return result
		}
	}

	// 凭证选择：指定 label 优先；否则第一条可用；再否则第一条。
	var cred *Credential
	if label != "" {
		for _, c := range creds {
			if c.Label == label {
				cred = c
				break
			}
		}
		if cred == nil {
			result.Error = fmt.Sprintf("credential %q not found in channel %q", label, ch.Name)
			result.ErrorCode = "credential_not_found"
			return result
		}
	} else {
		now := time.Now()
		for _, c := range creds {
			if c.usableAt(now) {
				cred = c
				break
			}
		}
		if cred == nil && len(creds) > 0 {
			cred = creds[0]
		}
	}

	target := &provider.Target{
		Name:        ch.Name,
		Protocol:    proto,
		BaseURL:     baseURL,
		Path:        ch.Path,
		Headers:     copyStringMap(ch.Headers),
		Auth:        ch.Auth,
		ClientModel: "ext:" + ch.Name,
	}
	if cred != nil {
		cred.mu.Lock()
		target.Secret = cred.Secret
		cred.mu.Unlock()
		target.Auth = cred.AuthFor(ch.Auth)
	}

	adapter, ok := provider.DefaultRegistry.Get(proto)
	if !ok {
		result.Error = fmt.Sprintf("no adapter for protocol %q", proto)
		result.ErrorCode = "invalid_protocol"
		return result
	}
	req, err := adapter.BuildModels(ctx, target)
	if err != nil {
		result.Error = provider.ScrubSecrets(err.Error(), target.Secret)
		result.ErrorCode = "invalid_request"
		return result
	}

	client := &http.Client{Timeout: probeTimeout, Transport: probeTransport}
	started := time.Now()
	resp, err := client.Do(req)
	result.LatencyMs = time.Since(started).Milliseconds()
	if err != nil {
		result.Error = provider.ScrubSecrets(err.Error(), target.Secret)
		result.ErrorCode = "transport_error"
		return result
	}
	defer resp.Body.Close()
	body, rerr := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if rerr != nil {
		result.Status = resp.StatusCode
		result.Error = provider.ScrubSecrets(rerr.Error(), target.Secret)
		result.ErrorCode = "read_error"
		return result
	}
	result.Status = resp.StatusCode
	if resp.StatusCode != http.StatusOK {
		result.Error = provider.ScrubSecrets(truncateForLog(string(body), 300), target.Secret)
		result.ErrorCode = fmt.Sprintf("upstream_%d", resp.StatusCode)
		return result
	}
	models, derr := adapter.DecodeModels(target, resp.StatusCode, body)
	if derr != nil {
		result.Error = provider.ScrubSecrets(derr.Error(), target.Secret)
		result.ErrorCode = "decode_error"
		return result
	}
	result.OK = true
	result.ModelsCount = len(models)
	if n := len(models); n > probeModelsSample {
		result.ModelsSample = append([]string(nil), models[:probeModelsSample]...)
	} else {
		result.ModelsSample = append([]string(nil), models...)
	}
	return result
}
