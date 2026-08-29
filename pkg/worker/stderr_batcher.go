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

	b.flushTimer = time.AfterFunc(cfg.BatchDelay, b.timedFlush)

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
			// but the failure must be visible somewhere (TSI-2365).
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

// FlushAndWait flushes any pending chunks and blocks until all in-flight
// stderr sends complete. Unlike Close it neither stops the flush timer nor
// cancels the context, so callers may keep appending afterward. It exists so
// a terminal job status can be reported after the tail stderr has actually
// reached the server on the fast-failure path, where the batcher timer may
// not have fired yet (TSI-2581).
func (b *StderrBatcher) FlushAndWait() {
	b.mu.Lock()
	b.flushLocked()
	b.mu.Unlock()
	b.wg.Wait()
}

// Close stops the batcher and flushes any remaining chunks.
// It waits for all pending goroutines to complete before returning
// to ensure all stderr chunks are sent before subsequent status updates.
func (b *StderrBatcher) Close() {
	b.flushTimer.Stop()
	b.cancel()
	b.Flush()
	// Wait for all pending goroutines to complete
	b.wg.Wait()
}

// StderrHandler returns a function that can be used as a stderr handler
func (b *StderrBatcher) StderrHandler() func(chunk string) {
	return func(chunk string) {
		b.Add(chunk)
	}
}
