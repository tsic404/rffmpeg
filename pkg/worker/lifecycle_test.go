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
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/tsix404/rffmpeg/pkg/protocol"
)

// TestExecutor_CancelReturnsWithGrandchildHoldingPipe verifies acceptance
// criterion 1: after ctx cancellation, ExecuteWithHandlers returns within a
// bounded time even when a grandchild process inherits the pipe write ends.
// Regression for the orphaned-process-group hang (executor.go).
func TestExecutor_CancelReturnsWithGrandchildHoldingPipe(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not installed")
	}

	executor := NewExecutor("sh", time.Minute)

	// The shell spawns a background grandchild that holds the pipe write end
	// forever; the shell itself exits immediately.
	args := []string{"-c", `sleep 30 & echo started; exit 0`}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan ExecResult, 1)
	go func() {
		done <- executor.ExecuteWithStderrHandler(ctx, args, nil)
	}()

	// Let the command start, then cancel the context.
	time.Sleep(300 * time.Millisecond)
	cancel()

	select {
	case <-done:
		// Returned — cancellation is no longer blocked by the grandchild.
	case <-time.After(45 * time.Second):
		t.Fatal("ExecuteWithHandlers still blocked 45s after ctx cancel with a grandchild holding the pipe write end")
	}
}

// TestExecutor_CancelReturnsStreamingStdoutWithGrandchildHoldingPipe locks
// the streaming stdout branch specifically: with a non-nil stdoutHandler the
// reader runs the chunked Read+callback loop (not io.Copy). A grandchild
// holding the pipe write end must still not block ExecuteWithHandlers after
// ctx cancellation.
func TestExecutor_CancelReturnsStreamingStdoutWithGrandchildHoldingPipe(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not installed")
	}

	executor := NewExecutor("sh", time.Minute)

	// Grandchild keeps the stdout pipe write end open forever; the shell
	// itself exits immediately.
	args := []string{"-c", `sleep 30 & echo started; exit 0`}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan ExecResult, 1)
	go func() {
		var streamed int
		done <- executor.ExecuteWithHandlers(ctx, args,
			func(chunk []byte) { streamed += len(chunk) },
			nil)
	}()

	time.Sleep(300 * time.Millisecond)
	cancel()

	select {
	case <-done:
		// Returned — the streaming reader did not block on the grandchild.
	case <-time.After(45 * time.Second):
		t.Fatal("ExecuteWithHandlers (streaming stdout) still blocked 45s after ctx cancel")
	}
}

// TestExecutor_ProcessGroupKill verifies the whole process tree dies on ctx
// cancellation: the spawned sleep must not outlive the cancelled command.
func TestExecutor_ProcessGroupKill(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not installed")
	}

	executor := NewExecutor("sh", time.Minute)

	// Write the grandchild PID to a file so the test can check it later.
	pidFile := filepath.Join(t.TempDir(), "pid")
	args := []string{"-c", fmt.Sprintf(`sleep 30 & echo $! > %s; echo started`, pidFile)}

	ctx, cancel := context.WithCancel(context.Background())
	cmdDone := make(chan struct{})
	go func() {
		defer close(cmdDone)
		executor.ExecuteWithStderrHandler(ctx, args, nil)
	}()

	time.Sleep(500 * time.Millisecond)
	cancel()
	select {
	case <-cmdDone:
	case <-time.After(45 * time.Second):
		t.Fatal("command did not return after cancel")
	}

	raw, err := os.ReadFile(pidFile)
	if err != nil || len(raw) == 0 {
		t.Skipf("could not read grandchild pid: %v", err)
	}
	var pid int
	if _, err := fmt.Sscanf(string(raw), "%d", &pid); err != nil || pid <= 0 {
		t.Skipf("bad pid file content %q", raw)
	}

	// Give the kill a moment to land, then verify the grandchild is gone.
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := syscall.Kill(pid, 0)
		if err == syscall.ESRCH {
			return // gone — process group kill worked
		}
		if time.Now().After(deadline) {
			t.Fatalf("grandchild pid %d still alive 5s after cancellation: %v", pid, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// TestProcessJob_PanicReportedWorkerSurvives verifies acceptance criterion 2:
// a panic inside processJob is recovered, reported as WORKER_CRASH, and the
// worker keeps processing subsequent jobs.
func TestProcessJob_PanicReportedWorkerSurvives(t *testing.T) {
	var crash atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/jobs/panic-job" && r.Method == http.MethodPatch {
			crash.Store(true)
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	w, err := New(Config{ServerURL: srv.URL})
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}

	job := protocol.JobInfo{
		ID:         "panic-job",
		InputFiles: []string{"in"},
		Args:       []string{"-i", "<INPUT_FILE>", "out.mp4"},
	}

	// Inject a deterministic panic into the middle of processJob: the cache
	// Check call panics before any ffmpeg work starts.
	w.cache = &panickyCache{}

	done := make(chan struct{})
	go func() {
		defer close(done)
		w.processJob(context.Background(), job, func() {}, false)
	}()

	select {
	case <-done:
		// processJob returned — panic did not take down the goroutine/process.
	case <-time.After(10 * time.Second):
		t.Fatal("processJob did not return after injected panic")
	}

	if !crash.Load() {
		t.Fatal("panic was not reported to the server as WORKER_CRASH")
	}

	w.mu.Lock()
	_, stillActive := w.activeJobs[job.ID]
	w.mu.Unlock()
	if stillActive {
		t.Error("job still registered in activeJobs after panic recovery")
	}
}

// panickyCache always panics, simulating an unexpected internal failure in
// the middle of processJob.
type panickyCache struct {
	*Cache
}

func (c *panickyCache) Check(key string) (string, bool) {
	panic("injected test panic")
}

// TestStopRejectsNewJobsAndWaitsForInFlight verifies acceptance criterion for
// Stop: after Stop(), the poll loop stops accepting jobs and Stop waits for
// in-flight jobs to finish before returning.
func TestStopRejectsNewJobsAndWaitsForInFlight(t *testing.T) {
	var mu sync.Mutex
	pulled := 0
	blockJob := make(chan struct{})
	jobStarted := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v1/workers/register":
			_, _ = w.Write([]byte(`{"worker_id":"stop-test-worker"}`))
		case r.URL.Path == "/api/v1/workers/stop-test-worker/jobs":
			mu.Lock()
			pulled++
			n := pulled
			mu.Unlock()
			if n == 1 {
				// First pull hands out exactly one long-running job whose
				// status update blocks until the test releases it.
				_, _ = w.Write([]byte(`{"jobs":[{"id":"long-job","input_files":[],"args":["-f","lavfi","-i","testsrc=duration=0.2","-f","null","-"]}]}`))
				return
			}
			_, _ = w.Write([]byte(`{"jobs":[]}`))
		case r.URL.Path == "/api/v1/jobs/long-job":
			jobStarted <- struct{}{}
			<-blockJob // hold the job's final status update until released
			_, _ = w.Write([]byte(`{}`))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()

	w, err := New(Config{
		ServerURL:         srv.URL,
		PollInterval:      20 * time.Millisecond,
		HeartbeatInterval: time.Hour,
	})
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	if err := w.Register(protocol.WorkerCapabilities{}); err != nil {
		t.Fatalf("Register() failed: %v", err)
	}

	ctx, cancelCtx := context.WithCancel(context.Background())
	defer cancelCtx()
	started := make(chan struct{})
	go func() {
		close(started)
		w.Start(ctx)
	}()
	<-started

	select {
	case <-jobStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("job was never handed out")
	}

	// Stop must block until the in-flight job finishes its reporting.
	stopDone := make(chan struct{})
	go func() {
		w.Stop()
		close(stopDone)
	}()

	select {
	case <-stopDone:
		t.Fatal("Stop returned while the in-flight job was still running")
	case <-time.After(200 * time.Millisecond):
		// Expected: Stop is waiting for the job.
	}

	close(blockJob) // let the job finish
	select {
	case <-stopDone:
	case <-time.After(10 * time.Second):
		t.Fatal("Stop did not return after the in-flight job completed")
	}

	// After Stop, Start's loop has exited: pulling again must not happen.
	mu.Lock()
	finalPulls := pulled
	mu.Unlock()
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	if pulled != finalPulls {
		t.Errorf("poll loop accepted new work after Stop: pulls went %d -> %d", finalPulls, pulled)
	}
	mu.Unlock()
}

// TestConcurrentJobs_AutoHWIsolation runs two jobs with different auto_hw
// flags concurrently and verifies the rewrite decisions stay isolated
// (acceptance criterion 3: no data race, no cross-job flag bleed).
func TestConcurrentJobs_AutoHWIsolation(t *testing.T) {
	adapter := NewRewriteAdapter()
	caps := &protocol.WorkerCapabilities{
		VideoEncoders: []protocol.EncoderInfo{
			{Name: "h264_nvenc", Type: "video", IsHW: true},
			{Name: "libx264", Type: "video", IsHW: false},
		},
		GPUDevices: []protocol.GPUDeviceInfo{
			{Type: "nvenc", Vendor: "NVIDIA", Accessible: true},
		},
	}
	adapter.SetHardwareCapabilities(caps)

	args := []string{"-i", "input.mp4", "-c:v", "libx264", "output.mp4"}

	var wg sync.WaitGroup
	errCh := make(chan error, 2)
	for _, autoHW := range []bool{true, false} {
		wg.Add(1)
		go func(autoHW bool) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				_, result, err := adapter.RewriteArgs(context.Background(), args, autoHW)
				if err != nil {
					errCh <- err
					return
				}
				if !autoHW && result.TargetEncoder == "h264_nvenc" {
					errCh <- fmt.Errorf("auto-hw bleed: job with auto_hw=false was upgraded to %s", result.TargetEncoder)
					return
				}
			}
		}(autoHW)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}
}

// TestConcurrentReregisterSingleflight hammers reregister() from many
// goroutines at once; only ONE registration request may reach the server.
func TestConcurrentReregisterSingleflight(t *testing.T) {
	var mu sync.Mutex
	registrations := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/workers/register" {
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		registrations++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"worker_id":"singleflight-worker"}`))
	}))
	defer srv.Close()

	w, err := New(Config{ServerURL: srv.URL})
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}

	const n = 20
	var wg sync.WaitGroup
	results := make([]bool, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = w.reregister()
		}(i)
	}
	wg.Wait()

	mu.Lock()
	total := registrations
	mu.Unlock()
	if total != 1 {
		t.Errorf("server received %d registration requests under concurrent reregister; want exactly 1", total)
	}
	for i, ok := range results {
		if !ok {
			t.Fatalf("reregister goroutine %d returned false", i)
		}
	}
}
