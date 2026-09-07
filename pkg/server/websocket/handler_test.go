package websocket

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/gorilla/websocket"
	"github.com/tsic404/rffmpeg/pkg/protocol"
)

// jobLogRequest builds a GET request carrying the jobId route param that
// HandleJobLogWithValidation reads via chi.URLParam.
func jobLogRequest(t *testing.T, header http.Header) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/jobs/j1/log", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("jobId", "j1")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	return req
}

// TestHandleJobLogWithValidationUpgradeRequired reproduces TSI-2713: a request
// carrying Connection: Upgrade but no Upgrade header is not a WebSocket
// handshake, so the server must answer 426 Upgrade Required (RFC 6455 §4.1)
// instead of gorilla's default 400, with the Upgrade: websocket header and a
// JSON error body matching the 405/404 shape.
func TestHandleJobLogWithValidationUpgradeRequired(t *testing.T) {
	req := jobLogRequest(t, http.Header{"Connection": []string{"Upgrade"}})
	rec := httptest.NewRecorder()

	HandleJobLogWithValidation(NewHub(), nil, rec, req)

	if rec.Code != http.StatusUpgradeRequired {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusUpgradeRequired, rec.Body.String())
	}
	if got := rec.Header().Get("Upgrade"); got != "websocket" {
		t.Errorf("Upgrade header = %q, want websocket", got)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	// gorilla's 400 path pre-sets these before the 426 rewrite; they must not
	// leak into the rewritten response.
	for _, h := range []string{"Sec-Websocket-Version", "X-Content-Type-Options"} {
		if got := rec.Header().Get(h); got != "" {
			t.Errorf("header %q = %q, want removed", h, got)
		}
	}

	var resp protocol.ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode body: %v; body=%s", err, rec.Body.String())
	}
	if resp.Code != "upgrade_required" {
		t.Errorf("code = %q, want upgrade_required", resp.Code)
	}
}

// TestHandleJobLogWithValidationUpgradeSucceeds exercises the success path the
// wrapper also covers: a valid handshake must still reach 101 via Hijack and
// register the client with the hub.
func TestHandleJobLogWithValidationUpgradeSucceeds(t *testing.T) {
	hub := NewHub()
	runHub(hub)

	r := chi.NewRouter()
	r.Get("/api/v1/jobs/{jobId}/log", func(w http.ResponseWriter, req *http.Request) {
		HandleJobLogWithValidation(hub, nil, w, req)
	})
	srv := httptest.NewServer(r)
	t.Cleanup(func() { srv.Close() })

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/api/v1/jobs/j1/log"
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	defer conn.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status = %d, want 101", resp.StatusCode)
	}

	// The handler registers the client in the hub asynchronously through the
	// Run loop; poll briefly for it to land.
	deadline := time.Now().Add(2 * time.Second)
	for hub.ClientCount("j1") == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := hub.ClientCount("j1"); got != 1 {
		t.Fatalf("hub client count = %d, want 1", got)
	}
}

// TestHandleJobLogWithValidationNon426HandshakeFailures pins the narrowed
// rewrite scope: handshake failures other than a missing Upgrade header keep
// gorilla's 400, per RFC 6455 §4.2.2.
func TestHandleJobLogWithValidationNon426HandshakeFailures(t *testing.T) {
	cases := []struct {
		name   string
		header http.Header
	}{
		{
			name: "malformed Sec-WebSocket-Key",
			header: http.Header{
				"Connection":            []string{"Upgrade"},
				"Upgrade":               []string{"websocket"},
				"Sec-Websocket-Version": []string{"13"},
				"Sec-Websocket-Key":     []string{"not-base64"},
			},
		},
		{
			name: "missing Sec-WebSocket-Version",
			header: http.Header{
				"Connection":        []string{"Upgrade"},
				"Upgrade":           []string{"websocket"},
				"Sec-Websocket-Key": []string{"dGhlIHNhbXBsZSBub25jZQ=="},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := jobLogRequest(t, tc.header)
			rec := httptest.NewRecorder()
			HandleJobLogWithValidation(NewHub(), nil, rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
			}
		})
	}
}
