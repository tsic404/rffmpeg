package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tsic404/rffmpeg/pkg/protocol"
	"github.com/tsic404/rffmpeg/pkg/worker/gpu"
)

// writeMockFFmpegOutputOpenFail creates an executable shell script that
// simulates ffmpeg n9's behavior for an output-open failure: it writes the
// "Error opening output file" line to stderr, does NOT create the output
// file, and exits 0 — exactly the TSI-2472 QA scenario
// (`-vn -c:a libmp3lame` into a nonexistent directory).
func writeMockFFmpegOutputOpenFail(dir string) (string, error) {
	scriptPath := filepath.Join(dir, "mock-ffmpeg-output-open")
	script := `#!/bin/bash
# Mock ffmpeg n9 output-open failure: stderr error, no output file, exit 0.
echo "[out#0/mp3 @ 0x55be9305cc00] Error opening output /nonexistent_dir/out.mp3: No such file or directory" >&2
echo "Error opening output file /nonexistent_dir/out.mp3." >&2
echo "Error opening output files: No such file or directory" >&2
exit 0
`
	if err := os.WriteFile(scriptPath, []byte(script), 0755); err != nil {
		return "", err
	}
	return scriptPath, nil
}

// outputOpenMockServer is a test HTTP server that captures every job PATCH
// body with a terminal status (failed/timeout/cancelled), so the test can
// assert the worker reported the right exit code and failure type.
type outputOpenMockServer struct {
	*httptest.Server
	mu            sync.Mutex
	terminalBody  []byte
	terminalCount int32
}

func newOutputOpenMockServer() *outputOpenMockServer {
	m := &outputOpenMockServer{}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/files/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("mock input data"))
	})
	mux.HandleFunc("/api/v1/jobs/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			w.WriteHeader(http.StatusOK)
			return
		}
		body := make([]byte, r.ContentLength)
		r.Body.Read(body)
		var update protocol.JobUpdateRequest
		if json.Unmarshal(body, &update) == nil {
			if update.Status == protocol.JobStatusFailed ||
				update.Status == protocol.JobStatusTimeout ||
				update.Status == protocol.JobStatusCancelled {
				m.mu.Lock()
				m.terminalBody = body
				atomic.AddInt32(&m.terminalCount, 1)
				m.mu.Unlock()
			}
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"message":"ok"}`))
	})
	mux.HandleFunc("/api/v1/workers/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"worker_id":"test-worker-001"}`))
	})
	m.Server = httptest.NewServer(mux)
	return m
}

// TestProcessJob_OutputOpenFailurePropagatesExitCode verifies the full
// processJob validation chain for the TSI-2472 QA scenario: ffmpeg exits 0
// with "Error opening output file" on stderr and no output file produced.
//
// Before the fix: os.Stat failure set result.Error but left result.ExitCode=0,
// the critical-error check was skipped (result.Error != nil guard), and the
// worker reported the job as completed with exit_code=0. After the fix:
// os.Stat failure sets result.ExitCode=1, ClassifyFailure recognizes the
// output-open stderr (not INPUT_UNREACHABLE), and the terminal update carries
// a non-zero exit code with FFMPEG_ERROR.
func TestProcessJob_OutputOpenFailurePropagatesExitCode(t *testing.T) {
	tmpDir := t.TempDir()

	ffmpegPath, err := writeMockFFmpegOutputOpenFail(tmpDir)
	if err != nil {
		t.Fatalf("failed to create mock ffmpeg: %v", err)
	}

	mockSrv := newOutputOpenMockServer()
	defer mockSrv.Close()

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

	ctx := context.Background()
	jobCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Output path under a nonexistent directory — os.Stat will fail, matching
	// the QA scenario where the output directory does not exist.
	job := protocol.JobInfo{
		ID:             "test-job-output-open-001",
		InputFiles:     []string{"input-001"},
		Args:           []string{"-i", "<INPUT_FILE>", "-vn", "-c:a", "libmp3lame"},
		OutputFilename: "out.mp3",
	}

	w.processJob(jobCtx, job, cancel, false)

	if atomic.LoadInt32(&mockSrv.terminalCount) == 0 {
		t.Fatal("expected a terminal (failed) job update PATCH, got none")
	}

	mockSrv.mu.Lock()
	body := mockSrv.terminalBody
	mockSrv.mu.Unlock()

	var update protocol.JobUpdateRequest
	if err := json.Unmarshal(body, &update); err != nil {
		t.Fatalf("failed to parse terminal update body: %v\nbody: %s", err, body)
	}

	if update.Status != protocol.JobStatusFailed {
		t.Errorf("Status = %q, want %q", update.Status, protocol.JobStatusFailed)
	}
	if update.ExitCode == 0 {
		t.Errorf("ExitCode = 0, want non-zero (TSI-2472: exit code must propagate)")
	}
	if update.FailureType == string(protocol.FailureInputUnreachable) {
		t.Errorf("FailureType = INPUT_UNREACHABLE, want FFMPEG_ERROR (output-open must not be misclassified as input unreachable)")
	}
	if update.FailureType != string(protocol.FailureFFmpegError) {
		t.Errorf("FailureType = %q, want %q", update.FailureType, protocol.FailureFFmpegError)
	}

	t.Logf("terminal update: status=%s exit_code=%d failure_type=%s details=%q",
		update.Status, update.ExitCode, update.FailureType, update.FailureDetails)
}

// avoid unused imports for uuid (kept for potential future use)
var _ = uuid.New
var _ = fmt.Sprintf
