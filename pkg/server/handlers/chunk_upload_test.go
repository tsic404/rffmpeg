package handlers

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	_ "github.com/mattn/go-sqlite3"
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

func TestAssembleErrorStatus(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   protocol.ErrorCode
		wantMsg    string
	}{
		{
			name:       "disk full",
			err:        fmt.Errorf("failed to copy chunk: %w", syscall.ENOSPC),
			wantStatus: http.StatusInsufficientStorage,
			wantCode:   protocol.ErrCodeInsufficientStorage,
			wantMsg:    "Insufficient storage: no space left on device (data-dir)",
		},
		{
			name:       "deeply wrapped disk full",
			err:        fmt.Errorf("assemble: %w", fmt.Errorf("copy: %w", syscall.ENOSPC)),
			wantStatus: http.StatusInsufficientStorage,
			wantCode:   protocol.ErrCodeInsufficientStorage,
			wantMsg:    "Insufficient storage: no space left on device (data-dir)",
		},
		{
			name:       "generic error preserves cause",
			err:        errors.New("failed to open chunk"),
			wantStatus: http.StatusInternalServerError,
			wantCode:   protocol.ErrCodeUploadFailed,
			wantMsg:    "Failed to assemble file: failed to open chunk",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, code, msg := assembleErrorStatus(tt.err)
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

// failingAssembleStorage delegates every method to a real *storage.Storage
// except AssembleChunks (which returns a close-time ENOSPC error) and
// GetChunkPaths (which reports an empty chunk set so the handler reaches the
// assemble step without materializing chunk files on disk).
type failingAssembleStorage struct {
	*storage.Storage
	err error
}

func (f *failingAssembleStorage) GetChunkPaths(uploadID string) ([]string, error) {
	return nil, nil
}

func (f *failingAssembleStorage) AssembleChunks(uploadID string, fileID string, chunkPaths []string) (string, int64, string, error) {
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

func TestCompleteChunkUpload_DiskFullReturns507AndLogs(t *testing.T) {
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

	// Inject a storage whose AssembleChunks reports close-time ENOSPC.
	h.storage = &failingAssembleStorage{
		Storage: store,
		err:     fmt.Errorf("failed to copy chunk: %w", syscall.ENOSPC),
	}

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
	if err := database.UpdateUploadSessionChunks(session.ID, 0); err != nil {
		t.Fatalf("UpdateUploadSessionChunks: %v", err)
	}

	body := []byte(fmt.Sprintf(`{"upload_id": %q}`, session.ID))
	req := httptest.NewRequest("POST", "/api/v1/upload/complete", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-token")

	rec := httptest.NewRecorder()
	h.CompleteChunkUpload(rec, req)

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
	if resp.Message != "Insufficient storage: no space left on device (data-dir)" {
		t.Errorf("unexpected message: %q", resp.Message)
	}

	if !strings.Contains(logBuf.String(), "Failed to assemble chunks") {
		t.Errorf("expected assemble failure in log, got: %q", logBuf.String())
	}
	if !strings.Contains(logBuf.String(), "no space left on device") {
		t.Errorf("expected ENOSPC cause in log, got: %q", logBuf.String())
	}
}

// TestCleanupExpiredSessionsRemovesChunkDirectory is the regression test for
// the residue bug: an abandoned/failed upload leaves chunks/<uuid>/ on disk
// because expiring a session only flips its status. The sweep must also delete
// the chunk directory and the session row.
func TestCleanupExpiredSessionsRemovesChunkDirectory(t *testing.T) {
	dataDir := t.TempDir()
	dbPath := filepath.Join(t.TempDir(), "rffmpeg.db")

	database, err := db.New(dbPath)
	if err != nil {
		t.Fatalf("Failed to create database: %v", err)
	}
	t.Cleanup(func() { database.Close() })

	store, err := storage.New(dataDir)
	if err != nil {
		t.Fatalf("Failed to create storage: %v", err)
	}

	h := NewChunkUploadHandler(database, store, DefaultChunkSize)
	t.Cleanup(h.Shutdown)

	session, err := database.CreateUploadSession("video.mp4", 1024, 1024, 1, nil)
	if err != nil {
		t.Fatalf("CreateUploadSession: %v", err)
	}
	if _, _, _, err := store.SaveChunk(session.ID, 0, strings.NewReader("data")); err != nil {
		t.Fatalf("SaveChunk: %v", err)
	}

	chunkDir := filepath.Join(dataDir, "chunks", session.ID)
	if _, err := os.Stat(chunkDir); err != nil {
		t.Fatalf("expected chunk dir on disk: %v", err)
	}

	// Backdate expires_at so the session reads as expired. A file-backed DB
	// lets a second connection reach the row; :memory: pins the pool to one
	// connection and every other handle would see its own empty database.
	raw, err := sql.Open("sqlite3", "file:"+dbPath+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("open raw sqlite: %v", err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`UPDATE upload_sessions SET expires_at = ? WHERE id = ?`,
		time.Now().Add(-time.Hour), session.ID); err != nil {
		t.Fatalf("backdate expires_at: %v", err)
	}

	h.cleanupExpiredSessionsOnce()

	if _, err := os.Stat(chunkDir); !os.IsNotExist(err) {
		t.Errorf("chunk dir still exists after cleanup (err=%v)", err)
	}
	if _, err := database.GetUploadSession(session.ID); err == nil {
		t.Error("expired session row still present after cleanup")
	}
}
