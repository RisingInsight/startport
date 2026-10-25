package upstream

import "testing"

func TestValidateOutboundBaseAccepts(t *testing.T) {
	for _, raw := range []string{
		"",                             // 空 = 内置默认
		"https://copilot.tencent.com",  // 公网 https
		"http://example.com:8080/path", // 公网 http + 端口 + 路径
		"https://www.codebuddy.cn/v2/", // 公网 https + 路径
	} {
		if err := ValidateOutboundBase(raw, "global.chat_base"); err != nil {
			t.Errorf("Validate(%q) = %v, want nil", raw, err)
		}
	}
}

func TestValidateOutboundBaseRejects(t *testing.T) {
	cases := map[string]string{
		"file:///etc/passwd":              "file scheme",
		"gopher://evil.internal:70/_x":    "gopher（经典 SSRF 打法）",
		"ftp://evil.example":              "ftp scheme",
		"https://169.254.169.254":         "云元数据端点",
		"http://127.0.0.1:8080":           "回环",
		"http://localhost:8080":           "localhost",
		"http://10.1.2.3":                 "私有网 10/8",
		"http://192.168.1.1":              "私有网 192.168/16",
		"http://172.16.0.1":               "私有网 172.16/12",
		"http://100.64.0.1":               "CGNAT",
		"http://0.0.0.0":                  "0/8",
		"http://[::1]":                    "IPv6 回环",
		"http://[fc00::1]":                "IPv6 ULA",
		"http://metadata.google.internal": "元数据主机名",
		"http://foo.local":                ".local",
		"https://":                        "缺 host",
		"://bad":                          "非法 URL",
	}
	for raw, why := range cases {
		if err := ValidateOutboundBase(raw, "global.chat_base"); err == nil {
			t.Errorf("Validate(%q) = nil, want error（%s）", raw, why)
		}
	}
}

// 域名形式不做 DNS 解析（理由见 IsForbiddenOutboundHost 注释）：无法拦截
// 「域名解析到内网」的场景，但也不应把正常公网域名误杀。
func TestForbiddenOutboundHostDomainNotResolved(t *testing.T) {
	if IsForbiddenOutboundHost("example.com") {
		t.Errorf("公网域名不应被拒")
	}
	for _, h := range []string{"127.0.0.1", "localhost", "LOCALHOST.", "169.254.169.254", "metadata", "a.internal"} {
		if !IsForbiddenOutboundHost(h) {
			t.Errorf("%q 应被拒", h)
		}
	}
}
