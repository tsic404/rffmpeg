package main

import (
	"net"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/tsic404/rffmpeg/pkg/server/db"
	"github.com/tsic404/rffmpeg/pkg/server/handlers"
	"github.com/tsic404/rffmpeg/pkg/server/storage"
	"github.com/tsic404/rffmpeg/pkg/server/workerhealth"
)

// healthRouter builds the real /health handler over a throwaway database and
// storage, matching the route main mounts for readiness probes.
func healthRouter(t *testing.T) http.Handler {
	t.Helper()

	dataDir := t.TempDir()
	database, err := db.New(filepath.Join(dataDir, "rffmpeg.db"))
	if err != nil {
		t.Fatalf("db.New: %v", err)
	}
	t.Cleanup(func() { database.Close() })

	store, err := storage.New(dataDir)
	if err != nil {
		t.Fatalf("storage.New: %v", err)
	}

	h := handlers.New(database, store, "test", workerhealth.NewWorkerStateTable(30*time.Second))

	r := chi.NewRouter()
	r.Get("/health", h.Health)
	return r
}

// TestListenerAcceptsBeforeInitializationCompletes locks the readiness
// contract the early-bind startup fix introduces: the port is in LISTEN state
// before the handler stack is initialized, so a probe fired at process start
// connects instead of hitting "connection refused"; and once initialization
// finished and the server is serving, GET /health answers 200.
func TestListenerAcceptsBeforeInitializationCompletes(t *testing.T) {
	ln, err := bindListener("127.0.0.1:0")
	if err != nil {
		t.Fatalf("bindListener: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	addr := ln.Addr().String()

	// Initialization (DB, storage, monitor, scheduler, router) runs after this
	// point in main, before serveListener. Nothing is initialized yet, so this
	// dial proves the socket accepts while startup is still in flight.
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("port must be LISTEN before initialization completes: %v", err)
	}
	conn.Close()

	// Initialization complete: mount the real /health handler and serve on the
	// pre-bound listener.
	srv := &http.Server{Addr: addr, Handler: healthRouter(t)}
	serveListener(srv, ln)
	t.Cleanup(func() { srv.Close() })

	client := &http.Client{Timeout: 2 * time.Second}
	var lastErr error
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		resp, err := client.Get("http://" + addr + "/health")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("GET /health = %d, want 200", resp.StatusCode)
			}
			return
		}
		lastErr = err
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("GET /health never became ready after initialization: %v", lastErr)
}
