// exthropic.go 外部渠道的 Anthropic Messages 出口（POST /v1/messages）。
//
// 目的：Claude Code / Anthropic SDK 客户端直连本网关使用外部渠道。转换是
// **协议级**的：请求 Anthropic Messages → OpenAI Chat Completions 发往 ext
// 渠道（由 extchan.Do 按协议适配器决定最终上游形态）；响应按原方向折回。
// 只覆盖 /v1/messages 主干（system / messages / tools / max_tokens /
// temperature / stream）；thinking 块、cache_control、citations 等 Anthropic
// 专有特性不透传（ext 渠道侧无对应语义）。
//
// 阶段三变化（语义未变）：
//   - 转发改走 extchan.Do（凭证池选证/换证 + 协议适配）；
//   - 记账带凭证 label，并累计凭证级进程内计数；
//   - 转换函数 anthropicToOpenAI / extMessagesSync / extMessagesStream 的
//     转换语义一字未改（只换了调用形态与参数字段）。
package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/RisingInsight/startport/internal/extchan"
	"github.com/RisingInsight/startport/internal/provider"
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

// handleExtMessages /v1/messages 入口（仅外部渠道）。
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
	if h.cfg.Ext == nil {
		writeAnthropicError(w, http.StatusNotImplemented, "api_error", "no ext channels configured; /v1/messages serves ext channels only")
		return
	}
	ch2, upstream, display, rerr := h.cfg.Ext.RouteModel(areq.Model)
	if rerr != nil {
		writeAnthropicError(w, http.StatusBadRequest, "not_found_error", rerr.Error())
		return
	}

	// Anthropic → OpenAI 请求转换（上游模型名：别名已解析）。
	oaiBody, convErr := anthropicToOpenAI(&areq, upstream)
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

// extMessagesSync 非流式：OpenAI 响应 → Anthropic Messages 形态。
// display 为客户端可见模型名（"ext:<渠道>/<模型或别名>"）。
func (h *Handler) extMessagesSync(w http.ResponseWriter, ch2 *extchan.Channel,
	cred *extchan.Credential, display string, respBody []byte,
	attemptStarted time.Time, apiKeyName string) {
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
		writeAnthropicError(w, http.StatusBadGateway, "api_error", "ext channel returned unparseable response")
		return
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
	stopReason := mapAnthropicStop(choice.FinishReason)
	resp := map[string]any{
		"id":            "msg_" + fmt.Sprintf("%d", time.Now().UnixNano()),
		"type":          "message",
		"role":          "assistant",
		"model":         display,
		"content":       blocks,
		"stop_reason":   stopReason,
		"stop_sequence": nil,
	}
	if oaiResp.Usage != nil {
		resp["usage"] = map[string]any{
			"input_tokens":  oaiResp.Usage.PromptTokens,
			"output_tokens": oaiResp.Usage.CompletionTokens,
		}
		// 用量记账（realm=ext，与 chat 路径同口径）+ 凭证级进程内计数。
		h.recordExtMessagesUsage(apiKeyName, ch2, cred, display, oaiResp.Usage.PromptTokens,
			oaiResp.Usage.CompletionTokens, oaiResp.Usage.TotalTokens, attemptStarted)
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

// extMessagesStream 流式：OpenAI SSE → Anthropic SSE 事件序列。
// 事件骨架：message_start → (content_block_start + content_block_delta* +
// content_block_stop)* → message_delta(stop_reason, usage) → message_stop。
// 工具调用流式拼装不做（OpenAI 侧 arguments 分片在第一刀直接聚合成完整
// tool_use 块后一次性下发 content_block_start+input_delta+stop）。
func (h *Handler) extMessagesStream(w http.ResponseWriter, ch2 *extchan.Channel,
	cred *extchan.Credential, display string, rc io.ReadCloser,
	attemptStarted time.Time, apiKeyName string) {
	defer rc.Close()
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	sendEvent := func(event string, payload any) {
		b, err := json.Marshal(payload)
		if err != nil {
			return
		}
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
		if flusher != nil {
			flusher.Flush()
		}
	}
	sendEvent("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": "msg_" + fmt.Sprintf("%d", time.Now().UnixNano()), "type": "message",
			"role": "assistant", "model": display, "content": []any{},
			"stop_reason": nil, "stop_sequence": nil,
			"usage": map[string]any{"input_tokens": 0, "output_tokens": 0},
		},
	})
	sendEvent("ping", map[string]any{"type": "ping"})

	var (
		textOpen     bool
		textIdx      int
		outTokens    int64
		inTokens     int64
		hasUsage     bool
		finishReason string
		// 工具调用聚合：OpenAI 流式 tool_calls 按 index 分片给 arguments。
		toolAgg = map[int]*openAIToolCall{}
		toolSeq []int
	)
	// text 块从 index 0 开始；工具块在文本块之后顺序编号。
	emitTextDelta := func(s string) {
		if !textOpen {
			sendEvent("content_block_start", map[string]any{
				"type": "content_block_start", "index": textIdx,
				"content_block": map[string]any{"type": "text", "text": ""},
			})
			textOpen = true
		}
		sendEvent("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": textIdx,
			"delta": map[string]any{"type": "text_delta", "text": s},
		})
	}
	closeText := func() {
		if textOpen {
			sendEvent("content_block_stop", map[string]any{"type": "content_block_stop", "index": textIdx})
			textOpen = false
			textIdx++
		}
	}

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
			continue
		}
		if chunk.Error != nil {
			// 流中错误帧：按 Anthropic 形态补发 error 事件并终止。
			sendEvent("error", map[string]any{
				"type":  "error",
				"error": map[string]any{"type": "api_error", "message": chunk.Error.Message},
			})
			return
		}
		for _, c := range chunk.Choices {
			if c.Delta.Content != "" {
				emitTextDelta(c.Delta.Content)
			}
			for _, tc := range c.Delta.ToolCalls {
				agg := toolAgg[tc.Index]
				if agg == nil {
					agg = &openAIToolCall{ID: tc.ID}
					agg.Function.Name = tc.Function.Name
					toolAgg[tc.Index] = agg
					toolSeq = append(toolSeq, tc.Index)
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
				finishReason = c.FinishReason
			}
		}
		if chunk.Usage != nil {
			hasUsage = true
			inTokens = chunk.Usage.PromptTokens
			outTokens = chunk.Usage.CompletionTokens
		}
	}
	closeText()
	// 工具块：整体下发（聚合一刀）。
	for _, idx := range toolSeq {
		tc := toolAgg[idx]
		var input json.RawMessage = json.RawMessage(tc.Function.Arguments)
		if len(input) == 0 {
			input = json.RawMessage("{}")
		}
		sendEvent("content_block_start", map[string]any{
			"type": "content_block_start", "index": textIdx,
			"content_block": map[string]any{"type": "tool_use", "id": tc.ID, "name": tc.Function.Name, "input": map[string]any{}},
		})
		sendEvent("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": textIdx,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": string(input)},
		})
		sendEvent("content_block_stop", map[string]any{"type": "content_block_stop", "index": textIdx})
		textIdx++
	}
	sendEvent("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": mapAnthropicStop(finishReason), "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": outTokens},
	})
	sendEvent("message_stop", map[string]any{"type": "message_stop"})

	if hasUsage {
		h.recordExtMessagesUsage(apiKeyName, ch2, cred, display, inTokens, outTokens, inTokens+outTokens, attemptStarted)
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
	h.cfg.Usage.Add(time.Now(), "ext", "ext:"+ch2.Name, displayModel, usage.Delta{
		KeyName:          apiKeyName,
		PromptTokens:     prompt,
		HasPromptTokens:  prompt > 0,
		CompletionTokens: completion,
		HasCompletion:    completion > 0,
		TotalTokens:      total,
		HasTotal:         total > 0,
		LatencyMs:        latencyMs,
		HasLatency:       true,
	}, prompt > 0 || completion > 0 || total > 0)
}
