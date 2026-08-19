package worker

import (
	"testing"
)

func TestPixelFormatChecker_isUnsupportedPixelFormatForVAAPI(t *testing.T) {
	checker := &PixelFormatChecker{}

	tests := []struct {
		name        string
		pixelFormat string
		unsupported bool
	}{
		// Supported formats
		{"yuv420p", "yuv420p", false},
		{"nv12", "nv12", false},
		{"nv21", "nv21", false},
		{"yuv420p10", "yuv420p10", false},

		// Unsupported formats - yuv444p variants
		{"yuv444p", "yuv444p", true},
		{"yuv444p10", "yuv444p10", true},
		{"yuv444p12", "yuv444p12", true},
		{"yuv444p16", "yuv444p16", true},

		// Unsupported formats - yuv422p variants
		{"yuv422p", "yuv422p", true},
		{"yuv422p10", "yuv422p10", true},
		{"yuv422p12", "yuv422p12", true},
		{"yuv422p16", "yuv422p16", true},

		// Unsupported formats - RGB
		{"rgb24", "rgb24", true},
		{"rgba", "rgba", true},
		{"bgr24", "bgr24", true},
		{"bgra", "bgra", true},

		// Unsupported formats - Planar RGB
		{"gbrp", "gbrp", true},
		{"gbrp10", "gbrp10", true},

		// Unsupported formats - Grayscale
		{"gray", "gray", true},
		{"gray10", "gray10", true},
		{"gray16", "gray16", true},

		// Unsupported formats - XYZ
		{"xyz12", "xyz12", true},
		{"xyz12le", "xyz12le", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := checker.isUnsupportedPixelFormatForVAAPI(tt.pixelFormat)
			if result != tt.unsupported {
				t.Errorf("isUnsupportedPixelFormatForVAAPI(%q) = %v, want %v", tt.pixelFormat, result, tt.unsupported)
			}
		})
	}
}

func TestIsVAAPIEncoder(t *testing.T) {
	tests := []struct {
		name     string
		encoder  string
		expected bool
	}{
		{"h264_vaapi", "h264_vaapi", true},
		{"hevc_vaapi", "hevc_vaapi", true},
		{"vp9_vaapi", "vp9_vaapi", true},
		{"av1_vaapi", "av1_vaapi", true},
		{"H264_VAAPI", "H264_VAAPI", true}, // case insensitive
		{"h264_nvenc", "h264_nvenc", false},
		{"h264_qsv", "h264_qsv", false},
		{"libx264", "libx264", false},
		{"copy", "copy", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isVAAPIEncoder(tt.encoder)
			if result != tt.expected {
				t.Errorf("isVAAPIEncoder(%q) = %v, want %v", tt.encoder, result, tt.expected)
			}
		})
	}
}

func TestPixelFormatChecker_extractPixelFormat(t *testing.T) {
	checker := &PixelFormatChecker{}

	tests := []struct {
		name     string
		result   *FFprobeResult
		expected string
	}{
		{
			name: "video stream with pix_fmt",
			result: &FFprobeResult{
				Streams: []map[string]interface{}{
					{
						"codec_type": "video",
						"pix_fmt":    "yuv420p",
					},
				},
			},
			expected: "yuv420p",
		},
		{
			name: "audio stream first, then video",
			result: &FFprobeResult{
				Streams: []map[string]interface{}{
					{
						"codec_type": "audio",
						"pix_fmt":    "fltp",
					},
					{
						"codec_type": "video",
						"pix_fmt":    "yuv444p",
					},
				},
			},
			expected: "yuv444p",
		},
		{
			name: "no video stream",
			result: &FFprobeResult{
				Streams: []map[string]interface{}{
					{
						"codec_type": "audio",
					},
				},
			},
			expected: "",
		},
		{
			name:     "nil result",
			result:   nil,
			expected: "",
		},
		{
			name: "video stream without pix_fmt",
			result: &FFprobeResult{
				Streams: []map[string]interface{}{
					{
						"codec_type": "video",
					},
				},
			},
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := checker.extractPixelFormat(tt.result)
			if result != tt.expected {
				t.Errorf("extractPixelFormat() = %q, want %q", result, tt.expected)
			}
		})
	}
}

func TestPixelFormatChecker_getSoftwareFallbackEncoder(t *testing.T) {
	checker := &PixelFormatChecker{}

	tests := []struct {
		name            string
		vaapiEncoder    string
		expectedContain string
	}{
		{"h264_vaapi", "h264_vaapi", "libx264"},
		{"hevc_vaapi", "hevc_vaapi", "libx265"},
		{"vp9_vaapi", "vp9_vaapi", "libvpx-vp9"},
		{"av1_vaapi", "av1_vaapi", "libaom-av1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := checker.getSoftwareFallbackEncoder(tt.vaapiEncoder)
			if result == "" {
				t.Errorf("getSoftwareFallbackEncoder(%q) returned empty string", tt.vaapiEncoder)
			}
			if tt.expectedContain != "" && !containsString(result, tt.expectedContain) {
				t.Errorf("getSoftwareFallbackEncoder(%q) = %q, want to contain %q", tt.vaapiEncoder, result, tt.expectedContain)
			}
		})
	}
}

func containsString(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(substr) == 0 || containsSubstring(s, substr))
}

func containsSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
