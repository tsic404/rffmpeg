package handlers_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/tsix404/rffmpeg/pkg/protocol"
	"github.com/tsix404/rffmpeg/pkg/server/auth"
	"github.com/tsix404/rffmpeg/pkg/server/db"
	"github.com/tsix404/rffmpeg/pkg/server/handlers"
	"github.com/tsix404/rffmpeg/pkg/server/ratelimit"
	"github.com/tsix404/rffmpeg/pkg/server/scheduler"
	"github.com/tsix404/rffmpeg/pkg/server/storage"
	"github.com/tsix404/rffmpeg/pkg/server/workerhealth"
)

// setupTestWithRateLimit creates a test environment with rate limit middleware applied.
func setupTestWithRateLimit(t *testing.T, rateLimit int) (*handlers.Handler, *chi.Mux, func()) {
	t.Helper()

	tmpDir, err := os.MkdirTemp("", "rffmpeg-ratelimit-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}

	database, err := db.New(fmt.Sprintf("%s/test.db", tmpDir))
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

	rateLimitCfg := &ratelimit.RuntimeConfig{
		Enabled: true,
		Limit:   rateLimit,
	}

	r := chi.NewRouter()
	r.Use(auth.Middleware("test-token"))

	r.Route("/api/v1", func(r chi.Router) {
		r.Post("/upload", h.Upload)
		r.Group(func(r chi.Router) {
			r.Use(ratelimit.JobSubmitMiddleware(h.GetRateLimiter(), rateLimitCfg))
			r.Post("/jobs", h.SubmitJob)
		})
		r.Get("/jobs/{jobId}", h.GetJob)
		r.Delete("/jobs/{jobId}", h.CancelJob)
		r.Patch("/jobs/{jobId}", h.UpdateJob)
		r.Get("/health", h.Health)
		r.Post("/workers/register", h.RegisterWorker)
	})

	cleanup := func() {
		database.Close()
		os.RemoveAll(tmpDir)
	}

	return h, r, cleanup
}

// setupTestWithScheduler creates a test environment with a scheduler for timeout testing.
func setupTestWithScheduler(t *testing.T, jobTimeout time.Duration) (*handlers.Handler, *chi.Mux, *db.Database, *scheduler.Scheduler, func()) {
	t.Helper()

	tmpDir, err := os.MkdirTemp("", "rffmpeg-timeout-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}

	database, err := db.New(fmt.Sprintf("%s/test.db", tmpDir))
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

	sched := scheduler.New(database, scheduler.Config{
		JobTimeout:           jobTimeout,
		ScheduleInterval:     100 * time.Millisecond,
		TimeoutCheckInterval: 100 * time.Millisecond,
		MaxJobsPerWorker:     1,
	})

	r := chi.NewRouter()
	r.Route("/api/v1", func(r chi.Router) {
		r.Post("/upload", h.Upload)
		r.Post("/jobs", h.SubmitJob)
		r.Get("/jobs/{jobId}", h.GetJob)
		r.Delete("/jobs/{jobId}", h.CancelJob)
		r.Patch("/jobs/{jobId}", h.UpdateJob)
		r.Post("/workers/register", h.RegisterWorker)
		r.Post("/workers/heartbeat", h.WorkerHeartbeat)
	})

	cleanup := func() {
		database.Close()
		os.RemoveAll(tmpDir)
	}

	return h, r, database, sched, cleanup
}

// makeAuthRequest creates an HTTP request with the test auth token.
func makeAuthRequest(method, path string, body []byte) *http.Request {
	var req *http.Request
	if body != nil {
		req = httptest.NewRequest(method, path, bytes.NewReader(body))
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-token")
	return req
}

// registerWorkerWithAuth registers a worker via the API with auth.
func registerWorkerWithAuth(t *testing.T, router *chi.Mux, workerID string, encoders []string) {
	t.Helper()
	workerReq := protocol.WorkerRegisterRequest{
		WorkerID: workerID,
		Name:     workerID,
		Capabilities: protocol.WorkerCapabilities{
			Encoders:      encoders,
			FFmpegVersion: "5.1",
		},
	}
	body, _ := json.Marshal(workerReq)
	req := makeAuthRequest("POST", "/api/v1/workers/register", body)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("Failed to register worker %s: status %d, body: %s", workerID, w.Code, w.Body.String())
	}
}

// uploadTestFile uploads a test file and returns the file ID.
func uploadTestFile(t *testing.T, router *chi.Mux) string {
	t.Helper()
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, _ := writer.CreateFormFile("file", "test.mp4")
	part.Write([]byte("test video content"))
	writer.Close()

	req := httptest.NewRequest("POST", "/api/v1/upload", body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Authorization", "Bearer test-token")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("Upload failed: status %d, body: %s", w.Code, w.Body.String())
	}

	var uploadResp protocol.UploadResponse
	if err := json.NewDecoder(w.Body).Decode(&uploadResp); err != nil {
		t.Fatalf("Failed to decode upload response: %v", err)
	}
	return uploadResp.FileID
}

// submitJobWithAuth submits a job and returns the response recorder.
func submitJobWithAuth(t *testing.T, router *chi.Mux, fileID string) *httptest.ResponseRecorder {
	t.Helper()
	jobReq := protocol.JobSubmitRequest{
		InputFiles: []string{fileID},
		Args:       []string{"-c:v", "libx264", "-preset", "fast"},
	}
	body, _ := json.Marshal(jobReq)
	req := makeAuthRequest("POST", "/api/v1/jobs", body)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w
}

// =============================================================================
// 7.2 Rate Limit Integration Tests
// =============================================================================

func TestIntegration_RateLimit_BasicExceeded(t *testing.T) {
	_, router, cleanup := setupTestWithRateLimit(t, 2)
	defer cleanup()

	registerWorkerWithAuth(t, router, "worker-1", []string{"libx264"})
	fileID := uploadTestFile(t, router)

	for i := 0; i < 2; i++ {
		w := submitJobWithAuth(t, router, fileID)
		if w.Code != http.StatusOK {
			t.Fatalf("Job %d should succeed, got status %d: %s", i+1, w.Code, w.Body.String())
		}
	}

	w := submitJobWithAuth(t, router, fileID)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("Expected 429, got %d: %s", w.Code, w.Body.String())
	}

	var resp protocol.RateLimitResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("Failed to decode rate limit response: %v", err)
	}
	if resp.Code != protocol.ErrCodeRateLimitExceeded {
		t.Errorf("Expected error code 'rate_limit_exceeded', got '%s'", resp.Code)
	}
	if resp.Limit != 2 {
		t.Errorf("Expected limit 2, got %d", resp.Limit)
	}
	if resp.Current != 2 {
		t.Errorf("Expected current 2, got %d", resp.Current)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Error("Expected Retry-After header to be set")
	}
}

func TestIntegration_RateLimit_ConcurrentSubmissions(t *testing.T) {
	_, router, setupCleanup := setupTestWithRateLimit(t, 3)
	defer setupCleanup()

	registerWorkerWithAuth(t, router, "worker-1", []string{"libx264"})
	fileID := uploadTestFile(t, router)

	totalJobs := 10
	var wg sync.WaitGroup
	var successCount int32
	var rateLimitedCount int32

	for i := 0; i < totalJobs; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := submitJobWithAuth(t, router, fileID)
			switch w.Code {
			case http.StatusOK:
				atomic.AddInt32(&successCount, 1)
			case http.StatusTooManyRequests:
				atomic.AddInt32(&rateLimitedCount, 1)
			default:
				t.Errorf("Unexpected status code %d: %s", w.Code, w.Body.String())
			}
		}()
	}
	wg.Wait()

	if successCount > 3 {
		t.Errorf("Expected at most 3 successful submissions, got %d", successCount)
	}
	if successCount+rateLimitedCount != int32(totalJobs) {
		t.Errorf("Expected all %d jobs accounted for, got accepted=%d rate-limited=%d",
			totalJobs, successCount, rateLimitedCount)
	}
}

func TestIntegration_RateLimit_ReleaseSlotOnCompletion(t *testing.T) {
	_, router, cleanup := setupTestWithRateLimit(t, 1)
	defer cleanup()

	registerWorkerWithAuth(t, router, "worker-1", []string{"libx264"})
	fileID := uploadTestFile(t, router)

	w := submitJobWithAuth(t, router, fileID)
	if w.Code != http.StatusOK {
		t.Fatalf("First job should succeed, got %d: %s", w.Code, w.Body.String())
	}
	var jobResp protocol.JobSubmitResponse
	json.NewDecoder(w.Body).Decode(&jobResp)

	w = submitJobWithAuth(t, router, fileID)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("Second job should be rate-limited, got %d: %s", w.Code, w.Body.String())
	}

	updateBody, _ := json.Marshal(protocol.JobUpdateRequest{
		Status:   protocol.JobStatusCompleted,
		ExitCode: 0,
	})
	req := makeAuthRequest("PATCH", fmt.Sprintf("/api/v1/jobs/%s", jobResp.JobID), updateBody)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("Job completion failed: %d: %s", w.Code, w.Body.String())
	}

	w = submitJobWithAuth(t, router, fileID)
	if w.Code != http.StatusOK {
		t.Errorf("Job after completion should succeed, got %d: %s", w.Code, w.Body.String())
	}
}

func TestIntegration_RateLimit_ReleaseSlotOnCancel(t *testing.T) {
	_, router, cleanup := setupTestWithRateLimit(t, 1)
	defer cleanup()

	registerWorkerWithAuth(t, router, "worker-1", []string{"libx264"})
	fileID := uploadTestFile(t, router)

	w := submitJobWithAuth(t, router, fileID)
	if w.Code != http.StatusOK {
		t.Fatalf("First job should succeed, got %d: %s", w.Code, w.Body.String())
	}
	var jobResp protocol.JobSubmitResponse
	json.NewDecoder(w.Body).Decode(&jobResp)

	w = submitJobWithAuth(t, router, fileID)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("Second job should be rate-limited, got %d", w.Code)
	}

	req := makeAuthRequest("DELETE", fmt.Sprintf("/api/v1/jobs/%s", jobResp.JobID), nil)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("Cancel failed: %d: %s", w.Code, w.Body.String())
	}

	w = submitJobWithAuth(t, router, fileID)
	if w.Code != http.StatusOK {
		t.Errorf("Job after cancel should succeed, got %d: %s", w.Code, w.Body.String())
	}
}

func TestIntegration_RateLimit_RollbackOnFailure(t *testing.T) {
	_, router, cleanup := setupTestWithRateLimit(t, 2)
	defer cleanup()

	registerWorkerWithAuth(t, router, "worker-1", []string{"libx264"})

	jobReq := protocol.JobSubmitRequest{
		InputFiles: []string{"non-existent-file-id"},
		Args:       []string{"-c:v", "libx264"},
	}
	body, _ := json.Marshal(jobReq)
	req := makeAuthRequest("POST", "/api/v1/jobs", body)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("Expected 400 for non-existent file, got %d: %s", w.Code, w.Body.String())
	}

	fileID := uploadTestFile(t, router)
	for i := 0; i < 2; i++ {
		w := submitJobWithAuth(t, router, fileID)
		if w.Code != http.StatusOK {
			t.Errorf("Job %d should succeed after rollback, got %d: %s", i+1, w.Code, w.Body.String())
		}
	}
}

func TestIntegration_RateLimit_ThrottleResponse(t *testing.T) {
	_, router, cleanup := setupTestWithRateLimit(t, 1)
	defer cleanup()

	registerWorkerWithAuth(t, router, "worker-1", []string{"libx264"})
	fileID := uploadTestFile(t, router)

	w := submitJobWithAuth(t, router, fileID)
	if w.Code != http.StatusOK {
		t.Fatalf("First job should succeed, got %d", w.Code)
	}

	w = submitJobWithAuth(t, router, fileID)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("Expected 429, got %d", w.Code)
	}

	var resp protocol.RateLimitResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("Failed to decode response: %v", err)
	}

	if resp.Code != protocol.ErrCodeRateLimitExceeded {
		t.Errorf("Expected code 'rate_limit_exceeded', got '%s'", resp.Code)
	}
	if resp.Current != 1 {
		t.Errorf("Expected current 1, got %d", resp.Current)
	}
	if resp.Limit != 1 {
		t.Errorf("Expected limit 1, got %d", resp.Limit)
	}
	if resp.RetryIn <= 0 {
		t.Errorf("Expected positive retry_in, got %d", resp.RetryIn)
	}
	if resp.Message == "" {
		t.Error("Expected non-empty message")
	}
}

// =============================================================================
// 7.3 Timeout Integration Tests
// =============================================================================

// TestIntegration_Timeout_BasicDetection verifies that GetTimedOutJobs correctly
// detects jobs that have exceeded the timeout window, and RescheduleJob resets
// them to pending.
func TestIntegration_Timeout_BasicDetection(t *testing.T) {
	database, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("Failed to create database: %v", err)
	}
	defer database.Close()

	_, err = database.CreateWorker("worker-1", "test-worker", protocol.WorkerCapabilities{
		Encoders:      []string{"libx264"},
		FFmpegVersion: "5.0",
		MaxConcurrent: 2,
	})
	if err != nil {
		t.Fatalf("Failed to create worker: %v", err)
	}

	job, err := database.CreateJob(`["file1.mp4"]`, `["-i", "input.mp4", "-c:v", "libx264"]`, "output.mp4", false)
	if err != nil {
		t.Fatalf("Failed to create job: %v", err)
	}

	err = database.AssignJobToWorker(job.ID, "worker-1")
	if err != nil {
		t.Fatalf("Failed to assign job: %v", err)
	}
	err = database.UpdateJobStatus(job.ID, protocol.JobStatusRunning, nil, nil)
	if err != nil {
		t.Fatalf("Failed to update job status: %v", err)
	}

	// Set started_at to 2 hours ago (simulate timeout)
	_, err = database.GetDB().Exec(
		`UPDATE jobs SET started_at = ? WHERE id = ?`,
		time.Now().Add(-2*time.Hour), job.ID,
	)
	if err != nil {
		t.Fatalf("Failed to set started_at: %v", err)
	}

	// Detect timed-out jobs (1 hour timeout)
	timedOut, err := database.GetTimedOutJobs(1 * time.Hour)
	if err != nil {
		t.Fatalf("GetTimedOutJobs failed: %v", err)
	}
	if len(timedOut) != 1 {
		t.Fatalf("Expected 1 timed out job, got %d", len(timedOut))
	}
	if timedOut[0].ID != job.ID {
		t.Errorf("Expected job %s, got %s", job.ID, timedOut[0].ID)
	}

	// Reschedule the job
	err = database.RescheduleJob(job.ID)
	if err != nil {
		t.Fatalf("RescheduleJob failed: %v", err)
	}

	// Verify job is back to pending with no worker
	updatedJob, err := database.GetJob(job.ID)
	if err != nil {
		t.Fatalf("Failed to get job: %v", err)
	}
	if updatedJob.Status != protocol.JobStatusPending {
		t.Errorf("Expected 'pending', got '%s'", updatedJob.Status)
	}
	if updatedJob.WorkerID.Valid {
		t.Error("Expected worker_id to be NULL after reschedule")
	}
	if updatedJob.StartedAt.Valid {
		t.Error("Expected started_at to be NULL after reschedule")
	}
}

// TestIntegration_Timeout_ConcurrentTimeouts verifies that multiple timed-out
// jobs are all correctly detected by GetTimedOutJobs in a single query.
func TestIntegration_Timeout_ConcurrentTimeouts(t *testing.T) {
	database, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("Failed to create database: %v", err)
	}
	defer database.Close()

	_, err = database.CreateWorker("worker-1", "test-worker", protocol.WorkerCapabilities{
		Encoders:      []string{"libx264"},
		FFmpegVersion: "5.0",
		MaxConcurrent: 10,
	})
	if err != nil {
		t.Fatalf("Failed to create worker: %v", err)
	}

	// Create 5 running jobs with old started_at, and 1 recent one
	oldJobIDs := make([]string, 5)
	for i := 0; i < 5; i++ {
		job, err := database.CreateJob(
			fmt.Sprintf(`["file%d.mp4"]`, i+1),
			fmt.Sprintf(`["-i", "input%d.mp4", "-c:v", "libx264"]`, i+1),
			fmt.Sprintf("output%d.mp4", i+1), false,
		)
		if err != nil {
			t.Fatalf("Failed to create job %d: %v", i+1, err)
		}
		oldJobIDs[i] = job.ID

		database.AssignJobToWorker(job.ID, "worker-1")
		database.UpdateJobStatus(job.ID, protocol.JobStatusRunning, nil, nil)
		database.GetDB().Exec(
			`UPDATE jobs SET started_at = ? WHERE id = ?`,
			time.Now().Add(-2*time.Hour), job.ID,
		)
	}

	// Create 1 recent running job (should NOT be timed out)
	recentJob, _ := database.CreateJob(`["recent.mp4"]`, `["args"]`, "recent_out.mp4", false)
	database.AssignJobToWorker(recentJob.ID, "worker-1")
	database.UpdateJobStatus(recentJob.ID, protocol.JobStatusRunning, nil, nil)
	// recentJob's started_at is set by UpdateJobStatus to now

	// Detect timed-out jobs
	timedOut, err := database.GetTimedOutJobs(1 * time.Hour)
	if err != nil {
		t.Fatalf("GetTimedOutJobs failed: %v", err)
	}
	if len(timedOut) != 5 {
		t.Fatalf("Expected 5 timed out jobs, got %d", len(timedOut))
	}

	// Verify the recent job is NOT in the timed-out list
	timedOutSet := make(map[string]bool)
	for _, j := range timedOut {
		timedOutSet[j.ID] = true
	}
	if timedOutSet[recentJob.ID] {
		t.Error("Recent job should not be in the timed-out list")
	}
	for _, id := range oldJobIDs {
		if !timedOutSet[id] {
			t.Errorf("Old job %s should be in the timed-out list", id)
		}
	}

	// Reschedule all timed-out jobs
	for _, j := range timedOut {
		database.RescheduleJob(j.ID)
	}

	// Verify all old jobs are now pending
	for _, id := range oldJobIDs {
		job, _ := database.GetJob(id)
		if job.Status != protocol.JobStatusPending {
			t.Errorf("Job %s: expected 'pending', got '%s'", id, job.Status)
		}
	}

	// Verify recent job is still running
	recentJobCheck, _ := database.GetJob(recentJob.ID)
	if recentJobCheck.Status != protocol.JobStatusRunning {
		t.Errorf("Recent job: expected 'running', got '%s'", recentJobCheck.Status)
	}
}

// TestIntegration_Timeout_NonTimedOutJobsUntouched verifies that jobs within
// the timeout window are not returned by GetTimedOutJobs.
func TestIntegration_Timeout_NonTimedOutJobsUntouched(t *testing.T) {
	database, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("Failed to create database: %v", err)
	}
	defer database.Close()

	database.CreateWorker("w1", "w1", protocol.WorkerCapabilities{
		Encoders: []string{"libx264"}, FFmpegVersion: "5.0",
	})

	job, _ := database.CreateJob(`["f.mp4"]`, `["args"]`, "o.mp4", false)
	database.AssignJobToWorker(job.ID, "w1")
	database.UpdateJobStatus(job.ID, protocol.JobStatusRunning, nil, nil)

	// 1 hour timeout — job just started, should not be timed out
	timedOut, err := database.GetTimedOutJobs(1 * time.Hour)
	if err != nil {
		t.Fatalf("GetTimedOutJobs failed: %v", err)
	}
	if len(timedOut) != 0 {
		t.Errorf("Expected 0 timed out jobs, got %d", len(timedOut))
	}
}

// TestIntegration_Timeout_MixedJobsInPool verifies that only timed-out jobs are
// detected while non-timed-out jobs in the same pool are left alone.
func TestIntegration_Timeout_MixedJobsInPool(t *testing.T) {
	database, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("Failed to create database: %v", err)
	}
	defer database.Close()

	database.CreateWorker("w1", "w1", protocol.WorkerCapabilities{
		Encoders: []string{"libx264"}, FFmpegVersion: "5.0", MaxConcurrent: 5,
	})

	// Old job (timed out)
	oldJob, _ := database.CreateJob(`["old.mp4"]`, `["args"]`, "old_o.mp4", false)
	database.AssignJobToWorker(oldJob.ID, "w1")
	database.UpdateJobStatus(oldJob.ID, protocol.JobStatusRunning, nil, nil)
	database.GetDB().Exec(
		`UPDATE jobs SET started_at = ? WHERE id = ?`,
		time.Now().Add(-2*time.Hour), oldJob.ID,
	)

	// New job (within timeout)
	newJob, _ := database.CreateJob(`["new.mp4"]`, `["args"]`, "new_o.mp4", false)
	database.AssignJobToWorker(newJob.ID, "w1")
	database.UpdateJobStatus(newJob.ID, protocol.JobStatusRunning, nil, nil)

	// Detect and reschedule timed-out jobs
	timedOut, _ := database.GetTimedOutJobs(1 * time.Hour)
	if len(timedOut) != 1 {
		t.Fatalf("Expected 1 timed out job, got %d", len(timedOut))
	}
	if timedOut[0].ID != oldJob.ID {
		t.Errorf("Expected old job, got %s", timedOut[0].ID)
	}

	database.RescheduleJob(oldJob.ID)

	// Verify: old job is pending, new job is still running
	oldCheck, _ := database.GetJob(oldJob.ID)
	if oldCheck.Status != protocol.JobStatusPending {
		t.Errorf("Old job: expected 'pending', got '%s'", oldCheck.Status)
	}
	newCheck, _ := database.GetJob(newJob.ID)
	if newCheck.Status != protocol.JobStatusRunning {
		t.Errorf("New job: expected 'running', got '%s'", newCheck.Status)
	}
}

// TestIntegration_Timeout_SchedulerDetectionAndReschedule verifies the full
// scheduler cycle: timeout detection -> reschedule -> re-assignment to idle worker.
// This tests that the scheduler loop correctly picks up timed-out jobs and
// re-schedules them (jobs cycle through pending -> queued -> running).
func TestIntegration_Timeout_SchedulerDetectionAndReschedule(t *testing.T) {
	_, _, database, sched, cleanup := setupTestWithScheduler(t, 1*time.Second)
	defer cleanup()

	database.CreateWorker("worker-1", "test-worker", protocol.WorkerCapabilities{
		Encoders: []string{"libx264"}, FFmpegVersion: "5.0", MaxConcurrent: 1,
	})

	job, _ := database.CreateJobWithStreaming(
		`["file1.mp4"]`, `["-i", "input.mp4", "-c:v", "libx264"]`,
		"output.mp4", false, false, nil, "[]",
	)
	database.AssignJobToWorker(job.ID, "worker-1")
	database.UpdateJobStatus(job.ID, protocol.JobStatusRunning, nil, nil)
	database.GetDB().Exec(
		`UPDATE jobs SET started_at = ? WHERE id = ?`,
		time.Now().Add(-2*time.Hour), job.ID,
	)

	sched.Start()
	defer sched.Stop()

	// Wait for timeout detection + re-assignment cycle
	// The scheduler will: detect timeout -> reschedule to pending -> re-assign to worker -> set queued
	time.Sleep(500 * time.Millisecond)

	// The job should have been rescheduled and re-assigned.
	// After the full cycle, it should be queued or running (worker re-accepted it).
	updatedJob, err := database.GetJob(job.ID)
	if err != nil {
		t.Fatalf("Failed to get job: %v", err)
	}

	// The key verification: the job was rescheduled (started_at reset) and then
	// re-assigned by the scheduler. It should now be queued (waiting for worker pickup).
	if updatedJob.Status != protocol.JobStatusQueued && updatedJob.Status != protocol.JobStatusPending {
		t.Errorf("Expected 'queued' or 'pending' after timeout-reschedule cycle, got '%s'", updatedJob.Status)
	}

	// If re-queued, verify it's assigned to the worker
	if updatedJob.Status == protocol.JobStatusQueued {
		if !updatedJob.WorkerID.Valid || updatedJob.WorkerID.String != "worker-1" {
			t.Error("Re-queued job should be assigned to worker-1")
		}
	}
}

// TestIntegration_Timeout_PendingJobsIgnored verifies that pending jobs
// (never started) are not affected by timeout detection.
func TestIntegration_Timeout_PendingJobsIgnored(t *testing.T) {
	database, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("Failed to create database: %v", err)
	}
	defer database.Close()

	// Create a pending job (no worker, no started_at)
	job, _ := database.CreateJob(`["f.mp4"]`, `["args"]`, "o.mp4", false)

	timedOut, err := database.GetTimedOutJobs(1 * time.Second)
	if err != nil {
		t.Fatalf("GetTimedOutJobs failed: %v", err)
	}
	if len(timedOut) != 0 {
		t.Errorf("Expected 0 timed out pending jobs, got %d", len(timedOut))
	}

	// Job should still be pending
	check, _ := database.GetJob(job.ID)
	if check.Status != protocol.JobStatusPending {
		t.Errorf("Expected 'pending', got '%s'", check.Status)
	}
}

// TestIntegration_Timeout_CompletedJobsIgnored verifies that completed jobs
// are not returned as timed out, even if their started_at is old.
func TestIntegration_Timeout_CompletedJobsIgnored(t *testing.T) {
	database, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("Failed to create database: %v", err)
	}
	defer database.Close()

	database.CreateWorker("w1", "w1", protocol.WorkerCapabilities{
		Encoders: []string{"libx264"}, FFmpegVersion: "5.0",
	})

	job, _ := database.CreateJob(`["f.mp4"]`, `["args"]`, "o.mp4", false)
	database.AssignJobToWorker(job.ID, "w1")
	database.UpdateJobStatus(job.ID, protocol.JobStatusRunning, nil, nil)
	// Set old started_at
	database.GetDB().Exec(
		`UPDATE jobs SET started_at = ? WHERE id = ?`,
		time.Now().Add(-2*time.Hour), job.ID,
	)
	// Now complete the job
	database.UpdateJobStatus(job.ID, protocol.JobStatusCompleted, nil, nil)

	timedOut, err := database.GetTimedOutJobs(1 * time.Hour)
	if err != nil {
		t.Fatalf("GetTimedOutJobs failed: %v", err)
	}
	if len(timedOut) != 0 {
		t.Errorf("Completed job should not be timed out, got %d", len(timedOut))
	}
}

// TestIntegration_Timeout_ConcurrentSchedulerAccess verifies that the scheduler
// handles concurrent timeout checks safely (no data races with 10 timed-out jobs).
func TestIntegration_Timeout_ConcurrentSchedulerAccess(t *testing.T) {
	_, _, database, sched, cleanup := setupTestWithScheduler(t, 500*time.Millisecond)
	defer cleanup()

	database.CreateWorker("worker-1", "test-worker", protocol.WorkerCapabilities{
		Encoders: []string{"libx264"}, FFmpegVersion: "5.0", MaxConcurrent: 1,
	})

	for i := 0; i < 10; i++ {
		job, _ := database.CreateJobWithStreaming(
			fmt.Sprintf(`["f%d.mp4"]`, i),
			fmt.Sprintf(`["-i", "f%d.mp4", "-c:v", "libx264"]`, i),
			fmt.Sprintf("o%d.mp4", i), false, false, nil, "[]",
		)
		database.AssignJobToWorker(job.ID, "worker-1")
		database.UpdateJobStatus(job.ID, protocol.JobStatusRunning, nil, nil)
		database.GetDB().Exec(
			`UPDATE jobs SET started_at = ? WHERE id = ?`,
			time.Now().Add(-2*time.Hour), job.ID,
		)
	}

	sched.Start()
	defer sched.Stop()

	// Let the scheduler run several cycles (timeout + re-assign)
	time.Sleep(1 * time.Second)

	// With MaxConcurrent=1, only 1 job can be active at a time.
	// The scheduler should have detected all timeouts and cycled through them.
	// At least 1 should be queued (currently assigned to worker), the rest pending.
	pendingJobs, _ := database.GetPendingJobs(20)
	// Check that at least some jobs were detected as timed out and rescheduled
	// (the scheduler will have re-assigned at most 1 since MaxConcurrent=1)
	totalJobs := len(pendingJobs)
	queuedOrRunning := 0
	for i := 0; i < 10; i++ {
		// We can't know exact IDs, but check total
	}
	// At minimum, 9 jobs should be pending (1 could be queued/running)
	if totalJobs < 9 {
		t.Errorf("Expected at least 9 pending jobs (with 1 re-assigned), got %d", totalJobs)
	}
	_ = queuedOrRunning
}
