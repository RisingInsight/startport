// qoderprotocol.go Qoder 协议转换：OpenAI Chat Completions ↔ Qoder 上游信封
// （v1.5.0；移植依据 `C:/tmp/ref/qoder/protocol.rs`）。
//
// 三个容易踩的点（都是上游实际约束，protocol.rs:13-27）：
//  1. **system 提示必须放进 messages 里**：上游顶层 `system` 字段无效，且要
//     排在消息序列最前面；
//  2. **`model_config` 必须剥掉 `thinking_config`**：原样回传会覆盖
//     `parameters.enable_thinking`，导致"关闭思考"失效；
//  3. **"关闭思考"降级成"不指定"**：Qwen3.8 系列传 enable_thinking=false 会
//     把思考混进正文甚至直接断连——发"不指定"让上游走默认行为。
package provider

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// 上游要求的会话常量（protocol.rs:34-38）。
const (
	qoderSessionType = "qodercli"
	qoderAgentID     = "agent_common"
	qoderChatTask    = "FREE_INPUT"
)

// qoderMaxOutputTokens 输出上限（models.rs:62）：关闭思考时仍输出思考内容、
// 约 49K 起返回空内容甚至断连——32K 是上游实际约束，不是保守取值。
const qoderMaxOutputTokens = 32768

// qoderModel 静态目录条目（协议层需要的最小集）。
type qoderModel struct {
	ID        string
	Key       string   // upstreamKey（X-Model-Key / body model_config.key）
	Reasoning bool     // 是否推理模型（决定要不要启用标签拆解器）
	Efforts   []string // 声明的思考档位池（空 = 保守三档 low/medium/xhigh）
	Vision    bool
}

// resolveQoderModel 客户端模型名 → 条目（models.rs:352-386）。
// 匹配顺序：本地区清单先查、另一地区兜底；清单内按 id → name → upstreamKey，
// 全部 trim + 忽略 ASCII 大小写（与目录校验同口径，防"目录放行、路由层却
// 静默回落"）。未命中返回 false——**硬失败 model_not_found**，不静默回落
// （设计 §9.5）。
func resolveQoderModel(intl bool, modelID string) (qoderModel, bool) {
	name := strings.ToLower(strings.TrimSpace(modelID))
	if name == "" {
		return qoderModel{}, false
	}
	tables := [][]qoderModel{qoderFallbackModels(intl), qoderFallbackModels(!intl)}
	for _, table := range tables {
		for _, m := range table {
			for _, candidate := range []string{m.ID, m.Key} {
				if strings.EqualFold(strings.TrimSpace(candidate), name) {
					return m, true
				}
			}
		}
	}
	return qoderModel{}, false
}

// qoderFallbackModels 内置兜底清单（models.rs:77-117；price_factor 快照
// 2026-09-20，行序即展示顺序）。条目：id / upstreamKey / reasoning / efforts / vision。
func qoderFallbackModels(intl bool) []qoderModel {
	type row struct {
		id, key   string
		reasoning bool
		efforts   []string
		vision    bool
	}
	global := []row{
		{"Qwen3.8-Flash", "qfmodel", true, []string{"low", "medium", "xhigh"}, true},
		{"Qwen3.8-Max", "qmodel_38max", true, []string{"low", "medium", "xhigh"}, true},
		{"Auto", "auto", true, nil, false},
		{"Ultimate", "ultimate", true, nil, true},
		{"Performance", "performance", true, nil, true},
		{"Efficient", "efficient", false, nil, false},
		{"Sonus", "smodel", true, nil, true},
		{"Cantus", "cmodel", true, nil, true},
		{"Qwen3.7-Max", "qmodel_latest", true, nil, false},
		{"Qwen3.7-Plus", "qmodel", false, nil, false},
		{"Kimi-K3", "kmodel_latest", false, nil, false},
		{"Kimi-K2.8-Preview", "kmodel", false, nil, false},
		{"GLM-5.3", "gmodel", true, nil, false},
		{"GLM-5.3-Flash", "gfmodel", true, nil, false},
		{"DeepSeek-V4-Pro", "dmodel", true, nil, false},
		{"DeepSeek-Flash", "dfmodel", false, nil, false},
		{"MiniMax-M3", "mmodel", false, nil, false},
	}
	cn := []row{
		{"Qwen3.8-Flash", "qfmodel", true, []string{"low", "medium", "xhigh"}, true},
		{"Qwen3.8-Max", "qmodel_38max", true, []string{"low", "medium", "xhigh"}, true},
		{"Auto", "auto", true, nil, false},
		{"Qwen3.7-Max", "qmodel_latest", true, nil, false},
		{"Qwen3.7-Plus", "qmodel", false, nil, false},
		{"DeepSeek-V4-Pro", "dmodel", true, nil, false},
		{"DeepSeek-Flash", "dfmodel", false, nil, false},
		{"GLM-5.3", "gmodel", true, nil, false},
		{"Kimi-K2.8-Preview", "kmodel", true, nil, false},
		{"MiniMax-M3", "mmodel", false, nil, false},
	}
	rows := cn
	if intl {
		rows = global
	}
	out := make([]qoderModel, 0, len(rows))
	for _, r := range rows {
		out = append(out, qoderModel{ID: r.id, Key: r.key, Reasoning: r.reasoning, Efforts: r.efforts, Vision: r.vision})
	}
	return out
}

// qoderStaticModelIDs 静态目录的 id 清单（StaticModels 用）。
func qoderStaticModelIDs(intl bool) []string {
	out := make([]string, 0, 16)
	for _, m := range qoderFallbackModels(intl) {
		out = append(out, m.ID)
	}
	return out
}

// ── content 处理 ────────────────────────────────────────────────

// qoderContentToText 把任意形态的 content 压平成文本（protocol.rs:45-76）。
// 数组里的 image_url 等非文本块跳过而不是报错。
func qoderContentToText(content any) string {
	switch c := content.(type) {
	case string:
		return c
	case []any:
		var b strings.Builder
		for _, item := range c {
			switch part := item.(type) {
			case string:
				b.WriteString(part)
			case map[string]any:
				if text, ok := part["text"].(string); ok {
					b.WriteString(text)
				} else if text, ok := part["content"].(string); ok {
					b.WriteString(text)
				}
			}
		}
		return b.String()
	}
	return ""
}

// qoderImagesOf 抽出消息里的图片块（OpenAI image_url 形态；protocol.rs:78-91）。
func qoderImagesOf(content any) []any {
	items, ok := content.([]any)
	if !ok {
		return nil
	}
	var out []any
	for _, item := range items {
		m, isMap := item.(map[string]any)
		if !isMap {
			continue
		}
		if t, _ := m["type"].(string); t == "image_url" {
			out = append(out, item)
		}
	}
	return out
}

// qoderNormalizeMessages 把 OpenAI messages 规整成上游能吃的形状
// （protocol.rs:100-213）。差异：
//   - 上一次出错/中断的 assistant 轮次（`__failed`/`__aborted`）连同其 tool
//     结果一起丢（否则留下孤儿 tool 结果，上游会拒绝）；
//   - assistant 只有工具调用时给一个非空 content（上游会拒纯空体）；
//   - user 消息里的图片保持 image_url 块形态；
//   - system/developer 压平成文本。
func qoderNormalizeMessages(messages []any) []any {
	out := make([]any, 0, len(messages))
	var droppedToolCallIDs []string
	for _, m := range messages {
		msg, isMap := m.(map[string]any)
		if !isMap {
			continue
		}
		role, _ := msg["role"].(string)
		if role == "" {
			continue
		}
		if role == "assistant" {
			failed := truthyQoder(msg["__failed"]) || truthyQoder(msg["__aborted"])
			if failed {
				if calls, ok := msg["tool_calls"].([]any); ok {
					for _, c := range calls {
						cm, isMap := c.(map[string]any)
						if !isMap {
							continue
						}
						if id, _ := cm["id"].(string); id != "" {
							droppedToolCallIDs = append(droppedToolCallIDs, id)
						}
					}
				}
				continue
			}
		}
		if role == "tool" {
			callID, _ := msg["tool_call_id"].(string)
			dropped := false
			for _, known := range droppedToolCallIDs {
				if known == callID {
					dropped = true
					break
				}
			}
			if dropped {
				continue
			}
		}
		switch role {
		case "user":
			content := msg["content"]
			images := qoderImagesOf(content)
			if len(images) == 0 {
				out = append(out, map[string]any{"role": "user", "content": qoderContentToText(content)})
				continue
			}
			var parts []any
			if text := qoderContentToText(content); text != "" {
				parts = append(parts, map[string]any{"type": "text", "text": text})
			}
			for _, image := range images {
				im, _ := image.(map[string]any)
				if im == nil {
					continue
				}
				parts = append(parts, map[string]any{"type": "image_url", "image_url": im["image_url"]})
			}
			out = append(out, map[string]any{"role": "user", "content": parts})
		case "assistant":
			text := qoderContentToText(msg["content"])
			var toolCalls []any
			if calls, ok := msg["tool_calls"].([]any); ok {
				for _, c := range calls {
					cm, isMap := c.(map[string]any)
					if !isMap {
						continue
					}
					fn, _ := cm["function"].(map[string]any)
					if fn == nil {
						fn = map[string]any{}
					}
					args := ""
					switch a := fn["arguments"].(type) {
					case string:
						args = a
					default:
						if a != nil {
							if raw, ok := qoderDeterministicJSON(a); ok {
								args = string(raw)
							}
						}
					}
					id, _ := cm["id"].(string)
					name, _ := fn["name"].(string)
					toolCalls = append(toolCalls, map[string]any{
						"id":   id,
						"type": "function",
						"function": map[string]any{
							"name":      name,
							"arguments": args,
						},
					})
				}
			}
			if text == "" && len(toolCalls) > 0 {
				text = " " // 只有工具调用时给一个非空 content（上游会拒纯空体）
			}
			mapped := map[string]any{"role": "assistant", "content": text}
			if len(toolCalls) > 0 {
				mapped["tool_calls"] = toolCalls
			}
			out = append(out, mapped)
		case "tool":
			out = append(out, map[string]any{
				"role":         "tool",
				"tool_call_id": msg["tool_call_id"],
				"content":      qoderContentToText(msg["content"]),
			})
		case "system", "developer":
			out = append(out, map[string]any{
				"role":    "system",
				"content": qoderContentToText(msg["content"]),
			})
		}
	}
	return out
}

// qoderNormalizeTools 把 OpenAI tools 转成上游形态（protocol.rs:214-236）。
// 只留 name/description/parameters；空清单返回 nil。
func qoderNormalizeTools(raw any) []any {
	items, ok := raw.([]any)
	if !ok || len(items) == 0 {
		return nil
	}
	var out []any
	for _, item := range items {
		m, isMap := item.(map[string]any)
		if !isMap {
			continue
		}
		fn, _ := m["function"].(map[string]any)
		if fn == nil {
			continue
		}
		if _, hasName := fn["name"]; !hasName {
			continue
		}
		function := map[string]any{}
		for _, key := range []string{"name", "description", "parameters"} {
			if v, exists := fn[key]; exists {
				function[key] = v
			}
		}
		out = append(out, map[string]any{"type": "function", "function": function})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// qoderThinkingChoice 一次请求的思考设置（protocol.rs:291-295）。
// Enable 为 nil = 不指定参数（让上游走默认）。
type qoderThinkingChoice struct {
	Enable *bool
	Effort string
}

// qoderResolveThinking 翻译下游的思考意图（protocol.rs:295-356）。
//
//   - 模型不支持思考 → 不指定；
//   - "关闭思考"（false / off / none / disabled）→ **降级为不指定**（上游无法
//     真正关闭，发 false 会断连或混流——上游行为，网关不可规避）；
//   - 未指定或 true → enable=true、不带 effort（沿用上游默认档位）；
//   - 显式档位：minimal/min→low、high/max→xhigh；不在声明池 → 回落 medium
//     （或声明档第一项），不发一个无效值。
func qoderResolveThinking(body map[string]any, model qoderModel) qoderThinkingChoice {
	if !model.Reasoning {
		return qoderThinkingChoice{}
	}
	raw, declared := body["reasoning_effort"]
	if !declared {
		if raw, declared = body["reasoning"]; !declared {
			raw, declared = body["thinking"]
		}
	}
	if !declared {
		// 未指定：沿用上游默认（enable=true 不带档位，与源实现同款）。
		enable := true
		return qoderThinkingChoice{Enable: &enable}
	}
	switch v := raw.(type) {
	case nil:
		enable := true
		return qoderThinkingChoice{Enable: &enable}
	case bool:
		if !v {
			return qoderThinkingChoice{} // 关闭 → 不指定（降级）
		}
		enable := true
		return qoderThinkingChoice{Enable: &enable}
	case string:
		asked := strings.ToLower(strings.TrimSpace(v))
		switch asked {
		case "", "off", "none", "disabled":
			return qoderThinkingChoice{}
		}
		asked = strings.TrimPrefix(asked, `"`) // 防御：客户端误传带引号的字符串
		asked = strings.TrimSuffix(asked, `"`)
		enable := true
		if asked == "true" {
			return qoderThinkingChoice{Enable: &enable}
		}
		return qoderThinkingChoice{Enable: &enable, Effort: qoderPickEffort(asked, model.Efforts)}
	case json.Number:
		return qoderThinkingChoice{} // 非法形态：不指定，交上游默认
	default:
		asked := strings.ToLower(strings.TrimSpace(fmt.Sprintf("%v", raw)))
		enable := true
		return qoderThinkingChoice{Enable: &enable, Effort: qoderPickEffort(asked, model.Efforts)}
	}
}

func qoderPickEffort(asked string, declared []string) string {
	switch asked {
	case "minimal", "min":
		asked = "low"
	case "high", "max":
		asked = "xhigh"
	}
	pool := declared
	if len(pool) == 0 {
		pool = []string{"low", "medium", "xhigh"}
	}
	for _, known := range pool {
		if known == asked {
			return asked
		}
	}
	for _, known := range declared {
		if known == "medium" {
			return known
		}
	}
	if len(declared) > 0 {
		return declared[0]
	}
	return "medium"
}

// qoderSlimModelConfig 精简发往上游的 model_config（protocol.rs:434-470）。
// **必须剥掉 thinking_config**：原样回传会覆盖 parameters.enable_thinking，
// 让"关闭思考"失效（实测带 thinking_config 时 enable_thinking=false 仍输出
// 数千字思考）。
func qoderSlimModelConfig(model qoderModel) map[string]any {
	slim := map[string]any{"key": model.Key, "is_reasoning": model.Reasoning, "is_vl": model.Vision, "source": "system"}
	return slim
}

// qoderBuildUpstreamBody 组装上游请求体（protocol.rs:358-433 的固定信封）。
//
// `sessionSeed`：下游给了 user / session_id 时用它派生，同一段对话复用同一
// session；否则每次新建。
func qoderBuildUpstreamBody(model qoderModel, finalMessages []any, tools []any,
	maxTokens int64, thinking qoderThinkingChoice, userID, sessionSeed string) map[string]any {
	limit := maxTokens
	if limit <= 0 || limit > qoderMaxOutputTokens {
		limit = qoderMaxOutputTokens
	}
	// 取最后一条用户文本：上游要求带在 context 与 business 里。
	lastUserText := ""
	for i := len(finalMessages) - 1; i >= 0; i-- {
		msg, isMap := finalMessages[i].(map[string]any)
		if !isMap {
			continue
		}
		if role, _ := msg["role"].(string); role == "user" {
			lastUserText = qoderContentToText(msg["content"])
			break
		}
	}

	parameters := map[string]any{"max_tokens": json.Number(fmt.Sprintf("%d", limit))}
	// thinking 为"不指定"时**不能落 false**：Qwen3.8 系列 enable_thinking=false
	// 会把思考混进正文或直接断连（protocol.rs:385-399）。
	if thinking.Enable != nil {
		if *thinking.Enable {
			parameters["enable_thinking"] = true
			if thinking.Effort != "" {
				parameters["reasoning_effort"] = thinking.Effort
			}
		} else {
			parameters["enable_thinking"] = false
		}
	}
	if tools == nil {
		tools = []any{}
	}
	businessName := lastUserText
	if len(businessName) > 30 {
		businessName = string([]rune(businessName)[:30])
	}
	return map[string]any{
		"request_id":       qoderRandomUUID(),
		"request_set_id":   qoderRecordID(model.Key, finalMessages, tools, limit),
		"chat_record_id":   qoderRecordID(model.Key, finalMessages, tools, limit),
		"session_id":       qoderSessionID(userID, model.Key, sessionSeed),
		"stream":           true,
		"chat_task":        qoderChatTask,
		"is_reply":         true,
		"is_retry":         false,
		"source":           1,
		"version":          "3",
		"session_type":     qoderSessionType,
		"agent_id":         qoderAgentID,
		"task_id":          "common",
		"code_language":    "",
		"chat_prompt":      "",
		"image_urls":       nil,
		"aliyun_user_type": "",
		// 上游不看顶层 system 字段，system 提示在 messages 里（protocol.rs:424）。
		"system":     "",
		"messages":   finalMessages,
		"tools":      tools,
		"parameters": parameters,
		"chat_context": map[string]any{
			"chatPrompt": "",
			"imageUrls":  nil,
			"extra": map[string]any{
				"context": []any{},
				"modelConfig": map[string]any{
					"key":          model.Key,
					"is_reasoning": model.Reasoning,
				},
				"originalContent": lastUserText,
			},
			"features": []any{},
			"text":     lastUserText,
		},
		"model_config": qoderSlimModelConfig(model),
		"business": map[string]any{
			"product":  "cli",
			"version":  "1.0.0",
			"type":     "agent",
			"stage":    "start",
			"id":       qoderRandomUUID(),
			"name":     businessName,
			"begin_at": time.Now().UnixNano() / int64(time.Millisecond),
		},
	}
}

// qoderFinalMessages 规整 + system 置顶（chat.rs:161-193）。
// system 提示必须放进 messages 且排在最前面（上游顶层 system 字段无效）。
func qoderFinalMessages(rawMessages []any) []any {
	var systemTexts []string
	for _, m := range rawMessages {
		msg, isMap := m.(map[string]any)
		if !isMap {
			continue
		}
		if role, _ := msg["role"].(string); role == "system" || role == "developer" {
			if text := qoderContentToText(msg["content"]); text != "" {
				systemTexts = append(systemTexts, text)
			}
		}
	}
	messages := qoderNormalizeMessages(rawMessages)
	if len(systemTexts) == 0 {
		return messages
	}
	ordered := make([]any, 0, len(messages)+len(systemTexts))
	for _, text := range systemTexts {
		ordered = append(ordered, map[string]any{"role": "system", "content": text})
	}
	for _, m := range messages {
		msg, isMap := m.(map[string]any)
		if !isMap {
			continue
		}
		if role, _ := msg["role"].(string); role == "system" {
			continue
		}
		ordered = append(ordered, m)
	}
	return ordered
}

// qoderMaxTokensOf 下游请求的 max_tokens（max_tokens 优先于
// max_completion_tokens；chat.rs:197-200）。
func qoderMaxTokensOf(body map[string]any) int64 {
	for _, key := range []string{"max_tokens", "max_completion_tokens"} {
		if n, ok := body[key].(json.Number); ok {
			if v, err := n.Int64(); err == nil {
				return v
			}
		}
	}
	return 0
}

// qoderSessionSeedOf 下游给的会话种子（user / session_id；chat.rs:201-205）。
func qoderSessionSeedOf(body map[string]any) string {
	for _, key := range []string{"user", "session_id"} {
		if s, ok := body[key].(string); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

// qoderModelOut 客户端可见的 model 名：固定回客户端请求的那个名字。
//
// 上游回的 `model` 是路由档位（请求 Qwen3.8-Flash 实测回 "auto"，chat.rs:428-435），
// 按它映射会把帧里的模型名换成另一个模型；本网关里 BuildRequest 已保证
// model_name 是用户请求的目录名（未命中早在构造期硬失败），因此固定回显。
// 与源实现 `map_model_back` 的差异及理由：源实现的映射只服务"客户端写了
// 内部 key"的形态，而本网关在解析期就用同一张表放行/拒绝，不存在漏网形态。
func qoderModelOut(modelName string) string {
	return modelName
}
