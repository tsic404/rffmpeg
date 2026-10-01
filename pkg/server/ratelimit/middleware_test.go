package ratelimit

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tsic404/rffmpeg/pkg/server/auth"
)

const probeTestToken = "test-token"

// serveProbe runs one probe through the production chain — auth middleware,
// ProbeMiddleware, handler — and returns the recorded response.
func serveProbe(counter ClientJobCounter, cfg *RuntimeConfig, handler http.Handler) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/probe", nil)
	req.Header.Set("Authorization", "Bearer "+probeTestToken)

	rec := httptest.NewRecorder()
	auth.Middleware(probeTestToken)(ProbeMiddleware(counter, cfg)(handler)).ServeHTTP(rec, req)
	return rec
}

// TestProbeMiddleware_ReleasesExactlyOnceOnFailure covers the probe path that
// leaked another job's slot: with a job already active, a rejected probe must
// return only the slot it reserved.
func TestProbeMiddleware_ReleasesExactlyOnceOnFailure(t *testing.T) {
	counter := NewInMemoryCounter()
	cfg := &RuntimeConfig{Enabled: true, Limit: 2}
	clientID := auth.GenerateClientID(probeTestToken)
	counter.TryIncrement(clientID, cfg.Limit) // slot held by an active job

	rec := serveProbe(counter, cfg, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("probe status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	if got := counter.Count(clientID); got != 1 {
		t.Errorf("count after failed probe = %d, want 1 (the active job's slot)", got)
	}
}

// TestProbeMiddleware_DisabledConfigLeavesReservationsAlone covers a runtime
// toggle: counts booked while rate limiting was enabled must survive a probe
// that made no reservation of its own.
func TestProbeMiddleware_DisabledConfigLeavesReservationsAlone(t *testing.T) {
	counter := NewInMemoryCounter()
	clientID := auth.GenerateClientID(probeTestToken)
	counter.TryIncrement(clientID, 2) // booked while rate limiting was enabled

	handlerRan := false
	rec := serveProbe(counter, &RuntimeConfig{Enabled: false, Limit: 2}, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		handlerRan = true
		w.WriteHeader(http.StatusBadRequest)
	}))

	if !handlerRan {
		t.Fatal("probe handler must run when rate limiting is disabled")
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("probe status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	if got := counter.Count(clientID); got != 1 {
		t.Errorf("count = %d, want 1: probe released a reservation it never made", got)
	}
}

// TestProbeMiddleware_RejectsAtLimit verifies an over-limit probe is refused
// before the handler runs and consumes no slot of its own.
func TestProbeMiddleware_RejectsAtLimit(t *testing.T) {
	counter := NewInMemoryCounter()
	cfg := &RuntimeConfig{Enabled: true, Limit: 1}
	clientID := auth.GenerateClientID(probeTestToken)
	counter.TryIncrement(clientID, cfg.Limit)

	rec := serveProbe(counter, cfg, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("probe handler must not run when the limit is reached")
	}))

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("probe status = %d, want %d", rec.Code, http.StatusTooManyRequests)
	}
	if got := counter.Count(clientID); got != 1 {
		t.Errorf("count = %d, want 1: a rejected probe must not change the count", got)
	}
}
