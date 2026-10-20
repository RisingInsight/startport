// responses.go 第三入站协议面：POST /v1/responses（OpenAI Responses API，
// Codex CLI 主入口）。
//
// 与 /v1/messages 同一哲学：请求折成 OpenAI Chat Completions wire 进既有调度
// ——ext 渠道可服务的模型走 extchan.Do；非 ext 模型回退 CodeBuddy 主池
// （chatCompletions 全套治理复用，响应经 poolResponsesBridge 实时折回）。
//
// 覆盖主干（Codex CLI 实际使用面）：instructions、input（string / message /
// function_call / function_call_output items）、function 工具（扁平形态）、
// tool_choice、max_output_tokens / temperature / top_p、stream。
// 明确不支持（400，不静默）：previous_response_id / store（网关无会话存储）、
// 非 function 工具（web_search / file_search 等）、input_image、item_reference。
// reasoning 输出不建模（上游不产生 reasoning item；缺失该类事件对 Codex 合法）。
//
// 流式事件骨架（OpenAI 官方序列的必需子集）：
// response.created → response.in_progress →
//   [message: output_item.added + content_part.added + output_text.delta* +
//    output_text.done + content_part.done + output_item.done] |
//   [function_call: output_item.added + function_call_arguments.delta* +
//    function_call_arguments.done + output_item.done] →
// response.completed（含 usage）。流中错误帧 → event: error 后终止（不补骨架）。
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
)

// responsesReq Responses 请求主干（未知字段忽略）。
type responsesReq struct {
	Model           string          `json:"model"`
	Input           json.RawMessage `json:"input"`
	Instructions    json.RawMessage `json:"instructions"`
	Stream          bool            `json:"stream"`
	MaxOutputTokens int             `json:"max_output_tokens"`
	Temperature     *float64        `json:"temperature"`
	TopP            *float64        `json:"top_p"`
	Tools           []responsesTool `json:"tools"`
	ToolChoice      json.RawMessage `json:"tool_choice"`
	ParallelToolCalls *bool         `json:"parallel_tool_calls"`
	// 网关无会话存储：previous_response_id / store 出现即 400（见 responsesToOpenAI）。
	PreviousResponseID string `json:"previous_response_id"`
}

// responsesTool Responses 的工具声明（function 为扁平形态，与 chat 的嵌套不同）。
type responsesTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// responsesUsage 用量（Responses 口径字段名）。
type responsesUsage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	TotalTokens  int64 `json:"total_tokens"`
}

// writeResponsesError Responses 错误信封（type 沿用状态映射，code 给状态语义串）。
func writeResponsesError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    anthropicErrTypeFor(status),
			"code":    responsesErrCode(status),
			"param":   nil,
		},
	})
}

// responsesErrCode 状态 → OpenAI 风格错误码串。
func responsesErrCode(status int) string {
	switch {
	case status == http.StatusTooManyRequests:
		return "rate_limit_exceeded"
	case status == http.StatusUnauthorized:
		return "invalid_api_key"
	case status == http.StatusForbidden:
		return "forbidden"
	case status == http.StatusNotFound:
		return "model_not_found"
	case status == http.StatusServiceUnavailable:
		return "overloaded"
	case status >= 500:
		return "server_error"
	default:
		return "invalid_request_error"
	}
}

// handleResponses /v1/responses 入口（外部渠道优先，CodeBuddy 池回退）。
// 路由判定与 handleExtMessages 完全同构（ErrModelNotExt 落池；ext: 前缀错误
// 与多渠道同名 400 不回退；Ext==nil 全部落池）。
func (h *Handler) handleResponses(w http.ResponseWriter, r *http.Request) {
	apiKeyName, _ := r.Context().Value(apiKeyNameKey{}).(string)
	body, err := io.ReadAll(io.LimitReader(r.Body, h.maxBodyOr(64<<20)))
	if err != nil {
		writeResponsesError(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	var req responsesReq
	if err := json.Unmarshal(body, &req); err != nil {
		writeResponsesError(w, http.StatusBadRequest, "parse body: "+err.Error())
		return
	}
	if req.Model == "" {
		writeResponsesError(w, http.StatusBadRequest, "model 不能为空")
		return
	}
	if h.cfg.Ext != nil {
		ch2, upstream, display, rerr := h.cfg.Ext.RouteModel(req.Model)
		switch {
		case rerr == nil:
			h.responsesExtRoute(w, r, apiKeyName, &req, ch2, upstream, display)
			return
		case errors.Is(rerr, extchan.ErrModelNotExt):
			// 裸名不属于任何外部渠道 → 落主池（下方）。
		default:
			writeResponsesError(w, http.StatusBadRequest, rerr.Error())
			return
		}
	}
	h.handleResponsesPoolFallback(w, r, &req)
}

// responsesExtRoute 外部渠道分支：Responses → OpenAI 转换后交 extchan.Do，
// 响应按原方向折回（流式逐事件 / 非流式整包）。
func (h *Handler) responsesExtRoute(w http.ResponseWriter, r *http.Request, apiKeyName string,
	req *responsesReq, ch2 *extchan.Channel, upstream, display string) {

	oaiBody, convErr := responsesToOpenAI(req, upstream)
	if convErr != nil {
		writeResponsesError(w, http.StatusBadRequest, convErr.Error())
		return
	}
	attemptStarted := time.Now()
	res, derr := h.cfg.Ext.Do(r.Context(), ch2, &extchan.OutRequest{
		Body:         oaiBody,
		Stream:       req.Stream,
		DisplayModel: display,
	})
	if derr != nil {
		status, _, msg := extErrorFields(derr)
		writeResponsesError(w, status, extErrMsg(msg))
		return
	}
	cred := ch2.CredentialByLabel(res.CredentialLabel)
	if res.Status >= 400 {
		writeResponsesError(w, res.Status, provider.ScrubSecrets(upstreamErrorMsg(res.Body), credSecret(cred)))
		return
	}
	if req.Stream {
		h.responsesStreamFromReader(w, r.Context(), display, res.Reader, apiKeyName, ch2, cred, attemptStarted)
		return
	}
	resp, u, ok := openAIRespToResponses(res.Body, display)
	if !ok {
		writeResponsesError(w, http.StatusBadGateway, "ext channel returned unparseable response")
		return
	}
	if u != nil {
		h.recordExtMessagesUsage(apiKeyName, ch2, cred, display, u.prompt, u.completion, u.total, attemptStarted)
	}
	writeJSON(w, http.StatusOK, resp)
}

// extErrMsg ext 错误 → 人话（脱敏后透传；凭证池错误语义由状态码承载）。
func extErrMsg(msg string) string {
	return strings.TrimSpace(msg)
}

// handleResponsesPoolFallback 主池回退：Responses 请求折成 OpenAI wire 交给
// chatCompletions 主链路（选号/粘性/轮转/记账复用），响应经 poolResponsesBridge
// 实时折回 Responses 形态；JSONL 归档 Path 记 /v1/responses。
func (h *Handler) handleResponsesPoolFallback(w http.ResponseWriter, r *http.Request, req *responsesReq) {
	if h.cfg.Pool == nil {
		writeResponsesError(w, http.StatusNotImplemented, "no account pool configured; /v1/responses serves ext channels or a configured pool")
		return
	}
	oaiBody, convErr := responsesToOpenAI(req, req.Model)
	if convErr != nil {
		writeResponsesError(w, http.StatusBadRequest, convErr.Error())
		return
	}
	r2 := r.Clone(r.Context())
	r2.Body = io.NopCloser(bytes.NewReader(oaiBody))
	r2.ContentLength = int64(len(oaiBody))
	r2.Header.Set("Content-Type", "application/json")

	bridge := &poolResponsesBridge{ResponseWriter: w, display: req.Model}
	if h.cfg.RequestLog != nil {
		trace := &requestTrace{id: reqlog.NewRequestID(), start: time.Now(), path: "/v1/responses"}
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

// responsesToOpenAI Responses 请求 → canonical OpenAI Chat Completions wire。
// bare 为出站模型名（ext 路径=渠道内上游名；主池路径=原模型名，前缀由
// chatCompletions 解析）。
func responsesToOpenAI(a *responsesReq, bare string) ([]byte, error) {
	if a.PreviousResponseID != "" {
		return nil, fmt.Errorf("previous_response_id 不支持：网关无会话存储，请每次携带完整 input")
	}
	oai := map[string]any{
		"model":    bare,
		"messages": []map[string]any{},
	}
	if a.MaxOutputTokens > 0 {
		oai["max_tokens"] = a.MaxOutputTokens
	}
	if a.Temperature != nil {
		oai["temperature"] = *a.Temperature
	}
	if a.TopP != nil {
		oai["top_p"] = *a.TopP
	}
	if a.Stream {
		oai["stream"] = true
		oai["stream_options"] = map[string]any{"include_usage": true}
	}
	if a.ParallelToolCalls != nil {
		oai["parallel_tool_calls"] = *a.ParallelToolCalls
	}

	msgs := []map[string]any{}
	appendMsg := func(role string, content any) {
		msgs = append(msgs, map[string]any{"role": role, "content": content})
	}
	// instructions → 首条 system（string 或 parts 数组；image part 拒绝）。
	if len(a.Instructions) > 0 && string(a.Instructions) != "null" {
		text, hasImage, err := responsesContentText(a.Instructions)
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(text) != "" {
			appendMsg("system", text)
		} else if hasImage {
			return nil, fmt.Errorf("instructions 不支持 image part")
		}
	}
	// input：string 或 item 数组。
	var inputRaw string
	if json.Unmarshal(a.Input, &inputRaw) == nil {
		if strings.TrimSpace(inputRaw) != "" {
			appendMsg("user", inputRaw)
		}
	} else {
		var items []responsesInputItem
		if err := json.Unmarshal(a.Input, &items); err != nil {
			return nil, fmt.Errorf("input 形态不合法（应为 string 或 item 数组）")
		}
		for _, it := range items {
			switch it.Type {
			case "", "message":
				role := it.Role
				if role == "" {
					role = "user"
				}
				if role != "user" && role != "assistant" && role != "system" && role != "developer" {
					return nil, fmt.Errorf("不支持的 message role %q", role)
				}
				text, hasImage, err := responsesContentText(it.Content)
				if err != nil {
					return nil, err
				}
				if hasImage {
					return nil, fmt.Errorf("input_image 暂不支持（图片输入走 /v1/chat/completions 的 image_url）")
				}
				if role == "developer" {
					role = "system"
				}
				appendMsg(role, text)
			case "function_call":
				if it.CallID == "" || it.Name == "" {
					return nil, fmt.Errorf("function_call item 缺 call_id/name")
				}
				msgs = append(msgs, map[string]any{
					"role":    "assistant",
					"content": nil,
					"tool_calls": []map[string]any{{
						"id":   it.CallID,
						"type": "function",
						"function": map[string]any{
							"name":      it.Name,
							"arguments": it.Arguments,
						},
					}},
				})
			case "function_call_output":
				if it.CallID == "" {
					return nil, fmt.Errorf("function_call_output item 缺 call_id")
				}
				msgs = append(msgs, map[string]any{
					"role":         "tool",
					"tool_call_id": it.CallID,
					"content":      it.Output,
				})
			case "reasoning":
				// 上游不消费 reasoning item（本网关不产生）：静默跳过，
				// Codex 回传的历史 reasoning 不阻断请求。
			default:
				return nil, fmt.Errorf("不支持的 input item type %q", it.Type)
			}
		}
	}
	oai["messages"] = msgs

	// tools：Responses 扁平 function → chat 嵌套 function；非 function 工具拒绝。
	if len(a.Tools) > 0 {
		oaiTools := make([]map[string]any, 0, len(a.Tools))
		for _, t := range a.Tools {
			if t.Type != "" && t.Type != "function" {
				return nil, fmt.Errorf("不支持的工具类型 %q（仅 function）", t.Type)
			}
			fn := map[string]any{
				"name":        t.Name,
				"description": t.Description,
				"parameters":  t.Parameters,
			}
			if len(t.Parameters) == 0 {
				fn["parameters"] = map[string]any{"type": "object", "properties": map[string]any{}}
			}
			oaiTools = append(oaiTools, map[string]any{"type": "function", "function": fn})
		}
		oai["tools"] = oaiTools
	}
	// tool_choice："none"/"auto"/"required" 直传；{type:function,name} → 嵌套。
	if len(a.ToolChoice) > 0 && string(a.ToolChoice) != "null" {
		var choice string
		if json.Unmarshal(a.ToolChoice, &choice) == nil {
			oai["tool_choice"] = choice
		} else {
			var named struct {
				Type string `json:"type"`
				Name string `json:"name"`
			}
			if err := json.Unmarshal(a.ToolChoice, &named); err != nil || named.Name == "" {
				return nil, fmt.Errorf("tool_choice 形态不合法")
			}
			oai["tool_choice"] = map[string]any{
				"type":     "function",
				"function": map[string]any{"name": named.Name},
			}
		}
	}
	out, err := json.Marshal(oai)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// responsesInputItem input item 的宽松形态。
type responsesInputItem struct {
	Type      string          `json:"type"`
	Role      string          `json:"role"`
	Content   json.RawMessage `json:"content"`
	Name      string          `json:"name"`
	CallID    string          `json:"call_id"`
	Arguments string          `json:"arguments"`
	Output    json.RawMessage `json:"output"`
}

// responsesContentText content（string 或 parts 数组）→ 合并文本。
// input_text / output_text 取 text；image/其他类型返回 hasImage（调用方拒绝）。
func responsesContentText(raw json.RawMessage) (string, bool, error) {
	if len(raw) == 0 {
		return "", false, nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, false, nil
	}
	var parts []map[string]any
	if json.Unmarshal(raw, &parts) != nil {
		return "", false, fmt.Errorf("content 形态不合法（应为 string 或 part 数组）")
	}
	var sb strings.Builder
	hasImage := false
	for _, p := range parts {
		switch p["type"] {
		case "input_text", "output_text", "text", "summary_text":
			if t, ok := p["text"].(string); ok {
				sb.WriteString(t)
			}
		case "input_image", "image":
			hasImage = true
		}
	}
	return sb.String(), hasImage, nil
}

// ---------------------------------------------------------------------------
// OpenAI 响应 → Responses 形态（非流式整包 + 流式逐帧两条转换路径）
// ---------------------------------------------------------------------------

// openAIRespToResponses 非流式 OpenAI Chat Completions 响应 → Responses 对象
// （纯转换不含记账）。ok=false 表示 body 不可解析（调用方回 502）。
func openAIRespToResponses(respBody []byte, model string) (map[string]any, *oa2aUsage, bool) {
	var oaiResp struct {
		Choices []struct {
			Message struct {
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
	output := []map[string]any{}
	if choice.Message.Content != "" {
		output = append(output, map[string]any{
			"type":     "message",
			"id":       "msg_" + fmt.Sprintf("%d", time.Now().UnixNano()),
			"status":   "completed",
			"role":     "assistant",
			"content": []map[string]any{{"type": "output_text", "text": choice.Message.Content, "annotations": []any{}}},
		})
	}
	for i, tc := range choice.Message.ToolCalls {
		var args json.RawMessage = json.RawMessage(tc.Function.Arguments)
		if len(args) == 0 {
			args = json.RawMessage("{}")
		}
		output = append(output, map[string]any{
			"type":      "function_call",
			"id":        fmt.Sprintf("fc_%d_%d", time.Now().UnixNano(), i),
			"call_id":   tc.ID,
			"name":      tc.Function.Name,
			"arguments": string(args),
			"status":    "completed",
		})
	}
	resp := map[string]any{
		"id":                 "resp_" + fmt.Sprintf("%d", time.Now().UnixNano()),
		"object":             "response",
		"created_at":         time.Now().Unix(),
		"status":             "completed",
		"model":              model,
		"output":             output,
		"parallel_tool_calls": true,
		"error":              nil,
		"incomplete_details": nil,
	}
	if oaiResp.Usage != nil {
		resp["usage"] = responsesUsage{
			InputTokens:  oaiResp.Usage.PromptTokens,
			OutputTokens: oaiResp.Usage.CompletionTokens,
			TotalTokens:  oaiResp.Usage.TotalTokens,
		}
		u := &oa2aUsage{
			prompt:     oaiResp.Usage.PromptTokens,
			completion: oaiResp.Usage.CompletionTokens,
			total:      oaiResp.Usage.TotalTokens,
		}
		return resp, u, true
	}
	return resp, nil, true
}

// responsesStreamXlator OpenAI chunk 流 → Responses 事件流的翻译状态机。
// 事件骨架见文件头。工具调用分片在 finish 阶段聚合下发 arguments.done。
type responsesStreamXlator struct {
	sendEvent func(event string, payload any)
	model     string
	respID    string
	started   bool
	// 文本块状态（单文本块：content_index 恒 0）。
	itemID   string
	itemOpen bool
	partOpen bool
	textAll  strings.Builder
	outIdx   int
	// 工具调用聚合（OpenAI 流式按 index 分片 arguments）。
	toolAgg map[int]*openAIToolCall
	toolSeq []int
	toolOutIdx map[int]int
	// 完成的文本块（finish 组装最终 output 用）。
	lastText       string
	lastTextItemID string
	// 用量与结束原因。
	finishReason string
	inTokens     int64
	outTokens    int64
	hasUsage     bool
}

func newResponsesStreamXlator(model string, sendEvent func(string, any)) *responsesStreamXlator {
	return &responsesStreamXlator{
		sendEvent: sendEvent,
		model:     model,
		respID:    "resp_" + fmt.Sprintf("%d", time.Now().UnixNano()),
		toolAgg:   map[int]*openAIToolCall{},
	}
}

// responsesRespSkeleton 事件里 response 对象的骨架（流中 output 恒空数组，
// 最终 output 在 response.completed 里给全量）。
func (x *responsesStreamXlator) responsesRespSkeleton(status string) map[string]any {
	return map[string]any{
		"id":         x.respID,
		"object":     "response",
		"created_at": time.Now().Unix(),
		"status":     status,
		"model":      x.model,
		"output":     []any{},
		"error":      nil,
	}
}

// start 发出 response.created + response.in_progress（幂等）。
func (x *responsesStreamXlator) start() {
	if x.started {
		return
	}
	x.started = true
	x.sendEvent("response.created", map[string]any{"response": x.responsesRespSkeleton("in_progress")})
	x.sendEvent("response.in_progress", map[string]any{"response": x.responsesRespSkeleton("in_progress")})
}

// ingest 处理一个 OpenAI chunk payload。返回 false = 流中错误帧已按
// event: error 透出，调用方立即终止（不再补 completed）。
func (x *responsesStreamXlator) ingest(payload string) bool {
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
		return true // 非法帧跳过（与既有翻译器语义一致）
	}
	if chunk.Error != nil {
		x.sendEvent("error", map[string]any{
			"code":    "api_error",
			"message": chunk.Error.Message,
			"param":   nil,
		})
		return false
	}
	for _, c := range chunk.Choices {
		if c.Delta.Content != "" {
			x.ensureMessageItem()
			x.textAll.WriteString(c.Delta.Content)
			x.sendEvent("response.output_text.delta", map[string]any{
				"item_id": x.itemID, "output_index": x.outIdx, "content_index": 0,
				"delta": c.Delta.Content,
			})
		}
		for _, tc := range c.Delta.ToolCalls {
			agg := x.toolAgg[tc.Index]
			if agg == nil {
				// 新工具调用：收尾打开中的文本块，开 function_call item。
				x.closeTextItem()
				x.outIdx++
				if x.toolOutIdx == nil {
					x.toolOutIdx = map[int]int{}
				}
				x.toolOutIdx[tc.Index] = x.outIdx
				agg = &openAIToolCall{ID: tc.ID}
				agg.Function.Name = tc.Function.Name
				x.toolAgg[tc.Index] = agg
				x.toolSeq = append(x.toolSeq, tc.Index)
				x.sendEvent("response.output_item.added", map[string]any{
					"output_index": x.outIdx,
					"item": map[string]any{
						"id": fmt.Sprintf("fc_%s_%d", strings.TrimPrefix(agg.ID, "call_"), tc.Index),
						"type": "function_call", "status": "in_progress",
						"call_id": agg.ID, "name": agg.Function.Name, "arguments": "",
					},
				})
			}
			if tc.Function.Name != "" {
				agg.Function.Name = tc.Function.Name
			}
			if tc.ID != "" {
				agg.ID = tc.ID
			}
			if tc.Function.Arguments != "" {
				agg.Function.Arguments += tc.Function.Arguments
				x.sendEvent("response.function_call_arguments.delta", map[string]any{
					"item_id": x.toolItemID(tc.Index), "output_index": x.toolOutIdx[tc.Index],
					"delta": tc.Function.Arguments,
				})
			}
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

// ensureMessageItem 懒开 message item + output_text part。
func (x *responsesStreamXlator) ensureMessageItem() {
	if x.itemOpen {
		return
	}
	x.itemID = "msg_" + fmt.Sprintf("%d", time.Now().UnixNano())
	x.sendEvent("response.output_item.added", map[string]any{
		"output_index": x.outIdx,
		"item": map[string]any{
			"id": x.itemID, "type": "message", "status": "in_progress",
			"role": "assistant", "content": []any{},
		},
	})
	x.sendEvent("response.content_part.added", map[string]any{
		"item_id": x.itemID, "output_index": x.outIdx, "content_index": 0,
		"part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
	})
	x.itemOpen = true
	x.partOpen = true
}

// closeTextItem 收尾打开中的文本块（done 三连 + output_index 前进）。
func (x *responsesStreamXlator) closeTextItem() {
	if !x.itemOpen {
		return
	}
	full := x.textAll.String()
	x.sendEvent("response.output_text.done", map[string]any{
		"item_id": x.itemID, "output_index": x.outIdx, "content_index": 0, "text": full,
	})
	x.sendEvent("response.content_part.done", map[string]any{
		"item_id": x.itemID, "output_index": x.outIdx, "content_index": 0,
		"part": map[string]any{"type": "output_text", "text": full, "annotations": []any{}},
	})
	x.sendEvent("response.output_item.done", map[string]any{
		"output_index": x.outIdx,
		"item": map[string]any{
			"id": x.itemID, "type": "message", "status": "completed",
			"role": "assistant",
			"content": []map[string]any{{"type": "output_text", "text": full, "annotations": []any{}}},
		},
	})
	x.lastText = full
	x.lastTextItemID = x.itemID
	x.itemOpen = false
	x.partOpen = false
	x.outIdx++
}

// toolItemID 工具 item 的事件定位 id（与 output_item.added 里一致）。
func (x *responsesStreamXlator) toolItemID(idx int) string {
	agg := x.toolAgg[idx]
	if agg == nil {
		return ""
	}
	return fmt.Sprintf("fc_%s_%d", strings.TrimPrefix(agg.ID, "call_"), idx)
}

// finish 收尾：文本块 done 三连 + 工具 arguments.done/output_item.done +
// response.completed（全量 output + usage）。空流也补齐骨架（completed 空输出）。
func (x *responsesStreamXlator) finish() {
	x.start()
	x.closeTextItem()
	output := []map[string]any{}
	for _, idx := range x.toolSeq {
		agg := x.toolAgg[idx]
		itemID := x.toolItemID(idx)
		args := agg.Function.Arguments
		if strings.TrimSpace(args) == "" {
			args = "{}"
		}
		x.sendEvent("response.function_call_arguments.done", map[string]any{
			"item_id": itemID, "output_index": x.toolOutIdx[idx], "arguments": args,
		})
		item := map[string]any{
			"id": itemID, "type": "function_call", "status": "completed",
			"call_id": agg.ID, "name": agg.Function.Name, "arguments": args,
		}
		x.sendEvent("response.output_item.done", map[string]any{"output_index": x.toolOutIdx[idx], "item": item})
		output = append(output, item)
	}
	final := x.responsesRespSkeleton("completed")
	if x.lastText != "" {
		output = append([]map[string]any{{
			"id": x.lastTextItemID, "type": "message", "status": "completed",
			"role": "assistant",
			"content": []map[string]any{{"type": "output_text", "text": x.lastText, "annotations": []any{}}},
		}}, output...)
	}
	if x.hasUsage {
		final["usage"] = responsesUsage{InputTokens: x.inTokens, OutputTokens: x.outTokens, TotalTokens: x.inTokens + x.outTokens}
	}
	x.sendEvent("response.completed", map[string]any{"response": final})
}

// ---------------------------------------------------------------------------
// 主池回退桥：OpenAI 形态 → Responses 形态。结构与 poolAnthropicBridge 同构
//（翻译器/错误形态/非流式转换不同），帧缓冲语义双桥一致（含半帧拼接修复，
// 追加写而非 [:0] 复用）。
// ---------------------------------------------------------------------------

type poolResponsesBridge struct {
	http.ResponseWriter // Header() 直通真实 w：流式头由 sse.go 正常落位
	display  string
	x        *responsesStreamXlator
	mode     int // 0=未定 1=SSE 逐帧翻译 2=整包缓冲
	status   int
	sseBuf   []byte
	bodyBuf  []byte
	sseDone  bool
	errored  bool
	finished bool
}

func (b *poolResponsesBridge) WriteHeader(code int) {
	b.status = code
	if code != http.StatusOK {
		b.mode = 2
	}
}

func (b *poolResponsesBridge) Write(p []byte) (int, error) {
	if b.mode == 0 {
		if strings.Contains(b.Header().Get("Content-Type"), "text/event-stream") {
			b.mode = 1
			fl, _ := b.ResponseWriter.(http.Flusher)
			b.x = newResponsesStreamXlator(b.display, responsesEventWriter(b.ResponseWriter, fl))
		} else {
			b.mode = 2
		}
	}
	if b.mode == 1 {
		b.sseBuf = append(b.sseBuf, p...)
		for {
			idx := bytes.Index(b.sseBuf, []byte("\n\n"))
			if idx < 0 {
				break // 半帧留缓冲，等下一次 Write 拼齐
			}
			frame := string(b.sseBuf[:idx])
			b.sseBuf = b.sseBuf[idx+2:]
			if !strings.HasPrefix(frame, "data: ") {
				continue
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
				b.errored = true
			}
		}
		return len(p), nil
	}
	b.bodyBuf = append(b.bodyBuf, p...)
	return len(p), nil
}

// Flush 实现 http.Flusher：sse.go 逐帧 flush 打到真实 w。
func (b *poolResponsesBridge) Flush() {
	if fl, ok := b.ResponseWriter.(http.Flusher); ok {
		fl.Flush()
	}
}

// finish chatCompletions 返回后收尾：流式补 completed（流中错误帧后不补）；
// 缓冲态把 OpenAI 整包/错误体折回 Responses 形态。
func (b *poolResponsesBridge) finish() {
	if b.finished {
		return
	}
	b.finished = true
	switch b.mode {
	case 1:
		if !b.errored {
			b.x.finish()
		}
	case 2:
		status := b.status
		if status == 0 {
			status = http.StatusOK
		}
		if status >= 400 {
			writeResponsesError(b.ResponseWriter, status, upstreamErrorMsg(b.bodyBuf))
			return
		}
		resp, _, ok := openAIRespToResponses(b.bodyBuf, b.display)
		if !ok {
			writeResponsesError(b.ResponseWriter, http.StatusBadGateway, "pool path returned unparseable response")
			return
		}
		writeJSON(b.ResponseWriter, http.StatusOK, resp)
	default:
		writeResponsesError(b.ResponseWriter, http.StatusBadGateway, "pool path returned no response")
	}
}

// responsesEventWriter SSE 事件写出闭包（Responses 事件面；ext 流式与主池桥共用）。
func responsesEventWriter(w http.ResponseWriter, fl http.Flusher) func(string, any) {
	return func(event string, payload any) {
		bs, err := json.Marshal(payload)
		if err != nil {
			return
		}
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, bs)
		if fl != nil {
			fl.Flush()
		}
	}
}

// responsesStreamFromReader ext 渠道流式：OpenAI SSE → Responses 事件流
//（记账与 extMessagesStream 同口径：realm=ext，凭证级计数）。
func (h *Handler) responsesStreamFromReader(w http.ResponseWriter, ctx context.Context,
	display string, rc io.ReadCloser, apiKeyName string, ch2 *extchan.Channel,
	cred *extchan.Credential, attemptStarted time.Time) {
	defer rc.Close()
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	fl, _ := w.(http.Flusher)
	x := newResponsesStreamXlator(display, responsesEventWriter(w, fl))
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
			return // 流中错误帧：error 事件已透出，不补 completed
		}
	}
	x.finish()
	if x.hasUsage {
		h.recordExtMessagesUsage(apiKeyName, ch2, cred, display, x.inTokens, x.outTokens, x.inTokens+x.outTokens, attemptStarted)
	}
}
