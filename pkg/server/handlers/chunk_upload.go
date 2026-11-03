package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/tsic404/rffmpeg/pkg/protocol"
	"github.com/tsic404/rffmpeg/pkg/server/auth"
	"github.com/tsic404/rffmpeg/pkg/server/db"
	"github.com/tsic404/rffmpeg/pkg/server/panicguard"
	"github.com/tsic404/rffmpeg/pkg/server/storage"
)

// MaxChunkSize caps the per-chunk size a client may request. Larger values
// are clamped server-side to prevent a single init request from forcing
// huge multipart memory buffers.
const MaxChunkSize = 64 * 1024 * 1024

// MaxTotalChunks bounds how many chunks a session may declare. Assembly holds
// one entry per chunk and walks the index range, so a client-declared
// total_chunks of 10^15 would otherwise buy a multi-terabyte allocation (or a
// near-endless walk) from a single init — file_size has no upper bound and
// chunk_size may be as small as one byte.
const MaxTotalChunks = 1 << 20

// Default chunk size: 10MB
const DefaultChunkSize = 10 * 1024 * 1024

// Max concurrent chunk uploads
const MaxConcurrentChunkUploads = 4

// chunkStorage is the subset of *storage.Storage the chunk upload handler
// depends on. It exists so handler tests can inject a storage that fails,
// exercising the 507/logging error path that a concrete *storage.Storage
// cannot reproduce portably (close-time ENOSPC).
type chunkStorage interface {
	ChunkExists(uploadID string, chunkIndex int, expectedSize int64) bool
	SaveChunk(uploadID string, chunkIndex int, reader io.Reader) (string, int64, string, error)
	DeleteChunk(uploadID string, chunkIndex int) error
	AssembleChunks(uploadID string, totalChunks int, chunkSize, fileSize int64) (string, int64, string, error)
	GetFilePath(fileID string) string
	DeleteFile(fileID string) error
	DeleteChunkDirectory(uploadID string) error
}

// ChunkUploadHandler handles chunked file uploads
type ChunkUploadHandler struct {
	db           *db.Database
	storage      chunkStorage
	chunkSize    int64
	authToken    string        // Non-empty when auth is configured
	semaphore    chan struct{} // For concurrent upload control
	sessionLocks *sessionLock  // Serializes completion per upload session
	cleanupOnce  sync.Once
	wg           sync.WaitGroup // Tracks async cleanup goroutines
	done         chan struct{}  // Signals shutdown to background goroutines
}

// sessionLock hands out one mutex per upload session. Completion holds it for
// the whole claim-assemble-finalize sequence, so concurrent completes of the
// same session assemble once and every caller returns the same file id.
type sessionLock struct {
	mu    sync.Mutex
	locks map[string]*sessionLockEntry
}

type sessionLockEntry struct {
	mu   sync.Mutex
	refs int
}

func newSessionLock() *sessionLock {
	return &sessionLock{locks: make(map[string]*sessionLockEntry)}
}

// Lock blocks until the session's mutex is held and returns its release
// function. Entries are dropped once no caller references them, so the map
// does not grow with the number of uploads served.
func (l *sessionLock) Lock(key string) func() {
	l.mu.Lock()
	entry := l.locks[key]
	if entry == nil {
		entry = &sessionLockEntry{}
		l.locks[key] = entry
	}
	entry.refs++
	l.mu.Unlock()

	entry.mu.Lock()

	return func() {
		entry.mu.Unlock()
		l.mu.Lock()
		entry.refs--
		if entry.refs == 0 {
			delete(l.locks, key)
		}
		l.mu.Unlock()
	}
}

// NewChunkUploadHandler creates a new chunk upload handler
func NewChunkUploadHandler(database *db.Database, store *storage.Storage, chunkSize int64) *ChunkUploadHandler {
	if chunkSize <= 0 {
		chunkSize = DefaultChunkSize
	}

	h := &ChunkUploadHandler{
		db:           database,
		storage:      store,
		chunkSize:    chunkSize,
		semaphore:    make(chan struct{}, MaxConcurrentChunkUploads),
		sessionLocks: newSessionLock(),
		done:         make(chan struct{}),
	}

	// Start cleanup routine
	go panicguard.Guard("chunk upload session cleaner", h.cleanupExpiredSessions)

	return h
}

// SetAuthToken sets the auth token for upload endpoint validation
func (h *ChunkUploadHandler) SetAuthToken(token string) {
	h.authToken = token
}

// validateAuthToken validates the Authorization header for chunk upload requests.
// Delegates to auth.ValidateBearer — the single shared Bearer implementation —
// so all endpoints drift-proof their token checks. When no auth token is
// configured the request is rejected: fail closed.
func (h *ChunkUploadHandler) validateAuthToken(w http.ResponseWriter, r *http.Request) (string, bool) {
	clientID, ok := auth.ValidateBearer(w, r, h.authToken)
	if !ok {
		return "", false
	}
	ctx := context.WithValue(r.Context(), auth.ClientIDKey, clientID)
	*r = *r.WithContext(ctx)
	return clientID, true
}

// Shutdown gracefully stops the background cleanup goroutine and waits for
// pending async operations to complete. Call this before closing the database.
// Safe to call multiple times.
func (h *ChunkUploadHandler) Shutdown() {
	h.cleanupOnce.Do(func() {
		close(h.done)
	})
	h.wg.Wait()
}

// InitChunkUpload initializes a new chunked upload session
func (h *ChunkUploadHandler) InitChunkUpload(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.validateAuthToken(w, r); !ok {
		writeError(w, http.StatusUnauthorized, protocol.NewProtocolError(
			protocol.ErrCodeUnauthorized, "Unauthorized: invalid or missing token", nil,
		))
		return
	}
	var req protocol.ChunkUploadInitRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, protocol.NewProtocolError(
			protocol.ErrCodeInvalidRequest, "Invalid JSON body", err,
		))
		return
	}

	// Validate required fields
	if req.Filename == "" || req.FileSize <= 0 {
		writeError(w, http.StatusBadRequest, protocol.NewProtocolError(
			protocol.ErrCodeInvalidRequest, "filename and file_size are required", nil,
		))
		return
	}

	// Validate file type
	if !validateFileType(req.Filename) {
		writeError(w, http.StatusBadRequest, protocol.NewProtocolError(
			protocol.ErrCodeInvalidRequest, "Invalid file type. Allowed types: video, audio, image, subtitle formats", nil,
		))
		return
	}

	// Determine chunk size. Client-requested values are clamped to
	// [1, MaxChunkSize]: a declared 10GB ChunkSize would otherwise drive a
	// ~10GB ParseMultipartForm memory buffer per request.
	chunkSize := h.chunkSize
	if req.ChunkSize > 0 {
		chunkSize = req.ChunkSize
		if chunkSize > MaxChunkSize {
			writeError(w, http.StatusBadRequest, protocol.NewProtocolError(
				protocol.ErrCodeInvalidRequest,
				fmt.Sprintf("chunk_size %d exceeds maximum of %d bytes", req.ChunkSize, MaxChunkSize), nil,
			))
			return
		}
	}
	if chunkSize <= 0 {
		chunkSize = DefaultChunkSize
	}

	// Calculate total chunks from FileSize — the client-supplied TotalChunks
	// is never trusted, so the accounting can't be bypassed by declaring an
	// inconsistent value.
	totalChunks := int(req.FileSize / chunkSize)
	if req.FileSize%chunkSize > 0 {
		totalChunks++
	}

	// A session is validated against the declared chunking here rather than
	// later: assembly sizes its working set from totalChunks, so an
	// unbounded value would turn one init + complete into a huge allocation.
	if totalChunks > MaxTotalChunks {
		writeError(w, http.StatusBadRequest, protocol.NewProtocolError(
			protocol.ErrCodeInvalidRequest,
			fmt.Sprintf("total_chunks %d exceeds maximum of %d (file_size %d with chunk_size %d)",
				totalChunks, MaxTotalChunks, req.FileSize, chunkSize), nil,
		))
		return
	}

	// If the client declares its planned chunking, it must match the
	// server-computed values exactly.
	if req.TotalChunks > 0 && req.TotalChunks != totalChunks {
		writeError(w, http.StatusBadRequest, protocol.NewProtocolError(
			protocol.ErrCodeInvalidRequest,
			fmt.Sprintf("total_chunks %d inconsistent with file_size %d and chunk_size %d (expected %d)",
				req.TotalChunks, req.FileSize, chunkSize, totalChunks), nil,
		))
		return
	}

	// Create upload session
	var checksum *string
	if req.Checksum != "" {
		checksum = &req.Checksum
	}

	session, err := h.db.CreateUploadSession(req.Filename, req.FileSize, chunkSize, totalChunks, checksum)
	if err != nil {
		writeError(w, http.StatusInternalServerError, protocol.NewProtocolError(
			protocol.ErrCodeInternalError, "Failed to create upload session", err,
		))
		return
	}

	writeJSON(w, http.StatusOK, protocol.ChunkUploadInitResponse{
		UploadID:    session.ID,
		ChunkSize:   session.ChunkSize,
		TotalChunks: session.TotalChunks,
		Message:     "Upload session created successfully",
	})
}

// chunkSaveErrorStatus classifies a SaveChunk failure for the HTTP response.
// A disk-full write (ENOSPC) is a client-actionable capacity failure: it is
// reported as 507 with a distinct error code and a readable message so the CLI
// can print an actionable hint. Every other error stays a generic 500.
func chunkSaveErrorStatus(err error) (int, protocol.ErrorCode, string) {
	if errors.Is(err, syscall.ENOSPC) {
		return http.StatusInsufficientStorage, protocol.ErrCodeInsufficientStorage, "Insufficient storage: no space left on device"
	}
	return http.StatusInternalServerError, protocol.ErrCodeUploadFailed, "Failed to save chunk"
}

// assembleErrorStatus classifies an AssembleChunks failure for the HTTP
// response. A disk-full write (ENOSPC) is a client-actionable capacity
// failure: it is reported as 507 with a distinct error code and a message
// naming the data directory so the CLI can print an actionable hint. Every
// other error stays a generic 500 whose message preserves the wrapped cause.
func assembleErrorStatus(err error) (int, protocol.ErrorCode, string) {
	if errors.Is(err, syscall.ENOSPC) {
		return http.StatusInsufficientStorage, protocol.ErrCodeInsufficientStorage, "Insufficient storage: no space left on device (data-dir)"
	}
	return http.StatusInternalServerError, protocol.ErrCodeUploadFailed, fmt.Sprintf("Failed to assemble file: %v", err)
}

// expectedChunkSize reports the exact byte count chunkIndex must carry under
// the session's chunking: every chunk is chunk_size except the last, which
// holds the remainder (C-37).
func expectedChunkSize(session *db.UploadSession, chunkIndex int) (int64, error) {
	if session.ChunkSize <= 0 || session.TotalChunks <= 0 || session.FileSize <= 0 {
		return 0, fmt.Errorf("session %s declares file_size=%d chunk_size=%d total_chunks=%d",
			session.ID, session.FileSize, session.ChunkSize, session.TotalChunks)
	}
	if chunkIndex == session.TotalChunks-1 {
		size := session.FileSize - session.ChunkSize*int64(session.TotalChunks-1)
		if size <= 0 {
			return 0, fmt.Errorf("session %s declares file_size=%d chunk_size=%d total_chunks=%d",
				session.ID, session.FileSize, session.ChunkSize, session.TotalChunks)
		}
		return size, nil
	}
	return session.ChunkSize, nil
}

// isChunkSetError reports whether err describes a chunk set the client can
// repair by resending, as opposed to a storage failure.
func isChunkSetError(err error) bool {
	return errors.Is(err, storage.ErrChunkMissing) ||
		errors.Is(err, storage.ErrChunkSizeMismatch) ||
		errors.Is(err, storage.ErrAssembledSizeMismatch)
}

// chunkAssemblyErrorMessage renders a chunk-set failure for the client.
func chunkAssemblyErrorMessage(err error) string {
	switch {
	case errors.Is(err, storage.ErrChunkMissing):
		return "Not all chunks uploaded"
	case errors.Is(err, storage.ErrChunkSizeMismatch):
		return "Chunk size does not match the upload session"
	default:
		return "Chunk set does not add up to the declared file size"
	}
}

// declaredChecksumMismatch returns a non-empty message when a checksum the
// client declared (at init or on completion) disagrees with the bytes actually
// received. Either claim being wrong means the assembled file is not the file
// that was meant to be uploaded (C-40).
func declaredChecksumMismatch(initChecksum, completeChecksum, actualChecksum string) string {
	if initChecksum != "" && !strings.EqualFold(initChecksum, actualChecksum) {
		return "Final file checksum mismatch with the checksum declared at init"
	}
	if completeChecksum != "" && !strings.EqualFold(completeChecksum, actualChecksum) {
		return "Final file checksum mismatch"
	}
	return ""
}

// UploadChunk handles uploading a single chunk
func (h *ChunkUploadHandler) UploadChunk(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.validateAuthToken(w, r); !ok {
		writeError(w, http.StatusUnauthorized, protocol.NewProtocolError(
			protocol.ErrCodeUnauthorized, "Unauthorized: invalid or missing token", nil,
		))
		return
	}

	uploadID := chi.URLParam(r, "uploadId")
	chunkIndexStr := chi.URLParam(r, "chunkIndex")

	chunkIndex, err := strconv.Atoi(chunkIndexStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, protocol.NewProtocolError(
			protocol.ErrCodeInvalidRequest, "Invalid chunk index", err,
		))
		return
	}

	// Get upload session
	session, err := h.db.GetUploadSession(uploadID)
	if err != nil {
		writeError(w, http.StatusNotFound, protocol.NewProtocolError(
			protocol.ErrCodeNotFound, "Upload session not found", err,
		))
		return
	}

	// Validate session status
	if session.Status != protocol.UploadSessionStatusInProgress {
		writeError(w, http.StatusBadRequest, protocol.NewProtocolError(
			protocol.ErrCodeInvalidRequest, "Upload session is not in progress", nil,
		))
		return
	}

	// Validate chunk index
	if chunkIndex < 0 || chunkIndex >= session.TotalChunks {
		writeError(w, http.StatusBadRequest, protocol.NewProtocolError(
			protocol.ErrCodeInvalidRequest, "Invalid chunk index", nil,
		))
		return
	}

	expectedSize, err := expectedChunkSize(session, chunkIndex)
	if err != nil {
		writeError(w, http.StatusBadRequest, protocol.NewProtocolError(
			protocol.ErrCodeInvalidRequest, "Upload session has an invalid chunking", err,
		))
		return
	}

	// Idempotent retry: a chunk only counts as uploaded when its stored size is
	// exactly the one the session implies. A crash-truncated chunk_N must be
	// rewritten here — otherwise it survives to assembly and the corrupt
	// content is reported as a successful upload.
	if h.storage.ChunkExists(uploadID, chunkIndex, expectedSize) {
		// Ensure session tracking is up-to-date even for idempotent retries.
		// If a previous attempt saved the chunk to disk but failed to update
		// the session (e.g., DB busy), the session's UploadedChunks may be
		// missing this chunk, which would later cause "Not all chunks uploaded"
		// or "Failed to assemble file" depending on the state.
		if err := h.db.UpdateUploadSessionChunks(uploadID, chunkIndex); err != nil {
			log.Printf("Warning: chunk %d exists on disk but failed to update session %s: %v",
				chunkIndex, uploadID, err)
		}

		writeJSON(w, http.StatusOK, protocol.ChunkUploadResponse{
			UploadID:   uploadID,
			ChunkIndex: chunkIndex,
			Message:    protocol.ChunkAlreadyUploadedMessage,
		})
		return
	}

	// Wait for a concurrent-upload slot without ignoring cancellation: four
	// slow clients can occupy every slot, and a queued request whose client is
	// already gone must leave the queue instead of waiting for one of them.
	select {
	case h.semaphore <- struct{}{}:
		defer func() { <-h.semaphore }()
	case <-r.Context().Done():
		return
	}

	// Parse multipart form. The session's ChunkSize was validated at init
	// time, but legacy sessions created before that validation may carry an
	// oversized value — clamp again here so the memory buffer stays bounded.
	chunkSize := session.ChunkSize
	if chunkSize <= 0 || chunkSize > MaxChunkSize {
		chunkSize = MaxChunkSize
	}

	maxChunkSize := chunkSize + 1024 // Allow small overhead
	r.Body = http.MaxBytesReader(w, r.Body, maxChunkSize)

	// Use the chunk size (plus overhead) as the in-memory threshold.
	if err := r.ParseMultipartForm(chunkSize + 1024*1024); err != nil {
		writeError(w, http.StatusBadRequest, protocol.NewProtocolError(
			protocol.ErrCodeInvalidRequest, "Failed to parse multipart form", err,
		))
		return
	}
	// Oversized parts spill to temp files the stdlib keeps for the caller to
	// read; unlink them when the request finishes so they cannot leak.
	defer r.MultipartForm.RemoveAll()

	file, _, err := r.FormFile("chunk")
	if err != nil {
		writeError(w, http.StatusBadRequest, protocol.NewProtocolError(
			protocol.ErrCodeInvalidRequest, "Missing chunk in request", err,
		))
		return
	}
	defer file.Close()

	// Get optional checksum from request
	expectedChecksum := r.FormValue("checksum")

	// Save chunk to storage
	chunkPath, size, actualChecksum, err := h.storage.SaveChunk(uploadID, chunkIndex, file)
	if err != nil {
		// A full disk previously surfaced as a bare 500 with no server-side
		// trace — log the cause so capacity failures are diagnosable.
		log.Printf("Failed to save chunk for upload %s chunk %d: %v", uploadID, chunkIndex, err)
		status, code, message := chunkSaveErrorStatus(err)
		writeError(w, status, protocol.NewProtocolError(code, message, err))
		return
	}

	// Verify the chunk carries exactly the bytes the session declares: every
	// chunk but the last is chunk_size, the last is the remainder. Without
	// this, an undersized chunk would only surface at completion, after the
	// client believes the chunk is safely stored.
	if size != expectedSize {
		h.storage.DeleteChunk(uploadID, chunkIndex)
		writeError(w, http.StatusBadRequest, protocol.NewProtocolError(
			protocol.ErrCodeInvalidRequest,
			fmt.Sprintf("Chunk %d has %d bytes, expected %d", chunkIndex, size, expectedSize), nil,
		))
		return
	}

	// Verify checksum if provided. Hex case is not part of a SHA-256's
	// identity, so an uppercase digest must be accepted like the whole-file
	// checksum comparison does.
	if expectedChecksum != "" && !strings.EqualFold(expectedChecksum, actualChecksum) {
		h.storage.DeleteChunk(uploadID, chunkIndex)
		writeError(w, http.StatusBadRequest, protocol.NewProtocolError(
			protocol.ErrCodeInvalidRequest, "Checksum mismatch", nil,
		))
		return
	}

	// Create chunk record in database
	_, err = h.db.CreateUploadChunk(uploadID, chunkIndex, size, actualChecksum, chunkPath)
	if err != nil {
		h.storage.DeleteChunk(uploadID, chunkIndex)
		writeError(w, http.StatusInternalServerError, protocol.NewProtocolError(
			protocol.ErrCodeInternalError, "Failed to create chunk record", err,
		))
		return
	}

	// Update session's uploaded chunks list
	if err := h.db.UpdateUploadSessionChunks(uploadID, chunkIndex); err != nil {
		writeError(w, http.StatusInternalServerError, protocol.NewProtocolError(
			protocol.ErrCodeInternalError, "Failed to update upload session", err,
		))
		return
	}

	writeJSON(w, http.StatusOK, protocol.ChunkUploadResponse{
		UploadID:   uploadID,
		ChunkIndex: chunkIndex,
		Message:    "Chunk uploaded successfully",
	})
}

// GetUploadProgress returns the upload progress
func (h *ChunkUploadHandler) GetUploadProgress(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.validateAuthToken(w, r); !ok {
		writeError(w, http.StatusUnauthorized, protocol.NewProtocolError(
			protocol.ErrCodeUnauthorized, "Unauthorized: invalid or missing token", nil,
		))
		return
	}

	uploadID := chi.URLParam(r, "uploadId")

	session, err := h.db.GetUploadSession(uploadID)
	if err != nil {
		writeError(w, http.StatusNotFound, protocol.NewProtocolError(
			protocol.ErrCodeNotFound, "Upload session not found", err,
		))
		return
	}

	// Parse uploaded chunks
	var completedChunks []int
	if err := json.Unmarshal([]byte(session.UploadedChunks), &completedChunks); err != nil {
		completedChunks = []int{}
	}

	// Calculate uploaded size
	uploadedSize := int64(len(completedChunks)) * session.ChunkSize
	// Adjust for last chunk which may be smaller
	if len(completedChunks) == session.TotalChunks {
		uploadedSize = session.FileSize
	} else if len(completedChunks) > 0 {
		// Check if we have the last chunk
		lastChunk := session.TotalChunks - 1
		hasLastChunk := false
		for _, idx := range completedChunks {
			if idx == lastChunk {
				hasLastChunk = true
				break
			}
		}
		if hasLastChunk {
			uploadedSize = int64(len(completedChunks)-1)*session.ChunkSize + (session.FileSize % session.ChunkSize)
			if session.FileSize%session.ChunkSize == 0 {
				uploadedSize = session.FileSize
			}
		}
	}

	writeJSON(w, http.StatusOK, protocol.UploadProgressResponse{
		UploadID:        session.ID,
		Filename:        session.Filename,
		FileSize:        session.FileSize,
		ChunkSize:       session.ChunkSize,
		TotalChunks:     session.TotalChunks,
		UploadedChunks:  len(completedChunks),
		UploadedSize:    uploadedSize,
		CompletedChunks: completedChunks,
		Status:          string(session.Status),
		CreatedAt:       session.CreatedAt,
		UpdatedAt:       session.UpdatedAt,
	})
}

// CompleteChunkUpload finalizes the chunked upload and assembles the file
func (h *ChunkUploadHandler) CompleteChunkUpload(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.validateAuthToken(w, r); !ok {
		writeError(w, http.StatusUnauthorized, protocol.NewProtocolError(
			protocol.ErrCodeUnauthorized, "Unauthorized: invalid or missing token", nil,
		))
		return
	}

	var req protocol.ChunkUploadCompleteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, protocol.NewProtocolError(
			protocol.ErrCodeInvalidRequest, "Invalid JSON body", err,
		))
		return
	}

	uploadID := req.UploadID

	// Serialize completions of one session inside this process: the first
	// caller assembles, the rest observe the completed session and return its
	// file id (C-41) instead of assembling a second time.
	release := h.sessionLocks.Lock(uploadID)
	defer release()

	// Get upload session
	session, err := h.db.GetUploadSession(uploadID)
	if err != nil {
		writeError(w, http.StatusNotFound, protocol.NewProtocolError(
			protocol.ErrCodeNotFound, "Upload session not found", err,
		))
		return
	}

	switch session.Status {
	case protocol.UploadSessionStatusCompleted:
		// Repeat completion after the response was lost: return the same file
		// identifier without rebuilding the file (C-41).
		if session.FileID.Valid && session.FileID.String != "" {
			writeJSON(w, http.StatusOK, protocol.ChunkUploadCompleteResponse{
				FileID:  session.FileID.String,
				Message: "File uploaded successfully",
			})
			return
		}
		// Sessions completed before the id was persisted cannot satisfy the
		// idempotency contract; rebuilding the file to derive it is forbidden.
		writeError(w, http.StatusBadRequest, protocol.NewProtocolError(
			protocol.ErrCodeInvalidRequest, "Upload session is already completed", nil,
		))
		return
	case protocol.UploadSessionStatusAssembling:
		// Reachable only when an earlier assembly died with the process: this
		// handler holds the session lock, so no live assembler exists here.
		log.Printf("Reclaiming upload session %s left in assembling state", uploadID)
		if err := h.db.ReopenUploadSession(uploadID); err != nil {
			writeError(w, http.StatusInternalServerError, protocol.NewProtocolError(
				protocol.ErrCodeInternalError, "Failed to resume upload session", err,
			))
			return
		}
	case protocol.UploadSessionStatusInProgress:
		// Claimed below.
	default:
		writeError(w, http.StatusBadRequest, protocol.NewProtocolError(
			protocol.ErrCodeInvalidRequest, "Upload session is not in progress", nil,
		))
		return
	}

	// Claim the session before reading any chunk. The CAS makes assembly
	// happen at most once even when two callers both saw the session as in
	// progress.
	claimed, err := h.db.ClaimUploadSessionAssembly(uploadID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, protocol.NewProtocolError(
			protocol.ErrCodeInternalError, "Failed to claim upload session", err,
		))
		return
	}
	if !claimed {
		current, getErr := h.db.GetUploadSession(uploadID)
		if getErr == nil && current.Status == protocol.UploadSessionStatusCompleted && current.FileID.Valid {
			writeJSON(w, http.StatusOK, protocol.ChunkUploadCompleteResponse{
				FileID:  current.FileID.String,
				Message: "File uploaded successfully",
			})
			return
		}
		writeError(w, http.StatusBadRequest, protocol.NewProtocolError(
			protocol.ErrCodeInvalidRequest, "Upload session is being assembled by another request", nil,
		))
		return
	}

	// From here the session is assembling: every failure path must put it back
	// to in progress, otherwise the chunks can never be repaired and completed.
	reopenAfterFailure := func() {
		if err := h.db.ReopenUploadSession(uploadID); err != nil {
			log.Printf("Warning: failed to reopen upload session %s after a failed completion: %v", uploadID, err)
		}
	}

	// Assemble the chunks into a temporary file. Chunks are selected by index
	// and each must carry exactly the size the session declares, so a missing
	// or truncated chunk fails here rather than yielding a short file that
	// looks like a successful upload.
	filePath, totalSize, actualChecksum, err := h.storage.AssembleChunks(
		uploadID, session.TotalChunks, session.ChunkSize, session.FileSize)
	if err != nil {
		// A full disk during assembly previously surfaced as a bare 500 with no
		// server-side trace — log the cause and classify ENOSPC as a
		// client-actionable 507, mirroring the SaveChunk path.
		log.Printf("Failed to assemble chunks for upload %s: %v", uploadID, err)
		reopenAfterFailure()
		if isChunkSetError(err) {
			writeError(w, http.StatusBadRequest, protocol.NewProtocolError(
				protocol.ErrCodeInvalidRequest, chunkAssemblyErrorMessage(err), err,
			))
			return
		}
		status, code, message := assembleErrorStatus(err)
		writeError(w, status, protocol.NewProtocolError(code, message, err))
		return
	}

	// Verify the whole-file checksum before the file becomes addressable.
	// Renaming first and deleting on mismatch cannot undo the published name
	// and leaves a final file behind (C-40).
	if mismatch := declaredChecksumMismatch(session.FileChecksum.String, req.Checksum, actualChecksum); mismatch != "" {
		os.Remove(filePath)
		reopenAfterFailure()
		writeError(w, http.StatusBadRequest, protocol.NewProtocolError(
			protocol.ErrCodeInvalidRequest, mismatch, nil,
		))
		return
	}

	// The file id is the content hash, so identical content keeps a stable
	// identifier across uploads.
	fileID := actualChecksum
	finalPath := h.storage.GetFilePath(fileID)
	published := false
	if _, statErr := os.Stat(finalPath); statErr == nil {
		// The same content is already stored: drop the duplicate temp copy.
		os.Remove(filePath)
	} else if os.IsNotExist(statErr) {
		if err := os.Rename(filePath, finalPath); err != nil {
			os.Remove(filePath)
			reopenAfterFailure()
			writeError(w, http.StatusInternalServerError, protocol.NewProtocolError(
				protocol.ErrCodeInternalError, "Failed to finalize uploaded file", err,
			))
			return
		}
		published = true
	} else {
		os.Remove(filePath)
		reopenAfterFailure()
		writeError(w, http.StatusInternalServerError, protocol.NewProtocolError(
			protocol.ErrCodeInternalError, "Failed to finalize uploaded file", statErr,
		))
		return
	}

	// Record the measured checksum: a declared one is only a claim and has
	// already been verified above when present.
	if _, err := h.db.CreateFile(session.Filename, finalPath, totalSize, &actualChecksum); err != nil {
		// Drop the blob only when this request created it; a deduplicated path
		// may already back another upload's file record.
		if published {
			h.storage.DeleteFile(fileID)
		}
		reopenAfterFailure()
		writeError(w, http.StatusInternalServerError, protocol.NewProtocolError(
			protocol.ErrCodeInternalError, "Failed to create file record", err,
		))
		return
	}

	// Persist completion together with the file id — the value a repeated
	// completion must return (C-41).
	if err := h.db.CompleteUploadSession(uploadID, fileID); err != nil {
		log.Printf("Error: failed to mark upload session %s as completed: %v", uploadID, err)
		reopenAfterFailure()
		writeError(w, http.StatusInternalServerError, protocol.NewProtocolError(
			protocol.ErrCodeInternalError, "Failed to complete upload session", err,
		))
		return
	}

	// Clean up chunks asynchronously
	h.wg.Add(1)
	go panicguard.Guard("chunk upload cleanup", func() {
		defer h.wg.Done()
		if err := h.storage.DeleteChunkDirectory(uploadID); err != nil {
			log.Printf("Warning: failed to clean up chunks for upload %s: %v", uploadID, err)
		}
		if err := h.db.DeleteUploadChunks(uploadID); err != nil {
			log.Printf("Warning: failed to delete chunk records for upload %s: %v", uploadID, err)
		}
	})

	writeJSON(w, http.StatusOK, protocol.ChunkUploadCompleteResponse{
		FileID:  fileID,
		Message: "File uploaded successfully",
	})
}

// CancelChunkUpload cancels an upload session
func (h *ChunkUploadHandler) CancelChunkUpload(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.validateAuthToken(w, r); !ok {
		writeError(w, http.StatusUnauthorized, protocol.NewProtocolError(
			protocol.ErrCodeUnauthorized, "Unauthorized: invalid or missing token", nil,
		))
		return
	}

	uploadID := chi.URLParam(r, "uploadId")

	// Get upload session
	session, err := h.db.GetUploadSession(uploadID)
	if err != nil {
		writeError(w, http.StatusNotFound, protocol.NewProtocolError(
			protocol.ErrCodeNotFound, "Upload session not found", err,
		))
		return
	}

	// Validate session status
	if session.Status != protocol.UploadSessionStatusInProgress {
		writeError(w, http.StatusBadRequest, protocol.NewProtocolError(
			protocol.ErrCodeInvalidRequest, "Upload session is not in progress", nil,
		))
		return
	}

	// Cancel session
	if err := h.db.CancelUploadSession(uploadID); err != nil {
		writeError(w, http.StatusInternalServerError, protocol.NewProtocolError(
			protocol.ErrCodeInternalError, "Failed to cancel upload session", err,
		))
		return
	}

	// Clean up chunks
	go panicguard.Guard("chunk upload cancel cleanup", func() {
		h.storage.DeleteChunkDirectory(uploadID)
		h.db.DeleteUploadChunks(uploadID)
	})

	writeJSON(w, http.StatusOK, map[string]string{
		"message": "Upload session cancelled",
	})
}

// cleanupExpiredSessions periodically cleans up expired upload sessions
func (h *ChunkUploadHandler) cleanupExpiredSessions() {
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()

	for {
		select {
		case <-h.done:
			return
		case <-ticker.C:
			h.cleanupExpiredSessionsOnce()
		}
	}
}

// cleanupExpiredSessionsOnce expires overdue upload sessions and removes their
// on-disk chunk directories and DB rows. Deleting the chunk directory is the
// load-bearing part: expiring a session only flips its status, so an abandoned
// or failed upload would otherwise leave chunks/<uuid>/ on disk until the
// data-dir fills up. Removing the session row (which cascades to its chunk
// records) keeps the next sweep from re-visiting already-cleaned sessions.
func (h *ChunkUploadHandler) cleanupExpiredSessionsOnce() {
	if _, err := h.db.ExpireUploadSessions(); err != nil {
		log.Printf("Warning: failed to expire upload sessions: %v", err)
		return
	}

	uploadIDs, err := h.db.GetExpiredUploadSessionIDs()
	if err != nil {
		log.Printf("Warning: failed to list expired upload sessions: %v", err)
		return
	}

	for _, uploadID := range uploadIDs {
		if err := h.storage.DeleteChunkDirectory(uploadID); err != nil {
			log.Printf("Warning: failed to clean up chunks for upload %s: %v", uploadID, err)
			continue
		}
		if err := h.db.DeleteUploadSession(uploadID); err != nil {
			log.Printf("Warning: failed to delete expired upload session %s: %v", uploadID, err)
		}
	}
}

// UploadFileFromReader is a helper for uploading a file from an io.Reader (for testing)
func (h *ChunkUploadHandler) UploadFileFromReader(filename string, fileSize int64, reader io.Reader) (string, error) {
	// Initialize session
	totalChunks := int(fileSize / h.chunkSize)
	if fileSize%h.chunkSize > 0 {
		totalChunks++
	}

	session, err := h.db.CreateUploadSession(filename, fileSize, h.chunkSize, totalChunks, nil)
	if err != nil {
		return "", err
	}

	claimed, err := h.db.ClaimUploadSessionAssembly(session.ID)
	if err != nil {
		return "", err
	}
	if !claimed {
		return "", fmt.Errorf("upload session %s could not be claimed", session.ID)
	}

	// Upload chunks
	for i := range totalChunks {
		expected, err := expectedChunkSize(session, i)
		if err != nil {
			return "", err
		}
		chunkPath, size, _, err := h.storage.SaveChunk(session.ID, i, io.LimitReader(reader, expected))
		if err != nil {
			return "", err
		}

		_, err = h.db.CreateUploadChunk(session.ID, i, size, "", chunkPath)
		if err != nil {
			return "", err
		}

		h.db.UpdateUploadSessionChunks(session.ID, i)
	}

	// Assemble and publish under the content hash, like CompleteChunkUpload.
	tmpPath, totalSize, checksum, err := h.storage.AssembleChunks(session.ID, totalChunks, h.chunkSize, fileSize)
	if err != nil {
		return "", err
	}
	finalPath := h.storage.GetFilePath(checksum)
	if err := os.Rename(tmpPath, finalPath); err != nil {
		os.Remove(tmpPath)
		return "", err
	}

	if _, err = h.db.CreateFile(filename, finalPath, totalSize, &checksum); err != nil {
		return "", err
	}

	if err := h.db.CompleteUploadSession(session.ID, checksum); err != nil {
		return "", err
	}

	go panicguard.Guard("chunk upload assembly cleanup", func() {
		h.storage.DeleteChunkDirectory(session.ID)
	})

	return checksum, nil
}

// ResumeChunkUpload allows resuming an interrupted upload
func (h *ChunkUploadHandler) ResumeChunkUpload(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.validateAuthToken(w, r); !ok {
		writeError(w, http.StatusUnauthorized, protocol.NewProtocolError(
			protocol.ErrCodeUnauthorized, "Unauthorized: invalid or missing token", nil,
		))
		return
	}

	uploadID := chi.URLParam(r, "uploadId")

	session, err := h.db.GetUploadSession(uploadID)
	if err != nil {
		writeError(w, http.StatusNotFound, protocol.NewProtocolError(
			protocol.ErrCodeNotFound, "Upload session not found", err,
		))
		return
	}

	// Return current progress
	var completedChunks []int
	if err := json.Unmarshal([]byte(session.UploadedChunks), &completedChunks); err != nil {
		completedChunks = []int{}
	}

	// Calculate missing chunks
	missingChunks := make([]int, 0)
	for i := 0; i < session.TotalChunks; i++ {
		found := false
		for _, completed := range completedChunks {
			if completed == i {
				found = true
				break
			}
		}
		if !found {
			missingChunks = append(missingChunks, i)
		}
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"upload_id":        session.ID,
		"filename":         session.Filename,
		"chunk_size":       session.ChunkSize,
		"total_chunks":     session.TotalChunks,
		"completed_chunks": completedChunks,
		"missing_chunks":   missingChunks,
		"status":           session.Status,
	})
}
