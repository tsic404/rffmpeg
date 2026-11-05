package rewrite

import (
	"context"
	"strings"
	"testing"

	"github.com/tsic404/rffmpeg/pkg/encoder"
)

func TestOutputFormat(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "extension guess", args: []string{"-i", "in.mp4", "out.mkv"}, want: "matroska"},
		{name: "explicit -f wins over the extension", args: []string{"-i", "in.mp4", "-f", "webm", "out.bin"}, want: "webm"},
		{name: "last -f wins", args: []string{"-i", "in.mp4", "-f", "webm", "-f", "mp4", "out.bin"}, want: "mp4"},
		{name: "input-section -f is not the output format", args: []string{"-f", "lavfi", "-i", "testsrc", "out.webm"}, want: "webm"},
		{name: "boolean flag before the output consumes no value", args: []string{"-i", "in.mp4", "-vn", "out.mp4"}, want: "mp4"},
		{name: "option value is not the output", args: []string{"-i", "in.mp4", "-map", "0:a", "out.m4a"}, want: "ipod"},
		{name: "streaming output needs an explicit -f", args: []string{"-i", "in.mp4", "-"}, want: ""},
		{name: "streaming output with -f", args: []string{"-i", "in.mp4", "-f", "mpegts", "-"}, want: "mpegts"},
		{name: "unknown extension", args: []string{"-i", "in.mp4", "out.out"}, want: ""},
		{name: "no input", args: []string{"out.mp4"}, want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := OutputFormat(tt.args); got != tt.want {
				t.Errorf("OutputFormat(%v) = %q, want %q", tt.args, got, tt.want)
			}
		})
	}
}

func TestContainerCodecFamily(t *testing.T) {
	cases := map[string]encoder.CodecFormat{
		"h264":       encoder.CodecH264,
		"hevc":       encoder.CodecHEVC,
		"vp9":        encoder.CodecVP9,
		"av1":        encoder.CodecAV1,
		"vp8":        encoder.CodecVP8,
		"mpeg2video": encoder.CodecMPEG2,
		"mpeg4":      encoder.CodecMPEG4,
		// Outside the substitutable families of C-70.
		"gif":        "",
		"mpeg1video": "",
		"theora":     "",
	}
	for codec, want := range cases {
		if got := ContainerCodecFamily(codec); got != want {
			t.Errorf("ContainerCodecFamily(%q) = %q, want %q", codec, got, want)
		}
	}
}

// TestEngineContainerTarget covers the unspecified-encoder decision end to
// end: the target family comes from the output container, and a container that
// carries no video, an undeterminable format or a family without a usable
// encoder leaves the command untouched.
func TestEngineContainerTarget(t *testing.T) {
	caps := HardwareCapabilities{
		AvailableEncoders: []encoder.EncoderFamily{
			encoder.EncoderLibX264,
			encoder.EncoderH264NVENC,
			encoder.EncoderLibVPX,
			encoder.EncoderLibVPX8,
			encoder.EncoderMpeg2Video,
			encoder.EncoderMpeg4,
		},
		HardwareEncoders: []encoder.EncoderFamily{encoder.EncoderH264NVENC},
		SoftwareEncoders: []encoder.EncoderFamily{
			encoder.EncoderLibX264,
			encoder.EncoderLibVPX,
			encoder.EncoderLibVPX8,
			encoder.EncoderMpeg2Video,
			encoder.EncoderMpeg4,
		},
		SupportedCodecs: []encoder.CodecFormat{
			encoder.CodecH264,
			encoder.CodecVP9,
			encoder.CodecVP8,
			encoder.CodecMPEG2,
			encoder.CodecMPEG4,
		},
		EncoderPriority: DefaultEncoderPriority(),
	}

	tests := []struct {
		name             string
		args             []string
		wantTarget       encoder.EncoderFamily
		wantRewritten    bool
		wantNotification string
	}{
		{
			name:             "mp4 keeps the h264 family",
			args:             []string{"-i", "in.mp4", "out.mp4"},
			wantTarget:       encoder.EncoderH264NVENC,
			wantRewritten:    true,
			wantNotification: "[rffmpeg] auto-selected h264_nvenc (container mp4)",
		},
		{
			name:             "webm targets the vp9 family",
			args:             []string{"-i", "in.mp4", "out.webm"},
			wantTarget:       encoder.EncoderLibVPX,
			wantRewritten:    true,
			wantNotification: "[rffmpeg] auto-selected libvpx-vp9 (container webm)",
		},
		{
			name:             "mpegts targets the mpeg2 family",
			args:             []string{"-i", "in.mp4", "out.ts"},
			wantTarget:       encoder.EncoderMpeg2Video,
			wantRewritten:    true,
			wantNotification: "[rffmpeg] auto-selected mpeg2video (container mpegts)",
		},
		{
			name:             "avi targets the mpeg4 family",
			args:             []string{"-i", "in.mp4", "out.avi"},
			wantTarget:       encoder.EncoderMpeg4,
			wantRewritten:    true,
			wantNotification: "[rffmpeg] auto-selected mpeg4 (container avi)",
		},
		{
			name:             "ivf targets the vp8 family",
			args:             []string{"-i", "in.mp4", "out.ivf"},
			wantTarget:       encoder.EncoderLibVPX8,
			wantRewritten:    true,
			wantNotification: "[rffmpeg] auto-selected libvpx (container ivf)",
		},
		{
			name:          "audio-only container carries no video",
			args:          []string{"-i", "in.mp4", "out.wav"},
			wantRewritten: false,
		},
		{
			name:          "undeterminable target family",
			args:          []string{"-i", "in.mp4", "out.gif"},
			wantRewritten: false,
		},
		{
			name:          "map selects no video stream",
			args:          []string{"-i", "in.mp4", "-map", "0:a", "out.mp4"},
			wantRewritten: false,
		},
		{
			name:          "unknown extension",
			args:          []string{"-i", "in.mp4", "out.out"},
			wantRewritten: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			engine := NewEngineCoordinator()
			response, err := engine.Rewrite(context.Background(), &EncoderRewriteRequest{
				OriginalArgs:         tt.args,
				HardwareCapabilities: caps,
			})
			if err != nil {
				t.Fatalf("Rewrite returned error: %v", err)
			}
			if string(response.TargetEncoder) != string(tt.wantTarget) {
				t.Errorf("TargetEncoder = %q, want %q", response.TargetEncoder, tt.wantTarget)
			}
			rewritten := strings.Join(response.RewrittenArgs, " ") != strings.Join(tt.args, " ")
			if rewritten != tt.wantRewritten {
				t.Errorf("args rewritten = %v, want %v (args: %v)", rewritten, tt.wantRewritten, response.RewrittenArgs)
			}
			if tt.wantNotification == "" {
				if len(response.Notifications) != 0 {
					t.Errorf("expected no notification, got %v", response.Notifications)
				}
				return
			}
			if len(response.Notifications) == 0 {
				t.Fatalf("expected the %q notification, got none", tt.wantNotification)
			}
			var found bool
			for _, notification := range response.Notifications {
				if notification.Message == tt.wantNotification {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("expected notification %q, got %v", tt.wantNotification, response.Notifications)
			}
		})
	}
}
