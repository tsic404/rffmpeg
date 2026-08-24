package websocket

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/tsix404/rffmpeg/pkg/protocol"
)

// runHub starts a Hub's main loop. Run has no built-in exit; the goroutine
// is scoped to the test process (same as production's `go h.wsHub.Run()`),
// so no stop function is needed.
func runHub(h *Hub) {
	go h.Run()
}

// newTestClient creates a Hub Client backed by a REAL WebSocket connection
// (upgraded over an httptest server). A nil conn would SIGSEGV when the hub
// calls client.Close() during Unregister.
func newTestClient(t *testing.T, h *Hub, jobID string) *Client {
	t.Helper()
	upgrader := &websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		// Keep the connection open until the test ends; ReadMessage returns
		// on close and the handler exits.
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	t.Cleanup(func() {
		srv.Close()
	})

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		srv.Close()
		t.Fatalf("dial test websocket: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	// The server handler is the sole reader on this connection: gorilla
	// websocket conns must have exactly one concurrent reader, and the
	// client-side conn here is only kept open so the hub's Close() has a
	// real fd to work with.

	return NewClient(conn, jobID, h)
}

// waitForSeq polls until pred over the hub's seq map value for jobID is true.
func waitForSeq(t *testing.T, h *Hub, jobID string, pred func(int64, bool) bool) int64 {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		h.mu.RLock()
		v, ok := h.seq[jobID]
		h.mu.RUnlock()
		if pred(v, ok) {
			return v
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting on seq state for job %s", jobID)
	return 0
}

// TestHub_SeqCounterOutlivesClientDisconnect is the regression test for the
// review blocker: the per-job seq counter must NOT reset when the last client
// unregisters. A reconnecting client carries lastSeq from the old stream; a
// counter reset would renumber new broadcasts from 1 and silently disable gap
// detection across the disconnect window.
func TestHub_SeqCounterOutlivesClientDisconnect(t *testing.T) {
	hub := NewHub()
	runHub(hub)

	jobID := "job-regression"

	msg1 := protocol.NewStderrMessage(jobID, "one")
	if err := hub.BroadcastWSMessage(msg1); err != nil {
		t.Fatalf("broadcast 1: %v", err)
	}
	seqAfterFirst := waitForSeq(t, hub, jobID, func(v int64, ok bool) bool { return ok && v == 1 })
	if seqAfterFirst != 1 {
		t.Fatalf("expected seq=1 after first broadcast, got %d", seqAfterFirst)
	}

	// Last client registers then unregisters — previously this deleted the counter.
	// The client uses a real WebSocket connection: Unregister calls
	// client.Close(), which panics on a nil conn.
	client := newTestClient(t, hub, jobID)
	hub.Register(client)
	// Give Run() time to process register before unregister.
	waitForSeq(t, hub, jobID, func(v int64, _ bool) bool { return v >= 1 })
	hub.Unregister(client)
	waitForSeq(t, hub, jobID, func(_ int64, _ bool) bool { return true })

	// Counter must still exist with its previous value.
	hub.mu.RLock()
	v, ok := hub.seq[jobID]
	hub.mu.RUnlock()
	if !ok || v != 1 {
		t.Fatalf("seq counter must survive last-client unregister: got value=%d ok=%v, want 1/true", v, ok)
	}

	// A broadcast after reconnection must continue from 2, not restart at 1.
	msg2 := protocol.NewStderrMessage(jobID, "two")
	if err := hub.BroadcastWSMessage(msg2); err != nil {
		t.Fatalf("broadcast 2: %v", err)
	}
	seqAfterSecond := waitForSeq(t, hub, jobID, func(v int64, _ bool) bool { return v >= 2 })
	if seqAfterSecond != 2 {
		t.Errorf("post-reconnect broadcast must continue sequence: expected next seq 2, got %d", seqAfterSecond)
	}
}

// TestHub_SeqCounterDeletedOnTerminalBroadcast verifies the counter cleanup
// happens exactly once the job reaches a terminal status.
func TestHub_SeqCounterDeletedOnTerminalBroadcast(t *testing.T) {
	hub := NewHub()
	runHub(hub)

	jobID := "job-terminal"

	for i := 0; i < 3; i++ {
		if err := hub.BroadcastStderr(jobID, "chunk"); err != nil {
			t.Fatalf("broadcast %d: %v", i, err)
		}
	}
	time.Sleep(50 * time.Millisecond)
	hub.mu.RLock()
	_, ok := hub.seq[jobID]
	hub.mu.RUnlock()
	if !ok {
		t.Fatal("non-terminal status broadcast must not delete the seq counter")
	}

	// Terminal status must delete it.
	if err := hub.BroadcastComplete(jobID, 0); err != nil {
		t.Fatalf("complete broadcast: %v", err)
	}
	waitForSeq(t, hub, jobID, func(_ int64, ok bool) bool { return !ok })

	// A later broadcast for the same job starts a fresh stream. The error
	// message is itself terminal, so the counter is deleted immediately and
	// the next data message restarts at 1.
	if err := hub.BroadcastError(jobID, "late"); err != nil {
		t.Fatalf("error broadcast: %v", err)
	}
	waitForSeq(t, hub, jobID, func(_ int64, ok bool) bool { return !ok })

	if err := hub.BroadcastStderr(jobID, "new-stream"); err != nil {
		t.Fatalf("post-terminal stderr: %v", err)
	}
	seq := waitForSeq(t, hub, jobID, func(v int64, ok bool) bool { return ok && v == 1 })
	if seq != 1 {
		t.Errorf("fresh stream after terminal must start at seq 1, got %d", seq)
	}
}

// TestHub_TerminalStatusVariantsAllCleanup verifies every terminal status
// deletes the counter.
func TestHub_TerminalStatusVariantsAllCleanup(t *testing.T) {
	statuses := []protocol.JobStatus{
		protocol.JobStatusCompleted,
		protocol.JobStatusFailed,
		protocol.JobStatusCancelled,
		protocol.JobStatusTimeout,
	}
	for _, st := range statuses {
		t.Run(string(st), func(t *testing.T) {
			hub := NewHub()
			runHub(hub)

			jobID := "job-term-" + string(st)
			if err := hub.BroadcastStderr(jobID, "x"); err != nil {
				t.Fatalf("stderr broadcast: %v", err)
			}
			waitForSeq(t, hub, jobID, func(v int64, ok bool) bool { return ok && v == 1 })

			if err := hub.BroadcastStatus(jobID, st, 0, ""); err != nil {
				t.Fatalf("status broadcast: %v", err)
			}
			waitForSeq(t, hub, jobID, func(_ int64, ok bool) bool { return !ok })
		})
	}
}
