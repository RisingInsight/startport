// exthropic.go 外部渠道的 Anthropic Messages 出口（POST /v1/messages）。
//
// 目的：Claude Code / Anthropic SDK 客户端直连本网关。转换是**协议级**的：
// 请求 Anthropic Messages → OpenAI Chat Completions，响应按原方向折回。
// 路由判定（v1.7.6 主池回退）：ext 渠道可服务的模型走 extchan.Do（由协议适配
// 器决定最终上游形态）；非 ext 模型（extchan.ErrModelNotExt）回退 CodeBuddy
// 主池——复用 chatCompletions 全套治理（选号/粘性/轮转/记账），响应经
// poolAnthropicBridge 实时折回 Anthropic 形态。
// 只覆盖 /v1/messages 主干（system / messages / tools / max_tokens /
// temperature / stream）；thinking 块、cache_control、citations 等 Anthropic
// 专有特性不透传（ext 渠道侧无对应语义）；image 块两条路径都 400。
//
// 阶段三变化（语义未变）：
//   - 转发改走 extchan.Do（凭证池选证/换证 + 协议适配）；
//   - 记账带凭证 label，并累计凭证级进程内计数；
//   - 转换函数 anthropicToOpenAI / extMessagesSync / extMessagesStream 的
//     转换语义一字未改（只换了调用形态与参数字段）。
package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/RisingInsight/startport/internal/extchan"
	"github.com/RisingInsight/startport/internal/provider"
	"github.com/RisingInsight/startport/internal/reqlog"
	"github.com/RisingInsight/startport/internal/upstream"
	"github.com/RisingInsight/startport/internal/usage"
)

// anthropicReq Anthropic Messages 请求主干。
type anthropicReq struct {
	Model         string          `json:"model"`
	Messages      []anthropicMsg  `json:"messages"`
	System        json.RawMessage `json:"system"` // string 或 []content block
	MaxTokens     int             `json:"max_tokens"`
	Temperature   *float64        `json:"temperature"`
	TopP          *float64        `json:"top_p"`
	Stream        bool            `json:"stream"`
	Tools         []anthropicTool `json:"tools"`
	StopSequences []string        `json:"stop_sequences"`
}

type anthropicMsg struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"` // string 或 []block
}

type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// anthropicErrShape Anthropic 错误信封（上游错误折回时保持 Anthropic 形态，
// Claude Code 才能按其错误处理逻辑走）。
type anthropicErrShape struct {
	Type  string `json:"type"` // 恒 "error"
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

func writeAnthropicError(w http.ResponseWriter, status int, errType, msg string) {
	var e anthropicErrShape
	e.Type = "error"
	e.Error.Type = errType
	e.Error.Message = msg
	writeJSON(w, status, e)
}

// handleExtMessages /v1/messages 入口（外部渠道优先，CodeBuddy 池回退）。
//
// 路由判定（v1.7.6 主池回退）：
//   - ext 渠道可服务（"ext:<渠道>/<模型>" 前缀，或裸名在渠道间唯一命中）→ ext 链路；
//   - 裸名不属于任何渠道（extchan.ErrModelNotExt）→ 回退 CodeBuddy 池——
//     Claude Code 用裸名模型即可直连主池额度，无需任何 ext 渠道配置；
//   - ext: 前缀指向不存在/停用渠道、裸名多渠道同名 → 400（路由协议错误，
//     回主池会掩盖配置问题，不回退）；
//   - 未配置任何 ext 渠道 → 全部模型走池回退（Ext==nil 不再是 501）。
func (h *Handler) handleExtMessages(w http.ResponseWriter, r *http.Request) {
	// 鉴权与 /v1/chat/completions 同通道（chatAuth 已在外层完成）。
	apiKeyName, _ := r.Context().Value(apiKeyNameKey{}).(string)
	body, err := io.ReadAll(io.LimitReader(r.Body, h.maxBodyOr(64<<20)))
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "read body: "+err.Error())
		return
	}
	var areq anthropicReq
	if err := json.Unmarshal(body, &areq); err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "parse body: "+err.Error())
		return
	}
	if h.cfg.Ext != nil {
		ch2, upstream, display, rerr := h.cfg.Ext.RouteModel(areq.Model)
		switch {
		case rerr == nil:
			h.extMessagesRoute(w, r, apiKeyName, &areq, ch2, upstream, display)
			return
		case errors.Is(rerr, extchan.ErrModelNotExt):
			// 裸名不属于任何外部渠道 → 落主池（下方）。
		default:
			writeAnthropicError(w, http.StatusBadRequest, "not_found_error", rerr.Error())
			return
		}
	}
	h.handleMessagesPoolFallback(w, r, &areq)
}

// extMessagesRoute 外部渠道分支（原 handleExtMessages 主体，语义未改）：
// Anthropic → OpenAI 转换后交 extchan.Do，响应按原方向折回。
func (h *Handler) extMessagesRoute(w http.ResponseWriter, r *http.Request, apiKeyName string,
	areq *anthropicReq, ch2 *extchan.Channel, upstream, display string) {

	// Anthropic → OpenAI 请求转换（上游模型名：别名已解析）。
	oaiBody, convErr := anthropicToOpenAI(areq, upstream)
	if convErr != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", convErr.Error())
		return
	}

	attemptStarted := time.Now()
	res, derr := h.cfg.Ext.Do(r.Context(), ch2, &extchan.OutRequest{
		Body:         oaiBody,
		Stream:       areq.Stream,
		DisplayModel: display,
	})
	if derr != nil {
		status, code, msg := extErrorFields(derr)
		writeAnthropicError(w, status, anthropicErrTypeForExt(code, status), msg)
		return
	}
	cred := ch2.CredentialByLabel(res.CredentialLabel)
	if res.Status >= 400 {
		// 上游错误已由 Do 折回 OpenAI 信封（跨协议时），这里转 Anthropic 形态。
		// 上游错误原文可能回显请求 URL/鉴权头：错误体是客户端可见面，先脱敏。
		writeAnthropicError(w, res.Status, anthropicErrTypeFor(res.Status),
			provider.ScrubSecrets(upstreamErrorMsg(res.Body), credSecret(cred)))
		return
	}

	if areq.Stream {
		h.extMessagesStream(w, ch2, cred, display, res.Reader, attemptStarted, apiKeyName)
		return
	}
	h.extMessagesSync(w, ch2, cred, display, res.Body, attemptStarted, apiKeyName)
}

func (h *Handler) maxBodyOr(d int64) int64 {
	if h.cfg.MaxBodyBytes > 0 {
		return h.cfg.MaxBodyBytes
	}
	return d
}

func logfmtTrunc(s string) string {
	if len(s) > 120 {
		return s[:120]
	}
	return s
}

// anthropicErrTypeFor HTTP 状态 → Anthropic 错误类型（Claude Code 按 type 分支处理）。
func anthropicErrTypeFor(status int) string {
	switch {
	case status == http.StatusTooManyRequests:
		return "rate_limit_error"
	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		return "authentication_error"
	case status == http.StatusNotFound:
		return "not_found_error"
	case status == http.StatusServiceUnavailable:
		return "overloaded_error"
	case status >= 500:
		return "api_error"
	default:
		return "invalid_request_error"
	}
}

// anthropicErrTypeForExt 先按错误码判定（凭证池/流式能力），再按状态码回落。
func anthropicErrTypeForExt(code string, status int) string {
	switch code {
	case "all_credentials_unavailable", "channel_cooling":
		return "overloaded_error" // 与 Anthropic 的 429/过载语义对齐
	case "gemini_stream_unsupported":
		return "api_error"
	default:
		return anthropicErrTypeFor(status)
	}
}

// upstreamErrorMsg 从上游 OpenAI 错误 body 提取 message（保持原文）。
func upstreamErrorMsg(body []byte) string {
	var probe struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &probe) == nil && probe.Error.Message != "" {
		return probe.Error.Message
	}
	s := strings.TrimSpace(string(body))
	if s == "" {
		return "ext channel returned an error"
	}
	if len(s) > 500 {
		return s[:500]
	}
	return s
}

// anthropicToOpenAI 请求转换。bare 为渠道内上游模型名（别名已解析）。
func anthropicToOpenAI(a *anthropicReq, bare string) ([]byte, error) {
	oai := map[string]any{
		"model":    bare,
		"messages": []map[string]any{},
	}
	if a.MaxTokens > 0 {
		oai["max_tokens"] = a.MaxTokens
	}
	if a.Temperature != nil {
		oai["temperature"] = *a.Temperature
	}
	if a.TopP != nil {
		oai["top_p"] = *a.TopP
	}
	if a.Stream {
		oai["stream"] = true
		// OpenAI 侧要求流式也回 usage（本网关用量记账依赖它）。
		oai["stream_options"] = map[string]any{"include_usage": true}
	}
	if len(a.StopSequences) > 0 {
		oai["stop"] = a.StopSequences
	}

	msgs := []map[string]any{}
	// system：string 或 block 数组 → 首条 system 消息。
	if len(a.System) > 0 {
		sysText, sysBlocks := decodeAnthropicContent(a.System)
		if s := strings.TrimSpace(sysText); s != "" {
			msgs = append(msgs, map[string]any{"role": "system", "content": s})
		} else if len(sysBlocks) > 0 {
			// block 数组形态：拼接 text 块（tool_result 之类不合法于 system，忽略）。
			var parts []string
			for _, b := range sysBlocks {
				if b["type"] == "text" {
					if t, ok := b["text"].(string); ok {
						parts = append(parts, t)
					}
				}
			}
			if len(parts) > 0 {
				msgs = append(msgs, map[string]any{"role": "system", "content": strings.Join(parts, "\n")})
			}
		}
	}
	// messages：role + content（string 或 block 数组）。
	for _, m := range a.Messages {
		text, blocks := decodeAnthropicContent(m.Content)
		switch m.Role {
		case "user", "assistant":
			role := m.Role
			if text != "" && (len(blocks) == 0 || onlyTextBlocks(blocks)) {
				msgs = append(msgs, map[string]any{"role": role, "content": text})
				continue
			}
			// block 数组：text → content 文本串（多块合并）；tool_use / tool_result
			// → OpenAI tool_calls / tool 消息；image → 暂不支持（报 400，见下）。
			var textParts []string
			var toolCalls []map[string]any
			for _, b := range blocks {
				switch b["type"] {
				case "text":
					if t, ok := b["text"].(string); ok {
						textParts = append(textParts, t)
					}
				case "tool_use":
					toolCalls = append(toolCalls, map[string]any{
						"id":   b["id"],
						"type": "function",
						"function": map[string]any{
							"name":      b["name"],
							"arguments": marshalCompact(b["input"]),
						},
					})
				case "tool_result":
					// Anthropic 的 tool_result 在 user 轮 → OpenAI 的 role=tool。
					content := ""
					if c, ok := b["content"].(string); ok {
						content = c
					}
					msgs = append(msgs, map[string]any{
						"role":         "tool",
						"tool_call_id": b["tool_use_id"],
						"content":      content,
					})
				case "image":
					return nil, fmt.Errorf("image blocks are not supported on ext channels via /v1/messages yet")
				}
			}
			// 先落 assistant 文本 + tool_calls，再落 tool 消息？OpenAI 顺序约束：
			// tool 消息必须紧跟带对应 tool_call 的 assistant 消息。Anthropic 的顺序
			// 是 assistant(tool_use) → user(tool_result)，转换天然满足。这里把
			// assistant 轮整体（文本 + tool_calls）先 append，tool_result 随后。
			if len(textParts) > 0 || len(toolCalls) > 0 {
				entry := map[string]any{"role": role}
				if len(textParts) > 0 {
					entry["content"] = strings.Join(textParts, "\n")
				} else {
					entry["content"] = nil
				}
				if len(toolCalls) > 0 {
					entry["tool_calls"] = toolCalls
				}
				msgs = append(msgs, entry)
			}
		default:
			return nil, fmt.Errorf("unsupported message role %q", m.Role)
		}
	}
	oai["messages"] = msgs
	// tools：Anthropic input_schema → OpenAI parameters。
	if len(a.Tools) > 0 {
		oaiTools := make([]map[string]any, 0, len(a.Tools))
		for _, t := range a.Tools {
			oaiTools = append(oaiTools, map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        t.Name,
					"description": t.Description,
					"parameters":  t.InputSchema,
				},
			})
		}
		oai["tools"] = oaiTools
	}
	out, err := json.Marshal(oai)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// anthropicBlock content block 的宽松形态。
type anthropicBlock = map[string]any

// decodeAnthropicContent 解析 content（string 或 block 数组），返回合并文本与块列表。
func decodeAnthropicContent(raw json.RawMessage) (string, []anthropicBlock) {
	if len(raw) == 0 {
		return "", nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, nil
	}
	var blocks []anthropicBlock
	if json.Unmarshal(raw, &blocks) != nil {
		return "", nil
	}
	var parts []string
	for _, b := range blocks {
		if b["type"] == "text" {
			if t, ok := b["text"].(string); ok {
				parts = append(parts, t)
			}
		}
	}
	return strings.Join(parts, "\n"), blocks
}

func onlyTextBlocks(blocks []anthropicBlock) bool {
	for _, b := range blocks {
		if b["type"] != "text" {
			return false
		}
	}
	return len(blocks) > 0
}

func marshalCompact(v any) string {
	if v == nil {
		return "{}"
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// oa2aUsage OpenAI usage 中转形态（非流式折回与流式累计共用）。
type oa2aUsage struct {
	prompt     int64
	completion int64
	total      int64
}

// openAIRespToAnthropic 非流式 OpenAI Chat Completions 响应 → Anthropic Messages
// 形态（纯转换，不含记账——ext 与主池回退两条路径的记账口径不同）。model 为
// 客户端可见模型名。ok=false 表示 body 不可解析（调用方回 502）。
func openAIRespToAnthropic(respBody []byte, model string) (map[string]any, *oa2aUsage, bool) {
	var oaiResp struct {
		Choices []struct {
			Message struct {
				Role      string           `json:"role"`
				Content   string           `json:"content"`
				ToolCalls []openAIToolCall `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage *struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
			TotalTokens      int64 `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(respBody, &oaiResp); err != nil || len(oaiResp.Choices) == 0 {
		return nil, nil, false
	}
	choice := oaiResp.Choices[0]
	// content blocks：text 块 + tool_use 块。
	blocks := []map[string]any{}
	if choice.Message.Content != "" {
		blocks = append(blocks, map[string]any{"type": "text", "text": choice.Message.Content})
	}
	for _, tc := range choice.Message.ToolCalls {
		var input json.RawMessage = json.RawMessage(tc.Function.Arguments)
		if len(input) == 0 {
			input = json.RawMessage("{}")
		}
		blocks = append(blocks, map[string]any{
			"type":  "tool_use",
			"id":    tc.ID,
			"name":  tc.Function.Name,
			"input": input,
		})
	}
	resp := map[string]any{
		"id":            "msg_" + fmt.Sprintf("%d", time.Now().UnixNano()),
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       blocks,
		"stop_reason":   mapAnthropicStop(choice.FinishReason),
		"stop_sequence": nil,
	}
	var u *oa2aUsage
	if oaiResp.Usage != nil {
		u = &oa2aUsage{prompt: oaiResp.Usage.PromptTokens, completion: oaiResp.Usage.CompletionTokens, total: oaiResp.Usage.TotalTokens}
		resp["usage"] = map[string]any{
			"input_tokens":  u.prompt,
			"output_tokens": u.completion,
		}
	}
	return resp, u, true
}

// extMessagesSync 非流式：OpenAI 响应 → Anthropic Messages 形态。
// display 为客户端可见模型名（"ext:<渠道>/<模型或别名>"）。
func (h *Handler) extMessagesSync(w http.ResponseWriter, ch2 *extchan.Channel,
	cred *extchan.Credential, display string, respBody []byte,
	attemptStarted time.Time, apiKeyName string) {
	resp, u, ok := openAIRespToAnthropic(respBody, display)
	if !ok {
		writeAnthropicError(w, http.StatusBadGateway, "api_error", "ext channel returned unparseable response")
		return
	}
	if u != nil {
		// 用量记账（realm=ext，与 chat 路径同口径）+ 凭证级进程内计数。
		h.recordExtMessagesUsage(apiKeyName, ch2, cred, display, u.prompt, u.completion, u.total, attemptStarted)
	}
	writeJSON(w, http.StatusOK, resp)
}

// openAIToolCall OpenAI 响应侧的工具调用（与请求侧字段同名复用解析）。
type openAIToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

func mapAnthropicStop(openaiFinish string) string {
	switch openaiFinish {
	case "tool_calls":
		return "tool_use"
	case "length":
		return "max_tokens"
	case "stop_sequence":
		return "stop_sequence"
	default:
		return "end_turn"
	}
}

// anthropicEventWriter 构造 SSE 事件写出闭包（ext 流式与主池回退桥共用）。
// w 取 io.Writer（而非 http.ResponseWriter）：ext 流式路径要传入 upstream.SSEWriter
// 以获得空闲心跳与写串行化，flush 职责也由它承担（此时 fl 传 nil）。
func anthropicEventWriter(w io.Writer, fl http.Flusher) func(string, any) {
	return func(event string, payload any) {
		b, err := json.Marshal(payload)
		if err != nil {
			return
		}
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
		if fl != nil {
			fl.Flush()
		}
	}
}

// anthropicStreamXlator OpenAI chunk 流 → Anthropic 事件流的共用翻译状态机。
// 事件骨架：message_start → (content_block_start + content_block_delta* +
// content_block_stop)* → message_delta(stop_reason, usage) → message_stop。
// 工具调用流式拼装不做（OpenAI 侧 arguments 分片在 finish 阶段聚合成完整
// tool_use 块后一次性下发 content_block_start+input_delta+stop）。
type anthropicStreamXlator struct {
	sendEvent    func(event string, payload any)
	model        string
	started      bool
	textOpen     bool
	textIdx      int
	outTokens    int64
	inTokens     int64
	hasUsage     bool
	finishReason string
	// 工具调用聚合：OpenAI 流式 tool_calls 按 index 分片给 arguments。
	toolAgg map[int]*openAIToolCall
	toolSeq []int
}

func newAnthropicStreamXlator(model string, sendEvent func(string, any)) *anthropicStreamXlator {
	return &anthropicStreamXlator{sendEvent: sendEvent, model: model, toolAgg: map[int]*openAIToolCall{}}
}

// start 发出 message_start + ping（幂等）。
func (x *anthropicStreamXlator) start() {
	if x.started {
		return
	}
	x.started = true
	x.sendEvent("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": "msg_" + fmt.Sprintf("%d", time.Now().UnixNano()), "type": "message",
			"role": "assistant", "model": x.model, "content": []any{},
			"stop_reason": nil, "stop_sequence": nil,
			"usage": map[string]any{"input_tokens": 0, "output_tokens": 0},
		},
	})
	x.sendEvent("ping", map[string]any{"type": "ping"})
}

// ingest 处理一个 OpenAI chunk payload（"data: " 后、"[DONE]" 前的 JSON）。
// 返回 false = 流中错误帧已按 Anthropic error 事件透出，调用方立即终止
// （不再 finish——error 之后补收尾骨架是畸形流）。
func (x *anthropicStreamXlator) ingest(payload string) bool {
	x.start()
	var chunk struct {
		Choices []struct {
			Delta struct {
				Content   string `json:"content"`
				ToolCalls []struct {
					Index    int    `json:"index"`
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"delta"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage *struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
			TotalTokens      int64 `json:"total_tokens"`
		} `json:"usage"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(payload), &chunk) != nil {
		return true // 非法帧跳过（与既有扫描器语义一致）
	}
	if chunk.Error != nil {
		// 流中错误帧：按 Anthropic 形态补发 error 事件并终止。
		x.sendEvent("error", map[string]any{
			"type":  "error",
			"error": map[string]any{"type": "api_error", "message": chunk.Error.Message},
		})
		return false
	}
	for _, c := range chunk.Choices {
		if c.Delta.Content != "" {
			x.emitTextDelta(c.Delta.Content)
		}
		for _, tc := range c.Delta.ToolCalls {
			agg := x.toolAgg[tc.Index]
			if agg == nil {
				agg = &openAIToolCall{ID: tc.ID}
				agg.Function.Name = tc.Function.Name
				x.toolAgg[tc.Index] = agg
				x.toolSeq = append(x.toolSeq, tc.Index)
			}
			if tc.Function.Name != "" {
				agg.Function.Name = tc.Function.Name
			}
			if tc.ID != "" {
				agg.ID = tc.ID
			}
			agg.Function.Arguments += tc.Function.Arguments
		}
		if c.FinishReason != "" {
			x.finishReason = c.FinishReason
		}
	}
	if chunk.Usage != nil {
		x.hasUsage = true
		x.inTokens = chunk.Usage.PromptTokens
		x.outTokens = chunk.Usage.CompletionTokens
	}
	return true
}

// emitTextDelta 文本块懒开 + delta 下发。
func (x *anthropicStreamXlator) emitTextDelta(s string) {
	if !x.textOpen {
		x.sendEvent("content_block_start", map[string]any{
			"type": "content_block_start", "index": x.textIdx,
			"content_block": map[string]any{"type": "text", "text": ""},
		})
		x.textOpen = true
	}
	x.sendEvent("content_block_delta", map[string]any{
		"type": "content_block_delta", "index": x.textIdx,
		"delta": map[string]any{"type": "text_delta", "text": s},
	})
}

// closeText 关闭打开中的文本块（index 前进）。
func (x *anthropicStreamXlator) closeText() {
	if x.textOpen {
		x.sendEvent("content_block_stop", map[string]any{"type": "content_block_stop", "index": x.textIdx})
		x.textOpen = false
		x.textIdx++
	}
}

// finish 收尾：关文本块 + 聚合下发工具块 + message_delta + message_stop。
// 空流（一帧未到）也会补齐骨架，客户端拿到空 message 而非悬挂连接。
func (x *anthropicStreamXlator) finish() {
	x.start()
	x.closeText()
	// 工具块：整体下发（聚合一刀）。
	for _, idx := range x.toolSeq {
		tc := x.toolAgg[idx]
		var input json.RawMessage = json.RawMessage(tc.Function.Arguments)
		if len(input) == 0 {
			input = json.RawMessage("{}")
		}
		x.sendEvent("content_block_start", map[string]any{
			"type": "content_block_start", "index": x.textIdx,
			"content_block": map[string]any{"type": "tool_use", "id": tc.ID, "name": tc.Function.Name, "input": map[string]any{}},
		})
		x.sendEvent("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": x.textIdx,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": string(input)},
		})
		x.sendEvent("content_block_stop", map[string]any{"type": "content_block_stop", "index": x.textIdx})
		x.textIdx++
	}
	x.sendEvent("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": mapAnthropicStop(x.finishReason), "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": x.outTokens},
	})
	x.sendEvent("message_stop", map[string]any{"type": "message_stop"})
}

// extMessagesStream 流式：OpenAI SSE → Anthropic SSE 事件序列（翻译状态机见上）。
func (h *Handler) extMessagesStream(w http.ResponseWriter, ch2 *extchan.Channel,
	cred *extchan.Credential, display string, rc io.ReadCloser,
	attemptStarted time.Time, apiKeyName string) {
	defer rc.Close()
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	// 客户端保活心跳（见 upstream/sse_heartbeat.go）：本出口是**翻译型**流——只有
	// 上游帧到达、翻译状态机才会写出 Anthropic 事件，上游静默期零字节。这与
	// /v1/chat/completions 是同一个故障面（Claude Code 走 /v1/messages，首帧前
	// prefill 静默同样会被判 idle timeout），此前只有 openai 入口有 SSEWriter。
	// 事件与心跳都经 SSEWriter 单锁串行化，心跳只在行边界补 `: keep-alive`，
	// 不打断任何 event:/data: 帧；defer Close 覆盖三条退出路径。
	out := upstream.NewSSEWriterInterval(w, flusher, sseHeartbeatInterval)
	defer out.Close()
	x := newAnthropicStreamXlator(display, anthropicEventWriter(out, nil))
	x.start()

	sc := bufio.NewScanner(rc)
	sc.Buffer(make([]byte, 0, 64*1024), 8<<20)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			break
		}
		if !x.ingest(payload) {
			return // 流中错误帧：error 事件已透出，不补收尾骨架
		}
	}
	x.finish()

	if x.hasUsage {
		h.recordExtMessagesUsage(apiKeyName, ch2, cred, display, x.inTokens, x.outTokens, x.inTokens+x.outTokens, attemptStarted)
	}
}

// recordExtMessagesUsage /v1/messages 的用量记账（与 chat 路径同桶：realm=ext）。
// 同时累计凭证级进程内计数（P0-9 降级方案 B）。
func (h *Handler) recordExtMessagesUsage(apiKeyName string, ch2 *extchan.Channel,
	cred *extchan.Credential, displayModel string, prompt, completion, total int64, started time.Time) {
	if cred != nil {
		cred.NoteUsage(prompt, completion)
	}
	if h.cfg.Usage == nil {
		return
	}
	latencyMs := time.Since(started).Milliseconds()
	if latencyMs < 1 {
		latencyMs = 1
	}
	uid := "ext:" + ch2.Name
	if cred != nil && cred.Label != "" {
		uid = cred.Label
	}
	h.cfg.Usage.Add(time.Now(), "ext", uid, displayModel, usage.Delta{
		KeyName:          apiKeyName,
		PromptTokens:     prompt,
		HasPromptTokens:  prompt > 0,
		CompletionTokens: completion,
		HasCompletion:    completion > 0,
		TotalTokens:      total,
		HasTotal:         total > 0,
		LatencyMs:        latencyMs,
		HasLatency:       true,
	}, true)
}

// ---------------------------------------------------------------------------
// 主池回退（v1.7.6）：/v1/messages 的非 ext 模型 → CodeBuddy 账号池
// ---------------------------------------------------------------------------

// handleMessagesPoolFallback 主池回退：Anthropic 请求先折成 OpenAI wire，交给
// 既有 chatCompletions 主链路——选号/粘性/轮转/冷却/错误处置/提示词模式/记账
// 全部复用，不新增第二套池语义。响应经 poolAnthropicBridge 实时折回 Anthropic
// 形态（流式逐帧翻译，不牺牲首字延迟）；JSONL 请求归档与 chat 路径同一套包装
// （Path 记 /v1/messages）。主池路径自带的用量记账已生效，本函数不重复记账。
func (h *Handler) handleMessagesPoolFallback(w http.ResponseWriter, r *http.Request, areq *anthropicReq) {
	if h.cfg.Pool == nil {
		writeAnthropicError(w, http.StatusNotImplemented, "api_error",
			"no account pool configured; /v1/messages serves ext channels or a configured pool")
		return
	}
	// 模型名保留原样（裸名 / cn: / global: 前缀）——chatCompletions 自己解析
	// realm 前缀；image 块沿用 anthropicToOpenAI 的 400（与 ext 路径同一边界）。
	oaiBody, convErr := anthropicToOpenAI(areq, areq.Model)
	if convErr != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", convErr.Error())
		return
	}
	r2 := r.Clone(r.Context())
	r2.Body = io.NopCloser(bytes.NewReader(oaiBody))
	r2.ContentLength = int64(len(oaiBody))
	r2.Header.Set("Content-Type", "application/json")

	bridge := &poolAnthropicBridge{ResponseWriter: w, display: areq.Model}
	if h.cfg.RequestLog != nil {
		// 与 ServeHTTP 的 chat 路径同一套归档包装（requestTrace + responseObserver），
		// 面板请求流水不缺 /v1/messages 的主池流量。
		trace := &requestTrace{id: reqlog.NewRequestID(), start: time.Now(), path: "/v1/messages"}
		if h.loadLive().RecordClientInfo {
			trace.captureClientInfo(r)
		}
		r2 = r2.WithContext(context.WithValue(r2.Context(), requestTraceKey{}, trace))
		w.Header().Set("X-Request-Id", trace.id)
		h.cfg.RequestLog.Begin()
		obs := &responseObserver{ResponseWriter: bridge}
		defer func() {
			status := obs.status
			if status == 0 {
				status = http.StatusOK
			}
			h.cfg.RequestLog.Record(trace.event(status))
		}()
		h.chatCompletions(obs, r2)
	} else {
		h.chatCompletions(bridge, r2)
	}
	bridge.finish()
}

// poolAnthropicBridge /v1/messages 主池回退的响应桥：chatCompletions 以 OpenAI
// 形态写响应（流式逐帧、非流式整包、错误信封三种形态都可能），本桥把它翻译成
// Anthropic 形态写给真实客户端：
//   - 流式：首写时按 Content-Type 探测 SSE，逐帧翻译（message_start 起手，
//     [DONE] 后停止；OpenAI 原始帧绝不透传）；
//   - 非流式 200：整包缓冲，finish 时折回 Anthropic message；
//   - 非 200：整包缓冲，finish 时折回 Anthropic 错误信封（type 按状态映射）。
type poolAnthropicBridge struct {
	http.ResponseWriter // Header() 直通真实 w：流式头（event-stream 等）由 sse.go 正常落位
	display             string
	x                   *anthropicStreamXlator
	mode                int    // 0=未定 1=SSE 逐帧翻译 2=整包缓冲
	status              int    // WriteHeader 记录（非 200 时 finish 折回错误形态）
	sseBuf              []byte // SSE 帧缓冲（每帧以 \\n\\n 结束后复用）
	bodyBuf             []byte // 非流式/错误体缓冲
	sseDone             bool   // [DONE] 或流中错误帧之后不再翻译
	errored             bool   // 流中错误帧已透出（finish 不再补收尾骨架——error 事件后接 message_stop 是畸形流）
	finished            bool
}

// WriteHeader 拦截：200 不转发（SSE 靠首写隐式 200，头已经 Header() 直通落位；
// 非流式由 finish 以折回后的形态统一写出）；非 200 进入缓冲模式。
func (b *poolAnthropicBridge) WriteHeader(code int) {
	b.status = code
	if code != http.StatusOK {
		b.mode = 2
	}
}

func (b *poolAnthropicBridge) Write(p []byte) (int, error) {
	if b.mode == 0 {
		if strings.Contains(b.Header().Get("Content-Type"), "text/event-stream") {
			b.mode = 1
			fl, _ := b.ResponseWriter.(http.Flusher)
			b.x = newAnthropicStreamXlator(b.display, anthropicEventWriter(b.ResponseWriter, fl))
		} else {
			b.mode = 2
		}
	}
	if b.mode == 1 {
		// 追加而非 [:0] 复用：跨 Write 的半帧必须保留前缀（若用 [:0] 复用底层数组，
		// 下一次 Write 会覆盖未消费的半帧前缀——当前管线每 Write 恰好一整帧不会
		// 触发，但这是正确性地雷；追加写内存有界：消费过的前缀随 realloc 丢弃）。
		b.sseBuf = append(b.sseBuf, p...)
		for {
			idx := bytes.Index(b.sseBuf, []byte("\n\n"))
			if idx < 0 {
				break // 半帧留缓冲，等下一次 Write 拼齐
			}
			frame := string(b.sseBuf[:idx])
			b.sseBuf = b.sseBuf[idx+2:]
			if !strings.HasPrefix(frame, "data: ") {
				// SSE 注释帧（`: keep-alive`）必须原样转发给客户端：它是网关心跳，
				// 唯一作用就是让客户端/代理看到「链路还活着」。此前这里一律 continue，
				// 于是 upstream.StreamHint 写出的注释帧在这一层被**静默吞掉**——
				// StreamHint 侧看到的是 Write 成功（n 字节 / err=nil），客户端却一个
				// 字节都没收到，/v1/messages 的首帧前静默期又退回零字节（Claude Code
				// 类客户端仍会 idle timeout）。
				// 只转发 `:` 开头的帧（标准注释语义，任何 SSE 客户端都忽略）；其他非
				// data 行（上游的 event:/id: 等）继续丢弃，翻译语义不变。
				if strings.HasPrefix(frame, ":") {
					if _, werr := io.WriteString(b.ResponseWriter, frame+"\n\n"); werr == nil {
						b.Flush()
					}
				}
				continue // 空行/其他非 data 行
			}
			payload := strings.TrimSpace(strings.TrimPrefix(frame, "data: "))
			if payload == "" {
				continue
			}
			if payload == "[DONE]" {
				b.sseDone = true
				continue
			}
			if b.sseDone {
				continue
			}
			if !b.x.ingest(payload) {
				b.sseDone = true
				b.errored = true // 流中错误帧已透出，停止翻译且不再补收尾骨架
			}
		}
		return len(p), nil
	}
	b.bodyBuf = append(b.bodyBuf, p...)
	return len(p), nil
}

// Flush 实现 http.Flusher：sse.go 逐帧 flush 打到真实 w（翻译事件内部已各自 flush）。
func (b *poolAnthropicBridge) Flush() {
	if fl, ok := b.ResponseWriter.(http.Flusher); ok {
		fl.Flush()
	}
}

// finish chatCompletions 返回后收尾：流式补齐 Anthropic 收尾骨架（错误帧已透出
// 时不再补）；缓冲态把 OpenAI 整包/错误体折回 Anthropic 形态。
func (b *poolAnthropicBridge) finish() {
	if b.finished {
		return
	}
	b.finished = true
	switch b.mode {
	case 1:
		if !b.errored {
			b.x.finish() // 正常/[DONE] 收尾；流中错误帧后不再补骨架（与 ext 路径同语义）
		}
	case 2:
		status := b.status
		if status == 0 {
			status = http.StatusOK
		}
		if status >= 400 {
			writeAnthropicError(b.ResponseWriter, status, anthropicErrTypeFor(status), upstreamErrorMsg(b.bodyBuf))
			return
		}
		resp, _, ok := openAIRespToAnthropic(b.bodyBuf, b.display)
		if !ok {
			writeAnthropicError(b.ResponseWriter, http.StatusBadGateway, "api_error", "pool path returned unparseable response")
			return
		}
		writeJSON(b.ResponseWriter, http.StatusOK, resp)
	default:
		writeAnthropicError(b.ResponseWriter, http.StatusBadGateway, "api_error", "pool path returned no response")
	}
}
