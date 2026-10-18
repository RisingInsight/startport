// extrelay.go 外部渠道（ext:<channel>/<model>）的聊天转发：独立于 CodeBuddy
// 轮转主链路。设计要点见 internal/extchan 包注释与 handler.Config.Ext。
//
// 与 CodeBuddy 路径的关键差异：
//   - 不轮换账号（凭证池在渠道内，由 extchan.Do 选证/换证）——失败按凭证与渠道
//     两级状态机治理；
//   - 不做提示词改写/指纹脱敏/DSML 修复（那是 CodeBuddy 上游的特有对抗，
//     标准上游不需要，透传是唯一正确语义）；
//   - model 字段重写为渠道内上游名（前缀/别名是网关侧路由协议）；
//   - usage 记账照常（realm 固定 "ext"，uid 用渠道名）+ 凭证级进程内计数。
package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/RisingInsight/startport/internal/extchan"
	"github.com/RisingInsight/startport/internal/logfmt"
	"github.com/RisingInsight/startport/internal/pool"
	"github.com/RisingInsight/startport/internal/provider"
	"github.com/RisingInsight/startport/internal/reqlog"
	"github.com/RisingInsight/startport/internal/usage"
)

// handleExtChat 外部渠道聊天入口（chatCompletions 前缀路由短路后进到这里）。
// body 为原始请求体（未做任何提示词改写）；model 为入站原始模型名。
func (h *Handler) handleExtChat(w http.ResponseWriter, r *http.Request,
	st *chatStat, body []byte, model string, apiKeyName string) {

	ch2, upstream, display, rerr := h.cfg.Ext.RouteModel(model)
	if rerr != nil {
		writeOpenAIError(w, http.StatusBadRequest, "model_not_found", rerr.Error())
		st.status = http.StatusBadRequest
		st.outcome = reqlog.OutcomeHTTPError
		return
	}
	st.uid = "ext:" + ch2.Name
	st.nick = ""
	st.model = display

	// model 字段重写为渠道内上游名（前缀/别名剥离）。body 不可解析时原样
	// 透传（上游会给出更准确的错误）。
	outBody := rewriteModel(body, upstream)

	// stream 判定：与主链路一致，按 body 的 stream 字段。
	var peek struct {
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(outBody, &peek)

	attemptStarted := time.Now()
	res, derr := h.cfg.Ext.Do(r.Context(), ch2, &extchan.OutRequest{
		Body:         outBody,
		Stream:       peek.Stream,
		DisplayModel: display,
	})
	if derr != nil {
		// 无可用凭证 / 渠道冷却 / 传输失败 / Gemini 流式不支持：统一映射。
		status, code, msg := extErrorFields(derr)
		recordExtAttempt(h, st, apiKeyName, ch2, nil, upstream, pool.TokenUsageDelta{}, attemptStarted, 0, display)
		st.status = status
		st.outcome = reqlog.OutcomeHTTPError
		log.Printf("WARN: [server] ext=%s model=%s: %s (%s)", ch2.Name, upstream, code, msg)
		writeOpenAIError(w, status, code, msg)
		return
	}
	cred := ch2.CredentialByLabel(res.CredentialLabel)

	if res.Status >= 400 {
		recordExtAttempt(h, st, apiKeyName, ch2, cred, upstream, pool.TokenUsageDelta{}, attemptStarted, 0, display)
		st.status = res.Status
		st.outcome = reqlog.OutcomeHTTPError
		// 错误原文透传（与主链路 error-passthrough 同哲学：客户端必须看到
		// 上游真实错误才能排查——额度耗尽/模型不存在/参数问题的处理方式不同）。
		writeUpstreamError(w, res.Status, res.Body, credSecret(cred))
		return
	}

	// 成功开流/拿到响应。流式：**逐字节透传**（挂 chatStatsReader 只做观测，
	// 不重建帧）——对 OpenAI 协议是上游原帧；对 anthropic/gemini 是 Do 已折回
	// 的 OpenAI chunk 帧，两者语义一致。
	if peek.Stream {
		st.status = http.StatusOK
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		stats := newChatStatsReaderSince(res.Reader, st.start)
		buf := make([]byte, 32*1024)
		var clientGone bool
		for {
			n, rerr := stats.Read(buf)
			if n > 0 {
				if _, werr := w.Write(buf[:n]); werr != nil {
					clientGone = true // 写失败 = 客户端断连；上游帧无恙
					break
				}
				if flusher != nil {
					flusher.Flush()
				}
			}
			if rerr != nil {
				break // EOF=正常结束；读错误=上游断（两种都停读）
			}
		}
		delta := stats.Usage()
		if clientGone {
			st.outcome = reqlog.OutcomeInterrupted
		} else if st.outcome == "" {
			st.outcome = reqlog.OutcomeSuccess
		}
		recordExtAttempt(h, st, apiKeyName, ch2, cred, upstream, delta, attemptStarted, stats.TTFB(), display)
		st.ttfb = stats.TTFB()
		if toks, hasUsage := stats.Tokens(); hasUsage {
			st.toks = toks
		}
		res.Reader.Close()
		return
	}

	// 非流式：body 已由 Do 折回 OpenAI 形态，原样回写。
	st.status = http.StatusOK
	st.outcome = reqlog.OutcomeSuccess
	var respObj map[string]any
	if json.Unmarshal(res.Body, &respObj) == nil {
		// model 字段回写客户端请求的名字（别名形态下客户端看到的永远是它请求的
		// 别名，而不是上游真实模型名）。流式帧不做改写（逐字节透传是硬约束）。
		if _, ok := respObj["model"]; ok || display != "" {
			respObj["model"] = display
		}
		delta := usageDeltaFromResponse(respObj)
		recordExtAttempt(h, st, apiKeyName, ch2, cred, upstream, delta, attemptStarted, 0, display)
		if u, ok := respObj["usage"].(map[string]any); ok {
			if ct, ok := u["completion_tokens"].(float64); ok {
				st.toks = int(ct)
			}
		}
		writeJSON(w, http.StatusOK, respObj)
	} else {
		// 上游回了 200 但 body 不是 JSON：按坏网关处理（不罚渠道——偶发形态）。
		recordExtAttempt(h, st, apiKeyName, ch2, cred, upstream, pool.TokenUsageDelta{}, attemptStarted, 0, display)
		st.status = http.StatusBadGateway
		st.outcome = reqlog.OutcomeHTTPError
		writeOpenAIError(w, http.StatusBadGateway, "upstream_parse", "ext channel returned non-JSON body")
	}
}

// extErrorFields 把出站错误翻译成 (HTTP 状态, error.code, message)。
// *extchan.Error 携带完整语义；其它错误按 502 传输失败处理。
func extErrorFields(err error) (int, string, string) {
	var he *extchan.Error
	if errors.As(err, &he) {
		return he.Status, he.Code, he.Message
	}
	return http.StatusBadGateway, "upstream_error", "ext channel error: " + logfmt.Truncate(err.Error(), 300)
}

// recordExtAttempt 外部渠道的用量记账（复用 usage.Recorder；realm 固定 "ext"，
// uid 用 "ext:<channel>"——用量视图天然按渠道分组），同时累计凭证级进程内计数。
//
// cred 为 nil 时（未能拿到凭证，如无可用凭证）只记渠道维度。
func recordExtAttempt(h *Handler, st *chatStat, apiKeyName string, ch2 *extchan.Channel,
	cred *extchan.Credential, model string, delta pool.TokenUsageDelta,
	started time.Time, ttfb time.Duration, displayModel string) {
	st.attempts++
	if delta.HasPromptTokens {
		st.promptTokens = delta.PromptTokens
	}
	if delta.HasCompletionTokens {
		st.completionTokens = delta.CompletionTokens
	}
	if delta.HasTotalTokens {
		st.totalTokens = delta.TotalTokens
	} else if delta.HasPromptTokens || delta.HasCompletionTokens {
		st.totalTokens = st.promptTokens + st.completionTokens
	}
	if displayModel == "" {
		displayModel = "ext:" + ch2.Name + "/" + model
	}
	delta.Model = displayModel
	latencyMs := time.Since(started).Milliseconds()
	if latencyMs < 1 {
		latencyMs = 1
	}
	delta.HasLatencyMs = true
	delta.LatencyMs = latencyMs
	if delta.HasCompletionTokens {
		if tps, ok := tokensPerSecond(delta.CompletionTokens,
			time.Duration(latencyMs)*time.Millisecond, ttfb); ok {
			delta.HasTokensPerSecond = true
			delta.TokensPerSecond = tps
		}
	}
	// 凭证级进程内计数（P0-9 降级方案 B：不碰 usage 桶格式/口径）。
	if cred != nil {
		cred.NoteUsage(delta.PromptTokens, delta.CompletionTokens)
	}
	if h.cfg.Usage != nil {
		h.cfg.Usage.Add(time.Now(), "ext", "ext:"+ch2.Name, delta.Model, usage.Delta{
			KeyName:          apiKeyName,
			PromptTokens:     delta.PromptTokens,
			HasPromptTokens:  delta.HasPromptTokens,
			CompletionTokens: delta.CompletionTokens,
			HasCompletion:    delta.HasCompletionTokens,
			TotalTokens:      delta.TotalTokens,
			HasTotal:         delta.HasTotalTokens,
			LatencyMs:        delta.LatencyMs,
			HasLatency:       delta.HasLatencyMs,
			TokensPerSecond:  delta.TokensPerSecond,
			HasTPS:           delta.HasTokensPerSecond,
			TTFBMs:           ttfb.Milliseconds(),
			HasTTFB:          ttfb > 0,
		}, delta.HasTotalTokens || delta.HasCompletionTokens || delta.HasPromptTokens)
	}
}

// credSecret 返回凭证秘密值（仅用于错误体脱敏；绝不输出/落盘）。
func credSecret(cred *extchan.Credential) string {
	if cred == nil {
		return ""
	}
	return cred.Secret
}

// writeUpstreamError 上游错误原文透传：body 是合法 OpenAI 错误 JSON 时原样转发
// （保留上游 error.code/type），否则包装成 OpenAI 形态（message 装原文）。
//
// secrets 非空时先做一层脱敏：上游偶尔会把请求 URL/鉴权头回显在 message 里
// （Gemini 的 ?key= 是典型），错误体是客户端可见面，不能泄露凭证。
func writeUpstreamError(w http.ResponseWriter, status int, body []byte, secrets ...string) {
	if len(secrets) > 0 {
		body = []byte(provider.ScrubSecrets(string(body), secrets...))
	}
	var probe struct {
		Error json.RawMessage `json:"error"`
	}
	if len(body) > 0 && json.Unmarshal(body, &probe) == nil && len(probe.Error) > 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(body)
		return
	}
	msg := string(body)
	if msg == "" {
		msg = fmt.Sprintf("ext channel returned %d", status)
	}
	// 上游原文可能是非 JSON 的 HTML/文本：包进 message 避免给客户端半截坏 JSON。
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": logfmt.Truncate(msg, 2000),
			"type":    "upstream_error",
			"code":    fmt.Sprintf("upstream_%d", status),
		},
	})
}
