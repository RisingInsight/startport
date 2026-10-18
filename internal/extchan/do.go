// do.go 出站请求的「选证 → 适配 → 上游 → 失败换证 → 响应折回」主链路。
//
// 这是阶段三的核心：治理（凭证状态机、换证、冷却、熔断）与协议适配在这里汇合，
// server 侧只负责「把 Do 的结果翻译成入站协议形态」。
//
// 换证策略（PRD P0-2 / 架构 §3.4）：
//   - 按配置顺序取第一条 usable 凭证；
//   - 429 / 401 / 403 且尚未换过证、且还有本轮没试过的 usable 凭证 → 换一条重试
//     **最多 1 次**（单请求最多 2 次上游往返，不无限重试）；
//   - 429 只冷却该凭证（60s 指数退避封顶 30m）；全渠道无可用凭证时渠道也冷却；
//   - 401/403 → 该凭证 invalid（退出轮换，改 secret 才恢复）；
//   - 5xx → 渠道 30s 轻冷却、**不换证**（上游整体抖动，换证无意义）；
//   - 传输层失败 → 渠道熔断计数（5 次/10m），不换证；
//   - 400/404/422 → 只记 lastErr、不罚（请求/配置问题，换证也解决不了）。
//
// 无可用凭证 → typed error *Error{Code:"all_credentials_unavailable", Status:503}，
// server 侧翻成 503（/v1/messages 走 Anthropic 的 overloaded_error 形态）。
package extchan

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/RisingInsight/startport/internal/provider"
)

// maxCredAttempts 单请求最多尝试的凭证数（1 次换证）。
const maxCredAttempts = 2

// 治理参数（与阶段一 server 侧同一组量级；现在统一在 extchan 内）。
const (
	// extServerCooldown 5xx 轻冷却（固定短窗防雪崩，不指数升级——5xx 多为上游
	// 自身抖动，很快自愈）。
	extServerCooldown = 30 * time.Second
	// extFailThreshold / extBreaker 传输失败熔断：连败 N 次熔 10 分钟。
	extFailThreshold = 5
	extBreaker       = 10 * time.Minute
)

// Error 带状态码/错误码的出站错误（server 侧据此选择客户端响应形态）。
type Error struct {
	Status  int    // 建议的客户端状态码
	Code    string // 机器可读错误码（error.code）
	Message string // 人类可读消息（已脱敏）
}

// Error 实现 error。
func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	return e.Code + ": " + e.Message
}

// ErrNoCredential 渠道内没有可用凭证（全部冷却/失效/停用/过期）。
var ErrNoCredential = &Error{
	Status:  http.StatusServiceUnavailable,
	Code:    "all_credentials_unavailable",
	Message: "all credentials of this channel are unavailable (cooling / invalid / disabled / expired)",
}

// IsCode 报告 err 是否为指定错误码的出站错误（server 侧分支用，不依赖文案）。
func IsCode(err error, code string) bool {
	var he *Error
	if errors.As(err, &he) {
		return he.Code == code
	}
	return false
}

// OutRequest 一次出站请求（canonical OpenAI wire）。
type OutRequest struct {
	// Body OpenAI Chat Completions JSON（model 已是渠道上游名/别名解析后的名字）。
	Body []byte
	// Stream 是否需要流式（决定 URL 形态与 Accept 头）。
	Stream bool
	// DisplayModel 客户端可见模型名（"ext:<渠道>/<模型或别名>"），
	// 转换响应时写回 model 字段。
	DisplayModel string
}

// DoResult 一次出站请求的结果。
type DoResult struct {
	// Reader 流式响应体（已转换为 OpenAI chunk SSE；调用方负责 Close）。
	Reader io.ReadCloser
	// Status 上游状态码（<400 为成功；>=400 时 Body 携带上游错误）。
	Status int
	// Body 非流式响应体（已转换为 OpenAI wire；status>=400 时是错误信封）。
	Body []byte
	// CredentialLabel 实际服务本次请求的凭证 label（记账/观测用；永不含 secret）。
	CredentialLabel string
	// Attempts 上游往返次数（1 = 没换证；2 = 换证一次）。
	Attempts int
	// Protocol 实际使用的出站协议。
	Protocol provider.Protocol
}

// BuildTarget 把渠道 + 选中凭证投影成出站目标。
// displayModel 为空时回落到 "ext:<渠道名>"（观测用，不影响路由）。
func (m *Manager) BuildTarget(ch *Channel, cred *Credential, displayModel string) *provider.Target {
	if ch == nil {
		return &provider.Target{Protocol: provider.ProtocolOpenAI}
	}
	ch.mu.Lock()
	name := ch.Name
	base := ch.BaseURL
	proto := ch.Protocol
	path := ch.Path
	headers := copyStringMap(ch.Headers)
	auth := ch.Auth
	timeout := time.Duration(ch.TimeoutSeconds) * time.Second
	ch.mu.Unlock()
	if !proto.Valid() {
		proto = provider.ProtocolOpenAI
	}
	secret := ""
	if cred != nil {
		cred.mu.Lock()
		secret = cred.Secret
		cred.mu.Unlock()
		auth = cred.AuthFor(auth)
	}
	if displayModel == "" {
		displayModel = "ext:" + name
	}
	return &provider.Target{
		Name:        name,
		Protocol:    proto,
		BaseURL:     base,
		Path:        path,
		Headers:     headers,
		Auth:        auth,
		Secret:      secret,
		Timeout:     timeout,
		ClientModel: displayModel,
	}
}

// usesNativeAdapter 报告该渠道是否挂了原生适配器。
//
// 用途：原生适配器承载的平台可能用非 OpenAI 的错误体（Loomy 是 {code,desc}），
// 错误路径也要过 ConvertResponse 才能给客户端一个可解析的信封；而三协议基准
// （adapter 为空）的错误体保持逐字节透传（保留上游 error.code，零回归）。
func (c *Channel) usesNativeAdapter() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.Adapter != ""
}

// adapterSelection 读 (protocol, adapter)（加锁；零值协议归一为 openai）。
func (c *Channel) adapterSelection() (provider.Protocol, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	proto := c.Protocol
	if !proto.Valid() {
		proto = provider.ProtocolOpenAI
	}
	return proto, c.Adapter
}

// adapterFor 解析该渠道要用的适配器。
//
//	adapter 非空 → 原生适配器（名字必须已注册、协议必须与 protocol 一致）
//	adapter 为空 → 协议适配器（三协议基准，老配置行为不变）
//
// 解析失败返回 typed error（code=ext_adapter_unknown）：配置写错必须显式暴露，
// 绝不静默回落到协议适配器——那会用错鉴权方式打上游，症状是难以定位的 401。
func adapterFor(ch *Channel) (provider.Adapter, error) {
	proto, name := ch.adapterSelection()
	a, err := provider.AdapterFor(proto, name)
	if err != nil {
		return nil, &Error{
			Status:  http.StatusBadRequest,
			Code:    "ext_adapter_unknown",
			Message: err.Error(),
		}
	}
	return a, nil
}

// Do 执行一次出站请求（含选证、换证与状态记账）。
//
// 返回：
//   - (*DoResult, nil)：拿到上游响应（含 4xx/5xx 的业务错误，Body 里是错误信封）；
//   - (nil, err)：传输层失败 / 无可用凭证 / 协议不支持（err 是 *Error 或原始错误）。
func (m *Manager) Do(ctx context.Context, ch *Channel, req *OutRequest) (*DoResult, error) {
	if ch == nil {
		return nil, &Error{Status: http.StatusBadRequest, Code: "ext_channel_missing", Message: "ext channel is nil"}
	}
	if req == nil {
		req = &OutRequest{}
	}
	if ch.Cooldown() {
		// 渠道冷却且已无可用凭证 → 报"凭证全不可用"（PRD P0-2 的错误码口径）；
		// 渠道冷却但凭证健康（5xx/传输熔断）→ 报渠道冷却（语义不同，客户端处置相同）。
		if ch.pickCredential(nil) == nil {
			return nil, ErrNoCredential
		}
		return nil, &Error{
			Status:  http.StatusServiceUnavailable,
			Code:    "channel_cooling",
			Message: fmt.Sprintf("ext channel %q is cooling down (rate limited / upstream errors), retry later", ch.Name),
		}
	}
	proto := ch.Protocol
	if !proto.Valid() {
		proto = provider.ProtocolOpenAI // 防御：未走 ReplaceAll 的零值协议
	}
	adapter, err := adapterFor(ch)
	if err != nil {
		return nil, err
	}

	tried := make(map[string]bool, maxCredAttempts)
	var last *DoResult
	attempts := 0

	for attempts < maxCredAttempts {
		var cred *Credential
		if attempts == 0 && len(ch.credentials()) == 0 {
			// 无凭证渠道（自建内网 one-api 场景）：不注入任何鉴权，单次尝试。
			cred = &Credential{Label: "none", state: CredHealthy}
		} else {
			cred = ch.pickCredential(tried)
		}
		if cred == nil {
			if attempts == 0 {
				return nil, ErrNoCredential
			}
			break // 换证后没有其它可用凭证：把上一次的上游结果交给客户端
		}
		tried[cred.Label] = true
		attempts++
		started := time.Now()

		target := m.BuildTarget(ch, cred, req.DisplayModel)
		httpReq, err := adapter.BuildRequest(ctx, target, req.Body, req.Stream)
		if err != nil {
			cred.NoteFailure("build request: " + provider.ScrubSecrets(err.Error(), target.Secret))
			return nil, err
		}
		cred.begin()
		rc, status, body, contentType, xerr := m.exec(ctx, ch, httpReq, req.Stream, target.Secret)
		// 在途计数在此处立刻归零：m.exec 已经返回（拿到了响应头或已失败），
		// 与 exec 内部 ch.inFlight 的 defer 语义一致（"上游往返进行中"）。
		// 必须放在所有分支之前，否则 429/401/403/5xx/4xx 这些不返回成功结果的
		// 分支会漏掉 end()，导致 /status 里 in_flight 永久停在 1 并逐次累加。
		cred.end()
		if xerr != nil {
			reason := "transport: " + provider.ScrubSecrets(xerr.Error(), target.Secret)
			cred.NoteFailure(reason)
			ch.NoteFailure(extFailThreshold, m.breaker, reason)
			log.Printf("WARN: [ext] ch=%s cred=%s attempt=%d transport error: %s",
				ch.Name, cred.Label, attempts, reason)
			// 传输错误原文可能带完整 URL（query 鉴权下含秘密值）：统一包成脱敏后的
			// typed error 再交给上层，避免错误体泄露。
			return nil, &Error{
				Status:  http.StatusBadGateway,
				Code:    "upstream_error",
				Message: "ext channel transport error: " + provider.ScrubSecrets(xerr.Error(), target.Secret),
			}
		}

		// 原生适配器可以把"状态码与 body 语义不一致"的响应归一（Loomy 用
		// `HTTP 200 + {code,desc}` 表达 session 过期）。不归一的话，按状态码
		// 判定成功的 SDK 会把错误当正常响应下发。
		//
		// 只在非流式做：流式 body 是待转发的流，预读会破坏逐字节透传
		// （README 已把"流式不做归一"记为已知边界）。
		if !req.Stream {
			if sn, ok := adapter.(provider.StatusNormalizer); ok {
				status, body = sn.NormalizeStatus(status, body)
				if status >= 400 {
					// 适配器侧拿不到 Target（接口不带 secret），这里补一次带本文
					// 凭证的脱敏，防上游把 session 回显进错误文案。
					body = []byte(provider.ScrubSecrets(string(body), target.Secret))
				}
			}
		}

		if status >= 400 {
			if rc != nil {
				_ = rc.Close()
				rc = nil
			}
			// 跨协议错误折回 OpenAI 信封（message 截断 + 脱敏）。
			// 原生适配器也一并折回：它承载的平台可能用非 OpenAI 的错误体
			// （如 Loomy 的 {code,desc}），不转换客户端就解析不了；
			// 三协议基准（adapter 为空）的错误体仍然逐字节透传，零回归。
			if proto != provider.ProtocolOpenAI || ch.usesNativeAdapter() {
				body = adapter.ConvertResponse(target, status, body)
			}
			last = &DoResult{
				Status:          status,
				Body:            body,
				CredentialLabel: cred.Label,
				Attempts:        attempts,
				Protocol:        proto,
			}
		}

		switch {
		case status < 400:
			if req.Stream {
				converted, cerr := adapter.ConvertStream(target, contentType, rc)
				if cerr != nil {
					if rc != nil {
						_ = rc.Close()
					}
					if errors.Is(cerr, provider.ErrStreamUnsupported) {
						cred.NoteFailure("stream unsupported by upstream")
						return nil, &Error{
							Status: http.StatusBadGateway,
							Code:   "gemini_stream_unsupported",
							Message: "upstream did not return text/event-stream for the streaming request; " +
								"retry with \"stream\": false (non-streaming is fully supported)",
						}
					}
					cred.NoteFailure("stream convert: " + provider.ScrubSecrets(cerr.Error(), target.Secret))
					return nil, cerr
				}
				cred.NoteSuccess()
				ch.NoteSuccess()
				logAttempt(ch, cred, status, attempts, started, "ok stream")
				return &DoResult{
					Reader:          converted,
					Status:          status,
					CredentialLabel: cred.Label,
					Attempts:        attempts,
					Protocol:        proto,
				}, nil
			}
			cred.NoteSuccess()
			ch.NoteSuccess()
			outBody := adapter.ConvertResponse(target, status, body)
			recordAttemptLog(ch, cred, status, attempts, started, "ok")
			return &DoResult{
				Status:          status,
				Body:            outBody,
				CredentialLabel: cred.Label,
				Attempts:        attempts,
				Protocol:        proto,
			}, nil

		case status == http.StatusTooManyRequests:
			cred.NoteRateLimited(m.base, m.max, "429 rate limited")
			if !ch.hasUsableCredential() {
				// 全部凭证都被限流：渠道也进入指数退避（客户端快速失败）。
				ch.NoteRateLimited(m.base, m.max, "429 rate limited (all credentials)")
			}
			// 换证前的失败尝试计入该凭证的请求数（v1.4.0 口径）：这次上游往返
			// 真实发生过，只是被换证淘汰。只在"确实会换证"时记——不换证时这次
			// 尝试就是最终结果，由 server 侧 NoteUsage 计，避免双计。
			if attempts < maxCredAttempts && ch.pickCredential(tried) != nil {
				cred.NoteAttempt()
			}
			recordAttemptLog(ch, cred, status, attempts, started, "429 rate limited")

		case status == http.StatusUnauthorized || status == http.StatusForbidden:
			cred.NoteInvalid(fmt.Sprintf("%d authentication failed", status))
			// 同 429：只在确实会换证时计（见上方注释）。
			if attempts < maxCredAttempts && ch.pickCredential(tried) != nil {
				cred.NoteAttempt()
			}
			recordAttemptLog(ch, cred, status, attempts, started, "invalid credential")

		case status >= 500:
			cred.NoteFailure(fmt.Sprintf("%d upstream error", status))
			ch.NoteServerUpstream(extServerCooldown, fmt.Sprintf("%d upstream error", status))
			recordAttemptLog(ch, cred, status, attempts, started, "5xx upstream")
			return last, nil

		default:
			// 400/404/422 等：请求/配置问题，只记 lastErr，不罚渠道也不换证。
			cred.NoteFailure(fmt.Sprintf("%d request error", status))
			recordAttemptLog(ch, cred, status, attempts, started, "4xx request")
			return last, nil
		}
		// 429 / 401 / 403 落到这里：下一轮尝试换证（pickCredential 会跳过已试过的）。
	}

	if last != nil {
		return last, nil
	}
	return nil, ErrNoCredential
}

// exec 执行 HTTP 往返（流式/非流式的超时语义见下方注释）。
// 返回 (reader, status, body, contentType, err)。
func (m *Manager) exec(ctx context.Context, ch *Channel, req *http.Request, stream bool, secret string) (io.ReadCloser, int, []byte, string, error) {
	ch.mu.Lock()
	ch.inFlight++
	hdrTimeout := time.Duration(ch.TimeoutSeconds) * time.Second
	ch.mu.Unlock()
	defer func() {
		ch.mu.Lock()
		ch.inFlight--
		ch.mu.Unlock()
	}()
	if hdrTimeout <= 0 {
		hdrTimeout = 120 * time.Second
	}

	if stream {
		// 流式响应**不能**用 context.WithTimeout 包请求——deadline 到点会取消 ctx
		// 掐断长流。正确形态：WithCancel + 看门狗定时器，首帧到达即停表。
		hc := *m.http
		hc.Timeout = 0
		hc.Transport = m.http.Transport // 共享连接池
		reqCtx, cancel := context.WithCancel(ctx)
		req = req.WithContext(reqCtx)
		resp, err := hc.Do(req)
		if err != nil {
			cancel()
			return nil, 0, nil, "", err
		}
		ct := resp.Header.Get("Content-Type")
		if resp.StatusCode >= 400 {
			defer resp.Body.Close()
			raw, rerr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			cancel()
			if rerr != nil {
				return nil, resp.StatusCode, nil, ct, rerr
			}
			return nil, resp.StatusCode, raw, ct, nil
		}
		mon := newHeaderTimeoutCloser(resp.Body, cancel, hdrTimeout)
		return mon, resp.StatusCode, nil, ct, nil
	}

	// 非流式：整体限时（默认 client.Timeout 已设，这里再给足大模型慢生成）。
	t := hdrTimeout
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
		return nil, 0, nil, "", err
	}
	defer resp.Body.Close()
	raw, rerr := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if rerr != nil {
		return nil, resp.StatusCode, nil, resp.Header.Get("Content-Type"), rerr
	}
	return nil, resp.StatusCode, raw, resp.Header.Get("Content-Type"), nil
}

// recordAttemptLog 单次尝试的观测日志（只打 label，永不打 secret/完整 URL）。
func recordAttemptLog(ch *Channel, cred *Credential, status, attempt int, started time.Time, classify string) {
	log.Printf("[ext] ch=%s cred=%s status=%d attempt=%d ms=%d %s",
		ch.Name, cred.Label, status, attempt, time.Since(started).Milliseconds(), classify)
}

// logAttempt 成功路径的日志（与 recordAttemptLog 分开是为了让 "ok" 与错误分类在
// 日志里一眼可分；两者输出格式一致）。
func logAttempt(ch *Channel, cred *Credential, status, attempt int, started time.Time, classify string) {
	recordAttemptLog(ch, cred, status, attempt, started, classify)
}
