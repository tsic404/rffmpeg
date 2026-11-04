package scheduler

// Regression test for the starvation sweep's notification target set: the
// broadcast, the WS sequence release and the quota release must follow the IDs
// the bulk UPDATE actually failed (RETURNING), not the pre-scan classification.
// A job a worker claimed after the scan is still running, so notifying it would
// tell the CLI the job failed, delete its sequence baseline (C-105) and release
// its quota slot (C-59).

import (
	"testing"
	"time"

	"github.com/tsic404/rffmpeg/pkg/protocol"
	"github.com/tsic404/rffmpeg/pkg/server/db"
	"github.com/tsic404/rffmpeg/pkg/server/ratelimit"
)

func TestStarvationSweepNotifiesOnlyActuallyFailedJobs(t *testing.T) {
	database, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("db.New: %v", err)
	}
	defer database.Close()

	counter := ratelimit.NewInMemoryCounter()
	database.SetTerminalHook(func(jobID string) { counter.DecrementByJob(jobID) })

	broadcaster := &fakeBroadcaster{}
	s := New(database, Config{
		ScheduleInterval:     time.Hour,
		TimeoutCheckInterval: time.Hour,
		MaxJobsPerWorker:     1,
		NoWorkerJobTimeout:   time.Minute,
	})
	s.SetJobNotifier(broadcaster)

	// Two jobs are pending past the no-worker deadline; no worker is live, so
	// both are scanned as starved.
	var jobs []*db.Job
	for range 2 {
		job, err := database.CreateJob(`["in.mp4"]`, `["-c:v","libx264"]`, "out.mp4", false)
		if err != nil {
			t.Fatalf("CreateJob: %v", err)
		}
		jobs = append(jobs, job)
		if _, ok := counter.TryIncrement("client-1", 10); !ok {
			t.Fatal("TryIncrement failed")
		}
		counter.RegisterJob("client-1", job.ID)
		if _, err := database.GetDB().Exec(
			`UPDATE jobs SET created_at = ? WHERE id = ?`,
			time.Now().Add(-5*time.Minute), job.ID); err != nil {
			t.Fatalf("backdate created_at: %v", err)
		}
	}
	kept, claimed := jobs[0], jobs[1]

	// Between the scan and the bulk UPDATE, a worker claims one of them: the
	// UPDATE must skip it and the notification must not run for it.
	s.beforeStarvationFail = func() {
		if err := database.AssignJobToWorker(claimed.ID, "late-worker"); err != nil {
			t.Errorf("claim job: %v", err)
		}
	}

	s.checkNoWorkerStarvation(starvationTriggerDirect)

	gotKept, err := database.GetJob(kept.ID)
	if err != nil {
		t.Fatalf("GetJob(kept): %v", err)
	}
	if gotKept.Status != protocol.JobStatusFailed {
		t.Fatalf("scanned+failed job status = %s, want failed", gotKept.Status)
	}
	gotClaimed, err := database.GetJob(claimed.ID)
	if err != nil {
		t.Fatalf("GetJob(claimed): %v", err)
	}
	if gotClaimed.Status != protocol.JobStatusQueued {
		t.Fatalf("claimed job was terminalized: status = %s", gotClaimed.Status)
	}

	// Terminal notifications target only the actually failed job.
	if len(broadcaster.statuses) != 1 || broadcaster.statuses[0].jobID != kept.ID {
		t.Fatalf("broadcasts = %+v, want only %s", broadcaster.statuses, kept.ID)
	}
	if len(broadcaster.released) != 1 || broadcaster.released[0] != kept.ID {
		t.Fatalf("released = %v, want [%s]", broadcaster.released, kept.ID)
	}
	if n := counter.Count("client-1"); n != 1 {
		t.Errorf("quota after sweep = %d, want 1 (the running job keeps its slot)", n)
	}
}
