// traestream.go TRAE 自有 SSE → OpenAI chunk / chat.completion。
//
// 这条流与 OpenAI 的三处硬差异：
//  1. 没有 [DONE]：结束帧是 event: done，[DONE] 由网关补；
//  2. 业务错误是 HTTP 200 里的 event: error；首帧错误必须在写任何字节前
//     转成 *HTTPError（extchan.Do 才能换证/判 401/429）；
//  3. token_usage 是"欠"给下一帧的：读到后不立即发，随 done 帧一起走。
package provider

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// traeAggregate 把整段 SSE 折成 chat.completion（非流式请求的唯一路径）。
func traeAggregate(body []byte, model string) ([]byte, error) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) > 0 && trimmed[0] == '{' &&
		!bytes.Contains(trimmed, []byte("event:")) && !bytes.Contains(trimmed, []byte("data:")) {
		// 200 + JSON 错误信封（不是 SSE）：显式报错，不伪装成空回答。
		return nil, NewHTTPError(http.StatusBadGateway, "trae_bad_stream",
			"Trae 上游返回了 JSON 而不是 SSE，无法聚合")
	}
	dec := newSSEDecoder(bytes.NewReader(body))
	var (
		content       strings.Builder
		reasoning     strings.Builder
		finishReason  = "stop"
		usage         map[string]any
		calls         []any
		upstreamError *HTTPError
	)
	for {
		f, ok, err := dec.next()
		if err != nil {
			return nil, err
		}
		if !ok {
			break
		}
		switch f.Event {
		case "output":
			resp, thought, toolCalls := traeParseOutput(f.Data)
			content.WriteString(resp)
			reasoning.WriteString(thought)
			if toolCalls != nil {
				traeMergeToolCalls(&calls, toolCalls)
			}
		case "token_usage":
			if u := traeParseUsage(f.Data); u != nil {
				usage = u
			}
		case "done":
			if reason := traeParseFinishReason(f.Data); reason != "" {
				finishReason = reason
			}
		case "error":
			code, message := traeParseErrorEvent(f.Data)
			upstreamError = traeStreamHTTPError(code, message)
		}
	}
	if upstreamError != nil {
		return nil, upstreamError
	}
	message := map[string]any{"role": "assistant", "content": content.String()}
	if reasoning.Len() > 0 {
		message["reasoning_content"] = reasoning.String()
	}
	if len(calls) > 0 {
		message["tool_calls"] = calls
	}
	now := time.Now()
	resp := map[string]any{
		"id":      traeResponseID(now),
		"object":  "chat.completion",
		"created": now.Unix(),
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       message,
			"finish_reason": finishReason,
		}},
	}
	if usage != nil {
		resp["usage"] = usage
	}
	raw, err := json.Marshal(resp)
	if err != nil {
		return nil, fmt.Errorf("marshal trae completion: %w", err)
	}
	return raw, nil
}

// traeStream 一条已完成首帧预读的流。
type traeStream struct {
	src     io.ReadCloser
	dec     *sseDecoder
	buf     bytes.Buffer
	id      string
	created int64
	model   string

	pendingUsage map[string]any
	sawDone      bool
	done         bool
}

// newTraeStream 预读首帧：首帧是 error → 返回 typed error（此时未向客户端
// 写任何字节，extchan.Do 可以换证）；首帧是内容 → 返回可读流。
func newTraeStream(t *Target, body io.ReadCloser) (io.ReadCloser, error) {
	now := time.Now()
	s := &traeStream{
		src:     body,
		dec:     newSSEDecoder(body),
		id:      traeResponseID(now),
		created: now.Unix(),
		model:   clientModelOf(t),
	}
	for {
		f, ok, err := s.dec.next()
		if err != nil {
			_ = body.Close()
			return nil, err
		}
		if !ok {
			return s, nil // 空流：Read 时补 [DONE]
		}
		switch f.Event {
		case "error":
			code, message := traeParseErrorEvent(f.Data)
			_ = body.Close()
			return nil, traeStreamHTTPError(code, message)
		case "output":
			resp, thought, toolCalls := traeParseOutput(f.Data)
			if resp == "" && thought == "" && toolCalls == nil {
				continue // 空内容帧：继续等到有实际内容或 done
			}
			s.emitOutput(resp, thought, toolCalls)
			return s, nil
		case "token_usage":
			if u := traeParseUsage(f.Data); u != nil {
				s.pendingUsage = u
			}
		case "done":
			s.emitDone(traeParseFinishReason(f.Data))
			return s, nil
		}
	}
}

func (s *traeStream) Read(p []byte) (int, error) {
	for s.buf.Len() == 0 && !s.done {
		f, ok, err := s.dec.next()
		if err != nil {
			return 0, err
		}
		if !ok {
			s.finish()
			break
		}
		s.handleFrame(f)
	}
	if s.buf.Len() > 0 {
		return s.buf.Read(p)
	}
	return 0, io.EOF
}

func (s *traeStream) Close() error {
	if s.src == nil {
		return nil
	}
	return s.src.Close()
}

func (s *traeStream) handleFrame(f sseFrame) {
	switch f.Event {
	case "output":
		resp, thought, toolCalls := traeParseOutput(f.Data)
		s.emitOutput(resp, thought, toolCalls)
	case "token_usage":
		if u := traeParseUsage(f.Data); u != nil {
			s.pendingUsage = u
		}
	case "done":
		s.emitDone(traeParseFinishReason(f.Data))
	case "error":
		// 内容已经下发过：只能在流里如实报错，不能改 HTTP 状态。
		code, message := traeParseErrorEvent(f.Data)
		he := traeStreamHTTPError(code, message)
		payload, _ := json.Marshal(map[string]any{"error": map[string]any{
			"message": he.Message, "type": "upstream_error", "code": he.Code,
		}})
		s.buf.Write(sseData(payload))
		s.buf.Write(sseDone())
		s.sawDone = true
		s.done = true
	}
}

func (s *traeStream) emitOutput(resp, thought string, toolCalls any) {
	delta := map[string]any{}
	if resp != "" {
		delta["content"] = resp
	}
	if thought != "" {
		delta["reasoning_content"] = thought
	}
	if toolCalls != nil {
		delta["tool_calls"] = traeNormalizeToolCalls(toolCalls)
	}
	if len(delta) == 0 {
		return
	}
	s.writeChunk(delta, "", nil)
}

func (s *traeStream) emitDone(reason string) {
	if reason == "" {
		reason = "stop"
	}
	s.writeChunk(map[string]any{}, reason, s.pendingUsage)
	s.pendingUsage = nil
	s.buf.Write(sseDone())
	s.sawDone = true
	s.done = true
}

func (s *traeStream) finish() {
	if s.sawDone {
		s.done = true
		return
	}
	s.buf.Write(sseDone())
	s.sawDone = true
	s.done = true
}

func (s *traeStream) writeChunk(delta map[string]any, finish string, usage map[string]any) {
	choice := map[string]any{"index": 0, "delta": delta}
	if finish != "" {
		choice["finish_reason"] = finish
	}
	chunk := map[string]any{
		"id":      s.id,
		"object":  "chat.completion.chunk",
		"created": s.created,
		"model":   s.model,
		"choices": []any{choice},
	}
	if usage != nil {
		chunk["usage"] = usage
	}
	raw, err := json.Marshal(chunk)
	if err != nil {
		return
	}
	s.buf.Write(sseData(raw))
}

// ── 事件解析 ─────────────────────────────────────────────────

func traeParseOutput(data string) (resp, reasoning string, toolCalls any) {
	var m map[string]any
	if json.Unmarshal([]byte(data), &m) != nil {
		return "", "", nil
	}
	resp = traeString(m["response"])
	reasoning = traeString(m["reasoning_content"])
	if tc, ok := m["tool_calls"]; ok && tc != nil {
		toolCalls = tc
	}
	return resp, reasoning, toolCalls
}

func traeParseUsage(data string) map[string]any {
	var m map[string]any
	if json.Unmarshal([]byte(data), &m) != nil {
		return nil
	}
	return m
}

func traeParseFinishReason(data string) string {
	var m map[string]any
	if json.Unmarshal([]byte(data), &m) != nil {
		return ""
	}
	return traeString(m["finish_reason"])
}

func traeParseErrorEvent(data string) (int64, string) {
	var m map[string]any
	if json.Unmarshal([]byte(data), &m) != nil {
		return 0, truncate(data, 300)
	}
	var code int64
	switch v := m["code"].(type) {
	case float64:
		code = int64(v)
	case json.Number:
		code, _ = v.Int64()
	case string:
		code, _ = strconv.ParseInt(strings.TrimSpace(v), 10, 64)
	}
	// 上游只认 message；写成 msg 会被静默读空（参考实现同款）。
	return code, traeString(m["message"])
}

func traeNormalizeToolCalls(v any) any {
	list, ok := v.([]any)
	if !ok {
		list = []any{v}
	}
	out := make([]any, 0, len(list))
	for _, item := range list {
		call, ok := item.(map[string]any)
		if !ok {
			out = append(out, item)
			continue
		}
		if fn, ok := call["function_call"]; ok {
			call["function"] = fn
			delete(call, "function_call")
		}
		if fn, ok := call["function"].(map[string]any); ok {
			delete(fn, "namespace")
			delete(fn, "partial_arguments")
			call["function"] = fn
		}
		out = append(out, call)
	}
	return out
}

// traeMergeToolCalls 非流式聚合：按 index 合并，arguments 拼接。
func traeMergeToolCalls(dst *[]any, incoming any) {
	list, ok := incoming.([]any)
	if !ok {
		list = []any{incoming}
	}
	for _, item := range list {
		call, ok := item.(map[string]any)
		if !ok {
			continue
		}
		index := int64(0)
		switch v := call["index"].(type) {
		case float64:
			index = int64(v)
		case json.Number:
			index, _ = v.Int64()
		}
		var slot map[string]any
		for _, existing := range *dst {
			em, _ := existing.(map[string]any)
			ei := int64(0)
			switch v := em["index"].(type) {
			case float64:
				ei = int64(v)
			case json.Number:
				ei, _ = v.Int64()
			}
			if ei == index {
				slot = em
				break
			}
		}
		if slot == nil {
			slot = map[string]any{"index": index}
			*dst = append(*dst, slot)
		}
		if id := traeString(call["id"]); id != "" {
			slot["id"] = id
		}
		if typ := traeString(call["type"]); typ != "" {
			slot["type"] = typ
		}
		fn, _ := call["function"].(map[string]any)
		if fn == nil {
			fn, _ = call["function_call"].(map[string]any)
		}
		if fn == nil {
			continue
		}
		target, _ := slot["function"].(map[string]any)
		if target == nil {
			target = map[string]any{}
			slot["function"] = target
		}
		if name := traeString(fn["name"]); name != "" {
			target["name"] = name
		}
		if args := traeString(fn["arguments"]); args != "" {
			prev, _ := target["arguments"].(string)
			target["arguments"] = prev + args
		}
	}
}

func traeResponseID(now time.Time) string {
	return fmt.Sprintf("chatcmpl-%d", now.UnixNano())
}
