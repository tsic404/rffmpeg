package handlers_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestJobAPIPopulatesClaimedAt guards the client-facing job endpoints: the
// claim time (pending -> queued) that the migration dispatch segment is judged
// on must be readable from both GET /api/v1/jobs/{id} and GET /api/v1/jobs.
// A pending job omits the key, matching the omitempty contract.
func TestJobAPIPopulatesClaimedAt(t *testing.T) {
	h, r, cleanup := setupTest(t)
	defer cleanup()

	fresh, err := h.GetDB().CreateJob(`["file-1"]`, `["-c:v","libx264"]`, "out.mp4", false)
	if err != nil {
		t.Fatalf("CreateJob(fresh): %v", err)
	}
	claimed, err := h.GetDB().CreateJob(`["file-2"]`, `["-c:v","libx264"]`, "out.mp4", false)
	if err != nil {
		t.Fatalf("CreateJob(claimed): %v", err)
	}
	if err := h.GetDB().AssignJobToWorker(claimed.ID, "worker-1"); err != nil {
		t.Fatalf("AssignJobToWorker: %v", err)
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

	assertClaim := func(id string, fields map[string]any) {
		t.Helper()
		raw, ok := fields["claimed_at"]
		switch id {
		case claimed.ID:
			if !ok {
				t.Fatalf("job %s must expose claimed_at", id)
			}
			s, ok := raw.(string)
			if !ok {
				t.Fatalf("job %s claimed_at = %v, want a timestamp string", id, raw)
			}
			if _, err := time.Parse(time.RFC3339Nano, s); err != nil {
				t.Errorf("job %s claimed_at = %q is not RFC3339: %v", id, s, err)
			}
		case fresh.ID:
			if ok {
				t.Errorf("pending job %s must omit claimed_at", id)
			}
		}
	}

	for _, id := range []string{claimed.ID, fresh.ID} {
		var resp struct {
			Job map[string]any `json:"job"`
		}
		if err := json.Unmarshal(getBody("/api/v1/jobs/"+id), &resp); err != nil {
			t.Fatalf("decode job response: %v", err)
		}
		assertClaim(id, resp.Job)
	}

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
	for _, id := range []string{claimed.ID, fresh.ID} {
		fields, ok := byID[id]
		if !ok {
			t.Fatalf("job %s missing from list response", id)
		}
		assertClaim(id, fields)
	}
}
