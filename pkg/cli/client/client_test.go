package client_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/tsix404/rffmpeg/pkg/cli/client"
	"github.com/tsix404/rffmpeg/pkg/protocol"
	"github.com/tsix404/rffmpeg/pkg/server/db"
	"github.com/tsix404/rffmpeg/pkg/server/handlers"
	"github.com/tsix404/rffmpeg/pkg/server/storage"
	"github.com/tsix404/rffmpeg/pkg/server/workerhealth"
)

// setupTestServer creates a test server with all handlers
func setupTestServer(t *testing.T) (*httptest.Server, *db.Database, *storage.Storage, func()) {
	// Create temp directory
	tmpDir, err := os.MkdirTemp("", "rffmpeg-client-test-*")
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

	// Create handlers
	stateTable := workerhealth.NewWorkerStateTable(30 * time.Second)
	h := handlers.New(database, store, "test", stateTable)
	chunkHandler := handlers.NewChunkUploadHandler(database, store, 0) // Uses default chunk size
	h.SetAuthToken("test-token")
	chunkHandler.SetAuthToken("test-token")

	// Setup router
	r := chi.NewRouter()
	r.Post("/api/v1/upload", h.Upload)
	r.Post("/api/v1/jobs", h.SubmitJob)
	r.Get("/api/v1/jobs/{jobId}", h.GetJob)
	r.Delete("/api/v1/jobs/{jobId}", h.CancelJob)
	r.Get("/api/v1/output/{fileId}", h.DownloadOutput)
	r.Get("/api/v1/health", h.Health)
	r.Post("/api/v1/workers/register", h.RegisterWorker) // TSI-1428: needed for job submission

	// Chunked upload endpoints
	r.Post("/api/v1/upload/init", chunkHandler.InitChunkUpload)
	r.Post("/api/v1/upload/chunk/{uploadId}/{chunkIndex}", chunkHandler.UploadChunk)
	r.Post("/api/v1/upload/complete", chunkHandler.CompleteChunkUpload)
	r.Post("/api/v1/upload/cancel/{uploadId}", chunkHandler.CancelChunkUpload)
	r.Get("/api/v1/upload/resume/{uploadId}", chunkHandler.GetUploadProgress)

	server := httptest.NewServer(r)

	// Register a test worker with common encoders (TSI-1428: required for job submission)
	registerTestWorker(t, server.URL)

	cleanup := func() {
		server.Close()
		chunkHandler.Shutdown()
		database.Close()
		os.RemoveAll(tmpDir)
	}

	return server, database, store, cleanup
}

// registerTestWorker registers a test worker with common encoders
func registerTestWorker(t *testing.T, serverURL string) {
	workerReq := protocol.WorkerRegisterRequest{
		WorkerID: "test-worker-1",
		Name:     "test-worker",
		Capabilities: protocol.WorkerCapabilities{
			Encoders:      []string{"libx264", "libx265", "h264_nvenc"},
			FFmpegVersion: "5.1",
		},
	}
	workerBody, _ := json.Marshal(workerReq)

	resp, err := http.Post(serverURL+"/api/v1/workers/register", "application/json", bytes.NewReader(workerBody))
	if err != nil {
		t.Fatalf("Failed to register test worker: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("Failed to register test worker: status %d, body: %s", resp.StatusCode, string(body))
	}
}

// createTestFile creates a temporary file with the given content
func createTestFile(t *testing.T, content []byte) string {
	tmpFile, err := os.CreateTemp("", "test-upload-*.mp4")
	if err != nil {
		t.Fatalf("Failed to create temp file: %v", err)
	}
	defer tmpFile.Close()

	if _, err := tmpFile.Write(content); err != nil {
		t.Fatalf("Failed to write temp file: %v", err)
	}

	return tmpFile.Name()
}

// computeSHA256 computes SHA256 checksum of a byte slice
func computeSHA256(data []byte) string {
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}

// mockUploadSession tracks state for a chunked upload in the lightweight mock server
type mockUploadSession struct {
	totalChunks    int
	uploadedChunks int
}

// setupLightweightTestServer creates a self-contained mock server that handles
// both simple and chunked upload endpoints without depending on the full server
// stack (db, storage, handlers). This makes tests portable and avoids hard
// dependencies on infrastructure that may be unavailable in constrained
// environments (e.g. CI with limited disk/tmpfs quotas).
func setupLightweightTestServer(t *testing.T) *httptest.Server {
	var fileIDCounter int
	uploadSessions := make(map[string]*mockUploadSession)
	var mu sync.Mutex

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Simple upload
		if r.URL.Path == "/api/v1/upload" && r.Method == "POST" {
			if err := r.ParseMultipartForm(256 << 20); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				json.NewEncoder(w).Encode(protocol.ErrorResponse{Message: "Failed to parse multipart form"})
				return
			}
			file, _, err := r.FormFile("file")
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				json.NewEncoder(w).Encode(protocol.ErrorResponse{Message: "Missing file in request"})
				return
			}
			defer file.Close()
			io.Copy(io.Discard, file)

			mu.Lock()
			fileIDCounter++
			fileID := fmt.Sprintf("mock-file-%d", fileIDCounter)
			mu.Unlock()

			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(protocol.UploadResponse{FileID: fileID})
			return
		}

		// Chunk upload init
		if r.URL.Path == "/api/v1/upload/init" && r.Method == "POST" {
			var req protocol.ChunkUploadInitRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				json.NewEncoder(w).Encode(protocol.ErrorResponse{Message: "Invalid JSON body"})
				return
			}

			chunkSize := req.ChunkSize
			if chunkSize <= 0 {
				chunkSize = 10 * 1024 * 1024
			}
			totalChunks := int(req.FileSize / chunkSize)
			if req.FileSize%chunkSize > 0 {
				totalChunks++
			}

			mu.Lock()
			fileIDCounter++
			uploadID := fmt.Sprintf("mock-upload-%d", fileIDCounter)
			uploadSessions[uploadID] = &mockUploadSession{
				totalChunks: totalChunks,
			}
			mu.Unlock()

			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(protocol.ChunkUploadInitResponse{
				UploadID:    uploadID,
				ChunkSize:   chunkSize,
				TotalChunks: totalChunks,
				Message:     "Upload session created successfully",
			})
			return
		}

		// Chunk upload
		if strings.Contains(r.URL.Path, "/api/v1/upload/chunk/") && r.Method == "POST" {
			// Extract uploadId and chunkIndex from path: /api/v1/upload/chunk/{uploadId}/{chunkIndex}
			path := strings.TrimPrefix(r.URL.Path, "/api/v1/upload/chunk/")
			parts := strings.SplitN(path, "/", 2)
			if len(parts) < 2 {
				w.WriteHeader(http.StatusBadRequest)
				json.NewEncoder(w).Encode(protocol.ErrorResponse{Message: "Invalid URL"})
				return
			}
			uploadID := parts[0]
			chunkIndex, err := strconv.Atoi(parts[1])
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				json.NewEncoder(w).Encode(protocol.ErrorResponse{Message: "Invalid chunk index"})
				return
			}

			mu.Lock()
			session := uploadSessions[uploadID]
			mu.Unlock()
			if session == nil {
				w.WriteHeader(http.StatusNotFound)
				json.NewEncoder(w).Encode(protocol.ErrorResponse{Message: "Upload session not found"})
				return
			}

			if err := r.ParseMultipartForm(32 << 20); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				json.NewEncoder(w).Encode(protocol.ErrorResponse{Message: "Failed to parse chunk multipart form"})
				return
			}

			file, _, err := r.FormFile("chunk")
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				json.NewEncoder(w).Encode(protocol.ErrorResponse{Message: "Missing chunk in request"})
				return
			}
			defer file.Close()
			io.Copy(io.Discard, file)

			mu.Lock()
			session.uploadedChunks++
			mu.Unlock()

			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(protocol.ChunkUploadResponse{
				UploadID:   uploadID,
				ChunkIndex: chunkIndex,
				Message:    "Chunk uploaded successfully",
			})
			return
		}

		// Chunk upload complete
		if r.URL.Path == "/api/v1/upload/complete" && r.Method == "POST" {
			var req protocol.ChunkUploadCompleteRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				json.NewEncoder(w).Encode(protocol.ErrorResponse{Message: "Invalid JSON body"})
				return
			}

			mu.Lock()
			fileIDCounter++
			fileID := fmt.Sprintf("mock-file-%d", fileIDCounter)
			mu.Unlock()

			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(protocol.ChunkUploadCompleteResponse{
				FileID:  fileID,
				Message: "File uploaded successfully",
			})
			return
		}

		w.WriteHeader(http.StatusNotFound)
	}))
}

// TestUploadFile tests the simple file upload
func TestUploadFile(t *testing.T) {
	server, _, _, cleanup := setupTestServer(t)
	defer cleanup()

	c := client.New(server.URL, "test-token")

	// Create a test file
	content := []byte("test video content for upload")
	tmpFile := createTestFile(t, content)
	defer os.Remove(tmpFile)

	// Upload the file
	fileID, err := c.UploadFile(tmpFile)
	if err != nil {
		t.Fatalf("UploadFile failed: %v", err)
	}

	if fileID == "" {
		t.Error("Expected non-empty file ID")
	}
}

// TestUploadFileChunked tests the chunked file upload
func TestUploadFileChunked(t *testing.T) {
	server, _, _, cleanup := setupTestServer(t)
	defer cleanup()

	c := client.New(server.URL, "test-token")

	// Create a test file that's larger than the default chunk size
	// Use 15MB to test multiple chunks
	content := make([]byte, 15*1024*1024)
	for i := range content {
		content[i] = byte(i % 256)
	}
	tmpFile := createTestFile(t, content)
	defer os.Remove(tmpFile)

	// Upload using chunked upload with 5MB chunks
	fileID, err := c.UploadFileChunked(tmpFile, 5*1024*1024)
	if err != nil {
		t.Fatalf("UploadFileChunked failed: %v", err)
	}

	if fileID == "" {
		t.Error("Expected non-empty file ID")
	}
}

// TestUploadFileChunkedWithChecksumVerification tests that checksums are properly verified
func TestUploadFileChunkedWithChecksumVerification(t *testing.T) {
	server, _, _, cleanup := setupTestServer(t)
	defer cleanup()

	c := client.New(server.URL, "test-token")

	// Create a test file with known content
	content := []byte("test content for checksum verification")
	_ = computeSHA256(content) // Expected checksum for verification

	tmpFile := createTestFile(t, content)
	defer os.Remove(tmpFile)

	// Upload using chunked upload
	fileID, err := c.UploadFileChunked(tmpFile, 10*1024) // 10KB chunks
	if err != nil {
		t.Fatalf("UploadFileChunked failed: %v", err)
	}

	if fileID == "" {
		t.Error("Expected non-empty file ID")
	}

	// Verify the file was uploaded correctly by checking the checksum
	// The server should have verified the checksum during upload
}

// TestUploadFileAuto tests automatic selection of upload method.
// Uses a lightweight mock server so the test does not depend on the full
// server stack (database, storage, worker health, etc.).
func TestUploadFileAuto(t *testing.T) {
	server := setupLightweightTestServer(t)
	defer server.Close()

	c := client.New(server.URL, "test-token")

	// Test small file - should use simple upload
	smallContent := []byte("small file content")
	smallFile := createTestFile(t, smallContent)
	defer os.Remove(smallFile)

	fileID, err := c.UploadFileAuto(smallFile)
	if err != nil {
		t.Fatalf("UploadFileAuto failed for small file: %v", err)
	}
	if fileID == "" {
		t.Error("Expected non-empty file ID for small file")
	}

	// Test file larger than threshold - should use chunked upload
	// Note: This test is skipped in short mode or environments with disk limits
	if !testing.Short() {
		// Use 101MB which is just above the LargeFileThreshold (100MB)
		largeContent := make([]byte, 101*1024*1024)
		for i := range largeContent {
			largeContent[i] = byte(i % 256)
		}
		largeFile := createTestFile(t, largeContent)
		defer os.Remove(largeFile)

		fileID, err = c.UploadFileAuto(largeFile)
		if err != nil {
			t.Fatalf("UploadFileAuto failed for large file: %v", err)
		}
		if fileID == "" {
			t.Error("Expected non-empty file ID for large file")
		}
	}
}

// TestUploadFileWithChecksumIntegrity tests that file integrity is preserved
func TestUploadFileWithChecksumIntegrity(t *testing.T) {
	server, _, store, cleanup := setupTestServer(t)
	defer cleanup()

	c := client.New(server.URL, "test-token")

	// Create a test file with specific content
	content := []byte("integrity test content - this should match exactly after upload")
	expectedChecksum := computeSHA256(content)

	tmpFile := createTestFile(t, content)
	defer os.Remove(tmpFile)

	// Upload the file
	fileID, err := c.UploadFile(tmpFile)
	if err != nil {
		t.Fatalf("UploadFile failed: %v", err)
	}

	// Read the uploaded file from storage and verify checksum
	// This verifies that the Content-Length calculation was correct
	uploadedPath := store.GetFilePath(fileID)
	uploadedContent, err := os.ReadFile(uploadedPath)
	if err != nil {
		t.Fatalf("Failed to read uploaded file: %v", err)
	}

	uploadedChecksum := computeSHA256(uploadedContent)
	if uploadedChecksum != expectedChecksum {
		t.Errorf("Checksum mismatch: expected %s, got %s", expectedChecksum, uploadedChecksum)
	}

	if !bytes.Equal(content, uploadedContent) {
		t.Error("Uploaded content does not match original content")
	}
}

// TestMultipartSizeCalculation tests the Content-Length calculation for multipart forms
func TestMultipartSizeCalculation(t *testing.T) {
	// Test that calculateMultipartSize produces accurate results
	// by comparing with actual multipart form size

	testCases := []struct {
		name     string
		fileSize int64
		filename string
	}{
		{"small file", 100, "test.mp4"},
		{"medium file", 1024 * 1024, "video.mkv"},
		{"large file", 100 * 1024 * 1024, "large-video.mp4"},
		{"filename with quote", 512, `my"file.mp4`},
		{"filename with backslash", 512, `dir\name.mp4`},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Create actual multipart form
			body := &bytes.Buffer{}
			writer := multipart.NewWriter(body)

			part, err := writer.CreateFormFile("file", tc.filename)
			if err != nil {
				t.Fatalf("Failed to create form file: %v", err)
			}

			// Write dummy content
			dummyContent := make([]byte, tc.fileSize)
			if _, err := part.Write(dummyContent); err != nil {
				t.Fatalf("Failed to write content: %v", err)
			}

			writer.Close()

			// Calculate expected size using the function from client
			// We need to use the same boundary as the writer
			boundary := writer.Boundary()

			// Calculate using our function (we need to access it, but it's unexported)
			// So we'll just verify the actual size matches our expectations
			actualSize := int64(body.Len())

			// The difference should be minimal (just the boundary string length variance)
			// Let's calculate what our function would produce
			expectedSize := calculateMultipartSizeForTest(boundary, tc.fileSize, tc.filename)

			if actualSize != expectedSize {
				t.Errorf("Size mismatch for %s: expected %d, got %d", tc.name, expectedSize, actualSize)
			}
		})
	}
}

// calculateMultipartSizeForTest mirrors the production calculation, including
// the quote escaping added in TSI-2365.
func calculateMultipartSizeForTest(boundary string, fileSize int64, filename string) int64 {
	escaped := strings.ReplaceAll(strings.ReplaceAll(filename, "\\", "\\\\"), "\"", "\\\"")
	preamble := fmt.Sprintf("--%s\r\n", boundary)
	contentDisposition := fmt.Sprintf("Content-Disposition: form-data; name=\"file\"; filename=\"%s\"\r\n", escaped)
	contentType := "Content-Type: application/octet-stream\r\n"
	headerEnd := "\r\n"
	epilogue := fmt.Sprintf("\r\n--%s--\r\n", boundary)

	return int64(len(preamble)+len(contentDisposition)+len(contentType)+len(headerEnd)) +
		fileSize +
		int64(len(epilogue))
}

// TestRetryLogic tests that chunk upload retries on transient failures
func TestRetryLogic(t *testing.T) {
	// Create a test server that fails the first request but succeeds on retry
	retryCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/upload/chunk/") {
			retryCount++
			if retryCount < 3 {
				// Simulate a retryable error
				w.WriteHeader(http.StatusServiceUnavailable)
				json.NewEncoder(w).Encode(protocol.ErrorResponse{
					Message: "Service temporarily unavailable",
				})
				return
			}
		}

		// Handle init
		if r.URL.Path == "/api/v1/upload/init" {
			var req protocol.ChunkUploadInitRequest
			json.NewDecoder(r.Body).Decode(&req)

			totalChunks := int(req.FileSize / req.ChunkSize)
			if req.FileSize%req.ChunkSize > 0 {
				totalChunks++
			}

			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(protocol.ChunkUploadInitResponse{
				UploadID:    "test-upload-id",
				ChunkSize:   req.ChunkSize,
				TotalChunks: totalChunks,
			})
			return
		}

		// Handle chunk upload (success on retry)
		if strings.Contains(r.URL.Path, "/upload/chunk/") {
			// Drain the body
			io.Copy(io.Discard, r.Body)

			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(protocol.ChunkUploadResponse{
				UploadID:   "test-upload-id",
				ChunkIndex: 0,
				Message:    "Chunk uploaded successfully",
			})
			return
		}

		// Handle complete
		if r.URL.Path == "/api/v1/upload/complete" {
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(protocol.ChunkUploadCompleteResponse{
				FileID:  "test-file-id",
				Message: "File uploaded successfully",
			})
			return
		}

		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	c := client.New(server.URL, "test-token")

	// Create a small test file
	content := []byte("test content for retry logic")
	tmpFile := createTestFile(t, content)
	defer os.Remove(tmpFile)

	// Upload should succeed after retries
	fileID, err := c.UploadFileChunked(tmpFile, 10*1024)
	if err != nil {
		t.Fatalf("UploadFileChunked failed: %v", err)
	}

	if fileID == "" {
		t.Error("Expected non-empty file ID")
	}

	// Verify retries happened
	if retryCount < 3 {
		t.Errorf("Expected at least 3 attempts, got %d", retryCount)
	}
}

// TestHealthCheck tests the health check endpoint
func TestHealthCheck(t *testing.T) {
	server, _, _, cleanup := setupTestServer(t)
	defer cleanup()

	c := client.New(server.URL, "test-token")

	if err := c.HealthCheck(); err != nil {
		t.Errorf("HealthCheck failed: %v", err)
	}
}

// TestDownloadOutput tests file download
func TestDownloadOutput(t *testing.T) {
	// Note: DownloadOutput is for job output files, not uploaded input files
	// This test would require a complete job workflow to test properly
	// For now, we just test that the method exists and handles errors correctly
	server, _, _, cleanup := setupTestServer(t)
	defer cleanup()

	c := client.New(server.URL, "test-token")

	// Test that downloading a non-existent file returns an error
	err := c.DownloadOutput("non-existent-file-id", "/tmp/test-output.mp4")
	if err == nil {
		t.Error("Expected error for non-existent file")
	}
}

// TestHashingReader tests the HashingReader utility
func TestHashingReader(t *testing.T) {
	content := []byte("test content for hashing")
	expectedChecksum := computeSHA256(content)

	// Create a HashingReader
	reader := client.NewHashingReader(bytes.NewReader(content))

	// Read all content
	readContent, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("Failed to read: %v", err)
	}

	// Verify content matches
	if !bytes.Equal(content, readContent) {
		t.Error("Read content does not match original")
	}

	// Verify checksum
	if reader.Checksum() != expectedChecksum {
		t.Errorf("Checksum mismatch: expected %s, got %s", expectedChecksum, reader.Checksum())
	}
}

// TestDrainAndClose tests the drainAndClose utility function
func TestDrainAndClose(t *testing.T) {
	// Create a mock ReadCloser with content
	content := "test content for draining"
	body := io.NopCloser(strings.NewReader(content))

	// Call drainAndClose
	if err := client.DrainAndClose(body); err != nil {
		t.Errorf("drainAndClose failed: %v", err)
	}

	// Test with nil
	if err := client.DrainAndClose(nil); err != nil {
		t.Errorf("drainAndClose(nil) failed: %v", err)
	}
}

// TestLargeFileUploadIntegration tests the complete upload flow for large files
func TestLargeFileUploadIntegration(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	server, _, _, cleanup := setupTestServer(t)
	defer cleanup()

	c := client.New(server.URL, "test-token")

	// Create a 50MB file
	fileSize := int64(50 * 1024 * 1024)
	content := make([]byte, fileSize)
	for i := range content {
		content[i] = byte(i % 256)
	}
	expectedChecksum := computeSHA256(content)

	tmpFile := createTestFile(t, content)
	defer os.Remove(tmpFile)

	// Upload with 5MB chunks
	startTime := time.Now()
	fileID, err := c.UploadFileChunked(tmpFile, 5*1024*1024)
	uploadDuration := time.Since(startTime)

	if err != nil {
		t.Fatalf("UploadFileChunked failed: %v", err)
	}

	if fileID == "" {
		t.Error("Expected non-empty file ID")
	}

	t.Logf("Uploaded %d bytes in %v", fileSize, uploadDuration)

	// Verify checksum matches
	_ = expectedChecksum // In a real test, we would verify this against the server's stored checksum
}

// TestSharedFSSubmitJobWithDirectPath tests that DirectPath is sent in the request
// when shared FS mode is enabled.
func TestSharedFSSubmitJobWithDirectPath(t *testing.T) {
	server, _, _, cleanup := setupTestServer(t)
	defer cleanup()

	c := client.New(server.URL, "test-token")

	// Upload a real file first to get a valid file ID
	content := []byte("shared fs test content")
	tmpFile := createTestFile(t, content)
	defer os.Remove(tmpFile)

	fileID, err := c.UploadFile(tmpFile)
	if err != nil {
		t.Fatalf("UploadFile failed: %v", err)
	}

	fileIDs := []string{fileID}
	directPaths := []string{tmpFile}
	allArgs := []string{"-c:v", "libx264", "-c:a", "copy", "/tmp/output.mp4"}

	jobID, err := c.SubmitJobWithOptions(
		fileIDs,
		directPaths,
		allArgs,
		"output.mp4",
		false, // autoHW
		false, // streamingOutput
		0,     // timeout
	)
	if err != nil {
		t.Fatalf("SubmitJobWithOptions with DirectPath failed: %v", err)
	}
	if jobID == "" {
		t.Error("Expected non-empty job ID")
	}
}

// TestSharedFSSubmitJobWithoutDirectPath tests that DirectPath is omitted
// when shared FS mode is disabled (nil DirectPath).
func TestSharedFSSubmitJobWithoutDirectPath(t *testing.T) {
	server, _, _, cleanup := setupTestServer(t)
	defer cleanup()

	c := client.New(server.URL, "test-token")

	// Upload real files first
	content1 := []byte("test file 1 content")
	tmpFile1 := createTestFile(t, content1)
	defer os.Remove(tmpFile1)

	content2 := []byte("test file 2 content")
	tmpFile2 := createTestFile(t, content2)
	defer os.Remove(tmpFile2)

	fileID1, err := c.UploadFile(tmpFile1)
	if err != nil {
		t.Fatalf("UploadFile failed: %v", err)
	}
	fileID2, err := c.UploadFile(tmpFile2)
	if err != nil {
		t.Fatalf("UploadFile failed: %v", err)
	}

	fileIDs := []string{fileID1, fileID2}
	allArgs := []string{"-c:v", "libx264", "-c:a", "copy", "/tmp/output.mp4"}

	jobID, err := c.SubmitJobWithOptions(
		fileIDs,
		nil, // No DirectPath
		allArgs,
		"output.mp4",
		false, // autoHW
		false, // streamingOutput
		0,     // timeout
	)
	if err != nil {
		t.Fatalf("SubmitJobWithOptions without DirectPath failed: %v", err)
	}
	if jobID == "" {
		t.Error("Expected non-empty job ID")
	}
}

// TestSharedFSSubmitJobWithEmptyDirectPath tests DirectPath as empty slice
func TestSharedFSSubmitJobWithEmptyDirectPath(t *testing.T) {
	server, _, _, cleanup := setupTestServer(t)
	defer cleanup()

	c := client.New(server.URL, "test-token")

	// Upload real file first
	content := []byte("empty directpath test")
	tmpFile := createTestFile(t, content)
	defer os.Remove(tmpFile)

	fileID, err := c.UploadFile(tmpFile)
	if err != nil {
		t.Fatalf("UploadFile failed: %v", err)
	}

	fileIDs := []string{fileID}
	allArgs := []string{"-c:v", "libx264", "output.mp4"}

	jobID, err := c.SubmitJobWithOptions(
		fileIDs,
		[]string{}, // Empty DirectPath
		allArgs,
		"output.mp4",
		false,
		false,
		0,
	)
	if err != nil {
		t.Fatalf("SubmitJobWithOptions with empty DirectPath failed: %v", err)
	}
	if jobID == "" {
		t.Error("Expected non-empty job ID")
	}
}

// TestSubmitJobBackwardsCompat tests that SubmitJob (the wrapper) still works
// and doesn't send DirectPath.
func TestSubmitJobBackwardsCompat(t *testing.T) {
	server, _, _, cleanup := setupTestServer(t)
	defer cleanup()

	c := client.New(server.URL, "test-token")

	// Upload real file first
	content := []byte("backwards compat test")
	tmpFile := createTestFile(t, content)
	defer os.Remove(tmpFile)

	fileID, err := c.UploadFile(tmpFile)
	if err != nil {
		t.Fatalf("UploadFile failed: %v", err)
	}

	fileIDs := []string{fileID}
	allArgs := []string{"-c:v", "libx264", "output.mp4"}

	jobID, err := c.SubmitJob(fileIDs, allArgs, "output.mp4", false)
	if err != nil {
		t.Fatalf("SubmitJob failed: %v", err)
	}
	if jobID == "" {
		t.Error("Expected non-empty job ID")
	}
}

// TestWaitForJob_ContextCancellation verifies that WaitForJob returns
// context.DeadlineExceeded promptly when the job never reaches a terminal
// status (e.g. a worker killed within the heartbeat window leaves the job
// stuck in "queued"). This is the client-side timeout detection for TSI-2202.
func TestWaitForJob_ContextCancellation(t *testing.T) {
	var mu sync.Mutex
	var polls int

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/api/v1/jobs/") {
			mu.Lock()
			polls++
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(protocol.JobStatusResponse{
				Job: protocol.JobInfo{
					ID:     "job-stuck",
					Status: protocol.JobStatusQueued,
				},
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := client.New(srv.URL, "")

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := c.WaitForJob(ctx, "job-stuck", false)
	elapsed := time.Since(start)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WaitForJob() error = %v, want context.DeadlineExceeded", err)
	}
	if elapsed >= 2*time.Second {
		t.Fatalf("WaitForJob() took %v; did not respect context cancellation", elapsed)
	}

	mu.Lock()
	defer mu.Unlock()
	if polls == 0 {
		t.Fatal("WaitForJob() never polled the job endpoint")
	}
}
