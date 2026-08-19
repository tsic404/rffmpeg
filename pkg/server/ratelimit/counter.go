// Package ratelimit provides per-client rate limiting for job submissions.
// It tracks active (non-terminal) job counts per client_id and enforces a
// configurable maximum concurrent jobs per client.
package ratelimit

import (
	"sync"
)

// ClientJobCounter tracks the number of active jobs per client.
// Implementations must be safe for concurrent use.
type ClientJobCounter interface {
	// TryIncrement attempts to increment the active job count for a client.
	// Returns (current count, allowed bool). If allowed is false, the client
	// has reached the maximum and should be rate-limited.
	TryIncrement(clientID string, limit int) (current int, allowed bool)

	// RegisterJob maps a jobID to a clientID so that DecrementByJob can find
	// the right client. Call after successful job creation.
	RegisterJob(clientID, jobID string)

	// Decrement decrements the active job count for a client.
	Decrement(clientID string) int

	// DecrementByJob decrements the count for the client associated with a job.
	// Used when a job transitions to a terminal state via UpdateJob
	// (where the caller may not have a client_id in context, e.g. worker updates).
	DecrementByJob(jobID string) int

	// Count returns the current active job count for a client.
	Count(clientID string) int

	// TotalActive returns the total number of active jobs across all clients.
	TotalActive() int

	// RejectedCount returns the total number of rejected requests since startup.
	RejectedCount() int64
}

// InMemoryCounter is a ClientJobCounter that stores counts in memory.
type InMemoryCounter struct {
	mu        sync.RWMutex
	counts    map[string]int    // clientID -> active job count
	jobClient map[string]string // jobID -> clientID
	rejected  int64
}

// NewInMemoryCounter creates a new in-memory client job counter.
func NewInMemoryCounter() *InMemoryCounter {
	return &InMemoryCounter{
		counts:    make(map[string]int),
		jobClient: make(map[string]string),
	}
}

func (c *InMemoryCounter) TryIncrement(clientID string, limit int) (int, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	current := c.counts[clientID]
	if current >= limit {
		c.rejected++
		return current, false
	}
	c.counts[clientID] = current + 1
	return current + 1, true
}

func (c *InMemoryCounter) RegisterJob(clientID, jobID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.jobClient[jobID] = clientID
}

func (c *InMemoryCounter) Decrement(clientID string) int {
	c.mu.Lock()
	defer c.mu.Unlock()

	current := c.counts[clientID]
	if current <= 1 {
		delete(c.counts, clientID)
		return 0
	}
	c.counts[clientID] = current - 1
	return current - 1
}

func (c *InMemoryCounter) DecrementByJob(jobID string) int {
	c.mu.Lock()
	defer c.mu.Unlock()

	clientID, ok := c.jobClient[jobID]
	if !ok {
		return 0
	}
	delete(c.jobClient, jobID)

	current := c.counts[clientID]
	if current <= 1 {
		delete(c.counts, clientID)
		return 0
	}
	c.counts[clientID] = current - 1
	return current - 1
}

func (c *InMemoryCounter) Count(clientID string) int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.counts[clientID]
}

func (c *InMemoryCounter) TotalActive() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	total := 0
	for _, count := range c.counts {
		total += count
	}
	return total
}

func (c *InMemoryCounter) RejectedCount() int64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.rejected
}
