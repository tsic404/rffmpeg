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

// TSI-2388: the server process died silently during QA. Every broadcast
// path (BroadcastStatus/Stderr/Stdout/Progress/Complete/Error →
// BroadcastWSMessage) sends on the unbuffered h.broadcast channel, which is
// only drained by the Run loop. If Run ever stops — a panic inside its loop,
// a missed StartWSHub call, or a blocked iteration — every handler goroutine
// that touches a job status update blocks forever on the channel send while
// holding no way to report the failure: the server looks alive (listeners
// open) but never answers. The same deadlock is reachable from inside Run
// itself: the buffer-full branch does `h.unregister <- client`, but Run is
// the only reader of h.unregister, so one slow client with a full 256-slot
// send buffer freezes the entire hub loop and, through it, every HTTP
// handler that broadcasts.
//
// These tests pin both halves of that contract:
//
//  1. Broadcast must not block indefinitely when the hub loop is down — it
//     must return an error instead of wedging the caller goroutine forever.
//  2. The Run-loop broadcast path must never block on unregister: a full
//     client buffer must drop the message (and shed the client), not stall
//     delivery to every other connected client.

// TestHub_BroadcastDoesNotBlockWhenLoopDown covers half 1: with nobody
// draining h.broadcast, Hub.Broadcast must return an error within a bounded
// time instead of blocking forever. Before the fix this test hangs until the
// go test deadline kills the whole package.
func TestHub_BroadcastDoesNotBlockWhenLoopDown(t *testing.T) {
	h := NewHub() // Run() intentionally NOT started

	// Fill the buffered channel first: a single send into an empty 256-slot
	// buffer would succeed even with no Run loop. The contract under test is
	// that a caller never blocks indefinitely once the buffer is exhausted.
	for range hubBroadcastBuffer {
		if err := h.Broadcast("job-fill", []byte(`{}`)); err != nil {
			t.Fatalf("fill send %d must fit the buffer: %v", err, err)
		}
	}

	done := make(chan error, 1)
	go func() {
		done <- h.BroadcastStatus("job-1", protocol.JobStatusFailed, -1, "boom")
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Broadcast against a full/stopped hub must return an error, not silently succeed")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Broadcast blocked >10s with no Run loop draining the channel — server-wide deadlock")
	}
}

// TestHub_RunSurvivesFullClientBuffer covers half 2: a client whose send
// buffer is full must not wedge the Run loop, and — the review blocker —
// the shed must be atomic: after the hub closes a saturated client, a
// broadcast for the SAME job must not select-send on the now-closed send
// channel (a panic that kills the Run goroutine, i.e. silent server death).
//
// The healthy client is observed through its own dial-up connection (the
// test acts as the WebSocket peer), so delivery proves the Run loop kept
// dispatching through the shed and the post-shed same-job broadcasts.
func TestHub_RunSurvivesFullClientBuffer(t *testing.T) {
	upgrader := &websocket.Upgrader{}
	received := make(chan []byte, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			select {
			case received <- data:
			default:
			}
		}
	}))
	t.Cleanup(srv.Close)

	hub := NewHubWithSeqStore(nil)
	runHub(hub)

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial observer websocket: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	fast := NewClient(conn, "job-fast", hub)
	go fast.WritePump()
	go fast.ReadPump()
	hub.Register(fast)

	// The saturated client gets a hub-managed connection with NO WritePump:
	// nothing drains client.send, so the 256-slot buffer fills and stays
	// full — the exact precondition of the shed path. (With a running
	// WritePump the loop batches and drains faster than the flood fills,
	// and the shed never triggers.) ReadPump runs so a real Unregister
	// path exists for the race the fix must close.
	slow := newTestClient(t, hub, "job-slow")
	go slow.ReadPump()
	hub.Register(slow)

	// Wait for both registrations so the first floods are dispatched rather
	// than dropped for an unknown job. waitForTotal polls TotalClients,
	// which reads h.clients under RLock — the authoritative map.
	waitForTotal(t, hub, 2)

	for i := range hubBroadcastBuffer * 4 {
		if err := hub.BroadcastStderr("job-slow", "chunk"); err != nil {
			t.Fatalf("broadcast %d to saturated client: %v", i, err)
		}
	}

	// Block until the Run loop actually processed the shed (client removed
	// from the map), then verify the invariant directly: the closed client
	// is gone BEFORE any later broadcast could reach it. This also drains
	// any race with the async ReadPump Unregister.
	waitForTotal(t, hub, 1)

	// Review-blocker regression: keep broadcasting to the SAME job after
	// the shed. Before the fix this raced the close→map-removal window and
	// select-sent on the closed channel inside the Run loop — a panic that
	// killed the goroutine. The failure surfaces below as the observer
	// never receiving "alive".
	for range 10 {
		if err := hub.BroadcastStderr("job-slow", "after-close"); err != nil {
			t.Fatalf("post-shed broadcast to job-slow: %v", err)
		}
	}

	if err := hub.BroadcastStderr("job-fast", "alive"); err != nil {
		t.Fatalf("broadcast to healthy client: %v", err)
	}

	select {
	case data := <-received:
		if !strings.Contains(string(data), "alive") && !strings.Contains(string(data), `"type":"stderr"`) {
			t.Fatalf("observer got unexpected frame: %.80s", string(data))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("healthy client received nothing after flooding slow client — Run loop blocked on unregister")
	}
}

// waitForTotal blocks until the hub's total client count reaches n — i.e.
// the Run loop has processed the shed and the closed client is out of the
// map. Fails the test on timeout.
func waitForTotal(t *testing.T, h *Hub, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if h.TotalClients() == n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for hub to shed down to %d clients; have %d", n, h.TotalClients())
}
