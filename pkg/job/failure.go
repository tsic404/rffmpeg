package job

import (
	"context"
	"strings"

	"github.com/tsix404/rffmpeg/pkg/protocol"
	"github.com/tsix404/rffmpeg/pkg/worker"
)

// ClassifyFailure categorizes a job failure into one of 6 FailureType values.
// Priority (highest first): WORKER_CRASH > TIMEOUT > DISK_FULL > INPUT_UNREACHABLE > ENCODER_UNSUPPORTED > FFMPEG_ERROR
func ClassifyFailure(exitCode int, stderr string, errorMessage string, isTimeout bool, isWorkerCrash bool) (protocol.FailureType, string) {
	// Check worker crash first — highest priority
	if isWorkerCrash {
		return protocol.FailureWorkerCrash, "Worker process terminated unexpectedly"
	}

	// Check timeout
	if isTimeout {
		return protocol.FailureTimeout, "Job execution timed out"
	}

	// Check disk full
	if isDiskFull(stderr) {
		return protocol.FailureDiskFull, "No space left on device"
	}

	// Check input unreachable
	if isInputUnreachable(stderr, errorMessage) {
		return protocol.FailureInputUnreachable, "Input file or stream cannot be reached"
	}

	// Check encoder unsupported — use ffmpeg error interceptor types
	if isEncoderUnsupported(stderr) {
		return protocol.FailureEncoderUnsupported, "Requested encoder is not supported or not found"
	}

	// Fallback: FFmpeg error
	return protocol.FailureFFmpegError, extractFFmpegSummary(stderr)
}

// ClassifyFromInterceptedResult uses an intercepted result for classification.
func ClassifyFromInterceptedResult(ctx context.Context, result *worker.InterceptedResult, isWorkerCrash bool) (protocol.FailureType, string, bool) {
	if result == nil {
		return protocol.FailureWorkerCrash, "No execution result available", true
	}

	if result.IsSuccess {
		return "", "", false
	}

	errMsg := ""
	if result.Error != nil {
		errMsg = result.Error.Error()
	}

	failureType, detail := ClassifyFailure(
		result.ExitCode,
		result.Stderr,
		errMsg,
		result.IsTimeout,
		isWorkerCrash,
	)

	return failureType, detail, failureType.Retryable()
}

// isDiskFull checks stderr for disk-full patterns.
func isDiskFull(stderr string) bool {
	patterns := []string{
		"No space left on device",
		"ENOSPC",
		"Disk full",
		"disk full",
		"not enough space",
	}
	for _, p := range patterns {
		if strings.Contains(stderr, p) {
			return true
		}
	}
	return false
}

// isInputUnreachable checks if the error indicates input file/stream cannot be reached.
func isInputUnreachable(stderr string, errorMessage string) bool {
	patterns := []string{
		"No such file",
		"No such device",
		"Connection refused",
		"Connection timed out",
		"Name or service not known",
		"No route to host",
		"Network is unreachable",
		"Protocol not found",
		"Invalid data found when processing input",
		"HTTP error 404",
		"HTTP error 403",
		"HTTP error 401",
		"Server returned",
		"Immediate exit requested",
		"end of file",
	}
	combined := stderr + "\n" + errorMessage
	for _, p := range patterns {
		if strings.Contains(combined, p) {
			return true
		}
	}
	return false
}

// isEncoderUnsupported checks stderr for encoder-related error patterns using existing error interceptor types.
func isEncoderUnsupported(stderr string) bool {
	patterns := []string{
		"No such encoder",
		"Unknown encoder",
		"Encoder not found",
		"not found in encoder list",
		"Requested encoder",
		"Selected encoder not available",
		"Device not found",
		"Cannot open device",
		"device not found",
		"Unsupported codec",
		"Codec not supported",
	}
	for _, p := range patterns {
		if strings.Contains(stderr, p) {
			return true
		}
	}
	return false
}

// extractFFmpegSummary extracts a brief summary from stderr for FFMPEG_ERROR classification.
func extractFFmpegSummary(stderr string) string {
	lines := strings.Split(strings.TrimSpace(stderr), "\n")
	// Return last non-empty line, or a truncated version
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line != "" && !strings.HasPrefix(line, "frame=") && !strings.HasPrefix(line, "size=") {
			if len(line) > 200 {
				line = line[:200] + "..."
			}
			return line
		}
	}
	if stderr != "" {
		if len(stderr) > 200 {
			return stderr[:200] + "..."
		}
		return stderr
	}
	return "ffmpeg exited with non-zero status"
}
