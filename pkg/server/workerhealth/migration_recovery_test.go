package workerhealth

// H7 regression: the health tick flips a worker offline before migrating its
// jobs. If the migration fails, the worker must NOT stay offline (it would
// never be selected again and its jobs would be stranded). The offline flag is
// rolled back so the next tick retries — and the retry must then succeed
// atomically with its audit trail.

import (
	"testing"
	"time"

	"github.com/tsic404/rffmpeg/pkg/protocol"
	"github.com/tsic404/rffmpeg/pkg/server/db/dbtest"
)

func TestFailedMigrationRestoresWorkerForRetry(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	worker, err := database.CreateWorker("worker-1", "crashy", protocol.WorkerCapabilities{
		Encoders:      []string{"libx264"},
		FFmpegVersion: "6.0",
		MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatalf("CreateWorker: %v", err)
	}

	job, err := database.CreateJob(`["in.mkv"]`, `["-c:v","libx264"]`, "out.mkv", false)
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if err := database.AssignJobToWorker(job.ID, worker.ID); err != nil {
		t.Fatalf("AssignJobToWorker: %v", err)
	}
	if err := dbtest.SetJobStatus(t, database, job.ID, protocol.JobStatusRunning, nil, nil, nil, nil); err != nil {
		t.Fatalf("set running: %v", err)
	}

	// Heartbeat goes stale so the next tick flips the worker offline.
	if _, err := database.GetDB().Exec(
		`UPDATE workers SET last_heartbeat = ? WHERE id = ?`,
		time.Now().Add(-2*time.Minute), worker.ID); err != nil {
		t.Fatalf("backdate heartbeat: %v", err)
	}

	m := New(database, Config{
		HeartbeatTimeout:    time.Minute,
		OfflineThreshold:    time.Hour,
		HealthCheckInterval: time.Second,
		MaxRetryCount:       3,
	})

	// First tick: the audit write is forced to fail, so the whole migration
	// transaction rolls back.
	if _, err := database.GetDB().Exec(`ALTER TABLE job_redistributions RENAME TO job_redistributions_off`); err != nil {
		t.Fatalf("hide redistribution table: %v", err)
	}
	m.checkWorkers()

	gotWorker, err := database.GetWorker(worker.ID)
	if err != nil {
		t.Fatalf("GetWorker: %v", err)
	}
	if gotWorker.Status == protocol.WorkerStatusOffline {
		t.Fatal("worker left offline after a failed migration: it will never be selected again")
	}
	gotJob, err := database.GetJob(job.ID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if gotJob.Status != protocol.JobStatusRunning || !gotJob.WorkerID.Valid {
		t.Fatalf("job released without its audit row: status=%s worker=%v", gotJob.Status, gotJob.WorkerID)
	}

	// Second tick: the audit table is back, the worker is re-detected and the
	// migration commits atomically.
	if _, err := database.GetDB().Exec(`ALTER TABLE job_redistributions_off RENAME TO job_redistributions`); err != nil {
		t.Fatalf("restore redistribution table: %v", err)
	}
	m.checkWorkers()

	gotJob, err = database.GetJob(job.ID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if gotJob.Status != protocol.JobStatusPending || gotJob.WorkerID.Valid {
		t.Fatalf("retried migration did not requeue the job: status=%s worker=%v", gotJob.Status, gotJob.WorkerID)
	}

	events, err := database.GetMigrationEventsByWorker(worker.ID, 10)
	if err != nil {
		t.Fatalf("GetMigrationEventsByWorker: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("migration events = %d, want exactly 1 after the retry", len(events))
	}

	gotWorker, err = database.GetWorker(worker.ID)
	if err != nil {
		t.Fatalf("GetWorker: %v", err)
	}
	if gotWorker.Status != protocol.WorkerStatusOffline {
		t.Errorf("worker status after successful migration = %s, want offline", gotWorker.Status)
	}
}
