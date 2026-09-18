package handlers

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/tsic404/rffmpeg/pkg/protocol"
	"github.com/tsic404/rffmpeg/pkg/server/db"
	"github.com/tsic404/rffmpeg/pkg/server/storage"
)

func TestChunkSaveErrorStatus(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   protocol.ErrorCode
		wantMsg    string
	}{
		{
			name:       "disk full",
			err:        fmt.Errorf("failed to write chunk: %w", syscall.ENOSPC),
			wantStatus: http.StatusInsufficientStorage,
			wantCode:   protocol.ErrCodeInsufficientStorage,
			wantMsg:    "Insufficient storage: no space left on device",
		},
		{
			name:       "deeply wrapped disk full",
			err:        fmt.Errorf("save: %w", fmt.Errorf("write: %w", syscall.ENOSPC)),
			wantStatus: http.StatusInsufficientStorage,
			wantCode:   protocol.ErrCodeInsufficientStorage,
			wantMsg:    "Insufficient storage: no space left on device",
		},
		{
			name:       "generic write error",
			err:        errors.New("read/write on closed pipe"),
			wantStatus: http.StatusInternalServerError,
			wantCode:   protocol.ErrCodeUploadFailed,
			wantMsg:    "Failed to save chunk",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, code, msg := chunkSaveErrorStatus(tt.err)
			if status != tt.wantStatus {
				t.Errorf("status = %d, want %d", status, tt.wantStatus)
			}
			if code != tt.wantCode {
				t.Errorf("code = %q, want %q", code, tt.wantCode)
			}
			if msg != tt.wantMsg {
				t.Errorf("message = %q, want %q", msg, tt.wantMsg)
			}
		})
	}
}

// failingSaveStorage delegates every method to a real *storage.Storage except
// SaveChunk, which returns a close-time ENOSPC error — the failure mode a real
// filesystem cannot reproduce portably (delalloc/NFS flush).
type failingSaveStorage struct {
	*storage.Storage
	err error
}

func (f *failingSaveStorage) SaveChunk(uploadID string, chunkIndex int, reader io.Reader) (string, int64, string, error) {
	return "", 0, "", f.err
}

func TestUploadChunk_DiskFullReturns507AndLogs(t *testing.T) {
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

	// Inject a storage whose SaveChunk reports close-time ENOSPC.
	h.storage = &failingSaveStorage{
		Storage: store,
		err:     fmt.Errorf("failed to close chunk file: %w", syscall.ENOSPC),
	}

	// Capture server logs so the failure-path log line can be asserted.
	var logBuf bytes.Buffer
	oldOutput := log.Writer()
	oldFlags := log.Flags()
	log.SetOutput(&logBuf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(oldOutput)
		log.SetFlags(oldFlags)
	})

	session, err := database.CreateUploadSession("video.mp4", 1024, 1024, 1, nil)
	if err != nil {
		t.Fatalf("CreateUploadSession: %v", err)
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("chunk", "chunk")
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	if _, err := part.Write([]byte("data")); err != nil {
		t.Fatalf("write part: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}

	req := httptest.NewRequest("POST", "/api/v1/upload/chunk/"+session.ID+"/0", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Authorization", "Bearer test-token")

	// Route through chi so URLParam("uploadId"/"chunkIndex") is populated;
	// invoking the handler directly leaves those params empty.
	r := chi.NewRouter()
	r.Post("/api/v1/upload/chunk/{uploadId}/{chunkIndex}", h.UploadChunk)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusInsufficientStorage {
		t.Fatalf("expected 507, got %d (body: %s)", rec.Code, rec.Body.String())
	}

	var resp protocol.ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.Code != protocol.ErrCodeInsufficientStorage {
		t.Errorf("expected code %q, got %q", protocol.ErrCodeInsufficientStorage, resp.Code)
	}
	if resp.Message != "Insufficient storage: no space left on device" {
		t.Errorf("unexpected message: %q", resp.Message)
	}

	if !strings.Contains(logBuf.String(), "Failed to save chunk") {
		t.Errorf("expected save-chunk failure in log, got: %q", logBuf.String())
	}
	if !strings.Contains(logBuf.String(), "no space left on device") {
		t.Errorf("expected ENOSPC cause in log, got: %q", logBuf.String())
	}
}
