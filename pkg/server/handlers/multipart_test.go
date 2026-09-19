package handlers

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/tsic404/rffmpeg/pkg/protocol"
	"github.com/tsic404/rffmpeg/pkg/server/db"
	"github.com/tsic404/rffmpeg/pkg/server/storage"
	"github.com/tsic404/rffmpeg/pkg/server/workerhealth"
)

func TestMultipartParseErrorStatus(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   protocol.ErrorCode
		wantMsg    string
	}{
		{
			name:       "disk full",
			err:        fmt.Errorf("parse: %w", syscall.ENOSPC),
			wantStatus: http.StatusInsufficientStorage,
			wantCode:   protocol.ErrCodeInsufficientStorage,
			wantMsg:    "Insufficient storage: no space left on device",
		},
		{
			name:       "deeply wrapped disk full",
			err:        fmt.Errorf("parse: %w", fmt.Errorf("create temp: %w", syscall.ENOSPC)),
			wantStatus: http.StatusInsufficientStorage,
			wantCode:   protocol.ErrCodeInsufficientStorage,
			wantMsg:    "Insufficient storage: no space left on device",
		},
		{
			name:       "malformed body",
			err:        errors.New("multipart: NextPart: EOF"),
			wantStatus: http.StatusBadRequest,
			wantCode:   protocol.ErrCodeInvalidRequest,
			wantMsg:    "Failed to parse multipart form",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, code, msg := multipartParseErrorStatus(tt.err)
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

func TestEnsureMultipartSpace(t *testing.T) {
	h := &Handler{} // multipartTmpDir empty → os.TempDir()

	// A body no filesystem can spool must be rejected with 507.
	w := httptest.NewRecorder()
	if h.ensureMultipartSpace(w, math.MaxInt64) {
		t.Fatal("expected a body too large for any disk to be rejected")
	}
	if w.Code != http.StatusInsufficientStorage {
		t.Errorf("status = %d, want %d", w.Code, http.StatusInsufficientStorage)
	}
	var resp protocol.ErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.Code != protocol.ErrCodeInsufficientStorage {
		t.Errorf("code = %q, want %q", resp.Code, protocol.ErrCodeInsufficientStorage)
	}

	// Unknown lengths skip the check.
	for _, n := range []int64{0, -1} {
		if !h.ensureMultipartSpace(httptest.NewRecorder(), n) {
			t.Errorf("expected length %d to skip the pre-check", n)
		}
	}

	// A tiny body always fits.
	if !h.ensureMultipartSpace(httptest.NewRecorder(), 1) {
		t.Error("expected a 1-byte body to pass the pre-check")
	}
}

// multipartSpillThreshold mirrors the 32MiB in-memory threshold the upload
// handlers pass to ParseMultipartForm; a file part larger than this spills to
// a temporary file.
const multipartSpillThreshold = 32 << 20

// newSpillingMultipartRequest builds a multipart body whose file part is size
// bytes (above the spill threshold), forcing the parser to write a temp file.
func newSpillingMultipartRequest(t *testing.T, target, field, filename string, size int) *http.Request {
	t.Helper()
	body := &bytes.Buffer{}
	w := multipart.NewWriter(body)
	part, err := w.CreateFormFile(field, filename)
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	chunk := bytes.Repeat([]byte("x"), 1024*1024)
	remaining := size
	for remaining >= len(chunk) {
		if _, err := part.Write(chunk); err != nil {
			t.Fatalf("write part: %v", err)
		}
		remaining -= len(chunk)
	}
	if remaining > 0 {
		if _, err := part.Write(chunk[:remaining]); err != nil {
			t.Fatalf("write remainder: %v", err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, target, body)
	req.Header.Set("Content-Type", w.FormDataContentType())
	return req
}

func assertNoSpillFiles(t *testing.T, dir string) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, "multipart-*"))
	if err != nil {
		t.Fatalf("glob spill dir: %v", err)
	}
	if len(matches) != 0 {
		t.Errorf("multipart spill files leaked: %v", matches)
	}
}

func TestUploadRemovesMultipartSpill(t *testing.T) {
	base := t.TempDir()
	spillDir := filepath.Join(base, "spill")
	if err := os.MkdirAll(spillDir, 0755); err != nil {
		t.Fatalf("mkdir spill: %v", err)
	}
	t.Setenv("TMPDIR", spillDir)

	database, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("db.New: %v", err)
	}
	t.Cleanup(func() { database.Close() })

	store, err := storage.New(filepath.Join(base, "data"))
	if err != nil {
		t.Fatalf("storage.New: %v", err)
	}

	h := New(database, store, "test", workerhealth.NewWorkerStateTable(time.Second))
	h.SetAuthToken("test-token")

	r := chi.NewRouter()
	r.Post("/api/v1/upload", h.Upload)

	req := newSpillingMultipartRequest(t, "/api/v1/upload", "file", "big.mp4", multipartSpillThreshold+1024)
	req.Header.Set("Authorization", "Bearer test-token")

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("upload status = %d (body: %s)", rec.Code, rec.Body.String())
	}
	assertNoSpillFiles(t, spillDir)
}

func TestUploadJobOutputRemovesMultipartSpill(t *testing.T) {
	base := t.TempDir()
	spillDir := filepath.Join(base, "spill")
	if err := os.MkdirAll(spillDir, 0755); err != nil {
		t.Fatalf("mkdir spill: %v", err)
	}
	t.Setenv("TMPDIR", spillDir)

	database, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("db.New: %v", err)
	}
	t.Cleanup(func() { database.Close() })

	store, err := storage.New(filepath.Join(base, "data"))
	if err != nil {
		t.Fatalf("storage.New: %v", err)
	}

	h := New(database, store, "test", workerhealth.NewWorkerStateTable(time.Second))
	h.SetAuthToken("test-token")

	job, err := database.CreateJob("[]", "[]", "out.mp4", false)
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	r := chi.NewRouter()
	r.Post("/api/v1/jobs/{jobId}/output", h.UploadJobOutput)

	req := newSpillingMultipartRequest(t, "/api/v1/jobs/"+job.ID+"/output", "file", "out.mp4", multipartSpillThreshold+1024)
	req.Header.Set("Authorization", "Bearer test-token")

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("output status = %d (body: %s)", rec.Code, rec.Body.String())
	}
	assertNoSpillFiles(t, spillDir)
}
