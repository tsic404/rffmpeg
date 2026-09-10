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

	"github.com/tsic404/rffmpeg/pkg/ffmpegopts"
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
	// Pdeathsig ensures ffmpeg is also killed if the worker itself dies
	// (SIGKILL, crash): the kernel delivers the signal to the child the moment
	// the parent exits, preventing orphaned ffmpeg processes from outliving the
	// worker and holding GPU/encoder resources (TSI-2476).
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid:   true,
		Pdeathsig: syscall.SIGKILL,
	}
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
		scanner.Split(splitProgressLines)
		for scanner.Scan() {
			line := scanner.Text()
			if line == "" {
				continue
			}
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
			// Go's exec.ExitError.ExitCode() returns -1 when the process
			// was killed by a signal, not the POSIX 128+signal value.
			// Use ProcessState to detect signal death and compute the
			// real exit code (128+signal) so downstream classifiers
			// (ClassifyFailure, ErrorAnalyzer) can recognize SIGABRT,
			// SIGSEGV, etc. as process crashes and trigger retry
			// (TSI-2458). Without this, SIGABRT falls through as a
			// generic FFMPEG_ERROR (exit -1) and the job never retries.
			exitCode := exitErr.ExitCode()
			if ps := exitErr.ProcessState; ps != nil {
				if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
					exitCode = 128 + int(ws.Signal())
				}
			}
			result.ExitCode = exitCode
			result.Error = fmt.Errorf("ffmpeg exited with code %d", exitCode)
		} else {
			result.Error = fmt.Errorf("failed to execute ffmpeg: %w", err)
			result.ExitCode = -1
		}
	} else {
		result.ExitCode = 0
	}

	return result
}

// BuildArgs constructs ffmpeg arguments from job parameters and input/output paths.
// It replaces <INPUT_FILE> placeholders in jobArgs with actual input file paths.
// User-supplied overwrite semantics (-y / -n) pass through verbatim: the worker
// never injects an overwrite flag, so a pre-existing output file follows native
// ffmpeg behavior (reject with "Not overwriting - exiting" unless the caller
// explicitly opted into -y). Retries remain safe because the RetryExecutor
// removes any partial output left by a prior attempt before re-running.
//
// jobArgs is a trusted passthrough channel: unlike DirectPaths and
// OutputFilename, it is deliberately NOT subject to the
// RFFMPEG_SHARED_FS_ALLOWED_PREFIX allow-list. ffmpeg arguments can carry
// paths in many forms (multiple -i, -map_metadata, filter paths, a dozen+
// network protocols), so enumerating them would rewrite passthrough
// semantics. Direct (shared-FS) mode is therefore a trust mode: the caller
// that submits Args must itself be trusted (TSI-2674).
func BuildArgs(jobArgs []string, inputPaths []string, outputPath string) []string {
	args := make([]string, 0, len(jobArgs)+1)

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

// hasOutputArg checks if the args already contain an output file argument.
//
// ffmpeg grammar: inputs are introduced by -i/--input, outputs are bare
// positional tokens (or "-" for stdout). A bare token is only a *clear*
// output when it appears in the output section, i.e. after at least one
// "-i <input>" pair. Before the first input, bare tokens are inputs whose
// "-i" was omitted by the caller — treating them as outputs would suppress
// the server-appended output path and leave ffmpeg with no input (TSI-2722).
// Note: This function does NOT handle "--" separator - use BuildArgs for that.
func hasOutputArg(args []string) bool {
	expectingValue := false
	seenInput := false
	for _, arg := range args {
		if expectingValue {
			// This arg is a value for the previous flag, not an output file.
			expectingValue = false
			continue
		}
		if arg == "-i" || arg == "--input" {
			// The next token is the input path.
			seenInput = true
			expectingValue = true
			continue
		}
		if !seenInput {
			// Still in the input section. A bare token here is an input whose
			// "-i" was omitted by the caller (the parser also accepts the first
			// positional as an input); it marks the end of the input section but
			// is never an output. Flag values must still be consumed so a later
			// "-i <input>" is tracked correctly.
			if !strings.HasPrefix(arg, "-") {
				seenInput = true
				continue
			}
			expectingValue = isFlagWithValue(arg)
			continue
		}
		// Output section: "-" is stdout, any other bare token is an output file.
		if arg == "-" || !strings.HasPrefix(arg, "-") {
			return true
		}
		expectingValue = isFlagWithValue(arg)
	}
	return false
}

// splitProgressLines is a bufio.SplitFunc that normalizes ffmpeg's
// \r-separated -stats updates into \n-separated lines: the progress line
// ("frame= ... time= ... speed= ...") is rewritten in place on stderr using
// \r separators — one update roughly every 0.5s of encoded media — and the
// default bufio.ScanLines collapses the whole run into a single token that
// only becomes available when the next \n arrives (at job end), which starved
// the ProgressRouter and reduced server-side progress pushes to 1-2 updates
// per job (TSI-2425). Splitting on both keeps the streamed output identical
// while making every intermediate update visible to the parser in real time.
func splitProgressLines(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	for i, b := range data {
		switch b {
		case '\r':
			// Swallow a following \n so a "\r\n" pair yields one empty
			// separator instead of two.
			n := i + 1
			if n < len(data) && data[n] == '\n' {
				n++
			}
			return n, data[:i], nil
		case '\n':
			return i + 1, data[:i], nil
		}
	}
	if atEOF {
		return len(data), data, nil
	}
	return 0, nil, nil
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
	// Check output path for network URL prefixes. HasPrefix, not Contains: a
	// local path that merely contains "http://" (e.g. /data/http://x.mp4) is
	// not a network output and its file must still be validated (TSI-2365).
	for _, prefix := range networkPrefixes {
		if strings.HasPrefix(outputPath, prefix) {
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
// Boolean flags (like -vn, -an, -y) don't take values and should not consume the
// next arg. Arity comes from the generated table in pkg/ffmpegopts.
func isFlagWithValue(flag string) bool {
	// If flag contains '=', the value is already attached (e.g., -loglevel=verbose)
	// In this case, the flag doesn't consume the next argument
	if strings.Contains(flag, "=") {
		return false
	}
	return !ffmpegopts.IsBoolean(flag)
}
