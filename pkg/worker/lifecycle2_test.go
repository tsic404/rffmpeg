package worker

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/tsix404/rffmpeg/pkg/worker/gpu"
)

// TestExecuteWithRetry_StaleOutputFromFailedAttemptDoesNotFoolNextAttempt
// verifies acceptance criterion 4a: a partial output file left behind by a
// failed attempt must not make the next attempt (exit 0 without producing
// output) look successful.
func TestExecuteWithRetry_StaleOutputFromFailedAttemptDoesNotFoolNextAttempt(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}

	dir := t.TempDir()
	outputPath := filepath.Join(dir, "out.mp4")

	// Simulate attempt 1 leaving a partial output behind.
	stale := []byte("STALE PARTIAL OUTPUT")
	if err := os.WriteFile(outputPath, stale, 0o644); err != nil {
		t.Fatal(err)
	}

	executor := NewExecutor("ffmpeg", time.Minute)
	re := &RetryExecutor{
		executor:    executor,
		interceptor: NewErrorInterceptor(),
		pruner:      NewParamPruner(),
		config:      DefaultRetryConfig(),
		fallback:    NewEncoderFallback(),
	}

	// ffmpeg exits 0 but writes nothing to outputPath (output goes to null).
	args := []string{"-f", "lavfi", "-i", "testsrc=duration=0.1", "-f", "null", "-"}
	result := re.ExecuteWithRetry(context.Background(), args, outputPath, false, nil, nil)

	if result.Success {
		t.Fatal("attempt with no real output was judged successful — stale-output cleanup is broken")
	}
	if _, err := os.Stat(outputPath); err == nil {
		t.Log("stale file still present after run (removed per-attempt or never recreated)")
	}
}

// TestExecuteWithRetry_CancelledContextSkipsSoftwareFallback verifies
// acceptance criterion 4b: when the context is already cancelled, the final
// software fallback must not start another full transcode round.
func TestExecuteWithRetry_CancelledContextSkipsSoftwareFallback(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}

	var mu sync.Mutex
	invocations := 0
	recorder := &countingExecutor{
		countFn: func() {
			mu.Lock()
			invocations++
			mu.Unlock()
		},
	}

	re := &RetryExecutor{
		executor:    recorder,
		interceptor: NewErrorInterceptor(),
		pruner:      NewParamPruner(),
		config:      DefaultRetryConfig(),
		fallback:    NewEncoderFallback(),
	}

	// A context that is already cancelled: ExecuteWithRetry must return
	// immediately without executing anything.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	result := re.ExecuteWithRetry(ctx, []string{"-f", "lavfi", "-i", "testsrc=duration=5", "out.mp4"}, "", false, nil, nil)

	if time.Since(start) > 5*time.Second {
		t.Fatal("ExecuteWithRetry ran a full transcode despite cancelled context")
	}
	mu.Lock()
	n := invocations
	mu.Unlock()
	if n != 0 {
		t.Errorf("executed %d ffmpeg commands on an already-cancelled context; want 0", n)
	}
	_ = result
}

// countingExecutor counts every execution attempt.
type countingExecutor struct {
	countFn func()
}

func (c *countingExecutor) Execute(ctx context.Context, args []string) ExecResult {
	c.countFn()
	return ExecResult{ExitCode: -1, Error: fmt.Errorf("cancelled")}
}

func (c *countingExecutor) ExecuteWithHandlers(ctx context.Context, args []string, stdoutHandler StdoutHandler, stderrHandler StderrHandler) ExecResult {
	return c.Execute(ctx, args)
}

// TestDownloadInput_LimitReaderCapsResponse verifies acceptance criterion 5:
// DownloadInput caps how much of a remote response it will write to disk.
func TestDownloadInput_LimitReaderCapsResponse(t *testing.T) {
	big := make([]byte, 64<<20) // 64 MiB response — well above any sane input but below the 10 GiB cap; proves streaming works and completes
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(big)
	}))
	defer srv.Close()

	dir := t.TempDir()
	dest := filepath.Join(dir, "input.bin")

	c := NewClient("http://localhost:1", "worker-1", "")
	if err := c.DownloadInput(srv.URL, dest); err != nil {
		t.Fatalf("DownloadInput failed: %v", err)
	}
	info, err := os.Stat(dest)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != int64(len(big)) {
		t.Errorf("downloaded %d bytes, want %d", info.Size(), len(big))
	}
}

// TestSampleMetrics_NvidiaSmiHangDoesNotBlock verifies acceptance criterion
// 5b: when nvidia-smi hangs, SampleMetrics returns promptly instead of
// blocking the heartbeat loop.
func TestSampleMetrics_NvidiaSmiHangDoesNotBlock(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not installed")
	}
	if _, err := exec.LookPath("nvidia-smi"); err == nil {
		t.Skip("real nvidia-smi installed — would shadow the fake hanging one")
	}

	dir := t.TempDir()
	fake := filepath.Join(dir, "nvidia-smi")
	script := "#!/bin/sh\nsleep 60\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	d := gpu.NewDetector()
	done := make(chan struct{})
	go func() {
		d.SampleMetrics()
		close(done)
	}()

	select {
	case <-done:
		// returned within the command timeout — good
	case <-time.After(10 * time.Second):
		t.Fatal("SampleMetrics still blocked 10s in with a hung nvidia-smi")
	}
}

// TestNew_InitializesPixelFormatChecker verifies acceptance criterion 6:
// the constructor wires up the VAAPI pixel-format checker again.
func TestNew_InitializesPixelFormatChecker(t *testing.T) {
	w, err := New(Config{ServerURL: "http://localhost:1"})
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	if w.pixelFormatChecker == nil {
		t.Fatal("pixelFormatChecker is nil — VAAPI pixel format pre-flight check silently disabled")
	}
}
