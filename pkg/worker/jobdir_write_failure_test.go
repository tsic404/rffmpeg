package worker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tsic404/rffmpeg/pkg/protocol"
	"github.com/tsic404/rffmpeg/pkg/worker/gpu"
)

// TestProcessJob_JobDirWriteFailureReportsInfraFailure locks the INFRA bucket
// end-to-end: when the worker cannot create the per-job working directory (a
// read-only or full temp filesystem), processJob must fail the job with
// failure_type=INFRA — never misclassify it as an ffmpeg or input error. INFRA
// was previously reachable only through unit tests (failure_classify_test.go);
// this drives the real processJob pipeline through reportInfraFailure.
func TestProcessJob_JobDirWriteFailureReportsInfraFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission-based test; requires non-root")
	}

	mockSrv := newProbeDirectMockServer()
	defer mockSrv.Close()

	tmpDir := t.TempDir()
	ffmpegPath, err := writeMockFFmpeg(tmpDir)
	if err != nil {
		t.Fatalf("write mock ffmpeg: %v", err)
	}

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
	// A non-writable temp root makes os.MkdirAll(jobDir) fail with EACCES — the
	// "job directory write failure" the INFRA bucket must capture. RemoveAll on
	// the still-absent jobDir returns nil first, so the failure lands exactly on
	// the MkdirAll call rather than the stale-output cleanup guard.
	if err := os.Chmod(workerTempDir, 0o555); err != nil {
		t.Fatalf("chmod worker temp dir read-only: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(workerTempDir, 0o755) })

	executor := NewExecutor(ffmpegPath, 30*time.Second)
	rewriteAdapter := NewRewriteAdapter()
	rewriteAdapter.config.Enabled = false

	w := &Worker{
		id:                 "test-worker-001",
		name:               "test-worker",
		client:             NewClient(mockSrv.URL, uuid.New().String(), ""),
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

	job := protocol.JobInfo{
		ID:             "job-dir-write-failure",
		InputFiles:     []string{"input-001"},
		Args:           []string{"-i", "<INPUT_FILE>", "-c:v", "libx264"},
		OutputFilename: "out.mp4",
	}

	ctx := context.Background()
	jobCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	w.processJob(jobCtx, job, cancel, false)

	update := terminalUpdate(t, mockSrv)
	if update.Status != protocol.JobStatusFailed {
		t.Fatalf("Status = %q, want %q", update.Status, protocol.JobStatusFailed)
	}
	if update.FailureType != string(protocol.FailureInfra) {
		t.Errorf("FailureType = %q, want %q", update.FailureType, protocol.FailureInfra)
	}
	if !strings.Contains(update.Error, "Failed to create job directory") {
		t.Errorf("Error = %q, want it to name the job-directory creation failure", update.Error)
	}
}
