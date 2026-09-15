package db_test

import (
	"sync"
	"testing"

	"github.com/tsic404/rffmpeg/pkg/protocol"
)

// TestConcurrentPullRespectsCapacity verifies the capacity guard: a worker
// with MaxConcurrent=1 that already has one running job must claim no further
// jobs no matter how many pulls race against it. Before the capacity COUNT and
// the claims ran in one write transaction, each racing pull read a stale zero
// and claimed its own pending job, over-assigning the worker past its capacity.
func TestConcurrentPullRespectsCapacity(t *testing.T) {
	database, cleanup := setupIntegrityDB(t)
	defer cleanup()

	if _, err := database.CreateWorker("w1", "w1", protocol.WorkerCapabilities{
		Encoders:      []string{"libx264"},
		FFmpegVersion: "5.0",
		MaxConcurrent: 1,
	}); err != nil {
		t.Fatalf("CreateWorker: %v", err)
	}

	// Pre-set one running job so the worker is already at capacity before the
	// concurrent pulls fire: every pull must hit the capacity guard
	// (maxNewJobs == 0) instead of the claim loop.
	running := createNJobs(t, database, 1)[0]
	if _, err := database.AssignPendingJobsToWorker("w1", 1); err != nil {
		t.Fatalf("AssignPendingJobsToWorker (pre-set): %v", err)
	}
	if err := database.UpdateJobStatusWithFailure(running.ID, protocol.JobStatusRunning, nil, nil, nil, nil); err != nil {
		t.Fatalf("UpdateJobStatusWithFailure (pre-set running): %v", err)
	}

	pending := createNJobs(t, database, 16)

	var wg sync.WaitGroup
	errCh := make(chan error, 16)
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := database.AssignPendingJobsToWorker("w1", 1); err != nil {
				select {
				case errCh <- err:
				default:
				}
			}
		}()
	}
	wg.Wait()
	close(errCh)
	if err := <-errCh; err != nil {
		t.Fatalf("concurrent pull: %v", err)
	}

	active, err := database.GetWorkerActiveJobCount("w1")
	if err != nil {
		t.Fatalf("GetWorkerActiveJobCount: %v", err)
	}
	if active != 1 {
		t.Errorf("worker has %d active jobs after concurrent pulls, want exactly 1 (the pre-set running job)", active)
	}

	// The pre-set running job filled the capacity, so no racing pull may have
	// dispatched a pending job.
	for _, j := range pending {
		got, err := database.GetJob(j.ID)
		if err != nil {
			t.Fatalf("GetJob %s: %v", j.ID, err)
		}
		if got.Status != protocol.JobStatusPending {
			t.Errorf("job %s was dispatched (status %s), want still pending", j.ID, got.Status)
		}
	}
}
