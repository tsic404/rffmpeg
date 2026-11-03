package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/tsic404/rffmpeg/pkg/protocol"
)

// File represents a file record in the database
type File struct {
	ID        string
	Filename  string
	Path      string
	Size      int64
	Checksum  sql.NullString
	CreatedAt time.Time
}

// ActiveInputFileIDs returns the set of input file IDs referenced by
// non-terminal jobs (pending, queued, running). The input file cleaner uses
// this to avoid deleting a content-addressed blob a live job still needs.
// It fails closed: a malformed input_files row returns an error so the caller
// aborts the sweep rather than treating that row's blobs as unreferenced and
// evicting them.
func (d *Database) ActiveInputFileIDs() (map[string]struct{}, error) {
	rows, err := d.db.Query(`
		SELECT input_files FROM jobs
		WHERE status IN (?, ?, ?)
	`, protocol.JobStatusPending, protocol.JobStatusQueued, protocol.JobStatusRunning)
	if err != nil {
		return nil, fmt.Errorf("failed to query active job input files: %w", err)
	}
	defer rows.Close()

	ids := make(map[string]struct{})
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, fmt.Errorf("failed to scan input files: %w", err)
		}
		var files []string
		if err := json.Unmarshal([]byte(raw), &files); err != nil {
			return nil, fmt.Errorf("failed to parse input_files JSON: %w", err)
		}
		for _, f := range files {
			if f != "" {
				ids[f] = struct{}{}
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to iterate input files: %w", err)
	}
	return ids, nil
}

// CreateFile creates a new file record
func (d *Database) CreateFile(filename, path string, size int64, checksum *string) (*File, error) {
	id := uuid.New().String()
	now := time.Now()

	var checksumVal interface{}
	if checksum != nil {
		checksumVal = *checksum
	}

	_, err := d.db.Exec(`
		INSERT INTO files (id, filename, path, size, checksum, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
	`, id, filename, path, size, checksumVal, now)

	if err != nil {
		return nil, fmt.Errorf("failed to create file: %w", err)
	}

	return d.GetFile(id)
}

// GetFile retrieves a file by ID
func (d *Database) GetFile(id string) (*File, error) {
	file := &File{}
	err := d.db.QueryRow(`
		SELECT id, filename, path, size, checksum, created_at
		FROM files WHERE id = ?
	`, id).Scan(
		&file.ID, &file.Filename, &file.Path, &file.Size,
		&file.Checksum, &file.CreatedAt,
	)

	if err == sql.ErrNoRows {
		return nil, protocol.ErrInvalidFileID
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get file: %w", err)
	}

	return file, nil
}
