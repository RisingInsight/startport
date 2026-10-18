// native.go 平台专属（原生）适配器的命名注册表。
//
// 为什么需要它：`Adapter` 是按**协议**注册的（openai / anthropic / gemini），
// 而有一类上游的「协议兼容但鉴权/请求形态不标准」——例如 Loomy 要求同一个
// session 同时出现在 `token` 与 `Authorization: Bearer` 两个头上，用四种 auth
// 中的任何一种都表达不了（只能二选一）。把它们塞进协议维度会让 `protocol`
// 的语义失真，因此单开一个**命名维度** `adapter`：
//
//	protocol  —— 报文形态（决定 URL 模板与 body 编解码）
//	adapter   —— 平台专属覆盖（决定鉴权/静态头等平台契约）
//
// 设计边界：
//   - `DefaultRegistry` 与 `MustGet` 的语义不变（三协议基准，零回归）；
//   - 未注册的 `adapter` 一律**返回 error**，绝不静默回落到协议适配器——
//     静默回落会让"配置写错一个字母"变成"用错鉴权方式打上游"，排查成本极高；
//   - 原生适配器与协议适配器同接口（`Adapter`），治理链路（选证/换证/冷却/
//     记账）完全不需要知道它的存在。
package provider

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

// NativeAdapter 平台专属适配器：在通用协议之外覆盖 URL / 鉴权 / 请求形态。
//
// 它仍然声明自己承载的 `Protocol`（Loomy 是 openai 形态），所以入站 → 出站的
// 两段转换与治理链路对它无感。
type NativeAdapter interface {
	Adapter
	// Name 稳定标识（config.ext_channels[].adapter 的取值）。
	Name() string
}

var (
	nativeMu       sync.RWMutex
	nativeRegistry = map[string]NativeAdapter{}
)

// RegisterNative 注册原生适配器（同名后者覆盖；nil / 空名忽略）。
//
// 只在装配期（包的 init）调用；运行期只读，走 Native / AdapterFor。
func RegisterNative(a NativeAdapter) {
	if a == nil {
		return
	}
	name := normalizeNativeName(a.Name())
	if name == "" {
		return
	}
	nativeMu.Lock()
	defer nativeMu.Unlock()
	nativeRegistry[name] = a
}

// Native 按名取原生适配器（大小写/空白不敏感）。
func Native(name string) (NativeAdapter, bool) {
	key := normalizeNativeName(name)
	if key == "" {
		return nil, false
	}
	nativeMu.RLock()
	defer nativeMu.RUnlock()
	a, ok := nativeRegistry[key]
	return a, ok
}

// NativeNames 已注册原生适配器的名字清单（排序，返回副本；错误文案/面板用）。
func NativeNames() []string {
	nativeMu.RLock()
	out := make([]string, 0, len(nativeRegistry))
	for k := range nativeRegistry {
		out = append(out, k)
	}
	nativeMu.RUnlock()
	sort.Strings(out)
	return out
}

// AdapterFor 按 (protocol, 原生适配器名) 选适配器。
//
//   - nativeName 非空：必须命中已注册的原生适配器，**且其协议与 protocol 一致**
//     （Loomy 只能是 openai；配成 anthropic 是明确的配置错，不是可回落的模糊态）；
//     未注册或协议不匹配都返回 error；
//   - nativeName 为空：回落 `DefaultRegistry` 的协议适配器（老配置零影响）；
//   - protocol 为空按 openai（与 ParseProtocol 的老配置语义一致）。
func AdapterFor(p Protocol, nativeName string) (Adapter, error) {
	proto := p
	if proto == "" {
		proto = ProtocolOpenAI
	}
	if !proto.Valid() {
		return nil, fmt.Errorf("protocol: %q 不是合法值（可用：%s）",
			string(p), strings.Join(ProtocolNames(), " / "))
	}
	name := normalizeNativeName(nativeName)
	if name == "" {
		a, ok := DefaultRegistry.Get(proto)
		if !ok {
			return nil, fmt.Errorf("protocol: 没有注册 %q 协议的适配器", string(proto))
		}
		return a, nil
	}
	na, ok := Native(name)
	if !ok {
		avail := NativeNames()
		if len(avail) == 0 {
			return nil, fmt.Errorf(
				"adapter: %q 不是已注册的原生适配器（当前没有可用的原生适配器；留空 = 按协议选择）",
				nativeName)
		}
		return nil, fmt.Errorf("adapter: %q 不是已注册的原生适配器（可用：%s；留空 = 按协议选择）",
			nativeName, strings.Join(avail, " / "))
	}
	if na.Protocol() != proto {
		return nil, fmt.Errorf("adapter: %q 只支持 protocol=%s（当前配置是 %q）",
			name, string(na.Protocol()), string(proto))
	}
	return na, nil
}

// normalizeNativeName 名字归一（小写 + 去首尾空白）；空串 = 未指定。
func normalizeNativeName(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}
