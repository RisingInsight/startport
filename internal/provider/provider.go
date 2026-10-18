// Package provider 外部渠道的「出站协议适配层」。
//
// 设计边界（多平台阶段三，见 deliverables/software-company/starport-multiplatform-arch.md）：
//
//   - **canonical wire format = OpenAI Chat Completions JSON**。本包不做一套独立的
//     IR 类型层级：Adapter 直接做 "OpenAI body ↔ Anthropic/Gemini body" 的转换。
//     入站 Anthropic 请求由 server 侧既有的 anthropicToOpenAI 先转成 OpenAI wire，
//     再交给 Adapter；响应由 Adapter 折回 OpenAI wire，server 侧既有
//     extMessagesSync/extMessagesStream 再折回 Anthropic。这样 2 入站 × 3 出站
//     六格复用同一条链路，转换只写一份。
//   - Adapter **只做协议，不做治理**：不碰渠道/凭证状态机、不重试、不记账、不落盘。
//     治理在 internal/extchan（Do / Credential）。Adapter 无状态，可并发共享。
//   - 零回归是硬约束：OpenAI 出站的 URL 规则必须与历史 endpoint() 逐字节一致
//     （legacyOpenAIRoot 是刻意保留的兼容 shim，新协议不要复用）。
package provider

import (
	"fmt"
	"strings"
)

// Protocol 出站协议类型（config.ext_channels[].protocol）。
type Protocol string

const (
	// ProtocolOpenAI OpenAI Chat Completions（缺省；老配置零修改）。
	ProtocolOpenAI Protocol = "openai"
	// ProtocolAnthropic Anthropic Messages（/v1/messages）。
	ProtocolAnthropic Protocol = "anthropic"
	// ProtocolGemini Google Generative Language（/v1beta/models/...）。
	ProtocolGemini Protocol = "gemini"
)

// protocolOrder 合法值清单（错误文案按此顺序给出，便于用户照抄）。
var protocolOrder = []Protocol{ProtocolOpenAI, ProtocolAnthropic, ProtocolGemini}

// ParseProtocol 解析协议名：空串归一为 openai（老配置语义），大小写不敏感，
// 未知值返回带合法值清单的错误（config 校验直接透出给用户）。
func ParseProtocol(s string) (Protocol, error) {
	p := Protocol(strings.ToLower(strings.TrimSpace(s)))
	switch p {
	case "":
		return ProtocolOpenAI, nil
	case ProtocolOpenAI, ProtocolAnthropic, ProtocolGemini:
		return p, nil
	default:
		return "", fmt.Errorf("protocol: %q 不是合法值（可用：openai / anthropic / gemini）", s)
	}
}

// Valid 报告协议是否在合法集合内。
func (p Protocol) Valid() bool {
	for _, v := range protocolOrder {
		if p == v {
			return true
		}
	}
	return false
}

// String 实现 fmt.Stringer（观测/日志用）。
func (p Protocol) String() string { return string(p) }

// Protocols 返回合法协议清单的副本（面板下拉/文档用）。
func Protocols() []Protocol {
	out := make([]Protocol, len(protocolOrder))
	copy(out, protocolOrder)
	return out
}

// ProtocolNames 返回合法协议名的字符串清单（错误文案/面板用）。
func ProtocolNames() []string {
	out := make([]string, 0, len(protocolOrder))
	for _, p := range protocolOrder {
		out = append(out, string(p))
	}
	return out
}
