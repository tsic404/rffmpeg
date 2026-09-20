package storage

import (
	"context"
	"log"
	"sync"
	"time"
)

// InUseProvider returns the set of input file IDs still referenced by
// non-terminal jobs. The cleaner skips these blobs regardless of age.
type InUseProvider func() (map[string]struct{}, error)

// InputFileCleaner periodically sweeps the content-addressed input blob
// directory, removing blobs that have not been used within the TTL window and
// are not referenced by any active job. TTL <= 0 disables the sweep.
type InputFileCleaner struct {
	storage  *Storage
	ttl      time.Duration
	interval time.Duration
	inUse    InUseProvider

	mu     sync.Mutex
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// NewInputFileCleaner creates a cleaner. inUse may be nil, in which case no
// blob is protected from age-based eviction.
func NewInputFileCleaner(s *Storage, ttl, interval time.Duration, inUse InUseProvider) *InputFileCleaner {
	return &InputFileCleaner{storage: s, ttl: ttl, interval: interval, inUse: inUse}
}

// Start begins the background sweep loop. It is a no-op when TTL <= 0, when
// the sweep interval <= 0 (time.NewTicker panics on a non-positive interval),
// or when the cleaner has already been started.
func (c *InputFileCleaner) Start(ctx context.Context) {
	c.mu.Lock()
	if c.ttl <= 0 || c.interval <= 0 || c.cancel != nil {
		c.mu.Unlock()
		return
	}
	c.ctx, c.cancel = context.WithCancel(ctx)
	c.mu.Unlock()

	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		ticker := time.NewTicker(c.interval)
		defer ticker.Stop()
		for {
			select {
			case <-c.ctx.Done():
				return
			case <-ticker.C:
				c.CleanupOnce()
			}
		}
	}()
}

// Stop stops the background sweep loop and waits for it to exit. Safe to call
// when Start was never called.
func (c *InputFileCleaner) Stop() {
	c.mu.Lock()
	cancel := c.cancel
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	c.wg.Wait()
}

// CleanupOnce runs a single sweep. Exported so startup and tests can trigger a
// deterministic sweep without waiting for the ticker.
//
// The sweep fails closed: if the in-use set cannot be loaded, no blob is
// deleted — an empty fallback set would evict blobs still referenced by active
// jobs.
func (c *InputFileCleaner) CleanupOnce() {
	if c.ttl <= 0 {
		return
	}

	inUse := map[string]struct{}{}
	if c.inUse != nil {
		ids, err := c.inUse()
		if err != nil {
			log.Printf("Input file cleanup: aborting sweep, failed to load active file IDs: %v", err)
			return
		}
		inUse = ids
	}

	removed, err := c.storage.CleanupExpiredInputFiles(time.Now().Add(-c.ttl), inUse)
	if err != nil {
		log.Printf("Input file cleanup: %v", err)
		return
	}
	if removed > 0 {
		log.Printf("Input file cleanup: removed %d expired blob(s)", removed)
	}
}
