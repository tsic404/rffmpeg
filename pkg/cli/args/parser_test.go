package args

import (
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
			name:       "concatenated -i option",
			args:       []string{"-iinput.mp4", "output.mp4"},
			wantInputs: []string{"input.mp4"},
			wantOutput: "output.mp4",
			wantErr:    false,
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

func TestParseImplicitInputAllArgs(t *testing.T) {
	p := NewParser()
	result, err := p.Parse([]string{"-c:v", "libx264", "input.mp4", "out.mp4"})
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}

	if len(result.InputFiles) != 1 || result.InputFiles[0] != "input.mp4" {
		t.Errorf("InputFiles = %v, want [input.mp4]", result.InputFiles)
	}
	if result.OutputFile != "out.mp4" {
		t.Errorf("OutputFile = %q, want out.mp4", result.OutputFile)
	}

	// The implicit input must be rewritten to the -i <INPUT_FILE> placeholder
	// form so the worker can substitute the real path (TSI-2749).
	want := []string{"-c:v", "libx264", "-i", "<INPUT_FILE>"}
	if len(result.AllArgs) != len(want) {
		t.Fatalf("AllArgs = %v, want %v", result.AllArgs, want)
	}
	for i := range want {
		if result.AllArgs[i] != want[i] {
			t.Errorf("AllArgs[%d] = %q, want %q", i, result.AllArgs[i], want[i])
		}
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
