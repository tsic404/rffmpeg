package db

import (
	"database/sql"
	"fmt"
	"time"
)

// LoadWSJobSeq returns the persisted WebSocket sequence counter for a job.
// Returns 0 when no counter exists — a fresh job starts numbering at 1, so
// callers treat 0 as "resume from scratch" without special-casing.
func (d *Database) LoadWSJobSeq(jobID string) (int64, error) {
	var lastSeq int64
	err := d.db.QueryRow(`SELECT last_seq FROM ws_job_seq WHERE job_id = ?`, jobID).Scan(&lastSeq)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("failed to load websocket seq for job %s: %w", jobID, err)
	}
	return lastSeq, nil
}

// SaveWSJobSeq persists the WebSocket sequence counter for a job. Called on
// every sequenced broadcast so a server restart resumes numbering where it
// left off instead of restarting at 1 (which clients read as lost data).
// Rows are keyed by job_id and upserted; the delete happens on terminal
// broadcast via DeleteWSJobSeq.
func (d *Database) SaveWSJobSeq(jobID string, lastSeq int64) error {
	_, err := d.db.Exec(`
		INSERT INTO ws_job_seq (job_id, last_seq, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(job_id) DO UPDATE SET last_seq = excluded.last_seq, updated_at = excluded.updated_at
	`, jobID, lastSeq, time.Now())
	if err != nil {
		return fmt.Errorf("failed to save websocket seq for job %s: %w", jobID, err)
	}
	return nil
}

// DeleteWSJobSeq drops the persisted sequence counter once the job reached a
// terminal state: no further sequenced data will be produced for it.
func (d *Database) DeleteWSJobSeq(jobID string) error {
	_, err := d.db.Exec(`DELETE FROM ws_job_seq WHERE job_id = ?`, jobID)
	if err != nil {
		return fmt.Errorf("failed to delete websocket seq for job %s: %w", jobID, err)
	}
	return nil
}
