package main

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/tsic404/rffmpeg/pkg/cli/client"
	"github.com/tsic404/rffmpeg/pkg/cli/config"
	"github.com/tsic404/rffmpeg/pkg/pathutil"
)

const (
	ExitSuccess = 0
	ExitError   = 1
	// ExitDisconnected is returned when a job was submitted successfully but
	// the client gave up waiting for it: the server stayed silent past the
	// server-loss budget (--server-loss-timeout, default 30s) or the retry
	// budget was spent. The job keeps running server-side; query its final
	// status via GET /api/v1/jobs/{id}. Distinct from ExitError, which also
	// covers submission-phase failures where no job exists to query.
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

// Options holds the rffmpeg-specific options extracted from the command line.
type Options struct {
	ServerURL      string
	Token          string
	Quiet          bool
	ShowHelp       bool
	ShowVersion    bool
	AutoHW         bool
	IsProbe        bool
	ProbeInput     string
	Timeout        time.Duration
	PollTimeout    time.Duration
	PollTimeoutSet bool
	MaxRetries     int
	MaxRetriesSet  bool
	// ServerLossTimeout is the post-submit contact-loss budget; a zero value
	// is meaningful (disable the cap), so Set records whether the flag was
	// actually present.
	ServerLossTimeout    time.Duration
	ServerLossTimeoutSet bool
	Retry                bool
	// ResumeJobID re-attaches to a job an earlier run submitted instead of
	// creating a new one; ResumeOutput is the optional explicit download target
	// (empty = the job's recorded output filename).
	ResumeJobID  string
	ResumeOutput string
	FmpegArgs    []string

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

			if opts.ResumeJobID != "" {
				return nil, errors.New("--resume does not accept a '--' separator: pass the output path and -y/-n directly")
			}

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
		case "--poll-timeout", "-poll-timeout":
			val, err := needValue(i, arg)
			if err != nil {
				return nil, err
			}
			parsed, perr := time.ParseDuration(val)
			if perr != nil {
				return nil, fmt.Errorf("invalid poll-timeout value: %s (use format like 30s, 5m, 2h, or 0 to disable)", val)
			}
			if parsed < 0 {
				return nil, fmt.Errorf("poll-timeout must be non-negative: %s", val)
			}
			warnDuplicate(arg)
			opts.PollTimeout = parsed
			opts.PollTimeoutSet = true
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
		case "--server-loss-timeout", "-server-loss-timeout":
			val, err := needValue(i, arg)
			if err != nil {
				return nil, err
			}
			parsed, perr := time.ParseDuration(val)
			if perr != nil {
				return nil, fmt.Errorf("invalid server-loss-timeout value: %s (use format like 5s, 30s, 2m, or 0 to disable)", val)
			}
			if parsed < 0 {
				return nil, fmt.Errorf("server-loss-timeout must be non-negative: %s", val)
			}
			warnDuplicate(arg)
			opts.ServerLossTimeout = parsed
			opts.ServerLossTimeoutSet = true
			i++
		case "--retry", "-retry":
			opts.Retry = true

		case "--resume", "-resume":
			val, err := needValue(i, arg)
			if err != nil {
				return nil, err
			}

			if val == "" || strings.HasPrefix(val, "-") {
				return nil, fmt.Errorf("flag %s requires a job id, got %q", arg, val)
			}
			warnDuplicate(arg)
			opts.ResumeJobID = val
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

			if len(argList) == 1 || (len(argList) == 2 && argList[len(argList)-1] == "-h") {
				opts.ShowHelp = true
			} else {
				positionalArgs = append(positionalArgs, arg)
			}
		default:

			positionalArgs = append(positionalArgs, arg)
		}
	}

	if len(positionalArgs) > 0 && positionalArgs[0] == "probe" {

		if opts.ResumeJobID != "" {
			return nil, errors.New("--resume cannot be combined with the probe subcommand")
		}
		opts.IsProbe = true
		positionalArgs = positionalArgs[1:]

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
	} else if opts.ResumeJobID != "" {

		if err := parseResumeArgs(opts, positionalArgs); err != nil {
			return nil, err
		}
	} else {

		opts.FmpegArgs = append(opts.FmpegArgs, positionalArgs...)
	}

	if opts.ResumeJobID != "" {
		if name := opts.infoFlagName(); name != "" {
			return nil, fmt.Errorf("--resume cannot be combined with %s", name)
		}
	}

	return opts, nil
}

// parseResumeArgs collects the arguments that follow --resume: at most one
// output path plus ffmpeg's -y / -n overwrite flags. Any other argument is
// rejected rather than dropped — --resume never re-runs ffmpeg, so ignoring an
// ffmpeg option would misrepresent what the recorded job actually did. A bare
// "probe" is refused too: it names the subcommand, not an output path.
func parseResumeArgs(opts *Options, args []string) error {
	for _, arg := range args {
		switch {
		case arg == "probe":

			return fmt.Errorf("%q is rffmpeg's subcommand; run it without --resume (use ./%s for a file of that name)", arg, arg)
		case arg == "-y" || arg == "-n":

			opts.FmpegArgs = append(opts.FmpegArgs, arg)
		case isStdoutTarget(arg):
			return fmt.Errorf("--resume writes the job's output to a file; %q (stdout) is not supported", arg)
		case strings.HasPrefix(arg, "-"):
			return fmt.Errorf("--resume accepts only an optional output path and -y/-n, got %q", arg)
		case opts.ResumeOutput != "":
			return fmt.Errorf("--resume accepts at most one output path, got %q and %q", opts.ResumeOutput, arg)
		case pathutil.IsRemoteURL(arg):
			return fmt.Errorf("--resume downloads to a local file; %q is a remote URL", arg)
		default:
			opts.ResumeOutput = arg
		}
	}
	return nil
}

// isStdoutTarget reports whether an --resume target names stdout, mirroring the
// submit path's normalization of the output token: ffmpeg treats "-" and
// "pipe:1" as the same stream, and matches protocol names without regard to
// case. A resumed job's byte stream cannot be replayed, so both spellings are
// refused rather than written out as a file literally named "pipe:1".
func isStdoutTarget(arg string) bool {
	return arg == "-" || strings.EqualFold(arg, "pipe:1")
}

// infoFlagName returns the set capability-query flag (-encoders and friends),
// or "" when none is set.
func (o *Options) infoFlagName() string {
	for _, flag := range []struct {
		name string
		set  bool
	}{
		{"-encoders", o.ShowEncoders},
		{"-decoders", o.ShowDecoders},
		{"-codecs", o.ShowCodecs},
		{"-hwaccels", o.ShowHwaccels},
		{"-filters", o.ShowFilters},
		{"-pix_fmts", o.ShowPixFmts},
		{"-formats", o.ShowFormats},
		{"-buildconf", o.ShowBuildconf},
		{"-layouts", o.ShowLayouts},
		{"-protocols", o.ShowProtocols},
		{"-sample_fmts", o.ShowSampleFmts},
		{"-bsfs", o.ShowBsfs},
		{"-colors", o.ShowColors},
	} {
		if flag.set {
			return flag.name
		}
	}
	return ""
}

func main() {
	os.Exit(run())
}

func run() (code int) {

	tee := startStderrTee()

	jobSubmitted = false

	serverURL := config.DefaultServerURL
	defer func() {
		tee.stop()
		if code != ExitSuccess && !jobSubmitted {
			tee.record(code, serverURL)
		}
	}()

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

	if opts.ShowFilters {
		return runFilters(opts.ServerURL, opts.Token, opts.ShowJSON)
	}
	if opts.ShowPixFmts {
		return runPixFmts(opts.ServerURL, opts.Token, opts.ShowJSON)
	}
	if opts.ShowFormats {
		return runFormats(opts.ServerURL, opts.Token, opts.ShowJSON)
	}

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

	if !opts.IsProbe && opts.ResumeJobID == "" && len(ffmpegArgs) == 0 {
		printUsage()
		return ExitError
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error loading config: %v\n", err)
		return ExitError
	}

	if opts.ServerURL != "" {
		cfg.ServerURL = opts.ServerURL
		cfg.ServerURLSource = config.ServerURLSourceFlag
	}
	if opts.Token != "" {
		cfg.Token = opts.Token
	}
	serverURL = cfg.ServerURL
	maxRetries := client.DefaultMaxRetries
	if opts.MaxRetriesSet {
		maxRetries = opts.MaxRetries
	} else if cfg.MaxRetries != nil {
		maxRetries = *cfg.MaxRetries
	}
	serverLossTimeout := client.DefaultServerLossTimeout
	if cfg.ServerLossTimeout != nil {
		serverLossTimeout = time.Duration(*cfg.ServerLossTimeout)
	}
	if opts.ServerLossTimeoutSet {
		serverLossTimeout = opts.ServerLossTimeout
	}

	sharedFS := cfg.IsSharedFS()
	if sharedFS && !opts.Quiet {
		fmt.Fprintln(os.Stderr, "Shared filesystem mode enabled: skipping upload/download")
	}

	submitRetries := 0
	if opts.Retry {
		submitRetries = client.DefaultSubmitRetries
	}
	cli := client.New(cfg.ServerURL, cfg.Token,
		client.WithMaxRetries(maxRetries),
		client.WithServerLossTimeout(serverLossTimeout),
		client.WithSubmitRetries(submitRetries))

	if !opts.Quiet {
		fmt.Fprintf(os.Stderr, "rffmpeg %s - Remote FFmpeg Client\n", version)
		fmt.Fprintf(os.Stderr, "Server: %s (%s)\n", cfg.ServerURL, cfg.ServerURLSource)
	}
	if warning := plaintextRemoteServerWarning(cfg.ServerURL); warning != "" {
		fmt.Fprintln(os.Stderr, warning)
	}

	if err := cli.HealthCheck(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: server health check failed: %v\n", err)
		if hint := unconfiguredServerURLHint(cfg); hint != "" {
			fmt.Fprintln(os.Stderr, hint)
		}
		return ExitError
	}

	if opts.IsProbe {
		return runProbe(cli, opts.ProbeInput, opts.Quiet, sharedFS)
	}

	if opts.ResumeJobID != "" {
		return runResume(cli, cfg, opts, tee)
	}

	return runTranscode(cli, cfg, opts, ffmpegArgs, sharedFS, tee)
}

// plaintextRemoteServerWarning returns a one-line warning when the CLI would
// send its auth token and media to a non-loopback server over plaintext HTTP.
// Loopback endpoints do not leave the host and stay silent.
func plaintextRemoteServerWarning(serverURL string) string {
	u, err := url.Parse(strings.TrimSpace(serverURL))
	if err != nil || !strings.EqualFold(u.Scheme, "http") {
		return ""
	}
	host := u.Hostname()
	if host == "" || strings.EqualFold(host, "localhost") {
		return ""
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return ""
	}
	return fmt.Sprintf("Warning: server %s uses plaintext HTTP; the auth token and media are sent unencrypted. Use https:// for a remote server.", serverURL)
}

// unconfiguredServerURLHint is the one line printed after a failed health check
// when nothing configured a server URL, so the CLI fell back to the built-in
// default. That is the first-run case: without the hint the operator only sees
// a bare "connection refused" and cannot tell which URL was even tried. Any
// explicit URL (--server, RFFMPEG_SERVER_URL, config file) returns "" — the
// choice was deliberate and the banner already names its source.
func unconfiguredServerURLHint(cfg *config.Config) string {
	if cfg.ServerURLSource != config.ServerURLSourceDefault {
		return ""
	}
	return fmt.Sprintf("Hint: no server URL configured, using the default %s; set RFFMPEG_SERVER_URL or pass --server <url>.", config.DefaultServerURL)
}

// pollTimeoutBudget resolves the client-side poll cap, flag > env/config >
// default. 0 means "no cap" (opt out). config.Load already folds
// RFFMPEG_POLL_TIMEOUT and rffmpeg.json's "poll_timeout" into cfg.PollTimeout
// (nil = unset).
func pollTimeoutBudget(cfg *config.Config, opts *Options) time.Duration {
	if opts.PollTimeoutSet {
		return opts.PollTimeout
	}
	if cfg.PollTimeout != nil {
		return time.Duration(*cfg.PollTimeout)
	}
	return config.DefaultPollTimeout
}

func printUsage() {
	fmt.Fprintf(os.Stderr, `rffmpeg - Remote FFmpeg Client
Usage:
  rffmpeg [rffmpeg_options] [ffmpeg_options]        Run ffmpeg transcoding
  rffmpeg --resume <job-id> [output-path] [-y|-n]    Re-attach to an existing job and download its output
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
  Capability queries (-encoders/-decoders/-codecs/-hwaccels/-filters/-pix_fmts/-formats)
  describe the cluster, so they fail when the server cannot answer instead of
  silently listing this machine's ffmpeg; RFFMPEG_LOCAL_FALLBACK=1 opts into the
  local list (announced on stderr).
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
  --poll-timeout DURATION Max time to poll a waiting-for-worker (pending) job (default: 10m; 0 = no cap)
  --max-retries N         Max WS reconnect attempts / HTTP poll retry budget (default: 14, ~5 min; 0 = no retries)
  --server-loss-timeout D Max wait for a submitted job once the server stops responding (default: 30s; 0 = no cap)
  --retry                 Retry job submission on rate-limit (429) with exponential backoff (default: off)
  --resume JOB_ID         Re-attach to a job an earlier run submitted and download its output.
                          Never cancels the job; --timeout/--poll-timeout bound the wait, and
                          the optional output-path overrides the job's recorded filename.

ffmpeg options:
  All standard ffmpeg options are supported and passed through to the server.
  rffmpeg options and ffmpeg options can be mixed freely.

  -y / -n are additionally enforced by rffmpeg on the local output path before
  the job is submitted (default and shared-FS modes alike): -y overwrites the
  existing file without prompting, -n fails immediately with "already exists",
  and the default refuses with ffmpeg's "Not overwriting - exiting" message.
  Passing both fails with ffmpeg's "both -y and -n supplied" error, whether or
  not the target exists. Streaming (-) and remote-URL outputs are unaffected.

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

  # Re-attach to a job an earlier run gave up on (exit code 2) and fetch its output
  rffmpeg --resume 3f2a9c1e-... -y
  rffmpeg --resume 3f2a9c1e-... /data/output.mp4 -y

Configuration:
  Config file locations (in order of priority):
    ./rffmpeg.json
    ~/.rffmpeg.json
    /etc/rffmpeg.json

  Environment variables:
    RFFMPEG_SERVER_URL  Server URL
    RFFMPEG_TOKEN       Auth token
    RFFMPEG_SHARED_FS   Enable shared filesystem mode (set to 1 or true)
    RFFMPEG_LOCAL_FALLBACK  Allow capability queries to answer from local ffmpeg
                        when the server cannot (set to 1 or true; default: fail)

`)
}
