package worker

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/tsix404/rffmpeg/pkg/protocol"
)

// TestRegisterPreservesCountersOnReregister verifies that an automatic
// re-register (worker had completed jobs) does NOT zero the counters:
// resetting dropped the worker out of the eviction median sample pool and
// re-opened the cold-start window. The heartbeat timestamp must still be
// refreshed so the next heartbeat reports throughput over a full interval.
func TestRegisterPreservesCountersOnReregister(t *testing.T) {
	var mu sync.Mutex
	var registrations int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/workers/register" {
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		registrations++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"worker_id":"test-worker-id"}`))
	}))
	defer srv.Close()

	w, err := New(Config{ServerURL: srv.URL})
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}

	// Simulate state accumulated before re-registration: jobs finished,
	// per-interval counter set for the next heartbeat, stale timestamp.
	stale := time.Now().Add(-10 * time.Minute)
	w.mu.Lock()
	w.totalJobsCompleted = 42
	w.jobsCompleted = 3
	w.lastHeartbeatTime = stale
	w.mu.Unlock()

	if err := w.Register(protocol.WorkerCapabilities{}); err != nil {
		t.Fatalf("Register() failed: %v", err)
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	// TSI-2365: totalJobsCompleted is the process-lifetime count feeding the
	// server-side median sample pool; it must SURVIVE re-registration.
	if w.totalJobsCompleted != 42 {
		t.Errorf("totalJobsCompleted = %d, want 42 (re-register must not reset)", w.totalJobsCompleted)
	}
	if w.jobsCompleted != 0 {
		t.Errorf("jobsCompleted = %d, want 0 after registration reset", w.jobsCompleted)
	}
	if !w.lastHeartbeatTime.After(stale) {
		t.Error("lastHeartbeatTime should be refreshed on registration")
	}

	mu.Lock()
	defer mu.Unlock()
	if registrations != 1 {
		t.Errorf("server received %d registrations, want exactly 1", registrations)
	}
}

func TestFfmpegStderrIndicatesEmptyOutput(t *testing.T) {
	tests := []struct {
		name   string
		stderr string
		want   bool
	}{
		{
			name:   "empty stderr",
			stderr: "",
			want:   false,
		},
		{
			name:   "normal ffmpeg output",
			stderr: "frame=  100 fps=30 q=28.0 size=    1024kB time=00:00:03.33 bitrate=2519.5kbits/s speed=1x",
			want:   false,
		},
		{
			name:   "output file is empty - ss beyond duration",
			stderr: "Output file is empty, nothing was encoded (check -ss / -t / -frames parameters if used)\n",
			want:   true,
		},
		{
			name:   "output file is empty - lowercase",
			stderr: "output file is empty, nothing was encoded\n",
			want:   true,
		},
		{
			name:   "nothing was encoded",
			stderr: "Error: nothing was encoded\n",
			want:   true,
		},
		{
			name:   "does not contain any stream",
			stderr: "Output file #0 does not contain any stream\n",
			want:   true,
		},
		{
			name:   "mixed case",
			stderr: "Output File Is Empty, Nothing Was Encoded\n",
			want:   true,
		},
		{
			name:   "partial match - empty string",
			stderr: "This output file is empty of errors\n",
			want:   true,
		},
		{
			name:   "encoding error message",
			stderr: "Error while opening encoder for output stream #0:0 - maybe incorrect parameters such as bit_rate, rate, width or height\n",
			want:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ffmpegStderrIndicatesEmptyOutput(tt.stderr)
			if got != tt.want {
				t.Errorf("ffmpegStderrIndicatesEmptyOutput(%q) = %v, want %v", tt.stderr, got, tt.want)
			}
		})
	}
}

func TestFFmpegStderrIndicatesCriticalError(t *testing.T) {
	tests := []struct {
		name   string
		stderr string
		want   bool
	}{
		{
			name:   "invalid data found",
			stderr: "[mov,mp4,m4a,3gp,3g2,mj2 @ 0x55b5e8d8c700] Invalid data found when reading input\n",
			want:   true,
		},
		{
			name:   "invalid nal unit size",
			stderr: "[h264 @ 0x55b5e8d8c700] Invalid NAL unit size\n",
			want:   true,
		},
		{
			name:   "header missing",
			stderr: "[h264 @ 0x55b5e8d8c700] header missing\n",
			want:   true,
		},
		{
			name:   "corrupted file",
			stderr: "[mov,mp4,m4a,3gp,3g2,mj2 @ 0x55b5e8d8c700] corrupted file\n",
			want:   true,
		},
		{
			name:   "error while decoding",
			stderr: "[h264 @ 0x55b5e8d8c700] error while decoding MB 123\n",
			want:   true,
		},
		{
			name:   "concealing errors",
			stderr: "[h264 @ 0x55b5e8d8c700] concealing errors\n",
			want:   true,
		},
		{
			name:   "normal stderr",
			stderr: "frame=  123 fps= 30 q=28.0 size=    1234kB time=00:00:04.00 bitrate=2527.5kbits/s speed=1.00x\n",
			want:   false,
		},
		{
			name:   "empty stderr",
			stderr: "",
			want:   false,
		},
		{
			name:   "mixed case invalid data",
			stderr: "[h264 @ 0x55b5e8d8c700] INVALID DATA FOUND WHEN READING INPUT\n",
			want:   true,
		},
		{
			name:   "decode_slice_header error",
			stderr: "[h264 @ 0x55b5e8d8c700] decode_slice_header error\n",
			want:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ffmpegStderrIndicatesCriticalError(tt.stderr)
			if got != tt.want {
				t.Errorf("ffmpegStderrIndicatesCriticalError(%q) = %v, want %v", tt.stderr, got, tt.want)
			}
		})
	}
}
