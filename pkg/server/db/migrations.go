package db

import (
	"database/sql"
	"encoding/json"
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

// CreateMigrationEvent creates a new migration event record.
//
// retryCount is the max cumulative migration count among the migrated jobs
// after this event (the first migration records 1, not 0), persisted verbatim.
// A batch whose jobs carry different histories stores each job's exact count
// in job_redistributions.retry_count (see CreateJobRedistributions).
func (d *Database) CreateMigrationEvent(workerID, workerName, reason string, retryCount int, jobIDs []string, jobsMigrated int) (*MigrationEvent, error) {
	id := uuid.New().String()
	now := time.Now()

	jobIDsJSON, err := json.Marshal(jobIDs)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal job IDs: %w", err)
	}

	var workerNameVal interface{}
	if workerName != "" {
		workerNameVal = workerName
	}

	_, err = d.db.Exec(`
		INSERT INTO migration_events (id, timestamp, worker_id, worker_name, reason, retry_count, job_ids, jobs_migrated, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, id, now, workerID, workerNameVal, reason, retryCount, string(jobIDsJSON), jobsMigrated, now)

	if err != nil {
		return nil, fmt.Errorf("failed to create migration event: %w", err)
	}

	return d.GetMigrationEvent(id)
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

// CreateJobRedistributions inserts an unresolved placeholder row per migrated
// job, linking it to the migration event that migrated it. retryCounts maps
// each job ID to its cumulative migration count after this event (first
// migration = 1), persisted per job so a batch whose jobs carry different
// migration histories keeps each job's exact count. The target is filled later
// by RecordMigrationTarget when the scheduler/pull path reassigns the job. This
// is a cold path (one call per worker-offline migration).
func (d *Database) CreateJobRedistributions(migrationEventID string, retryCounts map[string]int) error {
	tx, err := d.db.Begin()
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	now := time.Now()
	for jobID, retryCount := range retryCounts {
		id := uuid.New().String()
		if _, err := tx.Exec(`
			INSERT INTO job_redistributions (id, migration_event_id, job_id, retry_count, target_worker_id, target_worker_name, created_at)
			VALUES (?, ?, ?, ?, NULL, NULL, ?)
		`, id, migrationEventID, jobID, retryCount, now); err != nil {
			return fmt.Errorf("failed to create job redistribution for job %s: %w", jobID, err)
		}
	}

	return tx.Commit()
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

// MigrateJobsFromWorker migrates all in-progress jobs from a worker back to
// pending state. Returns the list of job IDs that were migrated.
//
// the whole migration runs in one transaction, and each reset is a
// conditional UPDATE on status IN (running, queued). This closes two holes:
// (1) a job that reaches a terminal state after the snapshot is no longer
// dragged back to pending and re-run, and (2) a partial failure mid-migration
// now rolls back instead of leaving jobs half-reset with orphaned outputs.
func (d *Database) MigrateJobsFromWorker(workerID string) ([]string, error) {
	tx, err := d.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	now := time.Now()

	rows, err := tx.Query(`
		UPDATE jobs SET status = ?, worker_id = NULL, started_at = NULL, claimed_at = NULL, updated_at = ?,
		                created_at = ?
		WHERE worker_id = ? AND status IN (?, ?)
		RETURNING id
	`, protocol.JobStatusPending, now, now, workerID,
		protocol.JobStatusRunning, protocol.JobStatusQueued)
	if err != nil {
		return nil, fmt.Errorf("failed to migrate jobs for worker %s: %w", workerID, err)
	}

	var jobIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, fmt.Errorf("failed to scan migrated job id: %w", err)
		}
		jobIDs = append(jobIDs, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("failed to iterate migrated jobs: %w", err)
	}
	rows.Close()

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("failed to commit migration: %w", err)
	}

	return jobIDs, nil
}

// MarkOfflineWorkersWithMigration marks workers as offline and returns their IDs for job migration.
// This is similar to MarkOfflineWorkers but returns the worker IDs that were marked offline.
func (d *Database) MarkOfflineWorkersWithMigration(heartbeatTimeout time.Duration) ([]string, error) {
	cutoff := time.Now().Add(-heartbeatTimeout)

	rows, err := d.db.Query(`
		SELECT id FROM workers
		WHERE status != ? AND last_heartbeat < ?
	`, protocol.WorkerStatusOffline, cutoff)
	if err != nil {
		return nil, fmt.Errorf("failed to query offline workers: %w", err)
	}

	var workerIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, fmt.Errorf("failed to scan worker id: %w", err)
		}
		workerIDs = append(workerIDs, id)
	}
	rows.Close()

	if len(workerIDs) == 0 {
		return nil, nil
	}

	_, err = d.db.Exec(`
		UPDATE workers SET status = ?
		WHERE status != ? AND last_heartbeat < ?
	`, protocol.WorkerStatusOffline, protocol.WorkerStatusOffline, cutoff)
	if err != nil {
		return nil, fmt.Errorf("failed to mark offline workers: %w", err)
	}

	return workerIDs, nil
}
