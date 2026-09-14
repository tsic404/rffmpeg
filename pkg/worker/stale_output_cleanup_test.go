package worker

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tsic404/rffmpeg/pkg/protocol"
	"github.com/tsic404/rffmpeg/pkg/worker/gpu"
)

// writeMockFFmpegRefuseOverwrite creates a mock ffmpeg that emulates ffmpeg's
// native no -y semantics: if the output file already exists it prints the
// "Not overwriting - exiting" refusal to stderr and exits 1; otherwise it
// writes a fake output and exits 0.
func writeMockFFmpegRefuseOverwrite(dir string) (string, error) {
	scriptPath := filepath.Join(dir, "mock-ffmpeg-refuse-overwrite")
	script := `#!/bin/bash
output=""
for arg in "$@"; do output="$arg"; done
if [ -e "$output" ]; then
  echo "File '$output' already exists. Overwrite? [y/N] Not overwriting - exiting" >&2
  exit 1
fi
mkdir -p "$(dirname "$output")" 2>/dev/null
echo "mock_transcoded_data" > "$output"
exit 0
`
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		return "", err
	}
	return scriptPath, nil
}

// newRefuseOverwriteWorker builds a Worker wired to a mock server and the
// refuse-to-overwrite mock ffmpeg. The cache starts empty (always a miss), so
// both tests exercise the ffmpeg execution path rather than the cache-hit path.
func newRefuseOverwriteWorker(t *testing.T) (*Worker, *mockJobServer) {
	t.Helper()

	tmpDir := t.TempDir()
	ffmpegPath, err := writeMockFFmpegRefuseOverwrite(tmpDir)
	if err != nil {
		t.Fatalf("write mock ffmpeg: %v", err)
	}

	mockSrv := newMockJobServer()
	t.Cleanup(mockSrv.Close)

	cacheDir := filepath.Join(tmpDir, "cache")
	cache, err := NewCache(CacheConfig{Enabled: true, Dir: cacheDir, TTL: 24 * time.Hour})
	if err != nil {
		t.Fatalf("NewCache failed: %v", err)
	}
	t.Cleanup(func() { cache.Stop() })

	workerTempDir := filepath.Join(tmpDir, "worker-temp")
	if err := os.MkdirAll(workerTempDir, 0o755); err != nil {
		t.Fatalf("mkdir worker-temp: %v", err)
	}

	executor := NewExecutor(ffmpegPath, 30*time.Second)
	client := NewClient(mockSrv.URL, "test-worker-001", "")

	rewriteAdapter := NewRewriteAdapter()
	rewriteAdapter.config.Enabled = false

	retryExecutor := NewRetryExecutor(executor, DefaultRetryConfig())

	w := &Worker{
		id:                 "test-worker-001",
		name:               "test-worker",
		client:             client,
		executor:           executor,
		retryExecutor:      retryExecutor,
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

	return w, mockSrv
}

// TestProcessJob_RemovesStaleOutputBeforeFFmpeg locks the
// fix: a re-dispatched job (worker-failure migration or server-restart
// recovery) re-enters processJob with a job-private temp directory that still
// holds a partial output from its interrupted previous attempt. Without
// cleanup, ffmpeg's native no -y semantics refuse to overwrite it and the
// re-dispatched run fails. processJob must remove the stale directory first so
// the mock ffmpeg (which refuses to overwrite an existing output) succeeds and
// the output is uploaded.
func TestProcessJob_RemovesStaleOutputBeforeFFmpeg(t *testing.T) {
	w, mockSrv := newRefuseOverwriteWorker(t)

	ctx := context.Background()
	jobCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	job := protocol.JobInfo{
		ID:             "job-restart-recovery",
		InputFiles:     []string{"input-001"},
		Args:           []string{"-i", "<INPUT_FILE>", "-c:v", "libx264"},
		OutputFilename: "out.mp4",
	}

	// Simulate the re-dispatch: a previous interrupted attempt left a partial
	// output in the job's private temp dir.
	staleOutput := filepath.Join(w.tempDir, job.ID, "out.mp4")
	if err := os.MkdirAll(filepath.Dir(staleOutput), 0o755); err != nil {
		t.Fatalf("mkdir stale dir: %v", err)
	}
	if err := os.WriteFile(staleOutput, []byte("partial output"), 0o644); err != nil {
		t.Fatalf("write stale output: %v", err)
	}

	beforeUploads := mockSrv.UploadCount()
	w.processJob(jobCtx, job, cancel, false)

	if got := mockSrv.UploadCount() - beforeUploads; got != 1 {
		t.Fatalf("expected exactly 1 output upload after stale-output cleanup, got %d", got)
	}
}

// TestProcessJob_PreservesExistingAbsoluteOutput locks the boundary:
// a user-specified absolute output that already exists must NOT be removed by
// the stale-output cleanup (the cleanup is scoped to the job-private temp
// directory), so ffmpeg's native refusal fails the job and the user's file
// survives untouched.
func TestProcessJob_PreservesExistingAbsoluteOutput(t *testing.T) {
	w, mockSrv := newRefuseOverwriteWorker(t)

	ctx := context.Background()
	jobCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	existingOutput := filepath.Join(t.TempDir(), "existing.mp4")
	if err := os.WriteFile(existingOutput, []byte("user data"), 0o644); err != nil {
		t.Fatalf("write existing output: %v", err)
	}

	job := protocol.JobInfo{
		ID:             "job-tsi-2964-boundary",
		InputFiles:     []string{"input-001"},
		Args:           []string{"-i", "<INPUT_FILE>", "-c:v", "libx264"},
		OutputFilename: existingOutput,
	}

	beforeUploads := mockSrv.UploadCount()
	w.processJob(jobCtx, job, cancel, false)

	if got := mockSrv.UploadCount() - beforeUploads; got != 0 {
		t.Fatalf("expected no upload (job must fail), got %d uploads", got)
	}
	if b, err := os.ReadFile(existingOutput); err != nil {
		t.Fatalf("existing output must survive: %v", err)
	} else if string(b) != "user data" {
		t.Fatalf("existing output was modified: %q", b)
	}
}

// TestProcessJob_RejectsTraversalJobID locks the removal-scope guard: a job ID
// containing ".." must not let os.RemoveAll escape the worker temp root and
// delete a sibling directory. The job fails as an infra failure and the
// sibling survives untouched.
func TestProcessJob_RejectsTraversalJobID(t *testing.T) {
	w, mockSrv := newRefuseOverwriteWorker(t)

	ctx := context.Background()
	jobCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// A sibling directory outside the worker temp root that must survive.
	// jobDir = filepath.Join(w.tempDir, "..") resolves to w.tempDir's parent,
	// so an unguarded RemoveAll would delete this sibling along with the root.
	sibling := filepath.Join(filepath.Dir(w.tempDir), "sibling-must-survive")
	if err := os.MkdirAll(sibling, 0o755); err != nil {
		t.Fatalf("mkdir sibling: %v", err)
	}
	marker := filepath.Join(sibling, "marker.txt")
	if err := os.WriteFile(marker, []byte("survive"), 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}

	job := protocol.JobInfo{
		ID:             "..",
		InputFiles:     []string{"input-001"},
		Args:           []string{"-i", "<INPUT_FILE>", "-c:v", "libx264"},
		OutputFilename: "out.mp4",
	}

	beforeUploads := mockSrv.UploadCount()
	w.processJob(jobCtx, job, cancel, false)

	if got := mockSrv.UploadCount() - beforeUploads; got != 0 {
		t.Fatalf("expected no upload (job must fail), got %d", got)
	}
	if b, err := os.ReadFile(marker); err != nil {
		t.Fatalf("sibling marker must survive: %v", err)
	} else if string(b) != "survive" {
		t.Fatalf("sibling marker was modified: %q", b)
	}
}
