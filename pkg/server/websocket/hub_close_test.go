package websocket

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// TestHubCloseSendsNormalCloseFrame pins C-104's server-side normal closure:
// http.Server.Shutdown does not close hijacked connections, so the hub's Close
// must, and the peer must observe code 1000 rather than an abnormal drop.
func TestHubCloseSendsNormalCloseFrame(t *testing.T) {
	hub := NewHub()
	runHub(hub)

	upgrader := &websocket.Upgrader{}
	serverConn := make(chan *websocket.Conn, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		serverConn <- conn
	}))
	t.Cleanup(srv.Close)

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"
	peer, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { peer.Close() })

	client := NewClient(<-serverConn, "job-close", hub)
	go client.WritePump()
	hub.Register(client)

	deadline := time.Now().Add(2 * time.Second)
	for hub.TotalClients() != 1 {
		if time.Now().After(deadline) {
			t.Fatal("client was not registered")
		}
		time.Sleep(time.Millisecond)
	}

	hub.Close()

	_ = peer.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, _, readErr := peer.ReadMessage()
	var closeErr *websocket.CloseError
	if !errors.As(readErr, &closeErr) {
		t.Fatalf("read error = %v, want a close frame", readErr)
	}
	if closeErr.Code != websocket.CloseNormalClosure {
		t.Fatalf("close code = %d, want %d", closeErr.Code, websocket.CloseNormalClosure)
	}
}
