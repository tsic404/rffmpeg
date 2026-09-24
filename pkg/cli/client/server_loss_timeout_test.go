package client

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// TestServerLossBudget pins the effective post-submit contact-loss budget: the
// tighter of the retry budget (--max-retries) and the server-loss timeout
// (--server-loss-timeout). A zero timeout disables that cap, while a zero retry
// budget ("no retries") stays the tighter bound.
func TestServerLossBudget(t *testing.T) {
	cases := []struct {
		name        string
		maxRetries  int
		lossTimeout time.Duration
		want        time.Duration
	}{
		{"loss timeout tighter than retry budget", DefaultMaxRetries, 5 * time.Second, 5 * time.Second},
		{"retry budget tighter than loss timeout", 1, time.Minute, WSReconnectDelay},
		{"zero loss timeout disables the cap", DefaultMaxRetries, 0, retryBudget(DefaultMaxRetries)},
		{"no retries stays the tighter bound", 0, time.Minute, 0},
		{"no retries and no cap", 0, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := serverLossBudget(tc.maxRetries, tc.lossTimeout); got != tc.want {
				t.Errorf("serverLossBudget(%d, %v) = %v, want %v", tc.maxRetries, tc.lossTimeout, got, tc.want)
			}
		})
	}
}

// TestConnectWithReconnect_ServerLossTimeoutFailsFast is the regression test
// for the post-submit wait: with a generous attempt budget (the default 14
// retries span ~5 minutes of backoff) a dead server must still end the wait
// once the contact-loss budget is spent, so an already-submitted job surfaces
// as a distinct error instead of leaving the CLI in backoff until an external
// timeout kills it.
func TestConnectWithReconnect_ServerLossTimeoutFailsFast(t *testing.T) {
	// Port 1 is reserved and closed; dialing it fails immediately.
	c := NewWSClient("http://127.0.0.1:1", "job-1", "",
		WithWSMaxRetries(DefaultMaxRetries), WithWSServerLossTimeout(200*time.Millisecond))
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	start := time.Now()
	err := c.ConnectWithReconnect(ctx)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("ConnectWithReconnect succeeded against a closed port, want error")
	}
	if !errors.Is(err, ErrRetriesExhausted) {
		t.Fatalf("error = %v, want it to wrap ErrRetriesExhausted", err)
	}
	if !strings.Contains(err.Error(), "200ms") {
		t.Errorf("error = %v, want it to report the spent contact-loss budget", err)
	}
	if elapsed > 3*time.Second {
		t.Errorf("ConnectWithReconnect took %v to give up, want the 200ms contact-loss budget rather than the retry budget", elapsed)
	}
}

// TestConnectWithReconnect_ReconnectWithinLossBudget pins the other side of the
// budget: a server that comes back inside it must be reconnected to, never
// abandoned after the first failed attempt.
func TestConnectWithReconnect_ReconnectWithinLossBudget(t *testing.T) {
	var isHealthy atomic.Bool
	upgrader := &websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isHealthy.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer srv.Close()

	// Down for less than one reconnect interval: the first dial fails, the
	// retry finds the server healthy again.
	go func() {
		time.Sleep(300 * time.Millisecond)
		isHealthy.Store(true)
	}()

	c := NewWSClient(srv.URL, "job-1", "", WithWSMaxRetries(DefaultMaxRetries))
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := c.ConnectWithReconnect(ctx); err != nil {
		t.Fatalf("ConnectWithReconnect = %v, want a successful reconnect within the contact-loss budget", err)
	}
}

// TestWaitForJobWithLogs_ServerLossTimeoutBoundsHTTPPoll covers the HTTP half
// of the post-submit wait: with the default attempt budget (~5 minutes) the
// backup poll must still give up once the contact-loss budget is spent, so a
// killed server cannot leave the CLI polling in silence.
func TestWaitForJobWithLogs_ServerLossTimeoutBoundsHTTPPoll(t *testing.T) {
	oldPoll := pollInterval
	pollInterval = time.Millisecond
	defer func() { pollInterval = oldPoll }()

	mux := http.NewServeMux()
	upgrader := &websocket.Upgrader{}
	mux.HandleFunc("/api/v1/jobs/job-1/log", func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	})
	mux.HandleFunc("/api/v1/jobs/job-1", func(w http.ResponseWriter, r *http.Request) {
		// A killed server answers nothing: drop the connection so the client
		// sees a transport-level failure, not an HTTP response.
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			t.Error("response writer does not support hijacking")
			return
		}
		conn, _, err := hijacker.Hijack()
		if err != nil {
			return
		}
		conn.Close()
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := New(srv.URL, "",
		WithMaxRetries(DefaultMaxRetries), WithServerLossTimeout(200*time.Millisecond))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	start := time.Now()
	_, err := c.WaitForJobWithLogs(ctx, "job-1", true)
	elapsed := time.Since(start)

	var re *RetriesExhaustedError
	if !errors.As(err, &re) {
		t.Fatalf("WaitForJobWithLogs error = %v (%T), want *RetriesExhaustedError", err, err)
	}
	if elapsed > 3*time.Second {
		t.Errorf("HTTP poll took %v to give up, want the 200ms contact-loss budget rather than the retry budget", elapsed)
	}
	if !strings.Contains(err.Error(), "lost contact") {
		t.Errorf("error = %v, want the silence verdict for a server that never answered", err)
	}
}

// TestConnectWithReconnect_HTTPResponseDoesNotBurnLossBudget is the review
// regression: a handshake the server answers — 404, the transient
// job-visibility race right after submit — proves the server is reachable, so
// it must restart the silence clock instead of burning the budget. Before the
// fix a 404 retry window longer than --server-loss-timeout ended in a bogus
// "no contact" verdict (exit code 2) instead of the bounded 404 retries that
// let the caller fall back to HTTP polling.
func TestConnectWithReconnect_HTTPResponseDoesNotBurnLossBudget(t *testing.T) {
	var handshakes atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handshakes.Add(1)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	// The loss budget is far below the 404 retry window (1s + 2s of backoff).
	c := NewWSClient(srv.URL, "job-1", "",
		WithWSMaxRetries(DefaultMaxRetries), WithWSServerLossTimeout(200*time.Millisecond))
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err := c.ConnectWithReconnect(ctx)

	var hsErr *HandshakeError
	if !errors.As(err, &hsErr) {
		t.Fatalf("error = %v (%T), want the 404 HandshakeError so the caller falls back to HTTP polling", err, err)
	}
	if errors.Is(err, ErrRetriesExhausted) {
		t.Errorf("error = %v, want no contact-loss verdict: the server answered every attempt", err)
	}
	if hsErr.Status != http.StatusNotFound {
		t.Errorf("handshake status = %d, want 404", hsErr.Status)
	}
	if got := handshakes.Load(); got != int64(wsNotFoundRetries)+1 {
		t.Errorf("handshake attempts = %d, want the bounded 404 retries (%d)", got, wsNotFoundRetries+1)
	}
}

// TestWaitForJobWithLogs_ResponsiveErrorsUseRetryBudget pins the other half of
// the silence rule: a server that answers every GetJob with an error status is
// reachable, so the poll must not report lost contact at the server-loss budget
// — it gives up at the retry budget instead of polling forever.
func TestWaitForJobWithLogs_ResponsiveErrorsUseRetryBudget(t *testing.T) {
	oldPoll := pollInterval
	pollInterval = time.Millisecond
	defer func() { pollInterval = oldPoll }()

	mux := http.NewServeMux()
	upgrader := &websocket.Upgrader{}
	mux.HandleFunc("/api/v1/jobs/job-1/log", func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	})
	mux.HandleFunc("/api/v1/jobs/job-1", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// maxRetries=1 ⇒ a 1s retry budget, well above the 200ms loss budget.
	c := New(srv.URL, "",
		WithMaxRetries(1), WithServerLossTimeout(200*time.Millisecond))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	start := time.Now()
	_, err := c.WaitForJobWithLogs(ctx, "job-1", true)
	elapsed := time.Since(start)

	var re *RetriesExhaustedError
	if !errors.As(err, &re) {
		t.Fatalf("WaitForJobWithLogs error = %v (%T), want *RetriesExhaustedError", err, err)
	}
	if elapsed < 500*time.Millisecond {
		t.Errorf("HTTP poll gave up after %v; an answered request must not burn the 200ms loss budget", elapsed)
	}
	if strings.Contains(err.Error(), "lost contact") {
		t.Errorf("error = %v, want a reachable-server verdict, not contact loss", err)
	}
}
