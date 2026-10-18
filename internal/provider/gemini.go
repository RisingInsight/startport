// gemini.go Google Gemini（Generative Language）出站适配器。
//
// 请求：OpenAI wire → POST {base}/v1beta/models/{model}:generateContent
//
//	流式：:streamGenerateContent?alt=sse
//	- system 消息 → systemInstruction.parts；
//	- assistant → role:"model"；tool → role:"user" + functionResponse（Gemini 用
//	  函数名关联，故需从上一轮 tool_calls 的 id→name 映射里取名字）；
//	- tools → [{functionDeclarations:[{name,description,parameters}]}]；
//	- temperature/top_p/max_tokens/stop → generationConfig。
//
// 响应：candidates[0].content.parts → OpenAI message.content + tool_calls；
// finishReason → finish_reason；usageMetadata → prompt/completion/total tokens。
//
// 流式的硬前提：上游必须按 ?alt=sse 返回 text/event-stream。若上游返回 JSON
// （忽略 alt=sse），**明确报 ErrStreamUnsupported**，绝不把整包 JSON 伪装成
// 流式（伪流式会让客户端 TTFT/吞吐观测全错，并掩盖"云端不支持"这个事实）。
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

// geminiAdapter Gemini 适配器（无状态）。
type geminiAdapter struct{}

// NewGeminiAdapter 构造 Gemini 适配器。
func NewGeminiAdapter() Adapter { return &geminiAdapter{} }

// Protocol 实现 Adapter。
func (a *geminiAdapter) Protocol() Protocol { return ProtocolGemini }

// BuildRequest 实现 Adapter：OpenAI wire → Gemini generateContent。
func (a *geminiAdapter) BuildRequest(ctx context.Context, t *Target, openAIBody []byte, stream bool) (*http.Request, error) {
	oaiReq, err := parseOpenAIRequest(openAIBody)
	if err != nil {
		return nil, err
	}
	body, err := openAIToGemini(oaiReq)
	if err != nil {
		return nil, err
	}
	u := BuildChatURL(t, oaiReq.Model, stream)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build gemini request: %w", err)
	}
	ApplyHeaders(req, t, stream, nil)
	return req, nil
}

// ConvertResponse 实现 Adapter：Gemini 响应 → OpenAI 响应 / 错误信封。
func (a *geminiAdapter) ConvertResponse(t *Target, status int, body []byte) []byte {
	if status >= 400 {
		return geminiErrorBody(t, status, body)
	}
	var resp geminiRespBody
	if err := json.Unmarshal(body, &resp); err != nil {
		return ErrorEnvelope(http.StatusBadGateway, "upstream_parse",
			scrubText("gemini upstream returned unparseable body: "+truncate(string(body), 300), t))
	}
	out := geminiToOpenAIResponse(t, &resp)
	b, err := json.Marshal(out)
	if err != nil {
		return ErrorEnvelope(http.StatusBadGateway, "upstream_parse", "marshal gemini response: "+err.Error())
	}
	return b
}

// ConvertStream 实现 Adapter：Gemini SSE → OpenAI chunk SSE（增量）。
// 上游 Content-Type 非 SSE 时返回 ErrStreamUnsupported（由调用方翻 502）。
func (a *geminiAdapter) ConvertStream(t *Target, contentType string, body io.ReadCloser) (io.ReadCloser, error) {
	if DetectStreamUnsupported(contentType) {
		_ = body.Close()
		return nil, ErrStreamUnsupported
	}
	st := &geminiStreamState{
		id:      "chatcmpl-" + randomSuffix(),
		model:   clientModelOf(t),
		created: time.Now().Unix(),
	}
	return newTransformReader(body, st.handle, st.finish), nil
}

// BuildModels 实现 Adapter（GET {base}/v1beta/models）。
func (a *geminiAdapter) BuildModels(ctx context.Context, t *Target) (*http.Request, error) {
	return buildModelsGET(ctx, t, BuildModelsURL(t))
}

// DecodeModels 实现 Adapter：{"models":[{"name":"models/gemini-1.5-pro",...}]}。
// 只取声明支持 generateContent 的条目（目录里还混有 embedding 模型）。
func (a *geminiAdapter) DecodeModels(_ *Target, status int, body []byte) ([]string, error) {
	if status != http.StatusOK {
		return nil, NewHTTPError(status, "models_upstream_error", truncate(string(body), 300))
	}
	var parsed struct {
		Models []struct {
			Name                       string   `json:"name"`
			SupportedGenerationMethods []string `json:"supportedGenerationMethods"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("parse gemini models: %w", err)
	}
	out := make([]string, 0, len(parsed.Models))
	for _, m := range parsed.Models {
		id := strings.TrimPrefix(m.Name, "models/")
		if id == "" {
			continue
		}
		if len(m.SupportedGenerationMethods) > 0 && !containsString(m.SupportedGenerationMethods, "generateContent") {
			continue
		}
		out = append(out, id)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// 请求转换
// ---------------------------------------------------------------------------

type geminiRequestBody struct {
	SystemInstruction *geminiContent  `json:"systemInstruction,omitempty"`
	Contents          []geminiContent `json:"contents"`
	Tools             []geminiTool    `json:"tools,omitempty"`
	ToolConfig        map[string]any  `json:"toolConfig,omitempty"`
	GenerationConfig  map[string]any  `json:"generationConfig,omitempty"`
}

type geminiContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []geminiPart `json:"parts"`
}

type geminiPart struct {
	Text             string                  `json:"text,omitempty"`
	FunctionCall     *geminiFunctionCall     `json:"functionCall,omitempty"`
	FunctionResponse *geminiFunctionResponse `json:"functionResponse,omitempty"`
}

type geminiFunctionCall struct {
	Name string          `json:"name"`
	Args json.RawMessage `json:"args,omitempty"`
}

type geminiFunctionResponse struct {
	Name     string          `json:"name"`
	Response json.RawMessage `json:"response"`
}

type geminiTool struct {
	FunctionDeclarations []map[string]any `json:"functionDeclarations"`
}

// openAIToGemini OpenAI wire 请求 → Gemini generateContent 请求。
func openAIToGemini(in *oaiChatRequest) ([]byte, error) {
	out := geminiRequestBody{Contents: []geminiContent{}}

	// system 消息 → systemInstruction（多条拼接）。
	var sysParts []string
	for _, m := range in.Messages {
		if strings.EqualFold(m.Role, "system") || strings.EqualFold(m.Role, "developer") {
			if txt := decodeOpenAIContent(m.Content); strings.TrimSpace(txt) != "" {
				sysParts = append(sysParts, txt)
			}
		}
	}
	if len(sysParts) > 0 {
		out.SystemInstruction = &geminiContent{
			Parts: []geminiPart{{Text: strings.Join(sysParts, "\n")}},
		}
	}

	// tool_call_id → 函数名（Gemini 的 functionResponse 用名字关联，不用 id）。
	callNames := map[string]string{}
	for _, m := range in.Messages {
		for _, tc := range m.ToolCalls {
			if tc.ID != "" && tc.Function.Name != "" {
				callNames[tc.ID] = tc.Function.Name
			}
		}
	}

	for _, m := range in.Messages {
		switch strings.ToLower(m.Role) {
		case "system", "developer":
			continue
		case "user":
			text := decodeOpenAIContent(m.Content)
			parts := []geminiPart{}
			if text != "" {
				parts = append(parts, geminiPart{Text: text})
			}
			appendGeminiContent(&out.Contents, "user", parts)
		case "assistant":
			parts := []geminiPart{}
			if text := decodeOpenAIContent(m.Content); text != "" {
				parts = append(parts, geminiPart{Text: text})
			}
			for _, tc := range m.ToolCalls {
				args := json.RawMessage(tc.Function.Arguments)
				if len(args) == 0 || !json.Valid(args) {
					args = json.RawMessage(`{}`)
				}
				parts = append(parts, geminiPart{FunctionCall: &geminiFunctionCall{
					Name: tc.Function.Name,
					Args: args,
				}})
			}
			appendGeminiContent(&out.Contents, "model", parts)
		case "tool", "function":
			name := callNames[m.ToolCallID]
			if name == "" {
				name = m.Name
			}
			if name == "" {
				name = "tool"
			}
			content := decodeOpenAIContent(m.Content)
			resp := json.RawMessage(content)
			if !json.Valid(resp) || len(resp) == 0 {
				// Gemini 要求 response 是对象：非 JSON 结果包一层 {"result": "..."}。
				b, _ := json.Marshal(map[string]any{"result": content})
				resp = b
			} else if t := bytes.TrimSpace(resp); len(t) == 0 || t[0] != '{' {
				b, _ := json.Marshal(map[string]any{"result": json.RawMessage(resp)})
				resp = b
			}
			appendGeminiContent(&out.Contents, "user", []geminiPart{{
				FunctionResponse: &geminiFunctionResponse{Name: name, Response: resp},
			}})
		default:
			return nil, fmt.Errorf("unsupported message role %q", m.Role)
		}
	}

	// tools 声明。
	var decls []map[string]any
	for _, tl := range in.Tools {
		if tl.Type != "" && tl.Type != "function" {
			continue
		}
		schema := tl.Function.Parameters
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		decls = append(decls, map[string]any{
			"name":        tl.Function.Name,
			"description": tl.Function.Description,
			"parameters":  schema,
		})
	}
	if len(decls) > 0 {
		out.Tools = []geminiTool{{FunctionDeclarations: decls}}
	}
	if tc := openAIToolChoiceToGemini(in.ToolChoice); tc != nil {
		out.ToolConfig = tc
	}

	// generationConfig（零值字段不写，避免上游按 0 处理）。
	gen := map[string]any{}
	if in.Temperature != nil {
		gen["temperature"] = *in.Temperature
	}
	if in.TopP != nil {
		gen["topP"] = *in.TopP
	}
	if mt := openAIMaxTokens(in); mt > 0 {
		gen["maxOutputTokens"] = mt
	}
	if stops := openAIStopSequences(in.Stop); len(stops) > 0 {
		gen["stopSequences"] = stops
	}
	if len(gen) > 0 {
		out.GenerationConfig = gen
	}

	b, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("marshal gemini request: %w", err)
	}
	return b, nil
}

// appendGeminiContent 追加 contents；相邻同角色合并 parts（Gemini 容忍同角色
// 连续条目，但合并后语义更接近"一轮对话"，也避免工具结果被拆成多轮）。
func appendGeminiContent(contents *[]geminiContent, role string, parts []geminiPart) {
	if len(parts) == 0 {
		return
	}
	if n := len(*contents); n > 0 {
		last := (*contents)[n-1]
		if last.Role == role {
			(*contents)[n-1].Parts = append(last.Parts, parts...)
			return
		}
	}
	*contents = append(*contents, geminiContent{Role: role, Parts: parts})
}

// openAIToolChoiceToGemini tool_choice → toolConfig.functionCallingConfig。
func openAIToolChoiceToGemini(raw json.RawMessage) map[string]any {
	if len(raw) == 0 {
		return nil
	}
	mode := ""
	allowed := []string(nil)
	var s string
	if json.Unmarshal(raw, &s) == nil {
		switch s {
		case "auto":
			mode = "AUTO"
		case "required":
			mode = "ANY"
		case "none":
			mode = "NONE"
		}
	} else {
		var obj struct {
			Type     string `json:"type"`
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		}
		if json.Unmarshal(raw, &obj) == nil && obj.Function.Name != "" {
			mode = "ANY"
			allowed = []string{obj.Function.Name}
		}
	}
	if mode == "" {
		return nil
	}
	cfg := map[string]any{"mode": mode}
	if len(allowed) > 0 {
		cfg["allowedFunctionNames"] = allowed
	}
	return map[string]any{"functionCallingConfig": cfg}
}

// ---------------------------------------------------------------------------
// 响应转换
// ---------------------------------------------------------------------------

type geminiRespBody struct {
	Candidates []struct {
		Content      geminiRespContent `json:"content"`
		FinishReason string            `json:"finishReason"`
	} `json:"candidates"`
	UsageMetadata *struct {
		PromptTokenCount     int64 `json:"promptTokenCount"`
		CandidatesTokenCount int64 `json:"candidatesTokenCount"`
		TotalTokenCount      int64 `json:"totalTokenCount"`
	} `json:"usageMetadata"`
	PromptFeedback *struct {
		BlockReason string `json:"blockReason"`
	} `json:"promptFeedback"`
}

type geminiRespContent struct {
	Role  string           `json:"role"`
	Parts []geminiRespPart `json:"parts"`
}

type geminiRespPart struct {
	Text         string          `json:"text"`
	Thought      bool            `json:"thought"`
	FunctionCall *geminiFunction `json:"functionCall"`
}

type geminiFunction struct {
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
}

// geminiToOpenAIResponse 非流式响应折回 OpenAI 形态。
func geminiToOpenAIResponse(t *Target, resp *geminiRespBody) map[string]any {
	msg := map[string]any{"role": "assistant"}
	var texts []string
	var toolCalls []map[string]any
	finish := ""
	if len(resp.Candidates) > 0 {
		cand := resp.Candidates[0]
		finish = cand.FinishReason
		seq := 0
		for _, p := range cand.Content.Parts {
			if p.Thought {
				continue // 思维链不外泄为正文（OpenAI 侧无对应字段）
			}
			if p.Text != "" {
				texts = append(texts, p.Text)
			}
			if p.FunctionCall != nil {
				args := "{}"
				if len(p.FunctionCall.Args) > 0 {
					args = string(p.FunctionCall.Args)
				}
				toolCalls = append(toolCalls, map[string]any{
					"id":   fmt.Sprintf("call_%d", seq),
					"type": "function",
					"function": map[string]any{
						"name":      p.FunctionCall.Name,
						"arguments": args,
					},
				})
				seq++
			}
		}
	}
	if len(texts) > 0 {
		msg["content"] = strings.Join(texts, "")
	} else {
		msg["content"] = nil
	}
	if len(toolCalls) > 0 {
		msg["tool_calls"] = toolCalls
	}
	out := map[string]any{
		"id":      "chatcmpl-" + randomSuffix(),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   clientModelOf(t),
		"choices": []map[string]any{{
			"index":         0,
			"message":       msg,
			"finish_reason": geminiToOAIFinish(finish, len(toolCalls) > 0),
		}},
	}
	if u := geminiUsage(resp.UsageMetadata); u != nil {
		out["usage"] = u
	}
	return out
}

// geminiToOAIFinish finishReason(+是否有工具调用) → finish_reason。
func geminiToOAIFinish(reason string, hasToolCalls bool) string {
	switch reason {
	case "MAX_TOKENS":
		return "length"
	case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII", "IMAGE_SAFETY":
		return "content_filter"
	case "", "STOP", "FINISH_REASON_UNSPECIFIED", "OTHER", "MALFORMED_FUNCTION_CALL":
		if hasToolCalls {
			return "tool_calls"
		}
		return "stop"
	default:
		if hasToolCalls {
			return "tool_calls"
		}
		return "stop"
	}
}

// geminiUsage usageMetadata → OpenAI usage。
func geminiUsage(u *struct {
	PromptTokenCount     int64 `json:"promptTokenCount"`
	CandidatesTokenCount int64 `json:"candidatesTokenCount"`
	TotalTokenCount      int64 `json:"totalTokenCount"`
}) map[string]any {
	if u == nil {
		return nil
	}
	total := u.TotalTokenCount
	if total == 0 {
		total = u.PromptTokenCount + u.CandidatesTokenCount
	}
	if u.PromptTokenCount == 0 && u.CandidatesTokenCount == 0 && total == 0 {
		return nil
	}
	return map[string]any{
		"prompt_tokens":     u.PromptTokenCount,
		"completion_tokens": u.CandidatesTokenCount,
		"total_tokens":      total,
	}
}

// geminiErrorBody 上游错误 → OpenAI 错误信封（Gemini 错误形如
// {"error":{"code":400,"message":"...","status":"INVALID_ARGUMENT"}}）。
func geminiErrorBody(t *Target, status int, body []byte) []byte {
	msg := ""
	var probe struct {
		Error struct {
			Message string `json:"message"`
			Status  string `json:"status"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &probe) == nil && probe.Error.Message != "" {
		msg = probe.Error.Message
	}
	if msg == "" {
		msg = strings.TrimSpace(string(body))
	}
	if msg == "" {
		msg = fmt.Sprintf("gemini upstream returned %d", status)
	}
	return ErrorEnvelope(status, fmt.Sprintf("upstream_%d", status), scrubText(msg, t))
}

// ---------------------------------------------------------------------------
// 流式转换
// ---------------------------------------------------------------------------

// geminiStreamState Gemini SSE → OpenAI chunk 的状态机。
// Gemini 的 functionCall 是整块下发（无参数分片），故每个 functionCall 一次性
// 作为一条 tool_calls delta 发出——OpenAI 客户端按 index 累积，语义等价。
type geminiStreamState struct {
	id      string
	model   string
	created int64

	roleSent   bool
	toolSeq    int
	finishSent bool
	finishWhy  string
	hasTools   bool
	inTokens   int64
	outTokens  int64
	totalTok   int64
	hasUsage   bool
}

// handle 处理一个 Gemini SSE 帧。
func (s *geminiStreamState) handle(f sseFrame) ([][]byte, bool, error) {
	if strings.TrimSpace(f.Data) == "" {
		return nil, false, nil
	}
	if f.Data == "[DONE]" {
		return s.finishFrames(), true, nil
	}
	var frame geminiRespBody
	if err := json.Unmarshal([]byte(f.Data), &frame); err != nil {
		return nil, false, nil // 非 JSON 帧忽略（不因单个坏帧中断整条流）
	}
	var out [][]byte
	if !s.roleSent {
		s.roleSent = true
		out = append(out, s.chunk(map[string]any{"role": "assistant", "content": ""}))
	}
	if len(frame.Candidates) > 0 {
		cand := frame.Candidates[0]
		for _, p := range cand.Content.Parts {
			if p.Thought {
				continue
			}
			if p.Text != "" {
				out = append(out, s.chunk(map[string]any{"content": p.Text}))
			}
			if p.FunctionCall != nil {
				args := "{}"
				if len(p.FunctionCall.Args) > 0 {
					args = string(p.FunctionCall.Args)
				}
				idx := s.toolSeq
				s.toolSeq++
				s.hasTools = true
				out = append(out, s.chunk(map[string]any{
					"tool_calls": []map[string]any{{
						"index": idx,
						"id":    fmt.Sprintf("call_%d", idx),
						"type":  "function",
						"function": map[string]any{
							"name":      p.FunctionCall.Name,
							"arguments": args,
						},
					}},
				}))
			}
		}
		if cand.FinishReason != "" {
			s.finishWhy = cand.FinishReason
		}
	}
	if frame.PromptFeedback != nil && frame.PromptFeedback.BlockReason != "" && s.finishWhy == "" {
		s.finishWhy = frame.PromptFeedback.BlockReason
	}
	if u := frame.UsageMetadata; u != nil {
		if u.PromptTokenCount > 0 {
			s.inTokens = u.PromptTokenCount
			s.hasUsage = true
		}
		if u.CandidatesTokenCount > 0 {
			s.outTokens = u.CandidatesTokenCount
			s.hasUsage = true
		}
		if u.TotalTokenCount > 0 {
			s.totalTok = u.TotalTokenCount
			s.hasUsage = true
		}
	}
	return out, false, nil
}

// finish 上游 EOF 时的收尾帧（幂等）。
func (s *geminiStreamState) finish() [][]byte { return s.finishFrames() }

// finishFrames finish chunk + usage chunk + [DONE]。
func (s *geminiStreamState) finishFrames() [][]byte {
	if s.finishSent {
		return nil
	}
	s.finishSent = true
	var out [][]byte
	if !s.roleSent {
		s.roleSent = true
		out = append(out, s.chunk(map[string]any{"role": "assistant", "content": ""}))
	}
	out = append(out, s.finishChunk())
	if s.hasUsage {
		out = append(out, s.usageChunk())
	}
	out = append(out, sseDone())
	return out
}

// chunk 构造一个带 choices 的 OpenAI chunk。
func (s *geminiStreamState) chunk(delta map[string]any) []byte {
	payload := map[string]any{
		"id":      s.id,
		"object":  "chat.completion.chunk",
		"created": s.created,
		"model":   s.model,
		"choices": []map[string]any{{
			"index":         0,
			"delta":         delta,
			"finish_reason": nil,
		}},
	}
	return marshalSSE(payload)
}

// finishChunk 带 finish_reason 的终止 chunk。
func (s *geminiStreamState) finishChunk() []byte {
	payload := map[string]any{
		"id":      s.id,
		"object":  "chat.completion.chunk",
		"created": s.created,
		"model":   s.model,
		"choices": []map[string]any{{
			"index":         0,
			"delta":         map[string]any{},
			"finish_reason": geminiToOAIFinish(s.finishWhy, s.hasTools),
		}},
	}
	return marshalSSE(payload)
}

// usageChunk 纯 usage 帧。
func (s *geminiStreamState) usageChunk() []byte {
	total := s.totalTok
	if total == 0 {
		total = s.inTokens + s.outTokens
	}
	payload := map[string]any{
		"id":      s.id,
		"object":  "chat.completion.chunk",
		"created": s.created,
		"model":   s.model,
		"choices": []any{},
		"usage": map[string]any{
			"prompt_tokens":     s.inTokens,
			"completion_tokens": s.outTokens,
			"total_tokens":      total,
		},
	}
	return marshalSSE(payload)
}

// marshalSSE map → SSE data 帧（序列化失败返回 nil）。
func marshalSSE(payload map[string]any) []byte {
	b, err := json.Marshal(payload)
	if err != nil {
		return nil
	}
	return sseData(b)
}

// containsString 切片包含判定。
func containsString(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
