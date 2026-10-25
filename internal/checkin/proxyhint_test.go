package checkin

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

// 回归（2026-10-25 真实案例）：机器环境变量 HTTP_PROXY 指向未启动的本机代理时，
// 签到/余额只报「请求失败（网络）」无从定位。接线 netdiag 后，拨号打到环境
// 代理地址的错误必须点名代理。e2e 断言走真实 http.Client → 传输层拨号。
func TestNetworkErrorCarriesProxyHint(t *testing.T) {
	// 借一个刚释放的本地端口当「死代理」：无人监听 → 拨号必被拒。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("无法占用临时端口: %v", err)
	}
	deadProxy := ln.Addr().String()
	ln.Close()

	// 代理变量隔离：不用 t.Setenv(k, "") 清除——Windows 上置空即删除且大小写
	// 不敏感，会把宿主机的大写变量一并删掉（本机真实带代理变量运行测试）。
	saved := map[string]string{}
	for _, k := range []string{"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy"} {
		if v, ok := os.LookupEnv(k); ok {
			saved[k] = v
		}
		os.Unsetenv(k)
	}
	os.Setenv("HTTP_PROXY", "http://"+deadProxy)
	t.Cleanup(func() {
		for k := range saved {
			os.Setenv(k, saved[k])
		}
	})

	// 目标故意用非回环域名：httpproxy 对 loopback 目标免代理，会绕开这条路径。
	// 代理不走环境变量驱动而用显式 ProxyURL：ProxyFromEnvironment 在进程内
	// 首次使用时缓存环境快照，全量测试里更早的请求已把宿主机真实代理固化，
	// 本测试再改环境变量不再生效（单独跑通过、全量跑必挂的根因）。
	proxyURL, err := url.Parse("http://" + deadProxy)
	if err != nil {
		t.Fatalf("解析代理地址: %v", err)
	}
	c := &autoclawChecker{Base: "http://upstream.invalid",
		Client: &http.Client{Timeout: 5 * time.Second,
			Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}},
		Now: time.Now}
	_, err = c.userapiGet(context.Background(), "tok", "/api/x")
	if err == nil {
		t.Fatal("死代理下请求应失败")
	}
	msg := err.Error()
	if !strings.Contains(msg, "（网络）") {
		t.Fatalf("应保留既有网络失败文案：%q", msg)
	}
	if !strings.Contains(msg, "环境代理 "+deadProxy) || !strings.Contains(msg, "不可达") {
		t.Fatalf("错误应点名不可达的环境代理 %s：%q", deadProxy, msg)
	}
}
