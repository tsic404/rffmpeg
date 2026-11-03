package main

import (
	"net/http"
	"testing"
	"time"
)

// The contract allows a 10 GiB single upload and single-shot downloads of
// arbitrarily large outputs, so the listener must not cap a whole request by
// time: a 5-minute ReadTimeout/WriteTimeout truncated every transfer slower
// than ~34 MB/s. Headers still need a bound, or a slowloris client holds the
// connection forever.
func TestNewHTTPServerHasNoWholeRequestDeadline(t *testing.T) {
	srv := newHTTPServer(":0", http.NotFoundHandler())

	if srv.ReadTimeout != 0 {
		t.Errorf("ReadTimeout = %s, want 0 (whole-request cap truncates large uploads)", srv.ReadTimeout)
	}
	if srv.WriteTimeout != 0 {
		t.Errorf("WriteTimeout = %s, want 0 (whole-request cap truncates large downloads)", srv.WriteTimeout)
	}
	if srv.ReadHeaderTimeout <= 0 || srv.ReadHeaderTimeout > 30*time.Second {
		t.Errorf("ReadHeaderTimeout = %s, want a bound in (0, 30s]", srv.ReadHeaderTimeout)
	}
	if srv.IdleTimeout <= 0 {
		t.Errorf("IdleTimeout = %s, want > 0", srv.IdleTimeout)
	}
}
