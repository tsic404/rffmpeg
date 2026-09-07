package scheduler

import (
	"testing"
	"time"

	"github.com/tsic404/rffmpeg/pkg/protocol"
	"github.com/tsic404/rffmpeg/pkg/server/db"
	"github.com/tsic404/rffmpeg/pkg/server/ratelimit"
)

// TSI-2562 review fix: the starvation sweep fails jobs with a bulk UPDATE that
// bypasses the per-job terminal-status writers. waitForProbeTerminal has no
// periodic DB poll to fall back on, so the sweep must wake the notifier for
// each failed job — otherwise a probe waiter idles to the 2-minute budget.
func TestSchedulerNoWorkerStarvationWakesTerminalNotifier(t *testing.T) {
	database, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("Failed to create database: %v", err)
	}
	defer database.Close()

	s := New(database, Config{
		ScheduleInterval:     time.Hour,
		TimeoutCheckInterval: time.Hour,
		MaxJobsPerWorker:     1,
		NoWorkerJobTimeout:   1 * time.Minute,
	})

	counter := ratelimit.NewInMemoryCounter()
	s.SetRateLimiter(counter)

	// A worker must exist but be offline for the sweep to fire.
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
	counter.RegisterJob("client-1", job.ID)
	if _, err := database.GetDB().Exec(`
		UPDATE jobs SET created_at = ? WHERE id = ?
	`, time.Now().Add(-5*time.Minute), job.ID); err != nil {
		t.Fatalf("Failed to backdate created_at: %v", err)
	}

	notifyCh, cancel := database.JobNotifier().Subscribe(job.ID)
	defer cancel()

	s.checkNoWorkerStarvation()

	select {
	case <-notifyCh:
	case <-time.After(2 * time.Second):
		t.Fatalf("starvation sweep failed to wake terminal notifier")
	}
}
