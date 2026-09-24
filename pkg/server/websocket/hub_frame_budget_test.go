package websocket

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/tsic404/rffmpeg/pkg/protocol"
)

// TestHub_WritePumpCapsFrameAtClientReadLimit is the regression test for the
// streaming-output data loss seen with `-f mp4 -`: the worker delivers
// transcoded bytes in bursts of WSMsgStdout messages, each up to
// WSMaxStdoutMessageBytes of base64 payload. WritePump drained its whole send
// queue into a single text frame with no byte cap, so a burst produced a frame
// larger than the CLI's SetReadLimit; gorilla discards such a frame WHOLE and
// the CLI can only report the loss as a reconnected sequence gap —
// "streaming output incomplete", with the transcoded bytes gone.
//
// The queue is filled before the pump starts, which is the state the old
// drain-everything loop turned into one oversized frame; the reading end is
// limited exactly like the CLI's, so an over-limit frame fails here the way it
// fails in production.
func TestHub_WritePumpCapsFrameAtClientReadLimit(t *testing.T) {
	const jobID = "job-stream"

	upgrader := &websocket.Upgrader{}
	serverConns := make(chan *websocket.Conn, 1)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		serverConns <- conn
		<-release
		conn.Close()
	}))
	t.Cleanup(func() {
		close(release)
		srv.Close()
	})

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"
	clientConn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	t.Cleanup(func() { clientConn.Close() })
	serverConn := <-serverConns

	hub := NewHubWithSeqStore(nil)
	runHub(hub)
	client := NewClient(serverConn, jobID, hub)
	hub.Register(client)
	waitForTotal(t, hub, 1)

	// One message of the largest size a producer can emit, times enough of
	// them that a single frame holding the burst would exceed the client's
	// read limit.
	payload := strings.Repeat("A", protocol.WSMaxStdoutMessageBytes)
	burst := protocol.WSClientReadLimit/protocol.WSMaxStdoutMessageBytes + 2
	for i := range burst {
		if err := hub.BroadcastStdout(jobID, payload); err != nil {
			t.Fatalf("broadcast %d: %v", i, err)
		}
	}
	// No WritePump yet, so nothing drains client.send: wait for the hub to
	// have dispatched the whole burst into the queue before the pump starts.
	deadline := time.Now().Add(2 * time.Second)
	for len(client.send) < burst && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if queued := len(client.send); queued != burst {
		t.Fatalf("hub dispatched %d/%d messages to the client queue", queued, burst)
	}

	go client.WritePump()

	// The CLI's acceptance limit: an over-limit frame fails this read the same
	// way it fails the real client.
	clientConn.SetReadLimit(protocol.WSClientReadLimit)

	lines := 0
	for lines < burst {
		_, data, err := clientConn.ReadMessage()
		if err != nil {
			t.Fatalf("read frame after %d/%d messages: %v", lines, burst, err)
		}
		lines += strings.Count(string(data), "\n") + 1
	}
	if lines != burst {
		t.Errorf("expected all %d messages across the frames, got %d", burst, lines)
	}
}
