package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/tsic404/rffmpeg/pkg/cli/args"
	"github.com/tsic404/rffmpeg/pkg/cli/client"
	"github.com/tsic404/rffmpeg/pkg/cli/config"
	"github.com/tsic404/rffmpeg/pkg/ffmpegopts"
	"github.com/tsic404/rffmpeg/pkg/pathutil"
	"github.com/tsic404/rffmpeg/pkg/protocol"
)

// runTranscode submits the transcoding job described by opts/ffmpegArgs and
// waits for it, streaming logs and downloading outputs.
func runTranscode(cli *client.Client, cfg *config.Config, opts *Options, ffmpegArgs []string, sharedFS bool, tee *stderrTee) int {
	quiet := opts.Quiet
	autoHW := opts.AutoHW
	timeout := opts.Timeout
	pollTimeout := pollTimeoutBudget(cfg, opts)

	parser := args.NewParser()
	result, err := parser.Parse(ffmpegArgs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error parsing arguments: %v\n", err)
		return ExitError
	}

	if msg := args.StreamingOutputError(result); msg != "" {
		fmt.Fprintln(os.Stderr, msg)
		return ExitError
	}

	if msg := args.OutputExtensionWarning(result); msg != "" {
		fmt.Fprintln(os.Stderr, msg)
	}

	if !quiet {
		fmt.Fprintf(os.Stderr, "Input files: %v\n", result.InputFiles)
		fmt.Fprintf(os.Stderr, "Output file: %s\n", result.OutputFile)
		if sharedFS {
			fmt.Fprintln(os.Stderr, "Mode: shared filesystem (passthrough)")
		}
	}

	if code := enforceOverwritePolicy(result.OutputFile, result.StreamingOutput, ffmpegArgs); code != ExitSuccess {
		return code
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	var (
		sigMu      sync.Mutex
		jobID      string
		cancelled  bool
		submitting bool
	)

	// Resolve the real stderr in this goroutine: the signal handler must not
	// read the process-wide os.Stderr, which a caller may restore as soon as
	// this function returns.
	stderr := tee.realStderr()
	go func() {
		var handlerCancelled bool
		for range sigChan {
			cancel()

			sigMu.Lock()
			id := jobID
			done := cancelled
			inFlight := submitting
			if id != "" && !done {
				cancelled = true
			}
			sigMu.Unlock()

			if id != "" && !done {
				handlerCancelled = true
				fmt.Fprintln(stderr, "\nReceived interrupt, cancelling job...")
				if err := cli.CancelJob(id); err != nil {
					fmt.Fprintf(stderr, "Failed to cancel job: %v\n", err)
					os.Exit(ExitError)
				}
				fmt.Fprintln(stderr, "Job cancellation requested. Press Ctrl+C again to force quit.")
				continue
			}

			if inFlight {
				continue
			}

			if id == "" || handlerCancelled {

				if id == "" {
					tee.record(ExitError, cfg.ServerURL)
				}
				os.Exit(ExitError)
			}
		}
	}()

	fileIDs := make([]string, 0, len(result.InputFiles))
	directPaths := make([]string, 0, len(result.InputFiles))
	for _, inputFile := range result.InputFiles {

		if strings.HasPrefix(inputFile, "file://") {
			filePath, _ := args.StripFileScheme(inputFile)
			absPath, err := args.GetAbsPath(filePath)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error resolving input path %s: %v\n", inputFile, err)
				return ExitError
			}

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

		if strings.Contains(inputFile, "://") {

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

		if err := args.ValidateFileExists(absPath); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			return ExitError
		}

		if sharedFS {

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

	if !quiet {
		fmt.Fprintln(os.Stderr, "Submitting job...")
	}

	outputFilename := ""
	if result.OutputFile != "" {
		outputFilename, err = resolveOutputFilename(result.OutputFile, sharedFS)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error resolving output path %s: %v\n", result.OutputFile, err)
			return ExitError
		}
	}

	if result.StreamingOutput {
		outputFilename = ""
	}

	// In shared FS mode, DirectPath carries the original paths; otherwise it's nil (omitempty)
	var directPathParam []string
	if sharedFS {
		directPathParam = directPaths
	}

	sigMu.Lock()
	submitting = true
	sigMu.Unlock()

	submittedID, err := cli.SubmitJobWithOptions(ctx, fileIDs, directPathParam, result.AllArgs, outputFilename, autoHW, result.StreamingOutput, timeout)

	sigMu.Lock()
	submitting = false
	if err == nil {
		jobID = submittedID
		jobSubmitted = true
	}
	interrupted := ctx.Err() != nil

	shouldCancel := false
	if interrupted && err == nil && !cancelled {
		cancelled = true
		shouldCancel = true
	}
	sigMu.Unlock()

	if err != nil {
		if interrupted {
			fmt.Fprintln(os.Stderr, "Submission cancelled.")
		} else {
			fmt.Fprintf(os.Stderr, "Error submitting job: %v\n", err)
		}
		return ExitError
	}

	if shouldCancel {
		fmt.Fprintln(os.Stderr, "\nReceived interrupt, cancelling job...")
		if cerr := cli.CancelJob(submittedID); cerr != nil {
			fmt.Fprintf(os.Stderr, "Failed to cancel job: %v\n", cerr)
		}
		return ExitError
	}

	tee.stop()

	if !quiet {
		fmt.Fprintf(os.Stderr, "Job submitted: %s\n", jobID)
		fmt.Fprintln(os.Stderr, "Waiting for completion...")
	}

	job, code := waitForJobLoop(cli, jobID, timeout, pollTimeout, result.StreamingOutput, quiet, giveUpCancels)
	if code != ExitSuccess {

		if code != ExitDisconnected {
			removeStaleOutputs(result.OutputFile, result.StreamingOutput, sharedFS, ffmpegArgs)
		}
		return code
	}

	if code := reportTerminalJob(job); code != ExitSuccess {

		removeStaleOutputs(result.OutputFile, result.StreamingOutput, sharedFS, ffmpegArgs)
		return code
	}

	if sharedFS {
		if !quiet {
			fmt.Fprintln(os.Stderr, "Shared FS mode: output files already at local paths, skipping download")
		}
	} else if result.StreamingOutput {

	} else if len(job.OutputFiles) > 0 {
		if code := downloadJobOutputs(cli, job, result.OutputFile, ffmpegArgs, quiet); code != ExitSuccess {
			return code
		}
	} else {
		fmt.Fprintln(os.Stderr, "Warning: no output files returned from server")
	}

	if !quiet {
		fmt.Fprintln(os.Stderr, "Done!")
	}

	return ExitSuccess
}

// outputDownloader is the subset of *client.Client the output download needs.
// It is an interface so tests can serve a scripted file without a server.
type outputDownloader interface {
	DownloadOutput(fileID, outputPath string) error
}

// jobResumeClient is the client surface --resume needs: the wait primitives
// (shared with the submit path) plus the output download.
type jobResumeClient interface {
	jobWaitClient
	outputDownloader
}

// downloadJobOutputs writes a completed job's server-side output files to the
// local target path(s). The caller has already established that the job has
// files to download and that a local target is the right destination.
//
// Each target is guarded by ffmpeg's overwrite semantics before it is written:
// in the default upload/download mode the CLI is the sole writer of the user's
// output file, so a pre-existing file must not be silently truncated. Without
// -y (or with -n), the download refuses with ffmpeg's message and a non-zero
// exit, mirroring the worker's shared-FS guard.
func downloadJobOutputs(cli outputDownloader, job *protocol.JobInfo, outputFile string, ffmpegArgs []string, quiet bool) int {
	for i, outputFileID := range job.OutputFiles {
		outputPath := multiOutputPath(outputFile, i)

		if code := rejectOverwriteIfNeeded(outputPath, ffmpegArgs); code != ExitSuccess {
			return code
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
	return ExitSuccess
}

// runResume re-attaches to a job an earlier CLI run submitted — typically one
// that gave up waiting with ExitDisconnected — and delivers its output. It
// never cancels the job: a client-side give-up here leaves the work running
// server-side, so the operator can attach again later.
func runResume(cli jobResumeClient, cfg *config.Config, opts *Options, tee *stderrTee) int {
	jobID := opts.ResumeJobID
	quiet := opts.Quiet

	job, err := cli.GetJob(jobID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: cannot resume job %s: %v\n", jobID, err)
		return ExitError
	}

	jobSubmitted = true
	tee.stop()

	if job.StreamingOutput {
		fmt.Fprintf(os.Stderr, "Error: job %s streamed its output to stdout; a resumed run cannot replay it\n", jobID)
		return ExitError
	}

	outputFile := opts.ResumeOutput
	if outputFile == "" {

		outputFile = job.OutputFilename
	}
	if outputFile == "" {
		fmt.Fprintf(os.Stderr, "Error: job %s records no output filename; pass the target explicitly: rffmpeg --resume %s <output-path>\n", jobID, jobID)
		return ExitError
	}

	if !pathutil.IsRemoteURL(outputFile) {
		abs, err := filepath.Abs(outputFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: resolving output path %s: %v\n", outputFile, err)
			return ExitError
		}
		outputFile = abs
	}

	jobSharedFS := len(job.DirectPaths) > 0
	if jobSharedFS && opts.ResumeOutput != "" {
		fmt.Fprintf(os.Stderr, "Error: job %s ran in shared filesystem mode; its output is at %s, not on the server\n", jobID, job.OutputFilename)
		return ExitError
	}

	if !jobSharedFS {
		if code := enforceOverwritePolicy(outputFile, false, opts.FmpegArgs); code != ExitSuccess {
			return code
		}
	}

	if !protocol.IsTerminalStatus(job.Status) {
		if !quiet {
			fmt.Fprintf(os.Stderr, "Resuming job %s (status: %s)\n", jobID, job.Status)
			fmt.Fprintln(os.Stderr, "Waiting for completion...")
		}
		var code int
		job, code = waitForJobLoop(cli, jobID, opts.Timeout, pollTimeoutBudget(cfg, opts), false, quiet, giveUpDetaches)
		if code != ExitSuccess {
			return code
		}
	}

	if code := reportTerminalJob(job); code != ExitSuccess {

		removeStaleOutputs(outputFile, false, jobSharedFS, opts.FmpegArgs)
		return code
	}

	if jobSharedFS {
		if !quiet {
			fmt.Fprintf(os.Stderr, "Job %s ran in shared filesystem mode; its output is at %s (nothing to download)\n", jobID, outputFile)
		}
		return ExitSuccess
	}
	if pathutil.IsRemoteURL(outputFile) {
		if !quiet {
			fmt.Fprintf(os.Stderr, "Job %s wrote its output to the network URL %s (nothing to download)\n", jobID, outputFile)
		}
		return ExitSuccess
	}
	if len(job.OutputFiles) == 0 {
		fmt.Fprintf(os.Stderr, "Error: job %s holds no output file on the server; nothing to download\n", jobID)
		return ExitError
	}

	if code := downloadJobOutputs(cli, job, outputFile, opts.FmpegArgs, quiet); code != ExitSuccess {
		return code
	}

	if !quiet {
		fmt.Fprintln(os.Stderr, "Done!")
	}
	return ExitSuccess
}

// overwriteConflictMessage is ffmpeg's own wording for a contradictory -y / -n
// pair. It names no path: the flags contradict each other before any output
// file is opened, so the target's state is irrelevant.
const overwriteConflictMessage = "Error, both -y and -n supplied. Exiting.\n"

// overwriteRefusal returns the ffmpeg-compatible refusal message for a
// pre-existing output file under the given policy, or "" when the policy
// authorizes overwriting it.
func overwriteRefusal(path string, policy ffmpegopts.OverwriteMode) string {
	switch policy {
	case ffmpegopts.OverwriteForce:
		return ""
	case ffmpegopts.OverwriteNever:
		return fmt.Sprintf("File '%s' already exists. Exiting.\n", path)
	case ffmpegopts.OverwriteConflict:
		return overwriteConflictMessage
	default:
		return fmt.Sprintf("File '%s' already exists. Overwrite? [y/N] Not overwriting - exiting\n", path)
	}
}

// enforceOverwritePolicy consumes ffmpeg's -y / -n on the local output target
// before anything is uploaded or submitted. -y proceeds (the download stage
// truncates the target, a shared-FS worker's ffmpeg overwrites natively), -n
// fails immediately, and the default refuses like non-interactive ffmpeg.
// Streaming (-) and remote-URL outputs have no local file to guard — the
// worker's ffmpeg decides there, as it does for ffmpeg's own per-output check,
// which exempts fileless (NOFILE) muxers such as "-f null -".
func enforceOverwritePolicy(outputFile string, streamingOutput bool, ffmpegArgs []string) int {
	if streamingOutput || outputFile == "" || pathutil.IsRemoteURL(outputFile) {
		return ExitSuccess
	}
	policy := ffmpegopts.OverwritePolicy(ffmpegArgs)
	if policy == ffmpegopts.OverwriteForce {
		return ExitSuccess
	}

	if policy == ffmpegopts.OverwriteConflict {
		fmt.Fprint(os.Stderr, overwriteConflictMessage)
		return ExitError
	}
	targets := existingOutputTargets(outputFile)
	if len(targets) == 0 {
		return ExitSuccess
	}
	fmt.Fprint(os.Stderr, overwriteRefusal(targets[0], policy))
	return ExitError
}

// existingOutputTargets returns the local paths the download stage would write
// that already exist: the primary output path plus every consecutive "_N"
// multi-output variant. The download stage writes consecutive indices, so the
// first gap ends the enumeration. Any other stat failure ends it too: an
// unreadable target (EACCES, ELOOP, ENOTDIR, ...) is not a confirmed collision,
// so the download stage reports the real error instead of the CLI guessing here.
func existingOutputTargets(outputFile string) []string {
	var targets []string
	for i := 0; ; i++ {
		path := multiOutputPath(outputFile, i)
		_, err := os.Stat(path)
		if err != nil {
			if i == 0 && os.IsNotExist(err) {

				continue
			}
			return targets
		}
		targets = append(targets, path)
	}
}

// rejectOverwriteIfNeeded enforces ffmpeg's overwrite semantics on a local
// output path before the CLI downloads and writes it. In the default
// upload/download mode the CLI is the only writer of the user's output file, so
// a pre-existing file must not be silently truncated: when the target
// exists and the caller did not pass -y (or passed -n), it refuses with ffmpeg's
// message and returns a non-zero exit, without touching the file.
func rejectOverwriteIfNeeded(outputPath string, ffmpegArgs []string) int {
	policy := ffmpegopts.OverwritePolicy(ffmpegArgs)
	if policy == ffmpegopts.OverwriteForce {
		return ExitSuccess
	}
	if _, err := os.Stat(outputPath); err != nil {
		return ExitSuccess
	}
	fmt.Fprint(os.Stderr, overwriteRefusal(outputPath, policy))
	return ExitError
}

// multiOutputPath returns the local path the download stage writes for the
// i-th output file: the primary path for i == 0, and a "_N"-suffixed variant
// (index inserted before the extension) for i > 0. The cleanup path enumerates
// the same expansion so it removes exactly what the download stage writes.
func multiOutputPath(outputFile string, i int) string {
	if i == 0 {
		return outputFile
	}
	ext := ""
	for j := len(outputFile) - 1; j >= 0; j-- {
		if outputFile[j] == '.' {
			ext = outputFile[j:]
			break
		}
	}
	return outputFile[:len(outputFile)-len(ext)] + "_" + strconv.Itoa(i) + ext
}

// removeStaleOutputs removes stale local output files after a job fails
// without producing output — a terminal failure/timeout/cancellation or a
// client-side give-up. In the default upload/download mode the CLI is the sole
// writer of the user's output files, so files left at the output path(s) are
// stale products from an earlier run and must not masquerade as this run's
// result.
//
// Overwrite semantics mirror the download guard (rejectOverwriteIfNeeded):
// only -y authorizes touching the output path, so a failed job removes the
// stale file only under -y; -n and the default (ask) preserve the user's
// existing file and print a hint. Streaming, shared-FS, and remote-URL outputs
// have no local download path and are left alone.
func removeStaleOutputs(outputFile string, streamingOutput, sharedFS bool, ffmpegArgs []string) {
	if streamingOutput || sharedFS || outputFile == "" || pathutil.IsRemoteURL(outputFile) {
		return
	}
	force := ffmpegopts.OverwritePolicy(ffmpegArgs) == ffmpegopts.OverwriteForce
	for _, path := range existingOutputTargets(outputFile) {
		if !force {
			fmt.Fprintf(os.Stderr, "Output file '%s' left in place (pass -y to overwrite).\n", path)
			continue
		}
		if err := os.Remove(path); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to remove stale output %s: %v\n", path, err)
		}
	}
}

// resolveOutputFilename determines the output filename sent to the server for
// the given ffmpeg output argument. Network URLs (rtmp://, srt://, https://,
// udp://, etc.) pass through unchanged — treating them as local paths would
// corrupt the URL. Local paths (including file:// and paths that merely
// contain "://" mid-string) are not remote: in shared FS mode they are
// resolved to absolute paths, otherwise reduced to their base name for the
// worker's own job directory, which the CLI downloads from afterward.
func resolveOutputFilename(outputFile string, sharedFS bool) (string, error) {
	if pathutil.IsRemoteURL(outputFile) {
		return outputFile, nil
	}
	if sharedFS {
		return args.GetAbsPath(outputFile)
	}
	return filepath.Base(outputFile), nil
}
