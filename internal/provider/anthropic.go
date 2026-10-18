// anthropic.go Anthropic Messages 出站适配器（OpenAI wire ↔ Anthropic wire）。
//
// 请求：OpenAI Chat Completions JSON → POST {base}/v1/messages
//   - system 消息抽离为顶层 system；
//   - assistant.tool_calls → tool_use 块；role=tool → user 轮 tool_result 块；
//   - tools: {type:function,function:{...}} → {name,description,input_schema}；
//   - max_tokens 缺省 4096（Anthropic 必填）。
//
// 响应：content 块 → OpenAI message.content + tool_calls；stop_reason →
// finish_reason；usage.input_tokens/output_tokens → prompt/completion_tokens。
//
// 流式：Anthropic SSE 事件 → OpenAI chunk（末尾 [DONE]）。text_delta 增量下发；
// input_json_delta 作为 tool_calls.arguments 分片增量下发（OpenAI 客户端按
// index 聚合，语义一致）。
package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// anthropicDefaultMaxTokens Anthropic 请求 max_tokens 必填，缺省给 4096。
const anthropicDefaultMaxTokens = 4096

// anthropicAPIVersion Anthropic 协议版本头（缺省值；可由静态 headers 覆盖）。
const anthropicAPIVersion = "2023-06-01"

// ---------------------------------------------------------------------------
// OpenAI wire 结构（三适配器共用的输入形态）
// ---------------------------------------------------------------------------

type oaiChatRequest struct {
	Model               string          `json:"model"`
	Messages            []oaiMessage    `json:"messages"`
	Tools               []oaiTool       `json:"tools"`
	ToolChoice          json.RawMessage `json:"tool_choice"`
	MaxTokens           int             `json:"max_tokens"`
	MaxCompletionTokens int             `json:"max_completion_tokens"`
	Temperature         *float64        `json:"temperature"`
	TopP                *float64        `json:"top_p"`
	Stop                json.RawMessage `json:"stop"`
	Stream              bool            `json:"stream"`
}

type oaiMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	Name       string          `json:"name"`
	ToolCalls  []oaiToolCall   `json:"tool_calls"`
	ToolCallID string          `json:"tool_call_id"`
}

type oaiToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type oaiTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

// parseOpenAIRequest 宽松解析 OpenAI wire 请求体。
func parseOpenAIRequest(body []byte) (*oaiChatRequest, error) {
	var req oaiChatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("parse openai body: %w", err)
	}
	return &req, nil
}

// decodeOpenAIContent 把 OpenAI message.content（string / null / parts 数组）
// 还原为纯文本；parts 里的非文本块（image_url/input_audio）被忽略——跨协议
// 图片转码成本高、收益低（README 已注明能力边界）。
func decodeOpenAIContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var texts []string
	for _, p := range parts {
		if (p.Type == "" || p.Type == "text") && p.Text != "" {
			texts = append(texts, p.Text)
		}
	}
	return strings.Join(texts, "\n")
}

// openAIStopSequences 解析 stop（string 或 []string）。
func openAIStopSequences(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var one string
	if json.Unmarshal(raw, &one) == nil {
		if one == "" {
			return nil
		}
		return []string{one}
	}
	var many []string
	if json.Unmarshal(raw, &many) == nil {
		return many
	}
	return nil
}

// openAIMaxTokens max_tokens 与 max_completion_tokens 二选一取非零。
func openAIMaxTokens(req *oaiChatRequest) int {
	if req.MaxTokens > 0 {
		return req.MaxTokens
	}
	return req.MaxCompletionTokens
}

// ---------------------------------------------------------------------------
// 适配器
// ---------------------------------------------------------------------------

// anthropicAdapter Anthropic Messages 适配器（无状态）。
type anthropicAdapter struct{}

// NewAnthropicAdapter 构造 Anthropic 适配器。
func NewAnthropicAdapter() Adapter { return &anthropicAdapter{} }

// Protocol 实现 Adapter。
func (a *anthropicAdapter) Protocol() Protocol { return ProtocolAnthropic }

// BuildRequest 实现 Adapter：OpenAI wire → Anthropic Messages。
func (a *anthropicAdapter) BuildRequest(ctx context.Context, t *Target, openAIBody []byte, stream bool) (*http.Request, error) {
	oaiReq, err := parseOpenAIRequest(openAIBody)
	if err != nil {
		return nil, err
	}
	anthropicBody, err := openAIToAnthropic(oaiReq, stream)
	if err != nil {
		return nil, err
	}
	u := BuildChatURL(t, oaiReq.Model, stream)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(anthropicBody))
	if err != nil {
		return nil, fmt.Errorf("build anthropic request: %w", err)
	}
	// Anthropic 版本头是协议硬要求；用户可用静态 headers 覆盖（校验层允许）。
	ApplyHeaders(req, t, stream, map[string]string{
		"anthropic-version": anthropicAPIVersion,
	})
	return req, nil
}

// ConvertResponse 实现 Adapter：Anthropic 响应 → OpenAI 响应 / 错误信封。
func (a *anthropicAdapter) ConvertResponse(t *Target, status int, body []byte) []byte {
	if status >= 400 {
		return anthropicErrorBody(t, status, body)
	}
	var resp anthropicRespBody
	if err := json.Unmarshal(body, &resp); err != nil {
		return ErrorEnvelope(http.StatusBadGateway, "upstream_parse",
			scrubText("anthropic upstream returned unparseable body: "+truncate(string(body), 300), t))
	}
	out := map[string]any{
		"id":      firstNonEmpty(resp.ID, "chatcmpl-anthropic"),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   clientModelOf(t),
		"choices": []map[string]any{{
			"index":         0,
			"message":       anthropicContentToOAIMessage(resp.Content),
			"finish_reason": anthropicToOAIFinish(resp.StopReason),
		}},
	}
	if usage := anthropicUsage(resp.Usage); usage != nil {
		out["usage"] = usage
	}
	b, err := json.Marshal(out)
	if err != nil {
		return ErrorEnvelope(http.StatusBadGateway, "upstream_parse", "marshal anthropic response: "+err.Error())
	}
	return b
}

// ConvertStream 实现 Adapter：Anthropic SSE → OpenAI chunk SSE（增量）。
func (a *anthropicAdapter) ConvertStream(t *Target, _ string, body io.ReadCloser) (io.ReadCloser, error) {
	st := &anthropicStreamState{
		id:      "chatcmpl-" + randomSuffix(),
		model:   clientModelOf(t),
		created: time.Now().Unix(),
	}
	return newTransformReader(body, st.handle, st.finish), nil
}

// BuildModels 实现 Adapter（GET {base}/v1/models，Anthropic 亦提供该端点）。
func (a *anthropicAdapter) BuildModels(ctx context.Context, t *Target) (*http.Request, error) {
	return buildModelsGET(ctx, t, BuildModelsURL(t))
}

// DecodeModels 实现 Adapter：同时接受 OpenAI 形态 {data:[{id}]} 与
// Anthropic 形态 {data:[{id,display_name}]}（后者兼容）。
func (a *anthropicAdapter) DecodeModels(_ *Target, status int, body []byte) ([]string, error) {
	if status != http.StatusOK {
		return nil, NewHTTPError(status, "models_upstream_error", truncate(string(body), 300))
	}
	var parsed struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("parse anthropic models: %w", err)
	}
	out := make([]string, 0, len(parsed.Data))
	for _, d := range parsed.Data {
		if d.ID != "" {
			out = append(out, d.ID)
		}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// 请求转换
// ---------------------------------------------------------------------------

type anthropicReqBody struct {
	Model         string           `json:"model"`
	MaxTokens     int              `json:"max_tokens"`
	System        string           `json:"system,omitempty"`
	Messages      []map[string]any `json:"messages"`
	Tools         []map[string]any `json:"tools,omitempty"`
	ToolChoice    map[string]any   `json:"tool_choice,omitempty"`
	Temperature   *float64         `json:"temperature,omitempty"`
	TopP          *float64         `json:"top_p,omitempty"`
	StopSequences []string         `json:"stop_sequences,omitempty"`
	Stream        bool             `json:"stream,omitempty"`
}

// openAIToAnthropic OpenAI wire 请求 → Anthropic wire 请求。
func openAIToAnthropic(in *oaiChatRequest, stream bool) ([]byte, error) {
	out := anthropicReqBody{
		Model:       in.Model,
		MaxTokens:   openAIMaxTokens(in),
		Messages:    []map[string]any{},
		Temperature: in.Temperature,
		TopP:        in.TopP,
		Stream:      stream,
	}
	if out.MaxTokens <= 0 {
		out.MaxTokens = anthropicDefaultMaxTokens
	}
	if stops := openAIStopSequences(in.Stop); len(stops) > 0 {
		out.StopSequences = stops
	}

	// system 抽离：多条 system 消息按顺序拼接。
	var sysParts []string
	// tool_call_id → 函数名（tool_result 轮需要名字无关，但保留映射便于调试）。
	for _, m := range in.Messages {
		if strings.EqualFold(m.Role, "system") || strings.EqualFold(m.Role, "developer") {
			if txt := decodeOpenAIContent(m.Content); strings.TrimSpace(txt) != "" {
				sysParts = append(sysParts, txt)
			}
		}
	}
	if len(sysParts) > 0 {
		out.System = strings.Join(sysParts, "\n")
	}

	for _, m := range in.Messages {
		switch strings.ToLower(m.Role) {
		case "system", "developer":
			continue
		case "user":
			text := decodeOpenAIContent(m.Content)
			blocks := []map[string]any{}
			if text != "" {
				blocks = append(blocks, map[string]any{"type": "text", "text": text})
			}
			mergeAnthropicMessage(&out.Messages, "user", blocks)
		case "assistant":
			blocks := []map[string]any{}
			if text := decodeOpenAIContent(m.Content); text != "" {
				blocks = append(blocks, map[string]any{"type": "text", "text": text})
			}
			for _, tc := range m.ToolCalls {
				blocks = append(blocks, map[string]any{
					"type":  "tool_use",
					"id":    firstNonEmpty(tc.ID, "toolu_"+randomSuffix()),
					"name":  tc.Function.Name,
					"input": parseToolArguments(tc.Function.Arguments),
				})
			}
			mergeAnthropicMessage(&out.Messages, "assistant", blocks)
		case "tool", "function":
			// OpenAI 的 role=tool → Anthropic user 轮的 tool_result 块。
			blocks := []map[string]any{{
				"type":        "tool_result",
				"tool_use_id": m.ToolCallID,
				"content":     decodeOpenAIContent(m.Content),
			}}
			mergeAnthropicMessage(&out.Messages, "user", blocks)
		default:
			return nil, fmt.Errorf("unsupported message role %q", m.Role)
		}
	}

	// tools 声明。
	for _, tl := range in.Tools {
		if tl.Type != "" && tl.Type != "function" {
			continue // 非 function 工具（内置工具）跨协议无对应语义，跳过
		}
		schema := tl.Function.Parameters
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		out.Tools = append(out.Tools, map[string]any{
			"name":         tl.Function.Name,
			"description":  tl.Function.Description,
			"input_schema": schema,
		})
	}
	// tool_choice 映射（auto/required/none/指定函数）。
	if tc := openAIToolChoiceToAnthropic(in.ToolChoice); tc != nil {
		out.ToolChoice = tc
	}
	b, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("marshal anthropic request: %w", err)
	}
	return b, nil
}

// mergeAnthropicMessage 追加消息；相邻同角色合并 content 块。
// Anthropic 要求 user/assistant 交替，tool_result 与同轮 user 文本必须同一条。
func mergeAnthropicMessage(msgs *[]map[string]any, role string, blocks []map[string]any) {
	if len(blocks) == 0 {
		return
	}
	if n := len(*msgs); n > 0 {
		last := (*msgs)[n-1]
		if last["role"] == role {
			if prev, ok := last["content"].([]map[string]any); ok {
				last["content"] = append(prev, blocks...)
				return
			}
		}
	}
	*msgs = append(*msgs, map[string]any{"role": role, "content": blocks})
}

// parseToolArguments 把 OpenAI 的 arguments 字符串解析为 JSON 值（Anthropic
// 要求 input 是对象）；非法 JSON 包成 {"raw": "<原文>"} 以保留信息。
func parseToolArguments(args string) any {
	args = strings.TrimSpace(args)
	if args == "" {
		return map[string]any{}
	}
	var v any
	if err := json.Unmarshal([]byte(args), &v); err != nil {
		return map[string]any{"raw": args}
	}
	return v
}

// openAIToolChoiceToAnthropic OpenAI tool_choice → Anthropic tool_choice。
func openAIToolChoiceToAnthropic(raw json.RawMessage) map[string]any {
	if len(raw) == 0 {
		return nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		switch s {
		case "auto":
			return map[string]any{"type": "auto"}
		case "required":
			return map[string]any{"type": "any"}
		case "none":
			return map[string]any{"type": "none"}
		}
		return nil
	}
	var obj struct {
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if json.Unmarshal(raw, &obj) == nil && obj.Function.Name != "" {
		return map[string]any{"type": "tool", "name": obj.Function.Name}
	}
	return nil
}

// ---------------------------------------------------------------------------
// 响应转换
// ---------------------------------------------------------------------------

type anthropicRespBody struct {
	ID         string `json:"id"`
	Type       string `json:"type"`
	Role       string `json:"role"`
	Model      string `json:"model"`
	StopReason string `json:"stop_reason"`
	Content    []struct {
		Type  string          `json:"type"`
		Text  string          `json:"text"`
		ID    string          `json:"id"`
		Name  string          `json:"name"`
		Input json.RawMessage `json:"input"`
	} `json:"content"`
	Usage anthropicUsageBody `json:"usage"`
}

type anthropicUsageBody struct {
	InputTokens          int64 `json:"input_tokens"`
	OutputTokens         int64 `json:"output_tokens"`
	CacheReadInputTokens int64 `json:"cache_read_input_tokens"`
}

// anthropicContentToOAIMessage content 块 → OpenAI message。
func anthropicContentToOAIMessage(blocks []struct {
	Type  string          `json:"type"`
	Text  string          `json:"text"`
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}) map[string]any {
	var texts []string
	var toolCalls []map[string]any
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if b.Text != "" {
				texts = append(texts, b.Text)
			}
		case "tool_use":
			args := "{}"
			if len(b.Input) > 0 {
				args = string(b.Input)
			}
			toolCalls = append(toolCalls, map[string]any{
				"id":   b.ID,
				"type": "function",
				"function": map[string]any{
					"name":      b.Name,
					"arguments": args,
				},
			})
		}
	}
	msg := map[string]any{"role": "assistant"}
	if len(texts) > 0 {
		msg["content"] = strings.Join(texts, "")
	} else {
		msg["content"] = nil
	}
	if len(toolCalls) > 0 {
		msg["tool_calls"] = toolCalls
	}
	return msg
}

// anthropicUsage usage 映射（total 由 in+out 计算）。
func anthropicUsage(u anthropicUsageBody) map[string]any {
	if u.InputTokens == 0 && u.OutputTokens == 0 && u.CacheReadInputTokens == 0 {
		return nil
	}
	out := map[string]any{
		"prompt_tokens":     u.InputTokens,
		"completion_tokens": u.OutputTokens,
		"total_tokens":      u.InputTokens + u.OutputTokens,
	}
	if u.CacheReadInputTokens > 0 {
		out["prompt_tokens_details"] = map[string]any{"cached_tokens": u.CacheReadInputTokens}
	}
	return out
}

// anthropicToOAIFinish stop_reason → finish_reason。
func anthropicToOAIFinish(stop string) string {
	switch stop {
	case "tool_use":
		return "tool_calls"
	case "max_tokens":
		return "length"
	case "stop_sequence", "end_turn", "":
		return "stop"
	case "refusal":
		return "content_filter"
	default:
		return "stop"
	}
}

// anthropicErrorBody 上游错误 → OpenAI 错误信封（原文截断 + 脱敏）。
func anthropicErrorBody(t *Target, status int, body []byte) []byte {
	msg := ""
	var probe struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &probe) == nil && probe.Error.Message != "" {
		msg = probe.Error.Message
	}
	if msg == "" {
		msg = strings.TrimSpace(string(body))
	}
	if msg == "" {
		msg = fmt.Sprintf("anthropic upstream returned %d", status)
	}
	return ErrorEnvelope(status, fmt.Sprintf("upstream_%d", status), scrubText(msg, t))
}

// ---------------------------------------------------------------------------
// 流式转换
// ---------------------------------------------------------------------------

// anthropicStreamState Anthropic SSE → OpenAI chunk 的状态机。
type anthropicStreamState struct {
	id      string
	model   string
	created int64

	roleSent   bool
	toolSeq    int
	blockTool  map[int]int // Anthropic content block index → OpenAI tool_calls index
	finishSent bool
	stopReason string
	inTokens   int64
	outTokens  int64
	hasUsage   bool
}

// handle 处理一个 Anthropic SSE 事件。
func (s *anthropicStreamState) handle(f sseFrame) ([][]byte, bool, error) {
	if strings.TrimSpace(f.Data) == "" {
		return nil, false, nil
	}
	var ev struct {
		Type    string `json:"type"`
		Index   int    `json:"index"`
		Message struct {
			Usage anthropicUsageBody `json:"usage"`
		} `json:"message"`
		ContentBlock struct {
			Type string `json:"type"`
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"content_block"`
		Delta struct {
			Type        string `json:"type"`
			Text        string `json:"text"`
			PartialJSON string `json:"partial_json"`
			StopReason  string `json:"stop_reason"`
		} `json:"delta"`
		Usage anthropicUsageBody `json:"usage"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(f.Data), &ev); err != nil {
		return nil, false, nil // 非 JSON 帧（心跳/注释）忽略
	}
	if ev.Type == "" {
		ev.Type = f.Event // 兼容只给 event: 不给 type 的上游
	}
	var out [][]byte
	switch ev.Type {
	case "message_start":
		if ev.Message.Usage.InputTokens > 0 {
			s.inTokens = ev.Message.Usage.InputTokens
			s.hasUsage = true
		}
		s.roleSent = true
		out = append(out, s.chunk(map[string]any{"role": "assistant", "content": ""}, nil, nil))
	case "content_block_start":
		if ev.ContentBlock.Type == "tool_use" {
			if s.blockTool == nil {
				s.blockTool = map[int]int{}
			}
			idx := s.toolSeq
			s.toolSeq++
			s.blockTool[ev.Index] = idx
			out = append(out, s.chunk(map[string]any{
				"tool_calls": []map[string]any{{
					"index": idx,
					"id":    ev.ContentBlock.ID,
					"type":  "function",
					"function": map[string]any{
						"name":      ev.ContentBlock.Name,
						"arguments": "",
					},
				}},
			}, nil, nil))
		}
	case "content_block_delta":
		switch ev.Delta.Type {
		case "text_delta":
			if ev.Delta.Text != "" {
				out = append(out, s.chunk(map[string]any{"content": ev.Delta.Text}, nil, nil))
			}
		case "input_json_delta":
			if ev.Delta.PartialJSON != "" {
				idx := s.blockTool[ev.Index]
				out = append(out, s.chunk(map[string]any{
					"tool_calls": []map[string]any{{
						"index": idx,
						"function": map[string]any{
							"arguments": ev.Delta.PartialJSON,
						},
					}},
				}, nil, nil))
			}
		}
	case "message_delta":
		if ev.Delta.StopReason != "" {
			s.stopReason = ev.Delta.StopReason
		}
		if ev.Usage.OutputTokens > 0 {
			s.outTokens = ev.Usage.OutputTokens
			s.hasUsage = true
		}
	case "message_stop":
		out = append(out, s.finishFrames()...)
		return out, true, nil
	case "error":
		msg := ev.Error.Message
		if msg == "" {
			msg = "anthropic stream error"
		}
		out = append(out, sseData(ErrorEnvelope(http.StatusBadGateway, "upstream_stream_error", msg)))
		out = append(out, sseDone())
		return out, true, nil
	}
	return out, false, nil
}

// finish 上游 EOF 但未见 message_stop 时的收尾。
func (s *anthropicStreamState) finish() [][]byte { return s.finishFrames() }

// finishFrames 结束帧：finish_reason chunk + usage chunk + [DONE]（幂等）。
func (s *anthropicStreamState) finishFrames() [][]byte {
	if s.finishSent {
		return nil
	}
	s.finishSent = true
	var out [][]byte
	if !s.roleSent {
		// 上游连 message_start 都没给（异常流）：补首帧 role，客户端才能初始化消息。
		s.roleSent = true
		out = append(out, s.chunk(map[string]any{"role": "assistant", "content": ""}, nil, nil))
	}
	out = append(out, s.chunk(map[string]any{}, anthropicToOAIFinish(s.stopReason), nil))
	if s.hasUsage {
		out = append(out, s.usageChunk())
	}
	out = append(out, sseDone())
	return out
}

// chunk 构造一个 OpenAI chunk（finish 为 nil 时写 null）。
func (s *anthropicStreamState) chunk(delta map[string]any, finish any, usage map[string]any) []byte {
	payload := map[string]any{
		"id":      s.id,
		"object":  "chat.completion.chunk",
		"created": s.created,
		"model":   s.model,
		"choices": []map[string]any{{
			"index":         0,
			"delta":         delta,
			"finish_reason": finish,
		}},
	}
	if usage != nil {
		payload["usage"] = usage
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return nil
	}
	return sseData(b)
}

// usageChunk 纯 usage 帧（choices 为空数组，OpenAI stream_options.include_usage 形态）。
func (s *anthropicStreamState) usageChunk() []byte {
	payload := map[string]any{
		"id":      s.id,
		"object":  "chat.completion.chunk",
		"created": s.created,
		"model":   s.model,
		"choices": []any{},
		"usage": map[string]any{
			"prompt_tokens":     s.inTokens,
			"completion_tokens": s.outTokens,
			"total_tokens":      s.inTokens + s.outTokens,
		},
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return nil
	}
	return sseData(b)
}

// ---------------------------------------------------------------------------
// 小工具（三适配器共用）
// ---------------------------------------------------------------------------

// clientModelOf 客户端可见模型名（Target.ClientModel 为空时回落到渠道名）。
func clientModelOf(t *Target) string {
	if t == nil {
		return ""
	}
	if t.ClientModel != "" {
		return t.ClientModel
	}
	return "ext:" + t.Name
}

// scrubText 上游原文脱敏（秘钥字面量 + URL query）。
func scrubText(s string, t *Target) string {
	if t == nil {
		return ScrubSecrets(truncate(s, 2000))
	}
	return ScrubSecrets(truncate(s, 2000), t.Secret)
}

// firstNonEmpty 返回第一个非空串。
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// randomSuffix 生成短随机后缀（id 用；不用于安全场景）。
func randomSuffix() string {
	return fmt.Sprintf("%d", time.Now().UnixNano()%1e12)
}
