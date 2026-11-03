package db_test

// Regression tests for the ownership-guarded report paths and the
// transactional worker-offline migration: a job may never be written by a
// worker that no longer owns it, and a migration can never release a job into
// scheduling before its audit trail is committed.

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/tsic404/rffmpeg/pkg/protocol"
	"github.com/tsic404/rffmpeg/pkg/server/db"
	"github.com/tsic404/rffmpeg/pkg/server/db/dbtest"
)

func jobRetryable(t *testing.T, d *db.Database, id string) int {
	t.Helper()
	var retryable int
	if err := d.GetDB().QueryRow(`SELECT retryable FROM jobs WHERE id = ?`, id).Scan(&retryable); err != nil {
		t.Fatalf("read retryable: %v", err)
	}
	return retryable
}

func stageOwnedRunning(t *testing.T, d *db.Database, jobID, workerID string) {
	t.Helper()
	if err := d.AssignJobToWorker(jobID, workerID); err != nil {
		t.Fatalf("AssignJobToWorker: %v", err)
	}
	if err := dbtest.SetJobStatus(t, d, jobID, protocol.JobStatusRunning, nil, nil, nil, nil); err != nil {
		t.Fatalf("set running: %v", err)
	}
}

// TestLateReportsRejectedAfterMigration covers H6: once a job is migrated off
// a worker, every report the old owner sends afterwards (running, progress,
// output, terminal) must be refused and must not mutate the job.
func TestLateReportsRejectedAfterMigration(t *testing.T) {
	database, cleanup := setupIntegrityDB(t)
	defer cleanup()

	mustCreateWorkers(t, database, "w1", "w2")
	job := createNJobs(t, database, 1)[0]
	stageOwnedRunning(t, database, job.ID, "w1")

	if err := database.UpdateWorkerStatus("w1", protocol.WorkerStatusOffline); err != nil {
		t.Fatalf("mark w1 offline: %v", err)
	}
	migration, err := database.MigrateJobsFromWorker("w1", "heartbeat_timeout", 3)
	if err != nil {
		t.Fatalf("MigrateJobsFromWorker: %v", err)
	}
	if len(migration.Migrated) != 1 || migration.Migrated[0] != job.ID {
		t.Fatalf("migrated = %v, want [%s]", migration.Migrated, job.ID)
	}

	err = database.UpdateJobRunningStatusWithOwner(job.ID, "w1", nil, nil, nil, nil)
	if !errors.Is(err, protocol.ErrJobNotOwned) {
		t.Fatalf("late running report: err = %v, want ErrJobNotOwned", err)
	}
	if _, err := database.UpdateJobProgressWithOwner(job.ID, "w1", 55, 10); !errors.Is(err, protocol.ErrJobNotOwned) {
		t.Fatalf("late progress report: err = %v, want ErrJobNotOwned", err)
	}
	if err := database.UpdateJobOutputWithOwner(job.ID, "w1", `["stale-file"]`); !errors.Is(err, protocol.ErrJobNotOwned) {
		t.Fatalf("late output upload: err = %v, want ErrJobNotOwned", err)
	}
	exitCode := 0
	if err := database.UpdateJobTerminalStatusWithOwner(job.ID, "w1", protocol.JobStatusCompleted, &exitCode, nil, nil, nil); !errors.Is(err, protocol.ErrJobNotOwned) {
		t.Fatalf("late terminal report: err = %v, want ErrJobNotOwned", err)
	}

	got, err := database.GetJob(job.ID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if got.Status != protocol.JobStatusPending {
		t.Errorf("job status = %s, want pending (late report resurrected it)", got.Status)
	}
	if got.WorkerID.Valid {
		t.Errorf("job worker_id = %s, want NULL", got.WorkerID.String)
	}
	if got.ProgressPercent != 0 || got.OutputFiles != "[]" {
		t.Errorf("late report wrote derived fields: progress=%v output_files=%q",
			got.ProgressPercent, got.OutputFiles)
	}

	// The new owner's reports land normally.
	stageOwnedRunning(t, database, job.ID, "w2")
	if err := database.UpdateJobRunningStatusWithOwner(job.ID, "w2", nil, nil, nil, nil); err != nil {
		t.Fatalf("new owner running report: %v", err)
	}
	if accepted, err := database.UpdateJobProgressWithOwner(job.ID, "w2", 25, 30); err != nil || !accepted {
		t.Fatalf("new owner progress report: accepted=%v err=%v", accepted, err)
	}
	if err := database.UpdateJobOutputWithOwner(job.ID, "w2", `["new-file"]`); err != nil {
		t.Fatalf("new owner output upload: %v", err)
	}
	if err := database.UpdateJobTerminalStatusWithOwner(job.ID, "w2", protocol.JobStatusCompleted, &exitCode, nil, nil, nil); err != nil {
		t.Fatalf("new owner terminal report: %v", err)
	}
}

// TestProgressRegressionStillAcceptedByOwner keeps the monotonic rule intact:
// an owner's backward report is a benign rejection (accepted=false, no error),
// not a lost-ownership conflict.
func TestProgressRegressionStillAcceptedByOwner(t *testing.T) {
	database, cleanup := setupIntegrityDB(t)
	defer cleanup()

	mustCreateWorkers(t, database, "w1")
	job := createNJobs(t, database, 1)[0]
	stageOwnedRunning(t, database, job.ID, "w1")

	if accepted, err := database.UpdateJobProgressWithOwner(job.ID, "w1", 40, 20); err != nil || !accepted {
		t.Fatalf("first progress report: accepted=%v err=%v", accepted, err)
	}
	accepted, err := database.UpdateJobProgressWithOwner(job.ID, "w1", 10, 60)
	if err != nil {
		t.Fatalf("regressing progress report errored: %v", err)
	}
	if accepted {
		t.Error("regressing progress report was accepted")
	}
	got, _ := database.GetJob(job.ID)
	if got.ProgressPercent != 40 {
		t.Errorf("progress = %v, want 40 (regression must not rewind)", got.ProgressPercent)
	}
}

// TestMigrationRevivedWorkerKeepsJobs covers the "worker was flipped offline
// but its heartbeat came back before the migration ran" race: the migration
// must not touch a live worker's jobs.
func TestMigrationRevivedWorkerKeepsJobs(t *testing.T) {
	database, cleanup := setupIntegrityDB(t)
	defer cleanup()

	mustCreateWorkers(t, database, "w1")
	job := createNJobs(t, database, 1)[0]
	stageOwnedRunning(t, database, job.ID, "w1")

	if err := database.UpdateWorkerStatus("w1", protocol.WorkerStatusOffline); err != nil {
		t.Fatal(err)
	}
	// Heartbeat arrives before the migration pass: the worker is live again.
	if err := database.UpdateWorkerStatus("w1", protocol.WorkerStatusBusy); err != nil {
		t.Fatal(err)
	}

	migration, err := database.MigrateJobsFromWorker("w1", "heartbeat_timeout", 3)
	if err != nil {
		t.Fatalf("MigrateJobsFromWorker: %v", err)
	}
	if len(migration.Migrated) != 0 || migration.EventID != "" {
		t.Fatalf("live worker's jobs were migrated: %+v", migration)
	}
	got, _ := database.GetJob(job.ID)
	if got.Status != protocol.JobStatusRunning || !got.WorkerID.Valid {
		t.Fatalf("live worker's job changed: status=%s worker=%v", got.Status, got.WorkerID)
	}
}

// TestMigrationAuditFailureReleasesNothing covers C-85: if the audit trail
// cannot be written, the jobs must not be requeued. Dropping the redistribution
// table makes the audit step fail; the reset in the same transaction must roll
// back.
func TestMigrationAuditFailureReleasesNothing(t *testing.T) {
	database, cleanup := setupIntegrityDB(t)
	defer cleanup()

	mustCreateWorkers(t, database, "w1")
	job := createNJobs(t, database, 1)[0]
	stageOwnedRunning(t, database, job.ID, "w1")

	if err := database.UpdateWorkerStatus("w1", protocol.WorkerStatusOffline); err != nil {
		t.Fatal(err)
	}
	if _, err := database.GetDB().Exec(`DROP TABLE job_redistributions`); err != nil {
		t.Fatalf("drop job_redistributions: %v", err)
	}

	if _, err := database.MigrateJobsFromWorker("w1", "heartbeat_timeout", 3); err == nil {
		t.Fatal("migration succeeded without its audit table")
	}

	got, err := database.GetJob(job.ID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if got.Status != protocol.JobStatusRunning {
		t.Fatalf("job released into scheduling without its audit row: status=%s", got.Status)
	}
	if !got.WorkerID.Valid || got.WorkerID.String != "w1" {
		t.Fatalf("job ownership lost on failed migration: worker_id=%v", got.WorkerID)
	}
}

// TestMigrationFailsJobsPastRetryBudget: a job whose prior migrations reached
// the budget is failed as WORKER_CRASH in the same transaction, not requeued.
func TestMigrationFailsJobsPastRetryBudget(t *testing.T) {
	database, cleanup := setupIntegrityDB(t)
	defer cleanup()

	mustCreateWorkers(t, database, "w1")
	job := createNJobs(t, database, 1)[0]
	stageOwnedRunning(t, database, job.ID, "w1")

	// Burn the budget through the real migration path: three heartbeat-timeout
	// migrations, re-claiming the job between rounds.
	for round := range 3 {
		if err := database.UpdateWorkerStatus("w1", protocol.WorkerStatusOffline); err != nil {
			t.Fatal(err)
		}
		res, err := database.MigrateJobsFromWorker("w1", "heartbeat_timeout", 3)
		if err != nil {
			t.Fatalf("migration round %d: %v", round+1, err)
		}
		if len(res.Migrated) != 1 || res.Migrated[0] != job.ID {
			t.Fatalf("migration round %d: migrated = %v, want [%s]", round+1, res.Migrated, job.ID)
		}
		if err := database.UpdateWorkerStatus("w1", protocol.WorkerStatusIdle); err != nil {
			t.Fatal(err)
		}
		stageOwnedRunning(t, database, job.ID, "w1")
	}
	if err := database.UpdateWorkerStatus("w1", protocol.WorkerStatusOffline); err != nil {
		t.Fatal(err)
	}

	migration, err := database.MigrateJobsFromWorker("w1", "heartbeat_timeout", 3)
	if err != nil {
		t.Fatalf("MigrateJobsFromWorker: %v", err)
	}
	if len(migration.Migrated) != 0 {
		t.Errorf("over-budget job was migrated: %v", migration.Migrated)
	}
	if len(migration.Failed) != 1 || migration.Failed[0] != job.ID {
		t.Fatalf("failed = %v, want [%s]", migration.Failed, job.ID)
	}

	got, _ := database.GetJob(job.ID)
	if got.Status != protocol.JobStatusFailed {
		t.Errorf("status = %s, want failed", got.Status)
	}
	if got.FailureType != string(protocol.FailureWorkerCrash) {
		t.Errorf("failure_type = %q, want WORKER_CRASH", got.FailureType)
	}
	if r := jobRetryable(t, database, job.ID); r != 1 {
		t.Errorf("retryable = %d, want 1", r)
	}
}

// TestStuckActiveJobSweep covers the queued-timeout and orphaned-worker net:
// only jobs that cannot make progress are selected, and the two terminal
// classifications stay distinct.
func TestStuckActiveJobSweep(t *testing.T) {
	database, cleanup := setupIntegrityDB(t)
	defer cleanup()

	mustCreateWorkers(t, database, "w-live", "w-gone", "w-offline")

	// Stuck queued on a live worker: swept as INFRA.
	queued := createNJobs(t, database, 1)[0]
	if err := database.AssignJobToWorker(queued.ID, "w-live"); err != nil {
		t.Fatal(err)
	}
	// Queued on a worker that disappeared: swept as WORKER_CRASH.
	orphaned := createNJobs(t, database, 1)[0]
	if err := database.AssignJobToWorker(orphaned.ID, "w-gone"); err != nil {
		t.Fatal(err)
	}
	// Running on a live worker: owned by checkTimeouts, never swept.
	running := createNJobs(t, database, 1)[0]
	stageOwnedRunning(t, database, running.ID, "w-live")
	// Running on a merely offline worker: the failover migration's case, not
	// the sweep's.
	offlineRunning := createNJobs(t, database, 1)[0]
	stageOwnedRunning(t, database, offlineRunning.ID, "w-offline")
	if err := database.UpdateWorkerStatus("w-offline", protocol.WorkerStatusOffline); err != nil {
		t.Fatal(err)
	}
	// Running on a worker record that is gone: nobody can migrate it.
	vanishedRunning := createNJobs(t, database, 1)[0]
	stageOwnedRunning(t, database, vanishedRunning.ID, "w-vanished")
	// Fresh queued job: not stuck yet.
	fresh := createNJobs(t, database, 1)[0]
	if err := database.AssignJobToWorker(fresh.ID, "w-live"); err != nil {
		t.Fatal(err)
	}

	if _, err := database.GetDB().Exec(`DELETE FROM workers WHERE id = 'w-gone'`); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-10 * time.Minute)
	for _, id := range []string{queued.ID, orphaned.ID, running.ID, offlineRunning.ID, vanishedRunning.ID} {
		if _, err := database.GetDB().Exec(`UPDATE jobs SET updated_at = ? WHERE id = ?`, past, id); err != nil {
			t.Fatal(err)
		}
	}

	stuck, err := database.GetStuckActiveJobs(5 * time.Minute)
	if err != nil {
		t.Fatalf("GetStuckActiveJobs: %v", err)
	}
	byID := map[string]db.StuckJob{}
	for _, sj := range stuck {
		byID[sj.ID] = sj
	}
	if _, ok := byID[fresh.ID]; ok {
		t.Error("fresh queued job was selected as stuck")
	}
	if _, ok := byID[running.ID]; ok {
		t.Error("running job on a live worker was selected as stuck")
	}
	if _, ok := byID[offlineRunning.ID]; ok {
		t.Error("running job on an offline worker was selected as stuck (the migration owns it)")
	}
	if sj, ok := byID[vanishedRunning.ID]; !ok || !sj.Crashed {
		t.Errorf("job on a vanished worker = %+v, want present and crashed", sj)
	}
	if sj, ok := byID[queued.ID]; !ok || sj.Crashed {
		t.Errorf("stuck queued job = %+v, want present and not crashed", sj)
	}
	if sj, ok := byID[orphaned.ID]; !ok || !sj.Crashed {
		t.Errorf("orphaned job = %+v, want present and crashed", sj)
	}

	failedIDs, err := database.FailStuckActiveJobs(5*time.Minute, stuck, "stuck queued", "worker vanished")
	if err != nil {
		t.Fatalf("FailStuckActiveJobs: %v", err)
	}
	if len(failedIDs) != 3 {
		t.Errorf("failed = %v, want 3 ids", failedIDs)
	}

	gotQueued, _ := database.GetJob(queued.ID)
	if gotQueued.Status != protocol.JobStatusFailed || gotQueued.FailureType != string(protocol.FailureInfra) {
		t.Errorf("queued job = status %s type %s, want failed/INFRA", gotQueued.Status, gotQueued.FailureType)
	}
	if r := jobRetryable(t, database, queued.ID); r != 0 {
		t.Errorf("INFRA retryable = %d, want 0", r)
	}
	gotOrphan, _ := database.GetJob(orphaned.ID)
	if gotOrphan.Status != protocol.JobStatusFailed || gotOrphan.FailureType != string(protocol.FailureWorkerCrash) {
		t.Errorf("orphaned job = status %s type %s, want failed/WORKER_CRASH", gotOrphan.Status, gotOrphan.FailureType)
	}
	gotRunning, _ := database.GetJob(running.ID)
	if gotRunning.Status != protocol.JobStatusRunning {
		t.Errorf("running job on a live worker was swept: %s", gotRunning.Status)
	}
	gotOfflineRunning, _ := database.GetJob(offlineRunning.ID)
	if gotOfflineRunning.Status != protocol.JobStatusRunning {
		t.Errorf("running job on an offline worker was swept: %s", gotOfflineRunning.Status)
	}
	gotVanished, _ := database.GetJob(vanishedRunning.ID)
	if gotVanished.Status != protocol.JobStatusFailed || gotVanished.FailureType != string(protocol.FailureWorkerCrash) {
		t.Errorf("job on a vanished worker = status %s type %s, want failed/WORKER_CRASH",
			gotVanished.Status, gotVanished.FailureType)
	}
}

// TestMigrationRacingLateReports runs the failover migration and a late report
// from the old owner concurrently: either the terminal report wins first (job
// completed by its owner) or the migration wins (job pending and the report is
// refused). The job must never end up running without an owner.
func TestMigrationRacingLateReports(t *testing.T) {
	database, cleanup := setupIntegrityDB(t)
	defer cleanup()

	mustCreateWorkers(t, database, "w1")

	for range 25 {
		job := createNJobs(t, database, 1)[0]
		stageOwnedRunning(t, database, job.ID, "w1")
		if err := database.UpdateWorkerStatus("w1", protocol.WorkerStatusOffline); err != nil {
			t.Fatal(err)
		}

		var wg sync.WaitGroup
		wg.Add(2)
		go func(id string) {
			defer wg.Done()
			if _, err := database.MigrateJobsFromWorker("w1", "heartbeat_timeout", 3); err != nil {
				t.Errorf("migration: %v", err)
			}
		}(job.ID)
		go func(id string) {
			defer wg.Done()
			_ = database.UpdateJobRunningStatusWithOwner(id, "w1", nil, nil, nil, nil)
			_, _ = database.UpdateJobProgressWithOwner(id, "w1", 50, 5)
			exitCode := 0
			err := database.UpdateJobTerminalStatusWithOwner(id, "w1", protocol.JobStatusCompleted, &exitCode, nil, nil, nil)
			if err != nil && !errors.Is(err, protocol.ErrJobNotOwned) {
				t.Errorf("late report: %v", err)
			}
		}(job.ID)
		wg.Wait()

		got, err := database.GetJob(job.ID)
		if err != nil {
			t.Fatalf("GetJob: %v", err)
		}
		switch got.Status {
		case protocol.JobStatusPending:
			if got.WorkerID.Valid {
				t.Fatalf("pending job still owned by %s", got.WorkerID.String)
			}
		case protocol.JobStatusCompleted:
			if !got.WorkerID.Valid || got.WorkerID.String != "w1" {
				t.Fatalf("completed job lost its owner: %v", got.WorkerID)
			}
		default:
			t.Fatalf("job ended in %s (worker=%v): migration and late report both lost", got.Status, got.WorkerID)
		}

		// Reset the worker for the next iteration.
		if err := database.UpdateWorkerStatus("w1", protocol.WorkerStatusIdle); err != nil {
			t.Fatal(err)
		}
	}
}
