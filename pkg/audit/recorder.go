package audit

import (
	"errors"
	"sync"
	"time"
)

// Common errors for AuditRecorder.
var (
	// ErrBufferFull indicates the audit buffer is full and cannot accept new records.
	ErrBufferFull = errors.New("audit buffer full")

	// ErrNotFound indicates no audit records were found for the query.
	ErrNotFound = errors.New("no audit records found")
)

// AuditRecorder defines the interface for recording and querying audit operations.
// Implementations must be safe for concurrent use.
type AuditRecorder interface {
	// Record records an audit operation.
	// Returns ErrBufferFull if the buffer cannot accept more records.
	Record(op AuditOperation) error

	// Query retrieves all audit operations for a specific request ID.
	// Returns ErrNotFound if no records exist for the request.
	Query(requestID string) ([]AuditOperation, error)

	// QueryByTimeRange retrieves audit operations within a time range.
	// Returns ErrNotFound if no records exist in the range.
	QueryByTimeRange(start, end time.Time) ([]AuditOperation, error)

	// QueryRecent retrieves the most recent N audit operations.
	QueryRecent(limit int) []AuditOperation

	// GetSummary returns a summary of audit operations for a request.
	GetSummary(requestID string) (*AuditSummary, error)

	// Clear clears all audit records from the buffer.
	Clear()

	// Size returns the current number of records in the buffer.
	Size() int

	// Capacity returns the maximum buffer capacity.
	Capacity() int
}

// RingBufferRecorder implements AuditRecorder using a ring buffer.
// It maintains a fixed-size circular buffer to prevent memory leaks
// while preserving recent audit history.
type RingBufferRecorder struct {
	mu       sync.RWMutex
	buffer   []AuditOperation
	capacity int
	size     int
	head     int // next write position
}

// NewRingBufferRecorder creates a new ring buffer recorder with the specified capacity.
// If capacity is <= 0, a default capacity of 1000 is used.
func NewRingBufferRecorder(capacity int) *RingBufferRecorder {
	if capacity <= 0 {
		capacity = 1000
	}
	return &RingBufferRecorder{
		buffer:   make([]AuditOperation, capacity),
		capacity: capacity,
		size:     0,
		head:     0,
	}
}

// Record adds an audit operation to the ring buffer.
// This method never returns ErrBufferFull; instead, it overwrites the oldest record.
func (r *RingBufferRecorder) Record(op AuditOperation) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Set timestamp if not already set
	if op.Timestamp.IsZero() {
		op.Timestamp = time.Now()
	}

	// Write to the current head position
	r.buffer[r.head] = op
	r.head = (r.head + 1) % r.capacity

	// Track actual size (grows until buffer is full)
	if r.size < r.capacity {
		r.size++
	}

	return nil
}

// Query retrieves all audit operations for a specific request ID.
// Operations are returned in chronological order (oldest first).
func (r *RingBufferRecorder) Query(requestID string) ([]AuditOperation, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var results []AuditOperation

	// Iterate through the buffer in chronological order
	for i := 0; i < r.size; i++ {
		// Calculate the actual index (oldest to newest)
		idx := (r.head - r.size + i + r.capacity) % r.capacity
		if r.buffer[idx].RequestID == requestID {
			results = append(results, r.buffer[idx])
		}
	}

	if len(results) == 0 {
		return nil, ErrNotFound
	}

	return results, nil
}

// QueryByTimeRange retrieves audit operations within a time range.
// Operations are returned in chronological order (oldest first).
func (r *RingBufferRecorder) QueryByTimeRange(start, end time.Time) ([]AuditOperation, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var results []AuditOperation

	// Iterate through the buffer in chronological order
	for i := 0; i < r.size; i++ {
		// Calculate the actual index (oldest to newest)
		idx := (r.head - r.size + i + r.capacity) % r.capacity
		op := r.buffer[idx]
		if (op.Timestamp.Equal(start) || op.Timestamp.After(start)) &&
			(op.Timestamp.Equal(end) || op.Timestamp.Before(end)) {
			results = append(results, op)
		}
	}

	if len(results) == 0 {
		return nil, ErrNotFound
	}

	return results, nil
}

// QueryRecent retrieves the most recent N audit operations.
// Operations are returned in reverse chronological order (newest first).
func (r *RingBufferRecorder) QueryRecent(limit int) []AuditOperation {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if limit <= 0 || limit > r.size {
		limit = r.size
	}

	results := make([]AuditOperation, 0, limit)

	// Iterate from newest to oldest
	for i := 0; i < limit; i++ {
		// Calculate the actual index (newest first)
		idx := (r.head - 1 - i + r.capacity) % r.capacity
		results = append(results, r.buffer[idx])
	}

	return results
}

// GetSummary returns a summary of audit operations for a request.
func (r *RingBufferRecorder) GetSummary(requestID string) (*AuditSummary, error) {
	ops, err := r.Query(requestID)
	if err != nil {
		return nil, err
	}

	summary := &AuditSummary{
		RequestID:       requestID,
		Scenarios:       make(map[ScenarioType]int),
		TotalOperations: len(ops),
		HasWarnings:     false,
		HasErrors:       false,
	}

	for i, op := range ops {
		if i == 0 {
			summary.StartTime = op.Timestamp
		}
		summary.EndTime = op.Timestamp

		summary.Scenarios[op.ScenarioType]++

		// TSI-2365: HasErrors is derived from scenarios where the rewrite
		// engine could not satisfy the request at all, instead of staying
		// hardcoded false.
		switch op.ScenarioType {
		case ScenarioEncoderFallback, ScenarioEncoderSubstitution:
			summary.HasWarnings = true
		case ScenarioFormatNotAvailable, ScenarioEncoderUnsupported:
			summary.HasErrors = true
		}
	}

	return summary, nil
}

// Clear removes all audit records from the buffer.
func (r *RingBufferRecorder) Clear() {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.buffer = make([]AuditOperation, r.capacity)
	r.size = 0
	r.head = 0
}

// Size returns the current number of records in the buffer.
func (r *RingBufferRecorder) Size() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.size
}

// Capacity returns the maximum buffer capacity.
func (r *RingBufferRecorder) Capacity() int {
	return r.capacity
}

// InMemoryRecorder is a simple in-memory recorder without size limits.
// Useful for testing or scenarios where memory is not a concern.
type InMemoryRecorder struct {
	mu     sync.RWMutex
	record []AuditOperation
}

// NewInMemoryRecorder creates a new unbounded in-memory recorder.
func NewInMemoryRecorder() *InMemoryRecorder {
	return &InMemoryRecorder{
		record: make([]AuditOperation, 0),
	}
}

// Record adds an audit operation to the recorder.
func (r *InMemoryRecorder) Record(op AuditOperation) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if op.Timestamp.IsZero() {
		op.Timestamp = time.Now()
	}

	r.record = append(r.record, op)
	return nil
}

// Query retrieves all audit operations for a specific request ID.
func (r *InMemoryRecorder) Query(requestID string) ([]AuditOperation, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var results []AuditOperation
	for _, op := range r.record {
		if op.RequestID == requestID {
			results = append(results, op)
		}
	}

	if len(results) == 0 {
		return nil, ErrNotFound
	}

	return results, nil
}

// QueryByTimeRange retrieves audit operations within a time range.
func (r *InMemoryRecorder) QueryByTimeRange(start, end time.Time) ([]AuditOperation, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var results []AuditOperation
	for _, op := range r.record {
		if (op.Timestamp.Equal(start) || op.Timestamp.After(start)) &&
			(op.Timestamp.Equal(end) || op.Timestamp.Before(end)) {
			results = append(results, op)
		}
	}

	if len(results) == 0 {
		return nil, ErrNotFound
	}

	return results, nil
}

// QueryRecent retrieves the most recent N audit operations.
func (r *InMemoryRecorder) QueryRecent(limit int) []AuditOperation {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if limit <= 0 || limit > len(r.record) {
		limit = len(r.record)
	}

	results := make([]AuditOperation, limit)
	copy(results, r.record[len(r.record)-limit:])
	// Reverse to get newest first
	for i, j := 0, len(results)-1; i < j; i, j = i+1, j-1 {
		results[i], results[j] = results[j], results[i]
	}
	return results
}

// GetSummary returns a summary of audit operations for a request.
func (r *InMemoryRecorder) GetSummary(requestID string) (*AuditSummary, error) {
	ops, err := r.Query(requestID)
	if err != nil {
		return nil, err
	}

	summary := &AuditSummary{
		RequestID:       requestID,
		Scenarios:       make(map[ScenarioType]int),
		TotalOperations: len(ops),
	}

	for i, op := range ops {
		if i == 0 {
			summary.StartTime = op.Timestamp
		}
		summary.EndTime = op.Timestamp
		summary.Scenarios[op.ScenarioType]++

		switch op.ScenarioType {
		case ScenarioEncoderFallback, ScenarioEncoderSubstitution:
			summary.HasWarnings = true
		case ScenarioFormatNotAvailable, ScenarioEncoderUnsupported:
			summary.HasErrors = true
		}
	}

	return summary, nil
}

// Clear removes all audit records.
func (r *InMemoryRecorder) Clear() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.record = make([]AuditOperation, 0)
}

// Size returns the current number of records.
func (r *InMemoryRecorder) Size() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.record)
}

// Capacity returns -1 to indicate unbounded capacity.
func (r *InMemoryRecorder) Capacity() int {
	return -1
}
