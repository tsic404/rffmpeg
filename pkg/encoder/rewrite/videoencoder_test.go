package rewrite

import (
	"slices"
	"testing"

	"github.com/tsic404/rffmpeg/pkg/encoder"
)

func TestParseVideoEncoderFlags(t *testing.T) {
	caps := &HardwareCapabilities{
		AvailableEncoders: []encoder.EncoderFamily{
			encoder.EncoderH264NVENC,
			encoder.EncoderLibX264,
			encoder.EncoderFamily("av1_amf"),
		},
	}

	tests := []struct {
		name        string
		args        []string
		wantValue   string
		wantName    string
		wantGeneric bool
		wantOK      bool
	}{
		{name: "explicit -c:v", args: []string{"-c:v", "libx264"}, wantValue: "libx264", wantName: "c:v", wantOK: true},
		{name: "explicit -codec:v", args: []string{"-codec:v", "libx264"}, wantValue: "libx264", wantName: "codec:v", wantOK: true},
		{name: "explicit -vcodec", args: []string{"-vcodec", "h264_nvenc"}, wantValue: "h264_nvenc", wantName: "vcodec", wantOK: true},
		{name: "stream specifier -c:v:0", args: []string{"-c:v:0", "libx264"}, wantValue: "libx264", wantName: "c:v:0", wantOK: true},
		{name: "stream specifier -codec:v:2", args: []string{"-codec:v:2", "libx264"}, wantValue: "libx264", wantName: "codec:v:2", wantOK: true},
		{name: "= joined -c:v:0 is not a selection", args: []string{"-c:v:0=libx264"}, wantOK: false},
		{name: "= joined -vcodec is not a selection", args: []string{"-vcodec=h264_nvenc"}, wantOK: false},
		{name: "generic -c known video encoder", args: []string{"-c", "libx264"}, wantValue: "libx264", wantName: "c", wantGeneric: true, wantOK: true},
		{name: "generic -codec known video encoder", args: []string{"-codec", "libvpx-vp9"}, wantValue: "libvpx-vp9", wantName: "codec", wantGeneric: true, wantOK: true},
		{name: "= joined generic -c is not a selection", args: []string{"-c=libx264"}, wantOK: false},
		{name: "generic value reported by worker", args: []string{"-c", "av1_amf"}, wantValue: "av1_amf", wantName: "c", wantGeneric: true, wantOK: true},
		{name: "generic copy", args: []string{"-c", "copy"}, wantValue: "copy", wantName: "c", wantGeneric: true, wantOK: true},
		{name: "generic non-video encoder ignored", args: []string{"-c", "aac"}, wantOK: false},
		{name: "audio stream flag ignored", args: []string{"-c:a", "aac"}, wantOK: false},
		{name: "unknown generic encoder ignored", args: []string{"-c", "not_an_encoder"}, wantOK: false},
		{name: "after separator ignored", args: []string{"--", "-c:v", "libx264"}, wantOK: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			flags := ParseVideoEncoderFlags(tt.args, caps)
			if !tt.wantOK {
				if len(flags) != 0 {
					t.Fatalf("ParseVideoEncoderFlags(%v) = %v, want none", tt.args, flags)
				}
				return
			}
			if len(flags) != 1 {
				t.Fatalf("ParseVideoEncoderFlags(%v) = %v, want exactly one", tt.args, flags)
			}
			got := flags[0]
			if got.Value != tt.wantValue || got.Name != tt.wantName || got.Generic != tt.wantGeneric {
				t.Errorf("flag = %+v, want value=%q name=%q generic=%v", got, tt.wantValue, tt.wantName, tt.wantGeneric)
			}
		})
	}
}

func TestRewriteVideoEncoderFlagKeepsSpelling(t *testing.T) {
	tests := []struct {
		flag VideoEncoderFlag
		want []string
	}{
		{flag: VideoEncoderFlag{Name: "c:v:0"}, want: []string{"-c:v:0", "h264_nvenc"}},
		{flag: VideoEncoderFlag{Name: "codec:v:1"}, want: []string{"-codec:v:1", "h264_nvenc"}},
		{flag: VideoEncoderFlag{Name: "c", Generic: true}, want: []string{"-c:v", "h264_nvenc"}},
	}
	for _, tt := range tests {
		got := tt.flag.RewrittenTokens(encoder.EncoderH264NVENC)
		if !slices.Equal(got, tt.want) {
			t.Errorf("%+v.RewrittenTokens = %v, want %v", tt.flag, got, tt.want)
		}
	}
}

func TestHasNoVideoOutput(t *testing.T) {
	if !HasNoVideoOutput([]string{"-i", "in.mp4", "-vn", "out.m4a"}) {
		t.Error("expected -vn to disable video output")
	}
	if HasNoVideoOutput([]string{"-i", "in.mp4", "--", "-vn"}) {
		t.Error("-vn after -- is passed through, not a video flag")
	}
	if HasNoVideoOutput([]string{"-i", "in.mp4", "out.mp4"}) {
		t.Error("expected video output")
	}
}

func TestHasInlineVideoEncoderSyntax(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want bool
	}{
		{name: "-c:v=", args: []string{"-c:v=libx264"}, want: true},
		{name: "-codec:v=", args: []string{"-codec:v=libx264"}, want: true},
		{name: "-vcodec=", args: []string{"-vcodec=libx264"}, want: true},
		{name: "generic -c=", args: []string{"-c=libx264"}, want: true},
		{name: "separate value", args: []string{"-c:v", "libx264"}, want: false},
		{name: "audio specifier", args: []string{"-c:a=aac"}, want: false},
		{name: "after separator", args: []string{"--", "-c:v=libx264"}, want: false},
	}
	for _, tt := range tests {
		if got := HasInlineVideoEncoderSyntax(tt.args); got != tt.want {
			t.Errorf("HasInlineVideoEncoderSyntax(%v) = %v, want %v", tt.args, got, tt.want)
		}
	}
}

func TestReplacePrimaryVideoEncoderCopyGuard(t *testing.T) {
	for _, args := range [][]string{
		{"-i", "in.mp4", "-c", "copy", "out.mp4"},
		{"-i", "in.mp4", "-c:v", "copy", "out.mp4"},
		{"-i", "in.mp4", "-c:v:0", "copy", "out.mp4"},
	} {
		got := ReplacePrimaryVideoEncoder(args, encoder.EncoderLibX264, nil)
		if !slices.Equal(got, args) {
			t.Errorf("copy must pass through unchanged: got %v, want %v", got, args)
		}
	}
}

func TestPrimaryVideoEncoderFlagLastDeclarationWins(t *testing.T) {
	flags := ParseVideoEncoderFlags([]string{"-c:v", "libvpx-vp9", "-c:v", "libvpx-vp9"}, nil)
	primary, ok := PrimaryVideoEncoderFlag(flags)
	if !ok || primary.Value != "libvpx-vp9" || primary.Index != 2 {
		t.Fatalf("primary = %+v (ok=%v), want the last declaration at index 2", primary, ok)
	}

	flags = ParseVideoEncoderFlags([]string{"-c:v", "libvpx-vp9", "-codec:v", "h264_nvenc"}, nil)
	primary, ok = PrimaryVideoEncoderFlag(flags)
	if !ok || primary.Value != "h264_nvenc" || primary.Index != 2 {
		t.Fatalf("primary = %+v (ok=%v), want the later equivalent spelling", primary, ok)
	}
}
