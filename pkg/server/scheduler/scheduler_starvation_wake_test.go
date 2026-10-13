package scheduler

import (
	"testing"
	"time"

	"github.com/tsic404/rffmpeg/pkg/protocol"
	"github.com/tsic404/rffmpeg/pkg/server/db"
)

// offlineWorkerFixture creates the worker row the starvation sweep's
// live-worker guard needs to see a cluster with nothing schedulable.
func offlineWorkerFixture(t *testing.T, database *db.Database) {
	t.Helper()
	if _, err := database.CreateWorker("worker-1", "test-worker", protocol.WorkerCapabilities{
		Encoders:      []string{"libx264"},
		FFmpegVersion: "5.0",
		MaxConcurrent: 1,
	}); err != nil {
		t.Fatalf("Failed to create worker: %v", err)
	}
	if err := database.UpdateWorkerStatus("worker-1", protocol.WorkerStatusOffline); err != nil {
		t.Fatalf("Failed to mark worker offline: %v", err)
	}
}

// backdatePendingAnchor rewrites a pending job's created_at, the clock the
// starvation sweep measures the no-worker timeout from.
func backdatePendingAnchor(t *testing.T, database *db.Database, jobID string, at time.Time) {
	t.Helper()
	if _, err := database.GetDB().Exec(`UPDATE jobs SET created_at = ? WHERE id = ?`, at, jobID); err != nil {
		t.Fatalf("Failed to backdate created_at: %v", err)
	}
}

func pendingJob(t *testing.T, database *db.Database) *db.Job {
	t.Helper()
	job, err := database.CreateJob(`["file1.mp4"]`, `["-i", "input.mp4"]`, "output.mp4", false)
	if err != nil {
		t.Fatalf("Failed to create job: %v", err)
	}
	return job
}

func assertStarvedNoWorker(t *testing.T, database *db.Database, jobID string) {
	t.Helper()
	updated, err := database.GetJob(jobID)
	if err != nil {
		t.Fatalf("Failed to get job: %v", err)
	}
	if updated.Status != protocol.JobStatusFailed {
		t.Errorf("Expected status failed, got %s", updated.Status)
	}
	if updated.FailureType != string(protocol.FailureNoWorkerAvailable) {
		t.Errorf("Expected failure_type %q, got %q", protocol.FailureNoWorkerAvailable, updated.FailureType)
	}
}

// The no-worker verdict must land just past the pending clock's deadline, not
// at whatever phase the timeout tick happens to be in: the tick-only spread —
// anywhere in (deadline, deadline+interval] — is what let the CLI give up and
// cancel a job the server was about to fail as WORKER_CRASH (AC-17). An
// hour-long check interval makes the tick irrelevant here, so only the expiry
// wake can produce a verdict at all.
func TestStarvationWakeJudgesJustPastDeadline(t *testing.T) {
	database, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("Failed to create database: %v", err)
	}
	defer database.Close()

	const noWorkerTimeout = 3 * time.Second
	s := New(database, Config{
		ScheduleInterval:     time.Hour,
		TimeoutCheckInterval: time.Hour, // the expiry wake is the only trigger
		MaxJobsPerWorker:     1,
		NoWorkerJobTimeout:   noWorkerTimeout,
		HeartbeatFreshness:   90 * time.Second,
	})

	offlineWorkerFixture(t, database)
	job := pendingJob(t, database)

	notifyCh, cancel := database.JobNotifier().Subscribe(job.ID)
	defer cancel()

	s.Start()
	defer s.Stop()

	// Anchor the pending clock only after the loop is running: a deadline
	// computed before Start can already be past by the time the loop arms the
	// wake under CI load, silently falling back to the hour-long tick. The
	// reschedule trigger re-arms from the freshly backdated anchor.
	deadline := time.Now().Add(2 * time.Second)
	backdatePendingAnchor(t, database, job.ID, deadline.Add(-noWorkerTimeout))
	s.TriggerReschedule()

	var verdictAt time.Time
	select {
	case <-notifyCh:
		verdictAt = time.Now()
	case <-time.After(30 * time.Second):
		t.Fatalf("expiry wake did not judge the job (deadline was %s after start)", time.Until(deadline))
	}

	if !verdictAt.After(deadline) {
		t.Errorf("verdict at %s, before the pending clock's deadline %s: the sweep must not judge a job that has waited no longer than the no-worker timeout", verdictAt, deadline)
	}
	if bound := deadline.Add(starvationExpiryMargin + 4*time.Second); verdictAt.After(bound) {
		t.Errorf("verdict at %s, more than %s past the deadline %s: it fell back to the check interval instead of the expiry wake", verdictAt, starvationExpiryMargin+4*time.Second, deadline)
	}
	assertStarvedNoWorker(t, database, job.ID)
}

// A job whose deadline elapsed before the scheduler started has no future
// instant to wake on; the timeout tick must still judge it. The expiry wake is
// an accelerator, never the only path to a verdict.
func TestStarvationWakeFallsBackToTickForAnExpiredDeadline(t *testing.T) {
	database, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("Failed to create database: %v", err)
	}
	defer database.Close()

	s := New(database, Config{
		ScheduleInterval:     time.Hour,
		TimeoutCheckInterval: 500 * time.Millisecond,
		MaxJobsPerWorker:     1,
		NoWorkerJobTimeout:   time.Minute,
	})

	offlineWorkerFixture(t, database)
	job := pendingJob(t, database)
	backdatePendingAnchor(t, database, job.ID, time.Now().Add(-10*time.Minute))

	notifyCh, cancel := database.JobNotifier().Subscribe(job.ID)
	defer cancel()

	s.Start()
	defer s.Stop()

	select {
	case <-notifyCh:
	case <-time.After(10 * time.Second):
		t.Fatal("timeout tick did not judge a job whose no-worker deadline was already past at start")
	}
	assertStarvedNoWorker(t, database, job.ID)
}

// While a live schedulable worker exists the sweep can never judge, so the
// wake must stay disarmed: arming it would run the unindexed anchor lookup on
// every tick for a verdict that cannot happen.
func TestStarvationWakeSkippedWhileWorkerIsLive(t *testing.T) {
	database, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("Failed to create database: %v", err)
	}
	defer database.Close()

	s := New(database, Config{
		ScheduleInterval:     time.Hour,
		TimeoutCheckInterval: time.Hour,
		MaxJobsPerWorker:     1,
		NoWorkerJobTimeout:   time.Minute,
		HeartbeatFreshness:   90 * time.Second,
	})

	if _, err := database.CreateWorker("worker-1", "test-worker", protocol.WorkerCapabilities{
		Encoders:      []string{"libx264"},
		FFmpegVersion: "5.0",
		MaxConcurrent: 1,
	}); err != nil {
		t.Fatalf("Failed to create worker: %v", err)
	}
	// The pending job's deadline is in the future, so only the live-worker
	// short-circuit keeps the wake from being armed.
	pendingJob(t, database)

	timer := time.NewTimer(time.Hour)
	timer.Stop()
	s.armStarvationWake(timer)

	if timer.Stop() {
		t.Fatal("wake was armed while a live schedulable worker exists: the sweep cannot judge, so the anchor lookup is wasted")
	}
}

// Two pending jobs with different deadlines: the wake must be armed for the
// OLDEST one's deadline, because the sweep fails every pending job past the
// cutoff. Arming for the newest would let the oldest starve until the next
// check tick — with the hour-long tick here, never — so this test fails under
// a DESC-ordered anchor.
func TestStarvationWakeJudgesOldestOfMultiplePending(t *testing.T) {
	database, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("Failed to create database: %v", err)
	}
	defer database.Close()

	s := New(database, Config{
		ScheduleInterval:     time.Hour,
		TimeoutCheckInterval: time.Hour, // the expiry wake is the only trigger
		MaxJobsPerWorker:     1,
		NoWorkerJobTimeout:   3 * time.Second,
		HeartbeatFreshness:   90 * time.Second,
	})

	offlineWorkerFixture(t, database)
	oldest := pendingJob(t, database)
	// The newest job expires a minute out; a wake armed for it (a DESC-ordered
	// anchor) would judge the oldest only after the newest deadline, which this
	// 20s window cannot observe.
	newest := pendingJob(t, database)

	notifyCh, cancel := database.JobNotifier().Subscribe(oldest.ID)
	defer cancel()

	s.Start()
	defer s.Stop()

	deadline := time.Now().Add(2 * time.Second)
	backdatePendingAnchor(t, database, oldest.ID, deadline.Add(-s.config.NoWorkerJobTimeout))
	backdatePendingAnchor(t, database, newest.ID, time.Now().Add(57*time.Second))
	s.TriggerReschedule()

	select {
	case <-notifyCh:
	case <-time.After(20 * time.Second):
		t.Fatalf("the oldest pending job was not judged within 20s: the wake must be armed for the earliest pending deadline, not a newer one")
	}

	bound := deadline.Add(starvationExpiryMargin + 4*time.Second)
	if verdictAt := time.Now(); verdictAt.After(bound) {
		t.Errorf("verdict at %s, more than %s past the oldest job's deadline %s: it fell back to the check interval instead of the expiry wake", verdictAt, starvationExpiryMargin+4*time.Second, deadline)
	}
	assertStarvedNoWorker(t, database, oldest.ID)
}
