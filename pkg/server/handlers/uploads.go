package handlers

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"

	"github.com/google/uuid"

	"github.com/go-chi/chi/v5"
	"github.com/tsic404/rffmpeg/pkg/protocol"
	"github.com/tsic404/rffmpeg/pkg/server/db"
	"github.com/tsic404/rffmpeg/pkg/server/storage"
)

// allowedFileTypes defines allowed file extensions for upload
var allowedFileTypes = map[string]bool{

	".mp4":  true,
	".mkv":  true,
	".avi":  true,
	".mov":  true,
	".wmv":  true,
	".flv":  true,
	".webm": true,
	".m4v":  true,
	".mpeg": true,
	".mpg":  true,

	".mp3":  true,
	".wav":  true,
	".flac": true,
	".aac":  true,
	".ogg":  true,
	".m4a":  true,
	".wma":  true,

	".jpg":  true,
	".jpeg": true,
	".png":  true,
	".gif":  true,
	".bmp":  true,
	".webp": true,
	".tiff": true,
	".tif":  true,

	".srt": true,
	".ass": true,
	".vtt": true,

	".ts":   true,
	".m2ts": true,
	".mts":  true,
}

// validateFileType checks if the file extension is allowed
func validateFileType(filename string) bool {
	ext := ""
	for i := len(filename) - 1; i >= 0; i-- {
		if filename[i] == '.' {
			ext = filename[i:]
			break
		}
	}
	if ext == "" {
		return false
	}

	lowerExt := make([]byte, len(ext))
	for i, c := range []byte(ext) {
		if c >= 'A' && c <= 'Z' {
			lowerExt[i] = c + 32
		} else {
			lowerExt[i] = c
		}
	}
	return allowedFileTypes[string(lowerExt)]
}

// Upload handles file upload
func (h *Handler) Upload(w http.ResponseWriter, r *http.Request) {

	if _, ok := h.validateAuthToken(w, r); !ok {
		writeError(w, http.StatusUnauthorized, protocol.NewProtocolError(
			protocol.ErrCodeUnauthorized, "Unauthorized: invalid or missing token", nil,
		))
		return
	}

	maxSize := int64(10 * 1024 * 1024 * 1024)
	r.Body = http.MaxBytesReader(w, r.Body, maxSize)

	if !h.ensureMultipartSpace(w, r.ContentLength) {
		return
	}
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeMultipartParseError(w, err)
		return
	}

	defer r.MultipartForm.RemoveAll()

	file, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, protocol.NewProtocolError(
			protocol.ErrCodeInvalidRequest, "Missing file in request", err,
		))
		return
	}
	defer file.Close()

	if !validateFileType(header.Filename) {
		writeError(w, http.StatusBadRequest, protocol.NewProtocolError(
			protocol.ErrCodeInvalidRequest, "Invalid file type. Allowed types: video, audio, image, subtitle formats", nil,
		))
		return
	}

	path, size, actualChecksum, err := h.storage.SaveFileByContent(file)
	if err != nil {
		writeError(w, http.StatusInternalServerError, protocol.NewProtocolError(
			protocol.ErrCodeUploadFailed, "Failed to save file", err,
		))
		return
	}
	fileID := actualChecksum

	// Handle optional checksum
	var checksum *string
	if cs := r.FormValue("checksum"); cs != "" {
		checksum = &cs
	} else {
		checksum = &actualChecksum
	}

	_, err = h.db.CreateFile(header.Filename, path, size, checksum)
	if err != nil {
		writeError(w, http.StatusInternalServerError, protocol.NewProtocolError(
			protocol.ErrCodeInternalError, "Failed to create file record", err,
		))
		return
	}

	writeJSON(w, http.StatusOK, protocol.UploadResponse{
		FileID:  fileID,
		Message: "File uploaded successfully",
	})
}

// UploadJobOutput handles output file upload from worker. The reporting
// worker identifies itself via the worker_id query parameter; an upload from a
// worker that no longer owns an active job is refused (409) before the body is
// read, so a stale worker cannot overwrite the new owner's product.
func (h *Handler) UploadJobOutput(w http.ResponseWriter, r *http.Request) {
	jobID := chi.URLParam(r, "jobId")

	workerID := db.NormalizeWorkerID(r.URL.Query().Get("worker_id"))
	if workerID == "" {
		writeError(w, http.StatusBadRequest, protocol.NewProtocolError(
			protocol.ErrCodeInvalidRequest, "worker_id is required", nil,
		))
		return
	}

	job, err := h.db.GetJob(jobID)
	if err != nil {
		if errors.Is(err, protocol.ErrJobNotFound) {
			writeError(w, http.StatusNotFound, protocol.NewProtocolError(
				protocol.ErrCodeNotFound, "Job not found", err,
			))
			return
		}
		writeError(w, http.StatusInternalServerError, protocol.NewProtocolError(
			protocol.ErrCodeInternalError, "Failed to get job", err,
		))
		return
	}
	owns := job.WorkerID.Valid &&
		db.NormalizeWorkerID(job.WorkerID.String) == workerID &&
		(job.Status == protocol.JobStatusRunning || job.Status == protocol.JobStatusQueued)
	if !owns {
		writeError(w, http.StatusConflict, protocol.NewProtocolError(
			protocol.ErrCodeConflict, "Job no longer assigned to this worker", protocol.ErrJobNotOwned,
		))
		return
	}

	maxSize := int64(10 * 1024 * 1024 * 1024)

	r.Body = http.MaxBytesReader(w, r.Body, maxSize)

	if !h.ensureMultipartSpace(w, r.ContentLength) {
		return
	}
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		log.Printf("UploadJobOutput: failed to parse multipart form for job %s: %v", jobID, err)
		writeMultipartParseError(w, err)
		return
	}

	defer r.MultipartForm.RemoveAll()

	file, _, err := r.FormFile("file")
	if err != nil {
		log.Printf("UploadJobOutput: failed to get file for job %s: %v", jobID, err)
		writeError(w, http.StatusBadRequest, protocol.NewProtocolError(
			protocol.ErrCodeInvalidRequest, "Missing file in request", err,
		))
		return
	}
	defer file.Close()

	fileID := uuid.New().String()
	_, _, err = h.storage.SaveOutput(jobID, fileID, file)
	if err != nil {
		writeError(w, http.StatusInternalServerError, protocol.NewProtocolError(
			protocol.ErrCodeUploadFailed, "Failed to save output file", err,
		))
		return
	}

	outputFiles := []string{fileID}
	outputFilesJSON, err := json.Marshal(outputFiles)
	if err != nil {
		writeError(w, http.StatusInternalServerError, protocol.NewProtocolError(
			protocol.ErrCodeInternalError, "Failed to marshal output files", err,
		))
		return
	}
	if err := h.db.UpdateJobOutputWithOwner(jobID, workerID, string(outputFilesJSON)); err != nil {
		if errors.Is(err, protocol.ErrJobNotOwned) {

			if rmErr := os.Remove(h.storage.GetOutputPath(jobID, fileID)); rmErr != nil && !os.IsNotExist(rmErr) {
				log.Printf("UploadJobOutput: failed to remove stale output for job %s: %v", jobID, rmErr)
			}
			writeError(w, http.StatusConflict, protocol.NewProtocolError(
				protocol.ErrCodeConflict, "Job no longer assigned to this worker", err,
			))
			return
		}
		writeError(w, http.StatusInternalServerError, protocol.NewProtocolError(
			protocol.ErrCodeInternalError, "Failed to update job output", err,
		))
		return
	}

	writeJSON(w, http.StatusOK, protocol.JobOutputUploadResponse{
		Message: "Output uploaded successfully",
	})
}

// DownloadOutput handles output file download
func (h *Handler) DownloadOutput(w http.ResponseWriter, r *http.Request) {
	fileID := chi.URLParam(r, "fileId")
	if !storage.ValidateOutputFileID(fileID) {
		writeError(w, http.StatusBadRequest, protocol.NewProtocolError(
			protocol.ErrCodeInvalidRequest, "Invalid file ID format", nil,
		))
		return
	}
	file, err := h.storage.OpenOutput(fileID)
	if err != nil {
		writeError(w, http.StatusNotFound, protocol.NewProtocolError(
			protocol.ErrCodeNotFound, "Output file not found", err,
		))
		return
	}
	defer file.Close()

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", "attachment; filename="+fileID)
	io.Copy(w, file)
}

// DownloadFile handles input file download
func (h *Handler) DownloadFile(w http.ResponseWriter, r *http.Request) {
	fileID := chi.URLParam(r, "fileId")
	if !storage.ValidateFileID(fileID) {
		writeError(w, http.StatusBadRequest, protocol.NewProtocolError(
			protocol.ErrCodeInvalidRequest, "Invalid file ID format", nil,
		))
		return
	}
	file, err := h.storage.OpenFile(fileID)
	if err != nil {
		writeError(w, http.StatusNotFound, protocol.NewProtocolError(
			protocol.ErrCodeNotFound, "File not found", err,
		))
		return
	}
	defer file.Close()

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", "attachment; filename="+fileID)
	io.Copy(w, file)
}
