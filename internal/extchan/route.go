// route.go 模型名 → 渠道的路由解析（前缀优先、裸名唯一匹配、别名解析）。
//
// 路由协议（阶段一起沿用，阶段三扩展别名）：
//   - "ext:<channel>/<model>" 前缀形态是权威路由协议；
//   - 裸名在全部启用渠道的 (models ∪ aliases.keys) 中唯一匹配时也路由，
//     多渠道同名 → 报错要求前缀消歧（静默选一个会隐藏另一渠道的存在）；
//   - 前缀形态下 <model> 先查渠道别名表：命中 → 上游模型名 = 别名值，
//     响应 model 仍回写用户请求的别名（PRD P0-6）。
//
// 入站差异保持现状（明确记录，不改）：/v1/chat/completions 只在前缀形态下
// 短路（裸名仍走 CodeBuddy 选号），/v1/messages 走 RouteFor（裸名有效）。
package extchan

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// HasPrefix 报告 model 是否以 "ext:<channel>/" 形式指向本 Manager 管辖的渠道。
// 返回渠道名与渠道内裸模型名（不做别名解析——调用方需要原始名做观测）。
func (m *Manager) HasPrefix(model string) (channel, bare string, ok bool) {
	const p = "ext:"
	if !strings.HasPrefix(model, p) {
		return "", "", false
	}
	rest := model[len(p):]
	idx := strings.IndexByte(rest, '/')
	if idx <= 0 || idx == len(rest)-1 {
		return "", "", false
	}
	ch := rest[:idx]
	if strings.ContainsAny(ch, "/ \t") {
		return "", "", false
	}
	return ch, rest[idx+1:], true
}

// errNotExt 模型不属于任何外部渠道（调用方回落 CodeBuddy 池）。
var errNotExt = errors.New("not an ext-channel model")

// errNoChannel 前缀指向的渠道不存在或已停用。
type errNoChannel struct{ name string }

func (e *errNoChannel) Error() string {
	return fmt.Sprintf("ext channel %q not found or disabled", e.name)
}

// ErrModelNotExt 导出给调用方做 errors.Is 判定（避免依赖错误文案）。
var ErrModelNotExt = errNotExt

// RouteModel 解析请求模型名 → (渠道, 上游模型名, 客户端可见模型名)。
//
// upstream 用于重写出站 body.model；display 用于响应 model 回写
// （别名形态下客户端看到的永远是它请求的别名）。
func (m *Manager) RouteModel(model string) (ch *Channel, upstream, display string, err error) {
	if name, bare, ok := m.HasPrefix(model); ok {
		c := m.Get(name)
		if c == nil {
			return nil, "", "", &errNoChannel{name: name}
		}
		if target, hit := c.aliasFor(bare); hit {
			return c, target, "ext:" + c.Name + "/" + bare, nil
		}
		return c, bare, "ext:" + c.Name + "/" + bare, nil
	}

	m.mu.RLock()
	channels := append([]*Channel(nil), m.channels...)
	m.mu.RUnlock()

	var found *Channel
	var foundName, foundUpstream string
	for _, c := range channels {
		if c.Disabled {
			continue
		}
		ids := c.availableModels()
		for _, id := range ids {
			if id != model {
				continue
			}
			if found != nil {
				return nil, "", "", fmt.Errorf(
					"model %q exists in multiple ext channels, use ext:<channel>/%s prefix", model, model)
			}
			found, foundName, foundUpstream = c, model, model
		}
		for _, alias := range c.aliasKeys() {
			if alias != model {
				continue
			}
			if found != nil {
				return nil, "", "", fmt.Errorf(
					"model %q exists in multiple ext channels, use ext:<channel>/%s prefix", model, model)
			}
			target, _ := c.aliasFor(alias)
			found, foundName, foundUpstream = c, alias, target
		}
	}
	if found == nil {
		return nil, "", "", errNotExt
	}
	return found, foundUpstream, "ext:" + found.Name + "/" + foundName, nil
}

// RouteFor 解析请求模型名 → (渠道, 上游模型名)。前缀 "ext:<channel>/<model>"
// 优先；裸名时在各渠道可用模型/别名中查唯一匹配。
//
// 兼容包装：只返回上游模型名（别名已解析），需要客户端可见名时用 RouteModel。
func (m *Manager) RouteFor(model string) (*Channel, string, error) {
	ch, upstream, _, err := m.RouteModel(model)
	return ch, upstream, err
}

// IsExtModel 报告模型是否可由外部渠道服务（裸名或前缀形态）。
func (m *Manager) IsExtModel(model string) bool {
	_, _, err := m.RouteFor(model)
	return err == nil
}

// aliasFor 查渠道别名表（返回上游模型名）。
func (c *Channel) aliasFor(alias string) (string, bool) {
	if c == nil || alias == "" {
		return "", false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.Aliases[alias]
	return v, ok
}

// aliasKeys 返回别名键清单（稳定顺序：按 map 遍历一次后排序，保证 /v1/models
// 输出可复现——客户端与测试都受益）。
func (c *Channel) aliasKeys() []string {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	keys := make([]string, 0, len(c.Aliases))
	for k := range c.Aliases {
		keys = append(keys, k)
	}
	c.mu.Unlock()
	sortStrings(keys)
	return keys
}

// sortStrings 简单插入排序（避免为一个字符串切片引入 sort 依赖的额外考虑；
// 别名数量有上限 200，插入排序足够）。
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// ModelList 汇总全部启用渠道的模型（/v1/models 合并用）。含自动发现（懒触发）
// 与别名条目（带 alias_of 字段；OpenAI 客户端忽略未知字段）。
func (m *Manager) ModelList(ctx context.Context) []map[string]any {
	m.mu.RLock()
	chs := append([]*Channel(nil), m.channels...)
	m.mu.RUnlock()
	out := []map[string]any{}
	for _, ch := range chs {
		if ch.Disabled {
			continue
		}
		ids := ch.availableModels()
		ch.mu.Lock()
		explicit := len(ch.Models) > 0
		ch.mu.Unlock()
		if !explicit {
			ids = m.EnsureModels(ctx, ch)
		}
		for _, id := range ids {
			out = append(out, map[string]any{
				"id":       "ext:" + ch.Name + "/" + id,
				"object":   "model",
				"created":  1753600000,
				"owned_by": "ext:" + ch.Name,
			})
		}
		for _, alias := range ch.aliasKeys() {
			target, _ := ch.aliasFor(alias)
			out = append(out, map[string]any{
				"id":       "ext:" + ch.Name + "/" + alias,
				"object":   "model",
				"created":  1753600000,
				"owned_by": "ext:" + ch.Name,
				"alias_of": target,
			})
		}
	}
	return out
}

// ModelExists 报告某渠道是否声明了某模型或别名（前缀路由用）。
func (m *Manager) ModelExists(channel, model string) bool {
	ch := m.Get(channel)
	if ch == nil {
		return false
	}
	if _, ok := ch.aliasFor(model); ok {
		return true
	}
	ids := ch.availableModels()
	if len(ids) == 0 {
		return true // 目录未知（显式空 + 未发现成功）：放行，错误由上游报
	}
	for _, id := range ids {
		if id == model {
			return true
		}
	}
	return false
}
