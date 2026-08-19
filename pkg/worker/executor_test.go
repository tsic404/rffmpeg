package worker

import (
	"reflect"
	"testing"
)

func TestBuildArgs(t *testing.T) {
	tests := []struct {
		name       string
		jobArgs    []string
		inputPaths []string
		outputPath string
		want       []string
	}{
		{
			name:       "single input with placeholder",
			jobArgs:    []string{"-i", "<INPUT_FILE>", "-c:v", "libx264"},
			inputPaths: []string{"/tmp/downloaded/input.mp4"},
			outputPath: "/tmp/output.mp4",
			want:       []string{"-y", "-i", "/tmp/downloaded/input.mp4", "-c:v", "libx264", "/tmp/output.mp4"},
		},
		{
			name:       "multiple inputs with placeholders",
			jobArgs:    []string{"-i", "<INPUT_FILE>", "-i", "<INPUT_FILE>", "-filter_complex", "concat", "-c:v", "libx264"},
			inputPaths: []string{"/tmp/downloaded/input1.mp4", "/tmp/downloaded/input2.mp4"},
			outputPath: "/tmp/output.mp4",
			want:       []string{"-y", "-i", "/tmp/downloaded/input1.mp4", "-i", "/tmp/downloaded/input2.mp4", "-filter_complex", "concat", "-c:v", "libx264", "/tmp/output.mp4"},
		},
		{
			name:       "no placeholder - passthrough args",
			jobArgs:    []string{"-f", "lavfi", "-i", "testsrc=duration=10", "-c:v", "libx264"},
			inputPaths: []string{},
			outputPath: "/tmp/output.mp4",
			want:       []string{"-y", "-f", "lavfi", "-i", "testsrc=duration=10", "-c:v", "libx264", "/tmp/output.mp4"},
		},
		{
			name:       "output already specified in jobArgs",
			jobArgs:    []string{"-i", "<INPUT_FILE>", "-c:v", "libx264", "/tmp/custom_output.mp4"},
			inputPaths: []string{"/tmp/downloaded/input.mp4"},
			outputPath: "/tmp/output.mp4",
			want:       []string{"-y", "-i", "/tmp/downloaded/input.mp4", "-c:v", "libx264", "/tmp/custom_output.mp4"},
		},
		{
			name:       "empty jobArgs",
			jobArgs:    []string{},
			inputPaths: []string{"/tmp/downloaded/input.mp4"},
			outputPath: "/tmp/output.mp4",
			want:       []string{"-y", "/tmp/output.mp4"},
		},
		{
			name:       "concatenated -i style placeholder",
			jobArgs:    []string{"-i", "<INPUT_FILE>", "-c:a", "aac"},
			inputPaths: []string{"/tmp/downloaded/audio.mp3"},
			outputPath: "/tmp/output.aac",
			want:       []string{"-y", "-i", "/tmp/downloaded/audio.mp3", "-c:a", "aac", "/tmp/output.aac"},
		},
		{
			name:       "args with -- separator at end (user mistake)",
			jobArgs:    []string{"-i", "<INPUT_FILE>", "-c:v", "libx264", "--"},
			inputPaths: []string{"/tmp/input.mp4"},
			outputPath: "/tmp/output.mp4",
			want:       []string{"-y", "-i", "/tmp/input.mp4", "-c:v", "libx264", "--", "/tmp/output.mp4"},
		},
		{
			name:       "args with -- separator and output after",
			jobArgs:    []string{"-i", "<INPUT_FILE>", "--", "output.mp4"},
			inputPaths: []string{"/tmp/input.mp4"},
			outputPath: "/tmp/output.mp4",
			want:       []string{"-y", "-i", "/tmp/input.mp4", "--", "output.mp4"},
		},
		{
			name:       "audio extraction with vn flag",
			jobArgs:    []string{"-i", "<INPUT_FILE>", "-vn", "-c:a", "libmp3lame"},
			inputPaths: []string{"/tmp/downloaded/input.mp4"},
			outputPath: "/tmp/output.mp3",
			want:       []string{"-y", "-i", "/tmp/downloaded/input.mp4", "-vn", "-c:a", "libmp3lame", "/tmp/output.mp3"},
		},
		{
			name:       "multiple inputs with -shortest boolean flag",
			jobArgs:    []string{"-i", "<INPUT_FILE>", "-i", "<INPUT_FILE>", "-c:v", "libx264", "-c:a", "aac", "-shortest"},
			inputPaths: []string{"/tmp/downloaded/video.mp4", "/tmp/downloaded/audio.m4a"},
			outputPath: "/tmp/output/merged.mp4",
			want:       []string{"-y", "-i", "/tmp/downloaded/video.mp4", "-i", "/tmp/downloaded/audio.m4a", "-c:v", "libx264", "-c:a", "aac", "-shortest", "/tmp/output/merged.mp4"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := BuildArgs(tt.jobArgs, tt.inputPaths, tt.outputPath)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("BuildArgs() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestHasOutputArg(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want bool
	}{
		{
			name: "has output file",
			args: []string{"-i", "input.mp4", "-c:v", "libx264", "output.mp4"},
			want: true,
		},
		{
			name: "no output file",
			args: []string{"-i", "input.mp4", "-c:v", "libx264"},
			want: false,
		},
		{
			name: "empty args",
			args: []string{},
			want: false,
		},
		{
			name: "only flags with values",
			args: []string{"-i", "input.mp4", "-c:v", "libx264", "-b:v", "1M"},
			want: false,
		},
		{
			name: "output after flags",
			args: []string{"-i", "input.mp4", "output.mp4"},
			want: true,
		},
		{
			name: "vn flag followed by codec flag - no output",
			args: []string{"-i", "input.mp4", "-vn", "-c:a", "libmp3lame"},
			want: false,
		},
		{
			name: "an flag followed by codec flag - no output",
			args: []string{"-i", "input.mp4", "-an", "-c:v", "libx264"},
			want: false,
		},
		{
			name: "boolean flags followed by output",
			args: []string{"-i", "input.mp4", "-vn", "-an", "output.mp4"},
			want: true,
		},
		{
			name: "vn flag with output",
			args: []string{"-i", "input.mp4", "-vn", "-c:a", "libmp3lame", "output.mp3"},
			want: true,
		},
		{
			name: "multiple boolean flags and codec",
			args: []string{"-i", "input.mp4", "-vn", "-sn", "-dn", "-c:a", "aac", "-b:a", "128k"},
			want: false,
		},
		{
			name: "y flag (overwrite) followed by output",
			args: []string{"-i", "input.mp4", "-y", "output.mp4"},
			want: true,
		},
		{
			name: "n flag (no overwrite) followed by output",
			args: []string{"-i", "input.mp4", "-n", "output.mp4"},
			want: true,
		},
		{
			name: "map flag takes a value - no output",
			args: []string{"-i", "input.mp4", "-map", "0:v", "-c:v", "libx264"},
			want: false,
		},
		{
			name: "map flag with output after value",
			args: []string{"-i", "input.mp4", "-map", "0:v", "output.mp4"},
			want: true,
		},
		{
			name: "map flag with stream specifier takes a value",
			args: []string{"-i", "input.mp4", "-map", "0:a:0", "-c:a", "aac"},
			want: false,
		},
		{
			name: "equals syntax - flag with value attached",
			args: []string{"-i", "input.mp4", "-loglevel=verbose", "-c:v", "libx264"},
			want: false,
		},
		{
			name: "equals syntax followed by output",
			args: []string{"-i", "input.mp4", "-loglevel=verbose", "output.mp4"},
			want: true,
		},
		{
			name: "multiple equals syntax flags - no output",
			args: []string{"-i", "input.mp4", "-loglevel=verbose", "-c:v=libx264"},
			want: false,
		},
		{
			name: "equals syntax for codec with output",
			args: []string{"-i", "input.mp4", "-c:v=libx264", "output.mp4"},
			want: true,
		},
		{
			name: "shortest boolean flag followed by output",
			args: []string{"-i", "input.mp4", "-c:v", "libx264", "-c:a", "aac", "-shortest", "output.mp4"},
			want: true,
		},
		{
			name: "multiple inputs with shortest and output",
			args: []string{"-i", "video.mp4", "-i", "audio.mp4", "-c:v", "libx264", "-c:a", "aac", "-shortest", "merged.mp4"},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := hasOutputArg(tt.args)
			if got != tt.want {
				t.Errorf("hasOutputArg() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestFindSeparatorIndex(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want int
	}{
		{
			name: "no separator",
			args: []string{"-c:v", "libx264"},
			want: -1,
		},
		{
			name: "separator at end",
			args: []string{"-c:v", "libx264", "--"},
			want: 2,
		},
		{
			name: "separator at beginning",
			args: []string{"--", "output.mp4"},
			want: 0,
		},
		{
			name: "separator in middle",
			args: []string{"-i", "input.mp4", "--", "output.mp4"},
			want: 2,
		},
		{
			name: "empty args",
			args: []string{},
			want: -1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := findSeparatorIndex(tt.args); got != tt.want {
				t.Errorf("findSeparatorIndex(%v) = %v, want %v", tt.args, got, tt.want)
			}
		})
	}
}
