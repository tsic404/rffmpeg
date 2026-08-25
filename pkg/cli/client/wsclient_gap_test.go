package client

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// lines, then a completion message, then closes the connection.
func wsGapTestServer(t *testing.T, messages []string, upgrader *websocket.Upgrader) *httptest.Server {
	t.Helper()
	if upgrader == nil {
		upgrader = &websocket.Upgrader{}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/jobs/job-1/log", func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for _, m := range messages {
			if err := conn.WriteMessage(websocket.TextMessage, []byte(m)); err != nil {
				return
			}
		}
		// Hold the connection open briefly so the client drains all frames,
		// then close with a NORMAL_CLOSURE frame: an abrupt drop (1006)
		// would trigger the client's auto-reconnect, which replays the same
		// messages and can false-positive the seq==1-after-restart guard.
		time.Sleep(300 * time.Millisecond)
		conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		_ = conn.WriteMessage(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
		time.Sleep(50 * time.Millisecond) // let the frame flush
	})
	return httptest.NewServer(mux)
}

// TestWSClient_GapDetected verifies acceptance criterion 4: when the client
// receives sequenced messages with a hole (seq 1, then 3), it must flag a gap
// instead of silently continuing.
func TestWSClient_GapDetected(t *testing.T) {
	msgs := []string{
		`{"type":"stderr","job_id":"job-1","payload":"one","seq":1}`,
		`{"type":"stderr","job_id":"job-1","payload":"three","seq":3}`,
	}
	srv := wsGapTestServer(t, msgs, nil)
	defer srv.Close()

	var stderrChunks []string
	c := NewWSClient(srv.URL, "job-1", "",
		WithOnStderr(func(chunk string) { stderrChunks = append(stderrChunks, chunk) }),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("connect: %v", err)
	}
	go c.Listen(ctx)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !c.HasGap() {
		time.Sleep(10 * time.Millisecond)
	}
	c.Close()

	if len(stderrChunks) != 2 {
		t.Errorf("expected 2 stderr chunks delivered, got %d", len(stderrChunks))
	}
	if !c.HasGap() {
		t.Error("expected gap detection after seq jump 1 -> 3, HasGap() = false")
	}
}

// TestWSClient_NoGapWithoutHole verifies that contiguous sequences do not
// trigger gap detection.
func TestWSClient_NoGapWithoutHole(t *testing.T) {
	msgs := []string{
		`{"type":"stderr","job_id":"job-1","payload":"one","seq":1}`,
		`{"type":"stderr","job_id":"job-1","payload":"two","seq":2}`,
		`{"type":"complete","job_id":"job-1","data":{"exit_code":0},"seq":3}`,
	}
	srv := wsGapTestServer(t, msgs, nil)
	defer srv.Close()

	c := NewWSClient(srv.URL, "job-1", "")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("connect: %v", err)
	}
	done := make(chan struct{})
	go func() {
		c.Listen(ctx)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
	}
	c.Close()

	if c.HasGap() {
		t.Error("contiguous seq 1,2,3 must not be flagged as a gap")
	}
}

// TestWSClient_UnsequencedMessagesDoNotTriggerGap verifies backward compat:
// servers that don't stamp seq (older versions) produce no false positives.
func TestWSClient_UnsequencedMessagesDoNotTriggerGap(t *testing.T) {
	msgs := []string{
		`{"type":"stderr","job_id":"job-1","payload":"a"}`,
		`{"type":"stderr","job_id":"job-1","payload":"b"}`,
	}
	srv := wsGapTestServer(t, msgs, nil)
	defer srv.Close()

	c := NewWSClient(srv.URL, "job-1", "")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("connect: %v", err)
	}
	go c.Listen(ctx)
	time.Sleep(300 * time.Millisecond)
	c.Close()

	if c.HasGap() {
		t.Error("unsequenced messages must not trigger gap detection")
	}
}

// TestStreamJobLogs_GapFailsJob verifies that the streaming-output consumer
// surfaces a detected gap as an error instead of reporting success — no
// silently corrupted stdout may pass as complete.
func TestStreamJobLogs_GapFailsJob(t *testing.T) {
	// seq jumps from 1 to 5 mid-stream; complete arrives after the gap.
	msgs := []string{
		`{"type":"stdout","job_id":"job-1","payload":"AAAA","seq":1}`,
		`{"type":"stdout","job_id":"job-1","payload":"QkJCQg==","seq":5}`,
		`{"type":"complete","job_id":"job-1","data":{"exit_code":0},"seq":6}`,
	}
	srv := wsGapTestServer(t, msgs, nil)
	defer srv.Close()

	err := StreamJobLogs(context.Background(), srv.URL, "job-1", "", true)
	if err == nil || !strings.Contains(err.Error(), "gap") {
		t.Errorf("expected gap error from StreamJobLogs, got %v", err)
	}
}

// TestWSClient_SeqRestartAfterDataStillFlagsGap proves the guard is not a
// blanket amnesty: when data-bearing messages flow and numbering THEN resets
// (case 2: seq==1 && lastSeq>1 && sawDataAfterRestart), messages between
// lastSeq and the reset are genuinely gone — still a gap.
func TestWSClient_SeqRestartAfterDataStillFlagsGap(t *testing.T) {
	msgs := []string{
		`{"type":"stdout","job_id":"job-1","payload":"AAAA","seq":4}`,
		// Hub restarted mid-stream; new data renumbered from 1 while the old
		// base was 4: chunks between are provably lost. Data already flowed
		// on this connection (seq 4 above), so the sawDataAfterRestart branch
		// must fire — no reconnection amnesty applies here.
		`{"type":"stdout","job_id":"job-1","payload":"QkJCQg==","seq":1}`,
		`{"type":"complete","job_id":"job-1","data":{"exit_code":0},"seq":2}`,
	}
	srv := wsGapTestServer(t, msgs, nil)
	defer srv.Close()

	err := StreamJobLogs(context.Background(), srv.URL, "job-1", "", true)
	if err == nil || !strings.Contains(err.Error(), "gap") {
		t.Errorf("expected gap error for reset-after-data, got %v", err)
	}
}

// TestWSClient_TerminalRenumberOnSameConnectionNoGap is the core TSI-2382
// regression test (QA-reproduced 3/3, no reconnect involved): on ONE
// connection the hub's terminal-status broadcast deleted the counter, so the
// trailing complete broadcast — and any stderr chunks reported in the same
// worker request — were renumbered starting at seq 1 while data had already
// flowed. That reset is a hub bookkeeping artifact, not lost data: the client
// must NOT fail the job over it. (The hub fix stops producing this wire; the
// client fix makes it tolerated either way.)
func TestWSClient_TerminalRenumberOnSameConnectionNoGap(t *testing.T) {
	msgs := []string{
		// Streaming phase: stdout chunks flow (data seen, lastSeq climbs).
		`{"type":"stdout","job_id":"job-1","payload":"AAAA","seq":1}`,
		`{"type":"stdout","job_id":"job-1","payload":"QkJCQg==","seq":2}`,
		// Terminal status at seq 3. With the pre-fix hub behavior the
		// counter was deleted HERE, so the wire below carries the OLD
		// numbering the QA run observed 3/3 times: trailing broadcasts
		// restart from seq 1 on the same connection.
		`{"type":"status","job_id":"job-1","data":{"status":"completed","exit_code":0},"seq":3}`,
		`{"type":"stderr","job_id":"job-1","payload":"late drain","seq":1}`,
		`{"type":"complete","job_id":"job-1","data":{"exit_code":0},"seq":2}`,
	}
	srv := wsGapTestServer(t, msgs, nil)
	defer srv.Close()

	var mu sync.Mutex
	var stdoutChunks []string
	c := NewWSClient(srv.URL, "job-1", "",
		WithOnStdout(func(chunk []byte) { mu.Lock(); stdoutChunks = append(stdoutChunks, string(chunk)); mu.Unlock() }),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("connect: %v", err)
	}
	done := make(chan struct{})
	go func() {
		c.Listen(ctx)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
	}
	c.Close()

	mu.Lock()
	defer mu.Unlock()
	if len(stdoutChunks) != 2 {
		t.Errorf("expected 2 stdout chunks delivered, got %d", len(stdoutChunks))
	}
	if c.HasGap() {
		t.Error("same-connection terminal renumbering must not be flagged as a gap (TSI-2382)")
	}
}

// wsReconnectServer serves the WS endpoint once per connection: the FIRST
// dial receives firstPhase messages then is dropped ABRUPTLY (no close
// frame — a graceful NORMAL_CLOSURE would make Listen treat it as an
// intentional shutdown and return without reconnecting). The forced abnormal
// drop exercises the real Listen() auto-reconnect path, including connect()'s
// flag reset; every subsequent dial (reconnections) gets secondPhase
// messages followed by a graceful close.
func wsReconnectServer(t *testing.T, firstPhase, secondPhase []string) *httptest.Server {
	t.Helper()
	upgrader := &websocket.Upgrader{}
	var conns int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/jobs/job-1/log", func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		msgs := secondPhase
		first := atomic.AddInt32(&conns, 1) == 1
		if first {
			msgs = firstPhase
		}
		for _, m := range msgs {
			if err := conn.WriteMessage(websocket.TextMessage, []byte(m)); err != nil {
				return
			}
		}
		time.Sleep(100 * time.Millisecond) // let the frames flush
		if first {
			// Abrupt drop: client observes an abnormal closure (1006) and
			// auto-reconnects.
			return
		}
		conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		_ = conn.WriteMessage(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
		time.Sleep(50 * time.Millisecond)
	})
	return httptest.NewServer(mux)
}

// TestWSClient_SeqRestartAfterReconnectNoGap is the TSI-2382 regression test,
// exercising the REAL reconnect path: a streaming job (-f mpegts -) streams
// stdout (seq 1..2), completes, and the connection drops. The client
// auto-reconnects — resetting sawDataAfterRestart in connect() — and only
// then receives late stderr that the fresh hub renumbered from 1. No data has
// flowed on the new connection, so nothing proves loss: the reset must be
// adopted as a new base, NOT flagged as a gap (the original code failed such
// jobs with exit code 1 despite intact output).
func TestWSClient_SeqRestartAfterReconnectNoGap(t *testing.T) {
	first := []string{
		`{"type":"stdout","job_id":"job-1","payload":"AAAA","seq":1}`,
		`{"type":"stdout","job_id":"job-1","payload":"QkJCQg==","seq":2}`,
		`{"type":"complete","job_id":"job-1","data":{"exit_code":0},"seq":3}`,
	}
	second := []string{
		// Late-arriving stderr batch: hub lost its counter and renumbered
		// starting at 1.
		`{"type":"stderr","job_id":"job-1","payload":"late stderr one","seq":1}`,
		`{"type":"stderr","job_id":"job-1","payload":"late stderr two","seq":2}`,
	}
	srv := wsReconnectServer(t, first, second)
	defer srv.Close()

	var mu sync.Mutex
	var stdoutChunks, stderrChunks []string
	c := NewWSClient(srv.URL, "job-1", "",
		WithOnStdout(func(chunk []byte) { mu.Lock(); stdoutChunks = append(stdoutChunks, string(chunk)); mu.Unlock() }),
		WithOnStderr(func(chunk string) { mu.Lock(); stderrChunks = append(stderrChunks, chunk); mu.Unlock() }),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("initial connect: %v", err)
	}

	listenDone := make(chan error, 1)
	go func() { listenDone <- c.Listen(ctx) }()

	// Wait until the reconnect actually happened and the second phase was
	// consumed (the first connection's close triggers auto-reconnect inside
	// Listen; the server hands out phase-two messages on the new conn).
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		gotStderr := len(stderrChunks)
		mu.Unlock()
		if gotStderr == 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	c.Close()

	mu.Lock()
	defer mu.Unlock()
	if len(stdoutChunks) != 2 {
		t.Errorf("expected 2 stdout chunks from phase one, got %d", len(stdoutChunks))
	}
	if len(stderrChunks) != 2 {
		t.Errorf("expected 2 stderr chunks from phase two, got %d", len(stderrChunks))
	}

	// The pre-reset invariant must have held on the real path: data flowed
	// during phase one AND the reconnect happened.
	if len(stdoutChunks) < 2 || len(stderrChunks) == 0 {
		t.Fatal("test did not exercise both phases; reconnect path not covered")
	}
	if c.HasGap() {
		t.Error("seq restart after reconnect with no post-reconnect data must not be flagged as a gap (TSI-2382)")
	}
}
