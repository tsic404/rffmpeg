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

// TestWSClient_KeepalivePreventsIdleReap is the regression test: a streaming
// job that goes quiet past the read deadline (a long transcode with no
// traffic) used to have the client's 60s read deadline reap an idle but
// healthy connection, losing broadcasts and surfacing a spurious "seq gap" on
// reconnect. The test shrinks wsReadTimeout below the ping interval and serves
// no data after the handshake: without keepalive the connection dies within
// ~wsReadTimeout, with it pings keep it open so no gap can be manufactured.
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
