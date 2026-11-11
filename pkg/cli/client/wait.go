package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tsic404/rffmpeg/pkg/protocol"
)

// GetJob gets job status.
func (c *Client) GetJob(jobID string) (*protocol.JobInfo, error) {
	return c.getJob(context.Background(), jobID)
}

// getJob is GetJob tied to ctx, so a wait that is ending can abandon an
// in-flight status request instead of holding its helper goroutine open for
// the transport timeout after the caller already returned.
func (c *Client) getJob(ctx context.Context, jobID string) (*protocol.JobInfo, error) {
	url := fmt.Sprintf(c.serverURL+JobEndpoint, jobID)

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
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

		return nil, fmt.Errorf("%w: %s", protocol.ErrJobNotFound, jobID)
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

// isServerSilence reports whether a GetJob failure means the server never
// answered at all. Only transport-level failures (dial, TLS, timeout, dropped
// connection) count as contact loss — http.Client.Do wraps exactly those in
// *url.Error. Any HTTP response, an error status or an unparsable body
// included, proves the server is reachable.
func isServerSilence(err error) bool {
	var urlErr *url.Error
	return errors.As(err, &urlErr)
}

// statusPollBudget bounds one run of failed status polls. Two failure runs are
// tracked separately: continuous silence — requests the server never answered
// — by silenceBudget, and any run of failures by failBudget, so a server that
// answers with errors still gives up eventually instead of polling forever. A
// zero budget means "no retries": the first failure of that kind is final.
type statusPollBudget struct {
	silenceBudget time.Duration
	failBudget    time.Duration
	failStart     time.Time // first failure of the current run, any cause
	silenceStart  time.Time // first unanswered failure of the current run
}

// failure records one failed poll and returns the give-up verdict once a
// budget is spent; nil means the caller keeps polling.
func (b *statusPollBudget) failure(jobID string, err error, now time.Time) error {
	if b.failStart.IsZero() {
		b.failStart = now
	}
	if isServerSilence(err) {
		if b.silenceStart.IsZero() {
			b.silenceStart = now
		}
	} else {

		b.silenceStart = time.Time{}
	}
	if !b.silenceStart.IsZero() && now.Sub(b.silenceStart) >= b.silenceBudget {
		return &RetriesExhaustedError{JobID: jobID, Cause: fmt.Errorf("job status polling lost contact with server for %s: %w", now.Sub(b.silenceStart).Round(time.Second), err)}
	}
	if now.Sub(b.failStart) >= b.failBudget {
		return &RetriesExhaustedError{JobID: jobID, Cause: fmt.Errorf("job status polling failed for %s: %w", now.Sub(b.failStart).Round(time.Second), err)}
	}
	return nil
}

// reset starts a fresh failure run after a successful poll.
func (b *statusPollBudget) reset() {
	b.failStart = time.Time{}
	b.silenceStart = time.Time{}
}

// backupPoll polls GetJob as a backup to the WebSocket stream, reporting the
// terminal job on pollDone or the give-up verdict on pollErr.
func (c *Client) backupPoll(ctx context.Context, jobID string, interval time.Duration, silenceBudget, failBudget time.Duration, pollDone chan<- *protocol.JobInfo, pollErr chan<- error) {
	budget := statusPollBudget{silenceBudget: silenceBudget, failBudget: failBudget}
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
			job, err := c.getJob(ctx, jobID)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				if errors.Is(err, protocol.ErrJobNotFound) {
					pollErr <- err
					return
				}
				if giveUp := budget.failure(jobID, err, time.Now()); giveUp != nil {
					pollErr <- giveUp
					return
				}
				continue
			}
			budget.reset()
			if protocol.IsTerminalStatus(job.Status) {
				pollDone <- job
				return
			}
		}
	}
}

// WaitForJob waits for job completion and returns exit code.
// It polls until the job reaches a terminal status or ctx is cancelled.
//
// A single failed poll is not fatal: this is the fallback channel for a job
// that was already submitted, so one transient error must not turn a running
// job into a client-side failure and leave the user without its output. The
// same failure budgets as the WebSocket backup poll bound the retry run, and
// spending them is reported as RetriesExhaustedError — the distinct "the job
// is still running server-side" verdict — rather than as an ordinary error.
func (c *Client) WaitForJob(ctx context.Context, jobID string, showProgress bool) (*protocol.JobInfo, error) {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	budget := statusPollBudget{
		silenceBudget: serverLossBudget(c.maxRetries, c.serverLossTimeout),
		failBudget:    retryBudget(c.maxRetries),
	}

	for {
		job, err := c.getJob(ctx, jobID)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			if errors.Is(err, protocol.ErrJobNotFound) {
				return nil, err
			}
			if giveUp := budget.failure(jobID, err, time.Now()); giveUp != nil {
				return nil, giveUp
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-ticker.C:
			}
			continue
		}
		budget.reset()

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

	ctx, cancel := context.WithCancel(ctx)
	var helpers sync.WaitGroup
	defer helpers.Wait()
	defer cancel()

	// terminalSeen is closed once the WebSocket delivers a terminal status or
	// complete event. The backup HTTP poll runs on a 2s tick, so without this
	// the CLI would linger up to a full poll interval after the worker's
	// terminal report is broadcast (e.g. a RFFMPEG_JOB_TIMEOUT verdict), even though the
	// job is already done.
	var terminalOnce sync.Once
	terminalSeen := make(chan struct{})
	markTerminal := func() { terminalOnce.Do(func() { close(terminalSeen) }) }

	notices := &noticeFilter{}
	wsClient := NewWSClient(c.serverURL, jobID, c.token,
		WithOnStderr(func(chunk string) {
			if quiet {
				if line := notices.filter(chunk); line != "" {
					fmt.Fprint(os.Stderr, line)
				}
				return
			}
			fmt.Fprint(os.Stderr, chunk)
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
		WithOnComplete(func(exitCode int) { markTerminal() }),
		WithOnProgress(func(p protocol.WSProgressPayload) {
			if quiet {
				return
			}
			fmt.Fprintln(os.Stderr, renderProgressLine(p))
		}),
		WithWSMaxRetries(c.maxRetries),
		WithWSServerLossTimeout(c.serverLossTimeout),
	)
	defer wsClient.Close()

	if err := wsClient.ConnectWithReconnect(ctx); err != nil {

		if errors.Is(err, ErrRetriesExhausted) {
			return nil, &RetriesExhaustedError{JobID: jobID, Cause: err}
		}

		fmt.Fprintf(os.Stderr, "Warning: WebSocket connection failed, falling back to HTTP polling: %v\n", err)
		return c.WaitForJob(ctx, jobID, !quiet)
	}

	listenDone := make(chan error, 1)
	helpers.Add(1)
	go func() {
		defer helpers.Done()
		listenDone <- wsClient.Listen(ctx)
	}()

	pollDone := make(chan *protocol.JobInfo, 1)
	pollErr := make(chan error, 1)
	// Capture the interval here, in the caller's goroutine: the poll goroutine
	// must not read the test-replaceable process-wide pollInterval after the
	// caller may have changed it.
	interval := pollInterval
	helpers.Add(1)
	go func() {
		defer helpers.Done()
		c.backupPoll(ctx, jobID, interval,
			serverLossBudget(c.maxRetries, c.serverLossTimeout), retryBudget(c.maxRetries),
			pollDone, pollErr)
	}()

	// The server's own shutdown closes the log stream with 1000/1001 (C-104).
	// Treat that as a server restart, not the end of the job: reconnect and
	// keep streaming. A one-shot HTTP fallback would fail on the first
	// connection-refused while the job keeps running server-side.
	maxNormalCloseReconnects := c.maxRetries + 1
	if maxNormalCloseReconnects < 1 {
		maxNormalCloseReconnects = 1
	}
	normalCloseReconnects := 0

	for {
		select {
		case job := <-pollDone:
			if wsClient.HasGap() {
				fmt.Fprintln(os.Stderr, "Warning: log stream incomplete: sequence gap detected (log lines lost during reconnect); output file is unaffected")
			}
			return job, nil
		case <-terminalSeen:

			terminalSeen = nil
			if job, getErr := c.GetJob(jobID); getErr == nil && protocol.IsTerminalStatus(job.Status) {
				if wsClient.HasGap() {
					fmt.Fprintln(os.Stderr, "Warning: log stream incomplete: sequence gap detected (log lines lost during reconnect); output file is unaffected")
				}
				return job, nil
			}

		case err := <-pollErr:

			if job, getErr := c.GetJob(jobID); getErr == nil && protocol.IsTerminalStatus(job.Status) {
				if wsClient.HasGap() {
					fmt.Fprintln(os.Stderr, "Warning: log stream incomplete: sequence gap detected (log lines lost during reconnect); output file is unaffected")
				}
				return job, nil
			}
			return nil, err
		case err := <-listenDone:
			if err != nil {

				if errors.Is(err, ErrRetriesExhausted) {
					return nil, &RetriesExhaustedError{JobID: jobID, Cause: err}
				}

				return c.WaitForJob(ctx, jobID, !quiet)
			}

			if job, getErr := c.GetJob(jobID); getErr == nil && protocol.IsTerminalStatus(job.Status) {
				if wsClient.HasGap() {
					fmt.Fprintln(os.Stderr, "Warning: log stream incomplete: sequence gap detected (log lines lost during reconnect); output file is unaffected")
				}
				return job, nil
			}

			normalCloseReconnects++
			if normalCloseReconnects > maxNormalCloseReconnects {
				return nil, &RetriesExhaustedError{JobID: jobID, Cause: fmt.Errorf("server closed the log stream %d times without completing the job", normalCloseReconnects)}
			}
			fmt.Fprintln(os.Stderr, "Warning: server closed the log stream; reconnecting...")
			if reconnectErr := wsClient.ConnectWithReconnect(ctx); reconnectErr != nil {
				if errors.Is(reconnectErr, ErrRetriesExhausted) {
					return nil, &RetriesExhaustedError{JobID: jobID, Cause: reconnectErr}
				}
				return nil, reconnectErr
			}
			nextListen := make(chan error, 1)
			listenDone = nextListen
			helpers.Add(1)
			go func() {
				defer helpers.Done()
				nextListen <- wsClient.Listen(ctx)
			}()
		case <-ctx.Done():

			if job, getErr := c.GetJob(jobID); getErr == nil && protocol.IsTerminalStatus(job.Status) {
				if wsClient.HasGap() {
					fmt.Fprintln(os.Stderr, "Warning: log stream incomplete: sequence gap detected (log lines lost during reconnect); output file is unaffected")
				}
				return job, nil
			}
			return nil, ctx.Err()
		}
	}
}

// WaitForJobWithStreamingOutput waits for job completion with streaming output to stdout.
// This is used when the output file is "-" to stream transcoded data directly to stdout.
func (c *Client) WaitForJobWithStreamingOutput(ctx context.Context, jobID string, quiet bool) (*protocol.JobInfo, error) {

	ctx, cancel := context.WithCancel(ctx)
	var helpers sync.WaitGroup
	defer helpers.Wait()
	defer cancel()

	// stdoutBytes counts bytes actually written to stdout (partial writes and
	// failures excluded); stdoutErr records the first write failure. A completed
	// streaming job that delivered zero bytes — or whose stdout write failed —
	// must surface as a failure, never a silent rc=0.
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

	notices := &noticeFilter{}
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

			if quiet {
				if line := notices.filter(chunk); line != "" {
					fmt.Fprint(os.Stderr, line)
				}
				return
			}
			fmt.Fprint(os.Stderr, chunk)
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
		WithWSServerLossTimeout(c.serverLossTimeout),
	)
	defer wsClient.Close()

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

	if err := wsClient.ConnectWithReconnect(ctx); err != nil {

		if errors.Is(err, ErrRetriesExhausted) {
			return nil, &RetriesExhaustedError{JobID: jobID, Cause: err}
		}

		fmt.Fprintf(os.Stderr, "Warning: WebSocket connection failed, falling back to HTTP polling: %v\n", err)
		return fallbackToPolling()
	}

	listenDone := make(chan error, 1)
	helpers.Add(1)
	go func() {
		defer helpers.Done()
		listenDone <- wsClient.Listen(ctx)
	}()

	pollDone := make(chan *protocol.JobInfo, 1)
	pollErr := make(chan error, 1)
	interval := pollInterval
	helpers.Add(1)
	go func() {
		defer helpers.Done()
		c.backupPoll(ctx, jobID, interval,
			serverLossBudget(c.maxRetries, c.serverLossTimeout), retryBudget(c.maxRetries),
			pollDone, pollErr)
	}()

	// Wait for completion, error, or context cancellation.
	var finalJob *protocol.JobInfo
	maxNormalCloseReconnects := c.maxRetries + 1
	if maxNormalCloseReconnects < 1 {
		maxNormalCloseReconnects = 1
	}
	normalCloseReconnects := 0
waitLoop:
	for {
		select {
		case job := <-pollDone:
			finalJob = job

			drainStreamingOutput(ctx, terminalSeen, listenDone)
			break waitLoop
		case err := <-pollErr:

			if job, getErr := c.GetJob(jobID); getErr == nil && protocol.IsTerminalStatus(job.Status) {
				finalJob = job
			} else {
				return nil, err
			}
			break waitLoop
		case err := <-listenDone:
			if err != nil {

				if errors.Is(err, ErrRetriesExhausted) {
					return nil, &RetriesExhaustedError{JobID: jobID, Cause: err}
				}

				return fallbackToPolling()
			}

			// A normal close is the server's own shutdown (C-104). The job
			// keeps running; reconnect rather than abandoning the stream.
			if job, getErr := c.GetJob(jobID); getErr == nil && protocol.IsTerminalStatus(job.Status) {
				finalJob = job
				break waitLoop
			}
			normalCloseReconnects++
			if normalCloseReconnects > maxNormalCloseReconnects {
				return nil, &RetriesExhaustedError{JobID: jobID, Cause: fmt.Errorf("server closed the log stream %d times without completing the job", normalCloseReconnects)}
			}
			fmt.Fprintln(os.Stderr, "Warning: server closed the log stream; reconnecting...")
			if reconnectErr := wsClient.ConnectWithReconnect(ctx); reconnectErr != nil {
				if errors.Is(reconnectErr, ErrRetriesExhausted) {
					return nil, &RetriesExhaustedError{JobID: jobID, Cause: reconnectErr}
				}
				return fallbackToPolling()
			}
			nextListen := make(chan error, 1)
			listenDone = nextListen
			helpers.Add(1)
			go func() {
				defer helpers.Done()
				nextListen <- wsClient.Listen(ctx)
			}()
		case <-ctx.Done():

			if job, getErr := c.GetJob(jobID); getErr == nil && protocol.IsTerminalStatus(job.Status) {
				finalJob = job
			} else {
				if e := integrityErr(nil); e != nil {
					return nil, e
				}
				return nil, ctx.Err()
			}
			break waitLoop
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
// running the integrity checks.
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
