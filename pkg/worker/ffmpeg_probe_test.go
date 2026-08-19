package worker

import (
	"context"
	"strings"
	"testing"
)

func TestParseCodecOutput(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected []CodecInfo
	}{
		{
			name: "parse video encoders",
			input: `ffmpeg version n8.1
Encoders:
 V..... = Video
 A..... = Audio
 S..... = Subtitle
 ------
 V....D libx264              libx264 H.264 / AVC / MPEG-4 AVC / MPEG-4 part 10 (codec h264)
 V....D h264_nvenc           NVIDIA NVENC H.264 encoder (codec h264)
 V..... hevc_qsv             HEVC (Intel Quick Sync Video acceleration) (codec hevc)
 A..... aac                  AAC (Advanced Audio Coding)
`,
			expected: []CodecInfo{
				{Name: "libx264", Type: CodecTypeVideo, IsHW: false},
				{Name: "h264_nvenc", Type: CodecTypeVideo, IsHW: true},
				{Name: "hevc_qsv", Type: CodecTypeVideo, IsHW: true},
				{Name: "aac", Type: CodecTypeAudio, IsHW: false},
			},
		},
		{
			name: "parse decoders with hw codecs",
			input: `Decoders:
 V..... = Video
 ------
 V....D h264                 H.264 / AVC / MPEG-4 AVC / MPEG-4 part 10
 V....D h264_cuvid           Nvidia CUVID H264 decoder (codec h264)
 V....D hevc_vdpau           HEVC (VDPAU) (codec hevc)
`,
			expected: []CodecInfo{
				{Name: "h264", Type: CodecTypeVideo, IsHW: false},
				{Name: "h264_cuvid", Type: CodecTypeVideo, IsHW: true},
				{Name: "hevc_vdpau", Type: CodecTypeVideo, IsHW: true},
			},
		},
		{
			name:     "empty input",
			input:    "",
			expected: nil,
		},
		{
			name: "no codec lines",
			input: `ffmpeg version n8.1
Encoders:
 V..... = Video
 ------
`,
			expected: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := parseCodecOutput(tt.input)

			if len(result) != len(tt.expected) {
				t.Errorf("parseCodecOutput() got %d codecs, want %d", len(result), len(tt.expected))
				return
			}

			for i, got := range result {
				want := tt.expected[i]
				if got.Name != want.Name {
					t.Errorf("codec[%d].Name = %q, want %q", i, got.Name, want.Name)
				}
				if got.Type != want.Type {
					t.Errorf("codec[%d].Type = %q, want %q", i, got.Type, want.Type)
				}
				if got.IsHW != want.IsHW {
					t.Errorf("codec[%d].IsHW = %v, want %v", i, got.IsHW, want.IsHW)
				}
			}
		})
	}
}

func TestIsHardwareCodec(t *testing.T) {
	tests := []struct {
		name     string
		codec    string
		expected bool
	}{
		{"nvenc encoder", "h264_nvenc", true},
		{"qsv encoder", "hevc_qsv", true},
		{"vaapi encoder", "h264_vaapi", true},
		{"amf encoder", "h264_amf", true},
		{"videotoolbox encoder", "h264_videotoolbox", true},
		{"cuvid decoder", "h264_cuvid", true},
		{"vdpau decoder", "h264_vdpau", true},
		{"nvdec decoder", "h264_nvdec", true},
		{"software codec", "libx264", false},
		{"software codec aac", "aac", false},
		{"software codec libaom", "libaom-av1", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isHardwareCodec(tt.codec)
			if result != tt.expected {
				t.Errorf("isHardwareCodec(%q) = %v, want %v", tt.codec, result, tt.expected)
			}
		})
	}
}

func TestFFmpegInfo_GetVideoEncoders(t *testing.T) {
	info := &FFmpegInfo{
		Encoders: []CodecInfo{
			{Name: "libx264", Type: CodecTypeVideo},
			{Name: "aac", Type: CodecTypeAudio},
			{Name: "h264_nvenc", Type: CodecTypeVideo},
			{Name: "srt", Type: CodecTypeSubtitle},
		},
	}

	result := info.GetVideoEncoders()
	if len(result) != 2 {
		t.Errorf("GetVideoEncoders() got %d, want 2", len(result))
	}
}

func TestFFmpegInfo_GetHWEncoders(t *testing.T) {
	info := &FFmpegInfo{
		Encoders: []CodecInfo{
			{Name: "libx264", Type: CodecTypeVideo, IsHW: false},
			{Name: "h264_nvenc", Type: CodecTypeVideo, IsHW: true},
			{Name: "hevc_qsv", Type: CodecTypeVideo, IsHW: true},
			{Name: "aac", Type: CodecTypeAudio, IsHW: false},
		},
	}

	result := info.GetHWEncoders()
	if len(result) != 2 {
		t.Errorf("GetHWEncoders() got %d, want 2", len(result))
	}
}

func TestFFmpegInfo_EncoderNames(t *testing.T) {
	info := &FFmpegInfo{
		Encoders: []CodecInfo{
			{Name: "libx264"},
			{Name: "h264_nvenc"},
		},
	}

	names := info.EncoderNames()
	if len(names) != 2 {
		t.Errorf("EncoderNames() got %d, want 2", len(names))
	}
	if names[0] != "libx264" {
		t.Errorf("EncoderNames()[0] = %q, want %q", names[0], "libx264")
	}
}

func TestFFmpegInfo_DecoderNames(t *testing.T) {
	info := &FFmpegInfo{
		Decoders: []CodecInfo{
			{Name: "h264"},
			{Name: "hevc"},
		},
	}

	names := info.DecoderNames()
	if len(names) != 2 {
		t.Errorf("DecoderNames() got %d, want 2", len(names))
	}
	if names[0] != "h264" {
		t.Errorf("DecoderNames()[0] = %q, want %q", names[0], "h264")
	}
}

// TestFFmpegProbe_Integration tests against actual ffmpeg binary
// This test will be skipped if ffmpeg is not available
func TestFFmpegProbe_Integration(t *testing.T) {
	probe := NewFFmpegProbe("ffmpeg")

	ctx := context.Background()

	// Test GetVersion
	version, err := probe.GetVersion(ctx)
	if err != nil {
		t.Skipf("ffmpeg not available: %v", err)
	}

	if version == "" {
		t.Error("GetVersion() returned empty string")
	}

	if !strings.Contains(version, "ffmpeg") && !strings.HasPrefix(version, "n") && !strings.HasPrefix(version, "4.") && !strings.HasPrefix(version, "5.") && !strings.HasPrefix(version, "6.") {
		t.Logf("Warning: unexpected version format: %s", version)
	}

	// Test GetEncoders
	encoders, err := probe.GetEncoders(ctx)
	if err != nil {
		t.Fatalf("GetEncoders() failed: %v", err)
	}

	if len(encoders) == 0 {
		t.Error("GetEncoders() returned empty list")
	}

	// Check that we have at least some common encoders
	encoderNames := make(map[string]bool)
	for _, e := range encoders {
		encoderNames[e.Name] = true
	}

	// Most ffmpeg builds should have these
	commonEncoders := []string{"libx264", "aac"}
	for _, name := range commonEncoders {
		if !encoderNames[name] {
			t.Logf("Warning: common encoder %s not found", name)
		}
	}

	// Test GetDecoders
	decoders, err := probe.GetDecoders(ctx)
	if err != nil {
		t.Fatalf("GetDecoders() failed: %v", err)
	}

	if len(decoders) == 0 {
		t.Error("GetDecoders() returned empty list")
	}

	// Test Probe (full integration)
	info, err := probe.Probe(ctx)
	if err != nil {
		t.Fatalf("Probe() failed: %v", err)
	}

	if info.Version == "" {
		t.Error("Probe() returned empty version")
	}
	if len(info.Encoders) == 0 {
		t.Error("Probe() returned empty encoders list")
	}
	if len(info.Decoders) == 0 {
		t.Error("Probe() returned empty decoders list")
	}

	// Test P0/P1 raw info fields from getRawInfo
	if info.Hwaccels == "" {
		t.Error("Probe() returned empty Hwaccels field")
	}
	if info.Codecs == "" {
		t.Error("Probe() returned empty Codecs field")
	}
	if info.Filters == "" {
		t.Error("Probe() returned empty Filters field")
	}
	if info.PixFmts == "" {
		t.Error("Probe() returned empty PixFmts field")
	}
	if info.Formats == "" {
		t.Error("Probe() returned empty Formats field")
	}
	t.Logf("Hwaccels length: %d bytes", len(info.Hwaccels))
	t.Logf("Codecs length: %d bytes", len(info.Codecs))
	t.Logf("Filters length: %d bytes", len(info.Filters))
	t.Logf("PixFmts length: %d bytes", len(info.PixFmts))
	t.Logf("Formats length: %d bytes", len(info.Formats))

	// Test helper methods
	videoEncoders := info.GetVideoEncoders()
	if len(videoEncoders) == 0 {
		t.Error("GetVideoEncoders() returned empty list")
	}

	hwEncoders := info.GetHWEncoders()
	t.Logf("Found %d hardware encoders", len(hwEncoders))
	for _, e := range hwEncoders {
		t.Logf("  HW encoder: %s - %s", e.Name, e.Description)
	}
}
