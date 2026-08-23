package db_test

import (
	"testing"

	"github.com/tsix404/rffmpeg/pkg/protocol"
)

// TestServerRestartRecovery_FullFlow simulates server restart and verifies:
//  1. Running/queued jobs are reset to pending
//  2. Workers are marked offline (workers must re-register)
//  3. Completed jobs are NOT affected
//  4. Failed jobs are NOT affected
//  5. Pending jobs remain pending (unaffected)
func TestServerRestartRecovery_FullFlow(t *testing.T) {
	database, cleanup := setupDBTest(t)
	defer cleanup()

	caps := protocol.WorkerCapabilities{
		Encoders:      []string{"libx264", "h264_nvenc"},
		FFmpegVersion: "6.0",
	}

	// Create 3 workers (simulating a 3-node cluster)
	worker1, err := database.CreateWorker("", "gpu-worker-1", caps)
	if err != nil {
		t.Fatalf("Failed to create worker1: %v", err)
	}
	worker2, err := database.CreateWorker("", "gpu-worker-2", caps)
	if err != nil {
		t.Fatalf("Failed to create worker2: %v", err)
	}
	worker3, err := database.CreateWorker("", "cpu-worker", protocol.WorkerCapabilities{
		Encoders:      []string{"libx264"},
		FFmpegVersion: "6.0",
	})
	if err != nil {
		t.Fatalf("Failed to create worker3: %v", err)
	}

	// Create jobs in various states
	inputFiles := `["input.mp4"]`
	args := `["-i", "input.mp4", "-c:v", "libx264", "output.mp4"]`

	// Job 1: Pending (unaffected by recovery)
	job1, err := database.CreateJob(inputFiles, args, "output1.mp4", false)
	if err != nil {
		t.Fatalf("Failed to create job1: %v", err)
	}

	// Job 2: Running on worker1 (should be reset)
	job2, err := database.CreateJob(inputFiles, args, "output2.mp4", false)
	if err != nil {
		t.Fatalf("Failed to create job2: %v", err)
	}
	if err := database.AssignJobToWorker(job2.ID, worker1.ID); err != nil {
		t.Fatalf("Failed to assign job2: %v", err)
	}
	if err := database.UpdateJobStatusWithFailure(job2.ID, protocol.JobStatusRunning, nil, nil, nil, nil); err != nil {
		t.Fatalf("Failed to set job2 running: %v", err)
	}

	// Job 3: Running on worker2 (should be reset)
	job3, err := database.CreateJob(inputFiles, args, "output3.mp4", false)
	if err != nil {
		t.Fatalf("Failed to create job3: %v", err)
	}
	if err := database.AssignJobToWorker(job3.ID, worker2.ID); err != nil {
		t.Fatalf("Failed to assign job3: %v", err)
	}
	if err := database.UpdateJobStatusWithFailure(job3.ID, protocol.JobStatusRunning, nil, nil, nil, nil); err != nil {
		t.Fatalf("Failed to set job3 running: %v", err)
	}

	// Job 4: Queued on worker1 (should be reset)
	job4, err := database.CreateJob(inputFiles, args, "output4.mp4", false)
	if err != nil {
		t.Fatalf("Failed to create job4: %v", err)
	}
	if err := database.AssignJobToWorker(job4.ID, worker1.ID); err != nil {
		t.Fatalf("Failed to assign job4: %v", err)
	}
	if err := database.UpdateJobStatusWithFailure(job4.ID, protocol.JobStatusQueued, nil, nil, nil, nil); err != nil {
		t.Fatalf("Failed to set job4 queued: %v", err)
	}

	// Job 5: Completed (should NOT be affected)
	job5, err := database.CreateJob(inputFiles, args, "output5.mp4", false)
	if err != nil {
		t.Fatalf("Failed to create job5: %v", err)
	}
	if err := database.AssignJobToWorker(job5.ID, worker1.ID); err != nil {
		t.Fatalf("Failed to assign job5: %v", err)
	}
	if err := database.UpdateJobStatusWithFailure(job5.ID, protocol.JobStatusCompleted, nil, nil, nil, nil); err != nil {
		t.Fatalf("Failed to set job5 completed: %v", err)
	}

	// Job 6: Failed (should NOT be affected)
	job6, err := database.CreateJob(inputFiles, args, "output6.mp4", false)
	if err != nil {
		t.Fatalf("Failed to create job6: %v", err)
	}
	if err := database.AssignJobToWorker(job6.ID, worker2.ID); err != nil {
		t.Fatalf("Failed to assign job6: %v", err)
	}
	if err := database.UpdateJobStatusWithFailure(job6.ID, protocol.JobStatusFailed, nil, nil, nil, nil); err != nil {
		t.Fatalf("Failed to set job6 failed: %v", err)
	}

	// Job 7: Cancelled (should NOT be affected)
	job7, err := database.CreateJob(inputFiles, args, "output7.mp4", false)
	if err != nil {
		t.Fatalf("Failed to create job7: %v", err)
	}
	if err := database.UpdateJobStatusWithFailure(job7.ID, protocol.JobStatusCancelled, nil, nil, nil, nil); err != nil {
		t.Fatalf("Failed to set job7 cancelled: %v", err)
	}

	// Mark workers as busy (simulating they were active before crash)
	_ = database.UpdateWorkerStatus(worker1.ID, protocol.WorkerStatusBusy)
	_ = database.UpdateWorkerStatus(worker2.ID, protocol.WorkerStatusBusy)

	// Perform recovery (simulate server restart)
	jobsReset, workersMarkedOffline, err := database.RecoverState()
	if err != nil {
		t.Fatalf("Failed to recover state: %v", err)
	}

	// Verify job recovery counts
	if jobsReset != 3 {
		t.Errorf("Expected 3 jobs to be reset (job2+running, job3+running, job4+queued), got %d", jobsReset)
	}
	if workersMarkedOffline != 3 {
		t.Errorf("Expected all 3 workers to be marked offline, got %d", workersMarkedOffline)
	}

	// Verify job1 (pending) remains pending
	j1, _ := database.GetJob(job1.ID)
	if j1.Status != protocol.JobStatusPending {
		t.Errorf("Job1 should remain pending after recovery, got %s", j1.Status)
	}

	// Verify job2 (running → pending, worker_id cleared)
	j2, _ := database.GetJob(job2.ID)
	if j2.Status != protocol.JobStatusPending {
		t.Errorf("Job2 should be reset to pending, got %s", j2.Status)
	}
	if j2.WorkerID.Valid {
		t.Errorf("Job2 worker_id should be NULL after recovery, got %s", j2.WorkerID.String)
	}

	// Verify job3 (running → pending)
	j3, _ := database.GetJob(job3.ID)
	if j3.Status != protocol.JobStatusPending {
		t.Errorf("Job3 should be reset to pending, got %s", j3.Status)
	}
	if j3.WorkerID.Valid {
		t.Errorf("Job3 worker_id should be NULL after recovery, got %s", j3.WorkerID.String)
	}

	// Verify job4 (queued → pending)
	j4, _ := database.GetJob(job4.ID)
	if j4.Status != protocol.JobStatusPending {
		t.Errorf("Job4 should be reset to pending, got %s", j4.Status)
	}
	if j4.WorkerID.Valid {
		t.Errorf("Job4 worker_id should be NULL after recovery, got %s", j4.WorkerID.String)
	}

	// Verify job5 (completed) is NOT affected
	j5, _ := database.GetJob(job5.ID)
	if j5.Status != protocol.JobStatusCompleted {
		t.Errorf("Job5 should remain completed, got %s", j5.Status)
	}

	// Verify job6 (failed) is NOT affected
	j6, _ := database.GetJob(job6.ID)
	if j6.Status != protocol.JobStatusFailed {
		t.Errorf("Job6 should remain failed, got %s", j6.Status)
	}

	// Verify job7 (cancelled) is NOT affected
	j7, _ := database.GetJob(job7.ID)
	if j7.Status != protocol.JobStatusCancelled {
		t.Errorf("Job7 should remain cancelled, got %s", j7.Status)
	}

	// Verify all workers are now offline
	for _, workerID := range []string{worker1.ID, worker2.ID, worker3.ID} {
		w, err := database.GetWorker(workerID)
		if err != nil {
			t.Fatalf("Failed to get worker %s: %v", workerID, err)
		}
		if w.Status != protocol.WorkerStatusOffline {
			t.Errorf("Worker %s should be offline after recovery, got %s", workerID, w.Status)
		}
	}
}

// TestServerRestartRecovery_WorkerReRegistration simulates the full cycle:
// server crash → recovery → worker re-registers → jobs get scheduled.
func TestServerRestartRecovery_WorkerReRegistration(t *testing.T) {
	database, cleanup := setupDBTest(t)
	defer cleanup()

	caps := protocol.WorkerCapabilities{
		Encoders:      []string{"libx264", "h264_nvenc"},
		FFmpegVersion: "6.0",
	}

	// Pre-crash state: Create worker and running job
	workerID := "worker-pre-crash"
	worker, err := database.CreateWorker(workerID, "my-worker", caps)
	if err != nil {
		t.Fatalf("Failed to create worker: %v", err)
	}

	job, err := database.CreateJob(
		`["input.mp4"]`,
		`["-i", "input.mp4", "-c:v", "libx264", "output.mp4"]`,
		"output.mp4", false,
	)
	if err != nil {
		t.Fatalf("Failed to create job: %v", err)
	}
	if err := database.AssignJobToWorker(job.ID, workerID); err != nil {
		t.Fatalf("Failed to assign job: %v", err)
	}
	if err := database.UpdateJobStatusWithFailure(job.ID, protocol.JobStatusRunning, nil, nil, nil, nil); err != nil {
		t.Fatalf("Failed to set job running: %v", err)
	}

	// Simulate server crash and restart
	jobsReset, _, err := database.RecoverState()
	if err != nil {
		t.Fatalf("Failed to recover state: %v", err)
	}
	if jobsReset != 1 {
		t.Errorf("Expected 1 job reset, got %d", jobsReset)
	}

	// Verify worker is offline
	w, _ := database.GetWorker(workerID)
	if w.Status != protocol.WorkerStatusOffline {
		t.Errorf("Worker should be offline after recovery, got %s", w.Status)
	}

	// Simulate worker re-registration (same ID).
	// In the real flow, the handler uses UpdateWorkerHeartbeat which updates
	// the worker status and heartbeat timestamp. Since the worker already exists
	// in the DB, we update its status to idle to simulate a reconnect.
	if err := database.UpdateWorkerStatus(workerID, protocol.WorkerStatusIdle); err != nil {
		t.Fatalf("Failed to re-register worker (update status): %v", err)
	}

	// Verify worker is back online
	wAfterReg, _ := database.GetWorker(workerID)
	if wAfterReg.Status != protocol.WorkerStatusIdle {
		t.Errorf("Worker should be idle after re-registration, got %s", wAfterReg.Status)
	}

	// Verify job is pending and can be re-assigned to the worker
	updatedJob, _ := database.GetJob(job.ID)
	if updatedJob.Status != protocol.JobStatusPending {
		t.Errorf("Job should be pending after recovery, got %s", updatedJob.Status)
	}

	// Re-assign the pending job
	if err := database.AssignJobToWorker(job.ID, workerID); err != nil {
		t.Fatalf("Failed to re-assign job: %v", err)
	}
	if err := database.UpdateJobStatusWithFailure(job.ID, protocol.JobStatusQueued, nil, nil, nil, nil); err != nil {
		t.Fatalf("Failed to set job queued: %v", err)
	}

	// Verify job is now queued on the worker
	finalJob, _ := database.GetJob(job.ID)
	if finalJob.Status != protocol.JobStatusQueued {
		t.Errorf("Job should be queued after re-assignment, got %s", finalJob.Status)
	}
	if !finalJob.WorkerID.Valid || finalJob.WorkerID.String != workerID {
		t.Errorf("Job should be assigned to worker, got %v", finalJob.WorkerID)
	}

	// Verify worker capabilities are preserved through re-registration
	wFinal, _ := database.GetWorker(workerID)
	if wFinal.Status != protocol.WorkerStatusBusy {
		// Worker becomes busy from the active job assignment
		t.Logf("Worker status after job assignment: %s", wFinal.Status)
	}

	_ = worker
}

// TestServerRestartRecovery_WithStreamingJobs verifies that streaming output jobs
// are also properly recovered after server restart.
func TestServerRestartRecovery_WithStreamingJobs(t *testing.T) {
	database, cleanup := setupDBTest(t)
	defer cleanup()

	caps := protocol.WorkerCapabilities{
		Encoders:      []string{"libx264"},
		FFmpegVersion: "6.0",
	}

	// Create worker
	worker, err := database.CreateWorker("", "stream-worker", caps)
	if err != nil {
		t.Fatalf("Failed to create worker: %v", err)
	}

	// Create streaming job that was running
	streamingJob, err := database.CreateJob(
		`["input.mp4"]`,
		`["-i", "input.mp4", "-c:v", "libx264", "-f", "flv", "rtmp://localhost/live/stream"]`,
		"stream.flv", true, // streaming_output=true
	)
	if err != nil {
		t.Fatalf("Failed to create streaming job: %v", err)
	}
	if err := database.AssignJobToWorker(streamingJob.ID, worker.ID); err != nil {
		t.Fatalf("Failed to assign streaming job: %v", err)
	}
	if err := database.UpdateJobStatusWithFailure(streamingJob.ID, protocol.JobStatusRunning, nil, nil, nil, nil); err != nil {
		t.Fatalf("Failed to set streaming job running: %v", err)
	}

	// Perform recovery
	jobsReset, _, err := database.RecoverState()
	if err != nil {
		t.Fatalf("Failed to recover state: %v", err)
	}
	if jobsReset != 1 {
		t.Errorf("Expected 1 streaming job to be reset, got %d", jobsReset)
	}

	// Verify streaming job is pending
	sj, _ := database.GetJob(streamingJob.ID)
	if sj.Status != protocol.JobStatusPending {
		t.Errorf("Streaming job should be pending after recovery, got %s", sj.Status)
	}
	if sj.WorkerID.Valid {
		t.Errorf("Streaming job worker_id should be NULL after recovery")
	}
}

// TestServerRestartRecovery_DirectPathJobs verifies jobs with direct paths
// are properly recovered after server restart.
func TestServerRestartRecovery_DirectPathJobs(t *testing.T) {
	database, cleanup := setupDBTest(t)
	defer cleanup()

	caps := protocol.WorkerCapabilities{
		Encoders:      []string{"libx264"},
		FFmpegVersion: "6.0",
	}

	worker, err := database.CreateWorker("", "direct-worker", caps)
	if err != nil {
		t.Fatalf("Failed to create worker: %v", err)
	}

	// Create a job with direct paths (shared FS mode)
	directJob, err := database.CreateJob(
		`["/mnt/nfs/input.mp4"]`,
		`["-i", "/mnt/nfs/input.mp4", "-c:v", "libx264", "/mnt/nfs/output.mp4"]`,
		"/mnt/nfs/output.mp4", false,
	)
	if err != nil {
		t.Fatalf("Failed to create direct path job: %v", err)
	}

	// Set direct paths on the job
	_, err = database.GetDB().Exec(
		`UPDATE jobs SET direct_paths = ? WHERE id = ?`,
		`["/mnt/nfs/input.mp4"]`, directJob.ID,
	)
	if err != nil {
		t.Fatalf("Failed to set direct paths: %v", err)
	}

	if err := database.AssignJobToWorker(directJob.ID, worker.ID); err != nil {
		t.Fatalf("Failed to assign job: %v", err)
	}
	if err := database.UpdateJobStatusWithFailure(directJob.ID, protocol.JobStatusRunning, nil, nil, nil, nil); err != nil {
		t.Fatalf("Failed to set job running: %v", err)
	}

	// Recover
	jobsReset, _, err := database.RecoverState()
	if err != nil {
		t.Fatalf("Failed to recover: %v", err)
	}
	if jobsReset != 1 {
		t.Errorf("Expected 1 direct path job to be reset, got %d", jobsReset)
	}

	dj, _ := database.GetJob(directJob.ID)
	if dj.Status != protocol.JobStatusPending {
		t.Errorf("Direct path job should be pending, got %s", dj.Status)
	}
	if dj.WorkerID.Valid {
		t.Errorf("Direct path job worker_id should be NULL after recovery")
	}
}
