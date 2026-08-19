package scheduler

import (
	"testing"
)

func TestGetCodecFamily(t *testing.T) {
	tests := []struct {
		name       string
		encoder    string
		wantFamily CodecFamily
	}{
		// H.264 encoders
		{"libx264 software", "libx264", CodecFamilyH264},
		{"h264_nvenc hardware", "h264_nvenc", CodecFamilyH264},
		{"h264_qsv hardware", "h264_qsv", CodecFamilyH264},
		{"h264_vaapi hardware", "h264_vaapi", CodecFamilyH264},
		{"h264_amf hardware", "h264_amf", CodecFamilyH264},
		{"h264_videotoolbox", "h264_videotoolbox", CodecFamilyH264},
		{"libx264rgb", "libx264rgb", CodecFamilyH264},

		// HEVC encoders
		{"libx265 software", "libx265", CodecFamilyHEVC},
		{"hevc_nvenc hardware", "hevc_nvenc", CodecFamilyHEVC},
		{"hevc_qsv hardware", "hevc_qsv", CodecFamilyHEVC},
		{"hevc_vaapi hardware", "hevc_vaapi", CodecFamilyHEVC},
		{"hevc_amf hardware", "hevc_amf", CodecFamilyHEVC},
		{"hevc_videotoolbox", "hevc_videotoolbox", CodecFamilyHEVC},

		// VP9 encoders
		{"libvpx-vp9", "libvpx-vp9", CodecFamilyVP9},
		{"vp9_qsv", "vp9_qsv", CodecFamilyVP9},
		{"vp9_vaapi", "vp9_vaapi", CodecFamilyVP9},

		// AV1 encoders
		{"libaom-av1", "libaom-av1", CodecFamilyAV1},
		{"libsvtav1", "libsvtav1", CodecFamilyAV1},
		{"av1_nvenc", "av1_nvenc", CodecFamilyAV1},
		{"av1_qsv", "av1_qsv", CodecFamilyAV1},
		{"av1_vaapi", "av1_vaapi", CodecFamilyAV1},

		// Unknown encoder pattern inference
		{"unknown_h264_encoder", "unknown_h264_encoder", CodecFamilyH264},
		{"custom_hevc_impl", "custom_hevc_impl", CodecFamilyHEVC},
		{"my_vp9_enc", "my_vp9_enc", CodecFamilyVP9},
		{"av1_custom", "av1_custom", CodecFamilyAV1},

		// Unknown
		{"unknown encoder", "some_unknown_encoder", CodecFamily("")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := GetCodecFamily(tt.encoder)
			if got != tt.wantFamily {
				t.Errorf("GetCodecFamily(%q) = %q, want %q", tt.encoder, got, tt.wantFamily)
			}
		})
	}
}

func TestGetCompatibleEncoders(t *testing.T) {
	tests := []struct {
		name         string
		encoder      string
		wantCount    int
		wantFirst    string // Check that first encoder in list is expected
		wantContains string // Check that the list contains this encoder
	}{
		{
			name:         "h264_nvenc returns H.264 family encoders",
			encoder:      "h264_nvenc",
			wantCount:    7,
			wantContains: "libx264",
		},
		{
			name:         "libx264 returns H.264 family encoders",
			encoder:      "libx264",
			wantCount:    7,
			wantContains: "h264_nvenc",
		},
		{
			name:         "hevc_qsv returns HEVC family encoders",
			encoder:      "hevc_qsv",
			wantCount:    7,
			wantContains: "libx265",
		},
		{
			name:         "libx265 returns HEVC family encoders",
			encoder:      "libx265",
			wantCount:    7,
			wantContains: "hevc_nvenc",
		},
		{
			name:      "unknown encoder returns nil",
			encoder:   "unknown_encoder_xyz",
			wantCount: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := GetCompatibleEncoders(tt.encoder)
			if len(got) != tt.wantCount {
				t.Errorf("GetCompatibleEncoders(%q) returned %d encoders, want %d", tt.encoder, len(got), tt.wantCount)
			}

			if tt.wantContains != "" && len(got) > 0 {
				found := false
				for _, enc := range got {
					if enc == tt.wantContains {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("GetCompatibleEncoders(%q) missing expected encoder %q", tt.encoder, tt.wantContains)
				}
			}
		})
	}
}

func TestIsHardwareEncoder(t *testing.T) {
	tests := []struct {
		name     string
		encoder  string
		expected bool
	}{
		{"h264_nvenc is hardware", "h264_nvenc", true},
		{"h264_qsv is hardware", "h264_qsv", true},
		{"h264_vaapi is hardware", "h264_vaapi", true},
		{"h264_amf is hardware", "h264_amf", true},
		{"h264_videotoolbox is hardware", "h264_videotoolbox", true},
		{"hevc_nvenc is hardware", "hevc_nvenc", true},
		{"av1_nvenc is hardware", "av1_nvenc", true},
		{"libx264 is software", "libx264", false},
		{"libx265 is software", "libx265", false},
		{"libvpx-vp9 is software", "libvpx-vp9", false},
		{"libaom-av1 is software", "libaom-av1", false},
		{"mpeg2video is software", "mpeg2video", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsHardwareEncoder(tt.encoder)
			if got != tt.expected {
				t.Errorf("IsHardwareEncoder(%q) = %v, want %v", tt.encoder, got, tt.expected)
			}
		})
	}
}

func TestIsSoftwareEncoder(t *testing.T) {
	tests := []struct {
		name     string
		encoder  string
		expected bool
	}{
		{"libx264 is software", "libx264", true},
		{"libx265 is software", "libx265", true},
		{"libvpx-vp9 is software", "libvpx-vp9", true},
		{"libaom-av1 is software", "libaom-av1", true},
		{"libsvtav1 is software", "libsvtav1", true},
		{"mpeg2video is software", "mpeg2video", true},
		{"mpeg4 is software", "mpeg4", true},
		{"h264_nvenc is not software", "h264_nvenc", false},
		{"hevc_qsv is not software", "hevc_qsv", false},
		{"av1_vaapi is not software", "av1_vaapi", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsSoftwareEncoder(tt.encoder)
			if got != tt.expected {
				t.Errorf("IsSoftwareEncoder(%q) = %v, want %v", tt.encoder, got, tt.expected)
			}
		})
	}
}
