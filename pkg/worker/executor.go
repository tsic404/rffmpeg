package worker

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

// StderrHandler is a callback function for processing stderr output
type StderrHandler func(chunk string)

// StdoutHandler is a callback function for processing raw stdout bytes (streaming mode)
type StdoutHandler func(chunk []byte)

// ExecResult holds the result of an ffmpeg command execution
type ExecResult struct {
	ExitCode  int
	Stdout    string
	Stderr    string
	Error     error
	IsTimeout bool
}

// Executor runs ffmpeg commands
type Executor struct {
	ffmpegPath string
	timeout    time.Duration
}

// NewExecutor creates a new ffmpeg executor
func NewExecutor(ffmpegPath string, timeout time.Duration) *Executor {
	if ffmpegPath == "" {
		ffmpegPath = "ffmpeg"
	}
	if timeout == 0 {
		timeout = 2 * time.Hour // Default 2-hour timeout
	}
	return &Executor{
		ffmpegPath: ffmpegPath,
		timeout:    timeout,
	}
}

// Execute runs an ffmpeg command with the given arguments
func (e *Executor) Execute(ctx context.Context, args []string) ExecResult {
	return e.ExecuteWithStderrHandler(ctx, args, nil)
}

// ExecuteWithStderrHandler runs an ffmpeg command with real-time stderr streaming
// The stderrHandler callback is called for each line of stderr output
func (e *Executor) ExecuteWithStderrHandler(ctx context.Context, args []string, stderrHandler StderrHandler) ExecResult {
	return e.ExecuteWithHandlers(ctx, args, nil, stderrHandler)
}

// ExecuteWithHandlers runs an ffmpeg command with real-time stdout and stderr streaming.
// The stdoutHandler callback is called for each chunk of stdout output (streaming mode).
// The stderrHandler callback is called for each line of stderr output.
func (e *Executor) ExecuteWithHandlers(ctx context.Context, args []string, stdoutHandler StdoutHandler, stderrHandler StderrHandler) ExecResult {
	// Calculate the actual timeout duration for accurate error messages
	// If the incoming context already has a deadline, use the shorter of the two
	actualTimeout := e.timeout
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining < e.timeout {
			actualTimeout = remaining
		}
	}

	// Create a context with timeout
	ctx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, e.ffmpegPath, args...)

	// Run ffmpeg in its own process group so ctx cancellation kills the whole
	// process tree (filters may spawn helper processes that would otherwise
	// survive as orphans holding the inherited pipe write ends).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		// ctx cancellation can fire before Start() sets cmd.Process (start vs
		// cancel race window); dereferencing nil would panic the worker.
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	// WaitDelay bounds how long cmd.Wait waits for I/O pipes to close after
	// the context is done. Without it, an orphaned grandchild inheriting the
	// stdout/stderr pipe write ends keeps the readers open forever and
	// ExecuteWithHandlers blocks permanently after cancellation.
	cmd.WaitDelay = 30 * time.Second

	// Get pipes for stdout and stderr
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return ExecResult{
			Error:    fmt.Errorf("failed to create stdout pipe: %w", err),
			ExitCode: -1,
		}
	}

	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return ExecResult{
			Error:    fmt.Errorf("failed to create stderr pipe: %w", err),
			ExitCode: -1,
		}
	}

	// Start the command
	if err := cmd.Start(); err != nil {
		return ExecResult{
			Error:    fmt.Errorf("failed to start ffmpeg: %w", err),
			ExitCode: -1,
		}
	}

	// Collect stdout and stderr with optional streaming
	var stdout, stderr bytes.Buffer
	var wg sync.WaitGroup

	// Read stdout with optional chunk streaming
	wg.Add(1)
	go func() {
		defer wg.Done()
		if stdoutHandler != nil {
			// Streaming mode: read in chunks and call handler
			buf := make([]byte, 32*1024) // 32KB chunks for streaming
			for {
				n, readErr := stdoutPipe.Read(buf)
				if n > 0 {
					chunk := buf[:n]
					stdout.Write(chunk)
					stdoutHandler(chunk)
				}
				if readErr != nil {
					break
				}
			}
		} else {
			// Buffered mode: collect all stdout
			io.Copy(&stdout, stdoutPipe)
		}
	}()

	// Read stderr with line-by-line streaming
	wg.Add(1)
	go func() {
		defer wg.Done()
		scanner := bufio.NewScanner(stderrPipe)
		for scanner.Scan() {
			line := scanner.Text()
			stderr.WriteString(line)
			stderr.WriteString("\n")

			// Call the stderr handler if provided
			if stderrHandler != nil {
				stderrHandler(line + "\n")
			}
		}
	}()

	// Wait for pipes to be fully read
	wg.Wait()

	// Wait for command to complete
	err = cmd.Wait()

	result := ExecResult{
		Stdout: stdout.String(),
		Stderr: stderr.String(),
	}

	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			result.Error = fmt.Errorf("ffmpeg command timed out after %v", actualTimeout)
			result.ExitCode = -1
			result.IsTimeout = true
		} else if exitErr, ok := err.(*exec.ExitError); ok {
			result.ExitCode = exitErr.ExitCode()
			result.Error = fmt.Errorf("ffmpeg exited with code %d", exitErr.ExitCode())
		} else {
			result.Error = fmt.Errorf("failed to execute ffmpeg: %w", err)
			result.ExitCode = -1
		}
	} else {
		result.ExitCode = 0
	}

	return result
}

// BuildArgs constructs ffmpeg arguments from job parameters and input/output paths
// It replaces <INPUT_FILE> placeholders in jobArgs with actual input file paths
// and prepends -y flag to enable output file overwriting (required for retry support)
func BuildArgs(jobArgs []string, inputPaths []string, outputPath string) []string {
	args := make([]string, 0, len(jobArgs)+5)

	// Prepend -y flag to enable overwriting output files
	// This is required for retry support: when a job fails midway and the worker
	// retries, the output file may already exist from the partial attempt.
	args = append(args, "-y")

	// Replace <INPUT_FILE> placeholders with actual input paths
	inputIdx := 0
	for _, arg := range jobArgs {
		if arg == "<INPUT_FILE>" {
			// Replace placeholder with actual input path
			if inputIdx < len(inputPaths) {
				args = append(args, inputPaths[inputIdx])
				inputIdx++
			} else {
				// No more input paths, keep placeholder (should not happen in practice)
				args = append(args, arg)
			}
		} else {
			args = append(args, arg)
		}
	}

	// Check if user args contain the "--" separator
	separatorIdx := findSeparatorIndex(args)

	if separatorIdx >= 0 {
		// User included "--" separator
		// If there's no output after "--", we need to add one
		// (user might have mistakenly included "--" thinking it's required)
		if separatorIdx == len(args)-1 {
			// "--" is the last argument, add output after it
			args = append(args, outputPath)
		}
		// Otherwise, user has already provided output after "--", don't add another
	} else {
		// No "--" separator, add output path as the last argument if not already specified
		if !hasOutputArg(args) {
			args = append(args, outputPath)
		}
	}

	return args
}

// findSeparatorIndex finds the index of "--" in args, or -1 if not found
func findSeparatorIndex(args []string) int {
	for i, arg := range args {
		if arg == "--" {
			return i
		}
	}
	return -1
}

// hasOutputArg checks if the args already contain an output file argument
// An output file is a positional argument (doesn't start with '-') that is not
// a value following a flag. We track when we expect a value after a flag.
// Note: This function does NOT handle "--" separator - use BuildArgs for that.
func hasOutputArg(args []string) bool {
	expectingValue := false
	for _, arg := range args {
		if expectingValue {
			// This arg is a value for the previous flag, not an output file
			expectingValue = false
			continue
		}
		// If an arg doesn't start with '-', it's likely an output file
		if !strings.HasPrefix(arg, "-") {
			return true
		}
		// Check if this flag expects a value (flags that take arguments)
		// Only set expectingValue for flags that actually take values
		expectingValue = isFlagWithValue(arg)
	}
	return false
}

// networkPrefixes contains URL scheme prefixes for ffmpeg network output protocols.
// When ffmpeg outputs to a network URL (RTMP, RTSP, UDP, etc.), no local output file
// is produced. File existence/size validation MUST be skipped for these outputs.
var networkPrefixes = []string{
	"rtmp://", "rtmps://", "rtmpe://", "rtmpt://", "rtmpts://", "rtmpte://",
	"rtsp://", "rtsps://",
	"udp://",
	"tcp://",
	"srt://",
	"rtp://",
	"rist://",
	"icecast://",
	"http://", "https://",
}

// isNetworkOutput checks if the output path or args indicate the output is a network URL.
// Network outputs (RTMP, RTSP, UDP, etc.) do not produce local files, so the
// output file existence/size validation in processJob and retry logic should be skipped.
// It checks both the output path string (which may contain the URL in shared-FS mode)
// and the args list (where the URL may appear after the -- separator in non-shared-FS mode).
func isNetworkOutput(outputPath string, args []string) bool {
	// Check output path for network URL prefixes
	for _, prefix := range networkPrefixes {
		if strings.Contains(outputPath, prefix) {
			return true
		}
	}
	// Check args for network URL output
	afterSeparator := false
	for _, arg := range args {
		if arg == "--" {
			afterSeparator = true
			continue
		}
		if afterSeparator {
			for _, prefix := range networkPrefixes {
				if strings.HasPrefix(arg, prefix) {
					return true
				}
			}
		}
	}
	// Also check all args for network URLs (catch URLs that appear without -- separator)
	for _, arg := range args {
		for _, prefix := range networkPrefixes {
			if strings.HasPrefix(arg, prefix) {
				return true
			}
		}
	}
	return false
}

// isFlagWithValue returns true if the given ffmpeg flag takes a value argument.
// Boolean flags (like -vn, -an, -y) don't take values and should not consume the next arg.
func isFlagWithValue(flag string) bool {
	// If flag contains '=', the value is already attached (e.g., -loglevel=verbose)
	// In this case, the flag doesn't consume the next argument
	if strings.Contains(flag, "=") {
		return false
	}

	// Strip any stream specifier suffix (e.g., -c:v -> -c, -b:a -> -b)
	baseFlag := flag
	if idx := strings.Index(flag, ":"); idx != -1 {
		baseFlag = flag[:idx]
	}

	// Boolean flags that don't take values
	booleanFlags := map[string]bool{
		"-vn": true, "-an": true, "-sn": true, "-dn": true, // disable streams
		"-y": true, "-n": true, // overwrite control
		"-version": true, "-buildconf": true, // info flags
		"-formats": true, "-devices": true, "-codecs": true,
		"-decoders": true, "-encoders": true, "-bsfs": true,
		"-protocols": true, "-filters": true, "-pix_fmts": true,
		"-layouts": true, "-sample_fmts": true, "-colors": true,
		"-h": true, "-?": true, "-help": true, // help flags
		"-benchmark": true, "-benchmark_all": true,
		"-copyts": true, "-start_at_zero": true,
		"-bitexact": true, "-re": true,
		"-stdin": true, "-vol": true,
		"-discard": true, "-disposition": true,
		"-shortest": true, "-stats": true, "-hide_banner": true,
		"-report": true, "-debug_ts": true, "-copyinkf": true,
	}

	if booleanFlags[baseFlag] {
		return false
	}

	// Most other flags take values
	return true
}
