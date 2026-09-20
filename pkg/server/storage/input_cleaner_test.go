package storage

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// saveBlob writes a content-addressed blob and returns its file ID.
func saveBlob(t *testing.T, s *Storage, content string) string {
	t.Helper()
	_, _, id, err := s.SaveFileByContent(bytes.NewReader([]byte(content)))
	if err != nil {
		t.Fatalf("SaveFileByContent: %v", err)
	}
	return id
}

// backdate sets a blob's mtime into the past so it reads as expired.
func backdate(t *testing.T, path string, age time.Duration) {
	t.Helper()
	old := time.Now().Add(-age)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatalf("backdate %s: %v", path, err)
	}
}

func TestCleanupExpiredInputFilesRemovesOnlyExpired(t *testing.T) {
	baseDir := t.TempDir()
	store, err := New(baseDir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	expiredID := saveBlob(t, store, "alpha")
	freshID := saveBlob(t, store, "beta")
	backdate(t, store.GetFilePath(expiredID), 48*time.Hour)

	removed, err := store.CleanupExpiredInputFiles(time.Now().Add(-24*time.Hour), nil)
	if err != nil {
		t.Fatalf("CleanupExpiredInputFiles: %v", err)
	}
	if removed != 1 {
		t.Fatalf("removed = %d, want 1", removed)
	}
	if store.FileExists(expiredID) {
		t.Error("expired blob should be removed")
	}
	if !store.FileExists(freshID) {
		t.Error("fresh blob should remain")
	}
}

func TestCleanupExpiredInputFilesSkipsInUse(t *testing.T) {
	baseDir := t.TempDir()
	store, err := New(baseDir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	inUseID := saveBlob(t, store, "live")
	backdate(t, store.GetFilePath(inUseID), 48*time.Hour)

	removed, err := store.CleanupExpiredInputFiles(time.Now().Add(-24*time.Hour), map[string]struct{}{inUseID: {}})
	if err != nil {
		t.Fatalf("CleanupExpiredInputFiles: %v", err)
	}
	if removed != 0 {
		t.Fatalf("removed = %d, want 0 (in-use blob must be kept)", removed)
	}
	if !store.FileExists(inUseID) {
		t.Error("in-use blob should remain")
	}
}

func TestCleanupExpiredInputFilesSkipsNonBlobNames(t *testing.T) {
	baseDir := t.TempDir()
	store, err := New(baseDir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// A stray temp file from an in-flight upload must never be swept.
	filesDir := filepath.Join(baseDir, "files")
	if err := os.MkdirAll(filesDir, 0755); err != nil {
		t.Fatalf("mkdir files: %v", err)
	}
	tempPath := filepath.Join(filesDir, "upload-tmp123")
	if err := os.WriteFile(tempPath, []byte("partial"), 0644); err != nil {
		t.Fatalf("write temp: %v", err)
	}
	backdate(t, tempPath, 48*time.Hour)

	removed, err := store.CleanupExpiredInputFiles(time.Now().Add(-24*time.Hour), nil)
	if err != nil {
		t.Fatalf("CleanupExpiredInputFiles: %v", err)
	}
	if removed != 0 {
		t.Fatalf("removed = %d, want 0 (non-blob name must be skipped)", removed)
	}
	if _, err := os.Stat(tempPath); err != nil {
		t.Errorf("temp file should remain: %v", err)
	}
}

func TestSaveFileByContentDedupRefreshesMtime(t *testing.T) {
	baseDir := t.TempDir()
	store, err := New(baseDir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	id := saveBlob(t, store, "same-content")
	path := store.GetFilePath(id)
	backdate(t, path, 48*time.Hour)

	// Re-uploading identical content hits dedup and must refresh mtime, so the
	// TTL sweep keeps a still-reused blob.
	_, _, dedupID, err := store.SaveFileByContent(bytes.NewReader([]byte("same-content")))
	if err != nil {
		t.Fatalf("SaveFileByContent dedup: %v", err)
	}
	if dedupID != id {
		t.Fatalf("dedup returned different ID: got %s, want %s", dedupID, id)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !info.ModTime().After(time.Now().Add(-time.Minute)) {
		t.Error("mtime should be refreshed on dedup hit")
	}
}

func TestInputFileCleanerCleanupOnce(t *testing.T) {
	baseDir := t.TempDir()
	store, err := New(baseDir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	expiredID := saveBlob(t, store, "sweep-me")
	backdate(t, store.GetFilePath(expiredID), 48*time.Hour)

	cleaner := NewInputFileCleaner(store, 24*time.Hour, time.Hour, nil)
	cleaner.CleanupOnce()

	if store.FileExists(expiredID) {
		t.Error("expired blob should be removed by CleanupOnce")
	}
}

func TestInputFileCleanerDisabledWhenTTLZero(t *testing.T) {
	baseDir := t.TempDir()
	store, err := New(baseDir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	blobID := saveBlob(t, store, "keep-forever")
	backdate(t, store.GetFilePath(blobID), 48*time.Hour)

	cleaner := NewInputFileCleaner(store, 0, time.Hour, nil)
	cleaner.CleanupOnce()

	if !store.FileExists(blobID) {
		t.Error("TTL=0 disables cleanup; blob should remain")
	}
}

func TestInputFileCleanerStartRejectsNonPositiveInterval(t *testing.T) {
	baseDir := t.TempDir()
	store, err := New(baseDir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	cleaner := NewInputFileCleaner(store, 24*time.Hour, 0, nil)
	cleaner.Start(context.Background())
	defer cleaner.Stop()

	cleaner.mu.Lock()
	defer cleaner.mu.Unlock()
	if cleaner.cancel != nil {
		t.Fatal("Start with interval<=0 must not launch the sweep loop (NewTicker would panic)")
	}
}

func TestInputFileCleanerAbortsSweepWhenInUseLoadFails(t *testing.T) {
	baseDir := t.TempDir()
	store, err := New(baseDir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	blobID := saveBlob(t, store, "active-unknown")
	backdate(t, store.GetFilePath(blobID), 48*time.Hour)

	inUse := func() (map[string]struct{}, error) {
		return nil, errors.New("db unavailable")
	}
	cleaner := NewInputFileCleaner(store, 24*time.Hour, time.Hour, inUse)
	cleaner.CleanupOnce()

	// Fail closed: an unloadable in-use set must not evict anything.
	if !store.FileExists(blobID) {
		t.Error("sweep must abort when the in-use set cannot be loaded")
	}
}

func TestTouchFileRefreshesMtimeAndPreventsEviction(t *testing.T) {
	baseDir := t.TempDir()
	store, err := New(baseDir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	blobID := saveBlob(t, store, "old-but-referenced")
	backdate(t, store.GetFilePath(blobID), 48*time.Hour)

	// A submit that references the old blob refreshes its mtime before commit.
	exists, err := store.TouchFile(blobID)
	if err != nil || !exists {
		t.Fatalf("TouchFile: exists=%v err=%v", exists, err)
	}

	removed, err := store.CleanupExpiredInputFiles(time.Now().Add(-24*time.Hour), nil)
	if err != nil {
		t.Fatalf("CleanupExpiredInputFiles: %v", err)
	}
	if removed != 0 {
		t.Fatalf("removed = %d, want 0 (blob refreshed by TouchFile must survive)", removed)
	}
	if !store.FileExists(blobID) {
		t.Error("blob refreshed by TouchFile should remain")
	}
}

func TestTouchFileMissingReturnsFalse(t *testing.T) {
	baseDir := t.TempDir()
	store, err := New(baseDir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	exists, err := store.TouchFile(strings.Repeat("a", 64))
	if err != nil {
		t.Fatalf("TouchFile missing blob should not error: %v", err)
	}
	if exists {
		t.Error("TouchFile must report a missing blob as not existing")
	}
}

func TestTouchFileRejectsTraversal(t *testing.T) {
	baseDir := t.TempDir()
	store, err := New(baseDir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// A file outside files/ that a traversal fragment must never reach.
	victim := filepath.Join(baseDir, "victim")
	if err := os.WriteFile(victim, []byte("x"), 0644); err != nil {
		t.Fatalf("write victim: %v", err)
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(victim, old, old); err != nil {
		t.Fatalf("backdate victim: %v", err)
	}

	exists, err := store.TouchFile("../victim")
	if err != nil {
		t.Fatalf("TouchFile traversal should reject without error: %v", err)
	}
	if exists {
		t.Error("traversal ID must be rejected as not existing")
	}

	info, err := os.Stat(victim)
	if err != nil {
		t.Fatalf("stat victim: %v", err)
	}
	if info.ModTime().After(old.Add(time.Minute)) {
		t.Error("TouchFile must not write outside files/ — victim mtime changed")
	}
}
