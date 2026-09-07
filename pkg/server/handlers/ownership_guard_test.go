package handlers_test

// End-to-end regression tests for the TSI-2362 review fixes:
//
//  1. The worker client sends worker_id on terminal updates, so the server's
//     ownership guard actually fires in production: a stale completed PATCH
//     from the OLD owner after failover gets 409 and must not overwrite the
//     new owner's result.
//  2. A late completion report from an OFFLINE (but still owning) worker does
//     not resurrect it into the schedulable pool — the terminal hook uses the
//     guarded conditional idle transition.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/tsic404/rffmpeg/pkg/protocol"
	"github.com/tsic404/rffmpeg/pkg/server/db"
	"github.com/tsic404/rffmpeg/pkg/server/handlers"
	"github.com/tsic404/rffmpeg/pkg/server/storage"
	"github.com/tsic404/rffmpeg/pkg/server/workerhealth"
	"github.com/tsic404/rffmpeg/pkg/worker"
)

// setupOwnershipTest wires a handler + router + database handle for ownership
// scenarios. Returns the raw *sql.DB wrapper so tests can stage job state
// directly (simulating scheduler/failover writes).
func setupOwnershipTest(t *testing.T) (*ownershipBundle, func()) {
	t.Helper()

	tmpDir, err := os.MkdirTemp("", "rffmpeg-own-test-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	database, err := db.New(tmpDir + "/test.db")
	if err != nil {
		os.RemoveAll(tmpDir)
		t.Fatalf("db.New: %v", err)
	}
	store, err := storage.New(tmpDir)
	if err != nil {
		database.Close()
		os.RemoveAll(tmpDir)
		t.Fatalf("storage.New: %v", err)
	}
	stateTable := workerhealth.NewWorkerStateTable(30 * time.Second)
	h := handlers.New(database, store, "test", stateTable)
	h.SetAuthToken("test-token")

	r := chi.NewRouter()
	r.Patch("/api/v1/jobs/{jobId}", h.UpdateJob)
	r.Post("/api/v1/workers/register", h.RegisterWorker)

	cleanup := func() {
		database.Close()
		os.RemoveAll(tmpDir)
	}
	return &ownershipBundle{router: r, db: database}, cleanup
}

type ownershipBundle struct {
	router chi.Router
	db     *db.Database
}

func TestStaleOwnerTerminalUpdateRejectedEndToEnd(t *testing.T) {
	bundle, cleanup := setupOwnershipTest(t)
	defer cleanup()
	database := bundle.db

	mustWorker(t, database, "worker-old")
	mustWorker(t, database, "worker-new")

	job, err := database.CreateJob(`["in.mkv"]`, `["-c:v","libx264"]`, "out.mkv", false)
	if err != nil {
		t.Fatal(err)
	}
	stageRunning(t, database, job.ID, "worker-old")

	// Failover: job reassigned to the new owner.
	if err := database.ResetJobToPending(job.ID); err != nil {
		t.Fatal(err)
	}
	stageRunning(t, database, job.ID, "worker-new")

	// OLD owner reports completion over HTTP — must get 409 and not overwrite.
	patchJob(t, bundle.router, job.ID, "worker-old", protocol.JobStatusCompleted, http.StatusConflict)

	got, err := database.GetJob(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != protocol.JobStatusRunning || got.WorkerID.String != "worker-new" {
		t.Fatalf("stale PATCH mutated job: status=%s worker=%s", got.Status, got.WorkerID.String)
	}

	// Current owner reports completion — accepted.
	patchJob(t, bundle.router, job.ID, "worker-new", protocol.JobStatusCompleted, http.StatusOK)

	got, _ = database.GetJob(job.ID)
	if got.Status != protocol.JobStatusCompleted {
		t.Fatalf("owner PATCH should complete the job, got %s", got.Status)
	}
}

// TestClientSendsWorkerIDOnTerminalUpdates pins the wire contract: the worker
// client must include worker_id in JobUpdateRequest payloads, or the server's
// ownership guard never engages in production.
func TestClientSendsWorkerIDOnTerminalUpdates(t *testing.T) {
	var captured protocol.JobUpdateRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch {
			if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
				t.Errorf("decode: %v", err)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"message":"ok"}`))
	}))
	defer srv.Close()

	client := worker.NewClient(srv.URL, "my-worker-id", "")
	if err := client.UpdateJobWithFailure("job-1", protocol.JobStatusFailed, 1, "boom", false, "FFMPEG_ERROR", "details"); err != nil {
		t.Fatal(err)
	}
	if captured.WorkerID != "my-worker-id" {
		t.Fatalf("UpdateJobWithFailure sent worker_id=%q, want %q", captured.WorkerID, "my-worker-id")
	}

	if err := client.SendProgress("job-1", 50, 10, 1, 2, 1.5); err != nil {
		t.Fatal(err)
	}
	if captured.WorkerID != "my-worker-id" {
		t.Fatalf("SendProgress sent worker_id=%q, want %q", captured.WorkerID, "my-worker-id")
	}
}

// TestOfflineWorkerNotResurrectedByCompletionReport covers acceptance 2b end to
// end: an offline worker whose final completion report is accepted (it still
// owns the job at guard time) must stay offline afterward.
func TestOfflineWorkerNotResurrectedByCompletionReport(t *testing.T) {
	bundle, cleanup := setupOwnershipTest(t)
	defer cleanup()
	database := bundle.db

	mustWorker(t, database, "worker-dying")

	job, err := database.CreateJob(`["in.mkv"]`, `["-c:v","libx264"]`, "out.mkv", false)
	if err != nil {
		t.Fatal(err)
	}
	stageRunning(t, database, job.ID, "worker-dying")

	// Worker goes offline (heartbeat lost) before its report arrives.
	if _, err := database.GetDB().Exec(
		`UPDATE workers SET status = ? WHERE id = ?`, protocol.WorkerStatusOffline, "worker-dying"); err != nil {
		t.Fatal(err)
	}

	// The report itself is legitimate (still the owner) → 200...
	patchJob(t, bundle.router, job.ID, "worker-dying", protocol.JobStatusCompleted, http.StatusOK)

	// ...but the terminal hook must NOT flip the offline worker back to idle.
	w, err := database.GetWorker("worker-dying")
	if err != nil {
		t.Fatal(err)
	}
	if w.Status != protocol.WorkerStatusOffline {
		t.Fatalf("offline worker resurrected by its own completion report: status=%s", w.Status)
	}
}

// --- helpers ---

func mustWorker(t *testing.T, d *db.Database, id string) {
	t.Helper()
	if _, err := d.CreateWorker(id, id, protocol.WorkerCapabilities{
		Encoders: []string{"libx264"}, FFmpegVersion: "5.0", MaxConcurrent: 1,
	}); err != nil {
		t.Fatal(err)
	}
}

func stageRunning(t *testing.T, d *db.Database, jobID, workerID string) {
	t.Helper()
	if err := d.AssignJobToWorker(jobID, workerID); err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateJobStatusWithFailure(jobID, protocol.JobStatusRunning, nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
}

func patchJob(t *testing.T, router chi.Router, jobID, workerID string, status protocol.JobStatus, wantCode int) {
	t.Helper()
	req := protocol.JobUpdateRequest{
		Status:   status,
		ExitCode: 0,
		WorkerID: workerID,
	}
	body, _ := json.Marshal(req)
	r := httptest.NewRequest(http.MethodPatch, "/api/v1/jobs/"+jobID, bytes.NewReader(body))
	r.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, r)
	if rec.Code != wantCode {
		t.Fatalf("PATCH by %q: got %d want %d body=%s", workerID, rec.Code, wantCode, rec.Body.String())
	}
}
