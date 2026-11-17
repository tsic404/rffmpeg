package args

import (
	"strings"
	"testing"
)

func TestOutputExtensionWarning(t *testing.T) {
	p := NewParser()

	tests := []struct {
		name       string
		args       []string
		wantSubstr string
		wantEmpty  bool
	}{
		{
			name:      "known mp4 extension passes",
			args:      []string{"-i", "in.mp4", "out.mp4"},
			wantEmpty: true,
		},
		{
			name:      "known mkv extension passes",
			args:      []string{"-i", "in.mp4", "out.mkv"},
			wantEmpty: true,
		},
		{
			name:      "uppercase known extension passes",
			args:      []string{"-i", "in.mp4", "out.MP4"},
			wantEmpty: true,
		},
		{
			name:       "unknown extension warns",
			args:       []string{"-i", "in.mp4", "out.out"},
			wantSubstr: "not a recognized muxer extension",
		},
		{
			name:       "no extension warns",
			args:       []string{"-i", "in.mp4", "out"},
			wantSubstr: "not a recognized muxer extension",
		},
		{
			// A token the native tool does not define consumes no value, so the
			// trailing token is the output candidate (and its extension is what
			// the warning is about).
			name:       "unknown native name keeps the output candidate",
			args:       []string{"-i", "in.mp4", "--verbose", "out.out"},
			wantSubstr: "not a recognized muxer extension",
		},
		{
			name:      "explicit output -f skips warning",
			args:      []string{"-i", "in.mp4", "-f", "mp4", "out.out"},
			wantEmpty: true,
		},
		{
			// An input-section "-f" (before the last "-i") must not suppress
			// the warning: only an output-section "-f" selects the muxer.
			// This depends on ValidateStreamingOutputFormat's last-input scan.
			name:       "input-section -f does not skip warning",
			args:       []string{"-f", "lavfi", "-i", "in.mp4", "out.out"},
			wantSubstr: "not a recognized muxer extension",
		},
		{
			name:      "streaming output skips warning",
			args:      []string{"-i", "in.mp4", "-f", "mpegts", "-"},
			wantEmpty: true,
		},
		{
			name:      "remote URL output skips warning",
			args:      []string{"-i", "in.mp4", "rtmp://host/app/stream"},
			wantEmpty: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := p.Parse(tt.args)
			if err != nil {
				t.Fatalf("Parse(%v) error = %v", tt.args, err)
			}
			msg := OutputExtensionWarning(result)
			if tt.wantEmpty {
				if msg != "" {
					t.Errorf("OutputExtensionWarning() = %q, want empty", msg)
				}
				return
			}
			if msg == "" {
				t.Fatalf("OutputExtensionWarning() = empty, want message containing %q", tt.wantSubstr)
			}
			if !strings.Contains(msg, tt.wantSubstr) {
				t.Errorf("OutputExtensionWarning() = %q, want containing %q", msg, tt.wantSubstr)
			}
		})
	}
}
