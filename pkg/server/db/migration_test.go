package db

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/tsix404/rffmpeg/pkg/protocol"
)

func setupTestDB(t *testing.T) *Database {
	t.Helper()
	db, err := New(":memory:")
	if err != nil {
		t.Fatalf("Failed to create test database: %v", err)
	}
	return db
}

func TestCreateMigrationEvent(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	jobIDs := []string{"job-1", "job-2", "job-3"}
	event, err := db.CreateMigrationEvent("worker-1", "gpu-worker-1", string(protocol.WorkerStatusOffline), 0, jobIDs, 3)
	if err != nil {
		t.Fatalf("Failed to create migration event: %v", err)
	}

	if event.ID == "" {
		t.Error("Expected non-empty event ID")
	}
	if event.WorkerID != "worker-1" {
		t.Errorf("Expected worker ID 'worker-1', got '%s'", event.WorkerID)
	}
	if !event.WorkerName.Valid || event.WorkerName.String != "gpu-worker-1" {
		t.Errorf("Expected worker name 'gpu-worker-1', got '%v'", event.WorkerName)
	}
	if event.Reason != string(protocol.WorkerStatusOffline) {
		t.Errorf("Expected reason 'offline', got '%s'", event.Reason)
	}
	if event.RetryCount != 0 {
		t.Errorf("Expected retry count 0, got %d", event.RetryCount)
	}
	if event.JobsMigrated != 3 {
		t.Errorf("Expected 3 jobs migrated, got %d", event.JobsMigrated)
	}

	// Verify job IDs are stored as JSON
	var storedJobIDs []string
	if err := json.Unmarshal([]byte(event.JobIDs), &storedJobIDs); err != nil {
		t.Fatalf("Failed to unmarshal job IDs: %v", err)
	}
	if len(storedJobIDs) != 3 {
		t.Errorf("Expected 3 job IDs, got %d", len(storedJobIDs))
	}
}

func TestGetMigrationEvent(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	// Create event
	event, err := db.CreateMigrationEvent("worker-1", "worker-name", "heartbeat_timeout", 1, []string{"job-1"}, 1)
	if err != nil {
		t.Fatalf("Failed to create migration event: %v", err)
	}

	// Retrieve event
	retrieved, err := db.GetMigrationEvent(event.ID)
	if err != nil {
		t.Fatalf("Failed to get migration event: %v", err)
	}

	if retrieved.ID != event.ID {
		t.Errorf("Expected ID '%s', got '%s'", event.ID, retrieved.ID)
	}
	if retrieved.WorkerID != "worker-1" {
		t.Errorf("Expected worker ID 'worker-1', got '%s'", retrieved.WorkerID)
	}
}

func TestGetMigrationEventNotFound(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	_, err := db.GetMigrationEvent("non-existent")
	if err == nil {
		t.Error("Expected error for non-existent event")
	}
}

func TestGetMigrationEvents(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	// Create multiple events
	_, err := db.CreateMigrationEvent("worker-1", "worker-1", "heartbeat_timeout", 0, []string{"job-1"}, 1)
	if err != nil {
		t.Fatalf("Failed to create first event: %v", err)
	}

	_, err = db.CreateMigrationEvent("worker-2", "worker-2", "heartbeat_timeout", 1, []string{"job-2", "job-3"}, 2)
	if err != nil {
		t.Fatalf("Failed to create second event: %v", err)
	}

	// Retrieve events
	events, err := db.GetMigrationEvents(10, 0)
	if err != nil {
		t.Fatalf("Failed to get migration events: %v", err)
	}

	if len(events) != 2 {
		t.Errorf("Expected 2 events, got %d", len(events))
	}

	// Verify ordering (most recent first)
	if events[0].WorkerID != "worker-2" {
		t.Errorf("Expected first event worker ID 'worker-2', got '%s'", events[0].WorkerID)
	}
}

func TestGetMigrationEventsByWorker(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	// Create events for different workers
	_, _ = db.CreateMigrationEvent("worker-1", "worker-1", "heartbeat_timeout", 0, []string{"job-1"}, 1)
	_, _ = db.CreateMigrationEvent("worker-2", "worker-2", "heartbeat_timeout", 0, []string{"job-2"}, 1)
	_, _ = db.CreateMigrationEvent("worker-1", "worker-1", "heartbeat_timeout", 1, []string{"job-3"}, 1)

	// Retrieve events for worker-1
	events, err := db.GetMigrationEventsByWorker("worker-1", 10)
	if err != nil {
		t.Fatalf("Failed to get migration events by worker: %v", err)
	}

	if len(events) != 2 {
		t.Errorf("Expected 2 events for worker-1, got %d", len(events))
	}
}

func TestGetRunningJobsByWorker(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	// Create a worker
	_, err := db.CreateWorker("worker-1", "test-worker", protocol.WorkerCapabilities{
		Encoders:      []string{"libx264"},
		FFmpegVersion: "6.0",
	})
	if err != nil {
		t.Fatalf("Failed to create worker: %v", err)
	}

	// Create and assign jobs
	job1, err := db.CreateJob(`["input1.mkv"]`, `["-c:v","libx264"]`, "output1.mkv", false)
	if err != nil {
		t.Fatalf("Failed to create job1: %v", err)
	}

	job2, err := db.CreateJob(`["input2.mkv"]`, `["-c:v","libx264"]`, "output2.mkv", false)
	if err != nil {
		t.Fatalf("Failed to create job2: %v", err)
	}

	// Assign job1 to worker and set to running
	_ = db.AssignJobToWorker(job1.ID, "worker-1")
	_ = db.UpdateJobStatusWithFailure(job1.ID, protocol.JobStatusQueued, nil, nil, nil, nil)
	_ = db.UpdateJobStatusWithFailure(job1.ID, protocol.JobStatusRunning, nil, nil, nil, nil)

	// Assign job2 to worker and set to queued
	_ = db.AssignJobToWorker(job2.ID, "worker-1")
	_ = db.UpdateJobStatusWithFailure(job2.ID, protocol.JobStatusQueued, nil, nil, nil, nil)

	// Get running jobs
	jobs, err := db.GetRunningJobsByWorker("worker-1")
	if err != nil {
		t.Fatalf("Failed to get running jobs by worker: %v", err)
	}

	if len(jobs) != 2 {
		t.Errorf("Expected 2 running jobs, got %d", len(jobs))
	}
}

func TestMigrateJobsFromWorker(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	// Create a worker
	_, err := db.CreateWorker("worker-1", "test-worker", protocol.WorkerCapabilities{
		Encoders:      []string{"libx264"},
		FFmpegVersion: "6.0",
	})
	if err != nil {
		t.Fatalf("Failed to create worker: %v", err)
	}

	// Create and assign jobs
	job1, err := db.CreateJob(`["input1.mkv"]`, `["-c:v","libx264"]`, "output1.mkv", false)
	if err != nil {
		t.Fatalf("Failed to create job1: %v", err)
	}

	job2, err := db.CreateJob(`["input2.mkv"]`, `["-c:v","libx264"]`, "output2.mkv", false)
	if err != nil {
		t.Fatalf("Failed to create job2: %v", err)
	}

	// Assign jobs to worker and set to running/queued
	_ = db.AssignJobToWorker(job1.ID, "worker-1")
	_ = db.UpdateJobStatusWithFailure(job1.ID, protocol.JobStatusRunning, nil, nil, nil, nil)

	_ = db.AssignJobToWorker(job2.ID, "worker-1")
	_ = db.UpdateJobStatusWithFailure(job2.ID, protocol.JobStatusQueued, nil, nil, nil, nil)

	// Migrate jobs
	migratedJobIDs, err := db.MigrateJobsFromWorker("worker-1")
	if err != nil {
		t.Fatalf("Failed to migrate jobs: %v", err)
	}

	if len(migratedJobIDs) != 2 {
		t.Fatalf("Expected 2 migrated job IDs, got %d", len(migratedJobIDs))
	}

	// Verify jobs are now pending
	for _, jobID := range migratedJobIDs {
		job, err := db.GetJob(jobID)
		if err != nil {
			t.Fatalf("Failed to get job %s: %v", jobID, err)
		}
		if job.Status != protocol.JobStatusPending {
			t.Errorf("Expected job %s status to be pending, got %s", jobID, job.Status)
		}
		if job.WorkerID.Valid {
			t.Errorf("Expected job %s worker_id to be NULL, got %s", jobID, job.WorkerID.String)
		}
		if job.StartedAt.Valid {
			t.Errorf("Expected job %s started_at to be NULL", jobID)
		}
	}
}

func TestMigrateJobsFromWorkerNoJobs(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	// Create a worker with no jobs
	_, err := db.CreateWorker("worker-1", "test-worker", protocol.WorkerCapabilities{
		Encoders:      []string{"libx264"},
		FFmpegVersion: "6.0",
	})
	if err != nil {
		t.Fatalf("Failed to create worker: %v", err)
	}

	// Migrate jobs (should return nil, nil)
	migratedJobIDs, err := db.MigrateJobsFromWorker("worker-1")
	if err != nil {
		t.Fatalf("Failed to migrate jobs: %v", err)
	}

	if migratedJobIDs != nil {
		t.Errorf("Expected nil migrated job IDs, got %v", migratedJobIDs)
	}
}

func TestMarkOfflineWorkersWithMigration(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	// Create workers
	_, err := db.CreateWorker("worker-1", "active-worker", protocol.WorkerCapabilities{
		Encoders:      []string{"libx264"},
		FFmpegVersion: "6.0",
	})
	if err != nil {
		t.Fatalf("Failed to create worker-1: %v", err)
	}

	_, err = db.CreateWorker("worker-2", "stale-worker", protocol.WorkerCapabilities{
		Encoders:      []string{"libx264"},
		FFmpegVersion: "6.0",
	})
	if err != nil {
		t.Fatalf("Failed to create worker-2: %v", err)
	}

	// Make worker-2's heartbeat stale (set it to the past)
	_, err = db.GetDB().Exec(`
		UPDATE workers SET last_heartbeat = ? WHERE id = ?
	`, time.Now().Add(-60*time.Second), "worker-2")
	if err != nil {
		t.Fatalf("Failed to update worker-2 heartbeat: %v", err)
	}

	// Mark offline workers
	offlineWorkers, err := db.MarkOfflineWorkersWithMigration(30 * time.Second)
	if err != nil {
		t.Fatalf("Failed to mark offline workers: %v", err)
	}

	if len(offlineWorkers) != 1 {
		t.Fatalf("Expected 1 offline worker, got %d", len(offlineWorkers))
	}

	if offlineWorkers[0] != "worker-2" {
		t.Errorf("Expected worker-2 to be offline, got %s", offlineWorkers[0])
	}

	// Verify worker-1 is still idle
	worker1, err := db.GetWorker("worker-1")
	if err != nil {
		t.Fatalf("Failed to get worker-1: %v", err)
	}
	if worker1.Status != protocol.WorkerStatusIdle {
		t.Errorf("Expected worker-1 to be idle, got %s", worker1.Status)
	}

	// Verify worker-2 is offline
	worker2, err := db.GetWorker("worker-2")
	if err != nil {
		t.Fatalf("Failed to get worker-2: %v", err)
	}
	if worker2.Status != protocol.WorkerStatusOffline {
		t.Errorf("Expected worker-2 to be offline, got %s", worker2.Status)
	}
}

func TestMarkOfflineWorkersWithMigrationNoOffline(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	// Create a worker with recent heartbeat
	_, err := db.CreateWorker("worker-1", "active-worker", protocol.WorkerCapabilities{
		Encoders:      []string{"libx264"},
		FFmpegVersion: "6.0",
	})
	if err != nil {
		t.Fatalf("Failed to create worker: %v", err)
	}

	// Mark offline workers (should return nil)
	offlineWorkers, err := db.MarkOfflineWorkersWithMigration(30 * time.Second)
	if err != nil {
		t.Fatalf("Failed to mark offline workers: %v", err)
	}

	if offlineWorkers != nil {
		t.Errorf("Expected nil offline workers, got %v", offlineWorkers)
	}
}

func TestGetJobRetryCount(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	jobID := "test-job-retry"

	// Initially should be 0
	count, err := db.GetJobRetryCount(jobID)
	if err != nil {
		t.Fatalf("Failed to get job retry count: %v", err)
	}
	if count != 0 {
		t.Errorf("Expected retry count 0, got %d", count)
	}

	// Create a migration event with this job
	_, err = db.CreateMigrationEvent("worker-1", "worker-1", "heartbeat_timeout", 0, []string{jobID}, 1)
	if err != nil {
		t.Fatalf("Failed to create migration event: %v", err)
	}

	// Should now be 1
	count, err = db.GetJobRetryCount(jobID)
	if err != nil {
		t.Fatalf("Failed to get job retry count: %v", err)
	}
	if count != 1 {
		t.Errorf("Expected retry count 1, got %d", count)
	}

	// Create another migration event with this job
	_, err = db.CreateMigrationEvent("worker-2", "worker-2", "heartbeat_timeout", 1, []string{jobID}, 1)
	if err != nil {
		t.Fatalf("Failed to create second migration event: %v", err)
	}

	// Should now be 2
	count, err = db.GetJobRetryCount(jobID)
	if err != nil {
		t.Fatalf("Failed to get job retry count: %v", err)
	}
	if count != 2 {
		t.Errorf("Expected retry count 2, got %d", count)
	}
}

func TestCreateMigrationEventEmptyWorkerName(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	event, err := db.CreateMigrationEvent("worker-1", "", "heartbeat_timeout", 0, []string{"job-1"}, 1)
	if err != nil {
		t.Fatalf("Failed to create migration event: %v", err)
	}

	if event.WorkerName.Valid {
		t.Errorf("Expected worker name to be NULL, got '%s'", event.WorkerName.String)
	}
}

func TestFailJob(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	// Create a worker and job
	_, err := db.CreateWorker("worker-1", "test-worker", protocol.WorkerCapabilities{
		Encoders:      []string{"libx264"},
		FFmpegVersion: "6.0",
	})
	if err != nil {
		t.Fatalf("Failed to create worker: %v", err)
	}

	job, err := db.CreateJob(`["input.mkv"]`, `["-c:v","libx264"]`, "output.mkv", false)
	if err != nil {
		t.Fatalf("Failed to create job: %v", err)
	}

	// Assign and set running
	_ = db.AssignJobToWorker(job.ID, "worker-1")
	_ = db.UpdateJobStatusWithFailure(job.ID, protocol.JobStatusRunning, nil, nil, nil, nil)

	// Fail the job
	err = db.FailJob(job.ID, "test failure reason", string(protocol.FailureWorkerCrash))
	if err != nil {
		t.Fatalf("Failed to fail job: %v", err)
	}

	// Verify job is marked as failed
	updatedJob, err := db.GetJob(job.ID)
	if err != nil {
		t.Fatalf("Failed to get job: %v", err)
	}
	if updatedJob.Status != protocol.JobStatusFailed {
		t.Errorf("Expected status 'failed', got '%s'", updatedJob.Status)
	}
	if !updatedJob.Error.Valid || updatedJob.Error.String != "test failure reason" {
		t.Errorf("Expected error 'test failure reason', got '%v'", updatedJob.Error)
	}
	if updatedJob.WorkerID.Valid {
		t.Errorf("Expected worker_id to be NULL, got '%s'", updatedJob.WorkerID.String)
	}
	if !updatedJob.ExitCode.Valid || updatedJob.ExitCode.Int32 != -1 {
		t.Errorf("Expected exit_code -1, got %v", updatedJob.ExitCode)
	}
	if updatedJob.FailureType != string(protocol.FailureWorkerCrash) {
		t.Errorf("Expected failure_type %q, got %q",
			protocol.FailureWorkerCrash, updatedJob.FailureType)
	}
}

func TestFailJobNotFound(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	err := db.FailJob("non-existent", "reason", string(protocol.FailureWorkerCrash))
	if err == nil {
		t.Error("Expected error for non-existent job")
	}
}

func TestResetJobToPending(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	// Create a worker and job
	_, err := db.CreateWorker("worker-1", "test-worker", protocol.WorkerCapabilities{
		Encoders:      []string{"libx264"},
		FFmpegVersion: "6.0",
	})
	if err != nil {
		t.Fatalf("Failed to create worker: %v", err)
	}

	job, err := db.CreateJob(`["input.mkv"]`, `["-c:v","libx264"]`, "output.mkv", false)
	if err != nil {
		t.Fatalf("Failed to create job: %v", err)
	}

	// Assign and set running
	_ = db.AssignJobToWorker(job.ID, "worker-1")
	_ = db.UpdateJobStatusWithFailure(job.ID, protocol.JobStatusRunning, nil, nil, nil, nil)

	// Reset to pending
	err = db.ResetJobToPending(job.ID)
	if err != nil {
		t.Fatalf("Failed to reset job to pending: %v", err)
	}

	// Verify job is pending
	updatedJob, err := db.GetJob(job.ID)
	if err != nil {
		t.Fatalf("Failed to get job: %v", err)
	}
	if updatedJob.Status != protocol.JobStatusPending {
		t.Errorf("Expected status 'pending', got '%s'", updatedJob.Status)
	}
	if updatedJob.WorkerID.Valid {
		t.Errorf("Expected worker_id to be NULL, got '%s'", updatedJob.WorkerID.String)
	}
}

func TestResetJobToPendingNotFound(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	err := db.ResetJobToPending("non-existent")
	if err == nil {
		t.Error("Expected error for non-existent job")
	}
}
