package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/tsix404/rffmpeg/pkg/protocol"
)

// UploadSession represents a chunked upload session
type UploadSession struct {
	ID             string
	Filename       string
	FileSize       int64
	ChunkSize      int64
	TotalChunks    int
	UploadedChunks string // JSON array of completed chunk indices
	FileChecksum   sql.NullString
	Status         protocol.UploadSessionStatus
	CreatedAt      time.Time
	UpdatedAt      time.Time
	ExpiresAt      sql.NullTime
}

// UploadChunk represents a single uploaded chunk
type UploadChunk struct {
	ID         string
	UploadID   string
	ChunkIndex int
	ChunkSize  int64
	Checksum   string
	Path       string
	CreatedAt  time.Time
}

// CreateUploadSession creates a new upload session for chunked upload
func (d *Database) CreateUploadSession(filename string, fileSize, chunkSize int64, totalChunks int, checksum *string) (*UploadSession, error) {
	id := uuid.New().String()
	now := time.Now()
	expiresAt := now.Add(24 * time.Hour) // Sessions expire in 24 hours

	var checksumVal interface{}
	if checksum != nil {
		checksumVal = *checksum
	}

	_, err := d.db.Exec(`
		INSERT INTO upload_sessions (id, filename, file_size, chunk_size, total_chunks, uploaded_chunks, file_checksum, status, created_at, updated_at, expires_at)
		VALUES (?, ?, ?, ?, ?, '[]', ?, ?, ?, ?, ?)
	`, id, filename, fileSize, chunkSize, totalChunks, checksumVal, protocol.UploadSessionStatusInProgress, now, now, expiresAt)

	if err != nil {
		return nil, fmt.Errorf("failed to create upload session: %w", err)
	}

	return d.GetUploadSession(id)
}

// GetUploadSession retrieves an upload session by ID
func (d *Database) GetUploadSession(id string) (*UploadSession, error) {
	session := &UploadSession{}
	err := d.db.QueryRow(`
		SELECT id, filename, file_size, chunk_size, total_chunks, uploaded_chunks, file_checksum, status, created_at, updated_at, expires_at
		FROM upload_sessions WHERE id = ?
	`, id).Scan(
		&session.ID, &session.Filename, &session.FileSize, &session.ChunkSize, &session.TotalChunks,
		&session.UploadedChunks, &session.FileChecksum, &session.Status,
		&session.CreatedAt, &session.UpdatedAt, &session.ExpiresAt,
	)

	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("upload session not found: %s", id)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get upload session: %w", err)
	}

	return session, nil
}

// UpdateUploadSessionChunks updates the uploaded chunks list for a session.
// This method is safe for concurrent use — it uses a retry loop to handle
// read-modify-write conflicts that can occur when multiple chunks complete
// at the same time.
func (d *Database) UpdateUploadSessionChunks(uploadID string, chunkIndex int) error {
	now := time.Now()

	// Retry up to 3 times to handle concurrent write conflicts.
	// Under high concurrency, two goroutines may read the same session state
	// before either writes, causing one update to be lost. Retrying with
	// a fresh read resolves this.
	const maxRetries = 3
	for attempt := 0; attempt < maxRetries; attempt++ {
		if attempt > 0 {
			// Small backoff before retry
			time.Sleep(time.Millisecond * time.Duration(10<<attempt))
		}

		// Get current session
		session, err := d.GetUploadSession(uploadID)
		if err != nil {
			return err
		}

		// Parse existing chunks
		var uploadedChunks []int
		if err := json.Unmarshal([]byte(session.UploadedChunks), &uploadedChunks); err != nil {
			return fmt.Errorf("failed to parse uploaded chunks: %w", err)
		}

		// Check if chunk already uploaded
		alreadyPresent := false
		for _, idx := range uploadedChunks {
			if idx == chunkIndex {
				alreadyPresent = true
				break
			}
		}
		if alreadyPresent {
			return nil // Already uploaded, not an error
		}

		// Add new chunk
		uploadedChunks = append(uploadedChunks, chunkIndex)

		// Marshal back to JSON
		chunksJSON, err := json.Marshal(uploadedChunks)
		if err != nil {
			return fmt.Errorf("failed to marshal uploaded chunks: %w", err)
		}

		// Use atomic update to avoid race: only update if the session still
		// has the original uploaded_chunks value we read.
		result, err := d.db.Exec(`
			UPDATE upload_sessions
			SET uploaded_chunks = ?, updated_at = ?
			WHERE id = ? AND uploaded_chunks = ?
		`, string(chunksJSON), now, uploadID, session.UploadedChunks)

		if err != nil {
			return fmt.Errorf("failed to update upload session: %w", err)
		}

		rowsAffected, _ := result.RowsAffected()
		if rowsAffected > 0 {
			return nil // Success
		}

		// RowsAffected == 0 means someone else updated the session.
		// Retry with fresh data.
	}

	return fmt.Errorf("failed to update upload session %s after %d retries: concurrent modification conflict", uploadID, maxRetries)
}

// CompleteUploadSession marks an upload session as completed
func (d *Database) CompleteUploadSession(id string) error {
	now := time.Now()
	_, err := d.db.Exec(`
		UPDATE upload_sessions SET status = ?, updated_at = ? WHERE id = ?
	`, protocol.UploadSessionStatusCompleted, now, id)

	if err != nil {
		return fmt.Errorf("failed to complete upload session: %w", err)
	}

	return nil
}

// CancelUploadSession marks an upload session as cancelled
func (d *Database) CancelUploadSession(id string) error {
	now := time.Now()
	_, err := d.db.Exec(`
		UPDATE upload_sessions SET status = ?, updated_at = ? WHERE id = ?
	`, protocol.UploadSessionStatusCancelled, now, id)

	if err != nil {
		return fmt.Errorf("failed to cancel upload session: %w", err)
	}

	return nil
}

// ExpireUploadSessions marks expired upload sessions
func (d *Database) ExpireUploadSessions() (int64, error) {
	now := time.Now()
	result, err := d.db.Exec(`
		UPDATE upload_sessions SET status = ? 
		WHERE status = ? AND expires_at IS NOT NULL AND expires_at < ?
	`, protocol.UploadSessionStatusExpired, protocol.UploadSessionStatusInProgress, now)

	if err != nil {
		return 0, fmt.Errorf("failed to expire upload sessions: %w", err)
	}

	return result.RowsAffected()
}

// DeleteUploadSession removes an upload session and its chunks
func (d *Database) DeleteUploadSession(id string) error {
	_, err := d.db.Exec(`DELETE FROM upload_sessions WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("failed to delete upload session: %w", err)
	}
	return nil
}

// CreateUploadChunk creates a chunk record. Inserting a (upload_id,
// chunk_index) pair that already exists is treated as idempotent success
// (TSI-2359): the UNIQUE index on upload_chunks(upload_id, chunk_index)
// rejects the duplicate insert, and the original row is returned.
func (d *Database) CreateUploadChunk(uploadID string, chunkIndex int, chunkSize int64, checksum, path string) (*UploadChunk, error) {
	id := uuid.New().String()
	now := time.Now()

	_, err := d.db.Exec(`
		INSERT INTO upload_chunks (id, upload_id, chunk_index, chunk_size, checksum, path, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(upload_id, chunk_index) DO NOTHING
	`, id, uploadID, chunkIndex, chunkSize, checksum, path, now)

	if err != nil {
		return nil, fmt.Errorf("failed to create upload chunk: %w", err)
	}

	return d.GetUploadChunk(uploadID, chunkIndex)
}

// GetUploadChunk retrieves a chunk by upload ID and index
func (d *Database) GetUploadChunk(uploadID string, chunkIndex int) (*UploadChunk, error) {
	chunk := &UploadChunk{}
	err := d.db.QueryRow(`
		SELECT id, upload_id, chunk_index, chunk_size, checksum, path, created_at
		FROM upload_chunks WHERE upload_id = ? AND chunk_index = ?
	`, uploadID, chunkIndex).Scan(
		&chunk.ID, &chunk.UploadID, &chunk.ChunkIndex, &chunk.ChunkSize, &chunk.Checksum, &chunk.Path, &chunk.CreatedAt,
	)

	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("upload chunk not found: upload_id=%s, chunk_index=%d", uploadID, chunkIndex)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get upload chunk: %w", err)
	}

	return chunk, nil
}

// GetUploadChunks retrieves all chunks for an upload session
func (d *Database) GetUploadChunks(uploadID string) ([]*UploadChunk, error) {
	rows, err := d.db.Query(`
		SELECT id, upload_id, chunk_index, chunk_size, checksum, path, created_at
		FROM upload_chunks WHERE upload_id = ? ORDER BY chunk_index ASC
	`, uploadID)

	if err != nil {
		return nil, fmt.Errorf("failed to get upload chunks: %w", err)
	}
	defer rows.Close()

	var chunks []*UploadChunk
	for rows.Next() {
		chunk := &UploadChunk{}
		err := rows.Scan(
			&chunk.ID, &chunk.UploadID, &chunk.ChunkIndex, &chunk.ChunkSize, &chunk.Checksum, &chunk.Path, &chunk.CreatedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan upload chunk: %w", err)
		}
		chunks = append(chunks, chunk)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating upload chunks: %w", err)
	}

	return chunks, nil
}

// DeleteUploadChunks removes all chunks for an upload session
func (d *Database) DeleteUploadChunks(uploadID string) error {
	_, err := d.db.Exec(`DELETE FROM upload_chunks WHERE upload_id = ?`, uploadID)
	if err != nil {
		return fmt.Errorf("failed to delete upload chunks: %w", err)
	}
	return nil
}

// ChunkExists checks if a chunk already exists. A query failure returns an
// error rather than a silent false: treating "unknown" as "missing" would
// let the caller overwrite an existing chunk under load (TSI-2359).
func (d *Database) ChunkExists(uploadID string, chunkIndex int) (bool, error) {
	var count int
	err := d.db.QueryRow(`
		SELECT COUNT(*) FROM upload_chunks WHERE upload_id = ? AND chunk_index = ?
	`, uploadID, chunkIndex).Scan(&count)

	if err != nil {
		return false, fmt.Errorf("failed to check chunk existence: %w", err)
	}

	return count > 0, nil
}
