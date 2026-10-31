// Package modelgroup 路由组（借鉴 magpie 的 routing groups）：把多个上游模型
// 合成一个可选名 group/<name>，请求按选员策略落到其中一个成员上。
//
// 与各路由层的关系：成员是**既有路由协议里的规范模型名**（cn:x / global:x /
// ext:<channel>/<model>），组只做"名字 → 成员"的一层改写，选号/凭证轮换/冷却
// 等仍由池与 extchan 各自承接——组不复制任何路由状态。选员是**准入时**的
// 健康感知决策（可用成员优先）；v1 不做请求内跨成员重试（成员失败由池内换号
// /渠道内换证先扛，客户端重试即换到健康成员），这是有意的范围裁剪。
//
// 嵌套组不支持：成员带 group/ 前缀在校验期拒绝（配置侧 validateModelGroups）。
package modelgroup

import (
	"sync"
	"sync/atomic"
)

// Def 路由组定义（config.json 的 model_groups 数组行）。
type Def struct {
	// Name 组名；group/<Name> 即对外模型名。禁含 '/'（与渠道名同一字符集约束）。
	Name string `json:"name"`
	// Members 成员模型名（规范前缀形态），按声明顺序参与选员。
	Members []string `json:"members"`
	// Routing 选员策略："order"（默认，首个可用成员）| "rotate"（可用成员间轮转）。
	Routing string `json:"routing,omitempty"`
	// Disabled 停用后 group/<Name> 不再被解析（400 未知路由组），也不进 /v1/models。
	Disabled bool   `json:"disabled,omitempty"`
	Note     string `json:"note,omitempty"`
}

// Routing 取值常量。
const (
	RoutingOrder  = "order"
	RoutingRotate = "rotate"
)

// NormalizeRouting 回落缺省策略（"" 与脏值都归 order——组开着就必须有确定语义）。
func NormalizeRouting(r string) string {
	if r == RoutingRotate {
		return RoutingRotate
	}
	return RoutingOrder
}

// Manager 组的运行态。并发安全；经 ReplaceAll 整表热替换（同 extchan 模式），
// 轮转计数按组名沿用（替换不清零，避免热改配置后轮转重新从首成员开始）。
type Manager struct {
	mu     sync.RWMutex
	byName map[string]*Def
	rotate map[string]*atomic.Uint64
}

// New 构建空 Manager。
func New() *Manager {
	return &Manager{byName: map[string]*Def{}, rotate: map[string]*atomic.Uint64{}}
}

// ReplaceAll 整表替换组定义（nil/空数组 = 清空）。
func (m *Manager) ReplaceAll(defs []Def) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.byName = make(map[string]*Def, len(defs))
	for i := range defs {
		d := defs[i]
		m.byName[d.Name] = &d
	}
}

// Get 按名取组定义。
func (m *Manager) Get(name string) (Def, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if d, ok := m.byName[name]; ok {
		return *d, true
	}
	return Def{}, false
}

// List 返回全部组定义（map 遍历序，调用方自行排序）。
func (m *Manager) List() []Def {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Def, 0, len(m.byName))
	for _, d := range m.byName {
		out = append(out, *d)
	}
	return out
}

// Pick 为 group/<name> 选出一个成员，返回 (成员规范模型名, 组定义, ok)。
//
// servable 是**咨询性**健康判定（nil = 全员视为可用）：order 取首个可用成员，
// rotate 从轮转指针起取首个可用成员；全部不可用时仍按各自策略返回一个成员
// （order=首个、rotate=轮转位），让请求落到既有错误路径拿到精确原因——组层
// 不吞错误，也不因为健康快照过旧而把组判死。
// ok=false：组不存在、已停用、或没有成员。
func (m *Manager) Pick(name string, servable func(member string) bool) (string, Def, bool) {
	m.mu.RLock()
	d, ok := m.byName[name]
	var def Def
	if ok {
		def = *d // 拷贝后释放读锁：成员表只在 ReplaceAll 写，快照读足够
	}
	m.mu.RUnlock()
	if !ok || def.Disabled || len(def.Members) == 0 {
		return "", Def{}, false
	}
	usable := func(s string) bool {
		if servable == nil {
			return true
		}
		return servable(s)
	}
	if NormalizeRouting(def.Routing) == RoutingRotate {
		m.mu.Lock()
		ctr, ok := m.rotate[def.Name]
		if !ok {
			ctr = &atomic.Uint64{}
			m.rotate[def.Name] = ctr
		}
		m.mu.Unlock()
		start := int(ctr.Add(1)-1) % len(def.Members)
		for i := 0; i < len(def.Members); i++ {
			cand := def.Members[(start+i)%len(def.Members)]
			if usable(cand) {
				return cand, def, true
			}
		}
		return def.Members[start], def, true
	}
	// order
	for _, cand := range def.Members {
		if usable(cand) {
			return cand, def, true
		}
	}
	return def.Members[0], def, true
}
