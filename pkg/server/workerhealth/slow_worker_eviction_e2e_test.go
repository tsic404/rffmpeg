package workerhealth

import (
	"testing"
	"time"

	"github.com/tsic404/rffmpeg/pkg/protocol"
	"github.com/tsic404/rffmpeg/pkg/server/db"
)

// TestSlowWorkerEvictionE2E_ThroughputDriven is the integration test distillation
// of the manual E2E verification (TSI-2432). It proves that slow-worker eviction
// works on throughput (completed jobs per heartbeat interval) alone — no GPU
// metrics required — mirroring a real cluster where two software-encoding workers
// diverge in throughput and the slow one is evicted mid-flight.
//
// E2E run that this test codifies (local, no nvidia-smi):
//   - server + 2 workers (libx264 ultrafast, no GPU)
//   - slow worker's ffmpeg was wrapped with `sleep 3` before exec, so it
//     completed far fewer jobs per 2s heartbeat than the fast worker
//   - after both cleared MinJobsForEviction, the monitor evicted the slow
//     worker: EWMA throughput below cluster median / SlowNodeThreshold, a
//     'worker EWMA throughput below cluster median threshold' audit event was
//     recorded, MarkWorkerEvicted set the DB flag, and the scheduler was
//     triggered to reschedule in-flight jobs away from the evicted node.
//
// GPU-metrics environment validation (a worker whose throughput is real but
// whose GPU utilization is the actual eviction signal) remains a leftover item
// for an nvidia-smi-equipped environment — this test covers the throughput
// path exclusively.
func TestSlowWorkerEvictionE2E_ThroughputDriven(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	// Two workers, both capable of libx264 (software encoding — what the E2E
	// run used). No GPU capability differences; eviction is throughput-only.
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

	stateTable := NewWorkerStateTable(30 * time.Second)

	// Simulate the EWMA convergence the real heartbeats produce: the fast
	// worker reports a high per-interval throughput (it completes jobs
	// quickly), the slow worker reports a low one (its 3s sleep dominates
	// the 2s heartbeat interval). Send enough heartbeats to push both past
	// MinJobsForEviction and to let the EWMA stabilize away from the first
	// sample — a single sample would make the eviction threshold a coin flip
	// on initialization noise rather than a converged signal.
	//
	// Fast worker: ~3 jobs/2s interval → 1.5 jobs/s per heartbeat.
	// Slow worker: ~0.2 jobs/2s interval → 0.1 jobs/s per heartbeat.
	// CompletedJobs advances each heartbeat (cumulative), crossing
	// MinJobsForEviction(5) after ~6 heartbeats.
	for i := 1; i <= 8; i++ {
		now := time.Now()
		stateTable.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
			WorkerID:      "worker-fast",
			Status:        string(protocol.WorkerStatusBusy),
			ThroughputFPS: 1.5,
			CompletedJobs: i,
			Timestamp:     now,
		})
		stateTable.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
			WorkerID:      "worker-slow",
			Status:        string(protocol.WorkerStatusBusy),
			ThroughputFPS: 0.1,
			CompletedJobs: i,
			Timestamp:     now,
		})
	}

	// Monitor wired exactly like cmd/server: state table set, scheduler set.
	monitor := New(database, Config{
		HeartbeatTimeout:    30 * time.Second,
		OfflineThreshold:    10 * time.Minute,
		HealthCheckInterval: 1 * time.Second,
		MaxRetryCount:       3,
	})
	mockSched := &mockScheduler{}
	monitor.SetScheduler(mockSched)
	monitor.SetStateTable(stateTable)

	// Run the eviction detection — this is what the monitor ticker fires
	// every HealthCheckInterval in the real server.
	monitor.detectSlowWorkers()

	// 1. The slow worker is evicted in the DB (MarkWorkerEvicted was called).
	evicted, err := database.IsWorkerEvicted("worker-slow")
	if err != nil {
		t.Fatalf("Failed to check slow worker eviction: %v", err)
	}
	if !evicted {
		t.Fatal("Expected worker-slow to be evicted in DB (throughput below median/SlowNodeThreshold)")
	}

	// 2. The fast worker is NOT evicted — eviction targets the slow node only.
	if evicted, _ := database.IsWorkerEvicted("worker-fast"); evicted {
		t.Error("Expected worker-fast to remain non-evicted")
	}

	// 3. An eviction audit event was recorded for the slow worker carrying
	// the throughput-below-median reason, so operators can trace the decision.
	events, err := database.GetEvictionEventsByWorker("worker-slow", 10)
	if err != nil {
		t.Fatalf("Failed to get eviction events for worker-slow: %v", err)
	}
	if len(events) == 0 {
		t.Fatal("Expected at least one eviction event for worker-slow")
	}
	if events[0].EventType != db.EvictionEventEvicted {
		t.Errorf("Expected most recent event type 'evicted', got %s", events[0].EventType)
	}
	if events[0].CurrentThroughput >= events[0].ClusterMedian {
		t.Errorf("Expected evicted worker throughput (%.4f) below cluster median (%.4f)",
			events[0].CurrentThroughput, events[0].ClusterMedian)
	}

	// 4. The scheduler was triggered to reschedule in-flight jobs away from
	// the newly-evicted worker (same path migration/failover uses).
	if !mockSched.rescheduleCalled {
		t.Error("Expected scheduler.TriggerReschedule to be called after eviction")
	}

	// 5. The evicted worker is excluded from the schedulable pool —
	// GetSchedulableWorkers filters evicted=1, so the slow node gets no new
	// work until it recovers.
	schedulable, err := database.GetSchedulableWorkers()
	if err != nil {
		t.Fatalf("Failed to get schedulable workers: %v", err)
	}
	for _, w := range schedulable {
		if w.ID == "worker-slow" {
			t.Error("Evicted worker-slow must not appear in the schedulable worker pool")
		}
	}
}

// TestSlowWorkerEvictionE2E_RecoveryAfterSpeedup verifies the companion path:
// once the previously-slow worker's throughput recovers within
// RecoveryThreshold of the cluster median, the monitor clears its eviction
// flag and records a recovery audit event — restoring it to the schedulable
// pool. This is the throughput-only recovery the E2E environment would observe
// if the slow worker's ffmpeg wrapper were removed.
func TestSlowWorkerEvictionE2E_RecoveryAfterSpeedup(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

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

	stateTable := NewWorkerStateTable(30 * time.Second)
	for i := 1; i <= 8; i++ {
		now := time.Now()
		stateTable.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
			WorkerID: "worker-fast", Status: string(protocol.WorkerStatusBusy),
			ThroughputFPS: 1.5, CompletedJobs: i, Timestamp: now,
		})
		stateTable.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
			WorkerID: "worker-slow", Status: string(protocol.WorkerStatusBusy),
			ThroughputFPS: 0.1, CompletedJobs: i, Timestamp: now,
		})
	}

	monitor := New(database, Config{
		HeartbeatTimeout:    30 * time.Second,
		OfflineThreshold:    10 * time.Minute,
		HealthCheckInterval: 1 * time.Second,
		MaxRetryCount:       3,
	})
	monitor.SetStateTable(stateTable)

	// Evict the slow worker first.
	monitor.detectSlowWorkers()
	if evicted, _ := database.IsWorkerEvicted("worker-slow"); !evicted {
		t.Fatal("Expected worker-slow to be evicted before recovery test")
	}

	// The slow worker speeds up: many high-throughput heartbeats drive its
	// EWMA above median/RecoveryThreshold. EWMA smoothing (α=0.4) converges
	// toward the new throughput over enough samples.
	for range 15 {
		stateTable.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
			WorkerID: "worker-slow", Status: string(protocol.WorkerStatusBusy),
			ThroughputFPS: 1.4, CompletedJobs: MinJobsForEviction + 20, Timestamp: time.Now(),
		})
	}

	monitor.detectSlowWorkers()

	// Eviction cleared in DB.
	if evicted, err := database.IsWorkerEvicted("worker-slow"); err != nil {
		t.Fatalf("Failed to check eviction after recovery: %v", err)
	} else if evicted {
		t.Error("Expected worker-slow eviction cleared after throughput recovery")
	}

	// Recovery audit event recorded.
	events, err := database.GetEvictionEventsByWorker("worker-slow", 10)
	if err != nil {
		t.Fatalf("Failed to get eviction events for worker-slow: %v", err)
	}
	hasRecovery := false
	for _, e := range events {
		if e.EventType == db.EvictionEventRecovered {
			hasRecovery = true
			break
		}
	}
	if !hasRecovery {
		t.Error("Expected a recovery audit event for worker-slow")
	}

	// Worker is back in the schedulable pool.
	schedulable, err := database.GetSchedulableWorkers()
	if err != nil {
		t.Fatalf("Failed to get schedulable workers after recovery: %v", err)
	}
	found := false
	for _, w := range schedulable {
		if w.ID == "worker-slow" {
			found = true
			break
		}
	}
	if !found {
		t.Error("Expected recovered worker-slow to be back in the schedulable pool")
	}
}
