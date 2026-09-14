package handlers_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tsic404/rffmpeg/pkg/protocol"
)

// TestPullWorkerJobsPopulatesRetryCount closes the server side: a job
// migrated off a failed worker must carry retry_count > 0 in the pull response
// so the new worker can remove the stale partial output before re-running
// ffmpeg. A fresh job (no migration event) stays 0.
func TestPullWorkerJobsPopulatesRetryCount(t *testing.T) {
	h, r, cleanup := setupTest(t)
	defer cleanup()

	workerID := registerTestWorker(t, r, []string{"libx264"})

	fresh, err := h.GetDB().CreateJob(`["file-1"]`, `["-c:v","libx264"]`, "out.mp4", false)
	if err != nil {
		t.Fatalf("CreateJob(fresh): %v", err)
	}
	if _, err := h.GetDB().GetDB().Exec(`UPDATE jobs SET direct_paths = '[]' WHERE id = ?`, fresh.ID); err != nil {
		t.Fatalf("set fresh direct_paths: %v", err)
	}

	migrated, err := h.GetDB().CreateJob(`["file-2"]`, `["-c:v","libx264"]`, "out.mp4", false)
	if err != nil {
		t.Fatalf("CreateJob(migrated): %v", err)
	}
	if _, err := h.GetDB().GetDB().Exec(`UPDATE jobs SET direct_paths = '[]' WHERE id = ?`, migrated.ID); err != nil {
		t.Fatalf("set migrated direct_paths: %v", err)
	}
	if _, err := h.GetDB().CreateMigrationEvent("old-worker", "old-worker", "heartbeat_timeout", 0, []string{migrated.ID}, 1); err != nil {
		t.Fatalf("CreateMigrationEvent: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workers/"+workerID+"/jobs", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("PullWorkerJobs status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	var resp protocol.WorkerJobPullResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode pull response: %v", err)
	}

	byID := make(map[string]protocol.JobInfo, len(resp.Jobs))
	for _, j := range resp.Jobs {
		byID[j.ID] = j
	}

	if j, ok := byID[migrated.ID]; !ok {
		t.Errorf("migrated job %s not present in pull response", migrated.ID)
	} else if j.RetryCount != 1 {
		t.Errorf("migrated job retry_count = %d, want 1", j.RetryCount)
	}

	if j, ok := byID[fresh.ID]; !ok {
		t.Errorf("fresh job %s not present in pull response", fresh.ID)
	} else if j.RetryCount != 0 {
		t.Errorf("fresh job retry_count = %d, want 0", j.RetryCount)
	}
}
