package workerhealth

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/tsix404/rffmpeg/pkg/protocol"
	"github.com/tsix404/rffmpeg/pkg/server/migration"
)

// TestMultiWorkerFailover_FullFlow tests the complete worker failover scenario:
//  1. 2+ workers register
//  2. Jobs get assigned to worker-A
//  3. worker-A goes offline (heartbeat timeout)
//  4. Monitor detects offline worker and migrates jobs
//  5. Migrated jobs return to pending pool
//  6. Scheduler assigns migrated jobs to worker-B
//  7. Migration audit events are recorded
func TestMultiWorkerFailover_FullFlow(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	// Step 1: Create 2 workers with different capabilities
	workerA, err := database.CreateWorker("worker-a", "gpu-worker", protocol.WorkerCapabilities{
		Encoders:      []string{"libx264", "h264_nvenc"},
		FFmpegVersion: "6.0",
		MaxConcurrent: 2,
	})
	if err != nil {
		t.Fatalf("Failed to create worker-a: %v", err)
	}

	workerB, err := database.CreateWorker("worker-b", "cpu-worker", protocol.WorkerCapabilities{
		Encoders:      []string{"libx264"},
		FFmpegVersion: "6.0",
		MaxConcurrent: 2,
	})
	if err != nil {
		t.Fatalf("Failed to create worker-b: %v", err)
	}

	// Step 2: Create and assign jobs to worker-A
	job1, err := database.CreateJob(
		`["input1.mp4"]`,
		`["-i", "input1.mp4", "-c:v", "libx264", "-preset", "fast", "output1.mp4"]`,
		"output1.mp4", false,
	)
	if err != nil {
		t.Fatalf("Failed to create job1: %v", err)
	}

	job2, err := database.CreateJob(
		`["input2.mp4"]`,
		`["-i", "input2.mp4", "-c:v", "h264_nvenc", "output2.mp4"]`,
		"output2.mp4", false,
	)
	if err != nil {
		t.Fatalf("Failed to create job2: %v", err)
	}

	job3, err := database.CreateJob(
		`["input3.mp4"]`,
		`["-i", "input3.mp4", "-c:v", "libx264", "output3.mp4"]`,
		"output3.mp4", false,
	)
	if err != nil {
		t.Fatalf("Failed to create job3: %v", err)
	}

	// Assign jobs to worker-A and set them to running/queued
	if err := database.AssignJobToWorker(job1.ID, workerA.ID); err != nil {
		t.Fatalf("Failed to assign job1 to worker-a: %v", err)
	}
	if err := database.UpdateJobStatusWithFailure(job1.ID, protocol.JobStatusRunning, nil, nil, nil, nil); err != nil {
		t.Fatalf("Failed to set job1 as running: %v", err)
	}

	if err := database.AssignJobToWorker(job2.ID, workerA.ID); err != nil {
		t.Fatalf("Failed to assign job2 to worker-a: %v", err)
	}
	if err := database.UpdateJobStatusWithFailure(job2.ID, protocol.JobStatusRunning, nil, nil, nil, nil); err != nil {
		t.Fatalf("Failed to set job2 as running: %v", err)
	}

	if err := database.AssignJobToWorker(job3.ID, workerA.ID); err != nil {
		t.Fatalf("Failed to assign job3 to worker-a: %v", err)
	}
	if err := database.UpdateJobStatusWithFailure(job3.ID, protocol.JobStatusQueued, nil, nil, nil, nil); err != nil {
		t.Fatalf("Failed to set job3 as queued: %v", err)
	}

	// Update worker-A status to busy
	if err := database.UpdateWorkerStatus(workerA.ID, protocol.WorkerStatusBusy); err != nil {
		t.Fatalf("Failed to set worker-a as busy: %v", err)
	}

	// Step 3: Simulate worker-A heartbeat timeout (set last_heartbeat to 60s ago)
	_, err = database.GetDB().Exec(
		`UPDATE workers SET last_heartbeat = ? WHERE id = ?`,
		time.Now().Add(-60*time.Second), workerA.ID,
	)
	if err != nil {
		t.Fatalf("Failed to update worker-a heartbeat: %v", err)
	}

	// Step 4: Create monitor and trigger health check
	monitor := New(database, Config{
		HeartbeatTimeout:    30 * time.Second,
		OfflineThreshold:    10 * time.Minute,
		HealthCheckInterval: 1 * time.Second,
		MaxRetryCount:       3,
	})

	mockSched := &mockScheduler{}
	monitor.SetScheduler(mockSched)
	monitor.checkWorkers()

	// Verify worker-A is now offline
	workerAafter, err := database.GetWorker(workerA.ID)
	if err != nil {
		t.Fatalf("Failed to get worker-a: %v", err)
	}
	if workerAafter.Status != protocol.WorkerStatusOffline {
		t.Errorf("Expected worker-a to be offline after heartbeat timeout, got %s", workerAafter.Status)
	}

	// Verify worker-B is still idle
	workerBafter, err := database.GetWorker(workerB.ID)
	if err != nil {
		t.Fatalf("Failed to get worker-b: %v", err)
	}
	if workerBafter.Status != protocol.WorkerStatusIdle {
		t.Errorf("Expected worker-b to remain idle, got %s", workerBafter.Status)
	}

	// Step 5: Verify all 3 jobs were migrated back to pending
	for _, jobID := range []string{job1.ID, job2.ID, job3.ID} {
		job, err := database.GetJob(jobID)
		if err != nil {
			t.Fatalf("Failed to get job %s: %v", jobID, err)
		}
		if job.Status != protocol.JobStatusPending {
			t.Errorf("Expected job %s to be pending after migration, got %s", jobID, job.Status)
		}
		if job.WorkerID.Valid {
			t.Errorf("Expected job %s to have NULL worker_id after migration", jobID)
		}
	}

	// Step 6: Verify scheduler was triggered for rescheduling
	if !mockSched.rescheduleCalled {
		t.Error("Expected scheduler to be triggered after job migration")
	}

	// Step 7: Verify migration audit event was recorded
	events, err := database.GetMigrationEventsByWorker(workerA.ID, 10)
	if err != nil {
		t.Fatalf("Failed to get migration events: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("Expected 1 migration event, got %d", len(events))
	}

	event := events[0]
	if string(event.Reason) != string(migration.ReasonHeartbeatTimeout) {
		t.Errorf("Expected migration reason '%s', got '%s'", migration.ReasonHeartbeatTimeout, event.Reason)
	}
	if event.JobsMigrated != 3 {
		t.Errorf("Expected 3 jobs migrated, got %d", event.JobsMigrated)
	}

	var jobIDs []string
	if err := json.Unmarshal([]byte(event.JobIDs), &jobIDs); err != nil {
		t.Fatalf("Failed to unmarshal job IDs: %v", err)
	}
	if len(jobIDs) != 3 {
		t.Errorf("Expected 3 job IDs in event, got %d", len(jobIDs))
	}

	// Step 8: Verify worker-B can pull the pending jobs
	idleWorkersWithCount, err := database.GetIdleWorkersWithJobCount()
	if err != nil {
		t.Fatalf("Failed to get idle workers: %v", err)
	}

	for _, w := range idleWorkersWithCount {
		if w.Worker.ID == workerB.ID {
			if w.Worker.Status != protocol.WorkerStatusIdle {
				t.Errorf("Expected worker-b to be idle, got %s", w.Worker.Status)
			}
			if w.ActiveJobs != 0 {
				t.Errorf("Expected worker-b to have 0 active jobs, got %d", w.ActiveJobs)
			}
		}
	}
}

// TestMultiWorkerFailover_CapabilityAwareRescheduling verifies that when a job
// requests a specific encoder and the original worker goes offline, the scheduler
// can find another worker with that capability.
func TestMultiWorkerFailover_CapabilityAwareRescheduling(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	// Create workers with different capabilities
	workerA, err := database.CreateWorker("worker-a", "gpu-worker-a", protocol.WorkerCapabilities{
		Encoders:      []string{"libx264", "h264_nvenc", "hevc_nvenc"},
		FFmpegVersion: "6.0",
		MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatalf("Failed to create worker-a: %v", err)
	}

	workerB, err := database.CreateWorker("worker-b", "gpu-worker-b", protocol.WorkerCapabilities{
		Encoders:      []string{"libx264", "h264_nvenc"},
		FFmpegVersion: "6.0",
		MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatalf("Failed to create worker-b: %v", err)
	}

	workerC, err := database.CreateWorker("worker-c", "cpu-worker", protocol.WorkerCapabilities{
		Encoders:      []string{"libx264"},
		FFmpegVersion: "6.0",
		MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatalf("Failed to create worker-c: %v", err)
	}

	// Create a job requesting h264_nvenc and assign to worker-A
	job, err := database.CreateJob(
		`["input.mp4"]`,
		`["-i", "input.mp4", "-c:v", "h264_nvenc", "-preset", "p4", "output.mp4"]`,
		"output.mp4", false,
	)
	if err != nil {
		t.Fatalf("Failed to create job: %v", err)
	}

	if err := database.AssignJobToWorker(job.ID, workerA.ID); err != nil {
		t.Fatalf("Failed to assign job: %v", err)
	}
	if err := database.UpdateJobStatusWithFailure(job.ID, protocol.JobStatusRunning, nil, nil, nil, nil); err != nil {
		t.Fatalf("Failed to set job running: %v", err)
	}

	// Simulate worker-A goes offline
	_, err = database.GetDB().Exec(
		`UPDATE workers SET last_heartbeat = ? WHERE id = ?`,
		time.Now().Add(-60*time.Second), workerA.ID,
	)
	if err != nil {
		t.Fatalf("Failed to update heartbeat: %v", err)
	}

	// Run monitor check
	monitor := New(database, Config{
		HeartbeatTimeout:    30 * time.Second,
		OfflineThreshold:    10 * time.Minute,
		HealthCheckInterval: 1 * time.Second,
		MaxRetryCount:       3,
	})
	mockSched := &mockScheduler{}
	monitor.SetScheduler(mockSched)
	monitor.checkWorkers()

	// Verify job is pending
	updatedJob, err := database.GetJob(job.ID)
	if err != nil {
		t.Fatalf("Failed to get job: %v", err)
	}
	if updatedJob.Status != protocol.JobStatusPending {
		t.Errorf("Expected job to be pending, got %s", updatedJob.Status)
	}

	// Verify worker-B with h264_nvenc capability is idle and can accept the job
	workersByEncoder, err := database.GetIdleWorkersByEncoder("h264_nvenc")
	if err != nil {
		t.Fatalf("Failed to get workers by encoder: %v", err)
	}

	// worker-B should be available for h264_nvenc
	if len(workersByEncoder) != 1 {
		t.Errorf("Expected 1 idle worker with h264_nvenc, got %d", len(workersByEncoder))
	} else if workersByEncoder[0].ID != workerB.ID {
		t.Errorf("Expected worker-b to be the idle worker with h264_nvenc, got %s", workersByEncoder[0].ID)
	}

	// worker-C (cpu only) should NOT appear for h264_nvenc queries
	workers, err := database.GetWorkersByEncoder("h264_nvenc")
	if err != nil {
		t.Fatalf("Failed to get workers by encoder: %v", err)
	}

	workerCFound := false
	for _, w := range workers {
		if w.ID == workerC.ID {
			workerCFound = true
		}
	}
	if workerCFound {
		t.Error("Worker-c without h264_nvenc should NOT appear in workers by encoder")
	}
}

// TestMultiWorkerFailover_StressMultipleWorkers tests failover with 4 workers and 10 jobs.
func TestMultiWorkerFailover_StressMultipleWorkers(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	// Create 4 workers
	workerIDs := make([]string, 4)
	for i := 0; i < 4; i++ {
		id := fmt.Sprintf("worker-%d", i+1)
		w, err := database.CreateWorker(id, id, protocol.WorkerCapabilities{
			Encoders:      []string{"libx264"},
			FFmpegVersion: "6.0",
			MaxConcurrent: 3,
		})
		if err != nil {
			t.Fatalf("Failed to create %s: %v", id, err)
		}
		workerIDs[i] = w.ID
	}

	// Create 10 jobs and assign them round-robin to workers
	for i := 0; i < 10; i++ {
		job, err := database.CreateJob(
			fmt.Sprintf(`["input%d.mp4"]`, i),
			fmt.Sprintf(`["-i", "input%d.mp4", "-c:v", "libx264", "output%d.mp4"]`, i, i),
			fmt.Sprintf("output%d.mp4", i), false,
		)
		if err != nil {
			t.Fatalf("Failed to create job %d: %v", i, err)
		}

		// Assign to a worker (round-robin)
		workerIdx := i % 4
		if err := database.AssignJobToWorker(job.ID, workerIDs[workerIdx]); err != nil {
			t.Fatalf("Failed to assign job %d: %v", i, err)
		}
		if err := database.UpdateJobStatusWithFailure(job.ID, protocol.JobStatusRunning, nil, nil, nil, nil); err != nil {
			t.Fatalf("Failed to set job %d running: %v", i, err)
		}
	}

	// Mark worker-1 and worker-2 as offline (simulate crash)
	for _, id := range []string{workerIDs[0], workerIDs[1]} {
		_, err := database.GetDB().Exec(
			`UPDATE workers SET last_heartbeat = ? WHERE id = ?`,
			time.Now().Add(-60*time.Second), id,
		)
		if err != nil {
			t.Fatalf("Failed to update heartbeat for %s: %v", id, err)
		}
	}

	// Run monitor
	monitor := New(database, Config{
		HeartbeatTimeout:    30 * time.Second,
		OfflineThreshold:    10 * time.Minute,
		HealthCheckInterval: 1 * time.Second,
		MaxRetryCount:       3,
	})
	mockSched := &mockScheduler{}
	monitor.SetScheduler(mockSched)
	monitor.checkWorkers()

	// Count how many jobs were migrated (are now pending)
	pendingJobs, err := database.GetPendingJobs(100)
	if err != nil {
		t.Fatalf("Failed to get pending jobs: %v", err)
	}
	t.Logf("Total pending jobs after failover: %d", len(pendingJobs))

	// Scheduler should have been triggered
	if !mockSched.rescheduleCalled {
		t.Error("Expected scheduler to be triggered after multi-worker failover")
	}

	// Verify no offline workers appear in idle list
	idleWorkers, err := database.GetIdleWorkers()
	if err != nil {
		t.Fatalf("Failed to get idle workers: %v", err)
	}
	for _, w := range idleWorkers {
		if w.ID == workerIDs[0] || w.ID == workerIDs[1] {
			t.Errorf("Offline worker %s should not appear in idle workers", w.ID)
		}
	}
}
