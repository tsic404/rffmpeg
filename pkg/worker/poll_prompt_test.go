package worker

import (
	"testing"
	"time"
)

// TestDefaultPollIntervalIsPrompt pins the default poll interval. A submitted
// job is claimed on the worker's next poll tick, so the default bounds the
// pickup latency that, added to the ffmpeg budget, is the end-to-end wall time
// of a --timeout job. A wall-time threshold cannot guard this: a regression to
// 2s still passes a loose bound yet breaches the ~7s acceptance, so the test
// asserts the concrete default instead.
func TestDefaultPollIntervalIsPrompt(t *testing.T) {
	w, err := New(Config{ServerURL: "http://localhost:1", TempDir: t.TempDir()})
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	if w.pollInterval != time.Second {
		t.Errorf("default poll interval = %v, want 1s; a coarser default delays short --timeout jobs past the skill's wall-time bound", w.pollInterval)
	}
}
