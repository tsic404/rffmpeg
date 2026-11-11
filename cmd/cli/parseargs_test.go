package main

import (
	"os"
	"strings"
	"testing"

	"github.com/tsic404/rffmpeg/pkg/cli/config"
)

// isolateParseEnv blanks every configuration-plane variable so a parsing case
// is decided by its arguments alone.
func isolateParseEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		config.EnvServerURL, config.EnvToken, config.EnvSharedFS, config.EnvQuiet,
		config.EnvAutoHW, config.EnvRetry, config.EnvJobTimeout, config.EnvPollTimeout,
		config.EnvMaxRetries, config.EnvServerLossTimeout, config.EnvInfoFormat,
		config.EnvLocalFallback, config.EnvLogFile, config.EnvJobID,
	} {
		t.Setenv(name, "")
	}
}

// TestParseArgs_ModeOrder pins the fixed mode order (C-6): the probe word
// first, then a job id from the environment, then the local answers, then the
// capability queries, and transcode last. A token a later stage would have
// consumed must not change an earlier stage's decision.
func TestParseArgs_ModeOrder(t *testing.T) {
	cases := []struct {
		name string
		args []string
		job  string
		want Mode
	}{
		{"probe first", []string{"probe", "in.mp4"}, "", ModeProbe},
		{"probe wins over -h", []string{"probe", "-h"}, "", ModeProbe},
		{"probe wins over a query", []string{"probe", "-encoders"}, "", ModeProbe},
		{"probe wins over a job id", []string{"probe", "in.mp4"}, "job-1", ModeProbe},
		{"job id before -h", []string{"-h"}, "job-1", ModeResume},
		{"job id before a query", []string{"-encoders"}, "job-1", ModeResume},
		{"job id before transcode", []string{"-i", "in.mp4", "out.mp4"}, "job-1", ModeResume},
		{"-h alone", []string{"-h"}, "", ModeHelp},
		{"-h last of two", []string{"in.mp4", "-h"}, "", ModeHelp},
		{"-help anywhere", []string{"-i", "in.mp4", "-help"}, "", ModeHelp},
		{"-help beats a query", []string{"-encoders", "-help"}, "", ModeHelp},
		{"-version anywhere", []string{"-i", "in.mp4", "-version"}, "", ModeVersion},
		{"-version beats a query", []string{"-filters", "-version"}, "", ModeVersion},
		{"query", []string{"-encoders"}, "", ModeCapability},
		{"query after ffmpeg tokens", []string{"-hide_banner", "-pix_fmts"}, "", ModeCapability},
		{"transcode", []string{"-i", "in.mp4", "out.mp4"}, "", ModeTranscode},
		{"-h after the separator", []string{"-i", "in.mp4", "--", "-h"}, "", ModeTranscode},
		{"-help after the separator", []string{"-i", "in.mp4", "--", "-help"}, "", ModeTranscode},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolateParseEnv(t)
			if tc.job != "" {
				t.Setenv(config.EnvJobID, tc.job)
			}
			// The selected mode's own argument parser may reject its
			// arguments (probe with -h, re-attach with -i); the mode decision
			// is what this test pins, so an error is only fatal for the modes
			// that must accept their argument list.
			opts, _ := parseArgs(tc.args)
			if opts == nil {
				t.Fatalf("parseArgs(%v) returned no options", tc.args)
			}
			if opts.Mode != tc.want {
				t.Errorf("parseArgs(%v).Mode = %v, want %v", tc.args, opts.Mode, tc.want)
			}
		})
	}
}

// TestParseArgs_ModeSignalsAfterSeparator pins C-6's "only tokens before --":
// a mode signal after the native separator neither selects that mode nor
// disappears from the argument list.
func TestParseArgs_ModeSignalsAfterSeparator(t *testing.T) {
	isolateParseEnv(t)
	args := []string{"-i", "in.mp4", "--", "-encoders"}
	opts, err := parseArgs(args)
	if err != nil {
		t.Fatalf("parseArgs(%v) error = %v", args, err)
	}
	if opts.Mode != ModeTranscode {
		t.Errorf("Mode = %v, want ModeTranscode: %q sits after the separator", opts.Mode, "-encoders")
	}
	if got := strings.Join(opts.FmpegArgs, " "); got != strings.Join(args, " ") {
		t.Errorf("FmpegArgs = %q, want the command line verbatim %q", got, strings.Join(args, " "))
	}
}

// TestParseArgs_FormerPrivateOptionsPassThrough pins C-6 / C-17 for the
// invocation shape: rffmpeg defines no option of its own, so every name it used
// to interpret now reaches the native tool as an ordinary token. None may select
// a mode, consume a value, or vanish from the argument list.
func TestParseArgs_FormerPrivateOptionsPassThrough(t *testing.T) {
	former := [][]string{
		{"--server", "http://other:9000"},
		{"-server", "http://other:9000"},
		{"--token", "SECRET"},
		{"-token", "SECRET"},
		{"--token=SECRET"},
		{"--quiet"},
		{"-quiet"},
		{"-q", "2"},
		{"--auto-hw"},
		{"--auto-hw=false"},
		{"-auto-hw=true"},
		{"--timeout", "30m"},
		{"--poll-timeout", "20m"},
		{"--max-retries", "5"},
		{"--server-loss-timeout", "15s"},
		{"--retry"},
		{"-retry"},
		{"--json"},
		{"--resume", "job-1"},
		{"-resume", "job-1"},
		{"--encoders"},
		{"--filters"},
		{"--help"},
		{"--version"},
		{"-show_streams"},
	}

	for _, extra := range former {
		t.Run(strings.Join(extra, " "), func(t *testing.T) {
			isolateParseEnv(t)
			args := append([]string{"-i", "in.mp4"}, extra...)
			args = append(args, "out.mp4")

			opts, err := parseArgs(args)
			if err != nil {
				t.Fatalf("parseArgs(%v) error = %v, want it passed through", args, err)
			}
			if opts.Mode != ModeTranscode {
				t.Errorf("Mode = %v, want ModeTranscode: %q is not an rffmpeg option", opts.Mode, strings.Join(extra, " "))
			}
			if got, want := strings.Join(opts.FmpegArgs, " "), strings.Join(args, " "); got != want {
				t.Errorf("FmpegArgs = %q, want %q", got, want)
			}
		})
	}
}

// TestParseArgs_CapabilityNames pins C-16's name set: the thirteen native
// single-dash names select the query mode, and the double-dash spellings do not.
func TestParseArgs_CapabilityNames(t *testing.T) {
	native := []string{
		"-encoders", "-decoders", "-codecs", "-hwaccels", "-filters", "-pix_fmts",
		"-formats", "-buildconf", "-layouts", "-protocols", "-sample_fmts", "-bsfs",
		"-colors",
	}
	for _, name := range native {
		t.Run(name, func(t *testing.T) {
			isolateParseEnv(t)
			opts, err := parseArgs([]string{name})
			if err != nil {
				t.Fatalf("parseArgs(%s) error = %v", name, err)
			}
			if opts.Mode != ModeCapability || opts.InfoFlag != name {
				t.Errorf("parseArgs(%s) = %v/%q, want ModeCapability/%q", name, opts.Mode, opts.InfoFlag, name)
			}
		})
	}

	for _, name := range native {
		t.Run("double-dash "+name, func(t *testing.T) {
			isolateParseEnv(t)
			opts, err := parseArgs([]string{"-" + name})
			if err != nil {
				t.Fatalf("parseArgs(-%s) error = %v", name, err)
			}
			if opts.Mode != ModeTranscode {
				t.Errorf("Mode = %v for the double-dash spelling %q, want ModeTranscode", opts.Mode, "-"+name)
			}
		})
	}
}

// TestParseArgs_NoArgs pins C-7's empty invocation: no mode signal and no
// arguments leaves the transcode mode with nothing to run, which the caller
// answers with the help text and a non-zero exit.
func TestParseArgs_NoArgs(t *testing.T) {
	isolateParseEnv(t)
	opts, err := parseArgs(nil)
	if err != nil {
		t.Fatalf("parseArgs(nil) error = %v", err)
	}
	if opts.Mode != ModeTranscode || len(opts.FmpegArgs) != 0 {
		t.Errorf("parseArgs(nil) = %v/%v, want ModeTranscode with no arguments", opts.Mode, opts.FmpegArgs)
	}
}

// TestParseProbeArgs covers C-18's probe acceptance surface: one input
// identifier, the two -show_* switches, and a json output format; everything
// else is rejected naming the token.
func TestParseProbeArgs(t *testing.T) {
	isolateParseEnv(t)

	accept := []struct {
		name  string
		args  []string
		input string
	}{
		{"positional", []string{"probe", "in.mp4"}, "in.mp4"},
		{"-i", []string{"probe", "-i", "in.mp4"}, "in.mp4"},
		{"show_streams", []string{"probe", "in.mp4", "-show_streams"}, "in.mp4"},
		{"show_format", []string{"probe", "-show_format", "in.mp4"}, "in.mp4"},
		{"of json", []string{"probe", "in.mp4", "-of", "json"}, "in.mp4"},
		{"print_format json", []string{"probe", "in.mp4", "-print_format", "json"}, "in.mp4"},
		{"remote URL", []string{"probe", "https://example.test/a.mp4"}, "https://example.test/a.mp4"},
	}
	for _, tc := range accept {
		t.Run(tc.name, func(t *testing.T) {
			opts, err := parseArgs(tc.args)
			if err != nil {
				t.Fatalf("parseArgs(%v) error = %v", tc.args, err)
			}
			if opts.ProbeInput != tc.input {
				t.Errorf("ProbeInput = %q, want %q", opts.ProbeInput, tc.input)
			}
		})
	}

	reject := []struct {
		name string
		args []string
		want string
	}{
		{"no input", []string{"probe"}, "requires an input"},
		{"duplicate -i", []string{"probe", "-i", "a.mp4", "-i", "b.mp4"}, "duplicate -i"},
		{"two positionals", []string{"probe", "a.mp4", "b.mp4"}, "unexpected probe argument"},
		{"unknown option", []string{"probe", "in.mp4", "-show_private"}, "unknown probe option"},
		{"non-json format", []string{"probe", "in.mp4", "-of", "ini"}, "only supports json"},
		{"-i without value", []string{"probe", "-i"}, "requires a value"},
		{"ffmpeg option", []string{"probe", "-i", "in.mp4", "-y"}, "unknown probe option"},
	}
	for _, tc := range reject {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseArgs(tc.args)
			if err == nil {
				t.Fatalf("parseArgs(%v) accepted an invalid probe call", tc.args)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to contain %q", err, tc.want)
			}
		})
	}
}

// TestParseResumeArgs covers C-15's argument surface: at most one output path
// plus -y / -n; anything else is rejected naming the token rather than dropped.
func TestParseResumeArgs(t *testing.T) {
	isolateParseEnv(t)
	t.Setenv(config.EnvJobID, "job-1")

	accept := []struct {
		name  string
		args  []string
		want  string
		flags []string
	}{
		{"no positional", nil, "", nil},
		{"one positional", []string{"out.mp4"}, "out.mp4", nil},
		{"with -y", []string{"out.mp4", "-y"}, "out.mp4", []string{"-y"}},
		{"with -n", []string{"-n", "out.mp4"}, "out.mp4", []string{"-n"}},
	}
	for _, tc := range accept {
		t.Run(tc.name, func(t *testing.T) {
			opts, err := parseArgs(tc.args)
			if err != nil {
				t.Fatalf("parseArgs(%v) error = %v", tc.args, err)
			}
			if opts.Mode != ModeResume || opts.ResumeJobID != "job-1" {
				t.Fatalf("parseArgs(%v) = %v/%q, want ModeResume/job-1", tc.args, opts.Mode, opts.ResumeJobID)
			}
			if opts.ResumeOutput != tc.want {
				t.Errorf("ResumeOutput = %q, want %q", opts.ResumeOutput, tc.want)
			}
			if got := strings.Join(opts.FmpegArgs, " "); got != strings.Join(tc.flags, " ") {
				t.Errorf("FmpegArgs = %q, want %q", got, strings.Join(tc.flags, " "))
			}
		})
	}

	reject := []struct {
		name string
		args []string
		want string
	}{
		{"two positionals", []string{"a.mp4", "b.mp4"}, "at most one output path"},
		{"ffmpeg option", []string{"-i", "in.mp4"}, `got "-i"`},
		{"capability query", []string{"-encoders"}, `got "-encoders"`},
		{"help", []string{"-help"}, `got "-help"`},
		{"version", []string{"-version"}, `got "-version"`},
		{"probe word", []string{"out.mp4", "probe"}, "is rffmpeg's subcommand"},
		{"stdout target", []string{"-"}, "stdout"},
		{"bare pipe", []string{"pipe:"}, "stdout"},
		{"pipe fd 1", []string{"pipe:1"}, "stdout"},
		{"remote URL", []string{"https://example.test/out.mp4"}, "remote URL"},
	}
	for _, tc := range reject {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseArgs(tc.args)
			if err == nil {
				t.Fatalf("parseArgs(%v) accepted an out-of-surface re-attach call", tc.args)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to contain %q", err, tc.want)
			}
		})
	}
}

// TestParseArgs_JobIDFromEnvironmentOnly pins C-15's trigger: only the
// environment variable selects re-attach; a config file carrying the key does
// not, and neither does any command-line spelling.
func TestParseArgs_JobIDFromEnvironmentOnly(t *testing.T) {
	isolateParseEnv(t)
	opts, err := parseArgs([]string{"--resume", "job-1"})
	if err != nil {
		t.Fatalf("parseArgs error = %v", err)
	}
	if opts.Mode != ModeTranscode {
		t.Errorf("Mode = %v, want ModeTranscode: --resume is not a trigger", opts.Mode)
	}

	// The identifier is never read from os.Args, so an empty environment leaves
	// the re-attach path unselected even when the token looks like a job id.
	opts, err = parseArgs([]string{"job-1"})
	if err != nil {
		t.Fatalf("parseArgs error = %v", err)
	}
	if opts.Mode != ModeTranscode {
		t.Errorf("Mode = %v, want ModeTranscode: a positional never selects re-attach", opts.Mode)
	}
	if _, err := os.Stat("job-1"); err == nil {
		t.Fatalf("test fixture wrote %q into the working directory", "job-1")
	}
}
