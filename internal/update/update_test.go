package update

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestParseVersionAndNewer(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"1.14.0", "1.13.0", true},
		{"v1.14.0", "1.13.0-panel", true},
		{"1.13.0", "1.13.0-panel", false}, // 同版本（-panel 后缀不影响）
		{"1.13.0", "1.14.0", false},
		{"1.13.1", "1.13.0", true},
		{"2.0.0", "1.99.99", true},
		{"garbage", "1.0.0", false}, // 解析失败 → 不误报
		{"1.0.0", "garbage", false},
	}
	for _, c := range cases {
		if got := newer(c.a, c.b); got != c.want {
			t.Errorf("newer(%q,%q)=%v want %v", c.a, c.b, got, c.want)
		}
	}
}

// 可控的假 GitHub 源。
type fakeGitHub struct {
	mu      sync.Mutex
	tag     string
	pub     string
	status  int
	redirect string
}

func (f *fakeGitHub) set(tag string) { f.mu.Lock(); f.tag = tag; f.mu.Unlock() }
func (f *fakeGitHub) setStatus(s int) { f.mu.Lock(); f.status = s; f.mu.Unlock() }

func (f *fakeGitHub) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		status, tag, pub, redir := f.status, f.tag, f.pub, f.redirect
		f.mu.Unlock()
		if redir != "" {
			http.Redirect(w, r, redir, http.StatusFound)
			return
		}
		if status != 0 && status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"tag_name": tag, "published_at": pub})
	}
}

func newTestChecker(t *testing.T, srv *httptest.Server, statePath string) *Checker {
	c := New(Config{Current: "1.13.0-panel", Enabled: true, Interval: time.Hour, StatePath: statePath})
	c.baseGitHub = srv.URL
	// 兜底源指向同一 server 的固定路径，避免测试触碰真实网络。
	c.baseJsDelivr = srv.URL + "/version.json"
	return c
}

func TestRefreshDetectsUpdate(t *testing.T) {
	f := &fakeGitHub{tag: "v1.14.0", pub: "2026-10-01T00:00:00Z"}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()

	c := newTestChecker(t, srv, "")
	info := c.Refresh(context.Background())
	if !info.Available || info.Latest != "v1.14.0" {
		t.Fatalf("info=%+v, want available with latest v1.14.0", info)
	}
	if info.ReleaseURL != "https://github.com/"+defaultRepo+"/releases/tag/v1.14.0" {
		t.Errorf("release_url=%q 应由网关拼装", info.ReleaseURL)
	}
	if info.Source != "github" || !info.Enabled {
		t.Errorf("source/enabled=%q/%v", info.Source, info.Enabled)
	}
}

func TestRefreshNoUpdateWhenSameOrLower(t *testing.T) {
	f := &fakeGitHub{tag: "v1.13.0"}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()

	c := newTestChecker(t, srv, "")
	if info := c.Refresh(context.Background()); info.Available {
		t.Fatalf("同版本不应报更新: %+v", info)
	}
}

// 单调性：先见 1.20.0，再返回 1.10.0（回放）必须被拒，latest 保持 1.20.0。
func TestRefreshRejectsDowngrade(t *testing.T) {
	f := &fakeGitHub{tag: "v1.20.0"}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()

	c := newTestChecker(t, srv, "")
	if info := c.Refresh(context.Background()); info.Latest != "v1.20.0" {
		t.Fatalf("首次应见 1.20.0: %+v", info)
	}
	f.set("v1.10.0")
	info := c.Refresh(context.Background())
	if info.Latest != "v1.20.0" {
		t.Fatalf("回放低版本被采用: latest=%q want v1.20.0", info.Latest)
	}
}

// 单调性跨重启：最高版本持久化后，新进程同样拒绝回放。
func TestMonotonicPersistsAcrossRestart(t *testing.T) {
	f := &fakeGitHub{tag: "v1.20.0"}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	statePath := filepath.Join(t.TempDir(), "update-state.json")

	c1 := newTestChecker(t, srv, statePath)
	c1.Refresh(context.Background())

	// 模拟重启：新建 checker（从 state 文件恢复 highest），源改为回放低版本。
	f.set("v1.10.0")
	c2 := newTestChecker(t, srv, statePath)
	if info := c2.Refresh(context.Background()); info.Latest != "v1.20.0" {
		t.Fatalf("跨重启未拒绝回放: latest=%q want v1.20.0", info.Latest)
	}
}

// 失败静默降级：源报错时不 panic、带出 error，且保留上次结果。
func TestRefreshSilentDegradeKeepsLastResult(t *testing.T) {
	f := &fakeGitHub{tag: "v1.15.0"}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()

	c := newTestChecker(t, srv, "")
	first := c.Refresh(context.Background())
	if !first.Available {
		t.Fatalf("首次应可用: %+v", first)
	}

	f.setStatus(http.StatusInternalServerError)
	info := c.Refresh(context.Background())
	if info.Error == "" {
		t.Errorf("失败时应带出 error 供观测")
	}
	if info.Latest != "v1.15.0" {
		t.Errorf("失败应保留上次结果, latest=%q", info.Latest)
	}
}

// 主源失败时回落 jsDelivr 兜底源。
func TestJsDelivrFallback(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/github", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	mux.HandleFunc("/version.json", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"version": "1.16.0"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := New(Config{Current: "1.13.0-panel", Enabled: true})
	c.baseGitHub = srv.URL + "/github"
	c.baseJsDelivr = srv.URL + "/version.json"

	info := c.Refresh(context.Background())
	if info.Source != "jsdelivr" || !info.Available || info.Latest != "1.16.0" {
		t.Fatalf("兜底源未生效: %+v", info)
	}
}

// 重定向到白名单外主机必须被拒（防被导向站外）。
func TestRedirectToDisallowedHostRejected(t *testing.T) {
	f := &fakeGitHub{redirect: "http://evil.example.com/x"}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()

	c := newTestChecker(t, srv, "")
	info := c.Refresh(context.Background())
	if info.Available {
		t.Fatalf("跨白名单重定向不应产生更新提示: %+v", info)
	}
	if info.Error == "" {
		t.Errorf("应带出重定向被拒的 error")
	}
}

// 未启用时不发起任何网络请求。
func TestDisabledChecker(t *testing.T) {
	c := New(Config{Current: "1.13.0-panel", Enabled: false})
	c.baseGitHub = "http://127.0.0.1:1/should-not-be-called"
	info := c.Refresh(context.Background())
	if info.Enabled || info.Available || info.Error != "" {
		t.Fatalf("禁用态不应有任何检查: %+v", info)
	}
}

// Cached 不做网络 I/O：未检查过时返回带 Current 的空结果。
func TestCachedNoNetwork(t *testing.T) {
	c := New(Config{Current: "1.13.0-panel", Enabled: true})
	c.baseGitHub = "http://127.0.0.1:1/should-not-be-called"
	info := c.Cached()
	if !info.Enabled || info.Current != "1.13.0-panel" || info.Latest != "" || info.Available {
		t.Fatalf("Cached 初值异常: %+v", info)
	}
}
