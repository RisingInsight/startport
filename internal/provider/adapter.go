// adapter.go 适配器接口、出站目标投影、注册表与 URL 构建。
//
// Adapter 的职责边界：**只做协议**。它把 canonical OpenAI Chat Completions
// JSON 编码成上游协议形态（含 URL/鉴权/静态头），把上游响应解码回 OpenAI
// 形态。它不碰渠道/凭证状态、不重试、不记账（那些在 internal/extchan）。
package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Target 一次出站请求的最终目标（渠道定义 + 选中凭证的投影，秘密值已在其中）。
// 只在进程内存中存在；任何日志/接口都不得打印 Secret。
type Target struct {
	// Name 渠道名（观测/日志）。
	Name string
	// Protocol 出站协议。
	Protocol Protocol
	// BaseURL 上游根地址（config.ext_channels[].base_url 原值）。
	BaseURL string
	// Path 自定义路径覆盖（空 = 协议默认模板；支持 {model} 占位）。
	Path string
	// Headers 渠道级静态头（不含秘密值；校验层已拒绝传输层头）。
	Headers map[string]string
	// Auth 生效的鉴权规格（凭证级覆盖渠道级后的结果）。
	Auth AuthSpec
	// Secret 选中凭证的秘密值（内存；日志/接口永不输出）。
	Secret string
	// Timeout 单次上游往返上限（连接+响应头）。<=0 由调用方回落 120s。
	Timeout time.Duration
	// ClientModel 面向客户端的模型名（"ext:<渠道>/<模型或别名>"）。
	// 转换响应时写回 model 字段，客户端看到的永远是它请求的名字。
	ClientModel string
}

// Adapter 一个出站协议的完整适配面。
type Adapter interface {
	// Protocol 该适配器承载的协议。
	Protocol() Protocol
	// BuildRequest 把 canonical OpenAI Chat Completions JSON 编码成上游
	// *http.Request（URL / 鉴权 / 静态头 / body 全在这里）。
	BuildRequest(ctx context.Context, t *Target, openAIBody []byte, stream bool) (*http.Request, error)
	// ConvertResponse 把非流式上游响应体转换为 OpenAI wire 形态。
	// status >= 400 时也返回 OpenAI 错误信封 {"error":{message,type,code}}，
	// 上游原文截断并脱敏（调用方可直接透传给客户端）。
	ConvertResponse(t *Target, status int, body []byte) []byte
	// ConvertStream 把上游 SSE 流包装成 OpenAI chunk SSE 流。
	// 上游明确不支持 SSE 时返回 ErrStreamUnsupported（绝不伪造流式）。
	ConvertStream(t *Target, contentType string, body io.ReadCloser) (io.ReadCloser, error)
	// BuildModels 构造模型目录探测请求（探测与自动发现共用，只读）。
	BuildModels(ctx context.Context, t *Target) (*http.Request, error)
	// DecodeModels 解析模型目录响应（宽容解析：只取 id 列表）。
	DecodeModels(t *Target, status int, body []byte) ([]string, error)
}

// StatusNormalizer 可选能力：把"状态码与 body 语义不一致"的上游响应归一。
//
// 典型场景：某些平台用 `HTTP 200 + {code,desc}` 表达业务错误（Loomy 的登录态
// 失效就是 `{"code":"020002"}`）。若只改 body 不改状态码，按状态码判定成功的
// SDK 会把错误当成功——所以需要同时改写状态码。
//
// 这是**可选接口**（不是 Adapter 的必备方法）：只有需要的适配器实现它，
// 三个协议基准适配器不实现，行为完全不变。
//
// 调用方约定：
//   - 只在**非流式**响应上调用（流式 body 是流，预读会破坏逐字节透传）；
//   - 返回 (原 status, 原 body) 表示"无需归一"；
//   - 归一后的错误体应是 OpenAI 错误信封，便于客户端直接解析。
type StatusNormalizer interface {
	NormalizeStatus(status int, body []byte) (int, []byte)
}

// ErrStreamUnsupported 上游不接受/不返回 SSE 流式（Gemini 未按 alt=sse 返回）。
// 调用方应把它翻译成 502 gemini_stream_unsupported 并提示改用非流式。
var ErrStreamUnsupported = errors.New("upstream does not support SSE streaming")

// HTTPError 带状态码/错误码的出站错误（面板探测与转发层共用）。
type HTTPError struct {
	Status  int    // 建议的客户端状态码
	Code    string // 机器可读错误码（写入 error.code）
	Message string // 人类可读消息（已脱敏）
}

// Error 实现 error。
func (e *HTTPError) Error() string {
	if e == nil {
		return ""
	}
	if e.Code == "" {
		return e.Message
	}
	return e.Code + ": " + e.Message
}

// NewHTTPError 构造 HTTPError（快捷方式）。
func NewHTTPError(status int, code, msg string) *HTTPError {
	return &HTTPError{Status: status, Code: code, Message: msg}
}

// ---------------------------------------------------------------------------
// 注册表
// ---------------------------------------------------------------------------

// Registry 协议 → 适配器。进程内只读，装配期一次性注册（无锁）。
type Registry struct {
	m map[Protocol]Adapter
}

// NewRegistry 构造注册表（重复协议后者覆盖前者）。
func NewRegistry(adapters ...Adapter) *Registry {
	r := &Registry{m: make(map[Protocol]Adapter, len(adapters))}
	for _, a := range adapters {
		if a == nil {
			continue
		}
		r.m[a.Protocol()] = a
	}
	return r
}

// Get 按协议取适配器。
func (r *Registry) Get(p Protocol) (Adapter, bool) {
	if r == nil {
		return nil, false
	}
	a, ok := r.m[p]
	return a, ok
}

// MustGet 按协议取适配器；未知协议 panic（装配期暴露，不带病到请求期）。
func (r *Registry) MustGet(p Protocol) Adapter {
	a, ok := r.Get(p)
	if !ok {
		panic("provider: no adapter registered for protocol " + string(p))
	}
	return a
}

// DefaultRegistry 内置三件套（openai / anthropic / gemini）。
var DefaultRegistry = NewRegistry(
	NewOpenAIAdapter(),
	NewAnthropicAdapter(),
	NewGeminiAdapter(),
)

// ---------------------------------------------------------------------------
// URL 构建
// ---------------------------------------------------------------------------

// LegacyOpenAIRoot 逐字节复刻历史 endpoint() 的 openai 分支
// （internal/extchan/extchan.go 阶段一的实现），含"base 里含 /v1 就不补"的
// 历史怪规则——老配置的行为不能变（含 /openai/v1、/api/v3 等形态）。
//
// 这是一条刻意保留的**历史兼容 shim**：不要为新协议复用它，新协议走 ensureVer。
func LegacyOpenAIRoot(base string) string {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if strings.HasSuffix(base, "/v1") {
		return base
	}
	if strings.Contains(base, "/v1") {
		// 形如 https://host/openai/v1 → 已含版本段，不再补。
		return base
	}
	return base + "/v1"
}

// ensureVer base 已以 /v1、/v1beta、/v1alpha 结尾则原样返回，否则追加 ver。
// 用于 anthropic（/v1）与 gemini（/v1beta）这类固定版本段的协议。
func ensureVer(base, ver string) string {
	b := strings.TrimRight(strings.TrimSpace(base), "/")
	for _, v := range []string{"/v1", "/v1beta", "/v1alpha"} {
		if strings.HasSuffix(b, v) {
			return b
		}
	}
	if ver == "" {
		return b
	}
	return b + ver
}

// renderPath 渲染 path 模板（{model} 占位替换为 URL 转义后的上游模型名）。
func renderPath(path, model string) string {
	out := strings.ReplaceAll(path, "{model}", url.PathEscape(model))
	return out
}

// BuildChatURL 按协议拼上游聊天 URL（导出给测试与探测复用）。
//
//   - Path 非空：base + render(path)（不再追加版本段——用户对完整路径负责，
//     这是火山 Ark ".../api/v3" 这类非 /v1 形态的唯一出路）；
//   - Path 为空：协议默认模板。
func BuildChatURL(t *Target, model string, stream bool) string {
	if t == nil {
		return ""
	}
	if p := strings.TrimSpace(t.Path); p != "" {
		return strings.TrimRight(strings.TrimSpace(t.BaseURL), "/") + renderPath(p, model)
	}
	base := strings.TrimRight(strings.TrimSpace(t.BaseURL), "/")
	switch t.Protocol {
	case ProtocolGemini:
		root := ensureVer(base, "/v1beta")
		p := "/models/" + url.PathEscape(model)
		if stream {
			return root + p + ":streamGenerateContent?alt=sse"
		}
		return root + p + ":generateContent"
	case ProtocolAnthropic:
		return ensureVer(base, "/v1") + "/messages"
	default:
		return LegacyOpenAIRoot(base) + "/chat/completions"
	}
}

// BuildModelsURL 按协议拼模型目录 URL（探测/自动发现用）。
//
// 显式 path 覆盖时（火山 Ark `/api/v3` 这类非 /v1 形态），模型目录按
// `<base>/models` 推断——比历史 shim 的 `<base>/v1/models` 更符合实际；
// 推断不中时用户显式配 `models` 即可（自动发现失败不阻塞）。
func BuildModelsURL(t *Target) string {
	if t == nil {
		return ""
	}
	base := strings.TrimRight(strings.TrimSpace(t.BaseURL), "/")
	if p := strings.TrimSpace(t.Path); p != "" {
		return base + "/models"
	}
	switch t.Protocol {
	case ProtocolGemini:
		return ensureVer(base, "/v1beta") + "/models"
	case ProtocolAnthropic:
		return ensureVer(base, "/v1") + "/models"
	default:
		return LegacyOpenAIRoot(base) + "/models"
	}
}

// ApplyHeaders 把协议默认头、静态头、鉴权注入到请求上（三个适配器共用）。
//
// 顺序：协议默认（Content-Type/Accept/协议版本头）→ 静态 headers（校验层已
// 拒绝传输层头）→ 鉴权（Auth.Apply 内部对已有头不覆盖）。
func ApplyHeaders(req *http.Request, t *Target, stream bool, extra map[string]string) {
	h := req.Header
	h.Set("Content-Type", "application/json")
	if stream {
		h.Set("Accept", "text/event-stream")
	} else {
		h.Set("Accept", "application/json")
	}
	for k, v := range extra {
		if k == "" || v == "" || HeaderForbidden(k) {
			continue
		}
		h.Set(k, v)
	}
	for k, v := range t.Headers {
		if k == "" || HeaderForbidden(k) {
			continue
		}
		h.Set(k, v)
	}
	// 空 mode 按协议默认解析（openai→bearer / anthropic→x-api-key /
	// gemini→x-goog-api-key），老配置（只有 api_key）因此行为不变。
	Apply(h, req.URL, t.Auth.Normalized(t.Protocol), t.Secret)
}

// Doer 出站请求执行器抽象（测试可注入）。
type Doer interface {
	Do(req *http.Request) (*http.Response, error)
}

// LimitedBody 读取上限包装（8MiB：模型目录/错误体都足够；防异常上游撑爆内存）。
func LimitedBody(r io.Reader) []byte {
	b, _ := io.ReadAll(io.LimitReader(r, 8<<20))
	return b
}

// DetectStreamUnsupported 判定上游是否明确不支持 SSE（Gemini 分支用）。
// contentType 非 text/event-stream 即视为不支持——不猜、不伪流式。
func DetectStreamUnsupported(contentType string) bool {
	ct := strings.ToLower(strings.TrimSpace(contentType))
	if ct == "" {
		return false // 上游没给 Content-Type：宽容地按 SSE 尝试解析
	}
	return !strings.Contains(ct, "text/event-stream")
}

// ErrorEnvelope 构造 OpenAI 错误信封 JSON（上游原文先截断后脱敏）。
func ErrorEnvelope(status int, code, message string) []byte {
	msg := truncate(message, 2000)
	return []byte(fmt.Sprintf(
		`{"error":{"message":%q,"type":"upstream_error","code":%q}}`, msg, code))
}

// truncate 截断字符串到 n 字节（不切坏 UTF-8：退到最后一个完整 rune 边界）。
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := s[:n]
	for len(cut) > 0 {
		if r := cut[len(cut)-1]; r < 0x80 || r >= 0xC0 {
			break // ASCII 或 rune 首字节边界
		}
		cut = cut[:len(cut)-1]
	}
	return cut + "…"
}
