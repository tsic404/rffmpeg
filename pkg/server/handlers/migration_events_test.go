package handlers_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/tsix404/rffmpeg/pkg/protocol"
	"github.com/tsix404/rffmpeg/pkg/server/handlers"
	"github.com/tsix404/rffmpeg/pkg/server/migration"
)

func TestListMigrationEvents(t *testing.T) {
	h, r, cleanup := setupTest(t)
	defer cleanup()

	r.Get("/api/v1/migrations", h.ListMigrationEvents)

	if _, err := h.GetDB().CreateMigrationEvent("worker-1", "w1", "heartbeat_timeout", 0, []string{"job-1"}, 1); err != nil {
		t.Fatalf("seed event 1: %v", err)
	}
	if _, err := h.GetDB().CreateMigrationEvent("worker-2", "w2", "worker_offline", 1, []string{"job-2", "job-3"}, 2); err != nil {
		t.Fatalf("seed event 2: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/migrations", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Events []migration.EventInfo `json:"events"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Events) != 2 {
		t.Fatalf("events = %d, want 2", len(resp.Events))
	}
	// Newest first (timestamp DESC).
	if resp.Events[0].WorkerID != "worker-2" {
		t.Errorf("events[0].worker_id = %q, want worker-2", resp.Events[0].WorkerID)
	}
	if len(resp.Events[0].JobIDs) != 2 {
		t.Errorf("events[0].job_ids = %v, want 2 entries", resp.Events[0].JobIDs)
	}
}

func TestGetMigrationEventMalformedJobIDs(t *testing.T) {
	h, r, cleanup := setupTest(t)
	defer cleanup()

	r.Get("/api/v1/migrations/{eventId}", h.GetMigrationEvent)

	// Seed a row whose job_ids column is not valid JSON, bypassing
	// CreateMigrationEvent which always marshals to valid JSON.
	if _, err := h.GetDB().GetDB().Exec(
		`INSERT INTO migration_events (id, timestamp, worker_id, worker_name, reason, retry_count, job_ids, jobs_migrated, created_at)
		 VALUES (?, datetime('now'), ?, NULL, ?, ?, ?, ?, datetime('now'))`,
		"malformed-1", "worker-1", "heartbeat_timeout", 0, "not-json", 1,
	); err != nil {
		t.Fatalf("seed malformed event: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/migrations/malformed-1", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Event migration.EventInfo `json:"event"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	// Malformed job_ids degrades to an empty JSON array, not null or an error.
	if resp.Event.JobIDs == nil || len(resp.Event.JobIDs) != 0 {
		t.Errorf("job_ids = %#v, want non-nil empty slice", resp.Event.JobIDs)
	}
}
func TestListMigrationEventsEmpty(t *testing.T) {
	h, r, cleanup := setupTest(t)
	defer cleanup()

	r.Get("/api/v1/migrations", h.ListMigrationEvents)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/migrations", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != "{\"events\":[]}\n" {
		t.Errorf("empty list body = %q, want JSON events array", got)
	}
}

func TestGetMigrationEvent(t *testing.T) {
	h, r, cleanup := setupTest(t)
	defer cleanup()

	r.Get("/api/v1/migrations/{eventId}", h.GetMigrationEvent)

	event, err := h.GetDB().CreateMigrationEvent("worker-1", "w1", "heartbeat_timeout", 0, []string{"job-1"}, 1)
	if err != nil {
		t.Fatalf("seed event: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/migrations/"+event.ID, nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Event migration.EventInfo `json:"event"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Event.ID != event.ID || resp.Event.WorkerID != "worker-1" {
		t.Errorf("event mismatch: %+v", resp.Event)
	}
	if resp.Event.Reason != migration.ReasonHeartbeatTimeout {
		t.Errorf("reason = %q, want heartbeat_timeout", resp.Event.Reason)
	}
}

func TestMigrationNotFoundConsistentJSON(t *testing.T) {
	h, r, cleanup := setupTest(t)
	defer cleanup()

	r.Get("/api/v1/migrations/{eventId}", h.GetMigrationEvent)
	r.NotFound(handlers.NotFound)

	// The trailing empty segment falls through to the chi NotFound handler;
	// the malformed ID is a resource-specific 404. Both must share the
	// {"code":"not_found"} JSON structure.
	for _, path := range []string{"/api/v1/migrations/", "/api/v1/migrations/nope"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Fatalf("GET %s status = %d, want 404; body=%s", path, rec.Code, rec.Body.String())
		}
		if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
			t.Errorf("GET %s content-type = %q, want application/json", path, ct)
		}
		var resp protocol.ErrorResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("GET %s decode response: %v; body=%s", path, err, rec.Body.String())
		}
		if resp.Code != protocol.ErrCodeNotFound {
			t.Errorf("GET %s code = %q, want %q", path, resp.Code, protocol.ErrCodeNotFound)
		}
	}
}

func TestMigrationMethodNotAllowedConsistentJSON(t *testing.T) {
	h, _, cleanup := setupTest(t)
	defer cleanup()

	// Middleware must be registered before routes: chi only wraps routes that
	// are registered after the middleware is in place.
	r := chi.NewRouter()
	r.Use(handlers.NormalizeMethodNotAllowed)
	r.Get("/api/v1/migrations", h.ListMigrationEvents)
	r.Get("/api/v1/migrations/{eventId}", h.GetMigrationEvent)

	// POST matches the GET-only /api/v1/migrations path and must fall through
	// to chi's default 405 handler, whose empty body the middleware replaces
	// with the {"code":"method_not_allowed"} JSON while keeping the Allow
	// header (RFC 9110 §10.2.1).
	req := httptest.NewRequest(http.MethodPost, "/api/v1/migrations", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /api/v1/migrations status = %d, want 405; body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type = %q, want application/json", ct)
	}
	if allow := rec.Header().Get("Allow"); allow != "GET" {
		t.Errorf("Allow = %q, want GET", allow)
	}
	var resp protocol.ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v; body=%s", err, rec.Body.String())
	}
	if resp.Code != "method_not_allowed" {
		t.Errorf("code = %q, want method_not_allowed", resp.Code)
	}
}

func TestGetMigrationEventNotFound(t *testing.T) {
	h, r, cleanup := setupTest(t)
	defer cleanup()

	r.Get("/api/v1/migrations/{eventId}", h.GetMigrationEvent)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/migrations/nope", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}
