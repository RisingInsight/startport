// qoder.go Qoder 原生适配器（v1.5.0）：中国版 `qoder` / 国际版 `qoder-intl`。
//
// 实现依据：`deliverables/software-company/starport-qoder-design.md`（v1.5.0
// 实现依据，行号引用以它为准）+ `C:/tmp/ref/qoder/`。
//
// ── 与 Loomy / AutoClaw 的三处关键差异 ──────────────────────
//  1. 认证是 **COSY 自签名**（RSA 包 AES 密钥 + MD5 签名 + 自定义 base64
//     字母表 + 请求体分段重排），每请求都有随机材料——见 qodercosy.go；
//  2. 上游**永远以流式响应**（body 里写死 stream:true，protocol.rs:409），
//     且多包一层 `{statusCodeValue, body}` 信封——非流式请求也要在
//     ConvertResponse 里把 SSE 聚合成完整 chat.completion；
//  3. 业务错误藏在信封里（HTTP 恒 200）——实现 StatusNormalizer 在状态层
//     归一（429 / 401 / 503），与 Do 的换证/冷却语义对齐。
//
// ── MVP 边界（设计 §1.2，刻意不做）──────────────────────────
// 不做 token 刷新 / PAT 兑换 / 上下文档位 / 签到 / 额度查询 / 设备授权登录 /
// 账号代理。401 → authentication_error + 提示重新粘贴；排队 → 503
// qoder_model_busy。未知名模型是**硬失败 model_not_found**（不是静默回落）。
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

// Qoder 适配器的两个注册名（config.ext_channels[].adapter）。
const (
	// QoderAdapterName 中国版（gateway.qoder.com.cn）。
	QoderAdapterName = "qoder"
	// QoderIntlAdapterName 国际版（api2/api3.qoder.sh，按令牌前缀分流）。
	QoderIntlAdapterName = "qoder-intl"
)

// qoderChatPathSuffix 对话路径（挂在我们 base 之后；sig_path 会剥掉 /algo 前缀，
// cosy.rs:252-257）。
const qoderChatPathSuffix = "/algo/api/v2/service/pro/sse/agent_chat_generation" +
	"?FetchKeys=llm_model_result&AgentId=agent_common&Encode=1"

// qoderAdapter Qoder 原生适配器（intl 区分两地区）。
type qoderAdapter struct {
	intl bool
}

// 编译期断言：Qoder 需要在状态层归一信封业务错误（HTTP 恒 200），并自带
// 静态模型目录（上游目录接口需要登录链，MVP 不接）。
var (
	_ StatusNormalizer = (*qoderAdapter)(nil)
	_ StaticModels     = (*qoderAdapter)(nil)
)

// NewQoderAdapter 构造中国版适配器。
func NewQoderAdapter() NativeAdapter { return &qoderAdapter{intl: false} }

// NewQoderIntlAdapter 构造国际版适配器。
func NewQoderIntlAdapter() NativeAdapter { return &qoderAdapter{intl: true} }

// init 注册两地区（与 Loomy / AutoClaw 同一显式登记纪律）。
func init() {
	RegisterNative(NewQoderAdapter())
	RegisterNative(NewQoderIntlAdapter())
}

// Name 实现 NativeAdapter。
func (a *qoderAdapter) Name() string {
	if a.intl {
		return QoderIntlAdapterName
	}
	return QoderAdapterName
}

// Protocol 实现 Adapter。
func (a *qoderAdapter) Protocol() Protocol { return ProtocolOpenAI }

// StaticModels 实现 StaticModels：模型清单是静态兜底表（上游目录接口需要
// 登录链，MVP 不接），FetchModels / 探测据此短路。
func (a *qoderAdapter) StaticModels() []string { return qoderStaticModelIDs(a.intl) }

// qoderCreds 从 secret 解析出的身份（设计 §3.3：secret 存整个账号 JSON）。
type qoderCreds struct {
	UserID    string
	MachineID string
	Name      string
	Email     string
	Token     string
}

// parseQoderSecret 解析凭证 JSON（credentials.rs:26-72 的键名兼容表）。
//
// secret 形态：{"mode":"global|cn","accessToken":"…","userId":"…",
// "machineId":"…","nickname":"…","email":"…"}。裸 token 不够——COSY 签名需要
// uid + machine_id。machineId 缺省按 HMAC 确定性派生（同账号跨重启稳定，
// 不回写 config）。mode 与适配器地区不一致是配置错，装配期即报。
func (a *qoderAdapter) parseQoderSecret(secret string) (qoderCreds, error) {
	trimmed := strings.TrimSpace(secret)
	if trimmed == "" || trimmed[0] != '{' {
		return qoderCreds{}, &HTTPError{Status: 400, Code: "qoder_credential_invalid",
			Message: "Qoder 凭证必须是账号 JSON（含 accessToken/userId/machineId）——" +
				"裸 token 无法参与 COSY 签名"}
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(trimmed), &payload); err != nil {
		return qoderCreds{}, &HTTPError{Status: 400, Code: "qoder_credential_invalid",
			Message: "Qoder 凭证 JSON 解析失败: " + err.Error()}
	}
	// 地区一致性（endpoints.rs:57-64 的 mode/edition；容忍 global/intl 两种写法）。
	if mode, ok := payload["mode"].(string); ok {
		mode = strings.ToLower(strings.TrimSpace(mode))
		declaredIntl := mode == "global" || mode == "intl"
		declaredCn := mode == "cn"
		if declaredIntl != a.intl && declaredCn != !a.intl {
			return qoderCreds{}, &HTTPError{Status: 400, Code: "qoder_region_mismatch",
				Message: fmt.Sprintf("Qoder 凭证地区（mode=%q）与适配器（%s）不匹配：两站令牌不通用", mode, a.Name())}
		}
	}
	token := ""
	for _, key := range []string{"accessToken", "access", "token", "access_token"} {
		if s, ok := payload[key].(string); ok && strings.TrimSpace(s) != "" {
			token = strings.TrimSpace(s)
			break
		}
	}
	token = strings.TrimPrefix(strings.TrimPrefix(token, "Bearer "), "bearer ")
	if token == "" {
		return qoderCreds{}, &HTTPError{Status: 400, Code: "qoder_credential_invalid",
			Message: "Qoder 凭证缺少 accessToken（可用键：accessToken/access/token/access_token）"}
	}
	userID := ""
	for _, key := range []string{"userId", "user_id", "uid"} {
		if s, ok := payload[key].(string); ok && strings.TrimSpace(s) != "" {
			userID = strings.TrimSpace(s)
			break
		}
	}
	if userID == "" {
		return qoderCreds{}, &HTTPError{Status: 400, Code: "qoder_credential_invalid",
			Message: "Qoder 凭证缺少 userId（COSY 签名需要用户标识）"}
	}
	machineID := ""
	for _, key := range []string{"machineId", "machine_id"} {
		if s, ok := payload[key].(string); ok && strings.TrimSpace(s) != "" {
			machineID = strings.TrimSpace(s)
			break
		}
	}
	if machineID == "" {
		machineID = qoderKeyFingerprint(secret) // 确定性派生（设计 §3.3 默认决策）
	}
	name := ""
	for _, key := range []string{"nickname", "name"} {
		if s, ok := payload[key].(string); ok && strings.TrimSpace(s) != "" {
			name = strings.TrimSpace(s)
			break
		}
	}
	email, _ := payload["email"].(string)
	creds := qoderCreds{UserID: userID, MachineID: machineID, Name: name, Email: email, Token: token}
	if err := qoderValidateIdentity(creds.UserID); err != nil {
		return qoderCreds{}, err
	}
	if err := qoderValidateIdentity(creds.MachineID); err != nil {
		return qoderCreds{}, err
	}
	return creds, nil
}

// qoderChatURL 拼 dialogue URL（chat.rs:229-234；sig_path 会剥 /algo 前缀）。
// base 由用户填推理网关主机（中国 gateway.qoder.com.cn / 国际 api2|api3.qoder.sh，
// **按令牌前缀分流是硬规则**：jt- 打到 api3 会被判 Login expired）。
func qoderChatURL(t *Target) string {
	return strings.TrimRight(strings.TrimSpace(t.BaseURL), "/") + qoderChatPathSuffix
}

// BuildRequest 实现 Adapter：协议转换 → 编码 → 签名 → 头集合。
//
// 模型未命中是**硬失败** `model_not_found`（设计 §9.5，不是静默回落）。
func (a *qoderAdapter) BuildRequest(
	ctx context.Context, t *Target, openAIBody []byte, stream bool,
) (*http.Request, error) {
	creds, err := a.parseQoderSecret(t.Secret)
	if err != nil {
		return nil, err
	}
	modelName := ExtractModel(openAIBody)
	model, ok := resolveQoderModel(a.intl, modelName)
	if !ok {
		return nil, &HTTPError{Status: 400, Code: "model_not_found",
			Message: fmt.Sprintf("模型不存在: %q。Qoder 可用模型见 GET /v1/models（未知名不静默回落）", modelName)}
	}

	// ── 协议转换（chat.rs:161-216）───────────────────────────
	dec := json.NewDecoder(bytes.NewReader(openAIBody))
	dec.UseNumber() // 保住整数字面量（max_tokens / tool index 精度）
	var clientBody map[string]any
	if err := dec.Decode(&clientBody); err != nil || clientBody == nil {
		clientBody = map[string]any{}
	}
	var rawMessages []any
	if msgs, ok := clientBody["messages"].([]any); ok {
		rawMessages = msgs
	}
	finalMessages := qoderFinalMessages(rawMessages)
	tools := qoderNormalizeTools(clientBody["tools"])
	thinking := qoderResolveThinking(clientBody, model)
	upstreamBody := qoderBuildUpstreamBody(model, finalMessages, tools,
		qoderMaxTokensOf(clientBody), thinking, creds.UserID, qoderSessionSeedOf(clientBody))

	// ── 编码 → 签名（顺序不能颠倒：签名覆盖编码后的字节）────────
	raw, err := json.Marshal(upstreamBody)
	if err != nil {
		return nil, &HTTPError{Status: 500, Code: "qoder_sign_error", Message: "Qoder 请求体序列化失败"}
	}
	encoded := qoderEncodeBody(raw)
	url := qoderChatURL(t)
	headers, err := BuildAuthHeaders(encoded, url, CosyIdentity{
		UserID:    creds.UserID,
		AuthToken: creds.Token,
		Name:      creds.Name,
		Email:     creds.Email,
		MachineID: creds.MachineID,
	}, nil)
	if err != nil {
		return nil, err
	}
	// 传输头与模型头（chat.rs:243-256）。
	headers = append(headers,
		[2]string{"Content-Type", "application/json"},
		[2]string{"Accept", "text/event-stream"},
		[2]string{"Cache-Control", "no-cache"},
		[2]string{"Accept-Encoding", "identity"},
		[2]string{"X-Model-Key", model.Key},
		[2]string{"X-Model-Source", "system"},
	)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(encoded))
	if err != nil {
		return nil, &HTTPError{Status: 500, Code: "qoder_build_error", Message: "构造 Qoder 请求失败: " + err.Error()}
	}
	for _, h := range headers {
		req.Header.Set(h[0], h[1])
	}
	return req, nil
}

// NormalizeStatus 实现 StatusNormalizer。
//
// 上游业务错误**不体现在 HTTP 状态码上**（HTTP 恒 200），都在 SSE 信封的
// statusCodeValue 里——HTTP 200 也要扫一遍信封；HTTP 层的 4xx/5xx 同样按
// 分类链归一（403 可能是排队、额度或鉴权，只看状态码必然误判，errors.rs:271-282）。
func (a *qoderAdapter) NormalizeStatus(status int, body []byte) (int, []byte) {
	if classified, raw := qoderScanBodyForBusinessError(status, body); classified != nil {
		return qoderMappedStatus(classified.kind), qoderErrorEnvelope(*classified, raw)
	}
	if status < 400 {
		return status, body
	}
	classified, raw := qoderClassifyUpstreamStatus(status, string(body))
	return qoderMappedStatus(classified.kind), qoderErrorEnvelope(classified, raw)
}

// qoderScanBodyForBusinessError 在 HTTP 200 的 SSE 文本里找信封业务错误
// （stream.rs:71-88）。找到返回分类与原文；没有返回 nil。
func qoderScanBodyForBusinessError(status int, body []byte) (*qoderClassified, string) {
	if status >= 400 {
		return nil, "" // HTTP 层错误交给 qoderClassifyUpstreamStatus
	}
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		line = strings.TrimPrefix(line, "data:")
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		ev := qoderParseSSELine(line)
		if ev.kind == qoderEventError {
			c := ev.classified
			return &c, ev.raw
		}
	}
	return nil, ""
}

// qoderClassifyUpstreamStatus HTTP 层错误分类（chat.rs:326-358 的 http_error）。
func qoderClassifyUpstreamStatus(status int, text string) (qoderClassified, string) {
	c := qoderClassifyUpstreamError(status, text)
	detail := text
	if len(detail) > 300 {
		detail = detail[:300]
	}
	return c, detail
}

// qoderMappedStatus 分类 → 网关状态码（chat.rs:343-352, 797-801）。
// 排队是 **503**：它既不是凭证问题（不能 401 触发刷新）也不是账号额度问题
// （不能 429 触发换号）；额度/限流 429；鉴权与裸 403 → 401；其余 502。
func qoderMappedStatus(kind qoderUpstreamKind) int {
	switch kind {
	case qoderKindQueued:
		return http.StatusServiceUnavailable
	case qoderKindQuota, qoderKindRate:
		return http.StatusTooManyRequests
	case qoderKindAuth, qoderKindForbidden:
		return http.StatusUnauthorized
	default:
		return http.StatusBadGateway
	}
}

// qoderEnvelopeCode 分类 → error.code（探测路径的 error_code 与信封共用）。
func qoderEnvelopeCode(c qoderClassified) string {
	switch c.kind {
	case qoderKindQueued:
		return "qoder_model_busy"
	case qoderKindAuth:
		return "token_invalid"
	case qoderKindForbidden:
		return "token_invalid"
	case qoderKindQuota, qoderKindRate:
		return "qoder_quota_exceeded"
	default:
		return "upstream_error"
	}
}

// qoderEnvelopeMessage 分类 → 错误文案（契约照 chat.rs:333-342）。
func qoderEnvelopeMessage(c qoderClassified, raw string) string {
	detail := raw
	if len(detail) > 300 {
		detail = detail[:300]
	}
	switch c.kind {
	case qoderKindQueued:
		return c.message + "（上游原文：" + detail + "）"
	case qoderKindAuth:
		message := c.message + "；请重新获取 Qoder 账号 JSON 并更新该凭证的 secret"
		if detail != "" {
			message += "；上游原文：" + detail
		}
		return message
	case qoderKindForbidden:
		message := c.message + "；请确认凭证有效、地区选择正确（两站令牌不通用）"
		if detail != "" {
			message += "；上游原文：" + detail
		}
		return message
	case qoderKindQuota, qoderKindRate:
		message := c.message
		if c.pricingURL != "" {
			message += " 套餐与额度：" + c.pricingURL
		}
		return message + "（上游原文：" + detail + "）"
	default:
		return "上游请求失败（上游原文：" + detail + "）"
	}
}

// qoderErrorEnvelope 分类结果 → OpenAI 错误信封（文案契约照 chat.rs:333-342）。
func qoderErrorEnvelope(c qoderClassified, raw string) []byte {
	return jsonErrorEnvelope(qoderEnvelopeType(c), qoderEnvelopeCode(c), qoderScrub(qoderEnvelopeMessage(c, raw)))
}

// qoderEnvelopeType 分类 → error.type。
func qoderEnvelopeType(c qoderClassified) string {
	switch c.kind {
	case qoderKindAuth, qoderKindForbidden:
		return "authentication_error"
	default:
		return "upstream_error"
	}
}

// qoderScrub 脱敏（COSY 材料里含 token，错误文案绝不能带出去）。
// NormalizeStatus 拿不到 Target，这里只能做通用脱敏（URL query + 已知形态），
// Do 会在状态被改写后再补一次带 secret 的脱敏。
func qoderScrub(message string) string {
	return ScrubSecrets(message)
}

// ConvertResponse 实现 Adapter：非流式聚合。
//
// 上游**永远以流式响应**（请求体写死 stream:true，protocol.rs:409），所以
// 非流式请求拿到的 body 是整段 SSE 文本——在这里聚合成完整 chat.completion。
// status >= 400 时 NormalizeStatus 已归一成信封，原样透传。
func (a *qoderAdapter) ConvertResponse(t *Target, status int, body []byte) []byte {
	if status >= 400 {
		return body
	}
	return a.aggregateSSE(body, t)
}

// ConvertStream 实现 Adapter：逐帧解包信封 + 翻译成 OpenAI chunk，**含首帧预读**
// （设计 §4.3 / mod.rs:551-575，issue #8）：HTTP 200 不直接返回成功——预读首个
// SSE 事件，业务错误在向客户端写任何字节之前转 typed error（换证无损）。
func (a *qoderAdapter) ConvertStream(t *Target, contentType string, body io.ReadCloser) (io.ReadCloser, error) {
	// 客户端请求的模型名（ClientModel = ext:<渠道>/<模型>）+ 思考开关
	// （模型不支持思考时不启用标签拆解器——正文里的尖括号是用户内容）。
	modelName := clientModelOf(t)
	if i := strings.LastIndex(modelName, "/"); i >= 0 {
		modelName = modelName[i+1:]
	}
	model, resolvable := resolveQoderModel(a.intl, modelName)
	tr := newQoderTranslator(t, modelName, resolvable && model.Reasoning)
	var streamErr error
	handle := func(f sseFrame) ([][]byte, bool, error) {
		ev := qoderParseSSELine(f.Data)
		switch ev.kind {
		case qoderEventSkip:
			return nil, false, nil
		case qoderEventDone:
			return append(tr.finalizeFrames(), sseDone()), true, nil
		case qoderEventError:
			streamErr = &HTTPError{
				Status:  qoderMappedStatus(ev.classified.kind),
				Code:    qoderEnvelopeCode(ev.classified),
				Message: ScrubSecrets(qoderEnvelopeMessage(ev.classified, ev.raw)),
			}
			return nil, true, streamErr
		case qoderEventChunk:
			return tr.consumeFrames(ev.chunk), false, nil
		}
		return nil, false, nil
	}
	reader := newTransformReader(body, handle, func() [][]byte {
		return append(tr.finalizeFrames(), sseDone())
	})
	// 首帧预读：读到第一个可见字节或 typed error 才返回。
	head := &bytes.Buffer{}
	tmp := make([]byte, 32*1024)
	for head.Len() == 0 {
		n, err := reader.Read(tmp)
		if n > 0 {
			head.Write(tmp[:n])
		}
		if err != nil {
			if head.Len() > 0 {
				break // 错误前已产出内容：交给调用方边读边收尾
			}
			return nil, err // 业务错误：此刻还没有任何字节下发，换证无损
		}
	}
	return &qoderPrefixedReader{head: head.Bytes(), rest: reader}, nil
}

// qoderPrefixedReader 先吐预读缓冲、再接转换读取器。
type qoderPrefixedReader struct {
	head []byte
	rest io.ReadCloser
}

func (r *qoderPrefixedReader) Read(p []byte) (int, error) {
	if len(r.head) > 0 {
		n := copy(p, r.head)
		r.head = r.head[n:]
		return n, nil
	}
	return r.rest.Read(p)
}

func (r *qoderPrefixedReader) Close() error { return r.rest.Close() }

// BuildModels 实现 Adapter：仅为接口完备性保留（Qoder 没有可拉取的目录，
// StaticModels 才是真实来源）。
func (a *qoderAdapter) BuildModels(ctx context.Context, t *Target) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimRight(strings.TrimSpace(t.BaseURL), "/")+"/models", nil)
	if err != nil {
		return nil, &HTTPError{Status: 500, Code: "qoder_build_error", Message: "构造 Qoder 目录请求失败"}
	}
	req.Header.Set("Accept", "application/json")
	return req, nil
}

// DecodeModels 实现 Adapter：防御性路径（生产走 StaticModels）。
func (a *qoderAdapter) DecodeModels(_ *Target, status int, body []byte) ([]string, error) {
	if status != http.StatusOK {
		return nil, NewHTTPError(status, "models_upstream_error", truncate(string(body), 300))
	}
	var parsed struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("parse models: %w", err)
	}
	out := make([]string, 0, len(parsed.Data))
	for _, d := range parsed.Data {
		if id := strings.TrimSpace(d.ID); id != "" {
			out = append(out, id)
		}
	}
	return out, nil
}

// ── Translator：上游 chunk → 客户端 delta（chat.rs:381-758）──────

// qoderToolCallState 工具调用按 index 累积。
type qoderToolCallState struct {
	id, name, arguments string
}

// qoderToolDelta 本轮新增的一条工具调用增量。
type qoderToolDelta struct {
	index     int64
	id        string
	name      string
	arguments string
}

// qoderTranslator 流式与非流式共用的翻译状态（解析与拆解规则只写一份，
// 两条出口各自决定怎么消费产出——chat.rs:381-387）。
//
// pending* 是"本轮 consume 新增的增量"：流式按它出帧，非流式聚合成完整体。
type qoderTranslator struct {
	responseID string
	created    int64
	modelName  string
	toolState  map[int64]*qoderToolCallState
	roleSent   bool
	parser     *qoderThinkingParser // nil = 模型不支持思考，正文不拆解
	usage      any
	finish     string
	content    string
	reasoning  string

	pendingContent   string           // 本轮新增正文
	pendingReasoning string           // 本轮新增思考
	pendingToolCalls []qoderToolDelta // 本轮新增工具调用增量
}

// newQoderTranslator 构造。modelName 是客户端请求的目录名（model_out 固定回它，
// 理由见 qoderModelOut 的注释）。
func newQoderTranslator(t *Target, modelName string, thinkingEnabled bool) *qoderTranslator {
	var parser *qoderThinkingParser
	if thinkingEnabled {
		parser = newQoderThinkingParser()
	}
	return &qoderTranslator{
		responseID: "chatcmpl-qoder-" + qoderRandomUUID(),
		created:    time.Now().Unix(),
		modelName:  modelName,
		toolState:  map[int64]*qoderToolCallState{},
		parser:     parser,
	}
}

// consume 消费一个上游 chunk（内层 OpenAI chunk；chat.rs:467-568）。
func (tr *qoderTranslator) consume(chunk map[string]any) {
	// 上游回的 model 是路由档位（请求 Qwen3.8-Flash 实测回 "auto"）：
	// model_out 固定回请求名，不按它映射（chat.rs:428-441）。
	if usage, ok := chunk["usage"].(map[string]any); ok {
		tr.usage = usage // usage 取最后一次出现
	}
	choices, _ := chunk["choices"].([]any)
	if len(choices) == 0 {
		return
	}
	choice, _ := choices[0].(map[string]any)
	if choice == nil {
		return
	}
	if finish, ok := choice["finish_reason"].(string); ok && finish != "" {
		tr.finish = finish // 取最后一次非空
	}
	delta, _ := choice["delta"].(map[string]any)
	if delta == nil {
		return
	}
	// 上游显式给出的 reasoning_content 优先（并剥掉偶带的标签；
	// chat.rs:501-508）。
	if reasoning, ok := delta["reasoning_content"].(string); ok && reasoning != "" {
		cleaned := qoderStripThinkingTags(reasoning)
		if cleaned != "" {
			tr.reasoning += cleaned
			tr.pendingReasoning += cleaned
		}
	}
	if content, ok := delta["content"].(string); ok && content != "" {
		if tr.parser != nil {
			// 思考可能混在正文里：按标签拆开。拆解器跨 chunk 长驻——
			// 标签前缀尾巴留在缓冲里等下一片（chat.rs:511-535）。
			tr.parser.push(content)
			for _, piece := range tr.parser.take() {
				if piece.isThinking {
					tr.reasoning += piece.text
					tr.pendingReasoning += piece.text
				} else {
					tr.content += piece.text
					tr.pendingContent += piece.text
				}
			}
		} else {
			tr.content += content
			tr.pendingContent += content
		}
	}
	if calls, ok := delta["tool_calls"].([]any); ok {
		for _, c := range calls {
			cm, isMap := c.(map[string]any)
			if !isMap {
				continue
			}
			var index int64
			switch n := cm["index"].(type) {
			case json.Number:
				index, _ = n.Int64()
			case float64:
				index = int64(n)
			}
			entry := tr.toolState[index]
			if entry == nil {
				entry = &qoderToolCallState{}
				tr.toolState[index] = entry
			}
			var id, name, args string
			if id, ok = cm["id"].(string); ok && id != "" {
				entry.id = id
			}
			if fn, ok := cm["function"].(map[string]any); ok {
				if name, ok = fn["name"].(string); ok && name != "" {
					entry.name = name
				}
				if args, ok = fn["arguments"].(string); ok {
					entry.arguments += args
				}
			}
			// 增量帧只在有内容时发（与源实现的 delta 形态一致）。
			if id != "" || name != "" || args != "" {
				tr.pendingToolCalls = append(tr.pendingToolCalls, qoderToolDelta{
					index: index, id: id, name: name, arguments: args,
				})
			}
		}
	}
}

// flushParser 收尾：冲刷解析器缓冲（必须在流结束调用——解析器留着"可能是
// 标签前缀"的尾巴，不冲刷回答末尾会少几个字符；chat.rs:570-593）。幂等。
func (tr *qoderTranslator) flushParser() {
	if tr.parser == nil {
		return
	}
	tr.parser.finish()
	for _, piece := range tr.parser.take() {
		if piece.isThinking {
			tr.reasoning += piece.text
			tr.pendingReasoning += piece.text
		} else {
			tr.content += piece.text
			tr.pendingContent += piece.text
		}
	}
}

// finalizeFrames 流结束的收尾帧：冲刷解析器 → 余量 delta → finish_reason 帧 →
// usage 帧（chat.rs:652-680）。
func (tr *qoderTranslator) finalizeFrames() [][]byte {
	tr.flushParser()
	frames := tr.deltasToFrames()
	finish := "stop"
	if len(tr.toolState) > 0 {
		finish = "tool_calls" // 有工具调用就是 tool_calls（chat.rs:644-650）
	} else if tr.finish != "" {
		finish = tr.finish
	}
	frames = append(frames, sseData(tr.chunkJSON(map[string]any{}, &finish)))
	usage := tr.usage
	if usage == nil {
		usage = map[string]any{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0}
	}
	frames = append(frames, sseData(qoderJSONOf(map[string]any{
		"id":      tr.responseID,
		"object":  "chat.completion.chunk",
		"created": tr.created,
		"model":   tr.modelName,
		"choices": []any{},
		"usage":   usage,
	})))
	return frames
}

// deltasToFrames 把本轮新增量合成 OpenAI chunk 帧。
// （增量帧携带本轮 content / reasoning_content / tool_calls 的新增量；
// 语义与逐段下发等价，帧数更少。）
func (tr *qoderTranslator) deltasToFrames() [][]byte {
	var frames [][]byte
	if tr.pendingReasoning != "" {
		frames = append(frames, sseData(tr.chunkJSON(map[string]any{"reasoning_content": tr.pendingReasoning}, nil)))
		tr.pendingReasoning = ""
	}
	if tr.pendingContent != "" {
		frames = append(frames, sseData(tr.chunkJSON(map[string]any{"content": tr.pendingContent}, nil)))
		tr.pendingContent = ""
	}
	for _, call := range tr.pendingToolCalls {
		delta := map[string]any{}
		if call.id != "" {
			delta["id"] = call.id
		}
		function := map[string]any{}
		if call.name != "" {
			function["name"] = call.name
		}
		if call.arguments != "" {
			function["arguments"] = call.arguments
		}
		delta["index"] = json.Number(fmt.Sprintf("%d", call.index))
		delta["type"] = "function"
		delta["function"] = function
		frames = append(frames, sseData(tr.chunkJSON(map[string]any{
			"tool_calls": []any{delta},
		}, nil)))
	}
	tr.pendingToolCalls = nil
	return frames
}

// consumeFrames 一个上游 chunk → 若干 OpenAI chunk 帧（流式下发用）。
// 首个产出 delta 前补一帧 role（chat.rs:606-613）。
func (tr *qoderTranslator) consumeFrames(chunk map[string]any) [][]byte {
	tr.consume(chunk)
	var frames [][]byte
	if !tr.roleSent && (tr.pendingContent != "" || tr.pendingReasoning != "" || len(tr.pendingToolCalls) > 0) {
		tr.roleSent = true
		frames = append(frames, sseData(tr.chunkJSON(map[string]any{"role": "assistant", "content": ""}, nil)))
	}
	frames = append(frames, tr.deltasToFrames()...)
	return frames
}

// chunkJSON OpenAI chat.completion.chunk 帧（chat.rs:652-668）。
func (tr *qoderTranslator) chunkJSON(delta map[string]any, finish *string) []byte {
	var finishValue any
	if finish != nil {
		finishValue = *finish
	}
	return qoderJSONOf(map[string]any{
		"id":      tr.responseID,
		"object":  "chat.completion.chunk",
		"created": tr.created,
		"model":   tr.modelName,
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         delta,
			"finish_reason": finishValue,
		}},
	})
}

// qoderJSONOf 紧凑 JSON（失败退 {}，绝不 panic）。
func qoderJSONOf(v any) []byte {
	out, err := json.Marshal(v)
	if err != nil {
		return []byte("{}")
	}
	return out
}

// aggregateSSE 非流式聚合（chat.rs:682-720 的 completion_body）。
// 上游永远以流式响应，非流式请求在这里把整段 SSE 聚合成完整 chat.completion。
func (a *qoderAdapter) aggregateSSE(body []byte, t *Target) []byte {
	modelName := clientModelOf(t)
	if i := strings.LastIndex(modelName, "/"); i >= 0 {
		modelName = modelName[i+1:]
	}
	model, resolvable := resolveQoderModel(a.intl, modelName)
	tr := newQoderTranslator(t, modelName, resolvable && model.Reasoning)
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		line = strings.TrimPrefix(line, "data:")
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		ev := qoderParseSSELine(line)
		if ev.kind == qoderEventChunk {
			tr.consume(ev.chunk)
		}
	}
	tr.flushParser()
	message := map[string]any{"role": "assistant"}
	if tr.content == "" {
		message["content"] = nil
	} else {
		message["content"] = tr.content
	}
	if tr.reasoning != "" {
		message["reasoning_content"] = tr.reasoning
	}
	var toolCalls []any
	for index, state := range tr.toolState {
		args := state.arguments
		if args == "" {
			args = "{}"
		}
		id := state.id
		if id == "" {
			id = fmt.Sprintf("call_%d", index)
		}
		toolCalls = append(toolCalls, map[string]any{
			"id":       id,
			"type":     "function",
			"function": map[string]any{"name": state.name, "arguments": args},
		})
	}
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
	}
	finish := "stop"
	if len(tr.toolState) > 0 {
		finish = "tool_calls"
	} else if tr.finish != "" {
		finish = tr.finish
	}
	usage := tr.usage
	if usage == nil {
		usage = map[string]any{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0}
	}
	return qoderJSONOf(map[string]any{
		"id":      tr.responseID,
		"object":  "chat.completion",
		"created": tr.created,
		"model":   tr.modelName,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       message,
			"finish_reason": finish,
		}},
		"usage": usage,
	})
}
