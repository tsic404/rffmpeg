package main

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/tsic404/rffmpeg/pkg/protocol"
)

func TestParseArgs_SingleDashOptions(t *testing.T) {
	tests := []struct {
		name            string
		args            []string
		wantServerURL   string
		wantToken       string
		wantQuiet       bool
		wantShowHelp    bool
		wantShowVersion bool
		wantAutoHW      bool
		wantFfmpegArgs  []string
	}{
		{
			name:           "-server with value",
			args:           []string{"-server", "http://localhost:8080", "-i", "video.mp4", "output.mp4"},
			wantServerURL:  "http://localhost:8080",
			wantFfmpegArgs: []string{"-i", "video.mp4", "output.mp4"},
		},
		{
			name:           "-token with value",
			args:           []string{"-token", "mytoken", "-i", "video.mp4", "output.mp4"},
			wantToken:      "mytoken",
			wantFfmpegArgs: []string{"-i", "video.mp4", "output.mp4"},
		},
		{
			name:           "-quiet flag",
			args:           []string{"-quiet", "-i", "video.mp4", "output.mp4"},
			wantQuiet:      true,
			wantFfmpegArgs: []string{"-i", "video.mp4", "output.mp4"},
		},
		{
			name:           "-auto-hw flag",
			args:           []string{"-auto-hw", "-i", "video.mp4", "output.mp4"},
			wantAutoHW:     true,
			wantFfmpegArgs: []string{"-i", "video.mp4", "output.mp4"},
		},
		{
			name:           "-auto-hw with -c:v libx264",
			args:           []string{"-auto-hw", "-i", "video.mp4", "-c:v", "libx264", "output.mp4"},
			wantAutoHW:     true,
			wantFfmpegArgs: []string{"-i", "video.mp4", "-c:v", "libx264", "output.mp4"},
		},
		{
			name:           "--auto-hw with -c:v libx264",
			args:           []string{"--auto-hw", "-i", "video.mp4", "-c:v", "libx264", "output.mp4"},
			wantAutoHW:     true,
			wantFfmpegArgs: []string{"-i", "video.mp4", "-c:v", "libx264", "output.mp4"},
		},
		{
			name:           "-auto-hw=true",
			args:           []string{"-auto-hw=true", "-i", "video.mp4", "output.mp4"},
			wantAutoHW:     true,
			wantFfmpegArgs: []string{"-i", "video.mp4", "output.mp4"},
		},
		{
			name:           "-auto-hw=false",
			args:           []string{"-auto-hw=false", "-i", "video.mp4", "output.mp4"},
			wantAutoHW:     false,
			wantFfmpegArgs: []string{"-i", "video.mp4", "output.mp4"},
		},
		{
			name:           "-help flag",
			args:           []string{"-help"},
			wantShowHelp:   true,
			wantFfmpegArgs: []string{},
		},
		{
			name:            "-version flag",
			args:            []string{"-version"},
			wantShowVersion: true,
			wantFfmpegArgs:  []string{},
		},
		{
			name:           "mixed single-dash and double-dash options",
			args:           []string{"-server", "http://localhost:8080", "-quiet", "--token", "mytoken", "-i", "video.mp4", "output.mp4"},
			wantServerURL:  "http://localhost:8080",
			wantToken:      "mytoken",
			wantQuiet:      true,
			wantFfmpegArgs: []string{"-i", "video.mp4", "output.mp4"},
		},
		{
			name:           "-server with missing value (not consumed)",
			args:           []string{"-i", "video.mp4", "output.mp4"},
			wantServerURL:  "",
			wantFfmpegArgs: []string{"-i", "video.mp4", "output.mp4"},
		},
		{
			name:           "-server without value consumes next arg (consistent with --server)",
			args:           []string{"-server", "-i", "video.mp4", "output.mp4"},
			wantServerURL:  "-i",
			wantFfmpegArgs: []string{"video.mp4", "output.mp4"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts, err := parseArgs(tt.args)
			if err != nil {
				t.Fatalf("parseArgs(%v) unexpected error: %v", tt.args, err)
			}
			if opts.ServerURL != tt.wantServerURL {
				t.Errorf("serverURL = %q, want %q", opts.ServerURL, tt.wantServerURL)
			}
			if opts.Token != tt.wantToken {
				t.Errorf("token = %q, want %q", opts.Token, tt.wantToken)
			}
			if opts.Quiet != tt.wantQuiet {
				t.Errorf("quiet = %v, want %v", opts.Quiet, tt.wantQuiet)
			}
			if opts.ShowHelp != tt.wantShowHelp {
				t.Errorf("showHelp = %v, want %v", opts.ShowHelp, tt.wantShowHelp)
			}
			if opts.ShowVersion != tt.wantShowVersion {
				t.Errorf("showVersion = %v, want %v", opts.ShowVersion, tt.wantShowVersion)
			}
			if opts.AutoHW != tt.wantAutoHW {
				t.Errorf("autoHW = %v, want %v", opts.AutoHW, tt.wantAutoHW)
			}
			if len(opts.FmpegArgs) != len(tt.wantFfmpegArgs) {
				t.Errorf("ffmpegArgs length = %d, want %d; got %v", len(opts.FmpegArgs), len(tt.wantFfmpegArgs), opts.FmpegArgs)
			} else {
				for i := range opts.FmpegArgs {
					if opts.FmpegArgs[i] != tt.wantFfmpegArgs[i] {
						t.Errorf("ffmpegArgs[%d] = %q, want %q", i, opts.FmpegArgs[i], tt.wantFfmpegArgs[i])
					}
				}
			}
		})
	}
}

func TestParseArgs_CodecsFlag(t *testing.T) {
	tests := []struct {
		name           string
		args           []string
		wantShowCodecs bool
		wantFfmpegArgs []string
	}{
		{
			name:           "-codecs alone",
			args:           []string{"-codecs"},
			wantShowCodecs: true,
			wantFfmpegArgs: []string{},
		},
		{
			name:           "--codecs alone",
			args:           []string{"--codecs"},
			wantShowCodecs: true,
			wantFfmpegArgs: []string{},
		},
		{
			name:           "-encoders only (codecs not set)",
			args:           []string{"-encoders"},
			wantShowCodecs: false,
			wantFfmpegArgs: []string{},
		},
		{
			name:           "normal ffmpeg args (codecs not set)",
			args:           []string{"-i", "video.mp4", "output.mp4"},
			wantShowCodecs: false,
			wantFfmpegArgs: []string{"-i", "video.mp4", "output.mp4"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts, err := parseArgs(tt.args)
			if err != nil {
				t.Fatalf("parseArgs(%v) unexpected error: %v", tt.args, err)
			}
			if opts.ShowCodecs != tt.wantShowCodecs {
				t.Errorf("showCodecs = %v, want %v", opts.ShowCodecs, tt.wantShowCodecs)
			}
			if len(opts.FmpegArgs) != len(tt.wantFfmpegArgs) {
				t.Errorf("ffmpegArgs length = %d, want %d; got %v", len(opts.FmpegArgs), len(tt.wantFfmpegArgs), opts.FmpegArgs)
			} else {
				for i := range opts.FmpegArgs {
					if opts.FmpegArgs[i] != tt.wantFfmpegArgs[i] {
						t.Errorf("ffmpegArgs[%d] = %q, want %q", i, opts.FmpegArgs[i], tt.wantFfmpegArgs[i])
					}
				}
			}
		})
	}
}

func TestParseArgs_ProbeSubcommand(t *testing.T) {
	tests := []struct {
		name           string
		args           []string
		wantErr        bool
		wantIsProbe    bool
		wantProbeInput string
		wantFfmpegArgs []string
		wantServerURL  string
		wantQuiet      bool
	}{
		{
			name:           "probe with file",
			args:           []string{"probe", "video.mp4"},
			wantIsProbe:    true,
			wantProbeInput: "video.mp4",
			wantFfmpegArgs: []string{},
		},
		{
			name:           "probe with options before",
			args:           []string{"--server", "http://localhost:8080", "probe", "video.mp4"},
			wantIsProbe:    true,
			wantProbeInput: "video.mp4",
			wantFfmpegArgs: []string{},
			wantServerURL:  "http://localhost:8080",
		},
		{
			name:           "probe with -q before",
			args:           []string{"-q", "probe", "video.mp4"},
			wantIsProbe:    true,
			wantProbeInput: "video.mp4",
			wantFfmpegArgs: []string{},
			wantQuiet:      true,
		},
		{
			name:           "probe with --quiet before",
			args:           []string{"--quiet", "probe", "video.mp4"},
			wantIsProbe:    true,
			wantProbeInput: "video.mp4",
			wantFfmpegArgs: []string{},
			wantQuiet:      true,
		},
		{
			name:           "probe alone",
			args:           []string{"probe"},
			wantIsProbe:    true,
			wantProbeInput: "",
			wantFfmpegArgs: []string{},
		},
		{
			name:           "probe with remote URL",
			args:           []string{"probe", "https://example.com/video.mp4"},
			wantIsProbe:    true,
			wantProbeInput: "https://example.com/video.mp4",
			wantFfmpegArgs: []string{},
		},
		{
			name:           "normal ffmpeg command",
			args:           []string{"-i", "video.mp4", "-c:v", "libx264", "output.mp4"},
			wantIsProbe:    false,
			wantProbeInput: "",
			wantFfmpegArgs: []string{"-i", "video.mp4", "-c:v", "libx264", "output.mp4"},
		},
		{
			name:           "probe with -i flag",
			args:           []string{"probe", "-i", "video.mp4"},
			wantIsProbe:    true,
			wantProbeInput: "video.mp4",
			wantFfmpegArgs: []string{},
		},
		{
			name:           "probe with -i flag and rffmpeg option before",
			args:           []string{"-q", "probe", "-i", "video.mp4"},
			wantIsProbe:    true,
			wantProbeInput: "video.mp4",
			wantFfmpegArgs: []string{},
			wantQuiet:      true,
		},
		{
			name:    "probe with -i flag but no value",
			args:    []string{"probe", "-i"},
			wantErr: true,
		},
		{
			name:    "probe rejects duplicate -i flag",
			args:    []string{"probe", "-i", "a.mp4", "-i", "b.mp4"},
			wantErr: true,
		},
		{
			name:    "probe rejects flag-like value after -i",
			args:    []string{"probe", "-i", "-show_streams", "video.mp4"},
			wantErr: true,
		},
		{
			name:           "probe with -show_streams and -of json",
			args:           []string{"probe", "video.mp4", "-show_streams", "-of", "json"},
			wantIsProbe:    true,
			wantProbeInput: "video.mp4",
			wantFfmpegArgs: []string{},
		},
		{
			name:           "probe with -i and trailing ffprobe flags",
			args:           []string{"probe", "-i", "video.mp4", "-show_format", "-print_format", "json"},
			wantIsProbe:    true,
			wantProbeInput: "video.mp4",
			wantFfmpegArgs: []string{},
		},
		{
			name:           "probe with ffprobe flags before -i",
			args:           []string{"probe", "-show_streams", "-of", "json", "-i", "video.mp4"},
			wantIsProbe:    true,
			wantProbeInput: "video.mp4",
			wantFfmpegArgs: []string{},
		},
		{
			name:    "probe rejects non-json output format",
			args:    []string{"probe", "video.mp4", "-of", "xml"},
			wantErr: true,
		},
		{
			name:    "probe rejects unknown option",
			args:    []string{"probe", "video.mp4", "-show_entries", "stream=codec_name"},
			wantErr: true,
		},
		{
			name:           "probe as filename (not subcommand, after -i)",
			args:           []string{"-i", "probe", "output.mp4"},
			wantIsProbe:    false,
			wantProbeInput: "",
			wantFfmpegArgs: []string{"-i", "probe", "output.mp4"},
		},
		{
			name:           "probe with --server and --token before",
			args:           []string{"--server", "http://example.com", "--token", "mytoken", "probe", "video.mp4"},
			wantIsProbe:    true,
			wantProbeInput: "video.mp4",
			wantFfmpegArgs: []string{},
			wantServerURL:  "http://example.com",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts, err := parseArgs(tt.args)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseArgs(%v) expected error, got opts=%+v", tt.args, opts)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseArgs(%v) unexpected error: %v", tt.args, err)
			}
			if opts.IsProbe != tt.wantIsProbe {
				t.Errorf("isProbe = %v, want %v", opts.IsProbe, tt.wantIsProbe)
			}
			if opts.ProbeInput != tt.wantProbeInput {
				t.Errorf("probeInput = %q, want %q", opts.ProbeInput, tt.wantProbeInput)
			}
			if len(opts.FmpegArgs) != len(tt.wantFfmpegArgs) {
				t.Errorf("ffmpegArgs length = %d, want %d; got %v", len(opts.FmpegArgs), len(tt.wantFfmpegArgs), opts.FmpegArgs)
			} else {
				for i := range opts.FmpegArgs {
					if opts.FmpegArgs[i] != tt.wantFfmpegArgs[i] {
						t.Errorf("ffmpegArgs[%d] = %q, want %q", i, opts.FmpegArgs[i], tt.wantFfmpegArgs[i])
					}
				}
			}
			if opts.ServerURL != tt.wantServerURL {
				t.Errorf("serverURL = %q, want %q", opts.ServerURL, tt.wantServerURL)
			}
			if opts.Quiet != tt.wantQuiet {
				t.Errorf("quiet = %v, want %v", opts.Quiet, tt.wantQuiet)
			}
		})
	}
}

// TestParseArgs_NewInfoFlags tests the new P0/P1/P2 info flag parsing.
func TestParseArgs_NewInfoFlags(t *testing.T) {
	tests := []struct {
		name           string
		args           []string
		wantEncoders   bool
		wantDecoders   bool
		wantHwaccels   bool
		wantFilters    bool
		wantPixFmts    bool
		wantFormats    bool
		wantBuildconf  bool
		wantLayouts    bool
		wantProtocols  bool
		wantSampleFmts bool
		wantBsfs       bool
		wantColors     bool
		wantJSON       bool
	}{
		{
			name:         "-encoders flag",
			args:         []string{"-encoders"},
			wantEncoders: true,
		},
		{
			name:         "--encoders flag",
			args:         []string{"--encoders"},
			wantEncoders: true,
		},
		{
			name:         "-decoders flag",
			args:         []string{"-decoders"},
			wantDecoders: true,
		},
		{
			name:         "-hwaccels flag",
			args:         []string{"-hwaccels"},
			wantHwaccels: true,
		},
		{
			name:        "-filters flag",
			args:        []string{"-filters"},
			wantFilters: true,
		},
		{
			name:        "--filters flag",
			args:        []string{"--filters"},
			wantFilters: true,
		},
		{
			name:        "-pix_fmts flag",
			args:        []string{"-pix_fmts"},
			wantPixFmts: true,
		},
		{
			name:        "--pix_fmts flag",
			args:        []string{"--pix_fmts"},
			wantPixFmts: true,
		},
		{
			name:        "-formats flag",
			args:        []string{"-formats"},
			wantFormats: true,
		},
		{
			name:        "--formats flag",
			args:        []string{"--formats"},
			wantFormats: true,
		},
		{
			name:          "-buildconf flag",
			args:          []string{"-buildconf"},
			wantBuildconf: true,
		},
		{
			name:          "--buildconf flag",
			args:          []string{"--buildconf"},
			wantBuildconf: true,
		},
		{
			name:        "-layouts flag",
			args:        []string{"-layouts"},
			wantLayouts: true,
		},
		{
			name:          "-protocols flag",
			args:          []string{"-protocols"},
			wantProtocols: true,
		},
		{
			name:           "-sample_fmts flag",
			args:           []string{"-sample_fmts"},
			wantSampleFmts: true,
		},
		{
			name:     "-bsfs flag",
			args:     []string{"-bsfs"},
			wantBsfs: true,
		},
		{
			name:       "-colors flag",
			args:       []string{"-colors"},
			wantColors: true,
		},
		{
			name:     "--json flag",
			args:     []string{"--json"},
			wantJSON: true,
		},
		{
			name:        "combined -filters --json",
			args:        []string{"-filters", "--json"},
			wantFilters: true,
			wantJSON:    true,
		},
		{
			name:        "--json before -filters",
			args:        []string{"--json", "-filters"},
			wantFilters: true,
			wantJSON:    true,
		},
		{
			name: "info flags not set in normal transcode command",
			args: []string{"-i", "video.mp4", "output.mp4"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts, err := parseArgs(tt.args)
			if err != nil {
				t.Fatalf("parseArgs(%v) unexpected error: %v", tt.args, err)
			}
			if opts.ShowEncoders != tt.wantEncoders {
				t.Errorf("showEncoders = %v, want %v", opts.ShowEncoders, tt.wantEncoders)
			}
			if opts.ShowDecoders != tt.wantDecoders {
				t.Errorf("showDecoders = %v, want %v", opts.ShowDecoders, tt.wantDecoders)
			}
			if opts.ShowHwaccels != tt.wantHwaccels {
				t.Errorf("showHwaccels = %v, want %v", opts.ShowHwaccels, tt.wantHwaccels)
			}
			if opts.ShowFilters != tt.wantFilters {
				t.Errorf("showFilters = %v, want %v", opts.ShowFilters, tt.wantFilters)
			}
			if opts.ShowPixFmts != tt.wantPixFmts {
				t.Errorf("showPixFmts = %v, want %v", opts.ShowPixFmts, tt.wantPixFmts)
			}
			if opts.ShowFormats != tt.wantFormats {
				t.Errorf("showFormats = %v, want %v", opts.ShowFormats, tt.wantFormats)
			}
			if opts.ShowBuildconf != tt.wantBuildconf {
				t.Errorf("showBuildconf = %v, want %v", opts.ShowBuildconf, tt.wantBuildconf)
			}
			if opts.ShowLayouts != tt.wantLayouts {
				t.Errorf("showLayouts = %v, want %v", opts.ShowLayouts, tt.wantLayouts)
			}
			if opts.ShowProtocols != tt.wantProtocols {
				t.Errorf("showProtocols = %v, want %v", opts.ShowProtocols, tt.wantProtocols)
			}
			if opts.ShowSampleFmts != tt.wantSampleFmts {
				t.Errorf("showSampleFmts = %v, want %v", opts.ShowSampleFmts, tt.wantSampleFmts)
			}
			if opts.ShowBsfs != tt.wantBsfs {
				t.Errorf("showBsfs = %v, want %v", opts.ShowBsfs, tt.wantBsfs)
			}
			if opts.ShowColors != tt.wantColors {
				t.Errorf("showColors = %v, want %v", opts.ShowColors, tt.wantColors)
			}
			if opts.ShowJSON != tt.wantJSON {
				t.Errorf("showJSON = %v, want %v", opts.ShowJSON, tt.wantJSON)
			}
		})
	}
}

func TestOutputInfoFlagJSON(t *testing.T) {
	encoders := []protocol.EncoderInfo{
		{Name: "libx264", Description: "H.264 encoder", Type: "video"},
		{Name: "aac", Description: "AAC encoder", Type: "audio"},
	}
	decoders := []protocol.DecoderInfo{
		{Name: "h264", Description: "H.264 decoder", Type: "video"},
	}

	output := captureStdout(func() {
		exitCode := outputInfoFlagJSON("codecs", encoders, decoders)
		if exitCode != ExitSuccess {
			t.Errorf("outputInfoFlagJSON returned %d, want %d", exitCode, ExitSuccess)
		}
	})

	var result map[string]interface{}
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("failed to parse JSON output: %v\nOutput: %s", err, output)
	}

	entries, ok := result["codecs"].([]interface{})
	if !ok {
		t.Fatalf("expected 'codecs' key with array, got %T", result["codecs"])
	}
	if len(entries) != 3 {
		t.Errorf("expected 3 entries (2 encoders + 1 decoder), got %d", len(entries))
	}
}

func TestPrintJSONEncoderInfo(t *testing.T) {
	encoders := []protocol.EncoderInfo{
		{Name: "libx264", Description: "H.264 encoder", Type: "video"},
	}

	output := captureStdout(func() {
		exitCode := printJSONEncoderInfo(encoders, "encoders")
		if exitCode != ExitSuccess {
			t.Errorf("printJSONEncoderInfo returned %d, want %d", exitCode, ExitSuccess)
		}
	})

	var result map[string]interface{}
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("failed to parse JSON output: %v\nOutput: %s", err, output)
	}

	entries, ok := result["encoders"].([]interface{})
	if !ok {
		t.Fatalf("expected 'encoders' key with array, got %T", result["encoders"])
	}
	if len(entries) != 1 {
		t.Errorf("expected 1 entry, got %d", len(entries))
	}
}

func TestPrintJSONDecoderInfo(t *testing.T) {
	decoders := []protocol.DecoderInfo{
		{Name: "h264", Description: "H.264 decoder", Type: "video"},
	}

	output := captureStdout(func() {
		exitCode := printJSONDecoderInfo(decoders, "decoders")
		if exitCode != ExitSuccess {
			t.Errorf("printJSONDecoderInfo returned %d, want %d", exitCode, ExitSuccess)
		}
	})

	var result map[string]interface{}
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("failed to parse JSON output: %v\nOutput: %s", err, output)
	}

	entries, ok := result["decoders"].([]interface{})
	if !ok {
		t.Fatalf("expected 'decoders' key with array, got %T", result["decoders"])
	}
	if len(entries) != 1 {
		t.Errorf("expected 1 entry, got %d", len(entries))
	}
}

func TestPrintJSONArray(t *testing.T) {
	items := []string{"nvenc_h264", "nvenc_hevc", "aac"}

	output := captureStdout(func() {
		exitCode := printJSONArray(items, "filters")
		if exitCode != ExitSuccess {
			t.Errorf("printJSONArray returned %d, want %d", exitCode, ExitSuccess)
		}
	})

	var result map[string]interface{}
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("failed to parse JSON output: %v\nOutput: %s", err, output)
	}

	arr, ok := result["filters"].([]interface{})
	if !ok {
		t.Fatalf("expected 'filters' key with array, got %T", result["filters"])
	}
	if len(arr) != 3 {
		t.Errorf("expected 3 items, got %d", len(arr))
	}
}

// TestParseArgsTimeout tests the --timeout flag parsing
func TestParseArgsTimeout(t *testing.T) {
	tests := []struct {
		name        string
		args        []string
		wantTimeout time.Duration
		wantError   bool
	}{
		{
			name:        "no timeout",
			args:        []string{"-i", "input.mp4", "output.mp4"},
			wantTimeout: 0,
			wantError:   false,
		},
		{
			name:        "timeout in seconds",
			args:        []string{"--timeout", "300s", "-i", "input.mp4", "output.mp4"},
			wantTimeout: 300 * time.Second,
			wantError:   false,
		},
		{
			name:        "timeout in minutes",
			args:        []string{"--timeout", "5m", "-i", "input.mp4", "output.mp4"},
			wantTimeout: 5 * time.Minute,
			wantError:   false,
		},
		{
			name:        "timeout in hours",
			args:        []string{"--timeout", "2h", "-i", "input.mp4", "output.mp4"},
			wantTimeout: 2 * time.Hour,
			wantError:   false,
		},
		{
			name:        "combined timeout",
			args:        []string{"--timeout", "1h30m", "-i", "input.mp4", "output.mp4"},
			wantTimeout: 90 * time.Minute,
			wantError:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts, err := parseArgs(tt.args)
			if tt.wantError {
				if err == nil {
					t.Fatalf("parseArgs(%v) expected error, got nil", tt.args)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseArgs(%v) unexpected error: %v", tt.args, err)
			}
			if opts.Timeout != tt.wantTimeout {
				t.Errorf("timeout = %v, want %v", opts.Timeout, tt.wantTimeout)
			}
		})
	}
}

// TestParseArgs_MissingValue verifies that value-taking flags without a value
// are a hard error instead of silently falling back to defaults (TSI-2365).
func TestParseArgs_MissingValue(t *testing.T) {
	for _, flag := range []string{"--server", "-server", "--token", "-token", "--timeout", "-timeout", "--max-retries", "-max-retries"} {
		if _, err := parseArgs([]string{flag}); err == nil {
			t.Errorf("parseArgs(%q) expected error for missing value, got nil", flag)
		}
	}
}

// TestParseArgs_MaxRetries verifies --max-retries parsing and validation.
func TestParseArgs_MaxRetries(t *testing.T) {
	opts, err := parseArgs([]string{"--max-retries", "7", "-i", "in.mp4", "out.mp4"})
	if err != nil {
		t.Fatalf("parseArgs unexpected error: %v", err)
	}
	if opts.MaxRetries != 7 {
		t.Errorf("MaxRetries = %d, want 7", opts.MaxRetries)
	}
	if !opts.MaxRetriesSet {
		t.Errorf("MaxRetriesSet = false, want true when flag present")
	}

	opts, err = parseArgs([]string{"-max-retries", "3", "-i", "in.mp4", "out.mp4"})
	if err != nil {
		t.Fatalf("parseArgs(-max-retries) unexpected error: %v", err)
	}
	if opts.MaxRetries != 3 {
		t.Errorf("MaxRetries = %d, want 3", opts.MaxRetries)
	}

	// 0 = no retries, and must be distinguishable from "flag absent".
	opts, err = parseArgs([]string{"--max-retries", "0", "-i", "in.mp4", "out.mp4"})
	if err != nil {
		t.Fatalf("parseArgs(--max-retries 0) unexpected error: %v", err)
	}
	if opts.MaxRetries != 0 || !opts.MaxRetriesSet {
		t.Errorf("MaxRetries = %d (set=%v), want 0 (set=true)", opts.MaxRetries, opts.MaxRetriesSet)
	}

	for _, bad := range []string{"-1", "abc"} {
		if _, err := parseArgs([]string{"--max-retries", bad}); err == nil {
			t.Errorf("parseArgs(--max-retries %s) expected error, got nil", bad)
		}
	}
}

// TestParseArgs_Retry verifies --retry is a boolean opt-in (default false)
// that does not consume the next argument.
func TestParseArgs_Retry(t *testing.T) {
	opts, err := parseArgs([]string{"--retry", "-i", "in.mp4", "out.mp4"})
	if err != nil {
		t.Fatalf("parseArgs(--retry) unexpected error: %v", err)
	}
	if !opts.Retry {
		t.Errorf("Retry = false, want true when --retry present")
	}
	if want := []string{"-i", "in.mp4", "out.mp4"}; len(opts.FmpegArgs) != len(want) {
		t.Fatalf("FmpegArgs = %v, want %v", opts.FmpegArgs, want)
	}

	opts, err = parseArgs([]string{"-retry", "-i", "in.mp4", "out.mp4"})
	if err != nil {
		t.Fatalf("parseArgs(-retry) unexpected error: %v", err)
	}
	if !opts.Retry {
		t.Errorf("Retry = false, want true when -retry present")
	}

	opts, err = parseArgs([]string{"-i", "in.mp4", "out.mp4"})
	if err != nil {
		t.Fatalf("parseArgs(default) unexpected error: %v", err)
	}
	if opts.Retry {
		t.Errorf("Retry = true, want false by default")
	}
}

// TestParseArgs_DashDashSeparator verifies that everything after "--" is
// passed through to ffmpeg verbatim.
func TestParseArgs_DashDashSeparator(t *testing.T) {
	opts, err := parseArgs([]string{"-q", "--", "-i", "a.mp4", "--server", "b.mp4"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"-i", "a.mp4", "--server", "b.mp4"}
	if len(opts.FmpegArgs) != len(want) {
		t.Fatalf("ffmpegArgs = %v, want %v", opts.FmpegArgs, want)
	}
	for i := range want {
		if opts.FmpegArgs[i] != want[i] {
			t.Errorf("ffmpegArgs[%d] = %q, want %q", i, opts.FmpegArgs[i], want[i])
		}
	}
}

// TestParseArgs_InvalidTimeout verifies bad timeout values error out instead
// of os.Exit mid-parse.
func TestParseArgs_InvalidTimeout(t *testing.T) {
	if _, err := parseArgs([]string{"--timeout", "abc"}); err == nil {
		t.Error("parseArgs(--timeout abc) expected error, got nil")
	}
	if _, err := parseArgs([]string{"--timeout", "-5m"}); err == nil {
		t.Error("parseArgs(--timeout -5m) expected error, got nil")
	}
}
