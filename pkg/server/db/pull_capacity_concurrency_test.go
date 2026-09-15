package db_test

import (
	"sync"
	"testing"

	"github.com/tsic404/rffmpeg/pkg/protocol"
)

// TestConcurrentPullRespectsCapacity verifies the atomic capacity recheck: a
// worker with MaxConcurrent=1 must end with exactly one active job no matter
// how many pulls race against it. Before the capacity COUNT and the claim ran
// in one transaction, each racing pull read a stale zero and claimed its own
// pending job, over-assigning the worker.
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

	createNJobs(t, database, 16)

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
		t.Errorf("worker has %d active jobs after concurrent pulls, want exactly 1", active)
	}
}
