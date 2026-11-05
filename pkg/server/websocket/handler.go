package websocket

import (
	"encoding/json"
	"log"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/gorilla/websocket"
	"github.com/tsic404/rffmpeg/pkg/protocol"
	"github.com/tsic404/rffmpeg/pkg/server/panicguard"
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
	go panicguard.Guard("websocket write pump", client.WritePump)
	go panicguard.Guard("websocket read pump", client.ReadPump)
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
	go panicguard.Guard("websocket write pump", client.WritePump)
	go panicguard.Guard("websocket read pump", client.ReadPump)
}

// writeUpgradeRequired writes the RFC 6455 §4.1 426 response for a request that
// never asked for a WebSocket upgrade: it advertises the required protocol and
// carries the C-52 error body.
func writeUpgradeRequired(w http.ResponseWriter) {
	h := w.Header()
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

	// A request that never asked for an upgrade (bare GET, or Connection:
	// Upgrade without Upgrade: websocket) is not a handshake gorilla can
	// classify, and RFC 6455 §4.1 answers it with 426 rather than a 400.
	if !websocket.IsWebSocketUpgrade(r) {
		writeUpgradeRequired(w)
		return
	}

	// Upgrade HTTP connection to WebSocket. gorilla already wrote the 400 for
	// a malformed handshake (bad Sec-WebSocket-Key, missing
	// Sec-WebSocket-Version, ...); that 400 is the endpoint's declared answer.
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}

	// Create and register client
	client := NewClient(conn, jobID, hub)
	hub.Register(client)

	// Start read and write pumps in goroutines
	go panicguard.Guard("websocket write pump", client.WritePump)
	go panicguard.Guard("websocket read pump", client.ReadPump)
}
