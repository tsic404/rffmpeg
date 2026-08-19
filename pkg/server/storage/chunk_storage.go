package storage

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// SaveChunk saves a single chunk to temporary storage
func (s *Storage) SaveChunk(uploadID string, chunkIndex int, reader io.Reader) (string, int64, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	chunkDir := filepath.Join(s.baseDir, "chunks", uploadID)
	if err := os.MkdirAll(chunkDir, 0755); err != nil {
		return "", 0, "", fmt.Errorf("failed to create chunk directory: %w", err)
	}

	chunkPath := filepath.Join(chunkDir, fmt.Sprintf("chunk_%d", chunkIndex))
	file, err := os.Create(chunkPath)
	if err != nil {
		return "", 0, "", fmt.Errorf("failed to create chunk file: %w", err)
	}
	defer file.Close()

	// Calculate checksum while copying
	hash := sha256.New()
	writer := io.MultiWriter(file, hash)
	size, err := io.Copy(writer, reader)
	if err != nil {
		os.Remove(chunkPath)
		return "", 0, "", fmt.Errorf("failed to write chunk: %w", err)
	}

	checksum := hex.EncodeToString(hash.Sum(nil))
	return chunkPath, size, checksum, nil
}

// OpenChunk opens a chunk file for reading
func (s *Storage) OpenChunk(uploadID string, chunkIndex int) (*os.File, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	chunkPath := filepath.Join(s.baseDir, "chunks", uploadID, fmt.Sprintf("chunk_%d", chunkIndex))
	file, err := os.Open(chunkPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open chunk: %w", err)
	}
	return file, nil
}

// DeleteChunk deletes a single chunk file
func (s *Storage) DeleteChunk(uploadID string, chunkIndex int) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	chunkPath := filepath.Join(s.baseDir, "chunks", uploadID, fmt.Sprintf("chunk_%d", chunkIndex))
	return os.Remove(chunkPath)
}

// DeleteChunkDirectory deletes all chunks for an upload session
func (s *Storage) DeleteChunkDirectory(uploadID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	chunkDir := filepath.Join(s.baseDir, "chunks", uploadID)
	return os.RemoveAll(chunkDir)
}

// AssembleChunks combines all chunks into a single file
func (s *Storage) AssembleChunks(uploadID string, fileID string, chunkPaths []string) (string, int64, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Sort chunk paths by their index to ensure correct order
	sort.Slice(chunkPaths, func(i, j int) bool {
		// Extract chunk index from filename
		var idxI, idxJ int
		fmt.Sscanf(filepath.Base(chunkPaths[i]), "chunk_%d", &idxI)
		fmt.Sscanf(filepath.Base(chunkPaths[j]), "chunk_%d", &idxJ)
		return idxI < idxJ
	})

	// Create the final file
	fileDir := filepath.Join(s.baseDir, "files")
	if err := os.MkdirAll(fileDir, 0755); err != nil {
		return "", 0, "", fmt.Errorf("failed to create files directory: %w", err)
	}

	filePath := filepath.Join(fileDir, fileID)
	finalFile, err := os.Create(filePath)
	if err != nil {
		return "", 0, "", fmt.Errorf("failed to create final file: %w", err)
	}
	defer finalFile.Close()

	// Calculate checksum while assembling
	hash := sha256.New()
	writer := io.MultiWriter(finalFile, hash)
	var totalSize int64

	for _, chunkPath := range chunkPaths {
		chunkFile, err := os.Open(chunkPath)
		if err != nil {
			os.Remove(filePath)
			return "", 0, "", fmt.Errorf("failed to open chunk %s: %w", chunkPath, err)
		}

		size, err := io.Copy(writer, chunkFile)
		chunkFile.Close()
		if err != nil {
			os.Remove(filePath)
			return "", 0, "", fmt.Errorf("failed to copy chunk %s: %w", chunkPath, err)
		}
		totalSize += size
	}

	checksum := hex.EncodeToString(hash.Sum(nil))
	return filePath, totalSize, checksum, nil
}

// CalculateChunkChecksum calculates SHA256 checksum for a chunk
func (s *Storage) CalculateChunkChecksum(uploadID string, chunkIndex int) (string, error) {
	file, err := s.OpenChunk(uploadID, chunkIndex)
	if err != nil {
		return "", err
	}
	defer file.Close()

	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", fmt.Errorf("failed to calculate chunk checksum: %w", err)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// ChunkExists checks if a chunk file exists
func (s *Storage) ChunkExists(uploadID string, chunkIndex int) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	chunkPath := filepath.Join(s.baseDir, "chunks", uploadID, fmt.Sprintf("chunk_%d", chunkIndex))
	_, err := os.Stat(chunkPath)
	return err == nil
}

// GetChunkPaths returns all chunk paths for an upload session
func (s *Storage) GetChunkPaths(uploadID string) ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	chunkDir := filepath.Join(s.baseDir, "chunks", uploadID)
	entries, err := os.ReadDir(chunkDir)
	if err != nil {
		return nil, fmt.Errorf("failed to read chunk directory: %w", err)
	}

	var paths []string
	for _, entry := range entries {
		if !entry.IsDir() {
			paths = append(paths, filepath.Join(chunkDir, entry.Name()))
		}
	}

	return paths, nil
}

// ChunkStorage manages temporary chunk storage with cleanup
type ChunkStorage struct {
	storage *Storage
	mu      sync.Mutex
}

// NewChunkStorage creates a chunk storage wrapper
func NewChunkStorage(storage *Storage) *ChunkStorage {
	return &ChunkStorage{storage: storage}
}

// CleanupExpiredChunks removes all chunk directories that belong to expired sessions
func (cs *ChunkStorage) CleanupExpiredChunks(uploadIDs []string) error {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	for _, uploadID := range uploadIDs {
		if err := cs.storage.DeleteChunkDirectory(uploadID); err != nil {
			// Log but don't fail - some directories may already be gone
			continue
		}
	}
	return nil
}
