package scheduler

import (
	"strings"
)

// CodecFamily represents a codec format family (h264, hevc, vp9, av1).
type CodecFamily string

const (
	CodecFamilyH264  CodecFamily = "h264"
	CodecFamilyHEVC  CodecFamily = "hevc"
	CodecFamilyVP9   CodecFamily = "vp9"
	CodecFamilyAV1   CodecFamily = "av1"
	CodecFamilyVP8   CodecFamily = "vp8"
	CodecFamilyMPEG2 CodecFamily = "mpeg2"
	CodecFamilyMPEG4 CodecFamily = "mpeg4"
)

// encoderFamilyMapping maps encoder names to their codec families.
// This enables the scheduler to find compatible encoders for fallback.
var encoderFamilyMapping = map[string]CodecFamily{
	// H.264/AVC encoders
	"libx264":           CodecFamilyH264,
	"libx264rgb":        CodecFamilyH264,
	"h264_nvenc":        CodecFamilyH264,
	"h264_qsv":          CodecFamilyH264,
	"h264_vaapi":        CodecFamilyH264,
	"h264_amf":          CodecFamilyH264,
	"h264_videotoolbox": CodecFamilyH264,
	"h264_v4l2m2m":      CodecFamilyH264,
	"h264_omx":          CodecFamilyH264,

	// H.265/HEVC encoders
	"libx265":           CodecFamilyHEVC,
	"libx265rgb":        CodecFamilyHEVC,
	"hevc_nvenc":        CodecFamilyHEVC,
	"hevc_qsv":          CodecFamilyHEVC,
	"hevc_vaapi":        CodecFamilyHEVC,
	"hevc_amf":          CodecFamilyHEVC,
	"hevc_videotoolbox": CodecFamilyHEVC,
	"hevc_v4l2m2m":      CodecFamilyHEVC,

	// VP9 encoders
	"libvpx-vp9":       CodecFamilyVP9,
	"vp9_nvenc":        CodecFamilyVP9,
	"vp9_qsv":          CodecFamilyVP9,
	"vp9_vaapi":        CodecFamilyVP9,
	"vp9_videotoolbox": CodecFamilyVP9,

	// AV1 encoders
	"libaom-av1": CodecFamilyAV1,
	"libsvtav1":  CodecFamilyAV1,
	"av1_nvenc":  CodecFamilyAV1,
	"av1_qsv":    CodecFamilyAV1,
	"av1_vaapi":  CodecFamilyAV1,
	"av1_amf":    CodecFamilyAV1,

	// VP8 encoders
	"libvpx":    CodecFamilyVP8,
	"vp8_vaapi": CodecFamilyVP8,

	// MPEG-2 encoders
	"mpeg2video":  CodecFamilyMPEG2,
	"mpeg2_qsv":   CodecFamilyMPEG2,
	"mpeg2_vaapi": CodecFamilyMPEG2,

	// MPEG-4 encoders
	"mpeg4": CodecFamilyMPEG4,
}

// encoderPriority defines the preferred order of encoders within each codec family.
// Hardware encoders are preferred over software when available.
// Lower index = higher priority.
var encoderPriority = map[CodecFamily][]string{
	CodecFamilyH264: {
		"h264_nvenc", "h264_qsv", "h264_vaapi", "h264_amf", "h264_videotoolbox",
		"libx264", "libx264rgb",
	},
	CodecFamilyHEVC: {
		"hevc_nvenc", "hevc_qsv", "hevc_vaapi", "hevc_amf", "hevc_videotoolbox",
		"libx265", "libx265rgb",
	},
	CodecFamilyVP9: {
		"vp9_nvenc", "vp9_qsv", "vp9_vaapi", "vp9_videotoolbox",
		"libvpx-vp9",
	},
	CodecFamilyAV1: {
		"av1_nvenc", "av1_qsv", "av1_vaapi", "av1_amf",
		"libsvtav1", "libaom-av1",
	},
	CodecFamilyVP8: {
		"vp8_vaapi",
		"libvpx",
	},
	CodecFamilyMPEG2: {
		"mpeg2_qsv", "mpeg2_vaapi",
		"mpeg2video",
	},
	CodecFamilyMPEG4: {
		"mpeg4",
	},
}

// GetCodecFamily returns the codec family for a given encoder name.
// Returns empty string if the encoder is unknown.
func GetCodecFamily(encoder string) CodecFamily {
	// Direct lookup
	if family, ok := encoderFamilyMapping[encoder]; ok {
		return family
	}

	// Fallback: infer from encoder name patterns
	encoderLower := strings.ToLower(encoder)
	switch {
	case strings.Contains(encoderLower, "h264") || strings.Contains(encoderLower, "avc"):
		return CodecFamilyH264
	case strings.Contains(encoderLower, "hevc") || strings.Contains(encoderLower, "h265") || strings.Contains(encoderLower, "x265"):
		return CodecFamilyHEVC
	case strings.Contains(encoderLower, "vp9"):
		return CodecFamilyVP9
	case strings.Contains(encoderLower, "av1"):
		return CodecFamilyAV1
	case strings.Contains(encoderLower, "vp8"):
		return CodecFamilyVP8
	case strings.Contains(encoderLower, "mpeg2"):
		return CodecFamilyMPEG2
	case strings.Contains(encoderLower, "mpeg4"):
		return CodecFamilyMPEG4
	}

	return ""
}

// GetCompatibleEncoders returns all encoders in the same codec family as the requested encoder.
// The returned list is sorted by priority (hardware encoders first).
// Returns nil if the encoder family is unknown.
func GetCompatibleEncoders(encoder string) []string {
	family := GetCodecFamily(encoder)
	if family == "" {
		return nil
	}

	if encoders, ok := encoderPriority[family]; ok {
		// Return a copy to avoid mutation
		result := make([]string, len(encoders))
		copy(result, encoders)
		return result
	}

	return nil
}

// EncoderFallbackConfig holds configuration for encoder fallback behavior.
type EncoderFallbackConfig struct {
	// Enabled controls whether fallback to compatible encoders is allowed.
	Enabled bool

	// PreferSoftwareEncoder controls whether software encoders are preferred
	// when no hardware encoder is available. If false, the job may remain pending.
	PreferSoftwareEncoder bool
}

// DefaultEncoderFallbackConfig returns the default fallback configuration.
func DefaultEncoderFallbackConfig() EncoderFallbackConfig {
	return EncoderFallbackConfig{
		Enabled:               true,
		PreferSoftwareEncoder: true,
	}
}

// IsHardwareEncoder checks if an encoder is hardware-accelerated.
func IsHardwareEncoder(encoder string) bool {
	hwSuffixes := []string{
		"_nvenc",
		"_qsv",
		"_vaapi",
		"_amf",
		"_videotoolbox",
		"_v4l2m2m",
		"_omx",
	}

	for _, suffix := range hwSuffixes {
		if strings.HasSuffix(encoder, suffix) {
			return true
		}
	}

	return false
}

// IsSoftwareEncoder checks if an encoder is a software encoder.
func IsSoftwareEncoder(encoder string) bool {
	softwareEncoders := map[string]bool{
		"libx264":    true,
		"libx264rgb": true,
		"libx265":    true,
		"libx265rgb": true,
		"libvpx":     true,
		"libvpx-vp9": true,
		"libaom-av1": true,
		"libsvtav1":  true,
		"mpeg2video": true,
		"mpeg4":      true,
	}

	return softwareEncoders[encoder]
}
