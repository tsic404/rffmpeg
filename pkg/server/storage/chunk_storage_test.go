package storage

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestSaveChunk(t *testing.T) {
	// Create temp directory
	tmpDir, err := os.MkdirTemp("", "storage_test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	store, err := New(tmpDir)
	if err != nil {
		t.Fatalf("Failed to create storage: %v", err)
	}

	// Test data
	testData := []byte("test chunk data")
	uploadID := "test-upload-123"
	chunkIndex := 0

	// Save chunk
	path, size, checksum, err := store.SaveChunk(uploadID, chunkIndex, bytes.NewReader(testData))
	if err != nil {
		t.Fatalf("Failed to save chunk: %v", err)
	}

	// Verify path
	expectedPath := filepath.Join(tmpDir, "chunks", uploadID, "chunk_0")
	if path != expectedPath {
		t.Errorf("Expected path %s, got %s", expectedPath, path)
	}

	// Verify size
	if size != int64(len(testData)) {
		t.Errorf("Expected size %d, got %d", len(testData), size)
	}

	// Verify checksum
	hash := sha256.Sum256(testData)
	expectedChecksum := hex.EncodeToString(hash[:])
	if checksum != expectedChecksum {
		t.Errorf("Expected checksum %s, got %s", expectedChecksum, checksum)
	}

	// Verify file exists
	if !store.ChunkExists(uploadID, chunkIndex, int64(len(testData))) {
		t.Error("Chunk should exist")
	}
}

func TestOpenChunk(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "storage_test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	store, err := New(tmpDir)
	if err != nil {
		t.Fatalf("Failed to create storage: %v", err)
	}

	testData := []byte("test chunk data for reading")
	uploadID := "test-upload-open"
	chunkIndex := 0

	// Save chunk first
	_, _, _, err = store.SaveChunk(uploadID, chunkIndex, bytes.NewReader(testData))
	if err != nil {
		t.Fatalf("Failed to save chunk: %v", err)
	}

	// Open and read chunk
	file, err := store.OpenChunk(uploadID, chunkIndex)
	if err != nil {
		t.Fatalf("Failed to open chunk: %v", err)
	}
	defer file.Close()

	buf := new(bytes.Buffer)
	buf.ReadFrom(file)
	if !bytes.Equal(buf.Bytes(), testData) {
		t.Error("Chunk content mismatch")
	}
}

func TestDeleteChunk(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "storage_test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	store, err := New(tmpDir)
	if err != nil {
		t.Fatalf("Failed to create storage: %v", err)
	}

	testData := []byte("test chunk to delete")
	uploadID := "test-upload-delete"
	chunkIndex := 0

	// Save chunk
	_, _, _, err = store.SaveChunk(uploadID, chunkIndex, bytes.NewReader(testData))
	if err != nil {
		t.Fatalf("Failed to save chunk: %v", err)
	}

	// Delete chunk
	if err := store.DeleteChunk(uploadID, chunkIndex); err != nil {
		t.Fatalf("Failed to delete chunk: %v", err)
	}

	// Verify chunk is deleted
	if store.ChunkExists(uploadID, chunkIndex, int64(len(testData))) {
		t.Error("Chunk should not exist after deletion")
	}
}

func TestDeleteChunkDirectory(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "storage_test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	store, err := New(tmpDir)
	if err != nil {
		t.Fatalf("Failed to create storage: %v", err)
	}

	uploadID := "test-upload-dir-delete"

	// Create multiple chunks
	for i := range 3 {
		_, _, _, err := store.SaveChunk(uploadID, i, bytes.NewReader([]byte("data")))
		if err != nil {
			t.Fatalf("Failed to save chunk %d: %v", i, err)
		}
	}

	// Delete all chunks
	if err := store.DeleteChunkDirectory(uploadID); err != nil {
		t.Fatalf("Failed to delete chunk directory: %v", err)
	}

	// Verify all chunks are deleted
	for i := range 3 {
		if store.ChunkExists(uploadID, i, int64(len("data"))) {
			t.Errorf("Chunk %d should not exist", i)
		}
	}
}

func TestAssembleChunks(t *testing.T) {
	tmpDir := t.TempDir()
	store, err := New(tmpDir)
	if err != nil {
		t.Fatalf("Failed to create storage: %v", err)
	}

	uploadID := "test-assemble"
	chunks := [][]byte{
		[]byte("chunk0"),
		[]byte("chunk1"),
		[]byte("chunk2"),
	}
	for i, data := range chunks {
		if _, _, _, err := store.SaveChunk(uploadID, i, bytes.NewReader(data)); err != nil {
			t.Fatalf("Failed to save chunk %d: %v", i, err)
		}
	}
	// A stray file outside the session's index range must never reach the
	// assembled output: assembly walks the explicit index range.
	if _, _, _, err := store.SaveChunk(uploadID, 9, bytes.NewReader([]byte("stray"))); err != nil {
		t.Fatalf("Failed to save stray chunk: %v", err)
	}

	assembledPath, totalSize, checksum, err := store.AssembleChunks(uploadID, len(chunks), int64(len(chunks[0])), int64(len(chunks[0])*len(chunks)))
	if err != nil {
		t.Fatalf("Failed to assemble chunks: %v", err)
	}
	if filepath.Dir(assembledPath) != filepath.Join(tmpDir, "files") {
		t.Errorf("Expected assembled file inside the files directory, got %s", assembledPath)
	}

	expectedData := append(append([]byte{}, chunks[0]...), chunks[1]...)
	expectedData = append(expectedData, chunks[2]...)

	// Verify total size
	if totalSize != int64(len(expectedData)) {
		t.Errorf("Expected size %d, got %d", len(expectedData), totalSize)
	}

	// Verify checksum
	hash := sha256.Sum256(expectedData)
	expectedChecksum := hex.EncodeToString(hash[:])
	if checksum != expectedChecksum {
		t.Errorf("Expected checksum %s, got %s", expectedChecksum, checksum)
	}

	// Read assembled file and verify content
	got, err := os.ReadFile(assembledPath)
	if err != nil {
		t.Fatalf("Failed to read assembled file: %v", err)
	}
	if !bytes.Equal(got, expectedData) {
		t.Errorf("Assembled file content mismatch: got %q, want %q", got, expectedData)
	}
}

func TestAssembleChunksRejectsMissingChunk(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("Failed to create storage: %v", err)
	}

	uploadID := "test-assemble-missing"
	if _, _, _, err := store.SaveChunk(uploadID, 0, bytes.NewReader([]byte("chunk0"))); err != nil {
		t.Fatalf("SaveChunk 0: %v", err)
	}
	if _, _, _, err := store.SaveChunk(uploadID, 2, bytes.NewReader([]byte("chunk2"))); err != nil {
		t.Fatalf("SaveChunk 2: %v", err)
	}

	_, _, _, err = store.AssembleChunks(uploadID, 3, 6, 18)
	if !errors.Is(err, ErrChunkMissing) {
		t.Fatalf("expected ErrChunkMissing, got %v", err)
	}
	if !strings.Contains(err.Error(), "chunk 1") {
		t.Errorf("expected the missing index in the error, got %v", err)
	}

	// Nothing may be left behind in the files directory.
	entries, readErr := os.ReadDir(filepath.Join(store.baseDir, "files"))
	if readErr != nil && !os.IsNotExist(readErr) {
		t.Fatalf("ReadDir files: %v", readErr)
	}
	if len(entries) != 0 {
		t.Errorf("expected no assembled file for a missing chunk, found %d entries", len(entries))
	}
}

func TestAssembleChunksRejectsTruncatedChunk(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("Failed to create storage: %v", err)
	}

	uploadID := "test-assemble-truncated"
	if _, _, _, err := store.SaveChunk(uploadID, 0, bytes.NewReader([]byte("chunk0"))); err != nil {
		t.Fatalf("SaveChunk 0: %v", err)
	}
	if _, _, _, err := store.SaveChunk(uploadID, 1, bytes.NewReader([]byte("chun"))); err != nil {
		t.Fatalf("SaveChunk 1: %v", err)
	}

	_, _, _, err = store.AssembleChunks(uploadID, 2, 6, 12)
	if !errors.Is(err, ErrChunkSizeMismatch) {
		t.Fatalf("expected ErrChunkSizeMismatch, got %v", err)
	}
}

func TestAssembleChunksRejectsInconsistentSession(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("Failed to create storage: %v", err)
	}

	// file_size 15 with chunk_size 10 and 3 chunks cannot add up: the last
	// chunk would have to carry -5 bytes.
	_, _, _, err = store.AssembleChunks("test-assemble-inconsistent", 3, 10, 15)
	if !errors.Is(err, ErrAssembledSizeMismatch) {
		t.Fatalf("expected ErrAssembledSizeMismatch, got %v", err)
	}
}

// failingReader yields some bytes and then fails, standing in for the write
// error (binary truncated by a client, disk error) that must never leave a
// chunk at the canonical path.
type failingReader struct {
	data []byte
	done bool
}

func (f *failingReader) Read(p []byte) (int, error) {
	if f.done {
		return 0, errors.New("read failed")
	}
	f.done = true
	return copy(p, f.data), nil
}

func TestSaveChunkLeavesNoPartialFileOnWriteFailure(t *testing.T) {
	tmpDir := t.TempDir()
	store, err := New(tmpDir)
	if err != nil {
		t.Fatalf("Failed to create storage: %v", err)
	}

	uploadID := "test-save-failure"
	if _, _, _, err := store.SaveChunk(uploadID, 0, &failingReader{data: []byte("partial")}); err == nil {
		t.Fatal("expected SaveChunk to fail when the reader fails")
	}

	if store.ChunkExists(uploadID, 0, int64(len("partial"))) {
		t.Error("a failed SaveChunk must not leave a chunk at the canonical path")
	}
	entries, err := os.ReadDir(filepath.Join(tmpDir, "chunks", uploadID))
	if err != nil {
		t.Fatalf("ReadDir chunk dir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("expected no leftover files in the chunk dir, found %d", len(entries))
	}
}

func TestChunkExistsRequiresExactSize(t *testing.T) {
	tmpDir := t.TempDir()
	store, err := New(tmpDir)
	if err != nil {
		t.Fatalf("Failed to create storage: %v", err)
	}

	uploadID := "test-chunk-exists-size"
	data := []byte("0123456789")
	path, _, _, err := store.SaveChunk(uploadID, 0, bytes.NewReader(data))
	if err != nil {
		t.Fatalf("SaveChunk: %v", err)
	}
	if !store.ChunkExists(uploadID, 0, int64(len(data))) {
		t.Fatal("a complete chunk must read as existing")
	}

	// Simulate a crash-truncated chunk: the file exists but is short.
	if err := os.Truncate(path, 4); err != nil {
		t.Fatalf("Truncate: %v", err)
	}
	if store.ChunkExists(uploadID, 0, int64(len(data))) {
		t.Error("a truncated chunk must not read as existing")
	}
	if !store.ChunkExists(uploadID, 0, 4) {
		t.Error("exact-size check should still accept the truncated size")
	}
}

// failingWriteCloser is an io.WriteCloser whose Write succeeds but whose Close
// returns a fixed error, simulating a close-time (flush) ENOSPC that a regular
// filesystem cannot reproduce portably.
type failingWriteCloser struct {
	closeErr error
}

func (f *failingWriteCloser) Write(p []byte) (int, error) {
	return len(p), nil
}

func (f *failingWriteCloser) Close() error {
	return f.closeErr
}

func TestAssembleChunksInto_CloseErrorPropagates(t *testing.T) {
	dst := &failingWriteCloser{closeErr: syscall.ENOSPC}

	_, _, err := assembleChunksInto(dst, nil)
	if err == nil {
		t.Fatal("expected close error to propagate, got nil")
	}
	if !errors.Is(err, syscall.ENOSPC) {
		t.Errorf("expected ENOSPC in wrapped close error, got: %v", err)
	}
}

func TestCalculateChunkChecksum(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "storage_test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	store, err := New(tmpDir)
	if err != nil {
		t.Fatalf("Failed to create storage: %v", err)
	}

	testData := []byte("test data for checksum")
	uploadID := "test-checksum"
	chunkIndex := 0

	// Save chunk
	_, _, savedChecksum, err := store.SaveChunk(uploadID, chunkIndex, bytes.NewReader(testData))
	if err != nil {
		t.Fatalf("Failed to save chunk: %v", err)
	}

	// Calculate checksum
	calculatedChecksum, err := store.CalculateChunkChecksum(uploadID, chunkIndex)
	if err != nil {
		t.Fatalf("Failed to calculate checksum: %v", err)
	}

	// Verify checksums match
	if calculatedChecksum != savedChecksum {
		t.Errorf("Checksum mismatch: calculated %s, saved %s", calculatedChecksum, savedChecksum)
	}
}

// TestAssembleChunksDoesNotPreallocateForHugeTotalChunks pins the regression:
// the working set must not be sized from the client-declared chunk count, so a
// session claiming 2^40 chunks fails on the first missing chunk instead of
// trying to reserve tens of terabytes.
func TestAssembleChunksDoesNotPreallocateForHugeTotalChunks(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("Failed to create storage: %v", err)
	}

	const hugeChunks = 1 << 40
	_, _, _, err = store.AssembleChunks("huge", hugeChunks, 1, hugeChunks)
	if !errors.Is(err, ErrChunkMissing) {
		t.Fatalf("expected ErrChunkMissing for the huge declared chunk count, got %v", err)
	}
	if !strings.Contains(err.Error(), "chunk 0") {
		t.Errorf("expected the first missing index in the error, got %v", err)
	}
}

// TestAssembleChunksRejectsOverflowingChunking covers the arithmetic guard: the
// product chunk_size*(total_chunks-1) would overflow int64, so the session must
// be rejected before the last-chunk size is computed from a wrapped value.
func TestAssembleChunksRejectsOverflowingChunking(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("Failed to create storage: %v", err)
	}

	_, _, _, err = store.AssembleChunks("overflow", 1<<34, 1<<40, 1<<40)
	if !errors.Is(err, ErrAssembledSizeMismatch) {
		t.Fatalf("expected ErrAssembledSizeMismatch for overflowing chunking, got %v", err)
	}
}
