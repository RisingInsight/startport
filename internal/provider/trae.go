// trae.go TRAE（字节 SOLO 国内）原生适配器。
//
// ── 上游形态（逐条对齐参考实现 agent2api/trae，2026-10）────────
//
//	chat   POST {base}/api/agent/v3/llm_utils_chat
//	目录   POST {base}/api/ide/v1/get_detail_param
//	鉴权   同一个 accessToken 进三个头：
//	       Authorization: Cloud-IDE-JWT <token> / X-Cloudide-Token / X-Ide-Token
//	响应   恒 SSE：event: output | token_usage | done | error；没有 [DONE]
//
// ── 为什么必须原生适配器 ─────────────────────────────────────
//  1. 请求体是**白名单重建**：多带字段不是被忽略，是被拒（流内 4023/4001）；
//  2. tools 的 parameters 必须是 JSON 字符串、tool_choice 必须是裸字符串、
//     assistant 的 tool_calls 要改名 function_call、developer 角色要降成 system；
//  3. 上游恒 stream:true，非流式必须由网关聚合；
//  4. 额度/登录态错误藏在 HTTP 200 的 SSE `event: error` 里，必须在向客户端
//     写任何字节之前预读首帧，否则换证/401 语义全丢。
//
// ── 本批边界 ─────────────────────────────────────────────────
// 只支持国内 SOLO（`llm_utils_chat` + `solo_work_lite`）。登录、签到、刷新
// 在第二批接入；国际版是另一套两步协议，显式拒绝而不是让凭据打到错主机。
package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// TraeAdapterName 注册名（config.ext_channels[].adapter）。
const TraeAdapterName = "trae"

const (
	traeChatPath       = "/api/agent/v3/llm_utils_chat"
	traeModelsPath     = "/api/ide/v1/get_detail_param"
	traeIDEVersion     = "0.1.61"
	traeIDEVersionCode = "20260820" // 与模型目录表绑定，见参考实现 headers.rs
	traeAppID          = "6eefa01c-1036-4c7e-9ca5-d891f63bfcd8"
	traeDeviceBrand    = "83DG"
	traeOSVersion      = "Windows 11 Pro"
	traeUserAgent      = "Trae/0.1.61"
	traeDefaultModel   = "glm-5.2"
	traeFunction       = "solo_work_lite"
	traeMaxTokens      = 1_000_000
)

// traeDeadModels 在 solo_work_lite 通道必定流内 4001 的配置名（精确名单，
// 不做前缀匹配——参考实现曾因前缀误杀可用模型）。
var traeDeadModels = map[string]bool{
	"agnes-2.0-flash":   true,
	"agnes-agent-x":     true,
	"deepseek-v4-flash": true,
	"deepseek-v4-pro":   true,
}

type traeAdapter struct{}

var _ StatusNormalizer = (*traeAdapter)(nil)

// NewTraeAdapter 构造 TRAE 适配器。
func NewTraeAdapter() NativeAdapter { return &traeAdapter{} }

func init() { RegisterNative(NewTraeAdapter()) }

func (a *traeAdapter) Name() string       { return TraeAdapterName }
func (a *traeAdapter) Protocol() Protocol { return ProtocolOpenAI }

// traeCreds 适配器需要的身份字段（其余字段留给第二批刷新链）。
type traeCreds struct {
	AccessToken string
	UID         string
	MachineID   string
	DeviceID    string
	Variant     string
}

// parseTraeSecret 解析账号 JSON；裸 token 也接受（无设备头，可能被上游拒，
// 但比在网关侧直接拒绝更可诊断）。
func parseTraeSecret(secret string) (traeCreds, error) {
	trimmed := strings.TrimSpace(secret)
	if trimmed == "" {
		return traeCreds{}, &HTTPError{Status: http.StatusUnauthorized, Code: "token_invalid",
			Message: "Trae 账号缺少 accessToken，无法转发（请重新粘贴登录态）"}
	}
	if trimmed[0] != '{' {
		return traeCreds{AccessToken: trimmed, Variant: "solo"}, nil
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(trimmed), &m); err != nil {
		return traeCreds{}, &HTTPError{Status: http.StatusBadRequest, Code: "trae_credential_invalid",
			Message: "Trae 凭证 JSON 解析失败: " + err.Error()}
	}
	auth := m
	if nested, ok := m["auth"].(map[string]any); ok {
		auth = nested
	}
	accountPart, _ := m["account"].(map[string]any)
	pick := func(keys ...string) string {
		for _, src := range []map[string]any{auth, m, accountPart} {
			if src == nil {
				continue
			}
			for _, k := range keys {
				if s, ok := src[k].(string); ok && strings.TrimSpace(s) != "" {
					return strings.TrimSpace(s)
				}
			}
		}
		return ""
	}
	token := pick("accessToken", "access_token")
	if token == "" {
		return traeCreds{}, &HTTPError{Status: http.StatusUnauthorized, Code: "token_invalid",
			Message: "Trae 凭证缺少 accessToken（Cloud-IDE-JWT）"}
	}
	variant := strings.ToLower(pick("variant"))
	if variant == "" {
		variant = "solo"
	}
	if variant == "intl" || variant == "solo-intl" {
		return traeCreds{}, &HTTPError{Status: http.StatusBadRequest, Code: "trae_intl_unsupported",
			Message: "Trae 国际版暂未接入（当前仅支持国内 SOLO 通道）；请在 trae.cn 登录后使用"}
	}
	return traeCreds{
		AccessToken: token,
		UID:         pick("uid", "user_id", "userId"),
		MachineID:   pick("machineId", "machine_id"),
		DeviceID:    pick("deviceId", "device_id"),
		Variant:     variant,
	}, nil
}

// BuildRequest 实现 Adapter。
func (a *traeAdapter) BuildRequest(ctx context.Context, t *Target, openAIBody []byte, stream bool) (*http.Request, error) {
	creds, err := parseTraeSecret(t.Secret)
	if err != nil {
		return nil, err
	}
	raw, resolved, err := traePrepareBody(openAIBody, creds.Variant, ExtractModel(openAIBody))
	if err != nil {
		return nil, err
	}
	u := traeURL(t, resolved)
	// 上游恒流式；stream 参数只影响 Accept（参考实现也恒发事件流）。
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("build trae request: %w", err)
	}
	applyTraeHeaders(req, t, creds, true)
	return req, nil
}

// ConvertResponse 实现 Adapter：上游恒 SSE，非流式在这里聚合。
func (a *traeAdapter) ConvertResponse(t *Target, status int, body []byte) []byte {
	if status >= 400 {
		if isOpenAIErrorEnvelope(body) {
			return body
		}
		return traeHTTPErrorEnvelope(status, body)
	}
	out, err := traeAggregate(body, clientModelOf(t))
	if err != nil {
		var he *HTTPError
		if errors.As(err, &he) {
			return jsonErrorEnvelope("upstream_error", he.Code, he.Message)
		}
		return jsonErrorEnvelope("upstream_error", "trae_aggregate_error", err.Error())
	}
	return out
}

// NormalizeStatus 实现 StatusNormalizer：非流式错误体归一成 OpenAI 信封。
func (a *traeAdapter) NormalizeStatus(status int, body []byte) (int, []byte) {
	if status < 400 {
		return status, body
	}
	if isOpenAIErrorEnvelope(body) {
		return traeNormalizedStatus(status), body
	}
	return traeNormalizedStatus(status), traeHTTPErrorEnvelope(status, body)
}

// traeNormalizedStatus 把上游状态码收敛到网关的处置语义：
//   - 401/429/404/413 保持原义（鉴权/限流/模型/输入）；
//   - 403 是请求/设备绑定一类的问题，**不能**当凭证失效（否则会误禁用账号）；
//     按参考实现归到 502（5xx 只触发 30s 渠道冷却，不换证）；
//   - 其它 5xx → 502。
func traeNormalizedStatus(status int) int {
	switch {
	case status == http.StatusUnauthorized, status == http.StatusTooManyRequests,
		status == http.StatusNotFound, status == http.StatusRequestEntityTooLarge:
		return status
	case status == http.StatusForbidden:
		return http.StatusBadGateway
	case status >= 500:
		return http.StatusBadGateway
	default:
		return status
	}
}

// ConvertStream 实现 Adapter：自有 SSE 方言 → OpenAI chunk SSE。
func (a *traeAdapter) ConvertStream(t *Target, contentType string, body io.ReadCloser) (io.ReadCloser, error) {
	ct := strings.ToLower(strings.TrimSpace(contentType))
	if ct != "" && !strings.Contains(ct, "text/event-stream") {
		raw, _ := io.ReadAll(io.LimitReader(body, 1<<20))
		_ = body.Close()
		return nil, NewHTTPError(http.StatusBadGateway, "trae_bad_stream",
			"Trae 上游未按 SSE 返回（Content-Type="+contentType+"）："+truncate(string(raw), 300))
	}
	return newTraeStream(t, body)
}

// BuildModels 实现 Adapter：POST get_detail_param（目录与 IDE 版本头绑定）。
func (a *traeAdapter) BuildModels(ctx context.Context, t *Target) (*http.Request, error) {
	creds, err := parseTraeSecret(t.Secret)
	if err != nil {
		return nil, err
	}
	body := map[string]any{
		"function":            traeFunction,
		"config_names":        nil,
		"need_prompt":         false,
		"current_config_info": nil,
		"poly_prompt":         true,
		"mode_type":           nil,
		"agent_type":          nil,
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal trae catalog body: %w", err)
	}
	u := strings.TrimRight(strings.TrimSpace(t.BaseURL), "/") + traeModelsPath
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("build trae models request: %w", err)
	}
	applyTraeHeaders(req, t, creds, false)
	return req, nil
}

// DecodeModels 实现 Adapter：过滤不可见/占位/已下线/本通道必死的配置。
func (a *traeAdapter) DecodeModels(_ *Target, status int, body []byte) ([]string, error) {
	if status != http.StatusOK {
		return nil, NewHTTPError(status, "trae_models_error", truncate(string(body), 300))
	}
	var payload struct {
		List []map[string]any `json:"config_info_list"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("parse trae catalog: %w", err)
	}
	out := make([]string, 0, len(payload.List))
	seen := map[string]bool{}
	for _, item := range payload.List {
		name := traeString(item["config_name"])
		if name == "" || seen[strings.ToLower(name)] {
			continue
		}
		seen[strings.ToLower(name)] = true
		if item["config_switch"] == false || item["is_invisible_to_user"] == true {
			continue
		}
		display := ""
		if dc, ok := item["display_config"].(map[string]any); ok {
			display = traeString(dc["display_name"])
		}
		if display == "" || traeDeadModels[strings.ToLower(name)] {
			continue
		}
		out = append(out, name)
	}
	return out, nil
}

// ── URL / 头 / 请求体 ─────────────────────────────────────────

func traeURL(t *Target, model string) string {
	base := strings.TrimRight(strings.TrimSpace(t.BaseURL), "/")
	if p := strings.TrimSpace(t.Path); p != "" {
		return base + renderPath(p, model)
	}
	return base + traeChatPath
}

// traeReservedHeader 平台契约独占的头：静态 headers 覆盖不掉。
func traeReservedHeader(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "authorization", "x-cloudide-token", "x-ide-token", "x-uid",
		"x-machine-id", "x-device-id", "x-app-id", "x-app-version",
		"x-ide-version", "x-ide-version-code", "x-app-version-code",
		"x-ide-version-type", "x-device-type", "x-os-version",
		"x-device-brand", "request-traffic-type", "user-agent":
		return true
	}
	return false
}

func applyTraeHeaders(req *http.Request, t *Target, c traeCreds, stream bool) {
	h := req.Header
	h.Set("Content-Type", "application/json")
	if stream {
		h.Set("Accept", "text/event-stream")
	} else {
		h.Set("Accept", "application/json")
	}
	h.Set("User-Agent", traeUserAgent)
	if t != nil {
		for k, v := range t.Headers {
			if k == "" || v == "" || HeaderForbidden(k) || traeReservedHeader(k) {
				continue
			}
			h.Set(k, v)
		}
	}
	h.Set("Authorization", "Cloud-IDE-JWT "+c.AccessToken)
	h.Set("X-Cloudide-Token", c.AccessToken)
	h.Set("X-Ide-Token", c.AccessToken)
	if c.UID != "" {
		h.Set("X-Uid", c.UID)
	}
	if c.MachineID != "" {
		h.Set("X-Machine-Id", c.MachineID)
	}
	if c.DeviceID != "" {
		h.Set("X-Device-Id", c.DeviceID)
	}
	h.Set("X-App-Id", traeAppID)
	h.Set("X-App-Version", "default")
	h.Set("X-Ide-Version", traeIDEVersion)
	h.Set("X-Ide-Version-Code", traeIDEVersionCode)
	h.Set("X-App-Version-Code", traeIDEVersionCode)
	h.Set("X-Ide-Version-Type", "stable")
	h.Set("X-Device-Type", "windows")
	h.Set("X-OS-Version", traeOSVersion)
	h.Set("X-Device-Brand", traeDeviceBrand)
	h.Set("Request-Traffic-Type", "prod")
}

// traePrepareBody 白名单重建出站请求体（逐条对齐参考实现 payload.rs）。
func traePrepareBody(src []byte, variant, model string) ([]byte, string, error) {
	dec := json.NewDecoder(bytes.NewReader(src))
	dec.UseNumber()
	var body map[string]any
	if err := dec.Decode(&body); err != nil || body == nil {
		return nil, "", &HTTPError{Status: http.StatusBadRequest, Code: "trae_bad_request",
			Message: "请求体必须是 JSON 对象"}
	}
	resolved := traeSanitizeModel(model, variant)
	if resolved == "" {
		resolved = traeDefaultModel
	}
	out := traeRebuildBody(body, resolved)
	raw, err := json.Marshal(out)
	if err != nil {
		return nil, "", &HTTPError{Status: http.StatusInternalServerError, Code: "trae_encode_error",
			Message: "Trae 请求体序列化失败"}
	}
	return raw, resolved, nil
}

func traeRebuildBody(body map[string]any, model string) map[string]any {
	if msgs, ok := body["messages"].([]any); ok {
		body["messages"] = traeNormalizeMessages(msgs)
	}
	traeNormalizeToolChoice(body)
	traeNormalizeTools(body)

	out := map[string]any{
		"function":    traeFunction,
		"stream":      true,
		"config_name": model,
		"model":       model,
	}
	for _, key := range []string{"messages", "tools", "tool_choice"} {
		if v, ok := body[key]; ok {
			out[key] = v
		}
	}
	for _, key := range []string{"temperature", "top_p", "max_tokens", "presence_penalty", "frequency_penalty", "seed", "n"} {
		if v, ok := body[key]; ok && traeIsNumber(v) {
			out[key] = v
		}
	}
	if _, ok := out["max_tokens"]; !ok {
		out["max_tokens"] = traeMaxTokens
	}
	if effort, ok := body["reasoning_effort"].(string); ok {
		switch strings.ToLower(strings.TrimSpace(effort)) {
		case "", "auto", "none", "off":
		default:
			out["reasoning_effort"] = effort
		}
	}
	switch stop := body["stop"].(type) {
	case string, []any:
		out["stop"] = stop
	}
	return out
}

func traeNormalizeMessages(messages []any) []any {
	normalized := make([]any, 0, len(messages))
	for _, item := range messages {
		msg, ok := item.(map[string]any)
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		if role == "" {
			continue
		}
		if role == "developer" {
			msg["role"] = "system"
			role = "system"
		}
		drop := false
		if role == "assistant" {
			if calls, ok := msg["tool_calls"].([]any); ok {
				kept := make([]any, 0, len(calls))
				for _, rawCall := range calls {
					call, ok := rawCall.(map[string]any)
					if !ok {
						continue
					}
					if fn, ok := call["function"]; ok {
						call["function_call"] = fn
						delete(call, "function")
					}
					fc, _ := call["function_call"].(map[string]any)
					if name := traeString(fc["name"]); strings.TrimSpace(name) != "" {
						kept = append(kept, call)
					}
				}
				if len(kept) == 0 {
					delete(msg, "tool_calls")
					if _, has := msg["content"]; !has || msg["content"] == nil {
						drop = true
					}
				} else {
					msg["tool_calls"] = kept
				}
			}
		}
		if drop {
			continue
		}
		if text, ok := msg["content"].(string); ok {
			msg["content"] = []any{map[string]any{"type": "text", "text": text}}
		}
		normalized = append(normalized, msg)
	}

	// 丢掉引用了不存在的 tool_call 的 role=tool 结果（上游对孤儿结果是空流）。
	known := map[string]bool{}
	for _, item := range normalized {
		msg, _ := item.(map[string]any)
		if role, _ := msg["role"].(string); role != "assistant" {
			continue
		}
		calls, _ := msg["tool_calls"].([]any)
		for _, rawCall := range calls {
			call, _ := rawCall.(map[string]any)
			if id := traeString(call["id"]); id != "" {
				known[id] = true
			}
		}
	}
	out := normalized[:0]
	for _, item := range normalized {
		msg, _ := item.(map[string]any)
		if role, _ := msg["role"].(string); role == "tool" {
			if id := traeString(msg["tool_call_id"]); !known[id] {
				continue
			}
		}
		out = append(out, item)
	}
	return out
}

func traeNormalizeToolChoice(body map[string]any) {
	raw, ok := body["tool_choice"]
	if !ok {
		return
	}
	switch v := raw.(type) {
	case string:
		if strings.EqualFold(strings.TrimSpace(v), "none") {
			delete(body, "tool_choice")
			delete(body, "tools")
			delete(body, "functions")
		}
	case map[string]any:
		kind := strings.ToLower(strings.TrimSpace(traeString(v["type"])))
		switch kind {
		case "none":
			delete(body, "tool_choice")
			delete(body, "tools")
			delete(body, "functions")
		case "auto", "required":
			body["tool_choice"] = kind
		case "function":
			name := ""
			if fn, ok := v["function"].(map[string]any); ok {
				name = traeString(fn["name"])
			}
			if name == "" {
				name = traeString(v["name"])
			}
			if name == "" {
				name = "auto"
			}
			body["tool_choice"] = name
		default:
			delete(body, "tool_choice")
		}
	default:
		delete(body, "tool_choice")
	}
}

func traeNormalizeTools(body map[string]any) {
	raw, ok := body["tools"].([]any)
	if !ok || len(raw) == 0 {
		return
	}
	kept := make([]any, 0, len(raw))
	for _, item := range raw {
		tool, ok := item.(map[string]any)
		if !ok {
			continue
		}
		fn, ok := tool["function"].(map[string]any)
		if !ok {
			continue
		}
		if params, ok := fn["parameters"].(map[string]any); ok {
			if encoded, err := json.Marshal(params); err == nil {
				fn["parameters"] = string(encoded)
			}
		}
		tool["function"] = fn
		kept = append(kept, tool)
	}
	if len(kept) == 0 {
		delete(body, "tools")
		return
	}
	body["tools"] = kept
}

func traeSanitizeModel(model, variant string) string {
	name := strings.TrimSpace(model)
	if strings.EqualFold(variant, "solo") {
		name = strings.TrimSuffix(name, "-solo")
	}
	name = strings.TrimSuffix(name, "-intl")
	return strings.TrimSpace(name)
}

func traeString(v any) string {
	if s, ok := v.(string); ok {
		return strings.TrimSpace(s)
	}
	return ""
}

func traeIsNumber(v any) bool {
	switch v.(type) {
	case json.Number, float64, int, int64:
		return true
	}
	return false
}

// ── 错误归一 ─────────────────────────────────────────────────

func traeHTTPErrorEnvelope(status int, body []byte) []byte {
	code := "upstream_error"
	typ := "upstream_error"
	switch status {
	case http.StatusUnauthorized:
		code, typ = "token_invalid", "authentication_error"
	case http.StatusRequestEntityTooLarge:
		code = "trae_input_too_large"
	case http.StatusTooManyRequests:
		code = "trae_rate_limited"
	case http.StatusNotFound:
		code = "trae_model_unavailable"
	}
	detail := strings.TrimSpace(string(body))
	if detail == "" {
		detail = fmt.Sprintf("HTTP %d", status)
	}
	return jsonErrorEnvelope(typ, code,
		fmt.Sprintf("Trae 上游返回 HTTP %d: %s", status, truncate(detail, 500)))
}

// traeStreamHTTPError 流内 event: error → typed error（首帧门用）。
func traeStreamHTTPError(code int64, message string) *HTTPError {
	text := fmt.Sprintf("solo error code=%d msg=%s", code, message)
	switch {
	case code == 1005 || code == 4008:
		return NewHTTPError(http.StatusTooManyRequests, "trae_plan_limit", "Trae 配额/计划不足："+text)
	case traeInputTooLarge(message):
		return NewHTTPError(http.StatusRequestEntityTooLarge, "trae_input_too_large", "Trae 输入过大："+text)
	case code == 4001:
		return NewHTTPError(http.StatusNotFound, "trae_model_unavailable",
			"该模型在当前 SOLO 通道不可用："+text)
	default:
		return NewHTTPError(http.StatusBadGateway, "trae_upstream_error", "Trae 上游错误："+text)
	}
}

func traeInputTooLarge(message string) bool {
	low := strings.ToLower(message)
	for _, marker := range []string{
		"too long", "too many tokens", "context length", "maximum context",
		"context window", "request entity too large", "input too large",
		"输入过长", "内容过长", "上下文过长", "上下文长度", "超出模型上限",
	} {
		if strings.Contains(low, marker) {
			return true
		}
	}
	return false
}
