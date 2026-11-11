package worker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tsic404/rffmpeg/pkg/protocol"
	"github.com/tsic404/rffmpeg/pkg/worker/gpu"
)

// terminalCaptureServer serves job inputs and records the terminal job update,
// whatever its status.
type terminalCaptureServer struct {
	*httptest.Server
	mu            sync.Mutex
	terminalBody  []byte
	terminalCount int32
}

func newTerminalCaptureServer() *terminalCaptureServer {
	m := &terminalCaptureServer{}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/files/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(testMediaBytes())
	})
	mux.HandleFunc("/api/v1/jobs/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch {
			body := make([]byte, r.ContentLength)
			_, _ = r.Body.Read(body)
			var update protocol.JobUpdateRequest
			if json.Unmarshal(body, &update) == nil && protocol.IsTerminalStatus(update.Status) {
				m.mu.Lock()
				m.terminalBody = body
				atomic.AddInt32(&m.terminalCount, 1)
				m.mu.Unlock()
			}
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"message":"ok"}`))
	})
	m.Server = httptest.NewServer(mux)
	return m
}

// writeMockFFmpegExitZeroWarning writes a fixture that reports decode/metadata
// warnings which once matched the worker's broad "critical error" phrase list,
// then finishes a transcode with exit code 0.
func writeMockFFmpegExitZeroWarning(dir string) (string, error) {
	scriptPath := filepath.Join(dir, "mock-ffmpeg-exit0-warning")
	script := `output=""
for arg in "$@"; do output="$arg"; done
echo "  Metadata:" >&2
echo "    title           : header missing" >&2
echo "    title           : Not overwriting - exiting" >&2
echo "    encoder         : Lavf61.7.100" >&2
echo "[h264 @ 0x55b5e8d8c700] header missing" >&2
echo "[h264 @ 0x55b5e8d8c700] corrupted file" >&2
echo "frame=  42 fps= 30 q=28.0 size=    256kB time=00:00:01.40 bitrate=1498.1kbits/s speed=1.00x" >&2
mkdir -p "$(dirname "$output")" 2>/dev/null
printf 'TRANSCODED' > "$output"
exit 0
`
	if err := writeShellFixture(scriptPath, script); err != nil {
		return "", err
	}
	return scriptPath, nil
}

// newExitZeroWorker builds a worker wired to the capture server and the given
// ffmpeg binary, with encoder rewrite disabled so args pass through verbatim.
func newExitZeroWorker(t *testing.T, tmpDir, serverURL, ffmpegPath string) *Worker {
	t.Helper()

	cache, err := NewCache(CacheConfig{
		Enabled: true,
		Dir:     filepath.Join(tmpDir, "cache"),
		TTL:     24 * time.Hour,
	})
	if err != nil {
		t.Fatalf("NewCache failed: %v", err)
	}
	t.Cleanup(func() { cache.Stop() })

	workerTempDir := filepath.Join(tmpDir, "worker-temp")
	if err := os.MkdirAll(workerTempDir, 0o755); err != nil {
		t.Fatalf("mkdir worker-temp: %v", err)
	}

	executor := NewExecutor(ffmpegPath, 30*time.Second)
	rewriteAdapter := NewRewriteAdapter()
	rewriteAdapter.config.Enabled = false

	return &Worker{
		id:                "test-worker-001",
		name:              "test-worker",
		client:            NewClient(serverURL, "test-worker-001", ""),
		executor:          executor,
		retryExecutor:     NewRetryExecutor(executor, DefaultRetryConfig()),
		rewriteAdapter:    rewriteAdapter,
		cache:             cache,
		tempDir:           workerTempDir,
		activeJobs:        make(map[string]context.CancelFunc),
		heartbeatInterval: 30 * time.Second,
		pollInterval:      5 * time.Second,
		lastHeartbeatTime: time.Now(),
		ffprobeExecutor:   NewFFprobeExecutor("false"),
		gpuDetector:       gpu.NewDetector(),
	}
}

// assertCompleted fails unless the captured terminal update reports a
// completed job with exit code 0.
func assertCompleted(t *testing.T, srv *terminalCaptureServer) {
	t.Helper()

	if atomic.LoadInt32(&srv.terminalCount) == 0 {
		t.Fatal("expected a terminal job update PATCH, got none")
	}
	srv.mu.Lock()
	body := srv.terminalBody
	srv.mu.Unlock()

	var update protocol.JobUpdateRequest
	if err := json.Unmarshal(body, &update); err != nil {
		t.Fatalf("parse terminal update: %v\nbody: %s", err, body)
	}
	if update.Status != protocol.JobStatusCompleted {
		t.Fatalf("Status = %q, want %q (failure_type=%q error=%q)",
			update.Status, protocol.JobStatusCompleted, update.FailureType, update.Error)
	}
	if update.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0 for a completed job", update.ExitCode)
	}
}

// TestProcessJob_ExitZeroWithErrorLikeMetadataCompletes is the regression for
// the over-broad stderr phrase match: a job that legitimately carries
// "-metadata title=header missing" prints "title : header missing" at the
// default log level, and ffmpeg still exits 0. The job is complete work and
// must not be reported as FFMPEG_ERROR or retried.
func TestProcessJob_ExitZeroWithErrorLikeMetadataCompletes(t *testing.T) {
	tmpDir := t.TempDir()
	ffmpegPath, err := writeMockFFmpegExitZeroWarning(tmpDir)
	if err != nil {
		t.Fatalf("write mock ffmpeg: %v", err)
	}

	mockSrv := newTerminalCaptureServer()
	defer mockSrv.Close()

	w := newExitZeroWorker(t, tmpDir, mockSrv.URL, ffmpegPath)

	job := protocol.JobInfo{
		ID:             "exit0-metadata-header-missing",
		InputFiles:     []string{testUploadID("input-001")},
		Args:           []string{"-i", "<INPUT_FILE>", "-metadata", "title=header missing", "-c:v", "libx264"},
		OutputFilename: "out.mp4",
	}

	w.processJob(context.Background(), job, func() {}, false)

	assertCompleted(t, mockSrv)
}

// TestProcessJob_ExitZeroWithRefusalLikeMetadataCompletes drives a real ffmpeg
// whose output metadata value is the overwrite-refusal sentence. ffmpeg prints
// "title : Not overwriting - exiting" and exits 0; matching that phrase
// anywhere in stderr turned a normal job into FFMPEG_ERROR. Only ffmpeg's own
// "File ... already exists" refusal line may be treated as a failed write.
func TestProcessJob_ExitZeroWithRefusalLikeMetadataCompletes(t *testing.T) {
	ffmpegPath, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}

	tmpDir := t.TempDir()
	mockSrv := newTerminalCaptureServer()
	defer mockSrv.Close()

	w := newExitZeroWorker(t, tmpDir, mockSrv.URL, ffmpegPath)

	job := protocol.JobInfo{
		ID:         "exit0-metadata-refusal-phrase",
		InputFiles: []string{testUploadID("input-001")},
		Args: []string{
			"-i", "<INPUT_FILE>",
			"-c:v", "libx264", "-preset", "ultrafast",
			"-metadata", "title=Not overwriting - exiting",
		},
		OutputFilename: "out.mp4",
	}

	w.processJob(context.Background(), job, func() {}, false)

	assertCompleted(t, mockSrv)
}
