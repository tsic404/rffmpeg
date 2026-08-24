package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/tsix404/rffmpeg/pkg/protocol"
	"github.com/tsix404/rffmpeg/pkg/server/auth"
	"github.com/tsix404/rffmpeg/pkg/server/db"
	"github.com/tsix404/rffmpeg/pkg/server/storage"
)

// MaxChunkSize caps the per-chunk size a client may request. Larger values
// are clamped server-side to prevent a single init request from forcing
// huge multipart memory buffers.
const MaxChunkSize = 64 * 1024 * 1024

// Default chunk size: 10MB
const DefaultChunkSize = 10 * 1024 * 1024

// Max concurrent chunk uploads
const MaxConcurrentChunkUploads = 4

// ChunkUploadHandler handles chunked file uploads
type ChunkUploadHandler struct {
	db          *db.Database
	storage     *storage.Storage
	chunkSize   int64
	authToken   string        // Non-empty when auth is configured
	semaphore   chan struct{} // For concurrent upload control
	cleanupOnce sync.Once
	wg          sync.WaitGroup // Tracks async cleanup goroutines
	done        chan struct{}  // Signals shutdown to background goroutines
}

// NewChunkUploadHandler creates a new chunk upload handler
func NewChunkUploadHandler(database *db.Database, store *storage.Storage, chunkSize int64) *ChunkUploadHandler {
	if chunkSize <= 0 {
		chunkSize = DefaultChunkSize
	}

	h := &ChunkUploadHandler{
		db:        database,
		storage:   store,
		chunkSize: chunkSize,
		semaphore: make(chan struct{}, MaxConcurrentChunkUploads),
		done:      make(chan struct{}),
	}

	// Start cleanup routine
	go h.cleanupExpiredSessions()

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

	// Check if chunk already exists (idempotent)
	if h.storage.ChunkExists(uploadID, chunkIndex) {
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
			Message:    "Chunk already uploaded",
		})
		return
	}

	// Acquire semaphore for concurrent upload control
	h.semaphore <- struct{}{}
	defer func() { <-h.semaphore }()

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
		writeError(w, http.StatusInternalServerError, protocol.NewProtocolError(
			protocol.ErrCodeUploadFailed, "Failed to save chunk", err,
		))
		return
	}

	// Verify checksum if provided
	if expectedChecksum != "" && expectedChecksum != actualChecksum {
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

	// Check all chunks are uploaded — use both the session JSON array
	// and the authoritative upload_chunks table to guard against race
	// conditions in UpdateUploadSessionChunks.
	var completedChunks []int
	if err := json.Unmarshal([]byte(session.UploadedChunks), &completedChunks); err != nil {
		writeError(w, http.StatusInternalServerError, protocol.NewProtocolError(
			protocol.ErrCodeInternalError, "Failed to parse uploaded chunks", err,
		))
		return
	}

	// Also count chunks from the upload_chunks table (authoritative source).
	dbChunks, dbErr := h.db.GetUploadChunks(uploadID)
	dbChunkCount := len(completedChunks) // fallback
	if dbErr == nil {
		dbChunkCount = len(dbChunks)
	}

	if len(completedChunks) != session.TotalChunks && dbChunkCount != session.TotalChunks {
		writeError(w, http.StatusBadRequest, protocol.NewProtocolError(
			protocol.ErrCodeInvalidRequest,
			fmt.Sprintf("Not all chunks uploaded: have %d (session) / %d (db) of %d total chunks",
				len(completedChunks), dbChunkCount, session.TotalChunks), nil,
		))
		return
	}

	// If session tracking is missing some chunks but DB has them all,
	// heal the session tracking before proceeding.
	if len(completedChunks) != session.TotalChunks && dbChunkCount == session.TotalChunks {
		log.Printf("Healing upload session %s: session tracks %d chunks but DB has %d, repairing...",
			uploadID, len(completedChunks), dbChunkCount)
		for _, chunk := range dbChunks {
			_ = h.db.UpdateUploadSessionChunks(uploadID, chunk.ChunkIndex)
		}
	}

	// Get all chunk paths
	chunkPaths, err := h.storage.GetChunkPaths(uploadID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, protocol.NewProtocolError(
			protocol.ErrCodeInternalError, "Failed to get chunk paths", err,
		))
		return
	}

	// Assemble chunks to a temp file first, then rename to content-hash-based path.
	// This ensures the same file content always produces the same file ID,
	// enabling cache key stability across uploads.
	tempFileID := uuid.New().String()
	filePath, totalSize, actualChecksum, err := h.storage.AssembleChunks(uploadID, tempFileID, chunkPaths)
	if err != nil {
		writeError(w, http.StatusInternalServerError, protocol.NewProtocolError(
			protocol.ErrCodeUploadFailed, "Failed to assemble file", err,
		))
		return
	}

	// Rename assembled file to content-hash-based file ID for cache key stability.
	// Only "already exists" is benign (same content uploaded twice): any other
	// error must fail the request — deleting the temp file and writing a DB
	// record pointing at a nonexistent path would corrupt the catalog.
	fileID := actualChecksum
	finalPath := h.storage.GetFilePath(fileID)
	if err := os.Rename(filePath, finalPath); err != nil {
		if os.IsExist(err) {
			// Destination already exists with identical content — drop the temp copy.
			h.storage.DeleteFile(tempFileID)
		} else {
			h.storage.DeleteFile(tempFileID)
			writeError(w, http.StatusInternalServerError, protocol.NewProtocolError(
				protocol.ErrCodeInternalError, "Failed to finalize uploaded file", err,
			))
			return
		}
	} else {
		filePath = finalPath
	}

	// Also check the checksum from the request
	if req.Checksum != "" && req.Checksum != actualChecksum {
		h.storage.DeleteFile(tempFileID)
		writeError(w, http.StatusBadRequest, protocol.NewProtocolError(
			protocol.ErrCodeInvalidRequest, "Final file checksum mismatch", nil,
		))
		return
	}

	// Create file record in database
	var checksum *string
	if session.FileChecksum.Valid {
		checksum = &session.FileChecksum.String
	} else {
		checksum = &actualChecksum
	}

	_, err = h.db.CreateFile(session.Filename, filePath, totalSize, checksum)
	if err != nil {
		h.storage.DeleteFile(fileID)
		writeError(w, http.StatusInternalServerError, protocol.NewProtocolError(
			protocol.ErrCodeInternalError, "Failed to create file record", err,
		))
		return
	}

	// Mark session as completed
	if err := h.db.CompleteUploadSession(uploadID); err != nil {
		log.Printf("Warning: failed to mark upload session as completed: %v", err)
	}

	// Clean up chunks asynchronously
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		if err := h.storage.DeleteChunkDirectory(uploadID); err != nil {
			log.Printf("Warning: failed to clean up chunks for upload %s: %v", uploadID, err)
		}
		if err := h.db.DeleteUploadChunks(uploadID); err != nil {
			log.Printf("Warning: failed to delete chunk records for upload %s: %v", uploadID, err)
		}
	}()

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
	go func() {
		h.storage.DeleteChunkDirectory(uploadID)
		h.db.DeleteUploadChunks(uploadID)
	}()

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
			// Expire sessions in database
			_, err := h.db.ExpireUploadSessions()
			if err != nil {
				log.Printf("Warning: failed to expire upload sessions: %v", err)
			}

			// Clean up chunk files for expired sessions
			// This is a simplified cleanup - in production, you'd query for expired sessions
			// and clean up their chunks specifically
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

	// Upload chunks
	for i := 0; i < totalChunks; i++ {
		chunkPath, size, _, err := h.storage.SaveChunk(session.ID, i, io.LimitReader(reader, h.chunkSize))
		if err != nil {
			return "", err
		}

		_, err = h.db.CreateUploadChunk(session.ID, i, size, "", chunkPath)
		if err != nil {
			return "", err
		}

		h.db.UpdateUploadSessionChunks(session.ID, i)
	}

	// Assemble
	fileID := uuid.New().String()
	chunkPaths, _ := h.storage.GetChunkPaths(session.ID)
	filePath, totalSize, _, err := h.storage.AssembleChunks(session.ID, fileID, chunkPaths)
	if err != nil {
		return "", err
	}

	_, err = h.db.CreateFile(filename, filePath, totalSize, nil)
	if err != nil {
		return "", err
	}

	h.db.CompleteUploadSession(session.ID)
	go h.storage.DeleteChunkDirectory(session.ID)

	return fileID, nil
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
