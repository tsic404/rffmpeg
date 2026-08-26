package worker

import (
	"strings"

	"github.com/google/uuid"
	"github.com/tsix404/rffmpeg/pkg/protocol"
)

// isRemoteURL reports whether s is a remote URL (e.g. http://, https://,
// ftp://) rather than a server file ID. This is the single source of truth
// for the "://" heuristic in the worker package — DownloadInput's remote
// branch and the download-failure classifier must never disagree.
func isRemoteURL(s string) bool {
	return strings.Contains(s, "://")
}

// maxInputBaseName caps the length of filenames derived from remote URLs so a
// hostile URL can't produce absurdly long path components.
const maxInputBaseName = 128

// allowedExtraChars lists additional safe punctuation for input basenames.
const allowedExtraChars = " ()[]"

// sanitizeInputBaseName converts the last segment of a remote URL into a
// filename that is safe to join under a job directory: restricted charset,
// bounded length, never empty, never a dot-prefixed traversal fragment.
func sanitizeInputBaseName(rawBase string) string {
	base := rawBase
	if base == "." || base == "/" || base == ".." {
		base = ""
	}
	var b strings.Builder
	for _, r := range base {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '.', r == '-', r == '_', strings.ContainsRune(allowedExtraChars, r):
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
		if b.Len() >= maxInputBaseName {
			break
		}
	}
	result := b.String()
	if result == "" || result == "." || result == ".." {
		return uuid.New().String()
	}
	return result
}

// ClassifyFailure categorizes an ffmpeg execution failure into one of the
// FailureType values. stderr must be ffmpeg output — generic error text is
// never matched against input/encoder patterns.
//
// Priority (highest first): WORKER_CRASH (incl. OOM-kill / SIGKILL) > TIMEOUT >
// DISK_FULL > INPUT_UNREACHABLE > ENCODER_UNSUPPORTED > FFMPEG_ERROR
func ClassifyFailure(exitCode int, stderr string, errorMessage string, isTimeout bool, isWorkerCrash bool) (protocol.FailureType, string) {
	// Check worker crash first — highest priority
	if isWorkerCrash {
		return protocol.FailureWorkerCrash, "Worker process terminated unexpectedly"
	}

	// OOM kill: the kernel SIGKILLs the process (-9, exit code 137) when the
	// cgroup/system runs out of memory. Surface it as WORKER_CRASH with an
	// explicit reason instead of a generic FFMPEG_ERROR (TSI-2365).
	if exitCode == 137 || isOOMKill(stderr) {
		return protocol.FailureWorkerCrash, "Process killed by the OS out-of-memory killer (SIGKILL)"
	}

	// Check timeout
	if isTimeout {
		return protocol.FailureTimeout, "Job execution timed out"
	}

	// Check disk full
	if isDiskFull(stderr) {
		return protocol.FailureDiskFull, "No space left on device"
	}

	// Check input unreachable — only against ffmpeg-style stderr. Generic
	// error text (infra failures) must not match these patterns, or a
	// worker↔server "connection refused" would be misread as an input problem.
	if isInputUnreachable(stderr) {
		return protocol.FailureInputUnreachable, "Input file or stream cannot be reached"
	}

	// Check encoder unsupported — use ffmpeg error interceptor types
	if isEncoderUnsupported(stderr) {
		return protocol.FailureEncoderUnsupported, "Requested encoder is not supported or not found"
	}

	// Fallback: FFmpeg error
	return protocol.FailureFFmpegError, extractFFmpegSummary(stderr)
}

// ClassifyInputDownloadFailure classifies a failed input-file download by
// input kind. A remote URL (see isRemoteURL) is fetched directly from the
// user-supplied source — if the worker cannot reach it, the job's input is
// unreachable → INPUT_UNREACHABLE. A server file ID is fetched over the
// worker↔server channel; failing there is infrastructure, not an input
// problem → INFRA (TSI-2365; previously misreported as FFMPEG_ERROR).
func ClassifyInputDownloadFailure(fileID string) protocol.FailureType {
	if isRemoteURL(fileID) {
		return protocol.FailureInputUnreachable
	}
	return protocol.FailureInfra
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

// isOOMKill checks stderr for out-of-memory kill indicators.
func isOOMKill(stderr string) bool {
	patterns := []string{
		"out of memory",
		"Out of memory",
		"OOM",
		"Cannot allocate memory",
		"Killed signal", // e.g. "Killed signal 9 (SIGKILL)"
	}
	for _, p := range patterns {
		if strings.Contains(stderr, p) {
			return true
		}
	}
	return false
}

// isInputUnreachable checks if the error indicates input file/stream cannot be reached.
// Only matches ffmpeg-style stderr output (e.g. "file.mp4: No such file or directory").
func isInputUnreachable(stderr string) bool {
	patterns := []string{
		"No such file",
		"No such device",
		"Connection refused",
		"Connection timed out",
		"Name or service not known",
		"No route to host",
		"Network is unreachable",
		// Go net package error strings (lowercase style) — remote inputs are
		// fetched by worker-side Go code, so its error text lands in stderr
		// too (TSI-2365).
		"connection refused",
		"i/o timeout",
		"no such host",
		"network is unreachable",
		"Protocol not found",
		"HTTP error 404",
		"HTTP error 403",
		"HTTP error 401",
		"Server returned",
		"end of file",
	}
	for _, p := range patterns {
		if strings.Contains(stderr, p) {
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
		"Encoder not recognized",
		"is not recognized",
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
// Truncation is rune-safe: cutting at a byte offset can split a multi-byte
// UTF-8 character and produce an invalid string on the wire (TSI-2365).
func extractFFmpegSummary(stderr string) string {
	const maxLen = 200
	truncate := func(s string) string {
		if len(s) <= maxLen {
			return s
		}
		runes := []rune(s)
		if len(runes) <= maxLen {
			return s // multi-byte: fewer runes than bytes
		}
		return string(runes[:maxLen]) + "..."
	}

	lines := strings.Split(strings.TrimSpace(stderr), "\n")
	// Return last non-empty line, or a truncated version
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line != "" && !strings.HasPrefix(line, "frame=") && !strings.HasPrefix(line, "size=") {
			return truncate(line)
		}
	}
	if stderr != "" {
		return truncate(stderr)
	}
	return "ffmpeg exited with non-zero status"
}
