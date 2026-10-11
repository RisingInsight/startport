package main

import (
	"errors"
	"net"
	"syscall"
	"testing"
)

func TestPanelURL(t *testing.T) {
	cases := map[string]string{
		"127.0.0.1:7863": "http://127.0.0.1:7863/startport/",
		":7863":          "http://127.0.0.1:7863/startport/",
		"0.0.0.0:7863":   "http://127.0.0.1:7863/startport/",
		"localhost:9000": "http://localhost:9000/startport/",
	}
	for in, want := range cases {
		if got := panelURL(in); got != want {
			t.Errorf("panelURL(%q)=%q want %q", in, got, want)
		}
	}
}

func TestShouldAutoOpenRespectsFlag(t *testing.T) {
	// 显式关闭时必须为 false（与是否在容器内无关）。
	if shouldAutoOpen(false) {
		t.Error("shouldAutoOpen(false) must be false")
	}
}

func TestIsAddrInUse(t *testing.T) {
	if !isAddrInUse(&net.OpError{Err: syscall.EADDRINUSE}) {
		t.Error("syscall.EADDRINUSE 应被识别")
	}
	if !isAddrInUse(errors.New("listen tcp 127.0.0.1:7863: bind: Only one usage of each socket address (protocol/network address/port) is normally permitted.")) {
		t.Error("Windows 措辞应被识别")
	}
	if !isAddrInUse(errors.New("bind: address already in use")) {
		t.Error("Unix 措辞应被识别")
	}
	if isAddrInUse(errors.New("permission denied")) {
		t.Error("无关错误不应被识别为端口占用")
	}
	if isAddrInUse(nil) {
		t.Error("nil 应为 false")
	}
}
