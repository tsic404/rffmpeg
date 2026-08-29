package db_test

import (
	"testing"
	"time"

	"github.com/tsix404/rffmpeg/pkg/protocol"
)

func TestJobNotifierWakesSubscriberOnTerminalWrite(t *testing.T) {
	database, cleanup := setupDBTest(t)
	defer cleanup()

	job, err := database.CreateJob(`["input"]`, `["-i","input"]`, "out.mp4", false)
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	notifyCh, cancel := database.JobNotifier().Subscribe(job.ID)
	defer cancel()

	// A non-terminal status update must not wake the waiter.
	if err := database.UpdateJobStatusWithFailure(job.ID, protocol.JobStatusRunning, nil, nil, nil, nil); err != nil {
		t.Fatalf("UpdateJobStatusWithFailure(running): %v", err)
	}
	select {
	case <-notifyCh:
		t.Fatalf("received wake-up for non-terminal status update")
	case <-time.After(100 * time.Millisecond):
	}

	if err := database.UpdateJobStatusWithFailure(job.ID, protocol.JobStatusCompleted, nil, nil, nil, nil); err != nil {
		t.Fatalf("UpdateJobStatusWithFailure(completed): %v", err)
	}
	select {
	case <-notifyCh:
	case <-time.After(2 * time.Second):
		t.Fatalf("no wake-up after terminal status write")
	}
}

func TestJobNotifierCancelledSubscriberNotWoken(t *testing.T) {
	database, cleanup := setupDBTest(t)
	defer cleanup()

	job, err := database.CreateJob(`["input"]`, `["-i","input"]`, "out.mp4", false)
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	notifyCh, cancel := database.JobNotifier().Subscribe(job.ID)
	cancel()

	if err := database.CancelJob(job.ID); err != nil {
		t.Fatalf("CancelJob: %v", err)
	}
	select {
	case <-notifyCh:
		t.Fatalf("cancelled subscriber was woken")
	case <-time.After(100 * time.Millisecond):
	}
}

func TestJobNotifierMultipleSubscribersAllWoken(t *testing.T) {
	database, cleanup := setupDBTest(t)
	defer cleanup()

	job, err := database.CreateJob(`["input"]`, `["-i","input"]`, "out.mp4", false)
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	ch1, cancel1 := database.JobNotifier().Subscribe(job.ID)
	defer cancel1()
	ch2, cancel2 := database.JobNotifier().Subscribe(job.ID)
	defer cancel2()

	if err := database.FailJob(job.ID, "boom", ""); err != nil {
		t.Fatalf("FailJob: %v", err)
	}

	for name, ch := range map[string]<-chan struct{}{"subscriber 1": ch1, "subscriber 2": ch2} {
		select {
		case <-ch:
		case <-time.After(2 * time.Second):
			t.Fatalf("%s was not woken", name)
		}
	}
}

func TestJobNotifierTerminalOwnerWriteWakes(t *testing.T) {
	database, cleanup := setupDBTest(t)
	defer cleanup()

	job, err := database.CreateJob(`["input"]`, `["-i","input"]`, "out.mp4", false)
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if err := database.AssignJobToWorker(job.ID, "worker-1"); err != nil {
		t.Fatalf("AssignJobToWorker: %v", err)
	}

	notifyCh, cancel := database.JobNotifier().Subscribe(job.ID)
	defer cancel()

	if err := database.UpdateJobTerminalStatusWithOwner(job.ID, "worker-1", protocol.JobStatusFailed, nil, nil, nil, nil); err != nil {
		t.Fatalf("UpdateJobTerminalStatusWithOwner: %v", err)
	}
	select {
	case <-notifyCh:
	case <-time.After(2 * time.Second):
		t.Fatalf("no wake-up after ownership-guarded terminal write")
	}
}
