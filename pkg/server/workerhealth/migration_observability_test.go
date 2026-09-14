package workerhealth

import (
	"testing"
	"time"

	"github.com/tsic404/rffmpeg/pkg/protocol"
	"github.com/tsic404/rffmpeg/pkg/server/migration"
)

// TestMigrationEventObservableViaMonitorLoop closes the gap: the QA run never
// saw a migration event because its jobs finished before the 90s heartbeat
// timeout, and other tests call checkWorkers() with a hand-staled heartbeat.
// This test exercises the real async path (Start/Stop with a shortened
// heartbeat timeout): a worker that stops heartbeating while a job runs gets
// marked offline and its job migrated, observable via the GET
// /api/v1/migrations query. HeartbeatTimeout=500ms + HealthCheckInterval=100ms
// makes last_heartbeat go stale with no manual UPDATE.
func TestMigrationEventObservableViaMonitorLoop(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	// A worker registers with a fresh heartbeat.
	worker, err := database.CreateWorker("worker-1", "crashy-worker", protocol.WorkerCapabilities{
		Encoders:      []string{"libx264"},
		FFmpegVersion: "6.0",
		MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatalf("Failed to create worker: %v", err)
	}

	// A job is assigned and running when the worker dies (crash: no terminal
	// status is ever reported).
	job, err := database.CreateJob(`["input.mkv"]`, `["-c:v","libx264"]`, "output.mkv", false)
	if err != nil {
		t.Fatalf("Failed to create job: %v", err)
	}
	if err := database.AssignJobToWorker(job.ID, worker.ID); err != nil {
		t.Fatalf("Failed to assign job to worker: %v", err)
	}
	if err := database.UpdateJobStatusWithFailure(job.ID, protocol.JobStatusRunning, nil, nil, nil, nil); err != nil {
		t.Fatalf("Failed to set job running: %v", err)
	}

	// Shortened timeout/interval: the worker's fresh heartbeat goes stale in
	// 500ms and the monitor must notice on its own ticker loop.
	monitor := New(database, Config{
		HeartbeatTimeout:    500 * time.Millisecond,
		OfflineThreshold:    1 * time.Minute,
		HealthCheckInterval: 100 * time.Millisecond,
		MaxRetryCount:       3,
	})
	mockSched := &mockScheduler{}
	monitor.SetScheduler(mockSched)
	monitor.Start()
	defer monitor.Stop()

	// Poll for the migration event to become observable. The monitor's first
	// tick may precede the heartbeat expiry; retry until the sweep lands or a
	// generous deadline passes (the loop runs every 100ms, so 5s is ample).
	deadline := time.Now().Add(5 * time.Second)
	var events []migration.EventInfo
	for time.Now().Before(deadline) {
		events, err = monitor.GetMigrationEventsByWorker(worker.ID, 10)
		if err != nil {
			t.Fatalf("Failed to list migration events: %v", err)
		}
		if len(events) > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if len(events) == 0 {
		t.Fatal("No migration event observed after worker heartbeat timeout — migration is not observable through the async monitor loop")
	}

	event := events[0]
	if event.Reason != migration.ReasonHeartbeatTimeout {
		t.Errorf("migration reason = %q, want %q", event.Reason, migration.ReasonHeartbeatTimeout)
	}
	if event.JobsMigrated != 1 {
		t.Errorf("jobs_migrated = %d, want 1", event.JobsMigrated)
	}
	if len(event.JobIDs) != 1 || event.JobIDs[0] != job.ID {
		t.Errorf("migrated job_ids = %v, want [%s]", event.JobIDs, job.ID)
	}

	// The worker is offline and the job was reset to pending for another worker.
	wAfter, err := database.GetWorker(worker.ID)
	if err != nil {
		t.Fatalf("Failed to get worker after timeout: %v", err)
	}
	if wAfter.Status != protocol.WorkerStatusOffline {
		t.Errorf("worker status = %s, want offline", wAfter.Status)
	}

	jAfter, err := database.GetJob(job.ID)
	if err != nil {
		t.Fatalf("Failed to get job after migration: %v", err)
	}
	if jAfter.Status != protocol.JobStatusPending {
		t.Errorf("job status = %s, want pending", jAfter.Status)
	}
	if jAfter.WorkerID.Valid {
		t.Errorf("job worker_id should be NULL after migration, got %s", jAfter.WorkerID.String)
	}

	// Stop the monitor before reading rescheduleCalled. Stop joins the run
	// goroutine (close(done) is received in Stop), which establishes a
	// happens-before edge so the plain-bool read is race-free, and guarantees
	// the ticker iteration that wrote the migration event also completed its
	// TriggerReschedule() call — CreateMigrationEvent runs before
	// TriggerReschedule in migrateJobsFromWorker, so reading the flag without
	// joining could race ahead and see false (the reviewer's flaky window).
	monitor.Stop()

	// The monitor must have triggered a reschedule so another worker picks up
	// the migrated job.
	if !mockSched.rescheduleCalled {
		t.Error("Expected scheduler reschedule trigger after migration")
	}
}

// TestMigrationEventRetryCountIsCumulative verifies that migration_events.
// retry_count records the job's cumulative migration count after each event
// (first migration = 1), not the pre-migration count (first = 0). Migrating
// the same job twice through the monitor must record 1 then 2, so the
// self-heal history is directly readable from the audit table without
// per-job aggregation.
func TestMigrationEventRetryCountIsCumulative(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	for _, w := range []struct{ id, name string }{
		{"worker-1", "w1"}, {"worker-2", "w2"},
	} {
		if _, err := database.CreateWorker(w.id, w.name, protocol.WorkerCapabilities{
			Encoders:      []string{"libx264"},
			FFmpegVersion: "6.0",
		}); err != nil {
			t.Fatalf("create worker %s: %v", w.id, err)
		}
	}

	job, err := database.CreateJob(`["input.mkv"]`, `["-c:v","libx264"]`, "output.mkv", false)
	if err != nil {
		t.Fatalf("create job: %v", err)
	}

	monitor := New(database, Config{
		HeartbeatTimeout:    30 * time.Second,
		OfflineThreshold:    10 * time.Minute,
		HealthCheckInterval: 1 * time.Second,
		MaxRetryCount:       3,
	})
	monitor.SetScheduler(&mockScheduler{})

	// First migration: worker-1 crashes while running the job.
	if err := database.AssignJobToWorker(job.ID, "worker-1"); err != nil {
		t.Fatalf("assign to worker-1: %v", err)
	}
	if err := database.UpdateJobStatusWithFailure(job.ID, protocol.JobStatusRunning, nil, nil, nil, nil); err != nil {
		t.Fatalf("set running: %v", err)
	}
	if _, err := database.GetDB().Exec(`UPDATE workers SET last_heartbeat = ? WHERE id = ?`,
		time.Now().Add(-60*time.Second), "worker-1"); err != nil {
		t.Fatalf("stale worker-1: %v", err)
	}
	monitor.checkWorkers()

	events, err := database.GetMigrationEventsByWorker("worker-1", 10)
	if err != nil {
		t.Fatalf("list worker-1 events: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("worker-1 events = %d, want 1", len(events))
	}
	if events[0].RetryCount != 1 {
		t.Errorf("first migration retry_count = %d, want 1", events[0].RetryCount)
	}

	// Second migration: worker-2 crashes after picking up the pending job.
	if err := database.AssignJobToWorker(job.ID, "worker-2"); err != nil {
		t.Fatalf("assign to worker-2: %v", err)
	}
	if err := database.UpdateJobStatusWithFailure(job.ID, protocol.JobStatusRunning, nil, nil, nil, nil); err != nil {
		t.Fatalf("set running (2nd): %v", err)
	}
	if _, err := database.GetDB().Exec(`UPDATE workers SET last_heartbeat = ? WHERE id = ?`,
		time.Now().Add(-60*time.Second), "worker-2"); err != nil {
		t.Fatalf("stale worker-2: %v", err)
	}
	monitor.checkWorkers()

	events, err = database.GetMigrationEventsByWorker("worker-2", 10)
	if err != nil {
		t.Fatalf("list worker-2 events: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("worker-2 events = %d, want 1", len(events))
	}
	if events[0].RetryCount != 2 {
		t.Errorf("second migration retry_count = %d, want 2", events[0].RetryCount)
	}
}

// TestMigrationEventRetryCountPerJobMixedBatch pins the multi-job case: when a
// single worker's running set mixes jobs at different retry stages, the
// event-level retry_count is the max over the migrated jobs only (not the
// failed ones), and each migrated job's exact cumulative count is persisted
// per job in job_redistributions. A worker holding a maxed-out job (will fail)
// and a fresh job (first migration) must record 1 — not the failed job's count
// + 1.
func TestMigrationEventRetryCountPerJobMixedBatch(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	if _, err := database.CreateWorker("worker-1", "w1", protocol.WorkerCapabilities{
		Encoders:      []string{"libx264"},
		FFmpegVersion: "6.0",
	}); err != nil {
		t.Fatalf("create worker: %v", err)
	}

	// jobA already migrated 3 times (== MaxRetryCount): it will fail, not migrate.
	jobA, err := database.CreateJob(`["a.mkv"]`, `["-c:v","libx264"]`, "out_a.mkv", false)
	if err != nil {
		t.Fatalf("create jobA: %v", err)
	}
	for i, w := range []string{"old-w1", "old-w2", "old-w3"} {
		if _, err := database.CreateMigrationEvent(w, w, string(migration.ReasonHeartbeatTimeout), i+1, []string{jobA.ID}, 1); err != nil {
			t.Fatalf("seed jobA migration %d: %v", i+1, err)
		}
	}

	// jobB is fresh: its first migration records a cumulative count of 1.
	jobB, err := database.CreateJob(`["b.mkv"]`, `["-c:v","libx264"]`, "out_b.mkv", false)
	if err != nil {
		t.Fatalf("create jobB: %v", err)
	}

	for _, jid := range []string{jobA.ID, jobB.ID} {
		if err := database.AssignJobToWorker(jid, "worker-1"); err != nil {
			t.Fatalf("assign %s: %v", jid, err)
		}
		if err := database.UpdateJobStatusWithFailure(jid, protocol.JobStatusRunning, nil, nil, nil, nil); err != nil {
			t.Fatalf("set %s running: %v", jid, err)
		}
	}
	if _, err := database.GetDB().Exec(`UPDATE workers SET last_heartbeat = ? WHERE id = ?`,
		time.Now().Add(-60*time.Second), "worker-1"); err != nil {
		t.Fatalf("stale worker-1: %v", err)
	}

	monitor := New(database, Config{
		HeartbeatTimeout:    30 * time.Second,
		OfflineThreshold:    10 * time.Minute,
		HealthCheckInterval: 1 * time.Second,
		MaxRetryCount:       3,
	})
	monitor.SetScheduler(&mockScheduler{})
	monitor.checkWorkers()

	// The event must record 1 (jobB's cumulative count), not jobA's count + 1.
	events, err := database.GetMigrationEventsByWorker("worker-1", 10)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	if events[0].RetryCount != 1 {
		t.Errorf("event retry_count = %d, want 1 (max over migrated jobs)", events[0].RetryCount)
	}

	// jobA failed on budget exhaustion; jobB migrated back to pending.
	gotA, err := database.GetJob(jobA.ID)
	if err != nil {
		t.Fatalf("get jobA: %v", err)
	}
	if gotA.Status != protocol.JobStatusFailed {
		t.Errorf("jobA status = %s, want failed", gotA.Status)
	}
	gotB, err := database.GetJob(jobB.ID)
	if err != nil {
		t.Fatalf("get jobB: %v", err)
	}
	if gotB.Status != protocol.JobStatusPending {
		t.Errorf("jobB status = %s, want pending", gotB.Status)
	}

	// jobB's exact per-job cumulative count is persisted on its redistribution.
	rs, err := database.GetJobRedistributionsByEvent(events[0].ID)
	if err != nil {
		t.Fatalf("list redistributions: %v", err)
	}
	if len(rs) != 1 {
		t.Fatalf("redistributions = %d, want 1 (only jobB migrated)", len(rs))
	}
	if rs[0].JobID != jobB.ID {
		t.Errorf("redistribution job_id = %q, want %q", rs[0].JobID, jobB.ID)
	}
	if rs[0].RetryCount != 1 {
		t.Errorf("redistribution retry_count = %d, want 1", rs[0].RetryCount)
	}
}
