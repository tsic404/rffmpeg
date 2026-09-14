package worker

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tsic404/rffmpeg/pkg/protocol"
)

// TestCacheHit_OverwritePolicyDegradesToMiss locks the cache-hit fix: when a
// cache hit would land on a pre-existing output file and the caller did not
// opt into -y (or passed -n), the cache-hit copy is skipped and the job
// degrades to a miss, so ffmpeg applies native overwrite semantics ("Not
// overwriting - exiting") instead of silently truncating via copyFile's
// O_TRUNC. The degradation is observable through the download counter: the
// cache-hit path never downloads inputs, the miss path re-downloads them.
// (The mock ffmpeg always overwrites, so content is not the signal here.)
func TestCacheHit_OverwritePolicyDegradesToMiss(t *testing.T) {
	w, mockSrv := setupTestWorker(t, 24*time.Hour)

	ctx := context.Background()
	jobCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Prime the cache: a miss run caches the output under key derived from
	// input files + args + output extension.
	prime := protocol.JobInfo{
		ID:             "tsi-2964-prime",
		InputFiles:     []string{"input-001"},
		Args:           []string{"-i", "<INPUT_FILE>", "-c:v", "libx264"},
		OutputFilename: "out.mp4",
	}
	w.processJob(jobCtx, prime, cancel, false)
	if w.cache.Stats().EntryCount != 1 {
		t.Fatalf("expected 1 cache entry after prime run, got %d", w.cache.Stats().EntryCount)
	}

	// Pre-existing absolute output that must not be silently truncated.
	existingOutput := filepath.Join(t.TempDir(), "out.mp4")
	if err := os.WriteFile(existingOutput, []byte("existing content"), 0o644); err != nil {
		t.Fatalf("write existing output: %v", err)
	}

	// Same cache key (same input/args/ext) but an absolute output path that
	// already exists, with no -y: must degrade to a cache miss.
	job := protocol.JobInfo{
		ID:             "tsi-2964-hit",
		InputFiles:     []string{"input-001"},
		Args:           []string{"-i", "<INPUT_FILE>", "-c:v", "libx264"},
		OutputFilename: existingOutput,
	}

	beforeDownloads := mockSrv.DownloadCount()
	w.processJob(jobCtx, job, cancel, false)

	if mockSrv.DownloadCount() == beforeDownloads {
		t.Errorf("expected cache hit to degrade to a miss (input re-downloaded) when output exists and overwrite not allowed")
	}
}
