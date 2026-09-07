package client

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/tsic404/rffmpeg/pkg/protocol"
)

const (
	// WebSocketEndpoint is the WebSocket endpoint for job logs
	WebSocketEndpoint = "/api/v1/jobs/%s/log"

	// WebSocket connection settings
	WSReconnectDelay    = 1 * time.Second
	WSMaxReconnectDelay = 30 * time.Second
	WSReadTimeout       = 60 * time.Second
	WSWriteTimeout      = 10 * time.Second

	// WSPingInterval bounds how often the client pings the server. It must
	// stay well below WSReadTimeout (and below the server's 60s read
	// deadline) so a healthy idle connection is never reaped by either
	// side's deadline (TSI-2414).
	WSPingInterval = 25 * time.Second

	// wsMaxFrameLineBytes is the maximum size of a single JSON WSMessage
	// line inside a batched text frame. It must exceed the largest possible
	// stdout message: StdoutBatcher concatenates up to 10 executor chunks of
	// 32KB each and base64-encodes the result (~427KB), plus JSON overhead.
	// 1MB matches the connection's SetReadLimit in connect().
	wsMaxFrameLineBytes = 1 << 20
)

// Overridable keepalive timings for regression tests; production values
// mirror the exported consts above. Stored atomically: tests shrink them
// while a keepalive pinger goroutine from a prior Listen may still be
// running and reading them (TSI-2804).
var (
	wsReadTimeout  atomic.Int64 // nanoseconds
	wsPingInterval atomic.Int64 // nanoseconds
)

func init() {
	wsReadTimeout.Store(int64(WSReadTimeout))
	wsPingInterval.Store(int64(WSPingInterval))
}

// WSClient handles WebSocket connections for real-time log streaming
type WSClient struct {
	serverURL    string
	jobID        string
	token        string
	conn         *websocket.Conn
	mu           sync.Mutex
	done         chan struct{}
	onStderr     func(chunk string)
	onStdout     func(chunk []byte)
	onStatus     func(status protocol.JobStatus, exitCode int, err string)
	onProgress   func(p protocol.WSProgressPayload)
	onComplete   func(exitCode int)
	onError      func(errMsg string)
	connected    bool
	reconnecting bool
	// maxRetries bounds the reconnect loop (--max-retries / RFFMPEG_MAX_RETRIES).
	// NewWSClient defaults it to DefaultMaxRetries; WithWSMaxRetries(0) means
	// "no retries" — fail fast after the first failed attempt.
	maxRetries int
	// lastSeq is the highest sequence number seen. A received Seq greater
	// than lastSeq+1 means messages were lost during a reconnect — output
	// has a hole and the stream must not silently continue.
	lastSeq    int64
	sawMessage bool // false until the first sequenced message arrives
	gapFound   bool // set once a gap is detected; sticky for the session
	// sawDataAfterRestart records whether any data-bearing (stderr/stdout)
	// message arrived on the current connection. A seq==1 restart observed
	// before any post-reconnect data is a benign renumbering (TSI-2382); the
	// same reset after real data flowed proves messages were lost. Reset to
	// false in connect() on every successful (re)connect.
	sawDataAfterRestart bool
	// terminalSeen records whether a terminal job status arrived on this
	// session: after it, the hub may legitimately restart numbering for any
	// trailing broadcasts (complete/error/late stderr), so seq resets are no
	// longer evidence of data loss.
	terminalSeen bool
}

// WSClientOption is a functional option for WSClient
type WSClientOption func(*WSClient)

// WithOnStderr sets the stderr handler
func WithOnStderr(handler func(chunk string)) WSClientOption {
	return func(c *WSClient) {
		c.onStderr = handler
	}
}

// WithOnStdout sets the stdout handler (streaming output mode).
//
// BREAKING (TSI-2355): the handler now receives decoded raw bytes. The
// WSMsgStdout Payload on the wire changed from raw text to standard base64 —
// required because JSON text frames corrupt invalid UTF-8 to U+FFFD — so CLIs
// built against the old protocol cannot parse stdout messages from new servers.
func WithOnStdout(handler func(chunk []byte)) WSClientOption {
	return func(c *WSClient) {
		c.onStdout = handler
	}
}

// WithOnStatus sets the status handler
func WithOnStatus(handler func(status protocol.JobStatus, exitCode int, err string)) WSClientOption {
	return func(c *WSClient) {
		c.onStatus = handler
	}
}

// WithOnProgress sets the progress handler. The handler receives the full
// WSProgressPayload — percent, speed, ETA and media timestamps — so callers
// can render an ETA line instead of a bare percentage (TSI-2425).
func WithOnProgress(handler func(p protocol.WSProgressPayload)) WSClientOption {
	return func(c *WSClient) {
		c.onProgress = handler
	}
}

// WithOnComplete sets the complete handler
func WithOnComplete(handler func(exitCode int)) WSClientOption {
	return func(c *WSClient) {
		c.onComplete = handler
	}
}

// WithOnError sets the error handler
func WithOnError(handler func(errMsg string)) WSClientOption {
	return func(c *WSClient) {
		c.onError = handler
	}
}

// WithWSMaxRetries bounds the WebSocket reconnect loop (--max-retries /
// RFFMPEG_MAX_RETRIES). 0 means "no retries" — fail fast after the first
// failed attempt.
func WithWSMaxRetries(n int) WSClientOption {
	return func(c *WSClient) {
		if n >= 0 {
			c.maxRetries = n
		}
	}
}

// NewWSClient creates a new WebSocket client
func NewWSClient(serverURL, jobID, token string, opts ...WSClientOption) *WSClient {
	// Convert HTTP URL to WebSocket URL
	wsURL := strings.Replace(serverURL, "http://", "ws://", 1)
	wsURL = strings.Replace(wsURL, "https://", "wss://", 1)

	client := &WSClient{
		serverURL:  wsURL,
		jobID:      jobID,
		token:      token,
		done:       make(chan struct{}),
		maxRetries: DefaultMaxRetries,
	}

	for _, opt := range opts {
		opt(client)
	}

	return client
}

// Connect establishes a WebSocket connection
func (c *WSClient) Connect(ctx context.Context) error {
	return c.connect(ctx)
}

func (c *WSClient) connect(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Build WebSocket URL
	u, err := url.Parse(c.serverURL + fmt.Sprintf(WebSocketEndpoint, c.jobID))
	if err != nil {
		return fmt.Errorf("failed to parse WebSocket URL: %w", err)
	}

	// Create request headers
	headers := http.Header{}
	if c.token != "" {
		headers.Set("Authorization", "Bearer "+c.token)
	}

	// Dial WebSocket
	dialer := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
	}

	conn, resp, err := dialer.Dial(u.String(), headers)
	if err != nil {
		if resp != nil {
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			return &HandshakeError{Status: resp.StatusCode, Body: string(body)}
		}
		return fmt.Errorf("WebSocket connection failed: %w", err)
	}

	c.conn = conn
	c.connected = true
	c.reconnecting = false
	// A fresh connection starts a new observation window: whether data has
	// flowed "since the last reconnect" must be re-evaluated from zero, or
	// the flag carried over from the dropped connection makes every seq==1
	// reset look like proven loss (TSI-2382 review blocker #1).
	c.sawDataAfterRestart = false

	// A pong from the server proves the connection is alive: push the read
	// deadline out again. Without this, a quiet period (long transcode with
	// no stderr/stdout output) longer than the deadline reaps an idle but
	// healthy connection, and broadcasts sent during the forced reconnect
	// window are lost — surfacing as a spurious seq gap (TSI-2414).
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(time.Duration(wsReadTimeout.Load())))
		return nil
	})

	// Set read limit
	conn.SetReadLimit(1 << 20) // 1MB max message size
	return nil
}

// HandshakeError is returned by connect when the WebSocket upgrade fails with
// an HTTP status (e.g. 401 for a bad token). Callers use it to stop retrying
// errors that no backoff can fix.
type HandshakeError struct {
	Status int
	Body   string
}

func (e *HandshakeError) Error() string {
	return fmt.Sprintf("WebSocket connection failed (status %d): %s", e.Status, e.Body)
}

// ConnectWithReconnect establishes a WebSocket connection with automatic reconnection.
// Transient failures retry with exponential backoff up to maxRetries attempts
// (--max-retries / RFFMPEG_MAX_RETRIES); a 4xx handshake rejection (bad token,
// unknown job, forbidden) is permanent and aborts immediately — no retry
// interval can fix it. When the retry budget is spent it returns an error
// wrapping ErrRetriesExhausted so callers can surface a distinct exit code.
func (c *WSClient) ConnectWithReconnect(ctx context.Context) error {
	reconnectDelay := WSReconnectDelay
	maxRetries := c.maxRetries

	retries := 0
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-c.done:
			return nil
		default:
		}

		err := c.connect(ctx)
		if err == nil {
			return nil
		}

		var hsErr *HandshakeError
		if errors.As(err, &hsErr) && hsErr.Status >= 400 && hsErr.Status < 500 {
			return err
		}

		if retries >= maxRetries {
			return fmt.Errorf("%w: WebSocket connection failed after %d retries: %v", ErrRetriesExhausted, retries, err)
		}
		retries++

		log.Printf("WebSocket connection failed: %v, retrying in %v...", err, reconnectDelay)

		select {
		case <-time.After(reconnectDelay):
		case <-ctx.Done():
			return ctx.Err()
		case <-c.done:
			return nil
		}

		// Exponential backoff
		reconnectDelay *= 2
		if reconnectDelay > WSMaxReconnectDelay {
			reconnectDelay = WSMaxReconnectDelay
		}
	}
}

// retryBudget returns the total wall-clock time spent on maxRetries WebSocket
// reconnect attempts under the exponential-backoff schedule (1s, 2s, 4s, 8s,
// 16s, then 30s cap). The HTTP status-poll fallback reuses the same budget so
// both retry paths give up after roughly the same amount of contact loss
// (TSI-2697).
func retryBudget(maxRetries int) time.Duration {
	if maxRetries <= 0 {
		return 0
	}
	delay := WSReconnectDelay
	var total time.Duration
	for i := 0; i < maxRetries; i++ {
		total += delay
		delay *= 2
		if delay > WSMaxReconnectDelay {
			delay = WSMaxReconnectDelay
		}
	}
	return total
}

// Listen starts listening for WebSocket messages
func (c *WSClient) Listen(ctx context.Context) error {

	// Client-side keepalive (TSI-2414): periodically ping the server; the
	// server's pong handler pushes its read deadline out, and our pong
	// handler (installed in connect) pushes ours out on every reply. A
	// quiet period — a long transcode with no stderr/stdout traffic — used
	// to trip the read deadline and force a reconnect whose missed
	// broadcasts surfaced downstream as a spurious seq gap.
	//
	// The pinger is a dedicated goroutine rather than a select arm: the
	// loop body blocks in ReadMessage for up to the read deadline, which
	// can exceed the ping interval — a select arm would starve. It is
	// deliberately NOT joined on return: a graceful server close (1000)
	// must return promptly while the pinger may still be inside its
	// WSWriteTimeout-bounded write. Its lifetime is bounded by ctx/done
	// and by conn.Close() (called by Close() and on ping failure), so it
	// never outlives the session meaningfully.
	go func() {
		ticker := time.NewTicker(time.Duration(wsPingInterval.Load()))
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-c.done:
				return
			case <-ticker.C:
			}

			c.mu.Lock()
			conn := c.conn
			c.mu.Unlock()
			if conn == nil {
				continue
			}
			if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(WSWriteTimeout)); err != nil {
				// A failed keepalive write means the connection is dead:
				// close it so the blocked ReadMessage wakes up and the
				// normal reconnect path takes over.
				log.Printf("WebSocket keepalive ping failed for job %s: %v", c.jobID, err)
				conn.Close()
				return
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-c.done:
			return nil
		default:
		}

		c.mu.Lock()
		conn := c.conn
		c.mu.Unlock()

		if conn == nil {
			return fmt.Errorf("not connected")
		}

		// Set read deadline
		conn.SetReadDeadline(time.Now().Add(time.Duration(wsReadTimeout.Load())))

		messageType, data, err := conn.ReadMessage()
		if err != nil {
			// Check if this is an intentional shutdown (Close() was called)
			select {
			case <-c.done:
				return nil
			default:
			}

			if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				return nil
			}

			// Try to reconnect
			c.mu.Lock()
			c.connected = false
			c.reconnecting = true
			c.mu.Unlock()

			log.Printf("WebSocket disconnected: %v, attempting to reconnect...", err)

			if err := c.ConnectWithReconnect(ctx); err != nil {
				return err
			}

			continue
		}

		if messageType != websocket.TextMessage {
			continue
		}

		// Handle multiple messages (batched). The buffer MUST be grown
		// beyond bufio's default 64KB token limit: a single WSMsgStdout
		// message carries up to 10×32KB of base64-encoded media data
		// (~430KB JSON) when the worker's StdoutBatcher flushes, and the
		// hub's WritePump may batch several messages into one frame.
		// With the default limit Scanner aborts with ErrTooLong — silently,
		// since the error was never checked — dropping every remaining
		// line in the frame. The next delivered message then trips the gap
		// detector: "expected seq N, got N+1" on short streaming jobs even
		// though nothing was lost on the wire.
		scanner := bufio.NewScanner(strings.NewReader(string(data)))
		scanner.Buffer(make([]byte, 64*1024), wsMaxFrameLineBytes)
		for scanner.Scan() {
			line := scanner.Text()
			if line == "" {
				continue
			}

			var msg protocol.WSMessage
			if err := json.Unmarshal([]byte(line), &msg); err != nil {
				log.Printf("Failed to parse WebSocket message: %v", err)
				continue
			}

			c.handleMessage(msg)
		}
		if err := scanner.Err(); err != nil {
			log.Printf("WebSocket frame dropped for job %s: %v (%d bytes)", c.jobID, err, len(data))
		}
	}
}

// handleMessage processes a WebSocket message. Every data-bearing message is
// first checked for a sequence gap; a detected gap is sticky and surfaced via
// HasGap so callers can refuse to treat the output as trustworthy.
func (c *WSClient) handleMessage(msg protocol.WSMessage) {
	if msg.Type != protocol.WSMsgHeartbeat {
		c.mu.Lock()
		// A terminal status ends the sequenced stream for this job: the hub
		// deletes the counter on its terminal-event broadcast, so anything
		// still arriving afterwards (trailing complete, stderr drained in the
		// same worker report) may legitimately carry fresh numbering. That
		// reset is hub bookkeeping, not lost data (TSI-2382).
		if msg.Type == protocol.WSMsgStatus {
			if payload, ok := msg.Data.(map[string]interface{}); ok {
				if st, _ := payload["status"].(string); protocol.IsTerminalStatus(protocol.JobStatus(st)) {
					c.terminalSeen = true
				}
			}
		}
		switch {
		case !c.sawMessage:
			// First message of the session: adopt the current sequence.
			c.sawMessage = true
			c.lastSeq = msg.Seq
		case c.terminalSeen:
			// Post-terminal messages trail the job's result; their numbering
			// is irrelevant to gap detection — nothing before them can be
			// lost that matters. Track but never flag.
			c.lastSeq = msg.Seq
		case msg.Seq == 1 && c.lastSeq > 1 && c.sawDataAfterRestart:
			// Numbering restarted mid-stream AND data-bearing messages were
			// already received since the restart: everything between lastSeq
			// and the reset is gone, so treat it as a gap — not a fresh
			// stream.
			log.Printf("WebSocket stream sequence restarted for job %s: had seq %d, got 1 — data before server restart was lost",
				c.jobID, c.lastSeq)
			c.lastSeq = msg.Seq
			c.gapFound = true
		case msg.Seq == 1 && c.lastSeq > 1:
			// First message after a reconnect with numbering reset to 1 (TSI-2382):
			// the hub lost its counter (server restart/crash), but no sequenced
			// data has arrived on this connection yet — nothing proves messages
			// were lost between lastSeq and here. The job's terminal broadcasts
			// may have drained during the outage; adopt the new base instead of
			// false-flagging a gap.
			//
			// Adopting is a one-time amnesty for this reconnection: lastSeq
			// becomes 1, so subsequent seq 2,3,... are contiguous by
			// construction and the msg.Seq > lastSeq+1 branch cannot fire on
			// this connection until another reset is observed. That is the
			// accepted cost of not being able to distinguish "renumbered, all
			// data intact" from "renumbered, hole at the seam" without server
			// cooperation.
			log.Printf("WebSocket stream renumbered to 1 after reconnect for job %s (had seq %d): server likely restarted — adopting new base",
				c.jobID, c.lastSeq)
			c.lastSeq = msg.Seq
		case msg.Seq > c.lastSeq+1:
			log.Printf("WebSocket stream gap detected for job %s: expected seq %d, got %d — data was lost",
				c.jobID, c.lastSeq+1, msg.Seq)
			c.lastSeq = msg.Seq
			c.gapFound = true
		case msg.Seq > c.lastSeq:
			c.lastSeq = msg.Seq
		default:
			// Duplicate or reordered seq at/below lastSeq: nothing to adopt,
			// and no evidence of loss.
		}
		c.mu.Unlock()
	}

	// Track whether real payload data has flowed on this connection: it
	// upgrades a later seq==1 reset from "benign renumbering" to "proven
	// loss". Reset to false in connect() when a new connection is
	// established.
	if msg.Type == protocol.WSMsgStderr || msg.Type == protocol.WSMsgStdout {
		c.mu.Lock()
		c.sawDataAfterRestart = true
		c.mu.Unlock()
	}

	switch msg.Type {
	case protocol.WSMsgStderr:
		if c.onStderr != nil && msg.Payload != "" {
			c.onStderr(msg.Payload)
		}

	case protocol.WSMsgStdout:
		if c.onStdout != nil && msg.Payload != "" {
			// Payload is base64-encoded raw bytes; JSON text frames would
			// corrupt arbitrary binary (invalid UTF-8 becomes U+FFFD).
			raw, err := protocol.StdoutChunkBase64(msg.Payload)
			if err != nil {
				log.Printf("Invalid base64 stdout chunk: %v", err)
				break
			}
			c.onStdout(raw)
		}

	case protocol.WSMsgStatus:
		if c.onStatus != nil {
			var payload protocol.WSStatusPayload
			if data, ok := msg.Data.(map[string]interface{}); ok {
				if status, ok := data["status"].(string); ok {
					payload.Status = protocol.JobStatus(status)
				}
				if exitCode, ok := data["exit_code"].(float64); ok {
					payload.ExitCode = int(exitCode)
				}
				if err, ok := data["error"].(string); ok {
					payload.Error = err
				}
			}
			c.onStatus(payload.Status, payload.ExitCode, payload.Error)
		}

	case protocol.WSMsgProgress:
		if c.onProgress != nil {
			var payload protocol.WSProgressPayload
			if data, ok := msg.Data.(map[string]interface{}); ok {
				if percent, ok := data["percent"].(float64); ok {
					payload.Percent = percent
				}
				if speed, ok := data["speed"].(float64); ok {
					payload.Speed = speed
				}
				if eta, ok := data["eta_seconds"].(float64); ok {
					payload.EtaSeconds = int(eta)
				}
			}
			c.onProgress(payload)
		}

	case protocol.WSMsgComplete:
		if c.onComplete != nil {
			var exitCode int
			if data, ok := msg.Data.(map[string]interface{}); ok {
				if code, ok := data["exit_code"].(float64); ok {
					exitCode = int(code)
				}
			}
			c.onComplete(exitCode)
		}

	case protocol.WSMsgError:
		if c.onError != nil && msg.Payload != "" {
			c.onError(msg.Payload)
		}

	case protocol.WSMsgHeartbeat:
		// Just ignore heartbeat messages
	}
}

// Close closes the WebSocket connection
func (c *WSClient) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	select {
	case <-c.done:
		return nil
	default:
		close(c.done)
	}

	c.connected = false

	if c.conn != nil {
		// Send close message
		c.conn.SetWriteDeadline(time.Now().Add(WSWriteTimeout))
		c.conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
		err := c.conn.Close()
		c.conn = nil
		return err
	}

	return nil
}

// IsConnected returns whether the client is connected
func (c *WSClient) IsConnected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.connected
}

// HasGap reports whether a sequence gap was detected during this session —
// i.e. some stderr/stdout/status messages were lost across a reconnect.
// Streaming consumers MUST treat the received output as incomplete when true.
func (c *WSClient) HasGap() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.gapFound
}

// StreamJobLogs is a convenience function that streams job logs to stderr
// and returns when the job completes or an error occurs
func StreamJobLogs(ctx context.Context, serverURL, jobID, token string, quiet bool) error {
	// Create a channel to signal completion
	completeChan := make(chan int, 1)
	errorChan := make(chan string, 1)

	client := NewWSClient(serverURL, jobID, token,
		WithOnStderr(func(chunk string) {
			// Write to local stderr
			if !quiet {
				fmt.Fprint(os.Stderr, chunk)
			}
		}),
		WithOnStatus(func(status protocol.JobStatus, exitCode int, err string) {
			if !quiet && status != protocol.JobStatusRunning {
				fmt.Fprintf(os.Stderr, "Job status: %s", status)
				if err != "" {
					fmt.Fprintf(os.Stderr, " (error: %s)", err)
				}
				fmt.Fprintln(os.Stderr)
			}
		}),
		WithOnProgress(func(p protocol.WSProgressPayload) {
			if quiet {
				return
			}
			fmt.Fprintln(os.Stderr, renderProgressLine(p))
		}),
		WithOnComplete(func(exitCode int) {
			completeChan <- exitCode
		}),
		WithOnError(func(errMsg string) {
			errorChan <- errMsg
		}),
	)
	defer client.Close()

	// Connect with reconnection support
	if err := client.ConnectWithReconnect(ctx); err != nil {
		return fmt.Errorf("failed to connect to WebSocket: %w", err)
	}

	// Start listening in a goroutine
	listenDone := make(chan error, 1)
	go func() {
		listenDone <- client.Listen(ctx)
	}()

	// Wait for completion, error, or context cancellation. On success paths,
	// a detected sequence gap downgrades the result to an error: the stream
	// lost data, so the output is not trustworthy.
	select {
	case exitCode := <-completeChan:
		if exitCode != 0 {
			return fmt.Errorf("job exited with code %d", exitCode)
		}
		return checkGap(client)
	case errMsg := <-errorChan:
		return fmt.Errorf("job error: %s", errMsg)
	case err := <-listenDone:
		if err != nil {
			return fmt.Errorf("WebSocket error: %w", err)
		}
		return checkGap(client)
	case <-ctx.Done():
		return ctx.Err()
	}
}

// checkGap converts a successful stream result into an error when a sequence
// gap was detected — the output has holes and must not be treated as complete.
func checkGap(c *WSClient) error {
	if c.HasGap() {
		return fmt.Errorf("streaming output incomplete: sequence gap detected (data lost during reconnect)")
	}
	return nil
}
