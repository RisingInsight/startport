package alert

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// BuildPayload 四渠道模板锚点。
func TestBuildPayloadTemplates(t *testing.T) {
	p, ct, err := BuildPayload(TypeFeishu, "标题", "细节行1\n细节行2")
	if err != nil || ct != "application/json" {
		t.Fatalf("feishu ct=%s err=%v", ct, err)
	}
	var m map[string]any
	if err := json.Unmarshal(p, &m); err != nil {
		t.Fatal(err)
	}
	if m["msg_type"] != "text" {
		t.Fatalf("feishu msg_type = %v", m["msg_type"])
	}

	p, ct, _ = BuildPayload(TypeDingtalk, "标题", "细节")
	m = nil
	_ = json.Unmarshal(p, &m)
	if m["msgtype"] != "text" || ct != "application/json" {
		t.Fatalf("dingtalk shape wrong: %s", p)
	}

	p, ct, _ = BuildPayload(TypeServerChan, "标题", "细节")
	if ct != "application/x-www-form-urlencoded" || !strings.Contains(string(p), "title=") {
		t.Fatalf("serverchan form wrong: %s / %s", ct, p)
	}

	p, ct, _ = BuildPayload(TypeGeneric, "标题", "细节")
	m = nil
	_ = json.Unmarshal(p, &m)
	if m["title"] != "标题" || ct != "application/json" {
		t.Fatalf("generic shape wrong: %s", p)
	}

	if _, _, err := BuildPayload("bogus", "t", "b"); err == nil {
		t.Fatal("bogus type should error")
	}
}

// 冷却去重：同键冷却内只发第一条；不同键互不影响；过冷却后再发。
func TestSendCooldownDedup(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	m := New(Config{Enabled: true, WebhookType: TypeGeneric, WebhookURL: srv.URL, CooldownMinutes: 30}, nil)
	m.Send("acct:u1", "第一次", "详情")
	m.Send("acct:u1", "第二次", "详情") // 冷却内：跳过
	m.Send("acct:u2", "别的键", "详情") // 不同键：照发
	if got := hits.Load(); got != 2 {
		t.Fatalf("hits = %d, want 2（同键去重、异键独立）", got)
	}
	// 把计时拨过冷却窗口 → 再发。
	m.mu.Lock()
	m.last["acct:u1"] = time.Now().Add(-31 * time.Minute)
	m.mu.Unlock()
	m.Send("acct:u1", "过了冷却", "详情")
	if got := hits.Load(); got != 3 {
		t.Fatalf("hits = %d, want 3", got)
	}
}

// 未启用/未配置：普通 Send 静默；SendTest 报错交回面板。
func TestSendInactive(t *testing.T) {
	m := New(Config{}, nil)
	m.Send("k", "t", "b") // 不 panic 不报错
	if err := m.SendTest("t", "b"); err == nil {
		t.Fatal("SendTest on inactive should error")
	}
	m = New(Config{Enabled: true, WebhookType: TypeFeishu, WebhookURL: ""}, nil)
	if err := m.SendTest("t", "b"); err == nil {
		t.Fatal("SendTest without url should error")
	}
}

// 失败（非 2xx / 不可达）返回错误且不记 last——下次事件还能再试。
func TestSendFailureNoDedup(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer srv.Close()
	m := New(Config{Enabled: true, WebhookType: TypeGeneric, WebhookURL: srv.URL}, nil)
	if err := m.SendTest("t", "b"); err == nil {
		t.Fatal("500 should surface as error")
	}
	m.mu.RLock()
	_, sent := m.last["k"]
	m.mu.RUnlock()
	if sent {
		t.Fatal("failed send must not arm cooldown")
	}
}

// Reconfigure 热生效：关掉后 Send 静默。
func TestReconfigure(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer srv.Close()
	m := New(Config{Enabled: true, WebhookType: TypeGeneric, WebhookURL: srv.URL}, nil)
	m.Send("k", "t", "b")
	m.Reconfigure(Config{Enabled: false, WebhookType: TypeGeneric, WebhookURL: srv.URL})
	m.Send("k2", "t", "b")
	if got := hits.Load(); got != 1 {
		t.Fatalf("hits = %d, want 1（禁用后静默）", got)
	}
	if m.Active() {
		t.Fatal("Active should be false after disable")
	}
}
