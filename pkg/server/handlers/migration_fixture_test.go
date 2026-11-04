package handlers_test

// Test fixtures for migration events. The production writer is
// db.MigrateJobsFromWorker (audit rows + requeue in one transaction); these
// helpers insert rows directly because the event-endpoint tests need arbitrary
// worker/job/reason combinations, including job IDs without a jobs row.

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/tsic404/rffmpeg/pkg/server/db"
)

// insertMigrationEventForTest writes one migration_events row and returns its ID.
func insertMigrationEventForTest(t *testing.T, d *db.Database, workerID, workerName, reason string, retryCount int, jobIDs []string) string {
	t.Helper()
	id := uuid.New().String()
	jobIDsJSON, err := json.Marshal(jobIDs)
	if err != nil {
		t.Fatalf("marshal job ids: %v", err)
	}
	var workerNameVal any
	if workerName != "" {
		workerNameVal = workerName
	}
	now := time.Now()
	if _, err := d.GetDB().Exec(`
		INSERT INTO migration_events (id, timestamp, worker_id, worker_name, reason, retry_count, job_ids, jobs_migrated, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, id, now, workerID, workerNameVal, reason, retryCount, string(jobIDsJSON), len(jobIDs), now); err != nil {
		t.Fatalf("insert migration event: %v", err)
	}
	return id
}

// insertRedistributionsForTest writes the per-job placeholder rows for an event.
func insertRedistributionsForTest(t *testing.T, d *db.Database, eventID string, retryCounts map[string]int) {
	t.Helper()
	now := time.Now()
	for jobID, retryCount := range retryCounts {
		if _, err := d.GetDB().Exec(`
			INSERT INTO job_redistributions (id, migration_event_id, job_id, retry_count, target_worker_id, target_worker_name, created_at)
			VALUES (?, ?, ?, ?, NULL, NULL, ?)
		`, uuid.New().String(), eventID, jobID, retryCount, now); err != nil {
			t.Fatalf("insert redistribution for %s: %v", jobID, err)
		}
	}
}
