// extrelay.go 外部渠道（ext:<channel>/<model>）的聊天转发：独立于 CodeBuddy
// 轮转主链路。设计要点见 internal/extchan 包注释与 handler.Config.Ext。
//
// 与 CodeBuddy 路径的关键差异：
//   - 不轮换（一个渠道一个 key，没有"换号"概念）——失败按渠道状态机治理；
//   - 不做提示词改写/指纹脱敏/DSML 修复（那是 CodeBuddy 上游的特有对抗，
//     标准 OpenAI 上游不需要，透传是唯一正确语义）；
//   - model 字段重写为渠道内裸名（前缀是网关侧路由协议）；
//   - usage 记账照常（realm 固定 "ext"，uid 用渠道名——用量视图按渠道分组）。
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/RisingInsight/startport/internal/extchan"
	"github.com/RisingInsight/startport/internal/logfmt"
	"github.com/RisingInsight/startport/internal/pool"
	"github.com/RisingInsight/startport/internal/reqlog"
	"github.com/RisingInsight/startport/internal/usage"
)

// extCooldownBase / extCooldownMax 429 软冷却参数（与 CodeBuddy soft_rate 独立：
// 渠道配额形态各异，短窗指数退避已足够；不做账号级长冷却）。
const (
	extCooldownBase = 60 * time.Second
	extCooldownMax  = 30 * time.Minute
	// extServerCooldown 5xx 轻冷却（固定短窗，不指数升级）。
	extServerCooldown = 30 * time.Second
	// extFailThreshold / extBreaker 传输失败熔断：连败 N 次熔 10 分钟。
	extFailThreshold = 5
	extBreaker       = 10 * time.Minute
)

// handleExtChat 外部渠道聊天入口（chatCompletions 前缀路由短路后进到这里）。
// body 为原始请求体（未做任何提示词改写）；model 为入站原始模型名。
func (h *Handler) handleExtChat(w http.ResponseWriter, r *http.Request,
	st *chatStat, body []byte, model string, apiKeyName string) {

	ch2, bare, rerr := h.cfg.Ext.RouteFor(model)
	if rerr != nil {
		writeOpenAIError(w, http.StatusBadRequest, "model_not_found", rerr.Error())
		st.status = http.StatusBadRequest
		st.outcome = reqlog.OutcomeHTTPError
		return
	}
	st.uid = "ext:" + ch2.Name
	st.nick = ""
	st.model = "ext:" + ch2.Name + "/" + bare

	// 冷却中的渠道直接拒绝（不排队——外部渠道没有粘性/保号诉求，快速失败
	// 让客户端切模型或切渠道）。
	if ch2.Cooldown() {
		st.status = http.StatusServiceUnavailable
		st.outcome = reqlog.OutcomeHTTPError
		writeOpenAIError(w, http.StatusServiceUnavailable, "channel_cooling",
			fmt.Sprintf("ext channel %q is cooling down (rate limited / upstream errors), retry later or use another channel", ch2.Name))
		return
	}

	// model 字段重写为渠道内裸名（前缀剥离）。body 不可解析时原样透传
	// （上游会给出更准确的错误）。
	outBody := rewriteModel(body, bare)

	// stream 判定：与主链路一致，按 body 的 stream 字段。
	var peek struct {
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(outBody, &peek)

	attemptStarted := time.Now()
	rc, status, respBody, err := h.cfg.Ext.Chat(r.Context(), ch2, outBody, peek.Stream)
	if err != nil {
		// 传输层失败：喂熔断计数（达阈熔断），快速失败。
		ch2.NoteFailure(extFailThreshold, extBreaker, "transport: "+logfmt.Truncate(err.Error(), 120))
		recordExtAttempt(h, st, apiKeyName, ch2.Name, bare, pool.TokenUsageDelta{}, attemptStarted, 0)
		st.status = http.StatusBadGateway
		st.outcome = reqlog.OutcomeHTTPError
		log.Printf("WARN: [server] ext=%s model=%s: transport error: %v", ch2.Name, bare, err)
		writeOpenAIError(w, http.StatusBadGateway, "upstream_error",
			"ext channel transport error: "+err.Error())
		return
	}
	if status >= 400 {
		recordExtAttempt(h, st, apiKeyName, ch2.Name, bare, pool.TokenUsageDelta{}, attemptStarted, 0)
		st.status = status
		st.outcome = reqlog.OutcomeHTTPError
		h.noteExtFailure(ch2, status, respBody)
		// 错误原文透传（与主链路 error-passthrough 同哲学：客户端必须看到
		// 上游真实错误才能排查——额度耗尽/模型不存在/参数问题的处理方式完全不同）。
		writeUpstreamError(w, status, respBody)
		return
	}

	// 成功开流/拿到响应。流式：**逐字节透传**（挂 chatStatsReader 只做观测，
	// 不重建帧）——StreamHint 的 SSE 帧白名单重建是 CodeBuddy 畸形上游的对抗
	// 语义，会吞掉第三方 provider 的私有 SSE 字段（OpenRouter 的 provider 归因、
	// 硅基流动的细节字段等），外部渠道透传是唯一正确语义。
	if peek.Stream {
		st.status = http.StatusOK
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		stats := newChatStatsReaderSince(rc, st.start)
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
		// 客户端断连：Interrupted（请求未完成）但渠道记成功（上游没有错）。
		// 正常读完：Success。上游断：Success + 渠道健康（单次读断不罚，偶发形态）。
		if clientGone {
			st.outcome = reqlog.OutcomeInterrupted
		} else if st.outcome == "" {
			st.outcome = reqlog.OutcomeSuccess
		}
		ch2.NoteSuccess()
		recordExtAttempt(h, st, apiKeyName, ch2.Name, bare, stats.Usage(), attemptStarted, stats.TTFB())
		st.ttfb = stats.TTFB()
		if toks, hasUsage := stats.Tokens(); hasUsage {
			st.toks = toks
		}
		rc.Close()
		return
	}

	// 非流式：body 原样回写（Content-Type 按 OpenAI 惯例 application/json）。
	st.status = http.StatusOK
	st.outcome = reqlog.OutcomeSuccess
	ch2.NoteSuccess()
	var respObj map[string]any
	if json.Unmarshal(respBody, &respObj) == nil {
		recordExtAttempt(h, st, apiKeyName, ch2.Name, bare, usageDeltaFromResponse(respObj), attemptStarted, 0)
		if u, ok := respObj["usage"].(map[string]any); ok {
			if ct, ok := u["completion_tokens"].(float64); ok {
				st.toks = int(ct)
			}
		}
		writeJSON(w, http.StatusOK, respObj)
	} else {
		// 上游回了 200 但 body 不是 JSON：按坏网关处理（不罚渠道——偶发形态）。
		recordExtAttempt(h, st, apiKeyName, ch2.Name, bare, pool.TokenUsageDelta{}, attemptStarted, 0)
		st.status = http.StatusBadGateway
		st.outcome = reqlog.OutcomeHTTPError
		writeOpenAIError(w, http.StatusBadGateway, "upstream_parse", "ext channel returned non-JSON body")
	}
}

// noteExtFailure 按上游状态码给渠道记账（429 指数退避 / 5xx 短冷却 / 其他只记日志）。
func (h *Handler) noteExtFailure(ch2 *extchan.Channel, status int, body []byte) {
	switch {
	case status == http.StatusTooManyRequests:
		ch2.NoteRateLimited(extCooldownBase, extCooldownMax, "429 rate limited")
		log.Printf("WARN: [server] ext=%s: 429 rate limited (cooldown base %s)", ch2.Name, extCooldownBase)
	case status >= 500:
		ch2.NoteServerUpstream(extServerCooldown, fmt.Sprintf("%d upstream error", status))
	default:
		// 4xx（400/401/403/404）：请求或配置问题，换渠道也解决不了，不冷却渠道；
		// 错误原文已透传给客户端。仅记录观测。
		ch2.NoteFailure(extFailThreshold, extBreaker, fmt.Sprintf("%d client error", status))
	}
}

// recordExtAttempt 外部渠道的用量记账（复用 usage.Recorder；realm 固定 "ext"，
// uid 用 "ext:<channel>"——用量视图天然按渠道分组）。
func recordExtAttempt(h *Handler, st *chatStat, apiKeyName, channel, model string,
	delta pool.TokenUsageDelta, started time.Time, ttfb time.Duration) {
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
	delta.Model = "ext:" + channel + "/" + model
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
	if h.cfg.Usage != nil {
		h.cfg.Usage.Add(time.Now(), "ext", "ext:"+channel, delta.Model, usage.Delta{
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

// writeUpstreamError 上游错误原文透传：body 是合法 OpenAI 错误 JSON 时原样转发
// （保留上游 error.code/type），否则包装成 OpenAI 形态（message 装原文）。
func writeUpstreamError(w http.ResponseWriter, status int, body []byte) {
	var probe struct {
		Error json.RawMessage `json:"error"`
	}
	if len(body) > 0 && json.Unmarshal(body, &probe) == nil && len(probe.Error) > 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(body)
		return
	}
	msg := strings.TrimSpace(string(body))
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

// extModelEntries 外部渠道的 /v1/models 条目（modelList 合并用；Ext 为 nil 时空）。
func (h *Handler) extModelEntries(ctx context.Context) []map[string]any {
	if h.cfg.Ext == nil {
		return nil
	}
	return h.cfg.Ext.ModelList(ctx)
}
