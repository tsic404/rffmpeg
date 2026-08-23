package worker

import (
	"testing"

	"github.com/tsix404/rffmpeg/pkg/protocol"
)

func TestClassifyFailure(t *testing.T) {
	tests := []struct {
		name          string
		exitCode      int
		stderr        string
		errorMessage  string
		isTimeout     bool
		isWorkerCrash bool
		wantType      protocol.FailureType
	}{
		{
			name:          "worker crash",
			exitCode:      -1,
			stderr:        "",
			errorMessage:  "signal: killed",
			isTimeout:     false,
			isWorkerCrash: true,
			wantType:      protocol.FailureWorkerCrash,
		},
		{
			name:          "timeout",
			exitCode:      -1,
			stderr:        "",
			errorMessage:  "context deadline exceeded",
			isTimeout:     true,
			isWorkerCrash: false,
			wantType:      protocol.FailureTimeout,
		},
		{
			name:          "disk full",
			exitCode:      1,
			stderr:        "Error writing output file: No space left on device",
			errorMessage:  "ffmpeg exited with code 1",
			isTimeout:     false,
			isWorkerCrash: false,
			wantType:      protocol.FailureDiskFull,
		},
		{
			name:          "input unreachable - no such file",
			exitCode:      1,
			stderr:        "file.mp4: No such file or directory",
			errorMessage:  "ffmpeg exited with code 1",
			isTimeout:     false,
			isWorkerCrash: false,
			wantType:      protocol.FailureInputUnreachable,
		},
		{
			name:          "input unreachable - connection refused",
			exitCode:      1,
			stderr:        "Connection refused",
			errorMessage:  "ffmpeg exited with code 1",
			isTimeout:     false,
			isWorkerCrash: false,
			wantType:      protocol.FailureInputUnreachable,
		},
		{
			name:          "encoder unsupported",
			exitCode:      1,
			stderr:        "Unknown encoder 'h265_fake'",
			errorMessage:  "ffmpeg exited with code 1",
			isTimeout:     false,
			isWorkerCrash: false,
			wantType:      protocol.FailureEncoderUnsupported,
		},
		{
			name:          "ffmpeg error fallback",
			exitCode:      1,
			stderr:        "Error while decoding stream #0:0: Generic error",
			errorMessage:  "ffmpeg exited with code 1",
			isTimeout:     false,
			isWorkerCrash: false,
			wantType:      protocol.FailureFFmpegError,
		},
		{
			name:          "ffmpeg error - empty stderr",
			exitCode:      1,
			stderr:        "",
			errorMessage:  "",
			isTimeout:     false,
			isWorkerCrash: false,
			wantType:      protocol.FailureFFmpegError,
		},
		{
			name:          "priority: crash over timeout",
			exitCode:      -1,
			stderr:        "",
			errorMessage:  "",
			isTimeout:     true,
			isWorkerCrash: true,
			wantType:      protocol.FailureWorkerCrash,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _ := ClassifyFailure(tt.exitCode, tt.stderr, tt.errorMessage, tt.isTimeout, tt.isWorkerCrash)
			if got != tt.wantType {
				t.Errorf("ClassifyFailure() = %q, want %q", got, tt.wantType)
			}
		})
	}
}

func TestFailureType_Retryable(t *testing.T) {
	if protocol.FailureTimeout.Retryable() != true {
		t.Error("TIMEOUT should be retryable")
	}
	if protocol.FailureWorkerCrash.Retryable() != true {
		t.Error("WORKER_CRASH should be retryable")
	}
	if protocol.FailureInputUnreachable.Retryable() != false {
		t.Error("INPUT_UNREACHABLE should not be retryable")
	}
	if protocol.FailureEncoderUnsupported.Retryable() != false {
		t.Error("ENCODER_UNSUPPORTED should not be retryable")
	}
	if protocol.FailureDiskFull.Retryable() != false {
		t.Error("DISK_FULL should not be retryable")
	}
	if protocol.FailureFFmpegError.Retryable() != false {
		t.Error("FFMPEG_ERROR should not be retryable")
	}
}

func TestReportFailureAlwaysSetsClassification(t *testing.T) {
	// reportFailure classifies every failure — even an empty message must
	// yield a non-empty failure type (FFMPEG_ERROR fallback), never "".
	failureType, details := ClassifyFailure(1, "", "", false, false)
	if failureType != protocol.FailureFFmpegError {
		t.Errorf("empty stderr fallback = %q, want FFMPEG_ERROR", failureType)
	}
	if details == "" {
		t.Error("details should never be empty for a classified failure")
	}
}

// TestClassifyInputDownloadFailure locks the TSI-2348 fix: a failed download
// of a remote-URL input is the user's input being unreachable (INPUT_UNREACHABLE),
// while a failed server-file fetch is worker↔server infrastructure (FFMPEG_ERROR).
func TestClassifyInputDownloadFailure(t *testing.T) {
	remoteURLs := []string{
		"http://example.com/video.mp4",
		"https://cdn.example.com/a/b/c.mkv",
		"ftp://files.example.com/media.avi",
		"http://10.0.0.1:8080/stream",
	}
	for _, url := range remoteURLs {
		if got := ClassifyInputDownloadFailure(url); got != protocol.FailureInputUnreachable {
			t.Errorf("remote URL %q classified as %q, want INPUT_UNREACHABLE", url, got)
		}
	}

	serverFileIDs := []string{
		"abc123",
		"0f8b3c2e-1d4a-4e5f-9a6b-upload0042",
		"",
	}
	for _, id := range serverFileIDs {
		if got := ClassifyInputDownloadFailure(id); got != protocol.FailureFFmpegError {
			t.Errorf("server file ID %q classified as %q, want FFMPEG_ERROR", id, got)
		}
	}
}

// TestClassifyFailureInfraTextNotInputUnreachable locks the review fix:
// generic infrastructure error text (server communication, upload, job-dir
// plumbing) must NOT be pattern-matched into INPUT_UNREACHABLE — that
// category is reserved for ffmpeg reporting unreachable inputs.
func TestClassifyFailureInfraTextNotInputUnreachable(t *testing.T) {
	infraMessages := []string{
		"Failed to update job status to running: connection refused",
		"Failed to download input file abc: Post \"http://server/files/abc\": connection refused",
		"Failed to upload output: dial tcp 10.0.0.1:8080: connect: connection timed out",
	}
	for _, msg := range infraMessages {
		got, _ := ClassifyFailure(1, msg, msg, false, false)
		if got == protocol.FailureInputUnreachable {
			t.Errorf("infra message %q misclassified as INPUT_UNREACHABLE", msg)
		}
		if got != protocol.FailureFFmpegError {
			t.Errorf("infra message %q should fall back to FFMPEG_ERROR, got %q", msg, got)
		}
	}
}

func TestFailureTypeIsValid(t *testing.T) {
	valid := []protocol.FailureType{
		protocol.FailureInputUnreachable, protocol.FailureEncoderUnsupported,
		protocol.FailureDiskFull, protocol.FailureTimeout,
		protocol.FailureWorkerCrash, protocol.FailureFFmpegError,
		protocol.FailureNoWorkerAvailable,
	}
	for _, f := range valid {
		if !f.IsValid() {
			t.Errorf("%q should be valid", f)
		}
	}
	for _, f := range []protocol.FailureType{"", "BOGUS", "input_unreachable"} {
		if f.IsValid() {
			t.Errorf("%q should be invalid", f)
		}
	}
}
