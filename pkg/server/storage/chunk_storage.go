package storage

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// Errors reported by AssembleChunks when the session's chunks on disk do not
// form the file the session declared. They describe a client-correctable
// condition (resend the gap, fix the declared sizes), so callers map them to
// 400 instead of a 5xx storage failure.
var (
	// ErrChunkMissing means an index in [0, totalChunks) has no chunk file.
	ErrChunkMissing = errors.New("chunk missing")
	// ErrChunkSizeMismatch means a chunk's byte count differs from the size
	// the session's chunk_size/file_size imply.
	ErrChunkSizeMismatch = errors.New("chunk size mismatch")
	// ErrAssembledSizeMismatch means the chunk set cannot add up to file_size
	// at all (the session's chunking is inconsistent).
	ErrAssembledSizeMismatch = errors.New("assembled size mismatch")
)

// ChunkPath returns the canonical on-disk path of a chunk. Chunk files are
// addressed by index, never by directory enumeration, so assembly cannot
// silently skip or reorder a chunk.
func (s *Storage) ChunkPath(uploadID string, chunkIndex int) string {
	return filepath.Join(s.baseDir, "chunks", uploadID, fmt.Sprintf("chunk_%d", chunkIndex))
}

// SaveChunk stores one chunk atomically and returns its path, size and SHA256
// checksum.
//
// The bytes are written to a temporary file in the chunk directory, fsynced,
// and only then renamed onto the canonical chunk path. A crash mid-write
// therefore leaves either the previous chunk or no chunk at all — never a
// truncated chunk_N that a later retry would mistake for a completed upload.
func (s *Storage) SaveChunk(uploadID string, chunkIndex int, reader io.Reader) (string, int64, string, error) {
	// The chunk body is read from the network while the lock is held, so it
	// must be the session-local lock: the files/ lock would block downloads
	// for the length of a slow transfer.
	defer s.lockUpload(uploadID)()

	chunkDir := filepath.Join(s.baseDir, "chunks", uploadID)
	if err := os.MkdirAll(chunkDir, 0755); err != nil {
		return "", 0, "", fmt.Errorf("failed to create chunk directory: %w", err)
	}

	tmpFile, err := os.CreateTemp(chunkDir, fmt.Sprintf(".chunk_%d.tmp-", chunkIndex))
	if err != nil {
		return "", 0, "", fmt.Errorf("failed to create chunk file: %w", err)
	}
	tmpPath := tmpFile.Name()
	committed := false
	defer func() {
		if !committed {
			os.Remove(tmpPath)
		}
	}()

	// Calculate checksum while copying
	hash := sha256.New()
	size, err := io.Copy(io.MultiWriter(tmpFile, hash), reader)
	if err != nil {
		tmpFile.Close()
		return "", 0, "", fmt.Errorf("failed to write chunk: %w", err)
	}

	// fsync before the rename: renaming an unsynced file can leave a
	// zero-length chunk_N after a power loss, which is exactly the "looks
	// uploaded, is truncated" state this path exists to prevent.
	if err := tmpFile.Sync(); err != nil {
		tmpFile.Close()
		return "", 0, "", fmt.Errorf("failed to sync chunk file: %w", err)
	}
	// Close explicitly rather than via defer: on delayed-allocation
	// filesystems (ext4 delalloc) and NFS, ENOSPC can surface only at
	// close/flush, after io.Copy has returned success. Swallowing the close
	// error would mark a truncated chunk as uploaded.
	if err := tmpFile.Close(); err != nil {
		return "", 0, "", fmt.Errorf("failed to close chunk file: %w", err)
	}

	chunkPath := s.ChunkPath(uploadID, chunkIndex)
	if err := os.Rename(tmpPath, chunkPath); err != nil {
		return "", 0, "", fmt.Errorf("failed to commit chunk file: %w", err)
	}
	committed = true

	// Make the rename itself durable. Without this a crash can lose the
	// directory entry while the data is already on disk — the upload then
	// reads as a missing chunk, which is safe but forces a needless resend.
	if err := syncDir(chunkDir); err != nil {
		return "", 0, "", fmt.Errorf("failed to sync chunk directory: %w", err)
	}

	checksum := hex.EncodeToString(hash.Sum(nil))
	return chunkPath, size, checksum, nil
}

// syncDir fsyncs a directory so a completed rename survives a crash.
func syncDir(dir string) error {
	handle, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer handle.Close()
	return handle.Sync()
}

// OpenChunk opens a chunk file for reading
func (s *Storage) OpenChunk(uploadID string, chunkIndex int) (*os.File, error) {
	defer s.lockUpload(uploadID)()

	file, err := os.Open(s.ChunkPath(uploadID, chunkIndex))
	if err != nil {
		return nil, fmt.Errorf("failed to open chunk: %w", err)
	}
	return file, nil
}

// DeleteChunk deletes a single chunk file
func (s *Storage) DeleteChunk(uploadID string, chunkIndex int) error {
	defer s.lockUpload(uploadID)()

	return os.Remove(s.ChunkPath(uploadID, chunkIndex))
}

// DeleteChunkDirectory deletes all chunks for an upload session
func (s *Storage) DeleteChunkDirectory(uploadID string) error {
	defer s.lockUpload(uploadID)()

	chunkDir := filepath.Join(s.baseDir, "chunks", uploadID)
	return os.RemoveAll(chunkDir)
}

// chunkSource is one input of an assembly: the chunk file and the byte count
// the session says it must hold.
type chunkSource struct {
	path string
	size int64
}

// AssembleChunks combines the session's chunks into a single file in the files
// directory and returns its temporary path, size and SHA256 checksum. The
// caller owns the result and publishes it only after verifying any declared
// checksum against the measured one.
//
// Chunks are read by explicit index and must carry exactly the size the
// session's chunk_size/file_size imply; a missing or truncated chunk fails the
// assembly instead of yielding a short file that looks like a successful upload.
func (s *Storage) AssembleChunks(uploadID string, totalChunks int, chunkSize, fileSize int64) (string, int64, string, error) {
	// Assembly only touches this session's chunks, so the session lock keeps
	// downloads and other uploads running while a large file is copied.
	defer s.lockUpload(uploadID)()

	if totalChunks <= 0 || chunkSize <= 0 || fileSize <= 0 {
		return "", 0, "", fmt.Errorf("%w: session declares file_size=%d chunk_size=%d total_chunks=%d",
			ErrAssembledSizeMismatch, fileSize, chunkSize, totalChunks)
	}

	// The last chunk holds fileSize-(totalChunks-1)*chunkSize bytes. Check the
	// multiplication against fileSize before performing it: totalChunks comes
	// from the client, and an overflowing product would wrap into a bogus
	// "valid" session.
	if int64(totalChunks-1) > (fileSize-1)/chunkSize {
		return "", 0, "", fmt.Errorf("%w: session declares file_size=%d chunk_size=%d total_chunks=%d",
			ErrAssembledSizeMismatch, fileSize, chunkSize, totalChunks)
	}
	lastChunkSize := fileSize - chunkSize*int64(totalChunks-1)
	if lastChunkSize <= 0 || lastChunkSize > chunkSize {
		return "", 0, "", fmt.Errorf("%w: session declares file_size=%d chunk_size=%d total_chunks=%d",
			ErrAssembledSizeMismatch, fileSize, chunkSize, totalChunks)
	}

	// Verify the whole chunk set before copying anything, so a missing or
	// short chunk fails fast instead of after a near-full assembly.
	// Capacity is capped: totalChunks is client-declared, and reserving a slot
	// per declared chunk would let one init+complete allocate terabytes.
	const maxChunkSourceCapacity = 1024
	sources := make([]chunkSource, 0, min(totalChunks, maxChunkSourceCapacity))
	var expectedTotal int64
	for i := range totalChunks {
		expected := chunkSize
		if i == totalChunks-1 {
			expected = lastChunkSize
		}
		expectedTotal += expected

		path := s.ChunkPath(uploadID, i)
		info, err := os.Stat(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return "", 0, "", fmt.Errorf("%w: chunk %d", ErrChunkMissing, i)
			}
			return "", 0, "", fmt.Errorf("failed to stat chunk %d: %w", i, err)
		}
		if !info.Mode().IsRegular() || info.Size() != expected {
			return "", 0, "", fmt.Errorf("%w: chunk %d has %d bytes, want %d",
				ErrChunkSizeMismatch, i, info.Size(), expected)
		}
		sources = append(sources, chunkSource{path: path, size: expected})
	}
	if expectedTotal != fileSize {
		return "", 0, "", fmt.Errorf("%w: chunks add up to %d bytes, want %d",
			ErrAssembledSizeMismatch, expectedTotal, fileSize)
	}

	fileDir := filepath.Join(s.baseDir, "files")
	if err := os.MkdirAll(fileDir, 0755); err != nil {
		return "", 0, "", fmt.Errorf("failed to create files directory: %w", err)
	}

	// Assemble inside files/: the final rename must stay on the same
	// filesystem to be atomic. The temporary name never matches a content
	// hash, so no reader can address a half-written file.
	tmpFile, err := os.CreateTemp(fileDir, ".assemble-*")
	if err != nil {
		return "", 0, "", fmt.Errorf("failed to create final file: %w", err)
	}
	tmpPath := tmpFile.Name()
	committed := false
	defer func() {
		if !committed {
			os.Remove(tmpPath)
		}
	}()

	totalSize, checksum, err := assembleChunksInto(tmpFile, sources)
	if err != nil {
		return "", 0, "", err
	}
	if totalSize != fileSize {
		return "", 0, "", fmt.Errorf("%w: assembled %d bytes, want %d",
			ErrAssembledSizeMismatch, totalSize, fileSize)
	}

	committed = true
	return tmpPath, totalSize, checksum, nil
}

// assembleChunksInto copies the ordered chunk files into dst and closes it,
// computing the total size and SHA256 checksum. The close error is captured
// explicitly (not deferred) because delayed-allocation and NFS filesystems can
// report ENOSPC only at flush/close time; discarding it would return success
// on a truncated file.
func assembleChunksInto(dst io.WriteCloser, sources []chunkSource) (int64, string, error) {
	hash := sha256.New()
	writer := io.MultiWriter(dst, hash)
	var totalSize int64

	for _, src := range sources {
		chunkFile, err := os.Open(src.path)
		if err != nil {
			dst.Close()
			return 0, "", fmt.Errorf("failed to open chunk %s: %w", src.path, err)
		}

		size, err := io.Copy(writer, chunkFile)
		chunkFile.Close()
		if err != nil {
			dst.Close()
			return 0, "", fmt.Errorf("failed to copy chunk %s: %w", src.path, err)
		}
		// The stat pass above validated the size; re-check the copied count
		// because the chunk could have been replaced between stat and copy.
		if size != src.size {
			dst.Close()
			return 0, "", fmt.Errorf("%w: chunk %s changed during assembly (%d bytes, want %d)",
				ErrChunkSizeMismatch, src.path, size, src.size)
		}
		totalSize += size
	}

	if err := dst.Close(); err != nil {
		return 0, "", fmt.Errorf("failed to close assembled file: %w", err)
	}

	return totalSize, hex.EncodeToString(hash.Sum(nil)), nil
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

// ChunkExists reports whether the chunk is stored *complete*: the file must
// exist and hold exactly expectedSize bytes. A truncated chunk left by a
// crash therefore reads as missing, and the retry rewrites it.
func (s *Storage) ChunkExists(uploadID string, chunkIndex int, expectedSize int64) bool {
	defer s.lockUpload(uploadID)()

	info, err := os.Stat(s.ChunkPath(uploadID, chunkIndex))
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	return info.Size() == expectedSize
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
