package worker

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tsic404/rffmpeg/pkg/protocol"
)

// TestCacheHitMkdirAllFailureDoesNotLeakBatcher is the TSI-2415 review-fix
// regression test: when os.MkdirAll fails on the cache-hit path, the early
// return must Close() the hit-path StderrBatcher.
//
// Leak mechanism (verified in TestStderrBatcherCloseStopsTimer): timedFlush
// re-arms its timer unconditionally whenever the batcher context is alive,
// even with nothing to flush. Skipping Close therefore pins an eternally
// armed timer (one runtime wakeup every batchDelay) to an unreachable
// batcher, per failed job, for the life of the worker. time.AfterFunc lives
// on the runtime timer heap — not in a parked goroutine — so a goroutine
// count cannot observe this; the deterministic observable is the batcher
// contract itself: after processJob returns, the context must be cancelled
// (exactly what Close guarantees).
func TestCacheHitMkdirAllFailureDoesNotLeakBatcher(t *testing.T) {
	w, _ := setupTestWorker(t, 24*time.Hour)

	ctx := context.Background()
	jobCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var leaked *StderrBatcher
	prevHook := batcherCreateHook
	batcherCreateHook = func(b *StderrBatcher) { leaked = b }
	t.Cleanup(func() { batcherCreateHook = prevHook })

	job := protocol.JobInfo{
		ID:         "tsi-2415-leak",
		InputFiles: []string{"input-001"},
		Args:       []string{"-i", "<INPUT_FILE>", "-c:v", "libx264", "-preset", "fast"},
	}

	// First run: cache miss → execute → output cached.
	w.processJob(jobCtx, job, cancel, false)
	if w.cache.Stats().EntryCount != 1 {
		t.Fatalf("expected 1 cache entry after first run, got %d", w.cache.Stats().EntryCount)
	}

	// Make MkdirAll fail for the hit path: replace tempDir with a regular
	// file so filepath.Join(tempDir, job.ID) cannot be created as a dir.
	tmpFile := filepath.Join(t.TempDir(), "tempdir-as-file")
	if err := os.WriteFile(tmpFile, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	w.tempDir = tmpFile

	job2 := protocol.JobInfo{
		ID:         "tsi-2415-leak-hit",
		InputFiles: []string{"input-001"},
		Args:       []string{"-i", "<INPUT_FILE>", "-c:v", "libx264", "-preset", "fast"},
	}
	w.processJob(jobCtx, job2, cancel, false)

	// TSI-2666 review-fix: the failed cache-hit run must not advance the
	// completion counters. The first run was a real success (total=1); the
	// second failing run must leave totalJobsCompleted untouched at 1.
	// jobsCompleted is a per-heartbeat-interval counter that the idle
	// heartbeat resets, so only the lifetime counter is asserted here.
	if _, total := readWorkerCounters(w); total != 1 {
		t.Errorf("totalJobsCompleted advanced on cache-hit MkdirAll failure: got %d, want 1", total)
	}

	if leaked == nil {
		t.Fatal("cache-hit path did not create a StderrBatcher; hook not invoked")
	}
	if leaked.ctx.Err() == nil {
		t.Fatal("hit-path batcher leaked: context still alive after processJob returned (Close was skipped on the MkdirAll-failure path)")
	}
}

// TestStderrBatcherCloseStopsTimer pins the leak mechanism at unit level:
// while alive, timedFlush re-arms unconditionally (even with nothing to
// flush); after Close, sustained re-arming stops. Sustained activity is
// sampled via Stop() — which does not re-arm — so the probe cannot
// self-poison the observation.
func TestStderrBatcherCloseStopsTimer(t *testing.T) {
	var sends int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			StderrChunk string `json:"stderr_chunk,omitempty"`
		}
		_ = json.Unmarshal(body, &req)
		if req.StderrChunk != "" {
			atomic.AddInt32(&sends, 1)
		}
	}))
	defer ts.Close()

	client := NewClient(ts.URL, "test-worker", "")
	b := NewStderrBatcher("leak-unit", client, DefaultStderrBatcherConfig())

	// While alive with undrained chunks, the natural timer must deliver them.
	b.Add("[rffmpeg] tick\n")
	time.Sleep(2 * DefaultStderrBatcherConfig().BatchDelay)
	if atomic.LoadInt32(&sends) == 0 {
		t.Fatal("expected the natural timed flush to deliver the chunk")
	}
	b.Close()

	if b.ctx.Err() == nil {
		t.Fatal("Close must cancel the batcher context")
	}

	// A live timedFlush loop re-arms every batchDelay; sample with Stop(),
	// which observes without arming. One stale fire that raced past Close's
	// Stop can arm the timer once more (benign single-shot), but it fires
	// within one batchDelay and nothing re-arms it — so no sample after the
	// first batchDelay may find the timer armed.
	time.Sleep(DefaultStderrBatcherConfig().BatchDelay + 50*time.Millisecond)
	for i := 0; i < 5; i++ {
		if b.flushTimer.Stop() {
			t.Fatalf("timer armed %d samples past Close: sustained timedFlush loop", i+1)
		}
		time.Sleep(DefaultStderrBatcherConfig().BatchDelay)
	}
}
