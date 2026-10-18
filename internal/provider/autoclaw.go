// autoclaw.go AutoClaw（智谱 autoglm）原生适配器：国内版 `autoclaw` 与国际版
// `autoclaw-intl` 两个实例（同一套协议代码，两套站点）。
//
// ── 上游形态（移植来源：autoclaw-local-proxy 源实现，逐条核对）──
//
//	chat   POST {base}/chat/completions     （base 已含 /autoclaw-proxy/proxy/autoclaw）
//	认证   X-Authorization: Bearer <token>  （**不是** Authorization——规格文字
//	        写错了，源实现与桌面端 openclaw.json 都是 X-Authorization）
//	双模型标识  X-Request-Model = 完整路由 ID（决定计费通道：zai_ 会员/按量、
//	        zaicoding_ Coding Plan、zai_auto 自动选型）；body.model = 剥前缀的模型 ID
//
// ── 上游 2026-09 起的几道闸（本文件的头集合与 body 改写都由它们而来）──
//  1. `X-Harness-Type: zcode` 会被区别对待（403 pay-view / 406 空体）——**不发**；
//  2. system 白名单：必须以 OpenClaw 身份句开头且带 `## Tooling` 段，且不得含
//     外来 harness 身份句（见 autoclawPromptNormalize）；
//  3. `max_tokens` 预校验：30..8192 的小正整数一律 406 空响应体——出站前归一；
//  4. `X-Tm: linux` 会被部分账号 403——恒不发 linux（服务端跑在 Linux 上，
//     官方客户端只有 win/mac）。
//
// ── 与 Loomy 样板的三处刻意差异 ────────────────────────────
//  1. body **重编码**：model / max_tokens / system 三处都要改写，字节透传做
//     不到（Loomy 三处都不改，所以能逐字节透传）；
//  2. **SSE 帧重建**：上游会回自己的路由名（`zai_auto` 等），源实现逐帧回写
//     客户端请求的名字——本适配器的 ConvertStream 用 transformReader 改写
//     model 字段（Loomy/OpenAI 是逐字节透传，AutoClaw 是声明的例外）；
//  3. **不实现 StatusNormalizer**：AutoClaw 没有"HTTP 200 + 业务错误码"的
//     形态（错误都走真实 HTTP 状态码），状态与 body 语义一致，无需改状态。
//
// ── 刻意不做的事（与参考实现对齐，别顺手补）─────────────────
//   - 不做登录 / 签到 / 积分 / token 刷新：token 由用户手工粘贴，401 即失效，
//     需重新获取（上游的 refresh 接口需要 X-Auth-Sign 客户端签名，本期不做）；
//   - 不接远程模型目录（`GET .../proxy/autoclaw-model-config` 需要签名头），
//     模型走静态路由表 + StaticModels 接口；
//   - 不透传 ZCode 会话头（X-Session-Id 等三个）：网关不转发客户端头，
//     需要时用户可配在渠道 headers 里（会照常合并）。
package provider

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strings"
	"time"
)

// AutoClaw 适配器的两个注册名（config.ext_channels[].adapter）。
const (
	// AutoClawAdapterName 国内版（智谱加速域）。
	AutoClawAdapterName = "autoclaw"
	// AutoClawIntlAdapterName 国际版（autoglm.ai）。
	AutoClawIntlAdapterName = "autoclaw-intl"
)

// autoclawChatPath 聊天路径（base 已含 /autoclaw-proxy/proxy/autoclaw）。
const autoclawChatPath = "/chat/completions"

// 上游识别的客户端形态常量（源实现 brandHeaders 的照抄值；实测无害才保留，
// 改动头集合要一次只改一个变量再实测）。
const (
	autoclawAppVersion = "1.17.8" // X-Version：上游按它识别客户端版本
	autoclawLang       = "zh-CN"  // X-Lang
	autoclawChannel    = "official"
	autoclawTM         = "win" // X-Tm：恒 win（linux 会被部分账号 403）
	autoclawAuthHeader = "X-Authorization"
)

// autoclawDefaultRoute 默认路由 ID（未给 model / 未知模型时的回退）。
const autoclawDefaultRoute = "zai_auto"

// autoclawRoutePrefixes 已知路由前缀（**顺序有意义**：先长后短，否则
// `zaicoding_glm-5.3` 会被 `zai_` 错剥成 `coding_glm-5.3`）。
var autoclawRoutePrefixes = []string{"zaicoding_", "zai_"}

// autoclawMaxTokensFloor max_tokens 归一下限：上游对 30..8192 的小正整数一律
// 406 空响应体，不传或 ≥10000 则放行——取实测过的安全值 10000。
const autoclawMaxTokensFloor = 10000

// autoclawStaticModels 静态模型路由表（对齐实际客户端当前提供的模型）。
// 顺序即展示顺序；`auto*` 两个条目在客户端已下架，不进目录，但 `zai_auto`
// 默认路由与 `x_yyy` 形态透传仍在（路由兜底不依赖目录条目）。
type autoclawModelEntry struct {
	ID      string // 目录 ID（客户端最常给的名字）
	RouteID string // 完整路由 ID（X-Request-Model）
	Name    string // 展示名
}

var autoclawStaticModels = []autoclawModelEntry{
	{ID: "glm-5.3", RouteID: "zaicoding_glm-5.3", Name: "GLM-5.3"},
	{ID: "glm-5.3-flash", RouteID: "zai_glm-5.3-flash", Name: "GLM-5.3-Flash"},
}

// ── 出站 system 规范化常量（上游 2026-09-22 起的白名单闸门）────────

// autoclawIdentityLine 上游要求"以它开头"的身份句（幂等判定用）。
const autoclawIdentityLine = "You are a personal assistant running inside OpenClaw."

// autoclawIdentityPrefix 身份句 + `## Tooling` 段（官方 prompt 的开头两行，
// 逐字照抄——出站形态与官方客户端一致，而不是自己编一句"看起来像"的话）。
const autoclawIdentityPrefix = "You are a personal assistant running inside OpenClaw.\n\n" +
	"## Tooling\n" +
	"Available tools are policy-filtered. Names are case-sensitive; call exactly as listed.\n"

// autoclawForeignIdentities 外来身份句 → 中性说法（**长的在前**：短的在前会把
// 长匹配串切碎，改写后仍含产品名的半吊子结果会漏网）。每条替换串都不含产品名——
// 闸门拦的是字面产品名，插词不够。匹配忽略 ASCII 大小写（闸门自己就不敏感）。
var autoclawForeignIdentities = [][2]string{
	{"You are ZCode, an interactive coding agent", "You are an interactive coding agent"},
	{"You are a coding agent running in the Codex CLI tool", "You are a coding agent running in a terminal CLI tool"},
	{"You are a coding agent running in the Codex CLI", "You are a coding agent running in a terminal CLI"},
	{"You are running as a coding agent in the Codex CLI", "You are running as a coding agent in a terminal CLI"},
	// Claude Code 的整句是 "You are Claude Code, Anthropic's official CLI tool
	// for Claude."；只改到 "You are Claude Code" 为止，后面的产品说明原样保留。
	{"You are Claude Code", "You are a coding assistant"},
	{"You are ZCode", "You are an interactive coding agent"},
	{"You are an AI agent powered by DeepSeek Harness", "You are an AI agent powered by a local coding harness"},
	// 放最后：它最短最通用（新版 Codex 首句就是它），必须让更长的先命中。
	{"You are Codex", "You are a coding agent"},
}

// autoclaw406Hint 406 空响应体的可执行提示（上游不给原因，只回 406 时客户端
// 无从下手；把实测过的三个方向列全——max_tokens 网关已归一这一条也要写明，
// 否则用户仍会先去改 max_tokens 白绕一圈）。
const autoclaw406Hint = "；上游拒收（406）且未给出原因，实测过的方向：" +
	"① 请求体 max_tokens 取小值（如 1024 / 2048 / 4096）会被上游预校验拦截 —— 网关已把小值归一到 10000，若仍复现请把客户端请求体发到项目 issue；" +
	"② system 首句命中逐字指纹拦截 —— 可在设置页「通用 → 系统提示词」切到「替换」模式绕过，或把漏网的 system 首句发到项目 issue 以便补进指纹表；" +
	"③ 请求形态被上游风控拦截（①②都排除时）—— 请把现象发到项目 issue"

// autoclawAdapter AutoClaw 原生适配器（intl 区分国内/国际两套站点）。
type autoclawAdapter struct {
	intl bool
}

// NewAutoClawAdapter 构造国内版适配器。
func NewAutoClawAdapter() NativeAdapter { return &autoclawAdapter{intl: false} }

// NewAutoClawIntlAdapter 构造国际版适配器。
func NewAutoClawIntlAdapter() NativeAdapter { return &autoclawAdapter{intl: true} }

// init 注册两个地区（显式调用，同 Loomy 的登记纪律）。
func init() {
	RegisterNative(NewAutoClawAdapter())
	RegisterNative(NewAutoClawIntlAdapter())
}

// Name 实现 NativeAdapter。
func (a *autoclawAdapter) Name() string {
	if a.intl {
		return AutoClawIntlAdapterName
	}
	return AutoClawAdapterName
}

// Protocol 实现 Adapter：报文形态是 OpenAI Chat Completions。
func (a *autoclawAdapter) Protocol() Protocol { return ProtocolOpenAI }

// StaticModels 实现 StaticModels：模型清单是静态路由表，不走 HTTP 发现
// （上游的目录接口需要客户端签名，本期不做）。
func (a *autoclawAdapter) StaticModels() []string {
	out := make([]string, 0, len(autoclawStaticModels))
	for _, m := range autoclawStaticModels {
		out = append(out, m.ID)
	}
	return out
}

// BuildRequest 实现 Adapter：头集合 + body 三处改写（model / max_tokens /
// system 规范化）。
func (a *autoclawAdapter) BuildRequest(
	ctx context.Context, t *Target, openAIBody []byte, stream bool,
) (*http.Request, error) {
	u := autoclawURL(t, autoclawChatPath)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(openAIBody))
	if err != nil {
		return nil, fmt.Errorf("build autoclaw request: %w", err)
	}
	// body 改写：成功则整体重编码（model/max_tokens/system 三处）；失败（非
	// JSON 对象）则原样透传，让上游报格式错误——比在网关里编一个空对象更能
	// 说明问题。
	route := resolveAutoClawRoute(ExtractModel(openAIBody))
	if rewritten, ok := autoclawRewriteBody(openAIBody, route.bodyModelID); ok {
		req.Body = io.NopCloser(bytes.NewReader(rewritten))
		req.ContentLength = int64(len(rewritten))
	}
	applyAutoClawHeaders(req, t, route.routeModelID)
	return req, nil
}

// ConvertResponse 实现 Adapter：错误分类 + 非流式 model 回写。
func (a *autoclawAdapter) ConvertResponse(t *Target, status int, body []byte) []byte {
	if status >= 400 {
		return autoclawErrorEnvelope(t, status, body)
	}
	// 非流式：payload.model 回写成客户端请求的名字（上游代理回自己的路由/
	// 内部名，客户端按请求名识别响应）。server 侧还会再回写一次（同一值），
	// 这里先做是为了让适配器自身语义完整（不经 server 的调用方也能拿到）。
	if cm := clientModelOf(t); cm != "" {
		if obj := autoclawDecodeObject(body); obj != nil {
			if _, ok := obj["model"]; ok {
				obj["model"] = cm
				if out, err := json.Marshal(obj); err == nil {
					return out
				}
			}
		}
	}
	return body
}

// ConvertStream 实现 Adapter：SSE 逐帧回写 model（**本适配器允许重建帧**，
// 是"逐字节透传"声明的例外——上游回自己的路由名，不回写客户端会看到
// `zai_auto` 这类名字）。
//
// 上游不回 SSE（流式请求拿到 JSON）时原样透传：与"绝不伪流式"的既有约定一致，
// 客户端表现为解析失败；面板探测可提前发现形态问题。
func (a *autoclawAdapter) ConvertStream(t *Target, contentType string, body io.ReadCloser) (io.ReadCloser, error) {
	ct := strings.ToLower(strings.TrimSpace(contentType))
	if ct != "" && !strings.Contains(ct, "text/event-stream") {
		return body, nil // 不是 SSE：不猜、不伪流式
	}
	cm := clientModelOf(t)
	if cm == "" {
		return body, nil // 没有客户端名可回写：透传
	}
	handle := func(f sseFrame) ([][]byte, bool, error) {
		data := strings.TrimSpace(f.Data)
		if data == "[DONE]" {
			return [][]byte{sseDone()}, true, nil
		}
		if strings.Contains(data, `"model"`) {
			var obj map[string]json.RawMessage
			if err := json.Unmarshal([]byte(data), &obj); err == nil {
				if _, ok := obj["model"]; ok {
					name, _ := json.Marshal(cm)
					obj["model"] = name
					if nb, err := json.Marshal(obj); err == nil {
						data = string(nb)
					}
				}
			}
		}
		var out [][]byte
		if f.Event != "" && f.Event != "message" {
			out = append(out, []byte("event: "+f.Event+"\n"))
		}
		out = append(out, sseData([]byte(data)))
		return out, false, nil
	}
	return newTransformReader(body, handle, nil), nil
}

// BuildModels 实现 Adapter：仅为接口完备性保留——AutoClaw 没有可拉取的模型
// 目录（StaticModels 才是真实来源，FetchModels/探测都会短路它），此方法只
// 在直接调用时给出一个合法的 GET 请求。
func (a *autoclawAdapter) BuildModels(ctx context.Context, t *Target) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, autoclawURL(t, "/models"), nil)
	if err != nil {
		return nil, fmt.Errorf("build autoclaw models request: %w", err)
	}
	applyAutoClawHeaders(req, t, autoclawDefaultRoute)
	return req, nil
}

// DecodeModels 实现 Adapter：宽容解析 OpenAI `{data:[{id}]}`（防御性路径，
// 生产上走 StaticModels）。
func (a *autoclawAdapter) DecodeModels(_ *Target, status int, body []byte) ([]string, error) {
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

// ── 内部实现 ────────────────────────────────────────────────────

// autoclawURL 拼上游 URL（base 由用户填，含 /autoclaw-proxy/proxy/autoclaw；
// **不走** LegacyOpenAIRoot——那会补 /v1，AutoClaw 的路径没有版本段）。
func autoclawURL(t *Target, suffix string) string {
	if t == nil {
		return suffix
	}
	return strings.TrimRight(strings.TrimSpace(t.BaseURL), "/") + suffix
}

// autoclawRoute 一次模型路由解析的结果。
type autoclawRoute struct {
	routeModelID   string // X-Request-Model（完整路由 ID，决定计费通道）
	bodyModelID    string // body.model（剥前缀的模型 ID）
	requestedModel string // 客户端原请求（响应回写与日志用）
}

// resolveAutoClawRoute 把客户端给的 model 解析成上游路由。
//
// 判定顺序（对齐参考实现，省去本地没有的两档）：
//  1. 空 → 默认路由 `zai_auto`（body 剥前缀为 `auto`，源实现同款）；
//  2. 静态目录命中（id / 展示名 / 完整 routeId，trim + 忽略 ASCII 大小写）→
//     用条目的规范 routeId 与剥前缀的 body 模型 ID；
//  3. 已知前缀开头 → 原样当路由 ID，body 剥前缀（大小写原样保留）；
//  4. 合法路由 ID 形态（`x_yyy`）→ 原样透传，body 剥前缀；
//  5. 都不中 → 默认路由（只兜真正未知的名字，如 `claude-*`）。
//
// 参考实现还有两档，这里刻意省略并记录为已知边界：远程目录命中（需要签名头）
// 与自定义模型登记（本网关没有该概念）。
func resolveAutoClawRoute(rawModel string) autoclawRoute {
	name := strings.TrimSpace(rawModel)
	if name == "" {
		return autoclawRoute{
			routeModelID:   autoclawDefaultRoute,
			bodyModelID:    stripAutoClawRoutePrefix(autoclawDefaultRoute),
			requestedModel: autoclawDefaultRoute,
		}
	}
	for _, entry := range autoclawStaticModels {
		if sameAutoClawName(name, entry.ID) || sameAutoClawName(name, entry.Name) ||
			sameAutoClawName(name, entry.RouteID) {
			return autoclawRoute{
				routeModelID:   entry.RouteID,
				bodyModelID:    stripAutoClawRoutePrefix(entry.RouteID),
				requestedModel: name,
			}
		}
	}
	for _, prefix := range autoclawRoutePrefixes {
		if strings.HasPrefix(name, prefix) {
			return autoclawRoute{
				routeModelID:   name,
				bodyModelID:    stripAutoClawRoutePrefix(name),
				requestedModel: name,
			}
		}
	}
	if isAutoClawRouteID(name) {
		return autoclawRoute{
			routeModelID:   name,
			bodyModelID:    stripAutoClawRoutePrefix(name),
			requestedModel: name,
		}
	}
	return autoclawRoute{
		routeModelID:   autoclawDefaultRoute,
		bodyModelID:    stripAutoClawRoutePrefix(autoclawDefaultRoute),
		requestedModel: name,
	}
}

// sameAutoClawName 目录匹配口径：trim + 忽略 ASCII 大小写（`GLM-5.3` 这类
// 展示名写法必须命中，否则同一名字在清单层放行、路由层却静默回退 zai_auto
// —— 选错模型且错得无声无息）。
func sameAutoClawName(a, b string) bool {
	return strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b))
}

// stripAutoClawRoutePrefix 剥掉已知路由前缀（先长后短）；没有前缀的原样返回。
func stripAutoClawRoutePrefix(routeID string) string {
	for _, prefix := range autoclawRoutePrefixes {
		if rest, ok := strings.CutPrefix(routeID, prefix); ok {
			return rest
		}
	}
	return routeID
}

// isAutoClawRouteID 合法路由 ID 形态：小写字母开头、首个 `_` 前全是小写字母/
// 数字、`_` 后 1..=127 个 [A-Za-z0-9._:-]（`zai_auto`、`x_yyy` 命中；
// `glm-5.3` 不含 `_` 不命中——那是目录 ID，走第 2 档）。
func isAutoClawRouteID(value string) bool {
	first, rest, ok := strings.Cut(value, "_")
	if !ok || first == "" || len(first) > 64 {
		return false
	}
	for i := 0; i < len(first); i++ {
		c := first[i]
		if !(c >= 'a' && c <= 'z') && !(i > 0 && c >= '0' && c <= '9') {
			return false
		}
	}
	if rest == "" || len(rest) > 127 {
		return false
	}
	for i := 0; i < len(rest); i++ {
		c := rest[i]
		if !(c >= 'a' && c <= 'z') && !(c >= 'A' && c <= 'Z') && !(c >= '0' && c <= '9') &&
			c != '.' && c != '_' && c != ':' && c != '-' {
			return false
		}
	}
	return true
}

// applyAutoClawHeaders 写入 AutoClaw 的头集合。
//
// 顺序：协议默认头 → 品牌头 → 认证/路由头 → 静态头（跳过传输层头与适配器
// 独占头）。**不发 X-Harness-Type**（带上会稳定 403 pay-view / 406，见文件头）。
func applyAutoClawHeaders(req *http.Request, t *Target, routeModelID string) {
	h := req.Header
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "*/*") // 源实现如此：LLM 代理两种响应形态都可能回
	// 品牌头（源实现 brandHeaders 逐条照抄，除 X-Harness-Type 外）。
	h.Set("X-Product", "autoclaw")
	h.Set("X-Client-Type", "pc")
	h.Set("X-Tm", autoclawPlatformTM())
	h.Set("X-Version", autoclawAppVersion)
	h.Set("X-Lang", autoclawLang)
	h.Set("X-Channel", autoclawChannel)
	h.Set("x_trace_id", "autoclaw-desktop") // 下划线写法是源实现的字面键名
	// 静态头：先于鉴权合并，但独占头覆盖不掉（平台契约）。
	if t != nil {
		for k, v := range t.Headers {
			if k == "" || v == "" || HeaderForbidden(k) || isAutoClawReservedHeader(k) {
				continue
			}
			h.Set(k, v)
		}
	}
	secret := ""
	if t != nil {
		secret = strings.TrimSpace(t.Secret)
	}
	if secret != "" {
		h.Set(autoclawAuthHeader, "Bearer "+secret)
	}
	h.Set("X-Request-Id", newAutoClawRequestID())
	h.Set("X-Request-Model", routeModelID)
}

// isAutoClawReservedHeader 报告头名是否属于 AutoClaw 的适配器独占集合
// （认证头 + 本次请求的关联/路由头，用户配置覆盖不了）。
func isAutoClawReservedHeader(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "x-authorization", "authorization", "x-request-id", "x-request-model",
		"x-harness-type", "x-product", "x-client-type", "x-tm", "x-version",
		"x-lang", "x-channel":
		return true
	}
	return false
}

// autoclawPlatformTM 平台标识：macOS 构建（若有）回 mac，其余一律 win。
// **没有 linux 分支**：服务端跑在 Linux 上，而官方桌面端只有 win/mac，
// 发 linux 会被部分账号 403。
func autoclawPlatformTM() string {
	if runtime.GOOS == "darwin" {
		return "mac"
	}
	return autoclawTM
}

// newAutoClawRequestID 生成 UUID v4 形态的请求关联 ID（X-Request-Id）。
// 随机源失败时退化为纳秒时间戳（观测用途不因熵源失败而丢请求）。
func newAutoClawRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("req-%016x", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// autoclawRewriteBody 出站 body 的三处改写（model / max_tokens / system）。
//
// 返回 ok=false 表示 body 不是 JSON 对象——调用方原样透传。重编码会按字母序
// 排列键：JSON 对象键序无语义，上游按字段名读取；换来的收益是三处改写都做在
// 结构化对象上，不做字符串替换（那会误伤用户提示词里的同名字段）。
func autoclawRewriteBody(openAIBody []byte, bodyModelID string) ([]byte, bool) {
	dec := json.NewDecoder(bytes.NewReader(openAIBody))
	dec.UseNumber() // 保住整数字面量（max_tokens 归一与回编码都不丢精度）
	var obj map[string]any
	if err := dec.Decode(&obj); err != nil || obj == nil {
		return nil, false
	}
	if bodyModelID != "" {
		obj["model"] = bodyModelID
	}
	autoclawNormalizeMaxTokens(obj)
	autoclawNormalizePrompt(obj)
	out, err := json.Marshal(obj)
	if err != nil {
		return nil, false
	}
	return out, true
}

// autoclawNormalizeMaxTokens max_tokens 的**下限归一**（就地修改）。
//
// 上游行为（两份独立实测，结论一致）：30..8192 的小正整数一律 406 空响应体；
// 不传或 ≥10000 则 200。规则：
//   - 存在、是正整数、且 < 下限 → 改写为 10000；
//   - **缺省时不注入**（上游对缺省 200，凭空注入反而改变行为）；
//   - ≥ 下限 / 0 / 负数 / null / 字符串 / 浮点 → 原样（只归一实测过被拒的形态，
//     其余交上游自己报错）；
//   - `max_completion_tokens` 不参与该校验（实测 30 也放行），一字不动。
func autoclawNormalizeMaxTokens(obj map[string]any) {
	raw, ok := obj["max_tokens"]
	if !ok {
		return
	}
	num, isNum := raw.(json.Number)
	if !isNum {
		return
	}
	v, err := num.Int64()
	if err != nil || v <= 0 || v >= autoclawMaxTokensFloor {
		return
	}
	obj["max_tokens"] = json.Number(fmt.Sprintf("%d", autoclawMaxTokensFloor))
}

// autoclawNormalizePrompt 出站 system 规范化（就地修改；详见
// autoclawPrependIdentityPrefix / autoclawRewriteIdentities 的注释）。
func autoclawNormalizePrompt(obj map[string]any) {
	msgs, ok := obj["messages"].([]any)
	if !ok {
		return // messages 缺失/不是数组：不是本层能修的形态，交上游报错
	}
	for _, m := range msgs {
		msg, isMap := m.(map[string]any)
		if !isMap || !autoclawIsSystemRole(msg) {
			continue
		}
		autoclawRewriteIdentitiesInPlace(msg)
	}
	if len(msgs) > 0 {
		if first, isMap := msgs[0].(map[string]any); isMap && autoclawIsSystemRole(first) {
			autoclawPrependIdentityPrefix(first)
			return
		}
	}
	// 首条不是 system：补一条只带前缀的 system 消息（不重排后面的消息——
	// 破坏客户端轮次结构的风险远大于收益）。
	obj["messages"] = append([]any{map[string]any{"role": "system", "content": autoclawIdentityPrefix}}, msgs...)
}

// autoclawIsSystemRole 该消息是否承载 system 级指令：角色**精确**等于
// system 或 developer（其余角色一律不动——"不动用户内容"的边界判据）。
func autoclawIsSystemRole(msg map[string]any) bool {
	role, _ := msg["role"].(string)
	return role == "system" || role == "developer"
}

// autoclawPrependIdentityPrefix 给首条 system 消息的 content 前置身份前缀
// （已经以身份句开头则不动——官方客户端的请求本身就带 OpenClaw 提示词，重复
// 前缀会写两遍，降级/同家重试路径上必然发生）。
//
// content 的形态：字符串直接拼；数组拼进第一个文本 part（一个都没有就在头部
// 插一个）；其它（null/对象/数字）换成前缀本身；缺 content 字段则补上。
func autoclawPrependIdentityPrefix(msg map[string]any) {
	raw, exists := msg["content"]
	if !exists {
		msg["content"] = autoclawIdentityPrefix
		return
	}
	switch c := raw.(type) {
	case string:
		if !strings.HasPrefix(c, autoclawIdentityLine) {
			msg["content"] = joinAutoClawPrefix(c)
		}
	case []any:
		for _, part := range c {
			pm, isMap := part.(map[string]any)
			if !isMap {
				continue
			}
			if text, isStr := pm["text"].(string); isStr {
				if !strings.HasPrefix(text, autoclawIdentityLine) {
					pm["text"] = joinAutoClawPrefix(text)
				}
				return
			}
		}
		msg["content"] = append([]any{map[string]any{"type": "text", "text": autoclawIdentityPrefix}}, c...)
	default:
		msg["content"] = autoclawIdentityPrefix
	}
}

// joinAutoClawPrefix 前缀 + 空行 + 原文（原文为空时不留多余空行）。
func joinAutoClawPrefix(original string) string {
	if original == "" {
		return autoclawIdentityPrefix
	}
	return autoclawIdentityPrefix + "\n" + original
}

// autoclawRewriteIdentitiesInPlace 就地改写 system 消息 content 里的外来身份句
// （字符串 / 文本 part 数组两种承载形态；其余无文本可改）。
func autoclawRewriteIdentitiesInPlace(msg map[string]any) {
	switch c := msg["content"].(type) {
	case string:
		if next, changed := autoclawRewriteIdentities(c); changed {
			msg["content"] = next
		}
	case []any:
		for _, part := range c {
			pm, isMap := part.(map[string]any)
			if !isMap {
				continue
			}
			if text, isStr := pm["text"].(string); isStr {
				if next, changed := autoclawRewriteIdentities(text); changed {
					pm["text"] = next
				}
			}
		}
	}
}

// autoclawRewriteIdentities 逐条套用外来身份句表（全量替换；匹配忽略 ASCII
// 大小写——闸门自己就不敏感，替换若敏感，同一句指纹换个大小写就漏网）。
func autoclawRewriteIdentities(text string) (string, bool) {
	out := text
	changed := false
	for _, pair := range autoclawForeignIdentities {
		if next, hit := replaceAllIgnoreASCIICase(out, pair[0], pair[1]); hit {
			out = next
			changed = true
		}
	}
	return out, changed
}

// replaceAllIgnoreASCIICase `from` 在 `text` 里的全量替换（忽略 ASCII 大小写）；
// 一次都没命中返回原串与 false。每轮从匹配处之后继续扫，替换串永不被复查
// （to 里即使含 from 也不会死循环）。
func replaceAllIgnoreASCIICase(text, from, to string) (string, bool) {
	if from == "" {
		return text, false
	}
	i := indexIgnoreASCIICase(text, from)
	if i < 0 {
		return text, false
	}
	var b strings.Builder
	b.Grow(len(text))
	rest := text
	for {
		b.WriteString(rest[:i])
		b.WriteString(to)
		rest = rest[i+len(from):]
		j := indexIgnoreASCIICase(rest, from)
		if j < 0 {
			break
		}
		i = j
	}
	b.WriteString(rest)
	return b.String(), true
}

// indexIgnoreASCIICase `from` 在 `text` 里首次出现的字节下标（ASCII 大小写
// 折叠；不用 strings.ToLower——Unicode 折叠会改变字节长度，切片会错位）。
// from 全是 ASCII，命中区间只可能由 ASCII 字节组成，起止点必是字符边界。
func indexIgnoreASCIICase(text, from string) int {
	n, m := len(text), len(from)
	if m == 0 || n < m {
		return -1
	}
	for i := 0; i+m <= n; i++ {
		if asciiEqualFold(text[i:i+m], from) {
			return i
		}
	}
	return -1
}

// asciiEqualFold 等长 ASCII 串的大小写不敏感比较。
func asciiEqualFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if ca >= 'A' && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if cb >= 'A' && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}

// autoclawErrorEnvelope 错误分类（判定依据见常量与方法注释）：
//   - 401 → authentication_error / token_invalid（token 失效，提示重新粘贴）；
//   - 403 + `pay-view`/`810001` → membership_required（需要会员/额度）；
//   - 其余 403 → authentication_error / token_invalid；
//   - 406 且上游没给说明 → upstream_406 + 可执行提示（预校验拒收的统一形态）；
//   - 其余 → 已是 OpenAI 信封的原样保留，否则包 upstream_<status> 信封。
//
// 所有文案过 ScrubSecrets。
func autoclawErrorEnvelope(t *Target, status int, body []byte) []byte {
	upstreamMsg := autoclawUpstreamMessage(body)
	switch {
	case status == http.StatusUnauthorized:
		return autoclawAuthEnvelope(t, status, upstreamMsg)
	case status == http.StatusForbidden:
		if strings.Contains(string(body), "pay-view") || strings.Contains(string(body), "810001") {
			msg := fmt.Sprintf("AutoClaw 上游返回 403（pay-view / 810001）：该能力需要会员或额度不足；"+
				"请升级会员或更换有额度的账号后重试。上游文案：%s", upstreamMsg)
			return jsonErrorEnvelope("upstream_error", "membership_required",
				scrubForTarget(t, truncate(msg, 2000)))
		}
		return autoclawAuthEnvelope(t, status, upstreamMsg)
	case status == http.StatusNotAcceptable && upstreamMsg == "":
		msg := "上游返回 406: 上游错误" + autoclaw406Hint
		return jsonErrorEnvelope("upstream_error", "upstream_406", scrubForTarget(t, msg))
	case isOpenAIErrorEnvelope(body):
		return body // 已是客户端可解析形态，保留上游 error.code
	default:
		return ErrorEnvelope(status, fmt.Sprintf("upstream_%d", status), scrubForTarget(t, string(body)))
	}
}

// autoclawAuthEnvelope token 失效信封（401 与非 pay-view 的 403 共用文案）。
func autoclawAuthEnvelope(t *Target, status int, upstreamMsg string) []byte {
	msg := fmt.Sprintf("AutoClaw token 已失效或被上游拒绝（HTTP %d）；"+
		"请重新获取桌面端 openclaw.json 里的 X-Authorization token 并更新该凭证的 secret", status)
	if upstreamMsg != "" {
		msg += "；上游文案：" + upstreamMsg
	}
	return jsonErrorEnvelope("authentication_error", "token_invalid", scrubForTarget(t, msg))
}

// autoclawUpstreamMessage 从错误体取一句可读文案（message 字段优先，非 JSON
// 时截断原文；空体返回空串——406 判定依赖它）。
func autoclawUpstreamMessage(body []byte) string {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return ""
	}
	var probe struct {
		Message string `json:"message"`
		Error   struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &probe); err == nil {
		for _, s := range []string{probe.Message, probe.Error.Message} {
			if v := strings.TrimSpace(s); v != "" {
				return truncate(v, 500)
			}
		}
		return ""
	}
	return truncate(trimmed, 500)
}

// autoclawDecodeObject 宽容解码 JSON 对象（失败返回 nil）。
func autoclawDecodeObject(body []byte) map[string]any {
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil || obj == nil {
		return nil
	}
	return obj
}
