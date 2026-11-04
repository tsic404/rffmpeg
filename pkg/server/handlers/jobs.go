package handlers

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/tsic404/rffmpeg/pkg/pathutil"
	"github.com/tsic404/rffmpeg/pkg/protocol"
	"github.com/tsic404/rffmpeg/pkg/server/auth"
	"github.com/tsic404/rffmpeg/pkg/server/db"
	"github.com/tsic404/rffmpeg/pkg/server/scheduler"
	"github.com/tsic404/rffmpeg/pkg/server/websocket"
)

// SubmitJob handles job submission
func (h *Handler) SubmitJob(w http.ResponseWriter, r *http.Request) {

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

	for _, path := range req.DirectPath {
		if pathutil.ContainsPathTraversal(path) {
			writeError(w, http.StatusBadRequest, protocol.NewProtocolError(
				protocol.ErrCodeInvalidRequest,
				"direct path contains '..' traversal: "+path, nil,
			))
			return
		}
	}

	if len(req.DirectPath) > 0 && req.OutputFilename != "-" && !pathutil.IsRemoteURL(req.OutputFilename) {
		if pathutil.ContainsPathTraversal(req.OutputFilename) {
			writeError(w, http.StatusBadRequest, protocol.NewProtocolError(
				protocol.ErrCodeInvalidRequest,
				"output filename contains '..' traversal: "+req.OutputFilename, nil,
			))
			return
		}
	}

	if len(req.DirectPath) == 0 {
		for _, fileID := range req.InputFiles {

			if strings.Contains(fileID, "://") {
				continue
			}

			exists, err := h.storage.TouchFile(fileID)
			if err != nil || !exists {
				writeError(w, http.StatusBadRequest, protocol.NewProtocolError(
					protocol.ErrCodeNotFound, "Input file not found: "+fileID, err,
				))
				return
			}
		}
	}

	argsJSON, err := json.Marshal(req.Args)
	if err != nil {
		writeError(w, http.StatusInternalServerError, protocol.NewProtocolError(
			protocol.ErrCodeInternalError, "Failed to marshal args", err,
		))
		return
	}

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

	if requestedEncoder != "" {

		workersWithEncoder, err := h.db.GetLiveSchedulableWorkersByEncoder(requestedEncoder, h.heartbeatTimeout)
		if err != nil {
			log.Printf("SubmitJob: Failed to check workers by encoder %s: %v", requestedEncoder, err)

		} else if len(workersWithEncoder) > 0 {

			goto jobCreate
		}

		compatibleEncoders := scheduler.GetCompatibleEncoders(requestedEncoder)
		hasCompatibleWorker := false
		for _, enc := range compatibleEncoders {
			if enc == requestedEncoder {
				continue
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

				writeError(w, http.StatusServiceUnavailable, protocol.NewProtocolError(
					protocol.ErrCodeWorkerUnavailable,
					"No workers available. Please ensure at least one worker is registered and online.",
					nil,
				))
				return
			}

			errMsg := fmt.Sprintf("No worker available with encoder: %s (or compatible encoders)", requestedEncoder)
			job, createErr := h.db.CreateFailedJob(string(inputFilesJSON), string(argsJSON), req.OutputFilename, req.AutoHW, req.StreamingOutput, req.Timeout, directPathsJSON, string(protocol.FailureEncoderUnavailable), errMsg)
			if createErr != nil {

				writeCreateJobError(w, createErr)
				return
			}

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

	liveWorkers, err := h.db.GetLiveSchedulableWorkers(h.heartbeatTimeout)
	if err != nil {
		log.Printf("SubmitJob: Failed to check schedulable workers: %v", err)

	} else if len(liveWorkers) == 0 {
		writeError(w, http.StatusServiceUnavailable, protocol.NewProtocolError(
			protocol.ErrCodeWorkerUnavailable,
			"No workers available. Please ensure at least one worker is registered and online.",
			nil,
		))
		return
	}

	job, err := h.db.CreateJobWithStreaming(string(inputFilesJSON), string(argsJSON), req.OutputFilename, req.AutoHW, req.StreamingOutput, req.Timeout, directPathsJSON)
	if err != nil {

		writeCreateJobError(w, err)
		return
	}

	if clientID := auth.GetClientID(r); clientID != "" {
		h.rateLimiter.RegisterJob(clientID, job.ID)
	}

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
// worker exists but none of them
// has the requested encoder or a compatible one. A workerless cluster is
// NO_WORKER_AVAILABLE, not an unavailable encoder.
func (h *Handler) failAsEncoderUnavailable(requestedEncoder string, compatibleEncoders []string) bool {

	all, err := h.db.GetSchedulableWorkers()
	if err != nil {
		log.Printf("SubmitJob: Failed to list schedulable workers: %v", err)
		return false
	}
	if len(all) == 0 {
		return false
	}

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
	jobInfo := h.dbJobToJobInfo(job, noLiveWorker, h.jobRetryCounts([]string{job.ID}))
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
	jobIDs := make([]string, len(jobs))
	for i, job := range jobs {
		jobIDs[i] = job.ID
	}
	retryCounts := h.jobRetryCounts(jobIDs)
	jobInfos := make([]protocol.JobInfo, len(jobs))
	for i, job := range jobs {
		jobInfos[i] = h.dbJobToJobInfo(job, noLiveWorker, retryCounts)
	}

	writeJSON(w, http.StatusOK, protocol.JobListResponse{Jobs: jobInfos})
}

// CancelJob handles job cancellation
func (h *Handler) CancelJob(w http.ResponseWriter, r *http.Request) {
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

	if err := h.wsHub.BroadcastStatus(jobID, protocol.JobStatusCancelled, 0, "Job cancelled"); err != nil {
		log.Printf("Failed to broadcast cancellation for job %s: %v", jobID, err)
	}

	if job.Status == protocol.JobStatusRunning {
		if err := h.wsHub.BroadcastComplete(jobID, 0); err != nil {
			log.Printf("Failed to broadcast completion for cancelled job %s: %v", jobID, err)
		}
	}

	h.wsHub.ReleaseJobStream(jobID)

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

	workerID := db.NormalizeWorkerID(req.WorkerID)
	if workerID == "" {
		writeError(w, http.StatusBadRequest, protocol.NewProtocolError(
			protocol.ErrCodeInvalidRequest, "worker_id is required", nil,
		))
		return
	}

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

	if req.Status != "" {

		if req.FailureType != "" && !protocol.FailureType(req.FailureType).IsValid() {
			writeError(w, http.StatusBadRequest, protocol.NewProtocolError(
				protocol.ErrCodeInvalidRequest, "Invalid failure_type: "+req.FailureType, nil,
			))
			return
		}

		var failureType, failureDetails *string
		if req.Status == protocol.JobStatusCompleted {

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
		if protocol.IsTerminalStatus(req.Status) {

			err = h.db.UpdateJobTerminalStatusWithOwnerAndCache(jobID, workerID, req.Status, exitCode, errMsg, failureType, failureDetails, req.Cached)
		} else {

			err = h.db.UpdateJobRunningStatusWithOwner(jobID, workerID, exitCode, errMsg, failureType, failureDetails)
		}
		if err != nil {
			if errors.Is(err, protocol.ErrJobNotFound) {
				writeError(w, http.StatusNotFound, protocol.NewProtocolError(
					protocol.ErrCodeNotFound, "Job not found", err,
				))
				return
			}
			if errors.Is(err, protocol.ErrJobTerminal) {

				writeError(w, http.StatusConflict, protocol.NewProtocolError(
					protocol.ErrCodeConflict, "Job already in terminal state", err,
				))
				return
			}
			if errors.Is(err, protocol.ErrJobNotOwned) {

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

					if h.scheduler != nil {
						h.scheduler.TriggerReschedule()
					}
				}
			}
		}
	}

	if req.StderrChunk != "" {
		if err := h.wsHub.BroadcastStderr(jobID, req.StderrChunk); err != nil {
			log.Printf("Failed to broadcast stderr for job %s: %v", jobID, err)
		}
	}

	if req.StdoutChunk != "" {
		if err := h.wsHub.BroadcastStdout(jobID, req.StdoutChunk); err != nil {
			log.Printf("Failed to broadcast stdout for job %s: %v", jobID, err)
		}
	}

	if req.Progress > 0 || req.EtaSeconds > 0 {

		accepted, err := h.db.UpdateJobProgressWithOwner(jobID, workerID, req.Progress, req.EtaSeconds)
		if err != nil {
			if errors.Is(err, protocol.ErrJobNotOwned) {
				writeError(w, http.StatusConflict, protocol.NewProtocolError(
					protocol.ErrCodeConflict, "Job no longer assigned to this worker", err,
				))
				return
			}
			if errors.Is(err, protocol.ErrJobNotFound) {
				writeError(w, http.StatusNotFound, protocol.NewProtocolError(
					protocol.ErrCodeNotFound, "Job not found", err,
				))
				return
			}
			log.Printf("Failed to update job progress for job %s: %v", jobID, err)
		}

		if accepted || err != nil {
			if err := h.wsHub.BroadcastProgress(jobID, req.Progress, req.TimeUs, req.DurationUs, req.Speed, req.EtaSeconds); err != nil {
				log.Printf("Failed to broadcast progress for job %s: %v", jobID, err)
			}
		}
	}

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

// jobRetryCounts returns the worker-failure migration counts for the given
// jobs in one batched query, retrying once on a transient DB error before
// degrading to nil. A nil result is treated by callers as "no count
// available", so a degraded read never silently rewrites a migrated job's
// retry_count back to 0.
func (h *Handler) jobRetryCounts(jobIDs []string) map[string]int {
	retryCounts, err := h.db.GetJobRetryCounts(jobIDs)
	if err != nil {
		log.Printf("Failed to get job retry counts (attempt 1): %v", err)

		retryCounts, err = h.db.GetJobRetryCounts(jobIDs)
		if err != nil {
			log.Printf("Failed to get job retry counts after retry; serving %d job(s) without retry_count: %v", len(jobIDs), err)
			return nil
		}
	}
	return retryCounts
}

// dbJobToJobInfo converts database Job to protocol JobInfo. retryCounts is a
// precomputed map of worker-failure migration counts keyed by job ID; a nil
// map (lookup failure) leaves the count at its zero value.
func (h *Handler) dbJobToJobInfo(job *db.Job, noLiveWorker bool, retryCounts map[string]int) protocol.JobInfo {
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

	if retryCounts != nil {
		info.RetryCount = retryCounts[job.ID]
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
	if job.ClaimedAt.Valid {
		info.ClaimedAt = &job.ClaimedAt.Time
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
