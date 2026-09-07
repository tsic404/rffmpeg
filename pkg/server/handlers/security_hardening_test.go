package handlers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tsic404/rffmpeg/pkg/protocol"
	"github.com/tsic404/rffmpeg/pkg/server/auth"
	"github.com/tsic404/rffmpeg/pkg/server/db"
	"github.com/tsic404/rffmpeg/pkg/server/storage"
)

// newChunkUploadTestHandler builds a ChunkUploadHandler wired to an in-memory
// database and a temp-dir storage, with auth enabled.
func newChunkUploadTestHandler(t *testing.T) (*ChunkUploadHandler, *db.Database, string) {
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

	return h, database, baseDir
}

// chunkInitRequest builds an authenticated init request with a JSON body.
func chunkInitRequest(t *testing.T, req protocol.ChunkUploadInitRequest) *http.Request {
	t.Helper()
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	r := httptest.NewRequest("POST", "/api/v1/upload/init", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer test-token")
	return r
}

const testFileID = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" // sha256 of empty

// TestInitChunkUpload_RejectsOversizedChunkSize verifies acceptance criterion 2:
// a client-declared ChunkSize above MaxChunkSize is rejected instead of being
// used to size the multipart memory buffer.
func TestInitChunkUpload_RejectsOversizedChunkSize(t *testing.T) {
	h, _, _ := newChunkUploadTestHandler(t)

	req := chunkInitRequest(t, protocol.ChunkUploadInitRequest{
		Filename:    "video.mp4",
		FileSize:    10 << 30, // 10GB file
		ChunkSize:   10 << 30, // 10GB chunks — memory exhaustion attempt
		TotalChunks: 1,
	})
	rec := httptest.NewRecorder()
	h.InitChunkUpload(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for oversized chunk_size, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestInitChunkUpload_TotalChunksMismatch verifies acceptance criterion 2:
// TotalChunks inconsistent with FileSize/ChunkSize is rejected with 400.
func TestInitChunkUpload_TotalChunksMismatch(t *testing.T) {
	h, _, _ := newChunkUploadTestHandler(t)

	// 25MB with 10MB chunks is exactly 3 chunks — declaring 2 must fail.
	req := chunkInitRequest(t, protocol.ChunkUploadInitRequest{
		Filename:    "video.mp4",
		FileSize:    25 << 20,
		ChunkSize:   DefaultChunkSize,
		TotalChunks: 2,
	})
	rec := httptest.NewRecorder()
	h.InitChunkUpload(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for inconsistent total_chunks, got %d: %s", rec.Code, rec.Body.String())
	}

	// The correct value must still be accepted.
	req = chunkInitRequest(t, protocol.ChunkUploadInitRequest{
		Filename:    "video.mp4",
		FileSize:    25 << 20,
		ChunkSize:   DefaultChunkSize,
		TotalChunks: 3,
	})
	rec = httptest.NewRecorder()
	h.InitChunkUpload(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 for consistent total_chunks, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp protocol.ChunkUploadInitResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.TotalChunks != 3 {
		t.Errorf("expected server-computed total_chunks=3, got %d", resp.TotalChunks)
	}
}

// TestValidateFileIDFormat verifies the hex-hash whitelist used by download
// endpoints: traversal fragments, empty strings and UUIDs are all rejected.
// Output file IDs use the UUID format instead.
func TestValidateFileIDFormat(t *testing.T) {
	valid := testFileID
	cases := []struct {
		id    string
		valid bool
	}{
		{valid, true},
		{"", false},
		{"../../etc/passwd", false},
		{strings.Repeat("a", 63), false},
		{strings.Repeat("A", 64), false}, // uppercase not allowed
		{"6ba7b810-9dad-11d1-80b4-00c04fd430c8", false},
		{"..", false},
	}
	for _, tc := range cases {
		if got := storage.ValidateFileID(tc.id); got != tc.valid {
			t.Errorf("ValidateFileID(%q) = %v, want %v", tc.id, got, tc.valid)
		}
	}

	if !storage.ValidateOutputFileID("6ba7b810-9dad-11d1-80b4-00c04fd430c8") {
		t.Error("ValidateOutputFileID should accept standard UUIDs")
	}
	for _, id := range []string{"", "../../etc/passwd", "..", strings.ToUpper("6ba7b810-9dad-11d1-80b4-00c04fd430c8")} {
		if storage.ValidateOutputFileID(id) {
			t.Errorf("ValidateOutputFileID(%q) = true, want false", id)
		}
	}
}

// TestInitChunkUpload_IgnoresClientTotalChunksWithoutValue verifies that when
// the client does not declare TotalChunks the server computes it.
func TestInitChunkUpload_IgnoresClientTotalChunksWithoutValue(t *testing.T) {
	h, _, _ := newChunkUploadTestHandler(t)

	req := chunkInitRequest(t, protocol.ChunkUploadInitRequest{
		Filename: "video.mp4",
		FileSize: 25 << 20,
	})
	rec := httptest.NewRecorder()
	h.InitChunkUpload(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp protocol.ChunkUploadInitResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.TotalChunks != 3 {
		t.Errorf("expected computed total_chunks=3, got %d", resp.TotalChunks)
	}
}

// TestCompleteChunkUpload_RenameFailureNoDanglingRecord verifies acceptance
// criterion 3: when the final rename fails for a reason other than existence,
// the handler returns 500, writes no DB record and cleans up the temp file.
//
// The failure is injected by making the files directory read-only so os.Rename
// of the assembled temp file into it fails (cross-device renames can't be
// simulated portably inside one filesystem; both surface as non-IsExist errors).
func TestCompleteChunkUpload_RenameFailureNoDanglingRecord(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root: read-only directory enforcement does not apply")
	}

	h, database, baseDir := newChunkUploadTestHandler(t)

	// Create a real upload session with one chunk containing known content.
	content := []byte("transcode me")
	session, err := database.CreateUploadSession("video.mp4", int64(len(content)), int64(len(content)), 1, nil)
	if err != nil {
		t.Fatalf("CreateUploadSession: %v", err)
	}
	chunkPath, size, _, err := h.storage.SaveChunk(session.ID, 0, bytes.NewReader(content))
	if err != nil {
		t.Fatalf("SaveChunk: %v", err)
	}
	if size != int64(len(content)) {
		t.Fatalf("unexpected chunk size %d", size)
	}
	if _, err := database.CreateUploadChunk(session.ID, 0, size, "", chunkPath); err != nil {
		t.Fatalf("CreateUploadChunk: %v", err)
	}
	if err := database.UpdateUploadSessionChunks(session.ID, 0); err != nil {
		t.Fatalf("UpdateUploadSessionChunks: %v", err)
	}

	filesDir := filepath.Join(baseDir, "files")
	if err := os.MkdirAll(filesDir, 0755); err != nil {
		t.Fatalf("MkdirAll files dir: %v", err)
	}
	// Make the destination directory read-only so the final rename fails.
	if err := os.Chmod(filesDir, 0555); err != nil {
		t.Fatalf("Chmod files dir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(filesDir, 0755) }) // allow TempDir cleanup

	body := fmt.Sprintf(`{"upload_id": %q}`, session.ID)
	req := httptest.NewRequest("POST", "/api/v1/upload/complete", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-token")

	rec := httptest.NewRecorder()
	h.CompleteChunkUpload(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 on rename failure, got %d: %s", rec.Code, rec.Body.String())
	}

	// DB must contain no file record pointing at the missing path.
	count := 0
	if err := database.GetDB().QueryRow(`SELECT COUNT(*) FROM files`).Scan(&count); err != nil {
		t.Fatalf("count files: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0 file records after failed rename, got %d", count)
	}
	// The assembled temp copy in files/ must be cleaned up — nothing may
	// linger at the content-hash path. Chunk files are intentionally kept:
	// the client can fix the transient error and re-complete the session.
	entries, err := os.ReadDir(filesDir)
	if err != nil {
		t.Fatalf("ReadDir files dir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("expected empty files dir after failed finalize, found %d entries", len(entries))
	}
}

// TestAuthMiddlewareIntegration_RegisterRequiresToken exercises the full
// middleware + handler stack to confirm acceptance criterion 1 end-to-end.
func TestAuthMiddlewareIntegration_RegisterRequiresToken(t *testing.T) {
	database, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("Failed to create database: %v", err)
	}
	defer database.Close()

	baseDir := t.TempDir()
	store, err := storage.New(baseDir)
	if err != nil {
		t.Fatalf("Failed to create storage: %v", err)
	}

	handler := New(database, store, "test", nil)

	mw := auth.Middleware("secret-token")
	registerBody := `{"worker_id":"ghost-1","name":"ghost","capabilities":{"encoders":["libx264"],"ffmpeg_version":"5.0"}}`

	post := func(token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/api/v1/workers/register", strings.NewReader(registerBody))
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		mw(http.HandlerFunc(handler.RegisterWorker)).ServeHTTP(rec, req)
		return rec
	}

	if rec := post(""); rec.Code != http.StatusUnauthorized {
		t.Errorf("register without token: expected 401, got %d", rec.Code)
	}
	if rec := post("wrong"); rec.Code != http.StatusUnauthorized {
		t.Errorf("register with wrong token: expected 401, got %d", rec.Code)
	}
	if rec := post("secret-token"); rec.Code != http.StatusOK {
		t.Errorf("register with correct token: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	workers, err := database.GetAllWorkers()
	if err != nil {
		t.Fatalf("GetAllWorkers: %v", err)
	}
	if len(workers) != 1 {
		t.Errorf("expected exactly 1 registered worker (only the authenticated attempt), got %d", len(workers))
	}
}
