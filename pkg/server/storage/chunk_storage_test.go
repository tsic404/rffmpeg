package storage

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
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
	if !store.ChunkExists(uploadID, chunkIndex) {
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
	if store.ChunkExists(uploadID, chunkIndex) {
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
	for i := 0; i < 3; i++ {
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
	for i := 0; i < 3; i++ {
		if store.ChunkExists(uploadID, i) {
			t.Errorf("Chunk %d should not exist", i)
		}
	}
}

func TestAssembleChunks(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "storage_test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	store, err := New(tmpDir)
	if err != nil {
		t.Fatalf("Failed to create storage: %v", err)
	}

	uploadID := "test-assemble"
	fileID := "test-file-id"

	// Create chunks
	chunks := [][]byte{
		[]byte("chunk0"),
		[]byte("chunk1"),
		[]byte("chunk2"),
	}
	var chunkPaths []string
	for i, data := range chunks {
		path, _, _, err := store.SaveChunk(uploadID, i, bytes.NewReader(data))
		if err != nil {
			t.Fatalf("Failed to save chunk %d: %v", i, err)
		}
		chunkPaths = append(chunkPaths, path)
	}

	// Assemble
	assembledPath, totalSize, checksum, err := store.AssembleChunks(uploadID, fileID, chunkPaths)
	if err != nil {
		t.Fatalf("Failed to assemble chunks: %v", err)
	}

	// Verify path
	expectedPath := filepath.Join(tmpDir, "files", fileID)
	if assembledPath != expectedPath {
		t.Errorf("Expected path %s, got %s", expectedPath, assembledPath)
	}

	// Verify total size
	expectedTotalSize := len(chunks[0]) + len(chunks[1]) + len(chunks[2])
	if totalSize != int64(expectedTotalSize) {
		t.Errorf("Expected size %d, got %d", expectedTotalSize, totalSize)
	}

	// Verify checksum
	expectedData := append(chunks[0], chunks[1]...)
	expectedData = append(expectedData, chunks[2]...)
	hash := sha256.Sum256(expectedData)
	expectedChecksum := hex.EncodeToString(hash[:])
	if checksum != expectedChecksum {
		t.Errorf("Expected checksum %s, got %s", expectedChecksum, checksum)
	}

	// Read assembled file and verify content
	file, err := os.Open(assembledPath)
	if err != nil {
		t.Fatalf("Failed to open assembled file: %v", err)
	}
	defer file.Close()

	buf := new(bytes.Buffer)
	buf.ReadFrom(file)
	if !bytes.Equal(buf.Bytes(), expectedData) {
		t.Error("Assembled file content mismatch")
	}
}

func TestGetChunkPaths(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "storage_test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	store, err := New(tmpDir)
	if err != nil {
		t.Fatalf("Failed to create storage: %v", err)
	}

	uploadID := "test-get-paths"

	// Create chunks
	for i := 0; i < 3; i++ {
		_, _, _, err := store.SaveChunk(uploadID, i, bytes.NewReader([]byte("data")))
		if err != nil {
			t.Fatalf("Failed to save chunk %d: %v", i, err)
		}
	}

	// Get paths
	paths, err := store.GetChunkPaths(uploadID)
	if err != nil {
		t.Fatalf("Failed to get chunk paths: %v", err)
	}

	if len(paths) != 3 {
		t.Errorf("Expected 3 paths, got %d", len(paths))
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
