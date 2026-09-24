package worker

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tsic404/rffmpeg/pkg/protocol"
)

// TestStdoutBatcher_SendsBatchesInOrder is the regression test for silently
// corrupted streamed output: every flush used to spawn its own goroutine
// sending an independent PATCH, so two batches in flight at once reached the
// server in whatever order their requests were served — splicing a 320KB batch
// into the wrong place of the streamed bytes. Every byte still arrived, so
// neither the byte count nor the sequence numbers showed a problem.
//
// The delay on the first request makes the race deterministic: an unordered
// sender lets the second batch's request overtake it.
func TestStdoutBatcher_SendsBatchesInOrder(t *testing.T) {
	var mu sync.Mutex
	var order []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			w.WriteHeader(http.StatusOK)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var update protocol.JobUpdateRequest
		if err := json.Unmarshal(body, &update); err != nil {
			t.Errorf("unmarshal update: %v", err)
			w.WriteHeader(http.StatusOK)
			return
		}
		raw, err := protocol.StdoutChunkBase64(update.StdoutChunk)
		if err != nil {
			t.Errorf("decode stdout chunk: %v", err)
		}
		// Hold the first batch's request open so a concurrently sent second
		// batch is served — and recorded — first.
		if string(raw) == "first" {
			time.Sleep(200 * time.Millisecond)
		}
		mu.Lock()
		order = append(order, string(raw))
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	client := NewClient(ts.URL, "test-worker", "")
	// BatchSize 1 flushes on every Add, so both chunks are in flight together;
	// an hour-long delay keeps the flush timer out of the picture.
	b := NewStdoutBatcher("test-job", client, StdoutBatcherConfig{BatchSize: 1, BatchDelay: time.Hour})
	defer b.Close()

	handler := b.StdoutHandler()
	handler([]byte("first"))
	handler([]byte("second"))

	if err := b.FlushAndWait(); err != nil {
		t.Fatalf("FlushAndWait: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if got := strings.Join(order, ","); got != "first,second" {
		t.Errorf("batches reached the server as %q, want \"first,second\" — out-of-order chunks corrupt the streamed output", got)
	}
}
