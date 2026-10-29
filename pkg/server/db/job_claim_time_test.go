package db

import (
	"testing"
	"time"

	"github.com/tsic404/rffmpeg/pkg/protocol"
)

// TestJobClaimTimeRecordsPendingToQueued pins the claim time that the
// migration dispatch segment is judged on: it is written by both claim paths
// (scheduler assignment and worker pull), and every requeue path clears it so
// a pending job never reports the previous attempt's claim.
func TestJobClaimTimeRecordsPendingToQueued(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	if _, err := database.CreateWorker("worker-1", "worker-1", protocol.WorkerCapabilities{
		Encoders:      []string{"libx264"},
		FFmpegVersion: "6.0",
	}); err != nil {
		t.Fatalf("CreateWorker: %v", err)
	}

	job, err := database.CreateJob(`["in.mkv"]`, `["-c:v","libx264"]`, "out.mkv", false)
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	fresh, err := database.GetJob(job.ID)
	if err != nil {
		t.Fatalf("GetJob(fresh): %v", err)
	}
	if fresh.ClaimedAt.Valid {
		t.Fatalf("pending job claimed_at = %v, want NULL", fresh.ClaimedAt.Time)
	}

	before := time.Now()
	if err := database.AssignJobToWorker(job.ID, "worker-1"); err != nil {
		t.Fatalf("AssignJobToWorker: %v", err)
	}
	claimed, err := database.GetJob(job.ID)
	if err != nil {
		t.Fatalf("GetJob(claimed): %v", err)
	}
	if !claimed.ClaimedAt.Valid {
		t.Fatal("assigned job claimed_at is NULL, want the pending -> queued moment")
	}
	if claimed.ClaimedAt.Time.Before(before) || claimed.ClaimedAt.Time.After(time.Now()) {
		t.Fatalf("claimed_at = %v, want within [%v, now]", claimed.ClaimedAt.Time, before)
	}

	if err := database.ResetJobToPending(job.ID); err != nil {
		t.Fatalf("ResetJobToPending: %v", err)
	}
	reset, err := database.GetJob(job.ID)
	if err != nil {
		t.Fatalf("GetJob(reset): %v", err)
	}
	if reset.ClaimedAt.Valid {
		t.Fatalf("requeued job claimed_at = %v, want NULL", reset.ClaimedAt.Time)
	}

	// The worker pull path claims directly from pending; the returned job must
	// carry the same claim time the database stores.
	pulled, err := database.AssignPendingJobsToWorker("worker-1", 1)
	if err != nil {
		t.Fatalf("AssignPendingJobsToWorker: %v", err)
	}
	if len(pulled) != 1 || pulled[0].ID != job.ID {
		t.Fatalf("pulled jobs = %v, want the requeued job", pulled)
	}
	if !pulled[0].ClaimedAt.Valid {
		t.Fatal("pulled job lacks claimed_at in the pull response")
	}
	reclaimed, err := database.GetJob(job.ID)
	if err != nil {
		t.Fatalf("GetJob(reclaimed): %v", err)
	}
	if !reclaimed.ClaimedAt.Valid {
		t.Fatal("pulled job claimed_at is NULL in the database")
	}
	if !reclaimed.ClaimedAt.Time.Equal(pulled[0].ClaimedAt.Time) {
		t.Errorf("pull response claimed_at = %v, database = %v", pulled[0].ClaimedAt.Time, reclaimed.ClaimedAt.Time)
	}
}
