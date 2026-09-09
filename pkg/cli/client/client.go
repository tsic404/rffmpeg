package client

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tsic404/rffmpeg/pkg/protocol"
)

// formatETA renders a remaining-seconds estimate as H:MM:SS (or M:SS under
// one hour). Negative or absurd values collapse to 0 — ffmpeg's speed
// estimate briefly spikes around encoder warm-up and would otherwise print
// a negative ETA.
func formatETA(seconds int) string {
	if seconds < 0 {
		seconds = 0
	}
	h := seconds / 3600
	m := (seconds % 3600) / 60
	s := seconds % 60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%d:%02d", m, s)
}

// renderProgressLine formats a WS progress payload as the CLI stderr
// progress line: "Progress: X% | ETA: <formatted or n/a>". The ETA segment is
// always present so monitoring scripts can parse a stable shape even on short
// tasks where the worker never produced an ETA (TSI-2845). Shared by every
// WebSocket wait/stream entry point.
func renderProgressLine(p protocol.WSProgressPayload) string {
	eta := "n/a"
	if p.EtaSeconds > 0 {
		eta = formatETA(p.EtaSeconds)
	}
	return fmt.Sprintf("Progress: %.1f%% | ETA: %s", p.Percent, eta)
}

const (
	UploadEndpoint  = "/api/v1/upload"
	JobsEndpoint    = "/api/v1/jobs"
	JobEndpoint     = "/api/v1/jobs/%s"
	OutputEndpoint  = "/api/v1/output/%s"
	ProbeEndpoint   = "/api/v1/probe"
	HealthEndpoint  = "/api/v1/health"
	DefaultTimeout  = 30 * time.Second
	UploadTimeout   = 10 * time.Minute
	DownloadTimeout = 10 * time.Minute
	PollInterval    = 2 * time.Second
	MaxRetries      = 3
	RetryDelay      = 1 * time.Second

	// DefaultMaxRetries bounds the CLI's retry loops when contact with the
	// server is lost mid-job. 14 retries on the WebSocket exponential-backoff
	// schedule (1s, 2s, 4s, 8s, 16s, then 30s) span about 5 minutes of total
	// retry time before the CLI gives up with a distinct "job submitted"
	// exit code (TSI-2697).
	DefaultMaxRetries = 14

	// DefaultSubmitRetries bounds the CLI's rate-limit (HTTP 429) resubmission
	// loop when --retry is enabled. Backoff starts at the server's retry_in
	// hint (or 1s) and doubles per attempt, capped at submitRetryMaxDelay
	// (TSI-2939).
	DefaultSubmitRetries = 5

	// submitRetryMaxDelay caps a single backoff wait so --retry never parks
	// the CLI for an unbounded time on a stuck server.
	submitRetryMaxDelay = 60 * time.Second

	// StreamingDrainTimeout bounds how long the streaming wait will block for
	// the WebSocket to deliver its terminal event after the HTTP poll reports
	// completion. The terminal event is sequenced after every stdout frame, so
	// once it arrives the stream is fully delivered; the timeout is a safety
	// bound for a stalled connection, never a normal-path cost (TSI-2905).
	StreamingDrainTimeout = 1 * time.Second
)

// Overridable poll interval for regression tests; production value mirrors
// PollInterval. Internal tests swap this to a short duration to exercise
// timing-sensitive code paths (e.g. the TSI-2452 race guard) without 2s waits.
var pollInterval = PollInterval

// Overridable sleep for rate-limit resubmission backoff; production value
// waits d or until ctx is done (whichever comes first). Internal tests stub
// it so the 429 retry loop runs instantly instead of waiting out real
// exponential backoff (TSI-2939).
var submitRetrySleep = sleepWithContext

// sleepWithContext waits d, or returns ctx.Err() if ctx is cancelled first.
func sleepWithContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		d = time.Second
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// Client is the HTTP client for rffmpeg server
type Client struct {
	serverURL     string
	token         string
	http          *http.Client
	maxRetries    int
	submitRetries int
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

// WithSubmitRetries sets the retry budget for rate-limit (HTTP 429) job
// submission. 0 (the default) means "no retries" — a rate-limited submission
// fails immediately. A positive n retries the submission up to n times with
// exponential backoff honoring the server's retry_in hint (TSI-2939).
func WithSubmitRetries(n int) ClientOption {
	return func(c *Client) {
		if n >= 0 {
			c.submitRetries = n
		}
	}
}

// ErrRetriesExhausted marks the point where the client's retry budget is spent.
// It is wrapped by RetriesExhaustedError, which carries the submitted job ID.
var ErrRetriesExhausted = errors.New("retries exhausted")

// RetriesExhaustedError reports that a job was submitted successfully but the
// client lost contact with the server after spending its retry budget. It must
// be surfaced as a distinct exit code — never a generic failure — so callers
// can tell "submitted, then disconnected" apart from "submission failed".
type RetriesExhaustedError struct {
	JobID string
	Cause error
}

func (e *RetriesExhaustedError) Error() string {
	return fmt.Sprintf(
		"lost contact with server after retries exhausted; job %s was submitted — query final status via GET /api/v1/jobs/%s (last error: %v)",
		e.JobID, e.JobID, e.Cause,
	)
}

func (e *RetriesExhaustedError) Unwrap() error {
	return e.Cause
}

// RateLimitError reports an HTTP 429 rate-limit rejection from job
// submission. It carries the server's suggested backoff so a caller with a
// retry budget can honor it instead of re-submitting immediately (TSI-2939).
// Error() renders the single-line message promised by the CLI (TSI-2938).
type RateLimitError struct {
	Current int
	Limit   int
	RetryIn int // suggested backoff in seconds; 0 when the server gave none
	// decoded reports whether the body parsed as a structured
	// RateLimitResponse (vs. an undecodable or empty body).
	decoded bool
	// rawBody holds an undecodable response body (diagnostic only).
	rawBody string
}

func (e *RateLimitError) Error() string {
	if e.decoded {
		return fmt.Sprintf(
			"rate limit exceeded: %d/%d concurrent jobs. Retry after %d seconds",
			e.Current, e.Limit, e.RetryIn,
		)
	}
	if e.rawBody != "" {
		return fmt.Sprintf("rate limit exceeded (HTTP 429): %s", e.rawBody)
	}
	return "rate limit exceeded (HTTP 429)"
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
		serverURL:  normalizeServerURL(serverURL),
		token:      token,
		http:       &http.Client{Timeout: DefaultTimeout},
		maxRetries: DefaultMaxRetries,
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
// undecodable body never degrades to a bare "<op> failed: " with no diagnostic
// (TSI-2730).
func parseErrorResponse(body io.Reader) (protocol.ErrorResponse, bool) {
	var errResp protocol.ErrorResponse
	if err := json.NewDecoder(body).Decode(&errResp); err != nil || errResp.Message == "" {
		return protocol.ErrorResponse{}, false
	}
	return errResp, true
}

// UploadFile uploads a file to the server using streaming to avoid loading
// large files entirely into memory.
func (c *Client) UploadFile(filePath string) (string, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return "", fmt.Errorf("failed to open file: %w", err)
	}
	defer file.Close()

	// Use io.Pipe for streaming upload to avoid loading large files into memory
	pr, pw := io.Pipe()
	writer := multipart.NewWriter(pw)

	// Get file info
	fileInfo, err := file.Stat()
	if err != nil {
		return "", fmt.Errorf("failed to get file info: %w", err)
	}

	// Calculate accurate Content-Length
	contentLength := calculateMultipartSize(writer.Boundary(), fileInfo.Size(), filepath.Base(filePath))

	// Write multipart form in a goroutine
	errChan := make(chan error, 1)
	go func() {
		defer pw.Close()
		defer writer.Close()

		part, err := writer.CreateFormFile("file", filepath.Base(filePath))
		if err != nil {
			errChan <- fmt.Errorf("failed to create form file: %w", err)
			return
		}

		if _, err := io.Copy(part, file); err != nil {
			errChan <- fmt.Errorf("failed to copy file: %w", err)
			return
		}
		errChan <- nil
	}()

	req, err := http.NewRequest("POST", c.serverURL+UploadEndpoint, pr)
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.ContentLength = contentLength
	c.setAuthHeader(req)

	// Use longer timeout for large files
	client := &http.Client{
		Timeout: UploadTimeout,
		Transport: &http.Transport{
			// Allow longer idle time for large uploads
			IdleConnTimeout: 5 * time.Minute,
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("upload request failed: %w", err)
	}
	defer DrainAndClose(resp.Body)

	// Check for an auth rejection before the pipe-writer error. The server
	// rejects unauthenticated uploads (fail-closed token check) without
	// draining the streaming body, so the multipart writer races the closed
	// connection and would otherwise surface "failed to copy file: io:
	// read/write on closed pipe", masking the real cause (TSI-2598).
	if resp.StatusCode == http.StatusUnauthorized {
		msg := "missing or invalid token"
		if errResp, ok := parseErrorResponse(resp.Body); ok {
			msg = errResp.Message
		}
		return "", fmt.Errorf("upload failed: authentication rejected (HTTP %d): %s", resp.StatusCode, msg)
	}

	// Check for errors from the goroutine
	if err := <-errChan; err != nil {
		return "", err
	}

	if resp.StatusCode != http.StatusOK {
		if errResp, ok := parseErrorResponse(resp.Body); ok {
			return "", fmt.Errorf("upload failed: %s", errResp.Message)
		}
		return "", fmt.Errorf("upload failed with status %d", resp.StatusCode)
	}

	var uploadResp protocol.UploadResponse
	if err := json.NewDecoder(resp.Body).Decode(&uploadResp); err != nil {
		return "", fmt.Errorf("failed to decode response: %w", err)
	}

	return uploadResp.FileID, nil
}

// calculateMultipartSize calculates the exact size of a multipart form
// with the given boundary, file size, and filename.
func calculateMultipartSize(boundary string, fileSize int64, filename string) int64 {
	// Calculate exact multipart overhead
	// --boundary\r\n
	// Content-Disposition: form-data; name="file"; filename="..."\r\n
	// Content-Type: application/octet-stream\r\n
	// \r\n
	// [file content]
	// \r\n--boundary--\r\n

	preamble := fmt.Sprintf("--%s\r\n", boundary)
	contentDisposition := fmt.Sprintf("Content-Disposition: form-data; name=\"file\"; filename=\"%s\"\r\n", escapeMultipartQuotes(filename))
	contentType := "Content-Type: application/octet-stream\r\n"
	headerEnd := "\r\n"
	epilogue := fmt.Sprintf("\r\n--%s--\r\n", boundary)

	return int64(len(preamble)+len(contentDisposition)+len(contentType)+len(headerEnd)) +
		fileSize +
		int64(len(epilogue))
}

// escapeMultipartQuotes mirrors mime/multipart's internal escapeQuotes: the
// stdlib escapes " and \ inside form-data parameter values. calculateMultipartSize
// must measure the escaped form or a filename containing a quote yields a
// Content-Length that disagrees with the real body and truncates the request
// (TSI-2365).
func escapeMultipartQuotes(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "\"", "\\\"")
	return s
}

// SubmitJob submits a transcoding job.
func (c *Client) SubmitJob(inputFiles []string, args []string, outputFilename string, autoHW bool) (string, error) {
	return c.SubmitJobWithOptions(context.Background(), inputFiles, nil, args, outputFilename, autoHW, false, 0)
}

// SubmitJobWithOptions submits a transcoding job with additional options.
// timeout is the job timeout duration (0 means use server default).
//
// When the client is configured with a submit-retry budget (WithSubmitRetries),
// a rate-limit rejection (HTTP 429) is retried with exponential backoff that
// starts at the server's retry_in hint and doubles per attempt, capped at
// submitRetryMaxDelay. Any other failure — network, auth, validation — fails
// immediately, and once the budget is spent the last rate-limit error is
// returned (TSI-2939).
//
// The backoff wait is interruptible: if ctx is cancelled it stops waiting and
// returns ctx.Err(), so callers can abort a rate-limited submission from a
// signal handler instead of blocking for the full backoff (TSI-2939).
func (c *Client) SubmitJobWithOptions(ctx context.Context, inputFiles []string, directPath []string, args []string, outputFilename string, autoHW bool, streamingOutput bool, timeout time.Duration) (string, error) {
	var lastErr error
	for attempt := 0; attempt <= c.submitRetries; attempt++ {
		jobID, err := c.submitJobOnce(ctx, inputFiles, directPath, args, outputFilename, autoHW, streamingOutput, timeout)
		if err == nil {
			return jobID, nil
		}
		lastErr = err

		var rl *RateLimitError
		if !errors.As(err, &rl) || attempt == c.submitRetries {
			break
		}
		if sleepErr := submitRetrySleep(ctx, rateLimitBackoff(rl.RetryIn, attempt)); sleepErr != nil {
			return "", sleepErr
		}
	}
	return "", lastErr
}

// submitJobOnce performs a single job-submission attempt with no retry logic.
func (c *Client) submitJobOnce(ctx context.Context, inputFiles []string, directPath []string, args []string, outputFilename string, autoHW bool, streamingOutput bool, timeout time.Duration) (string, error) {
	req := protocol.JobSubmitRequest{
		InputFiles:      inputFiles,
		DirectPath:      directPath,
		Args:            args,
		OutputFilename:  outputFilename,
		AutoHW:          autoHW,
		StreamingOutput: streamingOutput,
	}

	// The per-job timeout is an ffmpeg execution budget (a duration), not an
	// absolute wall-clock deadline. The worker anchors it at the ffmpeg
	// execution boundary, so upload/scheduling/probe time is never charged
	// against it (TSI-2684, TSI-2886).
	if timeout > 0 {
		req.Timeout = &timeout
	}

	body, err := json.Marshal(req)
	if err != nil {
		return "", fmt.Errorf("failed to marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", c.serverURL+JobsEndpoint, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	c.setAuthHeader(httpReq)

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("submit job request failed: %w", err)
	}
	defer DrainAndClose(resp.Body)

	if resp.StatusCode != http.StatusOK {
		// Rate limit (429): surface a typed error so a caller with a retry
		// budget can honor the server's backoff hint (TSI-2939).
		if resp.StatusCode == http.StatusTooManyRequests {
			return "", decodeRateLimitError(resp.Body)
		}
		if errResp, ok := parseErrorResponse(resp.Body); ok {
			return "", fmt.Errorf("job submission failed [%s]: %s", errResp.Code, errResp.Message)
		}
		return "", fmt.Errorf("job submission failed with status %d", resp.StatusCode)
	}

	var jobResp protocol.JobSubmitResponse
	if err := json.NewDecoder(resp.Body).Decode(&jobResp); err != nil {
		return "", fmt.Errorf("failed to decode response: %w", err)
	}

	return jobResp.JobID, nil
}

// decodeRateLimitError converts an HTTP 429 response body into a
// *RateLimitError, preserving the pre-existing error text for undecodable or
// empty bodies.
func decodeRateLimitError(body io.Reader) error {
	bodyBytes, readErr := io.ReadAll(body)
	if readErr == nil {
		var rlResp protocol.RateLimitResponse
		if json.Unmarshal(bodyBytes, &rlResp) == nil {
			return &RateLimitError{
				Current: rlResp.Current,
				Limit:   rlResp.Limit,
				RetryIn: rlResp.RetryIn,
				decoded: true,
			}
		}
		// Fallback: preserve the raw body in the diagnostic.
		return &RateLimitError{rawBody: string(bodyBytes)}
	}
	return &RateLimitError{}
}

// rateLimitBackoff computes the wait before the attempt-th retry (0-based),
// starting from the server's retry_in hint (or 1s when absent) and doubling
// per attempt, capped at submitRetryMaxDelay.
func rateLimitBackoff(retryIn, attempt int) time.Duration {
	// retryIn is a server-controlled int with no upper bound. Clamp it before
	// the time.Duration conversion: a huge value would overflow int64 and can
	// wrap to a small positive number that skips both the <=0 guard below and
	// the submitRetryMaxDelay cap (TSI-2939).
	if retryIn > int(submitRetryMaxDelay/time.Second) {
		retryIn = int(submitRetryMaxDelay / time.Second)
	}
	base := time.Duration(retryIn) * time.Second
	if base <= 0 {
		base = time.Second
	}
	delay := base
	for i := 0; i < attempt && delay < submitRetryMaxDelay; i++ {
		delay *= 2
	}
	if delay > submitRetryMaxDelay {
		delay = submitRetryMaxDelay
	}
	return delay
}

// GetJob gets job status
func (c *Client) GetJob(jobID string) (*protocol.JobInfo, error) {
	url := fmt.Sprintf(c.serverURL+JobEndpoint, jobID)

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	c.setAuthHeader(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("get job request failed: %w", err)
	}
	defer DrainAndClose(resp.Body)

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("job not found: %s", jobID)
	}

	if resp.StatusCode != http.StatusOK {
		if errResp, ok := parseErrorResponse(resp.Body); ok {
			return nil, fmt.Errorf("get job failed: %s", errResp.Message)
		}
		return nil, fmt.Errorf("get job failed with status %d", resp.StatusCode)
	}

	var statusResp protocol.JobStatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&statusResp); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return &statusResp.Job, nil
}

// WaitForJob waits for job completion and returns exit code.
// It polls until the job reaches a terminal status or ctx is cancelled.
func (c *Client) WaitForJob(ctx context.Context, jobID string, showProgress bool) (*protocol.JobInfo, error) {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		job, err := c.GetJob(jobID)
		if err != nil {
			return nil, err
		}

		if showProgress {
			fmt.Fprintf(os.Stderr, "Job %s: %s\n", jobID, job.Status)
		}

		if protocol.IsTerminalStatus(job.Status) {
			return job, nil
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

// WaitForJobWithLogs waits for job completion with real-time log streaming via WebSocket
func (c *Client) WaitForJobWithLogs(ctx context.Context, jobID string, quiet bool) (*protocol.JobInfo, error) {
	// Start WebSocket connection for real-time logs
	wsClient := NewWSClient(c.serverURL, jobID, c.token,
		WithOnStderr(func(chunk string) {
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
		WithWSMaxRetries(c.maxRetries),
	)
	defer wsClient.Close()

	// Connect to WebSocket
	if err := wsClient.ConnectWithReconnect(ctx); err != nil {
		// A spent retry budget means the job was submitted but the server is
		// unreachable; surface it with the distinct "submitted" exit code
		// rather than falling back to a poll that will fail the same way.
		if errors.Is(err, ErrRetriesExhausted) {
			return nil, &RetriesExhaustedError{JobID: jobID, Cause: err}
		}
		// If WebSocket fails, fall back to HTTP polling
		fmt.Fprintf(os.Stderr, "Warning: WebSocket connection failed, falling back to HTTP polling: %v\n", err)
		return c.WaitForJob(ctx, jobID, !quiet)
	}

	// Start listening in a goroutine
	listenDone := make(chan error, 1)
	go func() {
		listenDone <- wsClient.Listen(ctx)
	}()

	// Also poll for job completion as a backup. Consecutive GetJob failures
	// are bounded by the same retry budget as the WS reconnect loop, so a
	// server outage cannot wedge the CLI in a silent poll loop (TSI-2697).
	pollDone := make(chan *protocol.JobInfo, 1)
	pollErr := make(chan error, 1)
	budget := retryBudget(c.maxRetries)
	go func() {
		var failStart time.Time
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(pollInterval):
				job, err := c.GetJob(jobID)
				if err != nil {
					if failStart.IsZero() {
						failStart = time.Now()
					}
					if time.Since(failStart) >= budget {
						pollErr <- &RetriesExhaustedError{JobID: jobID, Cause: fmt.Errorf("job status polling lost contact with server for %s: %w", time.Since(failStart).Round(time.Second), err)}
						return
					}
					continue
				}
				failStart = time.Time{}
				if protocol.IsTerminalStatus(job.Status) {
					pollDone <- job
					return
				}
			}
		}
	}()

	// Wait for completion, error, or context cancellation. In non-streaming
	// mode (file output), stderr is display-only: a detected sequence gap
	// means some log lines were lost across a reconnect, but the transcoded
	// file itself is intact — surface it as a warning and still return the
	// job so main.go proceeds to GET /api/v1/output/{fileId}. Only streaming
	// output (stdout consumers) must fail on a gap.
	select {
	case job := <-pollDone:
		if wsClient.HasGap() {
			fmt.Fprintln(os.Stderr, "Warning: log stream incomplete: sequence gap detected (log lines lost during reconnect); output file is unaffected")
		}
		return job, nil
	case err := <-pollErr:
		// TSI-2452-style race guard: the backup poll's budget may expire at
		// the same moment the job actually reached a terminal status (WS
		// healthy, only the backup poll flapped). One final GetJob with a
		// fresh context distinguishes "job done" from "contact lost" before
		// the distinct exit 2 is surfaced.
		if job, getErr := c.GetJob(jobID); getErr == nil && protocol.IsTerminalStatus(job.Status) {
			if wsClient.HasGap() {
				fmt.Fprintln(os.Stderr, "Warning: log stream incomplete: sequence gap detected (log lines lost during reconnect); output file is unaffected")
			}
			return job, nil
		}
		return nil, err
	case err := <-listenDone:
		if err != nil {
			// A spent retry budget means "submitted, then disconnected":
			// surface the distinct exit code instead of a poll fallback.
			if errors.Is(err, ErrRetriesExhausted) {
				return nil, &RetriesExhaustedError{JobID: jobID, Cause: err}
			}
			// WebSocket failed, fall back to polling
			return c.WaitForJob(ctx, jobID, !quiet)
		}
		// WebSocket closed normally; poll until terminal status is reached.
		// A single GetJob call may return a non-terminal status if the
		// WebSocket closes before the server DB is updated.
		if _, err := c.WaitForJob(ctx, jobID, !quiet); err != nil {
			return nil, err
		}
		job, err := c.GetJob(jobID)
		if err != nil {
			return nil, err
		}
		// Non-streaming mode: a log-stream gap must not block the output
		// file download — stderr here is display-only. Warn and succeed.
		if wsClient.HasGap() {
			fmt.Fprintln(os.Stderr, "Warning: log stream incomplete: sequence gap detected (log lines lost during reconnect); output file is unaffected")
		}
		return job, nil
	case <-ctx.Done():
		// Race guard (TSI-2452): the client-side timeout fired, but the
		// job may have reached a terminal status on the server between
		// the last poll and ctx.Done() (e.g. a cache hit). The poll
		// goroutine may still be mid-GetJob, so pollDone is not yet
		// written. Do a final GetJob with a fresh context — if the job
		// is already done, return it instead of a spurious timeout.
		// main.go also does this as a belt-and-suspenders fallback;
		// doing it here means main.go's check is a no-op in the common
		// case and the gap warning is preserved.
		if job, getErr := c.GetJob(jobID); getErr == nil && protocol.IsTerminalStatus(job.Status) {
			if wsClient.HasGap() {
				fmt.Fprintln(os.Stderr, "Warning: log stream incomplete: sequence gap detected (log lines lost during reconnect); output file is unaffected")
			}
			return job, nil
		}
		return nil, ctx.Err()
	}
}

// WaitForJobWithStreamingOutput waits for job completion with streaming output to stdout.
// This is used when the output file is "-" to stream transcoded data directly to stdout.
func (c *Client) WaitForJobWithStreamingOutput(ctx context.Context, jobID string, quiet bool) (*protocol.JobInfo, error) {
	// stdoutBytes counts bytes actually written to stdout (partial writes and
	// failures excluded); stdoutErr records the first write failure. A completed
	// streaming job that delivered zero bytes — or whose stdout write failed —
	// must surface as a failure, never a silent rc=0 (TSI-2905).
	var stdoutBytes atomic.Int64
	var stdoutMu sync.Mutex
	var stdoutErr error

	// terminalSeen is closed once the WebSocket delivers the terminal event
	// (complete or a terminal status), which the hub sequences after every
	// stdout frame on the same channel — so it also means "stdout fully
	// delivered".
	var terminalOnce sync.Once
	terminalSeen := make(chan struct{})
	markTerminal := func() { terminalOnce.Do(func() { close(terminalSeen) }) }

	// Start WebSocket connection for real-time stdout streaming
	wsClient := NewWSClient(c.serverURL, jobID, c.token,
		WithOnStdout(func(chunk []byte) {
			n, err := os.Stdout.Write(chunk)
			stdoutBytes.Add(int64(n))
			if err != nil {
				stdoutMu.Lock()
				if stdoutErr == nil {
					stdoutErr = err
				}
				stdoutMu.Unlock()
			}
		}),
		WithOnStderr(func(chunk string) {
			// Write stderr to stderr (progress info, etc.)
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
			if protocol.IsTerminalStatus(status) {
				markTerminal()
			}
		}),
		WithOnProgress(func(p protocol.WSProgressPayload) {
			if quiet {
				return
			}
			fmt.Fprintln(os.Stderr, renderProgressLine(p))
		}),
		WithOnComplete(func(exitCode int) { markTerminal() }),
		WithWSMaxRetries(c.maxRetries),
	)
	defer wsClient.Close()

	// integrityErr reports whether the streamed result is trustworthy: a
	// sequence gap, a stdout write failure, or a completed job with zero
	// delivered bytes. Shared by the connected path and every polling fallback
	// so no completion path can bypass it (TSI-2905).
	integrityErr := func(job *protocol.JobInfo) error {
		if wsClient.HasGap() {
			return fmt.Errorf("streaming output incomplete: sequence gap detected (data lost during reconnect)")
		}
		stdoutMu.Lock()
		we := stdoutErr
		stdoutMu.Unlock()
		if we != nil {
			return fmt.Errorf("streaming output write failed: %w", we)
		}
		if job != nil && job.Status == protocol.JobStatusCompleted && stdoutBytes.Load() == 0 {
			return fmt.Errorf("streaming output empty: job completed but no output bytes were received")
		}
		return nil
	}

	// fallbackToPolling waits via HTTP polling (no WS streaming) and applies
	// the same integrity checks, so a WebSocket failure can never surface as a
	// clean rc=0 for a job whose streamed output never reached stdout.
	fallbackToPolling := func() (*protocol.JobInfo, error) {
		job, err := c.WaitForJob(ctx, jobID, !quiet)
		if err != nil {
			return job, err
		}
		if e := integrityErr(job); e != nil {
			return job, e
		}
		return job, nil
	}

	// Connect to WebSocket
	if err := wsClient.ConnectWithReconnect(ctx); err != nil {
		// A spent retry budget means the job was submitted but the server is
		// unreachable; surface the distinct "submitted" exit code rather than
		// falling back to a poll that will fail the same way.
		if errors.Is(err, ErrRetriesExhausted) {
			return nil, &RetriesExhaustedError{JobID: jobID, Cause: err}
		}
		// If WebSocket fails, fall back to HTTP polling (without streaming output)
		fmt.Fprintf(os.Stderr, "Warning: WebSocket connection failed, falling back to HTTP polling: %v\n", err)
		return fallbackToPolling()
	}

	// Start listening in a goroutine
	listenDone := make(chan error, 1)
	go func() {
		listenDone <- wsClient.Listen(ctx)
	}()

	// Poll for job completion as a backup, bounded by the same retry budget
	// as the WS reconnect loop (TSI-2697).
	pollDone := make(chan *protocol.JobInfo, 1)
	pollErr := make(chan error, 1)
	budget := retryBudget(c.maxRetries)
	go func() {
		var failStart time.Time
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(pollInterval):
				job, err := c.GetJob(jobID)
				if err != nil {
					if failStart.IsZero() {
						failStart = time.Now()
					}
					if time.Since(failStart) >= budget {
						pollErr <- &RetriesExhaustedError{JobID: jobID, Cause: fmt.Errorf("job status polling lost contact with server for %s: %w", time.Since(failStart).Round(time.Second), err)}
						return
					}
					continue
				}
				failStart = time.Time{}
				if protocol.IsTerminalStatus(job.Status) {
					pollDone <- job
					return
				}
			}
		}
	}()

	// Wait for completion, error, or context cancellation
	var finalJob *protocol.JobInfo
	select {
	case job := <-pollDone:
		finalJob = job
		// Drain: the HTTP poll may observe the terminal DB status a moment
		// before the WebSocket has read the final sequenced stdout frames. Wait
		// (bounded) for the terminal WS event so integrityErr runs against the
		// fully-delivered stream instead of a transient 0-byte state.
		drainStreamingOutput(ctx, terminalSeen, listenDone)
	case err := <-pollErr:
		// TSI-2452-style race guard (see WaitForJobWithLogs): a terminal
		// status reached at the same moment the backup poll budget expired
		// must not be reported as exit 2.
		if job, getErr := c.GetJob(jobID); getErr == nil && protocol.IsTerminalStatus(job.Status) {
			finalJob = job
		} else {
			return nil, err
		}
	case err := <-listenDone:
		if err != nil {
			// A spent retry budget means "submitted, then disconnected":
			// surface the distinct exit code instead of a poll fallback.
			if errors.Is(err, ErrRetriesExhausted) {
				return nil, &RetriesExhaustedError{JobID: jobID, Cause: err}
			}
			// WebSocket failed, fall back to polling (integrity checked).
			return fallbackToPolling()
		}
		// WebSocket closed normally; poll until terminal status is reached,
		// then apply the same integrity checks. A single GetJob call may return
		// a non-terminal status if the WebSocket closes before the server DB is
		// updated.
		return fallbackToPolling()
	case <-ctx.Done():
		// Race guard (TSI-2452): the client-side timeout fired, but the
		// job may have reached a terminal status on the server between
		// the last poll and ctx.Done() (e.g. a cache hit). The poll
		// goroutine may still be mid-GetJob, so pollDone is not yet
		// written. Do a final GetJob with a fresh context — if the job
		// is already done, fall through to the integrity check below and
		// return it; otherwise report the timeout/integrity error.
		if job, getErr := c.GetJob(jobID); getErr == nil && protocol.IsTerminalStatus(job.Status) {
			finalJob = job
		} else {
			if e := integrityErr(nil); e != nil {
				return nil, e
			}
			return nil, ctx.Err()
		}
	}

	if e := integrityErr(finalJob); e != nil {
		return finalJob, e
	}
	return finalJob, nil
}

// drainStreamingOutput waits (bounded) for the WebSocket to deliver its
// terminal event, which is sequenced after every stdout frame on the same
// channel. It lets the streaming wait confirm the full stream was read before
// running the integrity checks (TSI-2905).
func drainStreamingOutput(ctx context.Context, terminalSeen <-chan struct{}, listenDone <-chan error) {
	timer := time.NewTimer(StreamingDrainTimeout)
	defer timer.Stop()
	select {
	case <-terminalSeen:
	case <-listenDone:
	case <-ctx.Done():
	case <-timer.C:
	}
}

// DownloadOutput downloads an output file
func (c *Client) DownloadOutput(fileID, outputPath string) error {
	url := fmt.Sprintf(c.serverURL+OutputEndpoint, fileID)

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	c.setAuthHeader(req)

	client := &http.Client{Timeout: DownloadTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("download request failed: %w", err)
	}
	defer DrainAndClose(resp.Body)

	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("output file not found: %s", fileID)
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download failed with status %d", resp.StatusCode)
	}

	// Create output directory if needed
	dir := filepath.Dir(outputPath)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("failed to create output directory: %w", err)
		}
	}

	file, err := os.Create(outputPath)
	if err != nil {
		return fmt.Errorf("failed to create output file: %w", err)
	}
	defer file.Close()

	if _, err := io.Copy(file, resp.Body); err != nil {
		return fmt.Errorf("failed to write output file: %w", err)
	}

	return nil
}

// HealthCheck checks server health
func (c *Client) HealthCheck() error {
	req, err := http.NewRequest("GET", c.serverURL+HealthEndpoint, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	c.setAuthHeader(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("health check failed: %w", err)
	}
	defer DrainAndClose(resp.Body)

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("server unhealthy: status %d", resp.StatusCode)
	}

	return nil
}

// Probe sends a probe request to the server and returns media information.
func (c *Client) Probe(input string) (*protocol.ProbeResponse, error) {
	reqBody := protocol.ProbeRequest{Input: input}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal probe request: %w", err)
	}

	httpReq, err := http.NewRequest("POST", c.serverURL+ProbeEndpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create probe request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	c.setAuthHeader(httpReq)

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("probe request failed: %w", err)
	}
	defer DrainAndClose(resp.Body)

	if resp.StatusCode != http.StatusOK {
		var probeResp protocol.ProbeResponse
		if decodeErr := json.NewDecoder(resp.Body).Decode(&probeResp); decodeErr == nil && probeResp.Message != "" {
			return nil, fmt.Errorf("probe failed: %s", probeResp.Message)
		}
		return nil, fmt.Errorf("probe failed with status %d", resp.StatusCode)
	}

	var probeResp protocol.ProbeResponse
	if err := json.NewDecoder(resp.Body).Decode(&probeResp); err != nil {
		return nil, fmt.Errorf("failed to decode probe response: %w", err)
	}

	return &probeResp, nil
}

// CancelJob cancels a job
func (c *Client) CancelJob(jobID string) error {
	url := fmt.Sprintf(c.serverURL+JobEndpoint, jobID)

	req, err := http.NewRequest("DELETE", url, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	c.setAuthHeader(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("cancel request failed: %w", err)
	}
	defer DrainAndClose(resp.Body)

	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("job not found: %s", jobID)
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("cancel failed with status %d", resp.StatusCode)
	}

	return nil
}

// setAuthHeader sets the authorization header if token is present
func (c *Client) setAuthHeader(req *http.Request) {
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
}

// WorkerHealth carries live runtime metrics for a worker (TSI-2219).
type WorkerHealth struct {
	Status        string   `json:"status"`
	GPUUtilPct    float64  `json:"gpu_util_percent,omitempty"`
	GPUMemUsedMB  int      `json:"gpu_mem_used_mb,omitempty"`
	ActiveJobs    []string `json:"active_jobs,omitempty"`
	ThroughputFPS float64  `json:"throughput_fps,omitempty"`
	LastSeen      string   `json:"last_seen"`
}

// WorkerInfo represents worker information from the API
type WorkerInfo struct {
	ID            string        `json:"id"`
	Name          string        `json:"name,omitempty"`
	Status        string        `json:"status"`
	GPUModel      string        `json:"gpu_model,omitempty"`
	Encoders      []string      `json:"encoders"`
	Decoders      []string      `json:"decoders,omitempty"`
	FFmpegVersion string        `json:"ffmpeg_version"`
	MaxConcurrent int           `json:"max_concurrent"`
	LastHeartbeat string        `json:"last_heartbeat"`
	CreatedAt     string        `json:"created_at"`
	Hwaccels      string        `json:"hwaccels,omitempty"`
	Codecs        string        `json:"codecs,omitempty"`
	Filters       string        `json:"filters,omitempty"`
	PixFmts       string        `json:"pix_fmts,omitempty"`
	Formats       string        `json:"formats,omitempty"`
	Health        *WorkerHealth `json:"health"`
}

// ListWorkersResponse is the response for listing workers
type ListWorkersResponse struct {
	Workers []WorkerInfo `json:"workers"`
}

// ListEncodersResponse is the response for listing encoders
type ListEncodersResponse struct {
	Encoders []protocol.EncoderInfo `json:"encoders"`
}

// ListDecodersResponse is the response for listing decoders
type ListDecodersResponse struct {
	Decoders []protocol.DecoderInfo `json:"decoders"`
}

// ListHwaccelsResponse is the response for listing hwaccels
type ListHwaccelsResponse struct {
	Hwaccels []string `json:"hwaccels"`
}

// ListWorkersByEncoderResponse is the response for listing workers by encoder
type ListWorkersByEncoderResponse struct {
	Encoder string       `json:"encoder"`
	Workers []WorkerInfo `json:"workers"`
}

// ListWorkers retrieves all workers from the server
func (c *Client) ListWorkers() ([]WorkerInfo, error) {
	url := c.serverURL + "/api/v1/workers"

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	c.setAuthHeader(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("list workers request failed: %w", err)
	}
	defer DrainAndClose(resp.Body)

	if resp.StatusCode != http.StatusOK {
		if errResp, ok := parseErrorResponse(resp.Body); ok {
			return nil, fmt.Errorf("list workers failed: %s", errResp.Message)
		}
		return nil, fmt.Errorf("list workers failed with status %d", resp.StatusCode)
	}

	var listResp ListWorkersResponse
	if err := json.NewDecoder(resp.Body).Decode(&listResp); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return listResp.Workers, nil
}

// GetWorker retrieves a single worker by ID
func (c *Client) GetWorker(workerID string) (*WorkerInfo, error) {
	url := c.serverURL + "/api/v1/workers/" + workerID

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	c.setAuthHeader(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("get worker request failed: %w", err)
	}
	defer DrainAndClose(resp.Body)

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("worker not found: %s", workerID)
	}

	if resp.StatusCode != http.StatusOK {
		if errResp, ok := parseErrorResponse(resp.Body); ok {
			return nil, fmt.Errorf("get worker failed: %s", errResp.Message)
		}
		return nil, fmt.Errorf("get worker failed with status %d", resp.StatusCode)
	}

	var workerResp struct {
		Worker WorkerInfo `json:"worker"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&workerResp); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return &workerResp.Worker, nil
}

// ListAllEncoders retrieves all unique encoders across all workers
func (c *Client) ListAllEncoders() ([]protocol.EncoderInfo, error) {
	url := c.serverURL + "/api/v1/encoders"

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	c.setAuthHeader(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("list encoders request failed: %w", err)
	}
	defer DrainAndClose(resp.Body)

	if resp.StatusCode != http.StatusOK {
		if errResp, ok := parseErrorResponse(resp.Body); ok {
			return nil, fmt.Errorf("list encoders failed: %s", errResp.Message)
		}
		return nil, fmt.Errorf("list encoders failed with status %d", resp.StatusCode)
	}

	var listResp ListEncodersResponse
	if err := json.NewDecoder(resp.Body).Decode(&listResp); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return listResp.Encoders, nil
}

// ListAllDecoders retrieves all unique decoders across all workers
func (c *Client) ListAllDecoders() ([]protocol.DecoderInfo, error) {
	url := c.serverURL + "/api/v1/decoders"

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	c.setAuthHeader(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("list decoders request failed: %w", err)
	}
	defer DrainAndClose(resp.Body)

	if resp.StatusCode != http.StatusOK {
		if errResp, ok := parseErrorResponse(resp.Body); ok {
			return nil, fmt.Errorf("list decoders failed: %s", errResp.Message)
		}
		return nil, fmt.Errorf("list decoders failed with status %d", resp.StatusCode)
	}

	var listResp ListDecodersResponse
	if err := json.NewDecoder(resp.Body).Decode(&listResp); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return listResp.Decoders, nil
}

// ListAllHwaccels retrieves all unique hwaccels across all workers.
// The API returns text/plain by default; pass json=true for JSON.
func (c *Client) ListAllHwaccels() ([]string, error) {
	url := c.serverURL + "/api/v1/hwaccels"

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	c.setAuthHeader(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("list hwaccels request failed: %w", err)
	}
	defer DrainAndClose(resp.Body)

	if resp.StatusCode != http.StatusOK {
		if errResp, ok := parseErrorResponse(resp.Body); ok {
			return nil, fmt.Errorf("list hwaccels failed: %s", errResp.Message)
		}
		return nil, fmt.Errorf("list hwaccels failed with status %d", resp.StatusCode)
	}

	// Read the response body (text/plain, one hwaccel per line)
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read hwaccels response: %w", err)
	}

	lines := strings.Split(strings.TrimSpace(string(body)), "\n")
	result := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" {
			result = append(result, line)
		}
	}
	return result, nil
}

// ListAllFilters retrieves all unique filters across all workers.
// The API returns text/plain by default; pass json=true for JSON.
func (c *Client) ListAllFilters(jsonOut bool) ([]string, error) {
	url := c.serverURL + "/api/v1/filters"
	if jsonOut {
		url += "?json=1"
	}

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	c.setAuthHeader(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("list filters request failed: %w", err)
	}
	defer DrainAndClose(resp.Body)

	if resp.StatusCode != http.StatusOK {
		if errResp, ok := parseErrorResponse(resp.Body); ok {
			return nil, fmt.Errorf("list filters failed: %s", errResp.Message)
		}
		return nil, fmt.Errorf("list filters failed with status %d", resp.StatusCode)
	}

	if jsonOut {
		var listResp struct {
			Items []string `json:"items"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&listResp); err != nil {
			return nil, fmt.Errorf("failed to decode response: %w", err)
		}
		return listResp.Items, nil
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read filters response: %w", err)
	}

	lines := strings.Split(strings.TrimSpace(string(body)), "\n")
	result := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || line == "Filters:" {
			continue
		}
		result = append(result, line)
	}
	return result, nil
}

// ListAllPixFmts retrieves all unique pixel formats across all workers.
// The API returns text/plain by default; pass json=true for JSON.
func (c *Client) ListAllPixFmts(jsonOut bool) ([]string, error) {
	url := c.serverURL + "/api/v1/pix_fmts"
	if jsonOut {
		url += "?json=1"
	}

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	c.setAuthHeader(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("list pix_fmts request failed: %w", err)
	}
	defer DrainAndClose(resp.Body)

	if resp.StatusCode != http.StatusOK {
		if errResp, ok := parseErrorResponse(resp.Body); ok {
			return nil, fmt.Errorf("list pix_fmts failed: %s", errResp.Message)
		}
		return nil, fmt.Errorf("list pix_fmts failed with status %d", resp.StatusCode)
	}

	if jsonOut {
		var listResp struct {
			Items []string `json:"items"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&listResp); err != nil {
			return nil, fmt.Errorf("failed to decode response: %w", err)
		}
		return listResp.Items, nil
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read pix_fmts response: %w", err)
	}

	lines := strings.Split(strings.TrimSpace(string(body)), "\n")
	result := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || line == "Pixel formats:" {
			continue
		}
		result = append(result, line)
	}
	return result, nil
}

// ListAllFormats retrieves all unique formats across all workers.
// The API returns text/plain by default; pass json=true for JSON.
func (c *Client) ListAllFormats(jsonOut bool) ([]string, error) {
	url := c.serverURL + "/api/v1/formats"
	if jsonOut {
		url += "?json=1"
	}

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	c.setAuthHeader(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("list formats request failed: %w", err)
	}
	defer DrainAndClose(resp.Body)

	if resp.StatusCode != http.StatusOK {
		if errResp, ok := parseErrorResponse(resp.Body); ok {
			return nil, fmt.Errorf("list formats failed: %s", errResp.Message)
		}
		return nil, fmt.Errorf("list formats failed with status %d", resp.StatusCode)
	}

	if jsonOut {
		var listResp struct {
			Items []string `json:"items"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&listResp); err != nil {
			return nil, fmt.Errorf("failed to decode response: %w", err)
		}
		return listResp.Items, nil
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read formats response: %w", err)
	}

	lines := strings.Split(strings.TrimSpace(string(body)), "\n")
	result := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || line == "File formats:" {
			continue
		}
		result = append(result, line)
	}
	return result, nil
}

// ListWorkersByEncoder retrieves workers that have a specific encoder
func (c *Client) ListWorkersByEncoder(encoderName string) ([]WorkerInfo, error) {
	url := c.serverURL + "/api/v1/encoders/" + encoderName + "/workers"

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	c.setAuthHeader(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("list workers by encoder request failed: %w", err)
	}
	defer DrainAndClose(resp.Body)

	if resp.StatusCode != http.StatusOK {
		if errResp, ok := parseErrorResponse(resp.Body); ok {
			return nil, fmt.Errorf("list workers by encoder failed: %s", errResp.Message)
		}
		return nil, fmt.Errorf("list workers by encoder failed with status %d", resp.StatusCode)
	}

	var listResp ListWorkersByEncoderResponse
	if err := json.NewDecoder(resp.Body).Decode(&listResp); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return listResp.Workers, nil
}

// Chunked upload constants
const (
	// DefaultChunkSize is the default chunk size for chunked uploads (10MB)
	DefaultChunkSize = 10 * 1024 * 1024
	// LargeFileThreshold is the threshold above which chunked upload is used (100MB)
	LargeFileThreshold = 100 * 1024 * 1024
	// MaxChunkRetries is the maximum number of retries for chunk upload
	MaxChunkRetries = 3
	// InitialRetryDelay is the initial delay for retry
	InitialRetryDelay = 1 * time.Second
	// MaxRetryDelay is the maximum delay for retry
	MaxRetryDelay = 30 * time.Second
)

// UploadFileChunked uploads a large file using the chunked upload API.
// This is more reliable for very large files (>100MB) as it uploads in smaller chunks.
func (c *Client) UploadFileChunked(filePath string, chunkSize int64) (string, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return "", fmt.Errorf("failed to open file: %w", err)
	}
	defer file.Close()

	fileInfo, err := file.Stat()
	if err != nil {
		return "", fmt.Errorf("failed to get file info: %w", err)
	}

	if chunkSize <= 0 {
		chunkSize = DefaultChunkSize
	}

	// Calculate file checksum for integrity verification
	fileHash := sha256.New()
	if _, err := io.Copy(fileHash, file); err != nil {
		return "", fmt.Errorf("failed to calculate file checksum: %w", err)
	}
	fileChecksum := hex.EncodeToString(fileHash.Sum(nil))

	// Reset file position after reading
	if _, err := file.Seek(0, 0); err != nil {
		return "", fmt.Errorf("failed to reset file position: %w", err)
	}

	// Initialize chunked upload session
	initReq := protocol.ChunkUploadInitRequest{
		Filename:  filepath.Base(filePath),
		FileSize:  fileInfo.Size(),
		ChunkSize: chunkSize,
		Checksum:  fileChecksum, // File integrity checksum
	}

	initBody, err := json.Marshal(initReq)
	if err != nil {
		return "", fmt.Errorf("failed to marshal init request: %w", err)
	}

	req, err := http.NewRequest("POST", c.serverURL+"/api/v1/upload/init", bytes.NewReader(initBody))
	if err != nil {
		return "", fmt.Errorf("failed to create init request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	c.setAuthHeader(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("init request failed: %w", err)
	}
	defer DrainAndClose(resp.Body)

	// Surface auth rejections with the status code first, matching UploadFile
	// and uploadSingleChunk (TSI-2598 / TSI-2618). The init endpoint authenticates
	// before any chunk is uploaded, so a >100MB file with a bad token fails here
	// and never reaches the chunk endpoint (TSI-2625).
	if resp.StatusCode == http.StatusUnauthorized {
		msg := "missing or invalid token"
		if errResp, ok := parseErrorResponse(resp.Body); ok {
			msg = errResp.Message
		}
		return "", fmt.Errorf("init failed: authentication rejected (HTTP %d): %s", resp.StatusCode, msg)
	}

	if resp.StatusCode != http.StatusOK {
		if errResp, ok := parseErrorResponse(resp.Body); ok {
			return "", fmt.Errorf("init failed: %s", errResp.Message)
		}
		return "", fmt.Errorf("init failed with status %d", resp.StatusCode)
	}

	var initResp protocol.ChunkUploadInitResponse
	if err := json.NewDecoder(resp.Body).Decode(&initResp); err != nil {
		return "", fmt.Errorf("failed to decode init response: %w", err)
	}

	// Upload chunks
	uploadID := initResp.UploadID
	totalChunks := initResp.TotalChunks
	actualChunkSize := initResp.ChunkSize

	for i := 0; i < totalChunks; i++ {
		// Upload chunk with retry
		if err := c.uploadChunkWithRetry(file, uploadID, i, actualChunkSize, fileInfo.Size(), totalChunks); err != nil {
			return "", fmt.Errorf("chunk %d upload failed after retries: %w", i, err)
		}
	}

	// Complete upload
	completeReq := protocol.ChunkUploadCompleteRequest{
		UploadID: uploadID,
		Checksum: fileChecksum, // Final verification checksum
	}
	completeBody, err := json.Marshal(completeReq)
	if err != nil {
		return "", fmt.Errorf("failed to marshal complete request: %w", err)
	}

	req, err = http.NewRequest("POST", c.serverURL+"/api/v1/upload/complete", bytes.NewReader(completeBody))
	if err != nil {
		return "", fmt.Errorf("failed to create complete request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	c.setAuthHeader(req)

	resp, err = c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("complete request failed: %w", err)
	}
	defer DrainAndClose(resp.Body)

	if resp.StatusCode != http.StatusOK {
		if errResp, ok := parseErrorResponse(resp.Body); ok {
			return "", fmt.Errorf("complete failed: %s", errResp.Message)
		}
		return "", fmt.Errorf("complete failed with status %d", resp.StatusCode)
	}

	var completeResp protocol.ChunkUploadCompleteResponse
	if err := json.NewDecoder(resp.Body).Decode(&completeResp); err != nil {
		return "", fmt.Errorf("failed to decode complete response: %w", err)
	}

	return completeResp.FileID, nil
}

// uploadChunkWithRetry uploads a single chunk with exponential backoff retry
func (c *Client) uploadChunkWithRetry(file *os.File, uploadID string, chunkIndex int, chunkSize int64, fileSize int64, totalChunks int) error {
	var lastErr error

	for attempt := 0; attempt < MaxChunkRetries; attempt++ {
		if attempt > 0 {
			// Exponential backoff
			delay := InitialRetryDelay * time.Duration(1<<(attempt-1))
			if delay > MaxRetryDelay {
				delay = MaxRetryDelay
			}
			time.Sleep(delay)
		}

		err := c.uploadSingleChunk(file, uploadID, chunkIndex, chunkSize, fileSize, totalChunks)
		if err == nil {
			return nil // Success
		}
		lastErr = err

		// Check if error is retryable
		if !isRetryableError(err) {
			return err
		}
	}

	return lastErr
}

// isRetryableError determines if an error is worth retrying.
func isRetryableError(err error) bool {
	if err == nil {
		return false
	}
	// Network timeouts are retryable. Detect them via the net.Error interface
	// rather than substring matching: http.Client.Timeout surfaces as
	// "context deadline exceeded (Client.Timeout exceeded ...)" — its
	// capitalized "Client.Timeout" evades a lowercase "timeout" check, so the
	// chunk retry path silently never retried the most common client timeout
	// (TSI-2919 CI flake).
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	// Network errors, transient failures, and 5xx statuses are retryable.
	errStr := err.Error()
	return strings.Contains(errStr, "timeout") ||
		strings.Contains(errStr, "connection reset") ||
		strings.Contains(errStr, "temporary") ||
		strings.Contains(errStr, "unavailable") ||
		strings.Contains(errStr, "5xx") ||
		strings.Contains(errStr, "502") ||
		strings.Contains(errStr, "503") ||
		strings.Contains(errStr, "504")
}

// uploadSingleChunk uploads a single chunk without retry logic
func (c *Client) uploadSingleChunk(file *os.File, uploadID string, chunkIndex int, chunkSize int64, fileSize int64, totalChunks int) error {
	// Seek to chunk position
	offset := int64(chunkIndex) * chunkSize
	if _, err := file.Seek(offset, 0); err != nil {
		return fmt.Errorf("failed to seek to chunk %d: %w", chunkIndex, err)
	}

	// Calculate chunk size for this chunk (last chunk may be smaller)
	currentChunkSize := chunkSize
	if offset+chunkSize > fileSize {
		currentChunkSize = fileSize - offset
	}

	// Create a hash for checksum calculation
	chunkHash := sha256.New()

	// Create a tee reader to read chunk data and compute checksum simultaneously
	limitedReader := io.LimitReader(file, currentChunkSize)
	teeReader := io.TeeReader(limitedReader, chunkHash)

	// Read chunk data into buffer for checksum and upload
	chunkData, err := io.ReadAll(teeReader)
	if err != nil {
		return fmt.Errorf("failed to read chunk %d: %w", chunkIndex, err)
	}

	// Calculate chunk checksum
	chunkChecksum := hex.EncodeToString(chunkHash.Sum(nil))

	// Upload chunk using streaming
	pr, pw := io.Pipe()
	writer := multipart.NewWriter(pw)

	// Calculate accurate Content-Length
	contentLength := calculateChunkMultipartSize(writer.Boundary(), currentChunkSize, chunkChecksum)

	errChan := make(chan error, 1)
	go func() {
		defer pw.Close()
		defer writer.Close()

		part, err := writer.CreateFormFile("chunk", "chunk")
		if err != nil {
			errChan <- err
			return
		}

		if _, err := part.Write(chunkData); err != nil {
			errChan <- err
			return
		}

		// Add checksum as form field
		_ = writer.WriteField("checksum", chunkChecksum)

		errChan <- nil
	}()

	chunkURL := fmt.Sprintf("%s/api/v1/upload/chunk/%s/%d", c.serverURL, uploadID, chunkIndex)
	req, err := http.NewRequest("POST", chunkURL, pr)
	if err != nil {
		return fmt.Errorf("failed to create chunk request: %w", err)
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.ContentLength = contentLength
	c.setAuthHeader(req)

	// Dynamic timeout based on chunk size
	timeout := calculateChunkTimeout(currentChunkSize)
	client := &http.Client{Timeout: timeout}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("chunk request failed: %w", err)
	}
	defer DrainAndClose(resp.Body)

	// Check for an auth rejection before the pipe-writer error. The server
	// rejects unauthenticated chunk uploads (fail-closed token check) without
	// draining the streaming body, so the multipart writer races the closed
	// connection and would otherwise surface "chunk write failed: io:
	// read/write on closed pipe", masking the real cause (TSI-2598).
	if resp.StatusCode == http.StatusUnauthorized {
		msg := "missing or invalid token"
		if errResp, ok := parseErrorResponse(resp.Body); ok {
			msg = errResp.Message
		}
		return fmt.Errorf("chunk upload failed: authentication rejected (HTTP %d): %s", resp.StatusCode, msg)
	}

	if err := <-errChan; err != nil {
		return fmt.Errorf("chunk write failed: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		if errResp, ok := parseErrorResponse(resp.Body); ok {
			return fmt.Errorf("chunk upload failed: %s", errResp.Message)
		}
		return fmt.Errorf("chunk upload failed with status %d", resp.StatusCode)
	}

	return nil
}

// calculateChunkMultipartSize calculates the exact size of a chunk multipart form
func calculateChunkMultipartSize(boundary string, chunkSize int64, checksum string) int64 {
	// Calculate exact multipart overhead for chunk
	// --boundary\r\n
	// Content-Disposition: form-data; name="chunk"; filename="chunk"\r\n
	// Content-Type: application/octet-stream\r\n
	// \r\n
	// [chunk content]
	// \r\n--boundary\r\n
	// Content-Disposition: form-data; name="checksum"\r\n
	// \r\n
	// [checksum]
	// \r\n--boundary--\r\n

	preamble := fmt.Sprintf("--%s\r\n", boundary)
	contentDisposition := "Content-Disposition: form-data; name=\"chunk\"; filename=\"chunk\"\r\n"
	contentType := "Content-Type: application/octet-stream\r\n"
	headerEnd := "\r\n"

	checksumHeader := fmt.Sprintf("\r\n--%s\r\nContent-Disposition: form-data; name=\"checksum\"\r\n\r\n", boundary)
	checksumValue := checksum
	epilogue := fmt.Sprintf("\r\n--%s--\r\n", boundary)

	return int64(len(preamble)+len(contentDisposition)+len(contentType)+len(headerEnd)) +
		chunkSize +
		int64(len(checksumHeader)+len(checksumValue)+len(epilogue))
}

// calculateChunkTimeout calculates timeout based on chunk size
func calculateChunkTimeout(chunkSize int64) time.Duration {
	// Base timeout: 30 seconds for 10MB
	// Scale linearly: 3 seconds per MB
	baseTimeout := 30 * time.Second
	mbPerSec := int64(3 * 1024 * 1024) // 3MB/s

	timeout := baseTimeout + time.Duration(chunkSize/mbPerSec)*time.Second
	if timeout > UploadTimeout {
		timeout = UploadTimeout
	}
	return timeout
}

// UploadFileAuto automatically chooses between simple and chunked upload based on file size.
// Files larger than LargeFileThreshold (100MB) use chunked upload.
func (c *Client) UploadFileAuto(filePath string) (string, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return "", fmt.Errorf("failed to open file: %w", err)
	}
	defer file.Close()

	fileInfo, err := file.Stat()
	if err != nil {
		return "", fmt.Errorf("failed to get file info: %w", err)
	}

	// Use chunked upload for large files
	if fileInfo.Size() > LargeFileThreshold {
		return c.UploadFileChunked(filePath, DefaultChunkSize)
	}

	// Use simple streaming upload for smaller files
	return c.UploadFile(filePath)
}

// computeFileChecksum computes SHA256 checksum of a file
func computeFileChecksum(filePath string) (string, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return "", fmt.Errorf("failed to open file: %w", err)
	}
	defer file.Close()

	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", fmt.Errorf("failed to compute checksum: %w", err)
	}

	return hex.EncodeToString(hash.Sum(nil)), nil
}

// HashingReader wraps an io.Reader and computes a hash while reading
type HashingReader struct {
	reader io.Reader
	hash   hash.Hash
}

// NewHashingReader creates a new HashingReader
func NewHashingReader(reader io.Reader) *HashingReader {
	return &HashingReader{
		reader: reader,
		hash:   sha256.New(),
	}
}

// Read implements io.Reader
func (h *HashingReader) Read(p []byte) (n int, err error) {
	n, err = h.reader.Read(p)
	if n > 0 {
		h.hash.Write(p[:n])
	}
	return n, err
}

// Checksum returns the hex-encoded checksum
func (h *HashingReader) Checksum() string {
	return hex.EncodeToString(h.hash.Sum(nil))
}
