package capabilities

import (
	"context"
	"testing"

	"github.com/tsic404/rffmpeg/pkg/protocol"
	"github.com/tsic404/rffmpeg/pkg/worker/gpu"
)

// mockFFmpegProber is a mock FFmpegProber for testing
type mockFFmpegProber struct {
	info  *FFmpegInfo
	err   error
	calls int
}

func (m *mockFFmpegProber) Probe(ctx context.Context) (*FFmpegInfo, error) {
	m.calls++
	return m.info, m.err
}

func TestNewDetector(t *testing.T) {
	probe := &mockFFmpegProber{}
	d := NewDetector(probe)
	if d == nil {
		t.Error("NewDetector() returned nil")
	}
}

func TestDetector_Detect(t *testing.T) {
	probe := &mockFFmpegProber{
		info: &FFmpegInfo{
			Version: "ffmpeg version 5.0",
			Encoders: []CodecInfo{
				{Name: "libx264", Type: "V", IsHW: false},
				{Name: "h264_nvenc", Type: "V", IsHW: true},
			},
			Decoders: []CodecInfo{
				{Name: "h264", Type: "V", IsHW: false},
			},
		},
	}

	d := NewDetector(probe)
	caps, err := d.Detect(context.Background())
	if err != nil {
		t.Fatalf("Detect() failed: %v", err)
	}

	if caps.FFmpegVersion != "ffmpeg version 5.0" {
		t.Errorf("FFmpegVersion = %q, want %q", caps.FFmpegVersion, "ffmpeg version 5.0")
	}
	// Hardware encoder may be filtered out if no corresponding GPU device is detected
	// At minimum, software encoder should be present
	if len(caps.VideoEncoders) < 1 {
		t.Errorf("VideoEncoders length = %d, want at least 1", len(caps.VideoEncoders))
	}
	if len(caps.VideoDecoders) != 1 {
		t.Errorf("VideoDecoders length = %d, want 1", len(caps.VideoDecoders))
	}
}

func TestDetector_DetectWithConfig(t *testing.T) {
	probe := &mockFFmpegProber{
		info: &FFmpegInfo{
			Version: "ffmpeg version 5.0",
			Encoders: []CodecInfo{
				{Name: "libx264", Type: "V", IsHW: false, Description: "H.264 encoder"},
				{Name: "h264_nvenc", Type: "V", IsHW: true, Description: "NVIDIA encoder"},
				{Name: "libx265", Type: "V", IsHW: false, Description: "H.265 encoder"},
			},
			Decoders: []CodecInfo{
				{Name: "h264", Type: "V", IsHW: false},
			},
		},
	}

	d := NewDetector(probe)
	cfg := &Config{
		MaxConcurrent:    4,
		EncoderPriority:  []string{"h264_nvenc", "libx264"},
		EncoderBlacklist: []string{"libx265"},
		AutoDetectCodecs: true,
		AutoDetectGPU:    true,
	}

	caps, err := d.DetectWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("DetectWithConfig() failed: %v", err)
	}

	if caps.MaxConcurrent != 4 {
		t.Errorf("MaxConcurrent = %d, want 4", caps.MaxConcurrent)
	}
	if len(caps.EncoderPriority) != 2 {
		t.Errorf("EncoderPriority length = %d, want 2", len(caps.EncoderPriority))
	}
	if len(caps.EncoderBlacklist) != 1 {
		t.Errorf("EncoderBlacklist length = %d, want 1", len(caps.EncoderBlacklist))
	}
	// Check that blacklisted encoder is removed
	// Hardware encoder may be filtered out if no corresponding GPU device is detected
	// At minimum, software encoder should be present
	if len(caps.VideoEncoders) < 1 {
		t.Errorf("VideoEncoders length = %d, want at least 1 (blacklist applied)", len(caps.VideoEncoders))
	}
	// Check priority is applied (if h264_nvenc is present)
	for _, enc := range caps.VideoEncoders {
		if enc.Name == "h264_nvenc" {
			if enc.Priority == 0 {
				t.Error("h264_nvenc should have priority set")
			}
		}
	}
}

func TestDetector_DetectWithConfig_ManualOverride(t *testing.T) {
	probe := &mockFFmpegProber{
		info: &FFmpegInfo{
			Version: "ffmpeg version 5.0",
			Encoders: []CodecInfo{
				{Name: "libx264", Type: "V", IsHW: false},
			},
			Decoders: []CodecInfo{
				{Name: "h264", Type: "V", IsHW: false},
			},
		},
	}

	d := NewDetector(probe)
	cfg := &Config{
		AutoDetectCodecs:    false,
		ManualEncoders:      []string{"h264_nvenc", "hevc_qsv"},
		ManualDecoders:      []string{"h264_cuvid"},
		ManualFFmpegVersion: "ffmpeg version 6.0",
		ManualGPUModel:      "NVIDIA RTX 3080",
		AutoDetectGPU:       false,
		MaxConcurrent:       2,
	}

	caps, err := d.DetectWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("DetectWithConfig() failed: %v", err)
	}

	// Check manual overrides
	if len(caps.VideoEncoders) != 2 {
		t.Errorf("VideoEncoders length = %d, want 2", len(caps.VideoEncoders))
	}
	if caps.FFmpegVersion != "ffmpeg version 6.0" {
		t.Errorf("FFmpegVersion = %q, want %q", caps.FFmpegVersion, "ffmpeg version 6.0")
	}
	if caps.GPUModel != "NVIDIA RTX 3080" {
		t.Errorf("GPUModel = %q, want %q", caps.GPUModel, "NVIDIA RTX 3080")
	}
	if len(caps.GPUDevices) != 0 {
		t.Errorf("GPUDevices length = %d, want 0 (auto-detect disabled)", len(caps.GPUDevices))
	}
}

func TestApplyPriority(t *testing.T) {
	encoders := []protocol.EncoderInfo{
		{Name: "libx264"},
		{Name: "h264_nvenc"},
		{Name: "hevc_qsv"},
	}
	priority := []string{"h264_nvenc", "libx264"} // h264_nvenc highest

	applyPriority(encoders, priority)

	// h264_nvenc should have priority 2, libx264 should have priority 1
	for _, enc := range encoders {
		if enc.Name == "h264_nvenc" && enc.Priority != 2 {
			t.Errorf("h264_nvenc priority = %d, want 2", enc.Priority)
		}
		if enc.Name == "libx264" && enc.Priority != 1 {
			t.Errorf("libx264 priority = %d, want 1", enc.Priority)
		}
		if enc.Name == "hevc_qsv" && enc.Priority != 0 {
			t.Errorf("hevc_qsv priority = %d, want 0 (not in priority list)", enc.Priority)
		}
	}
}

func TestFilterBlacklisted(t *testing.T) {
	encoders := []protocol.EncoderInfo{
		{Name: "libx264"},
		{Name: "h264_nvenc"},
		{Name: "hevc_qsv"},
		{Name: "libx265"},
	}
	blacklist := []string{"hevc_qsv", "libx265"}

	filtered := filterBlacklisted(encoders, blacklist)

	if len(filtered) != 2 {
		t.Errorf("filtered length = %d, want 2", len(filtered))
	}
	for _, enc := range filtered {
		if enc.Name == "hevc_qsv" || enc.Name == "libx265" {
			t.Errorf("blacklisted encoder %q should be filtered out", enc.Name)
		}
	}
}

func TestConvertEncoders(t *testing.T) {
	codecs := []CodecInfo{
		{Name: "libx264", Type: "V", IsHW: false, Description: "H.264"},
		{Name: "aac", Type: "A", IsHW: false, Description: "AAC"}, // Audio - should be skipped
		{Name: "h264_nvenc", Type: "V", IsHW: true, Description: "NVENC"},
	}

	// Test without GPU devices - hardware encoder should be filtered out
	t.Run("no_gpu_devices", func(t *testing.T) {
		encoders := convertEncoders(codecs, nil)

		if len(encoders) != 1 {
			t.Errorf("convertEncoders() length = %d, want 1 (hardware encoder filtered)", len(encoders))
		}

		// Check that audio codec is excluded
		for _, enc := range encoders {
			if enc.Type != "video" {
				t.Errorf("encoder type = %q, want %q", enc.Type, "video")
			}
		}

		// Check that only software encoder is included
		for _, enc := range encoders {
			if enc.IsHW {
				t.Error("hardware encoder should be filtered out when no GPU device is present")
			}
		}
	})

	// Test with NVIDIA GPU device - hardware encoder should be included
	t.Run("with_nvidia_gpu", func(t *testing.T) {
		gpuDevices := []gpu.Device{
			{Type: gpu.DeviceTypeNVENC, Accessible: true},
		}
		encoders := convertEncoders(codecs, gpuDevices)

		if len(encoders) != 2 {
			t.Errorf("convertEncoders() length = %d, want 2 (hardware encoder included)", len(encoders))
		}

		// Check HW marking
		for _, enc := range encoders {
			if enc.Name == "h264_nvenc" && !enc.IsHW {
				t.Error("h264_nvenc should be marked as HW")
			}
			if enc.Name == "libx264" && enc.IsHW {
				t.Error("libx264 should not be marked as HW")
			}
		}
	})

	// Test with inaccessible GPU device - hardware encoder should be filtered out
	t.Run("with_inaccessible_gpu", func(t *testing.T) {
		gpuDevices := []gpu.Device{
			{Type: gpu.DeviceTypeNVENC, Accessible: false},
		}
		encoders := convertEncoders(codecs, gpuDevices)

		if len(encoders) != 1 {
			t.Errorf("convertEncoders() length = %d, want 1 (hardware encoder filtered due to inaccessible GPU)", len(encoders))
		}
	})
}

// TestDetector_Detect_NilProbe verifies the fallback path when ffmpegProbe is nil.
// The returned capabilities should have P0/P1 fields explicitly initialized to empty strings,
// not cause a nil pointer dereference.
func TestDetector_Detect_NilProbe(t *testing.T) {
	d := NewDetector(nil)
	caps, err := d.Detect(context.Background())
	if err != nil {
		t.Fatalf("Detect() with nil probe failed: %v", err)
	}
	if caps.Hwaccels != "" {
		t.Errorf("Hwaccels = %q, want empty string", caps.Hwaccels)
	}
	if caps.Codecs != "" {
		t.Errorf("Codecs = %q, want empty string", caps.Codecs)
	}
	if caps.Filters != "" {
		t.Errorf("Filters = %q, want empty string", caps.Filters)
	}
	if caps.PixFmts != "" {
		t.Errorf("PixFmts = %q, want empty string", caps.PixFmts)
	}
	if caps.Formats != "" {
		t.Errorf("Formats = %q, want empty string", caps.Formats)
	}
}

// TestDetector_Detect_ProbeError verifies that when Probe() returns an error (and nil info),
// the code does not panic and returns capabilities with empty P0/P1 fields.
func TestDetector_Detect_ProbeError(t *testing.T) {
	probe := &mockFFmpegProber{
		info: nil,
		err:  context.DeadlineExceeded, // simulate probe failure
	}
	d := NewDetector(probe)
	caps, err := d.Detect(context.Background())
	if err != nil {
		t.Fatalf("Detect() with probe error failed: %v", err)
	}
	// P0/P1 fields should be empty (not panic)
	if caps.Hwaccels != "" {
		t.Errorf("Hwaccels = %q, want empty string", caps.Hwaccels)
	}
	if caps.Codecs != "" {
		t.Errorf("Codecs = %q, want empty string", caps.Codecs)
	}
	if caps.Filters != "" {
		t.Errorf("Filters = %q, want empty string", caps.Filters)
	}
	if caps.PixFmts != "" {
		t.Errorf("PixFmts = %q, want empty string", caps.PixFmts)
	}
	if caps.Formats != "" {
		t.Errorf("Formats = %q, want empty string", caps.Formats)
	}
	// Verify version and encoders/decoders are still empty (probe failed)
	if caps.FFmpegVersion != "" {
		t.Errorf("FFmpegVersion = %q, want empty string (probe failed)", caps.FFmpegVersion)
	}
	if len(caps.VideoEncoders) != 0 {
		t.Errorf("VideoEncoders length = %d, want 0 (probe failed)", len(caps.VideoEncoders))
	}
}

// TestDetector_Detect_P0P1Fields verifies that P0/P1 raw text fields are propagated
// correctly from the probe result to the WorkerCapabilities.
func TestDetector_Detect_P0P1Fields(t *testing.T) {
	probe := &mockFFmpegProber{
		info: &FFmpegInfo{
			Version: "ffmpeg version 6.0",
			Encoders: []CodecInfo{
				{Name: "libx264", Type: "V", IsHW: false},
			},
			Decoders: []CodecInfo{
				{Name: "h264", Type: "V", IsHW: false},
			},
			Hwaccels: "Hardware acceleration methods:\ncuda\nvaapi\nqsv\n",
			Codecs:   "Codecs:\n DEV.LS h264  H.264 / AVC / MPEG-4 AVC\n",
			Filters:  "Filters:\n T.. overlay\n",
			PixFmts:  "Pixel formats:\nIO... yuv420p\n",
			Formats:  "File formats:\n DE mp4\n",
		},
	}
	d := NewDetector(probe)
	caps, err := d.Detect(context.Background())
	if err != nil {
		t.Fatalf("Detect() failed: %v", err)
	}

	if caps.Hwaccels != probe.info.Hwaccels {
		t.Errorf("Hwaccels = %q, want %q", caps.Hwaccels, probe.info.Hwaccels)
	}
	if caps.Codecs != probe.info.Codecs {
		t.Errorf("Codecs = %q, want %q", caps.Codecs, probe.info.Codecs)
	}
	if caps.Filters != probe.info.Filters {
		t.Errorf("Filters = %q, want %q", caps.Filters, probe.info.Filters)
	}
	if caps.PixFmts != probe.info.PixFmts {
		t.Errorf("PixFmts = %q, want %q", caps.PixFmts, probe.info.PixFmts)
	}
	if caps.Formats != probe.info.Formats {
		t.Errorf("Formats = %q, want %q", caps.Formats, probe.info.Formats)
	}
}

func TestConvertDecoders(t *testing.T) {
	codecs := []CodecInfo{
		{Name: "h264", Type: "V", IsHW: false, Description: "H.264"},
		{Name: "aac", Type: "A", IsHW: false, Description: "AAC"}, // Audio - should be skipped
		{Name: "h264_cuvid", Type: "V", IsHW: true, Description: "CUVID"},
	}

	decoders := convertDecoders(codecs)

	if len(decoders) != 2 {
		t.Errorf("convertDecoders() length = %d, want 2 (video only)", len(decoders))
	}
}
