package scheduler

// Tests for the C-61 dispatch freshness rule and the stuck-job sweep: a job
// must never be handed to a worker whose heartbeat is stale, and a job that
// cannot make progress may not sit in queued forever.

import (
	"testing"
	"time"

	"github.com/tsic404/rffmpeg/pkg/protocol"
	"github.com/tsic404/rffmpeg/pkg/server/db"
	"github.com/tsic404/rffmpeg/pkg/server/ratelimit"
)

func newIdleWorker(t *testing.T, database *db.Database, id string) *db.Worker {
	t.Helper()
	worker, err := database.CreateWorker(id, id, protocol.WorkerCapabilities{
		Encoders:      []string{"libx264"},
		FFmpegVersion: "5.0",
		MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatalf("CreateWorker: %v", err)
	}
	return worker
}

// TestScheduleSkipsStaleHeartbeatWorker pins C-61: dispatch candidates must
// have a fresh heartbeat, not merely status=idle. A worker whose heartbeat went
// stale but which the health tick has not flipped offline yet would never pick
// the job up, burning a migration retry.
func TestScheduleSkipsStaleHeartbeatWorker(t *testing.T) {
	database, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("db.New: %v", err)
	}
	defer database.Close()

	newIdleWorker(t, database, "worker-1")
	job, err := database.CreateJob(`["in.mp4"]`, `["-c:v","libx264"]`, "out.mp4", false)
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	s := New(database, Config{
		ScheduleInterval:     time.Hour,
		TimeoutCheckInterval: time.Hour,
		MaxJobsPerWorker:     1,
		HeartbeatFreshness:   90 * time.Second,
	})

	if _, err := database.GetDB().Exec(
		`UPDATE workers SET last_heartbeat = ? WHERE id = ?`,
		time.Now().Add(-5*time.Minute), "worker-1"); err != nil {
		t.Fatalf("backdate heartbeat: %v", err)
	}

	s.schedulePendingJobs()

	got, err := database.GetJob(job.ID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if got.Status != protocol.JobStatusPending || got.WorkerID.Valid {
		t.Fatalf("job dispatched to a stale-heartbeat worker: status=%s worker=%v", got.Status, got.WorkerID)
	}

	// A fresh heartbeat puts it back in the candidate set.
	if err := database.UpdateWorkerHeartbeat("worker-1", protocol.WorkerStatusIdle); err != nil {
		t.Fatalf("UpdateWorkerHeartbeat: %v", err)
	}
	s.schedulePendingJobs()

	got, err = database.GetJob(job.ID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if got.Status != protocol.JobStatusQueued || !got.WorkerID.Valid || got.WorkerID.String != "worker-1" {
		t.Fatalf("live worker did not receive the job: status=%s worker=%v", got.Status, got.WorkerID)
	}
}

// TestSweepStuckQueuedJobFailsItTerminally covers H7's missing fallback: a job
// stuck in queued (its worker never picked it up) must reach a terminal state,
// release the client's quota and notify listeners instead of hanging the CLI
// forever.
func TestSweepStuckQueuedJobFailsItTerminally(t *testing.T) {
	database, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("db.New: %v", err)
	}
	defer database.Close()

	worker := newIdleWorker(t, database, "worker-1")
	job, err := database.CreateJob(`["in.mp4"]`, `["-c:v","libx264"]`, "out.mp4", false)
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if err := database.AssignJobToWorker(job.ID, worker.ID); err != nil {
		t.Fatalf("AssignJobToWorker: %v", err)
	}

	// Production binds quota release to the database terminal hook: the sweep
	// must go through NotifyTerminal rather than decrementing on its own.
	counter := ratelimit.NewInMemoryCounter()
	database.SetTerminalHook(func(jobID string) { counter.DecrementByJob(jobID) })
	if _, ok := counter.TryIncrement("client-1", 10); !ok {
		t.Fatal("TryIncrement failed")
	}
	counter.RegisterJob("client-1", job.ID)

	broadcaster := &fakeBroadcaster{}
	s := New(database, Config{
		ScheduleInterval:     time.Hour,
		TimeoutCheckInterval: time.Hour,
		MaxJobsPerWorker:     1,
		StuckJobTimeout:      5 * time.Minute,
	})
	s.SetJobNotifier(broadcaster)

	// Fresh queued job: not stuck.
	s.sweepStuckJobs()
	got, _ := database.GetJob(job.ID)
	if got.Status != protocol.JobStatusQueued {
		t.Fatalf("fresh queued job swept too early: %s", got.Status)
	}

	// The worker never picks it up.
	if _, err := database.GetDB().Exec(
		`UPDATE jobs SET updated_at = ? WHERE id = ?`,
		time.Now().Add(-10*time.Minute), job.ID); err != nil {
		t.Fatalf("backdate updated_at: %v", err)
	}
	s.sweepStuckJobs()

	got, _ = database.GetJob(job.ID)
	if got.Status != protocol.JobStatusFailed {
		t.Fatalf("stuck queued job status = %s, want failed", got.Status)
	}
	if got.FailureType != string(protocol.FailureInfra) {
		t.Fatalf("failure_type = %q, want INFRA", got.FailureType)
	}
	if got.WorkerID.Valid {
		t.Errorf("failed job still owned by %s", got.WorkerID.String)
	}
	if n := counter.Count("client-1"); n != 0 {
		t.Errorf("quota after sweep = %d, want 0", n)
	}
	if len(broadcaster.statuses) != 1 || broadcaster.statuses[0].jobID != job.ID ||
		broadcaster.statuses[0].status != protocol.JobStatusFailed {
		t.Errorf("broadcasts = %+v, want one failed status for %s", broadcaster.statuses, job.ID)
	}
	if len(broadcaster.released) != 1 || broadcaster.released[0] != job.ID {
		t.Errorf("released = %v, want [%s]", broadcaster.released, job.ID)
	}
}
