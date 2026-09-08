package worker

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/tsic404/rffmpeg/pkg/protocol"
	"github.com/tsic404/rffmpeg/pkg/worker/gpu"
)

// writeStdoutMockFFmpeg creates an executable shell script that writes a fixed
// byte payload to stdout and exits 0, simulating a streaming transcode that
// produces output on ffmpeg's stdout pipe.
func writeStdoutMockFFmpeg(dir string) (string, error) {
	scriptPath := filepath.Join(dir, "mock-ffmpeg-stdout")
	script := "#!/bin/bash\nprintf 'mock-video-output-data'\nexit 0\n"
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		return "", err
	}
	return scriptPath, nil
}

// stdoutOrderMockServer records every job PATCH body in arrival order so the
// test can assert the stdout chunks precede the completed status.
type stdoutOrderMockServer struct {
	*httptest.Server
	mu     sync.Mutex
	events []string // "stdout", "completed", or "other", in arrival order
}

func newStdoutOrderMockServer() *stdoutOrderMockServer {
	m := &stdoutOrderMockServer{}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/files/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("mock input data"))
	})
	mux.HandleFunc("/api/v1/jobs/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch {
			body, _ := io.ReadAll(r.Body)
			var update protocol.JobUpdateRequest
			if json.Unmarshal(body, &update) == nil {
				ev := "other"
				if update.StdoutChunk != "" {
					ev = "stdout"
				} else if update.Status == protocol.JobStatusCompleted {
					ev = "completed"
				}
				m.mu.Lock()
				m.events = append(m.events, ev)
				m.mu.Unlock()
			}
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

// TestProcessJob_StreamingStdoutFlushedBeforeCompleted is the TSI-2905
// regression test: the worker must flush all streamed stdout chunks (and wait
// for them to reach the server) BEFORE reporting the completed status. If it
// reports completion first, the CLI observes the terminal status, closes its
// WebSocket, and every still-in-flight stdout chunk is lost — leaving the
// consumer with an empty redirect target despite a successful transcode.
func TestProcessJob_StreamingStdoutFlushedBeforeCompleted(t *testing.T) {
	tmpDir := t.TempDir()

	ffmpegPath, err := writeStdoutMockFFmpeg(tmpDir)
	if err != nil {
		t.Fatalf("failed to create mock ffmpeg: %v", err)
	}

	mockSrv := newStdoutOrderMockServer()
	defer mockSrv.Close()

	cache, err := NewCache(CacheConfig{Enabled: false})
	if err != nil {
		t.Fatalf("NewCache failed: %v", err)
	}

	workerTempDir := filepath.Join(tmpDir, "worker-temp")
	if err := os.MkdirAll(workerTempDir, 0755); err != nil {
		t.Fatalf("mkdir worker-temp: %v", err)
	}

	executor := NewExecutor(ffmpegPath, 30*time.Second)
	client := NewClient(mockSrv.URL, "test-worker-001", "")

	rewriteAdapter := NewRewriteAdapter()
	rewriteAdapter.config.Enabled = false

	retryExecutor := NewRetryExecutor(executor, DefaultRetryConfig())

	w := &Worker{
		id:                 "test-worker-001",
		name:               "test-worker",
		client:             client,
		executor:           executor,
		retryExecutor:      retryExecutor,
		rewriteAdapter:     rewriteAdapter,
		cache:              cache,
		tempDir:            workerTempDir,
		activeJobs:         make(map[string]context.CancelFunc),
		heartbeatInterval:  30 * time.Second,
		pollInterval:       5 * time.Second,
		lastHeartbeatTime:  time.Now(),
		ffprobeExecutor:    NewFFprobeExecutor(""),
		pixelFormatChecker: nil,
		gpuDetector:        gpu.NewDetector(),
	}

	ctx := context.Background()
	jobCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	job := protocol.JobInfo{
		ID:              "test-job-streaming-stdout",
		InputFiles:      []string{"input-001"},
		Args:            []string{"-i", "<INPUT_FILE>", "-f", "mp4", "-"},
		OutputFilename:  "-",
		StreamingOutput: true,
	}

	w.processJob(jobCtx, job, cancel, false)

	mockSrv.mu.Lock()
	events := append([]string(nil), mockSrv.events...)
	mockSrv.mu.Unlock()

	completedIdx := -1
	lastStdoutIdx := -1
	for i, ev := range events {
		if ev == "completed" {
			completedIdx = i
		}
		if ev == "stdout" {
			lastStdoutIdx = i
		}
	}
	if completedIdx == -1 {
		t.Fatalf("no completed status PATCH observed; events=%v", events)
	}
	if lastStdoutIdx == -1 {
		t.Fatalf("no stdout chunk PATCH observed; events=%v", events)
	}
	if lastStdoutIdx > completedIdx {
		t.Errorf("stdout chunk PATCH (%d) arrived after completed status (%d): streamed bytes are lost to a closed client; events=%v",
			lastStdoutIdx, completedIdx, events)
	}
}
