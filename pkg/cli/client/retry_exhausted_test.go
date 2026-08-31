package client

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestRetryBudget(t *testing.T) {
	cases := []struct {
		maxRetries int
		want       time.Duration
	}{
		{0, 0},
		{1, 1 * time.Second},
		{2, 3 * time.Second},
		{5, 31 * time.Second},
		{14, 301 * time.Second},
	}
	for _, tc := range cases {
		if got := retryBudget(tc.maxRetries); got != tc.want {
			t.Errorf("retryBudget(%d) = %v, want %v", tc.maxRetries, got, tc.want)
		}
	}
}

// TestConnectWithReconnect_ExhaustsRetries pins the WS-side bound: a
// permanently unreachable server must fail with an error wrapping
// ErrRetriesExhausted once the retry budget is spent, not retry forever
// (TSI-2697).
func TestConnectWithReconnect_ExhaustsRetries(t *testing.T) {
	// Port 1 is reserved and closed; dialing it fails immediately.
	c := NewWSClient("http://127.0.0.1:1", "job-1", "", WithWSMaxRetries(1))
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err := c.ConnectWithReconnect(ctx)
	if err == nil {
		t.Fatal("ConnectWithReconnect succeeded against a closed port, want error")
	}
	if !errors.Is(err, ErrRetriesExhausted) {
		t.Fatalf("error = %v, want it to wrap ErrRetriesExhausted", err)
	}
}

// TestWaitForJobWithLogs_HTTPPollExhaustsRetries pins the HTTP-side bound: when
// the WS stream stays open but every GetJob fails, the backup poll must give up
// with a RetriesExhaustedError carrying the job ID instead of looping silently.
func TestWaitForJobWithLogs_HTTPPollExhaustsRetries(t *testing.T) {
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

	c := New(srv.URL, "", WithMaxRetries(1))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	start := time.Now()
	_, err := c.WaitForJobWithLogs(ctx, "job-1", true)
	if err == nil {
		t.Fatal("WaitForJobWithLogs returned nil error, want retries-exhausted")
	}
	var re *RetriesExhaustedError
	if !errors.As(err, &re) {
		t.Fatalf("error = %v (%T), want *RetriesExhaustedError", err, err)
	}
	if re.JobID != "job-1" {
		t.Errorf("RetriesExhaustedError.JobID = %q, want %q", re.JobID, "job-1")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("HTTP poll took %v to exhaust retries, want it bounded well under ctx timeout", elapsed)
	}
}
