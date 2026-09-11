package handlers_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
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
	"github.com/tsic404/rffmpeg/pkg/server/storage"
	"github.com/tsic404/rffmpeg/pkg/server/workerhealth"
)

func setupTest(t *testing.T) (*handlers.Handler, *chi.Mux, func()) {
	// Create temp directory
	tmpDir, err := os.MkdirTemp("", "rffmpeg-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}

	// Initialize database
	database, err := db.New(fmt.Sprintf("%s/test.db", tmpDir))
	if err != nil {
		os.RemoveAll(tmpDir)
		t.Fatalf("Failed to create database: %v", err)
	}

	// Initialize storage
	store, err := storage.New(tmpDir)
	if err != nil {
		database.Close()
		os.RemoveAll(tmpDir)
		t.Fatalf("Failed to create storage: %v", err)
	}

	// Create handler. A default auth token keeps handler-level validation
	// active for every test; tests that exercise the tokenless fail-closed
	// path override it with SetAuthToken("").
	stateTable := workerhealth.NewWorkerStateTable(30 * time.Second)
	h := handlers.New(database, store, "test", stateTable)
	h.SetAuthToken("test-token")

	// Setup router
	r := chi.NewRouter()
	r.Post("/api/v1/upload", h.Upload)
	r.Post("/api/v1/jobs", h.SubmitJob)
	r.Get("/api/v1/jobs", h.ListJobs)
	r.Get("/api/v1/jobs/{jobId}", h.GetJob)
	r.Delete("/api/v1/jobs/{jobId}", h.CancelJob)
	r.Patch("/api/v1/jobs/{jobId}", h.UpdateJob)
	r.Post("/api/v1/jobs/{jobId}/output", h.UploadJobOutput)
	r.Get("/api/v1/output/{fileId}", h.DownloadOutput)
	r.Get("/api/v1/health", h.Health)
	r.Post("/api/v1/workers/register", h.RegisterWorker)
	r.Post("/api/v1/workers/heartbeat", h.WorkerHeartbeat)
	r.Get("/api/v1/workers/{workerId}/jobs", h.PullWorkerJobs)
	r.Get("/api/v1/workers/{workerId}", h.GetWorker)
	r.Get("/api/v1/workers", h.ListWorkers)
	r.Post("/api/v1/probe", h.Probe)

	cleanup := func() {
		database.Close()
		os.RemoveAll(tmpDir)
	}

	return h, r, cleanup
}

// registerTestWorker registers a test worker with the given encoders
func registerTestWorker(t *testing.T, router *chi.Mux, encoders []string) string {
	workerReq := protocol.WorkerRegisterRequest{
		WorkerID: "test-worker-1",
		Name:     "test-worker",
		Capabilities: protocol.WorkerCapabilities{
			Encoders:      encoders,
			FFmpegVersion: "5.1",
		},
	}
	workerBody, _ := json.Marshal(workerReq)

	req := httptest.NewRequest("POST", "/api/v1/workers/register", bytes.NewReader(workerBody))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("Failed to register worker: status %d, body: %s", w.Code, w.Body.String())
	}

	return "test-worker-1"
}

// registerTestWorkerWithID registers a test worker with an explicit worker ID
// and name, so tests can register multiple distinct workers.
func registerTestWorkerWithID(t *testing.T, router *chi.Mux, workerID, name string, encoders []string) string {
	workerReq := protocol.WorkerRegisterRequest{
		WorkerID: workerID,
		Name:     name,
		Capabilities: protocol.WorkerCapabilities{
			Encoders:      encoders,
			FFmpegVersion: "5.1",
		},
	}
	workerBody, _ := json.Marshal(workerReq)

	req := httptest.NewRequest("POST", "/api/v1/workers/register", bytes.NewReader(workerBody))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("Failed to register worker %s: status %d, body: %s", workerID, w.Code, w.Body.String())
	}

	return workerID
}

func TestHealthEndpoint(t *testing.T) {
	_, router, cleanup := setupTest(t)
	defer cleanup()

	req := httptest.NewRequest("GET", "/api/v1/health", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	w := httptest.NewRecorder()

	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", w.Code)
	}

	var resp protocol.HealthResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Errorf("Failed to decode response: %v", err)
	}

	if resp.Status != "healthy" {
		t.Errorf("Expected status 'healthy', got '%s'", resp.Status)
	}
}

func TestUploadAndSubmitJob(t *testing.T) {
	_, router, cleanup := setupTest(t)
	defer cleanup()

	// Register a worker with libx264 encoder (TSI-1428: required for job submission)
	registerTestWorker(t, router, []string{"libx264"})

	// Upload file
	fileContent := []byte("test video content")
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, _ := writer.CreateFormFile("file", "test.mp4")
	part.Write(fileContent)
	writer.Close()

	req := httptest.NewRequest("POST", "/api/v1/upload", body)
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", writer.FormDataContentType())
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Upload failed with status %d: %s", w.Code, w.Body.String())
	}

	var uploadResp protocol.UploadResponse
	if err := json.NewDecoder(w.Body).Decode(&uploadResp); err != nil {
		t.Fatalf("Failed to decode upload response: %v", err)
	}

	if uploadResp.FileID == "" {
		t.Error("Expected file_id in response")
	}

	// Submit job
	jobReq := protocol.JobSubmitRequest{
		InputFiles: []string{uploadResp.FileID},
		Args:       []string{"-c:v", "libx264", "-preset", "fast"},
	}
	jobBody, _ := json.Marshal(jobReq)

	req = httptest.NewRequest("POST", "/api/v1/jobs", bytes.NewReader(jobBody))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Job submit failed with status %d: %s", w.Code, w.Body.String())
	}

	var jobResp protocol.JobSubmitResponse
	if err := json.NewDecoder(w.Body).Decode(&jobResp); err != nil {
		t.Fatalf("Failed to decode job response: %v", err)
	}

	if jobResp.JobID == "" {
		t.Error("Expected job_id in response")
	}

	// Get job status
	req = httptest.NewRequest("GET", fmt.Sprintf("/api/v1/jobs/%s", jobResp.JobID), nil)
	req.Header.Set("Authorization", "Bearer test-token")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Get job failed with status %d", w.Code)
	}

	var statusResp protocol.JobStatusResponse
	if err := json.NewDecoder(w.Body).Decode(&statusResp); err != nil {
		t.Fatalf("Failed to decode status response: %v", err)
	}

	if statusResp.Job.Status != protocol.JobStatusPending {
		t.Errorf("Expected status 'pending', got '%s'", statusResp.Job.Status)
	}
}

// TestListJobs guards the GET /api/v1/jobs list endpoint: it must answer 200
// (not 405) with the submitted jobs, newest first.
func TestListJobs(t *testing.T) {
	_, router, cleanup := setupTest(t)
	defer cleanup()

	// Empty cluster: the endpoint exists and returns an empty list, not 405.
	req := httptest.NewRequest("GET", "/api/v1/jobs", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/jobs (empty) = %d, want 200: %s", w.Code, w.Body.String())
	}
	var emptyResp protocol.JobListResponse
	if err := json.NewDecoder(w.Body).Decode(&emptyResp); err != nil {
		t.Fatalf("Failed to decode empty list response: %v", err)
	}
	if len(emptyResp.Jobs) != 0 {
		t.Errorf("empty list has %d jobs, want 0", len(emptyResp.Jobs))
	}

	// Register a worker so jobs can be submitted (TSI-1428).
	registerTestWorker(t, router, []string{"libx264"})

	// Submit three jobs with a remote URL input to skip the file-existence check.
	submit := func() string {
		jobReq := protocol.JobSubmitRequest{
			InputFiles: []string{"http://example.com/in.mp4"},
			Args:       []string{"-c:v", "libx264"},
		}
		jobBody, _ := json.Marshal(jobReq)
		r := httptest.NewRequest("POST", "/api/v1/jobs", bytes.NewReader(jobBody))
		r.Header.Set("Authorization", "Bearer test-token")
		r.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, r)
		if rec.Code != http.StatusOK {
			t.Fatalf("Job submit failed with status %d: %s", rec.Code, rec.Body.String())
		}
		var resp protocol.JobSubmitResponse
		if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
			t.Fatalf("Failed to decode job response: %v", err)
		}
		return resp.JobID
	}
	ids := []string{submit(), submit(), submit()}

	list := func(query string) []protocol.JobInfo {
		r := httptest.NewRequest("GET", "/api/v1/jobs"+query, nil)
		r.Header.Set("Authorization", "Bearer test-token")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, r)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /api/v1/jobs%s = %d, want 200: %s", query, rec.Code, rec.Body.String())
		}
		var resp protocol.JobListResponse
		if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
			t.Fatalf("Failed to decode list response: %v", err)
		}
		return resp.Jobs
	}

	// Default page returns all three submitted jobs.
	all := list("")
	if len(all) != 3 {
		t.Fatalf("default list has %d jobs, want 3", len(all))
	}
	got := make(map[string]bool, len(all))
	for _, j := range all {
		got[j.ID] = true
	}
	for _, id := range ids {
		if !got[id] {
			t.Errorf("default list missing job %q", id)
		}
	}

	// limit caps the page size and offset skips rows (parsePagination HTTP path).
	if page := list("?limit=2"); len(page) != 2 {
		t.Errorf("limit=2 returned %d jobs, want 2", len(page))
	}
	if page := list("?limit=1"); len(page) != 1 {
		t.Errorf("limit=1 returned %d jobs, want 1", len(page))
	}
	if page := list("?limit=2&offset=2"); len(page) != 1 {
		t.Errorf("limit=2&offset=2 returned %d jobs, want 1", len(page))
	}
	if page := list("?limit=10&offset=1"); len(page) != 2 {
		t.Errorf("limit=10&offset=1 returned %d jobs, want 2", len(page))
	}
}

func TestUpdateJobStatus(t *testing.T) {
	_, router, cleanup := setupTest(t)
	defer cleanup()

	// Register a worker with libx264 encoder (TSI-1428: required for job submission)
	registerTestWorker(t, router, []string{"libx264"})

	// Upload and create job
	fileContent := []byte("test content")
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, _ := writer.CreateFormFile("file", "test.mp4")
	part.Write(fileContent)
	writer.Close()

	req := httptest.NewRequest("POST", "/api/v1/upload", body)
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", writer.FormDataContentType())
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	var uploadResp protocol.UploadResponse
	json.NewDecoder(w.Body).Decode(&uploadResp)

	jobReq := protocol.JobSubmitRequest{
		InputFiles: []string{uploadResp.FileID},
		Args:       []string{"-c:v", "libx264"},
	}
	jobBody, _ := json.Marshal(jobReq)

	req = httptest.NewRequest("POST", "/api/v1/jobs", bytes.NewReader(jobBody))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)

	var jobResp protocol.JobSubmitResponse
	json.NewDecoder(w.Body).Decode(&jobResp)

	// Update job to running
	updateReq := protocol.JobUpdateRequest{
		Status: protocol.JobStatusRunning,
	}
	updateBody, _ := json.Marshal(updateReq)

	req = httptest.NewRequest("PATCH", fmt.Sprintf("/api/v1/jobs/%s", jobResp.JobID), bytes.NewReader(updateBody))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Update job failed with status %d", w.Code)
	}

	// Verify status
	req = httptest.NewRequest("GET", fmt.Sprintf("/api/v1/jobs/%s", jobResp.JobID), nil)
	req.Header.Set("Authorization", "Bearer test-token")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)

	var statusResp protocol.JobStatusResponse
	json.NewDecoder(w.Body).Decode(&statusResp)

	if statusResp.Job.Status != protocol.JobStatusRunning {
		t.Errorf("Expected status 'running', got '%s'", statusResp.Job.Status)
	}

	// Complete job
	updateReq = protocol.JobUpdateRequest{
		Status:   protocol.JobStatusCompleted,
		ExitCode: 0,
	}
	updateBody, _ = json.Marshal(updateReq)

	req = httptest.NewRequest("PATCH", fmt.Sprintf("/api/v1/jobs/%s", jobResp.JobID), bytes.NewReader(updateBody))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)

	// Verify completed
	req = httptest.NewRequest("GET", fmt.Sprintf("/api/v1/jobs/%s", jobResp.JobID), nil)
	req.Header.Set("Authorization", "Bearer test-token")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)

	json.NewDecoder(w.Body).Decode(&statusResp)

	if statusResp.Job.Status != protocol.JobStatusCompleted {
		t.Errorf("Expected status 'completed', got '%s'", statusResp.Job.Status)
	}
}

// TestUpdateJobCachedFlag covers the TSI-2519 end-to-end contract through the
// HTTP layer: a worker that reports a completed job with cached=true persists
// the flag, and a CLI-style GET observes cached:true on the job.
func TestUpdateJobCachedFlag(t *testing.T) {
	_, router, cleanup := setupTest(t)
	defer cleanup()

	workerID := registerTestWorker(t, router, []string{"libx264"})

	fileContent := []byte("test content")
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, _ := writer.CreateFormFile("file", "test.mp4")
	part.Write(fileContent)
	writer.Close()

	req := httptest.NewRequest("POST", "/api/v1/upload", body)
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", writer.FormDataContentType())
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	var uploadResp protocol.UploadResponse
	json.NewDecoder(w.Body).Decode(&uploadResp)

	jobReq := protocol.JobSubmitRequest{
		InputFiles: []string{uploadResp.FileID},
		Args:       []string{"-c:v", "libx264"},
	}
	jobBody, _ := json.Marshal(jobReq)
	req = httptest.NewRequest("POST", "/api/v1/jobs", bytes.NewReader(jobBody))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)

	var jobResp protocol.JobSubmitResponse
	json.NewDecoder(w.Body).Decode(&jobResp)

	// Worker pulls the job, which assigns it and marks the worker busy.
	pullReq := httptest.NewRequest("GET", "/api/v1/workers/"+workerID+"/jobs", nil)
	pullReq.Header.Set("Authorization", "Bearer test-token")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, pullReq)
	if w.Code != http.StatusOK {
		t.Fatalf("Pull jobs failed with status %d: %s", w.Code, w.Body.String())
	}

	// Worker reports completion from cache (ownership-guarded terminal path).
	updateReq := protocol.JobUpdateRequest{
		Status:   protocol.JobStatusCompleted,
		ExitCode: 0,
		Cached:   true,
		WorkerID: workerID,
	}
	updateBody, _ := json.Marshal(updateReq)
	req = httptest.NewRequest("PATCH", fmt.Sprintf("/api/v1/jobs/%s", jobResp.JobID), bytes.NewReader(updateBody))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("Update job failed with status %d: %s", w.Code, w.Body.String())
	}

	// CLI-style GET must observe the cached flag.
	req = httptest.NewRequest("GET", fmt.Sprintf("/api/v1/jobs/%s", jobResp.JobID), nil)
	req.Header.Set("Authorization", "Bearer test-token")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)

	var statusResp protocol.JobStatusResponse
	if err := json.NewDecoder(w.Body).Decode(&statusResp); err != nil {
		t.Fatalf("Failed to decode status response: %v", err)
	}
	if !statusResp.Job.Cached {
		t.Errorf("Expected cached=true after cache-hit completion, got false")
	}

	// A plain (non-cache) completion must leave cached=false.
	jobReq2 := protocol.JobSubmitRequest{
		InputFiles: []string{uploadResp.FileID},
		Args:       []string{"-c:v", "libx264"},
	}
	jobBody2, _ := json.Marshal(jobReq2)
	req = httptest.NewRequest("POST", "/api/v1/jobs", bytes.NewReader(jobBody2))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	var jobResp2 protocol.JobSubmitResponse
	json.NewDecoder(w.Body).Decode(&jobResp2)

	pullReq2 := httptest.NewRequest("GET", "/api/v1/workers/"+workerID+"/jobs", nil)
	pullReq2.Header.Set("Authorization", "Bearer test-token")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, pullReq2)
	if w.Code != http.StatusOK {
		t.Fatalf("Second pull failed with status %d: %s", w.Code, w.Body.String())
	}

	updateReq2 := protocol.JobUpdateRequest{
		Status:   protocol.JobStatusCompleted,
		ExitCode: 0,
		WorkerID: workerID,
	}
	updateBody2, _ := json.Marshal(updateReq2)
	req = httptest.NewRequest("PATCH", fmt.Sprintf("/api/v1/jobs/%s", jobResp2.JobID), bytes.NewReader(updateBody2))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("Second update failed with status %d: %s", w.Code, w.Body.String())
	}

	req = httptest.NewRequest("GET", fmt.Sprintf("/api/v1/jobs/%s", jobResp2.JobID), nil)
	req.Header.Set("Authorization", "Bearer test-token")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	var statusResp2 protocol.JobStatusResponse
	json.NewDecoder(w.Body).Decode(&statusResp2)
	if statusResp2.Job.Cached {
		t.Errorf("Expected cached=false after plain completion, got true")
	}
}

// TestUpdateJobCachedFlagCoercedOnFailure guards the TSI-2519 review fix: the
// public PATCH /api/v1/jobs/{id} endpoint must not let a client fabricate a
// cache hit for a non-completed outcome. A failed report with cached=true
// persists cached=false.
func TestUpdateJobCachedFlagCoercedOnFailure(t *testing.T) {
	_, router, cleanup := setupTest(t)
	defer cleanup()

	workerID := registerTestWorker(t, router, []string{"libx264"})

	fileContent := []byte("test content")
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, _ := writer.CreateFormFile("file", "test.mp4")
	part.Write(fileContent)
	writer.Close()

	req := httptest.NewRequest("POST", "/api/v1/upload", body)
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", writer.FormDataContentType())
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	var uploadResp protocol.UploadResponse
	json.NewDecoder(w.Body).Decode(&uploadResp)

	jobReq := protocol.JobSubmitRequest{
		InputFiles: []string{uploadResp.FileID},
		Args:       []string{"-c:v", "libx264"},
	}
	jobBody, _ := json.Marshal(jobReq)
	req = httptest.NewRequest("POST", "/api/v1/jobs", bytes.NewReader(jobBody))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	var jobResp protocol.JobSubmitResponse
	json.NewDecoder(w.Body).Decode(&jobResp)

	// Worker pulls the job so it is owned and queued.
	pullReq := httptest.NewRequest("GET", "/api/v1/workers/"+workerID+"/jobs", nil)
	pullReq.Header.Set("Authorization", "Bearer test-token")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, pullReq)
	if w.Code != http.StatusOK {
		t.Fatalf("Pull jobs failed with status %d: %s", w.Code, w.Body.String())
	}

	// A failed terminal report with cached=true must not persist cached.
	updateReq := protocol.JobUpdateRequest{
		Status:   protocol.JobStatusFailed,
		ExitCode: 1,
		Cached:   true,
		WorkerID: workerID,
	}
	updateBody, _ := json.Marshal(updateReq)
	req = httptest.NewRequest("PATCH", fmt.Sprintf("/api/v1/jobs/%s", jobResp.JobID), bytes.NewReader(updateBody))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("Update job failed with status %d: %s", w.Code, w.Body.String())
	}

	req = httptest.NewRequest("GET", fmt.Sprintf("/api/v1/jobs/%s", jobResp.JobID), nil)
	req.Header.Set("Authorization", "Bearer test-token")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	var statusResp protocol.JobStatusResponse
	if err := json.NewDecoder(w.Body).Decode(&statusResp); err != nil {
		t.Fatalf("Failed to decode status response: %v", err)
	}
	if statusResp.Job.Cached {
		t.Errorf("Expected cached=false after failed completion with cached=true, got true")
	}
}

// TestUpdateJobFailureClassification verifies the worker failure_type flow:
// a failed update persists failure_type/failure_details, an invalid enum
// value is rejected with 400, and completing the job clears stale fields.
func TestUpdateJobFailureClassification(t *testing.T) {
	_, router, cleanup := setupTest(t)
	defer cleanup()

	registerTestWorker(t, router, []string{"libx264"})

	fileContent := []byte("test content")
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, _ := writer.CreateFormFile("file", "test.mp4")
	part.Write(fileContent)
	writer.Close()

	req := httptest.NewRequest("POST", "/api/v1/upload", body)
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", writer.FormDataContentType())
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	var uploadResp protocol.UploadResponse
	json.NewDecoder(w.Body).Decode(&uploadResp)

	jobReq := protocol.JobSubmitRequest{
		InputFiles: []string{uploadResp.FileID},
		Args:       []string{"-c:v", "libx264"},
	}
	jobBody, _ := json.Marshal(jobReq)
	req = httptest.NewRequest("POST", "/api/v1/jobs", bytes.NewReader(jobBody))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)

	var jobResp protocol.JobSubmitResponse
	json.NewDecoder(w.Body).Decode(&jobResp)

	patch := func(update protocol.JobUpdateRequest) *httptest.ResponseRecorder {
		updateBody, _ := json.Marshal(update)
		r := httptest.NewRequest("PATCH", "/api/v1/jobs/"+jobResp.JobID, bytes.NewReader(updateBody))
		r.Header.Set("Authorization", "Bearer test-token")
		r.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, r)
		return rec
	}
	getJob := func() protocol.JobInfo {
		r := httptest.NewRequest("GET", "/api/v1/jobs/"+jobResp.JobID, nil)
		r.Header.Set("Authorization", "Bearer test-token")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, r)
		var resp protocol.JobStatusResponse
		json.NewDecoder(rec.Body).Decode(&resp)
		return resp.Job
	}

	// 1. Invalid failure_type is rejected
	resp := patch(protocol.JobUpdateRequest{
		Status:      protocol.JobStatusFailed,
		FailureType: "NOT_A_REAL_TYPE",
	})
	if resp.Code != http.StatusBadRequest {
		t.Errorf("invalid failure_type: expected 400, got %d", resp.Code)
	}

	// 2. Valid classification is persisted
	resp = patch(protocol.JobUpdateRequest{
		Status:         protocol.JobStatusFailed,
		ExitCode:       1,
		Error:          "ffmpeg blew up",
		FailureType:    string(protocol.FailureInputUnreachable),
		FailureDetails: "Input file or stream cannot be reached",
	})
	if resp.Code != http.StatusOK {
		t.Fatalf("failed update: expected 200, got %d", resp.Code)
	}
	job := getJob()
	if job.FailureType != string(protocol.FailureInputUnreachable) {
		t.Errorf("expected failure_type %q persisted, got %q",
			protocol.FailureInputUnreachable, job.FailureType)
	}

	// 3. Completing clears stale failure metadata (retry-after-failure path)
	resp = patch(protocol.JobUpdateRequest{
		Status:   protocol.JobStatusCompleted,
		ExitCode: 0,
	})
	if resp.Code != http.StatusOK {
		t.Fatalf("complete update: expected 200, got %d", resp.Code)
	}
	job = getJob()
	if job.Status != protocol.JobStatusCompleted {
		t.Fatalf("expected completed, got %q", job.Status)
	}
	if job.FailureType != "" {
		t.Errorf("completed job should have empty failure_type, got %q", job.FailureType)
	}
	if job.FailureDetails != "" {
		t.Errorf("completed job should have empty failure_details, got %q", job.FailureDetails)
	}
}

// TestUpdateJobFailureTypeAllEnumsAccepted locks the TSI-2802 gap: every one
// of the nine documented FailureType values must be accepted by the server
// (not just TIMEOUT / INPUT_UNREACHABLE) and persisted onto the job, and each
// must report the retryable flag it is defined with. A failed→failed update
// is a legal retry-after-failure transition, so one job can exercise all
// eight values in sequence.
func TestUpdateJobFailureTypeAllEnumsAccepted(t *testing.T) {
	_, router, cleanup := setupTest(t)
	defer cleanup()

	registerTestWorker(t, router, []string{"libx264"})

	fileContent := []byte("test content")
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, _ := writer.CreateFormFile("file", "test.mp4")
	part.Write(fileContent)
	writer.Close()

	req := httptest.NewRequest("POST", "/api/v1/upload", body)
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", writer.FormDataContentType())
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("upload failed: status %d, body: %s", w.Code, w.Body.String())
	}

	var uploadResp protocol.UploadResponse
	if err := json.NewDecoder(w.Body).Decode(&uploadResp); err != nil {
		t.Fatalf("Failed to decode upload response: %v", err)
	}

	jobReq := protocol.JobSubmitRequest{
		InputFiles: []string{uploadResp.FileID},
		Args:       []string{"-c:v", "libx264"},
	}
	jobBody, _ := json.Marshal(jobReq)
	req = httptest.NewRequest("POST", "/api/v1/jobs", bytes.NewReader(jobBody))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("job submit failed: status %d, body: %s", w.Code, w.Body.String())
	}

	var jobResp protocol.JobSubmitResponse
	if err := json.NewDecoder(w.Body).Decode(&jobResp); err != nil {
		t.Fatalf("Failed to decode job response: %v", err)
	}

	allTypes := []protocol.FailureType{
		protocol.FailureInputUnreachable,
		protocol.FailureEncoderUnsupported,
		protocol.FailureEncoderUnavailable,
		protocol.FailureDiskFull,
		protocol.FailureTimeout,
		protocol.FailureWorkerCrash,
		protocol.FailureFFmpegError,
		protocol.FailureNoWorkerAvailable,
		protocol.FailureInfra,
	}

	for _, ft := range allTypes {
		if !ft.IsValid() {
			t.Fatalf("%s must be a valid enum value", ft)
		}

		update := protocol.JobUpdateRequest{
			Status:         protocol.JobStatusFailed,
			ExitCode:       1,
			Error:          "ffmpeg blew up",
			FailureType:    string(ft),
			FailureDetails: "injected for enum coverage",
		}
		updateBody, _ := json.Marshal(update)
		r := httptest.NewRequest("PATCH", "/api/v1/jobs/"+jobResp.JobID, bytes.NewReader(updateBody))
		r.Header.Set("Authorization", "Bearer test-token")
		r.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, r)
		if rec.Code != http.StatusOK {
			t.Fatalf("PATCH failure_type %s: expected 200, got %d (body: %s)",
				ft, rec.Code, rec.Body.String())
		}

		gr := httptest.NewRequest("GET", "/api/v1/jobs/"+jobResp.JobID, nil)
		gr.Header.Set("Authorization", "Bearer test-token")
		grec := httptest.NewRecorder()
		router.ServeHTTP(grec, gr)
		var statusResp protocol.JobStatusResponse
		json.NewDecoder(grec.Body).Decode(&statusResp)
		if statusResp.Job.FailureType != string(ft) {
			t.Errorf("failure_type %s not persisted, got %q", ft, statusResp.Job.FailureType)
		}
		if statusResp.Job.Retryable != ft.Retryable() {
			t.Errorf("failure_type %s: retryable = %v, want %v",
				ft, statusResp.Job.Retryable, ft.Retryable())
		}
	}
}

func TestCancelJob(t *testing.T) {
	_, router, cleanup := setupTest(t)
	defer cleanup()

	// Register a worker with libx264 encoder (TSI-1428: required for job submission)
	registerTestWorker(t, router, []string{"libx264"})

	// Create job
	fileContent := []byte("test content")
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, _ := writer.CreateFormFile("file", "test.mp4")
	part.Write(fileContent)
	writer.Close()

	req := httptest.NewRequest("POST", "/api/v1/upload", body)
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", writer.FormDataContentType())
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	var uploadResp protocol.UploadResponse
	json.NewDecoder(w.Body).Decode(&uploadResp)

	jobReq := protocol.JobSubmitRequest{
		InputFiles: []string{uploadResp.FileID},
		Args:       []string{"-c:v", "libx264"},
	}
	jobBody, _ := json.Marshal(jobReq)

	req = httptest.NewRequest("POST", "/api/v1/jobs", bytes.NewReader(jobBody))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)

	var jobResp protocol.JobSubmitResponse
	json.NewDecoder(w.Body).Decode(&jobResp)

	// Cancel job
	req = httptest.NewRequest("DELETE", fmt.Sprintf("/api/v1/jobs/%s", jobResp.JobID), nil)
	req.Header.Set("Authorization", "Bearer test-token")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Cancel job failed with status %d", w.Code)
	}

	// Verify cancelled
	req = httptest.NewRequest("GET", fmt.Sprintf("/api/v1/jobs/%s", jobResp.JobID), nil)
	req.Header.Set("Authorization", "Bearer test-token")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)

	var statusResp protocol.JobStatusResponse
	json.NewDecoder(w.Body).Decode(&statusResp)

	if statusResp.Job.Status != protocol.JobStatusCancelled {
		t.Errorf("Expected status 'cancelled', got '%s'", statusResp.Job.Status)
	}
}

func TestCancelRunningJob(t *testing.T) {
	_, router, cleanup := setupTest(t)
	defer cleanup()

	// Register a worker with libx264 encoder (TSI-1428: required for job submission)
	registerTestWorker(t, router, []string{"libx264"})

	// Create job
	fileContent := []byte("test content")
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, _ := writer.CreateFormFile("file", "test.mp4")
	part.Write(fileContent)
	writer.Close()

	req := httptest.NewRequest("POST", "/api/v1/upload", body)
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", writer.FormDataContentType())
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	var uploadResp protocol.UploadResponse
	json.NewDecoder(w.Body).Decode(&uploadResp)

	jobReq := protocol.JobSubmitRequest{
		InputFiles: []string{uploadResp.FileID},
		Args:       []string{"-c:v", "libx264"},
	}
	jobBody, _ := json.Marshal(jobReq)

	req = httptest.NewRequest("POST", "/api/v1/jobs", bytes.NewReader(jobBody))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)

	var jobResp protocol.JobSubmitResponse
	json.NewDecoder(w.Body).Decode(&jobResp)

	// Update job to running status
	updateReq := protocol.JobUpdateRequest{
		Status: protocol.JobStatusRunning,
	}
	updateBody, _ := json.Marshal(updateReq)

	req = httptest.NewRequest("PATCH", fmt.Sprintf("/api/v1/jobs/%s", jobResp.JobID), bytes.NewReader(updateBody))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Update job to running failed with status %d", w.Code)
	}

	// Cancel running job
	req = httptest.NewRequest("DELETE", fmt.Sprintf("/api/v1/jobs/%s", jobResp.JobID), nil)
	req.Header.Set("Authorization", "Bearer test-token")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Cancel running job failed with status %d: %s", w.Code, w.Body.String())
	}

	// Verify cancelled
	req = httptest.NewRequest("GET", fmt.Sprintf("/api/v1/jobs/%s", jobResp.JobID), nil)
	req.Header.Set("Authorization", "Bearer test-token")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)

	var statusResp protocol.JobStatusResponse
	json.NewDecoder(w.Body).Decode(&statusResp)

	if statusResp.Job.Status != protocol.JobStatusCancelled {
		t.Errorf("Expected status 'cancelled', got '%s'", statusResp.Job.Status)
	}
}

func TestUploadOutputAndDownload(t *testing.T) {
	_, router, cleanup := setupTest(t)
	defer cleanup()

	// Register a worker with libx264 encoder (TSI-1428: required for job submission)
	registerTestWorker(t, router, []string{"libx264"})

	// Create job
	fileContent := []byte("test content")
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, _ := writer.CreateFormFile("file", "test.mp4")
	part.Write(fileContent)
	writer.Close()

	req := httptest.NewRequest("POST", "/api/v1/upload", body)
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", writer.FormDataContentType())
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	var uploadResp protocol.UploadResponse
	json.NewDecoder(w.Body).Decode(&uploadResp)

	jobReq := protocol.JobSubmitRequest{
		InputFiles: []string{uploadResp.FileID},
		Args:       []string{"-c:v", "libx264"},
	}
	jobBody, _ := json.Marshal(jobReq)

	req = httptest.NewRequest("POST", "/api/v1/jobs", bytes.NewReader(jobBody))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)

	var jobResp protocol.JobSubmitResponse
	json.NewDecoder(w.Body).Decode(&jobResp)

	// Upload output
	outputContent := []byte("transcoded output")
	outputBody := &bytes.Buffer{}
	outputWriter := multipart.NewWriter(outputBody)
	outputPart, _ := outputWriter.CreateFormFile("file", "output.mp4")
	outputPart.Write(outputContent)
	outputWriter.Close()

	req = httptest.NewRequest("POST", fmt.Sprintf("/api/v1/jobs/%s/output", jobResp.JobID), outputBody)
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", outputWriter.FormDataContentType())
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Upload output failed with status %d", w.Code)
	}

	// Get job to find output file ID
	req = httptest.NewRequest("GET", fmt.Sprintf("/api/v1/jobs/%s", jobResp.JobID), nil)
	req.Header.Set("Authorization", "Bearer test-token")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)

	var statusResp protocol.JobStatusResponse
	json.NewDecoder(w.Body).Decode(&statusResp)

	if len(statusResp.Job.OutputFiles) == 0 {
		t.Error("Expected output files in job")
		return
	}

	outputFileID := statusResp.Job.OutputFiles[0]

	// Download output
	req = httptest.NewRequest("GET", fmt.Sprintf("/api/v1/output/%s", outputFileID), nil)
	req.Header.Set("Authorization", "Bearer test-token")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Download output failed with status %d", w.Code)
		return
	}

	downloadedContent, _ := io.ReadAll(w.Body)
	if string(downloadedContent) != string(outputContent) {
		t.Error("Downloaded content doesn't match uploaded content")
	}
}

func TestGetNonExistentJob(t *testing.T) {
	_, router, cleanup := setupTest(t)
	defer cleanup()

	req := httptest.NewRequest("GET", "/api/v1/jobs/non-existent-id", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("Expected status 404, got %d", w.Code)
	}
}

func TestSubmitJobWithInvalidFile(t *testing.T) {
	_, router, cleanup := setupTest(t)
	defer cleanup()

	jobReq := protocol.JobSubmitRequest{
		InputFiles: []string{"non-existent-file"},
		Args:       []string{"-c:v", "libx264"},
	}
	jobBody, _ := json.Marshal(jobReq)

	req := httptest.NewRequest("POST", "/api/v1/jobs", bytes.NewReader(jobBody))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("Expected status 400, got %d", w.Code)
	}
}

func TestWorkerRegistration(t *testing.T) {
	_, router, cleanup := setupTest(t)
	defer cleanup()

	// Register worker
	regReq := protocol.WorkerRegisterRequest{
		Name: "test-worker",
		Capabilities: protocol.WorkerCapabilities{
			GPUModel:      "NVIDIA RTX 3080",
			Encoders:      []string{"libx264", "h264_nvenc"},
			Decoders:      []string{"h264", "hevc"},
			FFmpegVersion: "5.1.2",
			MaxConcurrent: 2,
		},
	}
	regBody, _ := json.Marshal(regReq)

	req := httptest.NewRequest("POST", "/api/v1/workers/register", bytes.NewReader(regBody))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Worker registration failed with status %d: %s", w.Code, w.Body.String())
	}

	var resp protocol.WorkerRegisterResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("Failed to decode response: %v", err)
	}

	if resp.WorkerID == "" {
		t.Error("Expected worker_id in response")
	}

	if resp.Message != "Worker registered successfully" {
		t.Errorf("Expected success message, got '%s'", resp.Message)
	}
}

// TSI-2522: a spec-conformant client registers only the canonical encoders
// list; the server must derive video_encoders from it so that the
// GET /api/v1/encoders aggregation endpoint is not permanently empty.
func TestRegisterWorker_DerivesVideoEncoders(t *testing.T) {
	h, router, cleanup := setupTest(t)
	defer cleanup()
	router.Get("/api/v1/encoders", h.ListAllEncoders)
	router.Get("/api/v1/decoders", h.ListAllDecoders)

	regReq := protocol.WorkerRegisterRequest{
		Name: "derived-worker",
		Capabilities: protocol.WorkerCapabilities{
			Encoders:      []string{"libx264", "h264_nvenc"},
			Decoders:      []string{"h264", "h264_cuvid"},
			FFmpegVersion: "5.1.2",
		},
	}
	regBody, _ := json.Marshal(regReq)
	req := httptest.NewRequest("POST", "/api/v1/workers/register", bytes.NewReader(regBody))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("registration failed: %d %s", w.Code, w.Body.String())
	}

	encReq := httptest.NewRequest("GET", "/api/v1/encoders", nil)
	encReq.Header.Set("Authorization", "Bearer test-token")
	encW := httptest.NewRecorder()
	router.ServeHTTP(encW, encReq)
	if encW.Code != http.StatusOK {
		t.Fatalf("encoders endpoint failed: %d %s", encW.Code, encW.Body.String())
	}
	var encResp handlers.ListEncodersResponse
	if err := json.NewDecoder(encW.Body).Decode(&encResp); err != nil {
		t.Fatalf("decode encoders: %v", err)
	}
	byName := make(map[string]protocol.EncoderInfo, len(encResp.Encoders))
	for _, enc := range encResp.Encoders {
		byName[enc.Name] = enc
	}

	libx264, ok := byName["libx264"]
	if !ok {
		t.Fatal("derived encoder libx264 missing from /api/v1/encoders")
	}
	if libx264.IsHW {
		t.Errorf("libx264 IsHW = true, want false")
	}
	if libx264.Type != "video" {
		t.Errorf("libx264 Type = %q, want %q", libx264.Type, "video")
	}
	nvenc, ok := byName["h264_nvenc"]
	if !ok {
		t.Fatal("derived encoder h264_nvenc missing from /api/v1/encoders")
	}
	if !nvenc.IsHW {
		t.Errorf("h264_nvenc IsHW = false, want true")
	}
	if nvenc.Type != "video" {
		t.Errorf("h264_nvenc Type = %q, want %q", nvenc.Type, "video")
	}

	decReq := httptest.NewRequest("GET", "/api/v1/decoders", nil)
	decReq.Header.Set("Authorization", "Bearer test-token")
	decW := httptest.NewRecorder()
	router.ServeHTTP(decW, decReq)
	if decW.Code != http.StatusOK {
		t.Fatalf("decoders endpoint failed: %d %s", decW.Code, decW.Body.String())
	}
	var decResp handlers.ListDecodersResponse
	if err := json.NewDecoder(decW.Body).Decode(&decResp); err != nil {
		t.Fatalf("decode decoders: %v", err)
	}
	decByID := make(map[string]bool, len(decResp.Decoders))
	for _, dec := range decResp.Decoders {
		decByID[dec.Name] = true
	}
	if !decByID["h264_cuvid"] {
		t.Errorf("derived decoder h264_cuvid missing from /api/v1/decoders")
	}
}

// TestRegisterWorker_PreservesExplicitVideoEncoders verifies the derive guard:
// a rich client that already sends video_encoders/video_decoders is not
// overridden by the canonical flat list (len(...) == 0 branch is skipped).
func TestRegisterWorker_PreservesExplicitVideoEncoders(t *testing.T) {
	h, router, cleanup := setupTest(t)
	defer cleanup()
	router.Get("/api/v1/encoders", h.ListAllEncoders)

	regReq := protocol.WorkerRegisterRequest{
		Name: "rich-worker",
		Capabilities: protocol.WorkerCapabilities{
			Encoders:      []string{"libx264"},
			FFmpegVersion: "5.1.2",
			VideoEncoders: []protocol.EncoderInfo{
				{Name: "libx264", Description: "rich desc", Type: "video", IsHW: false, Priority: 7},
			},
		},
	}
	regBody, _ := json.Marshal(regReq)
	req := httptest.NewRequest("POST", "/api/v1/workers/register", bytes.NewReader(regBody))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("registration failed: %d %s", w.Code, w.Body.String())
	}

	encReq := httptest.NewRequest("GET", "/api/v1/encoders", nil)
	encReq.Header.Set("Authorization", "Bearer test-token")
	encW := httptest.NewRecorder()
	router.ServeHTTP(encW, encReq)
	if encW.Code != http.StatusOK {
		t.Fatalf("encoders endpoint failed: %d %s", encW.Code, encW.Body.String())
	}
	var encResp handlers.ListEncodersResponse
	if err := json.NewDecoder(encW.Body).Decode(&encResp); err != nil {
		t.Fatalf("decode encoders: %v", err)
	}
	if len(encResp.Encoders) != 1 {
		t.Fatalf("encoders count = %d, want 1 (no derived duplication)", len(encResp.Encoders))
	}
	got := encResp.Encoders[0]
	if got.Name != "libx264" {
		t.Errorf("encoder name = %q, want %q", got.Name, "libx264")
	}
	if got.Description != "rich desc" {
		t.Errorf("encoder description = %q, want %q (explicit value preserved)", got.Description, "rich desc")
	}
	if got.Priority != 7 {
		t.Errorf("encoder priority = %d, want 7 (explicit value preserved)", got.Priority)
	}
}

func TestWorkerReRegistration(t *testing.T) {
	_, router, cleanup := setupTest(t)
	defer cleanup()

	// Register worker first time
	regReq := protocol.WorkerRegisterRequest{
		WorkerID: "re-reg-worker-1",
		Name:     "test-worker",
		Capabilities: protocol.WorkerCapabilities{
			GPUModel:      "NVIDIA RTX 3080",
			Encoders:      []string{"libx264", "h264_nvenc"},
			Decoders:      []string{"h264", "hevc"},
			FFmpegVersion: "5.1.2",
			MaxConcurrent: 2,
		},
	}
	regBody, _ := json.Marshal(regReq)

	req := httptest.NewRequest("POST", "/api/v1/workers/register", bytes.NewReader(regBody))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("First registration failed with status %d: %s", w.Code, w.Body.String())
	}

	var resp1 protocol.WorkerRegisterResponse
	json.NewDecoder(w.Body).Decode(&resp1)

	// Re-register the same worker with updated capabilities
	regReq.Capabilities.Encoders = []string{"libx264", "h264_nvenc", "hevc_nvenc"}
	regReq.Capabilities.FFmpegVersion = "6.0.0"
	regBody, _ = json.Marshal(regReq)

	req = httptest.NewRequest("POST", "/api/v1/workers/register", bytes.NewReader(regBody))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("Re-registration failed with status %d: %s", w.Code, w.Body.String())
	}

	var resp2 protocol.WorkerRegisterResponse
	json.NewDecoder(w.Body).Decode(&resp2)

	if resp2.WorkerID != resp1.WorkerID {
		t.Errorf("Expected same worker ID on re-registration, got %s vs %s", resp1.WorkerID, resp2.WorkerID)
	}

	if resp2.Message != "Worker registered successfully" {
		t.Errorf("Expected success message on re-registration, got '%s'", resp2.Message)
	}
}

// TSI-2473: an empty worker name must be rejected at the handler. Without
// this guard, the DB-layer DELETE would wipe unrelated empty-name rows.
func TestRegisterWorker_RejectsEmptyName(t *testing.T) {
	_, router, cleanup := setupTest(t)
	defer cleanup()

	regReq := protocol.WorkerRegisterRequest{
		WorkerID: "empty-name-worker",
		Name:     "",
		Capabilities: protocol.WorkerCapabilities{
			Encoders:      []string{"libx264"},
			FFmpegVersion: "5.1.2",
		},
	}
	regBody, _ := json.Marshal(regReq)

	req := httptest.NewRequest("POST", "/api/v1/workers/register", bytes.NewReader(regBody))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("Expected 400 for empty name, got %d: %s", w.Code, w.Body.String())
	}
}

func TestWorkerHeartbeat(t *testing.T) {
	_, router, cleanup := setupTest(t)
	defer cleanup()

	// Register worker first
	regReq := protocol.WorkerRegisterRequest{
		Name: "test-worker",
		Capabilities: protocol.WorkerCapabilities{
			Encoders:      []string{"libx264"},
			FFmpegVersion: "5.1.2",
		},
	}
	regBody, _ := json.Marshal(regReq)

	req := httptest.NewRequest("POST", "/api/v1/workers/register", bytes.NewReader(regBody))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	var regResp protocol.WorkerRegisterResponse
	json.NewDecoder(w.Body).Decode(&regResp)

	// Send heartbeat
	heartbeatReq := protocol.WorkerHeartbeatRequest{
		WorkerID: regResp.WorkerID,
		Status:   protocol.WorkerStatusIdle,
	}
	heartbeatBody, _ := json.Marshal(heartbeatReq)

	req = httptest.NewRequest("POST", "/api/v1/workers/heartbeat", bytes.NewReader(heartbeatBody))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Heartbeat failed with status %d: %s", w.Code, w.Body.String())
	}

	var resp protocol.WorkerHeartbeatResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("Failed to decode response: %v", err)
	}

	if resp.Message != "Heartbeat acknowledged" {
		t.Errorf("Expected acknowledgment message, got '%s'", resp.Message)
	}
}

func TestWorkerHeartbeatNonExistent(t *testing.T) {
	_, router, cleanup := setupTest(t)
	defer cleanup()

	// Send heartbeat for non-existent worker
	heartbeatReq := protocol.WorkerHeartbeatRequest{
		WorkerID: "non-existent-worker-id",
		Status:   protocol.WorkerStatusIdle,
	}
	heartbeatBody, _ := json.Marshal(heartbeatReq)

	req := httptest.NewRequest("POST", "/api/v1/workers/heartbeat", bytes.NewReader(heartbeatBody))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("Expected status 404, got %d", w.Code)
	}
}

func TestWorkerHeartbeatWithThroughput(t *testing.T) {
	h, router, cleanup := setupTest(t)
	defer cleanup()

	// Register worker first
	regReq := protocol.WorkerRegisterRequest{
		Name: "test-worker-throughput",
		Capabilities: protocol.WorkerCapabilities{
			Encoders:      []string{"libx264"},
			FFmpegVersion: "5.1.2",
		},
	}
	regBody, _ := json.Marshal(regReq)

	req := httptest.NewRequest("POST", "/api/v1/workers/register", bytes.NewReader(regBody))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	var regResp protocol.WorkerRegisterResponse
	json.NewDecoder(rec.Body).Decode(&regResp)

	// Send heartbeat with throughput
	heartbeatReq := protocol.WorkerHeartbeatRequest{
		WorkerID:      regResp.WorkerID,
		Status:        protocol.WorkerStatusBusy,
		ActiveJobs:    []string{"job-1", "job-2"},
		ThroughputFPS: 1.5,
		CompletedJobs: 7,
	}
	heartbeatBody, _ := json.Marshal(heartbeatReq)

	req = httptest.NewRequest("POST", "/api/v1/workers/heartbeat", bytes.NewReader(heartbeatBody))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("Heartbeat with throughput failed with status %d: %s", rec.Code, rec.Body.String())
	}

	var resp protocol.WorkerHeartbeatResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("Failed to decode response: %v", err)
	}

	if resp.Message != "Heartbeat acknowledged" {
		t.Errorf("Expected acknowledgment message, got '%s'", resp.Message)
	}

	// Verify throughput data is in the state table (if stateTable is set)
	if h.GetStateTable() != nil {
		state, ok := h.GetStateTable().Get(regResp.WorkerID)
		if !ok {
			t.Errorf("Expected worker state to be in state table")
		} else if state.ThroughputFPS != 1.5 {
			t.Errorf("Expected throughput 1.5 in state table, got %f", state.ThroughputFPS)
		} else if len(state.ActiveJobs) != 2 {
			t.Errorf("Expected 2 active jobs in state table, got %d", len(state.ActiveJobs))
		} else if state.CompletedJobs != 7 {
			t.Errorf("Expected 7 completed jobs in state table, got %d", state.CompletedJobs)
		}
	}
}

// TestWorkerHealthInListResponse verifies that the workers endpoints always
// return a non-null health object (TSI-2219) and that GPU metrics sent via
// heartbeat flow into the response.
// workerHealth mirrors the handler's health object for JSON decoding in tests.
type WorkerHealth struct {
	Status        string   `json:"status"`
	GPUUtilPct    float64  `json:"gpu_util_percent,omitempty"`
	GPUMemUsedMB  int      `json:"gpu_mem_used_mb,omitempty"`
	ActiveJobs    []string `json:"active_jobs,omitempty"`
	ThroughputFPS float64  `json:"throughput_fps,omitempty"`
	LastSeen      string   `json:"last_seen"`
}

func TestWorkerHealthInListResponse(t *testing.T) {
	h, router, cleanup := setupTest(t)
	defer cleanup()

	// Register a worker
	regReq := protocol.WorkerRegisterRequest{
		Name: "test-worker-health",
		Capabilities: protocol.WorkerCapabilities{
			Encoders:      []string{"libx264"},
			FFmpegVersion: "5.1.2",
		},
	}
	regBody, _ := json.Marshal(regReq)
	req := httptest.NewRequest("POST", "/api/v1/workers/register", bytes.NewReader(regBody))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	var regResp protocol.WorkerRegisterResponse
	json.NewDecoder(rec.Body).Decode(&regResp)

	// Before any heartbeat: health must still be present (never null), derived from the DB record.
	req = httptest.NewRequest("GET", "/api/v1/workers/"+regResp.WorkerID, nil)
	req.Header.Set("Authorization", "Bearer test-token")
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	var getResp struct {
		Worker struct {
			ID     string        `json:"id"`
			Status string        `json:"status"`
			Health *WorkerHealth `json:"health"`
		} `json:"worker"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&getResp); err != nil {
		t.Fatalf("Failed to decode get worker response: %v", err)
	}
	if getResp.Worker.Health == nil {
		t.Fatalf("Expected non-null health for registered worker before first heartbeat")
	}
	if getResp.Worker.Health.Status != string(protocol.WorkerStatusIdle) {
		t.Errorf("Expected health status '%s', got '%s'", protocol.WorkerStatusIdle, getResp.Worker.Health.Status)
	}

	// Send a heartbeat with GPU metrics and active jobs.
	heartbeatReq := protocol.WorkerHeartbeatRequest{
		WorkerID:        regResp.WorkerID,
		Status:          protocol.WorkerStatusBusy,
		ActiveJobs:      []string{"job-1"},
		ThroughputFPS:   42.5,
		GPUUtilPct:      87,
		GPUMemUsedMB:    4096,
		GPUMetricsValid: true,
	}
	heartbeatBody, _ := json.Marshal(heartbeatReq)
	req = httptest.NewRequest("POST", "/api/v1/workers/heartbeat", bytes.NewReader(heartbeatBody))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("Heartbeat failed with status %d: %s", rec.Code, rec.Body.String())
	}

	// The list endpoint must surface the live metrics in health.
	req = httptest.NewRequest("GET", "/api/v1/workers", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	var listResp struct {
		Workers []struct {
			ID     string        `json:"id"`
			Status string        `json:"status"`
			Health *WorkerHealth `json:"health"`
		} `json:"workers"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&listResp); err != nil {
		t.Fatalf("Failed to decode list workers response: %v", err)
	}
	var found *struct {
		ID     string        `json:"id"`
		Status string        `json:"status"`
		Health *WorkerHealth `json:"health"`
	}
	for i := range listResp.Workers {
		if listResp.Workers[i].ID == regResp.WorkerID {
			found = &listResp.Workers[i]
		}
	}
	if found == nil {
		t.Fatalf("Registered worker missing from list response")
	}
	health := found.Health
	if health == nil {
		t.Fatalf("Expected non-null health after heartbeat")
	}
	if health.Status != string(protocol.WorkerStatusBusy) {
		t.Errorf("Expected health status 'busy', got '%s'", health.Status)
	}
	if health.GPUUtilPct != 87 {
		t.Errorf("Expected gpu_util_percent 87, got %f", health.GPUUtilPct)
	}
	if health.GPUMemUsedMB != 4096 {
		t.Errorf("Expected gpu_mem_used_mb 4096, got %d", health.GPUMemUsedMB)
	}
	if len(health.ActiveJobs) != 1 || health.ActiveJobs[0] != "job-1" {
		t.Errorf("Expected active_jobs [job-1], got %v", health.ActiveJobs)
	}
	if health.ThroughputFPS != 42.5 {
		t.Errorf("Expected throughput_fps 42.5, got %f", health.ThroughputFPS)
	}

	_ = h // handler kept for state table wiring via setupTest
}

// TestWorkerHealthThroughputAlwaysPresent verifies the health response always
// carries throughput_fps, even for an idle worker reporting zero throughput
// (or no heartbeat yet). omitempty previously dropped the field at 0, leaving
// an idle worker's health as {status, gpu_metrics_valid, last_seen} and
// breaking the QA assertion that throughput_fps is present (TSI-2999).
func TestWorkerHealthThroughputAlwaysPresent(t *testing.T) {
	_, router, cleanup := setupTest(t)
	defer cleanup()

	regReq := protocol.WorkerRegisterRequest{
		Name: "test-worker-throughput-always",
		Capabilities: protocol.WorkerCapabilities{
			Encoders:      []string{"libx264"},
			FFmpegVersion: "5.1.2",
		},
	}
	regBody, _ := json.Marshal(regReq)
	req := httptest.NewRequest("POST", "/api/v1/workers/register", bytes.NewReader(regBody))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	var regResp protocol.WorkerRegisterResponse
	json.NewDecoder(rec.Body).Decode(&regResp)

	// No heartbeat yet: idle worker, zero throughput. throughput_fps must
	// still be present (as 0) rather than omitted.
	req = httptest.NewRequest("GET", "/api/v1/workers/"+regResp.WorkerID, nil)
	req.Header.Set("Authorization", "Bearer test-token")
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, `"throughput_fps"`) {
		t.Fatalf("Expected health response to contain throughput_fps, got: %s", body)
	}

	var getResp struct {
		Worker struct {
			Health *WorkerHealth `json:"health"`
		} `json:"worker"`
	}
	if err := json.Unmarshal([]byte(body), &getResp); err != nil {
		t.Fatalf("Failed to decode get worker response: %v", err)
	}
	if getResp.Worker.Health == nil {
		t.Fatalf("Expected non-null health for idle worker")
	}
	if getResp.Worker.Health.ThroughputFPS != 0 {
		t.Errorf("Expected throughput_fps 0 for idle worker, got %f", getResp.Worker.Health.ThroughputFPS)
	}
}

// TestListWorkersActiveOnlyFilter (TSI-2919) verifies GET /api/v1/workers
// returns every registered worker by default, and that ?active_only=true
// excludes offline rows retained within the --worker-offline-threshold window.
func TestListWorkersActiveOnlyFilter(t *testing.T) {
	h, router, cleanup := setupTest(t)
	defer cleanup()

	registerTestWorkerWithID(t, router, "worker-active", "active-node", []string{"libx264"})
	registerTestWorkerWithID(t, router, "worker-offline", "offline-node", []string{"libx264"})

	if err := h.GetDB().UpdateWorkerStatus("worker-offline", protocol.WorkerStatusOffline); err != nil {
		t.Fatalf("Failed to mark worker offline: %v", err)
	}

	list := func(query string) []struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	} {
		req := httptest.NewRequest("GET", "/api/v1/workers"+query, nil)
		req.Header.Set("Authorization", "Bearer test-token")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("List workers%s failed with status %d: %s", query, rec.Code, rec.Body.String())
		}
		var resp struct {
			Workers []struct {
				ID     string `json:"id"`
				Status string `json:"status"`
			} `json:"workers"`
		}
		if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
			t.Fatalf("Failed to decode list workers response: %v", err)
		}
		return resp.Workers
	}

	cases := []struct {
		query string
		want  int
	}{
		{"", 2},                     // default: include offline rows
		{"?active_only=true", 1},    // boolean true
		{"?active_only=1", 1},       // ParseBool accepts 1
		{"?active_only=false", 2},   // boolean false
		{"?active_only=garbage", 2}, // non-boolean -> false
	}
	for _, tc := range cases {
		if got := list(tc.query); len(got) != tc.want {
			t.Errorf("List workers%s returned %d workers, want %d", tc.query, len(got), tc.want)
		}
	}

	active := list("?active_only=true")
	if len(active) == 1 && (active[0].ID != "worker-active" || active[0].Status != string(protocol.WorkerStatusIdle)) {
		t.Errorf("Expected only worker-active (idle), got %+v", active[0])
	}
}

// TestWorkerHealthStatusTracksJobLifecycle (TSI-2347) verifies health.status
// reflects a running job as "busy" without requiring a busy heartbeat: the
// scheduler and the job-pull path keep the DB status current, and health must
// derive its status from that record rather than from the heartbeat-fed state
// table, which only refreshes every heartbeat interval.
func TestWorkerHealthStatusTracksJobLifecycle(t *testing.T) {
	_, router, cleanup := setupTest(t)
	defer cleanup()

	// Register a worker; it stays idle (no busy heartbeat is ever sent).
	regReq := protocol.WorkerRegisterRequest{
		Name: "test-worker-health-busy",
		Capabilities: protocol.WorkerCapabilities{
			Encoders:      []string{"libx264"},
			FFmpegVersion: "5.1.2",
		},
	}
	regBody, _ := json.Marshal(regReq)
	req := httptest.NewRequest("POST", "/api/v1/workers/register", bytes.NewReader(regBody))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	var regResp protocol.WorkerRegisterResponse
	if err := json.NewDecoder(rec.Body).Decode(&regResp); err != nil {
		t.Fatalf("Failed to decode register response: %v", err)
	}

	getHealth := func() *WorkerHealth {
		req := httptest.NewRequest("GET", "/api/v1/workers/"+regResp.WorkerID, nil)
		req.Header.Set("Authorization", "Bearer test-token")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("Get worker failed with status %d: %s", rec.Code, rec.Body.String())
		}
		var resp struct {
			Worker struct {
				Status string        `json:"status"`
				Health *WorkerHealth `json:"health"`
			} `json:"worker"`
		}
		if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
			t.Fatalf("Failed to decode get worker response: %v", err)
		}
		return resp.Worker.Health
	}

	if h := getHealth(); h == nil || h.Status != string(protocol.WorkerStatusIdle) {
		t.Fatalf("Expected idle health before any job, got %+v", h)
	}

	// Submit a job and let the worker pull it. The pull path assigns the job
	// and must mark the worker busy in the same transaction.
	fileContent := []byte("test content")
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, _ := writer.CreateFormFile("file", "test.mp4")
	part.Write(fileContent)
	writer.Close()

	req = httptest.NewRequest("POST", "/api/v1/upload", body)
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", writer.FormDataContentType())
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	var uploadResp protocol.UploadResponse
	json.NewDecoder(w.Body).Decode(&uploadResp)

	jobReq := protocol.JobSubmitRequest{
		InputFiles: []string{uploadResp.FileID},
		Args:       []string{"-c:v", "libx264"},
	}
	jobBody, _ := json.Marshal(jobReq)
	req = httptest.NewRequest("POST", "/api/v1/jobs", bytes.NewReader(jobBody))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	var jobResp protocol.JobSubmitResponse
	if w.Code != http.StatusOK {
		t.Fatalf("Job submission failed with status %d: %s", w.Code, w.Body.String())
	}
	json.NewDecoder(w.Body).Decode(&jobResp)

	pullReq := httptest.NewRequest("GET", "/api/v1/workers/"+regResp.WorkerID+"/jobs", nil)
	pullReq.Header.Set("Authorization", "Bearer test-token")
	router.ServeHTTP(w, pullReq)
	if w.Code != http.StatusOK {
		t.Fatalf("Pull jobs failed with status %d: %s", w.Code, w.Body.String())
	}

	// While the job is queued/running on the worker, health.status must be busy.
	if h := getHealth(); h == nil || h.Status != string(protocol.WorkerStatusBusy) {
		t.Errorf("Expected health status 'busy' while a job is active, got %+v", h)
	}

	// Terminal status report flips the worker back to idle via the UpdateJob hook.
	updateReq := protocol.JobUpdateRequest{
		Status:   protocol.JobStatusCompleted,
		ExitCode: 0,
	}
	updateBody, _ := json.Marshal(updateReq)
	req = httptest.NewRequest("PATCH", fmt.Sprintf("/api/v1/jobs/%s", jobResp.JobID), bytes.NewReader(updateBody))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("Update job failed with status %d: %s", w.Code, w.Body.String())
	}

	if h := getHealth(); h == nil || h.Status != string(protocol.WorkerStatusIdle) {
		t.Errorf("Expected health status 'idle' after job completion, got %+v", h)
	}
}

// TestGetWorkerUUIDFormatMismatch (TSI-2346) verifies the detail endpoint
// resolves a worker whose ID is a UUID regardless of hyphenation or case in
// the URL, and that id/name/status/health are populated from the DB record
// when no state-table entry matches.
func TestGetWorkerUUIDFormatMismatch(t *testing.T) {
	_, router, cleanup := setupTest(t)
	defer cleanup()

	hyphenated := "550e8400-e29b-41d4-a716-446655440000"
	regReq := protocol.WorkerRegisterRequest{
		WorkerID: hyphenated,
		Name:     "uuid-worker",
		Capabilities: protocol.WorkerCapabilities{
			Encoders:      []string{"libx264"},
			FFmpegVersion: "5.1.2",
		},
	}
	regBody, _ := json.Marshal(regReq)
	req := httptest.NewRequest("POST", "/api/v1/workers/register", bytes.NewReader(regBody))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("Register failed with status %d: %s", rec.Code, rec.Body.String())
	}

	getWorker := func(id string) (int, map[string]any) {
		req := httptest.NewRequest("GET", "/api/v1/workers/"+id, nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		var body map[string]any
		json.NewDecoder(rec.Body).Decode(&body)
		return rec.Code, body
	}

	for name, id := range map[string]string{
		"exact":     hyphenated,
		"compact":   "550e8400e29b41d4a716446655440000",
		"uppercase": strings.ToUpper(hyphenated),
	} {
		code, body := getWorker(id)
		if code != http.StatusOK {
			t.Errorf("%s lookup failed with status %d: %v", name, code, body)
			continue
		}
		worker, _ := body["worker"].(map[string]any)
		if worker == nil {
			t.Fatalf("%s lookup missing worker object", name)
		}
		if worker["id"] != hyphenated {
			t.Errorf("%s lookup returned id %v, want stored ID %q", name, worker["id"], hyphenated)
		}
		if worker["name"] != "uuid-worker" {
			t.Errorf("%s lookup returned name %v, want uuid-worker", name, worker["name"])
		}
		if worker["status"] != string(protocol.WorkerStatusIdle) {
			t.Errorf("%s lookup returned status %v, want idle", name, worker["status"])
		}
		if worker["health"] == nil {
			t.Errorf("%s lookup returned null health", name)
		}
	}

	// Unknown non-UUID ID still 404s.
	if code, _ := getWorker("non-existent-id"); code != http.StatusNotFound {
		t.Errorf("Expected 404 for unknown ID, got %d", code)
	}
}

func TestCancelCompletedJob(t *testing.T) {
	_, router, cleanup := setupTest(t)
	defer cleanup()

	// Register a worker with libx264 encoder (TSI-1428: required for job submission)
	registerTestWorker(t, router, []string{"libx264"})

	// Create job
	fileContent := []byte("test content")
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, _ := writer.CreateFormFile("file", "test.mp4")
	part.Write(fileContent)
	writer.Close()

	req := httptest.NewRequest("POST", "/api/v1/upload", body)
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", writer.FormDataContentType())
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	var uploadResp protocol.UploadResponse
	json.NewDecoder(w.Body).Decode(&uploadResp)

	jobReq := protocol.JobSubmitRequest{
		InputFiles: []string{uploadResp.FileID},
		Args:       []string{"-c:v", "libx264"},
	}
	jobBody, _ := json.Marshal(jobReq)

	req = httptest.NewRequest("POST", "/api/v1/jobs", bytes.NewReader(jobBody))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)

	var jobResp protocol.JobSubmitResponse
	json.NewDecoder(w.Body).Decode(&jobResp)

	// Update job to completed status
	updateReq := protocol.JobUpdateRequest{
		Status:   protocol.JobStatusCompleted,
		ExitCode: 0,
	}
	updateBody, _ := json.Marshal(updateReq)

	req = httptest.NewRequest("PATCH", fmt.Sprintf("/api/v1/jobs/%s", jobResp.JobID), bytes.NewReader(updateBody))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)

	// Try to cancel completed job - should fail
	req = httptest.NewRequest("DELETE", fmt.Sprintf("/api/v1/jobs/%s", jobResp.JobID), nil)
	req.Header.Set("Authorization", "Bearer test-token")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("Expected status 400 for cancelling completed job, got %d", w.Code)
	}
}

func TestHeartbeatReturnsCancelledJobs(t *testing.T) {
	_, router, cleanup := setupTest(t)
	defer cleanup()

	// Register a worker with libx264 encoder (TSI-1428: required for job submission)
	registerTestWorker(t, router, []string{"libx264"})

	// Register worker
	regReq := protocol.WorkerRegisterRequest{
		Name: "test-worker",
		Capabilities: protocol.WorkerCapabilities{
			Encoders:      []string{"libx264"},
			FFmpegVersion: "5.1.2",
		},
	}
	regBody, _ := json.Marshal(regReq)

	req := httptest.NewRequest("POST", "/api/v1/workers/register", bytes.NewReader(regBody))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	var regResp protocol.WorkerRegisterResponse
	json.NewDecoder(w.Body).Decode(&regResp)

	// Create job
	fileContent := []byte("test content")
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, _ := writer.CreateFormFile("file", "test.mp4")
	part.Write(fileContent)
	writer.Close()

	req = httptest.NewRequest("POST", "/api/v1/upload", body)
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", writer.FormDataContentType())
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)

	var uploadResp protocol.UploadResponse
	json.NewDecoder(w.Body).Decode(&uploadResp)

	jobReq := protocol.JobSubmitRequest{
		InputFiles: []string{uploadResp.FileID},
		Args:       []string{"-c:v", "libx264"},
	}
	jobBody, _ := json.Marshal(jobReq)

	req = httptest.NewRequest("POST", "/api/v1/jobs", bytes.NewReader(jobBody))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)

	var jobResp protocol.JobSubmitResponse
	json.NewDecoder(w.Body).Decode(&jobResp)

	// Cancel the job
	req = httptest.NewRequest("DELETE", fmt.Sprintf("/api/v1/jobs/%s", jobResp.JobID), nil)
	req.Header.Set("Authorization", "Bearer test-token")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Cancel job failed with status %d", w.Code)
	}

	// Send heartbeat with the cancelled job as active
	heartbeatReq := protocol.WorkerHeartbeatRequest{
		WorkerID:   regResp.WorkerID,
		Status:     protocol.WorkerStatusBusy,
		ActiveJobs: []string{jobResp.JobID},
	}
	heartbeatBody, _ := json.Marshal(heartbeatReq)

	req = httptest.NewRequest("POST", "/api/v1/workers/heartbeat", bytes.NewReader(heartbeatBody))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Heartbeat failed with status %d: %s", w.Code, w.Body.String())
	}

	var resp protocol.WorkerHeartbeatResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("Failed to decode response: %v", err)
	}

	// The cancelled job should appear in the response
	if len(resp.CancelledJobs) != 1 || resp.CancelledJobs[0] != jobResp.JobID {
		t.Errorf("Expected cancelled_jobs to contain job ID %s, got %v", jobResp.JobID, resp.CancelledJobs)
	}
}

func TestWorkerRegistrationWithInvalidCapabilities(t *testing.T) {
	_, router, cleanup := setupTest(t)
	defer cleanup()

	// Register worker without required fields
	regReq := protocol.WorkerRegisterRequest{
		Name:         "test-worker",
		Capabilities: protocol.WorkerCapabilities{
			// Missing encoders and ffmpeg_version
		},
	}
	regBody, _ := json.Marshal(regReq)

	req := httptest.NewRequest("POST", "/api/v1/workers/register", bytes.NewReader(regBody))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("Expected status 400, got %d", w.Code)
	}
}

// TestSubmitJobNoWorkerAvailable tests that job submission fails fast when no workers are available (TSI-1428)
func TestSubmitJobNoWorkerAvailable(t *testing.T) {
	_, router, cleanup := setupTest(t)
	defer cleanup()

	// Upload file
	fileContent := []byte("test video content")
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, _ := writer.CreateFormFile("file", "test.mp4")
	part.Write(fileContent)
	writer.Close()

	req := httptest.NewRequest("POST", "/api/v1/upload", body)
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", writer.FormDataContentType())
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Upload failed with status %d: %s", w.Code, w.Body.String())
	}

	var uploadResp protocol.UploadResponse
	if err := json.NewDecoder(w.Body).Decode(&uploadResp); err != nil {
		t.Fatalf("Failed to decode upload response: %v", err)
	}

	// Submit job WITHOUT registering any worker
	jobReq := protocol.JobSubmitRequest{
		InputFiles: []string{uploadResp.FileID},
		Args:       []string{"-c:v", "libx264", "-preset", "fast"},
	}
	jobBody, _ := json.Marshal(jobReq)

	req = httptest.NewRequest("POST", "/api/v1/jobs", bytes.NewReader(jobBody))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)

	// Should return 503 Service Unavailable
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("Expected status 503, got %d: %s", w.Code, w.Body.String())
	}

	var errResp protocol.ErrorResponse
	if err := json.NewDecoder(w.Body).Decode(&errResp); err != nil {
		t.Fatalf("Failed to decode error response: %v", err)
	}

	if errResp.Code != protocol.ErrCodeWorkerUnavailable {
		t.Errorf("Expected error code 'worker_unavailable', got '%s'", errResp.Code)
	}

	if errResp.Message == "" {
		t.Error("Expected error message to be non-empty")
	}
}

// TestSubmitJobNoWorkerWithEncoder verifies a job requesting an encoder no
// worker supports is persisted and failed as ENCODER_UNAVAILABLE rather than
// rejected with a 503 that leaves no DB row (TSI-2846; formerly TSI-1428).
func TestSubmitJobNoWorkerWithEncoder(t *testing.T) {
	_, router, cleanup := setupTest(t)
	defer cleanup()

	// Register a worker with a different encoder (not libx265)
	registerTestWorker(t, router, []string{"libx264"})

	// Upload file
	fileContent := []byte("test video content")
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, _ := writer.CreateFormFile("file", "test.mp4")
	part.Write(fileContent)
	writer.Close()

	req := httptest.NewRequest("POST", "/api/v1/upload", body)
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", writer.FormDataContentType())
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	var uploadResp protocol.UploadResponse
	json.NewDecoder(w.Body).Decode(&uploadResp)

	// Submit job requesting libx265 encoder (not available)
	jobReq := protocol.JobSubmitRequest{
		InputFiles: []string{uploadResp.FileID},
		Args:       []string{"-c:v", "libx265", "-preset", "fast"},
	}
	jobBody, _ := json.Marshal(jobReq)

	req = httptest.NewRequest("POST", "/api/v1/jobs", bytes.NewReader(jobBody))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)

	// TSI-2846: the job is persisted and failed as ENCODER_UNAVAILABLE (200),
	// not rejected with a 503 that leaves no DB row.
	if w.Code != http.StatusOK {
		t.Fatalf("Expected status 200, got %d: %s", w.Code, w.Body.String())
	}

	var jobResp protocol.JobSubmitResponse
	if err := json.NewDecoder(w.Body).Decode(&jobResp); err != nil {
		t.Fatalf("Failed to decode job submit response: %v", err)
	}
	if jobResp.JobID == "" {
		t.Fatal("Expected non-empty job ID")
	}

	// The job must be failed with failure_type ENCODER_UNAVAILABLE.
	req = httptest.NewRequest("GET", "/api/v1/jobs/"+jobResp.JobID, nil)
	req.Header.Set("Authorization", "Bearer test-token")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("Failed to get job: status %d, body: %s", w.Code, w.Body.String())
	}
	var statusResp protocol.JobStatusResponse
	if err := json.NewDecoder(w.Body).Decode(&statusResp); err != nil {
		t.Fatalf("Failed to decode job status response: %v", err)
	}
	if statusResp.Job.Status != protocol.JobStatusFailed {
		t.Errorf("Expected job status 'failed', got '%s'", statusResp.Job.Status)
	}
	if statusResp.Job.FailureType != string(protocol.FailureEncoderUnavailable) {
		t.Errorf("Expected failure_type %q, got %q", protocol.FailureEncoderUnavailable, statusResp.Job.FailureType)
	}
	// TSI-2930: the pre-scheduling rejection never assigned a worker, so no
	// attribution is written — this distinguishes it from runtime failures.
	if statusResp.Job.AssignedWorker != "" || statusResp.Job.WorkerName != "" {
		t.Errorf("submit-time rejection must not write attribution, got assigned_worker=%q worker_name=%q",
			statusResp.Job.AssignedWorker, statusResp.Job.WorkerName)
	}
	if !strings.Contains(statusResp.Job.Error, "libx265") {
		t.Errorf("Expected error message to mention 'libx265', got: %s", statusResp.Job.Error)
	}
}

// TestSubmitJobUnknownEncoderPersistedAsUnavailable locks the TSI-2846 issue
// example: an encoder no worker has ever registered (e.g. a typo) is persisted
// and failed as ENCODER_UNAVAILABLE, not rejected with a 503 leaving no row.
func TestSubmitJobUnknownEncoderPersistedAsUnavailable(t *testing.T) {
	_, router, cleanup := setupTest(t)
	defer cleanup()

	registerTestWorker(t, router, []string{"libx264"})
	fileID := uploadTestFile(t, router)

	jobReq := protocol.JobSubmitRequest{
		InputFiles: []string{fileID},
		Args:       []string{"-c:v", "nonexistent_codec", "-preset", "fast"},
	}
	jobBody, _ := json.Marshal(jobReq)

	req := httptest.NewRequest("POST", "/api/v1/jobs", bytes.NewReader(jobBody))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("Expected status 200, got %d: %s", w.Code, w.Body.String())
	}
	var jobResp protocol.JobSubmitResponse
	if err := json.NewDecoder(w.Body).Decode(&jobResp); err != nil {
		t.Fatalf("Failed to decode job submit response: %v", err)
	}
	if jobResp.JobID == "" {
		t.Fatal("Expected non-empty job ID")
	}

	req = httptest.NewRequest("GET", "/api/v1/jobs/"+jobResp.JobID, nil)
	req.Header.Set("Authorization", "Bearer test-token")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	var statusResp protocol.JobStatusResponse
	if err := json.NewDecoder(w.Body).Decode(&statusResp); err != nil {
		t.Fatalf("Failed to decode job status response: %v", err)
	}
	if statusResp.Job.Status != protocol.JobStatusFailed {
		t.Errorf("Expected status 'failed', got '%s'", statusResp.Job.Status)
	}
	if statusResp.Job.FailureType != string(protocol.FailureEncoderUnavailable) {
		t.Errorf("Expected failure_type %q, got %q", protocol.FailureEncoderUnavailable, statusResp.Job.FailureType)
	}
}

// TestSubmitJobEncoderCheckDBErrorFailsClosed locks the TSI-2846 review fix: a
// DB error during the encoder-capability check must fail closed (503
// worker_unavailable), not be treated as "no worker has the encoder" and
// persisted as a terminal ENCODER_UNAVAILABLE.
func TestSubmitJobEncoderCheckDBErrorFailsClosed(t *testing.T) {
	h, router, cleanup := setupTest(t)
	defer cleanup()

	registerTestWorker(t, router, []string{"libx264"})
	// Corrupt the worker's encoders JSON so the json_each() encoder-capability
	// query fails while iterating rows, while the plain schedulable-worker
	// query still succeeds.
	if _, err := h.GetDB().GetDB().Exec(`UPDATE workers SET encoders = 'not-json' WHERE id = 'test-worker-1'`); err != nil {
		t.Fatalf("Failed to corrupt worker encoders: %v", err)
	}

	fileID := uploadTestFile(t, router)

	jobReq := protocol.JobSubmitRequest{
		InputFiles: []string{fileID},
		Args:       []string{"-c:v", "hevc_nvenc", "-preset", "fast"},
	}
	jobBody, _ := json.Marshal(jobReq)

	req := httptest.NewRequest("POST", "/api/v1/jobs", bytes.NewReader(jobBody))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("Expected 503 (fail-closed) on encoder-query DB error, got %d: %s", w.Code, w.Body.String())
	}
	var errResp protocol.ErrorResponse
	if err := json.NewDecoder(w.Body).Decode(&errResp); err != nil {
		t.Fatalf("Failed to decode error response: %v", err)
	}
	if errResp.Code != protocol.ErrCodeWorkerUnavailable {
		t.Errorf("Expected 'worker_unavailable', got '%s'", errResp.Code)
	}
}

// TestSubmitJobCreateFailedJobErrorReturns500 locks the TSI-2846 review fix:
// when the atomic failed-job INSERT fails (transient DB fault), submission must
// return 500 — never 200 claiming the job was recorded as ENCODER_UNAVAILABLE.
func TestSubmitJobCreateFailedJobErrorReturns500(t *testing.T) {
	h, router, cleanup := setupTest(t)
	defer cleanup()

	registerTestWorker(t, router, []string{"libx264"})
	fileID := uploadTestFile(t, router)

	// Drop the jobs table so the failed-job INSERT fails while the earlier
	// workers-table reads still succeed, isolating the create-failure path.
	if _, err := h.GetDB().GetDB().Exec(`DROP TABLE jobs`); err != nil {
		t.Fatalf("Failed to drop jobs table: %v", err)
	}

	jobReq := protocol.JobSubmitRequest{
		InputFiles: []string{fileID},
		Args:       []string{"-c:v", "hevc_nvenc", "-preset", "fast"},
	}
	jobBody, _ := json.Marshal(jobReq)

	req := httptest.NewRequest("POST", "/api/v1/jobs", bytes.NewReader(jobBody))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("Expected 500 on failed-job INSERT error, got %d: %s", w.Code, w.Body.String())
	}
}

// TSI-2204: Test that job submission is accepted (queued) when the only worker is busy,
// instead of being rejected with 503.
func TestSubmitJobBusyWorkerQueued(t *testing.T) {
	_, router, cleanup := setupTest(t)
	defer cleanup()

	// Register a single worker
	workerID := registerTestWorker(t, router, []string{"libx264"})

	// Mark the worker busy via heartbeat
	heartbeatReq := protocol.WorkerHeartbeatRequest{
		WorkerID: workerID,
		Status:   protocol.WorkerStatusBusy,
	}
	heartbeatBody, _ := json.Marshal(heartbeatReq)
	req := httptest.NewRequest("POST", "/api/v1/workers/heartbeat", bytes.NewReader(heartbeatBody))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("Failed to mark worker busy: status %d, body: %s", w.Code, w.Body.String())
	}

	// Upload file
	fileContent := []byte("test video content")
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, _ := writer.CreateFormFile("file", "test.mp4")
	part.Write(fileContent)
	writer.Close()

	req = httptest.NewRequest("POST", "/api/v1/upload", body)
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", writer.FormDataContentType())
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("Upload failed with status %d: %s", w.Code, w.Body.String())
	}

	var uploadResp protocol.UploadResponse
	if err := json.NewDecoder(w.Body).Decode(&uploadResp); err != nil {
		t.Fatalf("Failed to decode upload response: %v", err)
	}

	// Submit a job while the only worker is busy
	jobReq := protocol.JobSubmitRequest{
		InputFiles: []string{uploadResp.FileID},
		Args:       []string{"-c:v", "libx264", "-preset", "fast"},
	}
	jobBody, _ := json.Marshal(jobReq)

	req = httptest.NewRequest("POST", "/api/v1/jobs", bytes.NewReader(jobBody))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)

	// Should succeed (200) and queue the job instead of rejecting with 503
	if w.Code != http.StatusOK {
		t.Fatalf("Expected status 200 (queued), got %d. Body: %s", w.Code, w.Body.String())
	}

	var jobResp protocol.JobSubmitResponse
	if err := json.NewDecoder(w.Body).Decode(&jobResp); err != nil {
		t.Fatalf("Failed to decode job submit response: %v", err)
	}
	if jobResp.JobID == "" {
		t.Fatal("Expected non-empty job ID")
	}

	// The job should be created in pending state, waiting for the worker to become idle
	req = httptest.NewRequest("GET", "/api/v1/jobs/"+jobResp.JobID, nil)
	req.Header.Set("Authorization", "Bearer test-token")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("Failed to get job: status %d, body: %s", w.Code, w.Body.String())
	}

	var statusResp protocol.JobStatusResponse
	if err := json.NewDecoder(w.Body).Decode(&statusResp); err != nil {
		t.Fatalf("Failed to decode job status response: %v", err)
	}
	if statusResp.Job.Status != protocol.JobStatusPending {
		t.Errorf("Expected job status 'pending' (queued waiting for worker), got '%s'", statusResp.Job.Status)
	}
}

// TSI-1500: Test that handler allows job submission when compatible encoder is available
func TestSubmitJobWithCompatibleEncoderFallback(t *testing.T) {
	_, router, cleanup := setupTest(t)
	defer cleanup()

	// Register a worker with libx264 but NOT h264_nvenc
	registerTestWorker(t, router, []string{"libx264"})

	// Upload file
	fileContent := []byte("test video content")
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, _ := writer.CreateFormFile("file", "test.mp4")
	part.Write(fileContent)
	writer.Close()

	req := httptest.NewRequest("POST", "/api/v1/upload", body)
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", writer.FormDataContentType())
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("Upload failed with status %d: %s", w.Code, w.Body.String())
	}

	var uploadResp protocol.UploadResponse
	if err := json.NewDecoder(w.Body).Decode(&uploadResp); err != nil {
		t.Fatalf("Failed to decode upload response: %v", err)
	}

	// Submit job requesting h264_nvenc (worker has libx264, same codec family)
	jobReq := protocol.JobSubmitRequest{
		InputFiles: []string{uploadResp.FileID},
		Args:       []string{"-i", "input.mp4", "-c:v", "h264_nvenc", "output.mp4"},
	}
	jobBody, _ := json.Marshal(jobReq)

	req = httptest.NewRequest("POST", "/api/v1/jobs", bytes.NewReader(jobBody))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)

	// Should succeed (200 or 201) because libx264 is a compatible encoder
	if w.Code != http.StatusOK && w.Code != http.StatusCreated {
		t.Errorf("Expected status 200 or 201, got %d. Body: %s", w.Code, w.Body.String())
	}
}

// TSI-1500/TSI-2846: a job requesting an encoder in a codec family no worker
// supports is persisted and failed as ENCODER_UNAVAILABLE, not rejected 503.
func TestSubmitJobNoCompatibleEncoder(t *testing.T) {
	_, router, cleanup := setupTest(t)
	defer cleanup()

	// Register a worker with only libx264 (h264 family)
	registerTestWorker(t, router, []string{"libx264"})

	// Upload file
	fileContent := []byte("test video content")
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, _ := writer.CreateFormFile("file", "test.mp4")
	part.Write(fileContent)
	writer.Close()

	req := httptest.NewRequest("POST", "/api/v1/upload", body)
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", writer.FormDataContentType())
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("Upload failed with status %d: %s", w.Code, w.Body.String())
	}

	var uploadResp protocol.UploadResponse
	if err := json.NewDecoder(w.Body).Decode(&uploadResp); err != nil {
		t.Fatalf("Failed to decode upload response: %v", err)
	}

	// Submit job requesting hevc_nvenc (worker has libx264, DIFFERENT codec family)
	jobReq := protocol.JobSubmitRequest{
		InputFiles: []string{uploadResp.FileID},
		Args:       []string{"-i", "input.mp4", "-c:v", "hevc_nvenc", "output.mp4"},
	}
	jobBody, _ := json.Marshal(jobReq)

	req = httptest.NewRequest("POST", "/api/v1/jobs", bytes.NewReader(jobBody))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)

	// TSI-2846: persisted and failed as ENCODER_UNAVAILABLE (200), not 503.
	if w.Code != http.StatusOK {
		t.Fatalf("Expected status 200, got %d. Body: %s", w.Code, w.Body.String())
	}

	var jobResp protocol.JobSubmitResponse
	if err := json.NewDecoder(w.Body).Decode(&jobResp); err != nil {
		t.Fatalf("Failed to decode job submit response: %v", err)
	}
	if jobResp.JobID == "" {
		t.Fatal("Expected non-empty job ID")
	}

	req = httptest.NewRequest("GET", "/api/v1/jobs/"+jobResp.JobID, nil)
	req.Header.Set("Authorization", "Bearer test-token")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("Failed to get job: status %d, body: %s", w.Code, w.Body.String())
	}
	var statusResp protocol.JobStatusResponse
	if err := json.NewDecoder(w.Body).Decode(&statusResp); err != nil {
		t.Fatalf("Failed to decode job status response: %v", err)
	}
	if statusResp.Job.Status != protocol.JobStatusFailed {
		t.Errorf("Expected job status 'failed', got '%s'", statusResp.Job.Status)
	}
	if statusResp.Job.FailureType != string(protocol.FailureEncoderUnavailable) {
		t.Errorf("Expected failure_type %q, got %q", protocol.FailureEncoderUnavailable, statusResp.Job.FailureType)
	}
	if !strings.Contains(statusResp.Job.Error, "hevc_nvenc") {
		t.Errorf("Expected error message to mention 'hevc_nvenc', got: %s", statusResp.Job.Error)
	}
}

// TSI-1500: Test that exact encoder match is preferred
func TestSubmitJobExactEncoderMatchPreferred(t *testing.T) {
	_, router, cleanup := setupTest(t)
	defer cleanup()

	// Register a worker with both h264_nvenc and libx264
	registerTestWorker(t, router, []string{"h264_nvenc", "libx264"})

	// Upload file
	fileContent := []byte("test video content")
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, _ := writer.CreateFormFile("file", "test.mp4")
	part.Write(fileContent)
	writer.Close()

	req := httptest.NewRequest("POST", "/api/v1/upload", body)
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", writer.FormDataContentType())
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("Upload failed with status %d: %s", w.Code, w.Body.String())
	}

	var uploadResp protocol.UploadResponse
	if err := json.NewDecoder(w.Body).Decode(&uploadResp); err != nil {
		t.Fatalf("Failed to decode upload response: %v", err)
	}

	// Submit job requesting h264_nvenc (exact match available)
	jobReq := protocol.JobSubmitRequest{
		InputFiles: []string{uploadResp.FileID},
		Args:       []string{"-i", "input.mp4", "-c:v", "h264_nvenc", "output.mp4"},
	}
	jobBody, _ := json.Marshal(jobReq)

	req = httptest.NewRequest("POST", "/api/v1/jobs", bytes.NewReader(jobBody))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)

	// Should succeed (200 or 201) with exact match
	if w.Code != http.StatusOK && w.Code != http.StatusCreated {
		t.Errorf("Expected status 200 or 201, got %d. Body: %s", w.Code, w.Body.String())
	}
}

// TSI-2419: a registered worker whose last heartbeat is older than the
// configured heartbeat timeout must be treated as unavailable at submission
// time, so the CLI gets an immediate 503 instead of a pending job that only
// fails after the no-worker job timeout (default 2m).
func TestSubmitJobStaleHeartbeatWorkerRejected(t *testing.T) {
	h, router, cleanup := setupTest(t)
	defer cleanup()

	h.SetHeartbeatTimeout(90 * time.Second)

	// Register a worker with the requested encoder.
	registerTestWorker(t, router, []string{"libx264"})

	// Backdate its heartbeat beyond the freshness window without waiting:
	// the monitor would flip it offline on its next tick; submission must not
	// depend on that having happened yet.
	if _, err := h.GetDB().GetDB().Exec(
		`UPDATE workers SET last_heartbeat = ? WHERE id = ?`,
		time.Now().Add(-2*time.Minute), "test-worker-1",
	); err != nil {
		t.Fatalf("Failed to backdate worker heartbeat: %v", err)
	}

	fileID := uploadTestFile(t, router)

	jobReq := protocol.JobSubmitRequest{
		InputFiles: []string{fileID},
		Args:       []string{"-c:v", "libx264", "-preset", "fast"},
	}
	jobBody, _ := json.Marshal(jobReq)

	req := httptest.NewRequest("POST", "/api/v1/jobs", bytes.NewReader(jobBody))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("Expected status 503 for stale-heartbeat worker, got %d. Body: %s", w.Code, w.Body.String())
	}

	var errResp protocol.ErrorResponse
	if err := json.NewDecoder(w.Body).Decode(&errResp); err != nil {
		t.Fatalf("Failed to decode error response: %v", err)
	}
	if errResp.Code != protocol.ErrCodeWorkerUnavailable {
		t.Errorf("Expected error code 'worker_unavailable', got '%s'", errResp.Code)
	}
}

// TSI-2419: a worker with a fresh heartbeat still counts as available even
// when a freshness window is configured — busy workers queue (TSI-2204),
// stale ones are the only ones filtered out.
func TestSubmitJobFreshHeartbeatWorkerAccepted(t *testing.T) {
	h, router, cleanup := setupTest(t)
	defer cleanup()

	h.SetHeartbeatTimeout(90 * time.Second)

	registerTestWorker(t, router, []string{"libx264"})

	fileID := uploadTestFile(t, router)

	jobReq := protocol.JobSubmitRequest{
		InputFiles: []string{fileID},
		Args:       []string{"-c:v", "libx264", "-preset", "fast"},
	}
	jobBody, _ := json.Marshal(jobReq)

	req := httptest.NewRequest("POST", "/api/v1/jobs", bytes.NewReader(jobBody))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK && w.Code != http.StatusCreated {
		t.Errorf("Expected job acceptance with fresh-heartbeat worker, got %d. Body: %s", w.Code, w.Body.String())
	}
}

// TSI-2419: when no freshness window is configured (<=0), heartbeat age is
// ignored and the pre-existing behavior applies — any schedulable
// (non-offline, non-evicted) worker makes submission succeed.
func TestSubmitJobNoFreshnessCheckWhenDisabled(t *testing.T) {
	h, router, cleanup := setupTest(t)
	defer cleanup()

	h.SetHeartbeatTimeout(0)

	registerTestWorker(t, router, []string{"libx264"})
	if _, err := h.GetDB().GetDB().Exec(
		`UPDATE workers SET last_heartbeat = ? WHERE id = ?`,
		time.Now().Add(-2*time.Minute), "test-worker-1",
	); err != nil {
		t.Fatalf("Failed to backdate worker heartbeat: %v", err)
	}

	fileID := uploadTestFile(t, router)

	jobReq := protocol.JobSubmitRequest{
		InputFiles: []string{fileID},
		Args:       []string{"-c:v", "libx264", "-preset", "fast"},
	}
	jobBody, _ := json.Marshal(jobReq)

	req := httptest.NewRequest("POST", "/api/v1/jobs", bytes.NewReader(jobBody))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK && w.Code != http.StatusCreated {
		t.Errorf("Expected job acceptance when freshness check disabled, got %d. Body: %s", w.Code, w.Body.String())
	}
}

// TSI-2419 regression (review blocker #1 on PR #38): the requested-encoder
// path must apply heartbeat freshness too. A stale-heartbeat worker with the
// requested encoder must not satisfy the exact-match check just because
// another live worker WITHOUT that encoder exists — that combination used to
// pass both checks, accept the job as pending, and leave it hanging until
// the no-worker job timeout.
func TestSubmitJobStaleEncoderWorkerWithLiveOtherWorkerRejected(t *testing.T) {
	h, router, cleanup := setupTest(t)
	defer cleanup()

	h.SetHeartbeatTimeout(90 * time.Second)

	// Worker 1: has libx264 but its heartbeat is stale.
	registerTestWorker(t, router, []string{"libx264"})
	if _, err := h.GetDB().GetDB().Exec(
		`UPDATE workers SET last_heartbeat = ? WHERE id = ?`,
		time.Now().Add(-2*time.Minute), "test-worker-1",
	); err != nil {
		t.Fatalf("Failed to backdate worker heartbeat: %v", err)
	}

	// Worker 2: live, but no libx264 (different codec family to avoid the
	// compatible-encoder fallback matching it).
	registerTestWorkerWithID(t, router, "test-worker-2", "live-worker", []string{"libvpx-vp9"})

	fileID := uploadTestFile(t, router)

	jobReq := protocol.JobSubmitRequest{
		InputFiles: []string{fileID},
		Args:       []string{"-c:v", "libx264", "-preset", "fast"},
	}
	jobBody, _ := json.Marshal(jobReq)

	req := httptest.NewRequest("POST", "/api/v1/jobs", bytes.NewReader(jobBody))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("Expected status 503 when only stale worker has the encoder, got %d. Body: %s", w.Code, w.Body.String())
	}
}

// TSI-2474: a probe request when no live worker is available must fail
// immediately with 503 worker_unavailable instead of dispatching a job that
// sits pending for the entire 2-minute poll loop before returning "timeout".
func TestProbeNoWorkerFailsImmediately(t *testing.T) {
	h, router, cleanup := setupTest(t)
	defer cleanup()

	h.SetHeartbeatTimeout(90 * time.Second)

	// No worker registered — the cluster is empty.

	probeReq := protocol.ProbeRequest{Input: "rtmp://example.com/test"}
	body, _ := json.Marshal(probeReq)

	req := httptest.NewRequest("POST", "/api/v1/probe", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("Expected status 503 for probe with no workers, got %d. Body: %s", w.Code, w.Body.String())
	}

	var errResp protocol.ErrorResponse
	if err := json.NewDecoder(w.Body).Decode(&errResp); err != nil {
		t.Fatalf("Failed to decode error response: %v", err)
	}
	if errResp.Code != protocol.ErrCodeWorkerUnavailable {
		t.Errorf("Expected error code 'worker_unavailable', got '%s'", errResp.Code)
	}
}

// TSI-2474: a stale-heartbeat worker (registered but dead in practice) must
// not satisfy the probe availability check — the probe must still fail fast.
func TestProbeStaleHeartbeatWorkerFailsImmediately(t *testing.T) {
	h, router, cleanup := setupTest(t)
	defer cleanup()

	h.SetHeartbeatTimeout(90 * time.Second)

	registerTestWorker(t, router, []string{"libx264"})
	if _, err := h.GetDB().GetDB().Exec(
		`UPDATE workers SET last_heartbeat = ? WHERE id = ?`,
		time.Now().Add(-2*time.Minute), "test-worker-1",
	); err != nil {
		t.Fatalf("Failed to backdate worker heartbeat: %v", err)
	}

	// Upload a file so the input-exists check passes and the probe reaches
	// the worker-availability check.
	fileID := uploadTestFile(t, router)

	probeReq := protocol.ProbeRequest{Input: fileID}
	body, _ := json.Marshal(probeReq)

	req := httptest.NewRequest("POST", "/api/v1/probe", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("Expected status 503 for probe with stale worker, got %d. Body: %s", w.Code, w.Body.String())
	}

	var errResp protocol.ErrorResponse
	if err := json.NewDecoder(w.Body).Decode(&errResp); err != nil {
		t.Fatalf("Failed to decode error response: %v", err)
	}
	if errResp.Code != protocol.ErrCodeWorkerUnavailable {
		t.Errorf("Expected error code 'worker_unavailable', got '%s'", errResp.Code)
	}
}

// TSI-2474 happy path: a probe request when a live worker IS available must
// pass the worker-availability guard and proceed to job creation — it must
// NOT be rejected with 503 worker_unavailable. No worker pulls the job in
// this test, so the handler enters its poll loop; we cancel the request
// context to break out quickly and assert the response was not the guard's
// 503.
func TestProbeWithLiveWorkerProceeds(t *testing.T) {
	h, router, cleanup := setupTest(t)
	defer cleanup()

	h.SetHeartbeatTimeout(90 * time.Second)

	// Register a worker with a fresh heartbeat.
	registerTestWorker(t, router, []string{"libx264"})

	fileID := uploadTestFile(t, router)

	probeReq := protocol.ProbeRequest{Input: fileID}
	body, _ := json.Marshal(probeReq)

	// Give the request a cancellable context so the probe handler's poll
	// loop exits via the client-gone path instead of running for 2 minutes.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	req := httptest.NewRequestWithContext(ctx, "POST", "/api/v1/probe", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	// Run the handler in a goroutine; cancel the context shortly after it
	// starts so the poll loop's r.Context().Done() branch fires.
	done := make(chan struct{})
	go func() {
		router.ServeHTTP(w, req)
		close(done)
	}()

	time.Sleep(200 * time.Millisecond)
	cancel()
	<-done

	// The guard must not have rejected the probe: a 503 with
	// worker_unavailable would mean the live worker was not seen.
	if w.Code == http.StatusServiceUnavailable {
		var errResp protocol.ErrorResponse
		_ = json.NewDecoder(w.Body).Decode(&errResp)
		if errResp.Code == protocol.ErrCodeWorkerUnavailable {
			t.Fatalf("Probe was rejected by the worker-availability guard despite a live worker: %s (body: %s)",
				errResp.Code, w.Body.String())
		}
	}
}

// TSI-2520: in shared-FS mode the CLI sends an absolute local path as the
// probe input. The input-exists check must resolve direct paths against the
// server's filesystem (not the storage base dir), so an existing path must
// pass validation and reach the worker-availability guard (503 here, since
// no worker is registered) instead of being rejected with 404.
func TestProbeDirectPathExistsPassesInputValidation(t *testing.T) {
	_, router, cleanup := setupTest(t)
	defer cleanup()

	// No worker registered: the probe can only reach 503 if the input
	// validation passed.
	dir := t.TempDir()
	absPath := filepath.Join(dir, "media.mp4")
	if err := os.WriteFile(absPath, []byte("test video content"), 0644); err != nil {
		t.Fatalf("Failed to create direct-path test file: %v", err)
	}

	probeReq := protocol.ProbeRequest{Input: absPath}
	body, _ := json.Marshal(probeReq)
	req := httptest.NewRequest("POST", "/api/v1/probe", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("Expected 503 worker_unavailable after input validation passed, got %d. Body: %s", w.Code, w.Body.String())
	}
}

// TSI-2520: a direct path that does not exist on the server filesystem must
// still be rejected with 404 before worker dispatch.
func TestProbeDirectPathMissingRejected(t *testing.T) {
	_, router, cleanup := setupTest(t)
	defer cleanup()

	probeReq := protocol.ProbeRequest{Input: filepath.Join(t.TempDir(), "missing.mp4")}
	body, _ := json.Marshal(probeReq)
	req := httptest.NewRequest("POST", "/api/v1/probe", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("Expected 400 for missing direct path, got %d. Body: %s", w.Code, w.Body.String())
	}
	var errResp protocol.ErrorResponse
	if err := json.NewDecoder(w.Body).Decode(&errResp); err != nil {
		t.Fatalf("Failed to decode error response: %v", err)
	}
	if errResp.Code != protocol.ErrCodeNotFound {
		t.Errorf("Expected error code 'not_found', got '%s'", errResp.Code)
	}
}

// TSI-2706: a bare-API probe input containing a ".." component must be
// rejected server-side (same shared pathutil.ContainsPathTraversal the worker
// uses), even when the cleaned path would stat to an existing file.
func TestProbeDirectPathTraversalRejected(t *testing.T) {
	_, router, cleanup := setupTest(t)
	defer cleanup()

	dir := t.TempDir()
	absPath := filepath.Join(dir, "media.mp4")
	if err := os.WriteFile(absPath, []byte("test video content"), 0644); err != nil {
		t.Fatalf("Failed to create direct-path test file: %v", err)
	}
	// The ".." parent must exist so os.Stat would resolve the cleaned path to
	// an existing file — proving the rejection comes from the traversal guard,
	// not from a missing file.
	if err := os.MkdirAll(filepath.Join(dir, "subdir"), 0755); err != nil {
		t.Fatalf("Failed to create traversal parent dir: %v", err)
	}
	// String concatenation: filepath.Join would clean the ".." away before
	// it reaches the server-side guard.
	traversal := filepath.Join(dir, "subdir") + string(os.PathSeparator) + ".." + string(os.PathSeparator) + "media.mp4"
	probeReq := protocol.ProbeRequest{Input: traversal}
	body, _ := json.Marshal(probeReq)
	req := httptest.NewRequest("POST", "/api/v1/probe", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("Expected 400 for traversal direct path, got %d. Body: %s", w.Code, w.Body.String())
	}
	var errResp protocol.ErrorResponse
	if err := json.NewDecoder(w.Body).Decode(&errResp); err != nil {
		t.Fatalf("Failed to decode error response: %v", err)
	}
	if errResp.Code != protocol.ErrCodeNotFound {
		t.Errorf("Expected error code 'not_found', got '%s'", errResp.Code)
	}
}

// TSI-2718: a SubmitJob direct path containing a ".." component must be
// rejected server-side (same shared pathutil.ContainsPathTraversal the worker
// uses), fail-fast before job creation — the opposite of dispatching it and
// letting the worker reject it at runtime.
func TestSubmitJobDirectPathTraversalRejected(t *testing.T) {
	_, router, cleanup := setupTest(t)
	defer cleanup()

	jobReq := protocol.JobSubmitRequest{
		InputFiles: []string{"dummy.mp4"},
		DirectPath: []string{"../etc/passwd"},
		Args:       []string{"-c:v", "libx264", "-preset", "fast"},
	}
	jobBody, _ := json.Marshal(jobReq)
	req := httptest.NewRequest("POST", "/api/v1/jobs", bytes.NewReader(jobBody))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("Expected 400 for traversal direct path, got %d. Body: %s", w.Code, w.Body.String())
	}
	var errResp protocol.ErrorResponse
	if err := json.NewDecoder(w.Body).Decode(&errResp); err != nil {
		t.Fatalf("Failed to decode error response: %v", err)
	}
	if errResp.Code != protocol.ErrCodeInvalidRequest {
		t.Errorf("Expected error code 'invalid_request', got '%s'", errResp.Code)
	}
}

// TSI-2718: a legitimate dot-prefixed filename such as "my..video.mp4" must
// not be false-positived by the component-level guard.
func TestSubmitJobDirectPathDotFilenameAccepted(t *testing.T) {
	h, router, cleanup := setupTest(t)
	defer cleanup()

	// No worker is registered: reaching the worker-availability guard proves
	// the traversal validation passed (503) rather than being rejected (400).
	h.SetHeartbeatTimeout(0)

	jobReq := protocol.JobSubmitRequest{
		InputFiles: []string{"my..video.mp4"},
		DirectPath: []string{filepath.Join(t.TempDir(), "my..video.mp4")},
		Args:       []string{"-c:v", "libx264", "-preset", "fast"},
	}
	jobBody, _ := json.Marshal(jobReq)
	req := httptest.NewRequest("POST", "/api/v1/jobs", bytes.NewReader(jobBody))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("Expected 503 worker_unavailable after traversal validation passed, got %d. Body: %s", w.Code, w.Body.String())
	}
}

// TSI-2721: a SubmitJob output filename containing a ".." component must be
// rejected server-side in direct mode, symmetric with the worker's directMode
// output guard — refused before dispatch instead of by the worker at runtime.
func TestSubmitJobOutputFilenameTraversalRejected(t *testing.T) {
	_, router, cleanup := setupTest(t)
	defer cleanup()

	jobReq := protocol.JobSubmitRequest{
		InputFiles:     []string{"dummy.mp4"},
		DirectPath:     []string{"/srv/media/input.mp4"},
		Args:           []string{"-c:v", "libx264", "-preset", "fast"},
		OutputFilename: "../etc/passwd",
	}
	jobBody, _ := json.Marshal(jobReq)
	req := httptest.NewRequest("POST", "/api/v1/jobs", bytes.NewReader(jobBody))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("Expected 400 for traversal output filename, got %d. Body: %s", w.Code, w.Body.String())
	}
	var errResp protocol.ErrorResponse
	if err := json.NewDecoder(w.Body).Decode(&errResp); err != nil {
		t.Fatalf("Failed to decode error response: %v", err)
	}
	if errResp.Code != protocol.ErrCodeInvalidRequest {
		t.Errorf("Expected error code 'invalid_request', got '%s'", errResp.Code)
	}
}

// TSI-2721: a remote output URL containing a ".." path segment must not be
// false-positived by the component-level guard — the worker passes remote
// outputs through unchanged, so the server mirrors that exemption.
func TestSubmitJobOutputFilenameRemoteURLExempt(t *testing.T) {
	h, router, cleanup := setupTest(t)
	defer cleanup()

	// No worker is registered: reaching the worker-availability guard proves
	// the traversal validation passed (503) rather than being rejected (400).
	h.SetHeartbeatTimeout(0)

	jobReq := protocol.JobSubmitRequest{
		InputFiles:     []string{"dummy.mp4"},
		DirectPath:     []string{"/srv/media/input.mp4"},
		Args:           []string{"-c:v", "libx264", "-preset", "fast"},
		OutputFilename: "rtmp://example.com/live/../stream",
	}
	jobBody, _ := json.Marshal(jobReq)
	req := httptest.NewRequest("POST", "/api/v1/jobs", bytes.NewReader(jobBody))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("Expected 503 worker_unavailable after traversal validation passed, got %d. Body: %s", w.Code, w.Body.String())
	}
}

// TSI-2721: a "file://" output or a local path with a mid-string "://" must be
// treated as local, not remote — the worker's IsRemoteURL predicate excludes the
// file scheme and anchors the scheme at the start. Both must be rejected as
// traversal server-side rather than being exempted and refused by the worker.
func TestSubmitJobOutputFilenameLocalSchemeRejected(t *testing.T) {
	cases := []struct {
		name string
		out  string
	}{
		{"file url traversal", "file:///etc/../tmp/x.mp4"},
		{"mid-string scheme traversal", "/data/media/x://y/../../etc/passwd"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, router, cleanup := setupTest(t)
			defer cleanup()

			jobReq := protocol.JobSubmitRequest{
				InputFiles:     []string{"dummy.mp4"},
				DirectPath:     []string{"/srv/media/input.mp4"},
				Args:           []string{"-c:v", "libx264", "-preset", "fast"},
				OutputFilename: tc.out,
			}
			jobBody, _ := json.Marshal(jobReq)
			req := httptest.NewRequest("POST", "/api/v1/jobs", bytes.NewReader(jobBody))
			req.Header.Set("Authorization", "Bearer test-token")
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)

			if w.Code != http.StatusBadRequest {
				t.Fatalf("Expected 400 for local-scheme traversal output, got %d. Body: %s", w.Code, w.Body.String())
			}
			var errResp protocol.ErrorResponse
			if err := json.NewDecoder(w.Body).Decode(&errResp); err != nil {
				t.Fatalf("Failed to decode error response: %v", err)
			}
			if errResp.Code != protocol.ErrCodeInvalidRequest {
				t.Errorf("Expected error code 'invalid_request', got '%s'", errResp.Code)
			}
		})
	}
}

// TSI-2721: a legitimate dot-prefixed output filename such as "my..video.mp4"
// must not be false-positived by the component-level guard in direct mode.
func TestSubmitJobOutputFilenameDotFilenameAccepted(t *testing.T) {
	h, router, cleanup := setupTest(t)
	defer cleanup()

	// No worker is registered: reaching the worker-availability guard proves
	// the traversal validation passed (503) rather than being rejected (400).
	h.SetHeartbeatTimeout(0)

	jobReq := protocol.JobSubmitRequest{
		InputFiles:     []string{"dummy.mp4"},
		DirectPath:     []string{"/srv/media/input.mp4"},
		Args:           []string{"-c:v", "libx264", "-preset", "fast"},
		OutputFilename: "my..video.mp4",
	}
	jobBody, _ := json.Marshal(jobReq)
	req := httptest.NewRequest("POST", "/api/v1/jobs", bytes.NewReader(jobBody))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("Expected 503 worker_unavailable after traversal validation passed, got %d. Body: %s", w.Code, w.Body.String())
	}
}

// TestProbeDirectPathStoredInJob verifies that a shared-FS direct path input is
// persisted as the probe job's direct_paths so the worker can probe it locally.
// A storage file ID must keep direct_paths empty (downloaded normally), while a
// remote URL must also keep direct_paths empty. (TSI-2520)
func TestProbeDirectPathStoredInJob(t *testing.T) {
	h, router, cleanup := setupTest(t)
	defer cleanup()

	h.SetHeartbeatTimeout(90 * time.Second)
	registerTestWorker(t, router, []string{"libx264"})

	dir := t.TempDir()
	absPath := filepath.Join(dir, "media.mp4")
	if err := os.WriteFile(absPath, []byte("test video content"), 0644); err != nil {
		t.Fatalf("Failed to create direct-path test file: %v", err)
	}

	probeReq := protocol.ProbeRequest{Input: absPath}
	body, _ := json.Marshal(probeReq)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequestWithContext(ctx, "POST", "/api/v1/probe", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		router.ServeHTTP(w, req)
		close(done)
	}()
	time.Sleep(200 * time.Millisecond)
	cancel()
	<-done

	rows, err := h.GetDB().GetDB().Query(`SELECT direct_paths FROM jobs`)
	if err != nil {
		t.Fatalf("Failed to query jobs: %v", err)
	}
	defer rows.Close()

	var found bool
	for rows.Next() {
		var directPaths string
		if err := rows.Scan(&directPaths); err != nil {
			t.Fatalf("Failed to scan direct_paths: %v", err)
		}
		var paths []string
		if err := json.Unmarshal([]byte(directPaths), &paths); err != nil {
			t.Fatalf("direct_paths is not valid JSON: %v (%q)", err, directPaths)
		}
		if len(paths) == 1 && paths[0] == absPath {
			found = true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows iteration failed: %v", err)
	}
	if !found {
		t.Fatalf("probe job direct_paths does not contain %q", absPath)
	}
}
