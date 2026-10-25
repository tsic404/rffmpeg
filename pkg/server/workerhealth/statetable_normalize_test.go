package workerhealth

import (
	"testing"
	"time"

	"github.com/tsic404/rffmpeg/pkg/protocol"
	"github.com/tsic404/rffmpeg/pkg/server/db"
)

// TestWorkerStateTableNormalizesWorkerID pins the key shape: heartbeats may
// carry a compact or upper-case UUID for a worker the database stores in
// canonical form, and every lookup must reach the same entry. Otherwise a mark
// cleared for the worker never reaches the in-memory sample and the panel loses
// the worker's live metrics.
func TestWorkerStateTableNormalizesWorkerID(t *testing.T) {
	const (
		canonical = "550e8400-e29b-41d4-a716-446655440000"
		compact   = "550E8400E29B41D4A716446655440000"
		peer      = "6ba7b810-9dad-11d1-80f4-00c04fd430c8"
		peerRaw   = "6BA7B8109DAD11D180F400C04FD430C8"
	)

	table := NewWorkerStateTable(30 * time.Second)
	now := time.Now()
	for i := 1; i <= 8; i++ {
		table.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
			WorkerID: peerRaw, Status: string(protocol.WorkerStatusBusy),
			JobsPerSec: 1.5, CompletedJobs: i, Timestamp: now,
		})
		table.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
			WorkerID: compact, Status: string(protocol.WorkerStatusBusy),
			JobsPerSec: 0.1, CompletedJobs: i, Timestamp: now,
		})
	}

	for _, state := range table.GetAll() {
		if state.WorkerID != db.NormalizeWorkerID(state.WorkerID) {
			t.Errorf("State table kept a non-canonical key %q", state.WorkerID)
		}
	}
	for _, ref := range []string{canonical, compact} {
		if _, ok := table.Get(ref); !ok {
			t.Fatalf("State not reachable through %q", ref)
		}
	}
	// One entry per worker, not one per id spelling.
	if got := len(table.GetAll()); got != 2 {
		t.Fatalf("Expected 2 state entries, got %d", got)
	}

	table.DetectSlowWorkers()
	if state, ok := table.Get(canonical); !ok || !state.Evicted {
		t.Fatalf("Expected the slow worker marked under the canonical id, got %+v", state)
	}

	table.ClearEviction(compact)
	if state, _ := table.Get(canonical); state.Evicted {
		t.Error("Clearing through the compact form did not release the canonical mark")
	}

	table.RemoveWorker(peerRaw)
	if _, ok := table.Get(peer); ok {
		t.Error("Removing through the raw form did not drop the canonical entry")
	}
}
