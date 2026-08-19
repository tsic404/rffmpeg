package scheduler

import (
	"encoding/json"
	"strings"
)

// passthroughEncoders are FFmpeg stream copy directives that are NOT real
// encoders. They instruct ffmpeg to copy streams without re-encoding and
// should never trigger encoder-capability-aware worker scheduling.
var passthroughEncoders = map[string]bool{
	"copy": true,
}

// ExtractEncoderFromArgs extracts the video encoder name from ffmpeg arguments.
// It looks for common patterns like:
//
//	-c:v libx264
//	-c:v:0 libx264
//	-vcodec libx264
//	-codec:v libx264
//
// Returns empty string if no encoder is found or if the encoder is a
// passthrough directive (e.g. "copy") that any worker can handle.
func ExtractEncoderFromArgs(argsJSON string) string {
	var args []string
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return ""
	}

	for i := 0; i < len(args)-1; i++ {
		arg := args[i]
		nextArg := args[i+1]

		// Check for encoder options
		switch {
		case arg == "-c:v" || arg == "-codec:v":
			if passthroughEncoders[nextArg] {
				return ""
			}
			return nextArg
		case strings.HasPrefix(arg, "-c:v:") || strings.HasPrefix(arg, "-codec:v:"):
			if passthroughEncoders[nextArg] {
				return ""
			}
			return nextArg
		case arg == "-vcodec":
			if passthroughEncoders[nextArg] {
				return ""
			}
			return nextArg
		case arg == "-c" || arg == "-codec":
			// Generic codec option - could be video or audio
			// Check if it's a known video encoder
			if isVideoEncoder(nextArg) {
				return nextArg
			}
		}
	}

	return ""
}

// isVideoEncoder checks if the encoder name is a known video encoder
func isVideoEncoder(name string) bool {
	// Common video encoders
	videoEncoders := map[string]bool{
		// Software encoders
		"libx264":    true,
		"libx264rgb": true,
		"libx265":    true,
		"libx265rgb": true,
		"libvpx":     true,
		"libvpx-vp9": true,
		"libaom-av1": true,
		"librav1e":   true,
		"libsvtav1":  true,
		"mpeg2video": true,
		"mpeg4":      true,
		"msmpeg4":    true,
		"msmpeg4v2":  true,
		"wmv1":       true,
		"wmv2":       true,
		"flv":        true,
		"h261":       true,
		"h263":       true,
		"h263p":      true,
		"svq1":       true,
		"dvvideo":    true,

		// NVIDIA (NVENC)
		"h264_nvenc": true,
		"hevc_nvenc": true,
		"nvenc":      true,
		"nvenc_h264": true,
		"nvenc_hevc": true,
		"av1_nvenc":  true,

		// Intel (Quick Sync Video)
		"h264_qsv":  true,
		"hevc_qsv":  true,
		"av1_qsv":   true,
		"vp9_qsv":   true,
		"mpeg2_qsv": true,

		// AMD (AMF)
		"h264_amf": true,
		"hevc_amf": true,
		"av1_amf":  true,

		// VAAPI
		"h264_vaapi":  true,
		"hevc_vaapi":  true,
		"av1_vaapi":   true,
		"vp8_vaapi":   true,
		"vp9_vaapi":   true,
		"mpeg2_vaapi": true,

		// VideoToolbox (macOS)
		"h264_videotoolbox":   true,
		"hevc_videotoolbox":   true,
		"prores_videotoolbox": true,

		// Other hardware encoders
		"h264_v4l2m2m": true,
		"hevc_v4l2m2m": true,
	}

	return videoEncoders[name]
}
