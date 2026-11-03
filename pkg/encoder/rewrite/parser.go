// Package rewrite provides the core data structures and interfaces for the encoder
// parameter rewrite engine. It handles intelligent rewriting of FFmpeg encoder parameters
// based on local hardware capabilities and encoder mapping tables.
package rewrite

import (
	"fmt"

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

	// Only flags select an encoder: the general -c / -codec counts when its
	// value is a known video encoder, explicit video flags always do.
	flag, ok := PrimaryVideoEncoderFlag(ParseVideoEncoderFlags(args, nil))
	if ok {
		enc := encoder.EncoderFamily(flag.Value)
		// Validate it's a known encoder
		if enc.CodecFormat() != "" {
			detectedEncoder = enc
			targetCodec = enc.CodecFormat()
		}
	}

	// Extract encoder parameters (simplified - in real implementation would need more sophisticated parsing)
	// This would typically involve parsing parameter pairs like -b:v, -crf, -preset, etc.
	// For now, we'll return empty params

	return detectedEncoder, targetCodec, encoderParams, nil
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
