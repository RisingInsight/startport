package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/RisingInsight/startport/internal/apikeys"
	"github.com/RisingInsight/startport/internal/pool"
)

// newKeysTestHandler 构建带 agent 密钥存储的 handler:空池(0 账号,
// /v1/models 返回空列表 200,不触上游);管理密钥 pk。
func newKeysTestHandler(t *testing.T) (*Handler, *apikeys.APIKeys, string) {
	t.Helper()
	st := apikeys.New("")
	p := pool.New("")
	h := NewHandler(Config{
		Pool:    p,
		APIKey:  "panel-key",
		APIKeys: st,
	})
	full, _, err := st.Create("测试 agent")
	if err != nil {
		t.Fatalf("create agent key: %v", err)
	}
	return h, st, full
}

func getJSON(t *testing.T, h *Handler, path, bearer string) (int, map[string]any) {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, path, nil)
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	var m map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &m)
	return rec.Code, m
}

// 鉴权矩阵:agent 密钥只开 /v1/*;面板与 /status 只认管理密钥。
func TestAgentKeyAuthMatrix(t *testing.T) {
	h, _, agentKey := newKeysTestHandler(t)

	cases := []struct {
		path string
		auth string
		want int
	}{
		{"/v1/models", agentKey, http.StatusOK},          // agent 密钥 → /v1 通过
		{"/v1/models", "panel-key", http.StatusOK},       // 管理密钥 → /v1 仍通过(向后兼容)
		{"/v1/models", "", http.StatusUnauthorized},      // 未鉴权拒绝(api_key 非空)
		{"/v1/models", "wrong", http.StatusUnauthorized}, // 错误密钥拒绝
		{"/status", agentKey, http.StatusUnauthorized},   // agent 密钥打不进 /status
		{"/status", "panel-key", http.StatusOK},          // 管理密钥 → /status
	}
	for _, c := range cases {
		code, _ := getJSON(t, h, c.path, c.auth)
		if code != c.want {
			t.Errorf("%s auth=%q → %d, want %d", c.path, c.auth, code, c.want)
		}
	}
}

// 禁用立即失效;删除同理(经存储侧验证,这里走 HTTP 全链路再确认一次)。
func TestAgentKeyDisabledRejected(t *testing.T) {
	h, st, agentKey := newKeysTestHandler(t)
	rows := st.List()
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if _, err := st.SetDisabled(rows[0].ID, true); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if code, _ := getJSON(t, h, "/v1/models", agentKey); code != http.StatusUnauthorized {
		t.Fatalf("禁用后 /v1/models = %d, want 401", code)
	}
}
