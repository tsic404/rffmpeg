package websocket

import (
	"encoding/json"
	"log"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/tsix404/rffmpeg/pkg/protocol"
)

// Client represents a WebSocket client connection
type Client struct {
	conn   *websocket.Conn
	jobID  string
	send   chan []byte
	hub    *Hub
	mu     sync.Mutex
	closed bool
}

// Hub maintains the set of active WebSocket clients and broadcasts messages
type Hub struct {
	// clients maps jobID to a slice of clients listening for that job
	clients    map[string]map[*Client]bool
	broadcast  chan *BroadcastMessage
	register   chan *Client
	unregister chan *Client
	mu         sync.RWMutex
	// seq tracks the next per-job message sequence number for gap detection
	seq map[string]int64
}

// BroadcastMessage represents a message to be broadcast
type BroadcastMessage struct {
	JobID   string
	Message []byte
}

// NewHub creates a new Hub
func NewHub() *Hub {
	return &Hub{
		clients:    make(map[string]map[*Client]bool),
		broadcast:  make(chan *BroadcastMessage, 256),
		register:   make(chan *Client, 100), // Buffered to prevent blocking
		unregister: make(chan *Client, 100), // Buffered to prevent blocking
		seq:        make(map[string]int64),
	}
}

// Run starts the hub's main loop
func (h *Hub) Run() {
	for {
		select {
		case client := <-h.register:
			h.mu.Lock()
			if _, ok := h.clients[client.jobID]; !ok {
				h.clients[client.jobID] = make(map[*Client]bool)
			}
			h.clients[client.jobID][client] = true
			h.mu.Unlock()
			log.Printf("WebSocket client registered for job %s", client.jobID)

		case client := <-h.unregister:
			h.mu.Lock()
			if clients, ok := h.clients[client.jobID]; ok {
				if _, ok := clients[client]; ok {
					delete(clients, client)
					client.Close()
					if len(clients) == 0 {
						delete(h.clients, client.jobID)
						// The seq counter deliberately outlives its last
						// client: a reconnecting client carries lastSeq from
						// the old stream, and resetting to zero here would
						// make new broadcasts renumber from 1 — every gap
						// check would then silently pass while data is lost.
						// Cleanup happens on terminal broadcast instead.
					}
				}
			}
			h.mu.Unlock()

		case message := <-h.broadcast:
			h.mu.RLock()
			clients, ok := h.clients[message.JobID]
			h.mu.RUnlock()

			if ok {
				for client := range clients {
					select {
					case client.send <- message.Message:
					default:
						// Client buffer full, close the connection
						h.unregister <- client
					}
				}
			}
		}
	}
}

// Register registers a client with the hub
func (h *Hub) Register(client *Client) {
	h.register <- client
}

// Unregister unregisters a client from the hub
func (h *Hub) Unregister(client *Client) {
	h.unregister <- client
}

// Broadcast sends a message to all clients listening for a job
func (h *Hub) Broadcast(jobID string, message []byte) {
	h.broadcast <- &BroadcastMessage{
		JobID:   jobID,
		Message: message,
	}
}

// BroadcastWSMessage stamps a per-job monotonically increasing sequence
// number on data-bearing messages, then broadcasts to all clients for a job.
// The counter lives for the whole job: it is only removed once the terminal
// status broadcast (completed/failed/cancelled/timeout) has gone out, so a
// reconnecting client's lastSeq stays meaningful across disconnect windows.
func (h *Hub) BroadcastWSMessage(msg protocol.WSMessage) error {
	if msg.Type != protocol.WSMsgHeartbeat {
		h.mu.Lock()
		h.seq[msg.JobID]++
		msg.Seq = h.seq[msg.JobID]
		if msg.Type == protocol.WSMsgStatus {
			if payload, ok := msg.Data.(protocol.WSStatusPayload); ok && protocol.IsTerminalStatus(payload.Status) {
				delete(h.seq, msg.JobID)
			}
		}
		if msg.Type == protocol.WSMsgComplete || msg.Type == protocol.WSMsgError {
			// Complete/error are themselves terminal events: the job will
			// produce no further sequenced data worth tracking.
			delete(h.seq, msg.JobID)
		}
		h.mu.Unlock()
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	h.Broadcast(msg.JobID, data)
	return nil
}

// BroadcastStderr broadcasts a stderr chunk to all clients for a job
func (h *Hub) BroadcastStderr(jobID, chunk string) error {
	msg := protocol.NewStderrMessage(jobID, chunk)
	return h.BroadcastWSMessage(msg)
}

// BroadcastStdout broadcasts a base64-encoded stdout chunk to all clients for a
// job (streaming output mode). The worker already encodes raw bytes as standard
// base64, and the chunk is passed through unchanged: it is ASCII-safe inside the
// JSON text frame (encoding/json corrupts invalid UTF-8 to U+FFFD), so decoding
// and re-encoding here would be a wasted identity transform.
func (h *Hub) BroadcastStdout(jobID string, chunkB64 string) error {
	msg := protocol.NewStdoutMessage(jobID, chunkB64)
	return h.BroadcastWSMessage(msg)
}

// BroadcastStatus broadcasts a status update to all clients for a job
func (h *Hub) BroadcastStatus(jobID string, status protocol.JobStatus, exitCode int, err string) error {
	msg := protocol.NewStatusMessage(jobID, status, exitCode, err)
	return h.BroadcastWSMessage(msg)
}

// BroadcastProgress broadcasts a progress update to all clients for a job
func (h *Hub) BroadcastProgress(jobID string, percent float64, timeUs, durationUs int64, speed float64, etaSeconds int) error {
	msg := protocol.NewProgressMessage(jobID, percent, timeUs, durationUs, speed, etaSeconds)
	return h.BroadcastWSMessage(msg)
}

// BroadcastHeartbeatPayload broadcasts a heartbeat with detailed worker metrics
func (h *Hub) BroadcastHeartbeatPayload(payload protocol.WorkerHeartbeatPayload) error {
	msg := protocol.WSMessage{
		Type:      protocol.WSMsgHeartbeat,
		Timestamp: time.Now(),
		JobID:     payload.WorkerID,
		Data:      payload,
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	h.Broadcast(payload.WorkerID, data)
	return nil
}

// BroadcastComplete broadcasts a completion message for a job
func (h *Hub) BroadcastComplete(jobID string, exitCode int) error {
	msg := protocol.NewCompleteMessage(jobID, exitCode)
	return h.BroadcastWSMessage(msg)
}

// BroadcastError broadcasts an error message for a job
func (h *Hub) BroadcastError(jobID, errMsg string) error {
	msg := protocol.NewErrorMessage(jobID, errMsg)
	return h.BroadcastWSMessage(msg)
}

// NewClient creates a new WebSocket client
func NewClient(conn *websocket.Conn, jobID string, hub *Hub) *Client {
	return &Client{
		conn:  conn,
		jobID: jobID,
		send:  make(chan []byte, 256),
		hub:   hub,
	}
}

// ReadPump pumps messages from the WebSocket connection to the hub
func (c *Client) ReadPump() {
	defer func() {
		c.hub.Unregister(c)
	}()

	c.conn.SetReadLimit(512)
	c.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	c.conn.SetPongHandler(func(string) error {
		c.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		return nil
	})

	for {
		_, _, err := c.conn.ReadMessage()
		if err != nil {
			break
		}
	}
}

// WritePump pumps messages from the hub to the WebSocket connection
func (c *Client) WritePump() {
	ticker := time.NewTicker(30 * time.Second)
	defer func() {
		ticker.Stop()
		c.Close()
	}()

	for {
		select {
		case message, ok := <-c.send:
			c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if !ok {
				c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}

			w, err := c.conn.NextWriter(websocket.TextMessage)
			if err != nil {
				return
			}
			w.Write(message)

			// Batch queued messages
			n := len(c.send)
			for i := 0; i < n; i++ {
				w.Write([]byte{'\n'})
				w.Write(<-c.send)
			}

			if err := w.Close(); err != nil {
				return
			}

		case <-ticker.C:
			c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

// Close closes the client connection
func (c *Client) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.closed = true
	c.conn.Close()
	close(c.send)
}

// ClientCount returns the number of clients listening for a specific job
func (h *Hub) ClientCount(jobID string) int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if clients, ok := h.clients[jobID]; ok {
		return len(clients)
	}
	return 0
}

// TotalClients returns the total number of connected clients
func (h *Hub) TotalClients() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	count := 0
	for _, clients := range h.clients {
		count += len(clients)
	}
	return count
}
