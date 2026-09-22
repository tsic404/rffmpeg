package main

import (
	"log"
	"net"
	"net/http"

	"github.com/tsic404/rffmpeg/pkg/server/panicguard"
)

// bindListener binds the TCP socket for addr and logs the moment it enters
// LISTEN. Startup binds before the expensive initialization (DB, storage,
// monitor, scheduler, router) so a port-only readiness probe (docker
// healthcheck, orchestrator TCP check) observes "listening" within
// milliseconds of process start instead of a multi-second "connection
// refused" window where the process is up but not yet accepting. Requests
// arriving while initialization still runs wait in the kernel accept backlog
// and are served once Serve drains it; GET /health stays the strict readiness
// signal (200 only after the handler stack is up).
func bindListener(addr string) (net.Listener, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	log.Printf("Listening on %s (socket bound; initialization in progress)", addr)
	return ln, nil
}

// serveListener serves srv on an already-bound listener in a background
// goroutine, so binding can precede the initialization that builds the
// handler. A non-nil srv.TLSConfig wraps the pre-bound socket in TLS,
// preserving the HTTP/2 negotiation ListenAndServeTLS would have set up. A
// non-ErrServerClosed failure is fatal, exactly as it was when the serve call
// also owned the listener.
func serveListener(srv *http.Server, ln net.Listener) {
	go panicguard.Guard("http server", func() {
		var err error
		if srv.TLSConfig != nil {
			err = srv.ServeTLS(ln, "", "")
		} else {
			err = srv.Serve(ln)
		}
		if err != nil && err != http.ErrServerClosed {
			log.Fatalf("Server failed: %v", err)
		}
	})
}
