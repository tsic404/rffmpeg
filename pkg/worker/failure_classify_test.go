package worker

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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
			name:          "output open failure - missing directory not INPUT_UNREACHABLE",
			exitCode:      1,
			stderr:        "[out#0/mp3 @ 0x...] Error opening output /nonexistent_dir/out.mp3: No such file or directory\nError opening output file /nonexistent_dir/out.mp3.\nError opening output files: No such file or directory\n",
			errorMessage:  "output file not found: /nonexistent_dir/out.mp3",
			isTimeout:     false,
			isWorkerCrash: false,
			wantType:      protocol.FailureFFmpegError,
		},
		{
			name:          "output open failure - muxer init invalid argument",
			exitCode:      1,
			stderr:        "[AVFormatContext @ 0x...] Unable to choose an output format for 'output.xyz'; use a standard extension for the filename or specify the format manually.\n[out#0 @ 0x...] Error initializing the muxer for output.xyz: Invalid argument\nError opening output file output.xyz.\n",
			errorMessage:  "ffmpeg reported critical error: corrupted or invalid input data",
			isTimeout:     false,
			isWorkerCrash: false,
			wantType:      protocol.FailureFFmpegError,
		},

		{
			name:     "corrupted local file is FFMPEG_ERROR, not INPUT_UNREACHABLE",
			exitCode: 1,
			stderr: "[mov,mp4,m4a,3gp,3g2,mj2 @ 0x55b5e8d8c700] Invalid data found when processing input\n" +
				"[mov,mp4,m4a,3gp,3g2,mj2 @ 0x55b5e8d8c700] moov atom not found\n" +
				"file.mp4: Invalid data found when processing input",
			errorMessage:  "ffmpeg exited with code 1",
			isTimeout:     false,
			isWorkerCrash: false,
			wantType:      protocol.FailureFFmpegError,
		},

		{
			name:     "ffmpeg interrupted (Immediate exit requested) is FFMPEG_ERROR, not INPUT_UNREACHABLE",
			exitCode: 255,
			stderr: "[mpegts @ 0x55b5e8d8c700] Packet corrupt near timestamp\n" +
				"stream.mpeg: Immediate exit requested",
			errorMessage:  "ffmpeg exited with code 255",
			isTimeout:     false,
			isWorkerCrash: false,
			wantType:      protocol.FailureFFmpegError,
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
	cases := []struct {
		failureType protocol.FailureType
		want        bool
	}{
		// Transient failures that a retry can overcome.
		{protocol.FailureTimeout, true},
		{protocol.FailureWorkerCrash, true},
		// Deterministic failures — retrying without a change cannot help.
		{protocol.FailureInputUnreachable, false},
		{protocol.FailureEncoderUnsupported, false},
		{protocol.FailureDiskFull, false},
		{protocol.FailureFFmpegError, false},
		{protocol.FailureNoWorkerAvailable, false},
		{protocol.FailureInfra, false},
	}
	for _, tc := range cases {
		if got := tc.failureType.Retryable(); got != tc.want {
			t.Errorf("%s.Retryable() = %v, want %v", tc.failureType, got, tc.want)
		}
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

// TestClassifyFailureOOMKill locks the TSI-2365 fix: OOM-killed processes
// (exit 137 / SIGKILL text) surface as WORKER_CRASH with an explicit reason,
// not a generic FFMPEG_ERROR.
func TestClassifyFailureOOMKill(t *testing.T) {
	cases := []struct {
		name     string
		exitCode int
		stderr   string
	}{
		{"exit code 137", 137, ""},
		{"oom stderr", 1, "ffmpeg: Cannot allocate memory"},
		{"killed signal", -1, "Killed signal 9 (SIGKILL) on job"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, details := ClassifyFailure(tc.exitCode, tc.stderr, "", false, false)
			if got != protocol.FailureWorkerCrash {
				t.Errorf("exit=%d stderr=%q classified as %q, want WORKER_CRASH",
					tc.exitCode, tc.stderr, got)
			}
			if details == "" {
				t.Error("details should explain the OOM kill")
			}
		})
	}
}

// TestClassifyFailureSignalDeath locks the TSI-2458 fix: ffmpeg killed by
// an OS signal (SIGABRT=134, SIGSEGV=139, SIGKILL=137) is a process crash,
// not a generic FFMPEG_ERROR, so the job is retryable. These sporadic
// self-aborts happen under high load / temp-space pressure and succeed on
// re-run.
func TestClassifyFailureSignalDeath(t *testing.T) {
	cases := []struct {
		name     string
		exitCode int
		stderr   string
	}{
		{"sigabrt exit 134", 134, "frame=  120 fps= 30 q=28.0\nAborted\n"},
		{"sigabrt exit 134 no stderr", 134, ""},
		{"sigsegv exit 139", 139, ""},
		{"sigbus exit 135", 135, ""},
		{"sigfpe exit 136", 136, ""},
		{"sigkill exit 137", 137, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, details := ClassifyFailure(tc.exitCode, tc.stderr, "", false, false)
			if got != protocol.FailureWorkerCrash {
				t.Errorf("exit=%d stderr=%q classified as %q, want WORKER_CRASH",
					tc.exitCode, tc.stderr, got)
			}
			if details == "" {
				t.Error("details should explain the signal death")
			}
		})
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
		// TSI-2365: server-channel failures are infrastructure, not ffmpeg errors.
		if got := ClassifyInputDownloadFailure(id); got != protocol.FailureInfra {
			t.Errorf("server file ID %q classified as %q, want INFRA", id, got)
		}
	}
}

// TestClassifyFailureInfraTextNotInputUnreachable locks the review fix:
// generic infrastructure error text (server communication, upload, job-dir
// plumbing) must NOT be pattern-matched into INPUT_UNREACHABLE — that
// category is reserved for ffmpeg reporting unreachable inputs.
func TestClassifyFailureInfraTextNotInputUnreachable(t *testing.T) {
	infraMessages := []string{
		"Failed to update job status to running: job directory missing",
		"Failed to upload output: storage backend rejected the file",
		"Failed to create job directory: read-only file system",
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
		protocol.FailureNoWorkerAvailable, protocol.FailureInfra,
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

// TestReportFailureStoresConciseError locks the TSI-2523 fix: reportFailure
// must store the concise classification summary as the job's terminal Error,
// not echo the full ffmpeg stderr — the complete log already reached the CLI
// once via the live stderr stream, so echoing it again duplicates it.
func TestReportFailureStoresConciseError(t *testing.T) {
	var got protocol.JobUpdateRequest
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	w := &Worker{client: NewClient(ts.URL, "test-worker", "")}
	fullStderr := "ffmpeg version n9.0.1 Copyright (c) 2000-2026\n" +
		"  Stream #0:0: Video: h264\n" +
		"frame= 42 fps=20 q=28.0 size=100KiB time=00:00:01.68 bitrate=487.0kbits/s\n" +
		"Conversion failed!\n"
	w.reportFailure("job-1", 1, fullStderr, false)

	if got.Error == fullStderr {
		t.Error("Error field must not echo full stderr (TSI-2523)")
	}
	if got.Error != "Conversion failed!" {
		t.Errorf("Error = %q, want concise summary", got.Error)
	}
	if got.FailureType != string(protocol.FailureFFmpegError) {
		t.Errorf("failure_type = %q, want %q", got.FailureType, protocol.FailureFFmpegError)
	}
}

// TestReportFailure_RetryExhaustedStoresConciseError locks the TSI-2523 fix on
// the retry-exhausted path: even when worker.go prefixes the report with the
// full "All retry attempts exhausted (…): <stderr>" banner, the terminal Error
// must stay the concise ffmpeg summary — the full log already streamed live on
// every attempt.
func TestReportFailure_RetryExhaustedStoresConciseError(t *testing.T) {
	var got protocol.JobUpdateRequest
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	w := &Worker{client: NewClient(ts.URL, "test-worker", "")}
	fullStderr := "ffmpeg version n9.0.1 Copyright (c) 2000-2026\n" +
		"  Stream #0:0: Video: h264\n" +
		"frame= 42 fps=20 q=28.0 size=100KiB time=00:00:01.68 bitrate=487.0kbits/s\n" +
		"Conversion failed!\n"
	errMsg := fmt.Sprintf("All retry attempts exhausted (original encoder: %s, final stage: %s): %s",
		"h264_nvenc", "exhausted", fullStderr)
	w.reportFailure("job-1", 1, errMsg, false)

	if got.Error == fullStderr {
		t.Error("Error field must not echo full stderr (TSI-2523)")
	}
	if got.Error == errMsg {
		t.Error("Error field must not echo the retry-exhausted banner (TSI-2523)")
	}
	if got.Error != "Conversion failed!" {
		t.Errorf("Error = %q, want concise summary", got.Error)
	}
	if got.FailureType != string(protocol.FailureFFmpegError) {
		t.Errorf("failure_type = %q, want %q", got.FailureType, protocol.FailureFFmpegError)
	}
}
