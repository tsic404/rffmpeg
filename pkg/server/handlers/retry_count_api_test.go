package handlers_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestClientJobAPIPopulatesRetryCount guards the client-facing job endpoints:
// GET /api/v1/jobs/{id} and GET /api/v1/jobs must expose the worker-failure
// migration count (retry_count) that was previously always omitted. A migrated
// job reports retry_count=1; a fresh job omits the key entirely, matching the
// omitempty contract documented in openapi.yaml.
func TestClientJobAPIPopulatesRetryCount(t *testing.T) {
	h, r, cleanup := setupTest(t)
	defer cleanup()

	fresh, err := h.GetDB().CreateJob(`["file-1"]`, `["-c:v","libx264"]`, "out.mp4", false)
	if err != nil {
		t.Fatalf("CreateJob(fresh): %v", err)
	}
	migrated, err := h.GetDB().CreateJob(`["file-2"]`, `["-c:v","libx264"]`, "out.mp4", false)
	if err != nil {
		t.Fatalf("CreateJob(migrated): %v", err)
	}
	if _, err := h.GetDB().CreateMigrationEvent("old-worker", "old-worker", "heartbeat_timeout", 0, []string{migrated.ID}, 1); err != nil {
		t.Fatalf("CreateMigrationEvent: %v", err)
	}

	getBody := func(path string) []byte {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer test-token")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200; body=%s", path, rec.Code, rec.Body.String())
		}
		return rec.Body.Bytes()
	}

	assertJobFields := func(id string, fields map[string]any) {
		t.Helper()
		switch id {
		case migrated.ID:
			if rc, ok := fields["retry_count"]; !ok || rc != float64(1) {
				t.Errorf("job %s retry_count = %v, want 1", id, fields["retry_count"])
			}
		case fresh.ID:
			if _, ok := fields["retry_count"]; ok {
				t.Errorf("job %s must omit retry_count key", id)
			}
		}
	}

	// GET /api/v1/jobs/{id}
	for _, id := range []string{migrated.ID, fresh.ID} {
		var resp struct {
			Job map[string]any `json:"job"`
		}
		if err := json.Unmarshal(getBody("/api/v1/jobs/"+id), &resp); err != nil {
			t.Fatalf("decode job response: %v", err)
		}
		assertJobFields(id, resp.Job)
	}

	// GET /api/v1/jobs
	var list struct {
		Jobs []map[string]any `json:"jobs"`
	}
	if err := json.Unmarshal(getBody("/api/v1/jobs"), &list); err != nil {
		t.Fatalf("decode list response: %v", err)
	}
	byID := make(map[string]map[string]any, len(list.Jobs))
	for _, j := range list.Jobs {
		if id, ok := j["id"].(string); ok {
			byID[id] = j
		}
	}
	for _, id := range []string{migrated.ID, fresh.ID} {
		fields, ok := byID[id]
		if !ok {
			t.Errorf("job %s missing from list", id)
			continue
		}
		assertJobFields(id, fields)
	}
}
