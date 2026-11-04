package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/tsic404/rffmpeg/pkg/protocol"
	"github.com/tsic404/rffmpeg/pkg/server/auth"
	"github.com/tsic404/rffmpeg/pkg/server/db"
	"github.com/tsic404/rffmpeg/pkg/server/panicguard"
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
	// throughputScaler is the slow-worker eviction fault injection: when set,
	// heartbeat throughput is scaled for the workers it targets. nil (the
	// default) records heartbeats verbatim.
	throughputScaler *workerhealth.ThroughputScaler
	scheduler        *scheduler.Scheduler // scheduler reference for immediate job assignment
	authToken        string               // Non-empty when auth is configured
	// heartbeatTimeout mirrors the worker health monitor's knob: submit-time
	// fail-fast treats workers whose last heartbeat is older than this as dead
	// <=0 disables the freshness check.
	heartbeatTimeout time.Duration
	// maxJobsPerWorker mirrors the scheduler's MaxJobsPerWorker fallback:
	// when a worker reports MaxConcurrent <= 0 (unset), the pull path bounds
	// new claims by this value instead.
	maxJobsPerWorker int
	// noWorkerJobTimeout and timeoutCheckInterval mirror the scheduler's
	// starvation knobs so GetJob/PullWorkerJobs can attach NoWorkerDeadline
	// to unassigned pending jobs only when the cluster has no live
	// schedulable worker (the exact filter the sweep fails). <=0
	// noWorkerJobTimeout disables the deadline.
	noWorkerJobTimeout   time.Duration
	timeoutCheckInterval time.Duration
	// multipartTmpDir is the filesystem ensureMultipartSpace pre-flights
	// against. It does NOT control where the body actually spills — that
	// follows os.TempDir()/$TMPDIR, which mime/multipart uses directly.
	// main keeps the two in sync; see SetMultipartTmpDir.
	multipartTmpDir string
}

// New creates a new Handler
func New(database *db.Database, store *storage.Storage, version string, stateTable *workerhealth.WorkerStateTable) *Handler {
	return &Handler{
		db:               database,
		storage:          store,
		version:          version,
		wsHub:            websocket.NewHubWithSeqStore(database),
		rateLimiter:      rateLimiterWithTerminalHook(database),
		stateTable:       stateTable,
		maxJobsPerWorker: 1,
	}
}

// NewWithHub creates a new Handler with an existing WebSocket hub
func NewWithHub(database *db.Database, store *storage.Storage, version string, hub *websocket.Hub, stateTable *workerhealth.WorkerStateTable) *Handler {
	return &Handler{
		db:               database,
		storage:          store,
		version:          version,
		wsHub:            hub,
		rateLimiter:      rateLimiterWithTerminalHook(database),
		stateTable:       stateTable,
		maxJobsPerWorker: 1,
	}
}

// rateLimiterWithTerminalHook builds the per-client counter and binds quota
// release to the database's terminal notification, so a job that finishes on
// any path (worker report, cancel, scheduler/monitor failure) frees its slot
// exactly once and later submits from the same client are not 429'd.
func rateLimiterWithTerminalHook(database *db.Database) *ratelimit.InMemoryCounter {
	counter := ratelimit.NewInMemoryCounter()
	bindQuotaRelease(database, counter)
	return counter
}

// bindQuotaRelease routes every terminal job to the counter's per-job release.
// DecrementByJob forgets the job mapping, so a repeated terminal write for the
// same job releases at most one slot.
func bindQuotaRelease(database *db.Database, counter ratelimit.ClientJobCounter) {
	if database == nil {
		return
	}
	database.SetTerminalHook(func(jobID string) { counter.DecrementByJob(jobID) })
}

// SetRateLimiter sets the rate limiter counter (used when a custom counter is needed).
func (h *Handler) SetRateLimiter(counter ratelimit.ClientJobCounter) {
	h.rateLimiter = counter
	bindQuotaRelease(h.db, counter)
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

// GetStateTable returns the worker state table
func (h *Handler) GetStateTable() *workerhealth.WorkerStateTable {
	return h.stateTable
}

// SetThroughputScaler installs the slow-worker eviction fault injection.
// Without it heartbeats are recorded verbatim.
func (h *Handler) SetThroughputScaler(scaler *workerhealth.ThroughputScaler) {
	h.throughputScaler = scaler
}

// workerStateMap returns the worker state table contents keyed by canonical
// worker ID. Returns nil when no state table is configured. The key is
// canonicalized like the table itself, so a state entry built from a compact or
// upper-case heartbeat still joins the database row with the same identity.
func (h *Handler) workerStateMap() map[string]*protocol.WorkerState {
	if h.stateTable == nil {
		return nil
	}
	states := make(map[string]*protocol.WorkerState)
	for _, s := range h.stateTable.GetAll() {
		state := s
		states[db.NormalizeWorkerID(s.WorkerID)] = &state
	}
	return states
}

// SetScheduler sets the scheduler reference
func (h *Handler) SetScheduler(sched *scheduler.Scheduler) {
	h.scheduler = sched
}

// SetHeartbeatTimeout configures the submit-time worker freshness window
// Should match ServerConfig.WorkerHeartbeatTimeout.
func (h *Handler) SetHeartbeatTimeout(d time.Duration) {
	h.heartbeatTimeout = d
}

// SetMaxJobsPerWorker mirrors the scheduler's MaxJobsPerWorker so the pull
// path applies the same fallback when a worker reports MaxConcurrent <= 0.
func (h *Handler) SetMaxJobsPerWorker(maxJobs int) {
	h.maxJobsPerWorker = maxJobs
}

// SetMultipartTmpDir sets the directory ensureMultipartSpace pre-flights
// against. It does not change where the body actually spills: mime/multipart
// hardcodes os.CreateTemp("", ...) → os.TempDir() → $TMPDIR. Callers must set
// the process TMPDIR to the same path (main does); otherwise the pre-flight
// and the real spill silently disagree.
func (h *Handler) SetMultipartTmpDir(dir string) {
	h.multipartTmpDir = dir
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

	ctx := context.WithValue(r.Context(), auth.ClientIDKey, clientID)
	*r = *r.WithContext(ctx)

	return clientID, true
}

// StartWSHub starts the WebSocket hub
func (h *Handler) StartWSHub() {
	go panicguard.Guard("websocket hub loop", h.wsHub.Run)
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

// writeCreateJobError classifies a job-INSERT failure: a transient SQLite
// write conflict (a concurrent submission burst exhausting the busy timeout)
// is a retryable 429 (submit_conflict), not a 500 — the job was not
// persisted, so this aligns with the rate-limit semantics.
func writeCreateJobError(w http.ResponseWriter, err error) {
	if db.IsConcurrentWriteError(err) {
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusTooManyRequests, protocol.NewProtocolError(
			protocol.ErrCodeSubmitConflict,
			"Concurrent job submission conflict; retry shortly", err,
		))
		return
	}
	writeError(w, http.StatusInternalServerError, protocol.NewProtocolError(
		protocol.ErrCodeInternalError, "Failed to create job", err,
	))
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

// Health handles health check. An anonymous probe receives the minimal public
// surface; the diagnostic keys are added only when the middleware validated a
// presented credential. Both the root and the /api/v1 path share this handler,
// so their answers cannot diverge.
func (h *Handler) Health(w http.ResponseWriter, r *http.Request) {
	if auth.GetClientID(r) == "" {
		writeJSON(w, http.StatusOK, protocol.HealthResponse{Status: "healthy"})
		return
	}
	writeJSON(w, http.StatusOK, protocol.AuthenticatedHealthResponse{
		Status:      "healthy",
		Version:     h.version,
		AuthEnabled: h.authToken != "",
	})
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
