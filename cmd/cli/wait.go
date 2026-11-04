package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/tsic404/rffmpeg/pkg/cli/client"
	"github.com/tsic404/rffmpeg/pkg/protocol"
)

// jobWaitClient is the subset of *client.Client the wait loop needs. It is an
// interface so the loop can be driven in tests by a fake that returns a
// scripted status sequence without a real server.
type jobWaitClient interface {
	GetJob(jobID string) (*protocol.JobInfo, error)
	WaitForJobWithLogs(ctx context.Context, jobID string, quiet bool) (*protocol.JobInfo, error)
	WaitForJobWithStreamingOutput(ctx context.Context, jobID string, quiet bool) (*protocol.JobInfo, error)
	CancelJob(jobID string) error
}

// giveUpPolicy says what a client-side wait give-up does to the server-side
// job. The submit path owns the job it just created, so it cancels; --resume
// attaches to a job from an earlier run, so it detaches and leaves it running.
type giveUpPolicy int

const (
	giveUpCancels giveUpPolicy = iota
	giveUpDetaches
)

// waitForJobLoop waits for the job to reach a terminal status and returns it,
// or gives up on it when the client-side bound exhausts: onGiveUp decides
// whether that give-up cancels the server-side job (submit path) or leaves it
// running for a later --resume (detach path). It loops
// because the server's NO_WORKER_AVAILABLE verdict deadline is only attached
// to pending/unassigned jobs: a job queued at submit time carries none, but
// reverts to pending if its worker dies. Re-reading the deadline each
// iteration lets the client extend its wait to a freshly attached verdict
// instead of giving up before the server can emit it. pollTimeout is the
// client-side cap on the wait-for-worker (pending) phase, applied only when no
// --timeout budget and no server verdict deadline bound it.
func waitForJobLoop(cli jobWaitClient, jobID string, timeout, pollTimeout time.Duration, streamingOutput, quiet bool, onGiveUp giveUpPolicy) (*protocol.JobInfo, int) {
	var job *protocol.JobInfo
	for {
		var noWorkerDeadline *time.Time
		var startedAt *time.Time
		var status protocol.JobStatus
		if submitted, getErr := cli.GetJob(jobID); getErr == nil {
			noWorkerDeadline = submitted.NoWorkerDeadline
			startedAt = submitted.StartedAt
			status = submitted.Status
		}
		waitDeadline, hasDeadline := clientWaitDeadline(time.Now(), timeout, pollTimeout, status, startedAt, noWorkerDeadline)
		waitCtx := context.Background()
		var cancelWait context.CancelFunc
		if hasDeadline {
			waitCtx, cancelWait = context.WithDeadline(context.Background(), waitDeadline)
		}

		var waitErr error
		if streamingOutput {
			job, waitErr = cli.WaitForJobWithStreamingOutput(waitCtx, jobID, quiet)
		} else {
			job, waitErr = cli.WaitForJobWithLogs(waitCtx, jobID, quiet)
		}
		if cancelWait != nil {
			cancelWait()
		}
		if waitErr == nil {
			break
		}
		var retriesExhausted *client.RetriesExhaustedError
		if errors.As(waitErr, &retriesExhausted) {
			fmt.Fprintf(os.Stderr, "Error: %v\n", retriesExhausted)
			return nil, ExitDisconnected
		}
		if !errors.Is(waitErr, context.DeadlineExceeded) {
			fmt.Fprintf(os.Stderr, "Error waiting for job: %v\n", waitErr)
			return nil, ExitError
		}

		finalJob, finalErr := cli.GetJob(jobID)
		if finalErr == nil && protocol.IsTerminalStatus(finalJob.Status) {
			job = finalJob
			break
		}

		if finalErr == nil && extendWaitForNoWorkerDeadline(waitDeadline, finalJob.NoWorkerDeadline) {
			continue
		}
		if finalErr == nil && extendWaitForStartedAt(waitDeadline, finalJob.StartedAt, timeout) {
			continue
		}
		if finalErr == nil && extendWaitForQueued(finalJob.Status) {
			continue
		}

		budget := giveUpBudget(waitDeadline, finalJob, timeout, pollTimeout)

		if onGiveUp == giveUpDetaches {
			fmt.Fprintf(os.Stderr, "Error: job %s did not complete within %s; it keeps running server-side — re-run with --resume %s to attach again\n", jobID, budget, jobID)
			return nil, ExitDisconnected
		}

		if cancelErr := cli.CancelJob(jobID); cancelErr != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to cancel timed-out job %s: %v\n", jobID, cancelErr)
		}
		if finalErr == nil && (finalJob.Status == protocol.JobStatusPending || finalJob.Status == protocol.JobStatusQueued) {
			fmt.Fprintf(os.Stderr, "Error: job %s did not start within %s and was cancelled by the client (still waiting for a worker)\n", jobID, budget)
		} else {
			fmt.Fprintf(os.Stderr, "Error: job %s did not complete within %s and was cancelled by the client (client gave up; the job was still running)\n", jobID, budget)
		}
		return nil, ExitError
	}
	return job, ExitSuccess
}

// clientWaitDeadline computes the client-side give-up time. timeout is the
// per-job ffmpeg execution budget; pollTimeout is the client-side cap on the
// wait-for-worker (pending) phase; status is the job's current status;
// startedAt is the server's started_at (the budget's anchor); noWorkerDeadline
// is the server's NO_WORKER_AVAILABLE verdict time for a pending job. The
// client waits until the later bound (plus clientVerdictGrace) so it never
// cancels before the server can emit its verdict. Returns false when neither
// bound exists.
//
// --timeout is a worker-side execution budget, not a wall-clock bound. It
// bounds the client's wait only for pending jobs (anchored to now — the "did
// not start within --timeout" give-up) and running jobs (anchored to started_at
// so pre-exec latency is not charged and the worker's TIMEOUT verdict stays
// observable).
//
// A queued job is already claimed by a worker and spends its pre-exec phase
// (download, duration probe) in the queued state; that phase is bounded by the
// worker's own timeouts (30m dataClient download + ffprobe executor), not by
// --timeout, so the client must not cancel it before ffmpeg starts.
//
// The old code computed timeout+clientVerdictGrace as a duration sum, which
// overflowed negative for a --timeout near math.MaxInt64 and made
// context.WithTimeout expire immediately. The bounds are added to wall-clock
// times separately; a duration near MaxInt64 (~292 years) plus a 5s grace
// cannot wrap a time.Time anchored at the present (year ~2318), so no
// clamping is needed.
func clientWaitDeadline(now time.Time, timeout, pollTimeout time.Duration, status protocol.JobStatus, startedAt, noWorkerDeadline *time.Time) (time.Time, bool) {
	var deadline time.Time
	has := false
	if timeout > 0 && status != protocol.JobStatusQueued {
		anchor := now
		if startedAt != nil {
			anchor = *startedAt
		}
		deadline = anchor.Add(timeout)
		has = true
	}
	if noWorkerDeadline != nil {
		if !has || noWorkerDeadline.After(deadline) {
			deadline = *noWorkerDeadline
		}
		has = true
	}

	if !has && pollTimeout > 0 && status == protocol.JobStatusPending {
		deadline = now.Add(pollTimeout)
		has = true
	}
	if !has {
		return time.Time{}, false
	}
	return deadline.Add(clientVerdictGrace), true
}

// extendWaitForNoWorkerDeadline reports whether the client should extend its
// wait after the client-side deadline fired: the server must now report a
// verdict deadline (the job reverted to pending after its worker died) that
// is later than the bound just exhausted. Comparing the raw server verdict
// against the exhausted bound — not a recomputed now+timeout bound, which
// always advances with now — keeps the retry bounded: the deadline is fixed
// once attached, so this can extend the wait at most once.
func extendWaitForNoWorkerDeadline(exhaustedDeadline time.Time, noWorkerDeadline *time.Time) bool {
	return noWorkerDeadline != nil && noWorkerDeadline.After(exhaustedDeadline)
}

// extendWaitForStartedAt reports whether the client should extend its wait
// after the client-side deadline fired because the job just started running.
// With --timeout set, the first wait was anchored to submit time (started_at
// was nil) but the worker's ffmpeg budget only begins at started_at, so the
// TIMEOUT verdict is still ahead of the exhausted bound; the re-anchored bound
// (started_at + timeout + grace) is fixed — started_at never advances — so the
// extension fires at most once. With --timeout unset the fired bound was the
// client-side poll cap, which bounds only the wait-for-worker phase; a running
// job is past that phase, so extend unconditionally and let the worker's own
// timeout bound it from here.
func extendWaitForStartedAt(exhaustedDeadline time.Time, startedAt *time.Time, timeout time.Duration) bool {
	if startedAt == nil {
		return false
	}
	if timeout <= 0 {

		return true
	}
	return startedAt.Add(timeout).Add(clientVerdictGrace).After(exhaustedDeadline)
}

// extendWaitForQueued reports whether the client should extend its wait after
// the client-side deadline fired because the job was just claimed by a worker
// (pending → queued). A queued job spends --timeout on its pre-exec phase
// (download/probe), which the worker bounds independently, so the client must
// not cancel it as if it had never started.
func extendWaitForQueued(status protocol.JobStatus) bool {
	return status == protocol.JobStatusQueued
}

// giveUpBudget returns the duration to print in the client-gave-up error
// message, derived from the bound that actually exhausted. When the server's
// NO_WORKER_AVAILABLE verdict deadline was the controlling bound (the exhausted
// waitDeadline equals verdict+grace), report that verdict's horizon
// (no_worker_job_timeout + check interval, approximated by
// NoWorkerDeadline−CreatedAt) instead of the poll cap — otherwise a job whose
// wait was really bounded by the server prints a misleading "10m0s". Otherwise
// report --timeout when set, else the poll cap.
func giveUpBudget(waitDeadline time.Time, job *protocol.JobInfo, timeout, pollTimeout time.Duration) time.Duration {
	if job != nil && job.NoWorkerDeadline != nil && job.NoWorkerDeadline.Add(clientVerdictGrace).Equal(waitDeadline) {
		if d := job.NoWorkerDeadline.Sub(job.CreatedAt); d > 0 {
			return d
		}
	}
	if timeout > 0 {
		return timeout
	}
	return pollTimeout
}

// reportTerminalJob prints the outcome for a terminal job status and returns
// the process exit code. It returns ExitSuccess only for a completed job; all
// other terminal statuses print an error to stderr and return ExitError.
//
// Every JobStatusFailed line uses the single "Job failed: <tag><message>"
// template so stderr grep/automation has one parse branch. FailureNoWorkerAvailable
// is the server's starvation verdict (checkNoWorkerStarvation) — it prefixes the
// message with "server-side auto_fail: " so it reads as "the server judged this
// failed", never "the client gave up", which lets operators distinguish the two.
//
// Every failure terminal line carries the machine-readable failure_type tag
// (e.g. "[INPUT_UNREACHABLE]") so operators can grep stderr by category
// without parsing the human message.
func reportTerminalJob(job *protocol.JobInfo) int {
	if job == nil {
		fmt.Fprintln(os.Stderr, "Error: no job result returned from server")
		return ExitError
	}

	tag := failureTypeTag(job.FailureType)

	if job.Status == protocol.JobStatusFailed {
		message := job.Error
		if job.FailureType == string(protocol.FailureNoWorkerAvailable) {
			message = "server-side auto_fail: " + message
		}
		fmt.Fprintf(os.Stderr, "Job failed: %s%s\n", tag, message)

		return ExitError
	}

	if job.Status == protocol.JobStatusTimeout {
		fmt.Fprintf(os.Stderr, "Job timed out: %s%s\n", tag, job.Error)
		return ExitError
	}

	if job.Status == protocol.JobStatusCancelled {
		fmt.Fprintln(os.Stderr, "Job was cancelled")
		return ExitError
	}

	if job.Status != protocol.JobStatusCompleted {
		fmt.Fprintf(os.Stderr, "Error: job ended with unexpected status: %s\n", job.Status)
		return ExitError
	}

	return ExitSuccess
}

// failureTypeTag renders the machine-readable failure_type prefix for a
// terminal failure line (e.g. "[INPUT_UNREACHABLE] "), or "" when the job
// carries no classification.
func failureTypeTag(failureType string) string {
	if failureType == "" {
		return ""
	}
	return "[" + failureType + "] "
}
