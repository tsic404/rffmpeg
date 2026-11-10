package worker

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tsic404/rffmpeg/pkg/pathutil"
	"github.com/tsic404/rffmpeg/pkg/protocol"
	"github.com/tsic404/rffmpeg/pkg/worker/gpu"
)

// directOutputTestJob builds a shared-FS job whose output is the absolute path
// under test. Args carry no -y so the worker exercises the default overwrite
// policy.
func directOutputTestJob(id, inputPath, outputPath string) protocol.JobInfo {
	return protocol.JobInfo{
		ID:             id,
		InputFiles:     []string{inputPath},
		Args:           []string{"-i", "<INPUT_FILE>", "-c:v", "libx264"},
		OutputFilename: outputPath,
		DirectPaths:    []string{inputPath},
	}
}

// newDirectOutputWorker wires a worker to the given ffmpeg fixture with rewrite
// disabled, so the fixture receives the job args verbatim.
func newDirectOutputWorker(t *testing.T, ffmpegPath string) (*Worker, *probeDirectMockServer) {
	t.Helper()

	mockSrv := newProbeDirectMockServer()
	t.Cleanup(mockSrv.Close)

	cache, err := NewCache(CacheConfig{
		Enabled: true,
		Dir:     filepath.Join(t.TempDir(), "cache"),
		TTL:     24 * time.Hour,
	})
	if err != nil {
		t.Fatalf("NewCache failed: %v", err)
	}
	t.Cleanup(func() { cache.Stop() })

	workerTempDir := filepath.Join(t.TempDir(), "worker-temp")
	if err := os.MkdirAll(workerTempDir, 0o755); err != nil {
		t.Fatalf("mkdir worker-temp: %v", err)
	}

	executor := NewExecutor(ffmpegPath, 30*time.Second)
	rewriteAdapter := NewRewriteAdapter()
	rewriteAdapter.config.Enabled = false

	w := &Worker{
		id:                "test-worker-001",
		name:              "test-worker",
		client:            NewClient(mockSrv.URL, "test-worker-001", ""),
		executor:          executor,
		retryExecutor:     NewRetryExecutor(executor, DefaultRetryConfig()),
		rewriteAdapter:    rewriteAdapter,
		cache:             cache,
		tempDir:           workerTempDir,
		activeJobs:        make(map[string]context.CancelFunc),
		heartbeatInterval: 30 * time.Second,
		pollInterval:      5 * time.Second,
		lastHeartbeatTime: time.Now(),
		// Any probe failure is fine; these tests only exercise ffmpeg output.
		ffprobeExecutor: NewFFprobeExecutor("false"),
		gpuDetector:     gpu.NewDetector(),
		sharedFS:        pathutil.NewSpace(os.TempDir()),
	}

	return w, mockSrv
}

// assertNoOutputArtifacts fails when the shared output directory still holds
// either the final output or a staged temp file.
func assertNoOutputArtifacts(t *testing.T, dir, finalPath string) {
	t.Helper()
	if _, err := os.Stat(finalPath); err == nil {
		t.Fatalf("partial output left at shared target %s", finalPath)
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat shared target %s: %v", finalPath, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read output dir %s: %v", dir, err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".rffmpeg-") {
			t.Fatalf("staged temp output left behind: %s", filepath.Join(dir, entry.Name()))
		}
	}
}

// TestProcessJob_DirectOutputFailureLeavesNoPartialOutput covers the non-zero
// exit path: the fixture writes bytes an eventual reader would see as output,
// then fails. Those bytes must never land on the shared target.
func TestProcessJob_DirectOutputFailureLeavesNoPartialOutput(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "ffmpeg-partial-fail")
	if err := writeShellFixture(script, `output=""
for arg in "$@"; do output="$arg"; done
printf 'PARTIAL-OUTPUT' > "$output"
echo "mock ffmpeg failed" >&2
exit 1
`); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	inputPath := filepath.Join(dir, "input.mp4")
	if err := os.WriteFile(inputPath, []byte("input"), 0o644); err != nil {
		t.Fatalf("write input: %v", err)
	}
	outputPath := filepath.Join(dir, "out.mp4")

	w, mockSrv := newDirectOutputWorker(t, script)
	w.processJob(context.Background(), directOutputTestJob("direct-fail", inputPath, outputPath), func() {}, false)

	update := terminalUpdate(t, mockSrv)
	if update.Status != protocol.JobStatusFailed {
		t.Fatalf("Status = %q, want %q", update.Status, protocol.JobStatusFailed)
	}
	assertNoOutputArtifacts(t, dir, outputPath)
}

// TestProcessJob_DirectOutputTimeoutLeavesNoPartialOutput covers the timeout
// path: ffmpeg is still encoding when the execution budget expires, and the
// bytes written so far must not remain reachable at the shared target.
func TestProcessJob_DirectOutputTimeoutLeavesNoPartialOutput(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "ffmpeg-partial-sleep")
	if err := writeShellFixture(script, `output=""
for arg in "$@"; do output="$arg"; done
printf 'PARTIAL-OUTPUT' > "$output"
sleep 5
`); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	inputPath := filepath.Join(dir, "input.mp4")
	if err := os.WriteFile(inputPath, []byte("input"), 0o644); err != nil {
		t.Fatalf("write input: %v", err)
	}
	outputPath := filepath.Join(dir, "out.mp4")

	w, mockSrv := newDirectOutputWorker(t, script)
	job := directOutputTestJob("direct-timeout", inputPath, outputPath)
	timeout := 300 * time.Millisecond
	job.Timeout = &timeout

	w.processJob(context.Background(), job, func() {}, false)

	update := terminalUpdate(t, mockSrv)
	if update.Status != protocol.JobStatusTimeout {
		t.Fatalf("Status = %q, want %q", update.Status, protocol.JobStatusTimeout)
	}
	assertNoOutputArtifacts(t, dir, outputPath)
}

// TestProcessJob_DirectOutputCancelLeavesNoPartialOutput covers the cancel
// path: the client abandons a running encode, so the staged bytes must be
// discarded instead of committed.
func TestProcessJob_DirectOutputCancelLeavesNoPartialOutput(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "encoding-started")
	script := filepath.Join(dir, "ffmpeg-partial-cancel")
	if err := writeShellFixture(script, `output=""
for arg in "$@"; do output="$arg"; done
printf 'PARTIAL-OUTPUT' > "$output"
: > '`+marker+`'
sleep 5
`); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	inputPath := filepath.Join(dir, "input.mp4")
	if err := os.WriteFile(inputPath, []byte("input"), 0o644); err != nil {
		t.Fatalf("write input: %v", err)
	}
	outputPath := filepath.Join(dir, "out.mp4")

	w, mockSrv := newDirectOutputWorker(t, script)
	jobCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		w.processJob(jobCtx, directOutputTestJob("direct-cancel", inputPath, outputPath), cancel, false)
	}()

	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("ffmpeg fixture never started encoding")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done

	update := terminalUpdate(t, mockSrv)
	if update.Status != protocol.JobStatusCancelled {
		t.Fatalf("Status = %q, want %q", update.Status, protocol.JobStatusCancelled)
	}
	assertNoOutputArtifacts(t, dir, outputPath)
}

// TestProcessJob_DirectOutputSuccessRenamesIntoPlace proves the success path
// still publishes the output at the requested path and leaves no staged file.
func TestProcessJob_DirectOutputSuccessRenamesIntoPlace(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "ffmpeg-success")
	if err := writeShellFixture(script, `output=""
for arg in "$@"; do output="$arg"; done
printf 'COMPLETE-OUTPUT' > "$output"
exit 0
`); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	inputPath := filepath.Join(dir, "input.mp4")
	if err := os.WriteFile(inputPath, []byte("input"), 0o644); err != nil {
		t.Fatalf("write input: %v", err)
	}
	outputPath := filepath.Join(dir, "out.mp4")

	w, mockSrv := newDirectOutputWorker(t, script)
	w.processJob(context.Background(), directOutputTestJob("direct-success", inputPath, outputPath), func() {}, false)

	update := terminalUpdate(t, mockSrv)
	if update.Status != protocol.JobStatusCompleted {
		t.Fatalf("Status = %q, want %q (error=%q)", update.Status, protocol.JobStatusCompleted, update.Error)
	}
	data, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatalf("read final output: %v", err)
	}
	if string(data) != "COMPLETE-OUTPUT" {
		t.Fatalf("final output = %q, want the fixture bytes", data)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read output dir: %v", err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".rffmpeg-") {
			t.Fatalf("staged temp output survived success: %s", entry.Name())
		}
	}
}

// TestProcessJob_DirectOutputPreservesExistingWithoutOverwrite keeps native
// overwrite semantics intact now that ffmpeg no longer sees the final path:
// without -y an existing output must fail the job and stay byte-for-byte
// unchanged.
func TestProcessJob_DirectOutputPreservesExistingWithoutOverwrite(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "ffmpeg-success")
	if err := writeShellFixture(script, `output=""
for arg in "$@"; do output="$arg"; done
printf 'NEW-OUTPUT' > "$output"
exit 0
`); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	inputPath := filepath.Join(dir, "input.mp4")
	if err := os.WriteFile(inputPath, []byte("input"), 0o644); err != nil {
		t.Fatalf("write input: %v", err)
	}
	outputPath := filepath.Join(dir, "out.mp4")
	if err := os.WriteFile(outputPath, []byte("USER-DATA"), 0o644); err != nil {
		t.Fatalf("write existing output: %v", err)
	}

	w, mockSrv := newDirectOutputWorker(t, script)
	w.processJob(context.Background(), directOutputTestJob("direct-no-overwrite", inputPath, outputPath), func() {}, false)

	update := terminalUpdate(t, mockSrv)
	if update.Status == protocol.JobStatusCompleted {
		t.Fatalf("existing output was overwritten without -y")
	}
	data, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatalf("read existing output: %v", err)
	}
	if string(data) != "USER-DATA" {
		t.Fatalf("existing output modified: %q", data)
	}
	if atomic.LoadInt32(&mockSrv.terminalCount) != 1 {
		t.Fatalf("terminal updates = %d, want 1", mockSrv.terminalCount)
	}
}

// TestProcessJob_DirectOutputRetryFallbackPublishesOutput covers the
// multi-stage retry path in direct mode: the initial hardware encoder fails,
// the retry executor falls back to a software encoder, and the successful
// attempt must be validated and published at the shared target. Passing the
// final path to the retry executor instead of the staged path makes it stat a
// file no attempt wrote, so the successful fallback is reported as an empty
// output and the job fails with all retries exhausted.
func TestProcessJob_DirectOutputRetryFallbackPublishesOutput(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "ffmpeg-fallback")
	if err := writeShellFixture(script, `output=""
for arg in "$@"; do output="$arg"; done
case " $* " in
  *h264_nvenc*)
    echo "Unknown encoder 'h264_nvenc'" >&2
    exit 1
    ;;
esac
printf 'FALLBACK-OUTPUT' > "$output"
exit 0
`); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	inputPath := filepath.Join(dir, "input.mp4")
	if err := os.WriteFile(inputPath, []byte("input"), 0o644); err != nil {
		t.Fatalf("write input: %v", err)
	}
	outputPath := filepath.Join(dir, "out.mp4")

	w, mockSrv := newDirectOutputWorker(t, script)
	job := directOutputTestJob("direct-fallback", inputPath, outputPath)
	job.Args = []string{"-c:v", "h264_nvenc", "-i", "<INPUT_FILE>"}

	w.processJob(context.Background(), job, func() {}, false)

	update := terminalUpdate(t, mockSrv)
	if update.Status != protocol.JobStatusCompleted {
		t.Fatalf("Status = %q, want %q (failure_type=%q error=%q)",
			update.Status, protocol.JobStatusCompleted, update.FailureType, update.Error)
	}
	data, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatalf("read published output: %v", err)
	}
	if string(data) != "FALLBACK-OUTPUT" {
		t.Fatalf("published output = %q, want the fallback attempt's bytes", data)
	}
	assertNoStagingDirs(t, dir)
}

// TestProcessJob_DirectOutputHLSPublishesSegmentsAndPlaylist covers a
// segmented output: ffmpeg derives segment names from the playlist basename,
// so staging must keep that basename and publish every artifact under it. A
// hidden temp filename would leave the published playlist pointing at
// ".rffmpeg-...0.ts" files that do not exist.
func TestProcessJob_DirectOutputHLSPublishesSegmentsAndPlaylist(t *testing.T) {
	ffmpegPath, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}

	dir := t.TempDir()
	inputPath := genTestInputMP4(t, dir)
	outputPath := filepath.Join(dir, "out.m3u8")

	w, mockSrv := newDirectOutputWorker(t, ffmpegPath)
	job := directOutputTestJob("direct-hls", inputPath, outputPath)
	job.Args = []string{"-i", "<INPUT_FILE>", "-c:v", "libx264", "-preset", "ultrafast", "-f", "hls"}

	w.processJob(context.Background(), job, func() {}, false)

	update := terminalUpdate(t, mockSrv)
	if update.Status != protocol.JobStatusCompleted {
		t.Fatalf("Status = %q, want %q (failure_type=%q error=%q)",
			update.Status, protocol.JobStatusCompleted, update.FailureType, update.Error)
	}

	playlist, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatalf("read published playlist: %v", err)
	}
	if !strings.Contains(string(playlist), "out0.ts") {
		t.Fatalf("playlist does not reference out0.ts:\n%s", playlist)
	}
	if strings.Contains(string(playlist), ".rffmpeg") {
		t.Fatalf("playlist references a staging name:\n%s", playlist)
	}
	if info, err := os.Stat(filepath.Join(dir, "out0.ts")); err != nil {
		t.Fatalf("published segment missing: %v", err)
	} else if info.Size() == 0 {
		t.Fatal("published segment is empty")
	}

	// Only the playlist and its segments remain: no staging directory, no
	// hidden artifacts.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read output dir: %v", err)
	}
	published := make(map[string]bool, len(entries))
	for _, entry := range entries {
		published[entry.Name()] = true
		if strings.HasPrefix(entry.Name(), ".rffmpeg") {
			t.Fatalf("staging artifact left behind: %s", entry.Name())
		}
	}
	for _, want := range []string{"out.m3u8", "out0.ts", "input.mp4"} {
		if !published[want] {
			t.Fatalf("missing %s in output dir %v", want, published)
		}
	}
}

// assertNoStagingDirs fails when a staging directory survived the job.
func assertNoStagingDirs(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read output dir %s: %v", dir, err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".rffmpeg-staging-") {
			t.Fatalf("staging directory left behind: %s", entry.Name())
		}
	}
}

// TestProcessJob_DirectOutputStagesInsideWorkspace pins C-126's boundary: the
// job writes its staged output inside its own workspace, and the only path it
// creates beside the target is the target itself. A staging directory next to
// the target would be a second path outside the workspace, which the pass-
// through exception does not cover.
func TestProcessJob_DirectOutputStagesInsideWorkspace(t *testing.T) {
	dir := t.TempDir()
	record := filepath.Join(dir, "staging-path")
	script := filepath.Join(dir, "ffmpeg-record-staging")
	if err := writeShellFixture(script, `output=""
for arg in "$@"; do output="$arg"; done
printf '%s\n' "$output" > '`+record+`'
mkdir -p "$(dirname "$output")"
printf 'ENCODED' > "$output"
`); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	targetDir := filepath.Join(dir, "shared")
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		t.Fatalf("mkdir target dir: %v", err)
	}
	inputPath := filepath.Join(dir, "input.mp4")
	if err := os.WriteFile(inputPath, []byte("input"), 0o644); err != nil {
		t.Fatalf("write input: %v", err)
	}
	outputPath := filepath.Join(targetDir, "out.mp4")

	w, mockSrv := newDirectOutputWorker(t, script)
	runJob(t, w, directOutputTestJob("direct-staging", inputPath, outputPath))

	update := terminalUpdate(t, mockSrv)
	if update.Status != protocol.JobStatusCompleted {
		t.Fatalf("Status = %q, want completed (%s)", update.Status, update.Error)
	}

	raw, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("fixture did not record its output path: %v", err)
	}
	ffmpegOutput := strings.TrimSpace(string(raw))
	rel, err := filepath.Rel(w.tempDir, ffmpegOutput)
	if err != nil || strings.HasPrefix(rel, "..") {
		t.Fatalf("ffmpeg wrote outside the job workspace: %s (workspace %s)", ffmpegOutput, w.tempDir)
	}
	entries, err := os.ReadDir(targetDir)
	if err != nil {
		t.Fatalf("read target dir: %v", err)
	}
	for _, entry := range entries {
		if entry.Name() != "out.mp4" {
			t.Fatalf("job created %q beside the target; only the target itself may appear (C-126)", entry.Name())
		}
	}
}

// TestProcessJob_DirectOutputPublishFailureLeavesNoResidue covers the publish
// step: when the target directory refuses the move, neither the target nor a
// partial copy remains.
func TestProcessJob_DirectOutputPublishFailureLeavesNoResidue(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "ffmpeg-writes-output")
	if err := writeShellFixture(script, `output=""
for arg in "$@"; do output="$arg"; done
mkdir -p "$(dirname "$output")"
printf 'ENCODED' > "$output"
`); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	targetDir := filepath.Join(dir, "shared")
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		t.Fatalf("mkdir target dir: %v", err)
	}
	inputPath := filepath.Join(dir, "input.mp4")
	if err := os.WriteFile(inputPath, []byte("input"), 0o644); err != nil {
		t.Fatalf("write input: %v", err)
	}
	outputPath := filepath.Join(targetDir, "out.mp4")

	w, mockSrv := newDirectOutputWorker(t, script)
	// Refuse the publish: the target directory accepts no new entry.
	if err := os.Chmod(targetDir, 0o555); err != nil {
		t.Fatalf("chmod target dir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(targetDir, 0o755) })

	runJob(t, w, directOutputTestJob("direct-publish-fail", inputPath, outputPath))

	update := terminalUpdate(t, mockSrv)
	if update.Status == protocol.JobStatusCompleted {
		t.Fatal("publish failure was reported as completed")
	}
	if !strings.Contains(update.Error, "publish staged output") {
		t.Fatalf("job failed for %q, want the publish step", update.Error)
	}
	entries, err := os.ReadDir(targetDir)
	if err != nil {
		t.Fatalf("read target dir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("publish failure left %d entry/entries in the target directory", len(entries))
	}
}
