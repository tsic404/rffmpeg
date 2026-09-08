package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/tsic404/rffmpeg/pkg/cli/args"
	"github.com/tsic404/rffmpeg/pkg/cli/client"
	"github.com/tsic404/rffmpeg/pkg/cli/config"
	"github.com/tsic404/rffmpeg/pkg/pathutil"
	"github.com/tsic404/rffmpeg/pkg/protocol"
)

const (
	ExitSuccess = 0
	ExitError   = 1
	// ExitDisconnected is returned when a job was submitted successfully but
	// the client lost contact with the server after its retry budget was
	// spent. The job keeps running server-side; query its final status via
	// GET /api/v1/jobs/{id}. Distinct from ExitError, which also covers
	// submission-phase failures where no job exists to query (TSI-2697).
	ExitDisconnected = 2

	// clientVerdictGrace is the margin past the server's NO_WORKER_AVAILABLE
	// verdict deadline (learned via JobInfo.NoWorkerDeadline) that the client
	// keeps polling so the verdict is observable before it gives up. It is NOT
	// the whole grace window: the deadline already includes
	// no_worker_job_timeout + timeout_check_interval, so a small margin
	// covering the client's 2s poll is enough.
	clientVerdictGrace = 5 * time.Second
)

var version = "1.0.0"

// Options holds the rffmpeg-specific options extracted from the command line.
type Options struct {
	ServerURL     string
	Token         string
	Quiet         bool
	ShowHelp      bool
	ShowVersion   bool
	AutoHW        bool
	IsProbe       bool
	ProbeInput    string
	Timeout       time.Duration
	MaxRetries    int
	MaxRetriesSet bool
	FmpegArgs     []string

	// Info flags
	ShowEncoders   bool
	ShowDecoders   bool
	ShowCodecs     bool
	ShowHwaccels   bool
	ShowFilters    bool
	ShowPixFmts    bool
	ShowFormats    bool
	ShowBuildconf  bool
	ShowLayouts    bool
	ShowProtocols  bool
	ShowSampleFmts bool
	ShowBsfs       bool
	ShowColors     bool
	ShowJSON       bool
}

// parseArgs extracts rffmpeg-specific options from command-line arguments.
// Everything after a bare "--" separator is passed through to ffmpeg verbatim.
// An rffmpeg flag that requires a value but finds none is a hard error (the
// previous silent fallback to defaults masked typos like "-server" with no URL).
func parseArgs(argList []string) (*Options, error) {
	opts := &Options{FmpegArgs: make([]string, 0)}
	positionalArgs := make([]string, 0)
	seen := make(map[string]bool)

	warnDuplicate := func(flag string) {
		if seen[flag] {
			fmt.Fprintf(os.Stderr, "Warning: %s specified multiple times; last occurrence wins\n", flag)
		}
		seen[flag] = true
	}

	needValue := func(i int, flag string) (string, error) {
		if i+1 >= len(argList) {
			return "", fmt.Errorf("flag %s requires a value", flag)
		}
		return argList[i+1], nil
	}

	for i := 0; i < len(argList); i++ {
		arg := argList[i]

		switch arg {
		case "--":
			// Pass everything after "--" to ffmpeg verbatim.
			opts.FmpegArgs = append(opts.FmpegArgs, positionalArgs...)
			positionalArgs = positionalArgs[:0]
			opts.FmpegArgs = append(opts.FmpegArgs, argList[i+1:]...)
			i = len(argList)

		case "--help", "-help":
			opts.ShowHelp = true
		case "--version", "-version":
			opts.ShowVersion = true
		case "--json":
			opts.ShowJSON = true
		case "--server", "-server":
			val, err := needValue(i, arg)
			if err != nil {
				return nil, err
			}
			warnDuplicate(arg)
			opts.ServerURL = val
			i++
		case "--token", "-token":
			val, err := needValue(i, arg)
			if err != nil {
				return nil, err
			}
			warnDuplicate(arg)
			opts.Token = val
			i++
		case "-q", "--quiet", "-quiet":
			opts.Quiet = true
		case "--auto-hw", "-auto-hw", "--auto-hw=true", "-auto-hw=true":
			opts.AutoHW = true
		case "--auto-hw=false", "-auto-hw=false":
			opts.AutoHW = false
		case "--timeout", "-timeout":
			val, err := needValue(i, arg)
			if err != nil {
				return nil, err
			}
			parsed, perr := time.ParseDuration(val)
			if perr != nil {
				return nil, fmt.Errorf("invalid timeout value: %s (use format like 30s, 5m, 2h)", val)
			}
			if parsed <= 0 {
				return nil, fmt.Errorf("timeout must be positive: %s", val)
			}
			warnDuplicate(arg)
			opts.Timeout = parsed
			i++
		case "--max-retries", "-max-retries":
			val, err := needValue(i, arg)
			if err != nil {
				return nil, err
			}
			n, perr := strconv.Atoi(val)
			if perr != nil || n < 0 {
				return nil, fmt.Errorf("invalid max-retries value: %s (must be a non-negative integer, 0 = no retries)", val)
			}
			warnDuplicate(arg)
			opts.MaxRetries = n
			opts.MaxRetriesSet = true
			i++

		case "-encoders", "--encoders":
			opts.ShowEncoders = true
		case "-decoders", "--decoders":
			opts.ShowDecoders = true
		case "-codecs", "--codecs":
			opts.ShowCodecs = true
		case "-hwaccels", "--hwaccels":
			opts.ShowHwaccels = true
		case "-filters", "--filters":
			opts.ShowFilters = true
		case "-pix_fmts", "--pix_fmts":
			opts.ShowPixFmts = true
		case "-formats", "--formats":
			opts.ShowFormats = true
		case "-buildconf", "--buildconf":
			opts.ShowBuildconf = true
		case "-layouts", "--layouts":
			opts.ShowLayouts = true
		case "-protocols", "--protocols":
			opts.ShowProtocols = true
		case "-sample_fmts", "--sample_fmts":
			opts.ShowSampleFmts = true
		case "-bsfs", "--bsfs":
			opts.ShowBsfs = true
		case "-colors", "--colors":
			opts.ShowColors = true
		case "-h":
			// -h is ambiguous: could be rffmpeg help or ffmpeg help
			// If there are other args, treat as ffmpeg arg
			// If alone, treat as rffmpeg help
			if len(argList) == 1 || (len(argList) == 2 && argList[len(argList)-1] == "-h") {
				opts.ShowHelp = true
			} else {
				positionalArgs = append(positionalArgs, arg)
			}
		default:
			// Non-rffmpeg option — collect as positional (could be ffmpeg option, value, or subcommand)
			positionalArgs = append(positionalArgs, arg)
		}
	}

	// Check if first positional argument is "probe" subcommand.
	// The probe subcommand can appear after rffmpeg options (e.g. -q probe file.mp4).
	if len(positionalArgs) > 0 && positionalArgs[0] == "probe" {
		opts.IsProbe = true
		positionalArgs = positionalArgs[1:]

		// The probe subcommand accepts ffprobe-compatible flags. The server's
		// probe endpoint always returns JSON containing both format and stream
		// info, so -show_format, -show_streams and -of/-print_format json are
		// accepted and already satisfied; any other output format is rejected
		// up front because the server cannot honor it.
		for i := 0; i < len(positionalArgs); i++ {
			arg := positionalArgs[i]
			switch arg {
			case "-i":
				if opts.ProbeInput != "" {
					return nil, fmt.Errorf("duplicate -i flag: input already set to %q", opts.ProbeInput)
				}
				if i+1 >= len(positionalArgs) {
					return nil, fmt.Errorf("flag -i requires a value")
				}
				val := positionalArgs[i+1]
				if strings.HasPrefix(val, "-") {
					return nil, fmt.Errorf("flag -i requires a value")
				}
				opts.ProbeInput = val
				i++
			case "-show_format", "-show_streams":
				// Already included in the probe response; nothing to forward.
			case "-of", "-print_format":
				if i+1 >= len(positionalArgs) {
					return nil, fmt.Errorf("flag %s requires a value", arg)
				}
				if val := positionalArgs[i+1]; val != "json" {
					return nil, fmt.Errorf("probe only supports json output format, got %q for %s", val, arg)
				}
				i++
			default:
				if strings.HasPrefix(arg, "-") {
					return nil, fmt.Errorf("unknown probe option: %s", arg)
				}
				if opts.ProbeInput == "" {
					opts.ProbeInput = arg
				} else {
					return nil, fmt.Errorf("unexpected probe argument: %s (input already set to %q)", arg, opts.ProbeInput)
				}
			}
		}
	} else {
		// Not a probe subcommand — all positional args are ffmpeg args
		opts.FmpegArgs = append(opts.FmpegArgs, positionalArgs...)
	}

	return opts, nil
}

func main() {
	os.Exit(run())
}

func run() int {
	// Parse command-line arguments manually
	opts, err := parseArgs(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		printUsage()
		return ExitError
	}

	if opts.ShowHelp {
		printUsage()
		return ExitSuccess
	}

	if opts.ShowVersion {
		fmt.Printf("rffmpeg %s\n", version)
		return ExitSuccess
	}

	// Handle -encoders / -decoders flags (no -i input file required)
	if opts.ShowEncoders {
		return runEncoders(opts.ServerURL, opts.Token, opts.ShowJSON)
	}
	if opts.ShowDecoders {
		return runDecoders(opts.ServerURL, opts.Token, opts.ShowJSON)
	}
	if opts.ShowCodecs {
		return runCodecsFromServer(opts.ServerURL, opts.Token, opts.ShowJSON)
	}
	if opts.ShowHwaccels {
		return runHwaccels(opts.ServerURL, opts.Token, opts.ShowJSON)
	}

	// P1 info flags: filters, pix_fmts, formats (call Server API)
	if opts.ShowFilters {
		return runFilters(opts.ServerURL, opts.Token, opts.ShowJSON)
	}
	if opts.ShowPixFmts {
		return runPixFmts(opts.ServerURL, opts.Token, opts.ShowJSON)
	}
	if opts.ShowFormats {
		return runFormats(opts.ServerURL, opts.Token, opts.ShowJSON)
	}

	// P2 info flags: buildconf, layouts, protocols, sample_fmts, bsfs, colors
	// Run local ffmpeg for these since no server API endpoint exists
	if opts.ShowBuildconf {
		return runLocalFfmpegInfo("-buildconf")
	}
	if opts.ShowLayouts {
		return runLocalFfmpegInfo("-layouts")
	}
	if opts.ShowProtocols {
		return runLocalFfmpegInfo("-protocols")
	}
	if opts.ShowSampleFmts {
		return runLocalFfmpegInfo("-sample_fmts")
	}
	if opts.ShowBsfs {
		return runLocalFfmpegInfo("-bsfs")
	}
	if opts.ShowColors {
		return runLocalFfmpegInfo("-colors")
	}

	ffmpegArgs := opts.FmpegArgs

	// If no ffmpeg args and not in probe mode, print usage and exit non-zero.
	// ffmpeg exits 1 when invoked with no arguments, and rffmpeg must match
	// so callers can detect a missing command (TSI-2907).
	if !opts.IsProbe && len(ffmpegArgs) == 0 {
		printUsage()
		return ExitError
	}

	// Load configuration
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error loading config: %v\n", err)
		return ExitError
	}

	// Override config with command-line flags
	if opts.ServerURL != "" {
		cfg.ServerURL = opts.ServerURL
	}
	if opts.Token != "" {
		cfg.Token = opts.Token
	}
	maxRetries := client.DefaultMaxRetries
	if opts.MaxRetriesSet {
		maxRetries = opts.MaxRetries
	} else if cfg.MaxRetries != nil {
		maxRetries = *cfg.MaxRetries
	}

	// Detect shared filesystem mode
	sharedFS := cfg.IsSharedFS()
	if sharedFS && !opts.Quiet {
		fmt.Fprintln(os.Stderr, "Shared filesystem mode enabled: skipping upload/download")
	}

	// Create client
	cli := client.New(cfg.ServerURL, cfg.Token, client.WithMaxRetries(maxRetries))

	// Check server health
	if err := cli.HealthCheck(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: server health check failed: %v\n", err)
		return ExitError
	}

	// --- Probe subcommand ---
	if opts.IsProbe {
		return runProbe(cli, opts.ProbeInput, opts.Quiet, sharedFS)
	}

	return runTranscode(cli, cfg, opts, ffmpegArgs, sharedFS)
}

// runTranscode submits the transcoding job described by opts/ffmpegArgs and
// waits for it, streaming logs and downloading outputs.
func runTranscode(cli *client.Client, cfg *config.Config, opts *Options, ffmpegArgs []string, sharedFS bool) int {
	quiet := opts.Quiet
	autoHW := opts.AutoHW
	timeout := opts.Timeout

	// Parse ffmpeg arguments
	parser := args.NewParser()
	result, err := parser.Parse(ffmpegArgs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error parsing arguments: %v\n", err)
		return ExitError
	}

	// Streaming (stdout) output cannot be seeked by the server-side ffmpeg,
	// and the worker only auto-fixes mp4/mov via fragmented movflags
	// (TSI-2409). Fail fast here with guidance for any muxer that cannot
	// write to a pipe, instead of surfacing ffmpeg's opaque
	// "Error initializing the muxer for pipe:: Invalid argument" (TSI-2696).
	if msg := args.StreamingOutputError(result); msg != "" {
		fmt.Fprintln(os.Stderr, msg)
		return ExitError
	}

	if !quiet {
		fmt.Fprintf(os.Stderr, "rffmpeg %s - Remote FFmpeg Client\n", version)
		fmt.Fprintf(os.Stderr, "Server: %s\n", cfg.ServerURL)
		fmt.Fprintf(os.Stderr, "Input files: %v\n", result.InputFiles)
		fmt.Fprintf(os.Stderr, "Output file: %s\n", result.OutputFile)
		if sharedFS {
			fmt.Fprintln(os.Stderr, "Mode: shared filesystem (passthrough)")
		}
	}

	// Setup signal handling for graceful cancellation.
	// jobID/cancelled are guarded by sigMu: the goroutine reads them while
	// the main flow writes, so unsynchronized access would be a data race.
	// First interrupt cancels the running job; a second one exits hard.
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	var (
		sigMu     sync.Mutex
		jobID     string
		cancelled bool
	)

	go func() {
		for range sigChan {
			sigMu.Lock()
			id := jobID
			done := cancelled
			if id != "" && !done {
				cancelled = true
				sigMu.Unlock()
				fmt.Fprintln(os.Stderr, "\nReceived interrupt, cancelling job...")
				if err := cli.CancelJob(id); err != nil {
					fmt.Fprintf(os.Stderr, "Failed to cancel job: %v\n", err)
					os.Exit(ExitError)
				}
				fmt.Fprintln(os.Stderr, "Job cancellation requested. Press Ctrl+C again to force quit.")
				continue
			}
			sigMu.Unlock()
			os.Exit(ExitError)
		}
	}()

	// Upload input files (or skip in shared FS mode)
	fileIDs := make([]string, 0, len(result.InputFiles))
	directPaths := make([]string, 0, len(result.InputFiles))
	for _, inputFile := range result.InputFiles {
		// Handle file:// URIs — strip scheme and treat as local file path
		if strings.HasPrefix(inputFile, "file://") {
			filePath, _ := args.StripFileScheme(inputFile)
			absPath, err := args.GetAbsPath(filePath)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error resolving input path %s: %v\n", inputFile, err)
				return ExitError
			}
			// Validate that the file exists
			if err := args.ValidateFileExists(absPath); err != nil {
				fmt.Fprintf(os.Stderr, "Error validating input %s: %v\n", inputFile, err)
				return ExitError
			}
			if !quiet {
				fmt.Fprintf(os.Stderr, "Using local file: %s\n", absPath)
			}
			if sharedFS {
				fileIDs = append(fileIDs, absPath)
				directPaths = append(directPaths, absPath)
			} else {
				if !quiet {
					fmt.Fprintf(os.Stderr, "Uploading %s...\n", absPath)
				}
				fileID, err := cli.UploadFileAuto(absPath)
				if err != nil {
					fmt.Fprintf(os.Stderr, "Error uploading %s: %v\n", absPath, err)
					return ExitError
				}
				fileIDs = append(fileIDs, fileID)
				if !quiet {
					fmt.Fprintf(os.Stderr, "Uploaded: %s -> %s\n", absPath, fileID)
				}
			}
			continue
		}

		// Detect remote inputs (contain "://") — skip local path resolution
		if strings.Contains(inputFile, "://") {
			// Remote input: pass through as-is, server/worker will handle via InputSource
			if !quiet {
				fmt.Fprintf(os.Stderr, "Using remote input: %s\n", inputFile)
			}
			fileIDs = append(fileIDs, inputFile)
			if sharedFS {
				directPaths = append(directPaths, inputFile)
			}
			continue
		}

		absPath, err := args.GetAbsPath(inputFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error resolving input path %s: %v\n", inputFile, err)
			return ExitError
		}

		// Validate that the file exists
		if err := args.ValidateFileExists(absPath); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			return ExitError
		}

		if sharedFS {
			// Shared filesystem mode: skip upload, use original path directly
			if !quiet {
				fmt.Fprintf(os.Stderr, "Shared FS: using direct path %s\n", absPath)
			}
			fileIDs = append(fileIDs, absPath)
			directPaths = append(directPaths, absPath)
		} else {
			if !quiet {
				fmt.Fprintf(os.Stderr, "Uploading %s...\n", inputFile)
			}

			fileID, err := cli.UploadFileAuto(absPath)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error uploading %s: %v\n", inputFile, err)
				return ExitError
			}

			fileIDs = append(fileIDs, fileID)
			if !quiet {
				fmt.Fprintf(os.Stderr, "Uploaded: %s -> %s\n", inputFile, fileID)
			}
		}
	}

	// Submit job
	if !quiet {
		fmt.Fprintln(os.Stderr, "Submitting job...")
	}
	// Extract output filename for the server.
	// In shared FS mode, send the full absolute path so the Worker writes
	// directly to the CLI-specified path instead of its own temp directory.
	// Network URLs (rtmp://, srt://, https://, udp://, etc.) pass through
	// as-is in both modes — resolving them as local paths would corrupt the
	// URL (TSI-2690).
	outputFilename := ""
	if result.OutputFile != "" {
		outputFilename, err = resolveOutputFilename(result.OutputFile, sharedFS)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error resolving output path %s: %v\n", result.OutputFile, err)
			return ExitError
		}
	}

	// For streaming output, we don't send a filename
	if result.StreamingOutput {
		outputFilename = ""
	}

	// In shared FS mode, DirectPath carries the original paths; otherwise it's nil (omitempty)
	var directPathParam []string
	if sharedFS {
		directPathParam = directPaths
	}

	sigMu.Lock()
	jobID, err = cli.SubmitJobWithOptions(fileIDs, directPathParam, result.AllArgs, outputFilename, autoHW, result.StreamingOutput, timeout)
	sigMu.Unlock()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error submitting job: %v\n", err)
		return ExitError
	}

	if !quiet {
		fmt.Fprintf(os.Stderr, "Job submitted: %s\n", jobID)
		fmt.Fprintln(os.Stderr, "Waiting for completion...")
	}

	// The wait is a loop because the server's NO_WORKER_AVAILABLE deadline is
	// only attached to pending/unassigned jobs: a job that was queued at
	// submit time carries no deadline, but if its worker dies and the job
	// reverts to pending, the next GetJob reports one. Re-reading on each
	// iteration lets the client extend its wait to the freshly attached
	// verdict instead of giving up before the server can emit it (TSI-2571).
	job, code := waitForJobLoop(cli, jobID, timeout, result.StreamingOutput, quiet)
	if code != ExitSuccess {
		return code
	}

	// Handle job result: print the outcome for any non-completed terminal
	// status and return the process exit code. reportTerminalJob
	// special-cases NO_WORKER_AVAILABLE so a server-side starvation verdict
	// is observationally distinct from a client-side cancellation; it
	// returns ExitSuccess only for a completed job, which then falls through
	// to the output download below.
	if code := reportTerminalJob(job); code != ExitSuccess {
		return code
	}

	// Download output files (skip in shared FS mode — files are already at local path)
	if sharedFS {
		if !quiet {
			fmt.Fprintln(os.Stderr, "Shared FS mode: output files already at local paths, skipping download")
		}
	} else if result.StreamingOutput {
		// Streaming output: data already written to stdout via WebSocket
		// No file download needed
	} else if len(job.OutputFiles) > 0 {
		for i, outputFileID := range job.OutputFiles {
			outputPath := result.OutputFile
			if i > 0 {
				// Multiple outputs - append index
				ext := ""
				for j := len(outputPath) - 1; j >= 0; j-- {
					if outputPath[j] == '.' {
						ext = outputPath[j:]
						break
					}
				}
				outputPath = outputPath[:len(outputPath)-len(ext)] + fmt.Sprintf("_%d", i) + ext
			}

			if !quiet {
				fmt.Fprintf(os.Stderr, "Downloading output to %s...\n", outputPath)
			}

			if err := cli.DownloadOutput(outputFileID, outputPath); err != nil {
				fmt.Fprintf(os.Stderr, "Error downloading output: %v\n", err)
				return ExitError
			}

			if !quiet {
				fmt.Fprintf(os.Stderr, "Output saved: %s\n", outputPath)
			}
		}
	} else {
		fmt.Fprintln(os.Stderr, "Warning: no output files returned from server")
	}

	if !quiet {
		fmt.Fprintln(os.Stderr, "Done!")
	}

	// Return success exit code
	return ExitSuccess
}

// resolveOutputFilename determines the output filename sent to the server for
// the given ffmpeg output argument.
//
// Network URLs (rtmp://, srt://, https://, udp://, etc.) pass through
// unchanged in both modes, because treating them as local paths would corrupt
// the URL (TSI-2690). Local paths — including file:// and paths that merely
// contain "://" mid-string — are not remote: in shared FS mode they are
// resolved to absolute paths so the Worker writes directly to the CLI-specified
// path, otherwise they are reduced to their base name since the Worker writes
// into its own job directory and the CLI downloads the result afterward.
func resolveOutputFilename(outputFile string, sharedFS bool) (string, error) {
	if pathutil.IsRemoteURL(outputFile) {
		return outputFile, nil
	}
	if sharedFS {
		return args.GetAbsPath(outputFile)
	}
	return filepath.Base(outputFile), nil
}

// jobWaitClient is the subset of *client.Client the wait loop needs. It is an
// interface so the loop can be driven in tests by a fake that returns a
// scripted status sequence without a real server.
type jobWaitClient interface {
	GetJob(jobID string) (*protocol.JobInfo, error)
	WaitForJobWithLogs(ctx context.Context, jobID string, quiet bool) (*protocol.JobInfo, error)
	WaitForJobWithStreamingOutput(ctx context.Context, jobID string, quiet bool) (*protocol.JobInfo, error)
	CancelJob(jobID string) error
}

// waitForJobLoop waits for the job to reach a terminal status and returns it,
// or cancels the job and returns ExitError when the client gives up. It loops
// because the server's NO_WORKER_AVAILABLE verdict deadline is only attached
// to pending/unassigned jobs: a job queued at submit time carries none, but if
// its worker dies and the job reverts to pending, the next GetJob reports one.
// Re-reading the deadline on each iteration lets the client extend its wait to
// the freshly attached verdict instead of giving up before the server can emit
// it (TSI-2571). The deadline is authoritative server config, not a local
// guess; a failed GetJob lookup only means the client falls back to --timeout.
func waitForJobLoop(cli jobWaitClient, jobID string, timeout time.Duration, streamingOutput, quiet bool) (*protocol.JobInfo, int) {
	var job *protocol.JobInfo
	for {
		var noWorkerDeadline *time.Time
		var startedAt *time.Time
		var status protocol.JobStatus
		if submitted, getErr := cli.GetJob(jobID); getErr == nil {
			noWorkerDeadline = submitted.NoWorkerDeadline
			startedAt = submitted.StartedAt
			status = submitted.Status
		}
		waitDeadline, hasDeadline := clientWaitDeadline(time.Now(), timeout, status, startedAt, noWorkerDeadline)
		waitCtx := context.Background()
		var cancelWait context.CancelFunc
		if hasDeadline {
			waitCtx, cancelWait = context.WithDeadline(context.Background(), waitDeadline)
		}

		var waitErr error
		if streamingOutput {
			job, waitErr = cli.WaitForJobWithStreamingOutput(waitCtx, jobID, quiet)
		} else {
			job, waitErr = cli.WaitForJobWithLogs(waitCtx, jobID, quiet)
		}
		if cancelWait != nil {
			cancelWait()
		}
		if waitErr == nil {
			break
		}
		var retriesExhausted *client.RetriesExhaustedError
		if errors.As(waitErr, &retriesExhausted) {
			fmt.Fprintf(os.Stderr, "Error: %v\n", retriesExhausted)
			return nil, ExitDisconnected
		}
		if !errors.Is(waitErr, context.DeadlineExceeded) {
			fmt.Fprintf(os.Stderr, "Error waiting for job: %v\n", waitErr)
			return nil, ExitError
		}

		// Race guard (TSI-2452): the client-side wait deadline may have
		// fired even though the job already reached a terminal status on
		// the server (e.g. a cache hit completed between the last poll
		// and the context deadline). WaitForJobWithLogs /
		// WaitForJobWithStreamingOutput already do a final GetJob on
		// ctx.Done(); this is a belt-and-suspenders fallback in case a
		// future wait variant or a WS-reconnect edge case lets the
		// deadline through without the check. If the job is already
		// done, proceed with the result instead of cancelling a
		// completed job (which the server rejects with 400). The server
		// verdict (notably NO_WORKER_AVAILABLE) may have landed exactly
		// at the deadline; report it as the server's judgement, not a
		// client-side cancellation.
		finalJob, finalErr := cli.GetJob(jobID)
		if finalErr == nil && protocol.IsTerminalStatus(finalJob.Status) {
			job = finalJob
			break
		}

		// The job is not done. If the server now reports a verdict deadline
		// later than the bound just exhausted (the job reverted to pending
		// after its worker died), extend the wait instead of cancelling
		// early. Compare the raw server verdict against the exhausted bound
		// — not a recomputed now+timeout bound, which always advances with
		// now and would loop forever. The deadline is fixed once attached,
		// so this fires at most once.
		if finalErr == nil && extendWaitForNoWorkerDeadline(waitDeadline, finalJob.NoWorkerDeadline) {
			continue
		}
		if finalErr == nil && extendWaitForStartedAt(waitDeadline, finalJob.StartedAt, timeout) {
			continue
		}
		if finalErr == nil && extendWaitForQueued(finalJob.Status) {
			continue
		}

		// The job is genuinely not done. Cancel it server-side so it
		// doesn't linger, and report a clear client-side timeout — distinct
		// from a server verdict (NO_WORKER_AVAILABLE, TIMEOUT, ...) so
		// operators can tell who gave up. The message distinguishes a job
		// still waiting for a worker from one that had actually started but
		// stalled. No server verdict can still be pending here: the extend
		// guard above already continued whenever the server attached a
		// NoWorkerDeadline, so reaching this branch means either the lookup
		// failed or no deadline exists (sweep disabled or status not pending).
		if cancelErr := cli.CancelJob(jobID); cancelErr != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to cancel timed-out job %s: %v\n", jobID, cancelErr)
		}
		if finalErr == nil && (finalJob.Status == protocol.JobStatusPending || finalJob.Status == protocol.JobStatusQueued) {
			fmt.Fprintf(os.Stderr, "Error: job %s did not start within %s and was cancelled by the client (still waiting for a worker)\n", jobID, timeout)
		} else {
			fmt.Fprintf(os.Stderr, "Error: job %s did not complete within %s and was cancelled by the client (client gave up; the job was still running)\n", jobID, timeout)
		}
		return nil, ExitError
	}
	return job, ExitSuccess
}

// clientWaitDeadline computes the client-side give-up time. timeout is the
// per-job ffmpeg execution budget; status is the job's current status;
// startedAt is the server's started_at (the budget's anchor); noWorkerDeadline
// is the server's NO_WORKER_AVAILABLE verdict time for a pending job. The
// client waits until the later bound (plus clientVerdictGrace) so it never
// cancels before the server can emit its verdict. Returns false when neither
// bound exists.
//
// --timeout is a worker-side execution budget, not a wall-clock bound
// (TSI-2886). It bounds the client's wait only for pending jobs (anchored to
// now — the "did not start within --timeout" give-up) and running jobs
// (anchored to started_at so pre-exec latency is not charged and the worker's
// TIMEOUT verdict stays observable).
//
// A queued job is already claimed by a worker and spends its pre-exec phase
// (download, duration probe) in the queued state; that phase is bounded by the
// worker's own timeouts (30m dataClient download + ffprobe executor), not by
// --timeout, so the client must not cancel it before ffmpeg starts.
//
// The old code computed timeout+clientVerdictGrace as a duration sum, which
// overflowed negative for a --timeout near math.MaxInt64 and made
// context.WithTimeout expire immediately. The bounds are added to wall-clock
// times separately; a duration near MaxInt64 (~292 years) plus a 5s grace
// cannot wrap a time.Time anchored at the present (year ~2318), so no
// clamping is needed.
func clientWaitDeadline(now time.Time, timeout time.Duration, status protocol.JobStatus, startedAt, noWorkerDeadline *time.Time) (time.Time, bool) {
	var deadline time.Time
	has := false
	if timeout > 0 && status != protocol.JobStatusQueued {
		anchor := now
		if startedAt != nil {
			anchor = *startedAt
		}
		deadline = anchor.Add(timeout)
		has = true
	}
	if noWorkerDeadline != nil {
		if !has || noWorkerDeadline.After(deadline) {
			deadline = *noWorkerDeadline
		}
		has = true
	}
	if !has {
		return time.Time{}, false
	}
	return deadline.Add(clientVerdictGrace), true
}

// extendWaitForNoWorkerDeadline reports whether the client should extend its
// wait after the client-side deadline fired: the server must now report a
// verdict deadline (the job reverted to pending after its worker died) that
// is later than the bound just exhausted. Comparing the raw server verdict
// against the exhausted bound — not a recomputed now+timeout bound, which
// always advances with now — keeps the retry bounded: the deadline is fixed
// once attached, so this can extend the wait at most once.
func extendWaitForNoWorkerDeadline(exhaustedDeadline time.Time, noWorkerDeadline *time.Time) bool {
	return noWorkerDeadline != nil && noWorkerDeadline.After(exhaustedDeadline)
}

// extendWaitForStartedAt reports whether the client should extend its wait
// after the client-side deadline fired because the job just started running:
// the first wait was anchored to submit time (started_at was nil), but the
// worker's ffmpeg budget only begins at started_at, so the TIMEOUT verdict is
// still ahead of the exhausted bound. The re-anchored bound (started_at +
// timeout + grace) is fixed — started_at never advances — so this can extend
// the wait at most once (TSI-2886).
func extendWaitForStartedAt(exhaustedDeadline time.Time, startedAt *time.Time, timeout time.Duration) bool {
	if startedAt == nil || timeout <= 0 {
		return false
	}
	return startedAt.Add(timeout).Add(clientVerdictGrace).After(exhaustedDeadline)
}

// extendWaitForQueued reports whether the client should extend its wait after
// the client-side deadline fired because the job was just claimed by a worker
// (pending → queued). A queued job spends --timeout on its pre-exec phase
// (download/probe), which the worker bounds independently, so the client must
// not cancel it as if it had never started (TSI-2886).
func extendWaitForQueued(status protocol.JobStatus) bool {
	return status == protocol.JobStatusQueued
}

// reportTerminalJob prints the outcome for a terminal job status and returns
// the process exit code. It returns ExitSuccess only for a completed job; all
// other terminal statuses print an error to stderr and return ExitError.
//
// JobStatusFailed with FailureNoWorkerAvailable is the server's starvation
// verdict (checkNoWorkerStarvation) — it must read as "the server judged this
// failed", never "the client gave up", so operators can distinguish the two.
func reportTerminalJob(job *protocol.JobInfo) int {
	if job == nil {
		fmt.Fprintln(os.Stderr, "Error: no job result returned from server")
		return ExitError
	}

	if job.Status == protocol.JobStatusFailed {
		if job.FailureType == string(protocol.FailureNoWorkerAvailable) {
			fmt.Fprintf(os.Stderr, "Error: job %s failed: server reported no worker available (server-side auto_fail): %s\n", job.ID, job.Error)
		} else {
			fmt.Fprintf(os.Stderr, "Job failed: %s\n", job.Error)
		}
		// Normalize all non-zero ffmpeg exit codes to 1 (standard error exit code)
		// This ensures consistent error handling regardless of ffmpeg's specific exit codes
		return ExitError
	}

	if job.Status == protocol.JobStatusTimeout {
		fmt.Fprintf(os.Stderr, "Job timed out: %s\n", job.Error)
		return ExitError
	}

	if job.Status == protocol.JobStatusCancelled {
		fmt.Fprintln(os.Stderr, "Job was cancelled")
		return ExitError
	}

	// Safety net: if the job reached a non-terminal status (e.g., "running",
	// "pending", "queued") due to a race between WebSocket close and HTTP poll,
	// treat it as a failure rather than reporting success.
	if job.Status != protocol.JobStatusCompleted {
		fmt.Fprintf(os.Stderr, "Error: job ended with unexpected status: %s\n", job.Status)
		return ExitError
	}

	return ExitSuccess
}

// runProbe handles the "probe" subcommand.
func runProbe(cli *client.Client, input string, quiet bool, sharedFS bool) int {
	if input == "" {
		fmt.Fprintln(os.Stderr, "Error: probe requires an input file or URL")
		fmt.Fprintln(os.Stderr, "Usage: rffmpeg probe <file|URL>")
		return ExitError
	}

	if !quiet {
		fmt.Fprintf(os.Stderr, "Probing %s...\n", input)
	}

	// Handle file:// URI — strip scheme and treat as local file path
	if strings.HasPrefix(input, "file://") {
		filePath, _ := args.StripFileScheme(input)
		absPath, err := args.GetAbsPath(filePath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error resolving path %s: %v\n", input, err)
			return ExitError
		}
		// Validate that the file exists
		if err := args.ValidateFileExists(absPath); err != nil {
			fmt.Fprintf(os.Stderr, "Error validating input %s: %v\n", input, err)
			return ExitError
		}
		if !quiet {
			fmt.Fprintf(os.Stderr, "Using local file: %s\n", absPath)
		}
		// In shared FS mode, send the local path directly.
		// Otherwise upload and send the file ID.
		if sharedFS {
			return runProbeRequest(cli, absPath, quiet)
		}
		if !quiet {
			fmt.Fprintf(os.Stderr, "Uploading %s...\n", absPath)
		}
		fileID, err := cli.UploadFileAuto(absPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error uploading %s: %v\n", absPath, err)
			return ExitError
		}
		if !quiet {
			fmt.Fprintf(os.Stderr, "Uploaded: %s -> %s\n", absPath, fileID)
		}
		return runProbeRequest(cli, fileID, quiet)
	}

	// Detect remote URL input (contains "://") — skip upload, send URL directly
	if strings.Contains(input, "://") {
		if !quiet {
			fmt.Fprintf(os.Stderr, "Using remote URL: %s\n", input)
		}
		return runProbeRequest(cli, input, quiet)
	}

	// Shared FS mode: skip upload, use path directly
	if sharedFS {
		absPath, err := args.GetAbsPath(input)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error resolving path %s: %v\n", input, err)
			return ExitError
		}
		// Validate that the file exists
		if err := args.ValidateFileExists(absPath); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			return ExitError
		}
		if !quiet {
			fmt.Fprintf(os.Stderr, "Shared FS: using direct path %s\n", absPath)
		}
		return runProbeRequest(cli, absPath, quiet)
	}

	// Local file: resolve path, upload, then probe
	absPath, err := args.GetAbsPath(input)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error resolving path %s: %v\n", input, err)
		return ExitError
	}

	// Validate that the file exists
	if err := args.ValidateFileExists(absPath); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return ExitError
	}

	if !quiet {
		fmt.Fprintf(os.Stderr, "Uploading %s...\n", input)
	}

	fileID, err := cli.UploadFileAuto(absPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error uploading %s: %v\n", input, err)
		return ExitError
	}

	if !quiet {
		fmt.Fprintf(os.Stderr, "Uploaded: %s -> %s\n", input, fileID)
	}

	return runProbeRequest(cli, fileID, quiet)
}

// runEncoders queries encoders from the server and prints in ffmpeg-compatible format.
// When jsonOut is true, outputs JSON instead of text.
// Falls back to local ffmpeg if server is unavailable.
func runEncoders(serverURL, token string, jsonOut bool) int {
	cli, err := setupClient(serverURL, token)
	if err == nil {
		encoders, apiErr := cli.ListAllEncoders()
		if apiErr == nil {
			if jsonOut {
				return printJSONEncoderInfo(encoders, "encoders")
			}
			if len(encoders) == 0 {
				fmt.Fprintf(os.Stderr, "No encoders available\n")
				return ExitSuccess
			}
			printEncoderDecoderHeader()
			for _, enc := range encoders {
				capFlags := encoderCapabilityFlags(enc.Type)
				fmt.Printf("%s %-22s %s\n", capFlags, enc.Name, enc.Description)
			}
			return ExitSuccess
		}
	}
	// Server unavailable — fall back to local ffmpeg
	if !jsonOut {
		return runLocalFfmpegPassthrough("-encoders")
	}
	output, runErr := runLocalFfmpegOutput("-encoders")
	if runErr != nil {
		fmt.Fprintf(os.Stderr, "Error running local ffmpeg -encoders: %v\n", runErr)
		return ExitError
	}
	encoders := parseFFmpegEncoderLines(output)
	return printJSONEncoderInfo(encoders, "encoders")
}

// runDecoders queries decoders from the server and prints in ffmpeg-compatible format.
// When jsonOut is true, outputs JSON instead of text.
// Falls back to local ffmpeg if server is unavailable.
func runDecoders(serverURL, token string, jsonOut bool) int {
	cli, err := setupClient(serverURL, token)
	if err == nil {
		decoders, apiErr := cli.ListAllDecoders()
		if apiErr == nil {
			if jsonOut {
				return printJSONDecoderInfo(decoders, "decoders")
			}
			if len(decoders) == 0 {
				fmt.Fprintf(os.Stderr, "No decoders available\n")
				return ExitSuccess
			}
			printEncoderDecoderHeader()
			for _, dec := range decoders {
				capFlags := decoderCapabilityFlags(dec.Type)
				fmt.Printf("%s %-22s %s\n", capFlags, dec.Name, dec.Description)
			}
			return ExitSuccess
		}
	}
	// Server unavailable — fall back to local ffmpeg
	if !jsonOut {
		return runLocalFfmpegPassthrough("-decoders")
	}
	output, runErr := runLocalFfmpegOutput("-decoders")
	if runErr != nil {
		fmt.Fprintf(os.Stderr, "Error running local ffmpeg -decoders: %v\n", runErr)
		return ExitError
	}
	decoders := parseFFmpegEncoderLines(output)
	// Convert EncoderInfo slice to DecoderInfo slice (same fields)
	decoderInfos := make([]protocol.DecoderInfo, len(decoders))
	for i, e := range decoders {
		decoderInfos[i] = protocol.DecoderInfo{
			Name:        e.Name,
			Description: e.Description,
			Type:        e.Type,
		}
	}
	return printJSONDecoderInfo(decoderInfos, "decoders")
}

// runCodecsFromServer queries both encoders and decoders from the server and prints in
// ffmpeg-compatible format: encoder list first, then decoder list.
// When jsonOut is true, outputs JSON instead of text.
// Falls back to local ffmpeg if server is unavailable.
func runCodecsFromServer(serverURL, token string, jsonOut bool) int {
	cli, err := setupClient(serverURL, token)
	if err == nil {
		if jsonOut {
			encoders, apiErr1 := cli.ListAllEncoders()
			if apiErr1 != nil {
				goto fallback
			}
			decoders, apiErr2 := cli.ListAllDecoders()
			if apiErr2 != nil {
				goto fallback
			}
			return outputInfoFlagJSON("codecs", encoders, decoders)
		}

		encoders, apiErr := cli.ListAllEncoders()
		if apiErr == nil {
			decoders, apiErr2 := cli.ListAllDecoders()
			if apiErr2 == nil {
				printCodecsHeader("Encoders")
				if len(encoders) > 0 {
					for _, enc := range encoders {
						capFlags := encoderCapabilityFlags(enc.Type)
						fmt.Printf("%s %-22s %s\n", capFlags, enc.Name, enc.Description)
					}
				} else {
					fmt.Fprintf(os.Stderr, "No encoders available\n")
				}

				printCodecsHeader("Decoders")
				if len(decoders) > 0 {
					for _, dec := range decoders {
						capFlags := decoderCapabilityFlags(dec.Type)
						fmt.Printf("%s %-22s %s\n", capFlags, dec.Name, dec.Description)
					}
				} else {
					fmt.Fprintf(os.Stderr, "No decoders available\n")
				}
				return ExitSuccess
			}
		}
	}

fallback:
	// Server unavailable — fall back to local ffmpeg
	if !jsonOut {
		return runLocalFfmpegPassthrough("-codecs")
	}
	// JSON mode: run -encoders and -decoders separately and combine
	encOut, encErr := runLocalFfmpegOutput("-encoders")
	if encErr != nil {
		fmt.Fprintf(os.Stderr, "Error running local ffmpeg -encoders: %v\n", encErr)
		return ExitError
	}
	decOut, decErr := runLocalFfmpegOutput("-decoders")
	if decErr != nil {
		fmt.Fprintf(os.Stderr, "Error running local ffmpeg -decoders: %v\n", decErr)
		return ExitError
	}
	encoders := parseFFmpegEncoderLines(encOut)
	encDecoderLines := parseFFmpegEncoderLines(decOut)
	decoderInfos := make([]protocol.DecoderInfo, len(encDecoderLines))
	for i, e := range encDecoderLines {
		decoderInfos[i] = protocol.DecoderInfo{
			Name:        e.Name,
			Description: e.Description,
			Type:        e.Type,
		}
	}
	return outputInfoFlagJSON("codecs", encoders, decoderInfos)
}

// runLocalFfmpegInfo runs a local ffmpeg info flag and prints output to stdout.
// Used for P2 info flags that don't have a server API endpoint.
func runLocalFfmpegInfo(flag string) int {
	cmd := exec.Command("ffmpeg", flag)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Error running ffmpeg %s: %v\n", flag, err)
		return ExitError
	}
	return ExitSuccess
}

// === Local ffmpeg fallback helpers for P0/P1 info flags ===

// runLocalFfmpegOutput runs a local ffmpeg with the given flag, capturing stdout
// and discarding stderr. Returns the combined output string.
func runLocalFfmpegOutput(flag string) (string, error) {
	cmd := exec.Command("ffmpeg", flag)
	cmd.Stderr = nil // discard version banner on stderr

	var stdout bytes.Buffer
	cmd.Stdout = &stdout

	err := cmd.Run()
	return stdout.String(), err
}

// runLocalFfmpegPassthrough runs ffmpeg with the given flag, piping stdout and
// stderr directly to the parent process. Returns ffmpeg's exit code.
func runLocalFfmpegPassthrough(flag string) int {
	cmd := exec.Command("ffmpeg", flag)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Error running ffmpeg %s: %v\n", flag, err)
		return ExitError
	}
	return ExitSuccess
}

// parseFFmpegEncoderLines parses ffmpeg -encoders / -decoders output lines.
// Each data line has format: " V....D name               description"
// Returns a slice of EncoderInfo with Name, Description, and Type.
func parseFFmpegEncoderLines(output string) []protocol.EncoderInfo {
	var encoders []protocol.EncoderInfo
	lines := strings.Split(output, "\n")
	inData := false
	for _, line := range lines {
		line = strings.TrimRight(line, "\r")
		if !inData {
			if strings.Contains(line, "------") {
				inData = true
			}
			continue
		}
		// Trim leading spaces
		trimmed := strings.TrimLeft(line, " ")
		if len(trimmed) < 8 {
			continue
		}
		typeChar := trimmed[0]
		if typeChar != 'V' && typeChar != 'A' && typeChar != 'S' {
			continue
		}

		// Skip the 6-char flag (type + 5 capability chars) + space = 7 chars
		rest := trimmed[7:]
		// Name is the first non-whitespace token, description is after 2+ spaces
		parts := strings.SplitN(rest, "  ", 2)
		name := strings.TrimSpace(parts[0])
		desc := ""
		if len(parts) > 1 {
			desc = strings.TrimSpace(parts[1])
		}

		encoderType := "unknown"
		switch typeChar {
		case 'V':
			encoderType = "video"
		case 'A':
			encoderType = "audio"
		case 'S':
			encoderType = "subtitle"
		}

		encoders = append(encoders, protocol.EncoderInfo{
			Name:        name,
			Description: desc,
			Type:        encoderType,
		})
	}
	return encoders
}

// parseFFmpegNameList parses simple ffmpeg info output that lists names
// (hwaccels, filters, pix_fmts, formats). It skips header/legend lines and
// extracts the name field (second whitespace-delimited token) from each data line.
//
// Two modes:
//  1. Standard mode: if a separator line (--- / ------ ) is found, it parses
//     the two-column format used by -filters, -pix_fmts, and -formats (flags + name).
//  2. Fallback mode: if no separator is found, it treats the output as a
//     single-column name list (used by -hwaccels). Header lines containing ':'
//     are skipped, and every other non-empty line is treated as a name.
func parseFFmpegNameList(output string) []string {
	var names []string
	lines := strings.Split(output, "\n")
	inData := false
	for _, line := range lines {
		line = strings.TrimRight(line, "\r")
		if !inData {
			// Look for separator line (------ or ---)
			if strings.HasPrefix(strings.TrimSpace(line), "---") {
				inData = true
			}
			continue
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		// Extract the name: first token is flags (like "IO..." or " D " or " TS"),
		// second token is the name. Split on whitespace and take the second field.
		fields := strings.Fields(trimmed)
		if len(fields) >= 2 {
			name := fields[1]
			// Skip non-name entries (e.g., legend leftovers)
			if name == "=" || name == "---" || strings.HasPrefix(name, "---") {
				continue
			}
			names = append(names, name)
		}
	}
	// Fallback: if no data was collected (no separator found), treat as a
	// single-column list (e.g. -hwaccels output). Skip header lines containing
	// ':' and collect all remaining non-empty lines as names.
	if len(names) == 0 {
		for _, line := range lines {
			line = strings.TrimRight(line, "\r")
			trimmed := strings.TrimSpace(line)
			if trimmed == "" || strings.Contains(trimmed, ":") {
				continue
			}
			if strings.HasPrefix(trimmed, "---") {
				continue
			}
			names = append(names, trimmed)
		}
	}
	return names
}

// outputInfoFlagJSON outputs codecs (encoders+decoders) as JSON array.
func outputInfoFlagJSON(key string, encoders []protocol.EncoderInfo, decoders []protocol.DecoderInfo) int {
	type codecEntry struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Type        string `json:"type"`
		IsEncoder   bool   `json:"is_encoder"`
	}
	var entries []codecEntry
	for _, e := range encoders {
		entries = append(entries, codecEntry{
			Name:        e.Name,
			Description: e.Description,
			Type:        e.Type,
			IsEncoder:   true,
		})
	}
	for _, d := range decoders {
		entries = append(entries, codecEntry{
			Name:        d.Name,
			Description: d.Description,
			Type:        d.Type,
			IsEncoder:   false,
		})
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return jsonEncodeToStdout(enc, map[string]interface{}{key: entries})
}

// printJSONEncoderInfo outputs encoders as JSON to stdout.
func printJSONEncoderInfo(encoders []protocol.EncoderInfo, key string) int {
	type entry struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Type        string `json:"type"`
	}
	var entries []entry
	for _, e := range encoders {
		entries = append(entries, entry{
			Name:        e.Name,
			Description: e.Description,
			Type:        e.Type,
		})
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return jsonEncodeToStdout(enc, map[string]interface{}{key: entries})
}

// printJSONDecoderInfo outputs decoders as JSON to stdout.
func printJSONDecoderInfo(decoders []protocol.DecoderInfo, key string) int {
	type entry struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Type        string `json:"type"`
	}
	var entries []entry
	for _, d := range decoders {
		entries = append(entries, entry{
			Name:        d.Name,
			Description: d.Description,
			Type:        d.Type,
		})
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return jsonEncodeToStdout(enc, map[string]interface{}{key: entries})
}

// printJSONArray outputs a string array as JSON.
func printJSONArray(items []string, key string) int {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return jsonEncodeToStdout(enc, map[string]interface{}{key: items})
}

// jsonEncodeToStdout encodes a value to stdout as JSON with indentation.
func jsonEncodeToStdout(enc *json.Encoder, v interface{}) int {
	if err := enc.Encode(v); err != nil {
		fmt.Fprintf(os.Stderr, "Error encoding JSON: %v\n", err)
		return ExitError
	}
	return ExitSuccess
}

// runHwaccels queries hwaccels from the server and prints in ffmpeg-compatible format (single-column list).
// When jsonOut is true, outputs JSON instead of text.
// Falls back to local ffmpeg if server is unavailable.
func runHwaccels(serverURL, token string, jsonOut bool) int {
	cli, err := setupClient(serverURL, token)
	if err == nil {
		hwaccels, apiErr := cli.ListAllHwaccels()
		if apiErr == nil {
			if jsonOut {
				return printJSONArray(hwaccels, "hwaccels")
			}
			if len(hwaccels) == 0 {
				fmt.Fprintf(os.Stderr, "No hwaccels available\n")
				return ExitSuccess
			}
			for _, h := range hwaccels {
				fmt.Println(h)
			}
			return ExitSuccess
		}
	}
	// Server unavailable — fall back to local ffmpeg
	if !jsonOut {
		return runLocalFfmpegPassthrough("-hwaccels")
	}
	output, runErr := runLocalFfmpegOutput("-hwaccels")
	if runErr != nil {
		fmt.Fprintf(os.Stderr, "Error running local ffmpeg -hwaccels: %v\n", runErr)
		return ExitError
	}
	hwaccels := parseFFmpegNameList(output)
	return printJSONArray(hwaccels, "hwaccels")
}

// runFilters queries filters from the server and prints in ffmpeg-compatible format.
// Falls back to local ffmpeg if server is unavailable.
func runFilters(serverURL, token string, jsonOut bool) int {
	cli, err := setupClient(serverURL, token)
	if err == nil {
		items, apiErr := cli.ListAllFilters(jsonOut)
		if apiErr == nil {
			if jsonOut {
				return printJSONArray(items, "filters")
			}
			if len(items) == 0 {
				fmt.Fprintf(os.Stderr, "No filters available\n")
				return ExitSuccess
			}
			fmt.Println("Filters:")
			for _, item := range items {
				fmt.Println(item)
			}
			return ExitSuccess
		}
	}
	// Server unavailable — fall back to local ffmpeg
	if !jsonOut {
		return runLocalFfmpegPassthrough("-filters")
	}
	output, runErr := runLocalFfmpegOutput("-filters")
	if runErr != nil {
		fmt.Fprintf(os.Stderr, "Error running local ffmpeg -filters: %v\n", runErr)
		return ExitError
	}
	filters := parseFFmpegNameList(output)
	return printJSONArray(filters, "filters")
}

// runPixFmts queries pixel formats from the server and prints in ffmpeg-compatible format.
// Falls back to local ffmpeg if server is unavailable.
func runPixFmts(serverURL, token string, jsonOut bool) int {
	cli, err := setupClient(serverURL, token)
	if err == nil {
		items, apiErr := cli.ListAllPixFmts(jsonOut)
		if apiErr == nil {
			if jsonOut {
				return printJSONArray(items, "pix_fmts")
			}
			if len(items) == 0 {
				fmt.Fprintf(os.Stderr, "No pixel formats available\n")
				return ExitSuccess
			}
			fmt.Println("Pixel formats:")
			for _, item := range items {
				fmt.Println(item)
			}
			return ExitSuccess
		}
	}
	// Server unavailable — fall back to local ffmpeg
	if !jsonOut {
		return runLocalFfmpegPassthrough("-pix_fmts")
	}
	output, runErr := runLocalFfmpegOutput("-pix_fmts")
	if runErr != nil {
		fmt.Fprintf(os.Stderr, "Error running local ffmpeg -pix_fmts: %v\n", runErr)
		return ExitError
	}
	pixFmts := parseFFmpegNameList(output)
	return printJSONArray(pixFmts, "pix_fmts")
}

// runFormats queries formats from the server and prints in ffmpeg-compatible format.
// Falls back to local ffmpeg if server is unavailable.
func runFormats(serverURL, token string, jsonOut bool) int {
	cli, err := setupClient(serverURL, token)
	if err == nil {
		items, apiErr := cli.ListAllFormats(jsonOut)
		if apiErr == nil {
			if jsonOut {
				return printJSONArray(items, "formats")
			}
			if len(items) == 0 {
				fmt.Fprintf(os.Stderr, "No formats available\n")
				return ExitSuccess
			}
			fmt.Println("File formats:")
			for _, item := range items {
				fmt.Println(item)
			}
			return ExitSuccess
		}
	}
	// Server unavailable — fall back to local ffmpeg
	if !jsonOut {
		return runLocalFfmpegPassthrough("-formats")
	}
	output, runErr := runLocalFfmpegOutput("-formats")
	if runErr != nil {
		fmt.Fprintf(os.Stderr, "Error running local ffmpeg -formats: %v\n", runErr)
		return ExitError
	}
	formats := parseFFmpegNameList(output)
	return printJSONArray(formats, "formats")
}

// setupClient loads config, overrides with CLI flags, creates a client, and runs a
// health check. Returns the initialized client or an error.
func setupClient(serverURL, token string) (*client.Client, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, fmt.Errorf("loading config: %w", err)
	}
	if serverURL != "" {
		cfg.ServerURL = serverURL
	}
	if token != "" {
		cfg.Token = token
	}
	c := client.New(cfg.ServerURL, cfg.Token)
	if err := c.HealthCheck(); err != nil {
		return nil, fmt.Errorf("server health check failed: %w", err)
	}
	return c, nil
}

// encoderCapabilityFlags builds ffmpeg-compatible capability flags for an encoder.
// Format: 6 chars: [type_char][F][S][X][B][D]
// F = frame-level multithreading, S = slice-based, X = experimental,
// B = draw_horiz_band, D = direct rendering.
func encoderCapabilityFlags(encoderType string) string {
	flags := make([]byte, 6)
	for i := range flags {
		flags[i] = '.'
	}
	switch encoderType {
	case "video":
		flags[0] = 'V'
	case "audio":
		flags[0] = 'A'
	case "subtitle":
		flags[0] = 'S'
	default:
		flags[0] = '.'
	}
	return string(flags)
}

// decoderCapabilityFlags builds ffmpeg-compatible capability flags for a decoder.
// Same format as encoderCapabilityFlags.
func decoderCapabilityFlags(decoderType string) string {
	return encoderCapabilityFlags(decoderType)
}

// printEncoderDecoderHeader prints the column header matching ffmpeg -encoders output.
func printEncoderDecoderHeader() {
	fmt.Printf(" %s ------\n", "Encoders:")
	fmt.Printf(" %c..... = Video\n", 'V')
	fmt.Printf(" %c..... = Audio\n", 'A')
	fmt.Printf(" %c..... = Subtitle\n", 'S')
	fmt.Printf(" %s = Frame-level multithreading\n", ".F....")
	fmt.Printf(" %s = Slice-level multithreading\n", "..S...")
	fmt.Printf(" %s = Experimental\n", "...X..")
	fmt.Printf(" %s = Draw horizontal band\n", "....B.")
	fmt.Printf(" %s = Direct rendering\n", ".....D")
	fmt.Println(" ------")
}

// printCodecsHeader prints a section header for the -codecs output.
// This separates the encoder list from the decoder list.
func printCodecsHeader(section string) {
	fmt.Printf("\n %s:\n", section)
	fmt.Printf(" %c..... = %s\n", 'V', "Video")
	fmt.Printf(" %c..... = %s\n", 'A', "Audio")
	fmt.Printf(" %c..... = %s\n", 'S', "Subtitle")
	fmt.Printf(" %s = Frame-level multithreading\n", ".F....")
	fmt.Printf(" %s = Slice-level multithreading\n", "..S...")
	fmt.Printf(" %s = Experimental\n", "...X..")
	fmt.Printf(" %s = Draw horizontal band\n", "....B.")
	fmt.Printf(" %s = Direct rendering\n", ".....D")
	fmt.Println(" ------")
}

// runProbeRequest sends a probe request to the server and outputs the result.
func runProbeRequest(cli *client.Client, input string, quiet bool) int {
	if !quiet {
		fmt.Fprintln(os.Stderr, "Running ffprobe...")
	}

	// Request probe
	result, err := cli.Probe(input)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error probing file: %v\n", err)
		return ExitError
	}

	// Output JSON result
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(result); err != nil {
		fmt.Fprintf(os.Stderr, "Error encoding probe result: %v\n", err)
		return ExitError
	}

	return ExitSuccess
}

func printUsage() {
	fmt.Fprintf(os.Stderr, `rffmpeg - Remote FFmpeg Client
Usage:
  rffmpeg [rffmpeg_options] [ffmpeg_options]        Run ffmpeg transcoding
  rffmpeg probe <file|URL> [-i <file|URL>] [-show_streams] [-show_format] [-of json] [--server URL] [--token TOKEN] [-q]   Probe media file or URL
  rffmpeg -encoders                                  List available encoders (ffmpeg-compatible format)
  rffmpeg -decoders                                  List available decoders (ffmpeg-compatible format)
  rffmpeg -codecs                                    List available codecs (encoders + decoders, ffmpeg-compatible format)
  rffmpeg -hwaccels                                  List available hwaccels (ffmpeg-compatible format)
  rffmpeg -filters                                   List available filters (ffmpeg-compatible format)
  rffmpeg -pix_fmts                                  List available pixel formats (ffmpeg-compatible format)
  rffmpeg -formats                                   List available muxers/demuxers (ffmpeg-compatible format)
  rffmpeg -buildconf                                 Show build configuration (runs local ffmpeg)
  rffmpeg -layouts                                   List channel layouts (runs local ffmpeg)
  rffmpeg -protocols                                 List protocols (runs local ffmpeg)
  rffmpeg -sample_fmts                               List sample formats (runs local ffmpeg)
  rffmpeg -bsfs                                      List bitstream filters (runs local ffmpeg)
  rffmpeg -colors                                    List color names (runs local ffmpeg)

rffmpeg options:
  -h, --help      Show this help message
  --version       Show version
  --json          Output info flags as JSON (applies to: -encoders, -decoders, -codecs, -filters, -pix_fmts, -formats)
  -encoders       List all available encoders (queries server, no input file required)
  -decoders       List all available decoders (queries server, no input file required)
  -codecs         List all available codecs (encoders + decoders, queries server, no input file required)
  -hwaccels       List all available hwaccels (queries server, no input file required)
  -filters        List all available filters (queries server, no input file required)
  -pix_fmts       List all available pixel formats (queries server, no input file required)
  -formats        List all available muxers/demuxers (queries server, no input file required)
  -buildconf      Show build configuration (runs local ffmpeg)
  -layouts        List channel layouts (runs local ffmpeg)
  -protocols      List protocols (runs local ffmpeg)
  -sample_fmts    List sample formats (runs local ffmpeg)
  -bsfs           List bitstream filters (runs local ffmpeg)
  -colors         List color names (runs local ffmpeg)
  --server URL    Server root URL (overrides config, default: http://localhost:8080, no /api/v1)
  --token TOKEN   Auth token (overrides config)
  -q, --quiet     Quiet mode (suppress progress output)
  --auto-hw[=true|false]  Enable automatic hardware encoder upgrade (default: false)
  --timeout DURATION      Job execution timeout (e.g., 30s, 5m, 2h)
  --max-retries N         Max WS reconnect attempts / HTTP poll retry budget (default: 14, ~5 min; 0 = no retries)

ffmpeg options:
  All standard ffmpeg options are supported and passed through to the server.
  rffmpeg options and ffmpeg options can be mixed freely.

Examples:
  # Probe a media file
  rffmpeg probe video.mp4

  # Probe with ffprobe-style flags (-i / -show_streams / -of json)
  rffmpeg probe -i video.mp4 -show_streams -of json

  # List available encoders
  rffmpeg -encoders
  rffmpeg -encoders | grep nvenc

  # List available decoders
  rffmpeg -decoders

  # List available codecs
  rffmpeg -codecs

  # List available filters
  rffmpeg -filters

  # List pixel formats
  rffmpeg -pix_fmts

  # List formats (muxers/demuxers)
  rffmpeg -formats

  # JSON output for info flags
  rffmpeg -filters --json

  # Show ffmpeg build configuration
  rffmpeg -buildconf

  # Basic transcoding
  rffmpeg -i input.mp4 -c:v libx264 output.mp4

  # With custom server
  rffmpeg --server https://rffmpeg.example.com -i video.mkv -c:a aac audio.mp4

  # Hardware acceleration
  rffmpeg -i input.mp4 -vf scale=1280:720 -c:v h264_nvenc output.mp4

  # Auto hardware encoder upgrade
  rffmpeg --auto-hw -i input.mp4 -c:v libx264 output.mp4

Configuration:
  Config file locations (in order of priority):
    ./rffmpeg.json
    ~/.rffmpeg.json
    /etc/rffmpeg.json

  Environment variables:
    RFFMPEG_SERVER_URL  Server URL
    RFFMPEG_TOKEN       Auth token
    RFFMPEG_SHARED_FS   Enable shared filesystem mode (set to 1 or true)

`)
}
