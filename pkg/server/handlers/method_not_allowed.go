package handlers

import (
	"bufio"
	"errors"
	"net"
	"net/http"

	"github.com/tsix404/rffmpeg/pkg/protocol"
)

// NormalizeMethodNotAllowed converts chi's default empty-body 405 response into
// the protocol-consistent JSON body while preserving the Allow header that the
// router's default methodNotAllowedHandler set before writing the status.
func NormalizeMethodNotAllowed(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rw := &methodNotAllowedResponseWriter{ResponseWriter: w}
		next.ServeHTTP(rw, r)
		if rw.intercepted {
			writeJSON(w, http.StatusMethodNotAllowed, protocol.ErrorResponse{
				Code:    "method_not_allowed",
				Message: "Method not allowed",
			})
		}
	})
}

// methodNotAllowedResponseWriter intercepts WriteHeader(405) so the middleware
// can replace chi's empty 405 body after the Allow header is already set. Every
// other status delegates straight through, preserving streaming and upgrades.
type methodNotAllowedResponseWriter struct {
	http.ResponseWriter
	intercepted bool
}

func (w *methodNotAllowedResponseWriter) WriteHeader(code int) {
	if code == http.StatusMethodNotAllowed {
		w.intercepted = true
		return
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *methodNotAllowedResponseWriter) Write(b []byte) (int, error) {
	if w.intercepted {
		// chi's default 405 handler writes a nil body; drop it and let the
		// middleware emit the normalized JSON.
		return len(b), nil
	}
	return w.ResponseWriter.Write(b)
}

func (w *methodNotAllowedResponseWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *methodNotAllowedResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := w.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, errors.New("hijacking not supported")
}
