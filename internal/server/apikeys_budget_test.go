package server

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/RisingInsight/startport/internal/apikeys"
	"github.com/RisingInsight/startport/internal/pool"
	"github.com/RisingInsight/startport/internal/usage"
)

// newBudgetTestHandler 带用量记录器的 handler（密钥限额测试用）。
func newBudgetTestHandler(t *testing.T) (*Handler, *apikeys.APIKeys, *usage.Recorder, string) {
	t.Helper()
	st := apikeys.New("")
	ur := usage.New("")
	h := NewHandler(Config{
		Pool:    pool.New(""),
		APIKey:  "panel-key",
		APIKeys: st,
		Usage:   ur,
	})
	full, _, err := st.Create("受限 agent")
	if err != nil {
		t.Fatalf("create agent key: %v", err)
	}
	return h, st, ur, full
}

func burnTokens(ur *usage.Recorder, name string, n int) {
	for i := 0; i < n; i++ {
		ur.Add(time.Now(), "cn", "u1", "m", usage.Delta{
			KeyName: name, TotalTokens: 100, HasTotal: true,
		}, true)
	}
}

// 密钥限额全链路：日限额 1000 tokens，用满后 /v1 请求 429 insufficient_quota；
// 未超限与未设限的密钥照常通过。
func TestAgentKeyTokenBudget429(t *testing.T) {
	h, st, ur, full := newBudgetTestHandler(t)
	rows := st.List()
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if _, err := st.SetLimit(rows[0].ID, 1000, "day"); err != nil {
		t.Fatalf("set limit: %v", err)
	}

	// 未用：通过。
	if code, _ := getJSON(t, h, "/v1/models", full); code != http.StatusOK {
		t.Fatalf("under-budget /v1/models = %d, want 200", code)
	}

	// 用满 1000：429 + insufficient_quota。
	burnTokens(ur, "受限 agent", 10)
	code, body := getJSON(t, h, "/v1/models", full)
	if code != http.StatusTooManyRequests {
		t.Fatalf("over-budget /v1/models = %d, want 429", code)
	}
	if raw, _ := body["error"].(map[string]any); raw == nil {
		t.Fatalf("429 body missing error envelope: %v", body)
	} else if raw["code"] != "insufficient_quota" {
		t.Fatalf("429 error.code = %v, want insufficient_quota", raw["code"])
	}

	// 管理密钥不受限额约束。
	if code, _ := getJSON(t, h, "/v1/models", "panel-key"); code != http.StatusOK {
		t.Fatalf("admin key over budget = %d, want 200", code)
	}

	// 清除限额后恢复通过。
	if _, err := st.SetLimit(rows[0].ID, 0, ""); err != nil {
		t.Fatalf("clear limit: %v", err)
	}
	if code, _ := getJSON(t, h, "/v1/models", full); code != http.StatusOK {
		t.Fatalf("cleared-limit /v1/models = %d, want 200", code)
	}
}

// 限额错误文案点名周期与 used/limit，供用户知道等多久/调哪里。
func TestAgentKeyBudgetMessageNamesPeriod(t *testing.T) {
	h, st, ur, full := newBudgetTestHandler(t)
	rows := st.List()
	if _, err := st.SetLimit(rows[0].ID, 500, "week"); err != nil {
		t.Fatalf("set limit: %v", err)
	}
	burnTokens(ur, "受限 agent", 5)
	_, body := getJSON(t, h, "/v1/models", full)
	msg, _ := body["error"].(map[string]any)["message"].(string)
	for _, want := range []string{"周", "500/500"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q missing %q", msg, want)
		}
	}
}
