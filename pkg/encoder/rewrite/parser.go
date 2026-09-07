// Package rewrite provides the core data structures and interfaces for the encoder
// parameter rewrite engine. It handles intelligent rewriting of FFmpeg encoder parameters
// based on local hardware capabilities and encoder mapping tables.
package rewrite

import (
	"fmt"
	"strings"

	"github.com/tsic404/rffmpeg/pkg/encoder"
)

// FFmpegParser parses FFmpeg command-line arguments to extract encoder information.
type FFmpegParser struct{}

// NewFFmpegParser creates a new FFmpegParser.
func NewFFmpegParser() *FFmpegParser {
	return &FFmpegParser{}
}

// ParseArgs parses FFmpeg command-line arguments and extracts encoder information.
// It returns the detected encoder, target codec format, and encoder-specific parameters.
func (p *FFmpegParser) ParseArgs(args []string) (encoder.EncoderFamily, encoder.CodecFormat, map[string]string, error) {
	if len(args) == 0 {
		return "", "", nil, fmt.Errorf("no arguments provided")
	}

	var detectedEncoder encoder.EncoderFamily
	var targetCodec encoder.CodecFormat
	encoderParams := make(map[string]string)

	// Simple parsing: look for -c:v, -codec:v, -vcodec
	for i := 0; i < len(args); i++ {
		arg := args[i]

		switch arg {
		case "-c:v", "-codec:v", "-vcodec":
			if i+1 < len(args) {
				encoderName := args[i+1]
				enc := encoder.EncoderFamily(encoderName)
				// Validate it's a known encoder
				if enc.CodecFormat() != "" {
					detectedEncoder = enc
					targetCodec = enc.CodecFormat()
				}
				i++ // Skip next arg since we consumed it
			}
		default:
			// Try to detect encoder from other patterns
			if enc := p.detectEncoderFromArg(arg); enc != "" {
				detectedEncoder = enc
				targetCodec = enc.CodecFormat()
			}
		}
	}

	// Extract encoder parameters (simplified - in real implementation would need more sophisticated parsing)
	// This would typically involve parsing parameter pairs like -b:v, -crf, -preset, etc.
	// For now, we'll return empty params

	return detectedEncoder, targetCodec, encoderParams, nil
}

// detectEncoderFromArg tries to detect an encoder from a command-line argument.
func (p *FFmpegParser) detectEncoderFromArg(arg string) encoder.EncoderFamily {
	// Check if argument contains an encoder name
	for _, enc := range allEncoders() {
		if strings.Contains(arg, string(enc)) {
			return enc
		}
	}
	return ""
}

// allEncoders returns a list of all known encoder families.
func allEncoders() []encoder.EncoderFamily {
	return []encoder.EncoderFamily{
		// H.264 encoders
		encoder.EncoderLibX264,
		encoder.EncoderH264NVENC,
		encoder.EncoderH264QSV,
		encoder.EncoderH264VAAPI,
		encoder.EncoderH264AMF,
		encoder.EncoderH264VT,

		// HEVC encoders
		encoder.EncoderLibX265,
		encoder.EncoderHEVCNVENC,
		encoder.EncoderHEVCQSV,
		encoder.EncoderHEVCVAAPI,
		encoder.EncoderHEVCAMF,
		encoder.EncoderHEVCVT,

		// VP9 encoders
		encoder.EncoderLibVPX,
		encoder.EncoderVP9NVENC,
		encoder.EncoderVP9QSV,
		encoder.EncoderVP9VAAPI,

		// AV1 encoders
		encoder.EncoderLibSVTAV1,
		encoder.EncoderLibAOM,
		encoder.EncoderAV1NVENC,
		encoder.EncoderAV1QSV,
		encoder.EncoderAV1VAAPI,
	}
}

// CreateEncoderRewriteRequest creates an EncoderRewriteRequest from FFmpeg arguments
// and hardware capabilities.
func (p *FFmpegParser) CreateEncoderRewriteRequest(args []string, hwCaps *HardwareCapabilities, autoHW bool, requestID string) (*EncoderRewriteRequest, error) {
	specifiedEncoder, targetCodec, encoderParams, err := p.ParseArgs(args)
	if err != nil {
		return nil, fmt.Errorf("failed to parse FFmpeg arguments: %w", err)
	}

	// If no encoder specified but we have a target codec, we still need to handle it
	// In practice, FFmpeg might infer codec from output format

	req := &EncoderRewriteRequest{
		OriginalArgs:         args,
		SpecifiedEncoder:     specifiedEncoder,
		TargetCodec:          targetCodec,
		HardwareCapabilities: *hwCaps,
		EncoderParams:        encoderParams,
		AutoHW:               autoHW,
		RequestID:            requestID,
	}

	return req, nil
}
