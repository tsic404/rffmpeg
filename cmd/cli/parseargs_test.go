package main

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/tsix404/rffmpeg/pkg/protocol"
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
			serverURL, token, quiet, showHelp, showVersion, autoHW, _, _, _, _, _, _, _, _, _, _, _, _, _, _, _, _, _, ffmpegArgs := parseArgs(tt.args)
			if serverURL != tt.wantServerURL {
				t.Errorf("serverURL = %q, want %q", serverURL, tt.wantServerURL)
			}
			if token != tt.wantToken {
				t.Errorf("token = %q, want %q", token, tt.wantToken)
			}
			if quiet != tt.wantQuiet {
				t.Errorf("quiet = %v, want %v", quiet, tt.wantQuiet)
			}
			if showHelp != tt.wantShowHelp {
				t.Errorf("showHelp = %v, want %v", showHelp, tt.wantShowHelp)
			}
			if showVersion != tt.wantShowVersion {
				t.Errorf("showVersion = %v, want %v", showVersion, tt.wantShowVersion)
			}
			if autoHW != tt.wantAutoHW {
				t.Errorf("autoHW = %v, want %v", autoHW, tt.wantAutoHW)
			}
			if len(ffmpegArgs) != len(tt.wantFfmpegArgs) {
				t.Errorf("ffmpegArgs length = %d, want %d; got %v", len(ffmpegArgs), len(tt.wantFfmpegArgs), ffmpegArgs)
			} else {
				for i := range ffmpegArgs {
					if ffmpegArgs[i] != tt.wantFfmpegArgs[i] {
						t.Errorf("ffmpegArgs[%d] = %q, want %q", i, ffmpegArgs[i], tt.wantFfmpegArgs[i])
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
			_, _, _, _, _, _, _, _, _, _, _, showCodecs, _, _, _, _, _, _, _, _, _, _, _, ffmpegArgs := parseArgs(tt.args)
			if showCodecs != tt.wantShowCodecs {
				t.Errorf("showCodecs = %v, want %v", showCodecs, tt.wantShowCodecs)
			}
			if len(ffmpegArgs) != len(tt.wantFfmpegArgs) {
				t.Errorf("ffmpegArgs length = %d, want %d; got %v", len(ffmpegArgs), len(tt.wantFfmpegArgs), ffmpegArgs)
			} else {
				for i := range ffmpegArgs {
					if ffmpegArgs[i] != tt.wantFfmpegArgs[i] {
						t.Errorf("ffmpegArgs[%d] = %q, want %q", i, ffmpegArgs[i], tt.wantFfmpegArgs[i])
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
			name:           "probe with -i flag but no value",
			args:           []string{"probe", "-i"},
			wantIsProbe:    true,
			wantProbeInput: "",
			wantFfmpegArgs: []string{},
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
			serverURL, _, quiet, _, _, _, isProbe, probeInput, _, _, _, _, _, _, _, _, _, _, _, _, _, _, _, ffmpegArgs := parseArgs(tt.args)
			if isProbe != tt.wantIsProbe {
				t.Errorf("isProbe = %v, want %v", isProbe, tt.wantIsProbe)
			}
			if probeInput != tt.wantProbeInput {
				t.Errorf("probeInput = %q, want %q", probeInput, tt.wantProbeInput)
			}
			if len(ffmpegArgs) != len(tt.wantFfmpegArgs) {
				t.Errorf("ffmpegArgs length = %d, want %d; got %v", len(ffmpegArgs), len(tt.wantFfmpegArgs), ffmpegArgs)
			} else {
				for i := range ffmpegArgs {
					if ffmpegArgs[i] != tt.wantFfmpegArgs[i] {
						t.Errorf("ffmpegArgs[%d] = %q, want %q", i, ffmpegArgs[i], tt.wantFfmpegArgs[i])
					}
				}
			}
			if serverURL != tt.wantServerURL {
				t.Errorf("serverURL = %q, want %q", serverURL, tt.wantServerURL)
			}
			if quiet != tt.wantQuiet {
				t.Errorf("quiet = %v, want %v", quiet, tt.wantQuiet)
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
			_, _, _, _, _, _, _, _, _, showEncoders, showDecoders, showCodecs, showHwaccels, showFilters, showPixFmts, showFormats, showBuildconf, showLayouts, showProtocols, showSampleFmts, showBsfs, showColors, showJSON, _ := parseArgs(tt.args)
			if showEncoders != tt.wantEncoders {
				t.Errorf("showEncoders = %v, want %v", showEncoders, tt.wantEncoders)
			}
			if showDecoders != tt.wantDecoders {
				t.Errorf("showDecoders = %v, want %v", showDecoders, tt.wantDecoders)
			}
			if showHwaccels != tt.wantHwaccels {
				t.Errorf("showHwaccels = %v, want %v", showHwaccels, tt.wantHwaccels)
			}
			if showFilters != tt.wantFilters {
				t.Errorf("showFilters = %v, want %v", showFilters, tt.wantFilters)
			}
			if showPixFmts != tt.wantPixFmts {
				t.Errorf("showPixFmts = %v, want %v", showPixFmts, tt.wantPixFmts)
			}
			if showFormats != tt.wantFormats {
				t.Errorf("showFormats = %v, want %v", showFormats, tt.wantFormats)
			}
			if showBuildconf != tt.wantBuildconf {
				t.Errorf("showBuildconf = %v, want %v", showBuildconf, tt.wantBuildconf)
			}
			if showLayouts != tt.wantLayouts {
				t.Errorf("showLayouts = %v, want %v", showLayouts, tt.wantLayouts)
			}
			if showProtocols != tt.wantProtocols {
				t.Errorf("showProtocols = %v, want %v", showProtocols, tt.wantProtocols)
			}
			if showSampleFmts != tt.wantSampleFmts {
				t.Errorf("showSampleFmts = %v, want %v", showSampleFmts, tt.wantSampleFmts)
			}
			if showBsfs != tt.wantBsfs {
				t.Errorf("showBsfs = %v, want %v", showBsfs, tt.wantBsfs)
			}
			if showColors != tt.wantColors {
				t.Errorf("showColors = %v, want %v", showColors, tt.wantColors)
			}
			if showJSON != tt.wantJSON {
				t.Errorf("showJSON = %v, want %v", showJSON, tt.wantJSON)
			}
			_ = showCodecs
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
			_, _, _, _, _, _, _, _, timeout, _, _, _, _, _, _, _, _, _, _, _, _, _, _, _ := parseArgs(tt.args)
			if timeout != tt.wantTimeout {
				t.Errorf("timeout = %v, want %v", timeout, tt.wantTimeout)
			}
		})
	}
}
