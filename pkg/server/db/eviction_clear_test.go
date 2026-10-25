package db_test

import (
	"errors"
	"testing"

	"github.com/tsic404/rffmpeg/pkg/protocol"
	"github.com/tsic404/rffmpeg/pkg/server/db"
)

func createEvictionWorker(t *testing.T, database *db.Database, id, name string) {
	t.Helper()
	if _, err := database.CreateWorker(id, name, protocol.WorkerCapabilities{
		Encoders:      []string{"libx264"},
		FFmpegVersion: "6.0",
	}); err != nil {
		t.Fatalf("Failed to create worker %s: %v", id, err)
	}
}

// TestClearWorkerEvictionRecordsRecovery covers the clear contract: the flag
// drops together with one recovery audit row carrying the caller's reason and
// the decision values, and an already-clear worker is a successful no-op that
// writes nothing.
func TestClearWorkerEvictionRecordsRecovery(t *testing.T) {
	database, cleanup := setupDBTest(t)
	defer cleanup()

	createEvictionWorker(t, database, "w-slow", "slow-node")
	if err := database.MarkWorkerEvicted("w-slow"); err != nil {
		t.Fatalf("Failed to mark worker evicted: %v", err)
	}

	cleared, err := database.ClearWorkerEviction("w-slow", 0.1, 1.5, db.EvictionReasonManualClear)
	if err != nil {
		t.Fatalf("Failed to clear eviction: %v", err)
	}
	if !cleared {
		t.Fatal("Expected the eviction mark to be reported as cleared")
	}
	evicted, err := database.IsWorkerEvicted("w-slow")
	if err != nil {
		t.Fatalf("Failed to check eviction: %v", err)
	}
	if evicted {
		t.Error("Expected the eviction mark to be gone")
	}

	events, err := database.GetEvictionEventsByWorker("w-slow", 10)
	if err != nil {
		t.Fatalf("Failed to get eviction events: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("Expected exactly 1 recovery event, got %d", len(events))
	}
	if events[0].EventType != db.EvictionEventRecovered {
		t.Errorf("Expected event_type %q, got %q", db.EvictionEventRecovered, events[0].EventType)
	}
	if events[0].DecisionReason != db.EvictionReasonManualClear {
		t.Errorf("Expected reason %q, got %q", db.EvictionReasonManualClear, events[0].DecisionReason)
	}
	if events[0].CurrentThroughput != 0.1 || events[0].ClusterMedian != 1.5 {
		t.Errorf("Expected the decision values 0.1/1.5, got %v/%v",
			events[0].CurrentThroughput, events[0].ClusterMedian)
	}

	// Already clear: a successful no-op, and no second audit row.
	cleared, err = database.ClearWorkerEviction("w-slow", 0.2, 1.6, db.EvictionReasonManualClear)
	if err != nil {
		t.Fatalf("Clearing an unmarked worker must succeed, got %v", err)
	}
	if cleared {
		t.Error("Expected cleared=false for a worker that was not evicted")
	}
	events, err = database.GetEvictionEventsByWorker("w-slow", 10)
	if err != nil {
		t.Fatalf("Failed to get eviction events: %v", err)
	}
	if len(events) != 1 {
		t.Errorf("Expected the no-op to write no audit row, got %d events", len(events))
	}
}

// TestClearWorkerEvictionUnknownWorker pins the not-found contract: the clear
// path reports the missing row instead of a silent success.
func TestClearWorkerEvictionUnknownWorker(t *testing.T) {
	database, cleanup := setupDBTest(t)
	defer cleanup()

	cleared, err := database.ClearWorkerEviction("missing", 0, 0, db.EvictionReasonManualClear)
	if !errors.Is(err, protocol.ErrWorkerNotFound) {
		t.Fatalf("Expected ErrWorkerNotFound, got %v", err)
	}
	if cleared {
		t.Error("Expected cleared=false for an unknown worker")
	}
}

// TestEvictionRecoveryReasonsDistinguishClearPaths is the implementation-side
// assertion for the audit requirement: automatic throughput recovery and a
// manual clear must leave distinguishable recovery rows.
func TestEvictionRecoveryReasonsDistinguishClearPaths(t *testing.T) {
	database, cleanup := setupDBTest(t)
	defer cleanup()

	createEvictionWorker(t, database, "w-auto", "auto-node")
	createEvictionWorker(t, database, "w-manual", "manual-node")
	for _, id := range []string{"w-auto", "w-manual"} {
		if err := database.MarkWorkerEvicted(id); err != nil {
			t.Fatalf("Failed to mark %s evicted: %v", id, err)
		}
	}

	if _, err := database.ClearWorkerEviction("w-auto", 2.0, 2.4, db.EvictionReasonThroughputRecovered); err != nil {
		t.Fatalf("Failed to record automatic recovery: %v", err)
	}
	if _, err := database.ClearWorkerEviction("w-manual", 0.1, 2.4, db.EvictionReasonManualClear); err != nil {
		t.Fatalf("Failed to record manual clear: %v", err)
	}

	autoEvents, err := database.GetEvictionEventsByWorker("w-auto", 10)
	if err != nil {
		t.Fatalf("Failed to get automatic recovery events: %v", err)
	}
	manualEvents, err := database.GetEvictionEventsByWorker("w-manual", 10)
	if err != nil {
		t.Fatalf("Failed to get manual clear events: %v", err)
	}
	if len(autoEvents) != 1 || len(manualEvents) != 1 {
		t.Fatalf("Expected one recovery row per path, got %d and %d", len(autoEvents), len(manualEvents))
	}
	if autoEvents[0].DecisionReason != db.EvictionReasonThroughputRecovered {
		t.Errorf("Automatic recovery recorded reason %q", autoEvents[0].DecisionReason)
	}
	if manualEvents[0].DecisionReason != db.EvictionReasonManualClear {
		t.Errorf("Manual clear recorded reason %q", manualEvents[0].DecisionReason)
	}
	if autoEvents[0].DecisionReason == manualEvents[0].DecisionReason {
		t.Error("Automatic recovery and manual clear must be distinguishable in the audit")
	}
}

// TestGetWorkerByReference covers the single-object lookup the manual clear
// needs: identifier or registered name, with unknown/blank refs reported as
// not found.
func TestGetWorkerByReference(t *testing.T) {
	database, cleanup := setupDBTest(t)
	defer cleanup()

	createEvictionWorker(t, database, "w-ref", "ref-node")

	byID, err := database.GetWorkerByReference("w-ref")
	if err != nil {
		t.Fatalf("Failed to resolve by identifier: %v", err)
	}
	if byID.ID != "w-ref" {
		t.Errorf("Expected worker w-ref, got %s", byID.ID)
	}

	byName, err := database.GetWorkerByReference("ref-node")
	if err != nil {
		t.Fatalf("Failed to resolve by name: %v", err)
	}
	if byName.ID != "w-ref" {
		t.Errorf("Expected worker w-ref resolved by name, got %s", byName.ID)
	}

	for _, ref := range []string{"no-such-worker", "", "   "} {
		if _, err := database.GetWorkerByReference(ref); !errors.Is(err, protocol.ErrWorkerNotFound) {
			t.Errorf("Reference %q: expected ErrWorkerNotFound, got %v", ref, err)
		}
	}
}
