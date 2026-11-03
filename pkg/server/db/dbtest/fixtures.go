// Package dbtest provides fixtures for tests that need to stage job rows
// directly.
//
// Production job writes go through ownership-guarded writers
// (UpdateJobRunningStatusWithOwner, UpdateJobTerminalStatusWithOwner…) or
// through dedicated state-machine methods (AssignJobToWorker, CancelJob,
// FailJob, MigrateJobsFromWorker, RecoverState, the two sweeps), which enforce
// their own preconditions. Tests, however, often need to lay down arbitrary —
// even impossible — states so the code under test can be exercised from them
// (e.g. a job completed without an owner, or a terminal transition that skips
// running). These fixtures provide that setup only; no production code
// imports this package.
package dbtest

import (
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/tsic404/rffmpeg/pkg/protocol"
)

// DB is the subset of *db.Database the fixtures need. Keeping it an interface
// avoids importing the db package, so both package db and package db_test
// (which import db) can use these fixtures without an import cycle.
type DB interface {
	GetDB() *sql.DB
	NotifyTerminal(jobID string)
}

// SetJobStatus stages a job status (and optional failure classification)
// without an ownership guard. It mirrors the schema-level write the tests used
// to reach through the removed unguarded production writer: a terminal job is
// never overwritten, except failed→completed/failed, which the worker
// retry-after-failure staging needs. A terminal transition notifies in-process
// waiters and the terminal hook, like the production terminal writers.
func SetJobStatus(t *testing.T, d DB, id string, status protocol.JobStatus, exitCode *int, errMsg *string, failureType, failureDetails *string) error {
	t.Helper()
	return setJobStatus(d, id, status, exitCode, errMsg, failureType, failureDetails)
}

func setJobStatus(d DB, id string, status protocol.JobStatus, exitCode *int, errMsg *string, failureType, failureDetails *string) error {
	now := time.Now()

	var startedAt, finishedAt *time.Time
	if status == protocol.JobStatusRunning {
		startedAt = &now
	}
	if protocol.IsTerminalStatus(status) {
		finishedAt = &now
	}

	var result sql.Result
	var err error
	if status == protocol.JobStatusCompleted || status == protocol.JobStatusFailed {
		result, err = d.GetDB().Exec(`
			UPDATE jobs SET status = ?, updated_at = ?, exit_code = ?, error = ?,
			                failure_type = COALESCE(?, failure_type),
			                retryable = CASE WHEN COALESCE(?, failure_type) IN ('TIMEOUT', 'WORKER_CRASH') THEN 1 ELSE 0 END,
			                failure_details = COALESCE(?, failure_details),
			                started_at = COALESCE(started_at, ?), finished_at = ?,
			                assigned_worker = COALESCE(assigned_worker, worker_id),
			                worker_name = COALESCE(worker_name, (SELECT name FROM workers WHERE id = jobs.worker_id))
			WHERE id = ?
			  AND NOT EXISTS (
			      SELECT 1 FROM jobs WHERE id = ? AND status IN (?, ?, ?)
			  )
		`, status, now, exitCode, errMsg, failureType, failureType, failureDetails,
			startedAt, finishedAt, id, id,
			protocol.JobStatusCompleted, protocol.JobStatusCancelled, protocol.JobStatusTimeout)
	} else {
		result, err = d.GetDB().Exec(`
			UPDATE jobs SET status = ?, updated_at = ?, exit_code = ?, error = ?,
			                failure_type = COALESCE(?, failure_type),
			                retryable = CASE WHEN COALESCE(?, failure_type) IN ('TIMEOUT', 'WORKER_CRASH') THEN 1 ELSE 0 END,
			                failure_details = COALESCE(?, failure_details),
			                started_at = COALESCE(started_at, ?), finished_at = ?
			WHERE id = ?
			  AND NOT EXISTS (
			      SELECT 1 FROM jobs WHERE id = ? AND status IN (?, ?, ?, ?)
			  )
		`, status, now, exitCode, errMsg, failureType, failureType, failureDetails,
			startedAt, finishedAt, id, id,
			protocol.JobStatusCompleted, protocol.JobStatusFailed,
			protocol.JobStatusCancelled, protocol.JobStatusTimeout)
	}
	if err != nil {
		return fmt.Errorf("stage job status: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("stage job status rows: %w", err)
	}
	if rows == 0 {
		var exists int
		if err := d.GetDB().QueryRow(`SELECT COUNT(*) FROM jobs WHERE id = ?`, id).Scan(&exists); err != nil {
			return fmt.Errorf("stage job status lookup: %w", err)
		}
		if exists == 0 {
			return protocol.ErrJobNotFound
		}
		return protocol.ErrJobTerminal
	}
	if protocol.IsTerminalStatus(status) {
		d.NotifyTerminal(id)
	}
	return nil
}

// SetJobProgress stages progress/ETA without an ownership guard, mirroring the
// removed unguarded writer: the write is monotonic, so a regressing value is
// rejected with accepted=false.
func SetJobProgress(t *testing.T, d DB, id string, progress float64, etaSeconds int) (bool, error) {
	t.Helper()
	res, err := d.GetDB().Exec(`
		UPDATE jobs SET progress_percent = ?, eta_seconds = ?, updated_at = ?
		WHERE id = ? AND progress_percent <= ?
	`, progress, etaSeconds, time.Now(), id, progress)
	if err != nil {
		return false, fmt.Errorf("stage job progress: %w", err)
	}
	written, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("stage job progress rows: %w", err)
	}
	return written > 0, nil
}
