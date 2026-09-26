package handlers_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	gorillaws "github.com/gorilla/websocket"

	"github.com/tsic404/rffmpeg/pkg/protocol"
	"github.com/tsic404/rffmpeg/pkg/server/websocket"
)

// reportJobProgress PATCHes a progress/ETA report for jobID, as a worker does.
func reportJobProgress(t *testing.T, router *chi.Mux, jobID string, progress float64, eta int) {
	t.Helper()
	body, err := json.Marshal(protocol.JobUpdateRequest{Progress: progress, EtaSeconds: eta})
	if err != nil {
		t.Fatalf("marshal update: %v", err)
	}
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/jobs/"+jobID, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH progress %.1f = %d, want 200; body=%s", progress, rec.Code, rec.Body.String())
	}
}

// getJobProgress reads back the stored progress/ETA of jobID via the public API.
func getJobProgress(t *testing.T, router *chi.Mux, jobID string) (float64, int) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/jobs/"+jobID, nil)
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET job = %d, want 200", rec.Code)
	}
	var resp struct {
		Job struct {
			ProgressPercent float64 `json:"progress_percent"`
			EtaSeconds      int     `json:"eta_seconds"`
		} `json:"job"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode job: %v", err)
	}
	return resp.Job.ProgressPercent, resp.Job.EtaSeconds
}

// TestJobProgressNeverRegresses locks the job-level progress contract on the
// server. A job migrated after a worker failure re-encodes from zero on its new
// worker and reports low percentages again; those reports must not rewind the
// value the API and the CLI already published — the reported 71.1% → 0.5%
// sequence. A regressing report lands on neither sink (stored value and
// broadcast), while equal and higher reports still land so the ETA keeps
// refreshing at a held percent.
func TestJobProgressNeverRegresses(t *testing.T) {
	h, router, cleanup := setupTest(t)
	defer cleanup()

	job, err := h.GetDB().CreateJob(`["in.mp4"]`, `["-c:v","libx264"]`, "out.mp4", false)
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	reportJobProgress(t, router, job.ID, 71.1, 7)
	if got, _ := getJobProgress(t, router, job.ID); got != 71.1 {
		t.Fatalf("stored progress = %v, want 71.1", got)
	}

	// The migrated worker's attempt restarted from zero: rejected outright, so
	// the ETA that travelled with the regressing report is not stored either.
	reportJobProgress(t, router, job.ID, 0.5, 900)
	got, eta := getJobProgress(t, router, job.ID)
	if got != 71.1 {
		t.Errorf("progress after a regressing report = %v, want 71.1 (rewound)", got)
	}
	if eta != 7 {
		t.Errorf("eta after a regressing report = %d, want 7 (the report must not land)", eta)
	}

	// Equal and higher reports still land: the ETA refreshes while the percent
	// is held at the mark, and the restarted attempt advances past it.
	reportJobProgress(t, router, job.ID, 71.1, 3)
	if _, eta := getJobProgress(t, router, job.ID); eta != 3 {
		t.Errorf("eta at an equal percent = %d, want 3 (equal reports must still land)", eta)
	}
	reportJobProgress(t, router, job.ID, 80, 2)
	if got, _ := getJobProgress(t, router, job.ID); got != 80 {
		t.Errorf("progress after a higher report = %v, want 80", got)
	}
}

// TestJobProgressBroadcastGate covers the stream half of that contract: a
// rejected report must not reach CLI subscribers, while an equal report still
// must, since the CLI renders the stream rather than the stored row. Without
// this the gate could regress to an unconditional broadcast and the
// stored-value assertions above would stay green while every CLI showed the
// rewind again.
func TestJobProgressBroadcastGate(t *testing.T) {
	h, router, cleanup := setupTest(t)
	defer cleanup()

	job, err := h.GetDB().CreateJob(`["in.mp4"]`, `["-c:v","libx264"]`, "out.mp4", false)
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	// Subscribe a real WebSocket client to the job, the way the CLI does.
	hub := h.GetWSHub()
	h.StartWSHub() // the hub dispatches registrations from its Run loop
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

	// The hub registers asynchronously; a broadcast before that would be
	// dispatched to no one and read as "not broadcast".
	registered := time.Now().Add(2 * time.Second)
	for hub.TotalClients() < 1 {
		if time.Now().After(registered) {
			t.Fatal("websocket client was never registered with the hub")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// readProgress returns the next progress frame broadcast within window.
	// A timeout fails the test: every call here expects a frame (the negative
	// assertion below is expressed as wire order, not as expected silence, since
	// a read timeout corrupts a gorilla connection).
	readProgress := func(window time.Duration) (percent float64, eta int) {
		until := time.Now().Add(window)
		for {
			if err := conn.SetReadDeadline(until); err != nil {
				t.Fatalf("set read deadline: %v", err)
			}
			_, data, err := conn.ReadMessage()
			if err != nil {
				t.Fatalf("no progress frame broadcast within %v: %v", window, err)
			}
			var msg protocol.WSMessage
			if err := json.Unmarshal(data, &msg); err != nil || msg.Type != protocol.WSMsgProgress {
				continue // heartbeats and other frames are not progress
			}
			payload, _ := msg.Data.(map[string]interface{})
			percent, _ = payload["percent"].(float64)
			if secs, ok := payload["eta_seconds"].(float64); ok {
				eta = int(secs)
			}
			return percent, eta
		}
	}

	reportJobProgress(t, router, job.ID, 71.1, 7)
	if percent, eta := readProgress(2 * time.Second); percent != 71.1 || eta != 7 {
		t.Fatalf("accepted report broadcast = (%.1f, eta=%d), want (71.1, 7) — a gate that rejects everything would hide the rewind too", percent, eta)
	}

	// A rejected report and the next accepted one, back to back. The hub
	// broadcasts in report order, so a regressing frame would surface here as
	// 0.5 instead of the equal-percent frame that must follow it.
	reportJobProgress(t, router, job.ID, 0.5, 900)
	reportJobProgress(t, router, job.ID, 71.1, 3)
	if percent, eta := readProgress(2 * time.Second); percent != 71.1 || eta != 3 {
		t.Errorf("broadcast after a rejected report = (%.1f, eta=%d), want (71.1, 3): a regressing report must not reach subscribers, an equal one still must", percent, eta)
	}
}
