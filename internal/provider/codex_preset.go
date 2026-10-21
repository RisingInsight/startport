// codex_preset.go Codex 预设条目（与 codex.go 适配器同批引入）。
//
// 独立成文、以包内 init 追加 presetList：与适配器同源维护，语义与写在
// presets.go 完全一致（Presets/MatchPreset 都是调用时读同一切片）。
package provider

func init() {
	presetList = append(presetList, Preset{
		Key: "codex", Name: "Codex（ChatGPT 订阅编码后端）", Vendor: "OpenAI",
		Aliases: []string{"codex", "chatgpt codex", "openai codex"},
		// 原生适配器：上游只说 Responses 协议，鉴权是 Bearer 接入令牌 +
		// chatgpt-account-id 头 + originator 伪装的头组——四种 auth 表达不了，
		// 用 adapter 覆盖。
		Protocol: ProtocolResponses, Adapter: CodexAdapterName,
		Models: []string{"gpt-5.3-codex", "gpt-5.2-codex", "gpt-5.1-codex-max", "gpt-5.6-luna"},
		NativeRequired: false,
		Note: "原生适配器（adapter=codex，ChatGPT 订阅编码后端）：base 建议 https://chatgpt.com/backend-api/codex（以官方为准）；" +
			"凭证粘贴 codex login 产出的 auth.json 全文（ChatGPT 模式；也可只粘贴其中的接入令牌字符串，但将失去自动刷新能力）；" +
			"上游恒为 SSE（stream=true + store=false 强制），采样参数不上抛（Codex 后端拒绝）；" +
			"⚠ 接口契约来自参考实现逆向，尚未真实账号验收。",
	})
}
