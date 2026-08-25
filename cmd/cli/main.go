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
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/tsix404/rffmpeg/pkg/cli/args"
	"github.com/tsix404/rffmpeg/pkg/cli/client"
	"github.com/tsix404/rffmpeg/pkg/cli/config"
	"github.com/tsix404/rffmpeg/pkg/protocol"
)

const (
	ExitSuccess = 0
	ExitError   = 1
)

var version = "1.0.0"

// Options holds the rffmpeg-specific options extracted from the command line.
type Options struct {
	ServerURL   string
	Token       string
	Quiet       bool
	ShowHelp    bool
	ShowVersion bool
	AutoHW      bool
	IsProbe     bool
	ProbeInput  string
	Timeout     time.Duration
	FmpegArgs   []string

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

		// Everything after "probe" goes to FmpegArgs; first arg is the probe input.
		// Support both "probe <file>" and "probe -i <file>" syntax.
		opts.FmpegArgs = positionalArgs
		if len(opts.FmpegArgs) > 0 {
			if opts.FmpegArgs[0] == "-i" {
				// Skip the -i flag and use next arg as probe input
				if len(opts.FmpegArgs) >= 2 {
					opts.ProbeInput = opts.FmpegArgs[1]
					opts.FmpegArgs = opts.FmpegArgs[2:]
				} else {
					// "-i" without a value — skip it, ProbeInput remains empty
					opts.FmpegArgs = opts.FmpegArgs[1:]
				}
			} else {
				opts.ProbeInput = opts.FmpegArgs[0]
				opts.FmpegArgs = opts.FmpegArgs[1:]
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

	// If no ffmpeg args and not in probe mode, show help instead of connecting to server
	if !opts.IsProbe && len(ffmpegArgs) == 0 {
		printUsage()
		return ExitSuccess
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

	// Detect shared filesystem mode
	sharedFS := cfg.IsSharedFS()
	if sharedFS && !opts.Quiet {
		fmt.Fprintln(os.Stderr, "Shared filesystem mode enabled: skipping upload/download")
	}

	// Create client
	cli := client.New(cfg.ServerURL, cfg.Token)

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
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
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
	// For network URLs (rtmp://, udp://, etc.), pass the full URL directly.
	outputFilename := ""
	if result.OutputFile != "" {
		if sharedFS {
			absOut, err := args.GetAbsPath(result.OutputFile)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error resolving output path %s: %v\n", result.OutputFile, err)
				return ExitError
			}
			outputFilename = absOut
		} else if strings.Contains(result.OutputFile, "://") {
			// Network URL (rtmp://, rtsp://, udp://, etc.) — pass through as-is
			outputFilename = result.OutputFile
		} else {
			outputFilename = filepath.Base(result.OutputFile)
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

	// Wait for completion with real-time log streaming.
	// Enforce the client-side timeout so a stuck job (e.g. a worker killed
	// within the heartbeat window) never leaves the user waiting forever.
	waitCtx := context.Background()
	var cancelWait context.CancelFunc
	if timeout > 0 {
		waitCtx, cancelWait = context.WithTimeout(context.Background(), timeout)
		defer cancelWait()
	}

	var job *protocol.JobInfo
	if result.StreamingOutput {
		// Streaming output mode: receive stdout data via WebSocket
		job, err = cli.WaitForJobWithStreamingOutput(waitCtx, jobID, quiet)
	} else {
		job, err = cli.WaitForJobWithLogs(waitCtx, jobID, quiet)
	}
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			// The job never reached a terminal state. Cancel it server-side so
			// it doesn't linger, and report a clear timeout instead of hanging.
			if cancelErr := cli.CancelJob(jobID); cancelErr != nil {
				fmt.Fprintf(os.Stderr, "Warning: failed to cancel timed-out job %s: %v\n", jobID, cancelErr)
			}
			fmt.Fprintf(os.Stderr, "Error: job %s did not complete within %s and was cancelled (worker may be unavailable)\n", jobID, timeout)
		} else {
			fmt.Fprintf(os.Stderr, "Error waiting for job: %v\n", err)
		}
		return ExitError
	}

	// Handle job result
	if job.Status == protocol.JobStatusFailed {
		fmt.Fprintf(os.Stderr, "Job failed: %s\n", job.Error)
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
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
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
  rffmpeg probe <file|URL> [--server URL] [--token TOKEN] [-q]   Probe media file or URL
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
  --server URL    Server URL (overrides config, default: http://localhost:8080)
  --token TOKEN   Auth token (overrides config)
  -q, --quiet     Quiet mode (suppress progress output)
  --auto-hw[=true|false]  Enable automatic hardware encoder upgrade (default: false)
  --timeout DURATION      Job execution timeout (e.g., 30s, 5m, 2h)

ffmpeg options:
  All standard ffmpeg options are supported and passed through to the server.
  rffmpeg options and ffmpeg options can be mixed freely.

Examples:
  # Probe a media file
  rffmpeg probe video.mp4

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
