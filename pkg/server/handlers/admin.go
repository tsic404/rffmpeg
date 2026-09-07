package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/tsic404/rffmpeg/pkg/protocol"
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
