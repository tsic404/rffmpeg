package handlers

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/tsic404/rffmpeg/pkg/protocol"
	"github.com/tsic404/rffmpeg/pkg/server/db"
)

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

// WorkerHeartbeat handles worker heartbeat (enhanced with GPU and throughput metrics).
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

			if worker, gErr := h.db.GetWorker(req.WorkerID); gErr == nil && worker.Evicted {
				writeError(w, http.StatusConflict, protocol.NewProtocolError(
					protocol.ErrCodeWorkerEvicted, "Worker is evicted from scheduling", nil,
				))
				return
			}
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

	if h.stateTable != nil {
		jobsPerSec := req.JobsPerSec
		if h.throughputScaler.Enabled() {

			workerName := ""
			if worker, err := h.db.GetWorker(req.WorkerID); err == nil {
				workerName = worker.Name
			}
			jobsPerSec = h.throughputScaler.Scale(req.WorkerID, workerName, jobsPerSec)
		}
		statePayload := protocol.WorkerHeartbeatPayload{
			WorkerID:        req.WorkerID,
			Status:          string(req.Status),
			ActiveJobs:      req.ActiveJobs,
			JobsPerSec:      jobsPerSec,
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
	if worker.Evicted {
		writeError(w, http.StatusConflict, protocol.NewProtocolError(
			protocol.ErrCodeWorkerEvicted, "Worker is evicted from scheduling", nil,
		))
		return
	}
	if worker.Status == protocol.WorkerStatusOffline {
		writeError(w, http.StatusConflict, protocol.NewProtocolError(
			protocol.ErrCodeConflict, "Worker is offline; re-register to resume pulling", nil,
		))
		return
	}

	maxConcurrent := worker.MaxConcurrent
	if maxConcurrent <= 0 {
		maxConcurrent = h.maxJobsPerWorker
	}

	jobs, err := h.db.AssignPendingJobsToWorker(workerID, maxConcurrent)
	if err != nil {
		writeError(w, http.StatusInternalServerError, protocol.NewProtocolError(
			protocol.ErrCodeInternalError, "Failed to assign jobs", err,
		))
		return
	}

	jobInfos := make([]protocol.JobInfo, len(jobs))
	jobIDs := make([]string, len(jobs))
	for i, job := range jobs {
		jobIDs[i] = job.ID
	}

	retryCounts := h.jobRetryCounts(jobIDs)
	for i, job := range jobs {
		jobInfos[i] = h.dbJobToJobInfo(job, false, retryCounts)
	}

	writeJSON(w, http.StatusOK, protocol.WorkerJobPullResponse{
		Jobs: jobInfos,
	})
}

// ListWorkers handles listing all workers with their capabilities.
// GET /api/v1/workers
// Query param active_only=true returns only non-offline workers. The default
// list includes offline rows that are still retained within the
// --worker-offline-threshold window after a worker dies.
func (h *Handler) ListWorkers(w http.ResponseWriter, r *http.Request) {

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

	var clusterMedian float64
	if h.stateTable != nil {
		clusterMedian = h.stateTable.ClusterMedian()
	}

	writeJSON(w, http.StatusOK, ListWorkersResponse{
		Workers:                 response,
		ClusterMedianThroughput: clusterMedian,
	})
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

// WorkerHealth carries live runtime metrics for a worker.
// Populated from the server's worker state table when at least one heartbeat
// has been received; otherwise derived from the database record.
type WorkerHealth struct {
	Status string `json:"status"`
	// gpu_* are emitted unconditionally: omitempty hid a legitimate 0% reading
	// and left clients unable to tell "GPU idle" from "not sampled".
	GPUUtilPct      float64  `json:"gpu_util_percent"`
	GPUMemUsedMB    int      `json:"gpu_mem_used_mb"`
	GPUMetricsValid bool     `json:"gpu_metrics_valid"` // True when the GPU fields carry a fresh sample; false means stale/no sample (any GPU source)
	ActiveJobs      []string `json:"active_jobs,omitempty"`
	JobsPerSec      float64  `json:"jobs_per_sec"`
	EWMAJobsPerSec  float64  `json:"ewma_jobs_per_sec"` // EWMA-smoothed throughput (jobs/sec); 0 until the first heartbeat sample
	// CompletedJobs is cumulative since the current registration — the count
	// the slow-node warmup gate compares against. Emitted unconditionally: a
	// worker below the gate reports 0, so omitting the key hid the gate state.
	CompletedJobs int    `json:"completed_jobs"`
	LastSeen      string `json:"last_seen"`
}

// WorkerInfo represents worker information for API responses
type WorkerInfo struct {
	ID            string                   `json:"id"`
	Name          string                   `json:"name,omitempty"`
	Status        string                   `json:"status"`
	Evicted       bool                     `json:"evicted"`
	GPUModel      string                   `json:"gpu_model,omitempty"`
	GPUDevices    []protocol.GPUDeviceInfo `json:"gpu_devices,omitempty"`
	Encoders      []string                 `json:"encoders"`
	Decoders      []string                 `json:"decoders,omitempty"`
	FFmpegVersion string                   `json:"ffmpeg_version"`
	MaxConcurrent int                      `json:"max_concurrent"`
	LastHeartbeat string                   `json:"last_heartbeat"`
	CreatedAt     string                   `json:"created_at"`
	Hwaccels      string                   `json:"hwaccels,omitempty"`
	Codecs        string                   `json:"codecs,omitempty"`
	Filters       string                   `json:"filters,omitempty"`
	PixFmts       string                   `json:"pix_fmts,omitempty"`
	Formats       string                   `json:"formats,omitempty"`
	Health        *WorkerHealth            `json:"health"`
}

// ListWorkersResponse is the response for listing workers
type ListWorkersResponse struct {
	Workers []WorkerInfo `json:"workers"`
	// ClusterMedianThroughput is the cluster median of EWMA throughput across
	// eligible workers (workerhealth.WorkerStateTable.ClusterMedian); 0 when
	// fewer than two eligible samples exist.
	ClusterMedianThroughput float64 `json:"cluster_median_throughput"`
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

// dbWorkerToWorkerInfo converts database Worker to WorkerInfo.
// states may be nil; when provided, sampled live metrics (GPU, throughput,
// active jobs) come from the worker state table, while health.status always
// reflects the authoritative database state.
func dbWorkerToWorkerInfo(worker *db.Worker, states map[string]*protocol.WorkerState) WorkerInfo {
	var encoders, decoders []string
	if err := json.Unmarshal([]byte(worker.Encoders), &encoders); err != nil {
		log.Printf("Failed to unmarshal encoders for worker %s: %v", worker.ID, err)
	}
	if err := json.Unmarshal([]byte(worker.Decoders), &decoders); err != nil {
		log.Printf("Failed to unmarshal decoders for worker %s: %v", worker.ID, err)
	}
	var gpuDevices []protocol.GPUDeviceInfo
	if err := json.Unmarshal([]byte(worker.GPUDevices), &gpuDevices); err != nil {
		log.Printf("Failed to unmarshal gpu_devices for worker %s: %v", worker.ID, err)
	}

	info := WorkerInfo{
		ID:            worker.ID,
		Name:          worker.Name,
		Status:        string(worker.Status),
		Evicted:       worker.Evicted,
		Encoders:      encoders,
		Decoders:      decoders,
		GPUDevices:    gpuDevices,
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

	health := WorkerHealth{
		Status:     string(worker.Status),
		LastSeen:   worker.LastHeartbeat.Format(time.RFC3339),
		ActiveJobs: []string{},
	}
	if state, ok := states[worker.ID]; ok && state != nil && len(state.ActiveJobs) > 0 && worker.Status == protocol.WorkerStatusIdle {

		health.Status = string(protocol.WorkerStatusBusy)
	}
	if state, ok := states[worker.ID]; ok && state != nil {
		health.GPUUtilPct = state.GPUUtilPct
		health.GPUMemUsedMB = state.GPUMemUsedMB
		health.GPUMetricsValid = state.GPUMetricsValid
		health.JobsPerSec = state.JobsPerSec
		health.EWMAJobsPerSec = state.EWMAJobsPerSec
		health.CompletedJobs = state.CompletedJobs
		health.ActiveJobs = state.ActiveJobs

		if !state.LastSeen.Before(worker.LastHeartbeat) {
			health.LastSeen = state.LastSeen.Format(time.RFC3339)
		}
	}
	info.Health = &health

	return info
}
