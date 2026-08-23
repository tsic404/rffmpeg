package handlers_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/tsix404/rffmpeg/pkg/protocol"
	"github.com/tsix404/rffmpeg/pkg/server/db"
	"github.com/tsix404/rffmpeg/pkg/server/handlers"
	"github.com/tsix404/rffmpeg/pkg/server/storage"
	"github.com/tsix404/rffmpeg/pkg/server/workerhealth"
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

	// Create handler
	stateTable := workerhealth.NewWorkerStateTable(30 * time.Second)
	h := handlers.New(database, store, "test", stateTable)

	// Setup router
	r := chi.NewRouter()
	r.Post("/api/v1/upload", h.Upload)
	r.Post("/api/v1/jobs", h.SubmitJob)
	r.Get("/api/v1/jobs/{jobId}", h.GetJob)
	r.Delete("/api/v1/jobs/{jobId}", h.CancelJob)
	r.Patch("/api/v1/jobs/{jobId}", h.UpdateJob)
	r.Post("/api/v1/jobs/{jobId}/output", h.UploadJobOutput)
	r.Get("/api/v1/output/{fileId}", h.DownloadOutput)
	r.Get("/api/v1/health", h.Health)
	r.Post("/api/v1/workers/register", h.RegisterWorker)
	r.Post("/api/v1/workers/heartbeat", h.WorkerHeartbeat)
	r.Get("/api/v1/workers/{workerId}/jobs", h.PullWorkerJobs)
	r.Get("/api/v1/files/{fileId}", h.DownloadFile)

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
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("Failed to register worker: status %d, body: %s", w.Code, w.Body.String())
	}

	return "test-worker-1"
}

func TestHealthEndpoint(t *testing.T) {
	_, router, cleanup := setupTest(t)
	defer cleanup()

	req := httptest.NewRequest("GET", "/api/v1/health", nil)
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
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Update job failed with status %d", w.Code)
	}

	// Verify status
	req = httptest.NewRequest("GET", fmt.Sprintf("/api/v1/jobs/%s", jobResp.JobID), nil)
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
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)

	// Verify completed
	req = httptest.NewRequest("GET", fmt.Sprintf("/api/v1/jobs/%s", jobResp.JobID), nil)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)

	json.NewDecoder(w.Body).Decode(&statusResp)

	if statusResp.Job.Status != protocol.JobStatusCompleted {
		t.Errorf("Expected status 'completed', got '%s'", statusResp.Job.Status)
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
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)

	var jobResp protocol.JobSubmitResponse
	json.NewDecoder(w.Body).Decode(&jobResp)

	// Cancel job
	req = httptest.NewRequest("DELETE", fmt.Sprintf("/api/v1/jobs/%s", jobResp.JobID), nil)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Cancel job failed with status %d", w.Code)
	}

	// Verify cancelled
	req = httptest.NewRequest("GET", fmt.Sprintf("/api/v1/jobs/%s", jobResp.JobID), nil)
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
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Update job to running failed with status %d", w.Code)
	}

	// Cancel running job
	req = httptest.NewRequest("DELETE", fmt.Sprintf("/api/v1/jobs/%s", jobResp.JobID), nil)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Cancel running job failed with status %d: %s", w.Code, w.Body.String())
	}

	// Verify cancelled
	req = httptest.NewRequest("GET", fmt.Sprintf("/api/v1/jobs/%s", jobResp.JobID), nil)
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
	req.Header.Set("Content-Type", outputWriter.FormDataContentType())
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Upload output failed with status %d", w.Code)
	}

	// Get job to find output file ID
	req = httptest.NewRequest("GET", fmt.Sprintf("/api/v1/jobs/%s", jobResp.JobID), nil)
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
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)

	// Try to cancel completed job - should fail
	req = httptest.NewRequest("DELETE", fmt.Sprintf("/api/v1/jobs/%s", jobResp.JobID), nil)
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
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)

	var jobResp protocol.JobSubmitResponse
	json.NewDecoder(w.Body).Decode(&jobResp)

	// Cancel the job
	req = httptest.NewRequest("DELETE", fmt.Sprintf("/api/v1/jobs/%s", jobResp.JobID), nil)
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

// TestSubmitJobNoWorkerWithEncoder tests that job submission fails fast when no worker has the requested encoder (TSI-1428)
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

	// Message should mention the encoder
	if !bytes.Contains([]byte(errResp.Message), []byte("libx265")) {
		t.Errorf("Expected error message to mention 'libx265', got: %s", errResp.Message)
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
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)

	// Should succeed (200 or 201) because libx264 is a compatible encoder
	if w.Code != http.StatusOK && w.Code != http.StatusCreated {
		t.Errorf("Expected status 200 or 201, got %d. Body: %s", w.Code, w.Body.String())
	}
}

// TSI-1500: Test that handler rejects job when no compatible encoder is available
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
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)

	// Should fail (503) because no hevc encoder is available (different codec family)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("Expected status %d, got %d. Body: %s", http.StatusServiceUnavailable, w.Code, w.Body.String())
	}

	// Check error message mentions encoder
	if !bytes.Contains(w.Body.Bytes(), []byte("encoder")) {
		t.Errorf("Expected error message to mention encoder, got: %s", w.Body.String())
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
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)

	// Should succeed (200 or 201) with exact match
	if w.Code != http.StatusOK && w.Code != http.StatusCreated {
		t.Errorf("Expected status 200 or 201, got %d. Body: %s", w.Code, w.Body.String())
	}
}
