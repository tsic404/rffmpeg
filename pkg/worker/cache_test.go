package worker

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tsic404/rffmpeg/pkg/worker/workerconfig"
)

func TestGenerateCacheKey_Deterministic(t *testing.T) {
	inputSources := []string{"file-a", "file-b"}
	args := []string{"-c:v", "libx264", "-preset", "fast", "-b:v", "2M"}

	key1 := GenerateCacheKey(inputSources, args, false, "", "")
	key2 := GenerateCacheKey(inputSources, args, false, "", "")

	if key1 != key2 {
		t.Errorf("GenerateCacheKey is not deterministic: %s != %s", key1[:16], key2[:16])
	}
}

func TestGenerateCacheKey_DifferentInputs(t *testing.T) {
	args := []string{"-c:v", "libx264"}

	key1 := GenerateCacheKey([]string{"file-a"}, args, false, "", "")
	key2 := GenerateCacheKey([]string{"file-b"}, args, false, "", "")

	if key1 == key2 {
		t.Error("Different inputs should produce different keys")
	}
}

func TestGenerateCacheKey_DifferentArgs(t *testing.T) {
	sources := []string{"file-a"}

	key1 := GenerateCacheKey(sources, []string{"-c:v", "libx264"}, false, "", "")
	key2 := GenerateCacheKey(sources, []string{"-c:v", "libx265"}, false, "", "")

	if key1 == key2 {
		t.Error("Different args should produce different keys")
	}
}

// TSI-2352: a --auto-hw run upgrades the encoder (e.g. libx264 → h264_qsv), so its
// cached output must never be served to a request without --auto-hw (and vice versa).
func TestGenerateCacheKey_DifferentAutoHW(t *testing.T) {
	sources := []string{"file-a"}
	args := []string{"-c:v", "libx264"}

	keyNoHW := GenerateCacheKey(sources, args, false, "", "")
	keyHW := GenerateCacheKey(sources, args, true, "", "")

	if keyNoHW == keyHW {
		t.Error("auto_hw flag must produce different cache keys")
	}
}

func TestGenerateCacheKey_OrderSensitivity(t *testing.T) {
	sources := []string{"file-a"}

	// Different flag order should produce different keys
	// (order-sensitive semantics like -ss before -i vs after -i)
	args1 := []string{"-preset", "fast", "-c:v", "libx264"}
	args2 := []string{"-c:v", "libx264", "-preset", "fast"}

	key1 := GenerateCacheKey(sources, args1, false, "", "")
	key2 := GenerateCacheKey(sources, args2, false, "", "")

	if key1 == key2 {
		t.Error("Different flag order should produce different keys")
	}
}

func TestGenerateCacheKey_ShardingFormat(t *testing.T) {
	key := GenerateCacheKey([]string{"test"}, []string{"-c:v", "libx264"}, false, "", "")

	if len(key) != 64 {
		t.Errorf("Key should be 64 hex chars (SHA-256), got %d", len(key))
	}
}

func TestCacheConfigDefaults(t *testing.T) {
	cfg := DefaultCacheConfig()

	if !cfg.Enabled {
		t.Error("Default should have cache enabled")
	}
	if cfg.Dir != workerconfig.DefaultCacheDir() {
		t.Errorf("Default Dir should be %s, got %s", workerconfig.DefaultCacheDir(), cfg.Dir)
	}
	if cfg.TTL != 24*time.Hour {
		t.Errorf("Default TTL should be 24h, got %v", cfg.TTL)
	}
}

func TestCacheNewDisabled(t *testing.T) {
	cache, err := NewCache(CacheConfig{Enabled: false})
	if err != nil {
		t.Fatalf("NewCache with disabled should not error: %v", err)
	}
	if cache == nil {
		t.Fatal("NewCache returned nil")
	}
}

func TestCacheNewEnabled(t *testing.T) {
	dir := t.TempDir()
	cacheDir := filepath.Join(dir, "cache")

	cache, err := NewCache(CacheConfig{
		Enabled: true,
		Dir:     cacheDir,
	})
	if err != nil {
		t.Fatalf("NewCache failed: %v", err)
	}
	defer cache.Stop()

	if _, err := os.Stat(cacheDir); os.IsNotExist(err) {
		t.Error("Cache directory should have been created")
	}
}

func TestCachePutAndCheck(t *testing.T) {
	dir := t.TempDir()
	cacheDir := filepath.Join(dir, "cache")

	cache, err := NewCache(CacheConfig{
		Enabled: true,
		Dir:     cacheDir,
		TTL:     24 * time.Hour,
	})
	if err != nil {
		t.Fatalf("NewCache failed: %v", err)
	}
	defer cache.Stop()

	// Create a test file
	srcPath := filepath.Join(dir, "source.bin")
	srcData := []byte("test video output data")
	if err := os.WriteFile(srcPath, srcData, 0644); err != nil {
		t.Fatalf("Failed to write source: %v", err)
	}

	key := GenerateCacheKey([]string{"test-input-1"}, []string{"-c:v", "libx264"}, false, "", "")

	// Check before put - should miss
	cachedPath, hit := cache.Check(key)
	if hit {
		t.Error("Should be cache miss before Put")
	}

	// Put the file
	if err := cache.Put(key, srcPath); err != nil {
		t.Fatalf("Put failed: %v", err)
	}

	// Check after put - should hit
	cachedPath, hit = cache.Check(key)
	if !hit {
		t.Fatal("Should be cache hit after Put")
	}

	// Verify cached file exists and matches
	cachedData, err := os.ReadFile(cachedPath)
	if err != nil {
		t.Fatalf("Failed to read cached file: %v", err)
	}
	if string(cachedData) != string(srcData) {
		t.Error("Cached data doesn't match source")
	}

	// Check sharding
	shardDir := filepath.Join(cacheDir, key[:2])
	if _, err := os.Stat(shardDir); os.IsNotExist(err) {
		t.Errorf("Shard directory %s should exist", shardDir)
	}

	cache.ConfirmHit(key)
	// Verify stats
	stats := cache.Stats()
	if stats.Hits != 1 {
		t.Errorf("Expected 1 hit, got hits=%d", stats.Hits)
	}
	if stats.Misses != 1 {
		t.Errorf("Expected 1 miss, got misses=%d", stats.Misses)
	}
	if stats.HitRate() != 0.5 {
		t.Errorf("Expected hit rate 0.5, got %f", stats.HitRate())
	}
}

func TestCacheTTLExpiration(t *testing.T) {
	dir := t.TempDir()
	cacheDir := filepath.Join(dir, "cache")

	cache, err := NewCache(CacheConfig{
		Enabled: true,
		Dir:     cacheDir,
		TTL:     50 * time.Millisecond, // Very short TTL for testing
	})
	if err != nil {
		t.Fatalf("NewCache failed: %v", err)
	}
	defer cache.Stop()

	// Create and cache a file
	srcPath := filepath.Join(dir, "source.bin")
	if err := os.WriteFile(srcPath, []byte("data"), 0644); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	key := GenerateCacheKey([]string{"test-ttl"}, []string{"-c:v", "libx264"}, false, "", "")
	if err := cache.Put(key, srcPath); err != nil {
		t.Fatalf("Put failed: %v", err)
	}

	// Immediately check - should hit
	_, hit := cache.Check(key)
	if !hit {
		t.Error("Should be hit immediately after Put")
	}

	// Wait for TTL to expire
	time.Sleep(100 * time.Millisecond)

	// Check after TTL - should miss
	_, hit = cache.Check(key)
	if hit {
		t.Error("Should be miss after TTL expired")
	}
}

func TestCacheLRUEviction(t *testing.T) {
	dir := t.TempDir()
	cacheDir := filepath.Join(dir, "cache")

	// Set a very small max size to trigger LRU
	cache, err := NewCache(CacheConfig{
		Enabled:      true,
		Dir:          cacheDir,
		TTL:          24 * time.Hour,
		MaxSizeBytes: 50, // Very small - only allow ~50 bytes total
	})
	if err != nil {
		t.Fatalf("NewCache failed: %v", err)
	}
	defer cache.Stop()

	// Create test data
	data := []byte("this is some test data that takes space")

	// Put first entry
	srcPath1 := filepath.Join(dir, "src1.bin")
	if err := os.WriteFile(srcPath1, data, 0644); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}
	key1 := GenerateCacheKey([]string{"file-1"}, []string{"-c:v", "libx264"}, false, "", "")
	if err := cache.Put(key1, srcPath1); err != nil {
		t.Fatalf("Put key1 failed: %v", err)
	}

	// Put second entry - this should trigger LRU eviction of the first
	srcPath2 := filepath.Join(dir, "src2.bin")
	if err := os.WriteFile(srcPath2, data, 0644); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}
	key2 := GenerateCacheKey([]string{"file-2"}, []string{"-c:v", "libx265"}, false, "", "")
	if err := cache.Put(key2, srcPath2); err != nil {
		t.Fatalf("Put key2 failed: %v", err)
	}

	// Trigger LRU eviction check
	cache.evictLRUIfNeeded()

	// Check that key2 is still cached
	_, hit := cache.Check(key2)
	if !hit {
		t.Error("key2 should still be in cache")
	}
}

func TestCacheClear(t *testing.T) {
	dir := t.TempDir()
	cacheDir := filepath.Join(dir, "cache")

	cache, err := NewCache(CacheConfig{
		Enabled: true,
		Dir:     cacheDir,
	})
	if err != nil {
		t.Fatalf("NewCache failed: %v", err)
	}
	defer cache.Stop()

	// Put a file
	srcPath := filepath.Join(dir, "source.bin")
	if err := os.WriteFile(srcPath, []byte("data"), 0644); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	key := GenerateCacheKey([]string{"test-clear"}, []string{"-c:v", "libx264"}, false, "", "")
	if err := cache.Put(key, srcPath); err != nil {
		t.Fatalf("Put failed: %v", err)
	}

	// Clear
	if err := cache.Clear(); err != nil {
		t.Fatalf("Clear failed: %v", err)
	}

	// Check after clear - should miss
	_, hit := cache.Check(key)
	if hit {
		t.Error("Should be miss after Clear")
	}

	stats := cache.Stats()
	if stats.EntryCount != 0 {
		t.Errorf("EntryCount should be 0 after clear, got %d", stats.EntryCount)
	}
}

func TestCacheDisabledCheckAlwaysMiss(t *testing.T) {
	cache, err := NewCache(CacheConfig{Enabled: false})
	if err != nil {
		t.Fatalf("NewCache failed: %v", err)
	}

	key := GenerateCacheKey([]string{"test"}, []string{"-c:v", "libx264"}, false, "", "")
	_, hit := cache.Check(key)
	if hit {
		t.Error("Disabled cache should always miss")
	}
}

func TestCacheDisabledPutNoError(t *testing.T) {
	cache, err := NewCache(CacheConfig{Enabled: false})
	if err != nil {
		t.Fatalf("NewCache failed: %v", err)
	}

	key := GenerateCacheKey([]string{"test"}, []string{"-c:v", "libx264"}, false, "", "")
	if err := cache.Put(key, "/nonexistent/path"); err != nil {
		t.Errorf("Put on disabled cache should not error: %v", err)
	}
}

func TestCacheStats(t *testing.T) {
	dir := t.TempDir()
	cacheDir := filepath.Join(dir, "cache")

	cache, err := NewCache(CacheConfig{
		Enabled: true,
		Dir:     cacheDir,
	})
	if err != nil {
		t.Fatalf("NewCache failed: %v", err)
	}
	defer cache.Stop()

	// Initial stats
	stats := cache.Stats()
	if stats.Hits != 0 || stats.Misses != 0 {
		t.Error("Initial stats should be all zeros")
	}

	// Miss
	key := GenerateCacheKey([]string{"stats-test"}, []string{"-c:v", "libx264"}, false, "", "")
	cache.Check(key)
	cache.ConfirmHit(key)
	stats = cache.Stats()
	if stats.Misses != 1 {
		t.Errorf("Expected 1 miss, got %d", stats.Misses)
	}

	// Put and hit
	srcPath := filepath.Join(dir, "src.bin")
	os.WriteFile(srcPath, []byte("data"), 0644)
	cache.Put(key, srcPath)

	cache.Check(key)
	stats = cache.Stats()
	if stats.Hits != 1 {
		t.Errorf("Expected 1 hit, got %d", stats.Hits)
	}
}

func TestCacheConcurrentAccess(t *testing.T) {
	dir := t.TempDir()
	cacheDir := filepath.Join(dir, "cache")

	cache, err := NewCache(CacheConfig{
		Enabled: true,
		Dir:     cacheDir,
		TTL:     24 * time.Hour,
	})
	if err != nil {
		t.Fatalf("NewCache failed: %v", err)
	}
	defer cache.Stop()

	srcPath := filepath.Join(dir, "src.bin")
	os.WriteFile(srcPath, []byte("concurrent test data"), 0644)

	key := GenerateCacheKey([]string{"concurrent"}, []string{"-c:v", "libx264"}, false, "", "")
	cache.Put(key, srcPath)

	// Run concurrent checks
	done := make(chan bool)
	for i := 0; i < 10; i++ {
		go func() {
			for j := 0; j < 100; j++ {
				cache.Check(key)
				cache.ConfirmHit(key)
			}
			done <- true
		}()
	}

	// Wait for all goroutines
	for i := 0; i < 10; i++ {
		<-done
	}

	stats := cache.Stats()
	totalHits := stats.Hits
	if totalHits != 1000 {
		t.Errorf("Expected 1000 hits, got %d", totalHits)
	}
}

func TestCacheStartStop(t *testing.T) {
	dir := t.TempDir()
	cacheDir := filepath.Join(dir, "cache")

	cache, err := NewCache(CacheConfig{
		Enabled:         true,
		Dir:             cacheDir,
		TTL:             100 * time.Millisecond,
		TTLScanInterval: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewCache failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	// Start background eviction
	cache.Start(ctx)

	// Stop should not panic
	cancel()
	cache.Stop()
}

func TestCanonicalizeArgs_OrderPreserved(t *testing.T) {
	// Different flag order should produce different strings
	result1 := canonicalizeArgs([]string{"-preset", "fast", "-c:v", "libx264"})
	result2 := canonicalizeArgs([]string{"-c:v", "libx264", "-preset", "fast"})

	if result1 == result2 {
		t.Error("Different flag order should produce different canonicalized strings")
	}
}

func TestCanonicalizeArgs_BooleanFlags(t *testing.T) {
	result := canonicalizeArgs([]string{"-y", "-benchmark", "-i", "input.mp4", "-vn"})
	if result == "" {
		t.Error("Expected non-empty canonicalized args")
	}
}

func TestCanonicalizeArgs_EmptyArgs(t *testing.T) {
	result := canonicalizeArgs([]string{})
	if result != "" {
		t.Errorf("Empty args should produce empty string, got: %s", result)
	}
}

func TestLRUTracker(t *testing.T) {
	lt := newLRUTracker()

	// Add entries
	lt.add("key1", 100)
	lt.add("key2", 200)
	lt.add("key3", 300)

	if lt.len() != 3 {
		t.Errorf("Expected 3 entries, got %d", lt.len())
	}

	// Touch key1
	lt.touch("key1")

	// Get LRU - key2 should be oldest (key3 just added, key1 just touched)
	lru, ok := lt.getLRU()
	if !ok {
		t.Fatal("Should have LRU entry")
	}
	if lru.Key != "key2" {
		t.Errorf("Expected LRU key2, got %s", lru.Key)
	}

	// Remove key2
	lt.remove("key2")
	if lt.len() != 2 {
		t.Errorf("Expected 2 entries after remove, got %d", lt.len())
	}

	// Total size
	if lt.totalSize() != 400 {
		t.Errorf("Expected total size 400, got %d", lt.totalSize())
	}
}

func TestLRUTrackerEmpty(t *testing.T) {
	lt := newLRUTracker()

	_, ok := lt.getLRU()
	if ok {
		t.Error("Empty LRU should return false")
	}

	if lt.len() != 0 {
		t.Error("Empty LRU should have length 0")
	}
}

func TestGenerateCacheKey_DifferentOutputExt(t *testing.T) {
	sources := []string{"file-a"}
	args := []string{"-c:v", "libx264"}

	key1 := GenerateCacheKey(sources, args, false, ".mp4", "")
	key2 := GenerateCacheKey(sources, args, false, ".mkv", "")

	if key1 == key2 {
		t.Error("Different output extensions should produce different keys")
	}
}

func TestGenerateCacheKey_DifferentEncoder(t *testing.T) {
	sources := []string{"file-a"}
	args := []string{"-c:v", "libx264"}

	key1 := GenerateCacheKey(sources, args, false, ".mp4", "libx264")
	key2 := GenerateCacheKey(sources, args, false, ".mp4", "h264_qsv")

	if key1 == key2 {
		t.Error("Different encoders should produce different keys")
	}
}

func TestCachePerEntryTTL(t *testing.T) {
	dir := t.TempDir()
	cacheDir := filepath.Join(dir, "cache")

	cache, err := NewCache(CacheConfig{
		Enabled: true,
		Dir:     cacheDir,
		TTL:     24 * time.Hour,
	})
	if err != nil {
		t.Fatalf("NewCache failed: %v", err)
	}
	defer cache.Stop()

	srcPath := filepath.Join(dir, "source.bin")
	if err := os.WriteFile(srcPath, []byte("test data"), 0644); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	key := GenerateCacheKey([]string{"test-ttl-entry"}, []string{"-c:v", "libx264"}, false, "", "")

	// Put with a very short per-entry TTL (100ms)
	if err := cache.Put(key, srcPath, 100*time.Millisecond); err != nil {
		t.Fatalf("Put failed: %v", err)
	}

	// Should hit immediately
	if _, hit := cache.Check(key); !hit {
		t.Error("Expected hit immediately after Put with short TTL")
	}

	// Wait for TTL to expire
	time.Sleep(200 * time.Millisecond)

	// Should miss after TTL expiry
	if _, hit := cache.Check(key); hit {
		t.Error("Expected miss after short TTL expiry")
	}
}

func TestCacheConfirmHitDoesNotIncrementWithoutCall(t *testing.T) {
	dir := t.TempDir()
	cacheDir := filepath.Join(dir, "cache")

	cache, err := NewCache(CacheConfig{
		Enabled: true,
		Dir:     cacheDir,
		TTL:     24 * time.Hour,
	})
	if err != nil {
		t.Fatalf("NewCache failed: %v", err)
	}
	defer cache.Stop()

	srcPath := filepath.Join(dir, "source.bin")
	if err := os.WriteFile(srcPath, []byte("test data"), 0644); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	key := GenerateCacheKey([]string{"test-confirm"}, []string{"-c:v", "libx264"}, false, "", "")
	cache.Put(key, srcPath)

	// Check returns true but does NOT increment Hits
	if _, hit := cache.Check(key); !hit {
		t.Fatal("Expected hit from Check")
	}

	// Without ConfirmHit, Hits should be 0
	stats := cache.Stats()
	if stats.Hits != 0 {
		t.Errorf("Expected 0 hits without ConfirmHit, got %d", stats.Hits)
	}

	// After ConfirmHit, Hits should be 1
	cache.ConfirmHit(key)
	stats = cache.Stats()
	if stats.Hits != 1 {
		t.Errorf("Expected 1 hit after ConfirmHit, got %d", stats.Hits)
	}
}

func TestNewCacheFallbackDirMode(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	fallback := workerconfig.FallbackCacheDir(os.TempDir(), os.Getuid())

	c, err := NewCache(CacheConfig{Enabled: true, Dir: fallback})
	if err != nil {
		t.Fatalf("NewCache: %v", err)
	}

	info, err := os.Stat(c.cfg.Dir)
	if err != nil {
		t.Fatalf("stat %s: %v", c.cfg.Dir, err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Errorf("fallback cache dir mode = %o, want 0700", got)
	}
}
