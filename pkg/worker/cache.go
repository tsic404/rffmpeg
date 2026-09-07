package worker

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tsic404/rffmpeg/pkg/worker/workerconfig"
)

// CacheConfig holds configuration for the worker cache.
type CacheConfig struct {
	// Dir is the root cache directory. Empty means the dynamic default from
	// workerconfig.DefaultCacheDir() (root: /var/cache/rffmpeg, else ~/.cache/rffmpeg).
	Dir string

	// Enabled controls whether caching is active.
	Enabled bool

	// TTL is the time-to-live for cached entries.
	// Entries older than TTL are evicted by the background TTL scanner.
	TTL time.Duration

	// MaxSizeBytes is the maximum total size of the cache in bytes.
	// When exceeded, LRU eviction runs to reclaim space.
	MaxSizeBytes int64

	// TTLScanInterval is how often the background TTL scanner runs.
	TTLScanInterval time.Duration

	// LRUCheckInterval is how often the LRU eviction check runs.
	LRUCheckInterval time.Duration

	// URLTTL is the TTL for cache entries sourced from URL-based inputs.
	// URL content may change without the URL changing, so a shorter TTL
	// prevents stale results from being served for extended periods.
	URLTTL time.Duration
}

// DefaultCacheConfig returns sensible defaults for the cache configuration.
func DefaultCacheConfig() CacheConfig {
	return CacheConfig{
		Dir:              workerconfig.DefaultCacheDir(),
		Enabled:          true,
		TTL:              24 * time.Hour,
		URLTTL:           1 * time.Hour,
		MaxSizeBytes:     10 * 1024 * 1024 * 1024, // 10 GiB
		TTLScanInterval:  10 * time.Minute,
		LRUCheckInterval: 5 * time.Minute,
	}
}

// cacheEntryMeta is stored as JSON alongside each cached file.
type cacheEntryMeta struct {
	Key       string        `json:"key"`
	CreatedAt time.Time     `json:"created_at"`
	Size      int64         `json:"size"`
	TTL       time.Duration `json:"ttl,omitempty"`
}

// CacheStats holds cache usage statistics.
type CacheStats struct {
	Hits      int64 `json:"hits"`
	Misses    int64 `json:"misses"`
	Evictions int64 `json:"evictions"`
	// CurrentSizeBytes is the approximate current size of the cache.
	CurrentSizeBytes int64 `json:"current_size_bytes"`
	// EntryCount is the current number of cached entries.
	EntryCount int `json:"entry_count"`
}

// HitRate returns the cache hit rate as a float between 0 and 1.
func (s CacheStats) HitRate() float64 {
	total := s.Hits + s.Misses
	if total == 0 {
		return 0
	}
	return float64(s.Hits) / float64(total)
}

// Cache is the worker's local disk cache with TTL+LRU eviction.
type Cache struct {
	cfg     CacheConfig
	mu      sync.RWMutex
	stats   CacheStats
	lruList *lruTracker
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

// lruTracker maintains an ordered list of cache entries for LRU eviction.
type lruTracker struct {
	mu       sync.Mutex
	entries  []lruEntry
	keyIndex map[string]int // maps key -> index in entries slice
}

type lruEntry struct {
	Key      string
	LastUsed time.Time
	Size     int64
}

// newLRUTracker creates a new LRU tracker.
func newLRUTracker() *lruTracker {
	return &lruTracker{
		entries:  make([]lruEntry, 0),
		keyIndex: make(map[string]int),
	}
}

func (l *lruTracker) touch(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	idx, ok := l.keyIndex[key]
	if !ok {
		return
	}
	l.entries[idx].LastUsed = time.Now()
}

func (l *lruTracker) add(key string, size int64) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if idx, ok := l.keyIndex[key]; ok {
		l.entries[idx].LastUsed = time.Now()
		l.entries[idx].Size = size
		return
	}

	l.entries = append(l.entries, lruEntry{
		Key:      key,
		LastUsed: time.Now(),
		Size:     size,
	})
	l.keyIndex[key] = len(l.entries) - 1
}

func (l *lruTracker) remove(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	idx, ok := l.keyIndex[key]
	if !ok {
		return
	}

	// Swap with last and truncate
	lastIdx := len(l.entries) - 1
	if idx != lastIdx {
		l.entries[idx] = l.entries[lastIdx]
		l.keyIndex[l.entries[idx].Key] = idx
	}
	l.entries = l.entries[:lastIdx]
	delete(l.keyIndex, key)
}

// getLRU returns the least recently used entry that should be evicted.
// Returns the entry and true, or empty entry and false if list is empty.
func (l *lruTracker) getLRU() (lruEntry, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if len(l.entries) == 0 {
		return lruEntry{}, false
	}

	oldest := l.entries[0]
	for i := 1; i < len(l.entries); i++ {
		if l.entries[i].LastUsed.Before(oldest.LastUsed) {
			oldest = l.entries[i]
		}
	}
	return oldest, true
}

// len returns the number of tracked entries.
func (l *lruTracker) len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.entries)
}

// totalSize returns the total size of tracked entries.
func (l *lruTracker) totalSize() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	var total int64
	for _, e := range l.entries {
		total += e.Size
	}
	return total
}

// cacheDirMode returns 0700 for the per-user fallback dir (shared /tmp,
// private artifacts) and 0755 otherwise.
func cacheDirMode(dir string) os.FileMode {
	if workerconfig.IsFallbackCacheDir(dir) {
		return 0o700
	}
	return 0o755
}

// NewCache creates a new Cache with the given configuration.
func NewCache(cfg CacheConfig) (*Cache, error) {
	if !cfg.Enabled {
		return &Cache{cfg: cfg}, nil
	}

	if cfg.Dir == "" {
		cfg.Dir = workerconfig.DefaultCacheDir()
	}
	if cfg.TTL == 0 {
		cfg.TTL = 24 * time.Hour
	}
	if cfg.MaxSizeBytes == 0 {
		cfg.MaxSizeBytes = 10 * 1024 * 1024 * 1024
	}
	if cfg.TTLScanInterval == 0 {
		cfg.TTLScanInterval = 10 * time.Minute
	}
	if cfg.LRUCheckInterval == 0 {
		cfg.LRUCheckInterval = 5 * time.Minute
	}

	// Create cache directory if it doesn't exist
	if err := os.MkdirAll(cfg.Dir, cacheDirMode(cfg.Dir)); err != nil {
		return nil, fmt.Errorf("failed to create cache directory %s: %w", cfg.Dir, err)
	}

	c := &Cache{
		cfg:     cfg,
		lruList: newLRUTracker(),
	}

	// Scan existing cache entries to rebuild LRU state
	if err := c.scanExisting(); err != nil {
		log.Printf("Warning: cache directory scan failed: %v", err)
	}

	return c, nil
}

// Start begins the background eviction goroutines.
// Cfg returns a copy of the cache configuration.
func (c *Cache) Cfg() CacheConfig {
	return c.cfg
}

func (c *Cache) Start(ctx context.Context) {
	if !c.cfg.Enabled {
		return
	}

	c.ctx, c.cancel = context.WithCancel(ctx)

	c.wg.Add(2)
	go c.runTTLEviction()
	go c.runLRUEviction()
}

// Stop stops the background eviction goroutines.
func (c *Cache) Stop() {
	if c.cancel != nil {
		c.cancel()
	}
	c.wg.Wait()
}

// The first 2 hex chars of the key are used as a sharding subdirectory.
func GenerateCacheKey(inputSources []string, args []string, autoHW bool, outputExt string, encoder string) string {
	canonicalArgs := canonicalizeArgs(args)
	inputPart := strings.Join(inputSources, "|")
	payload := inputPart + "|" + canonicalArgs + "|ext=" + outputExt + "|encoder=" + encoder + "|auto_hw=" + strconv.FormatBool(autoHW)

	hash := sha256.Sum256([]byte(payload))
	return fmt.Sprintf("%x", hash)
}

// canonicalizeArgs produces a deterministic string from ffmpeg arguments.
// Unlike the old sorting-based approach, this preserves the original argument order
// to avoid breaking order-sensitive semantics (-ss before -i vs after -i, -map order, etc.).
// Non-flag arguments (like output paths) are excluded since they vary per-run.
// Input file markers (<INPUT_FILE>) are also excluded as they're part of InputSources.
func canonicalizeArgs(args []string) string {
	if len(args) == 0 {
		return ""
	}

	var tokens []string
	i := 0
	for i < len(args) {
		arg := args[i]

		// Skip input file markers (they're part of the InputURI, not args)
		if arg == "<INPUT_FILE>" {
			i++
			continue
		}

		// Skip output paths (positional, non-flag arguments)
		if !strings.HasPrefix(arg, "-") {
			// This could be an output path or a value for a previous flag.
			// Since we don't know which without full context, skip positional args
			// that don't look like flag values.
			i++
			continue
		}

		// Preserve the original flag-value pair
		if idx := strings.Index(arg, "="); idx != -1 {
			// =value form: keep as-is
			tokens = append(tokens, fmt.Sprintf("%d:%s", len(arg), arg))
		} else if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
			// Separate value form: keep both flag and value together
			// Use length-prefixed encoding to prevent token-boundary collisions
			tokens = append(tokens, fmt.Sprintf("%d:%s", len(arg), arg), fmt.Sprintf("%d:%s", len(args[i+1]), args[i+1]))
			i++ // consume the value
		} else {
			// Boolean flag or standalone flag
			tokens = append(tokens, fmt.Sprintf("%d:%s", len(arg), arg))
		}
		i++
	}

	return strings.Join(tokens, " ")
}

// keyPath returns the file path for a cache key (sharded by first 2 hex chars).
func (c *Cache) keyPath(key string) string {
	if len(key) < 4 {
		return filepath.Join(c.cfg.Dir, key)
	}
	return filepath.Join(c.cfg.Dir, key[:2], key)
}

// metaPath returns the metadata file path for a cache key.
func (c *Cache) metaPath(key string) string {
	return c.keyPath(key) + ".meta"
}

// Check looks up a cache key. Returns (cachedFilePath, true) on hit.
func (c *Cache) Check(key string) (string, bool) {
	if !c.cfg.Enabled {
		return "", false
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	cachePath := c.keyPath(key)
	metaPath := c.metaPath(key)

	// Check if cache file exists
	if _, err := os.Stat(cachePath); os.IsNotExist(err) {
		c.stats.Misses++
		return "", false
	}

	// Check if meta file exists and read TTL
	meta, err := c.loadMeta(metaPath)
	if err != nil {
		// Meta missing or corrupt — treat as miss
		c.stats.Misses++
		return "", false
	}

	// Check TTL (use per-entry TTL from metadata if set, otherwise global config)
	effectiveTTL := c.cfg.TTL
	if meta.TTL > 0 {
		effectiveTTL = meta.TTL
	}
	if time.Since(meta.CreatedAt) > effectiveTTL {
		// Expired — evict it
		c.removeEntry(key, cachePath, metaPath, meta.Size)
		c.stats.Misses++
		return "", false
	}

	// Verify cached file size matches metadata
	if info, err := os.Stat(cachePath); err != nil {
		// File missing — treat as miss
		c.removeEntry(key, cachePath, metaPath, meta.Size)
		c.stats.Misses++
		return "", false
	} else if info.Size() != meta.Size || info.Size() == 0 {
		// Size mismatch or zero-byte file — evict and treat as miss
		c.removeEntry(key, cachePath, metaPath, meta.Size)
		c.stats.Misses++
		return "", false
	}

	// Cache hit (caller must call ConfirmHit for stats tracking)

	return cachePath, true
}

// ConfirmHit records a confirmed cache hit (stats + LRU touch).
// Should be called after successfully using the cached file.
func (c *Cache) ConfirmHit(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stats.Hits++
	c.lruList.touch(key)
}

// Put stores a file in the cache.
// srcPath is the path to the source file (transcode output).
// The source file is copied into the cache; it is not moved.
func (c *Cache) Put(key string, srcPath string, ttlOverride ...time.Duration) error {
	if !c.cfg.Enabled {
		return nil
	}

	// Stat the source file for size
	srcInfo, err := os.Stat(srcPath)
	if err != nil {
		return fmt.Errorf("failed to stat source file: %w", err)
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	cachePath := c.keyPath(key)
	metaPath := c.metaPath(key)
	tmpPath := cachePath + ".tmp"

	// Create shard directory if needed
	if len(key) >= 4 {
		if err := os.MkdirAll(filepath.Join(c.cfg.Dir, key[:2]), 0755); err != nil {
			return fmt.Errorf("failed to create shard directory: %w", err)
		}
	}

	// Copy source file to temp path first for atomicity
	if err := copyFile(srcPath, tmpPath); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("failed to copy file to cache: %w", err)
	}

	// Atomic rename: tmp → cachePath
	if err := os.Rename(tmpPath, cachePath); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("failed to rename temp file: %w", err)
	}

	// Write metadata
	meta := cacheEntryMeta{
		Key:       key,
		CreatedAt: time.Now(),
		Size:      srcInfo.Size(),
	}
	if len(ttlOverride) > 0 && ttlOverride[0] > 0 {
		meta.TTL = ttlOverride[0]
	}
	if err := c.saveMeta(metaPath, meta); err != nil {
		// Clean up the cache file if metadata write fails
		os.Remove(cachePath)
		return fmt.Errorf("failed to write cache metadata: %w", err)
	}

	// Track in LRU
	c.lruList.add(key, meta.Size)
	c.stats.CurrentSizeBytes += meta.Size
	c.stats.EntryCount++

	return nil
}

// Stats returns a snapshot of the current cache statistics.
func (c *Cache) Stats() CacheStats {
	c.mu.RLock()
	defer c.mu.RUnlock()
	s := c.stats
	return s
}

// scanExisting scans the cache directory and rebuilds LRU state.
func (c *Cache) scanExisting() error {
	var totalSize int64
	var count int

	err := filepath.Walk(c.cfg.Dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // skip errors
		}
		if info.IsDir() {
			return nil
		}
		if strings.HasSuffix(path, ".meta") {
			meta, err := c.loadMeta(path)
			if err != nil {
				return nil
			}
			// Check if the corresponding cache file exists
			cachePath := strings.TrimSuffix(path, ".meta")
			if _, err := os.Stat(cachePath); os.IsNotExist(err) {
				// Orphaned meta file — remove it
				os.Remove(path)
				return nil
			}
			c.lruList.add(meta.Key, meta.Size)
			totalSize += meta.Size
			count++
		}
		return nil
	})

	if err != nil {
		return err
	}

	c.stats.CurrentSizeBytes = totalSize
	c.stats.EntryCount = count
	return nil
}

// runTTLEviction periodically scans for expired cache entries.
func (c *Cache) runTTLEviction() {
	defer c.wg.Done()

	ticker := time.NewTicker(c.cfg.TTLScanInterval)
	defer ticker.Stop()

	for {
		select {
		case <-c.ctx.Done():
			return
		case <-ticker.C:
			c.evictExpired()
		}
	}
}

// runLRUEviction periodically checks cache size and evicts LRU entries if over limit.
func (c *Cache) runLRUEviction() {
	defer c.wg.Done()

	ticker := time.NewTicker(c.cfg.LRUCheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-c.ctx.Done():
			return
		case <-ticker.C:
			c.evictLRUIfNeeded()
		}
	}
}

// evictExpired removes all cache entries that exceed TTL.
func (c *Cache) evictExpired() {
	c.mu.Lock()
	defer c.mu.Unlock()

	var expiredKeys []string

	err := filepath.Walk(c.cfg.Dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() || !strings.HasSuffix(path, ".meta") {
			return nil
		}

		meta, err := c.loadMeta(path)
		if err != nil {
			return nil
		}

		if time.Since(meta.CreatedAt) > c.cfg.TTL {
			expiredKeys = append(expiredKeys, meta.Key)
		}
		return nil
	})

	if err != nil {
		log.Printf("Cache TTL scan error: %v", err)
		return
	}

	for _, key := range expiredKeys {
		cachePath := c.keyPath(key)
		metaPath := c.metaPath(key)
		// Stat for size tracking before removal
		if fi, err := os.Stat(cachePath); err == nil {
			c.removeEntry(key, cachePath, metaPath, fi.Size())
		} else {
			c.removeEntry(key, cachePath, metaPath, 0)
		}
		c.stats.Evictions++
	}

	if len(expiredKeys) > 0 {
		log.Printf("TTL eviction: removed %d expired entries", len(expiredKeys))
	}
}

// evictLRUIfNeeded checks if cache size exceeds limit and evicts LRU entries.
func (c *Cache) evictLRUIfNeeded() {
	for {
		c.mu.RLock()
		currentSize := c.stats.CurrentSizeBytes
		c.mu.RUnlock()

		if currentSize <= c.cfg.MaxSizeBytes {
			return
		}

		lru, ok := c.lruList.getLRU()
		if !ok {
			return // nothing to evict
		}

		c.mu.Lock()
		cachePath := c.keyPath(lru.Key)
		metaPath := c.metaPath(lru.Key)
		c.removeEntry(lru.Key, cachePath, metaPath, lru.Size)
		c.stats.Evictions++
		c.mu.Unlock()

		log.Printf("LRU eviction: removed %s (size=%d)", lru.Key, lru.Size)
	}
}

// removeEntry removes a cache entry (file + meta + LRU tracking).
// Caller must hold c.mu.
func (c *Cache) removeEntry(key, cachePath, metaPath string, size int64) {
	os.Remove(cachePath)
	os.Remove(metaPath)
	c.lruList.remove(key)
	c.stats.CurrentSizeBytes -= size
	if c.stats.CurrentSizeBytes < 0 {
		c.stats.CurrentSizeBytes = 0
	}
	if c.stats.EntryCount > 0 {
		c.stats.EntryCount--
	}
}

// Clear removes all cache entries (used for admin command).
func (c *Cache) Clear() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if err := os.RemoveAll(c.cfg.Dir); err != nil {
		return fmt.Errorf("failed to clear cache: %w", err)
	}

	if err := os.MkdirAll(c.cfg.Dir, cacheDirMode(c.cfg.Dir)); err != nil {
		return fmt.Errorf("failed to recreate cache directory: %w", err)
	}

	c.lruList = newLRUTracker()
	c.stats = CacheStats{}

	return nil
}

// loadMeta reads and parses a cache metadata file.
func (c *Cache) loadMeta(path string) (cacheEntryMeta, error) {
	var meta cacheEntryMeta

	data, err := os.ReadFile(path)
	if err != nil {
		return meta, err
	}

	if err := json.Unmarshal(data, &meta); err != nil {
		return meta, fmt.Errorf("failed to parse meta file %s: %w", path, err)
	}

	return meta, nil
}

// saveMeta writes cache metadata to a file.
func (c *Cache) saveMeta(path string, meta cacheEntryMeta) error {
	data, err := json.Marshal(meta)
	if err != nil {
		return err
	}

	if err := os.WriteFile(path, data, 0644); err != nil {
		return err
	}

	return nil
}

// copyFile copies a file from src to dst, preserving permissions.
func copyFile(src, dst string) error {
	srcFile, err := os.Open(src)
	if err != nil {
		return err
	}
	defer srcFile.Close()

	srcInfo, err := srcFile.Stat()
	if err != nil {
		return err
	}

	dstFile, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, srcInfo.Mode())
	if err != nil {
		return err
	}
	defer dstFile.Close()

	if _, err := io.Copy(dstFile, srcFile); err != nil {
		return err
	}

	return dstFile.Sync()
}
