package scheduler

// Test fixtures for crafted migration histories. The production writer is
// db.MigrateJobsFromWorker (audit rows + requeue in one transaction); these
// helpers insert event rows directly because the starvation-classification
// tests need arbitrary reason/timestamp combinations that a full failover
// cannot produce.

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
