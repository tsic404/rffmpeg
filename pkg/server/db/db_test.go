package db_test

import (
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/tsix404/rffmpeg/pkg/protocol"
	"github.com/tsix404/rffmpeg/pkg/server/db"
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

	// Update heartbeat to busy
	err = database.UpdateWorkerHeartbeat(worker.ID, protocol.WorkerStatusBusy)
	if err != nil {
		t.Fatalf("Failed to update heartbeat: %v", err)
	}

	// Verify status
	retrieved, err := database.GetWorker(worker.ID)
	if err != nil {
		t.Fatalf("Failed to get worker: %v", err)
	}

	if retrieved.Status != protocol.WorkerStatusBusy {
		t.Errorf("Expected status 'busy', got '%s'", retrieved.Status)
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

	// Simulate worker1 heartbeat being stale by waiting and sending heartbeat from worker2
	time.Sleep(100 * time.Millisecond)
	err = database.UpdateWorkerHeartbeat(worker2.ID, protocol.WorkerStatusIdle)
	if err != nil {
		t.Fatalf("Failed to update worker2 heartbeat: %v", err)
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
	removed, err := database.RemoveOfflineWorkers(50 * time.Millisecond)
	if err != nil {
		t.Fatalf("Failed to remove offline workers: %v", err)
	}

	if removed != 1 {
		t.Errorf("Expected 1 worker to be removed, got %d", removed)
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
	err = database.UpdateJobStatus(job2.ID, protocol.JobStatusRunning, nil, nil)
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
	err = database.UpdateJobStatus(job4.ID, protocol.JobStatusCompleted, nil, nil)
	if err != nil {
		t.Fatalf("Failed to set job4 as completed: %v", err)
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
	worker2, err := database.CreateOrUpdateWorker(worker1.ID, "test-worker", updatedCaps)
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
	worker3, err := database.CreateOrUpdateWorker("", "new-worker", caps)
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
	err = database.UpdateJobStatus(job2.ID, protocol.JobStatusCompleted, nil, nil)
	if err != nil {
		t.Fatalf("Failed to update job2: %v", err)
	}

	// Mark one as failed
	err = database.UpdateJobStatus(job3.ID, protocol.JobStatusFailed, nil, nil)
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
	err = database.UpdateJobStatus(job.ID, protocol.JobStatusQueued, nil, nil)
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
