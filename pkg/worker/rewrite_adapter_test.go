package worker

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/tsic404/rffmpeg/pkg/audit"
	"github.com/tsic404/rffmpeg/pkg/encoder"
	"github.com/tsic404/rffmpeg/pkg/encoder/rewrite"
	"github.com/tsic404/rffmpeg/pkg/protocol"
)

// testHWEncoderNames lists every hardware encoder family the shared classifier
// recognises; the capabilities summary must mark all of them.
var testHWEncoderNames = []string{
	"h264_nvenc", "h264_qsv", "h264_vaapi", "h264_amf", "h264_videotoolbox",
	"hevc_nvenc", "hevc_qsv", "hevc_vaapi", "hevc_amf", "hevc_videotoolbox",
	"vp9_nvenc", "vp9_qsv", "vp9_vaapi",
	"av1_nvenc", "av1_qsv", "av1_vaapi",
}

// testEncoderInfos builds a capability set with the software encoders listed
// first, so a summary that puts hardware up front proves it reorders rather
// than echoing the input order.
func testEncoderInfos(hw, sw int) []protocol.EncoderInfo {
	encoders := make([]protocol.EncoderInfo, 0, hw+sw)
	for i := range sw {
		encoders = append(encoders, protocol.EncoderInfo{Name: fmt.Sprintf("sw-%02d", i), Type: "video"})
	}
	for _, name := range testHWEncoderNames[:hw] {
		encoders = append(encoders, protocol.EncoderInfo{Name: name, Type: "video", IsHW: true})
	}
	return encoders
}

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
			rewritten, result, err := adapter.RewriteArgs(context.Background(), tt.args, true)

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

// TestRewriteAdapter_RewriteArgs_InlineCodecForm pins the design rule for
// "=" joined encoder options: ffmpeg rejects that syntax, so it is not a
// selection and must not trigger auto-selection. The command passes through
// verbatim and ffmpeg reports the error.
func TestRewriteAdapter_RewriteArgs_InlineCodecForm(t *testing.T) {
	adapter := NewRewriteAdapter()
	adapter.SetHardwareCapabilities(&protocol.WorkerCapabilities{
		VideoEncoders: []protocol.EncoderInfo{
			{Name: "h264_nvenc", Type: "video", IsHW: true},
			{Name: "libx264", Type: "video", IsHW: false},
		},
		GPUDevices: []protocol.GPUDeviceInfo{
			{Type: "nvenc", Vendor: "NVIDIA", Accessible: true},
		},
	})

	forms := [][]string{
		{"-i", "input.mp4", "-vcodec=libx264", "output.mp4"},
		{"-i", "input.mp4", "-codec:v=libx264", "output.mp4"},
		{"-i", "input.mp4", "-c:v=libx264", "output.mp4"},
	}
	for _, args := range forms {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			rewritten, result, err := adapter.RewriteArgs(context.Background(), args, true)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !slices.Equal(rewritten, args) {
				t.Errorf("= form must pass through verbatim: got %v, want %v", rewritten, args)
			}
			if result.Performed {
				t.Errorf("= form must not be rewritten: %+v", result)
			}
			for _, arg := range rewritten {
				if arg == "h264_nvenc" {
					t.Errorf("auto-selection ran on invalid = syntax: %v", rewritten)
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
			result := adapter.ShouldRewrite(tt.args, true)
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
			name:           "-c:v= syntax is not a selection",
			args:           []string{"-i", "input.mp4", "-c:v=libx265", "output.mp4"},
			expectedResult: "",
		},
		{
			name:           "-codec:v= inline syntax is not a selection",
			args:           []string{"-i", "input.mp4", "-codec:v=h264_nvenc", "output.mp4"},
			expectedResult: "",
		},
		{
			name:           "-vcodec= inline syntax is not a selection",
			args:           []string{"-i", "input.mp4", "-vcodec=hevc_nvenc", "output.mp4"},
			expectedResult: "",
		},
		{
			name:           "bare -c copy",
			args:           []string{"-i", "input.mp4", "-c", "copy", "output.mp4"},
			expectedResult: encoder.EncoderFamily("copy"),
		},
		{
			name:           "bare -codec copy",
			args:           []string{"-i", "input.mp4", "-codec", "copy", "output.mp4"},
			expectedResult: encoder.EncoderFamily("copy"),
		},
		{
			name:           "bare -c=copy inline is not a selection",
			args:           []string{"-i", "input.mp4", "-c=copy", "output.mp4"},
			expectedResult: "",
		},
		{
			name:           "bare -codec=copy inline is not a selection",
			args:           []string{"-i", "input.mp4", "-codec=copy", "output.mp4"},
			expectedResult: "",
		},
		{
			name:           "video-specific overrides general -c copy",
			args:           []string{"-i", "input.mp4", "-c", "copy", "-c:v", "h264_qsv", "output.mp4"},
			expectedResult: encoder.EncoderH264QSV,
		},
		{
			name:           "inline -codec:v= does not override general -c copy",
			args:           []string{"-i", "input.mp4", "-c", "copy", "-codec:v=h264_qsv", "output.mp4"},
			expectedResult: encoder.EncoderFamily("copy"),
		},
		{
			name:           "inline -vcodec= does not override general -c copy",
			args:           []string{"-i", "input.mp4", "-c", "copy", "-vcodec=h264_qsv", "output.mp4"},
			expectedResult: encoder.EncoderFamily("copy"),
		},
		{
			name:           "bare -c non-copy codec left to auto-selection",
			args:           []string{"-i", "input.mp4", "-c", "h264", "output.mp4"},
			expectedResult: "",
		},
		{
			name:           "audio-only -c:a not treated as video",
			args:           []string{"-i", "input.mp4", "-c:a", "aac", "output.mp4"},
			expectedResult: "",
		},
		{
			name:           "no encoder",
			args:           []string{"-i", "input.mp4", "output.mp4"},
			expectedResult: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := adapter.parseEncoderFromArgs(tt.args, nil)
			if result != tt.expectedResult {
				t.Errorf("expected %s, got %s", tt.expectedResult, result)
			}
		})
	}
}

// TestRewriteAdapter_BareCCopyPassthrough is a regression test for the bug
// where bare "-c copy" was silently rewritten to a lossy hardware re-encode.
// The adapter must leave such args verbatim and resolve no target encoder.
func TestRewriteAdapter_BareCCopyPassthrough(t *testing.T) {
	adapter := NewRewriteAdapter()
	adapter.SetHardwareCapabilities(&protocol.WorkerCapabilities{
		VideoEncoders: []protocol.EncoderInfo{
			{Name: "h264_nvenc", Type: "video", IsHW: true},
		},
	})

	tests := []struct {
		name string
		args []string
	}{
		{"bare -c copy", []string{"-i", "input.mp4", "-c", "copy", "output.mp4"}},
		{"bare -c copy with audio re-encode", []string{"-i", "video.mp4", "-i", "audio.mp3", "-c", "copy", "-c:a", "aac", "merged.mp4"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rewritten, result, err := adapter.RewriteArgs(context.Background(), tt.args, true)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if result.Performed {
				t.Errorf("expected no rewrite, got target encoder %q", result.TargetEncoder)
			}
			if !slices.Equal(rewritten, tt.args) {
				t.Errorf("args must pass through verbatim, got %v", rewritten)
			}
			if target := adapter.ResolveTargetEncoder(tt.args, true); target != "" {
				t.Errorf("ResolveTargetEncoder = %q, want empty", target)
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

// TestRewriteAdapter_FallbackToSoftware_InlineCodecForm pins that the
// fallback helper itself leaves "=" joined encoder options untouched: they are
// not selections and must not be replaced by a software encoder.
func TestRewriteAdapter_FallbackToSoftware_InlineCodecForm(t *testing.T) {
	adapter := NewRewriteAdapter()
	hwCaps := &rewrite.HardwareCapabilities{
		SoftwareEncoders: []encoder.EncoderFamily{encoder.EncoderLibX264},
	}

	for _, args := range [][]string{
		{"-i", "input.mp4", "-codec:v=h264_nvenc", "output.mp4"},
		{"-i", "input.mp4", "-vcodec=h264_nvenc", "output.mp4"},
		{"-i", "input.mp4", "-c:v=h264_nvenc", "output.mp4"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			result, err := adapter.fallbackToSoftware(context.Background(), args, hwCaps)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !slices.Equal(result, args) {
				t.Errorf("fallback must leave = syntax unchanged: got %v, want %v", result, args)
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
	// auto-HW is disabled by passing false to RewriteArgs below

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
			rewritten, result, err := adapter.RewriteArgs(context.Background(), tt.args, false)

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
			name:           "skip encoder spec with codec:v= syntax",
			args:           []string{"-i", "input.mp4", "-codec:v=libx264", "-crf", "23", "output.mp4"},
			expectedParams: map[string]string{"crf": "23"},
		},
		{
			name:           "skip encoder spec with vcodec= syntax",
			args:           []string{"-i", "input.mp4", "-vcodec=libx264", "-crf", "23", "output.mp4"},
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
			result := adapter.parseEncoderParamsFromArgs(tt.args, nil)
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

// TestRewriteAdapter_NonEncoderParamsPreserved verifies that non-encoder
// parameters (like -preset, -movflags) survive the rewrite pipeline.
// Regression test for: -preset/-movflags were silently dropped when
// the rewrite engine re-emitted args, even though the DB stored them correctly.
func TestRewriteAdapter_NonEncoderParamsPreserved(t *testing.T) {
	adapter := NewRewriteAdapter()

	tests := []struct {
		name         string
		caps         *protocol.WorkerCapabilities
		args         []string
		expectInArgs []string
	}{
		{
			// x264 preset "fast" is translated to NVENC "p5" (same flag
			// name, converted value); -movflags has no translation rule
			// and must be preserved verbatim.
			name: "auto-hw upgrade to nvenc converts preset value fast->p5",
			caps: &protocol.WorkerCapabilities{
				VideoEncoders: []protocol.EncoderInfo{
					{Name: "h264_nvenc", Type: "video", IsHW: true},
					{Name: "libx264", Type: "video", IsHW: false},
				},
				GPUDevices: []protocol.GPUDeviceInfo{
					{Type: "nvenc", Vendor: "NVIDIA", Accessible: true},
				},
			},
			args:         []string{"-y", "-i", "input.mp4", "-c:v", "libx264", "-preset", "fast", "-movflags", "+faststart", "output.mp4"},
			expectInArgs: []string{"-preset", "p5", "-movflags", "+faststart"},
		},
		{
			name:         "no encoder specified with no HW keeps -preset and -movflags",
			caps:         &protocol.WorkerCapabilities{},
			args:         []string{"-y", "-i", "input.mp4", "-preset", "fast", "-movflags", "+faststart", "output.mp4"},
			expectInArgs: []string{"-preset", "fast", "-movflags", "+faststart"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			adapter.SetHardwareCapabilities(tt.caps)
			rewritten, _, err := adapter.RewriteArgs(context.Background(), tt.args, true)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			for i := 0; i+1 < len(tt.expectInArgs); i += 2 {
				flag, value := tt.expectInArgs[i], tt.expectInArgs[i+1]
				foundPair := false
				for j := 0; j+1 < len(rewritten); j++ {
					if rewritten[j] == flag && rewritten[j+1] == value {
						foundPair = true
						break
					}
				}
				if !foundPair {
					t.Errorf("expected %s %s to be preserved as a flag/value pair in rewritten args, got %v", flag, value, rewritten)
				}
			}
		})
	}
}

// TestRewriteAdapter_AutoHWPresetCompatibility covers scenario 5a:
// when the worker's preferred encoder is a hardware encoder that only supports
// a subset of x264 preset names (h264_qsv rejects "ultrafast"), an
// auto-selected hardware upgrade must translate the preset to a supported
// value instead of passing the incompatible name through verbatim.
func TestRewriteAdapter_AutoHWPresetCompatibility(t *testing.T) {
	adapter := NewRewriteAdapter()

	tests := []struct {
		name         string
		caps         *protocol.WorkerCapabilities
		args         []string
		expectPair   []string
		expectAbsent []string
	}{
		{
			name: "auto-selected qsv translates ultrafast to veryfast",
			caps: &protocol.WorkerCapabilities{
				VideoEncoders: []protocol.EncoderInfo{
					{Name: "h264_qsv", Type: "video", IsHW: true},
					{Name: "libx264", Type: "video", IsHW: false},
				},
				GPUDevices: []protocol.GPUDeviceInfo{
					{Type: "qsv", Vendor: "Intel", Accessible: true},
				},
			},
			args:         []string{"-i", "input.mp4", "-preset", "ultrafast", "output.mp4"},
			expectPair:   []string{"-preset", "7"},
			expectAbsent: []string{"ultrafast"},
		},
		{
			name: "auto-selected nvenc translates ultrafast to p1",
			caps: &protocol.WorkerCapabilities{
				VideoEncoders: []protocol.EncoderInfo{
					{Name: "h264_nvenc", Type: "video", IsHW: true},
					{Name: "libx264", Type: "video", IsHW: false},
				},
				GPUDevices: []protocol.GPUDeviceInfo{
					{Type: "nvenc", Vendor: "NVIDIA", Accessible: true},
				},
			},
			args:         []string{"-i", "input.mp4", "-preset", "ultrafast", "output.mp4"},
			expectPair:   []string{"-preset", "p1"},
			expectAbsent: []string{"ultrafast"},
		},
		{
			name: "auto-selected amf translates ultrafast to speed quality",
			caps: &protocol.WorkerCapabilities{
				VideoEncoders: []protocol.EncoderInfo{
					{Name: "h264_amf", Type: "video", IsHW: true},
					{Name: "libx264", Type: "video", IsHW: false},
				},
				GPUDevices: []protocol.GPUDeviceInfo{
					{Type: "amf", Vendor: "AMD", Accessible: true},
				},
			},
			args:         []string{"-i", "input.mp4", "-preset", "ultrafast", "output.mp4"},
			expectPair:   []string{"-quality", "speed"},
			expectAbsent: []string{"ultrafast"},
		},
		{
			name: "auto-selected qsv converts slower to its TargetUsage",
			caps: &protocol.WorkerCapabilities{
				VideoEncoders: []protocol.EncoderInfo{
					{Name: "h264_qsv", Type: "video", IsHW: true},
					{Name: "libx264", Type: "video", IsHW: false},
				},
				GPUDevices: []protocol.GPUDeviceInfo{
					{Type: "qsv", Vendor: "Intel", Accessible: true},
				},
			},
			args:         []string{"-i", "input.mp4", "-preset", "slower", "output.mp4"},
			expectPair:   []string{"-preset", "2"},
			expectAbsent: []string{"slower"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			adapter.SetHardwareCapabilities(tt.caps)
			rewritten, _, err := adapter.RewriteArgs(context.Background(), tt.args, true)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			flag, value := tt.expectPair[0], tt.expectPair[1]
			found := false
			for i := 0; i+1 < len(rewritten); i++ {
				if rewritten[i] == flag && rewritten[i+1] == value {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("expected %s %s in rewritten args, got %v", flag, value, rewritten)
			}

			for _, absent := range tt.expectAbsent {
				for _, arg := range rewritten {
					if arg == absent {
						t.Errorf("expected %q to be absent from rewritten args, got %v", absent, rewritten)
					}
				}
			}
		})
	}
}

// TestRewriteAdapter_PassThroughNotification pins the default-path
// transparency contract: an explicit software encoder without --auto-hw is
// left unchanged, but the user still gets a [rffmpeg] line confirming rffmpeg
// inspected the request ("no silent rewrite").
func TestRewriteAdapter_PassThroughNotification(t *testing.T) {
	adapter := NewRewriteAdapter()
	adapter.SetHardwareCapabilities(&protocol.WorkerCapabilities{
		VideoEncoders: []protocol.EncoderInfo{
			{Name: "h264_nvenc", Type: "video", IsHW: true},
			{Name: "libx264", Type: "video", IsHW: false},
		},
		GPUDevices: []protocol.GPUDeviceInfo{
			{Type: "nvenc", Vendor: "NVIDIA", Accessible: true},
		},
	})

	rewritten, result, err := adapter.RewriteArgs(
		context.Background(),
		[]string{"-i", "input.mp4", "-c:v", "libx264", "output.mp4"},
		false,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.Performed {
		t.Errorf("default path must not rewrite: performed=%v", result.Performed)
	}
	if result.TargetEncoder != "libx264" {
		t.Errorf("target encoder = %q, want libx264", result.TargetEncoder)
	}

	const want = "[rffmpeg] libx264 -> libx264 (Pass through unchanged)"
	var found bool
	for _, n := range result.Notifications {
		if n == want {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected pass-through notification %q in %v", want, result.Notifications)
	}

	// The user's args must survive verbatim: the default path must not
	// delete, reorder, append, or substitute any argument.
	wantArgs := []string{"-i", "input.mp4", "-c:v", "libx264", "output.mp4"}
	if !slices.Equal(rewritten, wantArgs) {
		t.Errorf("default path args changed: got %v, want %v", rewritten, wantArgs)
	}
}

// TestRewriteAdapter_AutoHWUpgradeReportsParamFate pins the CLI-visible
// contract of an auto-hw upgrade: alongside the encoder swap the user learns
// what became of the libx264 parameters they passed, and the mapped form is
// what actually reaches ffmpeg.
func TestRewriteAdapter_AutoHWUpgradeReportsParamFate(t *testing.T) {
	adapter := NewRewriteAdapter()
	adapter.SetHardwareCapabilities(&protocol.WorkerCapabilities{
		VideoEncoders: []protocol.EncoderInfo{
			{Name: "h264_qsv", Type: "video", IsHW: true},
			{Name: "libx264", Type: "video", IsHW: false},
		},
	})

	rewritten, result, err := adapter.RewriteArgs(
		context.Background(),
		[]string{"-i", "input.mp4", "-c:v", "libx264", "-crf", "23", "-preset", "fast", "output.mp4"},
		true,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	const wantNotice = "[rffmpeg] h264_qsv params: -crf 23 → -global_quality 23, -preset fast → -preset 6"
	if !slices.Contains(result.Notifications, wantNotice) {
		t.Errorf("missing parameter report %q in notifications %v", wantNotice, result.Notifications)
	}

	// Both the renamed (crf -> global_quality) and the re-valued (preset)
	// parameter must reach ffmpeg in the form the report claims.
	for _, wantPair := range [][]string{{"-global_quality", "23"}, {"-preset", "6"}} {
		found := false
		for i := 0; i+1 < len(rewritten); i++ {
			if rewritten[i] == wantPair[0] && rewritten[i+1] == wantPair[1] {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected %v in rewritten args, got %v", wantPair, rewritten)
		}
	}
	for _, arg := range rewritten {
		if arg == "-crf" {
			t.Errorf("original -crf must not survive the upgrade: %v", rewritten)
		}
	}
}

// TestRewriteAdapter_CapabilitiesSummaryMarksReportedHardware pins the summary
// the worker reports: every encoder it saw, in the reported order, with the
// hardware ones marked from the worker's own IsHW flag — not from the static
// EncoderFamily classifier, which only knows a fixed set of names. The summary
// stays complete; truncation for humans happens on the display line.
func TestRewriteAdapter_CapabilitiesSummaryMarksReportedHardware(t *testing.T) {
	tests := []struct {
		name        string
		encoders    []protocol.EncoderInfo
		wantSummary string
	}{
		{
			name:        "no encoders",
			wantSummary: "none",
		},
		{
			name: "reported hardware keeps its marker and order",
			encoders: []protocol.EncoderInfo{
				{Name: "libx264", Type: "video"},
				{Name: "h264_qsv", Type: "video", IsHW: true},
				{Name: "libx265", Type: "video"},
			},
			wantSummary: "libx264,h264_qsv[HW],libx265",
		},
		{
			// The probe classifies hardware by name suffix, so it reports
			// encoders the static family classifier does not know. Missing the
			// marker on these let them sort into the truncatable software group
			// and disappear from the display line behind "[... +N more]".
			name: "reported hardware unknown to the static classifier is marked",
			encoders: []protocol.EncoderInfo{
				{Name: "av1_amf", Type: "video", IsHW: true},
				{Name: "vp9_amf", Type: "video", IsHW: true},
				{Name: "mpeg2_vaapi", Type: "video", IsHW: true},
				{Name: "mjpeg_qsv", Type: "video", IsHW: true},
				{Name: "libx264", Type: "video"},
			},
			wantSummary: "av1_amf[HW],vp9_amf[HW],mpeg2_vaapi[HW],mjpeg_qsv[HW],libx264",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			adapter := NewRewriteAdapter()
			hwCaps := adapter.convertCapabilities(&protocol.WorkerCapabilities{VideoEncoders: tt.encoders})

			if got := adapter.buildCapabilitiesSummary(hwCaps); got != tt.wantSummary {
				t.Errorf("buildCapabilitiesSummary() = %q, want %q", got, tt.wantSummary)
			}
		})
	}

	if got := NewRewriteAdapter().buildCapabilitiesSummary(nil); got != "unknown" {
		t.Errorf("buildCapabilitiesSummary(nil) = %q, want %q", got, "unknown")
	}
}

// TestRewriteChainLineStaysBoundedForFullFFmpegBuild is the regression for the
// full-screen stderr dump: a worker reporting a stock ffmpeg build's video
// encoders must stream a single terminal-friendly line to CLI clients, with
// every hardware encoder still visible — including the ones the static
// classifier does not know.
func TestRewriteChainLineStaysBoundedForFullFFmpegBuild(t *testing.T) {
	encoders := append([]protocol.EncoderInfo{
		{Name: "av1_amf", Type: "video", IsHW: true},
		{Name: "mjpeg_qsv", Type: "video", IsHW: true},
	}, testEncoderInfos(4, 125)...)

	adapter := NewRewriteAdapter()
	adapter.SetHardwareCapabilities(&protocol.WorkerCapabilities{
		VideoEncoders: encoders,
		GPUDevices:    []protocol.GPUDeviceInfo{{Type: "nvenc", Vendor: "NVIDIA", Accessible: true}},
	})

	_, result, err := adapter.RewriteArgs(
		context.Background(),
		[]string{"-i", "input.mp4", "-c:v", "libx264", "output.mp4"},
		true,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	line := audit.FormatRewriteChainLine(
		result.CapabilitiesSummary, "libx264", result.TargetEncoder, result.DecisionReason, audit.InfoLevel)

	if strings.Count(line, "\n") != 1 {
		t.Errorf("rewrite chain must stay one line, got %q", line)
	}
	// 125 software encoders push the tail behind [... +N more]; hardware
	// encoders the classifier does not recognise must still be on the line.
	for _, want := range []string{"av1_amf[HW]", "mjpeg_qsv[HW]", "h264_nvenc[HW]", "[... +115 more]"} {
		if !strings.Contains(line, want) {
			t.Errorf("capabilities line missing %q, got %q", want, line)
		}
	}
	// 6 hardware names, 10 software names and one "[... +N more]" tail; listing
	// all 131 entries produced a line over 1 KB, i.e. several terminal screens.
	if len(line) > 512 {
		t.Errorf("rewrite chain line too long (%d bytes): %q", len(line), line)
	}
}

// TestRewriteAdapter_ExplicitEncoderSpellings covers every ffmpeg spelling of
// a video encoder selection: an available explicit encoder must survive
// verbatim and must never gain a second -c:v from the rewrite.
func TestRewriteAdapter_ExplicitEncoderSpellings(t *testing.T) {
	adapter := NewRewriteAdapter()
	adapter.SetHardwareCapabilities(&protocol.WorkerCapabilities{
		VideoEncoders: []protocol.EncoderInfo{
			{Name: "h264_nvenc", Type: "video", IsHW: true},
			{Name: "libvpx-vp9", Type: "video"},
		},
		GPUDevices: []protocol.GPUDeviceInfo{
			{Type: "nvenc", Vendor: "NVIDIA", Accessible: true},
		},
	})

	forms := []struct {
		name string
		args []string
	}{
		{name: "generic -c", args: []string{"-i", "in.mp4", "-c", "libvpx-vp9", "out.webm"}},
		{name: "stream specifier -c:v:0", args: []string{"-i", "in.mp4", "-c:v:0", "libvpx-vp9", "out.webm"}},
		{name: "stream specifier -codec:v:0", args: []string{"-i", "in.mp4", "-codec:v:0", "libvpx-vp9", "out.webm"}},
		{name: "inline stream specifier -c:v:0=", args: []string{"-i", "in.mp4", "-c:v:0=libvpx-vp9", "out.webm"}},
		{name: "inline -codec:v=", args: []string{"-i", "in.mp4", "-codec:v=libvpx-vp9", "out.webm"}},
	}

	for _, tt := range forms {
		t.Run(tt.name, func(t *testing.T) {
			rewritten, result, err := adapter.RewriteArgs(context.Background(), tt.args, false)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !slices.Equal(rewritten, tt.args) {
				t.Errorf("available encoder must pass through verbatim: got %v, want %v", rewritten, tt.args)
			}
			if result.Performed {
				t.Errorf("pass-through must not report a rewrite: %+v", result)
			}
			if slices.Contains(rewritten, "-c:v") {
				t.Errorf("must not append -c:v over the user's encoder: %v", rewritten)
			}
			if !strings.Contains(strings.Join(rewritten, " "), "libvpx-vp9") {
				t.Errorf("user encoder must survive: %v", rewritten)
			}
		})
	}
}

// TestRewriteAdapter_EncoderSpellingFallback verifies that the unrecognized
// spellings reach the same-family fallback path instead of being treated as
// "no encoder" and overridden with a cross-family target.
func TestRewriteAdapter_EncoderSpellingFallback(t *testing.T) {
	adapter := NewRewriteAdapter()
	adapter.SetHardwareCapabilities(&protocol.WorkerCapabilities{
		VideoEncoders: []protocol.EncoderInfo{
			{Name: "h264_nvenc", Type: "video", IsHW: true},
			{Name: "vp9_qsv", Type: "video", IsHW: true},
		},
		GPUDevices: []protocol.GPUDeviceInfo{
			{Type: "nvenc", Vendor: "NVIDIA", Accessible: true},
		},
	})

	forms := [][]string{
		{"-i", "in.mp4", "-c", "libvpx-vp9", "out.webm"},
		{"-i", "in.mp4", "-c:v:0", "libvpx-vp9", "out.webm"},
		{"-i", "in.mp4", "-codec:v", "libvpx-vp9", "out.webm"},
	}

	for _, args := range forms {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			rewritten, result, err := adapter.RewriteArgs(context.Background(), args, false)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if result.TargetEncoder != "vp9_qsv" {
				t.Errorf("target encoder = %q, want vp9_qsv (same family)", result.TargetEncoder)
			}
			if !slices.Contains(rewritten, "vp9_qsv") {
				t.Errorf("expected fallback encoder in %v", rewritten)
			}
			if slices.Contains(rewritten, "h264_nvenc") || slices.Contains(rewritten, "libvpx-vp9") {
				t.Errorf("user/other-family encoder must not survive: %v", rewritten)
			}
			var notified bool
			for _, msg := range result.Notifications {
				if strings.Contains(msg, "libvpx-vp9 unavailable, fallback to vp9_qsv") {
					notified = true
				}
			}
			if !notified {
				t.Errorf("notification must name the user's encoder: %v", result.Notifications)
			}
		})
	}
}

// TestRewriteAdapter_ParamFateNamesUserParams pins the parameter destination
// line for an unrecognized encoder spelling: the user's crf mapping must be
// reported, and hardware injection must not appear as a user parameter.
func TestRewriteAdapter_ParamFateNamesUserParams(t *testing.T) {
	adapter := NewRewriteAdapter()
	adapter.SetHardwareCapabilities(&protocol.WorkerCapabilities{
		VideoEncoders: []protocol.EncoderInfo{
			{Name: "vp9_qsv", Type: "video", IsHW: true},
		},
		GPUDevices: []protocol.GPUDeviceInfo{
			{Type: "qsv", Vendor: "Intel", Accessible: true, QSVHealthy: true},
		},
	})

	args := []string{"-i", "in.mp4", "-c:v:0", "libvpx-vp9", "-crf", "23", "out.webm"}
	rewritten, result, err := adapter.RewriteArgs(context.Background(), args, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !slices.Contains(rewritten, "vp9_qsv") {
		t.Fatalf("expected vp9_qsv fallback in %v", rewritten)
	}
	var fate string
	for _, msg := range result.Notifications {
		if strings.Contains(msg, " params: ") {
			fate = msg
		}
	}
	if !strings.Contains(fate, "-crf 23 → -global_quality 23") {
		t.Errorf("parameter fate line missing crf mapping: %v", result.Notifications)
	}
	for _, injected := range []string{"-init_hw_device", "-qsv_device", "-async_depth"} {
		if strings.Contains(fate, injected) {
			t.Errorf("hardware injection %q must not be reported as a user parameter: %q", injected, fate)
		}
	}
}

// TestRewriteAdapter_InjectionDoesNotOverrideUserParam is the C-73 guard: a
// hardware parameter injected by the target encoder must not replace the same
// parameter the user supplied.
func TestRewriteAdapter_InjectionDoesNotOverrideUserParam(t *testing.T) {
	adapter := NewRewriteAdapter()
	adapter.SetHardwareCapabilities(&protocol.WorkerCapabilities{
		VideoEncoders: []protocol.EncoderInfo{
			{Name: "h264_nvenc", Type: "video", IsHW: true},
			{Name: "libx264", Type: "video"},
		},
		GPUDevices: []protocol.GPUDeviceInfo{
			{Type: "nvenc", Vendor: "NVIDIA", Path: "0", Accessible: true},
		},
	})

	args := []string{"-i", "in.mp4", "-c:v", "libx264", "-gpu", "3", "out.mp4"}
	rewritten, _, err := adapter.RewriteArgs(context.Background(), args, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !slices.Contains(rewritten, "h264_nvenc") {
		t.Fatalf("expected h264_nvenc upgrade in %v", rewritten)
	}
	for i := 0; i+1 < len(rewritten); i++ {
		if rewritten[i] != "-gpu" {
			continue
		}
		if rewritten[i+1] != "3" {
			t.Errorf("injected -gpu %q overrode the user's -gpu 3: %v", rewritten[i+1], rewritten)
		}
	}
}

// TestRewriteAdapter_CopySpellingsPassThrough covers stream-copy written with
// a stream specifier: it must not be mistaken for "no encoder" and injected.
func TestRewriteAdapter_CopySpellingsPassThrough(t *testing.T) {
	adapter := NewRewriteAdapter()
	adapter.SetHardwareCapabilities(&protocol.WorkerCapabilities{
		VideoEncoders: []protocol.EncoderInfo{
			{Name: "h264_nvenc", Type: "video", IsHW: true},
		},
		GPUDevices: []protocol.GPUDeviceInfo{
			{Type: "nvenc", Vendor: "NVIDIA", Accessible: true},
		},
	})

	forms := [][]string{
		{"-i", "in.mp4", "-c:v:0", "copy", "out.mp4"},
		{"-i", "in.mp4", "-codec:v:0", "copy", "out.mp4"},
		{"-i", "in.mp4", "-c:v:0=copy", "out.mp4"},
	}
	for _, args := range forms {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			rewritten, _, err := adapter.RewriteArgs(context.Background(), args, true)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !slices.Equal(rewritten, args) {
				t.Errorf("stream copy must pass through verbatim: got %v, want %v", rewritten, args)
			}
		})
	}
}

// TestRewriteAdapter_DuplicateEncoderDeclaration covers repeated declarations
// of the same video stream specifier: ffmpeg applies the last one, so the
// fallback must replace the winning declaration instead of leaving a stale
// earlier value in the argument list.
func TestRewriteAdapter_DuplicateEncoderDeclaration(t *testing.T) {
	adapter := NewRewriteAdapter()
	adapter.SetHardwareCapabilities(&protocol.WorkerCapabilities{
		VideoEncoders: []protocol.EncoderInfo{
			{Name: "vp9_qsv", Type: "video", IsHW: true},
		},
		GPUDevices: []protocol.GPUDeviceInfo{
			{Type: "qsv", Vendor: "Intel", Accessible: true, QSVHealthy: true},
		},
	})

	args := []string{"-i", "in.mp4", "-c:v", "libvpx-vp9", "-c:v", "libvpx-vp9", "out.webm"}
	rewritten, result, err := adapter.RewriteArgs(context.Background(), args, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.TargetEncoder != "vp9_qsv" {
		t.Errorf("target encoder = %q, want vp9_qsv", result.TargetEncoder)
	}
	var encoderValues []string
	for i, arg := range rewritten {
		if arg == "-c:v" && i+1 < len(rewritten) {
			encoderValues = append(encoderValues, rewritten[i+1])
		}
	}
	if len(encoderValues) == 0 {
		t.Fatalf("no video encoder declaration in %v", rewritten)
	}
	if encoderValues[len(encoderValues)-1] != "vp9_qsv" {
		t.Errorf("last video encoder declaration must be the fallback target, got %v in %v", encoderValues, rewritten)
	}
}

// TestRewriteAdapter_CopyPreservedDuringEncoderRewrite exercises the engine
// rewrite path (unlike the bare-copy passthrough test, which short-circuits)
// with a general "-c copy" present: the copy directive must survive untouched
// and only the explicit video declaration may be rewritten.
func TestRewriteAdapter_CopyPreservedDuringEncoderRewrite(t *testing.T) {
	adapter := NewRewriteAdapter()
	adapter.SetHardwareCapabilities(&protocol.WorkerCapabilities{
		VideoEncoders: []protocol.EncoderInfo{
			{Name: "vp9_qsv", Type: "video", IsHW: true},
		},
		GPUDevices: []protocol.GPUDeviceInfo{
			{Type: "qsv", Vendor: "Intel", Accessible: true, QSVHealthy: true},
		},
	})

	forms := [][]string{
		{"-i", "in.mp4", "-c", "copy", "-c:v", "libvpx-vp9", "out.mkv"},
		{"-i", "in.mp4", "-c:v", "libvpx-vp9", "-c", "copy", "out.mkv"},
	}
	for _, args := range forms {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			rewritten, result, err := adapter.RewriteArgs(context.Background(), args, false)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if result.TargetEncoder != "vp9_qsv" {
				t.Errorf("target encoder = %q, want vp9_qsv", result.TargetEncoder)
			}
			var copyPairs int
			for i := 0; i+1 < len(rewritten); i++ {
				if rewritten[i] == "-c" && rewritten[i+1] == "copy" {
					copyPairs++
				}
			}
			if copyPairs != 1 {
				t.Errorf("-c copy must survive the rewrite, got %v", rewritten)
			}
			if slices.Contains(rewritten, "libvpx-vp9") {
				t.Errorf("explicit encoder should be rewritten, got %v", rewritten)
			}
		})
	}
}
