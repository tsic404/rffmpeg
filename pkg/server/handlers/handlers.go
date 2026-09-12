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
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/tsic404/rffmpeg/pkg/pathutil"
	"github.com/tsic404/rffmpeg/pkg/protocol"
	"github.com/tsic404/rffmpeg/pkg/server/auth"
	"github.com/tsic404/rffmpeg/pkg/server/db"
	"github.com/tsic404/rffmpeg/pkg/server/migration"
	"github.com/tsic404/rffmpeg/pkg/server/ratelimit"
	"github.com/tsic404/rffmpeg/pkg/server/scheduler"
	"github.com/tsic404/rffmpeg/pkg/server/storage"
	"github.com/tsic404/rffmpeg/pkg/server/websocket"
	"github.com/tsic404/rffmpeg/pkg/server/workerhealth"
)

// Handler holds dependencies for HTTP handlers
type Handler struct {
	db          *db.Database
	storage     *storage.Storage
	version     string
	wsHub       *websocket.Hub
	rateLimiter ratelimit.ClientJobCounter
	stateTable  *workerhealth.WorkerStateTable
	scheduler   *scheduler.Scheduler // TSI-1501: scheduler reference for immediate job assignment
	authToken   string               // Non-empty when auth is configured
	// heartbeatTimeout mirrors the worker health monitor's knob: submit-time
	// fail-fast treats workers whose last heartbeat is older than this as dead
	// (TSI-2419). <=0 disables the freshness check.
	heartbeatTimeout time.Duration
	// noWorkerJobTimeout and timeoutCheckInterval mirror the scheduler's
	// starvation knobs so GetJob/PullWorkerJobs can attach NoWorkerDeadline
	// to unassigned pending jobs only when the cluster has no live
	// schedulable worker (the exact filter the sweep fails). <=0
	// noWorkerJobTimeout disables the deadline.
	noWorkerJobTimeout   time.Duration
	timeoutCheckInterval time.Duration
}

// New creates a new Handler
func New(database *db.Database, store *storage.Storage, version string, stateTable *workerhealth.WorkerStateTable) *Handler {
	return &Handler{
		db:          database,
		storage:     store,
		version:     version,
		wsHub:       websocket.NewHubWithSeqStore(database),
		rateLimiter: ratelimit.NewInMemoryCounter(),
		stateTable:  stateTable,
	}
}

// NewWithHub creates a new Handler with an existing WebSocket hub
func NewWithHub(database *db.Database, store *storage.Storage, version string, hub *websocket.Hub, stateTable *workerhealth.WorkerStateTable) *Handler {
	return &Handler{
		db:          database,
		storage:     store,
		version:     version,
		wsHub:       hub,
		rateLimiter: ratelimit.NewInMemoryCounter(),
		stateTable:  stateTable,
	}
}

// SetRateLimiter sets the rate limiter counter (used when a custom counter is needed).
func (h *Handler) SetRateLimiter(counter ratelimit.ClientJobCounter) {
	h.rateLimiter = counter
}

// GetRateLimiter returns the rate limiter counter.
func (h *Handler) GetRateLimiter() ratelimit.ClientJobCounter {
	return h.rateLimiter
}

// SetStarvationConfig mirrors the scheduler's no-worker starvation knobs so
// job responses can carry NoWorkerDeadline without duplicating config
// derivation. noWorkerJobTimeout <= 0 disables the deadline (matching the
// scheduler's noWorkerEnabled gate).
func (h *Handler) SetStarvationConfig(noWorkerJobTimeout, timeoutCheckInterval time.Duration) {
	h.noWorkerJobTimeout = noWorkerJobTimeout
	h.timeoutCheckInterval = timeoutCheckInterval
}

// GetWSHub returns the WebSocket hub
func (h *Handler) GetWSHub() *websocket.Hub {
	return h.wsHub
}

// GetStateTable returns the worker state table (TSI-759)
func (h *Handler) GetStateTable() *workerhealth.WorkerStateTable {
	return h.stateTable
}

// workerStateMap returns the worker state table contents keyed by worker ID.
// Returns nil when no state table is configured.
func (h *Handler) workerStateMap() map[string]*protocol.WorkerState {
	if h.stateTable == nil {
		return nil
	}
	states := make(map[string]*protocol.WorkerState)
	for _, s := range h.stateTable.GetAll() {
		state := s // copy for stable pointer
		states[s.WorkerID] = &state
	}
	return states
}

// SetScheduler sets the scheduler reference (TSI-1501)
func (h *Handler) SetScheduler(sched *scheduler.Scheduler) {
	h.scheduler = sched
}

// SetHeartbeatTimeout configures the submit-time worker freshness window
// (TSI-2419). Should match ServerConfig.WorkerHeartbeatTimeout.
func (h *Handler) SetHeartbeatTimeout(d time.Duration) {
	h.heartbeatTimeout = d
}

// SetAuthToken sets the auth token for health check reporting
func (h *Handler) SetAuthToken(token string) {
	h.authToken = token
}

// GetDB returns the underlying database (used by tests to seed worker state).
func (h *Handler) GetDB() *db.Database {
	return h.db
}

// validateAuthToken validates the Authorization header against the configured auth token.
// Delegates to auth.ValidateBearer — the single shared Bearer implementation.
// Returns the client ID and true if valid. When no auth token is configured
// the request is rejected: the API must fail closed rather than serve
// unauthenticated traffic.
func (h *Handler) validateAuthToken(w http.ResponseWriter, r *http.Request) (string, bool) {
	clientID, ok := auth.ValidateBearer(w, r, h.authToken)
	if !ok {
		return "", false
	}

	// Also set client ID in request context for downstream handlers
	ctx := context.WithValue(r.Context(), auth.ClientIDKey, clientID)
	*r = *r.WithContext(ctx)

	return clientID, true
}

// StartWSHub starts the WebSocket hub
func (h *Handler) StartWSHub() {
	go h.wsHub.Run()
}

// writeJSON writes JSON response
func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// writeError writes error response
func writeError(w http.ResponseWriter, status int, err *protocol.ProtocolError) {
	writeJSON(w, status, err.ToResponse())
}

// NotFound responds with the protocol-consistent JSON 404 used across the API.
// It is wired into the chi router as r.NotFound so unmatched paths (e.g. a
// trailing empty segment like /api/v1/migrations/) return the same
// {"code":"not_found"} structure as resource-specific 404s such as
// GetMigrationEvent's.
func NotFound(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotFound, protocol.NewProtocolError(
		protocol.ErrCodeNotFound, "Not found", nil,
	))
}

// allowedFileTypes defines allowed file extensions for upload
var allowedFileTypes = map[string]bool{
	// Video formats
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
	// Audio formats
	".mp3":  true,
	".wav":  true,
	".flac": true,
	".aac":  true,
	".ogg":  true,
	".m4a":  true,
	".wma":  true,
	// Image formats
	".jpg":  true,
	".jpeg": true,
	".png":  true,
	".gif":  true,
	".bmp":  true,
	".webp": true,
	".tiff": true,
	".tif":  true,
	// Subtitle formats
	".srt": true,
	".ass": true,
	".vtt": true,
	// Container formats
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
	// Convert to lowercase for comparison
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
	// Validate auth token (defense-in-depth; middleware also validates,
	// but handler-level check ensures protection even if middleware is bypassed)
	if _, ok := h.validateAuthToken(w, r); !ok {
		writeError(w, http.StatusUnauthorized, protocol.NewProtocolError(
			protocol.ErrCodeUnauthorized, "Unauthorized: invalid or missing token", nil,
		))
		return
	}

	maxSize := int64(10 * 1024 * 1024 * 1024) // 10GB max
	r.Body = http.MaxBytesReader(w, r.Body, maxSize)

	// 32MB in-memory threshold for multipart parsing. Larger parts spill to
	// temporary files on disk. The old 256MB threshold let a handful of
	// concurrent uploads pin hundreds of MB of RSS (TSI-2365).
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeError(w, http.StatusBadRequest, protocol.NewProtocolError(
			protocol.ErrCodeInvalidRequest, "Failed to parse multipart form", err,
		))
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, protocol.NewProtocolError(
			protocol.ErrCodeInvalidRequest, "Missing file in request", err,
		))
		return
	}
	defer file.Close()

	// Validate file type
	if !validateFileType(header.Filename) {
		writeError(w, http.StatusBadRequest, protocol.NewProtocolError(
			protocol.ErrCodeInvalidRequest, "Invalid file type. Allowed types: video, audio, image, subtitle formats", nil,
		))
		return
	}

	// Use content SHA256 hash as file ID for cache key stability.
	// This ensures the same file content always produces the same file ID,
	// enabling cache hits across re-uploads of the same file.
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

	// Save file metadata (uses content hash as file ID)
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

// SubmitJob handles job submission
func (h *Handler) SubmitJob(w http.ResponseWriter, r *http.Request) {
	// Validate auth token (defense-in-depth; middleware also validates,
	// but handler-level check ensures protection even if middleware is bypassed)
	if _, ok := h.validateAuthToken(w, r); !ok {
		writeError(w, http.StatusUnauthorized, protocol.NewProtocolError(
			protocol.ErrCodeUnauthorized, "Unauthorized: invalid or missing token", nil,
		))
		return
	}

	var req protocol.JobSubmitRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, protocol.NewProtocolError(
			protocol.ErrCodeInvalidRequest, "Invalid JSON body", err,
		))
		return
	}

	if len(req.InputFiles) == 0 || len(req.Args) == 0 {
		writeError(w, http.StatusBadRequest, protocol.NewProtocolError(
			protocol.ErrCodeInvalidRequest, "input_files and args are required", nil,
		))
		return
	}

	// Reject direct-path traversal server-side (fail-fast), so a bare API call
	// carrying a ".." component is refused here instead of being dispatched
	// and only rejected by the worker at runtime (TSI-2718). The shared
	// pathutil.ContainsPathTraversal keeps this symmetric with the worker.
	for _, path := range req.DirectPath {
		if pathutil.ContainsPathTraversal(path) {
			writeError(w, http.StatusBadRequest, protocol.NewProtocolError(
				protocol.ErrCodeInvalidRequest,
				"direct path contains '..' traversal: "+path, nil,
			))
			return
		}
	}

	// Reject output-filename traversal server-side, symmetric with the worker's
	// directMode output guard (TSI-2721). The worker only stats the output path
	// in direct mode and passes "-" and remote URLs through unchanged, so those
	// are exempt here too — using the same shared pathutil.IsRemoteURL predicate
	// (anchored scheme + "file" exclusion) so the two layers never drift; a ".."
	// component in a direct-mode local output is refused before dispatch instead
	// of by the worker at runtime.
	if len(req.DirectPath) > 0 && req.OutputFilename != "-" && !pathutil.IsRemoteURL(req.OutputFilename) {
		if pathutil.ContainsPathTraversal(req.OutputFilename) {
			writeError(w, http.StatusBadRequest, protocol.NewProtocolError(
				protocol.ErrCodeInvalidRequest,
				"output filename contains '..' traversal: "+req.OutputFilename, nil,
			))
			return
		}
	}

	// Validate input files exist (skip for direct path mode and remote URLs)
	if len(req.DirectPath) == 0 {
		for _, fileID := range req.InputFiles {
			// Skip file existence check for remote URLs (http://, https://, s3://, etc.)
			if strings.Contains(fileID, "://") {
				continue
			}
			if !h.storage.FileExists(fileID) {
				writeError(w, http.StatusBadRequest, protocol.NewProtocolError(
					protocol.ErrCodeNotFound, "Input file not found: "+fileID, nil,
				))
				return
			}
		}
	}

	// Check if there are any available workers before creating the job (TSI-1428)
	// This provides fast failure when no workers are available instead of waiting 30s+ timeout
	argsJSON, err := json.Marshal(req.Args)
	if err != nil {
		writeError(w, http.StatusInternalServerError, protocol.NewProtocolError(
			protocol.ErrCodeInternalError, "Failed to marshal args", err,
		))
		return
	}

	// Marshal input files and direct paths to JSON up front: the
	// ENCODER_UNAVAILABLE rejection path below also persists the job, so these
	// must be available before the encoder-capability check (TSI-2846).
	inputFilesJSON, err := json.Marshal(req.InputFiles)
	if err != nil {
		writeError(w, http.StatusInternalServerError, protocol.NewProtocolError(
			protocol.ErrCodeInternalError, "Failed to marshal input files", err,
		))
		return
	}
	directPathsJSON := "[]"
	if len(req.DirectPath) > 0 {
		dp, err := json.Marshal(req.DirectPath)
		if err != nil {
			writeError(w, http.StatusInternalServerError, protocol.NewProtocolError(
				protocol.ErrCodeInternalError, "Failed to marshal direct path", err,
			))
			return
		}
		directPathsJSON = string(dp)
	}

	requestedEncoder := scheduler.ExtractEncoderFromArgs(string(argsJSON))

	// Check for available workers with the requested encoder capability
	// TSI-1500: Check for available workers with compatible encoder capability
	// Instead of exact match, check if any worker has an encoder in the same codec family
	if requestedEncoder != "" {
		// First try exact match
		workersWithEncoder, err := h.db.GetLiveSchedulableWorkersByEncoder(requestedEncoder, h.heartbeatTimeout)
		if err != nil {
			log.Printf("SubmitJob: Failed to check workers by encoder %s: %v", requestedEncoder, err)
			// Fall through to check compatible encoders
		} else if len(workersWithEncoder) > 0 {
			// Exact match found, continue with job creation
			goto jobCreate
		}

		// No exact match, try compatible encoders in the same codec family
		compatibleEncoders := scheduler.GetCompatibleEncoders(requestedEncoder)
		hasCompatibleWorker := false
		for _, enc := range compatibleEncoders {
			if enc == requestedEncoder {
				continue // Skip the requested encoder (already checked)
			}
			workers, err := h.db.GetLiveSchedulableWorkersByEncoder(enc, h.heartbeatTimeout)
			if err != nil {
				log.Printf("SubmitJob: Failed to check workers for compatible encoder %s: %v", enc, err)
				continue
			}
			if len(workers) > 0 {
				hasCompatibleWorker = true
				log.Printf("SubmitJob: No worker with %s, but found compatible encoder %s", requestedEncoder, enc)
				break
			}
		}

		if !hasCompatibleWorker {
			if !h.failAsEncoderUnavailable(requestedEncoder, compatibleEncoders) {
				// No live worker has the encoder (or a compatible one), but the
				// cluster either has no schedulable worker at all, or a worker
				// has the encoder but is merely stale — keep the fail-fast 503
				// (worker_unavailable) rather than misclassifying it as
				// ENCODER_UNAVAILABLE (TSI-2419). This is the worker_offline
				// sub-case: use the user guidance copy, not the
				// encoder-specific copy reserved for encoder_match_failed.
				writeError(w, http.StatusServiceUnavailable, protocol.NewProtocolError(
					protocol.ErrCodeWorkerUnavailable,
					"No worker available. Please ensure at least one worker is registered and online.",
					nil,
				))
				return
			}

			// TSI-2846: workers exist but none has the requested encoder or a
			// compatible one. Persist the job directly in the failed state
			// (atomic single INSERT, symmetric with INPUT_UNREACHABLE /
			// NO_WORKER_AVAILABLE) so the classification is recorded and
			// observable instead of vanishing with no DB row.
			errMsg := fmt.Sprintf("No worker available with encoder: %s (or compatible encoders)", requestedEncoder)
			job, createErr := h.db.CreateFailedJob(string(inputFilesJSON), string(argsJSON), req.OutputFilename, req.AutoHW, req.StreamingOutput, req.Timeout, directPathsJSON, string(protocol.FailureEncoderUnavailable), errMsg)
			if createErr != nil {
				// The rate-limit middleware auto-rolls-back the submit increment
				// on non-2xx, so no explicit decrement is needed on this path.
				writeError(w, http.StatusInternalServerError, protocol.NewProtocolError(
					protocol.ErrCodeInternalError, "Failed to create job", createErr,
				))
				return
			}
			// The job is terminal at creation; release the submit-time
			// rate-limit increment (the middleware only rolls back non-2xx).
			if clientID := auth.GetClientID(r); clientID != "" {
				h.rateLimiter.Decrement(clientID)
			}
			writeJSON(w, http.StatusOK, protocol.JobSubmitResponse{
				JobID:   job.ID,
				Message: errMsg,
			})
			return
		}
	}
jobCreate:

	// Check for any live schedulable worker (not offline, not evicted, fresh
	// heartbeat) before creating the job. Busy workers count as available: the
	// job is created in pending state and the scheduler queues it until a
	// worker becomes idle (TSI-2204).
	//
	// TSI-2419: freshness is enforced in SQL — a worker whose last heartbeat
	// is older than the heartbeat timeout is dead in practice (the monitor
	// only flips it offline on its next tick). Excluding such workers here
	// fails the submission fast with "no worker available" instead of
	// accepting the job and letting it sit pending until the no-worker job
	// timeout (default 2m).
	liveWorkers, err := h.db.GetLiveSchedulableWorkers(h.heartbeatTimeout)
	if err != nil {
		log.Printf("SubmitJob: Failed to check schedulable workers: %v", err)
		// Don't fail the request on database error - let the scheduler handle it
	} else if len(liveWorkers) == 0 {
		writeError(w, http.StatusServiceUnavailable, protocol.NewProtocolError(
			protocol.ErrCodeWorkerUnavailable,
			"No worker available. Please ensure at least one worker is registered and online.",
			nil,
		))
		return
	}

	job, err := h.db.CreateJobWithStreaming(string(inputFilesJSON), string(argsJSON), req.OutputFilename, req.AutoHW, req.StreamingOutput, req.Timeout, directPathsJSON)
	if err != nil {
		// Note: the rate limit middleware's rateLimitResponseWriter will
		// automatically decrement the counter on non-2xx responses, so we
		// do NOT need an explicit Decrement here.
		writeError(w, http.StatusInternalServerError, protocol.NewProtocolError(
			protocol.ErrCodeInternalError, "Failed to create job", err,
		))
		return
	}

	// Register the job-client mapping for rate limiter cleanup
	if clientID := auth.GetClientID(r); clientID != "" {
		h.rateLimiter.RegisterJob(clientID, job.ID)
	}

	// Trigger immediate scheduling to reduce race condition window (TSI-1501)
	if h.scheduler != nil {
		h.scheduler.TriggerReschedule()
	}

	writeJSON(w, http.StatusOK, protocol.JobSubmitResponse{
		JobID:   job.ID,
		Message: "Job submitted successfully",
	})
}

// failAsEncoderUnavailable reports whether a submission whose requested encoder
// has no live worker should be persisted as ENCODER_UNAVAILABLE rather than
// rejected as worker_unavailable. True only when at least one schedulable
// worker exists (a stale heartbeat still counts — TSI-2419) but none of them
// has the requested encoder or a compatible one. A workerless cluster is
// NO_WORKER_AVAILABLE, not an unavailable encoder (TSI-2846).
func (h *Handler) failAsEncoderUnavailable(requestedEncoder string, compatibleEncoders []string) bool {
	// At least one schedulable worker must exist for a missing encoder to mean
	// "unavailable" rather than "no worker available".
	all, err := h.db.GetSchedulableWorkers()
	if err != nil {
		log.Printf("SubmitJob: Failed to list schedulable workers: %v", err)
		return false
	}
	if len(all) == 0 {
		return false
	}

	// Does any schedulable worker (stale or not) already have the requested
	// encoder or a compatible one? If so, the live-match failure is a liveness
	// problem, not an encoder-capability gap. A query error here is fail-closed:
	// returning true on error would convert a transient DB fault into evidence
	// of "encoder unavailable" and persist a terminal misclassification.
	has := func(enc string) (bool, error) {
		workers, err := h.db.GetSchedulableWorkersByEncoder(enc)
		if err != nil {
			return false, fmt.Errorf("check encoder %s: %w", enc, err)
		}
		return len(workers) > 0, nil
	}
	present, err := has(requestedEncoder)
	if err != nil {
		log.Printf("SubmitJob: %v", err)
		return false
	}
	if present {
		return false
	}
	for _, enc := range compatibleEncoders {
		if enc == requestedEncoder {
			continue
		}
		present, err := has(enc)
		if err != nil {
			log.Printf("SubmitJob: %v", err)
			return false
		}
		if present {
			return false
		}
	}
	return true
}

// GetJob handles job status query
func (h *Handler) GetJob(w http.ResponseWriter, r *http.Request) {
	jobID := chi.URLParam(r, "jobId")

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

	var noLiveWorker bool
	if job.Status == protocol.JobStatusPending {
		noLiveWorker = h.hasNoLiveSchedulableWorker()
	}
	jobInfo := h.dbJobToJobInfo(job, noLiveWorker)
	writeJSON(w, http.StatusOK, protocol.JobStatusResponse{Job: jobInfo})
}

// ListJobs handles listing all jobs with pagination.
// GET /api/v1/jobs
// Query params: limit (default 50, capped at 500), offset (default 0).
// Results are ordered newest-first (created_at DESC).
func (h *Handler) ListJobs(w http.ResponseWriter, r *http.Request) {
	limit, offset := parsePagination(r, 50, 500)

	jobs, err := h.db.ListJobs(limit, offset)
	if err != nil {
		writeError(w, http.StatusInternalServerError, protocol.NewProtocolError(
			protocol.ErrCodeInternalError, "Failed to list jobs", err,
		))
		return
	}

	noLiveWorker := h.hasNoLiveSchedulableWorker()
	jobInfos := make([]protocol.JobInfo, len(jobs))
	for i, job := range jobs {
		jobInfos[i] = h.dbJobToJobInfo(job, noLiveWorker)
	}

	writeJSON(w, http.StatusOK, protocol.JobListResponse{Jobs: jobInfos})
}

// CancelJob handles job cancellation
func (h *Handler) CancelJob(w http.ResponseWriter, r *http.Request) {
	jobID := chi.URLParam(r, "jobId")

	// Get job before cancellation to know its previous status for broadcasting
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

	err = h.db.CancelJob(jobID)
	if err != nil {
		if errors.Is(err, protocol.ErrJobNotFound) {
			writeError(w, http.StatusNotFound, protocol.NewProtocolError(
				protocol.ErrCodeNotFound, "Job not found", err,
			))
			return
		}
		writeError(w, http.StatusBadRequest, protocol.NewProtocolError(
			protocol.ErrCodeInvalidRequest, "Cannot cancel job in current state", nil,
		))
		return
	}

	// Decrement rate limiter counter when job is cancelled
	h.rateLimiter.DecrementByJob(jobID)

	// Broadcast cancellation to WebSocket clients
	if err := h.wsHub.BroadcastStatus(jobID, protocol.JobStatusCancelled, 0, "Job cancelled"); err != nil {
		log.Printf("Failed to broadcast cancellation for job %s: %v", jobID, err)
	}

	// If job was running, also broadcast completion message
	if job.Status == protocol.JobStatusRunning {
		if err := h.wsHub.BroadcastComplete(jobID, 0); err != nil {
			log.Printf("Failed to broadcast completion for cancelled job %s: %v", jobID, err)
		}
	}

	writeJSON(w, http.StatusOK, protocol.JobCancelResponse{
		Message: "Job cancelled successfully",
	})
}

// UpdateJob handles job status update (for workers)
func (h *Handler) UpdateJob(w http.ResponseWriter, r *http.Request) {
	jobID := chi.URLParam(r, "jobId")

	var req protocol.JobUpdateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, protocol.NewProtocolError(
			protocol.ErrCodeInvalidRequest, "Invalid JSON body", err,
		))
		return
	}

	// Validate status if provided
	if req.Status != "" {
		if !protocol.IsTerminalStatus(req.Status) && req.Status != protocol.JobStatusRunning {
			writeError(w, http.StatusBadRequest, protocol.NewProtocolError(
				protocol.ErrCodeInvalidRequest, "Invalid status: "+string(req.Status), nil,
			))
			return
		}
	}

	var exitCode *int
	var errMsg *string
	if req.ExitCode != 0 {
		exitCode = &req.ExitCode
	}
	if req.Error != "" {
		errMsg = &req.Error
	}

	// Only update status if provided
	if req.Status != "" {
		// Validate failure classification against the documented enum
		// (openapi.yaml JobUpdateRequest.failure_type).
		if req.FailureType != "" && !protocol.FailureType(req.FailureType).IsValid() {
			writeError(w, http.StatusBadRequest, protocol.NewProtocolError(
				protocol.ErrCodeInvalidRequest, "Invalid failure_type: "+req.FailureType, nil,
			))
			return
		}

		var failureType, failureDetails *string
		if req.Status == protocol.JobStatusCompleted {
			// Success clears any stale failure metadata left over from a
			// previous failed attempt (e.g. after migration/retry).
			empty := ""
			failureType, failureDetails = &empty, &empty
		} else {
			if req.FailureType != "" {
				failureType = &req.FailureType
			}
			if req.FailureDetails != "" {
				failureDetails = &req.FailureDetails
			}
		}
		var err error
		if protocol.IsTerminalStatus(req.Status) && req.WorkerID != "" {
			// Ownership-guarded terminal update: the report only lands if
			// req.WorkerID still owns the job (running/queued). A stale
			// terminal report from a worker that lost the job to failover
			// returns 409 instead of overwriting the new owner's result.
			err = h.db.UpdateJobTerminalStatusWithOwnerAndCache(jobID, db.NormalizeWorkerID(req.WorkerID), req.Status, exitCode, errMsg, failureType, failureDetails, req.Cached)
		} else {
			err = h.db.UpdateJobStatusWithFailureAndCache(jobID, req.Status, exitCode, errMsg, failureType, failureDetails, req.Cached)
		}
		if err != nil {
			if errors.Is(err, protocol.ErrJobNotFound) {
				writeError(w, http.StatusNotFound, protocol.NewProtocolError(
					protocol.ErrCodeNotFound, "Job not found", err,
				))
				return
			}
			if errors.Is(err, protocol.ErrJobTerminal) {
				// A late report racing a terminal outcome (worker completion
				// vs. concurrent cancel) is a benign lost race, not a server
				// fault. 409 tells the worker the job is already finished so
				// it stops reporting instead of retrying a "500".
				writeError(w, http.StatusConflict, protocol.NewProtocolError(
					protocol.ErrCodeConflict, "Job already in terminal state", err,
				))
				return
			}
			if errors.Is(err, protocol.ErrJobNotOwned) {
				// Ownership guard tripped: the reporting worker no longer owns
				// this job (reassigned after failover) or the job is already in
				// a terminal state. 409 tells the stale reporter to stop.
				writeError(w, http.StatusConflict, protocol.NewProtocolError(
					protocol.ErrCodeConflict, "Job no longer assigned to this worker", err,
				))
				return
			}
			writeError(w, http.StatusInternalServerError, protocol.NewProtocolError(
				protocol.ErrCodeInternalError, "Failed to update job", err,
			))
			return
		}

		// Decrement rate limiter counter when job reaches terminal status
		if protocol.IsTerminalStatus(req.Status) {
			h.rateLimiter.DecrementByJob(jobID)
		}

		// When a job reaches terminal status, immediately flip the worker to
		// idle if it has no remaining active jobs. This eliminates the 10-15
		// second delay where the worker remains "busy" on the server until the
		// next heartbeat, causing subsequent jobs to be rejected.
		//
		// SetWorkerIdleIfNoActiveJobs does the active-count check and the
		// status write in ONE guarded statement (status != offline, no active
		// jobs): an offline worker can no longer be resurrected into the
		// schedulable pool by its own late completion report, and a racing
		// pull-path busy write cannot be overwritten to idle.
		if protocol.IsTerminalStatus(req.Status) {
			job, err := h.db.GetJob(jobID)
			if err == nil && job.WorkerID.Valid {
				if err := h.db.SetWorkerIdleIfNoActiveJobs(job.WorkerID.String); err != nil {
					if !errors.Is(err, protocol.ErrWorkerNotFound) {
						log.Printf("Failed to set worker %s to idle after job %s completed: %v",
							job.WorkerID.String, jobID, err)
					}
				} else {
					log.Printf("Worker %s set to idle after job %s completed (active jobs: 0)",
						job.WorkerID.String, jobID)
					// Trigger immediate rescheduling so pending jobs can be assigned
					if h.scheduler != nil {
						h.scheduler.TriggerReschedule()
					}
				}
			}
		}
	}

	// Broadcast stderr chunk if provided
	if req.StderrChunk != "" {
		if err := h.wsHub.BroadcastStderr(jobID, req.StderrChunk); err != nil {
			log.Printf("Failed to broadcast stderr for job %s: %v", jobID, err)
		}
	}

	// Broadcast stdout chunk if provided (streaming output mode)
	if req.StdoutChunk != "" {
		if err := h.wsHub.BroadcastStdout(jobID, req.StdoutChunk); err != nil {
			log.Printf("Failed to broadcast stdout for job %s: %v", jobID, err)
		}
	}

	// Handle progress update from worker
	if req.Progress > 0 || req.EtaSeconds > 0 {
		// Store progress in DB
		if err := h.db.UpdateJobProgress(jobID, req.Progress, req.EtaSeconds); err != nil {
			log.Printf("Failed to update job progress for job %s: %v", jobID, err)
		}
		// Broadcast progress via WebSocket
		if err := h.wsHub.BroadcastProgress(jobID, req.Progress, req.TimeUs, req.DurationUs, req.Speed, req.EtaSeconds); err != nil {
			log.Printf("Failed to broadcast progress for job %s: %v", jobID, err)
		}
	}

	// Broadcast status update to WebSocket clients only if status is provided
	if req.Status != "" {
		var statusErr string
		if errMsg != nil {
			statusErr = *errMsg
		}
		var ec int
		if exitCode != nil {
			ec = *exitCode
		}
		if err := h.wsHub.BroadcastStatus(jobID, req.Status, ec, statusErr); err != nil {
			log.Printf("Failed to broadcast status for job %s: %v", jobID, err)
		}

		// If job is completed, broadcast completion message
		if protocol.IsTerminalStatus(req.Status) {
			if err := h.wsHub.BroadcastComplete(jobID, ec); err != nil {
				log.Printf("Failed to broadcast completion for job %s: %v", jobID, err)
			}
		}
	}

	writeJSON(w, http.StatusOK, protocol.JobUpdateResponse{
		Message: "Job updated successfully",
	})
}

// UploadJobOutput handles output file upload from worker
func (h *Handler) UploadJobOutput(w http.ResponseWriter, r *http.Request) {
	jobID := chi.URLParam(r, "jobId")

	// Verify job exists
	_, err := h.db.GetJob(jobID)
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

	maxSize := int64(10 * 1024 * 1024 * 1024) // 10GB max

	r.Body = http.MaxBytesReader(w, r.Body, maxSize)
	// 32MB in-memory threshold, matching Upload: larger parts spill to
	// temporary files on disk. The old 256MB let concurrent uploads pin
	// hundreds of MB of RSS (TSI-2365).
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeError(w, http.StatusBadRequest, protocol.NewProtocolError(
			protocol.ErrCodeInvalidRequest, "Failed to parse multipart form", err,
		))
		return
	}

	file, _, err := r.FormFile("file")
	if err != nil {
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

	// Update job with output file
	outputFiles := []string{fileID}
	outputFilesJSON, err := json.Marshal(outputFiles)
	if err != nil {
		writeError(w, http.StatusInternalServerError, protocol.NewProtocolError(
			protocol.ErrCodeInternalError, "Failed to marshal output files", err,
		))
		return
	}
	if err := h.db.UpdateJobOutput(jobID, string(outputFilesJSON)); err != nil {
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

// Health handles health check
func (h *Handler) Health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, protocol.HealthResponse{
		Status:      "healthy",
		Timestamp:   time.Now(),
		Version:     h.version,
		AuthEnabled: h.authToken != "",
	})
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

// RegisterWorker handles worker registration
func (h *Handler) RegisterWorker(w http.ResponseWriter, r *http.Request) {
	var req protocol.WorkerRegisterRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, protocol.NewProtocolError(
			protocol.ErrCodeInvalidRequest, "Invalid JSON body", err,
		))
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, protocol.NewProtocolError(
			protocol.ErrCodeInvalidRequest, "name is required", nil,
		))
		return
	}
	if len(req.Capabilities.Encoders) == 0 || req.Capabilities.FFmpegVersion == "" {
		writeError(w, http.StatusBadRequest, protocol.NewProtocolError(
			protocol.ErrCodeInvalidRequest, "encoders and ffmpeg_version are required", nil,
		))
		return
	}

	// Derive the optional request-side rich metadata (video_encoders /
	// video_decoders) from the canonical flat lists when a minimal client
	// registers without them. The rich lists feed GET /api/v1/encoders and
	// GET /api/v1/decoders only; leaving them nil makes those endpoints return
	// empty for every spec-conformant client (TSI-2522).
	if len(req.Capabilities.VideoEncoders) == 0 {
		for _, name := range req.Capabilities.Encoders {
			req.Capabilities.VideoEncoders = append(req.Capabilities.VideoEncoders, protocol.EncoderInfo{
				Name: name,
				Type: "video",
				IsHW: isHWEncoder(name),
			})
		}
	}
	if len(req.Capabilities.VideoDecoders) == 0 {
		for _, name := range req.Capabilities.Decoders {
			req.Capabilities.VideoDecoders = append(req.Capabilities.VideoDecoders, protocol.DecoderInfo{
				Name: name,
				Type: "video",
				IsHW: isHWEncoder(name),
			})
		}
	}
	worker, err := h.db.CreateOrUpdateWorker(req.WorkerID, req.Name, req.Capabilities, h.heartbeatTimeout)
	if err != nil {
		writeError(w, http.StatusInternalServerError, protocol.NewProtocolError(
			protocol.ErrCodeInternalError, "Failed to register worker", err,
		))
		return
	}

	writeJSON(w, http.StatusOK, protocol.WorkerRegisterResponse{
		WorkerID: worker.ID,
		Message:  "Worker registered successfully",
	})
}

// WorkerHeartbeat handles worker heartbeat (enhanced with GPU and throughput metrics, TSI-756).
func (h *Handler) WorkerHeartbeat(w http.ResponseWriter, r *http.Request) {
	var req protocol.WorkerHeartbeatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, protocol.NewProtocolError(
			protocol.ErrCodeInvalidRequest, "Invalid JSON body", err,
		))
		return
	}

	if err := h.db.UpdateWorkerHeartbeat(req.WorkerID, req.Status); err != nil {
		if errors.Is(err, protocol.ErrWorkerNotFound) {
			writeError(w, http.StatusNotFound, protocol.NewProtocolError(
				protocol.ErrCodeNotFound, "Worker not found", err,
			))
			return
		}
		writeError(w, http.StatusInternalServerError, protocol.NewProtocolError(
			protocol.ErrCodeInternalError, "Failed to update heartbeat", err,
		))
		return
	}

	// Update WorkerStateTable with throughput data (TSI-759)
	if h.stateTable != nil {
		statePayload := protocol.WorkerHeartbeatPayload{
			WorkerID:        req.WorkerID,
			Status:          string(req.Status),
			ActiveJobs:      req.ActiveJobs,
			ThroughputFPS:   req.ThroughputFPS,
			CompletedJobs:   req.CompletedJobs,
			GPUUtilPct:      req.GPUUtilPct,
			GPUMemUsedMB:    req.GPUMemUsedMB,
			GPUMetricsValid: req.GPUMetricsValid,
			Timestamp:       time.Now(),
		}
		h.stateTable.UpdateFromHeartbeat(statePayload)
	}

	// Cancel notification: report any active job the worker listed that is no
	// longer (or not exclusively) its own — cancelled, re-queued/re-assigned to
	// another worker after failover, or already terminal. Checking only
	// status==cancelled missed migrated jobs: a partitioned worker kept running
	// a job concurrently with its new owner (double execution).
	var cancelledJobs []string
	for _, jobID := range req.ActiveJobs {
		job, err := h.db.GetJob(jobID)
		if err != nil {
			continue
		}
		if job.Status == protocol.JobStatusCancelled ||
			job.Status == protocol.JobStatusPending ||
			job.WorkerID.Valid && job.WorkerID.String != db.NormalizeWorkerID(req.WorkerID) {
			cancelledJobs = append(cancelledJobs, jobID)
		}
	}

	writeJSON(w, http.StatusOK, protocol.WorkerHeartbeatResponse{
		Message:       "Heartbeat acknowledged",
		CancelledJobs: cancelledJobs,
	})
}

// PullWorkerJobs handles worker pulling jobs
func (h *Handler) PullWorkerJobs(w http.ResponseWriter, r *http.Request) {
	workerID := chi.URLParam(r, "workerId")

	// Verify worker exists and is pull-eligible. An offline worker must
	// re-register before pulling (its jobs were already migrated); an evicted
	// (slow-node) worker gets no new work at all.
	worker, err := h.db.GetWorker(workerID)
	if err != nil {
		if errors.Is(err, protocol.ErrWorkerNotFound) {
			writeError(w, http.StatusNotFound, protocol.NewProtocolError(
				protocol.ErrCodeNotFound, "Worker not found", err,
			))
			return
		}
		writeError(w, http.StatusInternalServerError, protocol.NewProtocolError(
			protocol.ErrCodeInternalError, "Failed to get worker", err,
		))
		return
	}
	if worker.Status == protocol.WorkerStatusOffline {
		writeError(w, http.StatusConflict, protocol.NewProtocolError(
			protocol.ErrCodeConflict, "Worker is offline; re-register to resume pulling", nil,
		))
		return
	}
	if worker.Evicted {
		writeError(w, http.StatusConflict, protocol.NewProtocolError(
			protocol.ErrCodeConflict, "Worker is evicted from scheduling", nil,
		))
		return
	}

	// Assign pending jobs to this worker and return them
	jobs, err := h.db.AssignPendingJobsToWorker(workerID, 10) // Max 10 jobs at a time
	if err != nil {
		writeError(w, http.StatusInternalServerError, protocol.NewProtocolError(
			protocol.ErrCodeInternalError, "Failed to assign jobs", err,
		))
		return
	}

	// Pulled jobs are queued (assigned), never pending, so no NoWorkerDeadline
	// applies and no live-worker lookup is needed.
	jobInfos := make([]protocol.JobInfo, len(jobs))
	for i, job := range jobs {
		jobInfos[i] = h.dbJobToJobInfo(job, false)
	}

	writeJSON(w, http.StatusOK, protocol.WorkerJobPullResponse{
		Jobs: jobInfos,
	})
}

// inputExists reports whether a probe input resolves on the server. A
// well-formed storage file ID (64-hex SHA256) is resolved through storage;
// anything else — a direct shared-FS path or a relative filename — is
// checked directly against the server's filesystem.
func inputExists(store *storage.Storage, input string) bool {
	if storage.ValidateFileID(input) {
		return store.FileExists(input)
	}
	// Reject a direct path containing a ".." component before os.Stat resolves
	// it. A bare API call can otherwise bypass the worker-side traversal guard
	// (TSI-2706). The check lives in pathutil so both sides share one semantic.
	if pathutil.ContainsPathTraversal(input) {
		return false
	}
	_, err := os.Stat(input)
	return err == nil
}

// Probe handles ffprobe requests via job dispatch to workers.
// POST /api/v1/probe
func (h *Handler) Probe(w http.ResponseWriter, r *http.Request) {
	// Rate-limit quota release: JobSubmitMiddleware incremented this client's
	// counter, but unlike job submission there is no RegisterJob mapping and
	// no worker completion callback — a probe is synchronous. Release the
	// slot exactly once per request, here. (The middleware's non-2xx decrement
	// would double-release on error exits; the counter floors at 0, so the
	// net effect is still correct — one request consumes at most one slot.)
	clientID := auth.GetClientID(r)
	if clientID == "" {
		// Must mirror JobSubmitMiddleware's fallback or the decrement targets
		// a different bucket than the increment did.
		clientID = r.RemoteAddr
	}
	defer h.rateLimiter.Decrement(clientID)

	// Bound the JSON body — a probe request is a path/URL plus optional
	// options, never megabytes. Without a cap the endpoint is a free
	// memory-amplification DoS vector (TSI-2365).
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1MB
	var req protocol.ProbeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.rateLimiter.Decrement(clientID)
		writeError(w, http.StatusBadRequest, protocol.NewProtocolError(
			protocol.ErrCodeInvalidRequest, "Invalid JSON body", err,
		))
		return
	}
	if req.Input == "" {
		writeError(w, http.StatusBadRequest, protocol.NewProtocolError(
			protocol.ErrCodeInvalidRequest, "input is required", nil,
		))
		return
	}

	// Validate the input exists (skip for remote URLs). Two input semantics:
	// a storage file ID (64-hex content hash, resolved under the server's
	// storage dir) vs. a direct local path (shared-FS passthrough, resolved
	// against the server's own filesystem). FileExists can only resolve the
	// former; feeding it a direct path always 404s (TSI-2520).
	if !strings.Contains(req.Input, "://") && !inputExists(h.storage, req.Input) {
		writeError(w, http.StatusBadRequest, protocol.NewProtocolError(
			protocol.ErrCodeNotFound, "Input file not found: "+req.Input, nil,
		))
		return
	}
	// Check for a live schedulable worker before creating the probe job
	// (TSI-2474). Without this, a probe dispatched to a cluster with no
	// online workers sits pending for the entire 2-minute poll loop before
	// returning "timeout" — the user sees a hang, not an actionable error.
	// Failing fast mirrors the submit-time check in SubmitJob (TSI-2419).
	liveWorkers, err := h.db.GetLiveSchedulableWorkers(h.heartbeatTimeout)
	if err != nil {
		writeError(w, http.StatusInternalServerError, protocol.NewProtocolError(
			protocol.ErrCodeInternalError, "Failed to check worker availability", err,
		))
		return
	}
	if len(liveWorkers) == 0 {
		writeError(w, http.StatusServiceUnavailable, protocol.NewProtocolError(
			protocol.ErrCodeWorkerUnavailable,
			"No worker available. Please ensure at least one worker is registered and online.",
			nil,
		))
		return
	}

	// Create a probe job
	inputFilesJSON, err := json.Marshal([]string{req.Input})
	if err != nil {
		writeError(w, http.StatusInternalServerError, protocol.NewProtocolError(
			protocol.ErrCodeInternalError, "Failed to marshal input files", err,
		))
		return
	}
	argsJSON, err := json.Marshal([]string{"__rffmpeg_probe__", req.Input})
	if err != nil {
		writeError(w, http.StatusInternalServerError, protocol.NewProtocolError(
			protocol.ErrCodeInternalError, "Failed to marshal args", err,
		))
		return
	}

	// Direct paths (shared-FS passthrough): when the input is neither a
	// remote URL nor a storage file ID, it is a local path the worker must
	// read directly instead of downloading over HTTP (TSI-2520).
	directPathsJSON := "[]"
	if !strings.Contains(req.Input, "://") && !storage.ValidateFileID(req.Input) {
		dp, err := json.Marshal([]string{req.Input})
		if err != nil {
			writeError(w, http.StatusInternalServerError, protocol.NewProtocolError(
				protocol.ErrCodeInternalError, "Failed to marshal direct path", err,
			))
			return
		}
		directPathsJSON = string(dp)
	}

	// Set a 60s ffmpeg execution budget for the probe job.
	timeout := 60 * time.Second
	job, err := h.db.CreateJobWithStreaming(string(inputFilesJSON), string(argsJSON), "probe_result.json", false, false, &timeout, directPathsJSON)
	if err != nil {
		writeError(w, http.StatusInternalServerError, protocol.NewProtocolError(
			protocol.ErrCodeInternalError, "Failed to create probe job", err,
		))
		return
	}

	// Wait for the probe job to reach a terminal state. A per-job notifier
	// (woken by every terminal-status DB write) replaces the old 2-second
	// poll loop, so terminal-state propagation is immediate instead of up to
	// 2s late (TSI-2520). The overall budget mirrors the previous 60x2s loop
	// (~2 minutes), but the probe job itself times out after 60s, so the
	// extra headroom only matters for a late report racing the deadline.
	const probeWaitBudget = 2 * time.Minute
	finalJob, err := h.waitForProbeTerminal(r.Context(), job.ID, probeWaitBudget)
	if err != nil {
		writeError(w, http.StatusInternalServerError, protocol.NewProtocolError(
			protocol.ErrCodeInternalError, "Failed to get job status", err,
		))
		return
	}
	if finalJob == nil {
		// Job timed out waiting for a terminal state
		writeJSON(w, http.StatusGatewayTimeout, protocol.ProbeResponse{
			Error:   "timeout",
			Message: "Probe job did not complete within the timeout period",
		})
		return
	}

	// Handle failed/cancelled/timeout jobs
	if finalJob.Status == protocol.JobStatusFailed {
		errMsg := ""
		if finalJob.Error.Valid {
			errMsg = finalJob.Error.String
		}
		writeJSON(w, http.StatusInternalServerError, protocol.ProbeResponse{
			Error:   "probe_failed",
			Message: errMsg,
		})
		return
	}
	if finalJob.Status == protocol.JobStatusTimeout {
		writeJSON(w, http.StatusGatewayTimeout, protocol.ProbeResponse{
			Error:   "timeout",
			Message: "Probe job timed out",
		})
		return
	}
	if finalJob.Status == protocol.JobStatusCancelled {
		writeJSON(w, http.StatusOK, protocol.ProbeResponse{
			Error:   "cancelled",
			Message: "Probe job was cancelled",
		})
		return
	}

	// Read probe result from output file
	var outputFiles []string
	if err := json.Unmarshal([]byte(finalJob.OutputFiles), &outputFiles); err != nil || len(outputFiles) == 0 {
		writeError(w, http.StatusInternalServerError, protocol.NewProtocolError(
			protocol.ErrCodeInternalError, "No output file from probe job", err,
		))
		return
	}

	file, err := h.storage.OpenOutput(outputFiles[0])
	if err != nil {
		writeError(w, http.StatusInternalServerError, protocol.NewProtocolError(
			protocol.ErrCodeInternalError, "Failed to open probe output", err,
		))
		return
	}
	defer file.Close()

	var ffprobeResult struct {
		Format  map[string]interface{}   `json:"format,omitempty"`
		Streams []map[string]interface{} `json:"streams,omitempty"`
	}
	if err := json.NewDecoder(file).Decode(&ffprobeResult); err != nil {
		writeError(w, http.StatusInternalServerError, protocol.NewProtocolError(
			protocol.ErrCodeInternalError, "Failed to decode probe result", err,
		))
		return
	}

	// Populate _rffmpeg metadata (TSI-3048): workers[] stays lightweight, the
	// executing worker's encoder list is emitted once as the shared list, and
	// only workers whose list differs appear in worker_encoder_overrides — so a
	// homogeneous cluster emits a single copy while a heterogeneous one keeps
	// every per-worker capability.
	var rffmpegMeta *protocol.RffmpegMeta
	allWorkers, workersErr := h.db.GetAllWorkers()

	// The shared baseline is the encoder list of the worker that executed the
	// probe; it also drives the suggestion.
	var sharedEncoders []string
	if finalJob.WorkerID.Valid {
		if encoders, err := h.db.GetWorkerEncoders(finalJob.WorkerID.String); err == nil {
			sharedEncoders = encoders
		}
	}

	var workerSummaries []protocol.WorkerSummary
	var overrides map[string][]string
	if workersErr == nil {
		workerSummaries, overrides = buildWorkerSummariesAndOverrides(allWorkers, sharedEncoders)
	}

	if len(sharedEncoders) > 0 {
		rffmpegMeta = &protocol.RffmpegMeta{
			WorkerEncoders:         sharedEncoders,
			WorkerEncoderOverrides: overrides,
			Suggestion:             buildEncoderSuggestion(ffprobeResult.Streams, sharedEncoders),
			Workers:                workerSummaries,
		}
	} else if len(workerSummaries) > 0 {
		// No executing-worker baseline: fall back to summaries only.
		rffmpegMeta = &protocol.RffmpegMeta{
			Workers: workerSummaries,
		}
	}

	writeJSON(w, http.StatusOK, protocol.ProbeResponse{
		Format:  ffprobeResult.Format,
		Streams: ffprobeResult.Streams,
		Rffmpeg: rffmpegMeta,
	})
}

// buildWorkerSummariesAndOverrides reduces the worker list to lightweight
// summaries and returns sparse per-worker encoder overrides relative to the
// shared (executing-worker) list. Workers whose list matches the shared list
// are elided, so a homogeneous cluster emits no overrides (TSI-3048).
func buildWorkerSummariesAndOverrides(allWorkers []*db.Worker, sharedEncoders []string) ([]protocol.WorkerSummary, map[string][]string) {
	workerSummaries := make([]protocol.WorkerSummary, 0, len(allWorkers))
	overrides := make(map[string][]string)
	for _, w := range allWorkers {
		workerSummaries = append(workerSummaries, protocol.WorkerSummary{
			ID:            w.ID,
			Name:          w.Name,
			Status:        string(w.Status),
			FFmpegVersion: w.FFmpegVersion,
			MaxConcurrent: w.MaxConcurrent,
		})
		var encoders []string
		if err := json.Unmarshal([]byte(w.Encoders), &encoders); err != nil {
			encoders = []string{}
		}
		if !slices.Equal(encoders, sharedEncoders) {
			overrides[w.ID] = encoders
		}
	}
	return workerSummaries, overrides
}

// waitForProbeTerminal blocks until the probe job reaches a terminal state,
// the request context is cancelled, or the overall budget elapses. It returns
// (nil, nil) on budget exhaustion — the caller maps that to a 504 — and a
// non-nil error only on a database failure or a cancelled request (whose job
// is cancelled in flight, mirroring the old poll loop's client-gone path).
func (h *Handler) waitForProbeTerminal(ctx context.Context, jobID string, waitTimeout time.Duration) (*db.Job, error) {
	var notifyCh <-chan struct{}
	var unsubscribe func()
	if n := h.db.JobNotifier(); n != nil {
		// Subscribe BEFORE the first read so a terminal write racing this
		// function cannot fall between the read and the blocking select.
		notifyCh, unsubscribe = n.Subscribe(jobID)
		defer unsubscribe()
	}

	timer := time.NewTimer(waitTimeout)
	defer timer.Stop()

	for {
		j, err := h.db.GetJob(jobID)
		if err != nil {
			return nil, err
		}
		if protocol.IsTerminalStatus(j.Status) {
			return j, nil
		}

		select {
		case <-ctx.Done():
			// Client went away: stop burning server resources and cancel
			// the dispatched probe job (TSI-2365).
			_ = h.db.CancelJob(jobID)
			return nil, ctx.Err()
		case <-timer.C:
			return nil, nil
		case <-notifyCh:
			// Terminal write observed; loop re-reads the DB (the source of
			// truth) instead of trusting the hint directly.
		}
	}
}

// codecFormatPriority defines the preferred encoders (HW first, then SW) for each codec format.
// Hardware encoders are listed first (preferred), software encoders as fallback.
var codecFormatPriority = map[string][]string{
	"h264":   {"h264_nvenc", "h264_qsv", "h264_vaapi", "h264_amf", "h264_videotoolbox", "libx264", "libx264rgb"},
	"avc":    {"h264_nvenc", "h264_qsv", "h264_vaapi", "h264_amf", "h264_videotoolbox", "libx264", "libx264rgb"},
	"hevc":   {"hevc_nvenc", "hevc_qsv", "hevc_vaapi", "hevc_amf", "hevc_videotoolbox", "libx265"},
	"h265":   {"hevc_nvenc", "hevc_qsv", "hevc_vaapi", "hevc_amf", "hevc_videotoolbox", "libx265"},
	"vp9":    {"vp9_vaapi", "vp9_qsv", "vp9_videotoolbox", "libvpx-vp9"},
	"av1":    {"av1_nvenc", "av1_qsv", "av1_vaapi", "av1_amf", "libaom-av1", "libsvtav1"},
	"vp8":    {"libvpx"},
	"mpeg2":  {"mpeg2video"},
	"mpeg4":  {"mpeg4"},
	"mjpeg":  {"mjpeg"},
	"prores": {"prores_ks", "prores"},
}

// codecToFormat maps common ffprobe codec names to format keys used in codecFormatPriority.
// Includes both canonical names and container-specific FourCC/CodecID variants.
var codecToFormat = map[string]string{
	// H.264 / AVC variants
	"h264":       "h264",
	"h264_nvenc": "h264",
	"libx264":    "h264",
	"libx264rgb": "h264",
	"avc1":       "avc", // QuickTime/MP4 FourCC
	"avc3":       "avc", // QuickTime/MP4 FourCC (parameter sets in-band)
	"v_mp4/h264": "avc", // Matroska CodecID
	// HEVC / H.265 variants
	"hevc":       "hevc",
	"h265":       "hevc",
	"hevc_nvenc": "hevc",
	"libx265":    "hevc",
	"hvc1":       "hevc", // QuickTime/MP4 FourCC
	"hev1":       "hevc", // QuickTime/MP4 FourCC (parameter sets in-band)
	"v_mp4/hevc": "hevc", // Matroska CodecID
	// VP9 variants
	"vp9":        "vp9",
	"libvpx-vp9": "vp9",
	"vp09":       "vp9", // QuickTime/MP4 FourCC
	"v_vp9":      "vp9", // Matroska CodecID
	// AV1 variants
	"av1":        "av1",
	"libaom-av1": "av1",
	"libsvtav1":  "av1",
	"av01":       "av1", // QuickTime/MP4 FourCC
	"v_av1":      "av1", // Matroska CodecID
	// VP8 variants
	"vp8":    "vp8",
	"libvpx": "vp8",
	"v_vp8":  "vp8", // Matroska CodecID
	// MPEG-2 variants
	"mpeg2video": "mpeg2",
	"mpeg2":      "mpeg2",
	"v_mpeg2":    "mpeg2", // Matroska CodecID
	// MPEG-4 variants
	"mpeg4":   "mpeg4",
	"msmpeg4": "mpeg4",
	"mp4v":    "mpeg4", // QuickTime/MP4 FourCC
	"xvid":    "mpeg4", // Xvid
	"divx":    "mpeg4", // DivX
	"v_mpeg4": "mpeg4", // Matroska CodecID
	// MJPEG variants
	"mjpeg":      "mjpeg",
	"motionjpeg": "mjpeg",
	"mjpg":       "mjpeg", // QuickTime FourCC
	// ProRes variants
	"prores":    "prores",
	"prores_ks": "prores",
	"apcn":      "prores", // Apple ProRes 422 Proxy
	"apcs":      "prores", // Apple ProRes 422 LT
	"apco":      "prores", // Apple ProRes 422
	"apch":      "prores", // Apple ProRes 422 HQ
	"ap4h":      "prores", // Apple ProRes 4444
	"ap4x":      "prores", // Apple ProRes 4444 XQ
}

// buildEncoderSuggestion analyzes ffprobe streams and available encoders to produce
// a recommendation for the best encoder to use for transcoding.
func buildEncoderSuggestion(streams []map[string]interface{}, availableEncoders []string) *protocol.EncoderSuggestion {
	if len(streams) == 0 || len(availableEncoders) == 0 {
		return nil
	}

	// Find the first video stream and extract its codec
	var videoCodec string
	for _, stream := range streams {
		if codecType, ok := stream["codec_type"].(string); ok && codecType == "video" {
			if codecName, ok := stream["codec_name"].(string); ok {
				videoCodec = strings.ToLower(codecName)
				break
			}
		}
	}

	if videoCodec == "" {
		return nil
	}

	// Map the codec to a format
	format, ok := codecToFormat[videoCodec]
	if !ok {
		return nil
	}

	// Get preferred encoder list for this format
	preferredEncoders, ok := codecFormatPriority[format]
	if !ok {
		return nil
	}

	// Build a set of available encoders for quick lookup
	availableSet := make(map[string]bool, len(availableEncoders))
	for _, e := range availableEncoders {
		availableSet[e] = true
	}

	// Find the first matching encoder in priority order
	for _, candidate := range preferredEncoders {
		if availableSet[candidate] {
			reason := buildSuggestionReason(candidate, format)
			return &protocol.EncoderSuggestion{
				RecommendedEncoder: candidate,
				Reason:             reason,
			}
		}
	}

	return nil
}

// buildSuggestionReason generates a human-readable reason for the recommendation.
func buildSuggestionReason(encoder, format string) string {
	formatDisplay := format
	if format == "mjpeg" {
		formatDisplay = "MJPEG"
	} else {
		formatDisplay = strings.ToUpper(format)
	}

	if isHWEncoder(encoder) {
		return "Hardware-accelerated " + formatDisplay + " encoding available on this worker"
	}
	return "Detected " + formatDisplay + " video — use software encoder (no hardware encoder available for this format)"
}

// hwEncoderSuffixes lists the suffixes that identify hardware-accelerated encoders.
var hwEncoderSuffixes = []string{
	"_nvenc", "_qsv", "_vaapi", "_amf", "_videotoolbox", "_cuvid", "_vdpau", "_nvdec",
}

// isHWEncoder checks if an encoder name indicates a hardware-accelerated encoder.
func isHWEncoder(name string) bool {
	for _, suffix := range hwEncoderSuffixes {
		if strings.HasSuffix(name, suffix) {
			return true
		}
	}
	return false
}

// hasNoLiveSchedulableWorker reports whether the cluster currently has no live
// schedulable worker, so pending jobs can carry a NoWorkerDeadline. It returns
// false (deadline suppressed) when the starvation sweep is disabled
// (noWorkerJobTimeout <= 0) or the lookup fails — the sweep would also skip the
// tick on a failed lookup, so the job keeps waiting and no verdict is imminent.
func (h *Handler) hasNoLiveSchedulableWorker() bool {
	if h.noWorkerJobTimeout <= 0 {
		return false
	}
	liveWorkers, err := h.db.GetLiveSchedulableWorkers(h.heartbeatTimeout)
	if err != nil {
		log.Printf("failed to check live schedulable workers: %v", err)
		return false
	}
	return len(liveWorkers) == 0
}

// dbJobToJobInfo converts database Job to protocol JobInfo
func (h *Handler) dbJobToJobInfo(job *db.Job, noLiveWorker bool) protocol.JobInfo {
	var inputFiles, args, outputFiles, directPaths []string
	if err := json.Unmarshal([]byte(job.InputFiles), &inputFiles); err != nil {
		log.Printf("Failed to unmarshal input files for job %s: %v", job.ID, err)
	}
	if err := json.Unmarshal([]byte(job.Args), &args); err != nil {
		log.Printf("Failed to unmarshal args for job %s: %v", job.ID, err)
	}
	if err := json.Unmarshal([]byte(job.OutputFiles), &outputFiles); err != nil {
		log.Printf("Failed to unmarshal output files for job %s: %v", job.ID, err)
	}
	if err := json.Unmarshal([]byte(job.DirectPaths), &directPaths); err != nil {
		log.Printf("Failed to unmarshal direct paths for job %s: %v", job.ID, err)
	}

	info := protocol.JobInfo{
		ID:              job.ID,
		Status:          job.Status,
		InputFiles:      inputFiles,
		Args:            args,
		OutputFilename:  job.OutputFilename,
		StreamingOutput: job.StreamingOutput,
		OutputFiles:     outputFiles,
		AutoHW:          job.AutoHW,
		Cached:          job.Cached,
		FailureType:     job.FailureType,
		FailureDetails:  job.FailureDetails,
		Retryable:       protocol.FailureType(job.FailureType).Retryable(),
		DirectPaths:     directPaths,
		ProgressPercent: job.ProgressPercent,
		EtaSeconds:      job.EtaSeconds,
		CreatedAt:       job.CreatedAt,
		UpdatedAt:       job.UpdatedAt,
	}

	if job.WorkerID.Valid {
		info.WorkerID = job.WorkerID.String
	}
	if job.AssignedWorker.Valid {
		info.AssignedWorker = job.AssignedWorker.String
	}
	if job.WorkerName.Valid {
		info.WorkerName = job.WorkerName.String
	}
	if job.ExitCode.Valid {
		info.ExitCode = int(job.ExitCode.Int32)
	}
	if job.Error.Valid {
		info.Error = job.Error.String
	}
	if job.StartedAt.Valid {
		info.StartedAt = &job.StartedAt.Time
	}
	if job.FinishedAt.Valid {
		info.FinishedAt = &job.FinishedAt.Time
	}
	if job.Timeout.Valid {
		d := time.Duration(job.Timeout.Int64)
		info.Timeout = &d
	}
	// Match checkNoWorkerStarvation's exact filter: pending AND unassigned
	// AND the cluster has no live schedulable worker. A queued job already
	// has a worker_id and can never receive NO_WORKER_AVAILABLE; a pending
	// job behind a busy-but-live worker is never swept (the sweep's
	// live-worker guard short-circuits, TSI-2204), so neither carries a
	// deadline. noLiveWorker is computed once per request by the caller so a
	// page of pending jobs does not re-run the same live-worker query N times.
	if job.Status == protocol.JobStatusPending && h.noWorkerJobTimeout > 0 && noLiveWorker {
		d := job.CreatedAt.Add(h.noWorkerJobTimeout + h.timeoutCheckInterval)
		info.NoWorkerDeadline = &d
	}

	return info
}

// GetJobLogWS handles WebSocket connections for real-time job log streaming
// Endpoint: GET /api/v1/jobs/{jobId}/log
func (h *Handler) GetJobLogWS(w http.ResponseWriter, r *http.Request) {
	websocket.HandleJobLogWithValidation(h.wsHub, h.db, w, r)
}

// ListWorkers handles listing all workers with their capabilities.
// GET /api/v1/workers
// Query param active_only=true returns only non-offline workers. The default
// list includes offline rows that are still retained within the
// --worker-offline-threshold window after a worker dies (TSI-2919).
func (h *Handler) ListWorkers(w http.ResponseWriter, r *http.Request) {
	// active_only is declared boolean in the OpenAPI spec, so parse it as one;
	// any non-boolean value falls back to false (the default list).
	activeOnly, _ := strconv.ParseBool(r.URL.Query().Get("active_only"))

	var (
		workers []*db.Worker
		err     error
	)
	if activeOnly {
		workers, err = h.db.GetActiveWorkers()
	} else {
		workers, err = h.db.GetAllWorkers()
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, protocol.NewProtocolError(
			protocol.ErrCodeInternalError, "Failed to get workers", err,
		))
		return
	}

	response := make([]WorkerInfo, len(workers))
	states := h.workerStateMap()
	for i, worker := range workers {
		response[i] = dbWorkerToWorkerInfo(worker, states)
	}

	writeJSON(w, http.StatusOK, ListWorkersResponse{Workers: response})
}

// GetWorker retrieves a single worker by ID
func (h *Handler) GetWorker(w http.ResponseWriter, r *http.Request) {
	workerID := chi.URLParam(r, "workerId")

	worker, err := h.db.GetWorker(workerID)
	if err != nil {
		if errors.Is(err, protocol.ErrWorkerNotFound) {
			writeError(w, http.StatusNotFound, protocol.NewProtocolError(
				protocol.ErrCodeNotFound, "Worker not found", err,
			))
			return
		}
		writeError(w, http.StatusInternalServerError, protocol.NewProtocolError(
			protocol.ErrCodeInternalError, "Failed to get worker", err,
		))
		return
	}

	writeJSON(w, http.StatusOK, GetWorkerResponse{Worker: dbWorkerToWorkerInfo(worker, h.workerStateMap())})
}

// ListMigrationEvents handles listing worker migration audit events.
// GET /api/v1/migrations
// Query params: limit (default 50, capped at 500), offset (default 0).
func (h *Handler) ListMigrationEvents(w http.ResponseWriter, r *http.Request) {
	limit, offset := parsePagination(r, 50, 500)

	events, err := h.db.GetMigrationEvents(limit, offset)
	if err != nil {
		writeError(w, http.StatusInternalServerError, protocol.NewProtocolError(
			protocol.ErrCodeInternalError, "Failed to list migration events", err,
		))
		return
	}

	targets, err := h.redistributionTargets(events)
	if err != nil {
		writeError(w, http.StatusInternalServerError, protocol.NewProtocolError(
			protocol.ErrCodeInternalError, "Failed to list migration targets", err,
		))
		return
	}

	writeJSON(w, http.StatusOK, MigrationEventsResponse{
		Events: migrationEventsToInfo(events, targets),
	})
}

// GetMigrationEvent retrieves a single worker migration audit event by ID.
// GET /api/v1/migrations/{eventId}
func (h *Handler) GetMigrationEvent(w http.ResponseWriter, r *http.Request) {
	eventID := chi.URLParam(r, "eventId")

	event, err := h.db.GetMigrationEvent(eventID)
	if err != nil {
		if errors.Is(err, protocol.ErrMigrationEventNotFound) {
			writeError(w, http.StatusNotFound, protocol.NewProtocolError(
				protocol.ErrCodeNotFound, "Migration event not found", err,
			))
			return
		}
		writeError(w, http.StatusInternalServerError, protocol.NewProtocolError(
			protocol.ErrCodeInternalError, "Failed to get migration event", err,
		))
		return
	}

	redistributions, err := h.db.GetJobRedistributionsByEvent(eventID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, protocol.NewProtocolError(
			protocol.ErrCodeInternalError, "Failed to get migration targets", err,
		))
		return
	}

	info := migration.FromDBEvent(event)
	info.Targets = migration.RedistributionsByEvent(redistributions)[eventID]
	writeJSON(w, http.StatusOK, MigrationEventResponse{Event: info})
}

// redistributionTargets fetches the per-job redistribution targets for the
// given events in one batched query, grouped by event ID.
func (h *Handler) redistributionTargets(events []*db.MigrationEvent) (map[string][]migration.JobRedistributionTarget, error) {
	if len(events) == 0 {
		return nil, nil
	}
	ids := make([]string, len(events))
	for i, e := range events {
		ids[i] = e.ID
	}
	redistributions, err := h.db.GetJobRedistributionsByEvents(ids)
	if err != nil {
		return nil, err
	}
	return migration.RedistributionsByEvent(redistributions), nil
}

// migrationEventsToInfo converts db.MigrationEvent records into the public
// migration.EventInfo slice, attaching the per-job redistribution targets.
func migrationEventsToInfo(events []*db.MigrationEvent, targets map[string][]migration.JobRedistributionTarget) []migration.EventInfo {
	if events == nil {
		return []migration.EventInfo{}
	}
	result := make([]migration.EventInfo, len(events))
	for i, event := range events {
		info := migration.FromDBEvent(event)
		info.Targets = targets[event.ID]
		result[i] = info
	}
	return result
}

// MigrationEventResponse wraps a single migration event for JSON responses.
type MigrationEventResponse struct {
	Event migration.EventInfo `json:"event"`
}

// MigrationEventsResponse wraps a page of migration events for JSON responses.
type MigrationEventsResponse struct {
	Events []migration.EventInfo `json:"events"`
}

// parsePagination extracts limit/offset query parameters with defaults and a
// cap. Non-integer values and non-positive limits fall back to the defaults;
// the cap bounds the LIMIT sent to SQLite.
func parsePagination(r *http.Request, defaultLimit, maxLimit int) (int, int) {
	limit := defaultLimit
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > maxLimit {
		limit = maxLimit
	}

	offset := 0
	if v := r.URL.Query().Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			offset = n
		}
	}

	return limit, offset
}

// ListWorkersByEncoder handles listing workers that have a specific encoder
func (h *Handler) ListWorkersByEncoder(w http.ResponseWriter, r *http.Request) {
	encoderName := chi.URLParam(r, "encoder")

	workers, err := h.db.GetWorkersByEncoder(encoderName)
	if err != nil {
		writeError(w, http.StatusInternalServerError, protocol.NewProtocolError(
			protocol.ErrCodeInternalError, "Failed to get workers by encoder", err,
		))
		return
	}

	response := make([]WorkerInfo, len(workers))
	states := h.workerStateMap()
	for i, worker := range workers {
		response[i] = dbWorkerToWorkerInfo(worker, states)
	}

	writeJSON(w, http.StatusOK, ListWorkersByEncoderResponse{
		Encoder: encoderName,
		Workers: response,
	})
}

// ListAllEncoders handles listing all unique encoders across all workers
func (h *Handler) ListAllEncoders(w http.ResponseWriter, r *http.Request) {
	encoders, err := h.db.GetAllEncodersInfo()
	if err != nil {
		writeError(w, http.StatusInternalServerError, protocol.NewProtocolError(
			protocol.ErrCodeInternalError, "Failed to get encoders", err,
		))
		return
	}

	writeJSON(w, http.StatusOK, ListEncodersResponse{Encoders: encoders})
}

// ListAllDecoders handles listing all unique decoders across all workers
func (h *Handler) ListAllDecoders(w http.ResponseWriter, r *http.Request) {
	decoders, err := h.db.GetAllDecodersInfo()
	if err != nil {
		writeError(w, http.StatusInternalServerError, protocol.NewProtocolError(
			protocol.ErrCodeInternalError, "Failed to get decoders", err,
		))
		return
	}

	writeJSON(w, http.StatusOK, ListDecodersResponse{Decoders: decoders})
}

// ListAllHwaccels handles listing all unique hwaccels across all workers.
// GET /api/v1/hwaccels
// Default: text/plain single-column list (matching ffmpeg -hwaccels output).
// Query parameter ?json=true returns a JSON array.
func (h *Handler) ListAllHwaccels(w http.ResponseWriter, r *http.Request) {
	hwaccels, err := h.db.GetAllHwaccels()
	if err != nil {
		writeError(w, http.StatusInternalServerError, protocol.NewProtocolError(
			protocol.ErrCodeInternalError, "Failed to get hwaccels", err,
		))
		return
	}

	// Support ?json=true parameter for JSON output
	if r.URL.Query().Get("json") == "true" || r.URL.Query().Get("json") == "1" {
		writeJSON(w, http.StatusOK, ListHwaccelsResponse{Hwaccels: hwaccels})
		return
	}

	// Default: text/plain format — single column list like ffmpeg -hwaccels
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	for _, h := range hwaccels {
		fmt.Fprintln(w, h)
	}
}

// WorkerHealth carries live runtime metrics for a worker (TSI-2219).
// Populated from the server's worker state table when at least one heartbeat
// has been received; otherwise derived from the database record.
type WorkerHealth struct {
	Status          string   `json:"status"`
	GPUUtilPct      float64  `json:"gpu_util_percent,omitempty"`
	GPUMemUsedMB    int      `json:"gpu_mem_used_mb,omitempty"`
	GPUMetricsValid bool     `json:"gpu_metrics_valid"` // True when the GPU fields carry a fresh sample; false means stale/no sample (any GPU source)
	ActiveJobs      []string `json:"active_jobs,omitempty"`
	ThroughputFPS   float64  `json:"throughput_fps"`
	LastSeen        string   `json:"last_seen"`
}

// WorkerInfo represents worker information for API responses
type WorkerInfo struct {
	ID            string        `json:"id"`
	Name          string        `json:"name,omitempty"`
	Status        string        `json:"status"`
	Evicted       bool          `json:"evicted"`
	GPUModel      string        `json:"gpu_model,omitempty"`
	Encoders      []string      `json:"encoders"`
	Decoders      []string      `json:"decoders,omitempty"`
	FFmpegVersion string        `json:"ffmpeg_version"`
	MaxConcurrent int           `json:"max_concurrent"`
	LastHeartbeat string        `json:"last_heartbeat"`
	CreatedAt     string        `json:"created_at"`
	Hwaccels      string        `json:"hwaccels,omitempty"`
	Codecs        string        `json:"codecs,omitempty"`
	Filters       string        `json:"filters,omitempty"`
	PixFmts       string        `json:"pix_fmts,omitempty"`
	Formats       string        `json:"formats,omitempty"`
	Health        *WorkerHealth `json:"health"`
}

// ListWorkersResponse is the response for listing workers
type ListWorkersResponse struct {
	Workers []WorkerInfo `json:"workers"`
}

// GetWorkerResponse is the response for getting a single worker
type GetWorkerResponse struct {
	Worker WorkerInfo `json:"worker"`
}

// ListWorkersByEncoderResponse is the response for listing workers by encoder
type ListWorkersByEncoderResponse struct {
	Encoder string       `json:"encoder"`
	Workers []WorkerInfo `json:"workers"`
}

// ListEncodersResponse is the response for listing all encoders
type ListEncodersResponse struct {
	Encoders []protocol.EncoderInfo `json:"encoders"`
}

// ListDecodersResponse is the response for listing all decoders
type ListDecodersResponse struct {
	Decoders []protocol.DecoderInfo `json:"decoders"`
}

// ListHwaccelsResponse is the response for listing all hwaccels in JSON mode
type ListHwaccelsResponse struct {
	Hwaccels []string `json:"hwaccels"`
}

// Outputs plain text by default (matching ffmpeg -codecs format);
// use ?json=1 for structured JSON output.
func (h *Handler) ListAllCodecs(w http.ResponseWriter, r *http.Request) {
	items, err := h.db.GetAllCodecs()
	if err != nil {
		writeError(w, http.StatusInternalServerError, protocol.NewProtocolError(
			protocol.ErrCodeInternalError, "Failed to get codecs", err,
		))
		return
	}

	if r.URL.Query().Get("json") == "1" {
		writeJSON(w, http.StatusOK, InfoFlagResponse{Items: items})
		return
	}

	writeInfoFlagText(w, "Codecs", items)
}

// ListAllFilters handles listing all unique filters across all workers.
// Outputs plain text by default (matching ffmpeg -filters format);
// use ?json=1 for structured JSON output.
func (h *Handler) ListAllFilters(w http.ResponseWriter, r *http.Request) {
	items, err := h.db.GetAllFilters()
	if err != nil {
		writeError(w, http.StatusInternalServerError, protocol.NewProtocolError(
			protocol.ErrCodeInternalError, "Failed to get filters", err,
		))
		return
	}

	if r.URL.Query().Get("json") == "1" {
		writeJSON(w, http.StatusOK, InfoFlagResponse{Items: items})
		return
	}

	writeInfoFlagText(w, "Filters", items)
}

// ListAllPixFmts handles listing all unique pixel formats across all workers.
// Outputs plain text by default (matching ffmpeg -pix_fmts format);
// use ?json=1 for structured JSON output.
func (h *Handler) ListAllPixFmts(w http.ResponseWriter, r *http.Request) {
	items, err := h.db.GetAllPixFmts()
	if err != nil {
		writeError(w, http.StatusInternalServerError, protocol.NewProtocolError(
			protocol.ErrCodeInternalError, "Failed to get pixel formats", err,
		))
		return
	}

	if r.URL.Query().Get("json") == "1" {
		writeJSON(w, http.StatusOK, InfoFlagResponse{Items: items})
		return
	}

	writeInfoFlagText(w, "Pixel formats", items)
}

// ListAllFormats handles listing all unique formats across all workers.
// Outputs plain text by default (matching ffmpeg -formats format);
// use ?json=1 for structured JSON output.
func (h *Handler) ListAllFormats(w http.ResponseWriter, r *http.Request) {
	items, err := h.db.GetAllFormats()
	if err != nil {
		writeError(w, http.StatusInternalServerError, protocol.NewProtocolError(
			protocol.ErrCodeInternalError, "Failed to get formats", err,
		))
		return
	}

	if r.URL.Query().Get("json") == "1" {
		writeJSON(w, http.StatusOK, InfoFlagResponse{Items: items})
		return
	}

	writeInfoFlagText(w, "File formats", items)
}

// InfoFlagResponse is the JSON response for info flag endpoints (codecs/filters/pix_fmts/formats).
type InfoFlagResponse struct {
	Items []string `json:"items"`
}

// writeInfoFlagText writes a plain-text response in the ffmpeg info flag format.
// Header format: "Header:\nitem1\nitem2\n..."
func writeInfoFlagText(w http.ResponseWriter, header string, items []string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	// Build output: header + newline + items
	var sb strings.Builder
	sb.WriteString(header)
	sb.WriteString(":\n")
	for _, item := range items {
		sb.WriteString(item)
		sb.WriteByte('\n')
	}
	w.Write([]byte(sb.String()))
}

// dbWorkerToWorkerInfo converts database Worker to WorkerInfo.
// states may be nil; when provided, sampled live metrics (GPU, throughput,
// active jobs) come from the worker state table, while health.status always
// reflects the authoritative database state (TSI-2347).
func dbWorkerToWorkerInfo(worker *db.Worker, states map[string]*protocol.WorkerState) WorkerInfo {
	var encoders, decoders []string
	if err := json.Unmarshal([]byte(worker.Encoders), &encoders); err != nil {
		log.Printf("Failed to unmarshal encoders for worker %s: %v", worker.ID, err)
	}
	if err := json.Unmarshal([]byte(worker.Decoders), &decoders); err != nil {
		log.Printf("Failed to unmarshal decoders for worker %s: %v", worker.ID, err)
	}

	info := WorkerInfo{
		ID:            worker.ID,
		Name:          worker.Name,
		Status:        string(worker.Status),
		Evicted:       worker.Evicted,
		Encoders:      encoders,
		Decoders:      decoders,
		FFmpegVersion: worker.FFmpegVersion,
		MaxConcurrent: worker.MaxConcurrent,
		LastHeartbeat: worker.LastHeartbeat.Format(time.RFC3339),
		CreatedAt:     worker.CreatedAt.Format(time.RFC3339),
		Hwaccels:      worker.Hwaccels,
		Codecs:        worker.Codecs,
		Filters:       worker.Filters,
		PixFmts:       worker.PixFmts,
		Formats:       worker.Formats,
	}

	if worker.GPUModel.Valid {
		info.GPUModel = worker.GPUModel.String
	}

	// TSI-2347: health.status must track the worker's authoritative DB state,
	// which the scheduler, the terminal-status hook and heartbeats all keep
	// current. The state table only refreshes on periodic heartbeats, so
	// reading status from it left health stuck at "idle" between beats while a
	// job was running. The state table still supplies sampled live metrics.
	health := WorkerHealth{
		Status:     string(worker.Status),
		LastSeen:   worker.LastHeartbeat.Format(time.RFC3339),
		ActiveJobs: []string{},
	}
	if state, ok := states[worker.ID]; ok && state != nil && len(state.ActiveJobs) > 0 && worker.Status == protocol.WorkerStatusIdle {
		// Heartbeats no longer write the reported status into the DB
		// (status is derived from active job count), but a busy heartbeat's
		// ActiveJobs sample still proves activity: surface busy immediately.
		health.Status = string(protocol.WorkerStatusBusy)
	}
	if state, ok := states[worker.ID]; ok && state != nil {
		health.GPUUtilPct = state.GPUUtilPct
		health.GPUMemUsedMB = state.GPUMemUsedMB
		health.GPUMetricsValid = state.GPUMetricsValid
		health.ThroughputFPS = state.ThroughputFPS
		health.ActiveJobs = state.ActiveJobs
		// !Before (not After): when the two timestamps are equal, prefer the
		// fresher state-table sample instead of falling back to the DB record.
		if !state.LastSeen.Before(worker.LastHeartbeat) {
			health.LastSeen = state.LastSeen.Format(time.RFC3339)
		}
	}
	info.Health = &health

	return info
}
