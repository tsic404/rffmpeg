package main

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/tsic404/rffmpeg/pkg/cli/args"
	"github.com/tsic404/rffmpeg/pkg/cli/client"
	"github.com/tsic404/rffmpeg/pkg/cli/config"
	"github.com/tsic404/rffmpeg/pkg/pathutil"
)

const (
	ExitSuccess = 0
	ExitError   = 1
	// ExitDisconnected is returned when a job was submitted successfully but
	// the client gave up waiting for it: the server stayed silent past the
	// server-loss budget or the retry budget was spent. The job keeps running
	// server-side; query its final status via GET /api/v1/jobs/{id}. A resumed
	// run that gives up on a still-running job exits with the same code.
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

// jobSubmitted is set once a job is accepted by the server. It separates
// pre-submit failures — where no server-side job exists to query, the target
// of the RFFMPEG_LOG_FILE diagnostic — from post-submit failures.
var jobSubmitted bool

// Mode is the invocation mode. The order in which parseArgs tests these is
// fixed: probe subcommand, then re-attach (job id in the environment), then
// the local answers (help / version), then the capability queries, and
// transcode last.
type Mode int

const (
	ModeTranscode Mode = iota
	ModeProbe
	ModeResume
	ModeHelp
	ModeVersion
	ModeCapability
)

// capabilityQueries are the native single-dash capability-query names. The
// double-dash spellings are deliberately absent: rffmpeg defines no alias of
// its own, so those tokens reach the job verbatim.
var capabilityQueries = []string{
	"-encoders", "-decoders", "-codecs", "-hwaccels", "-filters", "-pix_fmts",
	"-formats", "-buildconf", "-layouts", "-protocols", "-sample_fmts", "-bsfs",
	"-colors",
}

// Options is the resolved invocation: the mode plus the arguments that mode
// consumes. rffmpeg defines no option of its own, so everything that is not a
// mode signal stays in FmpegArgs and reaches the job verbatim.
type Options struct {
	Mode Mode

	// ProbeInput is the probe input identifier (C-18). The accepted output
	// options are validated here and answered with JSON either way, so they
	// carry no state of their own.
	ProbeInput string

	// ResumeJobID re-attaches to an existing job; ResumeOutput is the optional
	// explicit download target (empty = the job's recorded output filename).
	ResumeJobID  string
	ResumeOutput string

	// InfoFlag is the capability-query name that selected ModeCapability.
	InfoFlag string

	// FmpegArgs holds the tokens left for the native tool: the raw command
	// line for transcode, and the -y / -n flags for re-attach.
	FmpegArgs []string
}

// parseArgs resolves the invocation mode in the fixed order and parses only
// the arguments that mode consumes. The mode is decided from tokens before a
// bare "--" separator; everything else is passed through untouched.
func parseArgs(argList []string) (*Options, error) {
	head := argList
	if i := tokenIndex(argList, "--"); i >= 0 {
		head = argList[:i]
	}

	// probe is the only reserved command word, recognised in first position.
	if len(argList) > 0 && argList[0] == "probe" {
		opts := &Options{Mode: ModeProbe}
		return opts, parseProbeArgs(opts, argList[1:])
	}

	// A job id in the environment selects re-attach before any of the local
	// answers: the config file layer never carries it.
	if id := os.Getenv(config.EnvJobID); id != "" {
		opts := &Options{Mode: ModeResume, ResumeJobID: id}
		return opts, parseResumeArgs(opts, argList)
	}

	// -h counts as help only as the sole argument or as the last of exactly
	// two tokens before the separator; -help / -version answer wherever they
	// appear. A post-separator -h is an output candidate, not an option.
	if len(head) == 1 && head[0] == "-h" ||
		len(head) == 2 && head[1] == "-h" {
		return &Options{Mode: ModeHelp}, nil
	}
	for _, arg := range head {
		switch arg {
		case "-help":
			return &Options{Mode: ModeHelp}, nil
		case "-version":
			return &Options{Mode: ModeVersion}, nil
		}
	}

	for _, arg := range head {
		if isCapabilityQuery(arg) {
			return &Options{Mode: ModeCapability, InfoFlag: arg}, nil
		}
	}

	return &Options{Mode: ModeTranscode, FmpegArgs: argList}, nil
}

// tokenIndex returns the index of the first bare "--", or -1.
func tokenIndex(argList []string, want string) int {
	for i, arg := range argList {
		if arg == want {
			return i
		}
	}
	return -1
}

// isCapabilityQuery reports whether tok is one of the native single-dash
// capability-query names.
func isCapabilityQuery(tok string) bool {
	for _, name := range capabilityQueries {
		if tok == name {
			return true
		}
	}
	return false
}

// parseProbeArgs parses the probe acceptance surface (C-18): an input
// identifier (-i <ident> or a single positional), -show_format, -show_streams,
// and a json output format. Everything else is rejected naming the token.
func parseProbeArgs(opts *Options, args []string) error {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch arg {
		case "-i":
			if opts.ProbeInput != "" {
				return fmt.Errorf("duplicate -i flag: input already set to %q", opts.ProbeInput)
			}
			if i+1 >= len(args) {
				return fmt.Errorf("flag -i requires a value")
			}
			val := args[i+1]
			if strings.HasPrefix(val, "-") {
				return fmt.Errorf("flag -i requires a value")
			}
			opts.ProbeInput = val
			i++
		case "-show_format", "-show_streams":
			// Accepted: probe always answers in JSON, which carries both.
		case "-of", "-print_format":
			if i+1 >= len(args) {
				return fmt.Errorf("flag %s requires a value", arg)
			}
			if val := args[i+1]; val != "json" {
				return fmt.Errorf("probe only supports json output format, got %q for %s", val, arg)
			}
			i++
		default:
			if strings.HasPrefix(arg, "-") {
				return fmt.Errorf("unknown probe option: %s", arg)
			}
			if opts.ProbeInput == "" {
				opts.ProbeInput = arg
			} else {
				return fmt.Errorf("unexpected probe argument: %s (input already set to %q)", arg, opts.ProbeInput)
			}
		}
	}
	if opts.ProbeInput == "" {
		return fmt.Errorf("probe requires an input: pass it as -i <file|URL> or as the only positional argument")
	}
	return nil
}

// parseResumeArgs collects the arguments of a re-attach run: at most one
// output path plus ffmpeg's -y / -n overwrite flags. Any other argument is
// rejected rather than dropped — a re-attach never re-runs ffmpeg, so ignoring
// an ffmpeg option would misrepresent what the recorded job actually did.
func parseResumeArgs(opts *Options, args []string) error {
	for _, arg := range args {
		switch {
		case arg == "probe":
			return fmt.Errorf("%q is rffmpeg's subcommand; unset %s to run it (use ./%s for a file of that name)", arg, config.EnvJobID, arg)
		case arg == "-y" || arg == "-n":
			opts.FmpegArgs = append(opts.FmpegArgs, arg)
		case isStdoutTarget(arg):
			return fmt.Errorf("re-attach writes the job's output to a file; %q (stdout) is not supported", arg)
		case strings.HasPrefix(arg, "-"):
			return fmt.Errorf("re-attach accepts only an optional output path and -y/-n, got %q", arg)
		case opts.ResumeOutput != "":
			return fmt.Errorf("re-attach accepts at most one output path, got %q and %q", opts.ResumeOutput, arg)
		case pathutil.IsRemoteURL(arg):
			return fmt.Errorf("re-attach downloads to a local file; %q is a remote URL", arg)
		default:
			opts.ResumeOutput = arg
		}
	}
	return nil
}

// isStdoutTarget reports whether a re-attach target names stdout, using the
// submit path's own predicate so both surfaces refuse exactly the same
// spellings. A resumed job's byte stream cannot be replayed, so a stdout
// target is refused rather than written out as a file literally named "pipe:".
func isStdoutTarget(arg string) bool {
	return arg == "-" || args.IsStdoutPipeAlias(arg)
}

func main() {
	os.Exit(run())
}

func run() (code int) {
	jobSubmitted = false

	opts, err := parseArgs(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return ExitError
	}

	if opts.Mode == ModeHelp {
		printUsage()
		return ExitSuccess
	}
	if opts.Mode == ModeVersion {
		fmt.Printf("rffmpeg %s\n", version)
		return ExitSuccess
	}

	// No ffmpeg arguments and no other mode signal: print the help and exit
	// non-zero, like the native tool.
	if opts.Mode == ModeTranscode && len(opts.FmpegArgs) == 0 {
		printUsage()
		return ExitError
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error loading config: %v\n", err)
		return ExitError
	}

	tee := startStderrTee(cfg.LogFile, cfg.Token)
	serverURL := cfg.ServerURL
	defer func() {
		tee.stop()
		if code != ExitSuccess && !jobSubmitted {
			tee.record(code, serverURL)
		}
	}()

	if !cfg.Quiet {
		fmt.Fprintf(os.Stderr, "rffmpeg %s - Remote FFmpeg Client\n", version)
		fmt.Fprintf(os.Stderr, "Server: %s (%s)\n", cfg.ServerURL, cfg.ServerURLSource)
	}
	if warning := plaintextRemoteServerWarning(cfg.ServerURL); warning != "" {
		fmt.Fprintln(os.Stderr, warning)
	}

	if opts.Mode == ModeCapability {
		return runCapabilityQuery(cfg, opts.InfoFlag)
	}

	// Parsing rejections happen before any submission (and before the health
	// probe): no job is created and no output file is written.
	var parsed *args.ParseResult
	if opts.Mode == ModeTranscode {
		parsed, err = args.NewParser().Parse(opts.FmpegArgs)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error parsing arguments: %v\n", err)
			return ExitError
		}
		if msg := args.StreamingOutputError(parsed); msg != "" {
			fmt.Fprintln(os.Stderr, msg)
			return ExitError
		}
		if msg := args.OutputExtensionWarning(parsed); msg != "" {
			fmt.Fprintln(os.Stderr, msg)
		}
	}

	cli := client.New(cfg.ServerURL, cfg.Token,
		client.WithMaxRetries(maxRetriesBudget(cfg)),
		client.WithServerLossTimeout(serverLossTimeoutBudget(cfg)),
		client.WithSubmitRetries(submitRetriesBudget(cfg)))

	if err := cli.HealthCheck(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: server health check failed: %v\n", err)
		if hint := unconfiguredServerURLHint(cfg); hint != "" {
			fmt.Fprintln(os.Stderr, hint)
		}
		return ExitError
	}

	switch opts.Mode {
	case ModeProbe:
		return runProbe(cli, opts.ProbeInput, cfg.Quiet, cfg.IsSharedFS())
	case ModeResume:
		return runResume(cli, cfg, opts, tee)
	default:
		return runTranscode(cli, cfg, parsed, tee)
	}
}

// plaintextRemoteServerWarning returns a one-line warning when the CLI would
// send its auth token and media to a non-loopback server over plaintext HTTP.
// Loopback endpoints (including a trailing-dot "localhost.") do not leave the
// host and stay silent. The message names only the host: echoing the raw URL
// would print any userinfo embedded in it.
func plaintextRemoteServerWarning(serverURL string) string {
	u, err := url.Parse(strings.TrimSpace(serverURL))
	if err != nil || !strings.EqualFold(u.Scheme, "http") {
		return ""
	}
	host := strings.TrimSuffix(u.Hostname(), ".")
	if host == "" || strings.EqualFold(host, "localhost") {
		return ""
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return ""
	}
	return fmt.Sprintf("Warning: server host %s uses plaintext HTTP; the auth token and media are sent unencrypted. Use https:// for a remote server.", host)
}

// unconfiguredServerURLHint is the one line printed after a failed health check
// when nothing configured a server URL, so the CLI fell back to the built-in
// default. That is the first-run case: without the hint the operator only sees
// a bare "connection refused" and cannot tell which URL was even tried. Any
// configured URL (environment or config file) returns "" — the choice was
// deliberate and the banner already names its source.
func unconfiguredServerURLHint(cfg *config.Config) string {
	if cfg.ServerURLSource != config.ServerURLSourceDefault {
		return ""
	}
	return fmt.Sprintf("Hint: no server URL configured, using the default %s; set %s or add a config file.", config.DefaultServerURL, config.EnvServerURL)
}

// maxRetriesBudget resolves the retry budget for the wait loops.
func maxRetriesBudget(cfg *config.Config) int {
	if cfg.MaxRetries != nil {
		return *cfg.MaxRetries
	}
	return client.DefaultMaxRetries
}

// serverLossTimeoutBudget resolves the post-submit contact-loss cap.
func serverLossTimeoutBudget(cfg *config.Config) time.Duration {
	if cfg.ServerLossTimeout != nil {
		return time.Duration(*cfg.ServerLossTimeout)
	}
	return client.DefaultServerLossTimeout
}

// submitRetriesBudget resolves the rate-limit resubmission budget: off by
// default, on only when the config plane enabled retries.
func submitRetriesBudget(cfg *config.Config) int {
	if cfg.Retry {
		return client.DefaultSubmitRetries
	}
	return 0
}

// jobTimeoutBudget resolves the configured job execution budget (C-13). Zero
// means "not configured": the field is not sent and the server's own job
// timeout governs.
func jobTimeoutBudget(cfg *config.Config) time.Duration {
	if cfg.JobTimeout != nil {
		return time.Duration(*cfg.JobTimeout)
	}
	return 0
}

// pollTimeoutBudget resolves the client-side poll cap. 0 means "no cap".
func pollTimeoutBudget(cfg *config.Config) time.Duration {
	if cfg.PollTimeout != nil {
		return time.Duration(*cfg.PollTimeout)
	}
	return config.DefaultPollTimeout
}

func printUsage() {
	fmt.Fprintf(os.Stderr, `rffmpeg - Remote FFmpeg Client
Usage:
  rffmpeg -i <input> [ffmpeg options] <output>    Transcode (native ffmpeg syntax)
  rffmpeg probe <file|URL> [-i <file|URL>] [-show_streams] [-show_format] [-of json]
  rffmpeg -h | -help | -version                   Local answer, no server contact
  rffmpeg -encoders | -decoders | -codecs | -hwaccels | -filters
          -pix_fmts | -formats | -buildconf | -layouts | -protocols
          -sample_fmts | -bsfs | -colors              Capability queries

rffmpeg accepts the same arguments as the native tool and defines no option of
its own. Every token that is not one of the modes above reaches the remote
ffmpeg verbatim; rffmpeg only interprets the input (-i) and the single output
target, and rejects a call with more than one output candidate.

Re-attach to an existing job (never re-runs ffmpeg):
  RFFMPEG_JOB_ID=<job-id> rffmpeg [output-path] [-y|-n]
  Downloads the job's output (default: its recorded output filename) without
  submitting a new job or cancelling the original one.

Configuration (no rffmpeg option on the command line):
  Resolved in this order, each layer overriding the keys it sets:
    1. environment variables
    2. ./`+config.ConfigFileName+` (current directory)
    3. ~/.`+config.ConfigFileName+` (user home)
    4. `+config.SystemConfigPath+` (system)
    5. built-in defaults
  The current-directory layer is used only when the file is a regular file
  owned by the running user and not writable by group or others; otherwise it
  is skipped with a warning that quiet mode does not suppress.

  Environment variables (config file key in parentheses):
`)
	for _, env := range configPlaneDoc {
		fmt.Fprintf(os.Stderr, "    %-27s %s\n", env[0], env[1])
	}
	fmt.Fprintf(os.Stderr, `
ffmpeg options:
  All native options are supported and passed through to the worker.
  -y / -n are additionally enforced by rffmpeg on the local output path before
  the job is submitted: -y overwrites, -n fails immediately with "already
  exists", and the default refuses with ffmpeg's "Not overwriting - exiting".
  Streaming (-) and remote-URL outputs are unaffected.

Examples:
  # Transcode on the cluster
  rffmpeg -i input.mp4 -c:v libx264 output.mp4

  # Remote server through the configuration plane
  RFFMPEG_SERVER_URL=https://rffmpeg.example.com rffmpeg -i video.mkv -c:a aac audio.mp4

  # Quiet run, hardware-encoder upgrade and a 30m ffmpeg budget
  RFFMPEG_QUIET=1 RFFMPEG_AUTO_HW=1 RFFMPEG_JOB_TIMEOUT=30m rffmpeg -i in.mp4 -c:v libx264 out.mp4

  # Re-attach to a job an earlier run gave up on (exit code 2)
  RFFMPEG_JOB_ID=3f2a9c1e-4b7d-4f6a-9c21-8d0e5b7a1234 rffmpeg -y

  # Capability queries
  rffmpeg -encoders
  RFFMPEG_INFO_FORMAT=json rffmpeg -filters
`)
}

// configPlaneDoc is the environment-variable table printed by the help: the
// variable, its meaning and the config file key it overrides.
var configPlaneDoc = [][2]string{
	{config.EnvServerURL, "Server URL (server_url); default " + config.DefaultServerURL},
	{config.EnvToken, "Auth token (token)"},
	{config.EnvQuiet, "Quiet: no banner, upload progress or submit lines (quiet)"},
	{config.EnvAutoHW, "Upgrade a software encoder to the worker's hardware encoder (auto_hw)"},
	{config.EnvRetry, "Retry rate-limited (429) submissions with backoff (retry)"},
	{config.EnvJobTimeout, "ffmpeg execution budget, e.g. 30m (job_timeout)"},
	{config.EnvPollTimeout, "Wait-for-worker cap, default 10m; 0 = no cap (poll_timeout)"},
	{config.EnvMaxRetries, "WS reconnect / HTTP poll retry budget, default 14 (max_retries)"},
	{config.EnvServerLossTimeout, "Wait cap once the server stops responding, default 30s (server_loss_timeout)"},
	{config.EnvInfoFormat, "Capability-query output format: text or json (info_format)"},
	{config.EnvLocalFallback, "Capability queries fall back to the LOCAL ffmpeg (local_fallback)"},
	{config.EnvSharedFS, "Shared filesystem mode: skip upload/download (shared_fs)"},
	{config.EnvLogFile, "Diagnostic log for pre-submit failures (log_file)"},
	{config.EnvJobID, "Re-attach to this job id instead of submitting (environment only)"},
}
