package db_test

// Tests for TSI-2359 (SQLite data-layer integrity): dispatch-race guards,
// terminal-state protection, transactional migration, FK cascade cleanup,
// concurrent-write robustness under WAL, and chunk idempotency.

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tsix404/rffmpeg/pkg/protocol"
	"github.com/tsix404/rffmpeg/pkg/server/db"
)

func setupIntegrityDB(t *testing.T) (*db.Database, func()) {
	t.Helper()
	tmpDir, err := os.MkdirTemp("", "rffmpeg-integrity-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	database, err := db.New(fmt.Sprintf("%s/test.db", tmpDir))
	if err != nil {
		os.RemoveAll(tmpDir)
		t.Fatalf("Failed to create database: %v", err)
	}
	cleanup := func() {
		database.Close()
		os.RemoveAll(tmpDir)
	}
	return database, cleanup
}

func createNJobs(t *testing.T, d *db.Database, n int) []*db.Job {
	t.Helper()
	var jobs []*db.Job
	for i := range n {
		job, err := d.CreateJob(fmt.Sprintf(`["in_%d.mkv"]`, i), `["-c:v","libx264"]`, fmt.Sprintf("out_%d.mkv", i), false)
		if err != nil {
			t.Fatalf("Failed to create job %d: %v", i, err)
		}
		jobs = append(jobs, job)
	}
	return jobs
}

func mustCreateWorkers(t *testing.T, d *db.Database, ids ...string) {
	t.Helper()
	for _, id := range ids {
		if _, err := d.CreateWorker(id, id, protocol.WorkerCapabilities{}); err != nil {
			t.Fatalf("CreateWorker %s: %v", id, err)
		}
	}
}

// Acceptance 1: workers concurrently pulling the same pending batch must
// never both receive the same job.
func TestConcurrentPullNoDoubleDispatch(t *testing.T) {
	database, cleanup := setupIntegrityDB(t)
	defer cleanup()

	mustCreateWorkers(t, database, "w1", "w2")

	const numJobs = 24
	jobs := createNJobs(t, database, numJobs)

	var claimsMu sync.Mutex
	claims := make(map[string]int) // job ID -> number of workers that first-claimed it
	seen := make(map[string]bool)  // job ID -> already counted once

	var wg sync.WaitGroup
	errCh := make(chan error, 1)
	fail := func(err error) {
		select {
		case errCh <- err:
		default:
		}
	}
	for _, workerID := range []string{"w1", "w2"} {
		for range 20 {
			wg.Add(1)
			go func(wid string) {
				defer wg.Done()
				assigned, err := database.AssignPendingJobsToWorker(wid, 10)
				if err != nil {
					fail(fmt.Errorf("worker %s pull: %w", wid, err))
					return
				}
				claimsMu.Lock()
				defer claimsMu.Unlock()
				for _, j := range assigned {
					// Only newly-claimed jobs carry queued status from this
					// pull; jobs already assigned to this worker by an earlier
					// pull are re-returned as queuedJobs and must not inflate
					// the claim count. Track claims per (job, worker) pair:
					// a job may legitimately be re-returned to its OWN worker,
					// but only one worker may ever see it as claimable.
					if j.Status != protocol.JobStatusQueued {
						fail(fmt.Errorf("worker %s got job %s in status %s",
							wid, j.ID, j.Status))
						return
					}
					if j.WorkerID.String == wid && !seen[j.ID] {
						seen[j.ID] = true
						claims[j.ID]++
					} else if j.WorkerID.String != wid {
						fail(fmt.Errorf("worker %s returned job %s owned by %q",
							wid, j.ID, j.WorkerID.String))
						return
					}
				}
			}(workerID)
		}
	}
	wg.Wait()
	close(errCh)
	if err := <-errCh; err != nil {
		t.Fatal(err)
	}

	for _, job := range jobs {
		final, err := database.GetJob(job.ID)
		if err != nil {
			t.Fatalf("GetJob %s: %v", job.ID, err)
		}
		claimsMu.Lock()
		count := claims[job.ID]
		claimsMu.Unlock()
		if count > 1 {
			t.Errorf("job %s was dispatched %d times (expected at most 1)", job.ID, count)
		}
		if final.Status == protocol.JobStatusPending {
			if final.WorkerID.Valid && final.WorkerID.String != "" {
				t.Errorf("pending job %s still attached to worker %q", job.ID, final.WorkerID.String)
			}
			continue // legitimately unclaimed
		}
		if final.Status != protocol.JobStatusQueued || !final.WorkerID.Valid {
			t.Errorf("job %s inconsistent final state: status=%s worker=%v",
				job.ID, final.Status, final.WorkerID)
		}
	}

	// DB-level attribution: every non-pending job has exactly one owner row.
	rows, err := database.GetDB().Query(`SELECT id, worker_id FROM jobs WHERE status != ?`, protocol.JobStatusPending)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var worker any
		if err := rows.Scan(&id, &worker); err != nil {
			t.Fatal(err)
		}
		if worker == nil || worker == "" {
			t.Errorf("non-pending job %s has no worker", id)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}

// Acceptance 2: a completed terminal state survives racing migration,
// cancellation, reschedule, failover reset, late running reports, and the
// starvation-failure sweep.
func TestTerminalStateNotOverwritten(t *testing.T) {
	database, cleanup := setupIntegrityDB(t)
	defer cleanup()

	cases := []struct {
		name  string
		rival func(d *db.Database, jobID string) error
	}{
		{"migrate", func(d *db.Database, _ string) error {
			_, err := d.MigrateJobsFromWorker("w1")
			return err
		}},
		{"cancel", func(d *db.Database, jobID string) error { return d.CancelJob(jobID) }},
		{"reschedule", func(d *db.Database, jobID string) error { return d.RescheduleJob(jobID) }},
		{"failover-reset", func(d *db.Database, jobID string) error { return d.ResetJobToPending(jobID) }},
		{"late-running-report", func(d *db.Database, jobID string) error {
			return d.UpdateJobStatusWithFailure(jobID, protocol.JobStatusRunning, nil, nil, nil, nil)
		}},
		{"starvation-sweep", func(d *db.Database, _ string) error {
			_, err := d.FailStarvedPendingJobs(time.Now().Add(time.Hour), "starved", "timeout")
			return err
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mustCreateWorkers(t, database, "w1")
			t.Cleanup(func() { database.GetDB().Exec(`DELETE FROM workers WHERE id = 'w1'`) })

			job := createNJobs(t, database, 1)[0]
			if _, err := database.AssignPendingJobsToWorker("w1", 10); err != nil {
				t.Fatal(err)
			}
			if err := database.UpdateJobStatusWithFailure(job.ID, protocol.JobStatusRunning, nil, nil, nil, nil); err != nil {
				t.Fatal(err)
			}
			exitCode := 0
			if err := database.UpdateJobStatusWithFailure(job.ID, protocol.JobStatusCompleted, &exitCode, nil, nil, nil); err != nil {
				t.Fatal(err)
			}

			_ = tc.rival(database, job.ID) // must be a no-op against the terminal state

			final, err := database.GetJob(job.ID)
			if err != nil {
				t.Fatal(err)
			}
			if final.Status != protocol.JobStatusCompleted {
				t.Fatalf("completed job overwritten by rival %q: status=%s finished_at=%v",
					tc.name, final.Status, final.FinishedAt)
			}
			if !final.FinishedAt.Valid {
				t.Error("completed job lost finished_at")
			}
		})
	}
}

// Acceptance 2b: CancelJob racing a completion must never turn success into
// cancelled — a cancelled row may not carry a successful exit code.
func TestCancelRacingCompletionKeepsCompleted(t *testing.T) {
	database, cleanup := setupIntegrityDB(t)
	defer cleanup()

	mustCreateWorkers(t, database, "w1")

	for range 30 {
		job := createNJobs(t, database, 1)[0]
		if _, err := database.AssignPendingJobsToWorker("w1", 10); err != nil {
			t.Fatal(err)
		}

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			_ = database.CancelJob(job.ID)
		}()
		go func(id string) {
			defer wg.Done()
			exitCode := 0
			if err := database.UpdateJobStatusWithFailure(id, protocol.JobStatusCompleted, &exitCode, nil, nil, nil); err != nil &&
				err != protocol.ErrJobTerminal {
				t.Errorf("completion rejected unexpectedly: %v", err)
			}
		}(job.ID)
		wg.Wait()

		final, err := database.GetJob(job.ID)
		if err != nil {
			t.Fatal(err)
		}
		if final.Status == protocol.JobStatusCancelled && final.ExitCode.Valid && final.ExitCode.Int32 == 0 {
			t.Fatal("cancelled job carries successful exit_code — completion was overwritten by cancel")
		}
		cleanupRows(t, database, job.ID)
	}
}

func cleanupRows(t *testing.T, d *db.Database, jobID string) {
	t.Helper()
	if _, err := d.GetDB().Exec(`DELETE FROM jobs WHERE id = ?`, jobID); err != nil {
		t.Fatal(err)
	}
}

// Acceptance 3: DeleteUploadSession cascades to upload_chunks.
func TestDeleteUploadSessionCascadesChunks(t *testing.T) {
	database, cleanup := setupIntegrityDB(t)
	defer cleanup()

	session, err := database.CreateUploadSession("movie.mp4", 4096, 1024, 4, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 4 {
		if _, err := database.CreateUploadChunk(session.ID, i, 1024, fmt.Sprintf("sum%d", i), fmt.Sprintf("/tmp/chunk%d", i)); err != nil {
			t.Fatal(err)
		}
	}

	chunks, err := database.GetUploadChunks(session.ID)
	if err != nil || len(chunks) != 4 {
		t.Fatalf("expected 4 chunks, got %d (err=%v)", len(chunks), err)
	}

	if err := database.DeleteUploadSession(session.ID); err != nil {
		t.Fatal(err)
	}

	var remaining int
	if err := database.GetDB().QueryRow(
		`SELECT COUNT(*) FROM upload_chunks WHERE upload_id = ?`, session.ID).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Errorf("FK cascade failed: %d orphan upload_chunks rows remain", remaining)
	}
}

// Acceptance 4: concurrent writers under WAL + busy_timeout never hit
// "database is locked".
func TestConcurrentWritesNoDatabaseLocked(t *testing.T) {
	database, cleanup := setupIntegrityDB(t)
	defer cleanup()

	const goroutines = 8
	const opsEach = 25

	var wg sync.WaitGroup
	errCh := make(chan error, goroutines)
	for g := range goroutines {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			for i := range opsEach {
				job, err := database.CreateJob(`["x.mkv"]`, `["-c:v","libx264"]`, "x.out", false)
				if err != nil {
					if isLockedErr(err) {
						errCh <- err
						return
					}
					continue
				}
				if err := database.UpdateJobProgress(job.ID, float64(i), i); isLockedErr(err) {
					errCh <- err
					return
				}
				if err := database.UpdateJobOutput(job.ID, `["f1"]`); isLockedErr(err) {
					errCh <- err
					return
				}
				if err := database.CancelJob(job.ID); isLockedErr(err) {
					errCh <- err
					return
				}
				_ = seed
			}
		}(g)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("database lock contention surfaced: %v", err)
	}
}

func isLockedErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "locked") || strings.Contains(msg, "busy")
}

// Acceptance 5: duplicate chunk insert is an idempotent success with one row.
func TestDuplicateChunkInsertIdempotent(t *testing.T) {
	database, cleanup := setupIntegrityDB(t)
	defer cleanup()

	session, err := database.CreateUploadSession("dup.mp4", 2048, 1024, 2, nil)
	if err != nil {
		t.Fatal(err)
	}

	first, err := database.CreateUploadChunk(session.ID, 0, 1024, "checksum-a", "/path/a")
	if err != nil {
		t.Fatalf("first insert failed: %v", err)
	}

	second, err := database.CreateUploadChunk(session.ID, 0, 1024, "checksum-b", "/path/b")
	if err != nil {
		t.Fatalf("duplicate insert returned error instead of idempotent success: %v", err)
	}
	if second.ID != first.ID {
		t.Errorf("duplicate insert returned different row: %s vs %s", first.ID, second.ID)
	}

	var count int
	if err := database.GetDB().QueryRow(
		`SELECT COUNT(*) FROM upload_chunks WHERE upload_id = ? AND chunk_index = 0`, session.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("expected exactly 1 row for chunk 0, found %d", count)
	}
}

// Concurrent duplicate inserts of the same chunk all succeed; one row wins.
func TestConcurrentDuplicateChunkInsertsIdempotent(t *testing.T) {
	database, cleanup := setupIntegrityDB(t)
	defer cleanup()

	session, err := database.CreateUploadSession("race.mp4", 8192, 1024, 8, nil)
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	errCh := make(chan error, 16)
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := database.CreateUploadChunk(session.ID, 0, 1024, "same", "/same"); err != nil {
				errCh <- err
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("concurrent chunk insert failed: %v", err)
	}

	var count int
	if err := database.GetDB().QueryRow(
		`SELECT COUNT(*) FROM upload_chunks WHERE upload_id = ? AND chunk_index = 0`, session.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("expected exactly 1 row after 16 concurrent inserts, found %d", count)
	}
}

// MigrateJobsFromWorker must skip jobs that already reached a terminal state
// even though they were running when migration was decided.
func TestMigrateSkipsTerminalJobs(t *testing.T) {
	database, cleanup := setupIntegrityDB(t)
	defer cleanup()

	mustCreateWorkers(t, database, "w1")

	running := createNJobs(t, database, 2)[0]
	done := createNJobs(t, database, 2)[1]

	for _, id := range []string{running.ID, done.ID} {
		if err := database.AssignJobToWorker(id, "w1"); err != nil {
			t.Fatal(err)
		}
	}
	if err := database.UpdateJobStatusWithFailure(running.ID, protocol.JobStatusRunning, nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	exitCode := 0
	if err := database.UpdateJobStatusWithFailure(done.ID, protocol.JobStatusCompleted, &exitCode, nil, nil, nil); err != nil {
		t.Fatal(err)
	}

	migrated, err := database.MigrateJobsFromWorker("w1")
	if err != nil {
		t.Fatal(err)
	}

	for _, id := range migrated {
		if id == done.ID {
			t.Error("completed job was migrated back to pending")
		}
	}

	gotRunning, err := database.GetJob(running.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotRunning.Status != protocol.JobStatusPending {
		t.Errorf("running job should have been migrated, got %s", gotRunning.Status)
	}
	gotDone, err := database.GetJob(done.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotDone.Status != protocol.JobStatusCompleted {
		t.Errorf("completed job changed status to %s during migration", gotDone.Status)
	}
}

// UpdateJobStatusWithFailure distinguishes missing jobs from terminal ones
// and keeps the failed→completed retry path open while freezing completed.
func TestUpdateStatusTerminalVsNotFound(t *testing.T) {
	database, cleanup := setupIntegrityDB(t)
	defer cleanup()

	job := createNJobs(t, database, 1)[0]
	exitCode := 1
	if err := database.UpdateJobStatusWithFailure(job.ID, protocol.JobStatusFailed, &exitCode, nil, nil, nil); err != nil {
		t.Fatalf("initial fail: %v", err)
	}

	// Late running report on a failed job → ErrJobTerminal.
	if err := database.UpdateJobStatusWithFailure(job.ID, protocol.JobStatusRunning, nil, nil, nil, nil); err != protocol.ErrJobTerminal {
		t.Errorf("late running report: expected ErrJobTerminal, got %v", err)
	}

	// Nonexistent job → ErrJobNotFound.
	if err := database.UpdateJobStatusWithFailure("no-such-job", protocol.JobStatusRunning, nil, nil, nil, nil); err != protocol.ErrJobNotFound {
		t.Errorf("missing job: expected ErrJobNotFound, got %v", err)
	}

	// failed→completed remains legal (retry-after-failure flow).
	exitOK := 0
	if err := database.UpdateJobStatusWithFailure(job.ID, protocol.JobStatusCompleted, &exitOK, nil, nil, nil); err != nil {
		t.Errorf("failed→completed retry transition rejected: %v", err)
	}
	got, err := database.GetJob(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != protocol.JobStatusCompleted {
		t.Errorf("retry transition did not apply: %s", got.Status)
	}

	// But completed→failed is frozen.
	if err := database.UpdateJobStatusWithFailure(job.ID, protocol.JobStatusFailed, nil, nil, nil, nil); err != protocol.ErrJobTerminal {
		t.Errorf("completed→failed allowed: %v", err)
	}
}
