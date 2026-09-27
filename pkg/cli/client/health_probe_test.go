package client

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// shortenHealthProbeBudget shrinks the probe's attempt timeout and backoff so
// retry-path tests run in milliseconds instead of the production ~2 minutes.
func shortenHealthProbeBudget(t *testing.T) {
	t.Helper()
	timeout, backoff := healthProbeTimeout, healthProbeBackoff
	healthProbeTimeout, healthProbeBackoff = 50*time.Millisecond, 10*time.Millisecond
	t.Cleanup(func() { healthProbeTimeout, healthProbeBackoff = timeout, backoff })
}

// TestHealthCheckRetriesStalledProbe pins the fix for the concurrent-load
// regression: a server that misses one probe because it is momentarily stalled
// must not fail the command, because a later attempt succeeds.
func TestHealthCheckRetriesStalledProbe(t *testing.T) {
	shortenHealthProbeBudget(t)

	var attempts atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) == 1 {
			// Longer than the probe's per-attempt timeout: the client must give
			// up on this attempt, not on the server.
			time.Sleep(10 * healthProbeTimeout)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := New(srv.URL, "test-token")
	if err := c.HealthCheck(); err != nil {
		t.Fatalf("HealthCheck() = %v, want nil after the stalled first attempt", err)
	}
	if got := attempts.Load(); got != 2 {
		t.Errorf("server saw %d probe(s), want 2 (stalled attempt + successful retry)", got)
	}
}

// TestHealthCheckGivesUpAfterRetryBudget pins the other side of the retry loop:
// a server that never answers ends in the probe error, after exactly the
// configured number of attempts.
func TestHealthCheckGivesUpAfterRetryBudget(t *testing.T) {
	shortenHealthProbeBudget(t)

	var attempts atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		time.Sleep(10 * healthProbeTimeout)
	}))
	defer srv.Close()

	c := New(srv.URL, "test-token")
	err := c.HealthCheck()
	if err == nil {
		t.Fatal("HealthCheck() = nil, want a timeout error for a server that never answers")
	}
	if !strings.Contains(err.Error(), "health check failed") || !strings.Contains(err.Error(), "context deadline exceeded") {
		t.Errorf("HealthCheck() = %v, want a health check timeout error", err)
	}
	if got := attempts.Load(); got != HealthProbeAttempts {
		t.Errorf("server saw %d probe(s), want %d (the full attempt budget)", got, HealthProbeAttempts)
	}
}

// TestHealthCheckDoesNotRetryRefusedConnection pins that an absent server is
// reported immediately: no retry makes a closed port answer, and the operator
// should not wait out a retry budget to learn the address is wrong. The retried
// path is recognizable by its "(after N attempts)" marker, so no wall-clock
// assertion is needed — those are jitter-prone under -race or a loaded CI host.
func TestHealthCheckDoesNotRetryRefusedConnection(t *testing.T) {
	shortenHealthProbeBudget(t)

	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	unreachable := srv.URL
	srv.Close()

	c := New(unreachable, "test-token")
	err := c.HealthCheck()
	if err == nil {
		t.Fatal("HealthCheck() = nil, want an error for a refused connection")
	}
	if !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("HealthCheck() = %v, want a connection-refused error", err)
	}
	if strings.Contains(err.Error(), "(after ") {
		t.Errorf("HealthCheck() = %v, want a refused connection reported without a retry", err)
	}
}

// TestHealthCheckDoesNotRetryUnhealthyStatus pins that a server which answers
// with a non-200 status is reported as unhealthy at once: it is a definite
// answer, not a transient transport failure.
func TestHealthCheckDoesNotRetryUnhealthyStatus(t *testing.T) {
	shortenHealthProbeBudget(t)

	var attempts atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	c := New(srv.URL, "test-token")
	err := c.HealthCheck()
	if err == nil {
		t.Fatal("HealthCheck() = nil, want an error for an unhealthy status")
	}
	if !strings.Contains(err.Error(), "status 503") {
		t.Errorf("HealthCheck() = %v, want the status reported", err)
	}
	if got := attempts.Load(); got != 1 {
		t.Errorf("server saw %d probe(s), want 1 (no retry for a definite answer)", got)
	}
}
