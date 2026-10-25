// Package netdiag 把出站传输层错误翻译成一句可操作的人话提示。
//
// 动机（真实案例，2026-10-25）：机器级环境变量 HTTP_PROXY/HTTPS_PROXY 指向
// 本机代理客户端（Clash 类，127.0.0.1:7897），代理进程未启动时，Go 的
// ProxyFromEnvironment 仍把请求拨向该地址 → connectex/connection refused。
// 表现是签到/余额/登录全链路只报「请求失败（网络）」，用户无从定位——
// 拨号目标就是代理本身这件事，只有把 host:port 点出来才能看清。
//
// 契约：提示只含代理地址与操作指引，不含目标 URL / 凭证——与 checkin
// 包「error.Message 必须已脱敏」的既有约定一致。
package netdiag

import (
	"net"
	"net/url"
	"os"
	"strings"
)

// ProxyHint 判断 err 是否为「拨号打到环境代理地址失败」的传输错误，
// 是则返回一句可追加在「请求失败（网络）」后的提示，否则返回空串。
//
// 只认 err 文本里 dial 目标 == 代理 host:port 的情形（如
// `dial tcp 127.0.0.1:7897: connectex: ...`，net.OpError 固定带网络名）：
// 上游自身故障不会误报成代理问题；DNS 解析失败类错误（无解析后地址）
// 天然不匹配。
func ProxyHint(err error) string {
	if err == nil {
		return ""
	}
	target := proxyDialTarget()
	if target == "" {
		return ""
	}
	if !strings.Contains(err.Error(), "dial tcp "+target) {
		return ""
	}
	return "（环境代理 " + target + " 不可达：请启动代理客户端，或清除 HTTP_PROXY/HTTPS_PROXY 环境变量后重启本程序）"
}

// proxyDialTarget 从 HTTPS_PROXY/HTTP_PROXY（大小写各一）解析出拨号地址。
// https 请求优先看 HTTPS_PROXY，全链路上游均为 https，故以它为先。
// 环境变量不是合法 URL、或缺端口且 scheme 无公认缺省端口时保守放弃
// 返回空串——宁可不给提示，不可报错地址。
func proxyDialTarget() string {
	raw := firstNonEmpty(
		os.Getenv("HTTPS_PROXY"), os.Getenv("https_proxy"),
		os.Getenv("HTTP_PROXY"), os.Getenv("http_proxy"),
	)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return ""
	}
	host, port := u.Hostname(), u.Port()
	if port == "" {
		if p, ok := map[string]string{"http": "80", "https": "443"}[u.Scheme]; ok {
			port = p
		} else {
			return ""
		}
	}
	// IPv6 字面量必须走 JoinHostPort（[::1]:7897），与 Go 错误文本同构。
	return net.JoinHostPort(host, port)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
