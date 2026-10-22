package worker

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tsic404/rffmpeg/pkg/protocol"
)

const testStreamJobID = "stream-1"

// jobStatusServer serves GET /api/v1/jobs/<testStreamJobID> with a switchable
// status and counts polls.
type jobStatusServer struct {
	server *httptest.Server
	polls  int32
	status atomic.Value // protocol.JobStatus
}

func newJobStatusServer(t *testing.T, initial protocol.JobStatus) *jobStatusServer {
	t.Helper()
	s := &jobStatusServer{}
	s.status.Store(initial)
	s.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/jobs/"+testStreamJobID {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		atomic.AddInt32(&s.polls, 1)
		_ = json.NewEncoder(w).Encode(protocol.JobStatusResponse{Job: protocol.JobInfo{
			ID:       testStreamJobID,
			Status:   s.status.Load().(protocol.JobStatus),
			WorkerID: "test-worker-1",
		}})
	}))
	t.Cleanup(s.server.Close)
	return s
}

func (s *jobStatusServer) url() string                     { return s.server.URL }
func (s *jobStatusServer) pollCount() int32                { return atomic.LoadInt32(&s.polls) }
func (s *jobStatusServer) setStatus(st protocol.JobStatus) { s.status.Store(st) }

func newWatcherWorker(url string, pollInterval time.Duration) *Worker {
	return &Worker{
		id:                          "test-worker-1",
		client:                      NewClient(url, "test-worker-1", ""),
		streamingCancelPollInterval: pollInterval,
	}
}

// TestStreamingCancelWatch_CancelsOnServerCancel is the regression test for a
// ctrl+C on a streaming job: the worker must tear down the job's context on its
// own, not wait for the next 30s heartbeat response.
func TestStreamingCancelWatch_CancelsOnServerCancel(t *testing.T) {
	srv := newJobStatusServer(t, protocol.JobStatusCancelled)
	w := newWatcherWorker(srv.url(), 100*time.Millisecond)

	ctx, cancelCtx := context.WithCancel(context.Background())
	defer cancelCtx()

	cancelled := make(chan struct{})
	stop := w.watchStreamingJobCancellation(ctx, testStreamJobID, func() { close(cancelled) })
	defer stop()

	select {
	case <-cancelled:
	case <-time.After(3 * time.Second):
		t.Fatalf("job context not cancelled after %d status polls", srv.pollCount())
	}
}

// TestStreamingCancelWatch_PollsUntilStatusChanges pins the watcher to the
// steady state, not a single sample: it must poll a running job repeatedly and
// cancel as soon as the server flips it to cancelled. A watcher that reads
// "running" once and exits would either fail the poll-count assertion or never
// observe the flip.
func TestStreamingCancelWatch_PollsUntilStatusChanges(t *testing.T) {
	srv := newJobStatusServer(t, protocol.JobStatusRunning)
	w := newWatcherWorker(srv.url(), 100*time.Millisecond)

	ctx, cancelCtx := context.WithCancel(context.Background())
	defer cancelCtx()

	cancelled := make(chan struct{}, 1)
	stop := w.watchStreamingJobCancellation(ctx, testStreamJobID, func() { cancelled <- struct{}{} })
	defer stop()

	waitFor(t, 2*time.Second, func() bool { return srv.pollCount() >= 2 },
		"watcher must keep polling a running job")
	select {
	case <-cancelled:
		t.Fatal("watcher cancelled a job the server reports as running")
	default:
	}

	srv.setStatus(protocol.JobStatusCancelled)
	select {
	case <-cancelled:
	case <-time.After(3 * time.Second):
		t.Fatalf("watcher did not cancel after the server reported cancelled (%d polls)", srv.pollCount())
	}
}

// TestStreamingCancelWatch_StopEndsPolling proves the stop function ends the
// watcher instead of merely muting it.
func TestStreamingCancelWatch_StopEndsPolling(t *testing.T) {
	srv := newJobStatusServer(t, protocol.JobStatusRunning)
	w := newWatcherWorker(srv.url(), 100*time.Millisecond)

	ctx, cancelCtx := context.WithCancel(context.Background())
	defer cancelCtx()

	stop := w.watchStreamingJobCancellation(ctx, testStreamJobID, func() { t.Error("cancel called for a running job") })
	waitFor(t, 2*time.Second, func() bool { return srv.pollCount() >= 2 }, "watcher never polled")
	stop()

	before := srv.pollCount()
	time.Sleep(3 * 100 * time.Millisecond)
	if after := srv.pollCount(); after != before {
		t.Fatalf("watcher kept polling after stop: %d -> %d", before, after)
	}
}

// TestStreamingCancelWatch_StopAbortsInFlightPoll pins the robustness fix: a
// status endpoint that hangs must not hold the watcher (or its stop function)
// for the control-plane client timeout. The handler blocks until the client
// abandons the request, which only happens when stop cancels the in-flight
// poll's context. The abort is normal shutdown: it must not be counted as a
// poll failure or logged as a fallback warning.
func TestStreamingCancelWatch_StopAbortsInFlightPoll(t *testing.T) {
	var logs syncLogBuffer
	log.SetOutput(&logs)
	defer log.SetOutput(os.Stderr)

	requestAborted := make(chan struct{}, 1)
	var polls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&polls, 1)
		<-r.Context().Done()
		requestAborted <- struct{}{}
	}))
	defer srv.Close()

	w := newWatcherWorker(srv.URL, time.Second)
	ctx, cancelCtx := context.WithCancel(context.Background())
	defer cancelCtx()

	stop := w.watchStreamingJobCancellation(ctx, testStreamJobID, func() {})
	waitFor(t, 3*time.Second, func() bool { return atomic.LoadInt32(&polls) >= 1 }, "watcher never polled")

	start := time.Now()
	stop() // waits for the watcher goroutine, so the log is settled
	select {
	case <-requestAborted:
		// The per-request budget is 2 intervals (2s); stop must be far quicker.
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Fatalf("stop took %v to abort the in-flight status poll", elapsed)
		}
	case <-time.After(1500 * time.Millisecond):
		t.Fatal("stop did not abort the in-flight status poll")
	}
	if got := logs.String(); strings.Contains(got, "heartbeat remains the backstop") {
		t.Fatalf("stop() emitted a degraded-poll warning: %s", got)
	}
}

// TestJobCancelledElsewhere pins the worker-side predicate to the Server's
// heartbeat cancel rule: cancelled, re-queued, or reassigned jobs are no
// longer this worker's to run.
func TestJobCancelledElsewhere(t *testing.T) {
	cases := []struct {
		name string
		job  protocol.JobInfo
		want bool
	}{
		{"cancelled", protocol.JobInfo{Status: protocol.JobStatusCancelled}, true},
		{"requeued pending", protocol.JobInfo{Status: protocol.JobStatusPending}, true},
		{"reassigned", protocol.JobInfo{Status: protocol.JobStatusRunning, WorkerID: "other-worker"}, true},
		{"running here", protocol.JobInfo{Status: protocol.JobStatusRunning, WorkerID: "test-worker-1"}, false},
		{"completed here", protocol.JobInfo{Status: protocol.JobStatusCompleted, WorkerID: "test-worker-1"}, false},
		{"unknown owner", protocol.JobInfo{Status: protocol.JobStatusRunning}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := jobCancelledElsewhere(&tc.job, "test-worker-1"); got != tc.want {
				t.Fatalf("jobCancelledElsewhere(%+v) = %v, want %v", tc.job, got, tc.want)
			}
		})
	}
}

// syncLogBuffer is a goroutine-safe io.Writer for capturing log output.
type syncLogBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncLogBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncLogBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// waitFor polls cond until it holds or the timeout expires.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal(msg)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
