package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/tsic404/rffmpeg/pkg/protocol"
)

// MigrationEvent represents a migration event record in the database.
type MigrationEvent struct {
	ID         string
	Timestamp  time.Time
	WorkerID   string
	WorkerName sql.NullString
	Reason     string
	// RetryCount is the max cumulative migration count among the migrated jobs
	// after this event (first migration = 1). Per-job exact counts, when jobs
	// in one event differ, live in job_redistributions.retry_count. Legacy rows
	// stored the pre-migration count and are not backfilled.
	RetryCount   int
	JobIDs       string // JSON array stored as string
	JobsMigrated int
	CreatedAt    time.Time
}

// GetMigrationEvent retrieves a migration event by ID.
func (d *Database) GetMigrationEvent(id string) (*MigrationEvent, error) {
	event := &MigrationEvent{}
	err := d.db.QueryRow(`
		SELECT id, timestamp, worker_id, worker_name, reason, retry_count, job_ids, jobs_migrated, created_at
		FROM migration_events WHERE id = ?
	`, id).Scan(
		&event.ID, &event.Timestamp, &event.WorkerID, &event.WorkerName,
		&event.Reason, &event.RetryCount, &event.JobIDs, &event.JobsMigrated, &event.CreatedAt,
	)

	if err == sql.ErrNoRows {
		return nil, protocol.ErrMigrationEventNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get migration event: %w", err)
	}

	return event, nil
}

// GetMigrationEvents retrieves migration events with pagination.
func (d *Database) GetMigrationEvents(limit int, offset int) ([]*MigrationEvent, error) {
	rows, err := d.db.Query(`
		SELECT id, timestamp, worker_id, worker_name, reason, retry_count, job_ids, jobs_migrated, created_at
		FROM migration_events
		ORDER BY timestamp DESC, id DESC
		LIMIT ? OFFSET ?
	`, limit, offset)

	if err != nil {
		return nil, fmt.Errorf("failed to get migration events: %w", err)
	}
	defer rows.Close()

	var events []*MigrationEvent
	for rows.Next() {
		event := &MigrationEvent{}
		err := rows.Scan(
			&event.ID, &event.Timestamp, &event.WorkerID, &event.WorkerName,
			&event.Reason, &event.RetryCount, &event.JobIDs, &event.JobsMigrated, &event.CreatedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan migration event: %w", err)
		}
		events = append(events, event)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating migration events: %w", err)
	}

	return events, nil
}

// GetMigrationEventsByWorker retrieves migration events for a specific worker.
func (d *Database) GetMigrationEventsByWorker(workerID string, limit int) ([]*MigrationEvent, error) {
	rows, err := d.db.Query(`
		SELECT id, timestamp, worker_id, worker_name, reason, retry_count, job_ids, jobs_migrated, created_at
		FROM migration_events
		WHERE worker_id = ?
		ORDER BY timestamp DESC, id DESC
		LIMIT ?
	`, workerID, limit)

	if err != nil {
		return nil, fmt.Errorf("failed to get migration events by worker: %w", err)
	}
	defer rows.Close()

	var events []*MigrationEvent
	for rows.Next() {
		event := &MigrationEvent{}
		err := rows.Scan(
			&event.ID, &event.Timestamp, &event.WorkerID, &event.WorkerName,
			&event.Reason, &event.RetryCount, &event.JobIDs, &event.JobsMigrated, &event.CreatedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan migration event: %w", err)
		}
		events = append(events, event)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating migration events: %w", err)
	}

	return events, nil
}

// JobRedistribution represents a per-job redistribution record: which worker a
// migrated job was reassigned to. Unlike a migration event (source worker +
// full job_ids list), the target is a per-job fact because the jobs of a single
// migration can fan out to different workers.
type JobRedistribution struct {
	ID               string
	MigrationEventID string
	JobID            string
	RetryCount       int
	TargetWorkerID   sql.NullString
	TargetWorkerName sql.NullString
	CreatedAt        time.Time
}

// RecordMigrationTarget fills the unresolved redistribution placeholder for a
// migrated job with the worker it was reassigned to. The lookup is an indexed
// point query on job_id, so a fresh job (no placeholder) matches zero rows and
// the UPDATE is a cheap no-op rather than a full-table json_each scan.
// targetWorkerName may be empty (worker registered without a name).
func (d *Database) RecordMigrationTarget(jobID, targetWorkerID, targetWorkerName string) error {
	var targetWorkerNameVal interface{}
	if targetWorkerName != "" {
		targetWorkerNameVal = targetWorkerName
	}

	_, err := d.db.Exec(`
		UPDATE job_redistributions
		SET target_worker_id = ?, target_worker_name = ?
		WHERE id = (
			SELECT id FROM job_redistributions
			WHERE job_id = ? AND target_worker_id IS NULL
			ORDER BY created_at DESC, id DESC
			LIMIT 1
		)
	`, targetWorkerID, targetWorkerNameVal, jobID)
	if err != nil {
		return fmt.Errorf("failed to record migration target for job %s: %w", jobID, err)
	}
	return nil
}

// GetJobRedistributionsByEvent lists the per-job redistribution records for a
// migration event, used to reconstruct the target side of a migration chain.
func (d *Database) GetJobRedistributionsByEvent(migrationEventID string) ([]JobRedistribution, error) {
	return d.GetJobRedistributionsByEvents([]string{migrationEventID})
}

// GetJobRedistributionsByEvents returns the per-job redistribution records for
// the given migration events in a single query, so the migration events list
// endpoint can attach targets without an N+1 per-event read.
func (d *Database) GetJobRedistributionsByEvents(migrationEventIDs []string) ([]JobRedistribution, error) {
	if len(migrationEventIDs) == 0 {
		return nil, nil
	}

	placeholders := make([]string, len(migrationEventIDs))
	args := make([]interface{}, len(migrationEventIDs))
	for i, id := range migrationEventIDs {
		placeholders[i] = "?"
		args[i] = id
	}

	rows, err := d.db.Query(fmt.Sprintf(`
		SELECT id, migration_event_id, job_id, retry_count, target_worker_id, target_worker_name, created_at
		FROM job_redistributions
		WHERE migration_event_id IN (%s)
		ORDER BY created_at ASC, id ASC
	`, strings.Join(placeholders, ",")), args...)
	if err != nil {
		return nil, fmt.Errorf("failed to get job redistributions: %w", err)
	}
	defer rows.Close()

	var redistributions []JobRedistribution
	for rows.Next() {
		r := JobRedistribution{}
		if err := rows.Scan(&r.ID, &r.MigrationEventID, &r.JobID, &r.RetryCount, &r.TargetWorkerID, &r.TargetWorkerName, &r.CreatedAt); err != nil {
			return nil, fmt.Errorf("failed to scan job redistribution: %w", err)
		}
		redistributions = append(redistributions, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating job redistributions: %w", err)
	}
	return redistributions, nil
}

// insertMigrationEventTx writes one migration_events row inside tx. It is the
// single copy of the audit INSERT: the batch failover migration and the
// single-job timeout requeue both go through it, so the column list cannot
// drift between the two writers. workerName is NULL when not valid.
func insertMigrationEventTx(tx *sql.Tx, id string, at time.Time, workerID string, workerName sql.NullString, reason string, retryCount int, jobIDs []string) error {
	jobIDsJSON, err := json.Marshal(jobIDs)
	if err != nil {
		return fmt.Errorf("marshal job ids: %w", err)
	}
	var nameVal any
	if workerName.Valid {
		nameVal = workerName.String
	}
	if _, err := tx.Exec(`
		INSERT INTO migration_events (id, timestamp, worker_id, worker_name, reason, retry_count, job_ids, jobs_migrated, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, id, at, workerID, nameVal, reason, retryCount, string(jobIDsJSON), len(jobIDs), at); err != nil {
		return fmt.Errorf("insert migration event: %w", err)
	}
	return nil
}

// insertRedistributionsTx writes one unresolved (target-empty) placeholder row
// per migrated job, the single copy of that INSERT for both requeue writers.
func insertRedistributionsTx(tx *sql.Tx, eventID string, retryCounts map[string]int, at time.Time) error {
	for jobID, retryCount := range retryCounts {
		if _, err := tx.Exec(`
			INSERT INTO job_redistributions (id, migration_event_id, job_id, retry_count, target_worker_id, target_worker_name, created_at)
			VALUES (?, ?, ?, ?, NULL, NULL, ?)
		`, uuid.New().String(), eventID, jobID, retryCount, at); err != nil {
			return fmt.Errorf("insert job redistribution for %s: %w", jobID, err)
		}
	}
	return nil
}

// WorkerMigrationResult summarises the migration of one offline worker's
// jobs. Migrated lists the jobs actually reset to pending; Failed lists the
// jobs that exhausted the migration retry budget and were judged
// WORKER_CRASH. Both lists are post-transaction facts, not the pre-read
// snapshot.
type WorkerMigrationResult struct {
	EventID    string
	WorkerName string
	Migrated   []string
	Failed     []string
}

// MigrateJobsFromWorker migrates all in-progress jobs from an offline worker
// back to pending in ONE transaction, writing the migration audit trail in the
// same transaction.
//
// The caller (health monitor) has just flipped the worker offline. Doing the
// classification, the audit rows and the job resets in one transaction makes
// C-85 hold structurally: no reset can commit before its migration event and
// per-job redistribution placeholder, and an audit write failure rolls the
// resets back so the jobs are never released into scheduling without their
// audit row. The worker's offline flag is re-checked inside the transaction:
// a worker whose heartbeat revived it in between keeps its jobs (it is alive
// again, so migrating them would double-execute).
//
// reason is recorded on the migration event (heartbeat_timeout / worker_offline
// / server_restart). Jobs whose prior migration count reached maxRetryCount are
// failed as WORKER_CRASH instead of migrated.
//
// A caller that receives an error must call RestoreWorkerStatusAfterFailedMigration
// for that worker, or the offline flag keeps it out of the scan that would
// retry the migration and its jobs stay stranded.
func (d *Database) MigrateJobsFromWorker(workerID, reason string, maxRetryCount int) (*WorkerMigrationResult, error) {
	workerID = NormalizeWorkerID(workerID)
	now := time.Now()

	tx, err := d.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("failed to begin migration for worker %s: %w", workerID, err)
	}
	defer tx.Rollback()

	var workerName sql.NullString
	var status protocol.WorkerStatus
	err = tx.QueryRow(`SELECT name, status FROM workers WHERE id = ?`, workerID).Scan(&workerName, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return &WorkerMigrationResult{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to read worker %s for migration: %w", workerID, err)
	}
	if status != protocol.WorkerStatusOffline {

		return &WorkerMigrationResult{WorkerName: workerName.String}, nil
	}

	rows, err := tx.Query(`
		SELECT j.id, COALESCE((
			SELECT COUNT(*) FROM migration_events me, json_each(me.job_ids)
			WHERE json_each.value = j.id AND me.reason != ?
		), 0)
		FROM jobs j
		WHERE j.worker_id = ? AND j.status IN (?, ?)
		ORDER BY j.created_at ASC
	`, "job_timeout", workerID, protocol.JobStatusRunning, protocol.JobStatusQueued)
	if err != nil {
		return nil, fmt.Errorf("failed to read active jobs for worker %s: %w", workerID, err)
	}

	type jobRetry struct {
		id    string
		count int
	}
	var migrated, failed []jobRetry
	for rows.Next() {
		var j jobRetry
		if err := rows.Scan(&j.id, &j.count); err != nil {
			rows.Close()
			return nil, fmt.Errorf("failed to scan active job for worker %s: %w", workerID, err)
		}
		if j.count >= maxRetryCount {
			failed = append(failed, j)
		} else {
			migrated = append(migrated, j)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("failed to iterate active jobs for worker %s: %w", workerID, err)
	}
	rows.Close()

	result := &WorkerMigrationResult{WorkerName: workerName.String}

	retryCounts := make(map[string]int, len(migrated))
	maxPostRetry := 0
	for _, j := range migrated {
		reset, err := resetJobToPendingExec(tx, j.id, workerID, now)
		if err != nil {
			return nil, fmt.Errorf("failed to migrate job %s: %w", j.id, err)
		}
		if !reset {

			continue
		}
		result.Migrated = append(result.Migrated, j.id)
		postCount := j.count + 1
		retryCounts[j.id] = postCount
		if postCount > maxPostRetry {
			maxPostRetry = postCount
		}
	}

	if len(result.Migrated) > 0 {
		eventID := uuid.New().String()
		if err := insertMigrationEventTx(tx, eventID, now, workerID, workerName, reason, maxPostRetry, result.Migrated); err != nil {
			return nil, fmt.Errorf("failed to create migration event for worker %s: %w", workerID, err)
		}
		if err := insertRedistributionsTx(tx, eventID, retryCounts, now); err != nil {
			return nil, fmt.Errorf("failed to create job redistributions for worker %s: %w", workerID, err)
		}
		result.EventID = eventID
	}

	// Jobs past the migration budget are terminal WORKER_CRASH. They stay in
	// the same transaction so a partial migration cannot leave them running on
	// a worker that will never be selected again.
	const maxRetryErrMsg = "max retry count exceeded after repeated worker failures"
	for _, j := range failed {
		res, err := tx.Exec(`
			UPDATE jobs SET status = ?, worker_id = NULL, started_at = NULL, claimed_at = NULL,
			                exit_code = -1, error = ?,
			                failure_type = ?, retryable = 1,
			                finished_at = ?, updated_at = ?
			WHERE id = ? AND worker_id = ? AND status IN (?, ?)
		`, protocol.JobStatusFailed, maxRetryErrMsg, string(protocol.FailureWorkerCrash), now, now,
			j.id, workerID, protocol.JobStatusRunning, protocol.JobStatusQueued)
		if err != nil {
			return nil, fmt.Errorf("failed to fail job %s past retry budget: %w", j.id, err)
		}
		if n, err := res.RowsAffected(); err != nil {
			return nil, fmt.Errorf("failed to count failed job %s: %w", j.id, err)
		} else if n > 0 {
			result.Failed = append(result.Failed, j.id)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("failed to commit migration for worker %s: %w", workerID, err)
	}

	for _, jobID := range result.Failed {
		d.notifyTerminal(jobID)
	}

	return result, nil
}

// MarkOfflineWorkersWithMigration flips heartbeat-stale workers offline and
// returns exactly the IDs that this call flipped, in one atomic
// UPDATE ... RETURNING. The previous SELECT-then-UPDATE pair could return a
// worker whose heartbeat refreshed in between (so it was never actually marked
// offline), and the caller would then migrate a live worker's jobs.
func (d *Database) MarkOfflineWorkersWithMigration(heartbeatTimeout time.Duration) ([]string, error) {
	cutoff := time.Now().Add(-heartbeatTimeout)

	rows, err := d.db.Query(`
		UPDATE workers SET status = ?
		WHERE status != ? AND last_heartbeat < ?
		RETURNING id
	`, protocol.WorkerStatusOffline, protocol.WorkerStatusOffline, cutoff)
	if err != nil {
		return nil, fmt.Errorf("failed to mark offline workers: %w", err)
	}
	defer rows.Close()

	var workerIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("failed to scan offline worker id: %w", err)
		}
		workerIDs = append(workerIDs, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to iterate marked-offline workers: %w", err)
	}
	return workerIDs, nil
}
