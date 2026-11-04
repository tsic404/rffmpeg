package handlers_test

// End-to-end regression tests for the ownership-guard fixes:
//
//  1. The worker client sends worker_id on terminal updates, so the guard
//     fires in production: a stale completed PATCH from the OLD owner after
//     failover gets 409 and must not overwrite the new owner's result.
//  2. A late completion report from an OFFLINE (still owning) worker must not
//     resurrect it into the schedulable pool — the terminal hook uses the
//     guarded conditional idle transition.

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	gorillaws "github.com/gorilla/websocket"
	"github.com/tsic404/rffmpeg/pkg/protocol"
	"github.com/tsic404/rffmpeg/pkg/server/db"
	"github.com/tsic404/rffmpeg/pkg/server/db/dbtest"
	"github.com/tsic404/rffmpeg/pkg/server/handlers"
	"github.com/tsic404/rffmpeg/pkg/server/storage"
	"github.com/tsic404/rffmpeg/pkg/server/websocket"
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
	r.Post("/api/v1/jobs/{jobId}/output", h.UploadJobOutput)
	r.Post("/api/v1/workers/register", h.RegisterWorker)

	cleanup := func() {
		database.Close()
		os.RemoveAll(tmpDir)
	}
	return &ownershipBundle{router: r, db: database, handler: h}, cleanup
}

type ownershipBundle struct {
	router  chi.Router
	db      *db.Database
	handler *handlers.Handler
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
	failoverJob(t, database, job.ID, "worker-old")
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

// TestStaleRunningReportCannotResurrectMigratedJob covers H6: the job was
// migrated back to pending (worker_id NULL) but the old owner's "running"
// report arrives afterwards. It must be refused, or the job would sit running
// with no owner — invisible to scheduling, migration and the starvation sweep.
func TestStaleRunningReportCannotResurrectMigratedJob(t *testing.T) {
	bundle, cleanup := setupOwnershipTest(t)
	defer cleanup()
	database := bundle.db

	mustWorker(t, database, "worker-old")
	job, err := database.CreateJob(`["in.mkv"]`, `["-c:v","libx264"]`, "out.mkv", false)
	if err != nil {
		t.Fatal(err)
	}
	stageRunning(t, database, job.ID, "worker-old")
	failoverJob(t, database, job.ID, "worker-old")

	rec := patchRequest(t, bundle.router, job.ID, protocol.JobUpdateRequest{
		Status:   protocol.JobStatusRunning,
		WorkerID: "worker-old",
	})
	if rec.Code != http.StatusConflict {
		t.Fatalf("stale running report: got %d want 409 body=%s", rec.Code, rec.Body.String())
	}

	got, _ := database.GetJob(job.ID)
	if got.Status != protocol.JobStatusPending {
		t.Fatalf("stale running report moved the job to %s, want pending", got.Status)
	}
	if got.WorkerID.Valid {
		t.Fatalf("stale running report claimed the job for %s", got.WorkerID.String)
	}
}

// TestStaleProgressAndChunkReportsRejected covers the stream side of H6: a
// stale worker's progress and stderr/stdout chunks must be refused as a whole
// request, not interleaved into the new owner's stream.
func TestStaleProgressAndChunkReportsRejected(t *testing.T) {
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
	failoverJob(t, database, job.ID, "worker-old")
	stageRunning(t, database, job.ID, "worker-new")
	if _, err := database.UpdateJobProgressWithOwner(job.ID, "worker-new", 10, 60); err != nil {
		t.Fatal(err)
	}

	rec := patchRequest(t, bundle.router, job.ID, protocol.JobUpdateRequest{
		WorkerID:    "worker-old",
		Progress:    99,
		EtaSeconds:  1,
		StderrChunk: "stale stderr",
		StdoutChunk: "c3RhbGU=",
	})
	if rec.Code != http.StatusConflict {
		t.Fatalf("stale progress/chunk report: got %d want 409 body=%s", rec.Code, rec.Body.String())
	}
	got, _ := database.GetJob(job.ID)
	if got.ProgressPercent != 10 {
		t.Fatalf("stale progress report wrote %v, want 10", got.ProgressPercent)
	}

	// The current owner's report is accepted and lands.
	rec = patchRequest(t, bundle.router, job.ID, protocol.JobUpdateRequest{
		WorkerID:   "worker-new",
		Progress:   20,
		EtaSeconds: 50,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("owner progress report: got %d want 200 body=%s", rec.Code, rec.Body.String())
	}
	got, _ = database.GetJob(job.ID)
	if got.ProgressPercent != 20 {
		t.Fatalf("owner progress = %v, want 20", got.ProgressPercent)
	}
}

// TestStaleChunkOnlyReportsRejected covers the stream-only report shape the
// production worker actually sends: SendStderrChunk/SendStdoutChunk carry no
// status and no progress. The combined progress+chunk case above is refused by
// the DB-level progress writer, so without this case the handler's top-level
// ownership guard could be removed and the package would stay green — a stale
// chunk would be written straight into the new owner's stream.
func TestStaleChunkOnlyReportsRejected(t *testing.T) {
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
	failoverJob(t, database, job.ID, "worker-old")
	stageRunning(t, database, job.ID, "worker-new")

	// Observe the job's stream the way the CLI does: a real WebSocket client.
	hub := bundle.handler.GetWSHub()
	bundle.handler.StartWSHub()
	wsRouter := chi.NewRouter()
	wsRouter.Get("/ws/jobs/{jobId}", func(w http.ResponseWriter, r *http.Request) {
		websocket.HandleJobLogWithHub(hub, w, r)
	})
	wsSrv := httptest.NewServer(wsRouter)
	defer wsSrv.Close()

	conn, _, err := gorillaws.DefaultDialer.Dial(
		"ws"+strings.TrimPrefix(wsSrv.URL, "http")+"/ws/jobs/"+job.ID, nil)
	if err != nil {
		t.Fatalf("dial job log websocket: %v", err)
	}
	defer conn.Close()
	registered := time.Now().Add(2 * time.Second)
	for hub.TotalClients() < 1 {
		if time.Now().After(registered) {
			t.Fatal("websocket client was never registered with the hub")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Chunk-only stale reports — the old owner's stream frames — must be
	// refused.
	if rec := patchRequest(t, bundle.router, job.ID, protocol.JobUpdateRequest{
		WorkerID:    "worker-old",
		StderrChunk: "stale stderr",
	}); rec.Code != http.StatusConflict {
		t.Fatalf("stale stderr-only report: got %d want 409 body=%s", rec.Code, rec.Body.String())
	}
	if rec := patchRequest(t, bundle.router, job.ID, protocol.JobUpdateRequest{
		WorkerID:    "worker-old",
		StdoutChunk: "c3RhbGU=",
	}); rec.Code != http.StatusConflict {
		t.Fatalf("stale stdout-only report: got %d want 409 body=%s", rec.Code, rec.Body.String())
	}

	readMessage := func() protocol.WSMessage {
		t.Helper()
		if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
			t.Fatalf("set read deadline: %v", err)
		}
		_, data, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("no frame broadcast within 2s: %v", err)
		}
		var msg protocol.WSMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			t.Fatalf("decode frame %q: %v", data, err)
		}
		return msg
	}

	// The owner's next chunk must be the FIRST frame of this stream: if either
	// stale report had been broadcast, its frame would arrive first (the hub
	// enqueues in report order) and carry seq 1.
	if rec := patchRequest(t, bundle.router, job.ID, protocol.JobUpdateRequest{
		WorkerID:    "worker-new",
		StderrChunk: "owner stderr",
	}); rec.Code != http.StatusOK {
		t.Fatalf("owner stderr chunk: got %d want 200 body=%s", rec.Code, rec.Body.String())
	}
	first := readMessage()
	if first.Type != protocol.WSMsgStderr || first.Payload != "owner stderr" || first.Seq != 1 {
		t.Fatalf("first frame = (type=%s seq=%d payload=%q), want the owner's stderr chunk at seq 1: a stale chunk leaked into the stream",
			first.Type, first.Seq, first.Payload)
	}

	// Numbering continues from that base, so the stale frames were absent, not
	// merely reordered.
	if rec := patchRequest(t, bundle.router, job.ID, protocol.JobUpdateRequest{
		WorkerID:    "worker-new",
		StdoutChunk: "b3duZXI=",
	}); rec.Code != http.StatusOK {
		t.Fatalf("owner stdout chunk: got %d want 200 body=%s", rec.Code, rec.Body.String())
	}
	second := readMessage()
	if second.Type != protocol.WSMsgStdout || second.Payload != "b3duZXI=" || second.Seq != 2 {
		t.Fatalf("second frame = (type=%s seq=%d payload=%q), want the owner's stdout chunk at seq 2",
			second.Type, second.Seq, second.Payload)
	}
}

// TestStaleOutputUploadRejected covers the output-attachment path of H6: the
// old owner's late upload must neither attach its file to the job nor overwrite
// the new owner's product.
func TestStaleOutputUploadRejected(t *testing.T) {
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
	failoverJob(t, database, job.ID, "worker-old")
	stageRunning(t, database, job.ID, "worker-new")

	rec := uploadOutputAs(t, bundle.router, job.ID, "worker-old")
	if rec.Code != http.StatusConflict {
		t.Fatalf("stale output upload: got %d want 409 body=%s", rec.Code, rec.Body.String())
	}
	got, _ := database.GetJob(job.ID)
	if got.OutputFiles != "[]" {
		t.Fatalf("stale upload attached output: %q", got.OutputFiles)
	}

	rec = uploadOutputAs(t, bundle.router, job.ID, "worker-new")
	if rec.Code != http.StatusOK {
		t.Fatalf("owner output upload: got %d want 200 body=%s", rec.Code, rec.Body.String())
	}
	got, _ = database.GetJob(job.ID)
	if got.OutputFiles == "[]" || got.OutputFiles == "" {
		t.Fatalf("owner upload was not attached: %q", got.OutputFiles)
	}
}

// TestOutputUploadWithoutWorkerRejected pins the protocol change: output
// uploads must identify their worker, otherwise there is nothing to guard.
func TestOutputUploadWithoutWorkerRejected(t *testing.T) {
	bundle, cleanup := setupOwnershipTest(t)
	defer cleanup()
	database := bundle.db

	mustWorker(t, database, "worker-1")
	job, err := database.CreateJob(`["in.mkv"]`, `["-c:v","libx264"]`, "out.mkv", false)
	if err != nil {
		t.Fatal(err)
	}
	stageRunning(t, database, job.ID, "worker-1")

	rec := uploadOutputAs(t, bundle.router, job.ID, "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("output upload without worker_id: got %d want 400 body=%s", rec.Code, rec.Body.String())
	}
}

// TestUpdateJobWithoutWorkerRejected is the PATCH counterpart of the output
// upload case: a report with no worker identity cannot be ownership-checked,
// so it is refused outright instead of falling back to an unguarded write.
func TestUpdateJobWithoutWorkerRejected(t *testing.T) {
	bundle, cleanup := setupOwnershipTest(t)
	defer cleanup()
	database := bundle.db

	mustWorker(t, database, "worker-1")
	job, err := database.CreateJob(`["in.mkv"]`, `["-c:v","libx264"]`, "out.mkv", false)
	if err != nil {
		t.Fatal(err)
	}
	stageRunning(t, database, job.ID, "worker-1")

	rec := patchRequest(t, bundle.router, job.ID, protocol.JobUpdateRequest{
		Status: protocol.JobStatusRunning,
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status PATCH without worker_id: got %d want 400 body=%s", rec.Code, rec.Body.String())
	}

	// A progress-only report is in the same class: it writes job state, so it
	// needs the same identity.
	rec = patchRequest(t, bundle.router, job.ID, protocol.JobUpdateRequest{
		Progress: 10,
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("progress PATCH without worker_id: got %d want 400 body=%s", rec.Code, rec.Body.String())
	}

	got, err := database.GetJob(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != protocol.JobStatusRunning || got.ProgressPercent != 0 {
		t.Fatalf("unidentified PATCH mutated the job: status=%s progress=%v", got.Status, got.ProgressPercent)
	}
}

func patchRequest(t *testing.T, router chi.Router, jobID string, req protocol.JobUpdateRequest) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPatch, "/api/v1/jobs/"+jobID, bytes.NewReader(body))
	r.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, r)
	return rec
}

func uploadOutputAs(t *testing.T, router chi.Router, jobID, workerID string) *httptest.ResponseRecorder {
	t.Helper()
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, err := writer.CreateFormFile("file", "out.mp4")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write([]byte("transcoded")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	url := "/api/v1/jobs/" + jobID + "/output"
	if workerID != "" {
		url += "?worker_id=" + workerID
	}
	r := httptest.NewRequest(http.MethodPost, url, body)
	r.Header.Set("Authorization", "Bearer test-token")
	r.Header.Set("Content-Type", writer.FormDataContentType())
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, r)
	return rec
}

// failoverJob runs the monitor's failover sequence for a job owned by
// workerID: flip the worker offline and migrate its jobs back to pending.
func failoverJob(t *testing.T, d *db.Database, jobID, workerID string) {
	t.Helper()
	if err := d.UpdateWorkerStatus(workerID, protocol.WorkerStatusOffline); err != nil {
		t.Fatalf("UpdateWorkerStatus: %v", err)
	}
	res, err := d.MigrateJobsFromWorker(workerID, "heartbeat_timeout", 3)
	if err != nil {
		t.Fatalf("MigrateJobsFromWorker: %v", err)
	}
	for _, id := range res.Migrated {
		if id == jobID {
			return
		}
	}
	t.Fatalf("job %s was not migrated (migrated=%v)", jobID, res.Migrated)
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
	if err := dbtest.SetJobStatus(t, d, jobID, protocol.JobStatusRunning, nil, nil, nil, nil); err != nil {
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
