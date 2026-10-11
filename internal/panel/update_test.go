package panel

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/RisingInsight/startport/internal/update"
)

// 未注入 Checker 时，更新端点返回 enabled:false（不报错、不联网）。
func TestUpdateEndpointWithoutChecker(t *testing.T) {
	p := New(Config{Version: "test", APIKey: "k"})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/startport/api/update", nil)
	req.Header.Set("Authorization", "Bearer k")
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"enabled":false`) {
		t.Errorf("body=%s, want enabled:false", rec.Body.String())
	}
}

// 禁用态 Checker：默认（无 refresh）走缓存，零网络 I/O，返回 enabled:false。
func TestUpdateEndpointDisabledChecker(t *testing.T) {
	p := New(Config{
		Version: "test", APIKey: "k",
		Update: update.New(update.Config{Current: "1.0.0", Enabled: false}),
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/startport/api/update", nil)
	req.Header.Set("Authorization", "Bearer k")
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"enabled":false`) {
		t.Errorf("body=%s, want enabled:false", body)
	}
	if !strings.Contains(body, `"current":"1.0.0"`) {
		t.Errorf("body=%s, want current 1.0.0", body)
	}
}

// updateSummary 形状：nil Checker → 未启用；禁用 Checker → 未启用且带 current。
func TestUpdateSummaryShape(t *testing.T) {
	if got := updateSummary(nil); got["enabled"] != false || got["available"] != false {
		t.Errorf("updateSummary(nil)=%v, want disabled", got)
	}
	c := update.New(update.Config{Current: "2.3.4", Enabled: false})
	got := updateSummary(c)
	if got["enabled"] != false || got["available"] != false || got["current"] != "2.3.4" {
		t.Errorf("updateSummary(disabled)=%v", got)
	}
}
