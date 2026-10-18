// loomy.go Loomy（讯飞）原生适配器——第一个 NativeAdapter 样板。
//
// ── 上游形态（逆向来源：Loomy 桌面端 `app.asar`）──────────────
//
//	模型网关  {集成网关}/api/v1     OpenAI 兼容（chat/completions、models）
//	鉴权      双头：`token: <session>` 与 `Authorization: Bearer <session>`
//
// 双头不是"随便选一个就行"：两个网关（模型 / 积分）对头名的偏好不同，官方
// 客户端在 authMode='token' 时**两个都发**。用本项目的四种 auth 表达不出来
// （每种只写一个头），这正是需要原生适配器的原因。
//
// ── 本文件刻意做与不做的事 ──────────────────────────────────
//   - **做**：URL（`{base}/chat/completions`、`{base}/models`）、双头、请求体
//     原样透传、SSE 原样透传、`{code,desc}` 业务错误 → OpenAI 错误信封；
//   - **不做**：短信登录 / HMAC 签名 / 签到 / 积分——签名只服务账号面（登录），
//     聊天转发不需要；本适配器只消费用户手工粘贴的 session；
//   - **不做**：session 续期。上游没有 refresh 接口，session 14 天过期后只能
//     重新登录换 secret（见 README「原生 Agent 适配器」）。
//
// ── 硬约束 ──────────────────────────────────────────────────
//   - 秘密值只进请求头，绝不进 URL、日志、错误体；
//   - 请求体**不重编码**：上游网关认 OpenAI wire，重新序列化会丢私有字段
//     （与 openai 适配器同一条理由）；
//   - 适配器无状态，可并发共享。
package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// LoomyAdapterName Loomy 原生适配器的注册名（config.ext_channels[].adapter）。
const LoomyAdapterName = "loomy"

// loomyChatPath / loomyModelsPath 模型网关下的固定路径（base 已含 /api/v1）。
const (
	loomyChatPath   = "/chat/completions"
	loomyModelsPath = "/models"
)

// loomyTokenHeader Loomy 的双头之一（另一个是 Authorization: Bearer）。
const loomyTokenHeader = "token"

// loomyAuthCodes 登录态失效业务码（与官方客户端 AUTH_ERROR_CODES 同源）。
// 上游用 **HTTP 200 + {code,desc}** 表达业务结果，所以这两个码出现时状态码
// 仍然是 200——必须靠 body 判定，不能只看 status。
//
// 键是**归一后**的形态（去掉前导零）：字符串 "020002" 与数字 20002 都会归一成
// "20002"，否则同一件事会因为上游序列化方式不同而漏判。
var loomyAuthCodes = map[string]bool{
	"20002":  true,
	"100002": true,
}

// loomyAdapter Loomy 原生适配器（无状态）。
type loomyAdapter struct{}

// 编译期断言：Loomy 需要改写"200 + 业务错误码"的状态，因此实现可选能力
// StatusNormalizer（三个协议基准适配器不实现它，行为不变）。
var _ StatusNormalizer = (*loomyAdapter)(nil)

// NewLoomyAdapter 构造 Loomy 适配器。
func NewLoomyAdapter() NativeAdapter { return &loomyAdapter{} }

// init 注册内置原生适配器。显式调用（不依赖跨文件 var 初始化顺序推断），
// 便于将来在别处追加 RegisterNative 而不改动本文件。
func init() { RegisterNative(NewLoomyAdapter()) }

// Name 实现 NativeAdapter。
func (a *loomyAdapter) Name() string { return LoomyAdapterName }

// Protocol 实现 Adapter：报文形态仍是 OpenAI Chat Completions。
func (a *loomyAdapter) Protocol() Protocol { return ProtocolOpenAI }

// BuildRequest 实现 Adapter：body 原样透传 + 双头 + 静态头。
func (a *loomyAdapter) BuildRequest(
	ctx context.Context, t *Target, openAIBody []byte, stream bool,
) (*http.Request, error) {
	model := ExtractModel(openAIBody)
	u := loomyURL(t, loomyChatPath, model)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(openAIBody))
	if err != nil {
		return nil, fmt.Errorf("build loomy request: %w", err)
	}
	applyLoomyHeaders(req, t, stream)
	return req, nil
}

// ConvertResponse 实现 Adapter。
//
// 三种输入形态：
//  1. 已是 OpenAI 可解析形态（正常 choices/data，或上游本来就给 error 信封）
//     → 原样透传（不二次包装）；
//  2. `{code,desc}` 业务错误（HTTP 200 也算）/ 裸 401/403 / 其它 >=400
//     → 走 loomyNormalizeError 的统一归一（与 NormalizeStatus 同一套文案）。
func (a *loomyAdapter) ConvertResponse(t *Target, status int, body []byte) []byte {
	// 401/403 一律表达为 session 过期——**即使 body 已经是 OpenAI 信封**：
	// Loomy 的鉴权失败只可能是 session 失效，客户端要拿到 authentication_error
	// 才会提示用户重新登录（原样透传 upstream_error 会让人无从下手）。
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		if _, out, ok := loomyNormalizeError(t, status, body); ok {
			return out
		}
	}
	// 已是 OpenAI 可解析形态（含 NormalizeStatus 的产物）→ 不二次包装。
	if isOpenAIShaped(body) {
		return body
	}
	if _, out, ok := loomyNormalizeError(t, status, body); ok {
		return out
	}
	return body
}

// NormalizeStatus 实现 StatusNormalizer。
//
// 目的：Loomy 用 `HTTP 200 + {code,desc}` 表达业务错误。只把 body 换成错误信封
// 而不改状态码，会让"按状态码判定成功"的 SDK 把 session 过期当成正常响应，
// 因此这里把状态码一起归一（session 过期 → 401，其它业务错误 → 400）。
//
// 正常响应（choices/data）返回原值不动。
func (a *loomyAdapter) NormalizeStatus(status int, body []byte) (int, []byte) {
	// 正常形态绝不改写（含上游 200 + choices 的正常响应）。
	if status < 400 && isOpenAIShaped(body) {
		return status, body
	}
	if st, out, ok := loomyNormalizeError(nil, status, body); ok {
		return st, out
	}
	return status, body
}

// ConvertStream 实现 Adapter：SSE 原样透传（上游是 OpenAI 兼容网关）。
//
// 已知边界：上游若在流式请求上返回 200 + `{code,desc}`（而不是 SSE），这里
// 仍按透传处理——"绝不伪流式"的既有约定只在 gemini 分支做硬判定；客户端会看到
// 一个非 SSE 的 body，表现为流式解析失败。面板探测与 README 已标注该边界。
func (a *loomyAdapter) ConvertStream(_ *Target, _ string, body io.ReadCloser) (io.ReadCloser, error) {
	return body, nil
}

// BuildModels 实现 Adapter：GET {base}/models（同样的双头 + 静态头）。
//
// 刻意**忽略 Path 覆盖**：Path 是聊天路径的逃生口（Loomy 聊天不需要它，但
// 保持与 BuildChatURL 同一套语义）；模型目录恒为 `{base}/models`，跟着 Path
// 走只会拼出 `.../{model}` 这种解析不了的地址。
func (a *loomyAdapter) BuildModels(ctx context.Context, t *Target) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, loomyModelsURL(t), nil)
	if err != nil {
		return nil, fmt.Errorf("build loomy models request: %w", err)
	}
	applyLoomyHeaders(req, t, false)
	return req, nil
}

// DecodeModels 实现 Adapter：解析 OpenAI `{data:[{id}]}`（也接受裸数组）。
//
// 上游把错误也塞在 200 里（`{code,desc}`），因此先判业务码再解析目录——
// 否则会得到一个"空目录"的假成功。
func (a *loomyAdapter) DecodeModels(_ *Target, status int, body []byte) ([]string, error) {
	if code, desc, ok := loomyBizError(body); ok {
		if loomyAuthCodes[normalizeLoomyCode(code)] {
			return nil, NewHTTPError(http.StatusUnauthorized, "session_expired",
				loomyExpiredMessage(code, desc))
		}
		return nil, NewHTTPError(status, "models_upstream_error", loomyBizMessage(code, desc))
	}
	if status != http.StatusOK {
		return nil, NewHTTPError(status, "models_upstream_error", truncate(string(body), 300))
	}
	var parsed struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err == nil {
		out := make([]string, 0, len(parsed.Data))
		for _, d := range parsed.Data {
			if id := strings.TrimSpace(d.ID); id != "" {
				out = append(out, id)
			}
		}
		return out, nil
	}
	// 兼容裸数组形态（上游某些边缘节点这么回）。
	var rows []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &rows); err != nil {
		return nil, fmt.Errorf("parse models: %w", err)
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		if id := strings.TrimSpace(r.ID); id != "" {
			out = append(out, id)
		}
	}
	return out, nil
}

// loomyURL 拼 Loomy 上游 URL。
//
// base 是用户填的模型网关根（`https://loomyad.xunfei.cn/api/v1`）。**不能**走
// LegacyOpenAIRoot：那条 shim 会给不带 /v1 的 base 补 `/v1`，得到
// `https://loomyad.xunfei.cn/v1/chat/completions`——而 Loomy 的版本段是
// `/api/v1`，补错了就是 404。Path 覆盖沿用 BuildChatURL 的规则（用户对完整
// 路径负责，支持 {model} 占位）。
func loomyURL(t *Target, suffix, model string) string {
	if t == nil {
		return suffix
	}
	base := strings.TrimRight(strings.TrimSpace(t.BaseURL), "/")
	if p := strings.TrimSpace(t.Path); p != "" {
		return base + renderPath(p, model)
	}
	return base + suffix
}

// loomyModelsURL 模型目录 URL：恒为 `{base}/models`（不受 Path 覆盖影响，
// 理由见 BuildModels 的注释）。
func loomyModelsURL(t *Target) string {
	if t == nil {
		return loomyModelsPath
	}
	return strings.TrimRight(strings.TrimSpace(t.BaseURL), "/") + loomyModelsPath
}

// applyLoomyHeaders 写入 Loomy 的请求头。
//
// 顺序刻意如此：协议默认头 → 静态头（禁传输层头、禁 token/Authorization）→
// **最后由适配器写死双头**。这样即使用户在 headers 里写了 token/Authorization，
// 也覆盖不掉平台契约（双头必须携带同一个 session），与 `auth` 字段一并忽略。
func applyLoomyHeaders(req *http.Request, t *Target, stream bool) {
	h := req.Header
	h.Set("Content-Type", "application/json")
	if stream {
		h.Set("Accept", "text/event-stream")
	} else {
		h.Set("Accept", "application/json")
	}
	if t == nil {
		return
	}
	for k, v := range t.Headers {
		if k == "" || v == "" || HeaderForbidden(k) || isLoomyReservedHeader(k) {
			continue
		}
		h.Set(k, v)
	}
	secret := strings.TrimSpace(t.Secret)
	if secret == "" {
		return // 无凭证：不注入任何鉴权（探测草稿的合法形态）
	}
	h.Set(loomyTokenHeader, secret)
	h.Set("Authorization", "Bearer "+secret)
}

// isLoomyReservedHeader 报告头名是否属于 Loomy 的平台契约（适配器独占写入）。
func isLoomyReservedHeader(name string) bool {
	n := strings.ToLower(strings.TrimSpace(name))
	return n == loomyTokenHeader || n == "authorization"
}

// loomyBizError 识别 `{code,desc,data}` 业务信封。
//
// 判定顺序很关键：**先看有没有 OpenAI 的正常载荷**（聊天 `choices` / 目录
// `data`），有就说明这是正常响应，绝不能因为 body 里恰好有 `code` 字段就翻成
// 错误（某些网关会在正常响应里带 code）。业务码接受字符串或数字形态。
func loomyBizError(body []byte) (code, desc string, ok bool) {
	if len(body) == 0 {
		return "", "", false
	}
	var probe struct {
		Code    json.RawMessage `json:"code"`
		Desc    string          `json:"desc"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
		Choices json.RawMessage `json:"choices"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return "", "", false
	}
	if len(probe.Data) > 0 || len(probe.Choices) > 0 {
		return "", "", false
	}
	if len(probe.Code) == 0 {
		return "", "", false
	}
	code = decodeLoomyCode(probe.Code)
	if code == "" {
		return "", "", false
	}
	desc = strings.TrimSpace(probe.Desc)
	if desc == "" {
		desc = strings.TrimSpace(probe.Message)
	}
	return code, desc, true
}

// decodeLoomyCode 把 code 字段（字符串或数字）解成字符串；无法解析返回空串。
func decodeLoomyCode(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return strings.TrimSpace(s)
	}
	var n json.Number
	if err := json.Unmarshal(raw, &n); err == nil {
		return n.String()
	}
	return ""
}

// normalizeLoomyCode 归一业务码：去掉前导零（`020002` → `20002`），
// 这样数字形态的 `20002` 与字符串形态的 `020002` 落在同一个判定上。
func normalizeLoomyCode(code string) string {
	c := strings.TrimSpace(code)
	if c == "" {
		return ""
	}
	trimmed := strings.TrimLeft(c, "0")
	if trimmed == "" {
		return "0" // 全零（如 "000000" 成功码）归一为 "0"
	}
	return trimmed
}

// loomyNormalizeError Loomy 的**唯一**错误归一入口（ConvertResponse 与
// NormalizeStatus 共用，避免两套文案/两套判定分叉）。
//
// 判定顺序：
//  1. `{code,desc}` 业务信封（HTTP 200 也算）：
//     登录态失效码 → (401, authentication_error/session_expired)；
//     其它业务码   → (400, upstream_error/loomy_<code>)；
//  2. 裸 HTTP 401/403：无论 body 是 OpenAI 信封还是纯文本 → (401, session_expired)；
//  3. 其它 status >= 400：本身就是可解析信封的原样返回（ok=false），
//     否则包成 (原状态, upstream_error) 信封。
//
// 返回 ok=false 表示"不需要归一"（2xx 的正常响应 / 2xx 且非业务信封）。
// t 允许为 nil（NormalizeStatus 拿不到 Target）：此时只做不含字面量的通用脱敏，
// 调用方（extchan.Do）会在状态被改写后补一次带 secret 的脱敏。
func loomyNormalizeError(t *Target, status int, body []byte) (int, []byte, bool) {
	// 业务信封只在 2xx 上改写状态码：4xx/5xx 自带语义（429 尤其不能被
	// 改写成业务错误码，否则渠道冷却策略会从"限流退避"变成"请求错误"）。
	if code, desc, isBiz := loomyBizError(body); isBiz && status < 400 {
		if loomyAuthCodes[normalizeLoomyCode(code)] {
			return http.StatusUnauthorized, loomyAuthEnvelope(t, code, desc), true
		}
		// 其它业务错误 → 400：内容是审核/参数/额度这类"换证或冷却都解决不了"
		// 的问题，用 5xx 会触发 Do 的 30s 渠道冷却（把上游正常拒绝误当成故障）。
		return http.StatusBadRequest, loomyBizEnvelope(t, code, desc), true
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return http.StatusUnauthorized, loomyAuthEnvelope(t, "", loomyUpstreamMessage(body)), true
	}
	if status >= 400 {
		if isOpenAIErrorEnvelope(body) {
			return status, body, false // 已是客户端可解析形态，不二次包装
		}
		return status, ErrorEnvelope(status, fmt.Sprintf("upstream_%d", status),
			scrubForTarget(t, string(body))), true
	}
	return status, body, false
}

// loomyAuthEnvelope 登录态失效信封（type/ code 固定，文案统一）。
func loomyAuthEnvelope(t *Target, code, desc string) []byte {
	return jsonErrorEnvelope("authentication_error", "session_expired",
		scrubForTarget(t, loomyExpiredMessage(code, desc)))
}

// loomyBizEnvelope 普通业务错误信封。
func loomyBizEnvelope(t *Target, code, desc string) []byte {
	return jsonErrorEnvelope("upstream_error", "loomy_"+sanitizeLoomyCode(code),
		scrubForTarget(t, loomyBizMessage(code, desc)))
}

// loomyUpstreamMessage 从任意错误体里取一句可读文案（`desc` → `error.message`
// → `message`，非 JSON 时截断原文）。用于裸 401/403 的 message。
func loomyUpstreamMessage(body []byte) string {
	var probe struct {
		Desc  string `json:"desc"`
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &probe); err == nil {
		for _, s := range []string{probe.Desc, probe.Error.Message, probe.Message} {
			if v := strings.TrimSpace(s); v != "" {
				return truncate(v, 500)
			}
		}
		return ""
	}
	return truncate(strings.TrimSpace(string(body)), 500)
}

// isOpenAIShaped 报告 body 是否已是 OpenAI 可解析形态：
// 正常响应（choices / data）或 error 信封。用于"不二次包装"的判定。
func isOpenAIShaped(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(body, &probe); err != nil {
		return false
	}
	for _, k := range []string{"choices", "data", "error"} {
		if raw, ok := probe[k]; ok && len(raw) > 0 && string(raw) != "null" {
			return true
		}
	}
	return false
}

// isOpenAIErrorEnvelope 报告 body 是否已是 OpenAI 错误信封（含非空 error 对象）。
func isOpenAIErrorEnvelope(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	var probe struct {
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return false
	}
	return len(probe.Error) > 0 && string(probe.Error) != "null"
}

// loomyBizMessage 普通业务错误的可读文案。
func loomyBizMessage(code, desc string) string {
	msg := "Loomy 上游返回业务错误（code=" + code + "）"
	if desc != "" {
		msg += ": " + desc
	}
	return msg
}

// loomyExpiredMessage 登录态失效的可读文案（指引用户下一步动作）。
// code 为空表示"没有业务码，只有 HTTP 401/403"。
func loomyExpiredMessage(code, desc string) string {
	msg := "Loomy session 已失效或过期"
	if code != "" {
		msg += "（code=" + code + "）"
	}
	msg += "；session 有效期 14 天且上游无续期接口，请在 Loomy 客户端重新登录后更新该凭证的 secret"
	if desc != "" {
		msg += "；上游文案：" + desc
	}
	return msg
}

// sanitizeLoomyCode 业务码进 error.code 前的净化（只留字母数字与下划线）。
func sanitizeLoomyCode(code string) string {
	var b strings.Builder
	for _, r := range code {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '_':
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "unknown"
	}
	return b.String()
}

// scrubForTarget 脱敏（t 可能是 nil；Secret 为空时只做 URL query 脱敏）。
func scrubForTarget(t *Target, s string) string {
	if t == nil {
		return ScrubSecrets(s)
	}
	return ScrubSecrets(s, t.Secret)
}

// jsonErrorEnvelope 构造指定 type/code 的 OpenAI 错误信封。
// 与 ErrorEnvelope 的差别只有 type 字段（鉴权类错误要标 authentication_error）。
func jsonErrorEnvelope(errType, code, message string) []byte {
	payload := struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}{}
	payload.Error.Message = truncate(message, 2000)
	payload.Error.Type = errType
	payload.Error.Code = code
	out, err := json.Marshal(payload)
	if err != nil {
		// 理论上不可达（纯字符串结构体）；退化为通用信封，绝不返回 nil。
		return ErrorEnvelope(http.StatusOK, code, message)
	}
	return out
}
