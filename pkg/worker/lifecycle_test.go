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

	"github.com/tsic404/rffmpeg/pkg/protocol"
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
	jobsCompleted := w.jobsCompleted
	totalJobsCompleted := w.totalJobsCompleted
	w.mu.Unlock()
	if stillActive {
		t.Error("job still registered in activeJobs after panic recovery")
	}
	// TSI-2666: a panic-recovered job must not advance the completion
	// counters — only successful jobs may inflate completed_jobs.
	if jobsCompleted != 0 || totalJobsCompleted != 0 {
		t.Errorf("completion counters advanced after panic: jobsCompleted=%d totalJobsCompleted=%d, want 0/0",
			jobsCompleted, totalJobsCompleted)
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

// TestHeartbeatFlowsWhileJobBlocked verifies the TSI-2492 regression: the
// heartbeat must keep flowing while the main poll loop is blocked. In the old
// single-loop design, a synchronous PullJobs blocked the loop and starved the
// heartbeat ticker; the server then marked the worker offline mid-job. With
// the heartbeat on its own goroutine, it keeps ticking while PullJobs (and a
// job's terminal status update) are both held open.
func TestHeartbeatFlowsWhileJobBlocked(t *testing.T) {
	var mu sync.Mutex
	heartbeats := 0
	pulls := 0
	blockJob := make(chan struct{})
	releasePull := make(chan struct{})
	// releaseHandlers unblocks the two httptest handlers exactly once. It must
	// run on the t.Fatal failure paths as well as the success path: an
	// unclosed blockJob/releasePull leaves handler goroutines stuck in
	// <-blockJob/<-releasePull, and srv.Close (deferred) then blocks waiting
	// for those outstanding requests, turning a fast test failure into a
	// go test -timeout hang.
	var releaseOnce sync.Once
	releaseHandlers := func() {
		releaseOnce.Do(func() {
			close(releasePull)
			close(blockJob)
		})
	}
	secondPullStarted := make(chan struct{}, 1)
	jobStarted := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/workers/heartbeat":
			mu.Lock()
			heartbeats++
			mu.Unlock()
			_, _ = w.Write([]byte(`{"message":"ok","cancelled_jobs":[]}`))
		case r.URL.Path == "/api/v1/workers/register":
			_, _ = w.Write([]byte(`{"worker_id":"hb-test-worker"}`))
		case r.URL.Path == "/api/v1/workers/hb-test-worker/jobs":
			mu.Lock()
			pulls++
			n := pulls
			mu.Unlock()
			switch n {
			case 1:
				// First pull hands out one job whose status update blocks.
				_, _ = w.Write([]byte(`{"jobs":[{"id":"block-job","input_files":[],"args":["-f","lavfi","-i","testsrc=duration=0.2","-f","null","-"]}]}`))
			case 2:
				// Second pull blocks the poll loop itself until released.
				secondPullStarted <- struct{}{}
				<-releasePull
				_, _ = w.Write([]byte(`{"jobs":[]}`))
			default:
				_, _ = w.Write([]byte(`{"jobs":[]}`))
			}
		case r.URL.Path == "/api/v1/jobs/block-job":
			jobStarted <- struct{}{}
			<-blockJob // hold the job's final status update until released
			_, _ = w.Write([]byte(`{}`))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()
	// Registered after srv.Close: defer is LIFO, so on a t.Fatal failure path
	// releaseHandlers runs FIRST and unblocks the handlers before srv.Close
	// waits for outstanding requests. On the success path the explicit call
	// below releases them early; the once-guard makes this deferred call a
	// no-op there.
	defer releaseHandlers()

	w, err := New(Config{
		ServerURL:         srv.URL,
		PollInterval:      20 * time.Millisecond,
		HeartbeatInterval: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	if err := w.Register(protocol.WorkerCapabilities{}); err != nil {
		t.Fatalf("Register() failed: %v", err)
	}

	ctx, cancelCtx := context.WithCancel(context.Background())
	defer cancelCtx()
	go w.Start(ctx)

	select {
	case <-jobStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("job was never handed out")
	}

	// Wait until the poll loop is blocked inside the second PullJobs call.
	select {
	case <-secondPullStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("second poll never started")
	}

	// Main loop is blocked in PullJobs and the job goroutine is blocked in
	// its status update. The heartbeat goroutine must still fire repeatedly.
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		n := heartbeats
		mu.Unlock()
		if n >= 3 {
			break
		}
		if time.Now().After(deadline) {
			mu.Lock()
			n = heartbeats
			mu.Unlock()
			t.Fatalf("heartbeats stalled at %d while loop was blocked; want >= 3", n)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Success path: release the handlers before w.Stop() waits on jobsWG, so
	// the blocked job's status-update request can complete and the deferred
	// srv.Close() below can drain. releaseHandlers is once-guarded, so the
	// later deferred call is a no-op on this path.
	releaseHandlers()
	w.Stop()
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
		first := registrations == 1
		mu.Unlock()
		if first {
			// Keep the winning registration in flight long enough for every
			// other caller to reach reregister()'s singleflight window and
			// coalesce onto it. A fast httptest server can otherwise answer
			// before the later goroutines are scheduled, so each of them sees
			// the registration as already finished and issues its own request
			// ("want exactly 1" flakes).
			time.Sleep(100 * time.Millisecond)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"worker_id":"singleflight-worker"}`))
	}))
	defer srv.Close()

	w, err := New(Config{ServerURL: srv.URL})
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}

	const n = 20
	start := make(chan struct{})
	var wg sync.WaitGroup
	results := make([]bool, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i] = w.reregister()
		}(i)
	}
	close(start)
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

// TestExecutor_PdeathsigKillsChildOnParentDeath verifies that the executor
// sets PR_SET_PDEATHSIG so ffmpeg is reaped by the kernel when the parent
// worker dies (SIGKILL/crash), preventing orphaned ffmpeg processes from
// outliving the worker and holding GPU/encoder resources (TSI-2476).
//
// The executor's ffmpeg subprocess is the DIRECT child of the worker
// process. Pdeathsig kills that direct child the moment the parent exits.
// We fork a helper Go process that starts the executor on `sh -c 'echo $$;
// exec sleep 30'` (exec replaces sh with sleep, so sleep keeps sh's PID =
// the direct child), then calls os.Exit(0). The kernel's Pdeathsig should
// kill the direct child; the parent test verifies it is gone within 5s.
func TestExecutor_PdeathsigKillsChildOnParentDeath(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping pdeathsig test in short mode")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not installed")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not installed")
	}

	tmpDir := t.TempDir()
	pidFile := filepath.Join(tmpDir, "child.pid")
	helperSrc := filepath.Join(tmpDir, "pdeathsig_helper.go")

	// Helper: starts an executor on `sh -c 'echo $$ > pidFile; exec
	// sleep 30'`, lets it run briefly, then exits via os.Exit(0). The
	// exec makes sleep the direct child (same PID as sh), so Pdeathsig
	// targets it. The helper exit triggers the kernel Pdeathsig.
	helper := fmt.Sprintf(`package main

import (
	"context"
	"os"
	"time"

	"github.com/tsic404/rffmpeg/pkg/worker"
)

func main() {
	ex := worker.NewExecutor("sh", time.Minute)
	args := []string{"-c", "echo $$ > %s; exec sleep 30"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		time.Sleep(500 * time.Millisecond)
		os.Exit(0)
	}()
	ex.ExecuteWithStderrHandler(ctx, args, nil)
}
`, pidFile)
	if err := os.WriteFile(helperSrc, []byte(helper), 0644); err != nil {
		t.Fatalf("write helper: %v", err)
	}

	helperCmd := exec.Command("go", "run", helperSrc)
	helperCmd.Dir = "."
	helperCmd.Stdout = os.Stderr
	helperCmd.Stderr = os.Stderr
	if err := helperCmd.Run(); err != nil {
		t.Fatalf("helper run failed: %v", err)
	}

	raw, err := os.ReadFile(pidFile)
	if err != nil || len(raw) == 0 {
		t.Skipf("could not read child pid file: %v", err)
	}
	var pid int
	if _, err := fmt.Sscanf(string(raw), "%d", &pid); err != nil || pid <= 0 {
		t.Skipf("bad pid file content %q", raw)
	}

	// Pdeathsig should have killed the direct child on helper exit.
	// Verify it is gone within 5s.
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := syscall.Kill(pid, 0)
		if err == syscall.ESRCH {
			return // gone — Pdeathsig worked
		}
		if time.Now().After(deadline) {
			t.Fatalf("child pid %d still alive 5s after parent exit: %v — Pdeathsig did not kill the child", pid, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
