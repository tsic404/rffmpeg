package db_test

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/tsic404/rffmpeg/pkg/protocol"
	"github.com/tsic404/rffmpeg/pkg/server/db"
)

func setupDBTest(t *testing.T) (*db.Database, func()) {
	tmpDir, err := os.MkdirTemp("", "rffmpeg-db-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}

	database, err := db.New(fmt.Sprintf("%s/test.db", tmpDir))
	if err != nil {
		os.RemoveAll(tmpDir)
		t.Fatalf("Failed to create database: %v", err)
	}

	cleanup := func() {
		database.Close()
		os.RemoveAll(tmpDir)
	}

	return database, cleanup
}

func TestCreateAndGetWorker(t *testing.T) {
	database, cleanup := setupDBTest(t)
	defer cleanup()

	caps := protocol.WorkerCapabilities{
		GPUModel:      "NVIDIA RTX 3080",
		Encoders:      []string{"libx264", "h264_nvenc"},
		Decoders:      []string{"h264", "hevc"},
		FFmpegVersion: "5.1.2",
		MaxConcurrent: 2,
	}

	worker, err := database.CreateWorker("", "test-worker", caps)
	if err != nil {
		t.Fatalf("Failed to create worker: %v", err)
	}

	if worker.ID == "" {
		t.Error("Expected worker ID to be set")
	}

	if worker.Name != "test-worker" {
		t.Errorf("Expected name 'test-worker', got '%s'", worker.Name)
	}

	if worker.Status != protocol.WorkerStatusIdle {
		t.Errorf("Expected status 'idle', got '%s'", worker.Status)
	}

	if worker.FFmpegVersion != "5.1.2" {
		t.Errorf("Expected ffmpeg version '5.1.2', got '%s'", worker.FFmpegVersion)
	}

	// Retrieve worker
	retrieved, err := database.GetWorker(worker.ID)
	if err != nil {
		t.Fatalf("Failed to get worker: %v", err)
	}

	if retrieved.ID != worker.ID {
		t.Errorf("Expected ID '%s', got '%s'", worker.ID, retrieved.ID)
	}
}

func TestUpdateWorkerHeartbeat(t *testing.T) {
	database, cleanup := setupDBTest(t)
	defer cleanup()

	caps := protocol.WorkerCapabilities{
		Encoders:      []string{"libx264"},
		FFmpegVersion: "5.1.2",
	}

	worker, err := database.CreateWorker("", "test-worker", caps)
	if err != nil {
		t.Fatalf("Failed to create worker: %v", err)
	}

	// Heartbeat refreshes last_heartbeat only; status is derived state and is
	// not written by heartbeats anymore (a late idle beat must not overwrite
	// the busy state set by the pull path).
	err = database.UpdateWorkerHeartbeat(worker.ID, protocol.WorkerStatusBusy)
	if err != nil {
		t.Fatalf("Failed to update heartbeat: %v", err)
	}

	// Verify liveness was refreshed without touching status
	retrieved, err := database.GetWorker(worker.ID)
	if err != nil {
		t.Fatalf("Failed to get worker: %v", err)
	}

	if retrieved.Status != protocol.WorkerStatusIdle {
		t.Errorf("Expected status to remain 'idle' after heartbeat, got '%s'", retrieved.Status)
	}
}

// TestUpdateWorkerHeartbeatUUIDFormatMismatch (TSI-2346 follow-up): the
// heartbeat path must canonicalize its ID argument like registration and
// lookup do — a heartbeat carrying a compact/uppercase variant of a stored
// hyphenated UUID refreshes the same row instead of returning 404.
// Heartbeats no longer write status (derived state, PR #24); the assertion
// targets liveness only.
func TestUpdateWorkerHeartbeatUUIDFormatMismatch(t *testing.T) {
	database, cleanup := setupDBTest(t)
	defer cleanup()

	hyphenated := "550e8400-e29b-41d4-a716-446655440000"
	if _, err := database.CreateOrUpdateWorker(hyphenated, "heartbeat-worker", protocol.WorkerCapabilities{
		Encoders:      []string{"libx264"},
		FFmpegVersion: "5.1.2",
	}, 90*time.Second); err != nil {
		t.Fatalf("Failed to create worker: %v", err)
	}

	before, err := database.GetWorker(hyphenated)
	if err != nil {
		t.Fatalf("Failed to get worker before heartbeat: %v", err)
	}

	for name, id := range map[string]string{
		"compact":              "550e8400e29b41d4a716446655440000",
		"uppercase hyphenated": strings.ToUpper(hyphenated),
	} {
		if err := database.UpdateWorkerHeartbeat(id, protocol.WorkerStatusBusy); err != nil {
			t.Errorf("%s heartbeat failed: %v", name, err)
		}
	}

	worker, err := database.GetWorker(hyphenated)
	if err != nil {
		t.Fatalf("Failed to get worker: %v", err)
	}
	if !worker.LastHeartbeat.After(before.LastHeartbeat) {
		t.Errorf("Expected last_heartbeat to be refreshed by compact-format heartbeats")
	}
}

func TestMarkOfflineWorkers(t *testing.T) {
	database, cleanup := setupDBTest(t)
	defer cleanup()

	caps := protocol.WorkerCapabilities{
		Encoders:      []string{"libx264"},
		FFmpegVersion: "5.1.2",
	}

	// Create two workers
	worker1, err := database.CreateWorker("", "worker-1", caps)
	if err != nil {
		t.Fatalf("Failed to create worker1: %v", err)
	}

	worker2, err := database.CreateWorker("", "worker-2", caps)
	if err != nil {
		t.Fatalf("Failed to create worker2: %v", err)
	}

	// Deterministically age worker1's heartbeat and keep worker2's fresh via a
	// direct UPDATE. The original wall-clock sleep left only a ~50ms margin
	// between worker2's heartbeat refresh and MarkOfflineWorkers, which flaked
	// under CI load by marking both workers offline.
	now := time.Now()
	if _, err := database.GetDB().Exec(
		`UPDATE workers SET last_heartbeat = ? WHERE id = ?`,
		now.Add(-time.Hour), worker1.ID,
	); err != nil {
		t.Fatalf("Failed to age worker1 heartbeat: %v", err)
	}
	if _, err := database.GetDB().Exec(
		`UPDATE workers SET last_heartbeat = ? WHERE id = ?`,
		now.Add(time.Hour), worker2.ID,
	); err != nil {
		t.Fatalf("Failed to refresh worker2 heartbeat: %v", err)
	}

	// Mark workers offline with a short timeout (50ms)
	marked, err := database.MarkOfflineWorkers(50 * time.Millisecond)
	if err != nil {
		t.Fatalf("Failed to mark offline workers: %v", err)
	}

	if marked != 1 {
		t.Errorf("Expected 1 worker to be marked offline, got %d", marked)
	}

	// Verify worker1 is offline
	retrieved1, err := database.GetWorker(worker1.ID)
	if err != nil {
		t.Fatalf("Failed to get worker1: %v", err)
	}

	if retrieved1.Status != protocol.WorkerStatusOffline {
		t.Errorf("Expected worker1 status 'offline', got '%s'", retrieved1.Status)
	}

	// Verify worker2 is still idle
	retrieved2, err := database.GetWorker(worker2.ID)
	if err != nil {
		t.Fatalf("Failed to get worker2: %v", err)
	}

	if retrieved2.Status != protocol.WorkerStatusIdle {
		t.Errorf("Expected worker2 status 'idle', got '%s'", retrieved2.Status)
	}
}

func TestRemoveOfflineWorkers(t *testing.T) {
	database, cleanup := setupDBTest(t)
	defer cleanup()

	caps := protocol.WorkerCapabilities{
		Encoders:      []string{"libx264"},
		FFmpegVersion: "5.1.2",
	}

	// Create worker
	worker, err := database.CreateWorker("", "test-worker", caps)
	if err != nil {
		t.Fatalf("Failed to create worker: %v", err)
	}

	// Mark worker offline first
	_, err = database.MarkOfflineWorkers(0)
	if err != nil {
		t.Fatalf("Failed to mark worker offline: %v", err)
	}

	// Wait to exceed threshold
	time.Sleep(100 * time.Millisecond)

	// Remove workers offline for more than 50ms
	removedIDs, err := database.RemoveOfflineWorkers(50 * time.Millisecond)
	if err != nil {
		t.Fatalf("Failed to remove offline workers: %v", err)
	}

	if len(removedIDs) != 1 {
		t.Errorf("Expected 1 worker to be removed, got %d", len(removedIDs))
	}

	// Verify worker is gone
	_, err = database.GetWorker(worker.ID)
	if err != protocol.ErrWorkerNotFound {
		t.Errorf("Expected ErrWorkerNotFound, got %v", err)
	}
}

func TestGetActiveWorkers(t *testing.T) {
	database, cleanup := setupDBTest(t)
	defer cleanup()

	caps := protocol.WorkerCapabilities{
		Encoders:      []string{"libx264"},
		FFmpegVersion: "5.1.2",
	}

	// Create two workers
	_, err := database.CreateWorker("", "worker-1", caps)
	if err != nil {
		t.Fatalf("Failed to create worker1: %v", err)
	}

	_, err = database.CreateWorker("", "worker-2", caps)
	if err != nil {
		t.Fatalf("Failed to create worker2: %v", err)
	}

	// Get active workers
	workers, err := database.GetActiveWorkers()
	if err != nil {
		t.Fatalf("Failed to get active workers: %v", err)
	}

	if len(workers) != 2 {
		t.Errorf("Expected 2 active workers, got %d", len(workers))
	}

	// Mark one as offline
	database.MarkOfflineWorkers(0)

	// Verify only one active
	workers, err = database.GetActiveWorkers()
	if err != nil {
		t.Fatalf("Failed to get active workers: %v", err)
	}

	if len(workers) != 0 {
		t.Errorf("Expected 0 active workers after marking offline, got %d", len(workers))
	}
}

func TestWorkerNotFound(t *testing.T) {
	database, cleanup := setupDBTest(t)
	defer cleanup()

	_, err := database.GetWorker("non-existent-id")
	if err != protocol.ErrWorkerNotFound {
		t.Errorf("Expected ErrWorkerNotFound, got %v", err)
	}
}

// TestGetWorkerUUIDFormatMismatch (TSI-2346): lookups must tolerate UUID
// formatting differences between the stored ID and the query parameter
// (hyphenated vs. compact, surrounding whitespace).
func TestGetWorkerUUIDFormatMismatch(t *testing.T) {
	database, cleanup := setupDBTest(t)
	defer cleanup()

	hyphenated := "550e8400-e29b-41d4-a716-446655440000"
	compact := "550e8400e29b41d4a716446655440000"

	if _, err := database.CreateOrUpdateWorker(hyphenated, "uuid-worker", protocol.WorkerCapabilities{
		Encoders:      []string{"libx264"},
		FFmpegVersion: "5.1.2",
	}, 90*time.Second); err != nil {
		t.Fatalf("Failed to create worker: %v", err)
	}

	for name, id := range map[string]string{
		"hyphenated":           hyphenated,
		"compact":              compact,
		"uppercase hyphenated": strings.ToUpper(hyphenated),
	} {
		worker, err := database.GetWorker(id)
		if err != nil {
			t.Errorf("%s lookup failed: %v", name, err)
			continue
		}
		if worker.ID != hyphenated {
			t.Errorf("%s lookup returned ID %q, want stored ID %q", name, worker.ID, hyphenated)
		}
	}

	// Registering the compact form of the same UUID must converge on the
	// existing hyphenated row (write path normalizes too, TSI-2346): it
	// upserts "uuid-worker" rather than inserting a second worker.
	if _, err := database.CreateOrUpdateWorker(compact, "uuid-worker", protocol.WorkerCapabilities{
		Encoders:      []string{"libx264"},
		FFmpegVersion: "5.1.2",
	}, 90*time.Second); err != nil {
		t.Fatalf("Failed to re-register worker via compact UUID: %v", err)
	}
	all, err := database.GetAllWorkers()
	if err != nil {
		t.Fatalf("Failed to list workers: %v", err)
	}
	if len(all) != 1 || all[0].ID != hyphenated {
		t.Errorf("Expected single converged row %q, got %d rows: %+v", hyphenated, len(all), all)
	}
	if worker, err := database.GetWorker(compact); err != nil {
		t.Fatalf("Compact lookup after convergence failed: %v", err)
	} else if worker.Name != "uuid-worker" {
		t.Errorf("Compact lookup returned name %q, want uuid-worker", worker.Name)
	}

	// Non-UUID identifiers are unaffected.
	if _, err := database.GetWorker("non-existent-id"); err != protocol.ErrWorkerNotFound {
		t.Errorf("Expected ErrWorkerNotFound for non-UUID unknown ID, got %v", err)
	}
}

func TestRecoverState(t *testing.T) {
	database, cleanup := setupDBTest(t)
	defer cleanup()

	caps := protocol.WorkerCapabilities{
		Encoders:      []string{"libx264"},
		FFmpegVersion: "5.1.2",
	}

	// Create workers
	worker1, err := database.CreateWorker("", "worker-1", caps)
	if err != nil {
		t.Fatalf("Failed to create worker1: %v", err)
	}

	worker2, err := database.CreateWorker("", "worker-2", caps)
	if err != nil {
		t.Fatalf("Failed to create worker2: %v", err)
	}

	// Create jobs with different statuses
	inputFiles := `["file1.mp4"]`
	args := `["-c:v", "libx264"]`
	outputFilename := "output.mp4"

	// Pending job
	job1, err := database.CreateJob(inputFiles, args, outputFilename, false)
	if err != nil {
		t.Fatalf("Failed to create job1: %v", err)
	}

	// Running job
	job2, err := database.CreateJob(inputFiles, args, outputFilename, false)
	if err != nil {
		t.Fatalf("Failed to create job2: %v", err)
	}
	err = database.AssignJobToWorker(job2.ID, worker1.ID)
	if err != nil {
		t.Fatalf("Failed to assign job2: %v", err)
	}
	err = database.UpdateJobStatusWithFailure(job2.ID, protocol.JobStatusRunning, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("Failed to set job2 as running: %v", err)
	}

	// Queued job - Create and assign via AssignPendingJobsToWorker which sets status to queued
	// Note: This will also assign job1 to worker2, making job1 queued too
	job3, err := database.CreateJob(inputFiles, args, outputFilename, false)
	if err != nil {
		t.Fatalf("Failed to create job3: %v", err)
	}
	// Use AssignPendingJobsToWorker which sets status to queued
	queuedJobs, err := database.AssignPendingJobsToWorker(worker2.ID, 10)
	if err != nil {
		t.Fatalf("Failed to assign jobs to worker2: %v", err)
	}
	// Verify job3 was assigned
	if len(queuedJobs) < 1 {
		t.Fatalf("Expected at least one job to be assigned to worker2")
	}
	// Find job3 in the queued jobs
	job3Queued := false
	for _, j := range queuedJobs {
		if j.ID == job3.ID {
			job3Queued = true
			break
		}
	}
	if !job3Queued {
		t.Fatalf("Job3 was not assigned to worker2")
	}

	// Completed job (should not be affected)
	job4, err := database.CreateJob(inputFiles, args, outputFilename, false)
	if err != nil {
		t.Fatalf("Failed to create job4: %v", err)
	}
	err = database.UpdateJobStatusWithFailure(job4.ID, protocol.JobStatusCompleted, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("Failed to set job4 as completed: %v", err)
	}

	// Backdate job2's created_at so the recovery refresh (TSI-2597) is
	// observable: RecoverState must re-anchor the starvation/NoWorkerDeadline
	// clock from the restart moment, not the pre-restart submission.
	backdated := time.Now().Add(-1 * time.Hour)
	if _, err := database.GetDB().Exec(`UPDATE jobs SET created_at = ? WHERE id = ?`, backdated, job2.ID); err != nil {
		t.Fatalf("Failed to backdate job2 created_at: %v", err)
	}

	// Perform recovery
	jobsReset, workersMarkedOffline, err := database.RecoverState()
	if err != nil {
		t.Fatalf("Failed to recover state: %v", err)
	}

	// Should have reset 3 jobs (job1 and job3 were queued, job2 was running)
	if jobsReset != 3 {
		t.Errorf("Expected 3 jobs to be reset, got %d", jobsReset)
	}

	// Should have marked 2 workers offline
	if workersMarkedOffline != 2 {
		t.Errorf("Expected 2 workers to be marked offline, got %d", workersMarkedOffline)
	}

	// Verify job1 is now pending and unassigned (was queued, now reset)
	retrievedJob1, err := database.GetJob(job1.ID)
	if err != nil {
		t.Fatalf("Failed to get job1: %v", err)
	}
	if retrievedJob1.Status != protocol.JobStatusPending {
		t.Errorf("Expected job1 status 'pending', got '%s'", retrievedJob1.Status)
	}
	if retrievedJob1.WorkerID.Valid {
		t.Errorf("Expected job1 worker_id to be NULL, got '%s'", retrievedJob1.WorkerID.String)
	}

	// Verify job2 is now pending and unassigned
	retrievedJob2, err := database.GetJob(job2.ID)
	if err != nil {
		t.Fatalf("Failed to get job2: %v", err)
	}
	if retrievedJob2.Status != protocol.JobStatusPending {
		t.Errorf("Expected job2 status 'pending', got '%s'", retrievedJob2.Status)
	}
	if retrievedJob2.WorkerID.Valid {
		t.Errorf("Expected job2 worker_id to be NULL, got '%s'", retrievedJob2.WorkerID.String)
	}
	if !retrievedJob2.CreatedAt.After(backdated) {
		t.Errorf("Expected job2 created_at to be refreshed past backdated time %v, got %v", backdated, retrievedJob2.CreatedAt)
	}

	// Verify job3 is now pending and unassigned
	retrievedJob3, err := database.GetJob(job3.ID)
	if err != nil {
		t.Fatalf("Failed to get job3: %v", err)
	}
	if retrievedJob3.Status != protocol.JobStatusPending {
		t.Errorf("Expected job3 status 'pending', got '%s'", retrievedJob3.Status)
	}
	if retrievedJob3.WorkerID.Valid {
		t.Errorf("Expected job3 worker_id to be NULL, got '%s'", retrievedJob3.WorkerID.String)
	}

	// Verify job4 is still completed
	retrievedJob4, err := database.GetJob(job4.ID)
	if err != nil {
		t.Fatalf("Failed to get job4: %v", err)
	}
	if retrievedJob4.Status != protocol.JobStatusCompleted {
		t.Errorf("Expected job4 status 'completed', got '%s'", retrievedJob4.Status)
	}

	// Verify workers are offline
	retrievedWorker1, err := database.GetWorker(worker1.ID)
	if err != nil {
		t.Fatalf("Failed to get worker1: %v", err)
	}
	if retrievedWorker1.Status != protocol.WorkerStatusOffline {
		t.Errorf("Expected worker1 status 'offline', got '%s'", retrievedWorker1.Status)
	}

	retrievedWorker2, err := database.GetWorker(worker2.ID)
	if err != nil {
		t.Fatalf("Failed to get worker2: %v", err)
	}
	if retrievedWorker2.Status != protocol.WorkerStatusOffline {
		t.Errorf("Expected worker2 status 'offline', got '%s'", retrievedWorker2.Status)
	}
}

func TestCreateOrUpdateWorker(t *testing.T) {
	database, cleanup := setupDBTest(t)
	defer cleanup()

	caps := protocol.WorkerCapabilities{
		Encoders:      []string{"libx264"},
		FFmpegVersion: "5.1.2",
	}

	// Create a worker first
	worker1, err := database.CreateWorker("worker-1", "test-worker", caps)
	if err != nil {
		t.Fatalf("Failed to create worker: %v", err)
	}
	if worker1.Status != protocol.WorkerStatusIdle {
		t.Errorf("Expected status idle, got %s", worker1.Status)
	}

	// Mark worker as offline (simulating server restart recovery)
	err = database.UpdateWorkerStatus(worker1.ID, protocol.WorkerStatusOffline)
	if err != nil {
		t.Fatalf("Failed to mark worker offline: %v", err)
	}

	// Now re-register with updated capabilities
	updatedCaps := protocol.WorkerCapabilities{
		Encoders:      []string{"libx264", "h264_nvenc"},
		FFmpegVersion: "6.0.0",
	}
	worker2, err := database.CreateOrUpdateWorker(worker1.ID, "test-worker", updatedCaps, 90*time.Second)
	if err != nil {
		t.Fatalf("Failed to upsert worker: %v", err)
	}

	// Should be the same ID
	if worker2.ID != worker1.ID {
		t.Errorf("Expected same worker ID, got %s vs %s", worker1.ID, worker2.ID)
	}

	// Status should be reset to idle
	if worker2.Status != protocol.WorkerStatusIdle {
		t.Errorf("Expected status idle after re-registration, got %s", worker2.Status)
	}

	// Capabilities should be updated
	if worker2.Encoders != `["libx264","h264_nvenc"]` {
		t.Errorf("Expected updated encoders, got %s", worker2.Encoders)
	}
	if worker2.FFmpegVersion != "6.0.0" {
		t.Errorf("Expected ffmpeg version 6.0.0, got %s", worker2.FFmpegVersion)
	}

	// Evicted flag should be cleared
	if worker2.Evicted {
		t.Error("Expected evicted to be false after re-registration")
	}

	// Test creating a brand new worker with CreateOrUpdateWorker
	worker3, err := database.CreateOrUpdateWorker("", "new-worker", caps, 90*time.Second)
	if err != nil {
		t.Fatalf("Failed to create new worker via upsert: %v", err)
	}
	if worker3.ID == "" {
		t.Error("Expected non-empty ID for new worker")
	}
	if worker3.Name != "new-worker" {
		t.Errorf("Expected name 'new-worker', got %s", worker3.Name)
	}
	if worker3.Status != protocol.WorkerStatusIdle {
		t.Errorf("Expected status idle for new worker, got %s", worker3.Status)
	}
}

// TSI-2844: startup cleanup is lazy — it removes only workers whose last
// heartbeat is stale (older than the threshold), preserving fresh-heartbeat
// workers that survived a restart.
func TestRemoveStaleWorkers(t *testing.T) {
	database, cleanup := setupDBTest(t)
	defer cleanup()

	caps := protocol.WorkerCapabilities{
		Encoders:      []string{"libx264"},
		FFmpegVersion: "5.1.2",
	}

	stale1, err := database.CreateWorker("stale-1", "stale-worker-1", caps)
	if err != nil {
		t.Fatalf("Failed to create stale worker 1: %v", err)
	}
	stale2, err := database.CreateWorker("stale-2", "stale-worker-2", caps)
	if err != nil {
		t.Fatalf("Failed to create stale worker 2: %v", err)
	}
	live, err := database.CreateWorker("live-1", "live-worker", caps)
	if err != nil {
		t.Fatalf("Failed to create live worker: %v", err)
	}

	// Backdate the two stale workers' heartbeats past the threshold; the
	// live worker's heartbeat is fresh.
	backdated := time.Now().Add(-10 * time.Minute)
	for _, id := range []string{stale1.ID, stale2.ID} {
		if _, err := database.GetDB().Exec(`UPDATE workers SET last_heartbeat = ? WHERE id = ?`, backdated, id); err != nil {
			t.Fatalf("Failed to backdate %s heartbeat: %v", id, err)
		}
	}

	removed, err := database.RemoveStaleWorkers(2 * time.Minute)
	if err != nil {
		t.Fatalf("Failed to remove stale workers: %v", err)
	}
	if removed != 2 {
		t.Errorf("Expected 2 stale workers removed, got %d", removed)
	}

	if _, err := database.GetWorker(stale1.ID); err != protocol.ErrWorkerNotFound {
		t.Errorf("Expected stale worker 1 to be gone, got %v", err)
	}
	if _, err := database.GetWorker(stale2.ID); err != protocol.ErrWorkerNotFound {
		t.Errorf("Expected stale worker 2 to be gone, got %v", err)
	}

	// Fresh-heartbeat workers are untouched.
	if _, err := database.GetWorker(live.ID); err != nil {
		t.Errorf("Expected live worker to survive cleanup, got %v", err)
	}

	// Idempotent: a second sweep removes nothing.
	removed, err = database.RemoveStaleWorkers(2 * time.Minute)
	if err != nil {
		t.Fatalf("Second sweep failed: %v", err)
	}
	if removed != 0 {
		t.Errorf("Expected 0 removals on second sweep, got %d", removed)
	}
}

// TSI-2366 end-to-end residue scenario: register two workers, restart the
// server (RecoverState marks everything offline), then only one worker comes
// back. The absent worker's record — whose heartbeat has gone stale — must be
// gone after the lazy cleanup (TSI-2844); the returning worker's fresh row
// survives.
func TestRecoverStateRemovesStaleOfflineRecords(t *testing.T) {
	database, cleanup := setupDBTest(t)
	defer cleanup()

	caps := protocol.WorkerCapabilities{
		Encoders:      []string{"libx264"},
		FFmpegVersion: "5.1.2",
	}

	returning, err := database.CreateOrUpdateWorker("worker-returning", "returning-worker", caps, 90*time.Second)
	if err != nil {
		t.Fatalf("Failed to register returning worker: %v", err)
	}
	gone, err := database.CreateOrUpdateWorker("worker-gone", "gone-worker", caps, 90*time.Second)
	if err != nil {
		t.Fatalf("Failed to register departing worker: %v", err)
	}

	// Simulated restart: RecoverState marks all workers offline.
	if _, _, err := database.RecoverState(); err != nil {
		t.Fatalf("RecoverState failed: %v", err)
	}

	// The returning worker re-registers and is recreated as idle with a fresh
	// heartbeat; the absent worker's heartbeat is backdated so the lazy
	// cleanup treats it as stale residue.
	if _, err := database.CreateOrUpdateWorker(returning.ID, "returning-worker", caps, 90*time.Second); err != nil {
		t.Fatalf("Failed to re-register returning worker: %v", err)
	}
	backdated := time.Now().Add(-10 * time.Minute)
	if _, err := database.GetDB().Exec(`UPDATE workers SET last_heartbeat = ? WHERE id = ?`, backdated, gone.ID); err != nil {
		t.Fatalf("Failed to backdate departing worker heartbeat: %v", err)
	}

	removed, err := database.RemoveStaleWorkers(135 * time.Second)
	if err != nil {
		t.Fatalf("Failed to remove stale workers: %v", err)
	}
	if removed != 1 {
		t.Errorf("Expected 1 stale record removed, got %d", removed)
	}

	all, err := database.GetAllWorkers()
	if err != nil {
		t.Fatalf("Failed to list workers: %v", err)
	}
	if len(all) != 1 || all[0].ID != returning.ID {
		t.Errorf("Expected exactly the returning worker %+v, got %d rows", returning, len(all))
	}
}

// TSI-2473: a worker process restart generates a fresh UUID while reusing
// the same name. The old row (offline residue) must be overwritten, not
// duplicated — otherwise each restart accumulates a same-name entry.
func TestCreateOrUpdateWorker_SameNameOverwritesStaleRow(t *testing.T) {
	database, cleanup := setupDBTest(t)
	defer cleanup()

	caps := protocol.WorkerCapabilities{
		Encoders:      []string{"libx264"},
		FFmpegVersion: "5.1.2",
	}

	// First registration: worker process A with id-A and name "node-1".
	workerA, err := database.CreateOrUpdateWorker("id-A", "node-1", caps, 90*time.Second)
	if err != nil {
		t.Fatalf("Failed to register worker A: %v", err)
	}

	// Simulate server restart: RecoverState marks the worker offline.
	if _, _, err := database.RecoverState(); err != nil {
		t.Fatalf("RecoverState failed: %v", err)
	}

	// Worker process restarted: new UUID (id-B), same name "node-1".
	workerB, err := database.CreateOrUpdateWorker("id-B", "node-1", caps, 90*time.Second)
	if err != nil {
		t.Fatalf("Failed to re-register worker B with same name: %v", err)
	}
	if workerB.ID != "id-B" {
		t.Errorf("Expected worker B id id-B, got %s", workerB.ID)
	}

	// Exactly one row for "node-1" must exist — the stale id-A row is gone.
	all, err := database.GetAllWorkers()
	if err != nil {
		t.Fatalf("Failed to list workers: %v", err)
	}
	var sameName []*db.Worker
	for _, w := range all {
		if w.Name == "node-1" {
			sameName = append(sameName, w)
		}
	}
	if len(sameName) != 1 {
		t.Fatalf("Expected exactly 1 worker named node-1, got %d", len(sameName))
	}
	if sameName[0].ID != "id-B" {
		t.Errorf("Expected surviving row id-B, got %s", sameName[0].ID)
	}

	// The old id-A row must be gone.
	if _, err := database.GetWorker(workerA.ID); err != protocol.ErrWorkerNotFound {
		t.Errorf("Expected old worker A to be deleted, got %v", err)
	}
}

// TSI-2473: when name is empty, the DELETE guard must be skipped so an
// empty-name registration does not wipe other empty-name rows.
func TestCreateOrUpdateWorker_EmptyNameSkipsDelete(t *testing.T) {
	database, cleanup := setupDBTest(t)
	defer cleanup()

	caps := protocol.WorkerCapabilities{
		Encoders:      []string{"libx264"},
		FFmpegVersion: "5.1.2",
	}

	// Two workers with empty names (pre-existing, defensive).
	if _, err := database.CreateWorker("empty-1", "", caps); err != nil {
		t.Fatalf("Failed to create empty-1: %v", err)
	}
	if _, err := database.CreateWorker("empty-2", "", caps); err != nil {
		t.Fatalf("Failed to create empty-2: %v", err)
	}

	// A third empty-name registration must NOT delete the existing rows.
	worker3, err := database.CreateOrUpdateWorker("empty-3", "", caps, 90*time.Second)
	if err != nil {
		t.Fatalf("Failed to register empty-3: %v", err)
	}

	// All three rows survive — empty name does not trigger the same-name DELETE.
	for _, id := range []string{"empty-1", "empty-2", "empty-3"} {
		if _, err := database.GetWorker(id); err != nil {
			t.Errorf("Expected %s to survive empty-name registration, got %v", id, err)
		}
	}
	_ = worker3
}

// TSI-2670: two concurrently running workers may legitimately share a name
// while holding distinct IDs. The same-name DELETE in CreateOrUpdateWorker
// must only remove stale offline residue, never a live (idle/busy) row —
// otherwise each registration deletes the other live worker and they ping-pong
// between 404 and re-register every poll cycle.
func TestCreateOrUpdateWorker_SameNameKeepsLiveRows(t *testing.T) {
	database, cleanup := setupDBTest(t)
	defer cleanup()

	caps := protocol.WorkerCapabilities{
		Encoders:      []string{"libx264"},
		FFmpegVersion: "5.1.2",
	}

	// Two live workers, same name "node-1", distinct IDs.
	workerA, err := database.CreateOrUpdateWorker("id-A", "node-1", caps, 90*time.Second)
	if err != nil {
		t.Fatalf("Failed to register worker A: %v", err)
	}
	workerB, err := database.CreateOrUpdateWorker("id-B", "node-1", caps, 90*time.Second)
	if err != nil {
		t.Fatalf("Failed to register worker B: %v", err)
	}

	// Both live rows must survive: the second registration must not delete
	// the first live worker's row.
	for _, id := range []string{workerA.ID, workerB.ID} {
		if _, err := database.GetWorker(id); err != nil {
			t.Errorf("Expected live worker %s to survive same-name registration, got %v", id, err)
		}
	}

	// Re-registration of either live worker (the poll-cycle upsert) must keep
	// the other live row intact as well.
	if _, err := database.CreateOrUpdateWorker(workerA.ID, "node-1", caps, 90*time.Second); err != nil {
		t.Fatalf("Failed to re-register worker A: %v", err)
	}
	if _, err := database.GetWorker(workerB.ID); err != nil {
		t.Errorf("Expected live worker B to survive worker A re-registration, got %v", err)
	}

	all, err := database.GetAllWorkers()
	if err != nil {
		t.Fatalf("Failed to list workers: %v", err)
	}
	var sameName []*db.Worker
	for _, w := range all {
		if w.Name == "node-1" {
			sameName = append(sameName, w)
		}
	}
	if len(sameName) != 2 {
		t.Errorf("Expected 2 live workers named node-1, got %d", len(sameName))
	}
}

// TSI-2670 review follow-up: a crashed worker's stale live row (idle/busy
// with a heartbeat older than the freshness window) must be swept on same-name
// re-registration, while a genuinely live same-name row with a fresh heartbeat
// survives. This closes the window where a dead worker's row kept receiving
// scheduler dispatches until the health monitor's next tick.
func TestCreateOrUpdateWorker_SameNameRemovesExpiredLiveRow(t *testing.T) {
	database, cleanup := setupDBTest(t)
	defer cleanup()

	caps := protocol.WorkerCapabilities{
		Encoders:      []string{"libx264"},
		FFmpegVersion: "5.1.2",
	}
	const heartbeatTimeout = 90 * time.Second

	// Two live same-name workers, distinct IDs.
	workerA, err := database.CreateOrUpdateWorker("id-A", "node-1", caps, heartbeatTimeout)
	if err != nil {
		t.Fatalf("Failed to register worker A: %v", err)
	}
	workerB, err := database.CreateOrUpdateWorker("id-B", "node-1", caps, heartbeatTimeout)
	if err != nil {
		t.Fatalf("Failed to register worker B: %v", err)
	}

	// Simulate worker B crashing: its row stays live (idle) but its heartbeat
	// goes stale past the freshness window before the health monitor tick.
	if _, err := database.GetDB().Exec(
		"UPDATE workers SET last_heartbeat = ? WHERE id = ?",
		time.Now().Add(-2*time.Minute), workerB.ID,
	); err != nil {
		t.Fatalf("Failed to backdate worker B heartbeat: %v", err)
	}

	// A restarted worker process re-registers under the same name with a new ID.
	workerC, err := database.CreateOrUpdateWorker("id-C", "node-1", caps, heartbeatTimeout)
	if err != nil {
		t.Fatalf("Failed to re-register worker C: %v", err)
	}

	// The live row (A) and the new row (C) survive; the expired-live row (B) is gone.
	if _, err := database.GetWorker(workerA.ID); err != nil {
		t.Errorf("Expected live worker A to survive, got %v", err)
	}
	if _, err := database.GetWorker(workerB.ID); err == nil {
		t.Error("Expected expired-live worker B to be removed, but it still exists")
	}
	if _, err := database.GetWorker(workerC.ID); err != nil {
		t.Errorf("Expected re-registered worker C to exist, got %v", err)
	}

	all, err := database.GetAllWorkers()
	if err != nil {
		t.Fatalf("Failed to list workers: %v", err)
	}
	var sameName []*db.Worker
	for _, w := range all {
		if w.Name == "node-1" {
			sameName = append(sameName, w)
		}
	}
	if len(sameName) != 2 {
		t.Errorf("Expected 2 workers named node-1 (A + C), got %d", len(sameName))
	}
}

// TSI-2366 companion fix: heartbeats and status updates from a UUID reported
// in non-canonical format (compact/hyphenless, uppercase) must reach the
// canonical row instead of silently matching nothing.
func TestHeartbeatAndStatusNormalizeUUID(t *testing.T) {
	database, cleanup := setupDBTest(t)
	defer cleanup()

	hyphenated := "550e8400-e29b-41d4-a716-446655440000"
	compact := strings.ToUpper("550e8400e29b41d4a716446655440000")

	if _, err := database.CreateOrUpdateWorker(hyphenated, "uuid-worker", protocol.WorkerCapabilities{
		Encoders:      []string{"libx264"},
		FFmpegVersion: "5.1.2",
	}, 90*time.Second); err != nil {
		t.Fatalf("Failed to create worker: %v", err)
	}

	// Heartbeats no longer write status (PR #24: status is derived state);
	// the compact-format heartbeat must still resolve to the canonical row.
	if err := database.UpdateWorkerHeartbeat(compact, protocol.WorkerStatusBusy); err != nil {
		t.Fatalf("Compact-format heartbeat rejected: %v", err)
	}
	worker, err := database.GetWorker(hyphenated)
	if err != nil {
		t.Fatalf("Failed to get worker: %v", err)
	}

	if err := database.UpdateWorkerStatus(compact, protocol.WorkerStatusIdle); err != nil {
		t.Fatalf("Compact-format status update rejected: %v", err)
	}
	worker, err = database.GetWorker(hyphenated)
	if err != nil {
		t.Fatalf("Failed to get worker: %v", err)
	}
	if worker.Status != protocol.WorkerStatusIdle {
		t.Errorf("Expected status idle from compact update, got %s", worker.Status)
	}

	if err := database.MarkWorkerEvicted(compact); err != nil {
		t.Fatalf("Compact-format eviction rejected: %v", err)
	}
	evicted, err := database.IsWorkerEvicted(compact)
	if err != nil {
		t.Fatalf("Failed to check eviction: %v", err)
	}
	if !evicted {
		t.Error("Expected worker evicted via compact ID")
	}
	if err := database.ClearWorkerEviction(compact); err != nil {
		t.Fatalf("Compact-format eviction clear rejected: %v", err)
	}
}

// TSI-2366 review follow-up: job read paths (pull queue, running-job lookup)
// and UpdateWorkerCapabilities must also resolve non-canonical UUID formats,
// otherwise a compact-format worker passes existence checks but silently
// matches zero rows.
func TestJobReadPathsNormalizeUUID(t *testing.T) {
	database, cleanup := setupDBTest(t)
	defer cleanup()

	hyphenated := "550e8400-e29b-41d4-a716-446655440000"
	compact := strings.ToUpper("550e8400e29b41d4a716446655440000")

	if _, err := database.CreateOrUpdateWorker(hyphenated, "uuid-worker", protocol.WorkerCapabilities{
		Encoders:      []string{"libx264"},
		FFmpegVersion: "5.1.2",
	}, 90*time.Second); err != nil {
		t.Fatalf("Failed to create worker: %v", err)
	}

	job, err := database.CreateJob(`["in.mkv"]`, `["-c:v","libx264"]`, "out.mkv", false)
	if err != nil {
		t.Fatalf("Failed to create job: %v", err)
	}
	if _, err := database.AssignPendingJobsToWorker(compact, 10); err != nil {
		t.Fatalf("Compact-format pull failed: %v", err)
	}

	// The compact-format pull must have claimed the pending job under the
	// canonical ID and marked the worker busy.
	pulled, err := database.GetJobsForWorker(compact, 10)
	if err != nil {
		t.Fatalf("Compact-format GetJobsForWorker failed: %v", err)
	}
	if len(pulled) != 1 || pulled[0].ID != job.ID {
		t.Errorf("Expected queued job %s via compact ID, got %d jobs", job.ID, len(pulled))
	}

	count, err := database.GetWorkerActiveJobCount(hyphenated)
	if err != nil {
		t.Fatalf("Failed to get active job count: %v", err)
	}
	if count != 1 {
		t.Errorf("Expected 1 active job on canonical row, got %d", count)
	}

	if err := database.UpdateJobStatusWithFailure(job.ID, protocol.JobStatusRunning, nil, nil, nil, nil); err != nil {
		t.Fatalf("Failed to set job running: %v", err)
	}
	running, err := database.GetRunningJobsByWorker(compact)
	if err != nil {
		t.Fatalf("Compact-format GetRunningJobsByWorker failed: %v", err)
	}
	if len(running) != 1 || running[0].ID != job.ID {
		t.Errorf("Expected running job %s via compact ID, got %d jobs", job.ID, len(running))
	}

	caps := protocol.WorkerCapabilities{
		Encoders:      []string{"h264_nvenc"},
		FFmpegVersion: "6.0",
	}
	if err := database.UpdateWorkerCapabilities(compact, caps); err != nil {
		t.Fatalf("Compact-format UpdateWorkerCapabilities failed: %v", err)
	}
	worker, err := database.GetWorker(hyphenated)
	if err != nil {
		t.Fatalf("Failed to get worker: %v", err)
	}
	if worker.Encoders != `["h264_nvenc"]` || worker.FFmpegVersion != "6.0" {
		t.Errorf("Capabilities not applied via compact ID: encoders=%s ffmpeg=%s", worker.Encoders, worker.FFmpegVersion)
	}
}

func TestGetJobsByStatus(t *testing.T) {
	database, cleanup := setupDBTest(t)
	defer cleanup()

	inputFiles := `["file1.mp4"]`
	args := `["-c:v", "libx264"]`
	outputFilename := "output.mp4"

	// Create multiple jobs
	job1, err := database.CreateJob(inputFiles, args, outputFilename, false)
	if err != nil {
		t.Fatalf("Failed to create job1: %v", err)
	}

	job2, err := database.CreateJob(inputFiles, args, outputFilename, false)
	if err != nil {
		t.Fatalf("Failed to create job2: %v", err)
	}

	job3, err := database.CreateJob(inputFiles, args, outputFilename, false)
	if err != nil {
		t.Fatalf("Failed to create job3: %v", err)
	}

	// Mark one as completed
	err = database.UpdateJobStatusWithFailure(job2.ID, protocol.JobStatusCompleted, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("Failed to update job2: %v", err)
	}

	// Mark one as failed
	err = database.UpdateJobStatusWithFailure(job3.ID, protocol.JobStatusFailed, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("Failed to update job3: %v", err)
	}

	// Get pending jobs
	pendingJobs, err := database.GetJobsByStatus(protocol.JobStatusPending, 10)
	if err != nil {
		t.Fatalf("Failed to get pending jobs: %v", err)
	}

	if len(pendingJobs) != 1 {
		t.Errorf("Expected 1 pending job, got %d", len(pendingJobs))
	}

	if pendingJobs[0].ID != job1.ID {
		t.Errorf("Expected pending job ID '%s', got '%s'", job1.ID, pendingJobs[0].ID)
	}

	// Get completed jobs
	completedJobs, err := database.GetJobsByStatus(protocol.JobStatusCompleted, 10)
	if err != nil {
		t.Fatalf("Failed to get completed jobs: %v", err)
	}

	if len(completedJobs) != 1 {
		t.Errorf("Expected 1 completed job, got %d", len(completedJobs))
	}

	// Get failed jobs
	failedJobs, err := database.GetJobsByStatus(protocol.JobStatusFailed, 10)
	if err != nil {
		t.Fatalf("Failed to get failed jobs: %v", err)
	}

	if len(failedJobs) != 1 {
		t.Errorf("Expected 1 failed job, got %d", len(failedJobs))
	}
}

func TestGetAllWorkers(t *testing.T) {
	database, cleanup := setupDBTest(t)
	defer cleanup()

	caps := protocol.WorkerCapabilities{
		Encoders:      []string{"libx264"},
		FFmpegVersion: "5.1.2",
	}

	// Create multiple workers
	worker1, err := database.CreateWorker("", "worker-1", caps)
	if err != nil {
		t.Fatalf("Failed to create worker1: %v", err)
	}

	worker2, err := database.CreateWorker("", "worker-2", caps)
	if err != nil {
		t.Fatalf("Failed to create worker2: %v", err)
	}

	// Get all workers
	workers, err := database.GetAllWorkers()
	if err != nil {
		t.Fatalf("Failed to get all workers: %v", err)
	}

	if len(workers) != 2 {
		t.Errorf("Expected 2 workers, got %d", len(workers))
	}

	// Verify worker IDs
	workerIDs := make(map[string]bool)
	for _, w := range workers {
		workerIDs[w.ID] = true
	}

	if !workerIDs[worker1.ID] || !workerIDs[worker2.ID] {
		t.Errorf("Expected to find both worker1 and worker2 in results")
	}
}

// TestAssignPendingJobsToWorkerWithSchedulerRaceCondition tests that workers can pull
// jobs that were already assigned to them by the scheduler (queued status)
func TestAssignPendingJobsToWorkerWithSchedulerRaceCondition(t *testing.T) {
	database, cleanup := setupDBTest(t)
	defer cleanup()

	caps := protocol.WorkerCapabilities{
		Encoders:      []string{"libx264"},
		FFmpegVersion: "5.1.2",
	}

	// Create worker
	worker, err := database.CreateWorker("", "test-worker", caps)
	if err != nil {
		t.Fatalf("Failed to create worker: %v", err)
	}

	inputFiles := `["file1.mp4"]`
	args := `["-c:v", "libx264"]`
	outputFilename := "output.mp4"

	// Create a job
	job, err := database.CreateJob(inputFiles, args, outputFilename, false)
	if err != nil {
		t.Fatalf("Failed to create job: %v", err)
	}

	// Simulate the scheduler assigning the job to the worker
	// (sets worker_id and status to queued)
	err = database.AssignJobToWorker(job.ID, worker.ID)
	if err != nil {
		t.Fatalf("Failed to assign job to worker: %v", err)
	}
	err = database.UpdateJobStatusWithFailure(job.ID, protocol.JobStatusQueued, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("Failed to update job status to queued: %v", err)
	}

	// Verify the job is now queued and assigned to the worker
	queuedJob, err := database.GetJob(job.ID)
	if err != nil {
		t.Fatalf("Failed to get job: %v", err)
	}
	if queuedJob.Status != protocol.JobStatusQueued {
		t.Errorf("Expected job status 'queued', got '%s'", queuedJob.Status)
	}
	if !queuedJob.WorkerID.Valid || queuedJob.WorkerID.String != worker.ID {
		t.Errorf("Expected job worker_id to be '%s', got '%v'", worker.ID, queuedJob.WorkerID)
	}

	// Now simulate the worker polling for jobs
	// This should return the job that was already assigned by the scheduler
	jobs, err := database.AssignPendingJobsToWorker(worker.ID, 10)
	if err != nil {
		t.Fatalf("Failed to get jobs for worker: %v", err)
	}

	// Verify the worker received the job
	if len(jobs) != 1 {
		t.Fatalf("Expected 1 job, got %d", len(jobs))
	}

	if jobs[0].ID != job.ID {
		t.Errorf("Expected job ID '%s', got '%s'", job.ID, jobs[0].ID)
	}

	if jobs[0].Status != protocol.JobStatusQueued {
		t.Errorf("Expected job status 'queued', got '%s'", jobs[0].Status)
	}

	if jobs[0].WorkerID.String != worker.ID {
		t.Errorf("Expected job worker_id to be '%s', got '%s'", worker.ID, jobs[0].WorkerID.String)
	}
}

// TestGetAllHwaccels tests aggregation of hwaccels across workers
func TestGetAllHwaccels(t *testing.T) {
	database, cleanup := setupDBTest(t)
	defer cleanup()

	// Worker 1: has hwaccels
	caps1 := protocol.WorkerCapabilities{
		Encoders:      []string{"libx264"},
		FFmpegVersion: "5.1.2",
		Hwaccels: `Hardware acceleration methods:
cuda
vaapi
qsv
drm`,
	}
	_, err := database.CreateWorker("", "worker-1", caps1)
	if err != nil {
		t.Fatalf("Failed to create worker1: %v", err)
	}

	// Worker 2: has different hwaccels
	caps2 := protocol.WorkerCapabilities{
		Encoders:      []string{"libx264"},
		FFmpegVersion: "5.1.2",
		Hwaccels: `Hardware acceleration methods:
vaapi
vulkan
opencl`,
	}
	_, err = database.CreateWorker("", "worker-2", caps2)
	if err != nil {
		t.Fatalf("Failed to create worker2: %v", err)
	}

	hwaccels, err := database.GetAllHwaccels()
	if err != nil {
		t.Fatalf("GetAllHwaccels failed: %v", err)
	}

	// Expect 5 unique: cuda, vaapi, qsv, drm, vulkan, opencl
	if len(hwaccels) != 6 {
		t.Errorf("Expected 6 unique hwaccels, got %d: %v", len(hwaccels), hwaccels)
	}

	expected := map[string]bool{
		"cuda": true, "vaapi": true, "qsv": true, "drm": true, "vulkan": true, "opencl": true,
	}
	for _, h := range hwaccels {
		if !expected[h] {
			t.Errorf("Unexpected hwaccel: %s", h)
		}
	}
}

// TestGetAllCodecs tests aggregation of codecs across workers
func TestGetAllCodecs(t *testing.T) {
	database, cleanup := setupDBTest(t)
	defer cleanup()

	caps1 := protocol.WorkerCapabilities{
		Encoders:      []string{"libx264"},
		FFmpegVersion: "5.1.2",
		Codecs: `Codecs:
 D..... = Decoding supported
 .E.... = Encoding supported
 ------
 D.VI.S h264                 H.264 / AVC / MPEG-4 AVC / MPEG-4 part 10
 DEV.LS hevc_nvenc           NVIDIA NVENC HEVC encoder`,
	}
	_, err := database.CreateWorker("", "worker-1", caps1)
	if err != nil {
		t.Fatalf("Failed to create worker1: %v", err)
	}

	caps2 := protocol.WorkerCapabilities{
		Encoders:      []string{"libx264"},
		FFmpegVersion: "5.1.2",
		Codecs: `Codecs:
 ------
 D.VI.S h264                 H.264 / AVC
 DEV.LS av1_nvenc            NVIDIA NVENC AV1 encoder`,
	}
	_, err = database.CreateWorker("", "worker-2", caps2)
	if err != nil {
		t.Fatalf("Failed to create worker2: %v", err)
	}

	codecs, err := database.GetAllCodecs()
	if err != nil {
		t.Fatalf("GetAllCodecs failed: %v", err)
	}

	// Expect 3 unique: h264, hevc_nvenc, av1_nvenc
	if len(codecs) != 3 {
		t.Errorf("Expected 3 unique codecs, got %d: %v", len(codecs), codecs)
	}

	expected := map[string]bool{"h264": true, "hevc_nvenc": true, "av1_nvenc": true}
	for _, c := range codecs {
		if !expected[c] {
			t.Errorf("Unexpected codec: %s", c)
		}
	}
}

// TestGetAllFilters tests aggregation of filters across workers
func TestGetAllFilters(t *testing.T) {
	database, cleanup := setupDBTest(t)
	defer cleanup()

	caps1 := protocol.WorkerCapabilities{
		Encoders:      []string{"libx264"},
		FFmpegVersion: "5.1.2",
		Filters: `Filters:
 T.. = Timeline support
 ------
 TS. scale             V->V       Scale the input video
 T.C yadif             V->V       Deinterlace the input image`,
	}
	_, err := database.CreateWorker("", "worker-1", caps1)
	if err != nil {
		t.Fatalf("Failed to create worker1: %v", err)
	}

	caps2 := protocol.WorkerCapabilities{
		Encoders:      []string{"libx264"},
		FFmpegVersion: "5.1.2",
		Filters: `Filters:
 ------
 TSC crop              V->V       Crop the input video
 T.C yadif             V->V       Deinterlace`,
	}
	_, err = database.CreateWorker("", "worker-2", caps2)
	if err != nil {
		t.Fatalf("Failed to create worker2: %v", err)
	}

	filters, err := database.GetAllFilters()
	if err != nil {
		t.Fatalf("GetAllFilters failed: %v", err)
	}

	// Expect 3 unique: scale, yadif, crop
	if len(filters) != 3 {
		t.Errorf("Expected 3 unique filters, got %d: %v", len(filters), filters)
	}

	expected := map[string]bool{"scale": true, "yadif": true, "crop": true}
	for _, f := range filters {
		if !expected[f] {
			t.Errorf("Unexpected filter: %s", f)
		}
	}
}

// TestGetAllPixFmts tests aggregation of pixel formats across workers
func TestGetAllPixFmts(t *testing.T) {
	database, cleanup := setupDBTest(t)
	defer cleanup()

	caps1 := protocol.WorkerCapabilities{
		Encoders:      []string{"libx264"},
		FFmpegVersion: "5.1.2",
		PixFmts: `Pixel formats:
I.... = Supported Input
.O... = Supported Output
-----
IO... yuv420p               3            12
IO... yuv422p               3            16`,
	}
	_, err := database.CreateWorker("", "worker-1", caps1)
	if err != nil {
		t.Fatalf("Failed to create worker1: %v", err)
	}

	caps2 := protocol.WorkerCapabilities{
		Encoders:      []string{"libx264"},
		FFmpegVersion: "5.1.2",
		PixFmts: `Pixel formats:
-----
IO... yuv420p               3            12
IO... nv12                  2            12`,
	}
	_, err = database.CreateWorker("", "worker-2", caps2)
	if err != nil {
		t.Fatalf("Failed to create worker2: %v", err)
	}

	pixFmts, err := database.GetAllPixFmts()
	if err != nil {
		t.Fatalf("GetAllPixFmts failed: %v", err)
	}

	// Expect 3 unique: yuv420p, yuv422p, nv12
	if len(pixFmts) != 3 {
		t.Errorf("Expected 3 unique pix_fmts, got %d: %v", len(pixFmts), pixFmts)
	}

	expected := map[string]bool{"yuv420p": true, "yuv422p": true, "nv12": true}
	for _, p := range pixFmts {
		if !expected[p] {
			t.Errorf("Unexpected pix_fmt: %s", p)
		}
	}
}

// TestGetAllFormats tests aggregation of formats across workers
func TestGetAllFormats(t *testing.T) {
	database, cleanup := setupDBTest(t)
	defer cleanup()

	caps1 := protocol.WorkerCapabilities{
		Encoders:      []string{"libx264"},
		FFmpegVersion: "5.1.2",
		Formats: `File formats:
 D. = Demuxing supported
 .E = Muxing supported
 --
 DE mp4             MP4 (MPEG-4 Part 14)
  E matroska        Matroska`,
	}
	_, err := database.CreateWorker("", "worker-1", caps1)
	if err != nil {
		t.Fatalf("Failed to create worker1: %v", err)
	}

	caps2 := protocol.WorkerCapabilities{
		Encoders:      []string{"libx264"},
		FFmpegVersion: "5.1.2",
		Formats: `File formats:
 --
 DE mp4             MP4
 DE webm            WebM`,
	}
	_, err = database.CreateWorker("", "worker-2", caps2)
	if err != nil {
		t.Fatalf("Failed to create worker2: %v", err)
	}

	formats, err := database.GetAllFormats()
	if err != nil {
		t.Fatalf("GetAllFormats failed: %v", err)
	}

	// Expect 3 unique: mp4, matroska, webm
	if len(formats) != 3 {
		t.Errorf("Expected 3 unique formats, got %d: %v", len(formats), formats)
	}

	expected := map[string]bool{"mp4": true, "matroska": true, "webm": true}
	for _, f := range formats {
		if !expected[f] {
			t.Errorf("Unexpected format: %s", f)
		}
	}
}

// TestGetAllInfoFlagsEmptyWorkers tests that aggregation functions handle empty fields
func TestGetAllInfoFlagsEmptyWorkers(t *testing.T) {
	database, cleanup := setupDBTest(t)
	defer cleanup()

	caps := protocol.WorkerCapabilities{
		Encoders:      []string{"libx264"},
		FFmpegVersion: "5.1.2",
	}
	_, err := database.CreateWorker("", "worker-no-flags", caps)
	if err != nil {
		t.Fatalf("Failed to create worker: %v", err)
	}

	hwaccels, err := database.GetAllHwaccels()
	if err != nil {
		t.Fatalf("GetAllHwaccels failed: %v", err)
	}
	if len(hwaccels) != 0 {
		t.Errorf("Expected 0 hwaccels, got %d", len(hwaccels))
	}

	codecs, err := database.GetAllCodecs()
	if err != nil {
		t.Fatalf("GetAllCodecs failed: %v", err)
	}
	if len(codecs) != 0 {
		t.Errorf("Expected 0 codecs, got %d", len(codecs))
	}

	filters, err := database.GetAllFilters()
	if err != nil {
		t.Fatalf("GetAllFilters failed: %v", err)
	}
	if len(filters) != 0 {
		t.Errorf("Expected 0 filters, got %d", len(filters))
	}

	pixFmts, err := database.GetAllPixFmts()
	if err != nil {
		t.Fatalf("GetAllPixFmts failed: %v", err)
	}
	if len(pixFmts) != 0 {
		t.Errorf("Expected 0 pix_fmts, got %d", len(pixFmts))
	}

	formats, err := database.GetAllFormats()
	if err != nil {
		t.Fatalf("GetAllFormats failed: %v", err)
	}
	if len(formats) != 0 {
		t.Errorf("Expected 0 formats, got %d", len(formats))
	}
}
