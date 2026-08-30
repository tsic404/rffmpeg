package handlers_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

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
