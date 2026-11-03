package scheduler

import (
	"testing"
	"time"

	"github.com/tsic404/rffmpeg/pkg/protocol"
	"github.com/tsic404/rffmpeg/pkg/server/db"
	"github.com/tsic404/rffmpeg/pkg/server/ratelimit"
)

// A job failed by the scheduler (timeout retries exhausted) reaches a terminal
// state without any HTTP handler, and its client must not stay 429'd: the
// terminal hook releases the quota slot.
func TestSchedulerTimeoutRetryExhaustedReleasesQuota(t *testing.T) {
	database, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("Failed to create database: %v", err)
	}
	defer database.Close()

	counter := ratelimit.NewInMemoryCounter()
	database.SetTerminalHook(func(jobID string) { counter.DecrementByJob(jobID) })

	if _, err := database.CreateWorker("w1", "worker", protocol.WorkerCapabilities{
		Encoders: []string{"libx264"}, FFmpegVersion: "6.0", MaxConcurrent: 1,
	}); err != nil {
		t.Fatalf("Failed to create worker: %v", err)
	}

	job, err := database.CreateJob(`["in.mp4"]`, `["-i","in.mp4"]`, "out.mp4", false)
	if err != nil {
		t.Fatalf("Failed to create job: %v", err)
	}
	if err := database.AssignJobToWorker(job.ID, "w1"); err != nil {
		t.Fatalf("Failed to assign job: %v", err)
	}
	if err := database.UpdateJobStatusWithFailure(job.ID, protocol.JobStatusRunning, nil, nil, nil, nil); err != nil {
		t.Fatalf("Failed to mark job running: %v", err)
	}
	if _, err := database.GetDB().Exec(
		`UPDATE jobs SET started_at = ? WHERE id = ?`, time.Now().Add(-time.Hour), job.ID,
	); err != nil {
		t.Fatalf("Failed to backdate started_at: %v", err)
	}

	if _, allowed := counter.TryIncrement("client-1", 1); !allowed {
		t.Fatalf("Failed to reserve a quota slot")
	}
	counter.RegisterJob("client-1", job.ID)

	s := New(database, Config{
		ScheduleInterval:     time.Hour,
		TimeoutCheckInterval: time.Hour,
		JobTimeout:           time.Minute,
		MaxJobsPerWorker:     1,
		MaxTimeoutRetries:    0, // fail on the first timeout: budget exhausted
	})
	s.checkTimeouts()

	failed, err := database.GetJob(job.ID)
	if err != nil {
		t.Fatalf("Failed to get job: %v", err)
	}
	if failed.Status != protocol.JobStatusFailed {
		t.Fatalf("job status = %s, want failed", failed.Status)
	}
	if failed.FailureType != string(protocol.FailureTimeout) {
		t.Fatalf("failure type = %q, want %q", failed.FailureType, protocol.FailureTimeout)
	}
	if got := counter.Count("client-1"); got != 0 {
		t.Errorf("quota after timeout terminal = %d, want 0", got)
	}
}
