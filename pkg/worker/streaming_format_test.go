package worker

import (
	"reflect"
	"testing"
)

func TestApplyStreamableFormat(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		outputPath string
		want       []string
		wantNote   bool
	}{
		{
			name:       "mp4 to stdout gets fragmentation flags",
			args:       []string{"-y", "-i", "in.mp4", "-c:v", "h264_qsv", "-f", "mp4", "-"},
			outputPath: "-",
			want: []string{"-y", "-i", "in.mp4", "-c:v", "h264_qsv", "-f", "mp4",
				"-movflags", "+empty_moov+frag_keyframe+default_base_moof", "-"},
			wantNote: true,
		},
		{
			name:       "mov to stdout gets fragmentation flags",
			args:       []string{"-y", "-i", "in.mp4", "-f", "mov", "-"},
			outputPath: "-",
			want: []string{"-y", "-i", "in.mp4", "-f", "mov",
				"-movflags", "+empty_moov+frag_keyframe+default_base_moof", "-"},
			wantNote: true,
		},
		{
			name:       "explicit movflags are preserved",
			args:       []string{"-y", "-i", "in.mp4", "-f", "mp4", "-movflags", "+faststart", "-"},
			outputPath: "-",
			want:       []string{"-y", "-i", "in.mp4", "-f", "mp4", "-movflags", "+faststart", "-"},
			wantNote:   false,
		},
		{
			name:       "mpegts untouched",
			args:       []string{"-y", "-i", "in.mp4", "-f", "mpegts", "-"},
			outputPath: "-",
			want:       []string{"-y", "-i", "in.mp4", "-f", "mpegts", "-"},
			wantNote:   false,
		},
		{
			name:       "input-only lavfi format not mistaken for output mp4",
			args:       []string{"-y", "-f", "lavfi", "-i", "testsrc=duration=0.1", "-c:v", "libx264", "out.mp4"},
			outputPath: "/tmp/jobs/job-1/out.mp4",
			want:       []string{"-y", "-f", "lavfi", "-i", "testsrc=duration=0.1", "-c:v", "libx264", "out.mp4"},
			wantNote:   false,
		},
		{
			name:       "non-streaming output untouched even with -f mp4",
			args:       []string{"-y", "-i", "in.mp4", "-c:v", "libx264", "-f", "mp4", "out.mp4"},
			outputPath: "/tmp/jobs/job-1/out.mp4",
			want:       []string{"-y", "-i", "in.mp4", "-c:v", "libx264", "-f", "mp4", "out.mp4"},
			wantNote:   false,
		},
		{
			name:       "streaming with lavfi input only output mp4 gets flags",
			args:       []string{"-y", "-f", "lavfi", "-i", "testsrc=duration=0.1", "-c:v", "libx264", "-f", "mp4", "-"},
			outputPath: "-",
			want: []string{"-y", "-f", "lavfi", "-i", "testsrc=duration=0.1", "-c:v", "libx264", "-f", "mp4",
				"-movflags", "+empty_moov+frag_keyframe+default_base_moof", "-"},
			wantNote: true,
		},
		{
			name:       "no format flag untouched",
			args:       []string{"-y", "-i", "in.mp4", "-c:v", "libx264", "-"},
			outputPath: "-",
			want:       []string{"-y", "-i", "in.mp4", "-c:v", "libx264", "-"},
			wantNote:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, note := applyStreamableFormat(tt.args, tt.outputPath)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("args = %v, want %v", got, tt.want)
			}
			if (note != "") != tt.wantNote {
				t.Errorf("note = %q, wantNote = %v", note, tt.wantNote)
			}
		})
	}
}

func TestApplyStreamableFormatDoesNotMutateInput(t *testing.T) {
	original := []string{"-y", "-i", "in.mp4", "-f", "mp4", "-"}
	before := append([]string(nil), original...)

	got, _ := applyStreamableFormat(original, "-")

	if !reflect.DeepEqual(original, before) {
		t.Errorf("input args mutated: %v, want %v", original, before)
	}
	if len(got) != len(original)+2 {
		t.Errorf("expected 2 injected flags, got %d extra", len(got)-len(original))
	}
}
