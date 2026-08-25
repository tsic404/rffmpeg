package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/tsix404/rffmpeg/pkg/protocol"
)

// gapLogServer serves the WS log endpoint once with a sequenced hole (seq 1
// then 3) and a GET /api/v1/jobs/{id} endpoint whose first response reports
// "running" and later responses report "completed" with an output file.
// holdWS keeps the WebSocket open (no graceful close), so callers that need
// the pollDone completion path get it deterministically.
func gapLogServer(t *testing.T, completedAfter time.Duration, holdWS bool) *httptest.Server {
	t.Helper()

	var mu sync.Mutex
	start := time.Now()
	completed := false

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/jobs/job-1/log", func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		msgs := []string{
			`{"type":"stderr","job_id":"job-1","payload":"one","seq":1}`,
			`{"type":"stderr","job_id":"job-1","payload":"three","seq":3}`,
			`{"type":"complete","job_id":"job-1","data":{"exit_code":0},"seq":4}`,
		}
		for _, m := range msgs {
			if err := conn.WriteMessage(websocket.TextMessage, []byte(m)); err != nil {
				return
			}
		}
		if holdWS {
			// Keep the connection open so the caller's select resolves via
			// pollDone (or ctx cancellation) rather than the graceful-close
			// fallback path.
			select {}
		}
		time.Sleep(300 * time.Millisecond)
		conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		_ = conn.WriteMessage(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
		time.Sleep(50 * time.Millisecond)
	})
	mux.HandleFunc("/api/v1/jobs/job-1", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		if !completed && time.Since(start) >= completedAfter {
			completed = true
		}
		status := protocol.JobStatusRunning
		if completed {
			status = protocol.JobStatusCompleted
		}
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(protocol.JobStatusResponse{
			Job: protocol.JobInfo{
				ID:          "job-1",
				Status:      status,
				OutputFiles: []string{"file-1"},
			},
		})
	})
	return httptest.NewServer(mux)
}

// TestWaitForJobWithLogs_GapDoesNotBlockDownload is the TSI-2404 regression
// test: in non-streaming (file output) mode a detected sequence gap on the
// stderr log stream must NOT fail the wait — the transcoded file is intact,
// so WaitForJobWithLogs returns the job and main.go proceeds to the output
// file download. Only streaming-output mode may fail on a gap.
func TestWaitForJobWithLogs_GapDoesNotBlockDownload(t *testing.T) {
	srv := gapLogServer(t, 400*time.Millisecond, false)
	defer srv.Close()

	cli := New(srv.URL, "")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	job, err := cli.WaitForJobWithLogs(ctx, "job-1", true)
	if err != nil {
		t.Fatalf("gap in non-streaming log stream must not block download, got error: %v", err)
	}
	if job == nil {
		t.Fatal("expected job info")
	}
	if job.Status != protocol.JobStatusCompleted {
		t.Errorf("expected status completed, got %s", job.Status)
	}
	if len(job.OutputFiles) == 0 {
		t.Error("expected output files to be returned for download")
	}
}

// TestWaitForJobWithStreamingOutput_GapStillFails pins the other half of the
// contract: in streaming-output mode a detected gap MUST still surface as an
// error — stdout bytes were already written to the consumer with holes.
func TestWaitForJobWithStreamingOutput_GapStillFails(t *testing.T) {
	srv := gapLogServer(t, 10*time.Minute, true) // job never completes via polling
	defer srv.Close()

	cli := New(srv.URL, "")
	// The server holds the WS open and reports "running" forever, so the
	// wait resolves via pollDone only after ctx expires — but the gap check
	// runs before returning. A 3s window keeps this fast; the gap is
	// detected as soon as the sequenced messages arrive.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	job, err := cli.WaitForJobWithStreamingOutput(ctx, "job-1", true)
	if err == nil || !strings.Contains(err.Error(), "gap") {
		t.Fatalf("expected gap error from streaming-output wait, got job=%v err=%v", job, err)
	}
}

// TestWaitForJobWithStreamingOutput_GracefulCloseFallsBackToPolling is the
// review regression test: when the WS closes gracefully (NORMAL_CLOSURE)
// with no gap and no ctx deadline, WaitForJobWithStreamingOutput must fall
// back to polling and return the completed job — never (nil, nil), which
// makes main.go panic dereferencing job.Status.
func TestWaitForJobWithStreamingOutput_GracefulCloseFallsBackToPolling(t *testing.T) {
	srv := gapLogServer(t, 400*time.Millisecond, false) // graceful close, then job completes
	defer srv.Close()

	cli := New(srv.URL, "")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	job, err := cli.WaitForJobWithStreamingOutput(ctx, "job-1", true)
	if err != nil {
		t.Fatalf("graceful WS close must fall back to polling, got error: %v", err)
	}
	if job == nil {
		t.Fatal("job must not be nil — caller dereferences job.Status")
	}
	if job.Status != protocol.JobStatusCompleted {
		t.Errorf("expected status completed, got %s", job.Status)
	}
}
