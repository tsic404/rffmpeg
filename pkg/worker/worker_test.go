package worker

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/tsic404/rffmpeg/pkg/protocol"
	"github.com/tsic404/rffmpeg/pkg/worker/workerconfig"
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

func TestNewDefaultTempDirPrivate(t *testing.T) {
	xdgBase := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", xdgBase)

	w, err := New(Config{ServerURL: "http://localhost:1"})
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(w.tempDir) })

	want := filepath.Join(xdgBase, "rffmpeg-worker", w.ID())
	if os.Geteuid() == 0 {
		// Root deployments ignore XDG_CACHE_HOME and use the FHS primary path.
		want = filepath.Join(workerconfig.RootTempDirBase, w.ID())
	}
	if w.tempDir != want {
		t.Errorf("default TempDir = %q, want %q", w.tempDir, want)
	}

	info, err := os.Stat(w.tempDir)
	if err != nil {
		t.Fatalf("stat %s: %v", w.tempDir, err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Errorf("default temp dir mode = %o, want 0700", got)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("default temp dir stat type = %T, want *syscall.Stat_t", info.Sys())
	}
	if st.Uid != uint32(os.Geteuid()) {
		t.Errorf("default temp dir uid = %d, want %d", st.Uid, os.Geteuid())
	}
}

func TestNewExplicitTempDirMode(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "worker-explicit")

	w, err := New(Config{ServerURL: "http://localhost:1", TempDir: dir})
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	if w.tempDir != dir {
		t.Errorf("TempDir = %q, want %q", w.tempDir, dir)
	}

	info, err := os.Stat(w.tempDir)
	if err != nil {
		t.Fatalf("stat %s: %v", w.tempDir, err)
	}
	if got := info.Mode().Perm(); got != 0o755 {
		t.Errorf("explicit temp dir mode = %o, want 0755", got)
	}
}

func TestNewRejectsNonPrivateParentTempDir(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root uses FHS primary path, not XDG_CACHE_HOME")
	}
	xdgBase := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", xdgBase)

	// Pre-create the first directory below the trusted base with loose mode;
	// MkdirAll will not tighten it, so the parent check must reject it.
	parent := filepath.Join(xdgBase, "rffmpeg-worker")
	if err := os.Mkdir(parent, 0o755); err != nil {
		t.Fatalf("mkdir parent: %v", err)
	}

	if _, err := New(Config{ServerURL: "http://localhost:1"}); err == nil {
		t.Fatal("New() = nil error, want rejection of non-private parent dir")
	}
}

func TestVerifyPrivateDir(t *testing.T) {
	t.Run("private dir accepted", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.Chmod(dir, 0o700); err != nil {
			t.Fatalf("chmod %s: %v", dir, err)
		}
		if err := verifyPrivateDir(dir); err != nil {
			t.Errorf("verifyPrivateDir(%q) = %v, want nil", dir, err)
		}
	})

	t.Run("group-readable rejected", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.Chmod(dir, 0o750); err != nil {
			t.Fatalf("chmod %s: %v", dir, err)
		}
		if err := verifyPrivateDir(dir); err == nil {
			t.Error("verifyPrivateDir(0750) = nil, want error")
		}
	})

	t.Run("missing dir rejected", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "missing")
		if err := verifyPrivateDir(dir); err == nil {
			t.Error("verifyPrivateDir(missing) = nil, want error")
		}
	})

	foreignUID := os.Geteuid() + 1

	t.Run("leaf owner mismatch rejected", func(t *testing.T) {
		leaf := filepath.Join(t.TempDir(), "rffmpeg-worker", "w")
		if err := os.MkdirAll(leaf, 0o700); err != nil {
			t.Fatalf("MkdirAll %s: %v", leaf, err)
		}
		if err := verifyPrivateDirAs(leaf, foreignUID); err == nil {
			t.Errorf("verifyPrivateDirAs(%q, uid=%d) = nil, want owner-mismatch error", leaf, foreignUID)
		}
	})

	t.Run("parent owner mismatch rejected", func(t *testing.T) {
		parent := filepath.Join(t.TempDir(), "rffmpeg-worker")
		if err := os.MkdirAll(parent, 0o700); err != nil {
			t.Fatalf("MkdirAll %s: %v", parent, err)
		}
		if err := verifyPrivateDirAs(parent, foreignUID); err == nil {
			t.Errorf("verifyPrivateDirAs(%q, uid=%d) = nil, want owner-mismatch error", parent, foreignUID)
		}
	})
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
			name:   "error opening output file - missing directory",
			stderr: "[out#0/mp3 @ 0x55be9305cc00] Error opening output /nonexistent_dir/out.mp3: No such file or directory\nError opening output file /nonexistent_dir/out.mp3.\nError opening output files: No such file or directory\n",
			want:   true,
		},
		{
			name:   "error initializing the muxer - unknown container",
			stderr: "[AVFormatContext @ 0x563fd6c0cd00] Unable to choose an output format for 'output.xyz'; use a standard extension for the filename or specify the format manually.\n[out#0 @ 0x563fd6c0cc00] Error initializing the muxer for output.xyz: Invalid argument\nError opening output file output.xyz.\n",
			want:   true,
		},
		{
			name:   "could not open output lowercase",
			stderr: "could not open output file /tmp/jobs/job-1/output.mp3\n",
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
