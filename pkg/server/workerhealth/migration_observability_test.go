package workerhealth

import (
	"testing"
	"time"

	"github.com/tsix404/rffmpeg/pkg/protocol"
	"github.com/tsix404/rffmpeg/pkg/server/migration"
)

// TestMigrationEventObservableViaMonitorLoop closes the TSI-2800 acceptance
// gap: the QA run never saw a migration event because its jobs finished before
// the 90s heartbeat timeout, and every existing migration test calls
// checkWorkers() directly with a hand-staled heartbeat. This test exercises
// the real async path — Start()/Stop() with a shortened heartbeat timeout —
// so a worker that stops heartbeating while a job is still running gets marked
// offline and its job migrated, and that migration is observable via the same
// query the GET /api/v1/migrations handler uses.
//
// It reproduces the issue's own recommendation (调小 --worker-heartbeat-timeout)
// deterministically: HeartbeatTimeout=500ms, HealthCheckInterval=100ms, so the
// worker's last_heartbeat goes stale without any manual UPDATE and the monitor
// must detect it on its own ticker.
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
