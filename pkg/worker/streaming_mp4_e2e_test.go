package worker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tsic404/rffmpeg/pkg/protocol"
	"github.com/tsic404/rffmpeg/pkg/worker/gpu"
)

// writeArgRecordingFFmpeg creates an executable shell script that records its
// argv (one argument per line) to recordFile and exits 0. Streaming jobs write
// to stdout ("-") and produce no local output file, so the recorded argv is
// the only observable the test needs.
func writeArgRecordingFFmpeg(dir, recordFile string) (string, error) {
	scriptPath := filepath.Join(dir, "mock-ffmpeg-args")
	script := "#!/bin/bash\nprintf '%s\\n' \"$@\" > '" + recordFile + "'\nexit 0\n"
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		return "", err
	}
	return scriptPath, nil
}

// TestProcessJob_StreamingMp4SingleOutputDash is the TSI-2683 regression test:
// `rffmpeg -i in.mp4 -f mp4 -` must execute ffmpeg with exactly ONE output
// dash ("-") for stdout. A duplicate ("- -") makes ffmpeg fail with
// "Unable to choose an output format for 'pipe:'". The parser drops the output
// token from AllArgs (single point of removal) and the worker appends the
// resolved output path exactly once (single point of addition).
func TestProcessJob_StreamingMp4SingleOutputDash(t *testing.T) {
	tmpDir := t.TempDir()
	recordFile := filepath.Join(tmpDir, "ffmpeg-args.txt")
	ffmpegPath, err := writeArgRecordingFFmpeg(tmpDir, recordFile)
	if err != nil {
		t.Fatalf("failed to create arg-recording mock ffmpeg: %v", err)
	}

	mockSrv := newMockJobServer()
	defer mockSrv.Close()

	cacheDir := filepath.Join(tmpDir, "cache")
	cache, err := NewCache(CacheConfig{Enabled: true, Dir: cacheDir, TTL: 24 * time.Hour})
	if err != nil {
		t.Fatalf("NewCache failed: %v", err)
	}
	t.Cleanup(func() { cache.Stop() })

	workerTempDir := filepath.Join(tmpDir, "worker-temp")
	if err := os.MkdirAll(workerTempDir, 0o755); err != nil {
		t.Fatalf("mkdir worker-temp: %v", err)
	}

	executor := NewExecutor(ffmpegPath, 30*time.Second)
	client := NewClient(mockSrv.URL, uuid.New().String(), "")

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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Args are the parser output for `-f mp4 -`: the output token is excluded
	// and StreamingOutput is set. The worker must append exactly one "-".
	job := protocol.JobInfo{
		ID:              "stream-mp4-1",
		InputFiles:      []string{"input-001"},
		Args:            []string{"-i", "<INPUT_FILE>", "-f", "mp4"},
		OutputFilename:  "",
		StreamingOutput: true,
	}

	w.processJob(ctx, job, cancel, false)

	data, err := os.ReadFile(recordFile)
	if err != nil {
		t.Fatalf("read recorded argv: %v", err)
	}
	args := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(args) == 0 || args[0] == "" {
		t.Fatalf("recorded argv is empty; mock ffmpeg did not run?")
	}

	// The fixture passes no bare "-" tokens other than the resolved output, so
	// a global count is unambiguous: the input is introduced via "-i <INPUT_FILE>"
	// and every remaining flag carries a value.
	dashCount := 0
	for _, a := range args {
		if a == "-" {
			dashCount++
		}
	}
	// The contract is exactly one output dash, nothing about its position. A
	// duplicate ("- -") surfaces here as dashCount == 2.
	if dashCount != 1 {
		t.Fatalf("ffmpeg argv contains %d output dashes, want exactly 1: %v", dashCount, args)
	}

	// The movflags assertion is intentionally coupled to the TSI-2409 worker
	// behavior: streaming mp4 must be made independent of a seekable output.
	// If the single output dash is right but the fragmented flags are lost,
	// this path would regress from "works" to ffmpeg's muxer error.
	hasMovflags := false
	for _, a := range args {
		if a == "-movflags" {
			hasMovflags = true
			break
		}
	}
	if !hasMovflags {
		t.Errorf("ffmpeg argv missing -movflags injection for streaming mp4: %v", args)
	}
}
