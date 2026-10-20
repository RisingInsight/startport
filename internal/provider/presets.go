// presets.go 平台情报预设：把「额度情报条目」变成「渠道草稿」的建议值来源。
//
// 边界（PRD §8 / P0-10）：
//   - 只提供**可编辑的建议值**（协议/鉴权方式/备注/官方文档），绝不据此产生任何
//     路由或出站决策；
//   - `base_url` **一律为空**——情报是第三方人工复核数据，会过期，地址必须由
//     用户按官方文档确认后自己填（这是"草稿"而不是"一键接入"的关键）；
//   - `native_required=true` 表示该平台的**凭证/签名形态**无法用静态 token
//     直连（需要客户端签名/设备指纹/有状态转发），本期置灰并提示改用替代方案；
//   - 平台与官方文档会变化，本表以架构 §7.2 为准，标注为"建议值"。
package provider

import (
	"strings"
)

// Preset 平台预设（情报条目 → 渠道草稿的建议值）。
type Preset struct {
	// Key 稳定键（slug；草稿渠道名前缀）。
	Key string `json:"key"`
	// Name 展示名。
	Name string `json:"name"`
	// Vendor 厂商（情报条目的 vendor 匹配用）。
	Vendor string `json:"vendor"`
	// Aliases 别名（情报条目 name 匹配用；大小写/空格/连字符不敏感）。
	Aliases []string `json:"aliases,omitempty"`
	// Protocol 建议出站协议。
	Protocol Protocol `json:"protocol"`
	// Adapter 建议原生适配器名（空 = 按协议；如 "loomy"）。
	Adapter string `json:"adapter,omitempty"`
	// Auth 建议鉴权形态。
	Auth AuthSpec `json:"auth"`
	// BaseURL 建议 base_url——**恒为空**（必须由用户确认后填）。
	BaseURL string `json:"base_url"`
	// Path 建议路径覆盖（非 /v1 形态的平台用）。
	Path string `json:"path,omitempty"`
	// Models 建议模型名单（可空）。
	Models []string `json:"models,omitempty"`
	// Headers 建议静态头（如 OpenRouter 的 HTTP-Referer）。
	Headers map[string]string `json:"headers,omitempty"`
	// NativeRequired true = 需要原生适配器（签名/指纹/有状态），本期不可直接接入。
	NativeRequired bool `json:"native_required"`
	// Note 备注（面板展示；NativeRequired 时说明原因与替代方案）。
	Note string `json:"note"`
	// DocsURL 官方文档。
	DocsURL string `json:"docs_url,omitempty"`
}

// presetList 内置预设目录（21 条，覆盖 PRD §5.1 点名的平台 + 三个协议基准）。
//
// 说明：`Protocol`/`Auth` 是**建议值**，来源于各平台公开文档与 token-fbi 情报；
// 用户可在向导里改。`BaseURL` 恒为空：草稿不替用户做"地址正确性"的承诺。
var presetList = []Preset{
	{
		Key: "workbuddy", Name: "WorkBuddy / CodeBuddy", Vendor: "腾讯",
		Aliases:  []string{"codebuddy", "workbuddy"},
		Protocol: ProtocolAnthropic, Auth: AuthSpec{Mode: AuthBearer},
		NativeRequired: true,
		Note:           "已由本项目 CodeBuddy 账号池原生覆盖（含保号/签到/刷新）；ext 渠道只能持静态凭证，不建议重复接入——请改用账号池。",
		DocsURL:        "https://copilot.tencent.com/",
	},
	{
		Key: "qoder", Name: "Qoder（阿里 · 中国版）", Vendor: "阿里",
		Aliases: []string{"qoder", "qoder cn"},
		// 原生适配器（中国版）：推理网关走 COSY 自签名（RSA 包 AES 密钥 + MD5
		// 签名 + 自定义 base64 编码），静态 key 无法直连——adapter 覆盖。
		Protocol: ProtocolOpenAI, Adapter: QoderAdapterName,
		Models:         []string{"Qwen3.8-Flash", "Qwen3.8-Max", "Auto", "GLM-5.3"},
		NativeRequired: false,
		Note: "原生适配器（adapter=qoder，中国版）：base 建议 https://gateway.qoder.com.cn（以官方为准）；" +
			"凭证存整个账号 JSON（accessToken/userId/machineId/nickname/email，COSY 签名需要用户与设备标识，裸 token 不够）；" +
			"模型未知名是硬失败（不静默回落）；业务错误在 SSE 信封里（HTTP 恒 200），排队会返回 503 qoder_model_busy；" +
			"401 = token 失效需重新粘贴。签名用 RSA/AES/MD5，密钥随代码分发。",
	},
	{
		Key: "qoder-intl", Name: "Qoder（阿里 · 国际版）", Vendor: "阿里",
		Aliases:  []string{"qoder intl", "qoder-intl"},
		Protocol: ProtocolOpenAI, Adapter: QoderIntlAdapterName,
		Models:         []string{"Qwen3.8-Flash", "Qwen3.8-Max", "Ultimate", "GLM-5.3"},
		NativeRequired: false,
		Note: "原生适配器（adapter=qoder-intl，国际版）：base 建议 https://api2.qoder.sh（PAT 换来的作业令牌 jt-；" +
			"设备流令牌 dt- 走 https://api3.qoder.sh——打错主机会 403 Login expired，以官方为准）；" +
			"凭证与签名同中国版（账号 JSON），但两站令牌不通用；模型未知名硬失败。",
	},
	{
		Key: "trae", Name: "TRAE（字节 · SOLO 国内）", Vendor: "字节跳动",
		Aliases:  []string{"trae", "trae cn", "trae solo"},
		Protocol: ProtocolOpenAI, Adapter: TraeAdapterName,
		NativeRequired: false,
		Note: "原生适配器（adapter=trae，国内 SOLO 通道）：base 建议 https://trae-api-cn.mchost.guru（以官方为准）；" +
			"凭证填 Trae 登录态（accessToken 必填；uid/machineId/deviceId 建议一并填写，上游按设备绑定）；" +
			"模型目录自动从 /api/ide/v1/get_detail_param 拉取，广告名不带后缀；" +
			"本批先支持粘贴凭据转发，登录/签到/刷新在下一批接入；国际版是另一套协议，暂不支持。",
		DocsURL: "https://www.trae.cn/",
	},
	{
		Key: "autoclaw", Name: "AutoClaw（智谱 · 国内版）", Vendor: "智谱 AI",
		Aliases: []string{"autoclaw", "auto claw", "autoglm"},
		// 原生适配器（国内版）：认证头是 X-Authorization（不是 Authorization）、
		// X-Request-Model 带路由前缀、system 要包 OpenClaw 身份前缀——四种 auth
		// 与三协议基准都表达不了，用 adapter 覆盖。
		Protocol: ProtocolOpenAI, Adapter: AutoClawAdapterName,
		Models: []string{"glm-5.3", "glm-5.3-flash"},
		// Auth 留空：adapter 非空时配置层明确禁止再配 auth。
		NativeRequired: false,
		Note: "原生适配器（adapter=autoclaw，国内版）：base 建议 " +
			"https://autoglm-acceleration-api.zhipuai.cn/autoclaw-proxy/proxy/autoclaw（以官方为准）；" +
			"凭证填桌面端 openclaw.json 里 X-Authorization 的 token（手工粘贴，401 即失效需重取）；" +
			"glm-5.3 → zaicoding_glm-5.3、glm-5.3-flash → zai_glm-5.3-flash，未知名回落 zai_auto；" +
			"system 会被包进 OpenClaw 身份前缀、max_tokens 小值归一到 10000（上游预校验）。",
	},
	{
		Key: "autoclaw-intl", Name: "AutoClaw（智谱 · 国际版）", Vendor: "智谱 AI",
		Aliases: []string{"autoclaw intl", "autoclaw-intl", "autoglm intl"},
		// 国际版：同一套客户端代码的另一个构建（站点不同，目录/账号互不相通）。
		Protocol: ProtocolOpenAI, Adapter: AutoClawIntlAdapterName,
		Models:         []string{"glm-5.3", "glm-5.3-flash"},
		NativeRequired: false,
		Note: "原生适配器（adapter=autoclaw-intl，国际版）：base 建议 " +
			"https://autoglm-api.autoglm.ai/autoclaw-proxy/proxy/autoclaw（以官方为准）；" +
			"头集合 / 模型路由 / system 规范化与国内版同一套，但站点、token 与目录互不相通；" +
			"被绑为 Windows 的账号对 X-Tm: linux 会 403——适配器恒发 win。",
	},
	{
		Key: "zcode", Name: "ZCode", Vendor: "ZCode",
		Aliases:  []string{"zcode"},
		Protocol: ProtocolOpenAI, Auth: AuthSpec{Mode: AuthBearer},
		Note: "同时提供 OpenAI 兼容与 Anthropic 兼容入口，可建两条渠道分别接（协议按入口选）。",
	},
	{
		Key: "cline", Name: "Cline", Vendor: "Cline",
		Aliases:  []string{"cline"},
		Protocol: ProtocolOpenAI, Auth: AuthSpec{Mode: AuthBearer},
		Note:    "是客户端而非上游平台：若改走其官方 Provider，请按 OpenAI 兼容 + 官方 base 接。",
		DocsURL: "https://cline.bot/",
	},
	{
		Key: "kilo", Name: "Kilo Code", Vendor: "Kilo",
		Aliases:  []string{"kilo", "kilo code"},
		Protocol: ProtocolOpenAI, Auth: AuthSpec{Mode: AuthBearer},
		Note:    "同上：客户端/聚合入口，非上游模型平台。",
		DocsURL: "https://kilocode.ai/",
	},
	{
		Key: "catpaw", Name: "CatPaw（美团）", Vendor: "美团",
		Aliases:  []string{"catpaw", "cat paw"},
		Protocol: ProtocolOpenAI, Auth: AuthSpec{Mode: AuthBearer},
		NativeRequired: true,
		Note:           "轮次状态机 + 签名，静态 key 无法直连（P2）。",
	},
	{
		Key: "siliconflow", Name: "硅基流动 SiliconFlow", Vendor: "硅基流动",
		Aliases:  []string{"siliconflow", "silicon flow", "硅基流动"},
		Protocol: ProtocolOpenAI, Auth: AuthSpec{Mode: AuthBearer},
		Note:    "标准 OpenAI 兼容，首发联调平台。base 建议 https://api.siliconflow.cn（以官方文档为准）。",
		DocsURL: "https://docs.siliconflow.cn/",
	},
	{
		Key: "openrouter", Name: "OpenRouter", Vendor: "OpenRouter",
		Aliases:  []string{"openrouter", "open router"},
		Protocol: ProtocolOpenAI, Auth: AuthSpec{Mode: AuthBearer},
		Headers: map[string]string{"HTTP-Referer": "https://github.com/RisingInsight/startport", "X-Title": "StarPort"},
		Note:    "标准兼容；建议加 HTTP-Referer/X-Title 静态头（排行归因，可选）。base 建议 https://openrouter.ai/api/v1。",
		DocsURL: "https://openrouter.ai/docs",
	},
	{
		Key: "modelscope", Name: "魔搭 ModelScope", Vendor: "阿里",
		Aliases:  []string{"modelscope", "model scope", "魔搭"},
		Protocol: ProtocolOpenAI, Auth: AuthSpec{Mode: AuthBearer},
		Note:    "标准兼容。base 建议 https://api-inference.modelscope.cn/v1。",
		DocsURL: "https://www.modelscope.cn/docs",
	},
	{
		Key: "dashscope", Name: "阿里云百炼 DashScope", Vendor: "阿里云",
		Aliases:  []string{"dashscope", "bailian", "百炼"},
		Protocol: ProtocolOpenAI, Auth: AuthSpec{Mode: AuthBearer},
		Note:    "走 compatible-mode。base 建议 https://dashscope.aliyuncs.com/compatible-mode/v1；原生 DashScope 协议不在本期。",
		DocsURL: "https://help.aliyun.com/zh/model-studio/",
	},
	{
		Key: "nvidia-nim", Name: "NVIDIA NIM", Vendor: "NVIDIA",
		Aliases:  []string{"nvidia", "nim", "nvidia nim"},
		Protocol: ProtocolOpenAI, Auth: AuthSpec{Mode: AuthBearer},
		Note:    "标准兼容。base 建议 https://integrate.api.nvidia.com/v1。",
		DocsURL: "https://docs.nvidia.com/nim/",
	},
	{
		Key: "volc-ark", Name: "火山方舟 Ark", Vendor: "字节跳动",
		Aliases:  []string{"volc", "ark", "火山", "火山方舟"},
		Protocol: ProtocolOpenAI, Auth: AuthSpec{Mode: AuthBearer},
		Path:    "/chat/completions",
		Note:    "非 /v1 形态：base 建议 https://ark.cn-beijing.volces.com/api/v3，并保留示例里的 path=/chat/completions（否则会被补 /v1）。",
		DocsURL: "https://www.volcengine.com/docs/82379",
	},
	{
		Key: "sensenova", Name: "商汤 SenseNova", Vendor: "商汤",
		Aliases:  []string{"sensenova", "sense nova", "商汤"},
		Protocol: ProtocolOpenAI, Auth: AuthSpec{Mode: AuthBearer},
		Note:    "兼容入口（以官方文档为准）。base 建议 https://api.sensenova.cn/compatible-mode/v1。",
		DocsURL: "https://platform.sensenova.cn/",
	},
	{
		Key: "monkeycode", Name: "MonkeyCode", Vendor: "MonkeyCode",
		Aliases:  []string{"monkeycode", "monkey code"},
		Protocol: ProtocolOpenAI, Auth: AuthSpec{Mode: AuthBearer},
		NativeRequired: true,
		Note:           "入口形态待核实；平台侧若需签名/特殊头，静态 key 无法直连（需原生适配）。",
	},
	{
		Key: "loomy", Name: "Loomy（讯飞）", Vendor: "Loomy",
		Aliases: []string{"loomy", "讯飞 loomy"},
		// 原生适配器样板：报文是 OpenAI 兼容，但鉴权是**双头**（token +
		// Authorization: Bearer），四种 auth 都表达不了 → 用 adapter 覆盖。
		Protocol: ProtocolOpenAI, Adapter: LoomyAdapterName,
		// Auth 留空：adapter 非空时配置层明确禁止再配 auth（校验会报错）。
		NativeRequired: false,
		Note: "原生适配器（adapter=loomy）：base 建议 https://loomyad.xunfei.cn/api/v1（以官方为准）；" +
			"凭证填 Loomy 客户端登录态里的 session，适配器会自动注入 token + Authorization 双头；" +
			"session 有效期 14 天且上游无续期接口，过期需重新登录后更新 secret；" +
			"聊天转发不需要签名（HMAC 只用于账号登录，本项目不做登录）。",
	},
	{
		Key: "huawei-codearts", Name: "华为云码道 CodeArts", Vendor: "华为云",
		Aliases:  []string{"codearts", "huawei codearts", "码道", "华为云"},
		Protocol: ProtocolOpenAI, Auth: AuthSpec{Mode: AuthCustom, Header: "X-Auth-Token"},
		NativeRequired: true,
		Note:           "华为云 AK/SK HMAC 签名，静态 token 不适用（需原生适配）。",
		DocsURL:        "https://www.huaweicloud.com/product/codearts.html",
	},
	{
		Key: "antigravity", Name: "Google Antigravity", Vendor: "Google",
		Aliases:  []string{"antigravity", "anti gravity"},
		Protocol: ProtocolGemini, Auth: AuthSpec{Mode: AuthCustom, Header: "x-goog-api-key"},
		NativeRequired: true,
		Note:           "Antigravity 客户端凭证不是标准 Gemini API key；请改用 google-gemini 预设 + 官方 Gemini key（同一适配器直接可用）。",
	},
	{
		Key: "google-gemini", Name: "Google Gemini API", Vendor: "Google",
		Aliases:  []string{"gemini", "google gemini", "google ai studio"},
		Protocol: ProtocolGemini, Auth: AuthSpec{Mode: AuthCustom, Header: "x-goog-api-key"},
		Note:    "Gemini 出站协议的基准实现。base 建议 https://generativelanguage.googleapis.com。",
		DocsURL: "https://ai.google.dev/gemini-api/docs",
	},
	{
		Key: "anthropic-official", Name: "Anthropic 官方", Vendor: "Anthropic",
		Aliases:  []string{"anthropic", "claude"},
		Protocol: ProtocolAnthropic, Auth: AuthSpec{Mode: AuthXAPIKey},
		Note:    "Anthropic 出站协议的基准实现。base 建议 https://api.anthropic.com。",
		DocsURL: "https://docs.anthropic.com/",
	},
}

// Presets 返回预设目录的副本（面板端点/测试用；调用方修改不影响内置表）。
func Presets() []Preset {
	out := make([]Preset, len(presetList))
	for i, p := range presetList {
		cp := p
		cp.Aliases = append([]string(nil), p.Aliases...)
		cp.Models = append([]string(nil), p.Models...)
		if len(p.Headers) > 0 {
			h := make(map[string]string, len(p.Headers))
			for k, v := range p.Headers {
				h[k] = v
			}
			cp.Headers = h
		}
		out[i] = cp
	}
	return out
}

// NormalizeKey 展示名 → 匹配键：小写、去掉空格/全角空格/连字符/下划线/点，
// 并剥离结尾的 "api" / "官方" / "cn" 后缀（"Qoder CN" → "qoder"）。
func NormalizeKey(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	for _, r := range s {
		switch r {
		case ' ', '\t', '\u3000', '-', '_', '.', '（', '）', '(', ')':
			continue
		default:
			b.WriteRune(r)
		}
	}
	out := strings.TrimPrefix(b.String(), "官方")
	for {
		changed := false
		for _, suffix := range []string{"官方", "api", "cn"} {
			if strings.HasSuffix(out, suffix) && len(out) > len(suffix) {
				out = strings.TrimSuffix(out, suffix)
				changed = true
			}
		}
		if !changed {
			break
		}
	}
	return out
}

// MatchPreset 情报条目 → 预设。匹配顺序：name 精确（规范化）→ alias → vendor。
func MatchPreset(name, vendor string) (Preset, bool) {
	key := NormalizeKey(name)
	if key != "" {
		for _, p := range presetList {
			if NormalizeKey(p.Name) == key || p.Key == key || NormalizeKey(p.Key) == key {
				return p, true
			}
		}
		for _, p := range presetList {
			for _, alias := range p.Aliases {
				if NormalizeKey(alias) == key {
					return p, true
				}
			}
		}
	}
	if v := NormalizeKey(vendor); v != "" {
		for _, p := range presetList {
			if NormalizeKey(p.Vendor) == v {
				return p, true
			}
		}
	}
	return Preset{}, false
}
