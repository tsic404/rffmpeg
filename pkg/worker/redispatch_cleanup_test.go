package worker

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tsic404/rffmpeg/pkg/protocol"
	"github.com/tsic404/rffmpeg/pkg/worker/gpu"
)

// newRedispatchWorker builds a direct-mode worker with rewrite disabled so
// args pass through verbatim, and a fresh in-memory cache.
func newRedispatchWorker(t *testing.T, allowedPrefix, serverURL string) *Worker {
	t.Helper()

	cacheDir := filepath.Join(t.TempDir(), "cache")
	cache, err := NewCache(CacheConfig{Enabled: true, Dir: cacheDir, TTL: 24 * time.Hour})
	if err != nil {
		t.Fatalf("NewCache failed: %v", err)
	}
	t.Cleanup(func() { cache.Stop() })

	workerTempDir := filepath.Join(t.TempDir(), "worker-temp")
	if err := os.MkdirAll(workerTempDir, 0755); err != nil {
		t.Fatalf("mkdir worker-temp: %v", err)
	}

	executor := NewExecutor("ffmpeg", 30*time.Second)
	rewriteAdapter := NewRewriteAdapter()
	rewriteAdapter.config.Enabled = false // keep args verbatim; no capability rewrite in this test

	return &Worker{
		id:                "test-worker-001",
		name:              "test-worker",
		client:            NewClient(serverURL, uuid.New().String(), ""),
		executor:          executor,
		retryExecutor:     NewRetryExecutor(executor, DefaultRetryConfig()),
		rewriteAdapter:    rewriteAdapter,
		cache:             cache,
		tempDir:           workerTempDir,
		activeJobs:        make(map[string]context.CancelFunc),
		heartbeatInterval: 30 * time.Second,
		pollInterval:      5 * time.Second,
		lastHeartbeatTime: time.Now(),
		ffprobeExecutor:   NewFFprobeExecutor("ffprobe"),
		gpuDetector:       gpu.NewDetector(),
		allowedPrefixes:   []string{allowedPrefix},
	}
}

// genTestInputMP4 produces a tiny real mp4 the worker can transcode. The caller
// must have already skipped when ffmpeg is unavailable.
func genTestInputMP4(t *testing.T, dir string) string {
	t.Helper()
	inputPath := filepath.Join(dir, "input.mp4")
	gen := exec.Command("ffmpeg", "-y",
		"-f", "lavfi", "-i", "testsrc=duration=0.1:size=64x64:rate=10",
		"-c:v", "libx264", "-preset", "ultrafast", inputPath)
	if out, err := gen.CombinedOutput(); err != nil {
		t.Fatalf("failed to generate input: %v\n%s", err, out)
	}
	return inputPath
}

// terminalUpdate returns the single terminal job update the mock server recorded.
func terminalUpdate(t *testing.T, srv *probeDirectMockServer) protocol.JobUpdateRequest {
	t.Helper()
	if atomic.LoadInt32(&srv.terminalCount) == 0 {
		t.Fatal("expected a terminal job update PATCH, got none")
	}
	srv.mu.Lock()
	body := srv.terminalBody
	srv.mu.Unlock()

	var update protocol.JobUpdateRequest
	if err := json.Unmarshal(body, &update); err != nil {
		t.Fatalf("failed to parse terminal update body: %v\nbody: %s", err, body)
	}
	return update
}

// TestProcessJob_RedispatchRemovesStaleOutputBeforeExecution closes the
// self-heal gap: after a worker crash, the job migrates to another worker and is
// re-dispatched (RetryCount > 0). In shared-FS mode the previous worker left a
// truncated partial output at the shared absolute path, so a naive re-run would
// make ffmpeg refuse with "Not overwriting - exiting" and break the self-heal
// chain. The fix removes that stale output before the first execution — exactly
// like RetryExecutor does between its attempts — but only on re-dispatch: a
// fresh job (RetryCount 0) keeps native ffmpeg overwrite semantics.
func TestProcessJob_RedispatchRemovesStaleOutputBeforeExecution(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}

	dir := t.TempDir()
	inputPath := genTestInputMP4(t, dir)
	outputPath := filepath.Join(dir, "out.mp4")

	mockSrv := newProbeDirectMockServer()
	defer mockSrv.Close()
	w := newRedispatchWorker(t, dir, mockSrv.URL)

	job := protocol.JobInfo{
		ID:             "tsi-3127-redispatch",
		InputFiles:     []string{inputPath},
		Args:           []string{"-i", "<INPUT_FILE>", "-c:v", "libx264", "-preset", "ultrafast"},
		OutputFilename: outputPath,
		DirectPaths:    []string{inputPath},
		RetryCount:     1, // migrated once — must clean stale output before re-running
	}

	// Simulate the crashed worker's truncated partial output at the shared path.
	if err := os.WriteFile(outputPath, []byte("STALE PARTIAL OUTPUT"), 0o644); err != nil {
		t.Fatalf("write stale output: %v", err)
	}

	w.processJob(context.Background(), job, func() {}, false)

	update := terminalUpdate(t, mockSrv)
	if update.Status != protocol.JobStatusCompleted {
		t.Fatalf("Status = %q, want %q (error=%q)", update.Status, protocol.JobStatusCompleted, update.Error)
	}

	// The re-run must have produced real output, not left the stale bytes.
	b, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatalf("read output after re-dispatch: %v", err)
	}
	if strings.TrimSpace(string(b)) == "STALE PARTIAL OUTPUT" {
		t.Fatal("output still contains the stale partial bytes — re-dispatch cleanup did not run")
	}
	if len(b) == 0 {
		t.Fatal("output is empty after re-dispatch")
	}
}

// TestProcessJob_RedispatchNeverOverwritePreservesOutput locks the overwrite
// policy guard: a re-dispatched job that explicitly passed -n must keep the
// "exists → fail" contract. The stale/valid output is left in place so ffmpeg
// rejects it, instead of being silently deleted and re-transcoded over the
// user's data.
func TestProcessJob_RedispatchNeverOverwritePreservesOutput(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}

	dir := t.TempDir()
	inputPath := genTestInputMP4(t, dir)
	outputPath := filepath.Join(dir, "out.mp4")

	// A valid completed output — the previous worker finished the encode and
	// only crashed before reporting its terminal state.
	valid := exec.Command("ffmpeg", "-y", "-i", inputPath,
		"-c:v", "libx264", "-preset", "ultrafast", outputPath)
	if out, err := valid.CombinedOutput(); err != nil {
		t.Fatalf("failed to generate valid output: %v\n%s", err, out)
	}
	before, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatalf("read valid output: %v", err)
	}

	mockSrv := newProbeDirectMockServer()
	defer mockSrv.Close()
	w := newRedispatchWorker(t, dir, mockSrv.URL)

	job := protocol.JobInfo{
		ID:             "tsi-3127-never",
		InputFiles:     []string{inputPath},
		Args:           []string{"-n", "-i", "<INPUT_FILE>", "-c:v", "libx264", "-preset", "ultrafast"},
		OutputFilename: outputPath,
		DirectPaths:    []string{inputPath},
		RetryCount:     1,
	}

	w.processJob(context.Background(), job, func() {}, false)

	update := terminalUpdate(t, mockSrv)
	if update.Status == protocol.JobStatusCompleted {
		t.Fatalf("-n re-dispatch was reported completed; explicit no-overwrite semantics were rewritten to overwrite")
	}

	after, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatalf("read output after -n re-dispatch: %v", err)
	}
	if string(after) != string(before) {
		t.Fatal("existing output was modified by -n re-dispatch; the user's data must be preserved")
	}
}

// TestProcessJob_RedispatchRemovalFailureReportsInfraFailure locks the error
// path: when the stale output cannot be removed, the worker must fail the job as
// an infrastructure fault with a diagnostic instead of silently proceeding to
// an ffmpeg run that still refuses to overwrite.
func TestProcessJob_RedispatchRemovalFailureReportsInfraFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission-based test; requires non-root")
	}
	base := t.TempDir()

	inputDir := filepath.Join(base, "in")
	if err := os.MkdirAll(inputDir, 0755); err != nil {
		t.Fatalf("mkdir input dir: %v", err)
	}
	inputPath := filepath.Join(inputDir, "input.mp4")
	if err := os.WriteFile(inputPath, []byte("dummy"), 0o644); err != nil {
		t.Fatalf("write dummy input: %v", err)
	}

	outDir := filepath.Join(base, "out")
	if err := os.MkdirAll(outDir, 0755); err != nil {
		t.Fatalf("mkdir output dir: %v", err)
	}
	outputPath := filepath.Join(outDir, "out.mp4")
	if err := os.WriteFile(outputPath, []byte("STALE"), 0o644); err != nil {
		t.Fatalf("write stale output: %v", err)
	}
	// A non-writable parent makes os.Remove fail with EACCES, leaving the stale
	// file in place — the exact self-heal-break scenario the guard must surface.
	if err := os.Chmod(outDir, 0o555); err != nil {
		t.Fatalf("chmod output dir read-only: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(outDir, 0o755) })

	mockSrv := newProbeDirectMockServer()
	defer mockSrv.Close()
	w := newRedispatchWorker(t, base, mockSrv.URL)

	job := protocol.JobInfo{
		ID:             "tsi-3127-remove-fail",
		InputFiles:     []string{inputPath},
		Args:           []string{"-i", "<INPUT_FILE>", "-c:v", "libx264"},
		OutputFilename: outputPath,
		DirectPaths:    []string{inputPath},
		RetryCount:     1,
	}

	w.processJob(context.Background(), job, func() {}, false)

	update := terminalUpdate(t, mockSrv)
	if update.Status != protocol.JobStatusFailed {
		t.Fatalf("Status = %q, want %q", update.Status, protocol.JobStatusFailed)
	}
	if update.FailureType != string(protocol.FailureInfra) {
		t.Errorf("FailureType = %q, want %q", update.FailureType, protocol.FailureInfra)
	}
	if !strings.Contains(update.Error, "remove stale output") {
		t.Errorf("Error = %q, want it to name the stale-output removal failure", update.Error)
	}
}
