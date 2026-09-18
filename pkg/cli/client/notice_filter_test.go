package client

import (
	"strings"
	"testing"
)

func TestNoticeFilterKeepsOnlyRffmpegLines(t *testing.T) {
	tests := []struct {
		name   string
		chunks []string
		want   string
	}{
		{
			name:   "single notice line",
			chunks: []string{"[rffmpeg] Cache hit: deadbeef\n"},
			want:   "[rffmpeg] Cache hit: deadbeef\n",
		},
		{
			name: "notice interleaved with ffmpeg output",
			chunks: []string{
				"Input #0, mov,mp4:\n[rffmpeg] Cache hit: deadbeef\nframe= 100 fps=30\n",
			},
			want: "[rffmpeg] Cache hit: deadbeef\n",
		},
		{
			name: "ffmpeg output only is dropped",
			chunks: []string{
				"Input #0, mov,mp4:\nwarning: something\n",
			},
			want: "",
		},
		{
			name: "notice split across chunks is reassembled",
			chunks: []string{
				"[rffmpeg] Cache hit: dead",
				"beef\n",
			},
			want: "[rffmpeg] Cache hit: deadbeef\n",
		},
		{
			name: "trailing partial ffmpeg line is buffered and dropped",
			chunks: []string{
				"[rffmpeg] fallback to software encoder libx264\nframe=",
				" 42 fps=30\n",
			},
			want: "[rffmpeg] fallback to software encoder libx264\n",
		},
		{
			name: "multiple notices in one chunk",
			chunks: []string{
				"[rffmpeg] Cache hit: a\n[rffmpeg] upgraded libx264 → h264_nvenc\nnoise\n",
			},
			want: "[rffmpeg] Cache hit: a\n[rffmpeg] upgraded libx264 → h264_nvenc\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var f noticeFilter
			var sb strings.Builder
			for _, c := range tt.chunks {
				sb.WriteString(f.filter(c))
			}
			if got := sb.String(); got != tt.want {
				t.Errorf("filter() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestNoticeFilterNonRffmpegPrefixIsDropped(t *testing.T) {
	var f noticeFilter
	// A line that merely contains "[rffmpeg]" mid-line is not a notice.
	if got := f.filter("prefix [rffmpeg] not at line start\n"); got != "" {
		t.Errorf("filter() = %q, want empty", got)
	}
}
