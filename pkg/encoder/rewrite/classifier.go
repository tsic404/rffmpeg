package rewrite

import (
	"context"
	"fmt"

	"github.com/tsic404/rffmpeg/pkg/encoder"
)

// ScenarioClassifierImpl implements the ScenarioClassifier interface.
// It analyzes encoder rewrite requests and determines which of the 6 scenarios applies
// based on the specified encoder, hardware capabilities, and available encoders.
type ScenarioClassifierImpl struct {
	// mapping is the encoder mapping for checking encoder support
	mapping EncoderMappingChecker
}

// EncoderMappingChecker defines the interface for checking encoder mapping support.
type EncoderMappingChecker interface {
	// GetCodecFormat returns the codec format for an encoder.
	GetCodecFormat(enc encoder.EncoderFamily) (encoder.CodecFormat, bool)

	// CanTranslate checks if translation is possible between two encoders.
	CanTranslate(source, target encoder.EncoderFamily) bool
}

// NewScenarioClassifier creates a new scenario classifier.
func NewScenarioClassifier() *ScenarioClassifierImpl {
	return &ScenarioClassifierImpl{}
}

// NewScenarioClassifierWithMapping creates a new scenario classifier with encoder mapping.
func NewScenarioClassifierWithMapping(mapping EncoderMappingChecker) *ScenarioClassifierImpl {
	return &ScenarioClassifierImpl{mapping: mapping}
}

// Classify analyzes the request and determines the applicable scenario.
func (c *ScenarioClassifierImpl) Classify(ctx context.Context, req *EncoderRewriteRequest) (ScenarioType, error) {
	if req == nil {
		return 0, fmt.Errorf("request is nil")
	}

	// Check for context cancellation
	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	default:
	}

	hwCaps := &req.HardwareCapabilities

	// Case 1: No encoder specified
	if req.SpecifiedEncoder == "" {
		// If hardware encoders are available, upgrade to best hardware encoder
		if hwCaps.HasHardwareEncoder() {
			return ScenarioUnspecifiedEncoderWithHW, nil
		}
		// Otherwise, use software baseline (libx264)
		return ScenarioUnspecifiedEncoderNoHW, nil
	}

	// Case 2: Encoder was specified
	specifiedEnc := req.SpecifiedEncoder

	// Check if the specified encoder is blacklisted
	if hwCaps.IsBlacklisted(specifiedEnc) {
		// Try to find an alternative
		codec := specifiedEnc.CodecFormat()
		if codec == "" {
			// Unknown encoder, can't find alternative
			return ScenarioEncoderUnsupported, nil
		}

		// Check if there's a hardware alternative
		if hwEnc := hwCaps.GetBestHardwareEncoder(codec); hwEnc != "" {
			return ScenarioSpecifiedEncoderUnsupportedWithAlternative, nil
		}

		// Check if there's a software alternative
		if swEnc := hwCaps.GetSoftwareEncoder(codec); swEnc != "" {
			return ScenarioSpecifiedEncoderUnsupportedFallbackSoftware, nil
		}

		return ScenarioFormatNotAvailable, nil
	}

	// Check if the specified encoder is available
	if hwCaps.HasEncoder(specifiedEnc) {
		// Encoder is supported
		if req.AutoHW && specifiedEnc.IsHardware() {
			// Auto-hw is enabled and encoder is already hardware - could try to upgrade
			// But for now, pass through unchanged
			return ScenarioSpecifiedEncoderSupported, nil
		}
		// Check if auto-hw upgrade is requested
		if req.AutoHW && !specifiedEnc.IsHardware() {
			// Auto-hw upgrade requested for software encoder
			codec := specifiedEnc.CodecFormat()
			if hwEnc := hwCaps.GetBestHardwareEncoder(codec); hwEnc != "" {
				// Hardware encoder available for upgrade
				return ScenarioSpecifiedEncoderUnsupportedWithAlternative, nil
			}
		}
		return ScenarioSpecifiedEncoderSupported, nil
	}

	// Encoder is not available - find the codec format
	codec := specifiedEnc.CodecFormat()
	if codec == "" {
		// Unknown encoder format - this is a user error (typo, non-existent encoder)
		// Return the new ENCODER_UNSUPPORTED scenario instead of FORMAT_NOT_AVAILABLE
		return ScenarioEncoderUnsupported, nil
	}

	// Check if the codec format is supported at all
	if !hwCaps.HasCodecSupport(codec) {
		return ScenarioFormatNotAvailable, nil
	}

	// Check for hardware alternative
	hwEnc := hwCaps.GetBestHardwareEncoder(codec)
	if hwEnc != "" {
		return ScenarioSpecifiedEncoderUnsupportedWithAlternative, nil
	}

	// Check for software fallback
	swEnc := hwCaps.GetSoftwareEncoder(codec)
	if swEnc != "" {
		return ScenarioSpecifiedEncoderUnsupportedFallbackSoftware, nil
	}

	// No suitable encoder found
	return ScenarioFormatNotAvailable, nil
}

// GetScenarioInfo returns detailed information about a scenario.
func (c *ScenarioClassifierImpl) GetScenarioInfo(scenario ScenarioType) ScenarioInfo {
	switch scenario {
	case ScenarioUnspecifiedEncoderWithHW:
		return ScenarioInfo{
			Type:                scenario,
			Description:         "Auto-upgrade to best hardware encoder",
			RequiresTranslation: true,
			RequiresHWInjection: true,
			IsError:             false,
			RecommendedAction:   "Select the best available hardware encoder and translate parameters",
		}
	case ScenarioUnspecifiedEncoderNoHW:
		return ScenarioInfo{
			Type:                scenario,
			Description:         "Use software baseline (libx264)",
			RequiresTranslation: false,
			RequiresHWInjection: false,
			IsError:             false,
			RecommendedAction:   "Use libx264 as the default software encoder",
		}
	case ScenarioSpecifiedEncoderSupported:
		return ScenarioInfo{
			Type:                scenario,
			Description:         "Pass through unchanged",
			RequiresTranslation: false,
			RequiresHWInjection: false,
			IsError:             false,
			RecommendedAction:   "Encoder is supported locally, pass parameters through unchanged",
		}
	case ScenarioSpecifiedEncoderUnsupportedWithAlternative:
		return ScenarioInfo{
			Type:                scenario,
			Description:         "Translate to local hardware encoder",
			RequiresTranslation: true,
			RequiresHWInjection: true,
			IsError:             false,
			RecommendedAction:   "Translate parameters to the local hardware encoder alternative",
		}
	case ScenarioSpecifiedEncoderUnsupportedFallbackSoftware:
		return ScenarioInfo{
			Type:                scenario,
			Description:         "Fallback to software encoder",
			RequiresTranslation: true,
			RequiresHWInjection: false,
			IsError:             false,
			RecommendedAction:   "Fallback to software encoder for the same codec format",
		}
	case ScenarioEncoderUnsupported:
		return ScenarioInfo{
			Type:                scenario,
			Description:         "Encoder not recognized (error)",
			RequiresTranslation: false,
			RequiresHWInjection: false,
			IsError:             true,
			RecommendedAction:   "Return error - the specified encoder name is not recognized",
		}
	case ScenarioFormatNotAvailable:
		return ScenarioInfo{
			Type:                scenario,
			Description:         "Format not available (error)",
			RequiresTranslation: false,
			RequiresHWInjection: false,
			IsError:             true,
			RecommendedAction:   "Return error - the requested codec format is not available",
		}
	default:
		return ScenarioInfo{
			Type:                scenario,
			Description:         "Unknown scenario",
			RequiresTranslation: false,
			RequiresHWInjection: false,
			IsError:             true,
			RecommendedAction:   "Unknown scenario type",
		}
	}
}

// SelectTargetEncoder selects the target encoder for a given scenario.
func (c *ScenarioClassifierImpl) SelectTargetEncoder(scenario ScenarioType, req *EncoderRewriteRequest) encoder.EncoderFamily {
	hwCaps := &req.HardwareCapabilities

	switch scenario {
	case ScenarioUnspecifiedEncoderWithHW:
		// Select best hardware encoder for default codec (H.264)
		if enc := hwCaps.GetBestHardwareEncoder(encoder.CodecH264); enc != "" {
			return enc
		}
		// Fallback to software
		return encoder.EncoderLibX264

	case ScenarioUnspecifiedEncoderNoHW:
		// Use software baseline
		return encoder.EncoderLibX264

	case ScenarioSpecifiedEncoderSupported:
		// Use the specified encoder
		return req.SpecifiedEncoder

	case ScenarioSpecifiedEncoderUnsupportedWithAlternative:
		// Find hardware alternative
		codec := req.SpecifiedEncoder.CodecFormat()
		if enc := hwCaps.GetBestHardwareEncoder(codec); enc != "" {
			return enc
		}
		return ""

	case ScenarioSpecifiedEncoderUnsupportedFallbackSoftware:
		// Find software fallback
		codec := req.SpecifiedEncoder.CodecFormat()
		if enc := hwCaps.GetSoftwareEncoder(codec); enc != "" {
			return enc
		}
		return ""

	case ScenarioEncoderUnsupported, ScenarioFormatNotAvailable:
		// No encoder available
		return ""

	default:
		return ""
	}
}
