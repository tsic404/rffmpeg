package worker

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/tsic404/rffmpeg/pkg/audit"
)

// TestFormatRewriteChainLine verifies the streamed audit-chain line uses the
// single-source audit formatter so CLI users see the same content that
// previously only appeared in worker-local stderr.
func TestFormatRewriteChainLine(t *testing.T) {
	got := audit.FormatRewriteChainLine("hw: h264_nvenc", "libx264", "h264_nvenc", "hardware upgrade", audit.InfoLevel)
	want := "[rffmpeg] INFO: Worker capabilities: hw: h264_nvenc | Requested: libx264 | Rewritten: h264_nvenc | Reason: hardware upgrade\n"
	if got != want {
		t.Errorf("FormatRewriteChainLine() = %q, want %q", got, want)
	}
}

// TestFormatRewriteChainLine_EmptyFields verifies empty segments are omitted,
// matching audit.Notifier formatting.
func TestFormatRewriteChainLine_EmptyFields(t *testing.T) {
	got := audit.FormatRewriteChainLine("", "", "h264_nvenc", "", audit.InfoLevel)
	want := "[rffmpeg] INFO: Rewritten: h264_nvenc\n"
	if got != want {
		t.Errorf("FormatRewriteChainLine() = %q, want %q", got, want)
	}
}

// TestStderrBatcherCarriesRewriteChainLine is the TSI-2349 regression test:
// the encoder-rewrite audit chain must travel through the job's stderr batcher
// (SendStderrChunk → server PATCH /jobs/{id} → WS stderr broadcast) instead of
// only being printed to the worker process' own stderr, which CLI clients
// never observe.
func TestStderrBatcherCarriesRewriteChainLine(t *testing.T) {
	var mu sync.Mutex
	var chunks []string

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			StderrChunk string `json:"stderr_chunk,omitempty"`
		}
		_ = json.Unmarshal(body, &req)
		mu.Lock()
		if req.StderrChunk != "" {
			chunks = append(chunks, req.StderrChunk)
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"message":"ok"}`))
	}))
	defer ts.Close()

	client := NewClient(ts.URL, "test-worker", "")
	batcher := NewStderrBatcher("job-tsi2349", client, DefaultStderrBatcherConfig())

	line := audit.FormatRewriteChainLine(
		"h264_nvenc,h264_qsv,libx264",
		"libx264",
		"h264_nvenc",
		"auto_hw hardware upgrade",
		audit.InfoLevel,
	)
	batcher.StderrHandler()(line)
	batcher.Close() // flushes and waits for delivery

	mu.Lock()
	defer mu.Unlock()
	combined := strings.Join(chunks, "\n")
	if !strings.Contains(combined, line) {
		t.Errorf("rewrite chain line not delivered via stderr channel; got chunks: %q", combined)
	}
	if !strings.HasPrefix(strings.TrimSpace(combined), "[rffmpeg] INFO:") {
		t.Errorf("delivered chunk missing [rffmpeg] prefix: %q", combined)
	}
}

// TestNotifyRewriteChainFormatParity guards that the streamed line stays in
// sync with what the audit notifier prints on the worker side.
func TestNotifyRewriteChainFormatParity(t *testing.T) {
	caps, requested, rewritten, reason := "caps-x", "libx264", "h264_nvenc", "upgrade-y"

	var buf syncBuffer
	notifier := audit.NewNotifierWithOutput(&buf)
	if err := notifier.NotifyRewriteChain(caps, requested, rewritten, reason, audit.InfoLevel); err != nil {
		t.Fatalf("NotifyRewriteChain failed: %v", err)
	}

	streamed := audit.FormatRewriteChainLine(caps, requested, rewritten, reason, audit.InfoLevel)
	if streamed != buf.String() {
		t.Errorf("streamed line %q does not match notifier output %q", streamed, buf.String())
	}
}

type syncBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
