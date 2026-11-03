package handlers

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
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
// except AssembleChunks, which returns a close-time ENOSPC error.
type failingAssembleStorage struct {
	*storage.Storage
	err error
}

func (f *failingAssembleStorage) AssembleChunks(uploadID string, totalChunks int, chunkSize, fileSize int64) (string, int64, string, error) {
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

// chunkTestServer wires the chunk upload handler to a real database, real
// storage and a chi router, so URL parameters resolve exactly as in
// production.
type chunkTestServer struct {
	handler *ChunkUploadHandler
	db      *db.Database
	store   *storage.Storage
	baseDir string
	router  chi.Router
}

func newChunkTestServer(t *testing.T) *chunkTestServer {
	t.Helper()

	database, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("Failed to create database: %v", err)
	}
	t.Cleanup(func() { database.Close() })

	baseDir := t.TempDir()
	store, err := storage.New(baseDir)
	if err != nil {
		t.Fatalf("Failed to create storage: %v", err)
	}

	h := NewChunkUploadHandler(database, store, DefaultChunkSize)
	h.SetAuthToken("test-token")
	t.Cleanup(h.Shutdown)

	r := chi.NewRouter()
	r.Post("/api/v1/upload/chunk/{uploadId}/{chunkIndex}", h.UploadChunk)
	r.Post("/api/v1/upload/complete", h.CompleteChunkUpload)

	return &chunkTestServer{handler: h, db: database, store: store, baseDir: baseDir, router: r}
}

func (s *chunkTestServer) uploadChunk(t *testing.T, uploadID string, chunkIndex int, data []byte) *httptest.ResponseRecorder {
	t.Helper()
	return s.uploadChunkWithChecksum(t, uploadID, chunkIndex, data, "")
}

func (s *chunkTestServer) uploadChunkWithChecksum(t *testing.T, uploadID string, chunkIndex int, data []byte, checksum string) *httptest.ResponseRecorder {
	t.Helper()

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("chunk", "chunk")
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	if _, err := part.Write(data); err != nil {
		t.Fatalf("write chunk part: %v", err)
	}
	if checksum != "" {
		if err := writer.WriteField("checksum", checksum); err != nil {
			t.Fatalf("write checksum field: %v", err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}

	req := httptest.NewRequest("POST", fmt.Sprintf("/api/v1/upload/chunk/%s/%d", uploadID, chunkIndex), &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Authorization", "Bearer test-token")

	rec := httptest.NewRecorder()
	s.router.ServeHTTP(rec, req)
	return rec
}

func (s *chunkTestServer) complete(t *testing.T, uploadID, checksum string) *httptest.ResponseRecorder {
	t.Helper()

	body, err := json.Marshal(protocol.ChunkUploadCompleteRequest{UploadID: uploadID, Checksum: checksum})
	if err != nil {
		t.Fatalf("marshal complete request: %v", err)
	}
	req := httptest.NewRequest("POST", "/api/v1/upload/complete", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-token")

	rec := httptest.NewRecorder()
	s.router.ServeHTTP(rec, req)
	return rec
}

// filesDirEntries lists what the final-file directory holds; an upload that
// failed before publishing must leave it empty.
func (s *chunkTestServer) filesDirEntries(t *testing.T) []os.DirEntry {
	t.Helper()

	entries, err := os.ReadDir(filepath.Join(s.baseDir, "files"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("ReadDir files: %v", err)
	}
	return entries
}

func (s *chunkTestServer) fileRecordCount(t *testing.T) int {
	t.Helper()

	var count int
	if err := s.db.GetDB().QueryRow(`SELECT COUNT(*) FROM files`).Scan(&count); err != nil {
		t.Fatalf("count files: %v", err)
	}
	return count
}

func (s *chunkTestServer) sessionStatus(t *testing.T, uploadID string) protocol.UploadSessionStatus {
	t.Helper()

	session, err := s.db.GetUploadSession(uploadID)
	if err != nil {
		t.Fatalf("GetUploadSession: %v", err)
	}
	return session.Status
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// TestUploadChunk_RetriesTruncatedChunk reproduces the crash-mid-write state:
// a short chunk_N exists on disk (and the session already counts it) while the
// client retries that index. The retry must rewrite the chunk, not answer
// "already uploaded", otherwise Complete assembles a corrupt file.
func TestUploadChunk_RetriesTruncatedChunk(t *testing.T) {
	srv := newChunkTestServer(t)

	content := []byte("helloworld")
	session, err := srv.db.CreateUploadSession("video.mp4", int64(len(content)), 5, 2, nil)
	if err != nil {
		t.Fatalf("CreateUploadSession: %v", err)
	}

	// Simulate the interrupted write: two of the five bytes made it to the
	// canonical path, and the session already tracks chunk 0 as uploaded.
	truncated := srv.store.ChunkPath(session.ID, 0)
	if err := os.MkdirAll(filepath.Dir(truncated), 0755); err != nil {
		t.Fatalf("MkdirAll chunk dir: %v", err)
	}
	if err := os.WriteFile(truncated, content[:2], 0644); err != nil {
		t.Fatalf("write truncated chunk: %v", err)
	}
	if err := srv.db.UpdateUploadSessionChunks(session.ID, 0); err != nil {
		t.Fatalf("UpdateUploadSessionChunks: %v", err)
	}

	rec := srv.uploadChunk(t, session.ID, 0, content[:5])
	if rec.Code != http.StatusOK {
		t.Fatalf("retry of truncated chunk: expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	var resp protocol.ChunkUploadResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal chunk response: %v", err)
	}
	if resp.Message == protocol.ChunkAlreadyUploadedMessage {
		t.Fatalf("truncated chunk was reported as already uploaded: %q", resp.Message)
	}

	if rec := srv.uploadChunk(t, session.ID, 1, content[5:]); rec.Code != http.StatusOK {
		t.Fatalf("chunk 1: expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}

	rec = srv.complete(t, session.ID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("complete: expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	var done protocol.ChunkUploadCompleteResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &done); err != nil {
		t.Fatalf("unmarshal complete response: %v", err)
	}
	if want := sha256Hex(content); done.FileID != want {
		t.Errorf("file id = %q, want %q", done.FileID, want)
	}

	got, err := os.ReadFile(srv.store.GetFilePath(done.FileID))
	if err != nil {
		t.Fatalf("read assembled file: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("assembled content = %q, want %q", got, content)
	}
}

func TestUploadChunk_RejectsWrongSizedChunk(t *testing.T) {
	srv := newChunkTestServer(t)

	session, err := srv.db.CreateUploadSession("video.mp4", 10, 5, 2, nil)
	if err != nil {
		t.Fatalf("CreateUploadSession: %v", err)
	}

	rec := srv.uploadChunk(t, session.ID, 0, []byte("abc"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for an undersized chunk, got %d (%s)", rec.Code, rec.Body.String())
	}
	if srv.store.ChunkExists(session.ID, 0, 3) {
		t.Error("a rejected chunk must not be left on disk")
	}
}

func TestCompleteChunkUpload_RejectsMissingChunk(t *testing.T) {
	srv := newChunkTestServer(t)

	session, err := srv.db.CreateUploadSession("video.mp4", 10, 5, 2, nil)
	if err != nil {
		t.Fatalf("CreateUploadSession: %v", err)
	}
	if rec := srv.uploadChunk(t, session.ID, 0, []byte("hello")); rec.Code != http.StatusOK {
		t.Fatalf("chunk 0: expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}

	rec := srv.complete(t, session.ID, "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a missing chunk, got %d (%s)", rec.Code, rec.Body.String())
	}
	var resp protocol.ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal error response: %v", err)
	}
	if resp.Code != protocol.ErrCodeInvalidRequest {
		t.Errorf("code = %q, want %q", resp.Code, protocol.ErrCodeInvalidRequest)
	}
	if entries := srv.filesDirEntries(t); len(entries) != 0 {
		t.Errorf("expected no file after missing chunk, found %d entries", len(entries))
	}
	if srv.fileRecordCount(t) != 0 {
		t.Error("expected no file record after missing chunk")
	}
	if status := srv.sessionStatus(t, session.ID); status != protocol.UploadSessionStatusInProgress {
		t.Errorf("session status = %s, want %s so the client can resend the gap", status, protocol.UploadSessionStatusInProgress)
	}
}

// TestCompleteChunkUpload_RejectsFileSizeMismatch covers a chunk set whose
// chunks exist but do not add up to the declared file size: the last chunk is
// short, which without size validation would be assembled into a truncated
// file and reported as a successful upload.
func TestCompleteChunkUpload_RejectsFileSizeMismatch(t *testing.T) {
	srv := newChunkTestServer(t)

	session, err := srv.db.CreateUploadSession("video.mp4", 10, 5, 2, nil)
	if err != nil {
		t.Fatalf("CreateUploadSession: %v", err)
	}
	if _, _, _, err := srv.store.SaveChunk(session.ID, 0, strings.NewReader("hello")); err != nil {
		t.Fatalf("SaveChunk 0: %v", err)
	}
	if _, _, _, err := srv.store.SaveChunk(session.ID, 1, strings.NewReader("wor")); err != nil {
		t.Fatalf("SaveChunk 1: %v", err)
	}

	rec := srv.complete(t, session.ID, "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a truncated chunk set, got %d (%s)", rec.Code, rec.Body.String())
	}
	if entries := srv.filesDirEntries(t); len(entries) != 0 {
		t.Errorf("expected no file after size mismatch, found %d entries", len(entries))
	}
	if srv.fileRecordCount(t) != 0 {
		t.Error("expected no file record after size mismatch")
	}
}

// countingAssembleStorage records how many assemblies actually ran.
type countingAssembleStorage struct {
	*storage.Storage
	assembles atomic.Int64
}

func (c *countingAssembleStorage) AssembleChunks(uploadID string, totalChunks int, chunkSize, fileSize int64) (string, int64, string, error) {
	c.assembles.Add(1)
	return c.Storage.AssembleChunks(uploadID, totalChunks, chunkSize, fileSize)
}

// TestCompleteChunkUpload_ConcurrentAssemblesOnce runs two completions of the
// same session concurrently: the chunks must be assembled exactly once and both
// callers must receive the same file identifier.
func TestCompleteChunkUpload_ConcurrentAssemblesOnce(t *testing.T) {
	srv := newChunkTestServer(t)

	content := []byte("helloworld")
	session, err := srv.db.CreateUploadSession("video.mp4", int64(len(content)), 5, 2, nil)
	if err != nil {
		t.Fatalf("CreateUploadSession: %v", err)
	}
	if rec := srv.uploadChunk(t, session.ID, 0, content[:5]); rec.Code != http.StatusOK {
		t.Fatalf("chunk 0: expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	if rec := srv.uploadChunk(t, session.ID, 1, content[5:]); rec.Code != http.StatusOK {
		t.Fatalf("chunk 1: expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}

	counting := &countingAssembleStorage{Storage: srv.store}
	srv.handler.storage = counting

	var wg sync.WaitGroup
	start := make(chan struct{})
	recorders := make([]*httptest.ResponseRecorder, 2)
	for i := range recorders {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			recorders[i] = srv.complete(t, session.ID, "")
		}(i)
	}
	close(start)
	wg.Wait()

	var fileIDs []string
	for i, rec := range recorders {
		if rec.Code != http.StatusOK {
			t.Fatalf("complete %d: expected 200, got %d (%s)", i, rec.Code, rec.Body.String())
		}
		var resp protocol.ChunkUploadCompleteResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("unmarshal complete %d: %v", i, err)
		}
		fileIDs = append(fileIDs, resp.FileID)
	}

	if got := counting.assembles.Load(); got != 1 {
		t.Errorf("assembly ran %d times, want exactly 1", got)
	}
	if fileIDs[0] != fileIDs[1] {
		t.Errorf("concurrent completions returned different file ids: %q vs %q", fileIDs[0], fileIDs[1])
	}
	if want := sha256Hex(content); fileIDs[0] != want {
		t.Errorf("file id = %q, want %q", fileIDs[0], want)
	}
	if entries := srv.filesDirEntries(t); len(entries) != 1 || entries[0].Name() != fileIDs[0] {
		t.Errorf("expected exactly one published file %q, got %v", fileIDs[0], entries)
	}
}

// TestCompleteChunkUpload_ChecksumMismatchLeavesNoFile verifies the assembled
// file is verified before it is published: a mismatching whole-file checksum
// returns 400, leaves no final file and no DB record, and lets a corrected
// retry succeed.
func TestCompleteChunkUpload_ChecksumMismatchLeavesNoFile(t *testing.T) {
	srv := newChunkTestServer(t)

	content := []byte("helloworld")
	session, err := srv.db.CreateUploadSession("video.mp4", int64(len(content)), 5, 2, nil)
	if err != nil {
		t.Fatalf("CreateUploadSession: %v", err)
	}
	if rec := srv.uploadChunk(t, session.ID, 0, content[:5]); rec.Code != http.StatusOK {
		t.Fatalf("chunk 0: expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	if rec := srv.uploadChunk(t, session.ID, 1, content[5:]); rec.Code != http.StatusOK {
		t.Fatalf("chunk 1: expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}

	rec := srv.complete(t, session.ID, sha256Hex([]byte("different")))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a checksum mismatch, got %d (%s)", rec.Code, rec.Body.String())
	}
	if entries := srv.filesDirEntries(t); len(entries) != 0 {
		t.Errorf("expected no published file after checksum mismatch, found %v", entries)
	}
	if srv.fileRecordCount(t) != 0 {
		t.Error("expected no file record after checksum mismatch")
	}
	if status := srv.sessionStatus(t, session.ID); status != protocol.UploadSessionStatusInProgress {
		t.Errorf("session status = %s, want %s so the client can retry", status, protocol.UploadSessionStatusInProgress)
	}

	rec = srv.complete(t, session.ID, sha256Hex(content))
	if rec.Code != http.StatusOK {
		t.Fatalf("corrected retry: expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}
}

func TestCompleteChunkUpload_InitChecksumMismatchLeavesNoFile(t *testing.T) {
	srv := newChunkTestServer(t)

	content := []byte("helloworld")
	declared := sha256Hex([]byte("some other file"))
	session, err := srv.db.CreateUploadSession("video.mp4", int64(len(content)), 5, 2, &declared)
	if err != nil {
		t.Fatalf("CreateUploadSession: %v", err)
	}
	if rec := srv.uploadChunk(t, session.ID, 0, content[:5]); rec.Code != http.StatusOK {
		t.Fatalf("chunk 0: expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	if rec := srv.uploadChunk(t, session.ID, 1, content[5:]); rec.Code != http.StatusOK {
		t.Fatalf("chunk 1: expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}

	rec := srv.complete(t, session.ID, "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for an init checksum mismatch, got %d (%s)", rec.Code, rec.Body.String())
	}
	if entries := srv.filesDirEntries(t); len(entries) != 0 {
		t.Errorf("expected no published file after checksum mismatch, found %v", entries)
	}
}

// TestCompleteChunkUpload_RepeatReturnsSameFileID covers the lost-response
// path: completing an already completed session returns the same identifier
// without rebuilding the file.
func TestCompleteChunkUpload_RepeatReturnsSameFileID(t *testing.T) {
	srv := newChunkTestServer(t)

	content := []byte("helloworld")
	session, err := srv.db.CreateUploadSession("video.mp4", int64(len(content)), 5, 2, nil)
	if err != nil {
		t.Fatalf("CreateUploadSession: %v", err)
	}
	if rec := srv.uploadChunk(t, session.ID, 0, content[:5]); rec.Code != http.StatusOK {
		t.Fatalf("chunk 0: expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	if rec := srv.uploadChunk(t, session.ID, 1, content[5:]); rec.Code != http.StatusOK {
		t.Fatalf("chunk 1: expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}

	first := srv.complete(t, session.ID, "")
	if first.Code != http.StatusOK {
		t.Fatalf("first complete: expected 200, got %d (%s)", first.Code, first.Body.String())
	}
	var firstResp protocol.ChunkUploadCompleteResponse
	if err := json.Unmarshal(first.Body.Bytes(), &firstResp); err != nil {
		t.Fatalf("unmarshal first complete: %v", err)
	}

	second := srv.complete(t, session.ID, "")
	if second.Code != http.StatusOK {
		t.Fatalf("repeat complete: expected 200, got %d (%s)", second.Code, second.Body.String())
	}
	var secondResp protocol.ChunkUploadCompleteResponse
	if err := json.Unmarshal(second.Body.Bytes(), &secondResp); err != nil {
		t.Fatalf("unmarshal repeat complete: %v", err)
	}
	if secondResp.FileID != firstResp.FileID {
		t.Errorf("repeat complete returned %q, want the original %q", secondResp.FileID, firstResp.FileID)
	}
	if entries := srv.filesDirEntries(t); len(entries) != 1 {
		t.Errorf("repeat complete must not rebuild the file, found %d entries", len(entries))
	}
}

// TestInitChunkUpload_RejectsUnboundedTotalChunks pins the resource-exhaustion
// guard on session creation: file_size has no upper bound and chunk_size may be
// a single byte, so an uncapped total_chunks would let one init + complete
// demand a terabyte-scale allocation during assembly.
func TestInitChunkUpload_RejectsUnboundedTotalChunks(t *testing.T) {
	h, database, _ := newChunkUploadTestHandler(t)

	cases := []struct {
		name      string
		fileSize  int64
		chunkSize int64
	}{
		{name: "max int64 file with one-byte chunks", fileSize: math.MaxInt64, chunkSize: 1},
		{name: "one chunk over the bound", fileSize: MaxTotalChunks + 1, chunkSize: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.InitChunkUpload(rec, chunkInitRequest(t, protocol.ChunkUploadInitRequest{
				Filename:  "huge.mp4",
				FileSize:  tc.fileSize,
				ChunkSize: tc.chunkSize,
			}))

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d (%s)", rec.Code, rec.Body.String())
			}
			var resp protocol.ErrorResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("unmarshal error response: %v", err)
			}
			if resp.Code != protocol.ErrCodeInvalidRequest {
				t.Errorf("code = %q, want %q", resp.Code, protocol.ErrCodeInvalidRequest)
			}
			if !strings.Contains(resp.Message, "total_chunks") {
				t.Errorf("message %q does not name the rejected parameter", resp.Message)
			}
		})
	}

	var count int
	if err := database.GetDB().QueryRow(`SELECT COUNT(*) FROM upload_sessions`).Scan(&count); err != nil {
		t.Fatalf("count upload sessions: %v", err)
	}
	if count != 0 {
		t.Errorf("rejected init requests created %d sessions, want 0", count)
	}

	// The bound itself stays usable: a session exactly at the limit is accepted.
	rec := httptest.NewRecorder()
	h.InitChunkUpload(rec, chunkInitRequest(t, protocol.ChunkUploadInitRequest{
		Filename:  "edge.mp4",
		FileSize:  MaxTotalChunks,
		ChunkSize: 1,
	}))
	if rec.Code != http.StatusOK {
		t.Fatalf("session at the bound: expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}
}

// TestUploadChunk_AcceptsUppercaseChecksum pins the case-insensitive
// comparison: hex case carries no meaning for a SHA-256, so an uppercase
// digest must not fail the chunk and delete it.
func TestUploadChunk_AcceptsUppercaseChecksum(t *testing.T) {
	srv := newChunkTestServer(t)

	session, err := srv.db.CreateUploadSession("video.mp4", 10, 10, 1, nil)
	if err != nil {
		t.Fatalf("CreateUploadSession: %v", err)
	}
	content := []byte("helloworld")

	rec := srv.uploadChunkWithChecksum(t, session.ID, 0, content, strings.ToUpper(sha256Hex(content)))
	if rec.Code != http.StatusOK {
		t.Fatalf("uppercase checksum: expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	if !srv.store.ChunkExists(session.ID, 0, int64(len(content))) {
		t.Error("uppercase checksum must leave the chunk in place")
	}
}

func TestUploadChunk_RejectsMismatchedChecksum(t *testing.T) {
	srv := newChunkTestServer(t)

	session, err := srv.db.CreateUploadSession("video.mp4", 10, 10, 1, nil)
	if err != nil {
		t.Fatalf("CreateUploadSession: %v", err)
	}
	content := []byte("helloworld")

	rec := srv.uploadChunkWithChecksum(t, session.ID, 0, content, sha256Hex([]byte("different")))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a mismatched chunk checksum, got %d (%s)", rec.Code, rec.Body.String())
	}
	if srv.store.ChunkExists(session.ID, 0, int64(len(content))) {
		t.Error("a rejected chunk must be deleted")
	}
}
