package handlers_test

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/tsix404/rffmpeg/pkg/server/db"
	"github.com/tsix404/rffmpeg/pkg/server/handlers"
	"github.com/tsix404/rffmpeg/pkg/server/storage"
)

// TestSubmitJob_RejectsUnauthenticated verifies handler-level auth enforcement
// for job submission: even when no middleware is applied, SubmitJob must reject
// requests without a valid token once an auth token is configured.
func TestSubmitJob_RejectsUnauthenticated(t *testing.T) {
	h, router, cleanup := setupTest(t)
	defer cleanup()
	h.SetAuthToken("test-token")

	body := []byte(`{"input_files":["test.mp4"],"args":["-c:v","libx264"],"output_filename":"out.mp4"}`)
	req := httptest.NewRequest("POST", "/api/v1/jobs", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("SubmitJob without auth: expected 401, got %d (body: %s)", w.Code, w.Body.String())
	}
}

// TestChunkUploadHandlers_RejectUnauthenticated verifies handler-level auth
// enforcement across every chunk upload endpoint. Without a token, each handler
// must reject the request with 401 before touching the database or storage.
func TestChunkUploadHandlers_RejectUnauthenticated(t *testing.T) {
	tmpDir := t.TempDir()
	database, err := db.New(filepath.Join(tmpDir, "test.db"))
	if err != nil {
		t.Fatalf("Failed to create database: %v", err)
	}
	defer database.Close()

	store, err := storage.New(tmpDir)
	if err != nil {
		t.Fatalf("Failed to create storage: %v", err)
	}

	ch := handlers.NewChunkUploadHandler(database, store, 0)
	defer ch.Shutdown()
	ch.SetAuthToken("test-token")

	r := chi.NewRouter()
	r.Post("/api/v1/upload/init", ch.InitChunkUpload)
	r.Post("/api/v1/upload/chunk/{uploadId}/{chunkIndex}", ch.UploadChunk)
	r.Get("/api/v1/upload/progress/{uploadId}", ch.GetUploadProgress)
	r.Post("/api/v1/upload/complete", ch.CompleteChunkUpload)
	r.Post("/api/v1/upload/cancel/{uploadId}", ch.CancelChunkUpload)
	r.Get("/api/v1/upload/resume/{uploadId}", ch.ResumeChunkUpload)

	tests := []struct {
		name   string
		method string
		path   string
	}{
		{"InitChunkUpload", "POST", "/api/v1/upload/init"},
		{"UploadChunk", "POST", "/api/v1/upload/chunk/session-1/0"},
		{"GetUploadProgress", "GET", "/api/v1/upload/progress/session-1"},
		{"CompleteChunkUpload", "POST", "/api/v1/upload/complete"},
		{"CancelChunkUpload", "POST", "/api/v1/upload/cancel/session-1"},
		{"ResumeChunkUpload", "GET", "/api/v1/upload/resume/session-1"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			if w.Code != http.StatusUnauthorized {
				t.Fatalf("%s without auth: expected 401, got %d (body: %s)", tc.name, w.Code, w.Body.String())
			}
		})
	}
}

// TestSubmitJob_RejectsWithoutConfiguredToken verifies that when NO auth token
// is configured, job submission is rejected (fail closed) rather than silently
// accepted — the TSI-2353 gap where an unset token bypassed authentication.
func TestSubmitJob_RejectsWithoutConfiguredToken(t *testing.T) {
	h, router, cleanup := setupTest(t)
	defer cleanup()
	h.SetAuthToken("") // explicitly no token configured

	body := []byte(`{"input_files":["test.mp4"],"args":["-c:v","libx264"],"output_filename":"out.mp4"}`)
	req := httptest.NewRequest("POST", "/api/v1/jobs", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("SubmitJob on tokenless server: expected 401, got %d (body: %s)", w.Code, w.Body.String())
	}
}

// TestChunkUploadHandlers_RejectWithoutConfiguredToken verifies chunk upload
// handlers also fail closed when no auth token is configured.
func TestChunkUploadHandlers_RejectWithoutConfiguredToken(t *testing.T) {
	tmpDir := t.TempDir()
	database, err := db.New(filepath.Join(tmpDir, "test.db"))
	if err != nil {
		t.Fatalf("Failed to create database: %v", err)
	}
	defer database.Close()

	store, err := storage.New(tmpDir)
	if err != nil {
		t.Fatalf("Failed to create storage: %v", err)
	}

	ch := handlers.NewChunkUploadHandler(database, store, 0)
	defer ch.Shutdown()
	ch.SetAuthToken("") // explicitly no token configured

	r := chi.NewRouter()
	r.Post("/api/v1/upload/init", ch.InitChunkUpload)

	req := httptest.NewRequest("POST", "/api/v1/upload/init", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("InitChunkUpload on tokenless server: expected 401, got %d (body: %s)", w.Code, w.Body.String())
	}
}
