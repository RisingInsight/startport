package server

import (
	"net/http/httptest"
	"testing"

	"github.com/RisingInsight/startport/internal/auth"
	"github.com/RisingInsight/startport/internal/pool"
	"github.com/RisingInsight/startport/internal/upstream"
)

func TestMuxDebugDiagnose(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: upstream.New()})
	req := httptest.NewRequest("GET", "/startport/api/diagnose", nil)
	_, pattern := h.mux.Handler(req)
	t.Logf("mux matched pattern: %q", pattern)
}
