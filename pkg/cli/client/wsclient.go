package client

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/tsix404/rffmpeg/pkg/protocol"
)

const (
	// WebSocketEndpoint is the WebSocket endpoint for job logs
	WebSocketEndpoint = "/api/v1/jobs/%s/log"

	// WebSocket connection settings
	WSReconnectDelay    = 1 * time.Second
	WSMaxReconnectDelay = 30 * time.Second
	WSReadTimeout       = 60 * time.Second
	WSWriteTimeout      = 10 * time.Second
	WSHeartbeatInterval = 30 * time.Second
)

// WSClient handles WebSocket connections for real-time log streaming
type WSClient struct {
	serverURL    string
	jobID        string
	token        string
	conn         *websocket.Conn
	mu           sync.Mutex
	done         chan struct{}
	onStderr     func(chunk string)
	onStdout     func(chunk string)
	onStatus     func(status protocol.JobStatus, exitCode int, err string)
	onProgress   func(percent float64)
	onComplete   func(exitCode int)
	onError      func(errMsg string)
	connected    bool
	reconnecting bool
}

// WSClientOption is a functional option for WSClient
type WSClientOption func(*WSClient)

// WithOnStderr sets the stderr handler
func WithOnStderr(handler func(chunk string)) WSClientOption {
	return func(c *WSClient) {
		c.onStderr = handler
	}
}

// WithOnStdout sets the stdout handler (streaming output mode)
func WithOnStdout(handler func(chunk string)) WSClientOption {
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

// WithOnProgress sets the progress handler
func WithOnProgress(handler func(percent float64)) WSClientOption {
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

// NewWSClient creates a new WebSocket client
func NewWSClient(serverURL, jobID, token string, opts ...WSClientOption) *WSClient {
	// Convert HTTP URL to WebSocket URL
	wsURL := strings.Replace(serverURL, "http://", "ws://", 1)
	wsURL = strings.Replace(wsURL, "https://", "wss://", 1)

	client := &WSClient{
		serverURL: wsURL,
		jobID:     jobID,
		token:     token,
		done:      make(chan struct{}),
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
			body, _ := io.ReadAll(resp.Body)
			return fmt.Errorf("WebSocket connection failed (status %d): %s", resp.StatusCode, string(body))
		}
		return fmt.Errorf("WebSocket connection failed: %w", err)
	}

	c.conn = conn
	c.connected = true
	c.reconnecting = false

	// Set read limit
	conn.SetReadLimit(1 << 20) // 1MB max message size

	return nil
}

// ConnectWithReconnect establishes a WebSocket connection with automatic reconnection
func (c *WSClient) ConnectWithReconnect(ctx context.Context) error {
	reconnectDelay := WSReconnectDelay

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

// Listen starts listening for WebSocket messages
func (c *WSClient) Listen(ctx context.Context) error {
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
		conn.SetReadDeadline(time.Now().Add(WSReadTimeout))

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

		// Handle multiple messages (batched)
		scanner := bufio.NewScanner(strings.NewReader(string(data)))
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
	}
}

// handleMessage processes a WebSocket message
func (c *WSClient) handleMessage(msg protocol.WSMessage) {
	switch msg.Type {
	case protocol.WSMsgStderr:
		if c.onStderr != nil && msg.Payload != "" {
			c.onStderr(msg.Payload)
		}

	case protocol.WSMsgStdout:
		if c.onStdout != nil && msg.Payload != "" {
			c.onStdout(msg.Payload)
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
			}
			c.onProgress(payload.Percent)
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
		WithOnProgress(func(percent float64) {
			if !quiet {
				fmt.Fprintf(os.Stderr, "Progress: %.1f%%\n", percent)
			}
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

	// Wait for completion, error, or context cancellation
	select {
	case exitCode := <-completeChan:
		if exitCode != 0 {
			return fmt.Errorf("job exited with code %d", exitCode)
		}
		return nil
	case errMsg := <-errorChan:
		return fmt.Errorf("job error: %s", errMsg)
	case err := <-listenDone:
		if err != nil {
			return fmt.Errorf("WebSocket error: %w", err)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
