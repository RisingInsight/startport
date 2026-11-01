package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/RisingInsight/startport/internal/auth"
	"github.com/RisingInsight/startport/internal/extchan"
	"github.com/RisingInsight/startport/internal/modelgroup"
	"github.com/RisingInsight/startport/internal/pool"
	"github.com/RisingInsight/startport/internal/upstream"
)

func poolEmptyForTest() *pool.Pool { return pool.New("") }

func testAuthForTest() *auth.Auth {
	return &auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}
}

// 路由组 failover（v2）：成员失败且**尚未写出任何字节**时按声明序换员——
// 池耗尽→ext、ext 预写失败→池、ext→ext、池→池 四向。响应一旦开始写出
// （成功或末端错误）绝不换员重放。

const extOKBody = `{"id":"c1","object":"chat.completion","created":1,"model":"m1","choices":[{"index":0,"message":{"role":"assistant","content":"从ext来"}}]}`

// newFailoverExt 构建渠道管理器：good 渠道回 200 完成，dead 渠道指向**已关闭**
// 的端口（拨号必拒 → Do 预写失败）。
func newFailoverExt(t *testing.T) *extchan.Manager {
	t.Helper()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(extOKBody))
	}))
	t.Cleanup(good.Close)
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	dead.Close() // 端口立即失效：连不上的权威形态

	mgr := extchan.New(extchan.Config{TimeoutSeconds: 5})
	mgr.ReplaceAll([]extchan.ChannelDef{
		{Name: "dead", BaseURL: dead.URL, Models: []string{"m1"},
			Credentials: []extchan.CredentialDef{{Label: "p", Secret: "s"}}},
		{Name: "good", BaseURL: good.URL, Models: []string{"m1"},
			Credentials: []extchan.CredentialDef{{Label: "p", Secret: "s"}}},
	})
	return mgr
}

// 池耗尽（空池）→ 组内 ext 成员兜底成功。
func TestGroupFailoverPoolToExt(t *testing.T) {
	mgr := newFailoverExt(t)
	gm := modelgroup.New()
	gm.ReplaceAll([]modelgroup.Def{{Name: "mix", Members: []string{"cn:glm-5.2", "ext:good/m1"}, Routing: "order"}})
	h := NewHandler(Config{Pool: poolEmptyForTest(), Ext: mgr, ModelGroups: gm, Upstream: upstream.New()})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"group/mix","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != 200 {
		t.Fatalf("code = %d body = %s, want 200 via ext fallback", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "从ext来") {
		t.Fatalf("body = %s, want ext upstream reply", rec.Body)
	}
}

// ext 成员预写失败（端口拒连）→ 换到组内池成员，走池路径成功。
func TestGroupFailoverExtToPool(t *testing.T) {
	mgr := newFailoverExt(t)
	gm := modelgroup.New()
	gm.ReplaceAll([]modelgroup.Def{{Name: "mix", Members: []string{"ext:dead/m1", "cn:glm-5.2"}, Routing: "order"}})
	up := newFakeUpstream(t, func(authz string) (int, string, bool) { return 200, sseOK, true })
	h := NewHandler(Config{Pool: testPoolWith(testAuthForTest()), Ext: mgr, ModelGroups: gm, Upstream: up})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"group/mix","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != 200 {
		t.Fatalf("code = %d body = %s, want 200 via pool after ext pre-write failure", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "你好") {
		t.Fatalf("body = %s, want pool upstream reply", rec.Body)
	}
}

// ext→ext：第一个 ext 渠道连不上，换第二个 ext 成员成功。
func TestGroupFailoverExtToExt(t *testing.T) {
	mgr := newFailoverExt(t)
	gm := modelgroup.New()
	gm.ReplaceAll([]modelgroup.Def{{Name: "two-ext", Members: []string{"ext:dead/m1", "ext:good/m1"}, Routing: "order"}})
	h := NewHandler(Config{Pool: poolEmptyForTest(), Ext: mgr, ModelGroups: gm})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"group/two-ext","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != 200 {
		t.Fatalf("code = %d body = %s, want 200 via second ext member", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "从ext来") {
		t.Fatalf("body = %s, want good channel reply", rec.Body)
	}
}

// 池→池：global 成员无账号 → 轮转内切到 cn 成员成功（粘性未绑定、模型率随成员切换）。
func TestGroupFailoverPoolToPool(t *testing.T) {
	gm := modelgroup.New()
	gm.ReplaceAll([]modelgroup.Def{{Name: "two-pool", Members: []string{"global:glm-5.2", "cn:glm-5.2"}, Routing: "order"}})
	up := newFakeUpstream(t, func(authz string) (int, string, bool) { return 200, sseOK, true })
	h := NewHandler(Config{Pool: testPoolWith(testAuthForTest()), ModelGroups: gm, Upstream: up})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"group/two-pool","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != 200 {
		t.Fatalf("code = %d body = %s, want 200 via cn member after global had no accounts", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "你好") {
		t.Fatalf("body = %s, want pool reply", rec.Body)
	}
}

// 组内全部成员失败：不无限重放，末端错误照常写出（池侧 no_healthy_account 口径）。
func TestGroupFailoverAllMembersDown(t *testing.T) {
	mgr := newFailoverExt(t)
	gm := modelgroup.New()
	gm.ReplaceAll([]modelgroup.Def{{Name: "all-down", Members: []string{"cn:glm-5.2", "ext:dead/m1"}, Routing: "order"}})
	h := NewHandler(Config{Pool: poolEmptyForTest(), Ext: mgr, ModelGroups: gm, Upstream: upstream.New()})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"group/all-down","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code == 0 || rec.Code == 200 {
		t.Fatalf("code = %d, want a definitive error (all members down)", rec.Code)
	}
}
