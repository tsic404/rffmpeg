package client

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestProbeTimeoutExceedsDefaultTimeout pins the alignment contract: the
// probe request must be allowed to run longer than the shared client's 30s
// default, because the server-side probe waits up to ~2 minutes for a worker
// to run ffprobe. A cold worker's first probe (ffmpeg/GPU warm-up) can exceed
// 30s, and the old code aborted it with "context deadline exceeded".
func TestProbeTimeoutExceedsDefaultTimeout(t *testing.T) {
	if probeTimeout <= DefaultTimeout {
		t.Fatalf("probeTimeout = %v, want > DefaultTimeout (%v); the probe endpoint waits up to ~2m server-side", probeTimeout, DefaultTimeout)
	}
}

// TestProbeUsesDedicatedTimeout is the behavioral regression: Probe must be
// bounded by probeTimeout, not the 30s DefaultTimeout. With probeTimeout
// stubbed to 100ms and a server that answers after 300ms, Probe must fail —
// before the fix it used the 30s shared client and returned success, so this
// test fails on the old code.
func TestProbeUsesDedicatedTimeout(t *testing.T) {
	oldTimeout := probeTimeout
	probeTimeout = 100 * time.Millisecond
	defer func() { probeTimeout = oldTimeout }()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"format":{},"streams":[]}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "")
	if _, err := c.Probe("input.mp4"); err == nil {
		t.Fatal("Probe succeeded against a 300ms-delayed server; want it bounded by the stubbed 100ms probeTimeout")
	}
}
