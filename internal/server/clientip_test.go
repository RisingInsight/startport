package server

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

func reqWith(remote, xff, real string) *http.Request {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = remote
	if xff != "" {
		r.Header.Set("X-Forwarded-For", xff)
	}
	if real != "" {
		r.Header.Set("X-Real-IP", real)
	}
	return r
}

// 白名单为空（安全缺省）→ 转发头一律不采信，用直连地址。
// 这是修复的核心：伪造 XFF 不再能污染审计来源与上游风控判据。
func TestResolveClientIPUntrustedByDefault(t *testing.T) {
	r := reqWith("203.0.113.7:5555", "1.2.3.4", "5.6.7.8")
	if got := resolveClientIP(r, nil); got != "203.0.113.7" {
		t.Errorf("trusted=nil: got %q, want 直连地址 203.0.113.7（XFF 必须被忽略）", got)
	}
}

// 直连来源不在白名单内 → 转发头不采信（即便全局配了白名单）。
func TestResolveClientIPDirectPeerNotTrusted(t *testing.T) {
	nets, err := ParseTrustedProxies([]string{"127.0.0.1", "10.0.0.0/8"})
	if err != nil {
		t.Fatal(err)
	}
	r := reqWith("203.0.113.7:5555", "1.2.3.4", "")
	if got := resolveClientIP(r, nets); got != "203.0.113.7" {
		t.Errorf("直连不在白名单: got %q, want 203.0.113.7", got)
	}
}

// 直连来源在白名单内 → 采信转发头首段（反代场景：拿到真实客户端）。
func TestResolveClientIPTrustsWhenPeerTrusted(t *testing.T) {
	nets, _ := ParseTrustedProxies([]string{"10.0.0.0/8"})
	cases := []struct {
		remote, xff, real, want string
	}{
		{"10.1.2.3:1234", "203.0.113.9, 10.1.2.3", "203.0.113.9", "203.0.113.9"}, // XFF 首段优先
		{"10.1.2.3:1234", "", "203.0.113.9", "203.0.113.9"},                      // 无 XFF 回落 X-Real-IP
		{"10.1.2.3:1234", "", "", "10.1.2.3"},                                    // 转发头全无 → 直连
	}
	for _, c := range cases {
		if got := resolveClientIP(reqWith(c.remote, c.xff, c.real), nets); got != c.want {
			t.Errorf("remote=%s xff=%q real=%q: got %q, want %q", c.remote, c.xff, c.real, got, c.want)
		}
	}
}

// CIDR 与单 IP 解析；非法条目必须报错（不 fail-open）。
func TestParseTrustedProxies(t *testing.T) {
	nets, err := ParseTrustedProxies([]string{"127.0.0.1", "10.0.0.0/8", "  ", "2001:db8::1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(nets) != 3 {
		t.Fatalf("解析出 %d 条，want 3（空条目跳过）", len(nets))
	}
	if !nets[0].Contains(net.ParseIP("127.0.0.1")) {
		t.Errorf("单 IP 127.0.0.1 应解析为 /32 且包含自身")
	}
	if !nets[1].Contains(net.ParseIP("10.9.9.9")) {
		t.Errorf("CIDR 10.0.0.0/8 应包含 10.9.9.9")
	}
	if !nets[2].Contains(net.ParseIP("2001:db8::1")) {
		t.Errorf("IPv6 单 IP 应解析为 /128")
	}
	if _, err := ParseTrustedProxies([]string{"not-an-ip"}); err == nil {
		t.Errorf("非法条目必须报错（安全配置不能 fail-open）")
	}
}

// 进程级白名单必须能被装配并生效（SetTrustedProxies 是启动装配入口）。
func TestSetTrustedProxiesApplies(t *testing.T) {
	old := trustedProxies
	t.Cleanup(func() { trustedProxies = old })
	SetTrustedProxies(nil)
	if got := clientIPForLog(reqWith("203.0.113.7:1", "1.2.3.4", "")); got != "203.0.113.7" {
		t.Errorf("缺省不采信: got %q", got)
	}
	nets, _ := ParseTrustedProxies([]string{"203.0.113.7"})
	SetTrustedProxies(nets)
	if got := clientIPForLog(reqWith("203.0.113.7:1", "1.2.3.4", "")); got != "1.2.3.4" {
		t.Errorf("命中白名单应采信: got %q, want 1.2.3.4", got)
	}
}
