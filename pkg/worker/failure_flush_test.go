package worker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tsix404/rffmpeg/pkg/protocol"
)

// TestStderrBatcherFlushAndWaitDeliversPendingChunks verifies that
// FlushAndWait flushes buffered chunks and blocks until the in-flight
// stderr send has reached the server, without requiring Close first.
func TestStderrBatcherFlushAndWaitDeliversPendingChunks(t *testing.T) {
	var mu sync.Mutex
	var chunks []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var req protocol.JobUpdateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err == nil && req.StderrChunk != "" {
			mu.Lock()
			chunks = append(chunks, req.StderrChunk)
			mu.Unlock()
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "worker-1", "")
	batcher := NewStderrBatcher("job-1", client, DefaultStderrBatcherConfig())
	defer batcher.Close()

	const tail = "tail line\n"
	batcher.Add(tail)
	batcher.FlushAndWait()

	mu.Lock()
	got := append([]string(nil), chunks...)
	mu.Unlock()

	if len(got) != 1 || got[0] != tail {
		t.Fatalf("FlushAndWait did not deliver pending chunk: got %q, want [%q]", got, tail)
	}
}

// TestReportFailureFlushesStderrBeforeTerminalStatus runs a fast-failing
// ffmpeg job through processJob and asserts the tail stderr chunk reaches the
// server before the terminal failed status (TSI-2581).
func TestReportFailureFlushesStderrBeforeTerminalStatus(t *testing.T) {
	tmpDir := t.TempDir()
	failingFFmpeg := filepath.Join(tmpDir, "fail-ffmpeg")
	script := `#!/bin/bash
echo "No space left on device" >&2
exit 1
`
	if err := os.WriteFile(failingFFmpeg, []byte(script), 0755); err != nil {
		t.Fatalf("write failing ffmpeg: %v", err)
	}

	w, _ := setupTestWorker(t, 24*time.Hour)
	w.executor = NewExecutor(failingFFmpeg, 30*time.Second)
	w.retryExecutor = NewRetryExecutor(w.executor, DefaultRetryConfig())

	var mu sync.Mutex
	var events []string

	srv := httptest.NewServer(http.HandlerFunc(func(wr http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			wr.WriteHeader(http.StatusOK)
			wr.Write([]byte("fake input"))
		case http.MethodPatch:
			var req protocol.JobUpdateRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err == nil {
				mu.Lock()
				if req.Status != "" {
					events = append(events, "status:"+string(req.Status))
				}
				if req.StderrChunk != "" {
					events = append(events, "stderr:"+req.StderrChunk)
				}
				mu.Unlock()
			}
			wr.WriteHeader(http.StatusOK)
		case http.MethodPost:
			wr.WriteHeader(http.StatusOK)
		default:
			wr.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	w.client.baseURL = srv.URL

	job := protocol.JobInfo{
		ID:         "tsi-2581-fastfail",
		InputFiles: []string{"input-001"},
		Args:       []string{"-i", "<INPUT_FILE>", "-c:v", "libx264", "-preset", "fast"},
	}
	jobCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	w.processJob(jobCtx, job, cancel, false)

	mu.Lock()
	recorded := append([]string(nil), events...)
	mu.Unlock()

	stderrIdx, failedIdx := -1, -1
	for i, ev := range recorded {
		if strings.HasPrefix(ev, "stderr:") && strings.Contains(ev, "No space left on device") {
			stderrIdx = i
		}
		if ev == "status:failed" {
			failedIdx = i
		}
	}

	if stderrIdx == -1 {
		t.Fatalf("tail stderr chunk not captured; events=%v", recorded)
	}
	if failedIdx == -1 {
		t.Fatalf("terminal failed status not captured; events=%v", recorded)
	}
	if stderrIdx > failedIdx {
		t.Fatalf("stderr chunk arrived after terminal status: stderr=%d failed=%d events=%v",
			stderrIdx, failedIdx, recorded)
	}
}
