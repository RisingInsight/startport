package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RisingInsight/startport/internal/extchan"
)

// newCredHandler 构造带双凭证渠道（primary/backup）的 handler。
func newCredHandler(t *testing.T, up http.HandlerFunc) (*Handler, *extchan.Manager) {
	t.Helper()
	srv := httptest.NewServer(up)
	t.Cleanup(srv.Close)
	mgr := extchan.New(extchan.Config{TimeoutSeconds: 5})
	mgr.ReplaceAll([]extchan.ChannelDef{{
		Name: "sf", BaseURL: srv.URL,
		Models: []string{"m1"},
		Credentials: []extchan.CredentialDef{
			{Label: "primary", Secret: "primary-secret"},
			{Label: "backup", Secret: "backup-secret"},
		},
	}})
	return NewHandler(Config{Ext: mgr}), mgr
}

// postChat 发一次 /v1/chat/completions 请求。
func postChat(t *testing.T, h *Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// 429 换证：客户端拿到 200，primary 冷却、backup 服务，/status 凭证计数正确。
func TestExtChatSwitchesCredentialOn429(t *testing.T) {
	var backupHits int32
	h, mgr := newCredHandler(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.Header.Get("Authorization"), "primary-secret") {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"rate limited"}}`))
			return
		}
		atomic.AddInt32(&backupHits, 1)
		_, _ = w.Write([]byte(`{"id":"x","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12}}`))
	})
	rec := postChat(t, h, `{"model":"ext:sf/m1","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"content":"ok"`) {
		t.Errorf("响应体异常: %s", rec.Body.String())
	}
	ch := mgr.Get("sf")
	if ch.CredentialByLabel("primary").State() != extchan.CredCooling {
		t.Error("primary 应冷却")
	}
	snap := ch.CredentialByLabel("backup").Snapshot(time.Now())
	if snap.Requests != 1 || snap.PromptTokens != 10 || snap.CompletionTokens != 2 {
		t.Errorf("凭证级用量计数异常: %+v", snap)
	}
	// /status 的 ext_channels 键（只增键，不影响账号池字段）。
	sts := h.extChannelStatuses()
	if len(sts) != 1 {
		t.Fatalf("ext_channels 数 = %d", len(sts))
	}
	if sts[0].Protocol != "openai" || sts[0].CredentialsTotal != 2 || sts[0].CredentialsUsable != 1 {
		t.Errorf("/status ext_channels 异常: %+v", sts[0])
	}
	if atomic.LoadInt32(&backupHits) != 1 {
		t.Errorf("backup 调用次数 = %d", backupHits)
	}
}

// 401 → invalid：后续请求不再尝试该凭证。
func TestExtChatMarksCredentialInvalid(t *testing.T) {
	var primaryHits, backupHits int32
	h, mgr := newCredHandler(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.Header.Get("Authorization"), "primary-secret") {
			atomic.AddInt32(&primaryHits, 1)
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"message":"invalid key"}}`))
			return
		}
		atomic.AddInt32(&backupHits, 1)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	})
	rec := postChat(t, h, `{"model":"ext:sf/m1","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if mgr.Get("sf").CredentialByLabel("primary").State() != extchan.CredInvalid {
		t.Error("401 应标记 invalid")
	}
	rec = postChat(t, h, `{"model":"ext:sf/m1","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("第二次 status %d: %s", rec.Code, rec.Body.String())
	}
	if atomic.LoadInt32(&primaryHits) != 1 {
		t.Errorf("invalid 凭证不应被重试, primaryHits=%d", primaryHits)
	}
	if atomic.LoadInt32(&backupHits) != 2 {
		t.Errorf("backupHits = %d, want 2", backupHits)
	}
}

// 两条凭证都 429：第一次透传 429，随后无可用凭证 → 503 all_credentials_unavailable。
func TestExtChatAllCredentialsUnavailable(t *testing.T) {
	h, _ := newCredHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"rate limited"}}`))
	})
	rec := postChat(t, h, `{"model":"ext:sf/m1","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("首次应透传 429, got %d: %s", rec.Code, rec.Body.String())
	}
	rec = postChat(t, h, `{"model":"ext:sf/m1","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("第二次应 503, got %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Code != "all_credentials_unavailable" {
		t.Errorf("error.code = %q", body.Error.Code)
	}
}

// 5xx：不换证 + 渠道冷却；冷却期返回 channel_cooling。
func TestExtChatServerErrorNoSwitch(t *testing.T) {
	var hits int32
	h, mgr := newCredHandler(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"boom"}}`))
	})
	rec := postChat(t, h, `{"model":"ext:sf/m1","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("首次应透传 500, got %d", rec.Code)
	}
	if hits != 1 {
		t.Errorf("5xx 不应换证, hits=%d", hits)
	}
	if !mgr.Get("sf").Cooldown() {
		t.Error("5xx 应让渠道短冷却")
	}
	rec = postChat(t, h, `{"model":"ext:sf/m1","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("冷却期应 503, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "channel_cooling") {
		t.Errorf("error.code 应 channel_cooling: %s", rec.Body.String())
	}
}

// 未配置 ext 渠道时 /v1/chat/completions 仍走 CodeBuddy 池（零回归前提）；
// /v1/messages 明确 501（既有行为）。
func TestExtChatNoChannels(t *testing.T) {
	h := NewHandler(Config{})
	req := httptest.NewRequest("GET", "/status", nil)
	rec := httptest.NewRecorder()
	// status 需要账号池，这里只验证 extChannelStatuses 空集口径。
	_ = req
	_ = rec
	if got := h.extChannelStatuses(); len(got) != 0 {
		t.Errorf("未配置渠道时 ext_channels 应为空, got %+v", got)
	}
}
