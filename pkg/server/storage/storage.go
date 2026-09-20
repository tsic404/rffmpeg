package storage

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"
)

// Storage manages file storage on the local filesystem
type Storage struct {
	baseDir string
	mu      sync.RWMutex
}

// New creates a new storage instance
func New(baseDir string) (*Storage, error) {
	if err := os.MkdirAll(baseDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create storage directory: %w", err)
	}

	return &Storage{baseDir: baseDir}, nil
}

// SaveFile saves a file to storage and returns the relative path, size, and SHA256 checksum.
func (s *Storage) SaveFile(fileID string, reader io.Reader) (string, int64, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	fileDir := filepath.Join(s.baseDir, "files")
	if err := os.MkdirAll(fileDir, 0755); err != nil {
		return "", 0, "", fmt.Errorf("failed to create files directory: %w", err)
	}

	filePath := filepath.Join(fileDir, fileID)
	file, err := os.Create(filePath)
	if err != nil {
		return "", 0, "", fmt.Errorf("failed to create file: %w", err)
	}
	defer file.Close()

	// Compute SHA256 checksum while writing
	hash := sha256.New()
	tee := io.TeeReader(reader, hash)
	size, err := io.Copy(file, tee)
	if err != nil {
		os.Remove(filePath)
		return "", 0, "", fmt.Errorf("failed to write file: %w", err)
	}

	checksum := hex.EncodeToString(hash.Sum(nil))
	return filePath, size, checksum, nil
}

// SaveFileByContent saves a file to storage using its SHA256 content hash as the file ID.
// This ensures the same file content always gets the same file ID, enabling
// cache key stability across uploads.
// If a file with the same content hash already exists, returns the existing file ID (dedup).
func (s *Storage) SaveFileByContent(reader io.Reader) (string, int64, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	fileDir := filepath.Join(s.baseDir, "files")
	if err := os.MkdirAll(fileDir, 0755); err != nil {
		return "", 0, "", fmt.Errorf("failed to create files directory: %w", err)
	}

	// Write to a temp file while computing SHA256
	tmpFile, err := os.CreateTemp(fileDir, "upload-*")
	if err != nil {
		return "", 0, "", fmt.Errorf("failed to create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath) // clean up temp file if not renamed

	hash := sha256.New()
	tee := io.TeeReader(reader, hash)
	size, err := io.Copy(tmpFile, tee)
	tmpFile.Close()
	if err != nil {
		return "", 0, "", fmt.Errorf("failed to write file: %w", err)
	}

	checksum := hex.EncodeToString(hash.Sum(nil))
	finalPath := filepath.Join(fileDir, checksum)

	// Check if file with same content already exists (dedup)
	if _, err := os.Stat(finalPath); err == nil {
		// File already exists — refresh its mtime so the TTL sweep keeps a
		// still-reused blob instead of treating a re-upload as age since the
		// original write. A failed touch must not fail the upload (the blob is
		// still valid), but it is logged: a silent failure would let the TTL
		// sweep evict a still-reused blob.
		now := time.Now()
		if err := os.Chtimes(finalPath, now, now); err != nil {
			log.Printf("Storage: failed to refresh mtime on dedup for %s: %v", checksum, err)
		}
		return finalPath, size, checksum, nil
	}

	// Rename temp file to content-hash-based path
	if err := os.Rename(tmpPath, finalPath); err != nil {
		return "", 0, "", fmt.Errorf("failed to rename file: %w", err)
	}

	return finalPath, size, checksum, nil
}

// GetFilePath returns the full path for a file ID
func (s *Storage) GetFilePath(fileID string) string {
	return filepath.Join(s.baseDir, "files", fileID)
}

// fileIDPattern restricts file IDs to lowercase hex SHA256 hashes (64 chars).
// File IDs are content hashes; anything else (e.g. "../..", empty, path
// separators) must never reach GetFilePath/OpenFile/DeleteFile.
var fileIDPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ValidateFileID reports whether fileID is a well-formed content-hash file ID.
func ValidateFileID(fileID string) bool {
	return fileIDPattern.MatchString(fileID)
}

// outputFileIDPattern accepts standard UUID strings — the format UploadJobOutput
// assigns to output files (uuid.New().String()).
var outputFileIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// ValidateOutputFileID reports whether fileID is a well-formed output file ID.
// The strict charset blocks traversal fragments from reaching OpenOutput's
// directory walk / path joins.
func ValidateOutputFileID(fileID string) bool {
	return outputFileIDPattern.MatchString(fileID)
}

// FileExists checks if a file exists
func (s *Storage) FileExists(fileID string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, err := os.Stat(s.GetFilePath(fileID))
	return err == nil
}

// OpenFile opens a file for reading
func (s *Storage) OpenFile(fileID string) (*os.File, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	path := s.GetFilePath(fileID)
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("failed to open file: %w", err)
	}
	// A download is the last-use event the TTL sweep keys off: refresh mtime
	// so an actively-served blob is not evicted out from under a re-dispatch.
	// The open itself already succeeded, so a failed touch is logged rather
	// than failing the read.
	now := time.Now()
	if err := os.Chtimes(path, now, now); err != nil {
		log.Printf("Storage: failed to refresh mtime on download for %s: %v", fileID, err)
	}
	return file, nil
}

// TouchFile refreshes a blob's mtime (marking it recently used) and reports
// whether it exists. Callers about to reference a blob — most importantly
// SubmitJob, which validates inputs before committing the job — use this to
// keep the TTL sweep from evicting it between validation and job commit. The
// write lock serializes the touch with the sweep's per-file delete, closing
// the check-then-delete window.
//
// fileID is client-controlled at the submit boundary, so it is validated
// before it reaches the filesystem: a traversal fragment must never turn this
// mtime refresh into a write syscall outside files/.
func (s *Storage) TouchFile(fileID string) (bool, error) {
	if !ValidateFileID(fileID) {
		return false, nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	path := s.GetFilePath(fileID)
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}

	now := time.Now()
	if err := os.Chtimes(path, now, now); err != nil {
		return false, err
	}
	return true, nil
}

// DeleteFile deletes a file
func (s *Storage) DeleteFile(fileID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return os.Remove(s.GetFilePath(fileID))
}

// SaveOutput saves an output file for a job
func (s *Storage) SaveOutput(jobID, fileID string, reader io.Reader) (string, int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	outputDir := filepath.Join(s.baseDir, "output", jobID)
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		return "", 0, fmt.Errorf("failed to create output directory: %w", err)
	}

	filePath := filepath.Join(outputDir, fileID)
	file, err := os.Create(filePath)
	if err != nil {
		return "", 0, fmt.Errorf("failed to create output file: %w", err)
	}
	defer file.Close()

	size, err := io.Copy(file, reader)
	if err != nil {
		os.Remove(filePath)
		return "", 0, fmt.Errorf("failed to write output file: %w", err)
	}

	return filePath, size, nil
}

// GetOutputPath returns the path for an output file
func (s *Storage) GetOutputPath(jobID, fileID string) string {
	return filepath.Join(s.baseDir, "output", jobID, fileID)
}

// OpenOutput opens an output file for reading
func (s *Storage) OpenOutput(fileID string) (*os.File, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	// Search in all output directories
	outputDir := filepath.Join(s.baseDir, "output")
	var foundPath string

	err := filepath.Walk(outputDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && filepath.Base(path) == fileID {
			foundPath = path
			return filepath.SkipAll
		}
		return nil
	})

	if err != nil && err != filepath.SkipAll {
		return nil, fmt.Errorf("failed to search for output file: %w", err)
	}

	if foundPath == "" {
		return nil, fmt.Errorf("output file not found: %s", fileID)
	}

	return os.Open(foundPath)
}

// CalculateChecksum calculates SHA256 checksum of a reader
func CalculateChecksum(reader io.Reader) (string, error) {
	hash := sha256.New()
	if _, err := io.Copy(hash, reader); err != nil {
		return "", fmt.Errorf("failed to calculate checksum: %w", err)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// CleanupExpiredInputFiles removes content-addressed input blobs whose mtime
// is at or before cutoff, except those listed in inUse (still referenced by a
// non-terminal job). It returns the number of blobs removed. Only 64-char
// lowercase-hex names are considered: stray temp files in files/ are left
// alone so a concurrent upload is never interrupted.
//
// The directory scan runs under the read lock so a large blob set does not
// block uploads/downloads for the whole walk. Each delete re-stats the file's
// mtime under the write lock: a concurrent TouchFile (submit/download/dedup)
// that refreshed the mtime in the meantime is serialized with the delete, so a
// freshly-referenced blob is never evicted.
func (s *Storage) CleanupExpiredInputFiles(cutoff time.Time, inUse map[string]struct{}) (int, error) {
	filesDir := filepath.Join(s.baseDir, "files")

	s.mu.RLock()
	entries, err := os.ReadDir(filesDir)
	s.mu.RUnlock()
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("failed to read files directory: %w", err)
	}

	var candidates []string
	for _, entry := range entries {
		if entry.IsDir() || !ValidateFileID(entry.Name()) {
			continue
		}
		if _, ok := inUse[entry.Name()]; ok {
			continue
		}
		candidates = append(candidates, entry.Name())
	}

	removed := 0
	for _, name := range candidates {
		path := filepath.Join(filesDir, name)
		s.mu.Lock()
		info, statErr := os.Stat(path)
		if statErr == nil && !info.ModTime().After(cutoff) {
			if rmErr := os.Remove(path); rmErr == nil {
				removed++
			}
		}
		s.mu.Unlock()
	}
	return removed, nil
}
