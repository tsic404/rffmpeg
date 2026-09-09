package scheduler

import (
	"testing"
	"time"

	"github.com/tsic404/rffmpeg/pkg/protocol"
	"github.com/tsic404/rffmpeg/pkg/server/db"
)

// TestSchedulerNoWorkerStarvationThroughLoop codifies the QA scenario 4
// "无可用 Worker" acceptance (TSI-2936): a running server with no live worker
// must fail a pending job through the starvation sweep once it has waited past
// NoWorkerJobTimeout, classifying it NO_WORKER_AVAILABLE. The QA run observes
// this after 300s of full offline against the default 2m timeout; the test
// reproduces the same sweep at accelerated time by shrinking
// NoWorkerJobTimeout and TimeoutCheckInterval.
//
// Unlike TestSchedulerNoWorkerStarvation and its companions, which invoke
// checkNoWorkerStarvation directly with a backdated created_at, this test
// drives the sweep through the real run() ticker: Start launches the loop and
// the timeout ticker calls checkNoWorkerStarvation, so the end-to-end wiring
// (loop -> sweep -> bulk failure -> terminal notify) is exercised rather than
// the method in isolation.
func TestSchedulerNoWorkerStarvationThroughLoop(t *testing.T) {
	database, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("Failed to create database: %v", err)
	}
	defer database.Close()

	s := New(database, Config{
		ScheduleInterval:     time.Hour, // keep the scheduling tick out of the way
		TimeoutCheckInterval: 25 * time.Millisecond,
		MaxJobsPerWorker:     1,
		NoWorkerJobTimeout:   200 * time.Millisecond,
	})

	// A worker exists but is offline, so the cluster has no schedulable worker.
	if _, err := database.CreateWorker("worker-1", "test-worker", protocol.WorkerCapabilities{
		Encoders:      []string{"libx264"},
		FFmpegVersion: "5.0",
		MaxConcurrent: 1,
	}); err != nil {
		t.Fatalf("Failed to create worker: %v", err)
	}
	if err := database.UpdateWorkerStatus("worker-1", protocol.WorkerStatusOffline); err != nil {
		t.Fatalf("Failed to mark worker offline: %v", err)
	}

	job, err := database.CreateJob(`["file1.mp4"]`, `["-i", "input.mp4"]`, "output.mp4", false)
	if err != nil {
		t.Fatalf("Failed to create job: %v", err)
	}

	notifyCh, cancel := database.JobNotifier().Subscribe(job.ID)
	defer cancel()

	s.Start()
	defer s.Stop()

	select {
	case <-notifyCh:
	case <-time.After(5 * time.Second):
		t.Fatalf("starvation sweep did not fire through the scheduler loop")
	}

	updatedJob, err := database.GetJob(job.ID)
	if err != nil {
		t.Fatalf("Failed to get job: %v", err)
	}
	if updatedJob.Status != protocol.JobStatusFailed {
		t.Errorf("Expected status failed, got %s", updatedJob.Status)
	}
	if updatedJob.FailureType != string(protocol.FailureNoWorkerAvailable) {
		t.Errorf("Expected failure_type %q, got %q",
			protocol.FailureNoWorkerAvailable, updatedJob.FailureType)
	}
	if updatedJob.Error.String == "" {
		t.Error("Expected non-empty error message")
	}
}
