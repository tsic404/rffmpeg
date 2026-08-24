package worker

import (
	"context"
	"sync"
	"time"
)

// StdoutBatcher collects stdout chunks and sends them in batches
// to reduce the number of HTTP requests to the server (streaming output mode)
type StdoutBatcher struct {
	jobID      string
	client     *Client
	chunks     [][]byte
	mu         sync.Mutex
	flushTimer *time.Timer
	batchSize  int
	batchDelay time.Duration
	ctx        context.Context
	cancel     context.CancelFunc
	wg         sync.WaitGroup // Track pending goroutines
}

// StdoutBatcherConfig holds configuration for the batcher
type StdoutBatcherConfig struct {
	BatchSize  int           // Number of chunks to batch before flushing (default: 10)
	BatchDelay time.Duration // Max time to wait before flushing (default: 50ms for streaming)
}

// DefaultStdoutBatcherConfig returns default configuration for stdout streaming
func DefaultStdoutBatcherConfig() StdoutBatcherConfig {
	return StdoutBatcherConfig{
		BatchSize:  10,
		BatchDelay: 50 * time.Millisecond, // Faster than stderr for lower latency
	}
}

// NewStdoutBatcher creates a new stdout batcher
func NewStdoutBatcher(jobID string, client *Client, cfg StdoutBatcherConfig) *StdoutBatcher {
	ctx, cancel := context.WithCancel(context.Background())

	b := &StdoutBatcher{
		jobID:      jobID,
		client:     client,
		chunks:     make([][]byte, 0, cfg.BatchSize),
		batchSize:  cfg.BatchSize,
		batchDelay: cfg.BatchDelay,
		ctx:        ctx,
		cancel:     cancel,
	}

	b.flushTimer = time.AfterFunc(cfg.BatchDelay, b.timedFlush)

	return b
}

// Add adds a stdout chunk to the batch
func (b *StdoutBatcher) Add(chunk []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()

	// Check if context is cancelled
	select {
	case <-b.ctx.Done():
		return
	default:
	}

	// Copy: the executor reuses its read buffer across chunks
	cp := make([]byte, len(chunk))
	copy(cp, chunk)
	b.chunks = append(b.chunks, cp)

	// Flush if batch is full
	if len(b.chunks) >= b.batchSize {
		b.flushLocked()
	}
}

// timedFlush is called by the timer to flush the batch
func (b *StdoutBatcher) timedFlush() {
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
func (b *StdoutBatcher) flushLocked() {
	if len(b.chunks) == 0 {
		return
	}

	// Concatenate chunks without separators (binary data may be involved)
	size := 0
	for _, chunk := range b.chunks {
		size += len(chunk)
	}
	combined := make([]byte, 0, size)
	for _, chunk := range b.chunks {
		combined = append(combined, chunk...)
	}

	// Track the goroutine so Close() can wait for it
	b.wg.Add(1)
	go func(chunk []byte) {
		defer b.wg.Done()
		if err := b.client.SendStdoutChunk(b.jobID, chunk); err != nil {
			// Log error but don't block
			// Error is logged in the caller
		}
	}(combined)

	// Clear chunks
	b.chunks = b.chunks[:0]
}

// Flush flushes any remaining chunks immediately
func (b *StdoutBatcher) Flush() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.flushLocked()
}

// Close stops the batcher and flushes any remaining chunks.
// It waits for all pending goroutines to complete before returning
// to ensure all stdout chunks are sent before subsequent status updates.
func (b *StdoutBatcher) Close() {
	b.flushTimer.Stop()
	b.cancel()
	b.Flush()
	// Wait for all pending goroutines to complete
	b.wg.Wait()
}

// StdoutHandler returns a function that can be used as a stdout handler
func (b *StdoutBatcher) StdoutHandler() func(chunk []byte) {
	return func(chunk []byte) {
		b.Add(chunk)
	}
}
