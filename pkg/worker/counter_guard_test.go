package worker

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tsic404/rffmpeg/pkg/protocol"
	"github.com/tsic404/rffmpeg/pkg/worker/gpu"
)

// readWorkerCounters snapshots the completion counters under w.mu.
func readWorkerCounters(w *Worker) (jobs, total int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.jobsCompleted, w.totalJobsCompleted
}

// newCounterTestWorker builds a Worker with a real cache, rewrite disabled,
// and a caller-chosen executor. The client is left for each test to assign so
// failure injection can point it at a custom server.
func newCounterTestWorker(t *testing.T, ffmpegPath string) *Worker {
	t.Helper()

	tmpDir := t.TempDir()
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

	return &Worker{
		id:                 "counter-guard-worker",
		name:               "counter-guard-worker",
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
}

// assignMockClient points the worker at a permissive mock server that accepts
// file downloads and job PATCHes (mirroring newMockJobServer).
func assignMockClient(t *testing.T, w *Worker) {
	t.Helper()
	mockSrv := newMockJobServer()
	t.Cleanup(mockSrv.Close)
	w.client = NewClient(mockSrv.URL, "counter-guard-worker", "")
}

// writeSleepScript creates an executable that ignores ffmpeg args and sleeps,
// used to force the executor timeout path.
func writeSleepScript(dir string, seconds int) (string, error) {
	scriptPath := filepath.Join(dir, "sleep-ffmpeg")
	script := fmt.Sprintf("#!/bin/sh\nsleep %d\n", seconds)
	if err := os.WriteFile(scriptPath, []byte(script), 0755); err != nil {
		return "", fmt.Errorf("write sleep script: %w", err)
	}
	return scriptPath, nil
}

// assertCountersZero fails if processJob advanced either completion counter.
func assertCountersZero(t *testing.T, w *Worker, path string) {
	t.Helper()
	jobs, total := readWorkerCounters(w)
	if jobs != 0 || total != 0 {
		t.Errorf("%s: counters advanced on failure: jobsCompleted=%d totalJobsCompleted=%d, want 0/0",
			path, jobs, total)
	}
}

// TestDirectPathTraversalDoesNotAdvanceCounters pins the direct-path traversal
// reject return: a rejected job must not count as completed.
func TestDirectPathTraversalDoesNotAdvanceCounters(t *testing.T) {
	w := newCounterTestWorker(t, filepath.Join(t.TempDir(), "unused"))
	assignMockClient(t, w)

	ctx := context.Background()
	jobCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	job := protocol.JobInfo{
		ID:          "counter-traversal",
		DirectPaths: []string{"../etc/passwd"},
		Args:        []string{"-i", "<INPUT_FILE>", "-c:v", "libx264"},
	}

	w.processJob(jobCtx, job, cancel, false)
	assertCountersZero(t, w, "direct traversal reject")
}

// TestDirectPathStatFailureDoesNotAdvanceCounters pins the direct-path
// os.Stat failure return.
func TestDirectPathStatFailureDoesNotAdvanceCounters(t *testing.T) {
	w := newCounterTestWorker(t, filepath.Join(t.TempDir(), "unused"))
	assignMockClient(t, w)

	ctx := context.Background()
	jobCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	missing := filepath.Join(t.TempDir(), "does-not-exist.mkv")
	job := protocol.JobInfo{
		ID:          "counter-stat-fail",
		DirectPaths: []string{missing},
		Args:        []string{"-i", "<INPUT_FILE>", "-c:v", "libx264"},
	}

	w.processJob(jobCtx, job, cancel, false)
	assertCountersZero(t, w, "direct path stat failure")
}

// TestCacheMissMkdirAllFailureDoesNotAdvanceCounters pins the cache-miss
// MkdirAll failure return.
func TestCacheMissMkdirAllFailureDoesNotAdvanceCounters(t *testing.T) {
	w := newCounterTestWorker(t, filepath.Join(t.TempDir(), "unused"))
	assignMockClient(t, w)

	// Replace tempDir with a regular file so MkdirAll(jobDir) fails.
	tmpFile := filepath.Join(t.TempDir(), "tempdir-as-file")
	if err := os.WriteFile(tmpFile, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	w.tempDir = tmpFile

	ctx := context.Background()
	jobCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	job := protocol.JobInfo{
		ID:         "counter-miss-mkdir",
		InputFiles: []string{"input-001"},
		Args:       []string{"-i", "<INPUT_FILE>", "-c:v", "libx264"},
	}

	w.processJob(jobCtx, job, cancel, false)
	assertCountersZero(t, w, "cache-miss MkdirAll failure")
}

// TestRunningUpdateFailureDoesNotAdvanceCounters pins the UpdateJob(Running)
// failure return: a job that never reached running must not count as completed.
func TestRunningUpdateFailureDoesNotAdvanceCounters(t *testing.T) {
	w := newCounterTestWorker(t, filepath.Join(t.TempDir(), "unused"))

	var patchCount int32
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/v1/files/"):
			rw.WriteHeader(http.StatusOK)
			rw.Write([]byte("mock input"))
		case r.Method == http.MethodPatch:
			atomic.AddInt32(&patchCount, 1)
			rw.WriteHeader(http.StatusInternalServerError)
		default:
			rw.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	w.client = NewClient(srv.URL, "counter-guard-worker", "")

	ctx := context.Background()
	jobCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	job := protocol.JobInfo{
		ID:         "counter-running-fail",
		InputFiles: []string{"input-001"},
		Args:       []string{"-i", "<INPUT_FILE>", "-c:v", "libx264"},
	}

	w.processJob(jobCtx, job, cancel, false)
	assertCountersZero(t, w, "UpdateJob(Running) failure")
	if atomic.LoadInt32(&patchCount) == 0 {
		t.Error("expected at least one PATCH for the running status update")
	}
}

// TestDownloadFailureDoesNotAdvanceCounters pins the DownloadInput failure
// return.
func TestDownloadFailureDoesNotAdvanceCounters(t *testing.T) {
	w := newCounterTestWorker(t, filepath.Join(t.TempDir(), "unused"))

	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/v1/files/"):
			rw.WriteHeader(http.StatusInternalServerError)
		case r.Method == http.MethodPatch:
			rw.WriteHeader(http.StatusOK)
			rw.Write([]byte(`{"message":"ok"}`))
		default:
			rw.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	w.client = NewClient(srv.URL, "counter-guard-worker", "")

	ctx := context.Background()
	jobCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	job := protocol.JobInfo{
		ID:         "counter-download-fail",
		InputFiles: []string{"input-001"},
		Args:       []string{"-i", "<INPUT_FILE>", "-c:v", "libx264"},
	}

	w.processJob(jobCtx, job, cancel, false)
	assertCountersZero(t, w, "DownloadInput failure")
}

// TestJobCancelDoesNotAdvanceCounters pins the cancellation return: a job
// cancelled by the server must not count as completed.
func TestJobCancelDoesNotAdvanceCounters(t *testing.T) {
	ffmpegPath, err := writeMockFFmpeg(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w := newCounterTestWorker(t, ffmpegPath)
	assignMockClient(t, w)

	jobCtx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancelled: processJob must take the cancel branch

	job := protocol.JobInfo{
		ID:         "counter-cancel",
		InputFiles: []string{"input-001"},
		Args:       []string{"-i", "<INPUT_FILE>", "-c:v", "libx264"},
	}

	w.processJob(jobCtx, job, cancel, false)
	assertCountersZero(t, w, "cancellation")
}

// TestJobTimeoutDoesNotAdvanceCounters pins the executor timeout return.
func TestJobTimeoutDoesNotAdvanceCounters(t *testing.T) {
	sleepScript, err := writeSleepScript(t.TempDir(), 3)
	if err != nil {
		t.Fatal(err)
	}
	w := newCounterTestWorker(t, sleepScript)
	assignMockClient(t, w)

	// Tight executor timeout so the sleep script is killed before finishing.
	w.executor = NewExecutor(sleepScript, 50*time.Millisecond)

	src := filepath.Join(t.TempDir(), "input.mkv")
	if err := os.WriteFile(src, []byte("dummy"), 0644); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	jobCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	job := protocol.JobInfo{
		ID:             "counter-timeout",
		DirectPaths:    []string{src},
		Args:           []string{"-i", "<INPUT_FILE>", "-c:v", "libx264"},
		OutputFilename: "out.mkv",
	}

	w.processJob(jobCtx, job, cancel, false)
	assertCountersZero(t, w, "executor timeout")
}
