package websocket

import (
	"log"
	"net/http"

	"github.com/go-chi/chi/v5"
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
