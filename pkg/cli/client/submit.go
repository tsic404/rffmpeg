package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/tsic404/rffmpeg/pkg/protocol"
)

// Overridable sleep for rate-limit resubmission backoff; production value
// waits d or until ctx is done (whichever comes first). Internal tests stub
// it so the 429 retry loop runs instantly instead of waiting out real
// exponential backoff.
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

// WithSubmitRetries sets the retry budget for rate-limit (HTTP 429) job
// submission. 0 (the default) means "no retries" — a rate-limited submission
// fails immediately. A positive n retries the submission up to n times with
// exponential backoff honoring the server's retry_in hint.
func WithSubmitRetries(n int) ClientOption {
	return func(c *Client) {
		if n >= 0 {
			c.submitRetries = n
		}
	}
}

// ErrRetriesExhausted marks the point where the client stops waiting for the
// server — its retry budget or its server-loss timeout was spent. It is wrapped
// by RetriesExhaustedError, which carries the submitted job ID.
var ErrRetriesExhausted = errors.New("retries exhausted")

// RetriesExhaustedError reports that a job was submitted successfully but the
// client gave up waiting for it: either the server went silent for longer than
// the server-loss timeout (RFFMPEG_SERVER_LOSS_TIMEOUT), or the retry budget was
// spent. It must be surfaced as a distinct exit code — never a generic failure
// — so callers can tell "submitted, then gave up" apart from "submission
// failed". The cause carries which bound fired and why.
type RetriesExhaustedError struct {
	JobID string
	Cause error
}

func (e *RetriesExhaustedError) Error() string {
	return fmt.Sprintf(
		"gave up waiting for job %s; the job keeps running server-side — query final status via GET /api/v1/jobs/%s (last error: %v)",
		e.JobID, e.JobID, e.Cause,
	)
}

func (e *RetriesExhaustedError) Unwrap() error {
	return e.Cause
}

// RateLimitError reports an HTTP 429 rejection from job submission — either
// a rate-limit (rate_limit_exceeded) or a concurrent-submission conflict
// (submit_conflict). It carries the server's suggested backoff so a caller
// with a retry budget can honor it instead of re-submitting immediately.
// Error renders the rejection fact without a retry promise; the retry hint
// is added only once a retry budget was spent, so the default (no RFFMPEG_RETRY)
// path never tells the user a retry is coming.
type RateLimitError struct {
	Current int
	Limit   int
	RetryIn int // suggested backoff in seconds; 0 when the server gave none
	Code    protocol.ErrorCode
	// decoded reports whether the body parsed as a structured
	// RateLimitResponse (vs. an undecodable or empty body).
	decoded bool
	// rawBody holds an undecodable response body (diagnostic only).
	rawBody string
	// retried reports that the client exhausted its submit-retry budget on
	// this rejection; only then does Error() carry the retry hint.
	retried bool
}

func (e *RateLimitError) Error() string {
	if e.decoded {
		if e.Code == protocol.ErrCodeSubmitConflict {
			if e.retried {

				return fmt.Sprintf("concurrent submission conflict; retry after %d seconds",
					int(rateLimitBackoff(e.RetryIn, 0).Seconds()))
			}
			return "concurrent submission conflict"
		}
		if e.retried {
			return fmt.Sprintf(
				"rate limit exceeded: %d/%d concurrent jobs. Retry after %d seconds",
				e.Current, e.Limit, e.RetryIn,
			)
		}
		return fmt.Sprintf("rate limit exceeded: %d/%d concurrent jobs", e.Current, e.Limit)
	}
	if e.rawBody != "" {
		return fmt.Sprintf("rate limit exceeded (HTTP 429): %s", e.rawBody)
	}
	return "rate limit exceeded (HTTP 429)"
}

// SubmitJob submits a transcoding job.
func (c *Client) SubmitJob(inputFiles []string, args []string, outputFilename string, autoHW bool) (string, error) {
	return c.SubmitJobWithOptions(context.Background(), inputFiles, nil, args, outputFilename, autoHW, false, 0)
}

// SubmitJobWithOptions submits a transcoding job with additional options;
// timeout is the job timeout duration (0 means use server default). With a
// submit-retry budget (WithSubmitRetries), an HTTP 429 is retried with
// exponential backoff starting at the server's retry_in hint, doubling per
// attempt and capped at submitRetryMaxDelay; any other failure (network, auth,
// validation) fails immediately, and once the budget is spent the last
// rate-limit error is returned. The wait is interruptible: ctx cancellation
// stops it and returns ctx.Err() instead of blocking for the full backoff.
func (c *Client) SubmitJobWithOptions(ctx context.Context, inputFiles []string, directPath []string, args []string, outputFilename string, autoHW bool, streamingOutput bool, timeout time.Duration) (string, error) {
	var lastErr error
	for attempt := 0; attempt <= c.submitRetries; attempt++ {
		jobID, err := c.submitJobOnce(ctx, inputFiles, directPath, args, outputFilename, autoHW, streamingOutput, timeout)
		if err == nil {
			return jobID, nil
		}
		lastErr = err

		var rl *RateLimitError
		if !errors.As(err, &rl) {
			break
		}
		if attempt == c.submitRetries {

			if c.submitRetries > 0 {
				rl.retried = true
			}
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

		if resp.StatusCode == http.StatusTooManyRequests {
			rlErr := decodeRateLimitError(resp.Body)
			if rl, ok := rlErr.(*RateLimitError); ok && rl.Code != "" {
				return "", fmt.Errorf("job submission failed [%s]: %w", rl.Code, rl)
			}

			return "", fmt.Errorf("job submission failed: %w", rlErr)
		}
		errResp, _ := parseErrorResponse(resp.Body)
		return "", serverRejection("job submission", resp.StatusCode, errResp.Code, errResp.Message)
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
				Code:    rlResp.Code,
				decoded: true,
			}
		}

		return &RateLimitError{rawBody: string(bodyBytes)}
	}
	return &RateLimitError{}
}

// rateLimitBackoff computes the wait before the attempt-th retry (0-based),
// starting from the server's retry_in hint (or 1s when absent) and doubling
// per attempt, capped at submitRetryMaxDelay.
func rateLimitBackoff(retryIn, attempt int) time.Duration {

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
