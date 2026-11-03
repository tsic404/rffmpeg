package transport

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// deadlineStub claims both deadline setters so the guards' arming can be
// observed without a real connection.
type deadlineStub struct {
	http.ResponseWriter
	mu     sync.Mutex
	writes []time.Time
}

func (s *deadlineStub) SetWriteDeadline(t time.Time) error { s.record(t); return nil }
func (s *deadlineStub) SetReadDeadline(t time.Time) error  { s.record(t); return nil }

func (s *deadlineStub) record(t time.Time) {
	s.mu.Lock()
	s.writes = append(s.writes, t)
	s.mu.Unlock()
}

func (s *deadlineStub) armed() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.writes)
}

func TestIdleBodyMiddlewareArmsDeadlineBeforeEveryRead(t *testing.T) {
	body := &countingBody{remaining: 3}
	var stub *deadlineStub
	handler := IdleBodyMiddleware(50 * time.Millisecond)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stub = w.(*deadlineStub)
		if got := stub.armed(); got != 0 {
			t.Errorf("deadline armed before the first read: %d", got)
		}
		io.Copy(io.Discard, r.Body)
	}))

	req := httptest.NewRequest("POST", "/", body)
	handler.ServeHTTP(&deadlineStub{ResponseWriter: httptest.NewRecorder()}, req)

	if body.reads != 4 {
		t.Fatalf("reads = %d, want 4 (3 bytes + EOF)", body.reads)
	}
	if got := stub.armed(); got != body.reads {
		t.Errorf("read deadlines = %d, want one per read (%d)", got, body.reads)
	}
}

// countingBody yields one byte per Read(r) call and counts them.
type countingBody struct {
	remaining int
	reads     int
}

func (b *countingBody) Read(p []byte) (int, error) {
	b.reads++
	if len(p) == 0 {
		return 0, nil
	}
	if b.remaining == 0 {
		return 0, io.EOF
	}
	b.remaining--
	p[0] = 'x'
	return 1, nil
}

func (b *countingBody) Close() error { return nil }

func TestIdleWriteMiddlewareArmsDeadlineBeforeEveryWrite(t *testing.T) {
	stub := &deadlineStub{ResponseWriter: httptest.NewRecorder()}
	handler := IdleWriteMiddleware(50 * time.Millisecond)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for range 3 {
			if _, err := w.Write([]byte("chunk")); err != nil {
				t.Fatalf("write failed: %v", err)
			}
		}
	}))

	handler.ServeHTTP(stub, httptest.NewRequest("GET", "/", nil))

	if got := stub.armed(); got != 3 {
		t.Errorf("write deadlines = %d, want 3 (one per write)", got)
	}
}

// A body that stops moving bytes is cut after the idle window instead of
// holding the connection open, while a slow but progressing body is allowed to
// finish. This is the transfer-side replacement for the removed whole-request
// read deadline.
func TestIdleBodyMiddlewareCutsStalledBodyButNotSlowOne(t *testing.T) {
	const idle = 300 * time.Millisecond
	handler := IdleBodyMiddleware(idle)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			w.WriteHeader(http.StatusRequestTimeout)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	srv := httptest.NewUnstartedServer(handler)
	srv.Start()
	defer srv.Close()

	send := func(total int, feed func(conn net.Conn)) string {
		t.Helper()
		conn, err := net.Dial("tcp", srv.Listener.Addr().String())
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer conn.Close()
		fmt.Fprintf(conn, "POST / HTTP/1.1\r\nHost: test\r\nContent-Length: %d\r\n\r\n", total)
		feed(conn)
		conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		status, err := bufio.NewReader(conn).ReadString('\n')
		if err != nil {
			t.Fatalf("read status: %v", err)
		}
		return status
	}

	// 10 bytes trickled every 50ms: the transfer outlives the idle window but
	// never stalls for it.
	trickledStatus := strings.TrimSpace(send(10, func(conn net.Conn) {
		for range 10 {
			if _, err := conn.Write([]byte("x")); err != nil {
				t.Errorf("write body: %v", err)
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	}))
	if !strings.Contains(trickledStatus, "200") {
		t.Errorf("slow progressing body: status line %q, want 200", trickledStatus)
	}

	// One byte then silence: the read deadline fires and the handler reports a
	// failed body read.
	stalledStatus := strings.TrimSpace(send(10, func(conn net.Conn) {
		conn.Write([]byte("x"))
		time.Sleep(2 * idle)
	}))
	if !strings.Contains(stalledStatus, "408") {
		t.Errorf("stalled body: status line %q, want 408", stalledStatus)
	}
}

// The request body wrapper must remain closeable so handlers and
// MaxBytesReader can release it.
func TestIdleBodyMiddlewareKeepsBodyCloseable(t *testing.T) {
	body := &countingBody{remaining: 1}
	handler := IdleBodyMiddleware(time.Second)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.Body.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	}))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/", body))
}

// hijackStub records the optional-interface calls the middleware must keep
// forwarding. The WebSocket log endpoint upgrades through http.Hijacker; if
// the write wrapper hid it, every log stream would break.
type hijackStub struct {
	http.ResponseWriter
	flushed  bool
	hijacked bool
}

func (s *hijackStub) Flush() { s.flushed = true }

func (s *hijackStub) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	s.hijacked = true
	return nil, nil, nil
}

func TestIdleWriteMiddlewarePreservesOptionalInterfaces(t *testing.T) {
	stub := &hijackStub{ResponseWriter: httptest.NewRecorder()}
	handler := IdleWriteMiddleware(50 * time.Millisecond)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			t.Error("wrapped writer does not implement http.Hijacker")
			return
		}
		if _, _, err := hijacker.Hijack(); err != nil {
			t.Errorf("hijack: %v", err)
		}
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("wrapped writer does not implement http.Flusher")
			return
		}
		flusher.Flush()
		if unwrapped, ok := w.(interface{ Unwrap() http.ResponseWriter }); !ok {
			t.Error("wrapped writer does not expose Unwrap")
		} else if unwrapped.Unwrap() != stub {
			t.Error("Unwrap does not return the base writer")
		}
	}))

	handler.ServeHTTP(stub, httptest.NewRequest("GET", "/", nil))

	if !stub.hijacked {
		t.Error("Hijack was not forwarded to the base writer")
	}
	if !stub.flushed {
		t.Error("Flush was not forwarded to the base writer")
	}
}

// The per-write deadline must not survive into the next request on a
// keep-alive connection: net/http clears the write deadline after every
// request, so a stale (already expired) deadline would otherwise close the
// connection on the next response.
func TestIdleWriteMiddlewareDoesNotLeakIntoNextKeepAliveRequest(t *testing.T) {
	const idle = 150 * time.Millisecond
	handler := IdleWriteMiddleware(idle)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	}))
	srv := httptest.NewUnstartedServer(handler)
	srv.Start()
	defer srv.Close()

	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	reader := bufio.NewReader(conn)

	for i := range 2 {
		if _, err := io.WriteString(conn, "GET / HTTP/1.1\r\nHost: test\r\n\r\n"); err != nil {
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
		// Wait past the idle window armed by the first response.
		time.Sleep(2 * idle)
	}
}

// A client that requests a large response and then stops reading must not pin
// the handler and its connection forever: the write blocks once the socket
// buffers fill, and the per-write deadline ends it.
func TestIdleWriteMiddlewareCutsClientThatStopsReading(t *testing.T) {
	const idle = 200 * time.Millisecond
	writeErr := make(chan error, 1)
	handler := IdleWriteMiddleware(idle)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chunk := make([]byte, 64<<10)
		for range 2048 { // up to 128 MiB, far beyond any socket buffer
			if _, err := w.Write(chunk); err != nil {
				writeErr <- err
				return
			}
		}
		writeErr <- nil
	}))
	srv := httptest.NewUnstartedServer(handler)
	srv.Start()
	defer srv.Close()

	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if _, err := io.WriteString(conn, "GET / HTTP/1.1\r\nHost: test\r\n\r\n"); err != nil {
		t.Fatalf("request: %v", err)
	}
	// Deliberately never read the response.

	select {
	case err := <-writeErr:
		if err == nil {
			t.Fatal("handler wrote the whole response although the client never read")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("blocked write was not released by the idle deadline")
	}
}

// The write wrapper must stay transparent to a real WebSocket upgrade: the
// upgrade hijacks the connection, so hiding http.Hijacker behind the deadline
// wrapper would break the job-log stream.
func TestIdleWriteMiddlewareAllowsWebSocketUpgrade(t *testing.T) {
	upgrader := websocket.Upgrader{}
	handler := IdleWriteMiddleware(50 * time.Millisecond)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		conn.Close()
	}))
	srv := httptest.NewServer(handler)
	defer srv.Close()

	url := "ws" + strings.TrimPrefix(srv.URL, "http")
	conn, resp, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		t.Fatalf("websocket dial failed (status %d): %v", status, err)
	}
	conn.Close()
}
