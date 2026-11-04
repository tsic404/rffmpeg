package client

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/tsic404/rffmpeg/pkg/protocol"
)

const (
	UploadEndpoint = "/api/v1/upload"
	JobsEndpoint   = "/api/v1/jobs"
	JobEndpoint    = "/api/v1/jobs/%s"
	OutputEndpoint = "/api/v1/output/%s"
	ProbeEndpoint  = "/api/v1/probe"
	HealthEndpoint = "/api/v1/health"
	DefaultTimeout = 30 * time.Second
	UploadTimeout  = 10 * time.Minute
	// DownloadIdleTimeout bounds a download by inactivity, not by total
	// duration: a multi-gigabyte output that keeps delivering bytes is never
	// cut off, while a stream that stops moving fails within one minute. There
	// is deliberately no overall transfer deadline.
	DownloadIdleTimeout = 60 * time.Second
	// HealthProbeTimeout bounds a single health probe attempt. The probe is a
	// readiness gate, not a data transfer: a server that accepts the connection
	// but cannot answer within this budget is stalled, and a fresh attempt is
	// cheaper than waiting longer on one. Kept below DefaultTimeout so the
	// probe never inherits the business timeout it does not need.
	HealthProbeTimeout = 20 * time.Second
	// HealthProbeAttempts bounds the probe's retry loop: 3 attempts of
	// HealthProbeTimeout plus 1s/2s backoff wait up to 63s in total before a
	// hung server is reported. That is deliberately longer than the single 30s
	// attempt it replaces — a server busy enough to miss one probe (a
	// concurrent-client burst, a host under memory or I/O pressure) is reached
	// by a later one instead of failing the command outright — and the worst
	// case is only paid when every attempt hangs; a refused connection still
	// fails immediately.
	HealthProbeAttempts = 3
	// HealthProbeBackoff is the first retry interval; it doubles per attempt.
	HealthProbeBackoff = time.Second
	// ProbeTimeout bounds a single probe request. The probe endpoint is
	// synchronous: it dispatches a job and waits for a worker to run ffprobe,
	// which on a cold worker (first ffmpeg/GPU initialization) can exceed the
	// default 30s client timeout. The value aligns with the server's 2-minute
	// probe wait budget plus margin for the terminal response to arrive.
	ProbeTimeout = 2*time.Minute + 30*time.Second
	PollInterval = 2 * time.Second
	MaxRetries   = 3
	RetryDelay   = 1 * time.Second

	// DefaultMaxRetries bounds the CLI's retry loops when contact with the
	// server is lost mid-job: 14 attempts on the WebSocket exponential-backoff
	// schedule (1s, 2s, 4s, 8s, 16s, then 30s) span about 5 minutes of retry
	// time. It is the attempt budget for every mid-job failure; the tighter
	// DefaultServerLossTimeout ends a wait on a silent server well before it.
	DefaultMaxRetries = 14

	// DefaultServerLossTimeout caps how long the CLI keeps waiting for an
	// already-submitted job once the server stops answering at all
	// (--server-loss-timeout / RFFMPEG_SERVER_LOSS_TIMEOUT): only transport-level
	// failures count, so an answered request — an error status included — resets
	// the clock. The cap must outlast a routine server restart, or an ordinary
	// restart turns into the "gave up waiting" exit code while the job keeps
	// running server-side; 30s spans the reconnect ramp (0s/1s/3s/7s/15s) and
	// stays far below the ~5 min retry budget. 0 = --max-retries decides alone.
	DefaultServerLossTimeout = 30 * time.Second

	// DefaultSubmitRetries bounds the CLI's rate-limit (HTTP 429) resubmission
	// loop when --retry is enabled. Backoff starts at the server's retry_in
	// hint (or 1s) and doubles per attempt, capped at submitRetryMaxDelay.
	DefaultSubmitRetries = 5

	// submitRetryMaxDelay caps a single backoff wait so --retry never parks
	// the CLI for an unbounded time on a stuck server.
	submitRetryMaxDelay = 60 * time.Second

	// StreamingDrainTimeout bounds how long the streaming wait will block for
	// the WebSocket to deliver its terminal event after the HTTP poll reports
	// completion. The terminal event is sequenced after every stdout frame, so
	// once it arrives the stream is fully delivered; the timeout is a safety
	// bound for a stalled connection, never a normal-path cost.
	StreamingDrainTimeout = 1 * time.Second
)

// Overridable poll interval for regression tests; production value mirrors
// PollInterval. Internal tests swap this to a short duration to exercise
// timing-sensitive code paths without 2s waits.
var pollInterval = PollInterval

// probeTimeout is the effective per-request timeout for the probe endpoint.
// A var (rather than a direct const reference) so regression tests can stub
// the wait and exercise the timing-sensitive path without a 30s+ real-time
// delay.
var probeTimeout = ProbeTimeout

// Client is the HTTP client for rffmpeg server
type Client struct {
	serverURL     string
	token         string
	http          *http.Client
	maxRetries    int
	submitRetries int
	// serverLossTimeout caps one continuous silence — requests the server never
	// answered — while waiting for an already-submitted job
	// (--server-loss-timeout / RFFMPEG_SERVER_LOSS_TIMEOUT). 0 disables the cap:
	// --max-retries alone decides when to give up.
	serverLossTimeout time.Duration
}

// ClientOption configures a Client.
type ClientOption func(*Client)

// WithMaxRetries sets the retry budget for the WS reconnect loop and the HTTP
// status-poll fallback (--max-retries / RFFMPEG_MAX_RETRIES). 0 means "no
// retries" — fail fast after the first failed attempt.
func WithMaxRetries(n int) ClientOption {
	return func(c *Client) {
		if n >= 0 {
			c.maxRetries = n
		}
	}
}

// WithServerLossTimeout caps how long the client keeps waiting for an
// already-submitted job once the server stops answering
// (--server-loss-timeout / RFFMPEG_SERVER_LOSS_TIMEOUT). 0 disables the cap and
// leaves --max-retries in charge.
func WithServerLossTimeout(d time.Duration) ClientOption {
	return func(c *Client) {
		if d >= 0 {
			c.serverLossTimeout = d
		}
	}
}

// normalizeServerURL strips a trailing /api/v1 (with or without trailing
// slashes) from a server root URL. The CLI appends /api/v1 to the root URL
// itself, so a user-supplied "http://host/api/v1" would otherwise produce
// "http://host/api/v1/api/v1/..." and fail with 404. Trailing slashes are
// removed as before.
func normalizeServerURL(serverURL string) string {
	serverURL = strings.TrimSuffix(serverURL, "/")
	serverURL = strings.TrimSuffix(serverURL, "/api/v1")
	return strings.TrimSuffix(serverURL, "/")
}

// New creates a new client.
func New(serverURL, token string, opts ...ClientOption) *Client {
	c := &Client{
		serverURL:         normalizeServerURL(serverURL),
		token:             token,
		http:              &http.Client{Timeout: DefaultTimeout},
		maxRetries:        DefaultMaxRetries,
		serverLossTimeout: DefaultServerLossTimeout,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// DrainAndClose drains the response body and closes it, returning any error
func DrainAndClose(body io.ReadCloser) error {
	if body == nil {
		return nil
	}
	_, drainErr := io.Copy(io.Discard, body)
	closeErr := body.Close()
	if drainErr != nil {
		return drainErr
	}
	return closeErr
}

// parseErrorResponse decodes a non-200 response body as protocol.ErrorResponse
// and reports whether it carried a usable (non-empty) server message. Callers
// fall back to a status-code error when it reports false, so an empty or
// undecodable body never degrades to a bare "<op> failed: " with no diagnostic.
func parseErrorResponse(body io.Reader) (protocol.ErrorResponse, bool) {
	var errResp protocol.ErrorResponse
	if err := json.NewDecoder(body).Decode(&errResp); err != nil || errResp.Message == "" {
		return protocol.ErrorResponse{}, false
	}
	return errResp, true
}

// serverRejection renders a non-2xx response from a job-submission endpoint
// (POST /jobs, POST /probe) as a one-line error. The server's classification
// code stays on that line — "<op> failed [<code>]: <message>" — so the
// rejection can be keyed off without parsing the message text. A response the
// server did not classify falls back to the HTTP status, and a body without a
// message never degrades to a bare "<op> failed: ".
func serverRejection(op string, status int, code protocol.ErrorCode, message string) error {
	switch {
	case message == "":
		return fmt.Errorf("%s failed with status %d", op, status)
	case code == "":
		return fmt.Errorf("%s failed: %s", op, message)
	default:
		return fmt.Errorf("%s failed [%s]: %s", op, code, message)
	}
}

// setAuthHeader sets the authorization header if token is present
func (c *Client) setAuthHeader(req *http.Request) {
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
}
