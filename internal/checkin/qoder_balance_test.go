package checkin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/RisingInsight/startport/internal/account"
)

func TestParseQoderBalance(t *testing.T) {
	snap := parseQoderBalance(map[string]any{
		"userQuota":          map[string]any{"total": 3000.0, "used": 100.0, "remaining": 2900.0, "unit": "credits"},
		"addOnQuota":         map[string]any{"total": 600.0, "used": 100.0, "remaining": 500.0, "unit": "credits"},
		"orgResourcePackage": map[string]any{"remaining": 460.0, "cap": 460.0, "available": true, "unit": "credits"},
	})
	if snap.Available != 3860 {
		t.Errorf("available = %v, want 3860（三桶相加）", snap.Available)
	}
	if snap.Total != 4060 {
		t.Errorf("total = %v, want 4060（user+addOn+org cap）", snap.Total)
	}
	if snap.Unit != "credits" || len(snap.Rows) != 3 {
		t.Errorf("snapshot = %+v", snap)
	}

	// 空组织包（available=false）不计入；空的 addOnQuota 不产生行。
	empty := parseQoderBalance(map[string]any{
		"userQuota":          map[string]any{"remaining": 100.0, "unit": "credits"},
		"addOnQuota":         map[string]any{"total": 0.0, "used": 0.0, "remaining": 0.0},
		"orgResourcePackage": map[string]any{"remaining": 999.0, "cap": 999.0, "available": false},
	})
	if empty.Available != 100 {
		t.Errorf("空桶不应计入: %+v", empty)
	}
}

func TestQueryQoderBalance(t *testing.T) {
	var sawAuth, sawVersion, sawClient, sawUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case qoderUsagePath:
			sawAuth = r.Header.Get("Authorization")
			sawVersion = r.Header.Get("Cosy-Version")
			sawClient = r.Header.Get("Cosy-ClientType")
			sawUA = r.Header.Get("User-Agent")
			_, _ = w.Write([]byte(`{"userQuota":{"total":300,"used":75,"remaining":225,"unit":"credits"},"addOnQuota":{"remaining":100,"total":100,"used":0},"expiresAt":1793595736281}`))
		case qoderPlanPath:
			_, _ = w.Write([]byte(`{"plan_tier_name":"Pro Trial"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	old := qoderOpenAPIBase[account.PlatformQoder]
	qoderOpenAPIBase[account.PlatformQoder] = srv.URL
	defer func() { qoderOpenAPIBase[account.PlatformQoder] = old }()

	a := &account.Account{ID: "acc-q", Platform: account.PlatformQoder,
		Cred: json.RawMessage(`{"access_token":"dt-1","user_id":"u-1"}`)}
	snap, err := QueryBalance(context.Background(), a)
	if err != nil {
		t.Fatalf("QueryBalance: %v", err)
	}
	if sawAuth != "Bearer dt-1" || sawVersion != "1.0.1" || sawClient != "5" || sawUA != "qoder-local-proxy" {
		t.Errorf("头集合异常: auth=%q version=%q client=%q ua=%q", sawAuth, sawVersion, sawClient, sawUA)
	}
	if snap.Available != 325 {
		t.Errorf("available = %v, want 325", snap.Available)
	}
	if snap.Total != 400 {
		t.Errorf("total = %v, want 400", snap.Total)
	}
	if snap.ExpiresAt.IsZero() || snap.ExpiresAt.Year() != 2026 {
		t.Errorf("expiresAt = %v, want 2026", snap.ExpiresAt)
	}
	if snap.PlanName != "Pro Trial" {
		t.Errorf("plan = %q", snap.PlanName)
	}
}

func TestQueryQoderBalanceAuthExpired(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	old := qoderOpenAPIBase[account.PlatformQoder]
	qoderOpenAPIBase[account.PlatformQoder] = srv.URL
	defer func() { qoderOpenAPIBase[account.PlatformQoder] = old }()

	a := &account.Account{ID: "acc-q", Platform: account.PlatformQoder,
		Cred: json.RawMessage(`{"access_token":"dt-1","user_id":"u-1"}`)}
	_, err := QueryBalance(context.Background(), a)
	if !errors.Is(err, ErrAuthExpired) {
		t.Fatalf("401 应返回 ErrAuthExpired: %v", err)
	}
}
