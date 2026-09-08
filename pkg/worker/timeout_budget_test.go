package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/tsic404/rffmpeg/pkg/protocol"
	"github.com/tsic404/rffmpeg/pkg/worker/gpu"
)

// writeMarkerSleepFFmpeg creates an executable that records its PID, touches a
// marker file, then sleeps — simulating a transcode that outlives a short
// timeout budget. The PID file lets the test assert no leftover process
// survives the executor's process-group kill.
func writeMarkerSleepFFmpeg(dir, markerPath, pidPath string, seconds int) (string, error) {
	scriptPath := filepath.Join(dir, "marker-sleep-ffmpeg")
	script := fmt.Sprintf("#!/bin/sh\necho $$ > %s\ntouch %s\nsleep %d\n", pidPath, markerPath, seconds)
	if err := os.WriteFile(scriptPath, []byte(script), 0755); err != nil {
		return "", fmt.Errorf("write marker-sleep script: %w", err)
	}
	return scriptPath, nil
}

// writeSlowFFprobe creates an executable that sleeps, so the pre-execution
// duration probe deterministically consumes wall-clock time (simulating a slow
// ffprobe). It ignores its arguments and emits no JSON; the probe loop treats
// the parse failure as a skip, exactly like a real unreadable input.
func writeSlowFFprobe(dir string, seconds int) (string, error) {
	scriptPath := filepath.Join(dir, "slow-ffprobe")
	script := fmt.Sprintf("#!/bin/sh\nsleep %d\n", seconds)
	if err := os.WriteFile(scriptPath, []byte(script), 0755); err != nil {
		return "", fmt.Errorf("write slow-ffprobe script: %w", err)
	}
	return scriptPath, nil
}

// writeStatefulRetryFFmpeg creates an executable whose first invocation fails
// with a retryable "Unknown encoder" error and whose subsequent invocations
// sleep until killed — so the initial attempt enters the retry path and the
// retry attempt itself exceeds the execution budget (TSI-2684).
func writeStatefulRetryFFmpeg(dir, counterPath, markerPath, pidPath string, sleepSeconds int) (string, error) {
	scriptPath := filepath.Join(dir, "stateful-retry-ffmpeg")
	script := fmt.Sprintf(`#!/bin/sh
n=0
[ -f '%s' ] && n=$(cat '%s')
n=$((n+1))
echo "$n" > '%s'
echo $$ > '%s'
if [ "$n" -eq 1 ]; then
  echo "Unknown encoder 'h264_fake'" >&2
  exit 1
fi
touch '%s'
sleep %d
`, counterPath, counterPath, counterPath, pidPath, markerPath, sleepSeconds)
	if err := os.WriteFile(scriptPath, []byte(script), 0755); err != nil {
		return "", fmt.Errorf("write stateful-retry script: %w", err)
	}
	return scriptPath, nil
}

// TestProcessJob_ShortTimeoutReachesFFmpegAndClassifiesTimeout is the
// TSI-2684 regression test. A short per-job timeout must be reserved for the
// ffmpeg execution budget, not spent on the pre-execution pipeline. With the
// old shared deadline, the 1s duration probe consumed the whole budget and the
// job failed in encoder classification as FFMPEG_ERROR without ever reaching
// ffmpeg. After the fix: ffmpeg runs, times out on its own budget, and the
// terminal update is classified TIMEOUT with no leftover process.
func TestProcessJob_ShortTimeoutReachesFFmpegAndClassifiesTimeout(t *testing.T) {
	tmpDir := t.TempDir()
	markerPath := filepath.Join(tmpDir, "ffmpeg-ran")
	pidPath := filepath.Join(tmpDir, "ffmpeg.pid")

	ffmpegPath, err := writeMarkerSleepFFmpeg(tmpDir, markerPath, pidPath, 3)
	if err != nil {
		t.Fatal(err)
	}
	slowProbe, err := writeSlowFFprobe(tmpDir, 1)
	if err != nil {
		t.Fatal(err)
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

	w := &Worker{
		id:                 "test-worker-001",
		name:               "test-worker",
		client:             client,
		executor:           executor,
		retryExecutor:      NewRetryExecutor(executor, DefaultRetryConfig()),
		rewriteAdapter:     rewriteAdapter,
		cache:              cache,
		tempDir:            workerTempDir,
		activeJobs:         make(map[string]context.CancelFunc),
		heartbeatInterval:  30 * time.Second,
		pollInterval:       5 * time.Second,
		lastHeartbeatTime:  time.Now(),
		ffprobeExecutor:    NewFFprobeExecutor(slowProbe),
		pixelFormatChecker: nil,
		gpuDetector:        gpu.NewDetector(),
	}

	src := filepath.Join(tmpDir, "input.mkv")
	if err := os.WriteFile(src, []byte("dummy"), 0644); err != nil {
		t.Fatal(err)
	}

	// Short per-job timeout: must be reserved for ffmpeg execution, not spent
	// on the 1s duration probe (TSI-2684).
	timeout := 300 * time.Millisecond
	job := protocol.JobInfo{
		ID:             "test-job-short-timeout",
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
	elapsed := time.Since(start)

	// The ffmpeg execution path must have been reached: the marker is only
	// written by the ffmpeg script itself.
	if _, err := os.Stat(markerPath); err != nil {
		t.Fatalf("ffmpeg execution path not reached (marker missing): %v", err)
	}

	// The 1s duration probe must have run to completion, not been truncated by
	// the 300ms job timeout — proving the timeout budget is decoupled from the
	// pre-execution pipeline.
	if elapsed < time.Second {
		t.Errorf("elapsed = %v, want >= 1s (probe must not be truncated by the job timeout)", elapsed)
	}

	// The terminal update must classify the ffmpeg timeout as TIMEOUT, not
	// FFMPEG_ERROR.
	mockSrv.mu.Lock()
	body := mockSrv.terminalBody
	mockSrv.mu.Unlock()
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

	// No leftover process: the executor's process-group kill must have reaped
	// the ffmpeg script and its sleep child.
	pidData, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatalf("ffmpeg pid file missing: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(pidData)))
	if err != nil {
		t.Fatalf("bad pid %q: %v", pidData, err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		err := syscall.Kill(pid, 0)
		if err == syscall.ESRCH {
			break // process gone — good
		}
		if time.Now().After(deadline) {
			t.Fatalf("ffmpeg process %d still alive after timeout", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestProcessJob_RetryTimeoutClassifiedAsTimeout is the TSI-2684 retry-path
// regression test. The initial attempt fails retryably (Unknown encoder) and
// enters the multi-stage retry; the retry attempt then sleeps past the shared
// execCtx budget and is killed. The retry-exhausted path must report the job as
// TIMEOUT (status=timeout, failure_type=TIMEOUT), not FFMPEG_ERROR.
func TestProcessJob_RetryTimeoutClassifiedAsTimeout(t *testing.T) {
	tmpDir := t.TempDir()
	counterPath := filepath.Join(tmpDir, "ffmpeg.counter")
	markerPath := filepath.Join(tmpDir, "retry-ran")
	pidPath := filepath.Join(tmpDir, "retry.pid")

	ffmpegPath, err := writeStatefulRetryFFmpeg(tmpDir, counterPath, markerPath, pidPath, 10)
	if err != nil {
		t.Fatal(err)
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

	w := &Worker{
		id:                 "test-worker-001",
		name:               "test-worker",
		client:             client,
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

	// Short budget: long enough for the initial retryable failure, short enough
	// that the sleeping retry attempt is killed by the deadline.
	timeout := 500 * time.Millisecond
	job := protocol.JobInfo{
		ID:             "test-job-retry-timeout",
		DirectPaths:    []string{src},
		Args:           []string{"-i", "<INPUT_FILE>", "-c:v", "h264_fake"},
		OutputFilename: filepath.Join(tmpDir, "out.mkv"),
		Timeout:        &timeout,
	}

	ctx := context.Background()
	jobCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	w.processJob(jobCtx, job, cancel, false)

	// The retry attempt must have started (marker written before the sleep).
	if _, err := os.Stat(markerPath); err != nil {
		t.Fatalf("retry attempt not reached (marker missing): %v", err)
	}

	// Terminal update must be TIMEOUT, not FFMPEG_ERROR.
	mockSrv.mu.Lock()
	body := mockSrv.terminalBody
	mockSrv.mu.Unlock()
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

	// No leftover retry process.
	pidData, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatalf("retry pid file missing: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(pidData)))
	if err != nil {
		t.Fatalf("bad pid %q: %v", pidData, err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		err := syscall.Kill(pid, 0)
		if err == syscall.ESRCH {
			break // process gone — good
		}
		if time.Now().After(deadline) {
			t.Fatalf("retry ffmpeg process %d still alive after timeout", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// writeAlwaysRetryableFailFFmpeg creates an executable that immediately exits
// with a retryable "Unknown encoder" error on every invocation, so the retry
// executor keeps failing fast and spends the budget in its backoff interval.
func writeAlwaysRetryableFailFFmpeg(dir string) (string, error) {
	scriptPath := filepath.Join(dir, "always-retryable-ffmpeg")
	script := "#!/bin/sh\necho \"Unknown encoder 'h264_fake'\" >&2\nexit 1\n"
	if err := os.WriteFile(scriptPath, []byte(script), 0755); err != nil {
		return "", fmt.Errorf("write always-retryable script: %w", err)
	}
	return scriptPath, nil
}

// TestProcessJob_RetryBackoffBudgetExhaustionClassifiedAsTimeout is the
// TSI-2684 backoff-interval regression test. Every attempt fails retryably
// immediately, so the execution budget (150ms) is exhausted inside the retry
// executor's applyRetryInterval wait (1s default), not inside an attempt:
// FinalResult.IsTimeout stays false and only execCtx.Err() reports
// DeadlineExceeded. The retry-exhausted path must still report TIMEOUT.
func TestProcessJob_RetryBackoffBudgetExhaustionClassifiedAsTimeout(t *testing.T) {
	tmpDir := t.TempDir()

	ffmpegPath, err := writeAlwaysRetryableFailFFmpeg(tmpDir)
	if err != nil {
		t.Fatal(err)
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

	w := &Worker{
		id:                 "test-worker-001",
		name:               "test-worker",
		client:             client,
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

	// Short budget: the initial + first retry attempts fail immediately, then
	// the budget expires during the 1s backoff interval.
	timeout := 150 * time.Millisecond
	job := protocol.JobInfo{
		ID:             "test-job-retry-backoff-timeout",
		DirectPaths:    []string{src},
		Args:           []string{"-i", "<INPUT_FILE>", "-c:v", "h264_fake"},
		OutputFilename: filepath.Join(tmpDir, "out.mkv"),
		Timeout:        &timeout,
	}

	ctx := context.Background()
	jobCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	w.processJob(jobCtx, job, cancel, false)

	// Terminal update must be TIMEOUT, not FFMPEG_ERROR.
	mockSrv.mu.Lock()
	body := mockSrv.terminalBody
	mockSrv.mu.Unlock()
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
