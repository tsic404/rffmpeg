package args

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseBasicInput(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantInputs []string
		wantOutput string
		wantErr    bool
	}{
		{
			name:       "simple input output",
			args:       []string{"-i", "input.mp4", "output.mp4"},
			wantInputs: []string{"input.mp4"},
			wantOutput: "output.mp4",
			wantErr:    false,
		},
		{
			name:       "multiple inputs",
			args:       []string{"-i", "input1.mp4", "-i", "input2.mp4", "output.mp4"},
			wantInputs: []string{"input1.mp4", "input2.mp4"},
			wantOutput: "output.mp4",
			wantErr:    false,
		},
		{
			name:       "input with codec options",
			args:       []string{"-i", "input.mp4", "-c:v", "libx264", "-c:a", "aac", "output.mp4"},
			wantInputs: []string{"input.mp4"},
			wantOutput: "output.mp4",
			wantErr:    false,
		},
		{
			// ffmpeg has no glued "-i<input>" form ("Unrecognized option
			// 'iinput.mp4'"), so rffmpeg must not invent one either.
			name:    "concatenated -i option is not an input",
			args:    []string{"-iinput.mp4", "output.mp4"},
			wantErr: true,
		},
		{
			name:    "no input",
			args:    []string{"output.mp4"},
			wantErr: true,
		},
		{
			name:    "no output",
			args:    []string{"-i", "input.mp4"},
			wantErr: true,
		},
		{
			name:    "empty args",
			args:    []string{},
			wantErr: true,
		},
		{
			name:       "input with preset and crf",
			args:       []string{"-i", "input.mp4", "-preset", "fast", "-crf", "23", "output.mp4"},
			wantInputs: []string{"input.mp4"},
			wantOutput: "output.mp4",
			wantErr:    false,
		},
		{
			name:       "hardware acceleration",
			args:       []string{"-hwaccel", "cuda", "-i", "input.mp4", "-c:v", "h264_nvenc", "output.mp4"},
			wantInputs: []string{"input.mp4"},
			wantOutput: "output.mp4",
			wantErr:    false,
		},
		{
			name:       "complex filter",
			args:       []string{"-i", "input.mp4", "-vf", "scale=1280:720", "-c:v", "libx264", "output.mp4"},
			wantInputs: []string{"input.mp4"},
			wantOutput: "output.mp4",
			wantErr:    false,
		},
		{
			name:       "multiple inputs with -shortest boolean flag",
			args:       []string{"-i", "video.mp4", "-i", "audio.mp3", "-c:v", "copy", "-c:a", "aac", "-shortest", "merged.mp4"},
			wantInputs: []string{"video.mp4", "audio.mp3"},
			wantOutput: "merged.mp4",
			wantErr:    false,
		},
		{
			name:       "multiple inputs with -y boolean flag",
			args:       []string{"-i", "input.mp4", "-y", "output.mp4"},
			wantInputs: []string{"input.mp4"},
			wantOutput: "output.mp4",
			wantErr:    false,
		},
		{
			name:       "multiple inputs with multiple boolean flags",
			args:       []string{"-i", "video.mp4", "-i", "audio.mp3", "-y", "-shortest", "-hide_banner", "merged.mp4"},
			wantInputs: []string{"video.mp4", "audio.mp3"},
			wantOutput: "merged.mp4",
			wantErr:    false,
		},
		{
			name:       "trailing -an boolean flag before output",
			args:       []string{"-i", "input.mp4", "-an", "output.mp4"},
			wantInputs: []string{"input.mp4"},
			wantOutput: "output.mp4",
			wantErr:    false,
		},
		{
			name:       "trailing -vn -sn -dn boolean flags before output",
			args:       []string{"-i", "input.mp4", "-vn", "-sn", "-dn", "output.mp4"},
			wantInputs: []string{"input.mp4"},
			wantOutput: "output.mp4",
			wantErr:    false,
		},

		{
			name:       "streaming output with bare dash",
			args:       []string{"-i", "input.mp4", "-c:v", "libx264", "-f", "matroska", "-"},
			wantInputs: []string{"input.mp4"},
			wantOutput: "-",
			wantErr:    false,
		},
		{
			name:       "streaming output with -o -",
			args:       []string{"-i", "input.mp4", "-c:v", "libx264", "-o", "-"},
			wantInputs: []string{"input.mp4"},
			wantOutput: "-",
			wantErr:    false,
		},
		{
			name:       "explicit output with -o flag",
			args:       []string{"-i", "input.mp4", "-o", "output.mp4", "-c:v", "libx264"},
			wantInputs: []string{"input.mp4"},
			wantOutput: "output.mp4",
			wantErr:    false,
		},
		{
			name:       "streaming output bare dash minimal",
			args:       []string{"-i", "input.mp4", "-"},
			wantInputs: []string{"input.mp4"},
			wantOutput: "-",
			wantErr:    false,
		},
	}

	p := NewParser()

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := p.Parse(tt.args)
			if (err != nil) != tt.wantErr {
				t.Errorf("Parse() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.wantErr {
				return
			}
			if len(result.InputFiles) != len(tt.wantInputs) {
				t.Errorf("Parse() got %d inputs, want %d", len(result.InputFiles), len(tt.wantInputs))
				return
			}
			for i, input := range result.InputFiles {
				if input != tt.wantInputs[i] {
					t.Errorf("Parse() input[%d] = %v, want %v", i, input, tt.wantInputs[i])
				}
			}
			if result.OutputFile != tt.wantOutput {
				t.Errorf("Parse() output = %v, want %v", result.OutputFile, tt.wantOutput)
			}
		})
	}
}

// TestParseMissingValue pins the fix: a value-taking option that
// finds no value must report "requires a value" instead of a misleading
// "no input/output file specified".
func TestParseMissingValue(t *testing.T) {
	p := NewParser()
	cases := [][]string{
		{"-c:v"},
		{"-i", "input.mp4", "-c:v"},
	}
	for _, args := range cases {
		_, err := p.Parse(args)
		if err == nil {
			t.Errorf("Parse(%v): expected error, got nil", args)
			continue
		}
		if !strings.Contains(err.Error(), "requires a value") {
			t.Errorf("Parse(%v): error = %q, want a 'requires a value' error", args, err)
		}
	}
}

// TestParseNegativeOptionValue pins the fix: a value-taking option
// whose value begins with "-" (e.g. "-map -1") must consume that value and
// preserve it, rather than misreading it as a missing value or another option.
func TestParseNegativeOptionValue(t *testing.T) {
	p := NewParser()
	cases := [][]string{
		{"-i", "input.mp4", "-map", "-1", "output.mp4"},
		{"-i", "input.mp4", "-ss", "-10", "output.mp4"},
		{"-i", "input.mp4", "-itsoffset", "-5", "output.mp4"},
	}
	for _, args := range cases {
		result, err := p.Parse(args)
		if err != nil {
			t.Errorf("Parse(%v): error = %v, want success", args, err)
			continue
		}
		if result.OutputFile != "output.mp4" {
			t.Errorf("Parse(%v): OutputFile = %q, want %q", args, result.OutputFile, "output.mp4")
		}
		opt, val := args[2], args[3]
		found := false
		for i, a := range result.AllArgs {
			if a == opt && i+1 < len(result.AllArgs) && result.AllArgs[i+1] == val {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("Parse(%v): AllArgs = %v, want %s %s preserved", args, result.AllArgs, opt, val)
		}
	}
}

// TestParseIPrefixedDemuxerOption pins the fix for demuxer
// AVOptions absent from `ffmpeg -h long`: "-input_format" starts with "-i"
// and must parse as a value-taking option, not as an input path that would
// silently drop "mjpeg".
func TestParseIPrefixedDemuxerOption(t *testing.T) {
	p := NewParser()
	result, err := p.Parse([]string{"-f", "v4l2", "-input_format", "mjpeg", "-i", "/dev/video0", "out.mp4"})
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if result.OutputFile != "out.mp4" {
		t.Errorf("OutputFile = %q, want %q", result.OutputFile, "out.mp4")
	}
	if len(result.InputFiles) != 1 || result.InputFiles[0] != "/dev/video0" {
		t.Errorf("InputFiles = %v, want [%q]", result.InputFiles, "/dev/video0")
	}
	found := false
	for i, a := range result.AllArgs {
		if a == "-input_format" && i+1 < len(result.AllArgs) && result.AllArgs[i+1] == "mjpeg" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("AllArgs = %v, want -input_format mjpeg preserved", result.AllArgs)
	}
}

// TestParseIPrefixedOptionsAreNotInputs pins the fix: only a bare "-i" names
// an input, exactly as in ffmpeg. Encoder-private and codec options whose
// names merely start with "-i" ("-intra-refresh", "-idr_interval",
// "-i_qfactor") are value-taking options and must keep their values.
func TestParseIPrefixedOptionsAreNotInputs(t *testing.T) {
	p := NewParser()
	options := [][2]string{
		{"-intra-refresh", "1"},
		{"-idr_interval", "2"},
		{"-i_qfactor", "0.71"},
	}
	for _, opt := range options {
		args := []string{"-i", "in.mp4", opt[0], opt[1], "out.mp4"}
		result, err := p.Parse(args)
		if err != nil {
			t.Errorf("Parse(%v): error = %v", args, err)
			continue
		}
		if len(result.InputFiles) != 1 || result.InputFiles[0] != "in.mp4" {
			t.Errorf("Parse(%v): InputFiles = %v, want [in.mp4]", args, result.InputFiles)
		}
		if result.OutputFile != "out.mp4" {
			t.Errorf("Parse(%v): OutputFile = %q, want out.mp4", args, result.OutputFile)
		}
		found := false
		for i, a := range result.AllArgs {
			if a == opt[0] && i+1 < len(result.AllArgs) && result.AllArgs[i+1] == opt[1] {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("Parse(%v): AllArgs = %v, want %s %s preserved", args, result.AllArgs, opt[0], opt[1])
		}
	}
}

// TestParseRejectsMultipleOutputs pins the fix: a second output used to be
// dropped silently (exit 0, no file produced). It is now a hard error.
func TestParseRejectsMultipleOutputs(t *testing.T) {
	p := NewParser()
	cases := [][]string{
		{"-i", "a.mp4", "o1.mp4", "o2.mp4"},
		{"-i", "a.mp4", "o1.mp4", "-c:v", "libx264", "o2.mp4"},
		{"-i", "a.mp4", "-o", "o1.mp4", "o2.mp4"},
		{"-i", "a.mp4", "o1.mp4", "-o", "o2.mp4"},
		// A positional before -i is an output, not an implicit input:
		// ffmpeg writes both positionals and rffmpeg must reject the extra.
		{"o1.mp4", "-i", "src.mp4", "o2.mp4"},
		{"o1.mp4", "o2.mp4", "-i", "src.mp4"},
	}
	for _, args := range cases {
		_, err := p.Parse(args)
		if !errors.Is(err, ErrMultipleOutputs) {
			t.Errorf("Parse(%v): error = %v, want ErrMultipleOutputs", args, err)
		}
	}
}

// TestParseOutputBeforeExplicitInput pins the other side of the pre-input
// rule: a lone positional before -i is the output, not an input. Only a
// second one makes the invocation ambiguous.
func TestParseOutputBeforeExplicitInput(t *testing.T) {
	p := NewParser()
	result, err := p.Parse([]string{"out.mp4", "-i", "in.mp4"})
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if len(result.InputFiles) != 1 || result.InputFiles[0] != "in.mp4" {
		t.Errorf("InputFiles = %v, want [in.mp4]", result.InputFiles)
	}
	if result.OutputFile != "out.mp4" {
		t.Errorf("OutputFile = %q, want out.mp4", result.OutputFile)
	}
}

func TestParseAllArgsPreserved(t *testing.T) {
	args := []string{"-i", "input.mp4", "-c:v", "libx264", "-preset", "fast", "output.mp4"}
	p := NewParser()
	result, err := p.Parse(args)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}

	// Check that codec and preset are preserved
	expectedArgs := map[string]bool{
		"-c:v":         false,
		"libx264":      false,
		"-preset":      false,
		"fast":         false,
		"-i":           false,
		"<INPUT_FILE>": false,
	}

	for _, arg := range result.AllArgs {
		if _, ok := expectedArgs[arg]; ok {
			expectedArgs[arg] = true
		}
	}

	for arg, found := range expectedArgs {
		if !found {
			t.Errorf("Expected arg %s not found in AllArgs", arg)
		}
	}
}

// TestParseRejectsInvocationWithoutInput pins the parse-time rejection: a
// bare positional is an output, never a source, so an invocation naming no -i
// is refused before submission. The error carries the -i fix, and a missing
// input outranks an extra output candidate.
func TestParseRejectsInvocationWithoutInput(t *testing.T) {
	p := NewParser()
	cases := [][]string{
		{"input.mp4", "out.mp4"},
		{"-c:v", "libx264", "input.mp4"},
		{"input.mp4", "out1.mp4", "out2.mp4"},
		{"-o", "out1.mp4", "-o", "out2.mp4"},
	}
	for _, args := range cases {
		_, err := p.Parse(args)
		if !errors.Is(err, ErrNoInputFile) {
			t.Errorf("Parse(%v): error = %v, want ErrNoInputFile", args, err)
			continue
		}
		if !strings.Contains(err.Error(), "-i") {
			t.Errorf("Parse(%v): error = %q, want an -i repair hint", args, err)
		}
	}
}

func TestParseStreamingDashExcludedFromAllArgs(t *testing.T) {
	p := NewParser()
	tests := []struct {
		name string
		args []string
	}{
		{name: "format then stdout", args: []string{"-i", "input.mp4", "-f", "mp4", "-"}},
		{name: "explicit -o stdout", args: []string{"-i", "input.mp4", "-o", "-"}},
		{name: "bare stdout", args: []string{"-i", "input.mp4", "-"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := p.Parse(tt.args)
			if err != nil {
				t.Fatalf("Parse() error = %v", err)
			}

			if !result.StreamingOutput {
				t.Errorf("StreamingOutput = false, want true for output '-'")
			}
			if result.OutputFile != "-" {
				t.Errorf("OutputFile = %q, want '-'", result.OutputFile)
			}

			// The output token "-" must not leak into AllArgs: the worker
			// resolves and appends the output path exactly once. A leaked "-"
			// would produce "- -" in the exec args.
			for _, arg := range result.AllArgs {
				if arg == "-" {
					t.Errorf("AllArgs must not contain the output dash '-': %v", result.AllArgs)
				}
			}
		})
	}
}

func TestParsePipe1NormalizedToStreamingOutput(t *testing.T) {
	p := NewParser()
	tests := []struct {
		name string
		args []string
	}{
		{name: "positional pipe:1", args: []string{"-i", "input.mp4", "-f", "mp4", "pipe:1"}},
		{name: "positional PIPE:1", args: []string{"-i", "input.mp4", "-f", "mp4", "PIPE:1"}},
		{name: "positional Pipe:1", args: []string{"-i", "input.mp4", "-f", "mp4", "Pipe:1"}},
		{name: "explicit -o pipe:1", args: []string{"-i", "input.mp4", "-f", "mp4", "-o", "pipe:1"}},
		{name: "bare pipe:1 without -f", args: []string{"-i", "input.mp4", "pipe:1"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := p.Parse(tt.args)
			if err != nil {
				t.Fatalf("Parse() error = %v", err)
			}

			// pipe:1 (stdout) must be recognized as a streaming job, exactly
			// as if the caller had written "-".
			if !result.StreamingOutput {
				t.Errorf("StreamingOutput = false, want true for output 'pipe:1'")
			}
			if result.OutputFile != "-" {
				t.Errorf("OutputFile = %q, want '-'", result.OutputFile)
			}

			// The normalized token must not leak into AllArgs: the worker
			// appends the output path exactly once.
			for _, arg := range result.AllArgs {
				if strings.EqualFold(arg, "pipe:1") || arg == "-" {
					t.Errorf("AllArgs must not contain the output token %q: %v", arg, result.AllArgs)
				}
			}
		})
	}
}

func TestParsePipeStdinNotNormalized(t *testing.T) {
	p := NewParser()
	// "pipe:" (no fd, or fd 0) is stdin, NOT stdout: it must not be mistaken
	// for a streaming output and rewritten to "-".
	for _, out := range []string{"pipe:", "pipe:0"} {
		result, err := p.Parse([]string{"-i", "input.mp4", "-f", "mp4", out})
		if err != nil {
			t.Fatalf("Parse(%q) error = %v", out, err)
		}
		if result.StreamingOutput {
			t.Errorf("StreamingOutput = true for %q, want false", out)
		}
		if result.OutputFile != out {
			t.Errorf("OutputFile = %q, want %q", result.OutputFile, out)
		}
	}
}

func TestParsePipe1AsInputNotNormalized(t *testing.T) {
	p := NewParser()
	// pipe:1 in the INPUT role (via -i) reads from stdin and must be left
	// untouched: only the OUTPUT path is normalized to "-". The output here is
	// a real file, so StreamingOutput stays false and InputFiles keeps "pipe:1"
	// verbatim.
	result, err := p.Parse([]string{"-i", "pipe:1", "out.mp4"})
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if result.StreamingOutput {
		t.Errorf("StreamingOutput = true, want false for input 'pipe:1' with file output")
	}
	if len(result.InputFiles) != 1 || result.InputFiles[0] != "pipe:1" {
		t.Errorf("InputFiles = %v, want [\"pipe:1\"]", result.InputFiles)
	}
	if result.OutputFile != "out.mp4" {
		t.Errorf("OutputFile = %q, want %q", result.OutputFile, "out.mp4")
	}
}

func TestIsFfmpegCommand(t *testing.T) {
	tests := []struct {
		args []string
		want bool
	}{
		{[]string{"ffmpeg", "-i", "input.mp4"}, true},
		{[]string{"rffmpeg", "-i", "input.mp4"}, true},
		{[]string{"/usr/bin/ffmpeg", "-i", "input.mp4"}, true},
		{[]string{"vlc", "video.mp4"}, false},
		{[]string{}, false},
	}

	for _, tt := range tests {
		got := IsFfmpegCommand(tt.args)
		if got != tt.want {
			t.Errorf("IsFfmpegCommand(%v) = %v, want %v", tt.args, got, tt.want)
		}
	}
}

func TestStripFileScheme(t *testing.T) {
	tests := []struct {
		name         string
		uri          string
		wantPath     string
		wantStripped bool
	}{
		{
			name:         "file:// URI",
			uri:          "file:///path/to/file.mp4",
			wantPath:     "/path/to/file.mp4",
			wantStripped: true,
		},
		{
			name:         "file:// URI with host",
			uri:          "file://localhost/path/to/file.mp4",
			wantPath:     "localhost/path/to/file.mp4",
			wantStripped: true,
		},
		{
			name:         "plain path",
			uri:          "/path/to/file.mp4",
			wantPath:     "/path/to/file.mp4",
			wantStripped: false,
		},
		{
			name:         "http:// URI",
			uri:          "http://example.com/file.mp4",
			wantPath:     "http://example.com/file.mp4",
			wantStripped: false,
		},
		{
			name:         "empty string",
			uri:          "",
			wantPath:     "",
			wantStripped: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotPath, gotStripped := StripFileScheme(tt.uri)
			if gotPath != tt.wantPath {
				t.Errorf("StripFileScheme() path = %v, want %v", gotPath, tt.wantPath)
			}
			if gotStripped != tt.wantStripped {
				t.Errorf("StripFileScheme() stripped = %v, want %v", gotStripped, tt.wantStripped)
			}
		})
	}
}

func TestParseFileScheme(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantInputs []string
		wantOutput string
		wantErr    bool
	}{
		{
			name:       "file:// URI as input",
			args:       []string{"-i", "file:///path/to/video.mp4", "output.mp4"},
			wantInputs: []string{"file:///path/to/video.mp4"},
			wantOutput: "output.mp4",
			wantErr:    false,
		},
		{
			name:       "multiple inputs with file://",
			args:       []string{"-i", "file:///path/to/video.mp4", "-i", "audio.mp3", "output.mp4"},
			wantInputs: []string{"file:///path/to/video.mp4", "audio.mp3"},
			wantOutput: "output.mp4",
			wantErr:    false,
		},
	}

	p := NewParser()

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := p.Parse(tt.args)
			if (err != nil) != tt.wantErr {
				t.Errorf("Parse() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.wantErr {
				return
			}
			if len(result.InputFiles) != len(tt.wantInputs) {
				t.Errorf("Parse() got %d inputs, want %d", len(result.InputFiles), len(tt.wantInputs))
				return
			}
			for i, input := range result.InputFiles {
				if input != tt.wantInputs[i] {
					t.Errorf("Parse() input[%d] = %v, want %v", i, input, tt.wantInputs[i])
				}
			}
			if result.OutputFile != tt.wantOutput {
				t.Errorf("Parse() output = %v, want %v", result.OutputFile, tt.wantOutput)
			}
		})
	}
}

// TestValidateFileExists tests the file existence validation.
func TestValidateFileExists(t *testing.T) {
	// Create a temporary file for testing
	tmpFile, err := os.CreateTemp(t.TempDir(), "test-*.txt")
	if err != nil {
		t.Fatalf("failed to create temp file: %v", err)
	}
	tmpFile.Close()

	tests := []struct {
		name    string
		path    string
		wantErr bool
		errMsg  string
	}{
		{
			name:    "existing file",
			path:    tmpFile.Name(),
			wantErr: false,
		},
		{
			name:    "nonexistent file",
			path:    filepath.Join(t.TempDir(), "nonexistent.mp4"),
			wantErr: true,
			errMsg:  "file not found",
		},
		{
			name:    "directory",
			path:    t.TempDir(),
			wantErr: true,
			errMsg:  "expected a file but got a directory",
		},
		{
			name:    "existing file with file:// prefix",
			path:    "file://" + tmpFile.Name(),
			wantErr: false,
		},
		{
			name:    "nonexistent file with file:// prefix",
			path:    "file://" + filepath.Join(t.TempDir(), "nonexistent2.mp4"),
			wantErr: true,
			errMsg:  "file not found",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateFileExists(tt.path)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateFileExists(%q) error = %v, wantErr %v", tt.path, err, tt.wantErr)
				return
			}
			if tt.wantErr && err != nil {
				if tt.errMsg != "" && !strings.Contains(err.Error(), tt.errMsg) {
					t.Errorf("ValidateFileExists(%q) error = %v, want error containing %q", tt.path, err, tt.errMsg)
				}
			}
		})
	}
}
