package client

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/tsic404/rffmpeg/pkg/protocol"
)

// streamingReconnectServer serves the streaming WS endpoint in two phases:
// the first connection delivers one stdout frame and closes with 1000 (the
// server's own shutdown), the second — reached only by a client that reconnects
// after a normal close — delivers the rest and completes. secondGap injects a
// sequence hole on the second connection (seq 2 skipped) to prove a gap is
// still fatal for streaming output.
func streamingReconnectServer(t *testing.T, secondGap bool) (*httptest.Server, *atomic.Int64) {
	t.Helper()

	var attempts atomic.Int64
	var completed atomic.Bool

	writeStdout := func(conn *websocket.Conn, seq int64, chunk string) error {
		msg := protocol.NewStdoutMessage("job-1", base64.StdEncoding.EncodeToString([]byte(chunk)))
		msg.Seq = seq
		data, err := json.Marshal(msg)
		if err != nil {
			return err
		}
		return conn.WriteMessage(websocket.TextMessage, data)
	}
	closeNormally := func(conn *websocket.Conn) {
		_ = conn.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
			time.Now().Add(time.Second))
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/jobs/job-1/log", func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()

		if attempts.Add(1) == 1 {
			if err := writeStdout(conn, 1, "first"); err != nil {
				return
			}
			closeNormally(conn)
			return
		}

		completed.Store(true)
		seq := int64(2)
		if secondGap {
			seq = 3 // expected 2: the skipped frame is the hole under test
		}
		if err := writeStdout(conn, seq, "second"); err != nil {
			return
		}
		complete := protocol.NewCompleteMessage("job-1", 0)
		complete.Seq = seq + 1
		data, err := json.Marshal(complete)
		if err != nil {
			return
		}
		if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
		closeNormally(conn)
	})
	mux.HandleFunc("/api/v1/jobs/job-1", func(w http.ResponseWriter, r *http.Request) {
		status := protocol.JobStatusRunning
		if completed.Load() {
			status = protocol.JobStatusCompleted
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(protocol.JobStatusResponse{
			Job: protocol.JobInfo{ID: "job-1", Status: status},
		})
	})

	return httptest.NewServer(mux), &attempts
}

// captureStdout runs fn with os.Stdout redirected to a pipe and returns what
// the streaming writer emitted.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w
	defer func() { os.Stdout = old }()

	fn()

	if err := w.Close(); err != nil {
		t.Fatalf("close pipe: %v", err)
	}
	os.Stdout = old
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read stdout: %v", err)
	}
	return string(data)
}

// TestWaitForJobWithStreamingOutput_NormalCloseReconnects is the regression
// test for the streaming wait on a server restart: the first connection closes
// with 1000, the client must reconnect (a one-shot HTTP fallback would fail on
// the stopped server), receive the remaining bytes, and finish cleanly.
func TestWaitForJobWithStreamingOutput_NormalCloseReconnects(t *testing.T) {
	srv, attempts := streamingReconnectServer(t, false)
	defer srv.Close()

	oldPoll := pollInterval
	pollInterval = 50 * time.Millisecond
	t.Cleanup(func() { pollInterval = oldPoll })

	cli := New(srv.URL, "", WithMaxRetries(3))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var (
		job *protocol.JobInfo
		err error
		out string
	)
	out = captureStdout(t, func() {
		job, err = cli.WaitForJobWithStreamingOutput(ctx, "job-1", true)
	})

	if err != nil {
		t.Fatalf("WaitForJobWithStreamingOutput failed after a normal server close: %v", err)
	}
	if job == nil || job.Status != protocol.JobStatusCompleted {
		t.Fatalf("expected completed job, got %+v", job)
	}
	if got := attempts.Load(); got < 2 {
		t.Fatalf("WS endpoint attempted %d time(s); a normal close must reconnect", got)
	}
	if out != "firstsecond" {
		t.Fatalf("streamed bytes = %q, want %q", out, "firstsecond")
	}
}

// TestWaitForJobWithStreamingOutput_GapAfterReconnectFails pins C-106: after
// the reconnect, a sequence hole in the streamed output must fail the wait —
// bytes were delivered with a hole. The hole exists only on the second
// connection, so a client that falls back to polling instead of reconnecting
// never sees it and would wrongly succeed.
func TestWaitForJobWithStreamingOutput_GapAfterReconnectFails(t *testing.T) {
	srv, attempts := streamingReconnectServer(t, true)
	defer srv.Close()

	oldPoll := pollInterval
	pollInterval = 50 * time.Millisecond
	t.Cleanup(func() { pollInterval = oldPoll })

	cli := New(srv.URL, "", WithMaxRetries(3))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var err error
	captureStdout(t, func() {
		_, err = cli.WaitForJobWithStreamingOutput(ctx, "job-1", true)
	})

	if err == nil || !strings.Contains(err.Error(), "streaming output incomplete") {
		t.Fatalf("sequence gap after reconnect must fail the streaming wait, got err=%v", err)
	}
	if got := attempts.Load(); got < 2 {
		t.Fatalf("WS endpoint attempted %d time(s); the gap is delivered on the second connection", got)
	}
}
