// trae_catalog.go TRAE CN 模型目录（静态元数据，实现 CatalogProvider）。
//
// 为什么是静态表：TRAE 上游没有公开的模型目录端点（对话走 /llm_utils_chat，
// 元数据不下发）。字段来自真实使用观测 + TRAE 客户端 UI 展示（思考档位、
// 工具支持）与主流模型常识（上下文长度、最大输出）。上游目录接口若日后
// 公开，替换为远程拉取 + 本表兜底（同 Qoder CatalogProvider 结构）。
package provider

import "context"

// traeModelCatalog TRAE CN 的模型元数据（按面板与客户端实际行为整理）。
// reasoning 档位：Turbo/Pro/Evolving 等新模型支持思考；glm-5.3 同名合并标注
// 来自渠道别名（旧名映射到同款上游模型）。
var traeModelCatalog = []ModelMeta{
	{ID: "Doubao-Seed-2.1-Turbo", Name: "Doubao Seed 2.1 Turbo", SupportsReasoning: true,
		SupportedEfforts: []string{"high"}, DefaultEffort: "high", SupportsTools: true, ContextLength: 256000, MaxOutputTokens: 64000},
	{ID: "Doubao-Seed-2.1-Pro", Name: "Doubao Seed 2.1 Pro", SupportsReasoning: true,
		SupportedEfforts: []string{"high"}, DefaultEffort: "high", SupportsTools: true, ContextLength: 256000, MaxOutputTokens: 64000},
	{ID: "Doubao-Seed-Evolving", Name: "Doubao Seed Evolving", SupportsReasoning: true,
		SupportedEfforts: []string{"high"}, DefaultEffort: "high", SupportsTools: true, ContextLength: 256000, MaxOutputTokens: 64000},
	{ID: "glm-5.3-flash", Name: "GLM-5.3 Flash", SupportsReasoning: true,
		SupportedEfforts: []string{"high"}, DefaultEffort: "high", SupportsTools: true, ContextLength: 200000, MaxOutputTokens: 128000},
	{ID: "glm-5.3-flashx", Name: "GLM-5.3 FlashX", SupportsTools: true, ContextLength: 200000, MaxOutputTokens: 128000},
	{ID: "glm-5.3", Name: "GLM-5.3", SupportsReasoning: true,
		SupportedEfforts: []string{"high"}, DefaultEffort: "high", SupportsTools: true, ContextLength: 200000, MaxOutputTokens: 128000},
	{ID: "deepseek-v4.1-flash", Name: "DeepSeek V4.1 Flash", SupportsReasoning: true,
		SupportedEfforts: []string{"high"}, DefaultEffort: "high", SupportsTools: true, ContextLength: 160000, MaxOutputTokens: 64000},
	{ID: "DeepSeek-V4-Pro-Official", Name: "DeepSeek V4 Pro (Official)", SupportsReasoning: true,
		SupportedEfforts: []string{"high"}, DefaultEffort: "high", SupportsTools: true, ContextLength: 160000, MaxOutputTokens: 64000},
	{ID: "kimi-k3.1-pro", Name: "Kimi K3.1 Pro", SupportsReasoning: true,
		SupportedEfforts: []string{"high"}, DefaultEffort: "high", SupportsTools: true, ContextLength: 256000, MaxOutputTokens: 64000},
	{ID: "kimi-k3", Name: "Kimi K3", SupportsReasoning: true,
		SupportedEfforts: []string{"high"}, DefaultEffort: "high", SupportsTools: true, ContextLength: 256000, MaxOutputTokens: 64000},
	{ID: "mimo-v2.6-flash", Name: "MiMo v2.6 Flash", SupportsTools: true, ContextLength: 128000, MaxOutputTokens: 32000},
}

// traeCatalogIndex 按 ID 索引（O(1) 查询）。
var traeCatalogIndex = func() map[string]ModelMeta {
	m := make(map[string]ModelMeta, len(traeModelCatalog))
	for _, meta := range traeModelCatalog {
		m[meta.ID] = meta
	}
	return m
}()

// traeAdapter 目录能力挂载（traeAdapter 在 trae.go 定义）。
func (a *traeAdapter) ModelCatalog(ctx context.Context, t *Target) ([]ModelMeta, error) {
	// 静态表：返回全部已知模型元数据（与渠道自动发现的 ID 清单对齐；
	// 未收录的模型面板回落"不支持思考"占位）。
	out := make([]ModelMeta, len(traeModelCatalog))
	copy(out, traeModelCatalog)
	return out, nil
}

// traeMetaOf 按 ID 查元数据（供将来的对话域参数注入使用）。
func traeMetaOf(id string) (ModelMeta, bool) {
	m, ok := traeCatalogIndex[id]
	return m, ok
}
