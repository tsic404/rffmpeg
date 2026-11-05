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

	// With no explicit encoder the output container decides the target family
	// (C-72): ffmpeg's default video codec for the effective output format,
	// provided the worker can run an encoder of that family. Otherwise the
	// target stays empty and the command is left to ffmpeg.
	if specifiedEncoder == "" {
		if _, family, ok := containerTargetCodec(args, hwCaps); ok {
			targetCodec = family
		}
	}

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
