package client

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/tsic404/rffmpeg/pkg/protocol"
)

// TestWaitForJobWithLogs_TransientWS404Retries is the regression test for the
// submit→wait race: under concurrent submissions the server persists the job
// before responding, but the WS handler's existence check can briefly 404 on
// the first handshake. The client must retry that 404 with backoff and connect
// on a later attempt — never immediately fall back to HTTP polling, whose first
// GetJob would observe the same transient miss and misreport "job not found".
func TestWaitForJobWithLogs_TransientWS404Retries(t *testing.T) {
	var wsAttempts atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/jobs/job-1/log", func(w http.ResponseWriter, r *http.Request) {
		if wsAttempts.Add(1) == 1 {
			// First handshake: the job is not yet visible to the WS handler,
			// exactly as the server's JobExists check would race a concurrent
			// submit commit.
			http.Error(w, "Job not found", http.StatusNotFound)
			return
		}
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		msg, err := json.Marshal(protocol.NewStatusMessage("job-1", protocol.JobStatusCompleted, 0, ""))
		if err != nil {
			t.Errorf("marshal status message: %v", err)
			return
		}
		if err := conn.WriteMessage(websocket.TextMessage, msg); err != nil {
			return
		}
		// Hold the connection open so the return comes only from the terminal
		// status wake-up, not a graceful-close fallback.
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
				Status: protocol.JobStatusCompleted,
			},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cli := New(srv.URL, "", WithMaxRetries(3))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	job, err := cli.WaitForJobWithLogs(ctx, "job-1", true)
	if err != nil {
		t.Fatalf("WaitForJobWithLogs misjudged a transient 404 as terminal: %v", err)
	}
	if job == nil || job.Status != protocol.JobStatusCompleted {
		t.Fatalf("expected completed job, got %+v", job)
	}
	if wsAttempts.Load() < 2 {
		t.Fatalf("WS endpoint attempted %d time(s); want the 404 to be retried", wsAttempts.Load())
	}
}

// TestWaitForJobWithLogs_PersistentWS404FallsBackToJobNotFound pins the
// permanent-404 semantics: when the WS endpoint 404s on every attempt AND
// GetJob 404s, the wait must fall back to HTTP polling and surface the
// "job not found" verdict — never RetriesExhaustedError ("submitted, then
// disconnected") — and the 404 retry must stay bounded (1 + wsNotFoundRetries)
// instead of exhausting the full reconnect budget.
func TestWaitForJobWithLogs_PersistentWS404FallsBackToJobNotFound(t *testing.T) {
	var wsAttempts atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/jobs/job-1/log", func(w http.ResponseWriter, r *http.Request) {
		wsAttempts.Add(1)
		http.Error(w, "Job not found", http.StatusNotFound)
	})
	mux.HandleFunc("/api/v1/jobs/job-1", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cli := New(srv.URL, "")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := cli.WaitForJobWithLogs(ctx, "job-1", true)
	if err == nil {
		t.Fatal("WaitForJobWithLogs returned nil error, want job-not-found")
	}
	if !strings.Contains(err.Error(), "job not found") {
		t.Fatalf("error = %v, want job-not-found semantics", err)
	}
	var re *RetriesExhaustedError
	if errors.As(err, &re) {
		t.Fatalf("persistent 404 must not surface as RetriesExhaustedError, got %v", err)
	}
	if got := wsAttempts.Load(); got != 1+wsNotFoundRetries {
		t.Fatalf("WS endpoint attempted %d times; want 1 + %d bounded 404 retries", got, wsNotFoundRetries)
	}
}
