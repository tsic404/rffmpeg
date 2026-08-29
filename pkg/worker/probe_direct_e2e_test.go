package worker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tsix404/rffmpeg/pkg/protocol"
	"github.com/tsix404/rffmpeg/pkg/worker/gpu"
)

// writeTestWAV writes a minimal 44-byte-header, 1-channel 16-bit PCM WAV file
// that any ffprobe build can parse — enough to prove the direct-path probe
// actually ran locally against the file rather than merely skipping download.
func writeTestWAV(path string) error {
	const sampleRate = 44100
	const numSamples = 4410 // 0.1s
	const numChannels = 1
	const bytesPerSample = 2

	dataLen := numSamples * numChannels * bytesPerSample
	buf := make([]byte, 44+dataLen)

	copy(buf[0:4], "RIFF")
	putLE32(buf[4:8], uint32(36+dataLen))
	copy(buf[8:12], "WAVE")
	copy(buf[12:16], "fmt ")
	putLE32(buf[16:20], 16) // PCM fmt chunk size
	putLE16(buf[20:22], 1)  // PCM
	putLE16(buf[22:24], numChannels)
	putLE32(buf[24:28], sampleRate)
	putLE32(buf[28:32], sampleRate*numChannels*bytesPerSample)
	putLE16(buf[32:34], numChannels*bytesPerSample)
	putLE16(buf[34:36], 16) // bits per sample
	copy(buf[36:40], "data")
	putLE32(buf[40:44], uint32(dataLen))

	// Silent PCM is sufficient — ffprobe only needs a parseable header.
	for i := range numSamples {
		putLE16(buf[44+i*2:46+i*2], 0)
	}
	return os.WriteFile(path, buf, 0644)
}

func putLE16(b []byte, v uint16) {
	b[0] = byte(v)
	b[1] = byte(v >> 8)
}

func putLE32(b []byte, v uint32) {
	b[0] = byte(v)
	b[1] = byte(v >> 8)
	b[2] = byte(v >> 16)
	b[3] = byte(v >> 24)
}

// probeDirectMockServer is a test HTTP server that records the terminal job
// PATCH and counts input downloads. The download endpoint returns 400 (exactly
// what the server does when ValidateFileID rejects an absolute path), so the
// test fails loudly if the worker ever takes the HTTP-download branch.
type probeDirectMockServer struct {
	*httptest.Server
	mu            sync.Mutex
	terminalBody  []byte
	terminalCount int32
	downloadCount int32
}

func newProbeDirectMockServer() *probeDirectMockServer {
	m := &probeDirectMockServer{}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/files/", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&m.downloadCount, 1)
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":{"code":"not_found","message":"invalid file id"}}`))
	})
	mux.HandleFunc("/api/v1/jobs/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && len(r.URL.Path) > 6 && r.URL.Path[len(r.URL.Path)-7:] == "/output" {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"message":"ok"}`))
			return
		}
		if r.Method != http.MethodPatch {
			w.WriteHeader(http.StatusOK)
			return
		}
		body := make([]byte, r.ContentLength)
		r.Body.Read(body)
		var update protocol.JobUpdateRequest
		if json.Unmarshal(body, &update) == nil && protocol.IsTerminalStatus(update.Status) {
			m.mu.Lock()
			m.terminalBody = body
			atomic.AddInt32(&m.terminalCount, 1)
			m.mu.Unlock()
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"message":"ok"}`))
	})
	mux.HandleFunc("/api/v1/workers/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"worker_id":"test-worker-001"}`))
	})
	m.Server = httptest.NewServer(mux)
	return m
}

// TestProcessProbeJob_DirectPathProbesLocally verifies the end-to-end
// shared-FS probe path (TSI-2520): a job whose DirectPaths carries an absolute
// local media path must be probed in place by ffprobe and reported completed —
// with no HTTP input download attempted. Before the fix, processProbeJob always
// downloaded job.InputFiles[0], sending the absolute path to
// GET /api/v1/files/<path> and failing the whole probe.
func TestProcessProbeJob_DirectPathProbesLocally(t *testing.T) {
	if _, err := os.Stat("/usr/bin/ffprobe"); err != nil {
		t.Skip("ffprobe not available for direct-path probe e2e test")
	}

	// A real media file on the local (shared) filesystem.
	tmpDir := t.TempDir()
	inputPath := filepath.Join(tmpDir, "tone.wav")
	if err := writeTestWAV(inputPath); err != nil {
		t.Fatalf("failed to write test media file: %v", err)
	}

	mockSrv := newProbeDirectMockServer()
	defer mockSrv.Close()

	cacheDir := filepath.Join(t.TempDir(), "cache")
	cache, err := NewCache(CacheConfig{Enabled: true, Dir: cacheDir, TTL: 24 * time.Hour})
	if err != nil {
		t.Fatalf("NewCache failed: %v", err)
	}
	t.Cleanup(func() { cache.Stop() })

	workerTempDir := filepath.Join(t.TempDir(), "worker-temp")
	if err := os.MkdirAll(workerTempDir, 0755); err != nil {
		t.Fatalf("mkdir worker-temp: %v", err)
	}

	client := NewClient(mockSrv.URL, "test-worker-001", "")
	w := &Worker{
		id:                 "test-worker-001",
		name:               "test-worker",
		client:             client,
		executor:           NewExecutor("ffmpeg", 30*time.Second),
		retryExecutor:      NewRetryExecutor(NewExecutor("ffmpeg", 30*time.Second), DefaultRetryConfig()),
		rewriteAdapter:     NewRewriteAdapter(),
		cache:              cache,
		tempDir:            workerTempDir,
		activeJobs:         make(map[string]context.CancelFunc),
		heartbeatInterval:  30 * time.Second,
		pollInterval:       5 * time.Second,
		lastHeartbeatTime:  time.Now(),
		ffprobeExecutor:    NewFFprobeExecutor("/usr/bin/ffprobe"),
		pixelFormatChecker: nil,
		gpuDetector:        gpu.NewDetector(),
	}

	job := protocol.JobInfo{
		ID:          "test-probe-direct-001",
		InputFiles:  []string{inputPath},
		Args:        []string{"__rffmpeg_probe__", inputPath},
		DirectPaths: []string{inputPath},
	}

	w.processProbeJob(context.Background(), job)

	if atomic.LoadInt32(&mockSrv.downloadCount) != 0 {
		t.Errorf("direct-path probe attempted %d HTTP input download(s), want 0",
			atomic.LoadInt32(&mockSrv.downloadCount))
	}
	if atomic.LoadInt32(&mockSrv.terminalCount) == 0 {
		t.Fatal("expected a terminal job update PATCH, got none")
	}

	mockSrv.mu.Lock()
	body := mockSrv.terminalBody
	mockSrv.mu.Unlock()

	var update protocol.JobUpdateRequest
	if err := json.Unmarshal(body, &update); err != nil {
		t.Fatalf("failed to parse terminal update body: %v\nbody: %s", err, body)
	}
	if update.Status != protocol.JobStatusCompleted {
		t.Errorf("Status = %q, want %q (probe must complete locally)", update.Status, protocol.JobStatusCompleted)
	}
	if update.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", update.ExitCode)
	}
	if update.Error != "" {
		t.Errorf("Error = %q, want empty", update.Error)
	}
}
