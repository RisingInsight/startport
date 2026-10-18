// qa_status_test.go QA 独立验收（server 侧）：/status 的 ext_channels 键（P0-12
// 老配置 credentials_total=1、P0-8 提醒计数、P0-11 不回显 secret）。
package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/RisingInsight/startport/internal/extchan"
	"github.com/RisingInsight/startport/internal/pool"
)

const qaStatusSecret = "QA-STATUS-SECRET-abcdef123456"

// qaStatusHandler 构造带账号池的最小 handler（/status 需要 Pool）。
func qaStatusHandler(t *testing.T, defs []extchan.ChannelDef) *Handler {
	t.Helper()
	mgr := extchan.New(extchan.Config{TimeoutSeconds: 5})
	mgr.ReplaceAll(defs)
	return NewHandler(Config{Pool: pool.New(""), Ext: mgr})
}

// qaGetStatus 取 /status 的原始 JSON 与解析结果。
func qaGetStatus(t *testing.T, h *Handler) (string, map[string]any) {
	t.Helper()
	req := httptest.NewRequest("GET", "/status", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("/status status=%d", rec.Code)
	}
	raw := rec.Body.String()
	var body map[string]any
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatalf("/status 不是合法 JSON: %v", err)
	}
	return raw, body
}

// P0-12：老配置（只有 api_key）在 /status 里表现为 1 条 label=default 凭证。
func TestQAStatusLegacyChannelCredentialsTotal(t *testing.T) {
	h := qaStatusHandler(t, []extchan.ChannelDef{{
		Name: "legacy", BaseURL: "https://api.example.com", APIKey: qaStatusSecret,
		Models: []string{"m1"},
	}})
	raw, body := qaGetStatus(t, h)

	chs, ok := body["ext_channels"].([]any)
	if !ok || len(chs) != 1 {
		t.Fatalf("ext_channels 形态异常: %v", body["ext_channels"])
	}
	ch := chs[0].(map[string]any)
	if got := ch["credentials_total"].(float64); got != 1 {
		t.Errorf("credentials_total = %v, want 1", got)
	}
	if got := ch["credentials_usable"].(float64); got != 1 {
		t.Errorf("credentials_usable = %v, want 1", got)
	}
	if got := ch["protocol"]; got != "openai" {
		t.Errorf("老配置 protocol 应归一为 openai, got %v", got)
	}
	if got := ch["state"]; got != extchan.StateAvailable {
		t.Errorf("state = %v", got)
	}
	creds, ok := ch["credentials"].([]any)
	if !ok || len(creds) != 1 {
		t.Fatalf("credentials 维度缺失: %v", ch["credentials"])
	}
	if label := creds[0].(map[string]any)["label"]; label != "default" {
		t.Errorf("api_key 应合成 label=default, got %v", label)
	}
	// 既有账号池字段仍在（只增键，零回归）。
	for _, key := range []string{"accounts", "total", "healthy", "cooling", "disabled",
		"in_flight_full", "realm_totals", "sticky_sessions", "redis_mode", "model_locks",
		"credit_floor", "cost_explore"} {
		if _, ok := body[key]; !ok {
			t.Errorf("/status 丢了既有键 %q（零回归要求）", key)
		}
	}
	if strings.Contains(raw, qaStatusSecret) {
		t.Error("/status 泄露 secret")
	}
}

// P0-8：invalid 与 3 天内到期在 /status 有字段与计数。
func TestQAStatusCredentialAlerts(t *testing.T) {
	soon := time.Now().Add(40 * time.Hour).Format("2006-01-02")
	h := qaStatusHandler(t, []extchan.ChannelDef{{
		Name: "alert", BaseURL: "https://api.example.com", Models: []string{"m1"},
		Credentials: []extchan.CredentialDef{
			{Label: "ok", Secret: qaStatusSecret},
			{Label: "soon", Secret: "QA-SOON-SECRET-000111", ExpiresAt: soon},
			{Label: "dead", Secret: "QA-DEAD-SECRET-222333"},
		},
	}})
	// 造一条 401 失效凭证。
	h.cfg.Ext.Get("alert").CredentialByLabel("dead").NoteInvalid("401 authentication failed")

	raw, body := qaGetStatus(t, h)
	ch := body["ext_channels"].([]any)[0].(map[string]any)
	if got := ch["credentials_invalid"].(float64); got != 1 {
		t.Errorf("credentials_invalid = %v", got)
	}
	if got := ch["credentials_expiring_soon"].(float64); got != 1 {
		t.Errorf("credentials_expiring_soon = %v", got)
	}
	for _, secret := range []string{qaStatusSecret, "QA-SOON-SECRET-000111", "QA-DEAD-SECRET-222333"} {
		if strings.Contains(raw, secret) {
			t.Errorf("/status 泄露 secret %q", secret)
		}
	}
}

// 未配置 ext 渠道时 /status 的 ext_channels 是空数组（脚本可直接遍历）。
func TestQAStatusNoExtChannels(t *testing.T) {
	h := NewHandler(Config{Pool: pool.New("")})
	_, body := qaGetStatus(t, h)
	chs, ok := body["ext_channels"].([]any)
	if !ok {
		t.Fatalf("ext_channels 应为数组（非 null）: %v", body["ext_channels"])
	}
	if len(chs) != 0 {
		t.Errorf("未配置渠道时应为空, got %v", chs)
	}
}
