// catalog.go 带元数据的外部模型目录（面板「模型与档位」专用）。
//
// 与 ModelList 的分工：
//   - ModelList：/v1/models 的 ID 清单（轻量，自动发现缓存 10 分钟）；
//   - ModelCatalog：面板展示用，优先调用适配器的 CatalogProvider（Qoder 等），
//     带倍率/思考档位/上下文/输出上限/能力位；失败回退 ID 清单。
package extchan

import (
	"context"
	"time"

	"github.com/RisingInsight/startport/internal/provider"
)

const catalogTTL = 10 * time.Minute

// ModelCatalog 返回带元数据的模型条目（ext:<channel>/<model>）。
// 结果缓存 10 分钟：/v1/models 与面板共用，避免每次客户端拉目录都打上游
// 元数据接口（Qoder 的 model/list 要 COSY 签名与 HTTP 往返）。
func (m *Manager) ModelCatalog(ctx context.Context) []map[string]any {
	m.catalogMu.Lock()
	if len(m.catalogRows) > 0 && time.Since(m.catalogAt) < catalogTTL {
		rows := cloneCatalogRows(m.catalogRows)
		m.catalogMu.Unlock()
		return rows
	}
	m.catalogMu.Unlock()

	rows := m.buildModelCatalog(ctx)
	if len(rows) > 0 {
		m.catalogMu.Lock()
		m.catalogRows, m.catalogAt = cloneCatalogRows(rows), time.Now()
		m.catalogMu.Unlock()
	}
	return rows
}

func cloneCatalogRows(rows []map[string]any) []map[string]any {
	out := make([]map[string]any, len(rows))
	for i, r := range rows {
		cp := make(map[string]any, len(r))
		for k, v := range r {
			if list, ok := v.([]string); ok {
				v = append([]string(nil), list...)
			}
			cp[k] = v
		}
		out[i] = cp
	}
	return out
}

func (m *Manager) buildModelCatalog(ctx context.Context) []map[string]any {
	m.mu.RLock()
	chs := append([]*Channel(nil), m.channels...)
	m.mu.RUnlock()
	out := make([]map[string]any, 0)
	for _, ch := range chs {
		if ch.Disabled {
			continue
		}
		adapter, err := adapterFor(ch)
		if err != nil {
			continue
		}
		cred := ch.pickCredential(nil)
		if cred == nil {
			creds := ch.credentials()
			if len(creds) > 0 {
				cred = creds[0]
			}
		}
		target := m.BuildTarget(ch, cred, "")
		entries := make([]map[string]any, 0)
		byID := map[string]map[string]any{}
		if cp, ok := adapter.(provider.CatalogProvider); ok {
			if metas, merr := cp.ModelCatalog(ctx, target); merr == nil {
				explicit := ch.explicitModels()
				for _, meta := range metas {
					if len(explicit) > 0 && !containsModel(explicit, meta.ID) {
						continue
					}
					e := modelMetaEntry(ch.Name, meta)
					entries = append(entries, e)
					byID[meta.ID] = e
				}
			}
		}
		if len(entries) == 0 {
			for _, id := range m.EnsureModels(ctx, ch) {
				e := baseModelEntry(ch.Name, id)
				entries = append(entries, e)
				byID[id] = e
			}
		}
		// 别名条目：复制目标模型的元数据，改 id/alias_of。
		for _, alias := range ch.aliasKeys() {
			targetID, _ := ch.aliasFor(alias)
			e := baseModelEntry(ch.Name, alias)
			e["alias_of"] = targetID
			if base, ok := byID[targetID]; ok {
				for k, v := range base {
					switch k {
					case "id", "owned_by", "alias_of":
						continue
					}
					e[k] = v
				}
			}
			entries = append(entries, e)
		}
		out = append(out, entries...)
	}
	return out
}

func (c *Channel) explicitModels() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.Models...)
}

func containsModel(list []string, id string) bool {
	for _, v := range list {
		if v == id {
			return true
		}
	}
	return false
}

func baseModelEntry(channel, model string) map[string]any {
	return map[string]any{
		"id":       "ext:" + channel + "/" + model,
		"object":   "model",
		"created":  1753600000,
		"owned_by": "ext:" + channel,
		"source":   "ext",
		"provider": channel,
	}
}

func modelMetaEntry(channel string, meta provider.ModelMeta) map[string]any {
	e := baseModelEntry(channel, meta.ID)
	if meta.Name != "" {
		e["name"] = meta.Name
	}
	if meta.Description != "" {
		e["description"] = meta.Description
	}
	if meta.Credits != "" {
		e["credits"] = meta.Credits
	}
	if meta.ContextLength > 0 {
		e["context_length"] = meta.ContextLength
	}
	if meta.MaxOutputTokens > 0 {
		e["max_output_tokens"] = meta.MaxOutputTokens
	}
	if len(meta.SupportedEfforts) > 0 {
		e["supported_efforts"] = append([]string(nil), meta.SupportedEfforts...)
		e["reasoning_supported_efforts"] = append([]string(nil), meta.SupportedEfforts...)
	}
	if meta.DefaultEffort != "" {
		e["default_effort"] = meta.DefaultEffort
		e["reasoning_default_effort"] = meta.DefaultEffort
	}
	if meta.SupportsTools {
		e["supports_tool_call"] = true
	}
	if meta.SupportsImages {
		e["supports_images"] = true
	}
	if meta.SupportsReasoning {
		e["supports_reasoning"] = true
	}
	if meta.CanDisableThinking {
		e["can_disable_thinking"] = true
	}
	return e
}
