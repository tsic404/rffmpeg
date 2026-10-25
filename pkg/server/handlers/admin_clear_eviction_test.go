package handlers_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/tsic404/rffmpeg/pkg/protocol"
	"github.com/tsic404/rffmpeg/pkg/server/auth"
	"github.com/tsic404/rffmpeg/pkg/server/db"
	"github.com/tsic404/rffmpeg/pkg/server/handlers"
	"github.com/tsic404/rffmpeg/pkg/server/ratelimit"
	"github.com/tsic404/rffmpeg/pkg/server/storage"
	"github.com/tsic404/rffmpeg/pkg/server/workerhealth"
)

const adminClearTestToken = "admin-clear-test-token"

// setupAdminClearTest wires the admin clear route exactly like cmd/server:
// the shared auth middleware guards it, so the 401 cases exercise the real
// refusal path instead of a handler-level check.
func setupAdminClearTest(t *testing.T) (*db.Database, *workerhealth.WorkerStateTable, *chi.Mux, func()) {
	t.Helper()

	tmpDir := t.TempDir()
	database, err := db.New(filepath.Join(tmpDir, "test.db"))
	if err != nil {
		t.Fatalf("Failed to create database: %v", err)
	}
	store, err := storage.New(tmpDir)
	if err != nil {
		database.Close()
		t.Fatalf("Failed to create storage: %v", err)
	}

	stateTable := workerhealth.NewWorkerStateTable(30 * time.Second)
	h := handlers.New(database, store, "test", stateTable)
	h.SetAuthToken(adminClearTestToken)

	adminHandler := handlers.NewAdminHandler(h, &ratelimit.RuntimeConfig{})
	router := chi.NewRouter()
	router.Use(auth.Middleware(adminClearTestToken))
	router.Route("/admin", func(r chi.Router) {
		r.Post("/workers/eviction/clear", adminHandler.ClearWorkerEviction)
	})
	// The panel route exercises workerStateMap's join between the state table
	// and the worker rows.
	router.Get("/api/v1/workers", h.ListWorkers)

	cleanup := func() {
		database.Close()
	}
	return database, stateTable, router, cleanup
}

func createAdminClearWorker(t *testing.T, database *db.Database, id, name string) {
	t.Helper()
	if _, err := database.CreateWorker(id, name, protocol.WorkerCapabilities{
		Encoders:      []string{"libx264"},
		FFmpegVersion: "6.0",
		MaxConcurrent: 1,
	}); err != nil {
		t.Fatalf("Failed to create worker %s: %v", id, err)
	}
}

// seedEvictedPair puts both workers into the state table and evicts the slow
// one through the real judgement, so the in-memory mark matches production.
func seedEvictedPair(t *testing.T, database *db.Database, stateTable *workerhealth.WorkerStateTable) {
	t.Helper()
	for _, id := range []string{"w-fast", "w-slow"} {
		if err := database.MarkWorkerEvicted(id); err != nil {
			t.Fatalf("Failed to mark %s evicted: %v", id, err)
		}
	}
	now := time.Now()
	for i := 1; i <= 8; i++ {
		stateTable.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
			WorkerID: "w-fast", Status: string(protocol.WorkerStatusBusy),
			JobsPerSec: 1.5, CompletedJobs: i, Timestamp: now,
		})
		stateTable.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
			WorkerID: "w-slow", Status: string(protocol.WorkerStatusBusy),
			JobsPerSec: 0.1, CompletedJobs: i, Timestamp: now,
		})
	}
	stateTable.DetectSlowWorkers()
}

func postClearEviction(t *testing.T, router *chi.Mux, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/admin/workers/eviction/clear", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

// clearEvictionBody mirrors the handler's JSON response. The handler type is
// unexported (like the other admin responses), so tests decode the wire shape.
type clearEvictionBody struct {
	WorkerID   string `json:"worker_id"`
	WorkerName string `json:"worker_name"`
	Evicted    bool   `json:"evicted"`
	Cleared    bool   `json:"cleared"`
}

func decodeClearEviction(t *testing.T, rec *httptest.ResponseRecorder) clearEvictionBody {
	t.Helper()
	var resp clearEvictionBody
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("Failed to decode response %q: %v", rec.Body.String(), err)
	}
	return resp
}

func decodeProtocolError(t *testing.T, rec *httptest.ResponseRecorder) protocol.ErrorResponse {
	t.Helper()
	var resp protocol.ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("Failed to decode error response %q: %v", rec.Body.String(), err)
	}
	return resp
}

// TestAdminClearWorkerEvictionReleasesMark covers the successful single-object
// release: the mark disappears from the panel state and the audit records the
// manual reason.
func TestAdminClearWorkerEvictionReleasesMark(t *testing.T) {
	database, stateTable, router, cleanup := setupAdminClearTest(t)
	defer cleanup()
	createAdminClearWorker(t, database, "w-fast", "fast-node")
	createAdminClearWorker(t, database, "w-slow", "slow-node")
	seedEvictedPair(t, database, stateTable)

	// Addressed by registered name.
	rec := postClearEviction(t, router, adminClearTestToken, `{"worker":"slow-node"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	resp := decodeClearEviction(t, rec)
	if resp.WorkerID != "w-slow" || resp.WorkerName != "slow-node" {
		t.Errorf("Response addressed %q/%q, want w-slow/slow-node", resp.WorkerID, resp.WorkerName)
	}
	if !resp.Cleared || resp.Evicted {
		t.Errorf("Expected cleared=true and evicted=false, got cleared=%v evicted=%v", resp.Cleared, resp.Evicted)
	}
	if evicted, err := database.IsWorkerEvicted("w-slow"); err != nil || evicted {
		t.Errorf("Expected the stored mark gone, got evicted=%v err=%v", evicted, err)
	}
	if state, ok := stateTable.Get("w-slow"); !ok || state.Evicted {
		t.Error("Expected the in-memory mark gone")
	}
	events, err := database.GetEvictionEventsByWorker("w-slow", 10)
	if err != nil {
		t.Fatalf("Failed to get eviction events: %v", err)
	}
	if len(events) != 1 || events[0].EventType != db.EvictionEventRecovered {
		t.Fatalf("Expected one recovery audit row, got %+v", events)
	}
	if events[0].DecisionReason != db.EvictionReasonManualClear {
		t.Errorf("Expected the manual clear reason, got %q", events[0].DecisionReason)
	}

	// Addressed by identifier after a second eviction.
	if err := database.MarkWorkerEvicted("w-slow"); err != nil {
		t.Fatalf("Failed to re-mark evicted: %v", err)
	}
	stateTable.DetectSlowWorkers()
	rec = postClearEviction(t, router, adminClearTestToken, `{"worker":"w-slow"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200 by identifier, got %d: %s", rec.Code, rec.Body.String())
	}
	if resp := decodeClearEviction(t, rec); !resp.Cleared || resp.WorkerID != "w-slow" {
		t.Errorf("Expected a second release for w-slow, got %+v", resp)
	}
}

// TestAdminClearWorkerEvictionAlreadyClearIsNoOp covers the idempotent case:
// a worker that carries no mark succeeds without a state change or audit row.
func TestAdminClearWorkerEvictionAlreadyClearIsNoOp(t *testing.T) {
	database, _, router, cleanup := setupAdminClearTest(t)
	defer cleanup()
	createAdminClearWorker(t, database, "w-idle", "idle-node")

	for i := range 2 {
		rec := postClearEviction(t, router, adminClearTestToken, `{"worker":"idle-node"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("Call %d: expected 200, got %d: %s", i+1, rec.Code, rec.Body.String())
		}
		resp := decodeClearEviction(t, rec)
		if resp.Cleared || resp.Evicted {
			t.Errorf("Call %d: expected cleared=false evicted=false, got %+v", i+1, resp)
		}
	}
	events, err := database.GetEvictionEventsByWorker("w-idle", 10)
	if err != nil {
		t.Fatalf("Failed to get eviction events: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("Expected no audit row for a no-op clear, got %d", len(events))
	}
	if evicted, err := database.IsWorkerEvicted("w-idle"); err != nil || evicted {
		t.Errorf("Expected the worker to stay unmarked, got evicted=%v err=%v", evicted, err)
	}
}

// TestAdminClearWorkerEvictionErrorSemantics covers C-119's error contract:
// unknown object is not_found, a missing object is invalid_request, and
// neither changes a mark.
func TestAdminClearWorkerEvictionErrorSemantics(t *testing.T) {
	database, stateTable, router, cleanup := setupAdminClearTest(t)
	defer cleanup()
	createAdminClearWorker(t, database, "w-fast", "fast-node")
	createAdminClearWorker(t, database, "w-slow", "slow-node")
	seedEvictedPair(t, database, stateTable)

	cases := []struct {
		name   string
		body   string
		status int
		code   protocol.ErrorCode
	}{
		{"unknown object", `{"worker":"no-such-worker"}`, http.StatusNotFound, protocol.ErrCodeNotFound},
		{"no object field", `{}`, http.StatusBadRequest, protocol.ErrCodeInvalidRequest},
		{"blank object", `{"worker":"   "}`, http.StatusBadRequest, protocol.ErrCodeInvalidRequest},
		{"malformed body", `not-json`, http.StatusBadRequest, protocol.ErrCodeInvalidRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := postClearEviction(t, router, adminClearTestToken, tc.body)
			if rec.Code != tc.status {
				t.Fatalf("Expected %d, got %d: %s", tc.status, rec.Code, rec.Body.String())
			}
			if got := decodeProtocolError(t, rec); got.Code != tc.code {
				t.Errorf("Expected code %q, got %q", tc.code, got.Code)
			}
		})
	}

	if evicted, err := database.IsWorkerEvicted("w-slow"); err != nil || !evicted {
		t.Errorf("Expected the mark untouched by the rejected calls, got evicted=%v err=%v", evicted, err)
	}
	events, err := database.GetEvictionEventsByWorker("w-slow", 10)
	if err != nil {
		t.Fatalf("Failed to get eviction events: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("Expected no audit rows from rejected calls, got %d", len(events))
	}
}

// TestAdminClearWorkerEvictionRequiresAuth covers the 401 half of AC-48: a
// missing or wrong bearer token is refused before the handler runs, so the
// mark is untouched.
func TestAdminClearWorkerEvictionRequiresAuth(t *testing.T) {
	database, stateTable, router, cleanup := setupAdminClearTest(t)
	defer cleanup()
	createAdminClearWorker(t, database, "w-fast", "fast-node")
	createAdminClearWorker(t, database, "w-slow", "slow-node")
	seedEvictedPair(t, database, stateTable)

	for _, token := range []string{"", "wrong-token"} {
		rec := postClearEviction(t, router, token, `{"worker":"slow-node"}`)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("Token %q: expected 401, got %d: %s", token, rec.Code, rec.Body.String())
		}
	}
	if evicted, err := database.IsWorkerEvicted("w-slow"); err != nil || !evicted {
		t.Errorf("Expected the mark untouched by unauthorized calls, got evicted=%v err=%v", evicted, err)
	}
	if state, ok := stateTable.Get("w-slow"); !ok || !state.Evicted {
		t.Error("Expected the in-memory mark untouched by unauthorized calls")
	}
}

// TestAdminClearWorkerEvictionNormalizesHeartbeatID covers the supported
// non-canonical UUID heartbeat form (compact / upper-case). The state entry must
// join the canonical worker row: otherwise the panel loses the worker's live
// metrics, the manual clear releases only the database mark, and the audit row
// records a zero decision throughput.
func TestAdminClearWorkerEvictionNormalizesHeartbeatID(t *testing.T) {
	database, stateTable, router, cleanup := setupAdminClearTest(t)
	defer cleanup()

	const (
		canonical = "550e8400-e29b-41d4-a716-446655440000"
		compact   = "550E8400E29B41D4A716446655440000"
		peer      = "6ba7b810-9dad-11d1-80f4-00c04fd430c8"
	)
	createAdminClearWorker(t, database, canonical, "uuid-slow")
	createAdminClearWorker(t, database, peer, "uuid-fast")

	// Both workers heartbeat in a non-canonical UUID form, as a worker that
	// reports its own formatting can do.
	now := time.Now()
	for i := 1; i <= 8; i++ {
		stateTable.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
			WorkerID: "6BA7B8109DAD11D180F400C04FD430C8", Status: string(protocol.WorkerStatusBusy),
			JobsPerSec: 1.5, CompletedJobs: i, Timestamp: now,
		})
		stateTable.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
			WorkerID: compact, Status: string(protocol.WorkerStatusBusy),
			JobsPerSec: 0.1, CompletedJobs: i, Timestamp: now,
		})
	}
	// The monitor syncs the judgement to the database; reproduce that mark.
	if err := database.MarkWorkerEvicted(canonical); err != nil {
		t.Fatalf("Failed to mark the worker evicted: %v", err)
	}
	stateTable.DetectSlowWorkers()
	if state, ok := stateTable.Get(canonical); !ok || !state.Evicted {
		t.Fatalf("Expected the compact-form heartbeat to be marked under the canonical id, got %+v", state)
	}

	// The panel joins the state entry to the canonical row.
	panelReq := httptest.NewRequest("GET", "/api/v1/workers", nil)
	panelReq.Header.Set("Authorization", "Bearer "+adminClearTestToken)
	panelRec := httptest.NewRecorder()
	router.ServeHTTP(panelRec, panelReq)
	if panelRec.Code != http.StatusOK {
		t.Fatalf("Worker panel failed: %d %s", panelRec.Code, panelRec.Body.String())
	}
	var panel struct {
		Workers []struct {
			ID     string `json:"id"`
			Health struct {
				EWMA      float64 `json:"ewma_jobs_per_sec"`
				Completed int     `json:"completed_jobs"`
			} `json:"health"`
		} `json:"workers"`
	}
	if err := json.Unmarshal(panelRec.Body.Bytes(), &panel); err != nil {
		t.Fatalf("Failed to decode the worker panel: %v", err)
	}
	found := false
	for _, w := range panel.Workers {
		if w.ID != canonical {
			continue
		}
		found = true
		if w.Health.EWMA <= 0 || w.Health.Completed < 5 {
			t.Errorf("Expected the panel to carry the compact-heartbeat sample for %s, got %+v", canonical, w.Health)
		}
	}
	if !found {
		t.Fatalf("Panel did not list the worker %s", canonical)
	}

	rec := postClearEviction(t, router, adminClearTestToken, `{"worker":"uuid-slow"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if resp := decodeClearEviction(t, rec); !resp.Cleared || resp.Evicted {
		t.Errorf("Expected a real release, got %+v", resp)
	}

	// The in-memory mark is gone, reachable through both id forms.
	for _, ref := range []string{canonical, compact} {
		if state, ok := stateTable.Get(ref); !ok || state.Evicted {
			t.Errorf("Expected the in-memory mark cleared for %q, got %+v", ref, state)
		}
	}

	// The audit row records the throughput visible at the clear, not a zero
	// caused by a missed state-table lookup.
	events, err := database.GetEvictionEventsByWorker(canonical, 10)
	if err != nil {
		t.Fatalf("Failed to get eviction events: %v", err)
	}
	if len(events) != 1 || events[0].EventType != db.EvictionEventRecovered {
		t.Fatalf("Expected one recovery audit row, got %+v", events)
	}
	if events[0].CurrentThroughput <= 0 {
		t.Errorf("Expected the decision throughput recorded, got %v", events[0].CurrentThroughput)
	}
}
