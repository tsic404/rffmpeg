package websocket

import (
	"encoding/json"
	"log"
	"sync"
	"sync/atomic"
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
	// seqStore, when non-nil, persists seq counters across server restarts
	// (TSI-2379): without it a restart resets numbering to 1 and reconnecting
	// streaming clients misread the new stream as lost data.
	seqStore SeqStore
	// seqPersistFailures counts failed store writes/deletes for observability.
	seqPersistFailures atomic.Int64
}

// BroadcastMessage represents a message to be broadcast
type BroadcastMessage struct {
	JobID   string
	Message []byte
}

// SeqStore persists per-job WebSocket sequence counters. Implemented by
// *db.Database; declared here to keep this package free of a db import.
type SeqStore interface {
	LoadWSJobSeq(jobID string) (int64, error)
	SaveWSJobSeq(jobID string, lastSeq int64) error
	DeleteWSJobSeq(jobID string) error
}

// NewHub creates a new Hub without sequence persistence (tests, embedders).
func NewHub() *Hub {
	return NewHubWithSeqStore(nil)
}

// NewHubWithSeqStore creates a Hub whose per-job sequence counters survive a
// restart via store. A nil store keeps the in-memory-only behavior.
func NewHubWithSeqStore(store SeqStore) *Hub {
	return &Hub{
		clients:    make(map[string]map[*Client]bool),
		broadcast:  make(chan *BroadcastMessage, 256),
		register:   make(chan *Client, 100), // Buffered to prevent blocking
		unregister: make(chan *Client, 100), // Buffered to prevent blocking
		seq:        make(map[string]int64),
		seqStore:   store,
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
// event broadcast (complete/error) has gone out — a terminal *status*
// broadcast carries it forward so the trailing complete message continues the
// numbering instead of restarting at 1 (TSI-2382) — keeping a reconnecting
// client's lastSeq meaningful across disconnect windows.
//
// With a SeqStore configured, the counter is persisted on every increment
// and restored on the first broadcast after a restart — a server restart no
// longer resets numbering to 1, which streaming clients would read as lost
// data and fail the job with a spurious gap error. Persistence happens
// outside h.mu: the store has its own locking and must not be called under
// the hub lock that Run-loop paths also take.
func (h *Hub) BroadcastWSMessage(msg protocol.WSMessage) error {
	if msg.Type != protocol.WSMsgHeartbeat {
		// Restore a persisted counter before locking, when the first
		// post-restart broadcast for this job arrives: LoadWSJobSeq does
		// SQLite I/O and must never run under h.mu, which the Run loop's
		// register/unregister/broadcast paths share. The locked re-check in
		// ensureSeq resolves races with concurrent broadcasts for the same
		// job (each may speculatively load here; only one result wins).
		h.prefetchSeq(msg.JobID)

		h.mu.Lock()
		h.ensureSeq(msg.JobID)
		// A terminal-status broadcast must NOT delete the counter here:
		// BroadcastComplete immediately follows BroadcastStatus(completed/
		// failed/...) in the handlers, and deleting at the status step made
		// the complete broadcast renumber from 1 — connected clients read
		// that reset as proven data loss and failed intact jobs with a
		// spurious gap (TSI-2382). The status carries lastSeq forward; the
		// complete/error branch below performs the actual deletion.
		terminal := false
		if msg.Type == protocol.WSMsgStatus {
			if payload, ok := msg.Data.(protocol.WSStatusPayload); ok && protocol.IsTerminalStatus(payload.Status) {
				terminal = true
			}
		}
		h.seq[msg.JobID]++
		msg.Seq = h.seq[msg.JobID]
		if msg.Type == protocol.WSMsgComplete || msg.Type == protocol.WSMsgError {
			// Complete/error are themselves terminal events: the job will
			// produce no further sequenced data worth tracking. This is the
			// single deletion point — after it, a genuinely new stream for
			// the same job ID restarts at 1.
			delete(h.seq, msg.JobID)
			terminal = true
		}
		h.mu.Unlock()

		h.persistSeq(msg.JobID, msg.Seq, terminal)
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	h.Broadcast(msg.JobID, data)
	return nil
}

// persistSeq writes the counter through to the store (best-effort) and drops
// it once the job is terminal. Errors are logged, never fatal: an unpersisted
// counter only costs gap detection across a restart, not the live stream.
// Failures are counted in seqPersistFailures (read via SeqPersistFailureCount)
// so monitoring can alert on a degraded store without a metrics framework.
// Called without h.mu held — same rule as prefetchSeq.
func (h *Hub) persistSeq(jobID string, seq int64, terminal bool) {
	if h.seqStore == nil {
		return
	}
	if terminal {
		if err := h.seqStore.DeleteWSJobSeq(jobID); err != nil {
			h.seqPersistFailures.Add(1)
			log.Printf("Failed to delete websocket seq for job %s: %v", jobID, err)
		}
		return
	}
	if err := h.seqStore.SaveWSJobSeq(jobID, seq); err != nil {
		h.seqPersistFailures.Add(1)
		log.Printf("Failed to persist websocket seq for job %s: %v", jobID, err)
	}
}

// SeqPersistFailureCount reports how many sequence-counter persistence
// operations have failed since hub creation. A rising value means the seq
// store is degraded and restart-resume numbering may silently regress to 1.
func (h *Hub) SeqPersistFailureCount() int64 {
	return h.seqPersistFailures.Load()
}

// prefetchSeq speculatively loads a persisted counter into memory before the
// first post-restart increment for a job. Called WITHOUT h.mu held: the load
// does SQLite I/O that must not block the Run loop's register/unregister/
// broadcast paths sharing the hub lock. Concurrent broadcasts for the same
// job may all load; ensureSeq's locked re-check makes exactly one injection
// win, so this is at worst a redundant read.
func (h *Hub) prefetchSeq(jobID string) {
	if h.seqStore == nil {
		return
	}
	h.mu.RLock()
	_, ok := h.seq[jobID]
	h.mu.RUnlock()
	if ok {
		return
	}
	lastSeq, err := h.seqStore.LoadWSJobSeq(jobID)
	if err != nil {
		log.Printf("Failed to load persisted websocket seq for job %s: %v", jobID, err)
		return
	}

	h.mu.Lock()
	h.injectSeqLocked(jobID, lastSeq)
	h.mu.Unlock()
}

// ensureSeq falls back to an in-lock restore when prefetchSeq missed (e.g.
// the job's first broadcast raced past it). Store errors are swallowed here —
// blocking on I/O under h.mu is worse than starting from scratch — so the
// common path is a pure map lookup.
func (h *Hub) ensureSeq(jobID string) {
	if _, ok := h.seq[jobID]; ok || h.seqStore == nil {
		return
	}
	lastSeq, err := h.seqStore.LoadWSJobSeq(jobID)
	if err != nil {
		log.Printf("Failed to load persisted websocket seq under lock for job %s: %v", jobID, err)
		return
	}
	h.injectSeqLocked(jobID, lastSeq)
}

// injectSeqLocked seeds the in-memory map with a persisted counter unless a
// concurrent broadcast already restored or advanced it. Called with h.mu held.
func (h *Hub) injectSeqLocked(jobID string, lastSeq int64) {
	if _, ok := h.seq[jobID]; !ok && lastSeq > 0 {
		h.seq[jobID] = lastSeq
	}
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
