package args

import (
	"strings"
	"testing"
)

func TestValidateStreamingOutputFormat(t *testing.T) {
	tests := []struct {
		name       string
		allArgs    []string
		wantFormat string
		wantOK     bool
	}{
		{
			name:       "mp4 is auto-fixed by worker",
			allArgs:    []string{"-i", "<INPUT_FILE>", "-f", "mp4"},
			wantFormat: "mp4",
			wantOK:     true,
		},
		{
			name:       "mov is auto-fixed by worker",
			allArgs:    []string{"-i", "<INPUT_FILE>", "-f", "mov"},
			wantFormat: "mov",
			wantOK:     true,
		},
		{
			name:       "mpegts streams fine",
			allArgs:    []string{"-i", "<INPUT_FILE>", "-f", "mpegts"},
			wantFormat: "mpegts",
			wantOK:     true,
		},
		{
			name:       "matroska streams fine",
			allArgs:    []string{"-i", "<INPUT_FILE>", "-f", "matroska"},
			wantFormat: "matroska",
			wantOK:     true,
		},
		{
			name:       "avif cannot be streamed",
			allArgs:    []string{"-i", "<INPUT_FILE>", "-f", "avif"},
			wantFormat: "avif",
			wantOK:     false,
		},
		{
			name:       "ipod cannot be streamed",
			allArgs:    []string{"-i", "<INPUT_FILE>", "-f", "ipod"},
			wantFormat: "ipod",
			wantOK:     false,
		},
		{
			name:       "m4a cannot be streamed",
			allArgs:    []string{"-i", "<INPUT_FILE>", "-f", "m4a"},
			wantFormat: "m4a",
			wantOK:     false,
		},
		{
			name:       "aac is not a muxer",
			allArgs:    []string{"-i", "<INPUT_FILE>", "-f", "aac"},
			wantFormat: "aac",
			wantOK:     false,
		},
		{
			name:       "3gp cannot be streamed",
			allArgs:    []string{"-i", "<INPUT_FILE>", "-f", "3gp"},
			wantFormat: "3gp",
			wantOK:     false,
		},
		{
			name:       "3g2 cannot be streamed",
			allArgs:    []string{"-i", "<INPUT_FILE>", "-f", "3g2"},
			wantFormat: "3g2",
			wantOK:     false,
		},
		{
			name:       "tg2 cannot be streamed",
			allArgs:    []string{"-i", "<INPUT_FILE>", "-f", "tg2"},
			wantFormat: "tg2",
			wantOK:     false,
		},
		{
			name:       "bare dash has no detectable format",
			allArgs:    []string{"-i", "<INPUT_FILE>"},
			wantFormat: "",
			wantOK:     false,
		},
		{
			name:       "format case is normalized",
			allArgs:    []string{"-i", "<INPUT_FILE>", "-f", "MP4"},
			wantFormat: "mp4",
			wantOK:     true,
		},
		{
			name:       "last output -f wins",
			allArgs:    []string{"-i", "<INPUT_FILE>", "-f", "mpegts", "-f", "avif"},
			wantFormat: "avif",
			wantOK:     false,
		},
		{
			name:       "input-section -f before last -i is ignored",
			allArgs:    []string{"-f", "lavfi", "-i", "<INPUT_FILE>", "-f", "mpegts"},
			wantFormat: "mpegts",
			wantOK:     true,
		},
		{
			name:       "input-section -f is only unsafe one before -i",
			allArgs:    []string{"-f", "avif", "-i", "<INPUT_FILE>", "-f", "mpegts"},
			wantFormat: "mpegts",
			wantOK:     true,
		},
		{
			name:       "no -i pair scans whole tail",
			allArgs:    []string{"-f", "avif"},
			wantFormat: "avif",
			wantOK:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotFormat, gotOK := ValidateStreamingOutputFormat(tt.allArgs)
			if gotFormat != tt.wantFormat {
				t.Errorf("ValidateStreamingOutputFormat() format = %q, want %q", gotFormat, tt.wantFormat)
			}
			if gotOK != tt.wantOK {
				t.Errorf("ValidateStreamingOutputFormat() ok = %v, want %v", gotOK, tt.wantOK)
			}
		})
	}
}

func TestValidateStreamingOutputFormatDoesNotMutateInput(t *testing.T) {
	args := []string{"-i", "<INPUT_FILE>", "-f", "avif"}
	original := append([]string(nil), args...)

	ValidateStreamingOutputFormat(args)

	for i := range args {
		if args[i] != original[i] {
			t.Fatalf("ValidateStreamingOutputFormat mutated input at %d: %q -> %q", i, original[i], args[i])
		}
	}
}

func TestStreamingOutputError(t *testing.T) {
	p := NewParser()

	tests := []struct {
		name       string
		args       []string
		wantSubstr string
		wantEmpty  bool
	}{
		{
			name:      "file output never errors",
			args:      []string{"-i", "input.mp4", "-f", "avif", "output.avif"},
			wantEmpty: true,
		},
		{
			name:      "streamable mpegts passes",
			args:      []string{"-i", "input.mp4", "-f", "mpegts", "-"},
			wantEmpty: true,
		},
		{
			name:      "streamable mp4 passes",
			args:      []string{"-i", "input.mp4", "-f", "mp4", "-"},
			wantEmpty: true,
		},
		{
			name:       "3gp reports format",
			args:       []string{"-i", "input.mp4", "-f", "3gp", "-"},
			wantSubstr: "format 3gp",
		},
		{
			name:       "3g2 reports format",
			args:       []string{"-i", "input.mp4", "-f", "3g2", "-"},
			wantSubstr: "format 3g2",
		},
		{
			name:       "tg2 reports format",
			args:       []string{"-i", "input.mp4", "-f", "tg2", "-"},
			wantSubstr: "format tg2",
		},
		{
			name:       "bare dash reports no muxer",
			args:       []string{"-i", "input.mp4", "-"},
			wantSubstr: `ffmpeg cannot auto-detect a muxer`,
		},
		{
			name:       "dash via -o reports no muxer",
			args:       []string{"-i", "input.mp4", "-o", "-"},
			wantSubstr: `ffmpeg cannot auto-detect a muxer`,
		},
		{
			name:       "avif reports format",
			args:       []string{"-i", "input.mp4", "-f", "avif", "-"},
			wantSubstr: "format avif",
		},
		{
			name:       "aac reports format",
			args:       []string{"-i", "input.mp4", "-f", "aac", "-"},
			wantSubstr: "format aac",
		},
		{
			name:       "input-section -f alone reports no muxer",
			args:       []string{"-f", "lavfi", "-i", "testsrc", "-"},
			wantSubstr: `ffmpeg cannot auto-detect a muxer`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := p.Parse(tt.args)
			if err != nil {
				t.Fatalf("Parse(%v) error = %v", tt.args, err)
			}
			msg := StreamingOutputError(result)
			if tt.wantEmpty {
				if msg != "" {
					t.Errorf("StreamingOutputError() = %q, want empty", msg)
				}
				return
			}
			if msg == "" {
				t.Fatalf("StreamingOutputError() = empty, want message containing %q", tt.wantSubstr)
			}
			if !strings.Contains(msg, tt.wantSubstr) {
				t.Errorf("StreamingOutputError() = %q, want containing %q", msg, tt.wantSubstr)
			}
		})
	}
}
