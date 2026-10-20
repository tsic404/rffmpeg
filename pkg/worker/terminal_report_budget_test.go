package worker

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/tsic404/rffmpeg/pkg/protocol"
	"github.com/tsic404/rffmpeg/pkg/worker/gpu"
)

// TestStderrBatcherFlushAndWaitIsBounded pins the convergence bound: a
// terminal-status flush must not wait out a stalled stderr send. Without the
// bound, FlushAndWait blocks until the slow PATCH returns, delaying the
// TIMEOUT verdict and blowing AC-31's budget-plus-convergence wall clock.
func TestStderrBatcherFlushAndWaitIsBounded(t *testing.T) {
	prev := terminalFlushBudget
	terminalFlushBudget = 50 * time.Millisecond
	t.Cleanup(func() { terminalFlushBudget = prev })

	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release // hold the stderr send until the test releases it
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	batcher := NewStderrBatcher("job-bounded", NewClient(srv.URL, "worker-1", ""),
		DefaultStderrBatcherConfig())
	defer func() {
		close(release) // unblock the in-flight send before draining it
		batcher.Close()
	}()

	batcher.Add("tail line\n")

	start := time.Now()
	batcher.FlushAndWait()
	elapsed := time.Since(start)

	if elapsed > 500*time.Millisecond {
		t.Fatalf("FlushAndWait blocked %v on a stalled stderr send; want it bounded by %v", elapsed, terminalFlushBudget)
	}
}

// TestStderrBatcherCloseIsBounded pins the same convergence bound on the
// defer path: processJob's deferred Close must not wait out a stalled stderr
// send either. Without the bound, Close blocks until the slow PATCH returns
// and keeps the job goroutine (activeJobs entry, idle heartbeat) alive long
// after the terminal verdict was reported.
func TestStderrBatcherCloseIsBounded(t *testing.T) {
	prev := terminalFlushBudget
	terminalFlushBudget = 50 * time.Millisecond
	t.Cleanup(func() { terminalFlushBudget = prev })

	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release // hold the stderr send until the test releases it
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	defer close(release) // runs before srv.Close: unblocks the held handler

	batcher := NewStderrBatcher("job-close-bounded", NewClient(srv.URL, "worker-1", ""),
		DefaultStderrBatcherConfig())

	batcher.Add("tail line\n")

	start := time.Now()
	batcher.Close() // the stalled send stays in flight; Close must not block on it
	elapsed := time.Since(start)

	if elapsed > 500*time.Millisecond {
		t.Fatalf("Close blocked %v on a stalled stderr send; want it bounded by %v", elapsed, terminalFlushBudget)
	}
}

// TestProcessJobTimeoutReportNotGatedByStderrDrain is the worker-level
// regression test for the TIMEOUT convergence path: the server's stderr PATCH
// endpoint is slow (simulating a congested control plane), and the job's ffmpeg
// budget expires while a stderr batch is still in flight. The terminal timeout
// PATCH must still reach the server within the flush budget — not wait for the
// stalled stderr send to drain — so a TIMEOUT verdict converges inside AC-31's
// wall-clock margin.
func TestProcessJobTimeoutReportNotGatedByStderrDrain(t *testing.T) {
	tmpDir := t.TempDir()

	ffmpegPath := filepath.Join(tmpDir, "noisy-sleep-ffmpeg")
	script := "#!/bin/sh\nprintf 'encoder noise line\\n' >&2\nsleep 5\n"
	if err := os.WriteFile(ffmpegPath, []byte(script), 0755); err != nil {
		t.Fatalf("write mock ffmpeg: %v", err)
	}

	var mu sync.Mutex
	var terminalBody []byte
	terminalAt := make(chan time.Time, 1)

	const stderrDelay = 4 * time.Second
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			w.WriteHeader(http.StatusOK)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusOK)
			return
		}
		var req protocol.JobUpdateRequest
		if err := json.Unmarshal(body, &req); err != nil {
			w.WriteHeader(http.StatusOK)
			return
		}
		if req.StderrChunk != "" {
			time.Sleep(stderrDelay)
			w.WriteHeader(http.StatusOK)
			return
		}
		if protocol.IsTerminalStatus(req.Status) {
			mu.Lock()
			terminalBody = body
			mu.Unlock()
			select {
			case terminalAt <- time.Now():
			default:
			}
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cacheDir := filepath.Join(tmpDir, "cache")
	cache, err := NewCache(CacheConfig{Enabled: true, Dir: cacheDir, TTL: 24 * time.Hour})
	if err != nil {
		t.Fatalf("NewCache failed: %v", err)
	}
	t.Cleanup(func() { cache.Stop() })

	workerTempDir := filepath.Join(tmpDir, "worker-temp")
	if err := os.MkdirAll(workerTempDir, 0755); err != nil {
		t.Fatalf("mkdir worker-temp: %v", err)
	}

	executor := NewExecutor(ffmpegPath, 30*time.Second)
	rewriteAdapter := NewRewriteAdapter()
	rewriteAdapter.config.Enabled = false
	w := &Worker{
		id:                 "test-worker-001",
		name:               "test-worker",
		client:             NewClient(srv.URL, "test-worker-001", ""),
		executor:           executor,
		retryExecutor:      NewRetryExecutor(executor, DefaultRetryConfig()),
		rewriteAdapter:     rewriteAdapter,
		cache:              cache,
		tempDir:            workerTempDir,
		activeJobs:         make(map[string]context.CancelFunc),
		heartbeatInterval:  30 * time.Second,
		pollInterval:       5 * time.Second,
		lastHeartbeatTime:  time.Now(),
		ffprobeExecutor:    NewFFprobeExecutor(""),
		pixelFormatChecker: nil,
		gpuDetector:        gpu.NewDetector(),
	}

	src := filepath.Join(tmpDir, "input.mkv")
	if err := os.WriteFile(src, []byte("dummy"), 0644); err != nil {
		t.Fatal(err)
	}

	timeout := 400 * time.Millisecond
	job := protocol.JobInfo{
		ID:             "test-job-timeout-drain",
		DirectPaths:    []string{src},
		Args:           []string{"-i", "<INPUT_FILE>", "-c:v", "libx264"},
		OutputFilename: filepath.Join(tmpDir, "out.mkv"),
		Timeout:        &timeout,
	}

	ctx := context.Background()
	jobCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	start := time.Now()
	w.processJob(jobCtx, job, cancel, false)

	select {
	case at := <-terminalAt:
		if elapsed := at.Sub(start); elapsed >= stderrDelay {
			t.Fatalf("terminal timeout PATCH arrived after %v; it waited out the %v stderr drain instead of the %v flush budget",
				elapsed, stderrDelay, terminalFlushBudget)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("terminal timeout PATCH never arrived")
	}

	mu.Lock()
	body := append([]byte(nil), terminalBody...)
	mu.Unlock()
	var update protocol.JobUpdateRequest
	if err := json.Unmarshal(body, &update); err != nil {
		t.Fatalf("failed to parse terminal update: %v\nbody: %s", err, body)
	}
	if update.Status != protocol.JobStatusTimeout {
		t.Errorf("Status = %q, want %q", update.Status, protocol.JobStatusTimeout)
	}
	if update.FailureType != string(protocol.FailureTimeout) {
		t.Errorf("FailureType = %q, want %q", update.FailureType, protocol.FailureTimeout)
	}
}
