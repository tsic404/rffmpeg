// Package transport bounds how long an HTTP transfer may stall without moving
// bytes.
//
// The server sets no whole-request read or write deadline: the contract allows
// a 10 GiB single upload and single-shot downloads of arbitrarily large
// outputs, so a whole-request cap would truncate every transfer slower than
// that size divided by the cap. The stall bound is enforced per read and per
// write instead: a connection that makes progress is never cut, a connection
// that stops moving bytes is dropped after IdleTimeout.
package transport

import (
	"bufio"
	"io"
	"log"
	"net"
	"net/http"
	"sync"
	"time"
)

// IdleTimeout is how long a transfer may go without moving a byte before the
// connection is cut. It replaces the former 5-minute whole-request deadline,
// which killed any transfer slower than ~34 MB/s regardless of progress.
const IdleTimeout = 5 * time.Minute

// IdleBodyMiddleware wraps every request body so each read re-arms the
// connection's read deadline. A client that sends headers and then stalls no
// longer holds a connection forever; one that streams slowly keeps receiving
// a fresh window per read.
//
// The read deadline is armed through http.ResponseController, which reports
// ErrNotSupported for transports without per-read deadlines (e.g. the
// ResponseRecorder used in tests); the read still proceeds in that case.
func IdleBodyMiddleware(idle time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Body != nil && r.Body != http.NoBody {
				r.Body = &idleBody{
					body: r.Body,
					rc:   http.NewResponseController(w),
					idle: idle,
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

type idleBody struct {
	body     io.ReadCloser
	rc       *http.ResponseController
	idle     time.Duration
	warnOnce sync.Once
}

func (b *idleBody) Read(p []byte) (int, error) {
	b.armDeadline()
	return b.body.Read(p)
}

// armDeadline installs the idle read deadline and, if that is impossible,
// says so once per request. http.ResponseController returns ErrNotSupported
// when a ResponseWriter wrapper in the chain lacks Unwrap, which would
// otherwise leave the request unbounded with no trace.
func (b *idleBody) armDeadline() {
	if err := b.rc.SetReadDeadline(time.Now().Add(b.idle)); err != nil {
		b.warnOnce.Do(func() {
			log.Printf("transport: idle read deadline unavailable (%v); a stalled upload is unbounded for this request", err)
		})
	}
}

func (b *idleBody) Close() error {
	return b.body.Close()
}

// IdleWriteMiddleware wraps the ResponseWriter so every write re-arms the
// connection's write deadline. Without it a client that stops reading pins the
// handler goroutine and its connection: with no whole-request WriteTimeout,
// any response larger than the free TCP window — a large JSON list or a file
// download — blocks in Write until the peer reads.
//
// The deadline does not leak into the next keep-alive request on HTTP/1:
// net/http clears the connection's write deadline after every request
// (finishRequest), and HTTP/2 deadlines are per stream.
func IdleWriteMiddleware(idle time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(&idleResponseWriter{
				ResponseWriter: w,
				rc:             http.NewResponseController(w),
				idle:           idle,
			}, r)
		})
	}
}

// idleResponseWriter re-arms the write deadline before each write. It keeps
// the optional interfaces handlers rely on: the WebSocket log endpoint
// upgrades through http.Hijacker, streaming handlers use http.Flusher, and
// Unwrap lets http.ResponseController reach the real writer.
type idleResponseWriter struct {
	http.ResponseWriter
	rc       *http.ResponseController
	idle     time.Duration
	warnOnce sync.Once
}

func (w *idleResponseWriter) Write(p []byte) (int, error) {
	w.armDeadline()
	return w.ResponseWriter.Write(p)
}

// armDeadline installs the idle write deadline and, if that is impossible,
// says so once per request. http.ResponseController returns ErrNotSupported
// when a ResponseWriter wrapper in the chain lacks Unwrap, which would
// otherwise leave the response unbounded with no trace.
func (w *idleResponseWriter) armDeadline() {
	if err := w.rc.SetWriteDeadline(time.Now().Add(w.idle)); err != nil {
		w.warnOnce.Do(func() {
			log.Printf("transport: idle write deadline unavailable (%v); a stalled response is unbounded for this request", err)
		})
	}
}

func (w *idleResponseWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *idleResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	return h.Hijack()
}

func (w *idleResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}
