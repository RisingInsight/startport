// mask_secret_test.go maskSecret 调试掩码单测：保证 STARPORT_DEBUG_CHAT 输出敏感头时
// 不外泄完整明文 token（stdout 会进 docker logs 被持久化采集）。
package upstream

import "testing"

// TestMaskSecret 掩码规则：长值保留前 8 字符 + "…"；≤8 字符一律只给 "…"（不泄露长度信息）。
func TestMaskSecret(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", "…"},
		{"short-1", "a", "…"},
		{"exactly-8", "12345678", "…"},
		{"long-9", "123456789", "12345678…"},
		{"bearer-token", "Bearer eyJhbGciOiJIUzI1NiJ9.payload.sig", "Bearer e…"},
		{"multi-byte", "令牌很长很长很长很长很长", "令牌很长很长很长…"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := maskSecret(tc.in); got != tc.want {
				t.Errorf("maskSecret(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestDebugHeadersCoversSensitive 断言敏感头集合覆盖契约头：若未来新增敏感头而
// 忘记登记，这里给出明确失败信号（而非靠人肉记忆）。
func TestDebugHeadersCoversSensitive(t *testing.T) {
	for _, h := range []string{"Authorization", "X-Refresh-Token", "X-Device-Token"} {
		if !debugHeaders[h] {
			t.Errorf("debugHeaders 缺少敏感头 %q", h)
		}
	}
	if debugHeaders["Content-Type"] || debugHeaders["User-Agent"] {
		t.Errorf("debugHeaders 不应掩码非敏感头")
	}
}
