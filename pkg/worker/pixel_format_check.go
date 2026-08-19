package worker

import (
	"context"
	"fmt"
	"log"
	"strings"
)

// PixelFormatChecker checks input video pixel format compatibility
// with hardware encoders and recommends fallback when needed.
type PixelFormatChecker struct {
	probeExecutor *FFprobeExecutor
}

// NewPixelFormatChecker creates a new pixel format checker.
func NewPixelFormatChecker(probeExecutor *FFprobeExecutor) *PixelFormatChecker {
	return &PixelFormatChecker{
		probeExecutor: probeExecutor,
	}
}

// PixelFormatCheckResult contains the result of a pixel format check.
type PixelFormatCheckResult struct {
	// PixelFormat is the detected pixel format (e.g., "yuv420p", "yuv444p").
	PixelFormat string `json:"pixel_format"`

	// NeedsFallback is true if the pixel format is incompatible with
	// the specified hardware encoder.
	NeedsFallback bool `json:"needs_fallback"`

	// Reason explains why fallback is needed.
	Reason string `json:"reason,omitempty"`

	// RecommendedEncoder is the recommended software encoder to use.
	RecommendedEncoder string `json:"recommended_encoder,omitempty"`
}

// CheckPixelFormat probes the input file and checks if the pixel format
// is compatible with the specified encoder.
// Returns nil if the check cannot be performed (e.g., probe fails).
func (c *PixelFormatChecker) CheckPixelFormat(ctx context.Context, inputPath string, encoder string) *PixelFormatCheckResult {
	// Probe the input file
	result, err := c.probeExecutor.Probe(ctx, inputPath)
	if err != nil {
		log.Printf("PixelFormatChecker: ffprobe failed for %s: %v", inputPath, err)
		return nil
	}

	// Extract pixel format from video stream
	pixelFormat := c.extractPixelFormat(result)
	if pixelFormat == "" {
		log.Printf("PixelFormatChecker: could not determine pixel format for %s", inputPath)
		return nil
	}

	checkResult := &PixelFormatCheckResult{
		PixelFormat: pixelFormat,
	}

	// Check if this is a VAAPI encoder
	if !isVAAPIEncoder(encoder) {
		return checkResult
	}

	// VAAPI encoders have limited support for yuv444p and other high chroma formats
	if c.isUnsupportedPixelFormatForVAAPI(pixelFormat) {
		checkResult.NeedsFallback = true
		checkResult.Reason = fmt.Sprintf("VAAPI encoder %s does not support pixel format %s", encoder, pixelFormat)
		checkResult.RecommendedEncoder = c.getSoftwareFallbackEncoder(encoder)
		log.Printf("PixelFormatChecker: %s (input: %s)", checkResult.Reason, inputPath)
	}

	return checkResult
}

// extractPixelFormat extracts the pixel format from the first video stream.
func (c *PixelFormatChecker) extractPixelFormat(result *FFprobeResult) string {
	if result == nil {
		return ""
	}

	for _, stream := range result.Streams {
		// Check if this is a video stream
		if codecType, ok := stream["codec_type"].(string); ok && codecType == "video" {
			if pixFmt, ok := stream["pix_fmt"].(string); ok {
				return pixFmt
			}
		}
	}

	return ""
}

// isUnsupportedPixelFormatForVAAPI returns true if the pixel format
// is not well supported by VAAPI encoders.
//
// VAAPI encoders typically only support yuv420p and nv12 pixel formats.
// yuv444p (4:4:4 chroma subsampling) and other formats may fail or
// produce incorrect output.
func (c *PixelFormatChecker) isUnsupportedPixelFormatForVAAPI(pixelFormat string) bool {
	// List of pixel formats that VAAPI encoders typically don't support
	unsupportedFormats := map[string]bool{
		"yuv444p":   true, // 4:4:4 chroma - not supported by most VAAPI implementations
		"yuv444p10": true, // 4:4:4 with 10-bit depth
		"yuv444p12": true, // 4:4:4 with 12-bit depth
		"yuv444p16": true, // 4:4:4 with 16-bit depth
		"yuv422p":   true, // 4:2:2 chroma - limited VAAPI support
		"yuv422p10": true,
		"yuv422p12": true,
		"yuv422p16": true,
		"yuv440p":   true,
		"yuv440p10": true,
		"yuv440p12": true,
		"rgb24":     true, // RGB formats not supported
		"rgba":      true,
		"bgr24":     true,
		"bgra":      true,
		"gbrp":      true, // Planar RGB
		"gbrp10":    true,
		"gbrp12":    true,
		"gbrp16":    true,
		"gray":      true, // Grayscale
		"gray10":    true,
		"gray12":    true,
		"gray16":    true,
		"xyz12":     true, // XYZ color space
		"xyz12le":   true,
		"xyz12be":   true,
	}

	return unsupportedFormats[pixelFormat]
}

// getSoftwareFallbackEncoder returns the software encoder equivalent
// for a VAAPI encoder.
func (c *PixelFormatChecker) getSoftwareFallbackEncoder(vaapiEncoder string) string {
	fallback := NewEncoderFallback()
	return fallback.GetSoftwareEncoder(vaapiEncoder)
}

// isVAAPIEncoder returns true if the encoder is a VAAPI hardware encoder.
func isVAAPIEncoder(encoder string) bool {
	return strings.HasSuffix(strings.ToLower(encoder), "_vaapi")
}

// CheckInputsForPixelFormatIncompatibility probes multiple input files
// and returns the first incompatibility found.
// Returns nil if no incompatibility is detected.
func (c *PixelFormatChecker) CheckInputsForPixelFormatIncompatibility(ctx context.Context, inputPaths []string, encoder string) *PixelFormatCheckResult {
	for _, inputPath := range inputPaths {
		result := c.CheckPixelFormat(ctx, inputPath, encoder)
		if result != nil && result.NeedsFallback {
			return result
		}
	}
	return nil
}
