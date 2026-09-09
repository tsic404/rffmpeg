package db_test

import (
	"testing"

	"github.com/tsic404/rffmpeg/pkg/protocol"
	"github.com/tsic404/rffmpeg/pkg/server/db"
)

// TestJobAttributionPersisted guards the executor attribution recorded on the
// jobs table (TSI-2920): assigned_worker + worker_name are written at
// assignment and survive the completed and failed terminal states, so
// GET /api/v1/jobs no longer reports a null executing worker.
func TestJobAttributionPersisted(t *testing.T) {
	database, cleanup := setupDBTest(t)
	defer cleanup()

	worker, err := database.CreateWorker("", "gpu-worker-1", protocol.WorkerCapabilities{
		Encoders:      []string{"libx264"},
		FFmpegVersion: "6.0",
		MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatalf("CreateWorker: %v", err)
	}

	newAssignedJob := func(t *testing.T) string {
		t.Helper()
		job, err := database.CreateJob(`["in.mkv"]`, `["-c:v","libx264"]`, "out.mkv", false)
		if err != nil {
			t.Fatalf("CreateJob: %v", err)
		}
		if err := database.AssignJobToWorker(job.ID, worker.ID); err != nil {
			t.Fatalf("AssignJobToWorker: %v", err)
		}
		return job.ID
	}

	assertAttribution := func(t *testing.T, jobID, phase string) {
		t.Helper()
		got, err := database.GetJob(jobID)
		if err != nil {
			t.Fatalf("GetJob: %v", err)
		}
		if !got.AssignedWorker.Valid || got.AssignedWorker.String != worker.ID {
			t.Errorf("%s: assigned_worker = %v, want %s", phase, got.AssignedWorker, worker.ID)
		}
		if !got.WorkerName.Valid || got.WorkerName.String != "gpu-worker-1" {
			t.Errorf("%s: worker_name = %v, want gpu-worker-1", phase, got.WorkerName)
		}
	}

	t.Run("assignment writes attribution", func(t *testing.T) {
		assertAttribution(t, newAssignedJob(t), "assignment")
	})

	t.Run("completed keeps attribution", func(t *testing.T) {
		jobID := newAssignedJob(t)
		if err := database.UpdateJobStatusWithFailure(jobID, protocol.JobStatusRunning, nil, nil, nil, nil); err != nil {
			t.Fatalf("set running: %v", err)
		}
		exitCode := 0
		if err := database.UpdateJobTerminalStatusWithOwner(jobID, worker.ID, protocol.JobStatusCompleted, &exitCode, nil, nil, nil); err != nil {
			t.Fatalf("complete: %v", err)
		}
		assertAttribution(t, jobID, "completed")
	})

	t.Run("failed keeps attribution after worker_id clear", func(t *testing.T) {
		jobID := newAssignedJob(t)
		if err := database.UpdateJobStatusWithFailure(jobID, protocol.JobStatusRunning, nil, nil, nil, nil); err != nil {
			t.Fatalf("set running: %v", err)
		}
		if err := database.FailJob(jobID, "boom", string(protocol.FailureWorkerCrash)); err != nil {
			t.Fatalf("FailJob: %v", err)
		}
		got, err := database.GetJob(jobID)
		if err != nil {
			t.Fatalf("GetJob: %v", err)
		}
		if got.WorkerID.Valid {
			t.Errorf("FailJob should clear worker_id, got %s", got.WorkerID.String)
		}
		assertAttribution(t, jobID, "failed")
	})

	t.Run("pull assignment returns in-memory attribution", func(t *testing.T) {
		job, err := database.CreateJob(`["in.mkv"]`, `["-c:v","libx264"]`, "out.mkv", false)
		if err != nil {
			t.Fatalf("CreateJob: %v", err)
		}
		claimed, err := database.AssignPendingJobsToWorker(worker.ID, 10)
		if err != nil {
			t.Fatalf("AssignPendingJobsToWorker: %v", err)
		}
		var got *db.Job
		for _, j := range claimed {
			if j.ID == job.ID {
				got = j
				break
			}
		}
		if got == nil {
			t.Fatalf("claimed job %s not returned by pull", job.ID)
		}
		if !got.AssignedWorker.Valid || got.AssignedWorker.String != worker.ID {
			t.Errorf("pull in-memory assigned_worker = %v, want %s", got.AssignedWorker, worker.ID)
		}
		if !got.WorkerName.Valid || got.WorkerName.String != "gpu-worker-1" {
			t.Errorf("pull in-memory worker_name = %v, want gpu-worker-1", got.WorkerName)
		}
		assertAttribution(t, job.ID, "pull-assignment")
	})

	t.Run("no-owner terminal backfills attribution", func(t *testing.T) {
		jobID := newAssignedJob(t)
		// Simulate a legacy row assigned before the attribution columns existed:
		// worker_id set, but assigned_worker/worker_name still NULL.
		if _, err := database.GetDB().Exec(
			`UPDATE jobs SET assigned_worker = NULL, worker_name = NULL WHERE id = ?`, jobID,
		); err != nil {
			t.Fatalf("null attribution: %v", err)
		}
		exitCode := 0
		if err := database.UpdateJobStatusWithFailure(jobID, protocol.JobStatusCompleted, &exitCode, nil, nil, nil); err != nil {
			t.Fatalf("complete via unguarded path: %v", err)
		}
		assertAttribution(t, jobID, "no-owner-terminal")
	})
}
