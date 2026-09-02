package client

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// TestWSClient_KeepalivePreventsIdleReap is the TSI-2414 regression test.
//
// Production failure mode: a streaming-output job goes quiet for longer than
// the read deadline (a long transcode with no stderr/stdout traffic). Before
// the fix the client had no ping/pong keepalive, so its own 60s read deadline
// reaped an idle but healthy connection; every broadcast the server sent
// during the forced reconnect window was lost, and on reconnect the advanced
// sequence counter surfaced as a spurious "seq gap" — failing an otherwise
// intact job with exit code 1.
//
// The test shrinks wsReadTimeout below the ping interval and serves NO data
// at all after the handshake. Without client-side keepalive the connection
// would die of read-deadline timeout within ~wsReadTimeout; with it, pings
// flow, the server's pong handler (mirrored here) keeps the connection open,
// and the session survives several multiples of the old deadline without a
// single disconnect/reconnect — so no gap can be manufactured.
func TestWSClient_KeepalivePreventsIdleReap(t *testing.T) {
	oldTimeout := time.Duration(wsReadTimeout.Load())
	oldPing := time.Duration(wsPingInterval.Load())
	wsReadTimeout.Store(int64(300 * time.Millisecond))
	wsPingInterval.Store(int64(150 * time.Millisecond))
	defer func() {
		wsReadTimeout.Store(int64(oldTimeout))
		wsPingInterval.Store(int64(oldPing))
	}()
	if time.Duration(wsPingInterval.Load()) >= time.Duration(wsReadTimeout.Load()) {
		t.Fatalf("test precondition: ping interval (%v) must be below the read timeout (%v) so pings, not luck, keep the connection alive", time.Duration(wsPingInterval.Load()), time.Duration(wsReadTimeout.Load()))
	}

	var pongs atomic.Int64
	upgrader := &websocket.Upgrader{}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/jobs/job-1/log", func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		// Mirror the server's ReadPump: count every inbound ping (gorilla
		// consumes control frames inside ReadMessage — they are never
		// returned as message types) and answer with a pong.
		conn.SetPingHandler(func(appData string) error {
			pongs.Add(1)
			return conn.WriteControl(websocket.PongMessage, []byte(appData), time.Now().Add(5*time.Second))
		})
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := NewWSClient(srv.URL, "job-1", "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("connect: %v", err)
	}

	listenDone := make(chan error, 1)
	go func() { listenDone <- c.Listen(ctx) }()

	// Hold the idle connection for 4x the (shrunken) read deadline. Without
	// keepalive Listen returns a read-deadline error well before this.
	hold := 4 * time.Duration(wsReadTimeout.Load())
	select {
	case err := <-listenDone:
		t.Fatalf("connection died during quiet period after %v: %v", hold, err)
	case <-time.After(hold):
	}

	cancel()
	c.Close()

	if got := pongs.Load(); got == 0 {
		t.Error("expected at least one keepalive ping round-trip during the quiet period")
	}
}
