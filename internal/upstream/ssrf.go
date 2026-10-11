// ssrf.go 出站 base URL 的 SSRF 防护（安全官 F-009）。
//
// 风险：`global.chat_base` / `global.billing_base` 是**用户可配**的出站地址，而发往
// 这两个地址的请求**携带账号 Bearer**（AccessToken）。若允许配置成任意 URL：
//
//  1. 指向攻击者主机 → 每次请求都把账号 token 外发出去（凭证泄漏）；
//  2. 指向 `169.254.169.254` 等云元数据端点或内网地址 → 用网关做跳板探测内网，
//     响应还会经由网关回传到调用方。
//
// 修法：在**配置装载时**校验（不是请求时——请求时校验意味着每次都多一次分支，
// 而配置是唯一的入口），非法即启动失败。绝不 fail-open。
//
// 内置默认（CN 的 copilot.tencent.com / www.codebuddy.cn，global 的内置域）不经过
// 本校验：它们是编译期常量，不是用户可控输入。
package upstream

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// ValidateOutboundBase 校验用户可配的出站 base URL。空串 = 使用内置默认（合法）。
func ValidateOutboundBase(raw, field string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%s: 非法 URL %q: %w", field, raw, err)
	}
	scheme := strings.ToLower(u.Scheme)
	// 只允许 http/https：file:// / gopher:// / ftp:// / data: 等一律拒绝
	// （gopher 与 file 是经典 SSRF 打法）。
	if scheme != "https" && scheme != "http" {
		return fmt.Errorf("%s: 只允许 http/https，拒绝 scheme %q", field, u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("%s: 缺少 host", field)
	}
	if IsForbiddenOutboundHost(host) {
		return fmt.Errorf("%s: 拒绝指向本机/内网/元数据的地址 %q（该请求带账号 Bearer，SSRF 风险）", field, host)
	}
	return nil
}

// IsForbiddenOutboundHost 报告 host 是否属于禁止出站的 SSRF 目标。
//
// 只做「字面判定」：**不解析 DNS**。理由——(1) 解析耗时与失败模式不可控，会拖慢
// 启动；(2) 解析结果与实际连接之间存在 TOCTOU，DNS rebinding 根本无法靠一次
// 解析杜绝；(3) 正常场景这两个 base 是公网域名，不需要解析。字面 IP 与明显的
// 内部/元数据主机名已覆盖绝大多数误配与攻击面。
func IsForbiddenOutboundHost(host string) bool {
	lower := strings.ToLower(strings.TrimSuffix(host, "."))
	if lower == "" {
		return true
	}
	// 域名形态：本机与常见内部/元数据命名
	switch {
	case lower == "localhost", strings.HasSuffix(lower, ".localhost"),
		strings.HasSuffix(lower, ".local"), strings.HasSuffix(lower, ".internal"),
		strings.HasPrefix(lower, "metadata."), lower == "metadata":
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false // 域名且非内部命名：放行（见上，不做 DNS 解析）
	}
	return IsForbiddenOutboundIP(ip)
}

// IsForbiddenOutboundIP 报告 IP 是否属于禁止出站的范围。
func IsForbiddenOutboundIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	// 回环 / 未指定 / 组播 / 链路本地 / 接口本地
	if ip.IsLoopback() || ip.IsUnspecified() || ip.IsMulticast() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() {
		return true
	}
	// 私有网（10/8、172.16/12、192.168/16）
	if ip.IsPrivate() {
		return true
	}
	if v4 := ip.To4(); v4 != nil {
		switch {
		case v4[0] == 0:
			return true // 0.0.0.0/8
		case v4[0] >= 224:
			return true // 组播(224-239) + 保留(240-255) + 255.255.255.255
		case v4[0] == 100 && v4[1]&0xc0 == 64:
			return true // 100.64/10 CGNAT（运营商内网）
		case v4[0] == 192 && v4[1] == 0 && v4[2] == 0:
			return true // 192.0.0.0/24 IETF 协议保留
		case v4[0] == 192 && v4[1] == 0 && v4[2] == 2:
			return true // 192.0.2.0/24 TEST-NET-1
		case v4[0] == 198 && (v4[1] == 18 || v4[1] == 19):
			return true // 198.18/15 基准测试网
		case v4[0] == 198 && v4[1] == 51 && v4[2] == 100:
			return true // 198.51.100/24 TEST-NET-2
		case v4[0] == 203 && v4[1] == 0 && v4[2] == 113:
			return true // 203.0.113/24 TEST-NET-3
		}
		return false
	}
	// IPv6 唯一本地地址 fc00::/7（ULA，等价于 IPv4 私有网）
	if len(ip) == net.IPv6len && ip[0]&0xfe == 0xfc {
		return true
	}
	return false
}
