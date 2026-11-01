// modelgroup.go 路由组的入口解析（借鉴 magpie routing groups 的准入改写层）。
//
// group/<name> 在三个入站协议入口（chat completions / messages / responses）
// 统一经 resolveGroupModel 改写为成员的规范模型名，随后的 ext 短路、realm 解析、
// 选号、body 重写全部复用既有链路——组层不引入第二条转发路径。
package server

import (
	"fmt"
	"strings"

	"github.com/RisingInsight/startport/internal/modelgroup"
)

// isGroupModel 报告模型名是否为路由组形态（group/<name>，name 禁再含 '/'）。
// "group/" 前缀是路由协议保留段（配置校验拒绝成员/裸名以它开头）。
func isGroupModel(model string) bool {
	rest, ok := strings.CutPrefix(model, "group/")
	return ok && rest != "" && !strings.Contains(rest, "/")
}

// resolveGroupModel 把 group/<name> 解析为按策略选出的成员模型名。
// 返回 (成员规范模型名, 组定义, nil)；模型不是 group/ 前缀形态时原样返回
// （零错误）——调用方无需预判。group/ 是路由协议保留段：前缀下的畸形形态
// （空名 / 多段路径）与未知/停用的组一样返回错误（三个入口各自按协议回 400）。
func (h *Handler) resolveGroupModel(model string) (string, modelgroup.Def, error) {
	if !strings.HasPrefix(model, "group/") {
		return model, modelgroup.Def{}, nil
	}
	if h.cfg.ModelGroups == nil {
		return "", modelgroup.Def{}, fmt.Errorf("路由组功能未启用（group/ 前缀已保留）")
	}
	name := strings.TrimPrefix(model, "group/")
	if name == "" || strings.Contains(name, "/") {
		return "", modelgroup.Def{}, fmt.Errorf("路由组名不合法：%q（应为 group/<名称>）", model)
	}
	member, g, ok := h.cfg.ModelGroups.Pick(name, h.memberServable)
	if !ok {
		return "", modelgroup.Def{}, fmt.Errorf("未知或已停用的路由组 group/%s", name)
	}
	return member, g, nil
}

// memberServable 路由组成员的**咨询性**健康判定（准入选员用，不是路由权威）。
// 判定不过的成员在选员时靠后/跳过；全部不过仍会选出一个成员（组层不吞错误，
// 请求落到既有错误路径拿精确原因）。健康快照允许过旧：
//   - ext 成员：渠道存在、未停用且状态 available（cooling / all_invalid /
//     unavailable 等一律视为暂不可用）；
//   - 池成员：该模型未全池模型级冷却（6004/11102），且对应 realm 当前可服务。
func (h *Handler) memberServable(raw string) bool {
	switch {
	case strings.HasPrefix(raw, "ext:"):
		if h.cfg.Ext == nil {
			return false
		}
		chName, _, ok := h.cfg.Ext.HasPrefix(raw)
		if !ok {
			return false
		}
		ch := h.cfg.Ext.Get(chName)
		if ch == nil {
			return false
		}
		return ch.Snapshot().State == "available"
	default:
		if h.cfg.Pool == nil {
			return false
		}
		realm, bare := resolveModel(raw)
		if h.cfg.Pool.ModelBlocked(bare).Blocked {
			return false
		}
		return h.cfg.Pool.ServableForRealm(realm)
	}
}

// nextGroupMember 返回组内下一个未尝试的成员（按声明序）；全部试过返回 ""。
// tried 的键是成员规范模型名（config 校验保证组内无重复）。
func nextGroupMember(g *modelgroup.Def, tried map[string]bool) string {
	for _, m := range g.Members {
		if !tried[m] {
			return m
		}
	}
	return ""
}
