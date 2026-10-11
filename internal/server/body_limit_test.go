package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 配置了请求体上限时，超限请求必须 413（安全官 F-011）。
// 上限检查发生在读取阶段、早于选号/上游调用，因此本测试无需构造 Pool。
func TestChatCompletionsRejectsOversizedBody(t *testing.T) {
	h := NewHandler(Config{MaxBodyBytes: 64})
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(strings.Repeat("x", 500)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status=%d want 413; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "exceeds limit") {
		t.Errorf("错误体应说明超限, got %s", rec.Body.String())
	}
}
