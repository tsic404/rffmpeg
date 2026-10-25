package workerhealth

import (
	"testing"
	"time"

	"github.com/tsic404/rffmpeg/pkg/protocol"
	"github.com/tsic404/rffmpeg/pkg/server/db"
)

func seedEvictionPair(stateTable *WorkerStateTable, completed int) {
	now := time.Now()
	stateTable.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
		WorkerID: "worker-fast", Status: string(protocol.WorkerStatusBusy),
		JobsPerSec: 1.5, CompletedJobs: completed, Timestamp: now,
	})
	stateTable.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
		WorkerID: "worker-slow", Status: string(protocol.WorkerStatusBusy),
		JobsPerSec: 0.1, CompletedJobs: completed, Timestamp: now,
	})
}

func newEvictionTestMonitor(t *testing.T, database *db.Database, stateTable *WorkerStateTable) *Monitor {
	t.Helper()
	monitor := New(database, Config{
		HeartbeatTimeout:    30 * time.Second,
		OfflineThreshold:    10 * time.Minute,
		HealthCheckInterval: 1 * time.Second,
		MaxRetryCount:       3,
	})
	monitor.SetStateTable(stateTable)
	return monitor
}

func createEvictionPair(t *testing.T, database *db.Database) {
	t.Helper()
	for _, w := range []struct{ id, name string }{
		{"worker-fast", "fast-worker"},
		{"worker-slow", "slow-worker"},
	} {
		if _, err := database.CreateWorker(w.id, w.name, protocol.WorkerCapabilities{
			Encoders:      []string{"libx264"},
			FFmpegVersion: "n9.0.1",
			MaxConcurrent: 1,
		}); err != nil {
			t.Fatalf("Failed to create worker %s: %v", w.id, err)
		}
	}
}

// TestManualClearLeavesWorkerReEvictable covers the manual clear's effect on
// the judgement loop: releasing the in-memory mark must not hide the worker
// from the next decision, so a worker whose throughput is still below the
// threshold is re-evicted instead of sitting in a stale "already evicted"
// state. The cumulative completion count must survive the clear.
func TestManualClearLeavesWorkerReEvictable(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()
	createEvictionPair(t, database)

	stateTable := NewWorkerStateTable(30 * time.Second)
	for i := 1; i <= 8; i++ {
		seedEvictionPair(stateTable, i)
	}
	monitor := newEvictionTestMonitor(t, database, stateTable)
	monitor.detectSlowWorkers()

	if evicted, err := database.IsWorkerEvicted("worker-slow"); err != nil || !evicted {
		t.Fatalf("Expected worker-slow evicted before the clear, got evicted=%v err=%v", evicted, err)
	}
	state, _ := stateTable.Get("worker-slow")
	ewmaBeforeClear := state.EWMAJobsPerSec
	completedBeforeClear := state.CompletedJobs

	cleared, err := database.ClearWorkerEviction(
		"worker-slow", ewmaBeforeClear, stateTable.ClusterMedian(), db.EvictionReasonManualClear,
	)
	if err != nil {
		t.Fatalf("Failed to clear eviction: %v", err)
	}
	if !cleared {
		t.Fatal("Expected the manual clear to release the mark")
	}
	stateTable.ClearEviction("worker-slow")

	schedulable, err := database.GetSchedulableWorkers()
	if err != nil {
		t.Fatalf("Failed to get schedulable workers: %v", err)
	}
	found := false
	for _, w := range schedulable {
		if w.ID == "worker-slow" {
			found = true
		}
	}
	if !found {
		t.Error("Expected the manually cleared worker back in the schedulable pool")
	}
	if state, ok := stateTable.Get("worker-slow"); !ok || state.Evicted {
		t.Error("Expected the in-memory eviction mark to be cleared")
	}
	if state, ok := stateTable.Get("worker-slow"); !ok || state.CompletedJobs != completedBeforeClear {
		t.Errorf("Expected the completion count to survive the clear, want %d", completedBeforeClear)
	}

	// Still slow: one more interval of samples is a valid judgement, and it
	// must re-mark the worker.
	seedEvictionPair(stateTable, 9)
	monitor.detectSlowWorkers()

	if evicted, err := database.IsWorkerEvicted("worker-slow"); err != nil || !evicted {
		t.Fatalf("Expected the still-slow worker re-evicted, got evicted=%v err=%v", evicted, err)
	}
	state, _ = stateTable.Get("worker-slow")
	if !state.Evicted {
		t.Error("Expected the in-memory mark back after the re-eviction")
	}

	// The manual clear stays the only recovery row: the automatic path did not
	// fabricate a second one after the operator's action.
	events, err := database.GetEvictionEventsByWorker("worker-slow", 10)
	if err != nil {
		t.Fatalf("Failed to get eviction events: %v", err)
	}
	recoveries := 0
	for _, e := range events {
		if e.EventType == db.EvictionEventRecovered {
			recoveries++
			if e.DecisionReason != db.EvictionReasonManualClear {
				t.Errorf("Expected the only recovery row to carry the manual reason, got %q", e.DecisionReason)
			}
		}
	}
	if recoveries != 1 {
		t.Errorf("Expected exactly 1 recovery row, got %d", recoveries)
	}
}

// TestAutomaticRecoveryReason verifies the monitor passes the throughput
// recovery reason, so the two clear paths stay distinguishable in the audit.
func TestAutomaticRecoveryReason(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()
	createEvictionPair(t, database)

	stateTable := NewWorkerStateTable(30 * time.Second)
	for i := 1; i <= 8; i++ {
		seedEvictionPair(stateTable, i)
	}
	monitor := newEvictionTestMonitor(t, database, stateTable)
	monitor.detectSlowWorkers()
	if evicted, err := database.IsWorkerEvicted("worker-slow"); err != nil || !evicted {
		t.Fatalf("Expected worker-slow evicted first, got evicted=%v err=%v", evicted, err)
	}

	for range 15 {
		stateTable.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
			WorkerID: "worker-slow", Status: string(protocol.WorkerStatusBusy),
			JobsPerSec: 1.4, CompletedJobs: MinJobsForEviction + 20, Timestamp: time.Now(),
		})
	}
	monitor.detectSlowWorkers()

	events, err := database.GetEvictionEventsByWorker("worker-slow", 10)
	if err != nil {
		t.Fatalf("Failed to get eviction events: %v", err)
	}
	recoveries := 0
	for _, e := range events {
		if e.EventType != db.EvictionEventRecovered {
			continue
		}
		recoveries++
		if e.DecisionReason != db.EvictionReasonThroughputRecovered {
			t.Errorf("Expected the automatic recovery reason, got %q", e.DecisionReason)
		}
	}
	if recoveries != 1 {
		t.Errorf("Expected exactly 1 automatic recovery row, got %d", recoveries)
	}
}
