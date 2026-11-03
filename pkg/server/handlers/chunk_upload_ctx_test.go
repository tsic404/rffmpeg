package handlers

import (
	"bytes"
	"context"
	"mime/multipart"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/tsic404/rffmpeg/pkg/server/db"
	"github.com/tsic404/rffmpeg/pkg/server/storage"
)

// Four slow chunk clients occupy every upload slot. A queued request whose
// client has already disconnected must not keep waiting for one of them: the
// wait has to observe the request context.
func TestUploadChunkCanceledWhileQueuedLeavesWithoutSlot(t *testing.T) {
	database, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("Failed to create database: %v", err)
	}
	t.Cleanup(func() { database.Close() })

	store, err := storage.New(t.TempDir())
	if err != nil {
		t.Fatalf("Failed to create storage: %v", err)
	}

	h := NewChunkUploadHandler(database, store, DefaultChunkSize)
	h.SetAuthToken("test-token")
	t.Cleanup(h.Shutdown)

	session, err := database.CreateUploadSession("video.mp4", 1024, 1024, 1, nil)
	if err != nil {
		t.Fatalf("CreateUploadSession: %v", err)
	}

	// Every slot taken by in-flight transfers.
	for range MaxConcurrentChunkUploads {
		h.semaphore <- struct{}{}
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("chunk", "chunk")
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	part.Write([]byte("data"))
	writer.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	req := httptest.NewRequest("POST", "/api/v1/upload/chunk/"+session.ID+"/0", &body).WithContext(ctx)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Authorization", "Bearer test-token")

	r := chi.NewRouter()
	r.Post("/api/v1/upload/chunk/{uploadId}/{chunkIndex}", h.UploadChunk)

	done := make(chan struct{})
	go func() {
		defer close(done)
		r.ServeHTTP(httptest.NewRecorder(), req)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("canceled request kept waiting for an upload slot")
	}

	if got := len(h.semaphore); got != MaxConcurrentChunkUploads {
		t.Errorf("slots in use = %d, want %d (canceled request must not take one)", got, MaxConcurrentChunkUploads)
	}
}
