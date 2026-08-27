package worker

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// ErrorAnalyzer analyzes ffmpeg stderr output to classify errors.
type ErrorAnalyzer struct {
	// patterns are the compiled error patterns.
	patterns []compiledPattern
}

// compiledPattern represents a compiled error pattern with regex.
type compiledPattern struct {
	errorType   FFmpegErrorType
	patterns    []*regexp.Regexp
	description string
}

// NewErrorAnalyzer creates a new error analyzer.
func NewErrorAnalyzer() *ErrorAnalyzer {
	return NewErrorAnalyzerWithPatterns(DefaultErrorPatterns())
}

// NewErrorAnalyzerWithPatterns creates a new error analyzer with custom patterns.
func NewErrorAnalyzerWithPatterns(patterns []ErrorPattern) *ErrorAnalyzer {
	analyzer := &ErrorAnalyzer{
		patterns: make([]compiledPattern, 0),
	}

	for _, p := range patterns {
		compiled := compiledPattern{
			errorType:   p.Type,
			description: p.Description,
			patterns:    make([]*regexp.Regexp, 0),
		}

		for _, pattern := range p.Patterns {
			// Compile as case-insensitive regex
			re, err := regexp.Compile("(?i)" + regexp.QuoteMeta(pattern))
			if err == nil {
				compiled.patterns = append(compiled.patterns, re)
			}
		}

		if len(compiled.patterns) > 0 {
			analyzer.patterns = append(analyzer.patterns, compiled)
		}
	}

	return analyzer
}

// AddPattern adds a new error pattern to the analyzer.
func (a *ErrorAnalyzer) AddPattern(pattern ErrorPattern) {
	compiled := compiledPattern{
		errorType:   pattern.Type,
		description: pattern.Description,
		patterns:    make([]*regexp.Regexp, 0),
	}

	for _, p := range pattern.Patterns {
		re, err := regexp.Compile("(?i)" + regexp.QuoteMeta(p))
		if err == nil {
			compiled.patterns = append(compiled.patterns, re)
		}
	}

	if len(compiled.patterns) > 0 {
		a.patterns = append(a.patterns, compiled)
	}
}

// Analyze analyzes stderr output and returns the parsed error.
func (a *ErrorAnalyzer) Analyze(stderr string, exitCode int) *FFmpegError {
	if stderr == "" && exitCode == 0 {
		return nil
	}

	// Detect process death by OS signal BEFORE pattern matching: a signal
	// kill (SIGABRT=134, SIGBUS=135, SIGSEGV=139, …) is a process crash
	// regardless of stderr text. ffmpeg n9.x sporadically self-aborts under
	// high load / temp-space pressure; classifying it as a generic unknown
	// error made it non-retryable, failing jobs that succeed on re-run
	// (TSI-2458).
	if isSignalDeath(exitCode) {
		return &FFmpegError{
			Type:      ErrorTypeProcessCrash,
			Message:   signalDeathMessage(exitCode, stderr),
			ExitCode:  exitCode,
			Stderr:    stderr,
			Timestamp: time.Now(),
		}
	}

	lines := strings.Split(stderr, "\n")

	// Find matching error types
	for _, compiled := range a.patterns {
		for _, re := range compiled.patterns {
			if re.MatchString(stderr) {
				// Found a match - extract the error details
				ffmpegErr := &FFmpegError{
					Type:            compiled.errorType,
					Message:         extractErrorMessage(stderr, re),
					ExitCode:        exitCode,
					Stderr:          stderr,
					MatchedPatterns: []string{re.String()},
					Timestamp:       time.Now(),
					ContextLines:    extractContextLines(lines, re),
				}
				return ffmpegErr
			}
		}
	}

	// No specific pattern matched - return as unknown error if exit code is non-zero
	if exitCode != 0 {
		return &FFmpegError{
			Type:         ErrorTypeUnknown,
			Message:      extractFirstErrorLine(stderr),
			ExitCode:     exitCode,
			Stderr:       stderr,
			Timestamp:    time.Now(),
			ContextLines: lines,
		}
	}

	return nil
}

// AnalyzeWithArgs analyzes stderr with command-line arguments for better context.
func (a *ErrorAnalyzer) AnalyzeWithArgs(stderr string, exitCode int, args []string) *FFmpegError {
	ffmpegErr := a.Analyze(stderr, exitCode)
	if ffmpegErr == nil {
		return nil
	}

	// Enhance error message with argument context
	ffmpegErr.Message = enhanceErrorMessage(ffmpegErr.Message, args, ffmpegErr.Type)

	return ffmpegErr
}

// extractErrorMessage extracts the most relevant error message from stderr.
func extractErrorMessage(stderr string, matchedPattern *regexp.Regexp) string {
	lines := strings.Split(stderr, "\n")

	// Find the first line matching the pattern
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if matchedPattern.MatchString(line) {
			return line
		}
	}

	// Fall back to first non-empty line
	return extractFirstErrorLine(stderr)
}

// extractFirstErrorLine returns the first non-empty line that looks like an error.
func extractFirstErrorLine(stderr string) string {
	lines := strings.Split(stderr, "\n")

	errorIndicators := []string{"error", "fail", "invalid", "cannot", "unable", "not found"}

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		lowerLine := strings.ToLower(line)
		for _, indicator := range errorIndicators {
			if strings.Contains(lowerLine, indicator) {
				return line
			}
		}
	}

	// Return first non-empty line
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" {
			return line
		}
	}

	return stderr
}

// extractContextLines extracts relevant context lines around the error.
func extractContextLines(lines []string, matchedPattern *regexp.Regexp) []string {
	contextLines := make([]string, 0)

	for i, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		if matchedPattern.MatchString(line) {
			// Add previous lines for context
			start := i - 2
			if start < 0 {
				start = 0
			}
			end := i + 3
			if end > len(lines) {
				end = len(lines)
			}

			for j := start; j < end; j++ {
				trimmedLine := strings.TrimSpace(lines[j])
				if trimmedLine != "" {
					contextLines = append(contextLines, trimmedLine)
				}
			}
			break
		}
	}

	// If no context found, return first few non-empty lines
	if len(contextLines) == 0 {
		for _, line := range lines {
			trimmedLine := strings.TrimSpace(line)
			if trimmedLine != "" {
				contextLines = append(contextLines, trimmedLine)
				if len(contextLines) >= 5 {
					break
				}
			}
		}
	}

	return contextLines
}

// enhanceErrorMessage enhances the error message with argument context.
func enhanceErrorMessage(message string, args []string, errorType FFmpegErrorType) string {
	switch errorType {
	case ErrorTypeInvalidArgument:
		// Try to identify which argument caused the error
		for _, arg := range args {
			if strings.Contains(strings.ToLower(message), strings.ToLower(arg)) {
				return message + " (related to argument: " + arg + ")"
			}
		}
	case ErrorTypeEncoderNotFound:
		// Try to identify which encoder was requested
		for i, arg := range args {
			if (arg == "-c:v" || arg == "-vcodec" || arg == "-codec:v") && i+1 < len(args) {
				return message + " (requested encoder: " + args[i+1] + ")"
			}
		}
	case ErrorTypeDeviceNotFound:
		// Try to identify device path
		for _, arg := range args {
			if strings.HasPrefix(arg, "/dev/") || strings.HasPrefix(arg, "hw:") {
				return message + " (device: " + arg + ")"
			}
		}
	}
	return message
}

// ClassifyErrorType is a convenience function that classifies error type from stderr.
func ClassifyErrorType(stderr string) FFmpegErrorType {
	analyzer := NewErrorAnalyzer()
	err := analyzer.Analyze(stderr, 1)
	if err != nil {
		return err.Type
	}
	return ErrorTypeUnknown
}

// signalExitCodes maps POSIX signal-death exit codes (128 + signal number)
// to the signal name. ffmpeg is killed by one of these when it self-aborts
// (SIGABRT from an internal assertion / glibc abort) or hits a memory
// fault (SIGSEGV/SIGBUS). They are process crashes, not normal ffmpeg
// errors, and are retryable (TSI-2458).
var signalExitCodes = map[int]string{
	134: "SIGABRT",
	135: "SIGBUS",
	136: "SIGFPE",
	137: "SIGKILL", // OOM kill — also handled by isOOMKill stderr path
	139: "SIGSEGV",
}

// isSignalDeath reports whether exitCode is a 128+signal POSIX signal death
// for a known fatal signal. Exit codes below 128 are ffmpeg's own; unknown
// codes ≥ 128 are left to the generic path to avoid false positives.
func isSignalDeath(exitCode int) bool {
	_, ok := signalExitCodes[exitCode]
	return ok
}

// signalDeathMessage builds a human-readable message for a signal-killed
// ffmpeg, preferring the first non-empty stderr line as context.
func signalDeathMessage(exitCode int, stderr string) string {
	sig := signalExitCodes[exitCode]
	msg := fmt.Sprintf("ffmpeg killed by OS signal %s (exit %d)", sig, exitCode)
	if first := extractFirstErrorLine(stderr); first != "" {
		msg += ": " + first
	}
	return msg
}
