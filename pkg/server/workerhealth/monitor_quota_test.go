package workerhealth

import (
	"testing"
	"time"

	"github.com/tsic404/rffmpeg/pkg/protocol"
	"github.com/tsic404/rffmpeg/pkg/server/migration"
	"github.com/tsic404/rffmpeg/pkg/server/ratelimit"
)

// A job failed by the health monitor (migration retry budget exhausted) is
// terminal without any HTTP handler; the terminal hook must still release the
// client's quota slot or every later submit from that client is 429'd.
func TestMonitorMigrationRetryExhaustedReleasesQuota(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	counter := ratelimit.NewInMemoryCounter()
	database.SetTerminalHook(func(jobID string) { counter.DecrementByJob(jobID) })

	if _, err := database.CreateWorker("worker-A", "worker-alpha", protocol.WorkerCapabilities{
		Encoders: []string{"libx264"}, FFmpegVersion: "6.0",
	}); err != nil {
		t.Fatalf("Failed to create worker: %v", err)
	}

	job, err := database.CreateJob(`["in.mkv"]`, `["-c:v","libx264"]`, "out.mkv", false)
	if err != nil {
		t.Fatalf("Failed to create job: %v", err)
	}
	if err := database.AssignJobToWorker(job.ID, "worker-A"); err != nil {
		t.Fatalf("Failed to assign job: %v", err)
	}
	if err := database.UpdateJobStatusWithFailure(job.ID, protocol.JobStatusRunning, nil, nil, nil, nil); err != nil {
		t.Fatalf("Failed to mark job running: %v", err)
	}
	for i := range 3 {
		if _, err := database.CreateMigrationEvent(
			"old-w"+string(rune('0'+i)), "old-worker",
			string(migration.ReasonHeartbeatTimeout), i, []string{job.ID}, 1,
		); err != nil {
			t.Fatalf("Failed to create migration event %d: %v", i, err)
		}
	}

	if _, allowed := counter.TryIncrement("client-1", 1); !allowed {
		t.Fatalf("Failed to reserve a quota slot")
	}
	counter.RegisterJob("client-1", job.ID)

	if _, err := database.GetDB().Exec(
		`UPDATE workers SET last_heartbeat = ? WHERE id = ?`,
		time.Now().Add(-60*time.Second), "worker-A",
	); err != nil {
		t.Fatalf("Failed to stale the heartbeat: %v", err)
	}

	monitor := New(database, Config{
		HeartbeatTimeout:    30 * time.Second,
		OfflineThreshold:    10 * time.Minute,
		HealthCheckInterval: time.Second,
		MaxRetryCount:       3,
	})
	monitor.SetScheduler(&mockScheduler{})
	monitor.checkWorkers()

	failed, err := database.GetJob(job.ID)
	if err != nil {
		t.Fatalf("Failed to get job: %v", err)
	}
	if failed.Status != protocol.JobStatusFailed {
		t.Fatalf("job status = %s, want failed", failed.Status)
	}
	if got := counter.Count("client-1"); got != 0 {
		t.Errorf("quota after migration-failure terminal = %d, want 0", got)
	}
}
