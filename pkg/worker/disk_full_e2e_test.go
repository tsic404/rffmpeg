package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tsic404/rffmpeg/pkg/protocol"
	"github.com/tsic404/rffmpeg/pkg/worker/gpu"
)

// diskFullServer serves a real input file and records the terminal job update
// plus every stderr chunk the worker streams, so a test can assert both the
// reported classification and that the actual ffmpeg stderr carried the
// out-of-space error (not merely a mock stderr line).
type diskFullServer struct {
	*httptest.Server

	mu       sync.Mutex
	terminal protocol.JobUpdateRequest
	stderr   strings.Builder
	input    []byte
}

func newDiskFullServer(input []byte) *diskFullServer {
	s := &diskFullServer{input: input}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/files/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(s.input)
	})
	mux.HandleFunc("/api/v1/jobs/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch {
			body, _ := io.ReadAll(r.Body)
			var u protocol.JobUpdateRequest
			if json.Unmarshal(body, &u) == nil {
				s.mu.Lock()
				if u.StderrChunk != "" {
					s.stderr.WriteString(u.StderrChunk)
				}
				if protocol.IsTerminalStatus(u.Status) {
					s.terminal = u
				}
				s.mu.Unlock()
			}
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"message":"ok"}`))
	})
	mux.HandleFunc("/api/v1/workers/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"worker_id":"test-worker-disk-full"}`))
	})
	s.Server = httptest.NewServer(mux)
	return s
}

func (s *diskFullServer) terminalUpdate(t *testing.T) protocol.JobUpdateRequest {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.terminal.Status == "" {
		t.Fatal("expected a terminal job update PATCH, got none")
	}
	return s.terminal
}

func (s *diskFullServer) stderrText() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stderr.String()
}

// sudoPrefix returns the command prefix needed to run privileged filesystem
// commands, or an error when root access is unavailable. Running as root needs
// no prefix; otherwise it requires a passwordless sudo (CI runners and this
// machine's wheel group both satisfy `sudo -n`).
func sudoPrefix() ([]string, error) {
	if os.Geteuid() == 0 {
		return nil, nil
	}
	if _, err := exec.LookPath("sudo"); err != nil {
		return nil, err
	}
	if err := exec.Command("sudo", "-n", "true").Run(); err != nil {
		return nil, fmt.Errorf("passwordless sudo unavailable: %w", err)
	}
	return []string{"sudo", "-n"}, nil
}

// mountTinyFS creates and mounts an 8 MiB ext4 loop filesystem and returns its
// mount point. It skips the test in any environment that cannot set one up
// (non-Linux, missing tools, no passwordless root, no loop support) so the
// suite stays green everywhere while still exercising real ENOSPC where the
// prerequisites exist.
func mountTinyFS(t *testing.T) string {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skipf("tiny-FS disk-full E2E requires Linux loop devices")
	}
	for _, tool := range []string{"mkfs.ext4", "losetup", "mount", "umount"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("missing %s for tiny-FS setup: %v", tool, err)
		}
	}
	sudo, err := sudoPrefix()
	if err != nil {
		t.Skipf("no root access for tiny-FS setup: %v", err)
	}

	base := t.TempDir()
	img := filepath.Join(base, "tiny.img")
	mnt := filepath.Join(base, "mnt")
	if err := os.MkdirAll(mnt, 0o755); err != nil {
		t.Fatalf("mkdir mountpoint: %v", err)
	}

	// run executes args, optionally under sudo, and returns trimmed stdout.
	run := func(args ...string) (string, error) {
		full := append(append([]string{}, sudo...), args...)
		out, err := exec.Command(full[0], full[1:]...).CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}

	if _, err := run("truncate", "-s", "8M", img); err != nil {
		t.Skipf("cannot create tiny-FS image: %v", err)
	}
	loopDev, err := run("losetup", "--find", "--show", img)
	if err != nil {
		t.Skipf("cannot attach loop device: %v", err)
	}
	t.Cleanup(func() {
		if _, err := run("losetup", "-d", loopDev); err != nil {
			t.Logf("cleanup: detach %s: %v", loopDev, err)
		}
	})
	if _, err := run("mkfs.ext4", "-q", "-F", loopDev); err != nil {
		t.Skipf("cannot mkfs.ext4 on %s: %v", loopDev, err)
	}
	if _, err := run("mount", loopDev, mnt); err != nil {
		t.Skipf("cannot mount %s at %s: %v", loopDev, mnt, err)
	}
	t.Cleanup(func() {
		if _, err := run("umount", mnt); err != nil {
			t.Logf("cleanup: unmount %s: %v", mnt, err)
		}
	})
	// A non-root test process writes through the worker to a root-owned mount
	// point, so open it up. Root needs no change.
	if _, err := run("chmod", "0777", mnt); err != nil {
		t.Skipf("cannot chmod mountpoint %s: %v", mnt, err)
	}
	return mnt
}

// ffmpegMissingCapability reports whether ffmpeg stderr shows a build without
// the encoder or filter a command asked for. Such a gap is an environment
// limitation to skip on, not an invocation error to fail on.
func ffmpegMissingCapability(stderr string) bool {
	for _, p := range []string{
		"Unknown encoder",
		"Encoder not found",
		"not found in encoder list",
		"Unknown filter",
		"No such filter",
	} {
		if strings.Contains(stderr, p) {
			return true
		}
	}
	return false
}

// TestProcessJob_RealDiskFull mounts an 8 MiB loop filesystem, points the
// worker's temp dir at it, and runs a real ffmpeg transcode whose rawvideo
// output (scaled to 1920x1080) is orders of magnitude larger than the
// filesystem, so ffmpeg genuinely hits ENOSPC. It asserts the worker reports
// DISK_FULL end-to-end and that the streamed stderr carried the real
// out-of-space error. The mock-stderr DISK_FULL row in
// failure_matrix_e2e_test.go cannot cover this path because it fakes the
// stderr; this test exercises the full write→ENOSPC→classify chain.
func TestProcessJob_RealDiskFull(t *testing.T) {
	ffmpegPath, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not available")
	}

	// Generate a small real input on the host filesystem so the transcode
	// exercises the real pipeline (input download, probe, encode, write). The
	// native mpeg4 encoder links no external library, so unlike libx264 it
	// exists in every ffmpeg build.
	inputPath := filepath.Join(t.TempDir(), "input.mp4")
	gen := exec.Command(ffmpegPath, "-y",
		"-f", "lavfi", "-i", "testsrc=duration=2:size=320x240:rate=25",
		"-c:v", "mpeg4", "-q:v", "5", inputPath)
	if out, err := gen.CombinedOutput(); err != nil {
		// A stripped build missing the encoder or the lavfi source filter is an
		// environment gap, not a failure of the behaviour under test — skip,
		// like every other missing capability in this file.
		if ffmpegMissingCapability(string(out)) {
			t.Skipf("ffmpeg cannot generate the test input: %v\n%s", err, out)
		}
		t.Fatalf("generate input: %v\n%s", err, out)
	}
	inputBytes, err := os.ReadFile(inputPath)
	if err != nil {
		t.Fatalf("read input: %v", err)
	}

	mnt := mountTinyFS(t)

	srv := newDiskFullServer(inputBytes)
	defer srv.Close()

	workerTempDir := filepath.Join(mnt, "worker-temp")
	if err := os.MkdirAll(workerTempDir, 0o755); err != nil {
		t.Fatalf("mkdir worker-temp on tiny FS: %v", err)
	}

	cache, err := NewCache(CacheConfig{Enabled: false})
	if err != nil {
		t.Fatalf("NewCache: %v", err)
	}

	executor := NewExecutor(ffmpegPath, 30*time.Second)
	retryExecutor := NewRetryExecutor(executor, DefaultRetryConfig())
	rewriteAdapter := NewRewriteAdapter()
	rewriteAdapter.config.Enabled = false

	w := &Worker{
		id:                "test-worker-disk-full",
		name:              "test-worker-disk-full",
		client:            NewClient(srv.URL, "test-worker-disk-full", ""),
		executor:          executor,
		retryExecutor:     retryExecutor,
		rewriteAdapter:    rewriteAdapter,
		cache:             cache,
		tempDir:           workerTempDir,
		activeJobs:        make(map[string]context.CancelFunc),
		heartbeatInterval: 30 * time.Second,
		pollInterval:      5 * time.Second,
		lastHeartbeatTime: time.Now(),
		ffprobeExecutor:   NewFFprobeExecutor(""),
		gpuDetector:       gpu.NewDetector(),
	}

	jobCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// rawvideo at 1920x1080 writes ~2.97 MiB per frame (1920*1080*1.5 bytes);
	// the 8 MiB filesystem is exhausted within the first second of a 2 s input.
	job := protocol.JobInfo{
		ID:             "disk-full-real",
		InputFiles:     []string{"input-001"},
		Args:           []string{"-i", "<INPUT_FILE>", "-c:v", "rawvideo", "-pix_fmt", "yuv420p", "-s", "1920x1080"},
		OutputFilename: "out.avi",
	}

	w.processJob(jobCtx, job, cancel, false)

	update := srv.terminalUpdate(t)
	if update.Status != protocol.JobStatusFailed {
		t.Errorf("Status = %q, want %q (failure_type=%q, error=%q)",
			update.Status, protocol.JobStatusFailed, update.FailureType, update.Error)
	}
	if update.FailureType != string(protocol.FailureDiskFull) {
		t.Errorf("FailureType = %q, want %q (error=%q)", update.FailureType, protocol.FailureDiskFull, update.Error)
	}
	if !strings.Contains(srv.stderrText(), "No space left on device") {
		t.Errorf("streamed ffmpeg stderr did not contain the real ENOSPC error; got:\n%s", srv.stderrText())
	}
}
