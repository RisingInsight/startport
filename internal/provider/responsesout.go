// responsesout.go Responses 出站协议（OpenAI chat wire ↔ Responses wire）。
//
// 首个消费者：Codex 原生适配器（chatgpt backend 只说 Responses）。与 server 包
// 的入站 responses 面（OpenAI ↔ Responses 的另一方向）形状互为镜像，但依赖
// 方向不允许复用（server → provider），协议知识在此独立成文。
//
// 请求（chat → Responses）：
//   - system/developer 消息 → instructions（多条拼接）；
//   - user/assistant 文本 → message 项（input_text / output_text）；
//   - assistant.tool_calls → function_call 项；role=tool → function_call_output 项；
//   - tools → 扁平 function 形态；tool_choice 同步改写；
//   - max_tokens / max_completion_tokens → max_output_tokens；
//   - reasoning_effort → reasoning.effort；
//   - 采样参数（temperature/top_p 等）不上抛——Codex 后端拒绝（codex CLI 从不发送）。
//
// 响应：output[] 折回 chat choices（message.content / tool_calls）+ usage。
// 流式：Responses SSE 事件折回 chat chunks（文本增量 / function_call 增量 /
// usage + finish + [DONE]）；上游恒为 SSE（stream 强制 true），非流式客户端由
// 适配器聚合。
package provider

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// ── 请求方向：OpenAI chat wire → Responses wire ─────────────────────────────

// chatToResponsesRequest 把 canonical OpenAI chat 请求体转成 Responses 请求体
// （map 形态，调用方补平台专属字段后自行 marshal）。
func chatToResponsesRequest(body []byte, stream bool) (map[string]any, error) {
	var probe struct {
		oaiChatRequest
		ReasoningEffort   string `json:"reasoning_effort"`
		ParallelToolCalls *bool  `json:"parallel_tool_calls"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return nil, fmt.Errorf("parse openai request: %w", err)
	}
	var instructions []string
	input := []map[string]any{}
	for _, m := range probe.Messages {
		switch strings.ToLower(m.Role) {
		case "system", "developer":
			if txt := decodeOpenAIContent(m.Content); strings.TrimSpace(txt) != "" {
				instructions = append(instructions, txt)
			}
		case "user":
			txt := decodeOpenAIContent(m.Content)
			if strings.TrimSpace(txt) == "" {
				continue
			}
			input = append(input, map[string]any{
				"type": "message", "role": "user",
				"content": []map[string]any{{"type": "input_text", "text": txt}},
			})
		case "assistant":
			if txt := decodeOpenAIContent(m.Content); strings.TrimSpace(txt) != "" {
				input = append(input, map[string]any{
					"type": "message", "role": "assistant",
					"content": []map[string]any{{"type": "output_text", "text": txt}},
				})
			}
			for _, tc := range m.ToolCalls {
				input = append(input, map[string]any{
					"type":      "function_call",
					"call_id":   firstNonEmpty(tc.ID, "call_"+randomSuffix()),
					"name":      tc.Function.Name,
					"arguments": tc.Function.Arguments,
				})
			}
		case "tool", "function":
			input = append(input, map[string]any{
				"type":    "function_call_output",
				"call_id": m.ToolCallID,
				"output":  decodeOpenAIContent(m.Content),
			})
		default:
			return nil, fmt.Errorf("unsupported message role %q", m.Role)
		}
	}
	req := map[string]any{
		"model":  probe.Model,
		"input":  input,
		"stream": stream,
		"store":  false, // Codex 后端不落库；历史由 input 完整携带
	}
	if len(instructions) > 0 {
		req["instructions"] = strings.Join(instructions, "\n")
	}
	if mt := openAIMaxTokens(&probe.oaiChatRequest); mt > 0 {
		req["max_output_tokens"] = mt
	}
	if eff := strings.ToLower(strings.TrimSpace(probe.ReasoningEffort)); eff != "" {
		req["reasoning"] = map[string]any{"effort": eff}
	}
	if probe.ParallelToolCalls != nil {
		req["parallel_tool_calls"] = *probe.ParallelToolCalls
	}
	if len(probe.Tools) > 0 {
		tools := []map[string]any{}
		for _, tl := range probe.Tools {
			if tl.Type != "" && tl.Type != "function" {
				continue // 非 function 工具（内置工具）跨协议无对应语义，跳过
			}
			schema := tl.Function.Parameters
			if len(schema) == 0 {
				schema = json.RawMessage(`{"type":"object","properties":{}}`)
			}
			tools = append(tools, map[string]any{
				"type": "function", "name": tl.Function.Name,
				"description": tl.Function.Description, "parameters": schema,
			})
		}
		if len(tools) > 0 {
			req["tools"] = tools
		}
	}
	if len(probe.ToolChoice) > 0 && string(probe.ToolChoice) != "null" {
		var tc any
		if err := json.Unmarshal(probe.ToolChoice, &tc); err == nil {
			switch v := tc.(type) {
			case string:
				if v != "" {
					req["tool_choice"] = v
				}
			case map[string]any:
				if fn, ok := v["function"].(map[string]any); ok {
					if name, ok := fn["name"].(string); ok && name != "" {
						req["tool_choice"] = map[string]any{"type": "function", "name": name}
					}
				}
			}
		}
	}
	return req, nil
}

// ── 响应方向（非流式）：Responses response → OpenAI chat envelope ───────────

func responsesResponseToChat(body []byte, clientModel string) []byte {
	var resp struct {
		ID     string `json:"id"`
		Model  string `json:"model"`
		Status string `json:"status"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
		Output []struct {
			Type      string          `json:"type"`
			Role      string          `json:"role"`
			CallID    string          `json:"call_id"`
			Name      string          `json:"name"`
			Arguments string          `json:"arguments"`
			Content   json.RawMessage `json:"content"`
		} `json:"output"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
			TotalTokens  int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return ErrorEnvelope(http.StatusBadGateway, "upstream_parse",
			"codex 上游响应解析失败: "+truncate(string(body), 300))
	}
	if resp.Error != nil && resp.Error.Message != "" {
		return ErrorEnvelope(http.StatusBadGateway, "upstream_error", resp.Error.Message)
	}
	content := ""
	var toolCalls []map[string]any
	for _, item := range resp.Output {
		switch item.Type {
		case "message":
			var parts []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}
			if err := json.Unmarshal(item.Content, &parts); err == nil {
				for _, p := range parts {
					if p.Type == "output_text" {
						content += p.Text
					}
				}
			}
		case "function_call":
			toolCalls = append(toolCalls, map[string]any{
				"id":   item.CallID,
				"type": "function",
				"function": map[string]any{
					"name":      item.Name,
					"arguments": item.Arguments,
				},
			})
		}
	}
	message := map[string]any{"role": "assistant", "content": content}
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
	}
	finish := "stop"
	if len(toolCalls) > 0 {
		finish = "tool_calls"
	}
	out := map[string]any{
		"id":     firstNonEmpty(resp.ID, "chatcmpl-codex"),
		"object": "chat.completion",
		"model":  clientModel,
		"choices": []map[string]any{{
			"index":         0,
			"message":       message,
			"finish_reason": finish,
		}},
	}
	if resp.Usage.InputTokens > 0 || resp.Usage.OutputTokens > 0 {
		out["usage"] = map[string]any{
			"prompt_tokens":     resp.Usage.InputTokens,
			"completion_tokens": resp.Usage.OutputTokens,
			"total_tokens":      resp.Usage.TotalTokens,
		}
	}
	b, err := json.Marshal(out)
	if err != nil {
		return ErrorEnvelope(http.StatusBadGateway, "upstream_parse", "marshal chat response: "+err.Error())
	}
	return b
}

// ── 流方向：Responses SSE → OpenAI chat chunks ──────────────────────────────

type responsesOutStream struct {
	id         string
	model      string
	created    int64
	roleSent   bool
	toolIdx    map[string]int // Responses item_id → OpenAI tool_calls index
	toolCount  int
	finishSent bool
	emitted    bool
}

func newResponsesOutStream(model string) *responsesOutStream {
	return &responsesOutStream{
		id:      "chatcmpl-codex-" + randomSuffix(),
		model:   model,
		created: timeNowUnix(),
		toolIdx: map[string]int{},
	}
}

func (s *responsesOutStream) chunk(delta map[string]any, finish any, usage map[string]any) []byte {
	choice := map[string]any{"index": 0, "delta": delta, "finish_reason": finish}
	body := map[string]any{
		"id": s.id, "object": "chat.completion.chunk", "created": s.created,
		"model": s.model, "choices": []map[string]any{choice},
	}
	if usage != nil {
		body["usage"] = usage
	}
	b, err := json.Marshal(body)
	if err != nil {
		return nil
	}
	return sseData(b)
}

func (s *responsesOutStream) roleChunk() [][]byte {
	if s.roleSent {
		return nil
	}
	s.roleSent = true
	return [][]byte{s.chunk(map[string]any{"role": "assistant", "content": ""}, nil, nil)}
}

func (s *responsesOutStream) finishChunks() [][]byte {
	if s.finishSent {
		return nil
	}
	s.finishSent = true
	return [][]byte{
		s.chunk(map[string]any{}, s.finishReason(), nil),
		sseDone(),
	}
}

// finishReason 有工具调用轮 = tool_calls，否则 stop。
func (s *responsesOutStream) finishReason() string {
	if s.toolCount > 0 {
		return "tool_calls"
	}
	return "stop"
}

// handle 处理单个 Responses SSE 事件，产出 OpenAI chunk 帧。
func (s *responsesOutStream) handle(f sseFrame) ([][]byte, bool, error) {
	switch f.Event {
	case "response.output_item.added":
		var ev struct {
			Item struct {
				ID     string `json:"id"`
				Type   string `json:"type"`
				Role   string `json:"role"`
				CallID string `json:"call_id"`
				Name   string `json:"name"`
			} `json:"item"`
		}
		if err := json.Unmarshal([]byte(f.Data), &ev); err != nil {
			return nil, false, nil // 形状不符的事件忽略（上游新增字段向前兼容）
		}
		switch ev.Item.Type {
		case "message":
			return s.roleChunk(), false, nil
		case "function_call":
			idx := s.toolCount
			s.toolCount++
			if ev.Item.ID != "" {
				s.toolIdx[ev.Item.ID] = idx
			}
			s.emitted = true
			out := append(s.roleChunk(), s.chunk(map[string]any{
				"tool_calls": []map[string]any{{
					"index": idx, "id": ev.Item.CallID, "type": "function",
					"function": map[string]any{"name": ev.Item.Name, "arguments": ""},
				}},
			}, nil, nil))
			return out, false, nil
		}
		return nil, false, nil
	case "response.output_text.delta":
		var ev struct {
			Delta string `json:"delta"`
		}
		if err := json.Unmarshal([]byte(f.Data), &ev); err != nil || ev.Delta == "" {
			return nil, false, nil
		}
		s.emitted = true
		out := append(s.roleChunk(), s.chunk(map[string]any{"content": ev.Delta}, nil, nil))
		return out, false, nil
	case "response.function_call_arguments.delta":
		var ev struct {
			ItemID string `json:"item_id"`
			Delta  string `json:"delta"`
		}
		if err := json.Unmarshal([]byte(f.Data), &ev); err != nil || ev.Delta == "" {
			return nil, false, nil
		}
		idx, ok := s.toolIdx[ev.ItemID]
		if !ok {
			return nil, false, nil
		}
		s.emitted = true
		return [][]byte{s.chunk(map[string]any{
			"tool_calls": []map[string]any{{"index": idx, "arguments": ev.Delta}},
		}, nil, nil)}, false, nil
	case "response.completed":
		var ev struct {
			Response struct {
				Usage struct {
					InputTokens  int `json:"input_tokens"`
					OutputTokens int `json:"output_tokens"`
					TotalTokens  int `json:"total_tokens"`
				} `json:"usage"`
			} `json:"response"`
		}
		if err := json.Unmarshal([]byte(f.Data), &ev); err == nil &&
			(ev.Response.Usage.InputTokens > 0 || ev.Response.Usage.OutputTokens > 0) {
			s.emitted = true
			return [][]byte{
				s.chunk(map[string]any{}, s.finishReason(), nil),
				s.chunk(map[string]any{}, nil, map[string]any{
					"prompt_tokens":     ev.Response.Usage.InputTokens,
					"completion_tokens": ev.Response.Usage.OutputTokens,
					"total_tokens":      ev.Response.Usage.TotalTokens,
				}),
				sseDone(),
			}, true, nil
		}
		return s.finishChunks(), true, nil
	case "response.failed", "error":
		// 上游失败：终止流并报错（已下发的增量保留，治理链路按上游错误呈现）。
		return nil, true, fmt.Errorf("codex 上游失败: %s", truncate(f.Data, 300))
	default:
		// response.in_progress / content_part.* / output_text.done / reasoning.*
		// 等事件在 chat wire 无对应语义，忽略。
		return nil, false, nil
	}
}

// looksLikeSSE 粗判响应体是否为 SSE（上游恒流式；非流式客户端在适配器聚合）。
func looksLikeSSE(body []byte) bool {
	t := strings.TrimLeft(string(body), " \t\r\n")
	return strings.HasPrefix(t, "data:") || strings.HasPrefix(t, "event:")
}

// aggregateResponsesSSE 从 Responses SSE 流里取最终 response 对象（非流式
// 客户端的聚合路径）。取 response.completed；response.failed 返回错误。
func aggregateResponsesSSE(body []byte) ([]byte, error) {
	dec := newSSEDecoder(strings.NewReader(string(body)))
	for {
		f, ok, err := dec.next()
		if err != nil || !ok {
			return nil, fmt.Errorf("codex 上游流未给出完整响应")
		}
		switch f.Event {
		case "response.completed":
			var ev struct {
				Response json.RawMessage `json:"response"`
			}
			if err := json.Unmarshal([]byte(f.Data), &ev); err != nil || len(ev.Response) == 0 {
				return nil, fmt.Errorf("codex 上游完成事件缺 response")
			}
			return ev.Response, nil
		case "response.failed", "error":
			return nil, fmt.Errorf("codex 上游失败: %s", truncate(f.Data, 300))
		}
	}
}

// timeNowUnix 秒级时间戳（测试可注入点保留）。
func timeNowUnix() int64 { return time.Now().Unix() }
