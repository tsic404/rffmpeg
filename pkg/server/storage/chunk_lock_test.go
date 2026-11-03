package storage

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"time"
)

// A chunk transfer or an assembly holds only the session lock: a download of
// an unrelated file must proceed while it runs. Before the per-session lock,
// the global files/ lock was held for the whole transfer, so four slow uploads
// blocked every download.
func TestChunkLockDoesNotBlockDownloads(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("Failed to create storage: %v", err)
	}

	content := []byte("downloadable")
	sum := sha256.Sum256(content)
	fileID := hex.EncodeToString(sum[:])
	if _, _, _, err := store.SaveFile(fileID, strings.NewReader(string(content))); err != nil {
		t.Fatalf("Failed to save file: %v", err)
	}

	release := store.lockUpload("upload-1")

	opened := make(chan error, 1)
	go func() {
		file, err := store.OpenFile(fileID)
		if err == nil {
			file.Close()
		}
		opened <- err
	}()

	select {
	case err := <-opened:
		if err != nil {
			t.Fatalf("OpenFile failed: %v", err)
		}
	case <-time.After(2 * time.Second):
		release()
		t.Fatal("download blocked by an in-progress chunk operation")
	}
	release()
}

func TestChunkLockSerializesOnlyTheSameSession(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("Failed to create storage: %v", err)
	}

	releaseFirst := store.lockUpload("upload-1")

	// Another session may proceed immediately.
	acquiredOther := make(chan struct{})
	go func() {
		release := store.lockUpload("upload-2")
		close(acquiredOther)
		release()
	}()
	select {
	case <-acquiredOther:
	case <-time.After(2 * time.Second):
		releaseFirst()
		t.Fatal("unrelated upload session was blocked")
	}

	// The same session must wait for the first holder.
	acquiredSame := make(chan struct{})
	go func() {
		release := store.lockUpload("upload-1")
		close(acquiredSame)
		release()
	}()
	select {
	case <-acquiredSame:
		releaseFirst()
		t.Fatal("same upload session was not serialized")
	case <-time.After(100 * time.Millisecond):
	}

	releaseFirst()
	select {
	case <-acquiredSame:
	case <-time.After(2 * time.Second):
		t.Fatal("same upload session never acquired the lock after release")
	}
}

// The lock table must not grow with every session ever seen.
func TestChunkLockTableDropsUnusedSessions(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("Failed to create storage: %v", err)
	}

	for _, id := range []string{"a", "b", "c"} {
		store.lockUpload(id)()
	}

	store.chunkLocksMu.Lock()
	remaining := len(store.chunkLocks)
	store.chunkLocksMu.Unlock()
	if remaining != 0 {
		t.Fatalf("chunk lock table retained %d idle entries, want 0", remaining)
	}
}
