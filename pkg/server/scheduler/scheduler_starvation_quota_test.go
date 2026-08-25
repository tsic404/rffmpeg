package scheduler

import (
	"testing"
	"time"

	"github.com/tsix404/rffmpeg/pkg/protocol"
	"github.com/tsix404/rffmpeg/pkg/server/db"
	"github.com/tsix404/rffmpeg/pkg/server/ratelimit"
)

// fakeBroadcaster records BroadcastStatus calls.
type fakeBroadcaster struct {
	statuses []broadcastCall
}

type broadcastCall struct {
	jobID  string
	status protocol.JobStatus
}

func (f *fakeBroadcaster) BroadcastStatus(jobID string, status protocol.JobStatus, exitCode int, err string) error {
	f.statuses = append(f.statuses, broadcastCall{jobID: jobID, status: status})
	return nil
}

// TSI-2365 review fix: the starvation sweep fails jobs in the DB layer,
// bypassing the HTTP handler that normally releases rate-limit quota and
// broadcasts the terminal status. Both must happen here or clients stay
// permanently 429'd and CLI listeners never learn the job died.
func TestSchedulerNoWorkerStarvationReleasesQuotaAndBroadcasts(t *testing.T) {
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
	broadcaster := &fakeBroadcaster{}
	s.SetRateLimiter(counter)
	s.SetJobNotifier(broadcaster)

	// Register two starved jobs against client-1 (limit 2 so both fit).
	for range 2 {
		counter.TryIncrement("client-1", 10)
	}
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

	var jobIDs []string
	for range 2 {
		job, err := database.CreateJob(`["file1.mp4"]`, `["-i", "input.mp4"]`, "output.mp4", false)
		if err != nil {
			t.Fatalf("Failed to create job: %v", err)
		}
		jobIDs = append(jobIDs, job.ID)
		counter.RegisterJob("client-1", job.ID)
		if _, err := database.GetDB().Exec(`
			UPDATE jobs SET created_at = ? WHERE id = ?
		`, time.Now().Add(-5*time.Minute), job.ID); err != nil {
			t.Fatalf("Failed to backdate created_at: %v", err)
		}
	}

	if got := counter.Count("client-1"); got != 2 {
		t.Fatalf("precondition: count = %d, want 2", got)
	}

	s.checkNoWorkerStarvation()

	if got := counter.Count("client-1"); got != 0 {
		t.Errorf("quota after sweep = %d, want 0 (both slots released)", got)
	}
	if len(broadcaster.statuses) != 2 {
		t.Fatalf("broadcasts = %d, want 2", len(broadcaster.statuses))
	}
	for i, id := range jobIDs {
		if broadcaster.statuses[i].jobID != id || broadcaster.statuses[i].status != protocol.JobStatusFailed {
			t.Errorf("broadcast[%d] = (%s, %s), want (%s, failed)",
				i, broadcaster.statuses[i].jobID, broadcaster.statuses[i].status, id)
		}
	}
}
