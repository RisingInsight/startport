// qa_v120_test.go QA 独立验收（v1.2.0）：Loomy 出站在**客户端可见面**的语义。
//
// 关注两件事（PRD/验收要求）：session 过期必须让客户端拿到
// `error.type=authentication_error` + `error.code=session_expired`；
// 业务错误不能被当成 HTTP 200 的正常响应下发。
package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/RisingInsight/startport/internal/extchan"
)

const qaLoomySession = "qa-loomy-session-abcdef-123456"

// qaLoomyHandler 构造 Loomy 渠道（adapter=loomy）的 handler。
func qaLoomyHandler(t *testing.T, respond http.HandlerFunc) *Handler {
	t.Helper()
	srv := httptest.NewServer(respond)
	t.Cleanup(srv.Close)
	mgr := extchan.New(extchan.Config{TimeoutSeconds: 5})
	mgr.ReplaceAll([]extchan.ChannelDef{{
		Name: "loomy", BaseURL: srv.URL + "/api/v1", Protocol: "openai", Adapter: "loomy",
		Models:      []string{"spark-x"},
		Credentials: []extchan.CredentialDef{{Label: "primary", Secret: qaLoomySession}},
	}})
	return NewHandler(Config{Ext: mgr})
}

// qaChat 发一次 /v1/chat/completions。
func qaChat(t *testing.T, h *Handler, model string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"`+model+`","messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// 必测 4（客户端可见面）：HTTP 401（无业务码）应表达为 session 过期。
func TestQALoomyClientSees401AsSessionExpired(t *testing.T) {
	h := qaLoomyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"unauthorized"}}`))
	})
	rec := qaChat(t, h, "ext:loomy/spark-x")
	var env struct {
		Error struct {
			Type    string `json:"type"`
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("响应不是 JSON(%d): %s", rec.Code, rec.Body.String())
	}
	if env.Error.Type != "authentication_error" || env.Error.Code != "session_expired" {
		t.Errorf("HTTP 401 应表达 session 过期，实际 status=%d type=%q code=%q message=%q",
			rec.Code, env.Error.Type, env.Error.Code, env.Error.Message)
	}
	if strings.Contains(rec.Body.String(), qaLoomySession) {
		t.Error("响应体泄露 session")
	}
}

// 必测 4（客户端可见面）：200 + 业务码 → 必须是错误语义，不能当 200 成功下发。
func TestQALoomyClientSees200BizError(t *testing.T) {
	h := qaLoomyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":"020002","desc":"登录已失效"}`))
	})
	rec := qaChat(t, h, "ext:loomy/spark-x")
	var env struct {
		Error struct {
			Type string `json:"type"`
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	if env.Error.Code != "session_expired" {
		t.Errorf("错误码应透出 session_expired, 实际 status=%d body=%s", rec.Code, rec.Body.String())
	}
	// 关键：错误必须带非 2xx 状态，否则按状态码判定的 SDK 会把它当成功。
	if rec.Code >= 200 && rec.Code < 300 {
		t.Errorf("业务错误被当成 HTTP %d 成功响应下发（客户端无法按状态码识别）: %s",
			rec.Code, rec.Body.String())
	}
}

// 必测 6（零回归）：普通 openai 渠道的 429 错误体与状态逐字节透传。
func TestQAPlainOpenAIClientErrorUnchanged(t *testing.T) {
	raw := `{"error":{"message":"rate limited","type":"rate_limit","code":"upstream_rate"}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(raw))
	}))
	defer srv.Close()
	mgr := extchan.New(extchan.Config{TimeoutSeconds: 5})
	mgr.ReplaceAll([]extchan.ChannelDef{{
		Name: "plain", BaseURL: srv.URL, Protocol: "openai",
		Models:      []string{"m1"},
		Credentials: []extchan.CredentialDef{{Label: "p", Secret: "sk-abcdef"}},
	}})
	h := NewHandler(Config{Ext: mgr})
	rec := qaChat(t, h, "ext:plain/m1")
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429", rec.Code)
	}
	if rec.Body.String() != raw {
		t.Errorf("错误体应逐字节透传:\n got %s\nwant %s", rec.Body.String(), raw)
	}
}

// 必测 4（ANT 入站）：Loomy session 过期在 /v1/messages 上应是 authentication_error。
func TestQALoomyAnthropicInbound401(t *testing.T) {
	h := qaLoomyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"unauthorized"}}`))
	})
	req := httptest.NewRequest("POST", "/v1/messages",
		strings.NewReader(`{"model":"ext:loomy/spark-x","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var env struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("响应不是 JSON(%d): %s", rec.Code, rec.Body.String())
	}
	if env.Type != "error" || env.Error.Type != "authentication_error" {
		t.Errorf("ANT 入站 401 语义异常: %+v", env)
	}
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
	if strings.Contains(rec.Body.String(), qaLoomySession) {
		t.Error("响应体泄露 session")
	}
}

// Loomy 正常响应（含 choices）在客户端侧形态不变，model 回写 ext:<ch>/<model>。
func TestQALoomyNormalResponseShape(t *testing.T) {
	h := qaLoomyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","choices":[{"message":{"role":"assistant","content":"pong"},"finish_reason":"stop"}],` +
			`"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`))
	})
	rec := qaChat(t, h, "ext:loomy/spark-x")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Model != "ext:loomy/spark-x" || len(resp.Choices) != 1 || resp.Choices[0].Message.Content != "pong" {
		t.Errorf("响应形态异常: %+v", resp)
	}
}
