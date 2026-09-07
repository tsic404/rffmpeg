// Package rewrite provides the core data structures and interfaces for the encoder
// parameter rewrite engine. It handles intelligent rewriting of FFmpeg encoder parameters
// based on local hardware capabilities and encoder mapping tables.
package rewrite

import (
	"time"

	"github.com/tsic404/rffmpeg/pkg/encoder"
)

// ScenarioType represents one of the 6 rewrite scenarios.
type ScenarioType int

const (
	// ScenarioUnspecifiedEncoderWithHW indicates the user did not specify an encoder
	// and hardware acceleration is available. The engine should auto-upgrade to the
	// best available hardware encoder.
	ScenarioUnspecifiedEncoderWithHW ScenarioType = iota + 1

	// ScenarioUnspecifiedEncoderNoHW indicates the user did not specify an encoder
	// and no hardware acceleration is available. The engine should use libx264
	// as the software baseline.
	ScenarioUnspecifiedEncoderNoHW

	// ScenarioSpecifiedEncoderSupported indicates the user specified an encoder
	// that is supported locally. The engine should pass through unchanged
	// (unless --auto-hw flag is set).
	ScenarioSpecifiedEncoderSupported

	// ScenarioSpecifiedEncoderUnsupportedWithAlternative indicates the user specified
	// an encoder that is not supported, but a same-format hardware encoder is available.
	// The engine should translate to the local hardware encoder.
	ScenarioSpecifiedEncoderUnsupportedWithAlternative

	// ScenarioSpecifiedEncoderUnsupportedFallbackSoftware indicates the user specified
	// a hardware encoder that is not supported, and no same-format hardware alternative
	// exists. The engine should fallback to the software encoder for that format.
	ScenarioSpecifiedEncoderUnsupportedFallbackSoftware

	// ScenarioFormatNotAvailable indicates the user specified an encoder for a format
	// that is completely unavailable. The engine should return an error without
	// cross-format translation.
	ScenarioFormatNotAvailable

	// ScenarioEncoderUnsupported indicates the user specified an encoder name
	// that is completely unrecognized. This is different from format not available
	// - it means the encoder name itself is invalid (e.g., typo, non-existent encoder).
	// The engine should return an error without attempting fallback.
	ScenarioEncoderUnsupported
)

// String returns the string representation of the scenario type.
func (s ScenarioType) String() string {
	switch s {
	case ScenarioUnspecifiedEncoderWithHW:
		return "unspecified_encoder_with_hw"
	case ScenarioUnspecifiedEncoderNoHW:
		return "unspecified_encoder_no_hw"
	case ScenarioSpecifiedEncoderSupported:
		return "specified_encoder_supported"
	case ScenarioSpecifiedEncoderUnsupportedWithAlternative:
		return "specified_encoder_unsupported_with_alternative"
	case ScenarioSpecifiedEncoderUnsupportedFallbackSoftware:
		return "specified_encoder_unsupported_fallback_software"
	case ScenarioFormatNotAvailable:
		return "format_not_available"
	case ScenarioEncoderUnsupported:
		return "encoder_unsupported"
	default:
		return "unknown"
	}
}

// Description returns a human-readable description of the scenario.
func (s ScenarioType) Description() string {
	switch s {
	case ScenarioUnspecifiedEncoderWithHW:
		return "Auto-upgrade to best hardware encoder"
	case ScenarioUnspecifiedEncoderNoHW:
		return "Use software baseline (libx264)"
	case ScenarioSpecifiedEncoderSupported:
		return "Pass through unchanged"
	case ScenarioSpecifiedEncoderUnsupportedWithAlternative:
		return "Translate to local hardware encoder"
	case ScenarioSpecifiedEncoderUnsupportedFallbackSoftware:
		return "Fallback to software encoder"
	case ScenarioFormatNotAvailable:
		return "Format not available (error)"
	case ScenarioEncoderUnsupported:
		return "Encoder not recognized (error)"
	default:
		return "Unknown scenario"
	}
}

// HardwareCapabilities represents the hardware acceleration capabilities of a worker.
type HardwareCapabilities struct {
	// AvailableEncoders is the list of encoders available on this worker.
	AvailableEncoders []encoder.EncoderFamily `json:"available_encoders"`

	// HardwareEncoders is the list of hardware-accelerated encoders available.
	HardwareEncoders []encoder.EncoderFamily `json:"hardware_encoders"`

	// SoftwareEncoders is the list of software encoders available.
	SoftwareEncoders []encoder.EncoderFamily `json:"software_encoders"`

	// SupportedCodecs is the list of codec formats supported by available encoders.
	SupportedCodecs []encoder.CodecFormat `json:"supported_codecs"`

	// GPUDevices contains information about detected GPU devices.
	GPUDevices []GPUDevice `json:"gpu_devices,omitempty"`

	// EncoderPriority defines the preferred encoder selection order.
	// Higher priority encoders are preferred when multiple options exist.
	EncoderPriority []EncoderPriorityEntry `json:"encoder_priority,omitempty"`

	// EncoderBlacklist contains encoders that should never be used.
	EncoderBlacklist []encoder.EncoderFamily `json:"encoder_blacklist,omitempty"`
}

// GPUDevice represents a detected GPU device.
type GPUDevice struct {
	// Type is the GPU acceleration type (nvenc, qsv, vaapi, amf, videotoolbox).
	Type string `json:"type"`

	// Path is the device path (e.g., /dev/dri/renderD128).
	Path string `json:"path,omitempty"`

	// Name is the device name/model.
	Name string `json:"name,omitempty"`

	// Vendor is the GPU vendor (NVIDIA, AMD, Intel, Apple).
	Vendor string `json:"vendor,omitempty"`

	// DriverVersion is the driver version.
	DriverVersion string `json:"driver_version,omitempty"`

	// Accessible indicates whether the device is accessible.
	Accessible bool `json:"accessible"`

	// QSVHealthy indicates whether the QSV MFX runtime is functional on Intel devices.
	QSVHealthy bool `json:"qsv_healthy,omitempty"`
}

// EncoderPriorityEntry represents an encoder with its priority value.
type EncoderPriorityEntry struct {
	// Encoder is the encoder family.
	Encoder encoder.EncoderFamily `json:"encoder"`

	// Priority is the priority value (higher = more preferred).
	Priority int `json:"priority"`
}

// HasEncoder checks if a specific encoder is available.
func (hc *HardwareCapabilities) HasEncoder(enc encoder.EncoderFamily) bool {
	for _, e := range hc.AvailableEncoders {
		if e == enc {
			return true
		}
	}
	return false
}

// HasHardwareEncoder checks if any hardware encoder is available.
func (hc *HardwareCapabilities) HasHardwareEncoder() bool {
	return len(hc.HardwareEncoders) > 0
}

// HasCodecSupport checks if a codec format is supported.
func (hc *HardwareCapabilities) HasCodecSupport(codec encoder.CodecFormat) bool {
	for _, c := range hc.SupportedCodecs {
		if c == codec {
			return true
		}
	}
	return false
}

// GetBestHardwareEncoder returns the best available hardware encoder for a codec.
func (hc *HardwareCapabilities) GetBestHardwareEncoder(codec encoder.CodecFormat) encoder.EncoderFamily {
	for _, entry := range hc.EncoderPriority {
		if entry.Encoder.CodecFormat() == codec && entry.Encoder.IsHardware() && hc.HasEncoder(entry.Encoder) {
			return entry.Encoder
		}
	}
	// Fallback: find any hardware encoder for the codec
	for _, enc := range hc.HardwareEncoders {
		if enc.CodecFormat() == codec {
			return enc
		}
	}
	return ""
}

// GetSoftwareEncoder returns the software encoder for a codec.
func (hc *HardwareCapabilities) GetSoftwareEncoder(codec encoder.CodecFormat) encoder.EncoderFamily {
	for _, enc := range hc.SoftwareEncoders {
		if enc.CodecFormat() == codec {
			return enc
		}
	}
	return ""
}

// IsBlacklisted checks if an encoder is blacklisted.
func (hc *HardwareCapabilities) IsBlacklisted(enc encoder.EncoderFamily) bool {
	for _, b := range hc.EncoderBlacklist {
		if b == enc {
			return true
		}
	}
	return false
}

// EncoderRewriteRequest represents a request to rewrite encoder parameters.
type EncoderRewriteRequest struct {
	// OriginalArgs contains the original FFmpeg command-line arguments.
	OriginalArgs []string `json:"original_args"`

	// SpecifiedEncoder is the encoder explicitly specified by the user.
	// Empty if no encoder was specified.
	SpecifiedEncoder encoder.EncoderFamily `json:"specified_encoder,omitempty"`

	// TargetCodec is the target codec format (if known).
	TargetCodec encoder.CodecFormat `json:"target_codec,omitempty"`

	// HardwareCapabilities describes the worker's hardware capabilities.
	HardwareCapabilities HardwareCapabilities `json:"hardware_capabilities"`

	// EncoderParams contains the parsed encoder-specific parameters.
	// Key: parameter name, Value: parameter value.
	EncoderParams map[string]string `json:"encoder_params,omitempty"`

	// AutoHW indicates whether auto hardware upgrade is enabled.
	// When true, even supported encoders may be upgraded to better hardware.
	AutoHW bool `json:"auto_hw"`

	// RequestID is a unique identifier for tracking this request.
	RequestID string `json:"request_id,omitempty"`

	// Timestamp is when the request was created.
	Timestamp time.Time `json:"timestamp"`
}

// EncoderRewriteResponse represents the result of an encoder rewrite operation.
type EncoderRewriteResponse struct {
	// RewrittenArgs contains the rewritten FFmpeg command-line arguments.
	RewrittenArgs []string `json:"rewritten_args"`

	// OriginalEncoder is the encoder that was originally specified or detected.
	OriginalEncoder encoder.EncoderFamily `json:"original_encoder"`

	// TargetEncoder is the encoder that was selected after rewriting.
	TargetEncoder encoder.EncoderFamily `json:"target_encoder"`

	// Scenario is the rewrite scenario that was applied.
	Scenario ScenarioType `json:"scenario"`

	// TranslationPerformed indicates whether any translation was needed.
	TranslationPerformed bool `json:"translation_performed"`

	// AuditRecords contains detailed records of all rewrite operations.
	AuditRecords []AuditRecord `json:"audit_records"`

	// Notifications contains messages to be output to stderr.
	Notifications []Notification `json:"notifications"`

	// Errors contains any errors that occurred during rewriting.
	Errors []RewriteError `json:"errors,omitempty"`

	// Warnings contains non-fatal warnings generated during rewriting.
	Warnings []string `json:"warnings,omitempty"`

	// RequestID matches the request ID for correlation.
	RequestID string `json:"request_id,omitempty"`

	// Timestamp is when the response was generated.
	Timestamp time.Time `json:"timestamp"`
}

// AuditRecord represents a single audit entry for a rewrite operation.
type AuditRecord struct {
	// ID is a unique identifier for this audit record.
	ID string `json:"id"`

	// Timestamp is when the operation was performed.
	Timestamp time.Time `json:"timestamp"`

	// Operation is the type of operation performed.
	Operation AuditOperation `json:"operation"`

	// SourceEncoder is the encoder before the operation.
	SourceEncoder encoder.EncoderFamily `json:"source_encoder,omitempty"`

	// TargetEncoder is the encoder after the operation.
	TargetEncoder encoder.EncoderFamily `json:"target_encoder,omitempty"`

	// SourceParam is the original parameter name.
	SourceParam string `json:"source_param,omitempty"`

	// TargetParam is the translated parameter name.
	TargetParam string `json:"target_param,omitempty"`

	// SourceValue is the original parameter value.
	SourceValue string `json:"source_value,omitempty"`

	// TargetValue is the translated parameter value.
	TargetValue string `json:"target_value,omitempty"`

	// Reason explains why this operation was performed.
	Reason string `json:"reason"`

	// Success indicates whether the operation succeeded.
	Success bool `json:"success"`

	// ErrorMessage contains the error message if the operation failed.
	ErrorMessage string `json:"error_message,omitempty"`
}

// AuditOperation represents the type of audit operation.
type AuditOperation string

const (
	// AuditOpScenarioClassify indicates scenario classification.
	AuditOpScenarioClassify AuditOperation = "scenario_classify"

	// AuditOpEncoderSelect indicates encoder selection.
	AuditOpEncoderSelect AuditOperation = "encoder_select"

	// AuditOpParamTranslate indicates parameter translation.
	AuditOpParamTranslate AuditOperation = "param_translate"

	// AuditOpHWParamInject indicates hardware parameter injection.
	AuditOpHWParamInject AuditOperation = "hw_param_inject"

	// AuditOpFallback indicates fallback to alternative encoder.
	AuditOpFallback AuditOperation = "fallback"
)

// Notification represents a message to be output to stderr.
type Notification struct {
	// Timestamp is when the notification was generated.
	Timestamp time.Time `json:"timestamp"`

	// Level is the notification level (info, warning, error).
	Level NotificationLevel `json:"level"`

	// Message is the notification message.
	Message string `json:"message"`

	// Details contains additional structured details.
	Details map[string]string `json:"details,omitempty"`
}

// NotificationLevel represents the severity of a notification.
type NotificationLevel string

const (
	// NotificationLevelInfo indicates an informational message.
	NotificationLevelInfo NotificationLevel = "info"

	// NotificationLevelWarning indicates a warning message.
	NotificationLevelWarning NotificationLevel = "warning"

	// NotificationLevelError indicates an error message.
	NotificationLevelError NotificationLevel = "error"
)

// RewriteError represents an error that occurred during rewriting.
type RewriteError struct {
	// Code is the error code.
	Code ErrorCode `json:"code"`

	// Message is the error message.
	Message string `json:"message"`

	// Detail contains additional error details.
	Detail string `json:"detail,omitempty"`

	// Encoder is the encoder related to the error.
	Encoder encoder.EncoderFamily `json:"encoder,omitempty"`

	// Codec is the codec format related to the error.
	Codec encoder.CodecFormat `json:"codec,omitempty"`
}
