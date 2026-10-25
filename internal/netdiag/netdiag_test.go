package netdiag

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
)

// withProxyEnv 隔离四个代理环境变量（保存→清空→注入用例值→测后还原）。
// 不用 t.Setenv(k, "") 做「清除」：Windows 上变量值置空等于删除，且名称
// 大小写不敏感——置空小写变体会把宿主机的大写 HTTPS_PROXY 一并删掉，
// 本机真实存在这些变量，直接踩中（详见 netdiag.go 包注释的案例）。
func withProxyEnv(t *testing.T, set map[string]string) {
	t.Helper()
	keys := []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy"}
	saved := map[string]string{}
	for _, k := range keys {
		if v, ok := os.LookupEnv(k); ok {
			saved[k] = v
		}
		os.Unsetenv(k)
	}
	for k, v := range set {
		os.Setenv(k, v)
	}
	t.Cleanup(func() {
		for _, k := range keys {
			os.Unsetenv(k)
		}
		for k, v := range saved {
			os.Setenv(k, v)
		}
	})
}

// 传输错误的真实形状：http.Client.Do 把 *url.Error 往上抛，
// 内层是 net.OpError 的文本（Windows connectex / Linux connection refused）。
func dialErr(target string) error {
	return &url.Error{
		Op:  "Post",
		URL: "https://example.invalid/api",
		Err: fmt.Errorf("dial tcp %s: connectex: No connection could be made because the target machine actively refused it", target),
	}
}

func TestProxyHintMatchesDialToEnvProxy(t *testing.T) {
	withProxyEnv(t, map[string]string{"HTTPS_PROXY": "http://127.0.0.1:7897"})

	got := ProxyHint(dialErr("127.0.0.1:7897"))
	if got == "" {
		t.Fatal("拨号目标 == 环境代理地址时应有提示")
	}
	if !strings.Contains(got, "127.0.0.1:7897") || !strings.Contains(got, "环境代理") {
		t.Fatalf("提示应点名代理地址与「环境代理」：%q", got)
	}
}

func TestProxyHintLinuxRefusedText(t *testing.T) {
	withProxyEnv(t, map[string]string{"https_proxy": "http://127.0.0.1:7897"})
	err := &url.Error{Op: "Get", URL: "https://example.invalid/api",
		Err: errors.New("dial tcp 127.0.0.1:7897: connect: connection refused")}
	if ProxyHint(err) == "" {
		t.Fatal("Linux connection refused 文本也应识别（小写环境变量）")
	}
}

func TestProxyHintNoEnvOrWrongTarget(t *testing.T) {
	for name, tt := range map[string]struct {
		env      map[string]string
		err      error
		wantHint bool
	}{
		"无环境变量":     {nil, dialErr("127.0.0.1:7897"), false},
		"拨的是上游而非代理": {map[string]string{"HTTPS_PROXY": "http://127.0.0.1:7897"}, dialErr("203.0.113.7:443"), false},
		"nil 错误":    {map[string]string{"HTTPS_PROXY": "http://127.0.0.1:7897"}, nil, false},
		"非网络错误":     {map[string]string{"HTTPS_PROXY": "http://127.0.0.1:7897"}, errors.New("boom"), false},
		"缺省端口补全":    {map[string]string{"HTTPS_PROXY": "http://proxy.lan"}, dialErr("proxy.lan:80"), true},
		"非法代理值放弃":   {map[string]string{"HTTPS_PROXY": "://bad"}, dialErr("127.0.0.1:7897"), false},
	} {
		t.Run(name, func(t *testing.T) {
			withProxyEnv(t, tt.env)
			got := ProxyHint(tt.err)
			if tt.wantHint && got == "" {
				t.Fatalf("应给出提示：%v", tt.err)
			}
			if !tt.wantHint && got != "" {
				t.Fatalf("不应给出提示，得到：%q", got)
			}
		})
	}
}
