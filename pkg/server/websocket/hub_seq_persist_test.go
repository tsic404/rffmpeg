package websocket

import (
	"errors"
	"testing"
	"time"
)

// seqPersistStore is an in-memory SeqStore standing in for the SQLite-backed
// *db.Database in restart-simulation tests.
type seqPersistStore struct {
	seq map[string]int64
}

func newSeqPersistStore() *seqPersistStore {
	return &seqPersistStore{seq: make(map[string]int64)}
}

func (s *seqPersistStore) LoadWSJobSeq(jobID string) (int64, error) {
	return s.seq[jobID], nil
}

func (s *seqPersistStore) SaveWSJobSeq(jobID string, lastSeq int64) error {
	s.seq[jobID] = lastSeq
	return nil
}

func (s *seqPersistStore) DeleteWSJobSeq(jobID string) error {
	delete(s.seq, jobID)
	return nil
}

// TestHub_SeqPersistsAcrossRestart is the TSI-2379 regression test: after a
// server restart (fresh Hub, same store), numbering must continue from the
// persisted counter. Before the fix a restarted hub renumbered from 1 and
// reconnecting streaming clients flagged a spurious gap — failing the CLI
// with exit code 1 even though nothing was lost.
func TestHub_SeqPersistsAcrossRestart(t *testing.T) {
	store := newSeqPersistStore()
	jobID := "job-restart"

	// First server lifetime: two sequenced broadcasts.
	hubA := NewHubWithSeqStore(store)
	runHub(hubA)
	for i := 0; i < 2; i++ {
		if err := hubA.BroadcastStderr(jobID, "chunk"); err != nil {
			t.Fatalf("broadcast %d: %v", i, err)
		}
	}
	waitForSeq(t, hubA, jobID, func(v int64, ok bool) bool { return ok && v == 2 })

	// Restart: fresh hub over the same persisted state.
	hubB := NewHubWithSeqStore(store)
	runHub(hubB)
	if err := hubB.BroadcastStderr(jobID, "after-restart"); err != nil {
		t.Fatalf("post-restart broadcast: %v", err)
	}
	got := waitForSeq(t, hubB, jobID, func(v int64, ok bool) bool { return ok && v >= 3 })
	if got != 3 {
		t.Fatalf("post-restart broadcast must continue at seq 3, got %d", got)
	}
}

// TestHub_TerminalBroadcastDeletesPersistedSeq verifies the store row is
// dropped on terminal broadcasts so a later stream for the same job ID starts
// clean at 1 instead of resuming a stale counter.
func TestHub_TerminalBroadcastDeletesPersistedSeq(t *testing.T) {
	store := newSeqPersistStore()
	jobID := "job-terminal-persist"

	hub := NewHubWithSeqStore(store)
	runHub(hub)
	if err := hub.BroadcastStderr(jobID, "x"); err != nil {
		t.Fatalf("stderr broadcast: %v", err)
	}
	waitForSeq(t, hub, jobID, func(v int64, _ bool) bool { return v >= 1 })

	// Terminal broadcast must drop the persisted row.
	if err := hub.BroadcastComplete(jobID, 0); err != nil {
		t.Fatalf("complete broadcast: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := store.seq[jobID]; !ok {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, ok := store.seq[jobID]; ok {
		t.Fatal("terminal broadcast must delete the persisted seq row")
	}

	// A later stream for the same job starts clean at 1.
	if err := hub.BroadcastStderr(jobID, "new-stream"); err != nil {
		t.Fatalf("new-stream broadcast: %v", err)
	}
	got := waitForSeq(t, hub, jobID, func(v int64, ok bool) bool { return ok && v == 1 })
	if got != 1 {
		t.Errorf("fresh stream after terminal must start at seq 1, got %d", got)
	}
}

// TestHub_NilSeqStoreKeepsLegacyBehavior verifies the no-store path still
// works: counters stay purely in memory and broadcasts sequence normally.
func TestHub_NilSeqStoreKeepsLegacyBehavior(t *testing.T) {
	hub := NewHub()
	runHub(hub)
	jobID := "job-legacy"

	if err := hub.BroadcastStderr(jobID, "one"); err != nil {
		t.Fatalf("broadcast 1: %v", err)
	}
	got := waitForSeq(t, hub, jobID, func(v int64, ok bool) bool { return ok && v == 1 })
	if got != 1 {
		t.Fatalf("expected seq=1, got %d", got)
	}
}

var _ SeqStore = (*seqPersistStore)(nil)

// failingSeqStore fails every save, exercising persistSeq's error path.
type failingSeqStore struct{}

func (failingSeqStore) LoadWSJobSeq(string) (int64, error) { return 0, nil }
func (failingSeqStore) SaveWSJobSeq(string, int64) error   { return errors.New("store down") }
func (failingSeqStore) DeleteWSJobSeq(string) error        { return nil }

// TestHub_SeqPersistFailureCounted verifies the observability counter: a
// degraded store must bump SeqPersistFailureCount instead of failing the
// broadcast.
func TestHub_SeqPersistFailureCounted(t *testing.T) {
	hub := NewHubWithSeqStore(failingSeqStore{})
	runHub(hub)

	if err := hub.BroadcastStderr("job-fail", "x"); err != nil {
		t.Fatalf("broadcast must not fail on store errors: %v", err)
	}
	waitForSeq(t, hub, "job-fail", func(v int64, _ bool) bool { return v >= 1 })

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && hub.SeqPersistFailureCount() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if hub.SeqPersistFailureCount() != 1 {
		t.Fatalf("expected 1 counted persistence failure, got %d", hub.SeqPersistFailureCount())
	}
}
