package websocket

import (
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/tsic404/rffmpeg/pkg/protocol"
)

// Client represents a WebSocket client connection
type Client struct {
	conn   *websocket.Conn
	jobID  string
	send   chan []byte
	hub    *Hub
	mu     sync.Mutex
	closed bool
	// pending holds a message taken from send that did not fit the frame
	// being written. Only WritePump touches it.
	pending []byte
}

// hubBroadcastBuffer is the capacity of the hub's broadcast queue.
const hubBroadcastBuffer = 256

// hubSendTimeout bounds how long any channel send into the hub may block.
// The hub is a shared service: one stalled consumer must never wedge the
// callers feeding it.
const hubSendTimeout = 5 * time.Second

// Hub maintains the set of active WebSocket clients and broadcasts messages
type Hub struct {
	// clients maps jobID to a slice of clients listening for that job
	clients    map[string]map[*Client]bool
	broadcast  chan *BroadcastMessage
	register   chan *Client
	unregister chan *Client
	mu         sync.RWMutex
	// bcastMu serializes broadcast-channel sends so that messages for the
	// same job are enqueued in seq order. It is independent of h.mu (which
	// the Run loop takes) so a blocked send cannot deadlock with the loop.
	bcastMu sync.Mutex
	// seq tracks the next per-job message sequence number for gap detection
	seq map[string]int64
	// seqStore, when non-nil, persists seq counters across server restarts
	// without it a restart resets numbering to 1 and reconnecting
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
		broadcast:  make(chan *BroadcastMessage, hubBroadcastBuffer),
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
			// Full write lock, not RLock: the shed path below mutates h.clients.
			// The invariant is "after Close, no broadcast may touch client.send"
			// — a select-send on the closed channel panics and would kill this
			// Run goroutine (silent server death). Removing the client from the
			// map synchronously here — instead of waiting for ReadPump's async
			// Unregister — closes that window entirely.
			h.mu.Lock()
			clients, ok := h.clients[message.JobID]

			if ok {
				for client := range clients {
					select {
					case client.send <- message.Message:
					default:
						// Client buffer full: shed it now. Sending
						// h.unregister <- client from inside the loop deadlocks
						// (Run is the only reader); Close alone leaves the client
						// in the map, so the next broadcast select-sends on the
						// closed channel — a panic. Delete from the map FIRST (we
						// hold the write lock), then close: later broadcasts can
						// never reach client.send again. WritePump exits on the
						// closed channel; ReadPump's Unregister is then a no-op.
						delete(clients, client)
						if len(clients) == 0 {
							delete(h.clients, message.JobID)
						}
						client.Close()
					}
				}
			}
			h.mu.Unlock()
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

// Broadcast sends a message to all clients listening for a job. The send is
// bounded, not blocking: h.broadcast is only drained by the Run loop, and if
// that loop is wedged (or never started) a blocking send would freeze every
// handler goroutine that broadcasts — the process stays up but never answers
// again ("silent server death"). A wedged or absent loop means the message
// cannot be delivered, so callers get an error and the drop is observable in
// the logs instead of an invisible hang.
func (h *Hub) Broadcast(jobID string, message []byte) error {
	msg := &BroadcastMessage{
		JobID:   jobID,
		Message: message,
	}
	select {
	case h.broadcast <- msg:
		return nil
	case <-time.After(hubSendTimeout):
		return fmt.Errorf("websocket hub not accepting broadcasts (loop stalled or stopped); dropped message for job %s", jobID)
	}
}

// BroadcastWSMessage stamps a per-job monotonically increasing sequence
// number on data-bearing messages, then broadcasts to all clients for a job.
// The counter lives until the terminal event broadcast (complete/error), so
// the trailing complete message continues the numbering and a reconnecting
// client's lastSeq stays meaningful. With a SeqStore, the counter is persisted
// on every increment (outside h.mu) and restored after a restart, so numbering
// never resets to 1. The seq increment and channel send are atomic under
// bcastMu so concurrent broadcasts stay in order.
func (h *Hub) BroadcastWSMessage(msg protocol.WSMessage) error {
	if msg.Type != protocol.WSMsgHeartbeat {
		// Restore a persisted counter before locking, when the first
		// post-restart broadcast for this job arrives: LoadWSJobSeq does
		// SQLite I/O and must never run under h.mu, which the Run loop's
		// register/unregister/broadcast paths share. The locked re-check in
		// ensureSeq resolves races with concurrent broadcasts for the same
		// job (each may speculatively load here; only one result wins).
		h.prefetchSeq(msg.JobID)

		// bcastMu spans the seq increment and the channel send so that
		// concurrent broadcasts for the same job are enqueued in seq order.
		// It is NOT taken by the Run loop, so a blocked send cannot
		// deadlock with it.
		h.bcastMu.Lock()
		h.mu.Lock()
		h.ensureSeq(msg.JobID)
		// A terminal-status broadcast must NOT delete the counter here:
		// BroadcastComplete immediately follows BroadcastStatus(completed/
		// failed/...) in the handlers, and deleting at the status step made
		// the complete broadcast renumber from 1 — connected clients read
		// that reset as proven data loss and failed intact jobs with a
		// spurious gap. The status carries lastSeq forward; the
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
		data, err := json.Marshal(msg)
		h.mu.Unlock()
		if err != nil {
			h.bcastMu.Unlock()
			return err
		}
		bcastErr := h.Broadcast(msg.JobID, data)
		h.bcastMu.Unlock()

		h.persistSeq(msg.JobID, msg.Seq, terminal)
		return bcastErr
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	h.bcastMu.Lock()
	err = h.Broadcast(msg.JobID, data)
	h.bcastMu.Unlock()
	return err
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
		c.closeConn()
	}()

	for {
		// A message that did not fit the previous frame goes out first: it was
		// already taken off send, so holding it back stalls the stream.
		if c.pending != nil {
			message := c.pending
			c.pending = nil
			if !c.writeFrame(message) {
				return
			}
			continue
		}

		select {
		case message, ok := <-c.send:
			if !ok {
				c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
				c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}

			if !c.writeFrame(message) {
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

// writeFrame writes message plus the queued messages that fit within
// protocol.WSFrameByteBudget as one newline-delimited text frame, and reports
// whether the connection is still usable. The first message is always written
// whole: a message must never be split across frames, and the client's read
// limit leaves room for one over-budget message. Draining the queue without a
// byte cap used to build frames larger than that limit, which the client
// discarded wholesale — surfacing as a sequence gap and, for streaming
// output, as lost transcoded data.
func (c *Client) writeFrame(message []byte) bool {
	c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	w, err := c.conn.NextWriter(websocket.TextMessage)
	if err != nil {
		return false
	}
	w.Write(message)
	total := len(message)

	for total < protocol.WSFrameByteBudget {
		next, ok := c.nextQueued()
		if !ok {
			break
		}
		if total+len(next) > protocol.WSFrameByteBudget {
			c.pending = next
			break
		}
		w.Write([]byte{'\n'})
		w.Write(next)
		total += 1 + len(next)
	}

	return w.Close() == nil
}

// nextQueued takes one already-queued message off send, reporting false when
// none is waiting. The channel being closed also reports false: the frame ends
// and the next WritePump iteration observes the close and shuts down.
func (c *Client) nextQueued() ([]byte, bool) {
	select {
	case next, ok := <-c.send:
		if !ok {
			return nil, false
		}
		return next, true
	default:
		return nil, false
	}
}

// Close closes the client's connection and send channel. It must be called
// only from the hub's Run loop (unregister/shed paths), after the client has
// been removed from h.clients: closing c.send while a broadcast could still
// reach it makes the Run loop panic ("send on closed channel") — the exact
// silent-server-death crash this invariant prevents.
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

// closeConn closes the underlying connection without touching c.send. The
// WritePump defer calls it on exit: when the write side fails first (ping
// write error, write deadline, peer gone), the connection must be torn down
// to wake the peer, but c.send must stay open until the Run loop's Unregister
// removes the client — otherwise a broadcast for the still-registered job
// select-sends on the now-closed channel and panics the Run loop (a second
// door into the crash, reachable under client churn rather than
// buffer shed).
func (c *Client) closeConn() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.conn.Close()
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
