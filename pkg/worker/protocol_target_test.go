package worker

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tsic404/rffmpeg/pkg/protocol"
)

// captureFFmpegArgs writes an ffmpeg stand-in that records every argument it
// receives and exits 0, so a network-output job can be proven to hand the
// protocol address to ffmpeg verbatim instead of writing a local file.
func captureFFmpegArgs(t *testing.T, argsFile string) string {
	t.Helper()
	script := filepath.Join(t.TempDir(), "ffmpeg-capture-args")
	body := "printf '%s\\n' \"$@\" > " + shellSingleQuote(argsFile) + "\nexit 0\n"
	if err := writeShellFixture(script, body); err != nil {
		t.Fatalf("write capture ffmpeg: %v", err)
	}
	return script
}

// TestProcessJobProtocolTargetIsHandedToFFmpeg covers the execution side of
// C-125 / C-129: a protocol address target is passed to ffmpeg verbatim, no
// local output file exists afterwards, nothing is uploaded and the cache is
// bypassed (C-96).
func TestProcessJobProtocolTargetIsHandedToFFmpeg(t *testing.T) {
	w, mockSrv := setupTestWorker(t, 24*time.Hour)
	argsFile := filepath.Join(t.TempDir(), "args.txt")
	w.executor = NewExecutor(captureFFmpegArgs(t, argsFile), 30*time.Second)

	target := "rtmp://10.20.30.40:1935/live/stream"
	job := protocol.JobInfo{
		ID:             "protocol-target-001",
		InputFiles:     []string{testUploadID("protocol-target")},
		Args:           []string{"-i", "<INPUT_FILE>", "-c:v", "libx264", "-f", "flv"},
		OutputFilename: target,
	}
	runJob(t, w, job)

	raw, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("ffmpeg never ran for a protocol address target: %v", err)
	}
	args := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if len(args) == 0 || args[len(args)-1] != target {
		t.Fatalf("ffmpeg output argument = %q, want the protocol address %q (args: %v)",
			args[len(args)-1], target, args)
	}
	for _, arg := range args {
		if filepath.IsAbs(arg) && strings.HasSuffix(arg, "rtmp:") {
			t.Errorf("ffmpeg received a rewritten local path instead of the address: %q", arg)
		}
	}

	var update protocol.JobUpdateRequest
	if err := json.Unmarshal(mockSrv.lastUpdateBody, &update); err != nil {
		t.Fatalf("decode terminal update: %v", err)
	}
	if update.Status != protocol.JobStatusCompleted {
		t.Errorf("job status = %q, want %q (error: %s)", update.Status, protocol.JobStatusCompleted, update.Error)
	}
	if got := mockSrv.UploadCount(); got != 0 {
		t.Errorf("uploaded %d output file(s) for a network output, want 0", got)
	}
	if got := w.cache.Stats().EntryCount; got != 0 {
		t.Errorf("cache holds %d entry for a network output, want 0", got)
	}
}

// TestProcessJobDeniedProtocolTargetFailsBeforeFFmpeg covers the execution-side
// address judgement of C-129: a host name resolving into a denied class fails
// the job as INFRA before ffmpeg starts, so the target receives no connection.
func TestProcessJobDeniedProtocolTargetFailsBeforeFFmpeg(t *testing.T) {
	w, mockSrv := setupTestWorker(t, 24*time.Hour)
	marker := filepath.Join(t.TempDir(), "ffmpeg-ran")
	script := filepath.Join(t.TempDir(), "ffmpeg-marker")
	if err := writeShellFixture(script, "touch "+shellSingleQuote(marker)+"\nexit 0\n"); err != nil {
		t.Fatalf("write marker ffmpeg: %v", err)
	}
	w.executor = NewExecutor(script, 30*time.Second)

	job := protocol.JobInfo{
		ID:             "protocol-target-denied",
		InputFiles:     []string{testUploadID("protocol-target-denied")},
		Args:           []string{"-i", "<INPUT_FILE>", "-c:v", "libx264", "-f", "flv"},
		OutputFilename: "rtmp://localhost:1935/live/stream",
	}
	runJob(t, w, job)

	if _, err := os.Stat(marker); err == nil {
		t.Fatal("ffmpeg ran for a job whose push target resolves into a denied address class")
	}
	var update protocol.JobUpdateRequest
	if err := json.Unmarshal(mockSrv.lastUpdateBody, &update); err != nil {
		t.Fatalf("decode terminal update: %v", err)
	}
	if update.Status != protocol.JobStatusFailed {
		t.Errorf("job status = %q, want %q", update.Status, protocol.JobStatusFailed)
	}
	if update.FailureType != string(protocol.FailureInfra) {
		t.Errorf("failure_type = %q, want %q", update.FailureType, protocol.FailureInfra)
	}
	if !strings.Contains(update.FailureDetails, "not permitted") {
		t.Errorf("failure_details = %q, want the address-policy refusal", update.FailureDetails)
	}
	if got := mockSrv.UploadCount(); got != 0 {
		t.Errorf("uploaded %d output file(s) for a denied target, want 0", got)
	}
}
