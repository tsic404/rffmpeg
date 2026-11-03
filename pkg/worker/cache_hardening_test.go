package worker

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestCacheTightensExistingDirMode covers a cache directory left behind by an
// older build: it must be usable but end up private to its owner.
func TestCacheTightensExistingDirMode(t *testing.T) {
	cacheDir := filepath.Join(t.TempDir(), "cache")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(cacheDir, 0o755); err != nil {
		t.Fatal(err)
	}

	cache, err := NewCache(CacheConfig{Enabled: true, Dir: cacheDir})
	if err != nil {
		t.Fatalf("NewCache: %v", err)
	}
	defer cache.Stop()

	info, err := os.Stat(cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Fatalf("cache dir mode = %o, want 0700", got)
	}
}

// TestCacheShardDirIsPrivate covers the per-key shard directory created on
// first write: it must carry the same private mode as the cache root.
func TestCacheShardDirIsPrivate(t *testing.T) {
	dir := t.TempDir()
	cacheDir := filepath.Join(dir, "cache")
	cache, err := NewCache(CacheConfig{Enabled: true, Dir: cacheDir, TTL: time.Hour})
	if err != nil {
		t.Fatalf("NewCache: %v", err)
	}
	defer cache.Stop()

	src := filepath.Join(dir, "out.bin")
	if err := os.WriteFile(src, []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	key := GenerateCacheKey([]string{"shard"}, []string{"-c:v", "libx264"}, false, "", "")
	if err := cache.Put(key, src); err != nil {
		t.Fatalf("Put: %v", err)
	}

	info, err := os.Stat(filepath.Join(cacheDir, key[:2]))
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Fatalf("shard dir mode = %o, want 0700", got)
	}
}

// TestNewCacheRejectsForeignOwnedDir covers the ownership half of the directory
// check. It needs root to hand a directory to another uid.
func TestNewCacheRejectsForeignOwnedDir(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root to chown a directory to a foreign uid")
	}
	cacheDir := filepath.Join(t.TempDir(), "cache")
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(cacheDir, 65534, 65534); err != nil {
		t.Fatalf("chown: %v", err)
	}

	if _, err := NewCache(CacheConfig{Enabled: true, Dir: cacheDir}); err == nil {
		t.Fatal("NewCache accepted a cache directory owned by another uid")
	}
}

// TestEvictExpiredUsesPerEntryTTL covers the URL-entry case: an entry recorded
// with a one-hour TTL must be reclaimed on the hour, not on the cache-wide
// default that is twenty-four times longer.
func TestEvictExpiredUsesPerEntryTTL(t *testing.T) {
	dir := t.TempDir()
	cacheDir := filepath.Join(dir, "cache")
	cache, err := NewCache(CacheConfig{Enabled: true, Dir: cacheDir, TTL: 24 * time.Hour})
	if err != nil {
		t.Fatalf("NewCache: %v", err)
	}
	defer cache.Stop()

	src := filepath.Join(dir, "out.bin")
	if err := os.WriteFile(src, []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	longKey := GenerateCacheKey([]string{"local-input"}, []string{"-c:v", "libx264"}, false, "", "")
	urlKey := GenerateCacheKey([]string{"https://example.test/in.mp4"}, []string{"-c:v", "libx264"}, false, "", "")
	if err := cache.Put(longKey, src); err != nil {
		t.Fatalf("Put long: %v", err)
	}
	if err := cache.Put(urlKey, src, time.Hour); err != nil {
		t.Fatalf("Put url: %v", err)
	}

	// Age both entries by two hours: past the one-hour entry TTL, well inside
	// the twenty-four-hour default.
	old := time.Now().Add(-2 * time.Hour)
	for _, key := range []string{longKey, urlKey} {
		metaPath := cache.metaPath(key)
		meta, err := cache.loadMeta(metaPath)
		if err != nil {
			t.Fatal(err)
		}
		meta.CreatedAt = old
		if err := cache.saveMeta(metaPath, meta); err != nil {
			t.Fatal(err)
		}
	}

	cache.evictExpired()

	if _, ok := cache.lruList.sizeOf(urlKey); ok {
		t.Error("URL entry with a one-hour TTL survived eviction")
	}
	if _, err := os.Stat(cache.keyPath(urlKey)); !os.IsNotExist(err) {
		t.Errorf("URL entry file still present: err=%v", err)
	}
	if _, ok := cache.lruList.sizeOf(longKey); !ok {
		t.Error("entry on the default TTL was evicted early")
	}
}

// TestScanExistingAccountsAndCleansTempFiles covers leftover ".tmp" files: a
// fresh one occupies the size budget (it is disk usage), while a stale one is
// residue from a crashed copy and is removed at scan time.
func TestScanExistingAccountsAndCleansTempFiles(t *testing.T) {
	cacheDir := filepath.Join(t.TempDir(), "cache")
	shard := filepath.Join(cacheDir, "ab")
	if err := os.MkdirAll(shard, 0o700); err != nil {
		t.Fatal(err)
	}

	fresh := filepath.Join(shard, "fresh.tmp")
	if err := os.WriteFile(fresh, make([]byte, 10), 0o600); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(shard, "stale.tmp")
	if err := os.WriteFile(stale, make([]byte, 20), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}

	cache, err := NewCache(CacheConfig{Enabled: true, Dir: cacheDir})
	if err != nil {
		t.Fatalf("NewCache: %v", err)
	}
	defer cache.Stop()

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale .tmp still present: err=%v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("fresh .tmp was removed: %v", err)
	}
	if got, want := cache.Stats().CurrentSizeBytes, int64(10); got != want {
		t.Errorf("CurrentSizeBytes = %d, want %d (fresh .tmp counts)", got, want)
	}
}

// TestCacheRejectsNonHexKey covers metadata-supplied keys: a key that is not
// canonical hex must never be turned into a path, or it could write outside
// the cache directory.
func TestCacheRejectsNonHexKey(t *testing.T) {
	dir := t.TempDir()
	cacheDir := filepath.Join(dir, "cache")
	cache, err := NewCache(CacheConfig{Enabled: true, Dir: cacheDir})
	if err != nil {
		t.Fatalf("NewCache: %v", err)
	}
	defer cache.Stop()

	src := filepath.Join(dir, "out.bin")
	if err := os.WriteFile(src, []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}

	bad := "../../escaped"
	if err := cache.Put(bad, src); err == nil {
		t.Fatal("Put accepted a non-hex key")
	}
	if _, hit := cache.Check(bad); hit {
		t.Fatal("Check reported a hit for a non-hex key")
	}
	if _, err := os.Stat(filepath.Join(dir, "escaped")); !os.IsNotExist(err) {
		t.Errorf("non-hex key escaped the cache directory: err=%v", err)
	}

	// A metadata file naming a non-hex key must be discarded, not resolved.
	metaDir := filepath.Join(cacheDir, "cd")
	if err := os.MkdirAll(metaDir, 0o700); err != nil {
		t.Fatal(err)
	}
	metaPath := filepath.Join(metaDir, "poison.meta")
	if err := os.WriteFile(metaPath, []byte(`{"key":"../../escaped","created_at":"2026-01-01T00:00:00Z","size":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := cache.scanExisting(); err != nil {
		t.Fatalf("scanExisting: %v", err)
	}
	if _, err := os.Stat(metaPath); !os.IsNotExist(err) {
		t.Errorf("metadata with non-hex key survived the scan: err=%v", err)
	}
}
