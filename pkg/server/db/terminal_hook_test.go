package db

import (
	"sync"
	"testing"

	"github.com/tsic404/rffmpeg/pkg/protocol"
	"github.com/tsic404/rffmpeg/pkg/server/ratelimit"
)

// The quota slot of a submitted job is released through the terminal hook, so
// every writer that can end a job must fire it — including the scheduler and
// worker-health failure paths that never pass through an HTTP handler.
func TestTerminalHookFiresOnEveryTerminalWriter(t *testing.T) {
	database, err := New(":memory:")
	if err != nil {
		t.Fatalf("Failed to create database: %v", err)
	}
	defer database.Close()

	var (
		mu    sync.Mutex
		fired = make(map[string]int)
	)
	database.SetTerminalHook(func(jobID string) {
		mu.Lock()
		fired[jobID]++
		mu.Unlock()
	})

	if _, err := database.CreateWorker("w1", "worker", protocol.WorkerCapabilities{
		Encoders:      []string{"libx264"},
		FFmpegVersion: "6.0",
		MaxConcurrent: 4,
	}); err != nil {
		t.Fatalf("Failed to create worker: %v", err)
	}

	newJob := func() *Job {
		t.Helper()
		job, err := database.CreateJob(`["in.mp4"]`, `["-i","in.mp4"]`, "out.mp4", false)
		if err != nil {
			t.Fatalf("Failed to create job: %v", err)
		}
		return job
	}

	running := func(job *Job) {
		t.Helper()
		if err := database.AssignJobToWorker(job.ID, "w1"); err != nil {
			t.Fatalf("Failed to assign job: %v", err)
		}
		if err := database.UpdateJobStatusWithFailure(job.ID, protocol.JobStatusRunning, nil, nil, nil, nil); err != nil {
			t.Fatalf("Failed to mark job running: %v", err)
		}
	}

	// A non-terminal write must not release anything: the job still runs.
	active := newJob()
	running(active)
	mu.Lock()
	if fired[active.ID] != 0 {
		t.Errorf("hook fired for a running job")
	}
	mu.Unlock()

	completed := newJob()
	running(completed)
	exitCode := 0
	if err := database.UpdateJobTerminalStatusWithOwnerAndCache(
		completed.ID, "w1", protocol.JobStatusCompleted, &exitCode, nil, nil, nil, false,
	); err != nil {
		t.Fatalf("Failed to complete job: %v", err)
	}

	cancelled := newJob()
	if err := database.CancelJob(cancelled.ID); err != nil {
		t.Fatalf("Failed to cancel job: %v", err)
	}

	failed := newJob()
	running(failed)
	if err := database.FailJob(failed.ID, "boom", string(protocol.FailureWorkerCrash)); err != nil {
		t.Fatalf("Failed to fail job: %v", err)
	}

	encoderUnavailable, err := database.CreateFailedJob(
		`["in.mp4"]`, `["-i","in.mp4"]`, "out.mp4", false, false, nil, "",
		string(protocol.FailureEncoderUnavailable), "no encoder",
	)
	if err != nil {
		t.Fatalf("Failed to create terminal job: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	for name, jobID := range map[string]string{
		"completed":         completed.ID,
		"cancelled":         cancelled.ID,
		"failed":            failed.ID,
		"terminal at birth": encoderUnavailable.ID,
	} {
		if fired[jobID] != 1 {
			t.Errorf("%s: hook fired %d times, want 1", name, fired[jobID])
		}
	}
}

// A repeated terminal write (worker retry after failure) must not release the
// same job's quota slot twice.
func TestTerminalHookReleasesQuotaOncePerJob(t *testing.T) {
	database, err := New(":memory:")
	if err != nil {
		t.Fatalf("Failed to create database: %v", err)
	}
	defer database.Close()

	counter := ratelimit.NewInMemoryCounter()
	database.SetTerminalHook(func(jobID string) { counter.DecrementByJob(jobID) })

	if _, err := database.CreateWorker("w1", "worker", protocol.WorkerCapabilities{
		Encoders: []string{"libx264"}, FFmpegVersion: "6.0", MaxConcurrent: 4,
	}); err != nil {
		t.Fatalf("Failed to create worker: %v", err)
	}
	job, err := database.CreateJob(`["in.mp4"]`, `["-i","in.mp4"]`, "out.mp4", false)
	if err != nil {
		t.Fatalf("Failed to create job: %v", err)
	}
	if _, allowed := counter.TryIncrement("client-1", 1); !allowed {
		t.Fatalf("Failed to reserve a quota slot")
	}
	counter.RegisterJob("client-1", job.ID)
	if err := database.AssignJobToWorker(job.ID, "w1"); err != nil {
		t.Fatalf("Failed to assign job: %v", err)
	}
	if err := database.UpdateJobStatusWithFailure(job.ID, protocol.JobStatusRunning, nil, nil, nil, nil); err != nil {
		t.Fatalf("Failed to mark job running: %v", err)
	}

	// failed -> failed is allowed for the worker retry-after-failure flow.
	failure := string(protocol.FailureWorkerCrash)
	if err := database.UpdateJobStatusWithFailure(job.ID, protocol.JobStatusFailed, nil, nil, &failure, nil); err != nil {
		t.Fatalf("Failed to fail job: %v", err)
	}
	if err := database.UpdateJobStatusWithFailure(job.ID, protocol.JobStatusFailed, nil, nil, &failure, nil); err != nil {
		t.Fatalf("Failed to repeat failure report: %v", err)
	}

	if got := counter.Count("client-1"); got != 0 {
		t.Errorf("quota after a repeated terminal write = %d, want 0", got)
	}
}
