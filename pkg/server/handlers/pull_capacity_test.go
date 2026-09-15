package handlers_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/tsic404/rffmpeg/pkg/protocol"
)

// registerTestWorkerWithCaps registers a worker with an explicit MaxConcurrent,
// so pull-path tests can pin the capacity guard instead of inheriting the
// zero-value ("unset") capability of the other register helpers.
func registerTestWorkerWithCaps(t *testing.T, router *chi.Mux, workerID, name string, encoders []string, maxConcurrent int) string {
	t.Helper()
	workerReq := protocol.WorkerRegisterRequest{
		WorkerID: workerID,
		Name:     name,
		Capabilities: protocol.WorkerCapabilities{
			Encoders:      encoders,
			FFmpegVersion: "5.1",
			MaxConcurrent: maxConcurrent,
		},
	}
	workerBody, _ := json.Marshal(workerReq)

	req := httptest.NewRequest("POST", "/api/v1/workers/register", bytes.NewReader(workerBody))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("Failed to register worker %s: status %d, body: %s", workerID, w.Code, w.Body.String())
	}
	return workerID
}

// pullJobs performs one worker pull and decodes the response.
func pullJobs(t *testing.T, router *chi.Mux, workerID string) protocol.WorkerJobPullResponse {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/workers/"+workerID+"/jobs", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("PullWorkerJobs status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp protocol.WorkerJobPullResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode pull response: %v", err)
	}
	return resp
}

// TestPullWorkerJobsRespectsMaxConcurrent guards the pull-path capacity fix: a
// worker that declares MaxConcurrent=1 must receive at most one new job per
// pull even when many pending jobs exist, leaving the rest pending.
func TestPullWorkerJobsRespectsMaxConcurrent(t *testing.T) {
	h, r, cleanup := setupTest(t)
	defer cleanup()

	workerID := registerTestWorkerWithCaps(t, r, "test-worker-1", "test-worker", []string{"libx264"}, 1)

	for i := range 3 {
		if _, err := h.GetDB().CreateJob(`["in.mkv"]`, `["-c:v","libx264"]`, "out.mkv", false); err != nil {
			t.Fatalf("CreateJob %d: %v", i, err)
		}
	}

	resp := pullJobs(t, r, workerID)
	if len(resp.Jobs) != 1 {
		t.Fatalf("pull returned %d jobs, want 1 (MaxConcurrent=1)", len(resp.Jobs))
	}

	pending, err := h.GetDB().GetPendingJobs(10)
	if err != nil {
		t.Fatalf("GetPendingJobs: %v", err)
	}
	if len(pending) != 2 {
		t.Errorf("pending jobs after pull = %d, want 2", len(pending))
	}
}

// TestPullWorkerJobsBusyWorkerGetsNoNewJobs guards the capacity recheck: a
// MaxConcurrent=1 worker that already holds a queued job must re-pull that
// queued job but claim no new pending work until it completes.
func TestPullWorkerJobsBusyWorkerGetsNoNewJobs(t *testing.T) {
	h, r, cleanup := setupTest(t)
	defer cleanup()

	workerID := registerTestWorkerWithCaps(t, r, "test-worker-1", "test-worker", []string{"libx264"}, 1)

	for range 2 {
		if _, err := h.GetDB().CreateJob(`["in.mkv"]`, `["-c:v","libx264"]`, "out.mkv", false); err != nil {
			t.Fatalf("CreateJob: %v", err)
		}
	}

	first := pullJobs(t, r, workerID)
	if len(first.Jobs) != 1 {
		t.Fatalf("first pull returned %d jobs, want 1", len(first.Jobs))
	}

	// Without completing the first job, the second pull must re-deliver the
	// already-queued job and claim nothing new: the other job stays pending.
	second := pullJobs(t, r, workerID)
	if len(second.Jobs) != 1 {
		t.Fatalf("second pull returned %d jobs, want 1 (re-delivered queued job)", len(second.Jobs))
	}

	pending, err := h.GetDB().GetPendingJobs(10)
	if err != nil {
		t.Fatalf("GetPendingJobs: %v", err)
	}
	if len(pending) != 1 {
		t.Errorf("pending jobs after second pull = %d, want 1 (no new claims)", len(pending))
	}
}

// TestPullWorkerJobsRunningJobBlocksCapacity guards the running-capacity path:
// a running job counts toward MaxConcurrent but is not re-delivered by a pull
// (unlike a queued job), so a MaxConcurrent=1 worker holding a running job
// must pull nothing and claim no new pending work until the job completes.
func TestPullWorkerJobsRunningJobBlocksCapacity(t *testing.T) {
	h, r, cleanup := setupTest(t)
	defer cleanup()

	workerID := registerTestWorkerWithCaps(t, r, "test-worker-1", "test-worker", []string{"libx264"}, 1)

	for range 2 {
		if _, err := h.GetDB().CreateJob(`["in.mkv"]`, `["-c:v","libx264"]`, "out.mkv", false); err != nil {
			t.Fatalf("CreateJob: %v", err)
		}
	}

	first := pullJobs(t, r, workerID)
	if len(first.Jobs) != 1 {
		t.Fatalf("first pull returned %d jobs, want 1", len(first.Jobs))
	}

	// Report the claimed job as running: it consumes the full MaxConcurrent=1
	// budget but, unlike a queued job, the pull path does not re-deliver it.
	if err := h.GetDB().UpdateJobStatusWithFailure(first.Jobs[0].ID, protocol.JobStatusRunning, nil, nil, nil, nil); err != nil {
		t.Fatalf("mark job running: %v", err)
	}

	second := pullJobs(t, r, workerID)
	if len(second.Jobs) != 0 {
		t.Fatalf("second pull returned %d jobs, want 0 (running job blocks capacity)", len(second.Jobs))
	}

	pending, err := h.GetDB().GetPendingJobs(10)
	if err != nil {
		t.Fatalf("GetPendingJobs: %v", err)
	}
	if len(pending) != 1 {
		t.Errorf("pending jobs after second pull = %d, want 1 (no new claims)", len(pending))
	}
}

// TestPullWorkerJobsFallsBackToMaxJobsPerWorker guards the MaxConcurrent<=0
// path: an unset worker capacity uses the handler's maxJobsPerWorker fallback
// (mirroring the scheduler) instead of claiming nothing or an unbounded batch.
func TestPullWorkerJobsFallsBackToMaxJobsPerWorker(t *testing.T) {
	h, r, cleanup := setupTest(t)
	defer cleanup()
	h.SetMaxJobsPerWorker(2)

	// MaxConcurrent=0 (omitted on the wire) means "unset".
	workerID := registerTestWorkerWithCaps(t, r, "test-worker-1", "test-worker", []string{"libx264"}, 0)

	for range 3 {
		if _, err := h.GetDB().CreateJob(`["in.mkv"]`, `["-c:v","libx264"]`, "out.mkv", false); err != nil {
			t.Fatalf("CreateJob: %v", err)
		}
	}

	resp := pullJobs(t, r, workerID)
	if len(resp.Jobs) != 2 {
		t.Fatalf("pull returned %d jobs, want 2 (MaxConcurrent<=0 falls back to maxJobsPerWorker)", len(resp.Jobs))
	}
}
