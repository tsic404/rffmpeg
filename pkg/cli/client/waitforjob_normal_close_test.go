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

// TestWaitForJobWithLogs_NormalCloseReconnects is the regression test for the
// server-restart path: the server's own shutdown closes the log stream with
// 1000 (C-104), while the job keeps running server-side. The wait must
// reconnect and keep streaming instead of falling back to a one-shot HTTP
// poll, whose first GetJob hits a stopped server and fails the whole wait.
func TestWaitForJobWithLogs_NormalCloseReconnects(t *testing.T) {
	var wsAttempts atomic.Int64
	var completed atomic.Bool

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/jobs/job-1/log", func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()

		if wsAttempts.Add(1) == 1 {
			// The server is shutting down: normal closure while the job is
			// still running. HTTP stays down until the "restart" completes.
			_ = conn.WriteControl(websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
				time.Now().Add(time.Second))
			return
		}

		completed.Store(true)
		msg, err := json.Marshal(protocol.NewStatusMessage("job-1", protocol.JobStatusCompleted, 0, ""))
		if err != nil {
			t.Errorf("marshal status message: %v", err)
			return
		}
		if err := conn.WriteMessage(websocket.TextMessage, msg); err != nil {
			return
		}
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	})
	mux.HandleFunc("/api/v1/jobs/job-1", func(w http.ResponseWriter, r *http.Request) {
		status := protocol.JobStatusRunning
		if completed.Load() {
			status = protocol.JobStatusCompleted
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(protocol.JobStatusResponse{
			Job: protocol.JobInfo{ID: "job-1", Status: status},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cli := New(srv.URL, "", WithMaxRetries(3))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	job, err := cli.WaitForJobWithLogs(ctx, "job-1", true)
	if err != nil {
		t.Fatalf("WaitForJobWithLogs failed after a normal server close: %v", err)
	}
	if job == nil || job.Status != protocol.JobStatusCompleted {
		t.Fatalf("expected completed job, got %+v", job)
	}
	if got := wsAttempts.Load(); got < 2 {
		t.Fatalf("WS endpoints attempted %d time(s); a normal close must reconnect", got)
	}
}
