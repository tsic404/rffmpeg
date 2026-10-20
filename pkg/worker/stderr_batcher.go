package worker

import (
	"context"
	"log"
	"strings"
	"sync"
	"time"
)

// StderrBatcher collects stderr chunks and sends them in batches
// to reduce the number of HTTP requests to the server
type StderrBatcher struct {
	jobID      string
	client     *Client
	chunks     []string
	mu         sync.Mutex
	flushTimer *time.Timer
	batchSize  int
	batchDelay time.Duration
	ctx        context.Context
	cancel     context.CancelFunc
	wg         sync.WaitGroup // Track pending goroutines
}

// StderrBatcherConfig holds configuration for the batcher
type StderrBatcherConfig struct {
	BatchSize  int           // Number of chunks to batch before flushing (default: 10)
	BatchDelay time.Duration // Max time to wait before flushing (default: 100ms)
}

// DefaultStderrBatcherConfig returns default configuration
func DefaultStderrBatcherConfig() StderrBatcherConfig {
	return StderrBatcherConfig{
		BatchSize:  10,
		BatchDelay: 100 * time.Millisecond,
	}
}

// NewStderrBatcher creates a new stderr batcher
func NewStderrBatcher(jobID string, client *Client, cfg StderrBatcherConfig) *StderrBatcher {
	ctx, cancel := context.WithCancel(context.Background())

	b := &StderrBatcher{
		jobID:      jobID,
		client:     client,
		chunks:     make([]string, 0, cfg.BatchSize),
		batchSize:  cfg.BatchSize,
		batchDelay: cfg.BatchDelay,
		ctx:        ctx,
		cancel:     cancel,
	}

	// Arm the flush timer while holding b.mu. time.AfterFunc schedules the
	// callback before this assignment completes, so the assignment itself is
	// not covered by the AfterFunc call's happens-before edge. Writing
	// flushTimer under b.mu (the same lock timedFlush holds when it calls
	// Reset) publishes it to the callback goroutine and removes the
	// -race-detected write/read race on the field.
	b.mu.Lock()
	b.flushTimer = time.AfterFunc(cfg.BatchDelay, b.timedFlush)
	b.mu.Unlock()

	return b
}

// Add adds a stderr chunk to the batch
func (b *StderrBatcher) Add(chunk string) {
	b.mu.Lock()
	defer b.mu.Unlock()

	// Check if context is cancelled
	select {
	case <-b.ctx.Done():
		return
	default:
	}

	b.chunks = append(b.chunks, chunk)

	// Flush if batch is full
	if len(b.chunks) >= b.batchSize {
		b.flushLocked()
	}
}

// timedFlush is called by the timer to flush the batch
func (b *StderrBatcher) timedFlush() {
	b.mu.Lock()
	defer b.mu.Unlock()

	// Check if context is cancelled
	select {
	case <-b.ctx.Done():
		return
	default:
	}

	if len(b.chunks) > 0 {
		b.flushLocked()
	}

	// Reset timer for next batch
	b.flushTimer.Reset(b.batchDelay)
}

// flushLocked flushes the current batch (must be called with lock held)
func (b *StderrBatcher) flushLocked() {
	if len(b.chunks) == 0 {
		return
	}

	// Combine chunks with newlines
	combined := strings.Join(b.chunks, "\n")

	// Track the goroutine so Close() can wait for it
	b.wg.Add(1)
	go func(chunk string) {
		defer b.wg.Done()
		if err := b.client.SendStderrChunk(b.jobID, chunk); err != nil {
			// Non-blocking: a dropped stderr chunk must not stall the job,
			// but the failure must be visible somewhere.
			log.Printf("Job %s: failed to send stderr batch: %v", b.jobID, err)
		}
	}(combined)

	// Clear chunks
	b.chunks = b.chunks[:0]
}

// Flush flushes any remaining chunks immediately
func (b *StderrBatcher) Flush() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.flushLocked()
}

// terminalFlushBudget bounds how long a terminal-status flush may wait for
// tail stderr sends. FlushAndWait exists so the terminal verdict is delivered
// after the tail stderr has actually reached the server — but an unresponsive
// server (e.g. a congested PATCH path) must not stall the terminal report for
// the full stderr HTTP timeout: ffmpeg already stopped, and every second the
// verdict is delayed shows up in the CLI's wall clock. The budget keeps the
// common prompt-server case unchanged while bounding the pathological one.
// A package var (not a const) so tests can shrink it.
var terminalFlushBudget = 2 * time.Second

// waitSends waits for the in-flight stderr sends, bounded by budget. It
// reports whether they all finished; on timeout the caller carries on (the
// sends stay bounded by the control-plane HTTP timeout and touch no mutable
// batcher state beyond the wait group).
func (b *StderrBatcher) waitSends(budget time.Duration) bool {
	done := make(chan struct{})
	go func() {
		b.wg.Wait()
		close(done)
	}()
	timer := time.NewTimer(budget)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
}

// FlushAndWait flushes any pending chunks and blocks until all in-flight
// stderr sends complete. Unlike Close it neither stops the flush timer nor
// cancels the context, so callers may keep appending afterward. It exists so
// a terminal job status can be reported after the tail stderr has actually
// reached the server on the fast-failure path, where the batcher timer may
// not have fired yet. The wait is bounded by terminalFlushBudget so a
// stalled server cannot delay the terminal verdict past the convergence
// budget (AC-31).
func (b *StderrBatcher) FlushAndWait() {
	b.mu.Lock()
	b.flushLocked()
	b.mu.Unlock()

	if !b.waitSends(terminalFlushBudget) {
		log.Printf("Job %s: terminal stderr flush timed out after %s; reporting terminal status anyway", b.jobID, terminalFlushBudget)
	}
}

// Close stops the batcher and flushes any remaining chunks. Like
// FlushAndWait, its wait for in-flight sends is bounded by
// terminalFlushBudget: Close runs from processJob's defer, after the terminal
// status was already reported, and a stalled server must not keep the job
// goroutine alive (activeJobs entry, idle heartbeat) for the full stderr HTTP
// timeout.
func (b *StderrBatcher) Close() {
	// Stop the timer under b.mu: the timer callback (timedFlush) also touches
	// flushTimer under b.mu when it re-arms via Reset, so an unsynchronized
	// Stop here races with that Reset (-race DATA RACE on the timer).
	b.mu.Lock()
	b.flushTimer.Stop()
	b.mu.Unlock()
	b.cancel()
	b.Flush()
	if !b.waitSends(terminalFlushBudget) {
		log.Printf("Job %s: stderr drain on close timed out after %s; closing anyway", b.jobID, terminalFlushBudget)
	}
}

// StderrHandler returns a function that can be used as a stderr handler
func (b *StderrBatcher) StderrHandler() func(chunk string) {
	return func(chunk string) {
		b.Add(chunk)
	}
}
