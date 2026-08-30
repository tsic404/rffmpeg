package worker

import (
	"log"
	"path/filepath"
	"sort"
	"strings"
)

// EncoderFallback manages software encoder fallback logic.
type EncoderFallback struct {
	// hardwareToSoftware maps hardware encoder names to their software equivalents.
	hardwareToSoftware map[string]string

	// formatEncoders maps codec formats to available software encoders.
	formatEncoders map[string][]string

	// softwareFallbackChain maps a software encoder to its next fallback alternative.
	// When a software encoder chosen as a fallback is also unavailable, the chain
	// provides an alternative instead of giving up. Entries MUST stay within the
	// same codec family (e.g. libaom-av1 -> libsvtav1); cross-format transitions
	// are rejected at read time by GetAlternativeSoftwareEncoder.
	softwareFallbackChain map[string]string
}

// NewEncoderFallback creates a new encoder fallback handler.
func NewEncoderFallback() *EncoderFallback {
	return &EncoderFallback{
		hardwareToSoftware:    defaultHardwareToSoftwareMap(),
		formatEncoders:        defaultFormatEncodersMap(),
		softwareFallbackChain: defaultSoftwareFallbackChain(),
	}
}

// defaultHardwareToSoftwareMap returns the default mapping from hardware to software encoders.
func defaultHardwareToSoftwareMap() map[string]string {
	return map[string]string{
		// H.264 encoders
		"h264_nvenc":        "libx264",
		"h264_qsv":          "libx264",
		"h264_vaapi":        "libx264",
		"h264_amf":          "libx264",
		"h264_videotoolbox": "libx264",

		// H.265/HEVC encoders
		"hevc_nvenc":        "libx265",
		"hevc_qsv":          "libx265",
		"hevc_vaapi":        "libx265",
		"hevc_amf":          "libx265",
		"hevc_videotoolbox": "libx265",

		// VP9 encoders
		"vp9_vaapi":        "libvpx-vp9",
		"vp9_qsv":          "libvpx-vp9",
		"vp9_videotoolbox": "libvpx-vp9",

		// AV1 encoders
		"av1_nvenc": "libaom-av1",
		"av1_qsv":   "libaom-av1",
		"av1_vaapi": "libaom-av1",
		"av1_amf":   "libaom-av1",
	}
}

// defaultFormatEncodersMap returns the default mapping from codec formats to software encoders.
func defaultFormatEncodersMap() map[string][]string {
	return map[string][]string{
		// H.264/AVC format
		"h264":   {"libx264", "libx264rgb"},
		"avc":    {"libx264", "libx264rgb"},
		"h264nv": {"libx264"},

		// H.265/HEVC format
		"hevc":   {"libx265"},
		"h265":   {"libx265"},
		"hevcnv": {"libx265"},

		// VP9 format
		"vp9": {"libvpx-vp9"},

		// AV1 format
		"av1": {"libaom-av1", "libsvtav1"},

		// VP8 format
		"vp8": {"libvpx"},

		// MPEG-2 format
		"mpeg2video": {"mpeg2video"},

		// MPEG-4 format
		"mpeg4": {"mpeg4"},

		// AVI format (often MJPEG)
		"mjpeg": {"mjpeg"},
	}
}

// defaultSoftwareFallbackChain returns the default chain for when a software
// encoder chosen as a fallback is itself unavailable. This enables multi-step
// fallback instead of giving up after the first software encoder fails.
// Each entry maps "current encoder" -> "next fallback to try".
//
// Only same-format transitions are allowed: silently re-encoding across codec
// families (e.g. AV1 -> H.264) changes the requested output format and is
// rejected rather than performed implicitly (TSI-2671).
func defaultSoftwareFallbackChain() map[string]string {
	return map[string]string{
		// AV1 software fallback chain (same format): libaom-av1 -> libsvtav1
		"libaom-av1": "libsvtav1",

		// H.264 software fallback chain (same format): libx264rgb -> libx264
		"libx264rgb": "libx264",
	}
}

// GetSoftwareEncoder returns the software encoder equivalent for a given encoder.
// If the encoder is already a software encoder, it returns the same encoder.
// If no equivalent is found, it returns an empty string.
func (f *EncoderFallback) GetSoftwareEncoder(encoder string) string {
	// Check if it's a hardware encoder with known software equivalent
	if sw, ok := f.hardwareToSoftware[encoder]; ok {
		return sw
	}

	// Check if it's already a software encoder
	if !f.IsHardwareEncoder(encoder) {
		return encoder
	}

	// Try to infer from encoder name
	return f.inferSoftwareEncoder(encoder)
}

// inferSoftwareEncoder tries to infer a software encoder from the encoder name.
func (f *EncoderFallback) inferSoftwareEncoder(encoder string) string {
	encoderLower := strings.ToLower(encoder)

	// Check for common patterns
	switch {
	case strings.Contains(encoderLower, "h264") || strings.Contains(encoderLower, "avc"):
		return "libx264"
	case strings.Contains(encoderLower, "hevc") || strings.Contains(encoderLower, "h265"):
		return "libx265"
	case strings.Contains(encoderLower, "vp9"):
		return "libvpx-vp9"
	case strings.Contains(encoderLower, "av1"):
		return "libaom-av1"
	case strings.Contains(encoderLower, "vp8"):
		return "libvpx"
	}

	return ""
}

// inferSoftwareEncoderFromContext tries to infer a software encoder from the
// command-line arguments context (output file extension, container format, etc.).
func (f *EncoderFallback) inferSoftwareEncoderFromContext(args []string, outputPath string) string {
	// First, try to infer from the output file extension
	ext := strings.ToLower(filepath.Ext(outputPath))
	switch ext {
	case ".mkv", ".mp4", ".mov", ".avi", ".flv", ".ts", ".m2ts":
		return "libx264" // H.264 is the most widely compatible
	case ".webm":
		return "libvpx-vp9" // WebM typically uses VP9
	case ".ivf":
		return "libvpx" // IVF containers often use VP8
	}

	// Try to find a container format flag in args
	for i, arg := range args {
		if arg == "-f" && i+1 < len(args) {
			format := strings.ToLower(args[i+1])
			switch format {
			case "mp4", "mov", "matroska", "flv":
				return "libx264"
			case "webm":
				return "libvpx-vp9"
			}
		}
	}

	return ""
}

// IsHardwareEncoder checks if an encoder is hardware-accelerated.
func (f *EncoderFallback) IsHardwareEncoder(encoder string) bool {
	hwSuffixes := []string{
		"_nvenc",
		"_qsv",
		"_vaapi",
		"_amf",
		"_videotoolbox",
		"_cuvid",
		"_vdpau",
		"_nvdec",
	}

	for _, suffix := range hwSuffixes {
		if strings.HasSuffix(encoder, suffix) {
			return true
		}
	}

	return false
}

// IsKnownSoftwareEncoder checks if an encoder is a recognized software encoder.
func (f *EncoderFallback) IsKnownSoftwareEncoder(encoder string) bool {
	for _, encoders := range f.formatEncoders {
		for _, e := range encoders {
			if e == encoder {
				return true
			}
		}
	}
	// Also check if it's the software fallback target of any HW encoder
	for _, sw := range f.hardwareToSoftware {
		if sw == encoder {
			return true
		}
	}
	return false
}

// PrepareFallbackArgs prepares ffmpeg arguments for software encoder fallback.
// isUserSelected reports whether the current encoder was explicitly chosen by
// the user (true) or by the system as a fallback from a previous failure (false).
// When false, the fallback chain is consulted to try the next alternative
// instead of giving up on a known-but-unavailable software encoder.
// Returns nil if no valid fallback can be determined.
func (f *EncoderFallback) PrepareFallbackArgs(args []string, outputPath string, isUserSelected bool) []string {
	return f.prepareFallbackArgsWithSource(args, outputPath, !isUserSelected)
}

// PrepareFallbackArgsWithSource prepares ffmpeg arguments for software encoder fallback
// with explicit control over whether the current encoder was user-selected.
//
// Parameters:
//   - args: current ffmpeg arguments
//   - outputPath: output file path
//   - isUserSelected: true if the current encoder was explicitly chosen by the user;
//     false if it was selected by the system as a fallback from a previous failure.
//
// When isUserSelected is false and the current encoder is a known software encoder
// that is the same as what GetSoftwareEncoder returns, the fallback chain is consulted
// to try the next alternative instead of giving up.
func (f *EncoderFallback) PrepareFallbackArgsWithSource(args []string, outputPath string, isUserSelected bool) []string {
	return f.prepareFallbackArgsWithSource(args, outputPath, !isUserSelected)
}

// prepareFallbackArgsWithSource is the internal implementation.
// isFallback means the current encoder was a system-chosen fallback, not user-selected.
func (f *EncoderFallback) prepareFallbackArgsWithSource(args []string, outputPath string, isFallback bool) []string {
	// Find the current encoder
	currentEncoder := extractEncoderFromArgs(args)
	if currentEncoder == "" {
		return nil
	}

	// Get software equivalent
	swEncoder := f.GetSoftwareEncoder(currentEncoder)
	if swEncoder == "" || swEncoder == currentEncoder {
		// The current encoder is not a known HW encoder. Check if it's a known SW encoder.
		if f.IsKnownSoftwareEncoder(currentEncoder) {
			// It's a known software encoder. If it's a fallback (not user-selected)
			// AND it matches the current encoder (no HW->SW mapping needed), try the
			// fallback chain to find an alternative instead of giving up.
			if isFallback {
				if altEncoder := f.GetAlternativeSoftwareEncoder(currentEncoder); altEncoder != "" && altEncoder != currentEncoder {
					return f.buildFallbackArgs(args, altEncoder, outputPath)
				}
			}
			return nil
		}
		// The current encoder is unknown (not a known HW encoder and not a known SW encoder).
		// Try to infer a software encoder from the output file extension or other args.
		swEncoder = f.inferSoftwareEncoderFromContext(args, outputPath)
		if swEncoder == "" {
			// Last resort: default to libx264 as a safe software encoder
			swEncoder = "libx264"
		}
	}

	return f.buildFallbackArgs(args, swEncoder, outputPath)
}

// buildFallbackArgs creates new args with the given software encoder, stripping
// hardware-specific parameters.
func (f *EncoderFallback) buildFallbackArgs(args []string, swEncoder string, outputPath string) []string {
	// Create new args with software encoder
	result := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]

		// Replace encoder argument
		if (arg == "-c:v" || arg == "-vcodec" || arg == "-codec:v") && i+1 < len(args) {
			result = append(result, arg, swEncoder)
			i++ // Skip the next argument (old encoder value)
			continue
		}

		// Handle encoder in key=value format
		if strings.HasPrefix(arg, "-c:v=") {
			result = append(result, "-c:v="+swEncoder)
			continue
		}
		if strings.HasPrefix(arg, "-vcodec=") {
			result = append(result, "-vcodec="+swEncoder)
			continue
		}

		// Skip hardware-specific parameters
		if f.isHardwareSpecificParam(arg) {
			// Also skip the value if this param takes one
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++
			}
			continue
		}

		result = append(result, arg)
	}

	// Ensure output path is preserved
	if outputPath != "" {
		// Check if output is already in args
		if !hasOutputArg(result) {
			result = append(result, outputPath)
		}
	}

	return result
}

// isHardwareSpecificParam checks if a parameter is hardware-specific.
func (f *EncoderFallback) isHardwareSpecificParam(param string) bool {
	hwParams := map[string]bool{
		"-hwaccel":               true,
		"-hwaccel_device":        true,
		"-hwaccel_output_format": true,
		"-init_hw_device":        true,
		"-vaapi_device":          true,
		"-qsv_device":            true,
		"-gpu":                   true,
		"-surfaces":              true,
		"-delay":                 true,
		"-2pass":                 true,
		"-multipass":             true,
		"-rc":                    true,
		"-rc_lookahead":          true,
		"-rc-lookahead":          true,
		"-spatial_aq":            true,
		"-spatial-aq":            true,
		"-temporal_aq":           true,
		"-temporal-aq":           true,
		"-aq_strength":           true,
		"-aq-strength":           true,
		"-low_power":             true,
		"-low-power":             true,
		"-async_depth":           true,
		"-async-depth":           true,
		"-load_plugin":           true,
		"-load-plugin":           true,
		"-enforce_hrd":           true,
		"-enforce-hrd":           true,
		"-filler_data":           true,
		"-filler-data":           true,
		"-frame_skipping":        true,
		"-frame-skipping":        true,
		"-allow_sw":              true,
		"-allow-sw":              true,
		"-require_sw":            true,
		"-require-sw":            true,
	}

	return hwParams[param]
}

// GetEncoderFormat extracts the codec format from an encoder name.
// Returns the format (h264, hevc, vp9, etc.) or empty string if unknown.
func (f *EncoderFallback) GetEncoderFormat(encoder string) string {
	encoderLower := strings.ToLower(encoder)

	// Check for known format patterns
	switch {
	case strings.Contains(encoderLower, "h264") || strings.Contains(encoderLower, "avc") || strings.Contains(encoderLower, "x264"):
		return "h264"
	case strings.Contains(encoderLower, "hevc") || strings.Contains(encoderLower, "h265") || strings.Contains(encoderLower, "x265"):
		return "hevc"
	case strings.Contains(encoderLower, "vp9"):
		return "vp9"
	case strings.Contains(encoderLower, "av1"):
		return "av1"
	case strings.Contains(encoderLower, "vp8"):
		return "vp8"
	case strings.Contains(encoderLower, "mpeg2"):
		return "mpeg2video"
	case strings.Contains(encoderLower, "mpeg4"):
		return "mpeg4"
	case strings.Contains(encoderLower, "mjpeg"):
		return "mjpeg"
	}

	return ""
}

// GetAvailableSoftwareEncoders returns available software encoders for a given format.
func (f *EncoderFallback) GetAvailableSoftwareEncoders(format string) []string {
	if encoders, ok := f.formatEncoders[format]; ok {
		// Return a copy to avoid mutation
		result := make([]string, len(encoders))
		copy(result, encoders)
		return result
	}
	return nil
}

// GetAlternativeSoftwareEncoder returns the next software encoder to try when
// the given encoder (which was itself chosen as a system fallback) is unavailable.
// This enables multi-step fallback chains (e.g., libaom-av1 -> libsvtav1).
//
// Cross-format transitions are refused: when the chained encoder belongs to a
// different codec family than the current one, the lookup returns empty instead
// of silently downgrading the requested output format (TSI-2671).
// Returns empty string if no alternative is configured or the configured
// alternative would change the codec family.
func (f *EncoderFallback) GetAlternativeSoftwareEncoder(encoder string) string {
	next, ok := f.softwareFallbackChain[encoder]
	if !ok || next == encoder {
		return ""
	}
	if f.IsCrossFormatFallback(encoder, next) {
		log.Printf("Refusing cross-format fallback: %s (%s) -> %s (%s)",
			encoder, f.GetEncoderFormat(encoder), next, f.GetEncoderFormat(next))
		return ""
	}
	return next
}

// IsCrossFormatFallback reports whether 'to' belongs to a different codec
// family than 'from' (e.g. libsvtav1 (av1) -> libx264 (h264)). Unknown encoder
// formats are treated as compatible so a fallback is never blocked when the
// family cannot be determined.
func (f *EncoderFallback) IsCrossFormatFallback(from, to string) bool {
	fromFmt := f.GetEncoderFormat(from)
	toFmt := f.GetEncoderFormat(to)
	if fromFmt == "" || toFmt == "" {
		return false
	}
	return fromFmt != toFmt
}

// AddSoftwareFallbackChain adds a fallback chain entry: when 'from' is unavailable,
// try 'to' as the next alternative.
func (f *EncoderFallback) AddSoftwareFallbackChain(from, to string) {
	if f.softwareFallbackChain == nil {
		f.softwareFallbackChain = make(map[string]string)
	}
	f.softwareFallbackChain[from] = to
}

// AddEncoderMapping adds or updates a hardware-to-software encoder mapping.
func (f *EncoderFallback) AddEncoderMapping(hardware, software string) {
	if f.hardwareToSoftware == nil {
		f.hardwareToSoftware = make(map[string]string)
	}
	f.hardwareToSoftware[hardware] = software
}

// AddFormatEncoder adds a software encoder for a given format.
func (f *EncoderFallback) AddFormatEncoder(format, encoder string) {
	if f.formatEncoders == nil {
		f.formatEncoders = make(map[string][]string)
	}

	encoders := f.formatEncoders[format]
	for _, e := range encoders {
		if e == encoder {
			return // Already exists
		}
	}
	f.formatEncoders[format] = append(encoders, encoder)

	// Sort for consistent ordering
	sort.Strings(f.formatEncoders[format])
}

// ValidateEncoder checks if an encoder name is valid (known format).
func (f *EncoderFallback) ValidateEncoder(encoder string) bool {
	// Check if it's a known hardware encoder
	if _, ok := f.hardwareToSoftware[encoder]; ok {
		return true
	}

	// Check if it's a known software encoder
	for _, encoders := range f.formatEncoders {
		for _, e := range encoders {
			if e == encoder {
				return true
			}
		}
	}

	// Check if we can determine the format
	return f.GetEncoderFormat(encoder) != ""
}
