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
	"github.com/tsic404/rffmpeg/pkg/protocol"
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
// review regression test: when the WS closes gracefully (NORMAL_CLOSURE),
// WaitForJobWithStreamingOutput falls back to polling and returns a non-nil
// job — never (nil, nil), which makes main.go panic dereferencing job.Status.
// The fallback must also apply the integrity checks: the streamed output
// carried a sequenced hole, so the gap must surface as an error instead of a
// clean rc=0 (TSI-2905).
func TestWaitForJobWithStreamingOutput_GracefulCloseFallsBackToPolling(t *testing.T) {
	srv := gapLogServer(t, 400*time.Millisecond, false) // graceful close, then job completes
	defer srv.Close()

	cli := New(srv.URL, "")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	job, err := cli.WaitForJobWithStreamingOutput(ctx, "job-1", true)
	if job == nil {
		t.Fatal("graceful WS close fallback must return a non-nil job — caller dereferences job.Status")
	}
	if err == nil || !strings.Contains(err.Error(), "gap") {
		t.Fatalf("graceful WS close with a sequenced gap must surface it, got err=%v", err)
	}
}

// TestWaitForJobWithLogs_CtxDoneRaceWithPollDone is the TSI-2452 regression
// test: when the client-side timeout fires after the job has already reached
// a terminal status on the server (e.g. a cache hit completed between the
// last poll and ctx.Done()), WaitForJobWithLogs must return the completed
// job, not DeadlineExceeded.
//
// The mock server upgrades the WS (so ConnectWithReconnect succeeds and the
// select is reached) and reports "completed" for every GetJob. The ctx
// timeout is short — the poll goroutine may or may not write to pollDone
// before ctx.Done(), but the race guard's final GetJob always sees the
// terminal status and returns the job.
//
// The assertion is strict: the job must always be returned with status
// completed, never an error. Without the race guard, ctx.Done() returns
// DeadlineExceeded and the test fails.
func TestWaitForJobWithLogs_CtxDoneRaceWithPollDone(t *testing.T) {
	const jobID = "job-race"

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/jobs/"+jobID+"/log", func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		// Hold the WS open so WaitForJobWithLogs reaches the select — the
		// race guard lives in the ctx.Done() case of that select.
		<-r.Context().Done()
		conn.Close()
	})
	mux.HandleFunc("/api/v1/jobs/"+jobID, func(w http.ResponseWriter, r *http.Request) {
		// Always report completed — the race guard's final GetJob must
		// see this terminal status and return the job.
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(protocol.JobStatusResponse{
			Job: protocol.JobInfo{
				ID:     jobID,
				Status: protocol.JobStatusCompleted,
			},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cli := New(srv.URL, "")

	// Very short ctx so the deadline fires before the first poll (2s)
	// — forcing the ctx.Done() path. The race guard's final GetJob
	// returns "completed" and the job is returned, not an error.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	job, err := cli.WaitForJobWithLogs(ctx, jobID, true)

	if err != nil {
		t.Fatalf("race guard should return completed job, got error: %v", err)
	}
	if job == nil {
		t.Fatal("race guard should return completed job, got nil")
	}
	if job.Status != protocol.JobStatusCompleted {
		t.Fatalf("expected status completed, got %s", job.Status)
	}
}

// TestWaitForJobWithStreamingOutput_ZeroBytesFails is the TSI-2905 regression
// test: a completed streaming job that delivered zero stdout bytes must return
// an error (surfacing a non-zero exit), never rc=0 with an empty redirect
// target. The worker produced output that never reached the client.
func TestWaitForJobWithStreamingOutput_ZeroBytesFails(t *testing.T) {
	const jobID = "job-empty-stream"

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/jobs/"+jobID+"/log", func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		// No stdout/stderr messages are ever written: the job completes
		// without delivering a single byte to the client.
		<-r.Context().Done()
		conn.Close()
	})
	mux.HandleFunc("/api/v1/jobs/"+jobID, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(protocol.JobStatusResponse{
			Job: protocol.JobInfo{
				ID:     jobID,
				Status: protocol.JobStatusCompleted,
			},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cli := New(srv.URL, "")
	// Generous budget: the wait costs the 2s poll interval plus the 1s
	// StreamingDrainTimeout before the integrity check can run.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	job, err := cli.WaitForJobWithStreamingOutput(ctx, jobID, true)
	if err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("expected zero-byte error from streaming-output wait, got job=%v err=%v", job, err)
	}
}
