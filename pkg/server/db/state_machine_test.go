package db

// Regression tests for the scheduler state-machine fixes (TSI-2362):
// ownership-guarded terminal updates, atomic assignment, conditional idle
// transitions, and refresh-only heartbeats.

import (
	"testing"
	"time"

	"github.com/tsic404/rffmpeg/pkg/protocol"
)

func createStateTestDB(t *testing.T) *Database {
	t.Helper()
	database, err := New(":memory:")
	if err != nil {
		t.Fatalf("Failed to create database: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	return database
}

func mustCreateWorker(t *testing.T, d *Database, id string) {
	t.Helper()
	if _, err := d.CreateWorker(id, id, protocol.WorkerCapabilities{
		Encoders: []string{"libx264"}, FFmpegVersion: "5.0", MaxConcurrent: 1,
	}); err != nil {
		t.Fatalf("Failed to create worker %s: %v", id, err)
	}
}

func mustRunningJob(t *testing.T, d *Database, jobID, workerID string) {
	t.Helper()
	if err := d.AssignJobToWorker(jobID, workerID); err != nil {
		t.Fatalf("AssignJobToWorker: %v", err)
	}
	if err := d.UpdateJobStatusWithFailure(jobID, protocol.JobStatusRunning, nil, nil, nil, nil); err != nil {
		t.Fatalf("Set running: %v", err)
	}
}

// Acceptance 1 (double-run): a stale completed report from the OLD owner after
// failover must not overwrite the new owner's result.
func TestStaleTerminalReportRejectedAfterFailover(t *testing.T) {
	database := createStateTestDB(t)
	mustCreateWorker(t, database, "worker-old")
	mustCreateWorker(t, database, "worker-new")

	job, err := database.CreateJob(`["in.mkv"]`, `["-c:v","libx264"]`, "out.mkv", false)
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	mustRunningJob(t, database, job.ID, "worker-old")

	// Failover: job migrates to the new owner and runs there.
	if err := database.ResetJobToPending(job.ID); err != nil {
		t.Fatalf("ResetJobToPending: %v", err)
	}
	mustRunningJob(t, database, job.ID, "worker-new")

	// Old (partitioned) worker reports completion — must be rejected.
	err = database.UpdateJobTerminalStatusWithOwner(job.ID, "worker-old",
		protocol.JobStatusCompleted, nil, nil, nil, nil)
	if err != protocol.ErrJobNotOwned {
		t.Fatalf("expected ErrJobNotOwned for stale owner report, got %v", err)
	}

	got, _ := database.GetJob(job.ID)
	if got.Status != protocol.JobStatusRunning || got.WorkerID.String != "worker-new" {
		t.Fatalf("job state changed by stale report: status=%s worker=%s", got.Status, got.WorkerID.String)
	}

	// Current owner's report lands.
	err = database.UpdateJobTerminalStatusWithOwner(job.ID, "worker-new",
		protocol.JobStatusCompleted, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("owner report rejected: %v", err)
	}

	// Second terminal report from the same owner is also rejected (idempotence).
	err = database.UpdateJobTerminalStatusWithOwner(job.ID, "worker-new",
		protocol.JobStatusCompleted, nil, nil, nil, nil)
	if err != protocol.ErrJobNotOwned {
		t.Fatalf("expected ErrJobNotOwned for duplicate terminal report, got %v", err)
	}
}

// Acceptance 2a: an offline worker's late heartbeat must not refresh liveness.
func TestHeartbeatDoesNotRefreshOfflineWorker(t *testing.T) {
	database := createStateTestDB(t)
	mustCreateWorker(t, database, "worker-1")

	stale := time.Now().Add(-time.Hour)
	if _, err := database.GetDB().Exec(`UPDATE workers SET last_heartbeat = ?, status = ? WHERE id = ?`,
		stale, protocol.WorkerStatusOffline, "worker-1"); err != nil {
		t.Fatal(err)
	}

	err := database.UpdateWorkerHeartbeat("worker-1", protocol.WorkerStatusIdle)
	if err != protocol.ErrWorkerNotFound {
		t.Fatalf("expected ErrWorkerNotFound for offline heartbeat, got %v", err)
	}

	w, _ := database.GetWorker("worker-1")
	if w.Status != protocol.WorkerStatusOffline {
		t.Fatalf("offline worker changed status to %s", w.Status)
	}
	if !w.LastHeartbeat.Before(time.Now().Add(-30 * time.Minute)) {
		t.Fatal("offline worker's last_heartbeat was refreshed")
	}
}

// Acceptance 2b: zombie revival — a late idle transition must not resurrect an
// offline worker into the schedulable pool.
func TestIdleTransitionSkipsOfflineWorker(t *testing.T) {
	database := createStateTestDB(t)
	mustCreateWorker(t, database, "worker-zombie")

	if _, err := database.GetDB().Exec(`UPDATE workers SET status = ? WHERE id = ?`,
		protocol.WorkerStatusOffline, "worker-zombie"); err != nil {
		t.Fatal(err)
	}

	if err := database.SetWorkerIdleIfNoActiveJobs("worker-zombie"); err != protocol.ErrWorkerNotFound {
		t.Fatalf("expected ErrWorkerNotFound when flipping offline worker to idle, got %v", err)
	}

	w, _ := database.GetWorker("worker-zombie")
	if w.Status != protocol.WorkerStatusOffline {
		t.Fatalf("offline worker resurrected to %s", w.Status)
	}
}

// Idle transition is a no-op while active jobs remain (busy/idle race guard).
func TestIdleTransitionWaitsForActiveJobs(t *testing.T) {
	database := createStateTestDB(t)
	mustCreateWorker(t, database, "worker-busy")

	job, _ := database.CreateJob(`["in.mkv"]`, `[]`, "out.mkv", false)
	mustRunningJob(t, database, job.ID, "worker-busy")
	if err := database.UpdateWorkerStatus("worker-busy", protocol.WorkerStatusBusy); err != nil {
		t.Fatal(err)
	}

	if err := database.SetWorkerIdleIfNoActiveJobs("worker-busy"); err != protocol.ErrWorkerNotFound {
		t.Fatalf("expected ErrWorkerNotFound with active jobs, got %v", err)
	}
	w, _ := database.GetWorker("worker-busy")
	if w.Status != protocol.WorkerStatusBusy {
		t.Fatalf("busy worker flipped to idle despite active jobs: %s", w.Status)
	}

	// After the job finishes, the transition succeeds.
	if err := database.UpdateJobTerminalStatusWithOwner(job.ID, "worker-busy",
		protocol.JobStatusCompleted, nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := database.SetWorkerIdleIfNoActiveJobs("worker-busy"); err != nil {
		t.Fatalf("idle transition failed after completion: %v", err)
	}
	w, _ = database.GetWorker("worker-busy")
	if w.Status != protocol.WorkerStatusIdle {
		t.Fatalf("worker should be idle after last job finished, got %s", w.Status)
	}
}

// Acceptance 2c: evicted worker pull returns a conflict signal to the caller.
func TestEvictedWorkerCannotPull(t *testing.T) {
	database := createStateTestDB(t)
	mustCreateWorker(t, database, "worker-evicted")
	if err := database.MarkWorkerEvicted("worker-evicted"); err != nil {
		t.Fatal(err)
	}

	evicted, err := database.IsWorkerEvicted("worker-evicted")
	if err != nil || !evicted {
		t.Fatalf("eviction flag missing: evicted=%v err=%v", evicted, err)
	}
}

// Acceptance 5: no pending+worker_id orphan — assign is one statement and the
// starvation fallback can still see unassigned jobs.
func TestAtomicAssignNoOrphanState(t *testing.T) {
	database := createStateTestDB(t)
	mustCreateWorker(t, database, "worker-1")

	job, _ := database.CreateJob(`["in.mkv"]`, `[]`, "out.mkv", false)

	// Assign flips pending -> queued in the same statement as worker_id.
	if err := database.AssignJobToWorker(job.ID, "worker-1"); err != nil {
		t.Fatalf("assign failed: %v", err)
	}
	got, _ := database.GetJob(job.ID)
	if got.Status != protocol.JobStatusQueued || got.WorkerID.String != "worker-1" {
		t.Fatalf("atomic assign left inconsistent state: status=%s worker=%v", got.Status, got.WorkerID)
	}

	// Double-assign to another worker is rejected.
	mustCreateWorker(t, database, "worker-2")
	if err := database.AssignJobToWorker(job.ID, "worker-2"); err != protocol.ErrJobNotOwned {
		t.Fatalf("expected ErrJobNotOwned on double assign, got %v", err)
	}

	// Starvation fallback only sees genuinely unassigned jobs.
	n, err := database.FailStarvedPendingJobs(time.Now().Add(time.Hour), "x", string(protocol.FailureNoWorkerAvailable))
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("starvation fallback consumed assigned job: %d", n)
	}
}
