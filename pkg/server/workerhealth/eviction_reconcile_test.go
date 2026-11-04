package workerhealth

// Regression tests for the two consistency gaps outside the migration path:
// the eviction mark must follow the persistent state (memory never diverges
// from it), and jobs this monitor terminalizes itself must release their
// WebSocket sequence bookkeeping (no complete frame follows them).

import (
	"testing"
	"time"

	"github.com/tsic404/rffmpeg/pkg/protocol"
	"github.com/tsic404/rffmpeg/pkg/server/db/dbtest"
)

type fakeStreamReleaser struct {
	released []string
}

func (f *fakeStreamReleaser) ReleaseJobStream(jobID string) {
	f.released = append(f.released, jobID)
}

func seedWorkerState(t *testing.T, table *WorkerStateTable, workerID string, jobsPerSec float64) {
	t.Helper()
	table.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
		WorkerID:      workerID,
		Status:        "online",
		JobsPerSec:    jobsPerSec,
		Timestamp:     time.Now(),
		CompletedJobs: MinJobsForEviction,
	})
}

// TestDetectSlowWorkersRevertsMarkWhenPersistFails covers the eviction side:
// DetectSlowWorkers marks the worker in memory first, so a failed persistent
// write must drop that mark again — otherwise the panel and the next judgement
// run on a state the DB does not have.
func TestDetectSlowWorkersRevertsMarkWhenPersistFails(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	table := NewWorkerStateTable(30 * time.Second)
	m := New(database, DefaultConfig())
	m.SetStateTable(table)

	// "fast" has a DB row; "ghost" does not, so its MarkWorkerEvicted fails
	// with ErrWorkerNotFound. Both are eligible samples, and ghost is the slow
	// one that detection tries to evict.
	if _, err := database.CreateWorker("fast", "fast", protocol.WorkerCapabilities{
		Encoders: []string{"libx264"}, FFmpegVersion: "6.0", MaxConcurrent: 1,
	}); err != nil {
		t.Fatalf("CreateWorker: %v", err)
	}
	seedWorkerState(t, table, "fast", 100)
	seedWorkerState(t, table, "ghost", 10)

	m.detectSlowWorkers()

	state, ok := table.Get("ghost")
	if !ok {
		t.Fatal("ghost state disappeared")
	}
	if state.Evicted {
		t.Error("in-memory eviction mark survived a failed persistent write")
	}
	events, err := database.GetEvictionEventsByWorker("ghost", 10)
	if err != nil {
		t.Fatalf("GetEvictionEventsByWorker: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("eviction audit row written for a mark that was not persisted: %+v", events)
	}
}

// TestDetectSlowWorkersKeepsMarkWhenClearFails covers the recovery side: when
// the persistent clear fails (here the worker row no longer exists), the
// in-memory mark must stay set so memory and DB keep describing the same state.
func TestDetectSlowWorkersKeepsMarkWhenClearFails(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	table := NewWorkerStateTable(30 * time.Second)
	m := New(database, DefaultConfig())
	m.SetStateTable(table)

	// Two samples: "recovering" is fast enough to hit the recovery branch,
	// and "peer" supplies the median.
	seedWorkerState(t, table, "recovering", 100)
	seedWorkerState(t, table, "peer", 50)
	table.SetEviction("recovering", true)

	// No worker row for "recovering": ClearWorkerEviction fails with
	// ErrWorkerNotFound.
	m.detectSlowWorkers()

	state, ok := table.Get("recovering")
	if !ok {
		t.Fatal("recovering state disappeared")
	}
	if !state.Evicted {
		t.Error("in-memory mark was cleared although the persistent clear failed")
	}
}

// TestMigrationBudgetExhaustionReleasesStream pins the WS cleanup for the jobs
// this monitor terminalizes: the migration path emits no complete frame, so the
// sequence counter the job's progress broadcasts created must be released.
func TestMigrationBudgetExhaustionReleasesStream(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	worker, err := database.CreateWorker("worker-1", "worker-1", protocol.WorkerCapabilities{
		Encoders: []string{"libx264"}, FFmpegVersion: "6.0", MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatalf("CreateWorker: %v", err)
	}
	job, err := database.CreateJob(`["in.mkv"]`, `["-c:v","libx264"]`, "out.mkv", false)
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if err := database.AssignJobToWorker(job.ID, worker.ID); err != nil {
		t.Fatalf("AssignJobToWorker: %v", err)
	}
	if err := dbtest.SetJobStatus(t, database, job.ID, protocol.JobStatusRunning, nil, nil, nil, nil); err != nil {
		t.Fatalf("set running: %v", err)
	}
	// Burn the migration budget so the monitor fails the job instead of
	// requeueing it.
	for i := range 3 {
		insertMigrationEventForTest(t, database, "worker-1", "worker-1", "heartbeat_timeout", i+1, []string{job.ID})
	}
	if err := database.UpdateWorkerStatus(worker.ID, protocol.WorkerStatusOffline); err != nil {
		t.Fatalf("mark worker offline: %v", err)
	}

	releaser := &fakeStreamReleaser{}
	m := New(database, Config{
		HeartbeatTimeout:    time.Minute,
		OfflineThreshold:    time.Hour,
		HealthCheckInterval: time.Second,
		MaxRetryCount:       3,
	})
	m.SetJobStreamReleaser(releaser)

	m.migrateJobsFromWorker(worker.ID)

	got, err := database.GetJob(job.ID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if got.Status != protocol.JobStatusFailed {
		t.Fatalf("job status = %s, want failed", got.Status)
	}
	if len(releaser.released) != 1 || releaser.released[0] != job.ID {
		t.Fatalf("released = %v, want [%s]", releaser.released, job.ID)
	}
}
