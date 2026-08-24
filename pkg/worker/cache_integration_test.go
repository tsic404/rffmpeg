package worker

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tsix404/rffmpeg/pkg/protocol"
)

// mockJobServer is a test HTTP server that handles client API calls.
type mockJobServer struct {
	*httptest.Server

	downloadCount int32
	uploadCount   int32
	updateCount   int32

	lastCached     bool
	lastUpdateBody []byte

	inputData []byte
}

func newMockJobServer() *mockJobServer {
	m := &mockJobServer{
		inputData: []byte("mock input video data for testing"),
	}

	mux := http.NewServeMux()

	// GET /api/v1/files/{fileID} — DownloadInput
	mux.HandleFunc("/api/v1/files/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		atomic.AddInt32(&m.downloadCount, 1)
		w.WriteHeader(http.StatusOK)
		w.Write(m.inputData)
	})

	// PATCH /api/v1/jobs/{jobID} — UpdateJob, SendStderrChunk
	// POST /api/v1/jobs/{jobID}/output — UploadOutput
	mux.HandleFunc("/api/v1/jobs/", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case r.Method == http.MethodPatch:
			atomic.AddInt32(&m.updateCount, 1)
			body := make([]byte, r.ContentLength)
			r.Body.Read(body)
			m.lastUpdateBody = body
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"message":"ok"}`))
		case r.Method == http.MethodPost && len(path) > 6 && path[len(path)-7:] == "/output":
			atomic.AddInt32(&m.uploadCount, 1)
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"message":"ok"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	m.Server = httptest.NewServer(mux)
	return m
}

func (m *mockJobServer) DownloadCount() int32 { return atomic.LoadInt32(&m.downloadCount) }
func (m *mockJobServer) UploadCount() int32   { return atomic.LoadInt32(&m.uploadCount) }
func (m *mockJobServer) UpdateCount() int32   { return atomic.LoadInt32(&m.updateCount) }

// writeMockFFmpeg creates an executable shell script that simulates ffmpeg.
func writeMockFFmpeg(dir string) (string, error) {
	scriptPath := filepath.Join(dir, "mock-ffmpeg")
	script := `#!/bin/bash
# Mock ffmpeg for testing — writes a fake output file.
output=""
for arg in "$@"; do output="$arg"; done
mkdir -p "$(dirname "$output")" 2>/dev/null
echo "mock_transcoded_data_$(date +%s%N)" > "$output"
exit 0
`
	if err := os.WriteFile(scriptPath, []byte(script), 0755); err != nil {
		return "", fmt.Errorf("write mock ffmpeg: %w", err)
	}
	return scriptPath, nil
}

// setupTestWorker creates a Worker with a mock HTTP server, mock ffmpeg, and real cache.
func setupTestWorker(t *testing.T, cacheTTL time.Duration) (*Worker, *mockJobServer) {
	t.Helper()

	tmpDir := t.TempDir()

	ffmpegPath, err := writeMockFFmpeg(tmpDir)
	if err != nil {
		t.Fatalf("failed to create mock ffmpeg: %v", err)
	}

	mockSrv := newMockJobServer()

	cacheDir := filepath.Join(tmpDir, "cache")

	cache, err := NewCache(CacheConfig{
		Enabled: true,
		Dir:     cacheDir,
		TTL:     cacheTTL,
	})
	if err != nil {
		t.Fatalf("NewCache failed: %v", err)
	}
	t.Cleanup(func() { cache.Stop() })

	workerTempDir := filepath.Join(tmpDir, "worker-temp")
	if err := os.MkdirAll(workerTempDir, 0755); err != nil {
		t.Fatalf("mkdir worker-temp: %v", err)
	}

	executor := NewExecutor(ffmpegPath, 30*time.Second)
	client := NewClient(mockSrv.URL, uuid.New().String(), "")

	rewriteAdapter := NewRewriteAdapter()
	rewriteAdapter.config.Enabled = false

	retryExecutor := NewRetryExecutor(executor, DefaultRetryConfig())

	w := &Worker{
		id:                 "test-worker-001",
		name:               "test-worker",
		client:             client,
		executor:           executor,
		retryExecutor:      retryExecutor,
		rewriteAdapter:     rewriteAdapter,
		cache:              cache,
		tempDir:            workerTempDir,
		activeJobs:         make(map[string]context.CancelFunc),
		heartbeatInterval:  30 * time.Second,
		pollInterval:       5 * time.Second,
		lastHeartbeatTime:  time.Now(),
		ffprobeExecutor:    NewFFprobeExecutor(""),
		pixelFormatChecker: nil,
	}

	return w, mockSrv
}

// TestCacheIntegration_MissThenHit verifies the full cache miss→hit flow
// through the worker's processJob cycle:
//
//	First run: cache miss → executor runs → output cached
//	Second run: cache hit → returns from cache (no re-execute)
func TestCacheIntegration_MissThenHit(t *testing.T) {
	w, mockSrv := setupTestWorker(t, 24*time.Hour)

	ctx := context.Background()
	jobCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	job := protocol.JobInfo{
		ID:         "test-job-001",
		InputFiles: []string{"input-001"},
		Args:       []string{"-i", "<INPUT_FILE>", "-c:v", "libx264", "-preset", "fast"},
	}

	beforeDownloads := mockSrv.DownloadCount()
	beforeUploads := mockSrv.UploadCount()

	cacheKey := GenerateCacheKey(job.InputFiles, job.Args, job.AutoHW)
	_, hit := w.cache.Check(cacheKey)
	if hit {
		t.Fatal("Expected cache miss before first run")
	}
	cacheStatsBefore := w.cache.Stats()

	w.processJob(jobCtx, job, cancel)

	if mockSrv.DownloadCount() <= beforeDownloads {
		t.Error("Expected at least one download on first run (miss path)")
	}
	if mockSrv.UploadCount() <= beforeUploads {
		t.Error("Expected at least one upload on first run")
	}

	cacheStatsAfter := w.cache.Stats()
	if cacheStatsAfter.Misses <= cacheStatsBefore.Misses {
		t.Error("Expected at least one cache miss recorded")
	}
	if cacheStatsAfter.EntryCount != 1 {
		t.Errorf("Expected 1 cache entry after first run, got %d", cacheStatsAfter.EntryCount)
	}

	cachedPath, hit := w.cache.Check(cacheKey)
	if !hit {
		t.Fatal("Expected cache hit after first run")
	}
	if _, err := os.Stat(cachedPath); os.IsNotExist(err) {
		t.Errorf("Cached file should exist at %s", cachedPath)
	}

	// ---- Second run: cache hit ----
	beforeDownloads2 := mockSrv.DownloadCount()

	job2 := protocol.JobInfo{
		ID:         "test-job-002",
		InputFiles: []string{"input-001"},
		Args:       []string{"-i", "<INPUT_FILE>", "-c:v", "libx264", "-preset", "fast"},
	}

	w.processJob(jobCtx, job2, cancel)

	if mockSrv.DownloadCount() != beforeDownloads2 {
		t.Errorf("Expected no downloads on cache hit, got %d new downloads",
			mockSrv.DownloadCount()-beforeDownloads2)
	}

	finalStats := w.cache.Stats()
	if finalStats.Hits < 1 {
		t.Errorf("Expected at least 1 cache hit, got %d", finalStats.Hits)
	}

	t.Logf("Final cache stats: hits=%d misses=%d entries=%d",
		finalStats.Hits, finalStats.Misses, finalStats.EntryCount)
}

// TestCacheIntegration_TTLEviction verifies TTL eviction through processJob:
//
//	First run: cache miss → executor runs → output cached
//	After TTL: cache miss → re-executes → output re-cached
func TestCacheIntegration_TTLEviction(t *testing.T) {
	w, mockSrv := setupTestWorker(t, 100*time.Millisecond)

	ctx := context.Background()
	jobCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	job := protocol.JobInfo{
		ID:         "test-job-ttl-001",
		InputFiles: []string{"input-ttl-001"},
		Args:       []string{"-i", "<INPUT_FILE>", "-c:v", "libx264", "-preset", "fast"},
	}

	cacheKey := GenerateCacheKey(job.InputFiles, job.Args, job.AutoHW)

	_, hit := w.cache.Check(cacheKey)
	if hit {
		t.Fatal("Expected cache miss before first run")
	}

	w.processJob(jobCtx, job, cancel)

	_, hit = w.cache.Check(cacheKey)
	if !hit {
		t.Fatal("Expected cache hit immediately after first run")
	}

	// Wait for TTL to expire
	time.Sleep(150 * time.Millisecond)

	_, hit = w.cache.Check(cacheKey)
	if hit {
		t.Fatal("Expected cache miss after TTL expired")
	}

	// Second run: should be miss → re-execute → re-cache
	beforeDownloads2 := mockSrv.DownloadCount()
	beforeUploads2 := mockSrv.UploadCount()

	job2 := protocol.JobInfo{
		ID:         "test-job-ttl-002",
		InputFiles: []string{"input-ttl-001"},
		Args:       []string{"-i", "<INPUT_FILE>", "-c:v", "libx264", "-preset", "fast"},
	}

	w.processJob(jobCtx, job2, cancel)

	if mockSrv.DownloadCount() <= beforeDownloads2 {
		t.Error("Expected downloads on second run (TTL expired → miss)")
	}
	if mockSrv.UploadCount() <= beforeUploads2 {
		t.Error("Expected upload on second run (TTL expired → re-execute)")
	}

	_, hit = w.cache.Check(cacheKey)
	if !hit {
		t.Fatal("Expected cache hit after second run re-caches the output")
	}

	finalStats := w.cache.Stats()
	t.Logf("Final cache stats: hits=%d misses=%d entries=%d",
		finalStats.Hits, finalStats.Misses, finalStats.EntryCount)
}

// TestCacheIntegration_MultipleDifferentJobs verifies different inputs/args
// produce different cache keys and don't cross-interfere.
func TestCacheIntegration_MultipleDifferentJobs(t *testing.T) {
	w, mockSrv := setupTestWorker(t, 24*time.Hour)

	ctx := context.Background()
	jobCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	jobA := protocol.JobInfo{
		ID:         "test-job-a",
		InputFiles: []string{"input-a"},
		Args:       []string{"-i", "<INPUT_FILE>", "-c:v", "libx264"},
	}
	jobB := protocol.JobInfo{
		ID:         "test-job-b",
		InputFiles: []string{"input-a"},
		Args:       []string{"-i", "<INPUT_FILE>", "-c:v", "libx265"},
	}

	w.processJob(jobCtx, jobA, cancel)
	w.processJob(jobCtx, jobB, cancel)

	stats := w.cache.Stats()
	if stats.Misses < 2 {
		t.Errorf("Expected at least 2 misses, got %d", stats.Misses)
	}
	if stats.EntryCount != 2 {
		t.Errorf("Expected 2 cache entries, got %d", stats.EntryCount)
	}

	// Re-run Job A — cache hit
	beforeDownloads := mockSrv.DownloadCount()
	jobA2 := protocol.JobInfo{
		ID:         "test-job-a2",
		InputFiles: []string{"input-a"},
		Args:       []string{"-i", "<INPUT_FILE>", "-c:v", "libx264"},
	}
	w.processJob(jobCtx, jobA2, cancel)
	if mockSrv.DownloadCount() != beforeDownloads {
		t.Error("Expected no downloads on cache hit for job A re-run")
	}

	// Re-run Job B — cache hit
	beforeDownloads = mockSrv.DownloadCount()
	jobB2 := protocol.JobInfo{
		ID:         "test-job-b2",
		InputFiles: []string{"input-a"},
		Args:       []string{"-i", "<INPUT_FILE>", "-c:v", "libx265"},
	}
	w.processJob(jobCtx, jobB2, cancel)
	if mockSrv.DownloadCount() != beforeDownloads {
		t.Error("Expected no downloads on cache hit for job B re-run")
	}

	finalStats := w.cache.Stats()
	if finalStats.Hits < 2 {
		t.Errorf("Expected at least 2 hits, got %d", finalStats.Hits)
	}
	t.Logf("Final stats: hits=%d misses=%d entries=%d",
		finalStats.Hits, finalStats.Misses, finalStats.EntryCount)
}

// TestCacheIntegration_CanonicalizationHit verifies flag order canonicalization:
// same flags in different order → same cache key → cache hit.
func TestCacheIntegration_CanonicalizationHit(t *testing.T) {
	w, mockSrv := setupTestWorker(t, 24*time.Hour)

	ctx := context.Background()
	jobCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	job1 := protocol.JobInfo{
		ID:         "test-job-canon-1",
		InputFiles: []string{"input-canon"},
		Args:       []string{"-i", "<INPUT_FILE>", "-c:v", "libx264", "-preset", "fast"},
	}
	w.processJob(jobCtx, job1, cancel)

	beforeDownloads := mockSrv.DownloadCount()
	job2 := protocol.JobInfo{
		ID:         "test-job-canon-2",
		InputFiles: []string{"input-canon"},
		Args:       []string{"-i", "<INPUT_FILE>", "-preset", "fast", "-c:v", "libx264"},
	}
	w.processJob(jobCtx, job2, cancel)

	if mockSrv.DownloadCount() != beforeDownloads {
		t.Error("Expected no downloads on second run — canonicalization should produce same key")
	}

	stats := w.cache.Stats()
	if stats.Hits < 1 {
		t.Errorf("Expected at least 1 hit, got %d", stats.Hits)
	}
}
