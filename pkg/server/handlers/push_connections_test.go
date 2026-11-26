package handlers_test

// End-to-end coverage for the push connection list reaching the API response:
// a terminal PATCH carrying the list must be readable through
// GET /api/v1/jobs/{jobId}, and a terminal PATCH that omits the list must not
// erase the list an earlier report stored.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/tsic404/rffmpeg/pkg/protocol"
)

func newPushConnectionsRouter(bundle *ownershipBundle) chi.Router {
	r := chi.NewRouter()
	r.Patch("/api/v1/jobs/{jobId}", bundle.handler.UpdateJob)
	r.Get("/api/v1/jobs/{jobId}", bundle.handler.GetJob)
	return r
}

func patchJobWithConnections(t *testing.T, router chi.Router, jobID, workerID string, status protocol.JobStatus, conns []protocol.PushConnection, wantCode int) {
	t.Helper()
	req := protocol.JobUpdateRequest{
		Status:          status,
		WorkerID:        workerID,
		PushConnections: conns,
	}
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	r := httptest.NewRequest(http.MethodPatch, "/api/v1/jobs/"+jobID, bytes.NewReader(body))
	r.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, r)
	if rec.Code != wantCode {
		t.Fatalf("PATCH by %q: got %d want %d body=%s", workerID, rec.Code, wantCode, rec.Body.String())
	}
}

func getJobInfo(t *testing.T, router chi.Router, jobID string) protocol.JobInfo {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/jobs/"+jobID, nil)
	r.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET job: got %d want 200 body=%s", rec.Code, rec.Body.String())
	}
	var resp protocol.JobStatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode job response: %v", err)
	}
	return resp.Job
}

func TestPushConnectionsRoundTripThroughAPI(t *testing.T) {
	bundle, cleanup := setupOwnershipTest(t)
	defer cleanup()
	router := newPushConnectionsRouter(bundle)

	mustWorker(t, bundle.db, "worker-1")
	job, err := bundle.db.CreateJob(`["in.mkv"]`, `["-c:v","libx264"]`, "out.mkv", false)
	if err != nil {
		t.Fatal(err)
	}
	stageRunning(t, bundle.db, job.ID, "worker-1")

	want := []protocol.PushConnection{
		{Family: "rtmp", Purpose: "session", Target: "10.0.0.1:1935", Origin: "request", Result: "ok"},
		{Family: "rtp", Purpose: "stream", Target: "[2001:db8::5]:5004", Origin: "server", Result: "unreachable"},
	}
	patchJobWithConnections(t, router, job.ID, "worker-1", protocol.JobStatusCompleted, want, http.StatusOK)

	got := getJobInfo(t, router, job.ID)
	if len(got.PushConnections) != len(want) {
		t.Fatalf("push_connections length = %d, want %d (%+v)", len(got.PushConnections), len(want), got.PushConnections)
	}
	for i, conn := range got.PushConnections {
		if conn != want[i] {
			t.Fatalf("push_connections[%d] = %+v, want %+v", i, conn, want[i])
		}
	}
}

func TestPushConnectionsPreservedWhenReportOmitsList(t *testing.T) {
	bundle, cleanup := setupOwnershipTest(t)
	defer cleanup()
	router := newPushConnectionsRouter(bundle)

	mustWorker(t, bundle.db, "worker-1")
	job, err := bundle.db.CreateJob(`["in.mkv"]`, `["-c:v","libx264"]`, "out.mkv", false)
	if err != nil {
		t.Fatal(err)
	}
	stageRunning(t, bundle.db, job.ID, "worker-1")

	first := []protocol.PushConnection{
		{Family: "srt", Purpose: "session", Target: "10.0.0.9:9000", Origin: "request", Result: "ok"},
	}
	// Stage the list an earlier report would have stored. A job reaches a
	// terminal state once, so a second terminal PATCH cannot create this
	// precondition through the API alone.
	stored, err := json.Marshal(first)
	if err != nil {
		t.Fatalf("marshal staged list: %v", err)
	}
	if _, err := bundle.db.GetDB().Exec(`UPDATE jobs SET push_connections = ? WHERE id = ?`, string(stored), job.ID); err != nil {
		t.Fatalf("stage push_connections: %v", err)
	}

	// A later terminal report carrying no list must keep the stored one.
	patchJobWithConnections(t, router, job.ID, "worker-1", protocol.JobStatusCompleted, nil, http.StatusOK)

	got := getJobInfo(t, router, job.ID)
	if got.Status != protocol.JobStatusCompleted {
		t.Fatalf("status = %s, want completed", got.Status)
	}
	if len(got.PushConnections) != 1 || got.PushConnections[0] != first[0] {
		t.Fatalf("push_connections = %+v, want %+v", got.PushConnections, first)
	}
}
