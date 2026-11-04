package db

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/tsic404/rffmpeg/pkg/protocol"
)

// Job represents a job record in the database
type Job struct {
	ID              string
	Status          protocol.JobStatus
	InputFiles      string // JSON array stored as string
	Args            string // JSON array stored as string
	OutputFilename  string // Original output filename with extension
	StreamingOutput bool   // Output to stdout via WebSocket
	OutputFiles     string // JSON array stored as string
	WorkerID        sql.NullString
	AssignedWorker  sql.NullString // Worker ID of the last/current executor (survives terminal state)
	WorkerName      sql.NullString // Human-readable name of the assigned worker
	ExitCode        sql.NullInt32
	Error           sql.NullString
	FailureType     string        // Classified failure type
	FailureDetails  string        // Human-readable failure detail
	AutoHW          bool          // Enable automatic hardware encoder upgrade
	Cached          bool          // Result was served from the worker cache
	Timeout         sql.NullInt64 // Per-job ffmpeg execution budget in nanoseconds (nil = worker default)
	DirectPaths     string        // JSON array of direct output paths for pass-through mode
	ProgressPercent float64       // Current progress percentage (0-100)
	EtaSeconds      int           // Estimated seconds remaining (0 if unknown)
	CreatedAt       time.Time
	UpdatedAt       time.Time
	ClaimedAt       sql.NullTime // pending -> queued moment; NULL while unclaimed
	StartedAt       sql.NullTime
	FinishedAt      sql.NullTime
}

// CreateJob creates a new job record
func (d *Database) CreateJob(inputFiles, args, outputFilename string, autoHW bool) (*Job, error) {
	return d.CreateJobWithStreaming(inputFiles, args, outputFilename, autoHW, false, nil, "")
}

// CreateJobWithStreaming creates a new job record with streaming output option and optional direct paths
func (d *Database) CreateJobWithStreaming(inputFiles, args, outputFilename string, autoHW bool, streamingOutput bool, timeout *time.Duration, directPaths string) (*Job, error) {
	id := uuid.New().String()
	now := time.Now()

	var timeoutVal interface{}
	if timeout != nil {
		timeoutVal = int64(*timeout)
	}

	_, err := d.db.Exec(`
		INSERT INTO jobs (id, status, input_files, args, output_filename, streaming_output, output_files, auto_hw, timeout, direct_paths, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, '[]', ?, ?, ?, ?, ?)
	`, id, protocol.JobStatusPending, inputFiles, args, outputFilename, streamingOutput, autoHW, timeoutVal, directPaths, now, now)

	if err != nil {
		return nil, fmt.Errorf("failed to create job: %w", err)
	}

	return d.GetJob(id)
}

// CreateFailedJob persists a job that is terminal at submission — no worker
// can serve it (e.g. ENCODER_UNAVAILABLE). A single INSERT writes the job
// directly in the failed state, so a deterministic submit-time rejection is
// recorded atomically: there is no pending→failed window in which a crash
// would leave the job pending and later failed by the starvation sweep as
// NO_WORKER_AVAILABLE with a classification that contradicts the response.
func (d *Database) CreateFailedJob(inputFiles, args, outputFilename string, autoHW bool, streamingOutput bool, timeout *time.Duration, directPaths, failureType, errMsg string) (*Job, error) {
	id := uuid.New().String()
	now := time.Now()

	var timeoutVal interface{}
	if timeout != nil {
		timeoutVal = int64(*timeout)
	}

	const exitCode = -1
	retryable := protocol.FailureType(failureType).Retryable()
	_, err := d.db.Exec(`
		INSERT INTO jobs (id, status, input_files, args, output_filename, streaming_output, output_files, exit_code, error, failure_type, retryable, auto_hw, timeout, direct_paths, created_at, updated_at, finished_at)
		VALUES (?, ?, ?, ?, ?, ?, '[]', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, id, protocol.JobStatusFailed, inputFiles, args, outputFilename, streamingOutput, exitCode, errMsg, failureType, retryable, autoHW, timeoutVal, directPaths, now, now, now)

	if err != nil {
		return nil, fmt.Errorf("failed to create failed job: %w", err)
	}

	d.notifyTerminal(id)

	return d.GetJob(id)
}

// GetJob retrieves a job by ID
func (d *Database) GetJob(id string) (*Job, error) {
	job := &Job{}
	err := d.db.QueryRow(`
		SELECT id, status, input_files, args, output_filename, streaming_output, output_files, worker_id, exit_code, error,
	       failure_type, failure_details, auto_hw, cached, timeout, direct_paths, progress_percent, eta_seconds,
	       created_at, updated_at, claimed_at, started_at, finished_at, assigned_worker, worker_name
		FROM jobs WHERE id = ?
	`, id).Scan(
		&job.ID, &job.Status, &job.InputFiles, &job.Args, &job.OutputFilename, &job.StreamingOutput, &job.OutputFiles,
		&job.WorkerID, &job.ExitCode, &job.Error, &job.FailureType, &job.FailureDetails, &job.AutoHW, &job.Cached,
		&job.Timeout, &job.DirectPaths, &job.ProgressPercent, &job.EtaSeconds,
		&job.CreatedAt, &job.UpdatedAt, &job.ClaimedAt, &job.StartedAt, &job.FinishedAt, &job.AssignedWorker, &job.WorkerName,
	)

	if err == sql.ErrNoRows {
		return nil, protocol.ErrJobNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get job: %w", err)
	}

	return job, nil
}

// JobExists checks if a job with the given ID exists
func (d *Database) JobExists(id string) (bool, error) {
	var exists bool
	err := d.db.QueryRow(`
		SELECT EXISTS(SELECT 1 FROM jobs WHERE id = ?)
	`, id).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("failed to check job existence: %w", err)
	}
	return exists, nil
}

// UpdateJobTerminalStatusWithOwner updates a job to a terminal state only if
// the job is still owned by the reporting worker and still active (running or
// queued). This closes the double-execution window: after failover the job's
// worker_id points at the new worker, so a stale terminal report from the old
// worker (network partition survivor) matches zero rows and returns
// protocol.ErrJobNotOwned instead of overwriting the new owner's result.
// A report for a job already in a terminal state is equally rejected.
func (d *Database) UpdateJobTerminalStatusWithOwner(jobID, workerID string, status protocol.JobStatus, exitCode *int, errMsg *string, failureType, failureDetails *string) error {
	return d.updateJobTerminalStatusWithOwner(jobID, workerID, status, exitCode, errMsg, failureType, failureDetails, false)
}

// UpdateJobTerminalStatusWithOwnerAndCache is UpdateJobTerminalStatusWithOwner
// with the cached flag persisted on the terminal transition.
// cached is coerced to false unless the target status is completed.
func (d *Database) UpdateJobTerminalStatusWithOwnerAndCache(jobID, workerID string, status protocol.JobStatus, exitCode *int, errMsg *string, failureType, failureDetails *string, cached bool) error {
	return d.updateJobTerminalStatusWithOwner(jobID, workerID, status, exitCode, errMsg, failureType, failureDetails, cached)
}

func (d *Database) updateJobTerminalStatusWithOwner(jobID, workerID string, status protocol.JobStatus, exitCode *int, errMsg *string, failureType, failureDetails *string, cached bool) error {
	now := time.Now()

	if status != protocol.JobStatusCompleted {
		cached = false
	}
	finishedAt := now
	result, err := d.db.Exec(`
		UPDATE jobs SET status = ?, updated_at = ?, exit_code = ?,
		                error = ?,
		                failure_type = COALESCE(?, failure_type),
		                retryable = CASE WHEN COALESCE(?, failure_type) IN ('TIMEOUT', 'WORKER_CRASH') THEN 1 ELSE 0 END,
		                failure_details = COALESCE(?, failure_details),
		                finished_at = ?, cached = ?,
		                assigned_worker = COALESCE(assigned_worker, ?),
		                worker_name = COALESCE(worker_name, (SELECT name FROM workers WHERE id = ?))
		WHERE id = ? AND worker_id = ? AND status IN (?, ?)
	`, status, now, exitCode, errMsg, failureType, failureType, failureDetails, finishedAt, cached,
		workerID, workerID, jobID, workerID, protocol.JobStatusRunning, protocol.JobStatusQueued)
	if err != nil {
		return fmt.Errorf("failed to update job terminal status: %w", err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to check rows affected: %w", err)
	}
	if rows == 0 {
		return protocol.ErrJobNotOwned
	}
	d.notifyTerminal(jobID)
	return nil
}

// UpdateJobRunningStatusWithOwner applies a non-terminal (running) report only
// while workerID still owns an active job. The non-terminal path used to
// ignore ownership entirely: a worker that lost the job to failover could flip
// a requeued (pending, unassigned) job back to running, leaving it owned by
// nobody — invisible to scheduling, migration and the starvation sweep. A
// report that loses the guard returns protocol.ErrJobNotOwned.
func (d *Database) UpdateJobRunningStatusWithOwner(jobID, workerID string, exitCode *int, errMsg *string, failureType, failureDetails *string) error {
	now := time.Now()
	result, err := d.db.Exec(`
		UPDATE jobs SET status = ?, updated_at = ?, exit_code = ?, error = ?,
		                failure_type = COALESCE(?, failure_type),
		                retryable = CASE WHEN COALESCE(?, failure_type) IN ('TIMEOUT', 'WORKER_CRASH') THEN 1 ELSE 0 END,
		                failure_details = COALESCE(?, failure_details),
		                started_at = COALESCE(started_at, ?)
		WHERE id = ? AND worker_id = ? AND status IN (?, ?)
	`, protocol.JobStatusRunning, now, exitCode, errMsg, failureType, failureType, failureDetails,
		now, jobID, workerID, protocol.JobStatusRunning, protocol.JobStatusQueued)
	if err != nil {
		return fmt.Errorf("failed to update job running status: %w", err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to check rows affected: %w", err)
	}
	if rows == 0 {
		return protocol.ErrJobNotOwned
	}
	return nil
}

// UpdateJobProgressWithOwner stores the progress and ETA seconds of a job the
// reporting worker still owns. A job's progress is monotonic: a report that
// would move the percent backward is rejected without writing, so a
// re-dispatched job — its new worker re-encodes from zero and reports low
// percentages again — cannot rewind the value the API and the CLI already
// published. The comparison runs inside the UPDATE, so concurrent reporters (a
// late request racing the new owner's) serialize on the row. accepted is false
// for a regression; a report that loses the ownership guard returns
// protocol.ErrJobNotOwned instead, so the caller can answer 409.
func (d *Database) UpdateJobProgressWithOwner(id, workerID string, progress float64, etaSeconds int) (bool, error) {
	now := time.Now()

	res, err := d.db.Exec(`
		UPDATE jobs SET progress_percent = ?, eta_seconds = ?, updated_at = ?
		WHERE id = ? AND worker_id = ? AND status IN (?, ?) AND progress_percent <= ?
	`, progress, etaSeconds, now, id, workerID,
		protocol.JobStatusRunning, protocol.JobStatusQueued, progress)
	if err != nil {
		return false, fmt.Errorf("failed to update job progress: %w", err)
	}
	written, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("failed to update job progress: %w", err)
	}
	if written > 0 {
		return true, nil
	}

	// Zero rows: tell a regression (acceptable, not owned by this guard) from
	// a reporter that no longer owns the job.
	var owner sql.NullString
	var status protocol.JobStatus
	err = d.db.QueryRow(`SELECT worker_id, status FROM jobs WHERE id = ?`, id).Scan(&owner, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return false, protocol.ErrJobNotFound
	}
	if err != nil {
		return false, fmt.Errorf("failed to classify rejected progress report: %w", err)
	}
	if !owner.Valid || NormalizeWorkerID(owner.String) != NormalizeWorkerID(workerID) ||
		(status != protocol.JobStatusRunning && status != protocol.JobStatusQueued) {
		return false, protocol.ErrJobNotOwned
	}
	return false, nil
}

// AssignJobToWorker atomically assigns a job to a worker: one statement sets
// worker_id AND flips the status to queued. The previous two-step version
// (worker_id write, then queued status) left a pending+worker_id orphan when
// it failed in between — such a job was invisible to both the scheduler
// (status=pending scan) and FailStarvedPendingJobs (WHERE worker_id IS NULL),
// hanging CLI clients forever. Returns ErrJobNotOwned if the job is no longer
// schedulable (already assigned, or not pending).
func (d *Database) AssignJobToWorker(jobID, workerID string) error {
	now := time.Now()
	result, err := d.db.Exec(`
		UPDATE jobs SET worker_id = ?, assigned_worker = ?, worker_name = (SELECT name FROM workers WHERE id = ?), status = ?, updated_at = ?, claimed_at = ?
		WHERE id = ? AND status = ? AND worker_id IS NULL
	`, workerID, workerID, workerID, protocol.JobStatusQueued, now, now, jobID, protocol.JobStatusPending)
	if err != nil {
		return fmt.Errorf("failed to assign job to worker: %w", err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to check rows affected: %w", err)
	}
	if rows == 0 {
		return protocol.ErrJobNotOwned
	}
	return nil
}

// CancelJob cancels a job if it's in a cancellable state
// Jobs can be cancelled if they are pending, queued, or running.
//
// the state check and the update are a single conditional UPDATE,
// so a worker completing the job between the check and the write can no longer
// have its terminal status overwritten by a stale cancel (TOCTOU fix).
func (d *Database) CancelJob(id string) error {
	now := time.Now()
	result, err := d.db.Exec(`
		UPDATE jobs SET status = ?, updated_at = ?, finished_at = ?
		WHERE id = ? AND status IN (?, ?, ?)
	`, protocol.JobStatusCancelled, now, now, id,
		protocol.JobStatusPending, protocol.JobStatusQueued, protocol.JobStatusRunning)
	if err != nil {
		return fmt.Errorf("failed to cancel job: %w", err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to check rows affected: %w", err)
	}
	if rows == 0 {
		job, getErr := d.GetJob(id)
		if getErr != nil {
			return protocol.ErrJobNotFound
		}
		return fmt.Errorf("cannot cancel job in status %s", job.Status)
	}

	d.notifyTerminal(id)
	return nil
}

// UpdateJobOutputWithOwner attaches uploaded output files only while workerID
// still owns an active job. A stale worker whose job was migrated must not
// overwrite the new owner's product.
func (d *Database) UpdateJobOutputWithOwner(id, workerID, outputFiles string) error {
	now := time.Now()
	res, err := d.db.Exec(`
		UPDATE jobs SET output_files = ?, updated_at = ?
		WHERE id = ? AND worker_id = ? AND status IN (?, ?)
	`, outputFiles, now, id, workerID, protocol.JobStatusRunning, protocol.JobStatusQueued)
	if err != nil {
		return fmt.Errorf("failed to update job output: %w", err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to check rows affected: %w", err)
	}
	if rows > 0 {
		return nil
	}
	if _, getErr := d.GetJob(id); getErr != nil {
		return protocol.ErrJobNotFound
	}
	return protocol.ErrJobNotOwned
}

// GetPendingJobs retrieves all pending jobs
func (d *Database) GetPendingJobs(limit int) ([]*Job, error) {
	rows, err := d.db.Query(`
		SELECT id, status, input_files, args, output_filename, streaming_output, output_files, worker_id, exit_code, error, failure_type, failure_details, auto_hw, cached, timeout, direct_paths, progress_percent, eta_seconds,
		       created_at, updated_at, claimed_at, started_at, finished_at, assigned_worker, worker_name
		FROM jobs WHERE status = ?
		ORDER BY created_at ASC LIMIT ?
	`, protocol.JobStatusPending, limit)

	if err != nil {
		return nil, fmt.Errorf("failed to get pending jobs: %w", err)
	}
	defer rows.Close()

	return d.scanJobs(rows)
}

// scanJobs scans multiple job rows
func (d *Database) scanJobs(rows *sql.Rows) ([]*Job, error) {
	var jobs []*Job
	for rows.Next() {
		job := &Job{}
		err := rows.Scan(
			&job.ID, &job.Status, &job.InputFiles, &job.Args, &job.OutputFilename, &job.StreamingOutput, &job.OutputFiles,
			&job.WorkerID, &job.ExitCode, &job.Error, &job.FailureType, &job.FailureDetails, &job.AutoHW, &job.Cached,
			&job.Timeout, &job.DirectPaths, &job.ProgressPercent, &job.EtaSeconds,
			&job.CreatedAt, &job.UpdatedAt, &job.ClaimedAt, &job.StartedAt, &job.FinishedAt, &job.AssignedWorker, &job.WorkerName,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan job: %w", err)
		}
		jobs = append(jobs, job)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating jobs: %w", err)
	}

	return jobs, nil
}

// GetJobsForWorker retrieves jobs assigned to a specific worker with pending or queued status
func (d *Database) GetJobsForWorker(workerID string, limit int) ([]*Job, error) {
	workerID = NormalizeWorkerID(workerID)
	rows, err := d.db.Query(`
		SELECT id, status, input_files, args, output_filename, streaming_output, output_files, worker_id, exit_code, error, failure_type, failure_details, auto_hw, cached, timeout, direct_paths, progress_percent, eta_seconds,
		       created_at, updated_at, claimed_at, started_at, finished_at, assigned_worker, worker_name
		FROM jobs WHERE worker_id = ? AND status IN (?, ?)
		ORDER BY created_at ASC LIMIT ?
	`, workerID, protocol.JobStatusPending, protocol.JobStatusQueued, limit)

	if err != nil {
		return nil, fmt.Errorf("failed to get jobs for worker: %w", err)
	}
	defer rows.Close()

	return d.scanJobs(rows)
}

// AssignPendingJobsToWorker returns every job already queued for the worker
// plus as many newly claimed pending jobs as the worker's capacity allows.
// Queued jobs are delivered in full: they are already committed to this worker
// by the scheduler, so capping them would strand the overflow. The active-job
// count and the claims share one write transaction, so two concurrent pulls
// cannot both read a stale count and over-assign a worker past maxConcurrent.
func (d *Database) AssignPendingJobsToWorker(workerID string, maxConcurrent int) ([]*Job, error) {

	workerID = NormalizeWorkerID(workerID)

	workerName := ""
	if w, err := d.GetWorker(workerID); err == nil {
		workerName = w.Name
	}

	tx, err := d.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	queuedRows, err := tx.Query(`
		SELECT id, status, input_files, args, output_filename, streaming_output, output_files, worker_id, exit_code, error, failure_type, failure_details, auto_hw, cached, timeout, direct_paths, progress_percent, eta_seconds,
		       created_at, updated_at, claimed_at, started_at, finished_at, assigned_worker, worker_name
		FROM jobs WHERE worker_id = ? AND status IN (?, ?)
		ORDER BY created_at ASC
	`, workerID, protocol.JobStatusPending, protocol.JobStatusQueued)
	if err != nil {
		return nil, fmt.Errorf("failed to get queued jobs for worker: %w", err)
	}
	queuedJobs, err := d.scanJobs(queuedRows)
	if err != nil {
		return nil, err
	}

	// Recheck capacity atomically with the claims below: the COUNT runs in
	// this same write transaction (SQLite serializes writers), so a concurrent
	// pull observes the jobs this one has already claimed instead of a stale
	// zero and cannot push the worker past maxConcurrent.
	var activeCount int
	if err := tx.QueryRow(`
		SELECT COUNT(*) FROM jobs WHERE worker_id = ? AND status IN (?, ?)
	`, workerID, protocol.JobStatusRunning, protocol.JobStatusQueued).Scan(&activeCount); err != nil {
		return nil, fmt.Errorf("failed to get active job count: %w", err)
	}

	maxNewJobs := maxConcurrent - activeCount
	if maxNewJobs < 0 {
		maxNewJobs = 0
	}
	if maxNewJobs == 0 {
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("failed to commit transaction: %w", err)
		}
		return queuedJobs, nil
	}

	rows, err := tx.Query(`
		SELECT id, status, input_files, args, output_filename, streaming_output, output_files, worker_id, exit_code, error, failure_type, failure_details, auto_hw, cached, timeout, direct_paths, progress_percent, eta_seconds,
		       created_at, updated_at, claimed_at, started_at, finished_at, assigned_worker, worker_name
		FROM jobs WHERE status = ? AND worker_id IS NULL
		ORDER BY created_at ASC LIMIT ?
	`, protocol.JobStatusPending, maxNewJobs)

	if err != nil {
		return nil, fmt.Errorf("failed to get pending jobs: %w", err)
	}

	newJobs, err := d.scanJobs(rows)
	if err != nil {
		return nil, err
	}

	now := time.Now()
	claimedJobs := make([]*Job, 0, len(newJobs))
	for _, job := range newJobs {

		result, err := tx.Exec(`
			UPDATE jobs SET worker_id = ?, assigned_worker = ?, worker_name = (SELECT name FROM workers WHERE id = ?), status = ?, updated_at = ?, claimed_at = ?
			WHERE id = ? AND status = ? AND worker_id IS NULL
		`, workerID, workerID, workerID, protocol.JobStatusQueued, now, now, job.ID, protocol.JobStatusPending)
		if err != nil {
			return nil, fmt.Errorf("failed to assign job %s: %w", job.ID, err)
		}
		claimed, err := result.RowsAffected()
		if err != nil {
			return nil, fmt.Errorf("failed to check rows affected for job %s: %w", job.ID, err)
		}
		if claimed == 0 {
			continue
		}
		job.WorkerID = sql.NullString{String: workerID, Valid: true}
		job.AssignedWorker = sql.NullString{String: workerID, Valid: true}
		job.WorkerName = sql.NullString{String: workerName, Valid: workerName != ""}
		job.Status = protocol.JobStatusQueued
		job.ClaimedAt = sql.NullTime{Time: now, Valid: true}
		claimedJobs = append(claimedJobs, job)
	}

	if len(claimedJobs) > 0 {
		if _, err := tx.Exec(`
			UPDATE workers SET status = ? WHERE id = ? AND status = ?
		`, protocol.WorkerStatusBusy, workerID, protocol.WorkerStatusIdle); err != nil {
			return nil, fmt.Errorf("failed to mark worker busy: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("failed to commit transaction: %w", err)
	}

	for _, job := range claimedJobs {
		if err := d.RecordMigrationTarget(job.ID, workerID, workerName); err != nil {
			log.Printf("Failed to record migration target for job %s: %v", job.ID, err)
		}
	}

	return append(queuedJobs, claimedJobs...), nil
}

// GetTimedOutJobs retrieves jobs that have been running longer than the timeout
func (d *Database) GetTimedOutJobs(timeout time.Duration) ([]*Job, error) {
	cutoff := time.Now().Add(-timeout)
	rows, err := d.db.Query(`
		SELECT id, status, input_files, args, output_filename, streaming_output, output_files, worker_id, exit_code, error, failure_type, failure_details, auto_hw, cached, timeout, direct_paths, progress_percent, eta_seconds,
		       created_at, updated_at, claimed_at, started_at, finished_at, assigned_worker, worker_name
		FROM jobs WHERE status = ? AND started_at IS NOT NULL AND started_at < ?
	`, protocol.JobStatusRunning, cutoff)
	if err != nil {
		return nil, fmt.Errorf("failed to get timed out jobs: %w", err)
	}
	defer rows.Close()

	return d.scanJobs(rows)
}

// RescheduleJob resets a job for rescheduling after timeout.
//
// conditional on the job still being running. A timeout sweep that
// races a worker's completion report must not drag a finished job back to
// pending; the guarded UPDATE simply matches 0 rows and the caller logs it.
func (d *Database) RescheduleJob(id string) error {
	now := time.Now()

	result, err := d.db.Exec(`
		UPDATE jobs SET status = ?, worker_id = NULL, started_at = NULL, claimed_at = NULL, updated_at = ?,
		                created_at = ?
		WHERE id = ? AND status = ?
	`, protocol.JobStatusPending, now, now, id, protocol.JobStatusRunning)
	if err != nil {
		return fmt.Errorf("failed to reschedule job: %w", err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to check rows affected: %w", err)
	}
	if rows == 0 {
		job, getErr := d.GetJob(id)
		if getErr != nil {
			return protocol.ErrJobNotFound
		}
		return fmt.Errorf("cannot reschedule job in status %s: %w", job.Status, protocol.ErrJobTerminal)
	}

	return nil
}

// FailJob marks a job as failed with the given error message.
// The worker assignment is cleared and timestamps are updated.
// failureType is the machine-readable classification (e.g.
// protocol.FailureWorkerCrash when repeated worker crashes exhausted
// retries); pass "" to keep any existing classification.
func (d *Database) FailJob(id, errMsg string, failureType string) error {
	now := time.Now()
	exitCode := -1

	result, err := d.db.Exec(`
		UPDATE jobs SET status = ?, worker_id = NULL, started_at = NULL,
		                exit_code = ?, error = ?,
		                failure_type = COALESCE(NULLIF(?, ''), failure_type),
		                retryable = CASE WHEN COALESCE(NULLIF(?, ''), failure_type) IN ('TIMEOUT', 'WORKER_CRASH') THEN 1 ELSE 0 END,
		                finished_at = ?, updated_at = ?
		WHERE id = ? AND status IN (?, ?, ?)
	`, protocol.JobStatusFailed, exitCode, errMsg, failureType, failureType, now, now, id,
		protocol.JobStatusPending, protocol.JobStatusQueued, protocol.JobStatusRunning)
	if err != nil {
		return fmt.Errorf("failed to fail job: %w", err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to check rows affected: %w", err)
	}
	if rows == 0 {
		job, getErr := d.GetJob(id)
		if getErr != nil {
			return protocol.ErrJobNotFound
		}
		return fmt.Errorf("cannot fail job in status %s: %w", job.Status, protocol.ErrJobTerminal)
	}

	d.notifyTerminal(id)
	return nil
}

// StarvedJob identifies a pending job eligible for the starvation sweep,
// together with its terminal classification source: Crashed is true when the
// job's most recent migration event was a worker crash (reason == crashReason,
// i.e. heartbeat_timeout), which changes the terminal failure from
// NO_WORKER_AVAILABLE to WORKER_CRASH.
type StarvedJob struct {
	ID      string
	Crashed bool
}

// pendingUnassigned is the shared WHERE predicate of the starvation anchor
// lookup and the starved-job sweep: a job counts toward starvation only while
// it is pending and unclaimed. One predicate keeps the two queries from
// drifting apart.
const pendingUnassigned = "status = ? AND worker_id IS NULL"

// EarliestPendingCreatedAt returns the created_at of the oldest pending,
// unassigned job — the anchor the starvation sweep's pending clock is measured
// from, since every requeue path (failover, timeout requeue, restart recovery)
// refreshes created_at on the way back to pending.
//
// ok is false when no such job exists, i.e. no starvation verdict can be due.
// The scheduler arms a one-shot wake for anchor + NoWorkerJobTimeout so a due
// verdict is emitted just past the deadline instead of at the next check
// interval, which would place it anywhere in the (120s, 150s] acceptance
// window the tick quantizes (AC-17).
//
// ORDER BY created_at ASC must pick the job whose clock expires first: the
// sweep fails every pending job past the cutoff, so arming for anything newer
// would postpone the older jobs' verdict past the acceptance window.
func (d *Database) EarliestPendingCreatedAt() (time.Time, bool, error) {
	var anchor time.Time
	err := d.db.QueryRow(`
		SELECT created_at FROM jobs
		WHERE `+pendingUnassigned+`
		ORDER BY created_at ASC
		LIMIT 1
	`, protocol.JobStatusPending).Scan(&anchor)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("failed to read the earliest pending job: %w", err)
	}
	return anchor, true, nil
}

// GetStarvedPendingJobs lists the pending jobs created before the cutoff (the
// set the starvation sweep fails) and classifies each by its most recent
// migration event. The classification is computed once here and reused by
// FailStarvedPendingJobs and the scheduler's per-job broadcast, instead of
// rescanning migration_events three times per tick.
//
// A job whose most recent migration was a worker-crash heartbeat timeout keeps
// the WORKER_CRASH semantics; a job last requeued for a job_timeout (or never
// migrated) is genuine starvation and is classified NO_WORKER_AVAILABLE.
func (d *Database) GetStarvedPendingJobs(cutoff time.Time, crashReason string) ([]StarvedJob, error) {
	rows, err := d.db.Query(`
		SELECT j.id,
		       COALESCE((
		         SELECT me.reason
		         FROM migration_events me, json_each(me.job_ids)
		         WHERE json_each.value = j.id
		         ORDER BY me.timestamp DESC, me.rowid DESC
		         LIMIT 1
		       ), '') = ?
		FROM jobs j
		WHERE `+pendingUnassigned+` AND j.created_at < ?
	`, crashReason, protocol.JobStatusPending, cutoff)
	if err != nil {
		return nil, fmt.Errorf("failed to list starved pending jobs: %w", err)
	}
	defer rows.Close()

	starved := make([]StarvedJob, 0, 16)
	for rows.Next() {
		var sj StarvedJob
		if err := rows.Scan(&sj.ID, &sj.Crashed); err != nil {
			return nil, fmt.Errorf("failed to scan starved pending job: %w", err)
		}
		starved = append(starved, sj)
	}
	return starved, rows.Err()
}

// FailStarvedPendingJobs fails the pre-classified starved jobs in a single
// transaction and returns exactly the IDs it failed. Used by the scheduler
// starvation guard so a backlog larger than any fetch limit converges within
// one tick.
//
// The classification comes from GetStarvedPendingJobs and is reused here, not
// recomputed: Crashed jobs keep WORKER_CRASH, the rest are NO_WORKER_AVAILABLE.
// The UPDATE re-checks the pending/unassigned/cutoff predicate, so a job that a
// worker claimed after the scan is left alone and absent from the returned set —
// callers must notify/release only for that set, never for the pre-read
// classification. Both UPDATEs run in one transaction so a partial failure
// rolls back instead of leaving already-failed jobs without their quota release
// and terminal broadcast.
func (d *Database) FailStarvedPendingJobs(cutoff time.Time, starved []StarvedJob, noWorkerErrMsg, crashErrMsg string) ([]string, error) {
	var crashIDs, noWorkerIDs []string
	for _, sj := range starved {
		if sj.Crashed {
			crashIDs = append(crashIDs, sj.ID)
		} else {
			noWorkerIDs = append(noWorkerIDs, sj.ID)
		}
	}

	tx, err := d.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("failed to begin starvation sweep: %w", err)
	}
	defer tx.Rollback()

	now := time.Now()
	exitCode := -1

	failed, err := failStarvedJobsChunked(tx, now, exitCode, crashIDs, crashErrMsg, string(protocol.FailureWorkerCrash), cutoff)
	if err != nil {
		return nil, err
	}
	noWorkerFailed, err := failStarvedJobsChunked(tx, now, exitCode, noWorkerIDs, noWorkerErrMsg, string(protocol.FailureNoWorkerAvailable), cutoff)
	if err != nil {
		return nil, err
	}
	failed = append(failed, noWorkerFailed...)

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("failed to commit starvation sweep: %w", err)
	}
	return failed, nil
}

// starvedUpdateChunkSize bounds the number of ids in one UPDATE ... IN (...)
// clause, keeping each statement comfortably under SQLite's bind-variable
// limit for arbitrarily large backlogs.
const starvedUpdateChunkSize = 500

// failStarvedJobsChunked applies the starvation failure to the given job IDs in
// chunks, re-checking each is still a pending, unassigned job past the cutoff,
// and returns the IDs each statement actually changed. The guard matters
// because GetStarvedPendingJobs and this UPDATE are separate statements: a job
// re-migrated (or claimed) in between has a fresh created_at (or a worker_id)
// and must be left for the next tick — and must not be reported as failed, or
// its quota and WS sequence would be released while it is still running.
func failStarvedJobsChunked(tx *sql.Tx, now time.Time, exitCode int, ids []string, errMsg, failureType string, cutoff time.Time) ([]string, error) {
	var failed []string
	retryable := protocol.FailureType(failureType).Retryable()
	for start := 0; start < len(ids); start += starvedUpdateChunkSize {
		end := start + starvedUpdateChunkSize
		if end > len(ids) {
			end = len(ids)
		}
		chunk := ids[start:end]
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(chunk)), ",")
		args := make([]any, 0, len(chunk)+7)
		args = append(args, protocol.JobStatusFailed, exitCode, errMsg, failureType, retryable, now, now)
		for _, id := range chunk {
			args = append(args, id)
		}
		args = append(args, protocol.JobStatusPending, cutoff)

		rows, err := tx.Query(fmt.Sprintf(`
			UPDATE jobs SET status = ?, worker_id = NULL,
			                exit_code = ?, error = ?,
			                failure_type = ?, retryable = ?, finished_at = ?, updated_at = ?
			WHERE id IN (%s) AND status = ? AND worker_id IS NULL AND created_at < ?
			RETURNING id
		`, placeholders), args...)
		if err != nil {
			return failed, fmt.Errorf("failed to fail starved pending jobs: %w", err)
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return failed, fmt.Errorf("failed to scan starved job id: %w", err)
			}
			failed = append(failed, id)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return failed, fmt.Errorf("failed to iterate starved jobs: %w", err)
		}
		rows.Close()
	}
	return failed, nil
}

// resetJobToPendingExec re-queues one job only while workerID still owns it and
// the job is still active. The ownership condition is mandatory: a failover
// sweep racing a job already re-claimed by another worker must not drag that
// job back to pending (which would double-execute it). created_at is refreshed
// because a requeue re-anchors the starvation sweep and NoWorkerDeadline
// clocks they share. Returns false when the job left the owned active set.
func resetJobToPendingExec(e jobExec, id, workerID string, now time.Time) (bool, error) {
	result, err := e.Exec(`
		UPDATE jobs SET status = ?, worker_id = NULL, started_at = NULL, claimed_at = NULL, updated_at = ?,
		                created_at = ?,
		                failure_type = '', retryable = 0, failure_details = '', exit_code = NULL, error = NULL
		WHERE id = ? AND worker_id = ? AND status IN (?, ?)
	`, protocol.JobStatusPending, now, now, id, workerID,
		protocol.JobStatusQueued, protocol.JobStatusRunning)
	if err != nil {
		return false, fmt.Errorf("failed to reset job to pending: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("failed to check rows affected: %w", err)
	}
	return rows > 0, nil
}

// RecoverState performs recovery operations after server restart: it resets
// jobs that were in running/queued state back to pending and marks all workers
// as offline. Stale-worker cleanup is a separate pass (RemoveStaleWorkers)
// invoked from cmd/server/main.go after recovery.
func (d *Database) RecoverState() (jobsReset int64, workersMarkedOffline int64, err error) {
	tx, err := d.db.Begin()
	if err != nil {
		return 0, 0, fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	now := time.Now()
	result, err := tx.Exec(`
		UPDATE jobs SET status = ?, updated_at = ?, created_at = ?, worker_id = NULL, started_at = NULL, claimed_at = NULL
		WHERE status = ? OR status = ?
	`, protocol.JobStatusPending, now, now, protocol.JobStatusRunning, protocol.JobStatusQueued)
	if err != nil {
		return 0, 0, fmt.Errorf("failed to reset running jobs: %w", err)
	}
	jobsReset, err = result.RowsAffected()
	if err != nil {
		return 0, 0, fmt.Errorf("failed to get rows affected: %w", err)
	}

	result, err = tx.Exec(`
		UPDATE workers SET status = ?
	`, protocol.WorkerStatusOffline)
	if err != nil {
		return 0, 0, fmt.Errorf("failed to mark workers offline: %w", err)
	}
	workersMarkedOffline, err = result.RowsAffected()
	if err != nil {
		return 0, 0, fmt.Errorf("failed to get rows affected: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return 0, 0, fmt.Errorf("failed to commit transaction: %w", err)
	}

	return jobsReset, workersMarkedOffline, nil
}

// GetJobsByStatus retrieves all jobs with a specific status
func (d *Database) GetJobsByStatus(status protocol.JobStatus, limit int) ([]*Job, error) {
	rows, err := d.db.Query(`
		SELECT id, status, input_files, args, output_filename, streaming_output, output_files, worker_id, exit_code, error, failure_type, failure_details, auto_hw, cached, timeout, direct_paths, progress_percent, eta_seconds,
		       created_at, updated_at, claimed_at, started_at, finished_at, assigned_worker, worker_name
		FROM jobs WHERE status = ?
		ORDER BY created_at ASC LIMIT ?
	`, status, limit)

	if err != nil {
		return nil, fmt.Errorf("failed to get jobs by status: %w", err)
	}
	defer rows.Close()

	return d.scanJobs(rows)
}

// ListJobs retrieves all jobs with pagination, newest first. Used by the
// GET /api/v1/jobs list endpoint.
func (d *Database) ListJobs(limit int, offset int) ([]*Job, error) {
	rows, err := d.db.Query(`
		SELECT id, status, input_files, args, output_filename, streaming_output, output_files, worker_id, exit_code, error, failure_type, failure_details, auto_hw, cached, timeout, direct_paths, progress_percent, eta_seconds,
		       created_at, updated_at, claimed_at, started_at, finished_at, assigned_worker, worker_name
		FROM jobs
		ORDER BY created_at DESC, id DESC
		LIMIT ? OFFSET ?
	`, limit, offset)

	if err != nil {
		return nil, fmt.Errorf("failed to list jobs: %w", err)
	}
	defer rows.Close()

	return d.scanJobs(rows)
}

// StuckJob is an active job the orphan sweep judged unable to make progress:
// either stuck in queued past the stale threshold, or still attached to a
// worker record that is gone. Crashed is true for the worker-loss cases, which
// classify the terminal outcome as WORKER_CRASH instead of INFRA.
type StuckJob struct {
	ID      string
	Crashed bool
}

// GetStuckActiveJobs lists active jobs that have not been touched for
// staleAfter and can no longer make progress:
//
//   - any job stuck in queued that long (its worker never picked it up), and
//   - any job whose owning worker record is gone or unset.
//
// A running job on a live worker belongs to checkTimeouts; a running job on a
// merely offline worker belongs to the failover migration.
func (d *Database) GetStuckActiveJobs(staleAfter time.Duration) ([]StuckJob, error) {
	cutoff := time.Now().Add(-staleAfter)
	rows, err := d.db.Query(`
		SELECT j.id,
		       CASE WHEN `+missingWorkerPredicate+` THEN 1
		            WHEN `+offlineWorkerPredicate+` THEN 1
		            ELSE 0 END
		FROM jobs j
		WHERE j.status IN (?, ?)
		  AND j.updated_at < ?
		  AND (j.status = ? OR `+missingWorkerPredicate+`)
		ORDER BY j.updated_at ASC
	`,
		protocol.WorkerStatusOffline,
		protocol.JobStatusQueued, protocol.JobStatusRunning,
		cutoff, protocol.JobStatusQueued)
	if err != nil {
		return nil, fmt.Errorf("failed to list stuck active jobs: %w", err)
	}
	defer rows.Close()

	stuck := make([]StuckJob, 0, 16)
	for rows.Next() {
		var sj StuckJob
		if err := rows.Scan(&sj.ID, &sj.Crashed); err != nil {
			return nil, fmt.Errorf("failed to scan stuck active job: %w", err)
		}
		stuck = append(stuck, sj)
	}
	return stuck, rows.Err()
}

// FailStuckActiveJobs fails the pre-classified stuck jobs in one transaction
// and returns exactly the IDs it failed. Each update re-checks the same
// stale/worker-availability predicate as the scan, so a job that recovered in
// between (its worker came back, another path claimed or requeued it) is left
// alone and absent from the result — callers must notify/release only for the
// returned IDs, never for the pre-read classification.
func (d *Database) FailStuckActiveJobs(staleAfter time.Duration, stuck []StuckJob, infraErrMsg, crashErrMsg string) ([]string, error) {
	var crashIDs, infraIDs []string
	for _, sj := range stuck {
		if sj.Crashed {
			crashIDs = append(crashIDs, sj.ID)
		} else {
			infraIDs = append(infraIDs, sj.ID)
		}
	}

	tx, err := d.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("failed to begin stuck-job sweep: %w", err)
	}
	defer tx.Rollback()

	now := time.Now()
	cutoff := now.Add(-staleAfter)

	failed, err := failStuckJobsChunked(tx, now, cutoff, crashIDs, crashErrMsg,
		string(protocol.FailureWorkerCrash), true)
	if err != nil {
		return nil, err
	}
	infraFailed, err := failStuckJobsChunked(tx, now, cutoff, infraIDs, infraErrMsg,
		string(protocol.FailureInfra), false)
	if err != nil {
		return nil, err
	}
	failed = append(failed, infraFailed...)

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("failed to commit stuck-job sweep: %w", err)
	}
	return failed, nil
}

// failStuckJobsChunked applies the terminal failure to one classification of
// stuck jobs in id chunks, returning the IDs each statement actually changed.
// crashed selects the re-check guard: the worker-loss cases (missing record, or
// offline record with a queued job) vs. a job still queued on a live worker.
func failStuckJobsChunked(tx *sql.Tx, now, cutoff time.Time, ids []string, errMsg, failureType string, crashed bool) ([]string, error) {
	var failed []string
	retryable := protocol.FailureType(failureType).Retryable()
	for start := 0; start < len(ids); start += starvedUpdateChunkSize {
		end := start + starvedUpdateChunkSize
		if end > len(ids) {
			end = len(ids)
		}
		chunk := ids[start:end]
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(chunk)), ",")

		args := make([]any, 0, len(chunk)+10)
		args = append(args, protocol.JobStatusFailed, -1, errMsg, failureType, retryable, now, now)
		for _, id := range chunk {
			args = append(args, id)
		}
		args = append(args, protocol.JobStatusQueued, protocol.JobStatusRunning, cutoff)

		guard := `AND j.status = ? AND ` + liveWorkerPredicate
		if crashed {
			guard = `AND (` + missingWorkerPredicate + ` OR (j.status = ? AND ` + offlineWorkerPredicate + `))`
			args = append(args, protocol.JobStatusQueued, protocol.WorkerStatusOffline)
		} else {
			args = append(args, protocol.JobStatusQueued, protocol.WorkerStatusOffline)
		}

		rows, err := tx.Query(fmt.Sprintf(`
			UPDATE jobs AS j SET status = ?, worker_id = NULL, started_at = NULL, claimed_at = NULL,
			                exit_code = ?, error = ?,
			                failure_type = ?, retryable = ?, finished_at = ?, updated_at = ?
			WHERE j.id IN (%s) AND j.status IN (?, ?) AND j.updated_at < ? `+guard+`
			RETURNING id
		`, placeholders), args...)
		if err != nil {
			return failed, fmt.Errorf("failed to fail stuck jobs: %w", err)
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return failed, fmt.Errorf("failed to scan stuck job id: %w", err)
			}
			failed = append(failed, id)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return failed, fmt.Errorf("failed to iterate stuck jobs: %w", err)
		}
		rows.Close()
	}
	return failed, nil
}

// GetJobRetryCounts returns the worker-failure migration count for each job in
// a single batched query, keyed by job ID. The count is the number of
// migration events whose job_ids contain the job, excluding job_timeout
// requeues (a separate budget); batching avoids an N+1 query storm when a
// worker pull returns up to MaxJobsPerWorker jobs in one response. Jobs with
// no migration history are absent from the map.
func (d *Database) GetJobRetryCounts(jobIDs []string) (map[string]int, error) {
	counts := make(map[string]int, len(jobIDs))
	if len(jobIDs) == 0 {
		return counts, nil
	}

	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(jobIDs)), ",")
	args := make([]any, len(jobIDs))
	for i, id := range jobIDs {
		args[i] = id
	}

	rows, err := d.db.Query(`
		SELECT json_each.value AS job_id, COUNT(*)
		FROM migration_events me, json_each(me.job_ids)
		WHERE me.reason != 'job_timeout' AND json_each.value IN (`+placeholders+`)
		GROUP BY json_each.value
	`, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to get job retry counts: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var jobID string
		var count int
		if err := rows.Scan(&jobID, &count); err != nil {
			return nil, fmt.Errorf("failed to scan job retry count: %w", err)
		}
		counts[jobID] = count
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to iterate job retry counts: %w", err)
	}
	return counts, nil
}

// GetJobTimeoutRetryCount counts only timeout-driven requeues for a job. The
// scheduler's MaxTimeoutRetries budget must not be consumed by unrelated
// migrations: a job twice migrated for worker crashes would otherwise burn its
// entire timeout budget on the first hang.
func (d *Database) GetJobTimeoutRetryCount(jobID string) (int, error) {
	var count int
	err := d.db.QueryRow(`
		SELECT COUNT(*) FROM migration_events
		WHERE reason = 'job_timeout'
		  AND id IN (
			SELECT me.id FROM migration_events me, json_each(me.job_ids)
			WHERE json_each.value = ?
		  )
	`, jobID).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("failed to get job timeout retry count: %w", err)
	}
	return count, nil
}

// RecordJobTimeoutMigration atomically reschedules a timed-out job, records a
// job_timeout migration event, and inserts its per-job redistribution
// placeholder in one transaction — all three succeed together or none do. The
// reschedule is a conditional UPDATE (status = 'running'); if the job already
// left the running set, zero rows match and the transaction rolls back, leaving
// no ghost event or permanently-NULL placeholder. retryCount is the cumulative
// timeout-requeue count after this event (first requeue = 1), persisted
// verbatim in migration_events.retry_count. workerID must be captured by
// the caller beforehand, since the reschedule clears worker_id. Returns
// rescheduled=false, nil when the job was no longer running.
func (d *Database) RecordJobTimeoutMigration(workerID, jobID string, retryCount int) (bool, error) {
	tx, err := d.db.Begin()
	if err != nil {
		return false, fmt.Errorf("begin timeout migration: %w", err)
	}
	defer tx.Rollback()

	now := time.Now()

	res, err := tx.Exec(`
		UPDATE jobs SET status = ?, worker_id = NULL, started_at = NULL, claimed_at = NULL, updated_at = ?, created_at = ?
		WHERE id = ? AND status = ?
	`, protocol.JobStatusPending, now, now, jobID, protocol.JobStatusRunning)
	if err != nil {
		return false, fmt.Errorf("reschedule job %s: %w", jobID, err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("check reschedule rows for job %s: %w", jobID, err)
	}
	if rows == 0 {
		return false, nil
	}

	var workerName sql.NullString
	if err := tx.QueryRow(`SELECT name FROM workers WHERE id = ?`, workerID).Scan(&workerName); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("read worker %s for timeout migration: %w", workerID, err)
	}

	eventID := uuid.New().String()
	if err := insertMigrationEventTx(tx, eventID, now, workerID, workerName, "job_timeout", retryCount, []string{jobID}); err != nil {
		return false, fmt.Errorf("insert timeout migration event for job %s: %w", jobID, err)
	}
	if err := insertRedistributionsTx(tx, eventID, map[string]int{jobID: retryCount}, now); err != nil {
		return false, fmt.Errorf("insert job redistribution for job %s: %w", jobID, err)
	}

	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit timeout migration for job %s: %w", jobID, err)
	}
	return true, nil
}
