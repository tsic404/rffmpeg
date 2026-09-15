package handlers_test

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/tsic404/rffmpeg/pkg/protocol"
	"github.com/tsic404/rffmpeg/pkg/server/db"
	"github.com/tsic404/rffmpeg/pkg/server/handlers"
	"github.com/tsic404/rffmpeg/pkg/server/migration"
	"github.com/tsic404/rffmpeg/pkg/server/scheduler"
	"github.com/tsic404/rffmpeg/pkg/server/storage"
	"github.com/tsic404/rffmpeg/pkg/server/workerhealth"
)

// setupStarvationSweepE2E wires the full server stack — handler, scheduler, and
// worker-health monitor — with accelerated timeouts so the no-worker starvation
// sweep can be observed end-to-end in about a second of wall-clock instead of
// the production 2-minute --no-worker-job-timeout. The wiring mirrors main.go:
// the monitor triggers the scheduler on migration, and the handler's starvation
// knobs match the scheduler's so job responses carry NoWorkerDeadline.
func setupStarvationSweepE2E(t *testing.T) (*handlers.Handler, *chi.Mux, *db.Database, *scheduler.Scheduler, *workerhealth.Monitor, func()) {
	t.Helper()

	tmpDir, err := os.MkdirTemp("", "rffmpeg-starvation-e2e-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}

	database, err := db.New(filepath.Join(tmpDir, "test.db"))
	if err != nil {
		os.RemoveAll(tmpDir)
		t.Fatalf("Failed to create database: %v", err)
	}

	store, err := storage.New(tmpDir)
	if err != nil {
		database.Close()
		os.RemoveAll(tmpDir)
		t.Fatalf("Failed to create storage: %v", err)
	}

	stateTable := workerhealth.NewWorkerStateTable(30 * time.Second)
	h := handlers.New(database, store, "test", stateTable)
	h.SetAuthToken("test-token")
	// Generous submit-time freshness window so submission is accepted while the
	// worker is live; the monitor below uses a much shorter window to detect
	// the kill quickly.
	h.SetHeartbeatTimeout(3 * time.Second)
	h.SetStarvationConfig(500*time.Millisecond, 100*time.Millisecond)
	h.SetMaxJobsPerWorker(1)

	sched := scheduler.New(database, scheduler.Config{
		JobTimeout:           30 * time.Minute,
		ScheduleInterval:     100 * time.Millisecond,
		TimeoutCheckInterval: 100 * time.Millisecond,
		MaxJobsPerWorker:     1,
		NoWorkerJobTimeout:   500 * time.Millisecond,
		HeartbeatFreshness:   3 * time.Second,
		MaxTimeoutRetries:    2,
	})

	monitor := workerhealth.New(database, workerhealth.Config{
		HeartbeatTimeout:    1 * time.Second,
		OfflineThreshold:    1 * time.Minute,
		HealthCheckInterval: 100 * time.Millisecond,
		MaxRetryCount:       3,
	})
	monitor.SetScheduler(sched)

	r := chi.NewRouter()
	r.Route("/api/v1", func(r chi.Router) {
		r.Post("/upload", h.Upload)
		r.Post("/jobs", h.SubmitJob)
		r.Get("/jobs/{jobId}", h.GetJob)
		r.Delete("/jobs/{jobId}", h.CancelJob)
		r.Post("/workers/register", h.RegisterWorker)
		r.Post("/workers/heartbeat", h.WorkerHeartbeat)
		r.Get("/workers/{workerId}/jobs", h.PullWorkerJobs)
		r.Get("/migrations", h.ListMigrationEvents)
	})

	sched.Start()
	monitor.Start()

	cleanup := func() {
		monitor.Stop()
		sched.Stop()
		database.Close()
		os.RemoveAll(tmpDir)
	}

	return h, r, database, sched, monitor, cleanup
}

func starvationRegisterWorker(t *testing.T, router *chi.Mux, workerID, name string) {
	t.Helper()
	regReq := protocol.WorkerRegisterRequest{
		WorkerID: workerID,
		Name:     name,
		Capabilities: protocol.WorkerCapabilities{
			Encoders:      []string{"libx264"},
			FFmpegVersion: "6.0",
			MaxConcurrent: 1,
		},
	}
	body, _ := json.Marshal(regReq)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/workers/register", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("worker register: status %d, body %s", w.Code, w.Body.String())
	}
}

func starvationHeartbeat(t *testing.T, router *chi.Mux, workerID string) {
	t.Helper()
	hbReq := protocol.WorkerHeartbeatRequest{WorkerID: workerID, Status: protocol.WorkerStatusIdle}
	body, _ := json.Marshal(hbReq)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/workers/heartbeat", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("worker heartbeat: status %d, body %s", w.Code, w.Body.String())
	}
}

func starvationSubmitJob(t *testing.T, router *chi.Mux) string {
	t.Helper()
	// Upload a placeholder input; its content is irrelevant because the worker
	// dies before ever executing ffmpeg on it.
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", "input.mp4")
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, err := part.Write([]byte("placeholder")); err != nil {
		t.Fatalf("write form file: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/upload", &body)
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", writer.FormDataContentType())
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("upload: status %d, body %s", w.Code, w.Body.String())
	}
	var uploadResp protocol.UploadResponse
	if err := json.NewDecoder(w.Body).Decode(&uploadResp); err != nil {
		t.Fatalf("decode upload response: %v", err)
	}

	jobReq := protocol.JobSubmitRequest{
		InputFiles:     []string{uploadResp.FileID},
		Args:           []string{"-c:v", "libx264"},
		OutputFilename: "output.mp4",
	}
	jobBody, err := json.Marshal(jobReq)
	if err != nil {
		t.Fatalf("marshal job request: %v", err)
	}
	req = httptest.NewRequest(http.MethodPost, "/api/v1/jobs", bytes.NewReader(jobBody))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("job submit: status %d, body %s", w.Code, w.Body.String())
	}
	var jobResp protocol.JobSubmitResponse
	if err := json.NewDecoder(w.Body).Decode(&jobResp); err != nil {
		t.Fatalf("decode submit response: %v", err)
	}
	if jobResp.JobID == "" {
		t.Fatalf("job submit returned empty job ID (status %d)", w.Code)
	}
	return jobResp.JobID
}

func starvationGetJob(t *testing.T, router *chi.Mux, jobID string) protocol.JobInfo {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/jobs/"+jobID, nil)
	req.Header.Set("Authorization", "Bearer test-token")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("get job %s: status %d, body %s", jobID, w.Code, w.Body.String())
	}
	var resp protocol.JobStatusResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode job response: %v", err)
	}
	return resp.Job
}

// starvationWaitForStatus polls GET /api/v1/jobs/{id} until the job reaches the
// wanted status or the deadline elapses, returning the final observed JobInfo.
func starvationWaitForStatus(t *testing.T, router *chi.Mux, jobID string, want protocol.JobStatus, timeout time.Duration) protocol.JobInfo {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		job := starvationGetJob(t, router, jobID)
		if job.Status == want {
			return job
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("job %s did not reach status %q within %s (last status %q)", jobID, want, timeout, starvationGetJob(t, router, jobID).Status)
	return protocol.JobInfo{}
}

// TestStarvationSweep_SubmitThenKillWorker_EndToEnd reproduces the QA gap: a
// job submitted while a worker is live, then the worker is killed before the
// job finishes. The end-to-end chain is submit -> scheduler assigns (queued) ->
// monitor detects the dead worker and migrates the job back to pending with a
// heartbeat_timeout (crash) event -> the no-worker starvation sweep fails it.
//
// The terminal classification is WORKER_CRASH, not NO_WORKER_AVAILABLE: the
// job reached pending via a worker crash, so the crash semantics survive the
// sweep (the `server reported no worker available` NO_WORKER_AVAILABLE message
// is reserved for genuinely starved jobs that never ran on a crashed worker).
func TestStarvationSweep_SubmitThenKillWorker_EndToEnd(t *testing.T) {
	_, router, database, _, _, cleanup := setupStarvationSweepE2E(t)
	defer cleanup()

	const workerID = "worker-1"
	starvationRegisterWorker(t, router, workerID, "crashy-worker")
	starvationHeartbeat(t, router, workerID)

	jobID := starvationSubmitJob(t, router)

	// Wait for the scheduler to claim the job for the live worker (pending ->
	// queued). This pins the scenario to "worker died while holding the job"
	// rather than the race where the worker dies before the job is ever claimed
	// (which classifies as NO_WORKER_AVAILABLE instead).
	starvationWaitForStatus(t, router, jobID, protocol.JobStatusQueued, 3*time.Second)

	// Kill the worker: refresh its heartbeat once so the kill has a clean
	// clock, then send no further heartbeats.
	starvationHeartbeat(t, router, workerID)

	final := starvationWaitForStatus(t, router, jobID, protocol.JobStatusFailed, 5*time.Second)

	if final.FailureType != string(protocol.FailureWorkerCrash) {
		t.Errorf("failure_type = %q, want %q", final.FailureType, protocol.FailureWorkerCrash)
	}
	if !strings.Contains(final.Error, "worker crashed (heartbeat timeout)") {
		t.Errorf("error = %q, want crash-starvation message", final.Error)
	}
	if !strings.Contains(final.Error, "no surviving worker") {
		t.Errorf("error = %q, want no-surviving-worker message", final.Error)
	}

	// The kill -> offline -> migrate transition must be observable as a
	// heartbeat_timeout migration event, proving the job took the crash path
	// rather than the never-claimed no-worker path.
	events, err := database.GetMigrationEventsByWorker(workerID, 10)
	if err != nil {
		t.Fatalf("list migration events: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("migration events = %d, want 1", len(events))
	}
	if events[0].Reason != string(migration.ReasonHeartbeatTimeout) {
		t.Errorf("migration reason = %q, want %q", events[0].Reason, migration.ReasonHeartbeatTimeout)
	}
	if events[0].JobsMigrated != 1 {
		t.Errorf("jobs_migrated = %d, want 1", events[0].JobsMigrated)
	}
}
