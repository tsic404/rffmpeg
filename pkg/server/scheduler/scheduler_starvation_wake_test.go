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

// A pending job whose no-worker deadline is already past when the loop starts
// has no future instant to wake on. It must be judged immediately by the arm
// path, not queued behind the next timeout tick: the tick may be up to a full
// interval away, which is enough to push the verdict past the acceptance
// window. The hour-long tick makes the arm the only possible trigger here.
func TestStarvationDeadlineAlreadyDueIsJudgedAtArm(t *testing.T) {
	database, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("Failed to create database: %v", err)
	}
	defer database.Close()

	s := New(database, Config{
		ScheduleInterval:     time.Hour,
		TimeoutCheckInterval: time.Hour, // the arm path is the only trigger
		MaxJobsPerWorker:     1,
		NoWorkerJobTimeout:   time.Minute,
		HeartbeatFreshness:   90 * time.Second,
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
	case <-time.After(5 * time.Second):
		t.Fatal("a due deadline was left to the timeout tick instead of being judged while arming")
	}
	assertStarvedNoWorker(t, database, job.ID)
}

// The expiry wake is an accelerator, never the only path. A pending job whose
// deadline passes while the wake could not be armed — a live worker kept it
// disarmed and no reschedule signal fires afterwards — must still be judged by
// the periodic timeout tick's sweep (which runs before that tick re-arms).
func TestStarvationTickFallbackJudgesAfterWakeCouldNotArm(t *testing.T) {
	database, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("Failed to create database: %v", err)
	}
	defer database.Close()

	s := New(database, Config{
		ScheduleInterval:     time.Hour,
		TimeoutCheckInterval: 50 * time.Millisecond,
		MaxJobsPerWorker:     1,
		NoWorkerJobTimeout:   time.Second,
		HeartbeatFreshness:   90 * time.Second,
	})

	// A live worker keeps the wake disarmed at loop start.
	if _, err := database.CreateWorker("worker-1", "test-worker", protocol.WorkerCapabilities{
		Encoders:      []string{"libx264"},
		FFmpegVersion: "5.0",
		MaxConcurrent: 1,
	}); err != nil {
		t.Fatalf("Failed to create worker: %v", err)
	}
	job := pendingJob(t, database)

	notifyCh, cancel := database.JobNotifier().Subscribe(job.ID)
	defer cancel()

	// The arm's due path and the tick sweep both end in the same terminal
	// state; the trigger is what pins which path judged.
	triggers := make(chan starvationTrigger, 4)
	s.starvationObserver = func(trigger starvationTrigger) { triggers <- trigger }

	s.Start()
	defer s.Stop()

	// Let the loop start and run its initial arm while the worker is still
	// live (the wake stays disarmed). The trigger assertion below still holds
	// if this sleep loses the race: the startup arm would report deadline_due.
	time.Sleep(50 * time.Millisecond)

	// The worker dies and the deadline passes. Nothing triggers a reschedule,
	// so the tick sweep is the first path that can judge.
	if err := database.UpdateWorkerStatus("worker-1", protocol.WorkerStatusOffline); err != nil {
		t.Fatalf("Failed to mark worker offline: %v", err)
	}
	backdatePendingAnchor(t, database, job.ID, time.Now().Add(-2*time.Second))

	select {
	case <-notifyCh:
	case <-time.After(10 * time.Second):
		t.Fatal("timeout tick did not judge a due job whose wake could not be armed")
	}
	select {
	case trigger := <-triggers:
		if trigger != starvationTriggerTick {
			t.Fatalf("verdict trigger = %q, want %q: the tick fallback must judge, not the arm's due path", trigger, starvationTriggerTick)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("verdict recorded no trigger")
	}
	assertStarvedNoWorker(t, database, job.ID)
}

// A crash migration requeues the job (ResetJobToPending refreshes created_at)
// and signals the scheduler. That path must arm the expiry wake: with the tick
// an hour out, only the wake can produce the verdict just past the re-anchored
// deadline. This is the scheduling sequence behind the WORKER_CRASH branch of
// the acceptance case, pinned without the real heartbeat timeout.
func TestStarvationWakeJudgesCrashRequeuedJob(t *testing.T) {
	database, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("Failed to create database: %v", err)
	}
	defer database.Close()

	const noWorkerTimeout = 2 * time.Second
	s := New(database, Config{
		ScheduleInterval:     time.Hour,
		TimeoutCheckInterval: time.Hour, // the expiry wake is the only trigger
		MaxJobsPerWorker:     1,
		NoWorkerJobTimeout:   noWorkerTimeout,
		HeartbeatFreshness:   90 * time.Second,
	})

	if _, err := database.CreateWorker("worker-1", "test-worker", protocol.WorkerCapabilities{
		Encoders:      []string{"libx264"},
		FFmpegVersion: "5.0",
		MaxConcurrent: 1,
	}); err != nil {
		t.Fatalf("Failed to create worker: %v", err)
	}
	job := pendingJob(t, database)
	if err := database.AssignJobToWorker(job.ID, "worker-1"); err != nil {
		t.Fatalf("Failed to assign job: %v", err)
	}
	if _, err := database.CreateMigrationEvent(
		"worker-1", "test-worker", "heartbeat_timeout", 1, []string{job.ID}, 1,
	); err != nil {
		t.Fatalf("Failed to create migration event: %v", err)
	}
	if err := database.UpdateWorkerStatus("worker-1", protocol.WorkerStatusOffline); err != nil {
		t.Fatalf("Failed to mark worker offline: %v", err)
	}

	notifyCh, cancel := database.JobNotifier().Subscribe(job.ID)
	defer cancel()

	triggers := make(chan starvationTrigger, 4)
	s.starvationObserver = func(trigger starvationTrigger) { triggers <- trigger }

	s.Start()
	defer s.Stop()

	// The monitor's failover sequence: requeue (refreshes the pending anchor)
	// then signal the scheduler.
	if err := database.ResetJobToPending(job.ID); err != nil {
		t.Fatalf("Failed to requeue job: %v", err)
	}
	requeuedAt := time.Now()
	s.TriggerReschedule()

	select {
	case <-notifyCh:
	case <-time.After(20 * time.Second):
		t.Fatal("the requeued job was not judged: the reschedule did not arm the expiry wake")
	}
	select {
	case trigger := <-triggers:
		if trigger != starvationTriggerWake {
			t.Fatalf("verdict trigger = %q, want %q: the requeued job's deadline must be judged by the expiry wake", trigger, starvationTriggerWake)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("verdict recorded no trigger")
	}

	deadline := requeuedAt.Add(noWorkerTimeout + starvationExpiryMargin)
	if verdictAt := time.Now(); verdictAt.After(deadline.Add(starvationExpiryMargin + 4*time.Second)) {
		t.Errorf("verdict at %s, more than %s past the requeued job's deadline %s: it fell back to the check interval instead of the expiry wake",
			verdictAt, starvationExpiryMargin+4*time.Second, deadline)
	}

	updated, err := database.GetJob(job.ID)
	if err != nil {
		t.Fatalf("Failed to get job: %v", err)
	}
	if updated.Status != protocol.JobStatusFailed {
		t.Errorf("Expected status failed, got %s", updated.Status)
	}
	if updated.FailureType != string(protocol.FailureWorkerCrash) {
		t.Errorf("Expected failure_type %q, got %q", protocol.FailureWorkerCrash, updated.FailureType)
	}
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
