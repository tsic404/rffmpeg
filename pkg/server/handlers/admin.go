package handlers

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/tsic404/rffmpeg/pkg/protocol"
	"github.com/tsic404/rffmpeg/pkg/server/db"
	"github.com/tsic404/rffmpeg/pkg/server/ratelimit"
)

// AdminHandler handles administrative endpoints.
type AdminHandler struct {
	handler      *Handler
	rateLimitCfg *ratelimit.RuntimeConfig
}

// NewAdminHandler creates a new admin handler.
func NewAdminHandler(h *Handler, rateLimitCfg *ratelimit.RuntimeConfig) *AdminHandler {
	return &AdminHandler{
		handler:      h,
		rateLimitCfg: rateLimitCfg,
	}
}

// rateLimitConfigPatch is the request body for PATCH /admin/config.
type rateLimitConfigPatch struct {
	RateLimitEnabled           *bool `json:"rate_limit_enabled"`
	MaxConcurrentJobsPerClient *int  `json:"max_concurrent_jobs_per_client"`
}

// UpdateConfig handles PATCH /admin/config — runtime config update.
func (a *AdminHandler) UpdateConfig(w http.ResponseWriter, r *http.Request) {
	var patch rateLimitConfigPatch
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		writeError(w, http.StatusBadRequest, protocol.NewProtocolError(
			protocol.ErrCodeInvalidRequest, "Invalid JSON body", err,
		))
		return
	}

	if patch.RateLimitEnabled != nil {
		a.rateLimitCfg.SetEnabled(*patch.RateLimitEnabled)
	}
	if patch.MaxConcurrentJobsPerClient != nil {
		if *patch.MaxConcurrentJobsPerClient > 0 {
			a.rateLimitCfg.SetLimit(*patch.MaxConcurrentJobsPerClient)
		}
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"rate_limit_enabled":             a.rateLimitCfg.IsEnabled(),
		"max_concurrent_jobs_per_client": a.rateLimitCfg.GetLimit(),
	})
}

// GetConfig handles GET /admin/config — returns current runtime config.
func (a *AdminHandler) GetConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"rate_limit_enabled":             a.rateLimitCfg.IsEnabled(),
		"max_concurrent_jobs_per_client": a.rateLimitCfg.GetLimit(),
	})
}

// metricsResponse is the response for GET /admin/metrics.
type metricsResponse struct {
	TotalActiveJobs int   `json:"total_active_jobs"`
	RejectedCount   int64 `json:"rejected_count"`
}

// GetMetrics handles GET /admin/metrics — exposes rate limit statistics.
func (a *AdminHandler) GetMetrics(w http.ResponseWriter, r *http.Request) {
	counter := a.handler.GetRateLimiter()
	writeJSON(w, http.StatusOK, metricsResponse{
		TotalActiveJobs: counter.TotalActive(),
		RejectedCount:   counter.RejectedCount(),
	})
}

// clearEvictionRequest is the body for POST /admin/workers/eviction/clear.
type clearEvictionRequest struct {
	Worker string `json:"worker"` // worker identifier or registered name
}

// writeClearEvictionFailure maps a manual-clear failure onto the protocol
// response. A worker row can disappear between the lookup and the clear (the
// monitor's offline sweep removes it), which is still an unknown object for
// this request: 404 not_found, never a 500.
func writeClearEvictionFailure(w http.ResponseWriter, err error) {
	if errors.Is(err, protocol.ErrWorkerNotFound) {
		writeError(w, http.StatusNotFound, protocol.NewProtocolError(
			protocol.ErrCodeNotFound, "Worker not found", err,
		))
		return
	}
	writeError(w, http.StatusInternalServerError, protocol.NewProtocolError(
		protocol.ErrCodeInternalError, "Failed to clear worker eviction", err,
	))
}

// clearEvictionResponse reports the addressed object and the release outcome.
// cleared is false when the object carried no mark, which is a successful
// no-op (no audit row).
type clearEvictionResponse struct {
	WorkerID   string `json:"worker_id"`
	WorkerName string `json:"worker_name,omitempty"`
	Evicted    bool   `json:"evicted"`
	Cleared    bool   `json:"cleared"`
}

// ClearWorkerEviction handles POST /admin/workers/eviction/clear — releases the
// eviction mark of a single worker, located by identifier or registered name.
// The mark is the only thing released: registration/pull restrictions lift
// with it, the cumulative completion count stays untouched, and a worker whose
// throughput is still below the threshold is re-evicted by the next valid
// judgement.
func (a *AdminHandler) ClearWorkerEviction(w http.ResponseWriter, r *http.Request) {
	var req clearEvictionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, protocol.NewProtocolError(
			protocol.ErrCodeInvalidRequest, "Invalid JSON body", err,
		))
		return
	}
	ref := strings.TrimSpace(req.Worker)
	if ref == "" {
		writeError(w, http.StatusBadRequest, protocol.NewProtocolError(
			protocol.ErrCodeInvalidRequest, "Worker identifier or name is required", nil,
		))
		return
	}

	worker, err := a.handler.db.GetWorkerByReference(ref)
	if err != nil {
		if errors.Is(err, protocol.ErrWorkerNotFound) {
			writeError(w, http.StatusNotFound, protocol.NewProtocolError(
				protocol.ErrCodeNotFound, "Worker not found", err,
			))
			return
		}
		writeError(w, http.StatusInternalServerError, protocol.NewProtocolError(
			protocol.ErrCodeInternalError, "Failed to look up worker", err,
		))
		return
	}

	// Audit the decision with the values visible at the moment of the manual
	// clear, so the row stays comparable with automatic recoveries.
	var throughput, clusterMedian float64
	if st := a.handler.stateTable; st != nil {
		clusterMedian = st.ClusterMedian()
		if state, ok := st.Get(worker.ID); ok {
			throughput = state.EWMAJobsPerSec
		}
	}

	cleared, err := a.handler.db.ClearWorkerEviction(
		worker.ID, throughput, clusterMedian, db.EvictionReasonManualClear,
	)
	if err != nil {
		writeClearEvictionFailure(w, err)
		return
	}

	// Drop the in-memory mark too: a stale mark would keep the next judgement
	// on the recovery branch, so a still-slow worker could never be re-evicted.
	if a.handler.stateTable != nil {
		a.handler.stateTable.ClearEviction(worker.ID)
	}

	writeJSON(w, http.StatusOK, clearEvictionResponse{
		WorkerID:   worker.ID,
		WorkerName: worker.Name,
		Evicted:    false,
		Cleared:    cleared,
	})
}
