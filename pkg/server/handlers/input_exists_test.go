package handlers

import (
	"path/filepath"
	"testing"

	"github.com/tsic404/rffmpeg/pkg/server/storage"
)

// TestInputExistsOnlyProbesRegisteredInputs pins the guard's scope: only a
// storage file ID is resolved against the server's storage; a path, URL or
// arbitrary string is passed through without touching the filesystem.
func TestInputExistsOnlyProbesRegisteredInputs(t *testing.T) {
	store, err := storage.New(t.TempDir())
	if err != nil {
		t.Fatalf("storage.New: %v", err)
	}

	missingID := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	if inputExists(store, missingID) {
		t.Error("a registered-form file ID that is absent from storage must report missing")
	}

	existingPath := filepath.Join(t.TempDir(), "media.mp4")
	if !inputExists(store, existingPath) {
		t.Error("a direct path must be left to the worker, not rejected by a server-side stat")
	}

	if !inputExists(store, "https://example.test/in.mp4") {
		t.Error("a remote URL must be left to the worker")
	}

	traversal := t.TempDir() + string(filepath.Separator) + "sub" + string(filepath.Separator) + ".." + string(filepath.Separator) + "escape.mp4"
	if inputExists(store, traversal) {
		t.Error("a traversal path must be rejected without being resolved")
	}
}
