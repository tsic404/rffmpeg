package websocket

import (
	"bufio"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/gorilla/websocket"
	"github.com/tsic404/rffmpeg/pkg/protocol"
)

// JobStore defines the interface for job validation
type JobStore interface {
	JobExists(jobID string) (bool, error)
}

// Handler holds WebSocket handler dependencies
type Handler struct {
	hub *Hub
	db  JobStore
}

// NewHandler creates a new WebSocket handler
func NewHandler(hub *Hub) *Handler {
	return &Handler{
		hub: hub,
	}
}

// NewHandlerWithDB creates a new WebSocket handler with job validation
func NewHandlerWithDB(hub *Hub, db JobStore) *Handler {
	return &Handler{
		hub: hub,
		db:  db,
	}
}

// GetHub returns the WebSocket hub
func (h *Handler) GetHub() *Hub {
	return h.hub
}

// HandleJobLog handles WebSocket connections for job log streaming
// Endpoint: GET /api/v1/jobs/{jobId}/log
func (h *Handler) HandleJobLog(w http.ResponseWriter, r *http.Request) {
	jobID := chi.URLParam(r, "jobId")
	if jobID == "" {
		http.Error(w, "job_id is required", http.StatusBadRequest)
		return
	}

	// Validate job exists if database is configured
	if h.db != nil {
		exists, err := h.db.JobExists(jobID)
		if err != nil {
			log.Printf("Failed to validate job %s: %v", jobID, err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}
		if !exists {
			http.Error(w, "Job not found", http.StatusNotFound)
			return
		}
	}

	// Upgrade HTTP connection to WebSocket
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}

	// Create and register client
	client := NewClient(conn, jobID, h.hub)
	h.hub.Register(client)

	// Start read and write pumps in goroutines
	go client.WritePump()
	go client.ReadPump()
}

// HandleJobLogWithHub handles WebSocket connections using a provided hub
// This is a convenience function for use with existing handlers
func HandleJobLogWithHub(hub *Hub, w http.ResponseWriter, r *http.Request) {
	jobID := chi.URLParam(r, "jobId")
	if jobID == "" {
		http.Error(w, "job_id is required", http.StatusBadRequest)
		return
	}

	// Upgrade HTTP connection to WebSocket
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}

	// Create and register client
	client := NewClient(conn, jobID, hub)
	hub.Register(client)

	// Start read and write pumps in goroutines
	go client.WritePump()
	go client.ReadPump()
}

// handshakeResponseWriter intercepts gorilla/websocket's built-in 400 Bad
// Request write for a failed upgrade so the handler can decide whether to
// replace it with the RFC 6455 §4.1 426 Upgrade Required response (missing
// Upgrade header) or pass the original 400 through unchanged (malformed
// Sec-WebSocket-Key, missing Sec-WebSocket-Version, ...). Every other status
// delegates straight through, preserving 403/405 and the successful-upgrade
// hijack path.
type handshakeResponseWriter struct {
	http.ResponseWriter
	intercepted bool
	status      int
	body        []byte
}

func (w *handshakeResponseWriter) WriteHeader(code int) {
	if code == http.StatusBadRequest {
		w.intercepted = true
		w.status = code
		return
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *handshakeResponseWriter) Write(b []byte) (int, error) {
	if w.intercepted {
		w.body = append(w.body, b...)
		return len(b), nil
	}
	return w.ResponseWriter.Write(b)
}

func (w *handshakeResponseWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *handshakeResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := w.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, errors.New("hijacking not supported")
}

// replay forwards gorilla's original 400 response that WriteHeader/Write
// buffered, preserving the pre-426 behavior for handshake failures that RFC
// 6455 does not map to 426.
func (w *handshakeResponseWriter) replay() {
	w.ResponseWriter.WriteHeader(w.status)
	if len(w.body) > 0 {
		_, _ = w.ResponseWriter.Write(w.body)
	}
}

// missingUpgradeHeaderErr is the gorilla/websocket v1.5.3 HandshakeError text
// for a request that lacks the "Upgrade: websocket" header — the one handshake
// failure RFC 6455 §4.1 maps to 426 Upgrade Required. Other 400s (malformed
// Sec-WebSocket-Key, missing Sec-WebSocket-Version, ...) stay 400.
const missingUpgradeHeaderErr = "websocket: the client is not using the websocket protocol: 'websocket' token not found in 'Upgrade' header"

func isMissingUpgradeHeader(err websocket.HandshakeError) bool {
	return err.Error() == missingUpgradeHeaderErr
}

// writeUpgradeRequired writes the RFC 6455 §4.1 426 response for a request that
// did not perform a WebSocket handshake. The body mirrors the 405/404 JSON
// error shape and advertises the required upgrade protocol; gorilla's residual
// Sec-Websocket-Version / X-Content-Type-Options headers from its 400 path are
// dropped so the 426 response carries only its own headers.
func writeUpgradeRequired(w http.ResponseWriter) {
	h := w.Header()
	h.Del("Sec-Websocket-Version")
	h.Del("X-Content-Type-Options")
	h.Set("Upgrade", "websocket")
	h.Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUpgradeRequired)
	_ = json.NewEncoder(w).Encode(protocol.ErrorResponse{
		Code:    protocol.ErrCodeUpgradeRequired,
		Message: "Upgrade required",
	})
}

// HandleJobLogWithValidation handles WebSocket connections with job validation
func HandleJobLogWithValidation(hub *Hub, db JobStore, w http.ResponseWriter, r *http.Request) {
	jobID := chi.URLParam(r, "jobId")
	if jobID == "" {
		http.Error(w, "job_id is required", http.StatusBadRequest)
		return
	}

	// Validate job exists
	if db != nil {
		exists, err := db.JobExists(jobID)
		if err != nil {
			log.Printf("Failed to validate job %s: %v", jobID, err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}
		if !exists {
			http.Error(w, "Job not found", http.StatusNotFound)
			return
		}
	}

	// Upgrade HTTP connection to WebSocket. Wrap the writer so gorilla's
	// built-in 400 for a non-upgrade handshake can be rewritten to 426 — but
	// only for the missing "Upgrade: websocket" header case RFC 6455 §4.1
	// requires; other 400s (malformed key, missing version, ...) are replayed.
	rw := &handshakeResponseWriter{ResponseWriter: w}
	conn, err := upgrader.Upgrade(rw, r, nil)
	if err != nil {
		var handshake websocket.HandshakeError
		isHandshake := errors.As(err, &handshake)
		if rw.intercepted {
			if isHandshake && isMissingUpgradeHeader(handshake) {
				writeUpgradeRequired(w)
			} else {
				// Replay gorilla's original 400 (malformed key, missing
				// version, ...). gorilla v1.5.3 always returns a
				// HandshakeError alongside a 400; if a future version returns
				// another type, log it and still replay rather than leave an
				// empty 200.
				if !isHandshake {
					log.Printf("websocket upgrade failed with non-HandshakeError after writing 400: %v", err)
				}
				rw.replay()
			}
		}
		return
	}

	// Create and register client
	client := NewClient(conn, jobID, hub)
	hub.Register(client)

	// Start read and write pumps in goroutines
	go client.WritePump()
	go client.ReadPump()
}
