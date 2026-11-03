package handlers

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/tsic404/rffmpeg/pkg/server/transport"
)

// idleStackRouter wires the production middleware order: the 405 normalizer
// runs before the idle deadline middlewares and therefore wraps the writer
// first. Deadline arming must survive that wrapper, which requires it to
// forward Unwrap to the real response writer; a chain that stops there leaves
// every request without an idle bound.
func idleStackRouter(idle time.Duration, writeResult chan<- error) http.Handler {
	r := chi.NewRouter()
	r.Use(NormalizeMethodNotAllowed)
	r.Use(transport.IdleBodyMiddleware(idle))
	r.Use(transport.IdleWriteMiddleware(idle))

	r.Get("/big", func(w http.ResponseWriter, r *http.Request) {
		chunk := make([]byte, 64<<10)
		for range 2048 { // up to 128 MiB, far beyond any socket buffer
			if _, err := w.Write(chunk); err != nil {
				writeResult <- err
				return
			}
		}
		writeResult <- nil
	})
	r.Get("/small", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	})
	r.Post("/echo", func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			w.WriteHeader(http.StatusRequestTimeout)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	return r
}

// Through the real middleware stack, a client that requests a large response
// and then stops reading must be released by the per-write idle deadline.
func TestIdleStackCutsClientThatStopsReading(t *testing.T) {
	const idle = 200 * time.Millisecond
	writeResult := make(chan error, 1)
	srv := httptest.NewUnstartedServer(idleStackRouter(idle, writeResult))
	srv.Start()
	defer srv.Close()

	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if _, err := io.WriteString(conn, "GET /big HTTP/1.1\r\nHost: test\r\n\r\n"); err != nil {
		t.Fatalf("request: %v", err)
	}
	// Deliberately never read the response.

	select {
	case err := <-writeResult:
		if err == nil {
			t.Fatal("handler wrote the whole response although the client never read")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("blocked write was not released: idle write deadline never reached the connection")
	}
}

// The write deadline armed for one response must not leak into the next
// request on the same keep-alive connection.
func TestIdleStackKeepAliveSecondRequestStillOK(t *testing.T) {
	const idle = 150 * time.Millisecond
	srv := httptest.NewUnstartedServer(idleStackRouter(idle, make(chan error, 1)))
	srv.Start()
	defer srv.Close()

	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	reader := bufio.NewReader(conn)

	for i := range 2 {
		if _, err := io.WriteString(conn, "GET /small HTTP/1.1\r\nHost: test\r\n\r\n"); err != nil {
			t.Fatalf("request %d: %v", i+1, err)
		}
		resp, err := http.ReadResponse(reader, &http.Request{Method: http.MethodGet})
		if err != nil {
			t.Fatalf("response %d: %v", i+1, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || string(body) != "ok" {
			t.Fatalf("response %d = %d %q, want 200 %q", i+1, resp.StatusCode, body, "ok")
		}
		time.Sleep(2 * idle)
	}
}

// The read side goes through the same wrapper chain: a body that stalls is cut
// by the per-read idle deadline instead of holding the handler forever.
func TestIdleStackCutsStalledRequestBody(t *testing.T) {
	const idle = 200 * time.Millisecond
	srv := httptest.NewUnstartedServer(idleStackRouter(idle, make(chan error, 1)))
	srv.Start()
	defer srv.Close()

	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	fmt.Fprint(conn, "POST /echo HTTP/1.1\r\nHost: test\r\nContent-Length: 10\r\n\r\nx")
	// One byte then silence: the handler's body read must time out.
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	status, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatalf("read status: %v", err)
	}
	if !strings.Contains(status, "408") {
		t.Fatalf("status line %q, want 408: idle read deadline never reached the connection", status)
	}
}
