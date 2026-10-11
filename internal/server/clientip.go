// clientip.go 客户端来源 IP 的可信解析（安全官 F-008）。
//
// 背景：X-Forwarded-For / X-Real-IP 是**客户端可任意伪造**的请求头。原实现
// （upstream.ExtractClientIP）无条件采信首段，带来两个后果：
//
//  1. 审计日志的来源列被污染——攻击者发一个 XFF 就能把自己的来源伪装成任意地址，
//     日志追溯失效；
//  2. 该值在 upstream.passthrough_ip 开启时会**原样透传给上游**（X-Forwarded-For /
//     X-Real-IP / X-Client-IP 三等价头），污染上游的风控判据——等于让攻击者
//     决定「上游眼里我是谁」。
//
// 修法：引入可信代理白名单。**只有直连来源（RemoteAddr）落在白名单内时**才采信
// 转发头；否则一律使用直连地址。
//
// 安全缺省：白名单为空 = **从不采信**转发头。未配反代时 RemoteAddr 就是真实来源，
// 这是正确且最安全的行为；配了反代再显式声明代理网段。绝不 fail-open——配置解析
// 出错直接启动失败，而不是静默退回「无条件采信」。
package server

import (
	"fmt"
	"net"
	"net/http"
	"strings"

	"github.com/RisingInsight/startport/internal/upstream"
)

// trustedProxies 可信代理网段（进程级；启动时由 config.server.trusted_proxies 解析
// 后经 SetTrustedProxies 写入，此后只读）。空 = 从不采信转发头。
var trustedProxies []*net.IPNet

// SetTrustedProxies 设置可信代理网段（启动时调用一次）。传 nil/空切片 = 从不采信
// 转发头。仅供启动装配与测试使用。
func SetTrustedProxies(nets []*net.IPNet) { trustedProxies = nets }

// ParseTrustedProxies 把配置里的 CIDR / 单 IP 字符串解析成网段列表。
// 单 IP（不含 "/"）按 /32（IPv4）或 /128（IPv6）处理。空条目跳过。
// 任一条目非法即返回错误——静默忽略会让用户以为白名单已生效而实际没有。
func ParseTrustedProxies(items []string) ([]*net.IPNet, error) {
	var out []*net.IPNet
	for _, raw := range items {
		s := strings.TrimSpace(raw)
		if s == "" {
			continue
		}
		if _, n, err := net.ParseCIDR(s); err == nil {
			out = append(out, n)
			continue
		}
		ip := net.ParseIP(s)
		if ip == nil {
			return nil, fmt.Errorf("trusted_proxies: 非法条目 %q（需 CIDR 或 IP，如 127.0.0.1 或 10.0.0.0/8）", raw)
		}
		bits := 32
		if ip.To4() == nil {
			bits = 128
		}
		out = append(out, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
	}
	return out, nil
}

// remoteHost 取 RemoteAddr 的 host 部分（去掉端口）。无端口时原样返回。
func remoteHost(r *http.Request) string {
	if r == nil {
		return ""
	}
	addr := strings.TrimSpace(r.RemoteAddr)
	if addr == "" {
		return ""
	}
	if host, _, err := net.SplitHostPort(addr); err == nil {
		return host
	}
	return addr
}

// resolveClientIP 解析客户端 IP：仅当直连来源命中可信代理白名单时才采信转发头。
//
// trusted 为空 → 一律用直连地址（安全缺省）；非空但直连地址不在白名单内 → 同样
// 用直连地址（伪造的转发头被忽略）；命中白名单 → 用转发头首段（反代场景），
// 转发头缺失则回落直连地址。
func resolveClientIP(r *http.Request, trusted []*net.IPNet) string {
	host := remoteHost(r)
	if host == "" {
		return ""
	}
	if len(trusted) == 0 {
		return host
	}
	ip := net.ParseIP(host)
	if ip == nil || !ipInAny(ip, trusted) {
		return host
	}
	if fwd := upstream.ExtractClientIP(r); fwd != "" {
		return fwd
	}
	return host
}

// ipInAny 报告 ip 是否落在任一网段内。
func ipInAny(ip net.IP, nets []*net.IPNet) bool {
	for _, n := range nets {
		if n != nil && n.Contains(ip) {
			return true
		}
	}
	return false
}
