package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tsic404/rffmpeg/pkg/protocol"
	"github.com/tsic404/rffmpeg/pkg/worker/gpu"
)

// shellSingleQuote wraps s so a shell reads it as one literal argument.
func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// writeMockFFmpegFailure writes an executable standing in for ffmpeg in one
// deterministic failure mode: it optionally sleeps (so a short job timeout
// kills it mid-run), prints the given stderr lines, and exits with exitCode.
func writeMockFFmpegFailure(dir, name string, exitCode, sleepSeconds int, stderrLines ...string) (string, error) {
	var script strings.Builder
	script.WriteString("#!/bin/bash\n")
	if sleepSeconds > 0 {
		fmt.Fprintf(&script, "sleep %d\n", sleepSeconds)
	}
	for _, line := range stderrLines {
		fmt.Fprintf(&script, "printf '%%s\\n' %s >&2\n", shellSingleQuote(line))
	}
	fmt.Fprintf(&script, "exit %d\n", exitCode)

	scriptPath := filepath.Join(dir, name)
	if err := os.WriteFile(scriptPath, []byte(script.String()), 0755); err != nil {
		return "", err
	}
	return scriptPath, nil
}

// failureMatrixServer records the terminal job PATCH and can break either
// worker↔server data path the matrix needs: fetching an input file (a server
// file ID that cannot be fetched is INFRA, not an input problem) and uploading
// the output.
type failureMatrixServer struct {
	*httptest.Server
	mu            sync.Mutex
	terminalBody  []byte
	terminalCount int32

	failInputDownload bool
	failOutputUpload  bool
}

func newFailureMatrixServer(failInputDownload, failOutputUpload bool) *failureMatrixServer {
	m := &failureMatrixServer{failInputDownload: failInputDownload, failOutputUpload: failOutputUpload}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/files/", func(w http.ResponseWriter, r *http.Request) {
		if m.failInputDownload {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"code":"internal","message":"data channel down"}}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("mock input video data"))
	})
	mux.HandleFunc("/api/v1/jobs/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/output") {
			if m.failOutputUpload {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"error":{"code":"internal","message":"upload channel down"}}`))
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"message":"ok"}`))
			return
		}
		if r.Method == http.MethodPatch {
			body, _ := io.ReadAll(r.Body)
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
	mux.HandleFunc("/api/v1/workers/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"worker_id":"test-worker-001"}`))
	})
	m.Server = httptest.NewServer(mux)
	return m
}

// terminalUpdate returns the terminal job update the server recorded.
func (m *failureMatrixServer) terminalUpdate(t *testing.T) protocol.JobUpdateRequest {
	t.Helper()
	if atomic.LoadInt32(&m.terminalCount) == 0 {
		t.Fatal("expected a terminal job update PATCH, got none")
	}
	m.mu.Lock()
	body := m.terminalBody
	m.mu.Unlock()

	var update protocol.JobUpdateRequest
	if err := json.Unmarshal(body, &update); err != nil {
		t.Fatalf("failed to parse terminal update body: %v\nbody: %s", err, body)
	}
	return update
}

// failureMatrixCase is one row of the classification matrix: the fixture that
// provokes the failure, plus the category and terminal status the worker must
// report for it.
type failureMatrixCase struct {
	name       string
	wantType   protocol.FailureType
	wantStatus protocol.JobStatus

	// stderrLines/exitCode/sleepSeconds configure a mock ffmpeg that fails. A
	// case whose failure happens before or after ffmpeg (both INFRA rows) leaves
	// them zero-valued.
	stderrLines  []string
	exitCode     int
	sleepSeconds int

	// writesOutput swaps in a mock ffmpeg that exits 0 and writes the output
	// file, so the job reaches the output-upload stage instead of failing in
	// ffmpeg.
	writesOutput bool

	args       []string
	jobTimeout time.Duration

	// wantErrorContains pins the write point a row covers when several share a
	// category (the two INFRA rows), so the row proves that path ran rather
	// than merely producing the right label.
	wantErrorContains string

	failInputDownload bool
	failOutputUpload  bool
}

// failureMatrixCases covers every failure category a worker observes. The two
// categories the server decides before a worker ever runs
// (ENCODER_UNAVAILABLE, NO_WORKER_AVAILABLE) cannot be rows here; their
// end-to-end coverage is named in serverDecidedFailureTypes.
var failureMatrixCases = []failureMatrixCase{
	{
		name:        "DISK_FULL",
		wantType:    protocol.FailureDiskFull,
		wantStatus:  protocol.JobStatusFailed,
		stderrLines: []string{"Error writing output file: No space left on device"},
		exitCode:    1,
		args:        []string{"-i", "<INPUT_FILE>", "-c:v", "libx264", "-preset", "fast"},
	},
	{
		name:        "ENCODER_UNSUPPORTED",
		wantType:    protocol.FailureEncoderUnsupported,
		wantStatus:  protocol.JobStatusFailed,
		stderrLines: []string{"Unknown encoder 'h265_fake'"},
		exitCode:    1,
		args:        []string{"-i", "<INPUT_FILE>", "-c:v", "h265_fake"},
	},
	{
		name:        "INPUT_UNREACHABLE",
		wantType:    protocol.FailureInputUnreachable,
		wantStatus:  protocol.JobStatusFailed,
		stderrLines: []string{"input.mp4: No such file or directory"},
		exitCode:    1,
		args:        []string{"-i", "<INPUT_FILE>", "-c:v", "libx264", "-preset", "fast"},
	},
	{
		name:        "FFMPEG_ERROR",
		wantType:    protocol.FailureFFmpegError,
		wantStatus:  protocol.JobStatusFailed,
		stderrLines: []string{"Error while decoding stream #0:0: Generic error"},
		exitCode:    1,
		args:        []string{"-i", "<INPUT_FILE>", "-c:v", "libx264", "-preset", "fast"},
	},
	{
		// 137 is SIGKILL — what the OS OOM-killer leaves behind, and the exit
		// code ffmpeg n9 sporadically reports when it self-aborts under load.
		name:       "WORKER_CRASH",
		wantType:   protocol.FailureWorkerCrash,
		wantStatus: protocol.JobStatusFailed,
		exitCode:   137,
		args:       []string{"-i", "<INPUT_FILE>", "-c:v", "libx264", "-preset", "fast"},
	},
	{
		name:         "TIMEOUT",
		wantType:     protocol.FailureTimeout,
		wantStatus:   protocol.JobStatusTimeout,
		sleepSeconds: 10,
		jobTimeout:   300 * time.Millisecond,
		args:         []string{"-i", "<INPUT_FILE>", "-c:v", "libx264", "-preset", "fast"},
	},
	{
		// A server file ID that cannot be fetched over the worker↔server
		// channel is infrastructure, not a user input problem.
		name:              "INFRA_INPUT_DOWNLOAD",
		wantType:          protocol.FailureInfra,
		wantStatus:        protocol.JobStatusFailed,
		wantErrorContains: "Failed to download input file",
		failInputDownload: true,
		args:              []string{"-i", "<INPUT_FILE>", "-c:v", "libx264", "-preset", "fast"},
	},
	{
		// A successful transcode whose output upload fails over the
		// worker↔server channel is infrastructure, not an ffmpeg error.
		name:              "INFRA_OUTPUT_UPLOAD",
		wantType:          protocol.FailureInfra,
		wantStatus:        protocol.JobStatusFailed,
		wantErrorContains: "Failed to upload output",
		writesOutput:      true,
		failOutputUpload:  true,
		args:              []string{"-i", "<INPUT_FILE>", "-c:v", "libx264", "-preset", "fast"},
	},
}

// serverDecidedFailureTypes are set by the server before a worker is involved,
// so no worker fixture can produce them. coveredBy names the end-to-end server
// test that does.
var serverDecidedFailureTypes = []struct {
	failureType protocol.FailureType
	coveredBy   string
}{
	{protocol.FailureEncoderUnavailable, "pkg/server/handlers TestSubmitJobNoWorkerWithEncoder"},
	{protocol.FailureNoWorkerAvailable, "pkg/server/scheduler TestSchedulerNoWorkerStarvation"},
}

// newFailureMatrixWorker builds a worker wired to a mock ffmpeg and a mock
// server. The retry budget is a single attempt: the matrix asserts the reported
// failure category, while multi-attempt retry/fallback behaviour has its own
// tests (retry_executor_test.go, timeout_budget_test.go).
func newFailureMatrixWorker(t *testing.T, ffmpegPath, serverURL, tmpDir string) *Worker {
	t.Helper()

	cache, err := NewCache(CacheConfig{Enabled: true, Dir: filepath.Join(tmpDir, "cache"), TTL: 24 * time.Hour})
	if err != nil {
		t.Fatalf("NewCache failed: %v", err)
	}
	t.Cleanup(func() { cache.Stop() })

	workerTempDir := filepath.Join(tmpDir, "worker-temp")
	if err := os.MkdirAll(workerTempDir, 0755); err != nil {
		t.Fatalf("mkdir worker-temp: %v", err)
	}

	executor := NewExecutor(ffmpegPath, 30*time.Second)
	retryExecutor := NewRetryExecutor(executor, &RetryConfig{
		MaxRetries:      1,
		InitialInterval: 10 * time.Millisecond,
	})

	rewriteAdapter := NewRewriteAdapter()
	rewriteAdapter.config.Enabled = false

	return &Worker{
		id:                "test-worker-001",
		name:              "test-worker",
		client:            NewClient(serverURL, "test-worker-001", ""),
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
}

// TestProcessJobFailureClassificationMatrix drives the real processJob pipeline
// once per worker-observable failure category — each with its own fixture — and
// asserts the terminal job update carries that category. It is the end-to-end
// counterpart of the per-category unit tests in failure_classify_test.go.
func TestProcessJobFailureClassificationMatrix(t *testing.T) {
	for _, tc := range failureMatrixCases {
		t.Run(tc.name, func(t *testing.T) {
			tmpDir := t.TempDir()

			var ffmpegPath string
			var err error
			if tc.writesOutput {
				ffmpegPath, err = writeMockFFmpeg(tmpDir)
			} else {
				ffmpegPath, err = writeMockFFmpegFailure(tmpDir, "mock-ffmpeg",
					tc.exitCode, tc.sleepSeconds, tc.stderrLines...)
			}
			if err != nil {
				t.Fatalf("write mock ffmpeg: %v", err)
			}

			srv := newFailureMatrixServer(tc.failInputDownload, tc.failOutputUpload)
			defer srv.Close()

			w := newFailureMatrixWorker(t, ffmpegPath, srv.URL, tmpDir)

			job := protocol.JobInfo{
				ID:             "matrix-" + strings.ToLower(tc.name),
				InputFiles:     []string{"input-001"},
				Args:           tc.args,
				OutputFilename: "out.mp4",
			}
			if tc.jobTimeout > 0 {
				timeout := tc.jobTimeout
				job.Timeout = &timeout
			}

			jobCtx, cancel := context.WithCancel(context.Background())
			defer cancel()

			w.processJob(jobCtx, job, cancel, false)

			update := srv.terminalUpdate(t)
			if update.Status != tc.wantStatus {
				t.Errorf("Status = %q, want %q (failure_type=%q, error=%q)",
					update.Status, tc.wantStatus, update.FailureType, update.Error)
			}
			if update.FailureType != string(tc.wantType) {
				t.Errorf("FailureType = %q, want %q (error=%q, details=%q)",
					update.FailureType, tc.wantType, update.Error, update.FailureDetails)
			}
			if tc.wantErrorContains != "" && !strings.Contains(update.Error, tc.wantErrorContains) {
				t.Errorf("Error = %q, want it to contain %q (the write point this row covers)",
					update.Error, tc.wantErrorContains)
			}
		})
	}
}

// TestFailureClassificationMatrixCoversAllTypes locks the matrix's
// completeness: every failure category the protocol defines must have at least
// one end-to-end trigger, either a worker row above or a server-decided
// category below.
//
// The guarantee is bounded by two hand-maintained lists, because Go cannot
// enumerate the protocol.FailureType constants and this package cannot see
// another package's tests: `all` mirrors the constants and
// serverDecidedFailureTypes' coveredBy strings name the server tests that
// exercise them. Adding a category therefore means updating `all`, adding its
// trigger, and — for a submit-time category — listing the server test that
// covers it; renaming or deleting a referenced server test is a manual sync
// obligation this test cannot detect.
func TestFailureClassificationMatrixCoversAllTypes(t *testing.T) {
	all := []protocol.FailureType{
		protocol.FailureInputUnreachable,
		protocol.FailureEncoderUnsupported,
		protocol.FailureEncoderUnavailable,
		protocol.FailureDiskFull,
		protocol.FailureTimeout,
		protocol.FailureWorkerCrash,
		protocol.FailureFFmpegError,
		protocol.FailureNoWorkerAvailable,
		protocol.FailureInfra,
	}

	// One category may have several rows (INFRA has one per write point), so
	// each type maps to every trigger that covers it.
	covered := make(map[protocol.FailureType][]string)
	for _, tc := range failureMatrixCases {
		if !tc.wantType.IsValid() {
			t.Errorf("matrix case %s expects invalid failure type %q", tc.name, tc.wantType)
		}
		covered[tc.wantType] = append(covered[tc.wantType], "worker matrix case "+tc.name)
	}
	for _, sd := range serverDecidedFailureTypes {
		if !sd.failureType.IsValid() {
			t.Errorf("server-decided entry %q is not a valid failure type", sd.failureType)
		}
		covered[sd.failureType] = append(covered[sd.failureType], sd.coveredBy)
	}

	if len(covered) != len(all) {
		t.Errorf("protocol defines %d failure types, the matrix accounts for %d", len(all), len(covered))
	}
	for _, ft := range all {
		if len(covered[ft]) == 0 {
			t.Errorf("failure type %s has no end-to-end coverage in the matrix", ft)
		}
	}
}
