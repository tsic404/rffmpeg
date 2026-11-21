package db_test

import (
	"errors"
	"testing"
	"time"

	"github.com/tsic404/rffmpeg/pkg/protocol"
)

// TestEvictionMarkSurvivesRestartRecovery pins the persistence boundary a
// Server restart relies on: recovery marks every worker offline, the startup
// stale-row sweep may delete unmarked rows, but an evicted row survives with its
// mark and keeps it through same-id re-registration.
func TestEvictionMarkSurvivesRestartRecovery(t *testing.T) {
	database, cleanup := setupDBTest(t)
	defer cleanup()

	caps := protocol.WorkerCapabilities{
		Encoders:      []string{"libx264"},
		FFmpegVersion: "6.0",
		MaxConcurrent: 1,
	}
	slow, err := database.CreateOrUpdateWorker("w-slow", "slow-node", caps, 90*time.Second)
	if err != nil {
		t.Fatalf("create slow worker: %v", err)
	}
	peer, err := database.CreateOrUpdateWorker("w-peer", "fast-node", caps, 90*time.Second)
	if err != nil {
		t.Fatalf("create peer worker: %v", err)
	}
	if err := database.MarkWorkerEvicted(slow.ID); err != nil {
		t.Fatalf("mark slow worker evicted: %v", err)
	}

	// Make both rows stale enough for the startup sweep. The peer must be
	// removed to prove the sweep ran; the evicted row must be exempt.
	backdated := time.Now().Add(-10 * time.Minute)
	if _, err := database.GetDB().Exec(
		`UPDATE workers SET last_heartbeat = ? WHERE id IN (?, ?)`,
		backdated, slow.ID, peer.ID,
	); err != nil {
		t.Fatalf("backdate worker heartbeats: %v", err)
	}

	if _, _, err := database.RecoverState(); err != nil {
		t.Fatalf("recover state: %v", err)
	}
	removed, err := database.RemoveStaleWorkers(135 * time.Second)
	if err != nil {
		t.Fatalf("remove stale workers: %v", err)
	}
	if removed != 1 {
		t.Fatalf("stale sweep removed %d workers, want 1 (the unmarked peer)", removed)
	}
	if _, err := database.GetWorker(peer.ID); !errors.Is(err, protocol.ErrWorkerNotFound) {
		t.Fatalf("peer lookup after sweep = %v, want ErrWorkerNotFound", err)
	}
	got, err := database.GetWorker(slow.ID)
	if err != nil {
		t.Fatalf("evicted worker was removed by the stale sweep: %v", err)
	}
	if !got.Evicted {
		t.Fatal("eviction mark was cleared by restart recovery")
	}
	if !got.EvictedAt.Valid {
		t.Error("evicted_at must survive restart recovery with the mark")
	}
	if got.Status != protocol.WorkerStatusOffline {
		t.Errorf("status after recovery = %q, want offline", got.Status)
	}

	// The offline-retention sweep is the other row-deletion path; it must leave
	// the evicted row alone even though its heartbeat is stale.
	removedIDs, err := database.RemoveOfflineWorkers(10 * time.Minute)
	if err != nil {
		t.Fatalf("remove offline workers: %v", err)
	}
	if len(removedIDs) != 0 {
		t.Fatalf("offline sweep removed %v, want none", removedIDs)
	}

	// A worker process re-registers with the same id after the restart. The
	// mark belongs to the worker, not to the registration attempt.
	reregistered, err := database.CreateOrUpdateWorker(slow.ID, slow.Name, caps, 90*time.Second)
	if err != nil {
		t.Fatalf("re-register evicted worker: %v", err)
	}
	if !reregistered.Evicted {
		t.Fatal("same-id re-registration cleared the eviction mark")
	}
	if !reregistered.EvictedAt.Valid {
		t.Error("same-id re-registration cleared evicted_at")
	}

	// The panel reads GetAllWorkers; the surviving row must carry the same
	// observable mark there.
	all, err := database.GetAllWorkers()
	if err != nil {
		t.Fatalf("list workers: %v", err)
	}
	if len(all) != 1 || all[0].ID != slow.ID {
		t.Fatalf("workers after restart = %+v, want only the evicted slow worker", all)
	}
	if !all[0].Evicted {
		t.Error("panel source row lost the eviction mark")
	}
}
