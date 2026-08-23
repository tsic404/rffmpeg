package workerhealth

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/tsix404/rffmpeg/pkg/protocol"
	"github.com/tsix404/rffmpeg/pkg/server/db"
	"github.com/tsix404/rffmpeg/pkg/server/migration"
)

// mockScheduler implements SchedulerInterface for testing
type mockScheduler struct {
	rescheduleCalled bool
}

func (m *mockScheduler) TriggerReschedule() {
	m.rescheduleCalled = true
}

func setupTestDB(t *testing.T) *db.Database {
	t.Helper()
	database, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("Failed to create test database: %v", err)
	}
	return database
}

func TestMonitorHeartbeatTimeout(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	// Create a worker
	_, err := database.CreateWorker("worker-1", "test-worker", protocol.WorkerCapabilities{
		Encoders:      []string{"libx264"},
		FFmpegVersion: "6.0",
	})
	if err != nil {
		t.Fatalf("Failed to create worker: %v", err)
	}

	// Make the worker's heartbeat stale
	_, err = database.GetDB().Exec(`
		UPDATE workers SET last_heartbeat = ? WHERE id = ?
	`, time.Now().Add(-60*time.Second), "worker-1")
	if err != nil {
		t.Fatalf("Failed to update worker heartbeat: %v", err)
	}

	// Create monitor with short timeout
	monitor := New(database, Config{
		HeartbeatTimeout:    30 * time.Second,
		OfflineThreshold:    10 * time.Minute,
		HealthCheckInterval: 1 * time.Second,
		MaxRetryCount:       3,
	})

	mockSched := &mockScheduler{}
	monitor.SetScheduler(mockSched)

	// Call checkWorkers directly
	monitor.checkWorkers()

	// Verify worker is now offline
	worker, err := database.GetWorker("worker-1")
	if err != nil {
		t.Fatalf("Failed to get worker: %v", err)
	}
	if worker.Status != protocol.WorkerStatusOffline {
		t.Errorf("Expected worker to be offline, got %s", worker.Status)
	}
}

func TestMonitorJobMigration(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	// Create a worker
	_, err := database.CreateWorker("worker-1", "test-worker", protocol.WorkerCapabilities{
		Encoders:      []string{"libx264"},
		FFmpegVersion: "6.0",
	})
	if err != nil {
		t.Fatalf("Failed to create worker: %v", err)
	}

	// Create and assign jobs to the worker
	job1, err := database.CreateJob(`["input1.mkv"]`, `["-c:v","libx264"]`, "output1.mkv", false)
	if err != nil {
		t.Fatalf("Failed to create job1: %v", err)
	}

	job2, err := database.CreateJob(`["input2.mkv"]`, `["-c:v","libx264"]`, "output2.mkv", false)
	if err != nil {
		t.Fatalf("Failed to create job2: %v", err)
	}

	// Assign jobs to worker and set to running/queued
	_ = database.AssignJobToWorker(job1.ID, "worker-1")
	_ = database.UpdateJobStatusWithFailure(job1.ID, protocol.JobStatusRunning, nil, nil, nil, nil)

	_ = database.AssignJobToWorker(job2.ID, "worker-1")
	_ = database.UpdateJobStatusWithFailure(job2.ID, protocol.JobStatusQueued, nil, nil, nil, nil)

	// Make the worker's heartbeat stale
	_, err = database.GetDB().Exec(`
		UPDATE workers SET last_heartbeat = ? WHERE id = ?
	`, time.Now().Add(-60*time.Second), "worker-1")
	if err != nil {
		t.Fatalf("Failed to update worker heartbeat: %v", err)
	}

	// Create monitor
	monitor := New(database, Config{
		HeartbeatTimeout:    30 * time.Second,
		OfflineThreshold:    10 * time.Minute,
		HealthCheckInterval: 1 * time.Second,
		MaxRetryCount:       3,
	})

	mockSched := &mockScheduler{}
	monitor.SetScheduler(mockSched)

	// Call checkWorkers directly
	monitor.checkWorkers()

	// Verify jobs are now pending
	job1After, err := database.GetJob(job1.ID)
	if err != nil {
		t.Fatalf("Failed to get job1: %v", err)
	}
	if job1After.Status != protocol.JobStatusPending {
		t.Errorf("Expected job1 to be pending, got %s", job1After.Status)
	}

	job2After, err := database.GetJob(job2.ID)
	if err != nil {
		t.Fatalf("Failed to get job2: %v", err)
	}
	if job2After.Status != protocol.JobStatusPending {
		t.Errorf("Expected job2 to be pending, got %s", job2After.Status)
	}

	// Verify migration event was recorded
	events, err := database.GetMigrationEventsByWorker("worker-1", 10)
	if err != nil {
		t.Fatalf("Failed to get migration events: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("Expected 1 migration event, got %d", len(events))
	}

	// Verify migration event details
	var jobIDs []string
	if err := json.Unmarshal([]byte(events[0].JobIDs), &jobIDs); err != nil {
		t.Fatalf("Failed to unmarshal job IDs: %v", err)
	}
	if len(jobIDs) != 2 {
		t.Errorf("Expected 2 job IDs in migration event, got %d", len(jobIDs))
	}

	// Verify scheduler was triggered
	if !mockSched.rescheduleCalled {
		t.Error("Expected scheduler to be triggered")
	}
}

func TestMonitorNoMigrationWhenNoJobs(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	// Create a worker with no jobs
	_, err := database.CreateWorker("worker-1", "test-worker", protocol.WorkerCapabilities{
		Encoders:      []string{"libx264"},
		FFmpegVersion: "6.0",
	})
	if err != nil {
		t.Fatalf("Failed to create worker: %v", err)
	}

	// Make the worker's heartbeat stale
	_, err = database.GetDB().Exec(`
		UPDATE workers SET last_heartbeat = ? WHERE id = ?
	`, time.Now().Add(-60*time.Second), "worker-1")
	if err != nil {
		t.Fatalf("Failed to update worker heartbeat: %v", err)
	}

	// Create monitor
	monitor := New(database, Config{
		HeartbeatTimeout:    30 * time.Second,
		OfflineThreshold:    10 * time.Minute,
		HealthCheckInterval: 1 * time.Second,
		MaxRetryCount:       3,
	})

	mockSched := &mockScheduler{}
	monitor.SetScheduler(mockSched)

	// Call checkWorkers directly
	monitor.checkWorkers()

	// Verify worker is offline
	worker, err := database.GetWorker("worker-1")
	if err != nil {
		t.Fatalf("Failed to get worker: %v", err)
	}
	if worker.Status != protocol.WorkerStatusOffline {
		t.Errorf("Expected worker to be offline, got %s", worker.Status)
	}

	// Verify no migration events
	events, err := database.GetMigrationEventsByWorker("worker-1", 10)
	if err != nil {
		t.Fatalf("Failed to get migration events: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("Expected 0 migration events, got %d", len(events))
	}

	// Scheduler should not be triggered
	if mockSched.rescheduleCalled {
		t.Error("Expected scheduler not to be triggered when no jobs")
	}
}

func TestMonitorRetryCount(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	// Create a worker
	_, err := database.CreateWorker("worker-1", "test-worker", protocol.WorkerCapabilities{
		Encoders:      []string{"libx264"},
		FFmpegVersion: "6.0",
	})
	if err != nil {
		t.Fatalf("Failed to create worker: %v", err)
	}

	// Create a job
	job1, err := database.CreateJob(`["input1.mkv"]`, `["-c:v","libx264"]`, "output1.mkv", false)
	if err != nil {
		t.Fatalf("Failed to create job1: %v", err)
	}

	// Assign job to worker and set to running
	_ = database.AssignJobToWorker(job1.ID, "worker-1")
	_ = database.UpdateJobStatusWithFailure(job1.ID, protocol.JobStatusRunning, nil, nil, nil, nil)

	// Create a previous migration event for this job (simulating previous retry)
	_, err = database.CreateMigrationEvent("worker-old", "old-worker", string(migration.ReasonHeartbeatTimeout), 0, []string{job1.ID}, 1)
	if err != nil {
		t.Fatalf("Failed to create previous migration event: %v", err)
	}

	// Make the worker's heartbeat stale
	_, err = database.GetDB().Exec(`
		UPDATE workers SET last_heartbeat = ? WHERE id = ?
	`, time.Now().Add(-60*time.Second), "worker-1")
	if err != nil {
		t.Fatalf("Failed to update worker heartbeat: %v", err)
	}

	// Create monitor
	monitor := New(database, Config{
		HeartbeatTimeout:    30 * time.Second,
		OfflineThreshold:    10 * time.Minute,
		HealthCheckInterval: 1 * time.Second,
		MaxRetryCount:       3,
	})

	// Call checkWorkers directly
	monitor.checkWorkers()

	// Verify migration event was recorded with correct retry count
	events, err := database.GetMigrationEventsByWorker("worker-1", 10)
	if err != nil {
		t.Fatalf("Failed to get migration events: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("Expected 1 migration event, got %d", len(events))
	}

	// The retry count should be 1 (previous migration)
	if events[0].RetryCount != 1 {
		t.Errorf("Expected retry count 1, got %d", events[0].RetryCount)
	}
}

func TestGetMigrationEvents(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	// Create migration events
	_, _ = database.CreateMigrationEvent("worker-1", "worker-1", string(migration.ReasonHeartbeatTimeout), 0, []string{"job-1"}, 1)
	_, _ = database.CreateMigrationEvent("worker-2", "worker-2", string(migration.ReasonHeartbeatTimeout), 0, []string{"job-2"}, 1)

	// Create monitor
	monitor := New(database, Config{
		HeartbeatTimeout:    30 * time.Second,
		OfflineThreshold:    10 * time.Minute,
		HealthCheckInterval: 1 * time.Second,
		MaxRetryCount:       3,
	})

	// Get migration events
	events, err := monitor.GetMigrationEvents(10, 0)
	if err != nil {
		t.Fatalf("Failed to get migration events: %v", err)
	}

	if len(events) != 2 {
		t.Errorf("Expected 2 migration events, got %d", len(events))
	}

	// Verify event details
	for _, event := range events {
		if event.Reason != migration.ReasonHeartbeatTimeout {
			t.Errorf("Expected reason heartbeat_timeout, got %s", event.Reason)
		}
	}
}

func TestGetMigrationEventsByWorker(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	// Create migration events for different workers
	_, _ = database.CreateMigrationEvent("worker-1", "worker-1", string(migration.ReasonHeartbeatTimeout), 0, []string{"job-1"}, 1)
	_, _ = database.CreateMigrationEvent("worker-2", "worker-2", string(migration.ReasonHeartbeatTimeout), 0, []string{"job-2"}, 1)
	_, _ = database.CreateMigrationEvent("worker-1", "worker-1", string(migration.ReasonHeartbeatTimeout), 1, []string{"job-3"}, 1)

	// Create monitor
	monitor := New(database, Config{
		HeartbeatTimeout:    30 * time.Second,
		OfflineThreshold:    10 * time.Minute,
		HealthCheckInterval: 1 * time.Second,
		MaxRetryCount:       3,
	})

	// Get migration events for worker-1
	events, err := monitor.GetMigrationEventsByWorker("worker-1", 10)
	if err != nil {
		t.Fatalf("Failed to get migration events by worker: %v", err)
	}

	if len(events) != 2 {
		t.Errorf("Expected 2 migration events for worker-1, got %d", len(events))
	}

	// All events should be for worker-1
	for _, event := range events {
		if event.WorkerID != "worker-1" {
			t.Errorf("Expected worker ID worker-1, got %s", event.WorkerID)
		}
	}
}

// --- Slow Node Eviction + Audit Tests (TSI-761 / TSI-762) ---

func TestSlowNodeEvictionDB(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	// Create workers
	for _, w := range []struct {
		id   string
		name string
	}{
		{"w1", "worker-1"},
		{"w2", "worker-2"},
		{"w3", "worker-3"},
	} {
		_, err := database.CreateWorker(w.id, w.name, protocol.WorkerCapabilities{
			Encoders:      []string{"libx264"},
			FFmpegVersion: "6.0",
		})
		if err != nil {
			t.Fatalf("Failed to create worker %s: %v", w.id, err)
		}
	}

	// Mark w2 as evicted
	if err := database.MarkWorkerEvicted("w2"); err != nil {
		t.Fatalf("Failed to mark worker evicted: %v", err)
	}

	// Verify w2 is evicted
	evicted, err := database.IsWorkerEvicted("w2")
	if err != nil {
		t.Fatalf("Failed to check eviction: %v", err)
	}
	if !evicted {
		t.Error("Expected w2 to be evicted")
	}

	// Verify w1 is NOT evicted
	evicted, err = database.IsWorkerEvicted("w1")
	if err != nil {
		t.Fatalf("Failed to check eviction: %v", err)
	}
	if evicted {
		t.Error("Expected w1 to NOT be evicted")
	}

	// Verify GetIdleWorkers excludes evicted w2
	// Make w2 idle first
	_ = database.UpdateWorkerStatus("w2", protocol.WorkerStatusIdle)
	_ = database.UpdateWorkerStatus("w1", protocol.WorkerStatusIdle)

	idleWorkers, err := database.GetIdleWorkers()
	if err != nil {
		t.Fatalf("Failed to get idle workers: %v", err)
	}
	for _, w := range idleWorkers {
		if w.ID == "w2" {
			t.Error("Evicted worker w2 should not appear in idle workers list")
		}
	}

	// Clear w2 eviction
	if err := database.ClearWorkerEviction("w2"); err != nil {
		t.Fatalf("Failed to clear eviction: %v", err)
	}

	evicted, err = database.IsWorkerEvicted("w2")
	if err != nil {
		t.Fatalf("Failed to check eviction: %v", err)
	}
	if evicted {
		t.Error("Expected w2 to NOT be evicted after clear")
	}
}

func TestSlowNodeEvictionEvents(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	// Create a worker
	_, err := database.CreateWorker("w1", "worker-1", protocol.WorkerCapabilities{
		Encoders:      []string{"libx264"},
		FFmpegVersion: "6.0",
	})
	if err != nil {
		t.Fatalf("Failed to create worker: %v", err)
	}

	// Record an eviction event
	evt, err := database.CreateEvictionEvent("w1", db.EvictionEventEvicted, 10.0, 100.0, "throughput below threshold")
	if err != nil {
		t.Fatalf("Failed to create eviction event: %v", err)
	}
	if evt.EventType != db.EvictionEventEvicted {
		t.Errorf("Expected event type 'evicted', got %s", evt.EventType)
	}
	if evt.CurrentThroughput != 10.0 {
		t.Errorf("Expected throughput 10.0, got %.2f", evt.CurrentThroughput)
	}
	if evt.ClusterMedian != 100.0 {
		t.Errorf("Expected median 100.0, got %.2f", evt.ClusterMedian)
	}

	// Record a recovery event
	evt2, err := database.CreateEvictionEvent("w1", db.EvictionEventRecovered, 80.0, 100.0, "throughput recovered")
	if err != nil {
		t.Fatalf("Failed to create recovery event: %v", err)
	}
	if evt2.EventType != db.EvictionEventRecovered {
		t.Errorf("Expected event type 'recovered', got %s", evt2.EventType)
	}

	// Retrieve events
	events, err := database.GetEvictionEvents(10, 0)
	if err != nil {
		t.Fatalf("Failed to get eviction events: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("Expected 2 events, got %d", len(events))
	}

	// Most recent event should be recovery
	if events[0].EventType != db.EvictionEventRecovered {
		t.Errorf("Expected most recent event to be 'recovered', got %s", events[0].EventType)
	}

	// Retrieve events by worker
	workerEvents, err := database.GetEvictionEventsByWorker("w1", 10)
	if err != nil {
		t.Fatalf("Failed to get eviction events by worker: %v", err)
	}
	if len(workerEvents) != 2 {
		t.Fatalf("Expected 2 events for w1, got %d", len(workerEvents))
	}
}

func TestSlowNodeEvictionViaMonitor(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	// Create 3 workers
	for _, w := range []struct {
		id   string
		name string
	}{
		{"w1", "worker-1"},
		{"w2", "worker-2"},
		{"w3", "worker-3"},
	} {
		_, err := database.CreateWorker(w.id, w.name, protocol.WorkerCapabilities{
			Encoders:      []string{"libx264"},
			FFmpegVersion: "6.0",
		})
		if err != nil {
			t.Fatalf("Failed to create worker %s: %v", w.id, err)
		}
	}

	// Create state table with 3 workers: w3 is slow (throughput 10 vs median 100)
	stateTable := NewWorkerStateTable(30 * time.Second)
	stateTable.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
		WorkerID: "w1", Status: "online", ThroughputFPS: 100, CompletedJobs: MinJobsForEviction, Timestamp: time.Now(),
	})
	stateTable.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
		WorkerID: "w2", Status: "online", ThroughputFPS: 110, CompletedJobs: MinJobsForEviction, Timestamp: time.Now(),
	})
	stateTable.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
		WorkerID: "w3", Status: "online", ThroughputFPS: 10, CompletedJobs: MinJobsForEviction, Timestamp: time.Now(),
	})

	// Create monitor and set state table
	monitor := New(database, Config{
		HeartbeatTimeout:    30 * time.Second,
		OfflineThreshold:    10 * time.Minute,
		HealthCheckInterval: 1 * time.Second,
		MaxRetryCount:       3,
	})
	mockSched := &mockScheduler{}
	monitor.SetScheduler(mockSched)
	monitor.SetStateTable(stateTable)

	// Run detectSlowWorkers directly
	monitor.detectSlowWorkers()

	// Verify w3 is evicted in DB
	evicted, err := database.IsWorkerEvicted("w3")
	if err != nil {
		t.Fatalf("Failed to check eviction: %v", err)
	}
	if !evicted {
		t.Error("Expected w3 to be evicted in DB")
	}

	// Verify eviction event was recorded
	events, err := database.GetEvictionEventsByWorker("w3", 10)
	if err != nil {
		t.Fatalf("Failed to get eviction events: %v", err)
	}
	if len(events) < 1 {
		t.Fatal("Expected at least 1 eviction event for w3")
	}
	if events[0].EventType != db.EvictionEventEvicted {
		t.Errorf("Expected event type 'evicted', got %s", events[0].EventType)
	}

	// Verify scheduler was triggered
	if !mockSched.rescheduleCalled {
		t.Error("Expected scheduler to be triggered after eviction")
	}
}

func TestSlowNodeRecoveryViaMonitor(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	// Create 3 workers
	for _, w := range []struct {
		id   string
		name string
	}{
		{"w1", "worker-1"},
		{"w2", "worker-2"},
		{"w3", "worker-3"},
	} {
		_, err := database.CreateWorker(w.id, w.name, protocol.WorkerCapabilities{
			Encoders:      []string{"libx264"},
			FFmpegVersion: "6.0",
		})
		if err != nil {
			t.Fatalf("Failed to create worker %s: %v", w.id, err)
		}
	}

	// Create state table: w3 is slow
	stateTable := NewWorkerStateTable(30 * time.Second)
	stateTable.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
		WorkerID: "w1", Status: "online", ThroughputFPS: 100, CompletedJobs: MinJobsForEviction, Timestamp: time.Now(),
	})
	stateTable.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
		WorkerID: "w2", Status: "online", ThroughputFPS: 110, CompletedJobs: MinJobsForEviction, Timestamp: time.Now(),
	})
	stateTable.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
		WorkerID: "w3", Status: "online", ThroughputFPS: 10, CompletedJobs: MinJobsForEviction, Timestamp: time.Now(),
	})

	monitor := New(database, Config{
		HeartbeatTimeout:    30 * time.Second,
		OfflineThreshold:    10 * time.Minute,
		HealthCheckInterval: 1 * time.Second,
		MaxRetryCount:       3,
	})
	monitor.SetStateTable(stateTable)

	// First: detect w3 as slow
	monitor.detectSlowWorkers()

	// Verify w3 is evicted
	evicted, _ := database.IsWorkerEvicted("w3")
	if !evicted {
		t.Fatal("Expected w3 to be evicted")
	}

	// Now simulate w3 recovering: many heartbeats with high throughput
	for i := 0; i < 15; i++ {
		stateTable.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
			WorkerID: "w3", Status: "online", ThroughputFPS: 90, CompletedJobs: MinJobsForEviction, Timestamp: time.Now(),
		})
	}

	// Run detection again — w3 should recover
	monitor.detectSlowWorkers()

	// Verify w3 is no longer evicted
	evicted, err := database.IsWorkerEvicted("w3")
	if err != nil {
		t.Fatalf("Failed to check eviction: %v", err)
	}
	if evicted {
		t.Error("Expected w3 to be recovered (no longer evicted)")
	}

	// Verify recovery event was recorded
	events, err := database.GetEvictionEventsByWorker("w3", 10)
	if err != nil {
		t.Fatalf("Failed to get eviction events: %v", err)
	}

	// Should have at least one recovery event (most recent)
	hasRecovery := false
	for _, e := range events {
		if e.EventType == db.EvictionEventRecovered {
			hasRecovery = true
			break
		}
	}
	if !hasRecovery {
		t.Error("Expected at least one recovery event for w3")
	}
}

func TestSlowNodeEvictionClearedWhenClusterShrinks(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	// Create 3 workers.
	for _, w := range []struct {
		id   string
		name string
	}{
		{"w1", "worker-1"},
		{"w2", "worker-2"},
		{"w3", "worker-3"},
	} {
		if _, err := database.CreateWorker(w.id, w.name, protocol.WorkerCapabilities{
			Encoders:      []string{"libx264"},
			FFmpegVersion: "6.0",
		}); err != nil {
			t.Fatalf("Failed to create worker %s: %v", w.id, err)
		}
	}

	stateTable := NewWorkerStateTable(30 * time.Second)
	stateTable.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{WorkerID: "w1", Status: "online", ThroughputFPS: 100, CompletedJobs: MinJobsForEviction, Timestamp: time.Now()})
	stateTable.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{WorkerID: "w2", Status: "online", ThroughputFPS: 110, CompletedJobs: MinJobsForEviction, Timestamp: time.Now()})
	stateTable.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{WorkerID: "w3", Status: "online", ThroughputFPS: 10, CompletedJobs: MinJobsForEviction, Timestamp: time.Now()})

	monitor := New(database, Config{
		HeartbeatTimeout:    30 * time.Second,
		OfflineThreshold:    10 * time.Minute,
		HealthCheckInterval: 1 * time.Second,
		MaxRetryCount:       3,
	})
	monitor.SetStateTable(stateTable)

	// First detection: w3 is slow and gets evicted in the DB.
	monitor.detectSlowWorkers()
	evicted, _ := database.IsWorkerEvicted("w3")
	if !evicted {
		t.Fatal("Expected w3 to be evicted in DB")
	}

	// Cluster shrinks to a single worker: w1 and w2 leave.
	stateTable.RemoveWorker("w1")
	stateTable.RemoveWorker("w2")

	// Second detection must clear w3's DB eviction so it becomes schedulable.
	monitor.detectSlowWorkers()
	evicted, err := database.IsWorkerEvicted("w3")
	if err != nil {
		t.Fatalf("Failed to check eviction: %v", err)
	}
	if evicted {
		t.Error("Expected w3 to no longer be evicted in DB after cluster shrink")
	}
}

// --- Multi-Worker Failover Tests (TSI-1641 Scene 10.1) ---

func TestMultiWorkerHeartbeatTimeout(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	// Create 3 workers
	for _, w := range []struct {
		id   string
		name string
	}{
		{"worker-1", "worker-alpha"},
		{"worker-2", "worker-beta"},
		{"worker-3", "worker-gamma"},
	} {
		_, err := database.CreateWorker(w.id, w.name, protocol.WorkerCapabilities{
			Encoders:      []string{"libx264"},
			FFmpegVersion: "6.0",
		})
		if err != nil {
			t.Fatalf("Failed to create worker %s: %v", w.id, err)
		}
	}

	// Make all 3 workers' heartbeats stale
	for _, wid := range []string{"worker-1", "worker-2", "worker-3"} {
		_, err := database.GetDB().Exec(`
			UPDATE workers SET last_heartbeat = ? WHERE id = ?
		`, time.Now().Add(-60*time.Second), wid)
		if err != nil {
			t.Fatalf("Failed to update worker %s heartbeat: %v", wid, err)
		}
	}

	// Create monitor and run check
	monitor := New(database, Config{
		HeartbeatTimeout:    30 * time.Second,
		OfflineThreshold:    10 * time.Minute,
		HealthCheckInterval: 1 * time.Second,
		MaxRetryCount:       3,
	})
	mockSched := &mockScheduler{}
	monitor.SetScheduler(mockSched)

	monitor.checkWorkers()

	// Verify all 3 workers are now offline
	for _, wid := range []string{"worker-1", "worker-2", "worker-3"} {
		worker, err := database.GetWorker(wid)
		if err != nil {
			t.Fatalf("Failed to get worker %s: %v", wid, err)
		}
		if worker.Status != protocol.WorkerStatusOffline {
			t.Errorf("Expected worker %s to be offline, got %s", wid, worker.Status)
		}
	}

	// Scheduler should be triggered after batch detection
	if mockSched.rescheduleCalled {
		t.Error("Expected scheduler NOT to be triggered when workers have no jobs to migrate")
	}
}

func TestMultiWorkerJobMigration(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	// Create 3 workers
	for _, w := range []struct {
		id   string
		name string
	}{
		{"worker-1", "worker-alpha"},
		{"worker-2", "worker-beta"},
		{"worker-3", "worker-gamma"},
	} {
		_, err := database.CreateWorker(w.id, w.name, protocol.WorkerCapabilities{
			Encoders:      []string{"libx264"},
			FFmpegVersion: "6.0",
		})
		if err != nil {
			t.Fatalf("Failed to create worker %s: %v", w.id, err)
		}
	}

	// Create and assign jobs to each worker with different statuses
	job1, _ := database.CreateJob(`["input_1a.mkv"]`, `["-c:v","libx264"]`, "output_1a.mkv", false)
	job2, _ := database.CreateJob(`["input_1b.mkv"]`, `["-c:v","libx264"]`, "output_1b.mkv", false)
	database.AssignJobToWorker(job1.ID, "worker-1")
	database.AssignJobToWorker(job2.ID, "worker-1")
	database.UpdateJobStatusWithFailure(job1.ID, protocol.JobStatusRunning, nil, nil, nil, nil)
	database.UpdateJobStatusWithFailure(job2.ID, protocol.JobStatusQueued, nil, nil, nil, nil)

	job3, _ := database.CreateJob(`["input_2a.mkv"]`, `["-c:v","libx264"]`, "output_2a.mkv", false)
	database.AssignJobToWorker(job3.ID, "worker-2")
	database.UpdateJobStatusWithFailure(job3.ID, protocol.JobStatusRunning, nil, nil, nil, nil)

	job4, _ := database.CreateJob(`["input_3a.mkv"]`, `["-c:v","libx264"]`, "output_3a.mkv", false)
	database.AssignJobToWorker(job4.ID, "worker-3")
	database.UpdateJobStatusWithFailure(job4.ID, protocol.JobStatusQueued, nil, nil, nil, nil)

	// Make all 3 workers' heartbeats stale
	for _, wid := range []string{"worker-1", "worker-2", "worker-3"} {
		_, err := database.GetDB().Exec(`
			UPDATE workers SET last_heartbeat = ? WHERE id = ?
		`, time.Now().Add(-60*time.Second), wid)
		if err != nil {
			t.Fatalf("Failed to update worker %s heartbeat: %v", wid, err)
		}
	}

	// Create monitor and run check
	monitor := New(database, Config{
		HeartbeatTimeout:    30 * time.Second,
		OfflineThreshold:    10 * time.Minute,
		HealthCheckInterval: 1 * time.Second,
		MaxRetryCount:       3,
	})
	mockSched := &mockScheduler{}
	monitor.SetScheduler(mockSched)

	monitor.checkWorkers()

	// Verify all jobs are back to pending
	for _, job := range []*db.Job{job1, job2, job3, job4} {
		jobAfter, err := database.GetJob(job.ID)
		if err != nil {
			t.Fatalf("Failed to get job %s: %v", job.ID, err)
		}
		if jobAfter.Status != protocol.JobStatusPending {
			t.Errorf("Expected job %s to be pending, got %s", job.ID, jobAfter.Status)
		}
	}

	// Verify migration events were created for each worker
	for _, wid := range []string{"worker-1", "worker-2", "worker-3"} {
		events, err := database.GetMigrationEventsByWorker(wid, 10)
		if err != nil {
			t.Fatalf("Failed to get migration events for %s: %v", wid, err)
		}
		if len(events) != 1 {
			t.Errorf("Expected 1 migration event for %s, got %d", wid, len(events))
		}
	}

	allEvents, err := database.GetMigrationEvents(10, 0)
	if err != nil {
		t.Fatalf("Failed to get all migration events: %v", err)
	}
	if len(allEvents) != 3 {
		t.Errorf("Expected 3 total migration events, got %d", len(allEvents))
	}
}

func TestMultiWorkerPartialFailure(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	// Create 3 workers
	for _, w := range []struct {
		id   string
		name string
	}{
		{"worker-A", "worker-alpha"},
		{"worker-B", "worker-beta"},
		{"worker-C", "worker-gamma"},
	} {
		_, err := database.CreateWorker(w.id, w.name, protocol.WorkerCapabilities{
			Encoders:      []string{"libx264"},
			FFmpegVersion: "6.0",
		})
		if err != nil {
			t.Fatalf("Failed to create worker %s: %v", w.id, err)
		}
	}

	// Assign a running job to each worker
	jobs := make([]*db.Job, 3)
	for i, wid := range []string{"worker-A", "worker-B", "worker-C"} {
		job, err := database.CreateJob(`["input.mkv"]`, `["-c:v","libx264"]`, "output.mkv", false)
		if err != nil {
			t.Fatalf("Failed to create job for %s: %v", wid, err)
		}
		database.AssignJobToWorker(job.ID, wid)
		database.UpdateJobStatusWithFailure(job.ID, protocol.JobStatusRunning, nil, nil, nil, nil)
		jobs[i] = job
	}

	// Only worker-A and worker-C go offline; worker-B stays healthy
	for _, wid := range []string{"worker-A", "worker-C"} {
		_, err := database.GetDB().Exec(`UPDATE workers SET last_heartbeat = ? WHERE id = ?`,
			time.Now().Add(-60*time.Second), wid)
		if err != nil {
			t.Fatalf("Failed to update worker %s heartbeat: %v", wid, err)
		}
	}
	database.GetDB().Exec(`UPDATE workers SET last_heartbeat = ? WHERE id = ?`,
		time.Now(), "worker-B")

	monitor := New(database, Config{
		HeartbeatTimeout:    30 * time.Second,
		OfflineThreshold:    10 * time.Minute,
		HealthCheckInterval: 1 * time.Second,
		MaxRetryCount:       3,
	})
	mockSched := &mockScheduler{}
	monitor.SetScheduler(mockSched)

	monitor.checkWorkers()

	// worker-A and worker-C should be offline
	for _, wid := range []string{"worker-A", "worker-C"} {
		worker, _ := database.GetWorker(wid)
		if worker.Status != protocol.WorkerStatusOffline {
			t.Errorf("Expected worker %s to be offline, got %s", wid, worker.Status)
		}
	}

	// worker-B should still be healthy
	workerB, _ := database.GetWorker("worker-B")
	if workerB.Status == protocol.WorkerStatusOffline {
		t.Error("Expected worker-B to remain healthy, got offline")
	}

	// worker-A and worker-C jobs should be pending; worker-B's stays running
	jobA, _ := database.GetJob(jobs[0].ID)
	if jobA.Status != protocol.JobStatusPending {
		t.Errorf("Expected worker-A job pending, got %s", jobA.Status)
	}
	jobB, _ := database.GetJob(jobs[1].ID)
	if jobB.Status != protocol.JobStatusRunning {
		t.Errorf("Expected worker-B job still running, got %s", jobB.Status)
	}
	jobC, _ := database.GetJob(jobs[2].ID)
	if jobC.Status != protocol.JobStatusPending {
		t.Errorf("Expected worker-C job pending, got %s", jobC.Status)
	}

	// Only 2 migration events (for worker-A and worker-C)
	events, _ := database.GetMigrationEvents(10, 0)
	if len(events) != 2 {
		t.Errorf("Expected 2 migration events, got %d", len(events))
	}
}

func TestMultiWorkerMixedJobStates(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	// Create 2 workers
	for _, w := range []struct {
		id   string
		name string
	}{
		{"worker-A", "worker-alpha"},
		{"worker-B", "worker-beta"},
	} {
		_, err := database.CreateWorker(w.id, w.name, protocol.WorkerCapabilities{
			Encoders:      []string{"libx264"},
			FFmpegVersion: "6.0",
		})
		if err != nil {
			t.Fatalf("Failed to create worker %s: %v", w.id, err)
		}
	}

	// worker-A: running + queued + completed jobs
	jobRun, _ := database.CreateJob(`["run.mkv"]`, `["-c:v","libx264"]`, "out_run.mkv", false)
	jobQueue, _ := database.CreateJob(`["queue.mkv"]`, `["-c:v","libx264"]`, "out_queue.mkv", false)
	jobDone, _ := database.CreateJob(`["done.mkv"]`, `["-c:v","libx264"]`, "out_done.mkv", false)
	database.AssignJobToWorker(jobRun.ID, "worker-A")
	database.AssignJobToWorker(jobQueue.ID, "worker-A")
	database.AssignJobToWorker(jobDone.ID, "worker-A")
	database.UpdateJobStatusWithFailure(jobRun.ID, protocol.JobStatusRunning, nil, nil, nil, nil)
	database.UpdateJobStatusWithFailure(jobQueue.ID, protocol.JobStatusQueued, nil, nil, nil, nil)
	database.UpdateJobStatusWithFailure(jobDone.ID, protocol.JobStatusCompleted, nil, nil, nil, nil)

	// worker-B: one queued job
	jobQ2, _ := database.CreateJob(`["q2.mkv"]`, `["-c:v","libx264"]`, "out_q2.mkv", false)
	database.AssignJobToWorker(jobQ2.ID, "worker-B")
	database.UpdateJobStatusWithFailure(jobQ2.ID, protocol.JobStatusQueued, nil, nil, nil, nil)

	// Both workers go offline
	for _, wid := range []string{"worker-A", "worker-B"} {
		database.GetDB().Exec(`UPDATE workers SET last_heartbeat = ? WHERE id = ?`,
			time.Now().Add(-60*time.Second), wid)
	}

	monitor := New(database, Config{
		HeartbeatTimeout:    30 * time.Second,
		OfflineThreshold:    10 * time.Minute,
		HealthCheckInterval: 1 * time.Second,
		MaxRetryCount:       3,
	})
	monitor.SetScheduler(&mockScheduler{})
	monitor.checkWorkers()

	// Only running/queued jobs migrated; completed stays as-is
	jobAfter, _ := database.GetJob(jobRun.ID)
	if jobAfter.Status != protocol.JobStatusPending {
		t.Errorf("Expected running job pending, got %s", jobAfter.Status)
	}
	jobAfter, _ = database.GetJob(jobQueue.ID)
	if jobAfter.Status != protocol.JobStatusPending {
		t.Errorf("Expected queued job pending, got %s", jobAfter.Status)
	}
	jobAfter, _ = database.GetJob(jobDone.ID)
	if jobAfter.Status != protocol.JobStatusCompleted {
		t.Errorf("Expected completed job to stay completed, got %s", jobAfter.Status)
	}
	jobAfter, _ = database.GetJob(jobQ2.ID)
	if jobAfter.Status != protocol.JobStatusPending {
		t.Errorf("Expected worker-B queued job pending, got %s", jobAfter.Status)
	}

	// 2 migration events
	events, _ := database.GetMigrationEvents(10, 0)
	if len(events) != 2 {
		t.Errorf("Expected 2 migration events, got %d", len(events))
	}
}

func TestMultiWorkerRetryCountTracking(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	// Create 2 workers
	for _, w := range []struct {
		id   string
		name string
	}{
		{"worker-A", "worker-alpha"},
		{"worker-B", "worker-beta"},
	} {
		_, err := database.CreateWorker(w.id, w.name, protocol.WorkerCapabilities{
			Encoders:      []string{"libx264"},
			FFmpegVersion: "6.0",
		})
		if err != nil {
			t.Fatalf("Failed to create worker %s: %v", w.id, err)
		}
	}

	// worker-A: job with 2 previous migrations
	job1, _ := database.CreateJob(`["retry.mkv"]`, `["-c:v","libx264"]`, "out_retry.mkv", false)
	database.AssignJobToWorker(job1.ID, "worker-A")
	database.UpdateJobStatusWithFailure(job1.ID, protocol.JobStatusRunning, nil, nil, nil, nil)

	database.CreateMigrationEvent("old-w1", "old-1", string(migration.ReasonHeartbeatTimeout), 0, []string{job1.ID}, 1)
	database.CreateMigrationEvent("old-w2", "old-2", string(migration.ReasonHeartbeatTimeout), 1, []string{job1.ID}, 1)

	// worker-B: fresh job, no prior migrations
	job2, _ := database.CreateJob(`["fresh.mkv"]`, `["-c:v","libx264"]`, "out_fresh.mkv", false)
	database.AssignJobToWorker(job2.ID, "worker-B")
	database.UpdateJobStatusWithFailure(job2.ID, protocol.JobStatusRunning, nil, nil, nil, nil)

	// Both workers go offline
	for _, wid := range []string{"worker-A", "worker-B"} {
		database.GetDB().Exec(`UPDATE workers SET last_heartbeat = ? WHERE id = ?`,
			time.Now().Add(-60*time.Second), wid)
	}

	monitor := New(database, Config{
		HeartbeatTimeout:    30 * time.Second,
		OfflineThreshold:    10 * time.Minute,
		HealthCheckInterval: 1 * time.Second,
		MaxRetryCount:       3,
	})
	monitor.SetScheduler(&mockScheduler{})
	monitor.checkWorkers()

	// worker-A retry count should be 2 (previous migrations)
	eventsA, _ := database.GetMigrationEventsByWorker("worker-A", 10)
	if len(eventsA) != 1 {
		t.Fatalf("Expected 1 migration event for worker-A, got %d", len(eventsA))
	}
	if eventsA[0].RetryCount != 2 {
		t.Errorf("Expected retry count 2 for worker-A, got %d", eventsA[0].RetryCount)
	}

	// worker-B retry count should be 0 (fresh)
	eventsB, _ := database.GetMigrationEventsByWorker("worker-B", 10)
	if len(eventsB) != 1 {
		t.Fatalf("Expected 1 migration event for worker-B, got %d", len(eventsB))
	}
	if eventsB[0].RetryCount != 0 {
		t.Errorf("Expected retry count 0 for worker-B, got %d", eventsB[0].RetryCount)
	}

	// Both jobs should be pending after migration
	for _, job := range []*db.Job{job1, job2} {
		jobAfter, _ := database.GetJob(job.ID)
		if jobAfter.Status != protocol.JobStatusPending {
			t.Errorf("Expected job %s pending, got %s", job.ID, jobAfter.Status)
		}
	}
}

// TestWorkerFailoverE2E verifies the complete worker failover flow:
// kill worker-A -> heartbeat timeout detection -> job re-enqueue -> worker-B takeover
func TestWorkerFailoverE2E(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	// Create 2 workers with identical capabilities
	_, err := database.CreateWorker("worker-A", "worker-alpha", protocol.WorkerCapabilities{
		Encoders:      []string{"libx264"},
		FFmpegVersion: "6.0",
		MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatalf("Failed to create worker-A: %v", err)
	}

	_, err = database.CreateWorker("worker-B", "worker-beta", protocol.WorkerCapabilities{
		Encoders:      []string{"libx264"},
		FFmpegVersion: "6.0",
		MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatalf("Failed to create worker-B: %v", err)
	}

	// Create a job and assign to worker-A, set to running
	job, err := database.CreateJob(`["input.mkv"]`, `["-c:v","libx264"]`, "output.mkv", false)
	if err != nil {
		t.Fatalf("Failed to create job: %v", err)
	}

	err = database.AssignJobToWorker(job.ID, "worker-A")
	if err != nil {
		t.Fatalf("Failed to assign job to worker-A: %v", err)
	}

	err = database.UpdateJobStatusWithFailure(job.ID, protocol.JobStatusRunning, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("Failed to set job running: %v", err)
	}

	// Set worker-A status to busy
	_ = database.UpdateWorkerStatus("worker-A", protocol.WorkerStatusBusy)

	// Make worker-A's heartbeat stale (simulating crash/network loss)
	_, err = database.GetDB().Exec(`
		UPDATE workers SET last_heartbeat = ? WHERE id = ?
	`, time.Now().Add(-60*time.Second), "worker-A")
	if err != nil {
		t.Fatalf("Failed to update worker-A heartbeat: %v", err)
	}

	// Keep worker-B healthy
	_ = database.UpdateWorkerStatus("worker-B", protocol.WorkerStatusIdle)

	// Create monitor with scheduler
	monitor := New(database, Config{
		HeartbeatTimeout:    30 * time.Second,
		OfflineThreshold:    10 * time.Minute,
		HealthCheckInterval: 1 * time.Second,
		MaxRetryCount:       3,
	})

	mockSched := &mockScheduler{}
	monitor.SetScheduler(mockSched)

	// Run health check: should detect worker-A offline and migrate its job
	monitor.checkWorkers()

	// Verify worker-A is now offline
	workerA, err := database.GetWorker("worker-A")
	if err != nil {
		t.Fatalf("Failed to get worker-A: %v", err)
	}
	if workerA.Status != protocol.WorkerStatusOffline {
		t.Errorf("Expected worker-A to be offline, got %s", workerA.Status)
	}

	// Verify job was migrated (reset to pending)
	jobAfter, err := database.GetJob(job.ID)
	if err != nil {
		t.Fatalf("Failed to get job: %v", err)
	}
	if jobAfter.Status != protocol.JobStatusPending {
		t.Errorf("Expected job status pending, got %s", jobAfter.Status)
	}
	if jobAfter.WorkerID.Valid {
		t.Errorf("Expected job worker_id to be NULL after migration, got %s", jobAfter.WorkerID.String)
	}

	// Verify scheduler was triggered
	if !mockSched.rescheduleCalled {
		t.Error("Expected scheduler to be triggered after job migration")
	}

	// Now simulate scheduler picking up the pending job and assigning to worker-B
	// The scheduler uses GetPendingJobs and GetIdleWorkers
	pendingJobs, err := database.GetPendingJobs(10)
	if err != nil {
		t.Fatalf("Failed to get pending jobs: %v", err)
	}
	if len(pendingJobs) != 1 {
		t.Fatalf("Expected 1 pending job, got %d", len(pendingJobs))
	}
	if pendingJobs[0].ID != job.ID {
		t.Errorf("Expected pending job %s, got %s", job.ID, pendingJobs[0].ID)
	}

	// Get idle workers (should be only worker-B, since worker-A is offline)
	idleWorkers, err := database.GetIdleWorkers()
	if err != nil {
		t.Fatalf("Failed to get idle workers: %v", err)
	}
	if len(idleWorkers) != 1 {
		t.Fatalf("Expected 1 idle worker, got %d", len(idleWorkers))
	}
	if idleWorkers[0].ID != "worker-B" {
		t.Errorf("Expected idle worker to be worker-B, got %s", idleWorkers[0].ID)
	}

	// Assign job to worker-B (simulating scheduler action)
	err = database.AssignJobToWorker(job.ID, "worker-B")
	if err != nil {
		t.Fatalf("Failed to assign job to worker-B: %v", err)
	}

	err = database.UpdateJobStatusWithFailure(job.ID, protocol.JobStatusQueued, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("Failed to set job queued: %v", err)
	}

	// Verify job is now assigned to worker-B
	finalJob, err := database.GetJob(job.ID)
	if err != nil {
		t.Fatalf("Failed to get final job: %v", err)
	}
	if finalJob.Status != protocol.JobStatusQueued {
		t.Errorf("Expected job status queued, got %s", finalJob.Status)
	}
	if !finalJob.WorkerID.Valid || finalJob.WorkerID.String != "worker-B" {
		t.Errorf("Expected job assigned to worker-B, got %v", finalJob.WorkerID)
	}

	// Verify migration event was recorded
	events, err := database.GetMigrationEventsByWorker("worker-A", 10)
	if err != nil {
		t.Fatalf("Failed to get migration events: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("Expected 1 migration event, got %d", len(events))
	}
	if events[0].JobsMigrated != 1 {
		t.Errorf("Expected 1 job migrated, got %d", events[0].JobsMigrated)
	}
}

// TestWorkerFailoverRetryLimit verifies that jobs exceeding MaxRetryCount are failed rather than migrated.
func TestWorkerFailoverRetryLimit(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	// Create 2 workers
	_, err := database.CreateWorker("worker-A", "worker-alpha", protocol.WorkerCapabilities{
		Encoders:      []string{"libx264"},
		FFmpegVersion: "6.0",
	})
	if err != nil {
		t.Fatalf("Failed to create worker-A: %v", err)
	}

	_, err = database.CreateWorker("worker-B", "worker-beta", protocol.WorkerCapabilities{
		Encoders:      []string{"libx264"},
		FFmpegVersion: "6.0",
	})
	if err != nil {
		t.Fatalf("Failed to create worker-B: %v", err)
	}

	// Create a job that has already been migrated 3 times (reached MaxRetryCount)
	job, err := database.CreateJob(`["input.mkv"]`, `["-c:v","libx264"]`, "output.mkv", false)
	if err != nil {
		t.Fatalf("Failed to create job: %v", err)
	}

	// Assign to worker-A and set running
	err = database.AssignJobToWorker(job.ID, "worker-A")
	if err != nil {
		t.Fatalf("Failed to assign job to worker-A: %v", err)
	}
	err = database.UpdateJobStatusWithFailure(job.ID, protocol.JobStatusRunning, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("Failed to set job running: %v", err)
	}

	// Create 3 previous migration events (simulating MaxRetryCount reached)
	for i := 0; i < 3; i++ {
		_, err = database.CreateMigrationEvent(
			"old-w"+string(rune('0'+i)),
			"old-worker",
			string(migration.ReasonHeartbeatTimeout),
			i,
			[]string{job.ID},
			1,
		)
		if err != nil {
			t.Fatalf("Failed to create previous migration event %d: %v", i, err)
		}
	}

	// Verify retry count is 3
	count, err := database.GetJobRetryCount(job.ID)
	if err != nil {
		t.Fatalf("Failed to get retry count: %v", err)
	}
	if count != 3 {
		t.Fatalf("Expected retry count 3, got %d", count)
	}

	// Make worker-A's heartbeat stale
	_, err = database.GetDB().Exec(`
		UPDATE workers SET last_heartbeat = ? WHERE id = ?
	`, time.Now().Add(-60*time.Second), "worker-A")
	if err != nil {
		t.Fatalf("Failed to update worker-A heartbeat: %v", err)
	}

	// Create monitor with MaxRetryCount=3
	monitor := New(database, Config{
		HeartbeatTimeout:    30 * time.Second,
		OfflineThreshold:    10 * time.Minute,
		HealthCheckInterval: 1 * time.Second,
		MaxRetryCount:       3,
	})
	monitor.SetScheduler(&mockScheduler{})

	// Run health check
	monitor.checkWorkers()

	// Verify job is marked as FAILED (not migrated back to pending)
	jobAfter, err := database.GetJob(job.ID)
	if err != nil {
		t.Fatalf("Failed to get job: %v", err)
	}
	if jobAfter.Status != protocol.JobStatusFailed {
		t.Errorf("Expected job status 'failed' after exceeding retry limit, got %s", jobAfter.Status)
	}
	if !jobAfter.Error.Valid || jobAfter.Error.String != "max retry count exceeded after repeated worker failures" {
		t.Errorf("Expected error message about max retries, got '%v'", jobAfter.Error)
	}

	// Verify NO new migration event was created for worker-A (job was failed, not migrated)
	events, err := database.GetMigrationEventsByWorker("worker-A", 10)
	if err != nil {
		t.Fatalf("Failed to get migration events: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("Expected 0 migration events for worker-A (job failed, not migrated), got %d", len(events))
	}
}
