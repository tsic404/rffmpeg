package worker

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tsic404/rffmpeg/pkg/protocol"
)

// TestCacheHitStreamsStderrNotice is the TSI-2415 regression test: the
// worker's cache-hit notice must travel through the job's stderr stream
// (SendStderrChunk → server PATCH /jobs/{id} → WS stderr broadcast) so CLI
// clients observe it — not only the worker process' own log, which CLI users
// never see.
func TestCacheHitStreamsStderrNotice(t *testing.T) {
	w, _ := setupTestWorker(t, 24*time.Hour)

	ctx := context.Background()
	jobCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Capture stderr chunks and output uploads PATCHed/POSTed to the server.
	var mu sync.Mutex
	var stderrPatches []string
	var uploads int32
	captureSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/output") {
			atomic.AddInt32(&uploads, 1)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req struct {
			StderrChunk string `json:"stderr_chunk,omitempty"`
		}
		_ = json.Unmarshal(body, &req)
		mu.Lock()
		if req.StderrChunk != "" {
			stderrPatches = append(stderrPatches, req.StderrChunk)
		}
		mu.Unlock()
	}))
	defer captureSrv.Close()
	w.client.baseURL = captureSrv.URL

	job := protocol.JobInfo{
		ID:         "tsi-2415-first",
		InputFiles: []string{"input-001"},
		Args:       []string{"-i", "<INPUT_FILE>", "-c:v", "libx264", "-preset", "fast"},
	}

	// First run: cache miss → execute → output cached.
	w.processJob(jobCtx, job, cancel, false)
	if w.cache.Stats().EntryCount != 1 {
		t.Fatalf("expected 1 cache entry after first run, got %d", w.cache.Stats().EntryCount)
	}

	// Second run with a different job ID must hit the cache and emit the
	// cache-hit notice on the job's stderr channel.
	job2 := protocol.JobInfo{
		ID:         "tsi-2415-second",
		InputFiles: []string{"input-001"},
		Args:       []string{"-i", "<INPUT_FILE>", "-c:v", "libx264", "-preset", "fast"},
	}

	w.processJob(jobCtx, job2, cancel, false)
	if w.cache.Stats().Hits < 1 {
		t.Fatal("expected at least 1 confirmed cache hit on second run")
	}
	if atomic.LoadInt32(&uploads) < 1 {
		t.Fatalf("expected cached output upload on second run, uploads=%d", atomic.LoadInt32(&uploads))
	}

	mu.Lock()
	combined := strings.Join(stderrPatches, "\n")
	n := len(stderrPatches)
	mu.Unlock()

	if !strings.Contains(combined, "[rffmpeg] Cache hit:") {
		t.Errorf("cache-hit notice not delivered via job stderr channel; got %d chunk(s): %q", n, combined)
	}
}
