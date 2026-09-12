package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/tsic404/rffmpeg/pkg/protocol"
)

// TestWaitForJobWithLogs_TerminalStatusReturnsPromptly is the TSI-3081
// regression test: when the WebSocket delivers a terminal status (here a
// --timeout verdict), WaitForJobWithLogs must return immediately — fetching
// the terminal job with a single GetJob — instead of waiting out the backup
// poll's pollInterval tick. pollInterval is set far beyond the test window, so
// the only path that can return before the context deadline is the terminal
// wake-up; without the fix the wait blocks until ctx.Done and the test fails.
func TestWaitForJobWithLogs_TerminalStatusReturnsPromptly(t *testing.T) {
	oldPoll := pollInterval
	pollInterval = time.Hour
	defer func() { pollInterval = oldPoll }()

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/jobs/job-1/log", func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		msg, err := json.Marshal(protocol.NewStatusMessage("job-1", protocol.JobStatusTimeout, -1, "ffmpeg command timed out"))
		if err != nil {
			t.Errorf("marshal status message: %v", err)
			return
		}
		if err := conn.WriteMessage(websocket.TextMessage, msg); err != nil {
			return
		}
		// Hold the connection open so the return can only come from the
		// terminal status wake-up, not a graceful-close fallback. Drain reads
		// until the client closes the connection (the wait returns and calls
		// wsClient.Close, making gorilla's ReadMessage error), so the handler
		// exits and its deferred conn.Close() runs. Waiting on r.Context().Done()
		// would never fire here: net/http aborts its background read on hijack,
		// so the request context is only cancelled when the handler returns —
		// the same permanent block as select{} and the same leaked goroutine
		// and TCP connection per test run.
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	})
	mux.HandleFunc("/api/v1/jobs/job-1", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(protocol.JobStatusResponse{
			Job: protocol.JobInfo{
				ID:     "job-1",
				Status: protocol.JobStatusTimeout,
				Error:  "ffmpeg command timed out",
			},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cli := New(srv.URL, "")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	start := time.Now()
	job, err := cli.WaitForJobWithLogs(ctx, "job-1", true)
	if err != nil {
		t.Fatalf("WaitForJobWithLogs returned error after terminal WS status: %v", err)
	}
	if job == nil {
		t.Fatal("expected job info")
	}
	if job.Status != protocol.JobStatusTimeout {
		t.Errorf("expected status timeout, got %s", job.Status)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("WaitForJobWithLogs took %v after terminal WS status; want prompt return without waiting for the backup poll", elapsed)
	}
}

// TestWaitForJobWithLogs_TerminalEventGetJobMissFallsBackToPoll covers the
// terminalSeen fallback: the WebSocket delivers the terminal status, but the
// immediate GetJob in the wake-up case observes "running" (the terminal event
// raced the DB write). The wait must NOT return early or error — it keeps
// waiting and the backup poll observes the terminal status on a later tick.
// The GetJob call count is asserted to stay bounded so a regression that turns
// the fallback into a busy-loop (re-checking the closed terminalSeen channel)
// is caught: a spin would issue far more than a handful of poll-tick fetches.
func TestWaitForJobWithLogs_TerminalEventGetJobMissFallsBackToPoll(t *testing.T) {
	oldPoll := pollInterval
	pollInterval = 20 * time.Millisecond
	defer func() { pollInterval = oldPoll }()

	var getCalls atomic.Int64
	start := time.Now()

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/jobs/job-1/log", func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		msg, err := json.Marshal(protocol.NewStatusMessage("job-1", protocol.JobStatusTimeout, -1, "ffmpeg command timed out"))
		if err != nil {
			t.Errorf("marshal status message: %v", err)
			return
		}
		if err := conn.WriteMessage(websocket.TextMessage, msg); err != nil {
			return
		}
		// Drain until the client closes the connection so the handler exits and
		// its deferred conn.Close() runs — see the happy-path test above for why
		// r.Context().Done() cannot be used here (hijacked connection).
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	})
	mux.HandleFunc("/api/v1/jobs/job-1", func(w http.ResponseWriter, r *http.Request) {
		getCalls.Add(1)
		// The wake-up's GetJob lands before the DB write is visible, so it
		// reads "running"; the status flips to "timeout" shortly after and the
		// backup poll observes it.
		status := protocol.JobStatusRunning
		if time.Since(start) >= 100*time.Millisecond {
			status = protocol.JobStatusTimeout
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(protocol.JobStatusResponse{
			Job: protocol.JobInfo{
				ID:     "job-1",
				Status: status,
				Error:  "ffmpeg command timed out",
			},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cli := New(srv.URL, "")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	job, err := cli.WaitForJobWithLogs(ctx, "job-1", true)
	if err != nil {
		t.Fatalf("WaitForJobWithLogs returned error after terminal GetJob miss: %v", err)
	}
	if job == nil || job.Status != protocol.JobStatusTimeout {
		t.Fatalf("expected timeout job via backup poll, got job=%+v", job)
	}
	if calls := getCalls.Load(); calls > 50 {
		t.Errorf("GetJob called %d times; want bounded (<50) — terminalSeen fallback appears to busy-loop", calls)
	}
}
