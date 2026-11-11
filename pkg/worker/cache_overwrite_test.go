package worker

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tsic404/rffmpeg/pkg/protocol"
)

// TestCacheHit_OverwritePolicyDegradesToMiss locks the cache-hit guard: when the
// target file already exists inside the job workspace and the caller did not opt
// into -y (or passed -n), the cached copy is skipped and the job degrades to a
// miss, so ffmpeg applies native overwrite semantics ("Not overwriting -
// exiting") instead of silently truncating via copyFile's O_TRUNC.
//
// A re-dispatched job keeps its job ID, so its workspace can still hold the
// output of the interrupted attempt — that is exactly the reachable case: the
// cache-hit path reuses the workspace without clearing it. The degradation is
// observable through the download counter: the cache-hit path never downloads
// inputs, the miss path re-downloads them.
func TestCacheHit_OverwritePolicyDegradesToMiss(t *testing.T) {
	w, mockSrv := setupTestWorker(t, 24*time.Hour)

	input := testUploadID("cache-overwrite")
	prime := protocol.JobInfo{
		ID:             "cache-overwrite-prime",
		InputFiles:     []string{input},
		Args:           []string{"-i", "<INPUT_FILE>", "-c:v", "libx264"},
		OutputFilename: "out.mp4",
	}
	runJob(t, w, prime)
	if w.cache.Stats().EntryCount != 1 {
		t.Fatalf("expected 1 cache entry after prime run, got %d", w.cache.Stats().EntryCount)
	}

	// The second job hits the same cache key and finds its own earlier output
	// still in the workspace (same job ID, re-dispatch), with no -y.
	hit := protocol.JobInfo{
		ID:             "cache-overwrite-hit",
		InputFiles:     []string{input},
		Args:           []string{"-i", "<INPUT_FILE>", "-c:v", "libx264"},
		OutputFilename: "out.mp4",
	}
	jobDir := filepath.Join(w.tempDir, hit.ID)
	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		t.Fatalf("mkdir job dir: %v", err)
	}
	defer os.RemoveAll(jobDir)
	if err := os.WriteFile(filepath.Join(jobDir, "out.mp4"), []byte("existing content"), 0o644); err != nil {
		t.Fatalf("write existing output: %v", err)
	}

	beforeDownloads := mockSrv.DownloadCount()
	runJob(t, w, hit)

	if mockSrv.DownloadCount() == beforeDownloads {
		t.Errorf("expected cache hit to degrade to a miss (input re-downloaded) when the workspace output exists and overwrite is not allowed")
	}
}
