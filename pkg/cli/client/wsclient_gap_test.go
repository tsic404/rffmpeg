package client

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
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
