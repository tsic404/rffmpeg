package websocket

import "testing"

// TestHub_ReleaseJobStreamDropsCounterAndStore pins the cleanup hook used by
// terminal paths that emit no complete/error frame (starvation sweep, stuck-job
// sweep, timeout-exhausted failures): the in-memory counter and the persisted
// row must both be gone, and a later broadcast for the same job starts a fresh
// stream at 1 instead of continuing a stale counter.
func TestHub_ReleaseJobStreamDropsCounterAndStore(t *testing.T) {
	store := newSeqPersistStore()
	jobID := "job-release"

	hub := NewHubWithSeqStore(store)
	runHub(hub)

	if err := hub.BroadcastStderr(jobID, "x"); err != nil {
		t.Fatalf("stderr broadcast: %v", err)
	}
	waitForSeq(t, hub, jobID, func(v int64, ok bool) bool { return ok && v == 1 })

	hub.ReleaseJobStream(jobID)

	hub.mu.RLock()
	_, inMemory := hub.seq[jobID]
	hub.mu.RUnlock()
	if inMemory {
		t.Error("in-memory seq counter survived ReleaseJobStream")
	}
	if _, inStore := store.seq[jobID]; inStore {
		t.Errorf("persisted seq row survived ReleaseJobStream: %v", store.seq)
	}

	// A fresh stream for the same job ID restarts cleanly.
	if err := hub.BroadcastStderr(jobID, "after-release"); err != nil {
		t.Fatalf("post-release broadcast: %v", err)
	}
	if got := waitForSeq(t, hub, jobID, func(v int64, ok bool) bool { return ok && v == 1 }); got != 1 {
		t.Fatalf("post-release numbering started at %d, want 1", got)
	}
}

// TestHub_ReleaseJobStreamIsIdempotent covers the handler cancel path, which
// releases after an optional complete frame: the second release must be a
// no-op rather than an error or a store rewrite.
func TestHub_ReleaseJobStreamIsIdempotent(t *testing.T) {
	store := newSeqPersistStore()
	jobID := "job-release-twice"

	hub := NewHubWithSeqStore(store)
	runHub(hub)

	if err := hub.BroadcastStderr(jobID, "x"); err != nil {
		t.Fatalf("stderr broadcast: %v", err)
	}
	waitForSeq(t, hub, jobID, func(v int64, _ bool) bool { return v == 1 })

	hub.ReleaseJobStream(jobID)
	hub.ReleaseJobStream(jobID)

	if err := hub.BroadcastStderr(jobID, "y"); err != nil {
		t.Fatalf("broadcast after double release: %v", err)
	}
	if got := waitForSeq(t, hub, jobID, func(v int64, ok bool) bool { return ok && v == 1 }); got != 1 {
		t.Fatalf("numbering after double release started at %d, want 1", got)
	}
}
