package worker

import (
	"context"
	"strings"
	"testing"

	"github.com/tsix404/rffmpeg/pkg/encoder"
	"github.com/tsix404/rffmpeg/pkg/encoder/rewrite"
	"github.com/tsix404/rffmpeg/pkg/protocol"
)

func TestRewriteAdapter_RewriteArgs(t *testing.T) {
	adapter := NewRewriteAdapter()

	// Set up hardware capabilities
	caps := &protocol.WorkerCapabilities{
		VideoEncoders: []protocol.EncoderInfo{
			{Name: "h264_nvenc", Type: "video", IsHW: true},
			{Name: "libx264", Type: "video", IsHW: false},
		},
		GPUDevices: []protocol.GPUDeviceInfo{
			{Type: "nvenc", Vendor: "NVIDIA", Accessible: true},
		},
	}
	adapter.SetHardwareCapabilities(caps)

	tests := []struct {
		name            string
		args            []string
		expectRewrite   bool
		expectedEncoder string
	}{
		{
			name:            "no encoder specified - auto upgrade to hardware",
			args:            []string{"-i", "input.mp4", "output.mp4"},
			expectRewrite:   true,
			expectedEncoder: "h264_nvenc",
		},
		{
			name:            "software encoder with hardware available - auto upgrade",
			args:            []string{"-i", "input.mp4", "-c:v", "libx264", "-crf", "23", "output.mp4"},
			expectRewrite:   true,
			expectedEncoder: "h264_nvenc",
		},
		{
			name:            "hardware encoder already specified",
			args:            []string{"-i", "input.mp4", "-c:v", "h264_nvenc", "output.mp4"},
			expectRewrite:   false,
			expectedEncoder: "h264_nvenc",
		},
		{
			name:            "copy encoder - should NOT be rewritten",
			args:            []string{"-i", "video.mp4", "-i", "audio.mp3", "-c:v", "copy", "-c:a", "aac", "-shortest", "merged.mp4"},
			expectRewrite:   false,
			expectedEncoder: "copy",
		},
		{
			name:            "-vn flag with no encoder - should NOT be rewritten",
			args:            []string{"-i", "input.mp4", "-vn", "-c:a", "aac", "output.aac"},
			expectRewrite:   false,
			expectedEncoder: "",
		},
		{
			name:            "-vn flag with explicit encoder - should NOT be rewritten",
			args:            []string{"-i", "input.mp4", "-vn", "-c:v", "libx264", "-c:a", "aac", "output.aac"},
			expectRewrite:   false,
			expectedEncoder: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rewritten, result, err := adapter.RewriteArgs(context.Background(), tt.args)

			if err != nil {
				t.Errorf("unexpected error: %v", err)
				return
			}

			// For scenarios where rewrite is expected, verify the target encoder is set
			if tt.expectRewrite && result.TargetEncoder == "" {
				t.Error("expected target encoder to be set")
			}

			// Check if expected encoder is in args
			if tt.expectedEncoder != "" {
				found := false
				for _, arg := range rewritten {
					if arg == tt.expectedEncoder {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("expected encoder %s in rewritten args, got %v", tt.expectedEncoder, rewritten)
				}
			}
		})
	}
}

func TestRewriteAdapter_ShouldRewrite(t *testing.T) {
	adapter := NewRewriteAdapter()

	// Set up hardware capabilities
	caps := &protocol.WorkerCapabilities{
		VideoEncoders: []protocol.EncoderInfo{
			{Name: "h264_nvenc", Type: "video", IsHW: true},
			{Name: "libx264", Type: "video", IsHW: false},
		},
	}
	adapter.SetHardwareCapabilities(caps)

	tests := []struct {
		name          string
		args          []string
		expectRewrite bool
	}{
		{
			name:          "no encoder - should rewrite",
			args:          []string{"-i", "input.mp4", "output.mp4"},
			expectRewrite: true,
		},
		{
			name:          "software encoder with hw available - should rewrite",
			args:          []string{"-i", "input.mp4", "-c:v", "libx264", "output.mp4"},
			expectRewrite: true,
		},
		{
			name:          "hardware encoder available - no rewrite needed",
			args:          []string{"-i", "input.mp4", "-c:v", "h264_nvenc", "output.mp4"},
			expectRewrite: false,
		},
		{
			name:          "copy encoder - should NOT be rewritten",
			args:          []string{"-i", "video.mp4", "-i", "audio.mp3", "-c:v", "copy", "-c:a", "aac", "-shortest", "merged.mp4"},
			expectRewrite: false,
		},
		{
			name:          "-vn flag with no encoder - should NOT rewrite",
			args:          []string{"-i", "input.mp4", "-vn", "-c:a", "aac", "output.aac"},
			expectRewrite: false,
		},
		{
			name:          "-vn flag with explicit encoder - should NOT rewrite",
			args:          []string{"-i", "input.mp4", "-vn", "-c:v", "libx264", "-c:a", "aac", "output.aac"},
			expectRewrite: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := adapter.ShouldRewrite(tt.args)
			if result != tt.expectRewrite {
				t.Errorf("expected ShouldRewrite=%v, got %v", tt.expectRewrite, result)
			}
		})
	}
}

func TestRewriteAdapter_ParseEncoderFromArgs(t *testing.T) {
	adapter := NewRewriteAdapter()

	tests := []struct {
		name           string
		args           []string
		expectedResult encoder.EncoderFamily
	}{
		{
			name:           "-c:v syntax",
			args:           []string{"-i", "input.mp4", "-c:v", "libx264", "output.mp4"},
			expectedResult: encoder.EncoderLibX264,
		},
		{
			name:           "-codec:v syntax",
			args:           []string{"-i", "input.mp4", "-codec:v", "h264_nvenc", "output.mp4"},
			expectedResult: encoder.EncoderH264NVENC,
		},
		{
			name:           "-vcodec syntax",
			args:           []string{"-i", "input.mp4", "-vcodec", "h264_qsv", "output.mp4"},
			expectedResult: encoder.EncoderH264QSV,
		},
		{
			name:           "-c:v= syntax",
			args:           []string{"-i", "input.mp4", "-c:v=libx265", "output.mp4"},
			expectedResult: encoder.EncoderLibX265,
		},
		{
			name:           "no encoder",
			args:           []string{"-i", "input.mp4", "output.mp4"},
			expectedResult: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := adapter.parseEncoderFromArgs(tt.args)
			if result != tt.expectedResult {
				t.Errorf("expected %s, got %s", tt.expectedResult, result)
			}
		})
	}
}

func TestRewriteAdapter_ConvertCapabilities(t *testing.T) {
	adapter := NewRewriteAdapter()

	caps := &protocol.WorkerCapabilities{
		VideoEncoders: []protocol.EncoderInfo{
			{Name: "h264_nvenc", Type: "video", IsHW: true},
			{Name: "libx264", Type: "video", IsHW: false},
			{Name: "hevc_nvenc", Type: "video", IsHW: true},
		},
		GPUDevices: []protocol.GPUDeviceInfo{
			{Type: "nvenc", Vendor: "NVIDIA", Path: "0", Accessible: true},
		},
		EncoderPriority:  []string{"h264_nvenc", "libx264"},
		EncoderBlacklist: []string{"h264_qsv"},
	}

	hwCaps := adapter.convertCapabilities(caps)

	// Verify available encoders
	if len(hwCaps.AvailableEncoders) != 3 {
		t.Errorf("expected 3 available encoders, got %d", len(hwCaps.AvailableEncoders))
	}

	// Verify hardware encoders
	if len(hwCaps.HardwareEncoders) != 2 {
		t.Errorf("expected 2 hardware encoders, got %d", len(hwCaps.HardwareEncoders))
	}

	// Verify software encoders
	if len(hwCaps.SoftwareEncoders) != 1 {
		t.Errorf("expected 1 software encoder, got %d", len(hwCaps.SoftwareEncoders))
	}

	// Verify GPU devices
	if len(hwCaps.GPUDevices) != 1 {
		t.Errorf("expected 1 GPU device, got %d", len(hwCaps.GPUDevices))
	}

	// Verify blacklist
	if len(hwCaps.EncoderBlacklist) != 1 {
		t.Errorf("expected 1 blacklisted encoder, got %d", len(hwCaps.EncoderBlacklist))
	}
}

func TestRewriteAdapter_FallbackToSoftware(t *testing.T) {
	adapter := NewRewriteAdapter()

	hwCaps := &rewrite.HardwareCapabilities{
		SoftwareEncoders: []encoder.EncoderFamily{encoder.EncoderLibX264},
	}

	tests := []struct {
		name           string
		args           []string
		expectedResult encoder.EncoderFamily
	}{
		{
			name:           "fallback from nvenc to libx264",
			args:           []string{"-i", "input.mp4", "-c:v", "h264_nvenc", "output.mp4"},
			expectedResult: encoder.EncoderLibX264,
		},
		{
			name:           "no encoder - add libx264",
			args:           []string{"-i", "input.mp4", "output.mp4"},
			expectedResult: encoder.EncoderLibX264,
		},
		{
			name:           "-vn flag - should not inject encoder",
			args:           []string{"-i", "input.mp4", "-vn", "-c:a", "aac", "output.aac"},
			expectedResult: "", // no encoder should be injected
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := adapter.fallbackToSoftware(context.Background(), tt.args, hwCaps)
			if err != nil {
				t.Errorf("unexpected error: %v", err)
				return
			}

			// Check if expected encoder is in result
			// When expectedResult is empty, verify no encoder was injected
			found := false
			if tt.expectedResult == "" {
				// Verify no -c:v encoder is present
				found = true // assume pass
				for i, arg := range result {
					if (arg == "-c:v" || arg == "-codec:v" || arg == "-vcodec") && i+1 < len(result) {
						// Found a video encoder specifier — fail unless it's self-contained
						found = false
						break
					}
					if len(arg) > 4 && arg[:4] == "-c:v" && arg[4] == '=' {
						found = false
						break
					}
				}
			} else {
				for _, arg := range result {
					if arg == string(tt.expectedResult) {
						found = true
						break
					}
				}
			}
			if !found {
				t.Errorf("expected encoder %s in result, got %v", tt.expectedResult, result)
			}
		})
	}
}

func TestRewriteAdapter_ConfigMethods(t *testing.T) {
	adapter := NewRewriteAdapter()

	// Test SetEnabled
	adapter.SetEnabled(false)
	if adapter.config.Enabled != false {
		t.Error("SetEnabled failed")
	}

	adapter.SetEnabled(true)
	if adapter.config.Enabled != true {
		t.Error("SetEnabled failed")
	}

	// Test SetAutoHW
	adapter.SetAutoHW(false)
	if adapter.config.AutoHW != false {
		t.Error("SetAutoHW failed")
	}

	adapter.SetAutoHW(true)
	if adapter.config.AutoHW != true {
		t.Error("SetAutoHW failed")
	}

	// Test SetSilent
	adapter.SetSilent(true)
	if adapter.config.Silent != true {
		t.Error("SetSilent failed")
	}

	// Test GetRewriteEngine
	engine := adapter.GetRewriteEngine()
	if engine == nil {
		t.Error("GetRewriteEngine returned nil")
	}
}

func TestRewriteAdapter_NonexistentEncoderReturnsError(t *testing.T) {
	adapter := NewRewriteAdapter()

	// Set up hardware capabilities with only libx264 as software encoder
	caps := &protocol.WorkerCapabilities{
		VideoEncoders: []protocol.EncoderInfo{
			{Name: "h264_nvenc", Type: "video", IsHW: true},
			{Name: "libx264", Type: "video", IsHW: false},
		},
		GPUDevices: []protocol.GPUDeviceInfo{
			{Type: "nvenc", Vendor: "NVIDIA", Accessible: true},
		},
	}
	adapter.SetHardwareCapabilities(caps)
	// Disable auto-HW so it doesn't try to upgrade a known SW encoder
	adapter.SetAutoHW(false)

	tests := []struct {
		name          string
		args          []string
		expectError   bool
		errorContains string
	}{
		{
			name:          "nonexistent encoder returns error",
			args:          []string{"-i", "input.mp4", "-c:v", "nonexistent_codec", "output.mp4"},
			expectError:   true,
			errorContains: "not recognized",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rewritten, result, err := adapter.RewriteArgs(context.Background(), tt.args)

			if tt.expectError {
				if err == nil {
					t.Errorf("expected error containing %q, got nil", tt.errorContains)
					return
				}
				if !strings.Contains(err.Error(), tt.errorContains) {
					t.Errorf("expected error containing %q, got %v", tt.errorContains, err)
				}
				// Should NOT use fallback for unknown encoders
				if result.FallbackUsed {
					t.Error("expected FallbackUsed=false for unknown encoder, got true")
				}
				return
			}

			if err != nil {
				t.Errorf("unexpected error: %v", err)
				return
			}

			_ = rewritten
			_ = result
		})
	}
}

func TestRewriteAdapter_ParseEncoderParamsFromArgs(t *testing.T) {
	adapter := NewRewriteAdapter()

	tests := []struct {
		name           string
		args           []string
		expectedParams map[string]string
	}{
		{
			name:           "crf and preset",
			args:           []string{"-i", "input.mp4", "-c:v", "libx264", "-crf", "23", "-preset", "slow", "output.mp4"},
			expectedParams: map[string]string{"crf": "23", "preset": "slow"},
		},
		{
			name:           "crf with equals syntax",
			args:           []string{"-i", "input.mp4", "-c:v", "libx264", "-crf=23", "output.mp4"},
			expectedParams: map[string]string{"crf": "23"},
		},
		{
			name:           "multiple encoder params",
			args:           []string{"-i", "input.mp4", "-c:v", "libx264", "-crf", "23", "-preset", "fast", "-tune", "film", "output.mp4"},
			expectedParams: map[string]string{"crf": "23", "preset": "fast", "tune": "film"},
		},
		{
			name:           "b:v bitrate param",
			args:           []string{"-i", "input.mp4", "-c:v", "h264_nvenc", "-b:v", "5M", "-preset", "p4", "output.mp4"},
			expectedParams: map[string]string{"b:v": "5M", "preset": "p4"},
		},
		{
			name:           "cq param for nvenc",
			args:           []string{"-i", "input.mp4", "-c:v", "h264_nvenc", "-cq", "23", "-preset", "p6", "output.mp4"},
			expectedParams: map[string]string{"cq": "23", "preset": "p6"},
		},
		{
			name:           "no encoder params",
			args:           []string{"-i", "input.mp4", "-c:v", "libx264", "output.mp4"},
			expectedParams: map[string]string{},
		},
		{
			name:           "skip encoder spec with c:v= syntax",
			args:           []string{"-i", "input.mp4", "-c:v=libx264", "-crf", "23", "output.mp4"},
			expectedParams: map[string]string{"crf": "23"},
		},
		{
			name:           "input file looks like flag value",
			args:           []string{"-i", "-flag.mp4", "-c:v", "libx264", "-crf", "23", "output.mp4"},
			expectedParams: map[string]string{"crf": "23"},
		},
		{
			name:           "-shortest boolean flag does not consume output as value",
			args:           []string{"-y", "-i", "video.mp4", "-i", "audio.m4a", "-c:v", "libx264", "-c:a", "aac", "-shortest", "merged.mp4"},
			expectedParams: map[string]string{},
		},
		{
			name:           "-shortest with encoder params does not consume output",
			args:           []string{"-y", "-i", "video.mp4", "-i", "audio.m4a", "-c:v", "libx264", "-crf", "23", "-c:a", "aac", "-shortest", "merged.mp4"},
			expectedParams: map[string]string{"crf": "23"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := adapter.parseEncoderParamsFromArgs(tt.args)
			if len(result) != len(tt.expectedParams) {
				t.Errorf("expected %d params, got %d: %v", len(tt.expectedParams), len(result), result)
				return
			}
			for k, v := range tt.expectedParams {
				if result[k] != v {
					t.Errorf("expected params[%s] = %s, got %s", k, v, result[k])
				}
			}
		})
	}
}
