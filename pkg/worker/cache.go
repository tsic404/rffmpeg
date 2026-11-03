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
	"syscall"
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
	evictCh chan struct{}
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

// sizeOf reports the tracked size of a key.
func (l *lruTracker) sizeOf(key string) (int64, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()

	idx, ok := l.keyIndex[key]
	if !ok {
		return 0, false
	}
	return l.entries[idx].Size, true
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

// cacheDirMode is the permission of every cache directory. Cached outputs are
// private worker artifacts, so neither group nor others may read or traverse
// them; the mode is enforced on directories the process did not create.
const cacheDirMode = os.FileMode(0o700)

// ensureCacheDir creates dir and validates an existing directory: the owner
// must be the effective user and the mode must be 0700. A directory left over
// from an older build (0755) is tightened to 0700; one owned by another user is
// an error, because writing cache entries into it would hand that user the
// transcoded outputs.
func ensureCacheDir(dir string) error {
	return ensureCacheDirAs(dir, os.Geteuid())
}

// ensureCacheDirAs is ensureCacheDir with the expected owner injected, so the
// ownership branch is testable without root (and therefore actually runs in
// CI).
func ensureCacheDirAs(dir string, euid int) error {
	if err := os.MkdirAll(dir, cacheDirMode); err != nil {
		return fmt.Errorf("failed to create cache directory %s: %w", dir, err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("failed to stat cache directory %s: %w", dir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("cache path %s is not a directory", dir)
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		if euid >= 0 && int(st.Uid) != euid {
			return fmt.Errorf("cache directory %s is owned by uid %d, want %d", dir, st.Uid, euid)
		}
	}
	if info.Mode().Perm()&0o077 != 0 {
		if err := os.Chmod(dir, cacheDirMode); err != nil {
			return fmt.Errorf("failed to restrict permissions on cache directory %s: %w", dir, err)
		}
	}
	return nil
}

// validCacheKey reports whether key is the canonical cache key form: 64
// lowercase hex characters (SHA-256). Keys reach keyPath from metadata files,
// so an unvalidated value could escape the cache directory via path
// separators or "..".
func validCacheKey(key string) bool {
	if len(key) != 64 {
		return false
	}
	for i := range len(key) {
		c := key[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// entryTTL is the TTL that governs one entry: the per-entry value when the
// entry recorded one (URL-derived inputs use a short TTL), otherwise the
// cache-wide default.
func (c *Cache) entryTTL(meta cacheEntryMeta) time.Duration {
	if meta.TTL > 0 {
		return meta.TTL
	}
	return c.cfg.TTL
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

	// Create the cache directory and validate an existing one (owner and mode).
	if err := ensureCacheDir(cfg.Dir); err != nil {
		return nil, err
	}

	c := &Cache{
		cfg:     cfg,
		lruList: newLRUTracker(),
		evictCh: make(chan struct{}, 1),
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

// keyPath returns the file path for a cache key (sharded by first 2 hex
// chars). It returns "" for a key that is not canonical 64-hex, so a value
// read from a metadata file can never traverse outside the cache directory.
func (c *Cache) keyPath(key string) string {
	if !validCacheKey(key) {
		return ""
	}
	return filepath.Join(c.cfg.Dir, key[:2], key)
}

// metaPath returns the metadata file path for a cache key.
func (c *Cache) metaPath(key string) string {
	p := c.keyPath(key)
	if p == "" {
		return ""
	}
	return p + ".meta"
}

// Check looks up a cache key. Returns (cachedFilePath, true) on hit.
func (c *Cache) Check(key string) (string, bool) {
	if !c.cfg.Enabled {
		return "", false
	}
	if !validCacheKey(key) {
		c.mu.Lock()
		c.stats.Misses++
		c.mu.Unlock()
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
	if time.Since(meta.CreatedAt) > c.entryTTL(meta) {
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
	if !validCacheKey(key) {
		return
	}
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
	if !validCacheKey(key) {
		return fmt.Errorf("invalid cache key %q", key)
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

	// Create shard directory if needed, with the same owner/permission
	// validation as the cache root.
	if err := ensureCacheDir(filepath.Join(c.cfg.Dir, key[:2])); err != nil {
		return fmt.Errorf("failed to create shard directory: %w", err)
	}

	// A fresh .tmp at this path (left by a crashed copy, counted by
	// scanExisting but absent from the LRU list) is truncated and reused by
	// copyFile below. Release its accounted bytes first, or the renamed entry
	// is counted twice and the ghost survives until the next restart.
	if fi, err := os.Stat(tmpPath); err == nil {
		c.stats.CurrentSizeBytes -= fi.Size()
		if c.stats.CurrentSizeBytes < 0 {
			c.stats.CurrentSizeBytes = 0
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

	// Track in LRU. Re-caching a key replaces the expired entry it overwrites,
	// so swap the accounted size instead of adding a second copy.
	if prev, ok := c.lruList.sizeOf(key); ok {
		c.stats.CurrentSizeBytes -= prev
	} else {
		c.stats.EntryCount++
	}
	c.lruList.add(key, meta.Size)
	c.stats.CurrentSizeBytes += meta.Size

	// Enforce the size budget right away instead of waiting for the periodic
	// tick: the cap must hold for writes that arrive between two checks.
	c.requestLRUEviction()

	return nil
}

// Stats returns a snapshot of the current cache statistics.
func (c *Cache) Stats() CacheStats {
	c.mu.RLock()
	defer c.mu.RUnlock()
	s := c.stats
	return s
}

// staleTempFileAge is how old a leftover ".tmp" file must be before the scan
// removes it. Put writes it immediately before the atomic rename, so anything
// this old is residue from a crashed copy, not a write in flight.
const staleTempFileAge = time.Hour

// scanExisting scans the cache directory and rebuilds LRU state. Leftover
// ".tmp" files count toward the cache size (they occupy the budget) and are
// removed once they are older than staleTempFileAge. Metadata whose key is not
// canonical hex is discarded instead of being trusted as a path.
func (c *Cache) scanExisting() error {
	var totalSize int64
	var count int
	tmpCutoff := time.Now().Add(-staleTempFileAge)

	err := filepath.Walk(c.cfg.Dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // skip errors
		}
		if info.IsDir() {
			if path != c.cfg.Dir {
				if err := ensureCacheDir(path); err != nil {
					log.Printf("Warning: %v", err)
				}
			}
			return nil
		}
		if strings.HasSuffix(path, ".meta") {
			meta, err := c.loadMeta(path)
			if err != nil {
				return nil
			}
			if !validCacheKey(meta.Key) {
				log.Printf("Warning: discarding cache metadata with invalid key: %s", path)
				os.Remove(path)
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
			return nil
		}
		if strings.HasSuffix(path, ".tmp") {
			if info.ModTime().Before(tmpCutoff) {
				os.Remove(path)
				return nil
			}
			totalSize += info.Size()
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

// runLRUEviction checks cache size on every write request and, as a backstop,
// on the periodic tick. The initial pass covers a cache that is already over
// budget at startup, e.g. after the size limit was lowered.
func (c *Cache) runLRUEviction() {
	defer c.wg.Done()

	ticker := time.NewTicker(c.cfg.LRUCheckInterval)
	defer ticker.Stop()

	c.evictLRUIfNeeded()

	for {
		select {
		case <-c.ctx.Done():
			return
		case <-c.evictCh:
			c.evictLRUIfNeeded()
		case <-ticker.C:
			c.evictLRUIfNeeded()
		}
	}
}

// requestLRUEviction asks the background eviction loop to re-check the size
// budget after a write. It never blocks and never drops the request when the
// loop is busy: a pending notification already covers every later write.
func (c *Cache) requestLRUEviction() {
	if c.evictCh == nil {
		return
	}
	select {
	case c.evictCh <- struct{}{}:
	default:
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
		if !validCacheKey(meta.Key) {
			log.Printf("Warning: discarding cache metadata with invalid key: %s", path)
			os.Remove(path)
			return nil
		}

		// Each entry ages out on its own TTL: URL-derived entries carry a
		// short one and must not wait for the cache-wide default.
		if time.Since(meta.CreatedAt) > c.entryTTL(meta) {
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

	// Leftover ".tmp" files count toward the size budget but are not in the
	// LRU list, so a crashed Put would otherwise hold budget forever and push
	// real entries out. The periodic TTL scan reclaims those older than the
	// in-flight window.
	tmpCutoff := time.Now().Add(-staleTempFileAge)
	var removedTmp int
	var reclaimed int64
	walkErr := filepath.Walk(c.cfg.Dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".tmp") {
			return nil
		}
		if info.ModTime().After(tmpCutoff) {
			return nil
		}
		if err := os.Remove(path); err != nil {
			return nil
		}
		removedTmp++
		reclaimed += info.Size()
		return nil
	})
	if walkErr != nil {
		log.Printf("Cache temp scan error: %v", walkErr)
	}
	if reclaimed > 0 {
		c.stats.CurrentSizeBytes -= reclaimed
		if c.stats.CurrentSizeBytes < 0 {
			c.stats.CurrentSizeBytes = 0
		}
		log.Printf("TTL cleanup: removed %d stale temp file(s), reclaimed %d bytes", removedTmp, reclaimed)
	}
}

// evictLRUIfNeeded checks if cache size exceeds limit and evicts LRU entries.
// The whole pass runs under c.mu so a concurrent Put cannot add an entry that
// the currently computed budget misses.
func (c *Cache) evictLRUIfNeeded() {
	c.mu.Lock()
	defer c.mu.Unlock()

	for c.stats.CurrentSizeBytes > c.cfg.MaxSizeBytes {
		lru, ok := c.lruList.getLRU()
		if !ok {
			return // nothing to evict
		}

		c.removeEntry(lru.Key, c.keyPath(lru.Key), c.metaPath(lru.Key), lru.Size)
		c.stats.Evictions++

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

	if err := ensureCacheDir(c.cfg.Dir); err != nil {
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
