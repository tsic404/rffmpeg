// Package audit provides audit tracking and real-time notification for encoder rewrite operations.
// It implements the "rewrite is not silent" principle by recording all rewrite operations
// and outputting human-readable notifications to stderr.
package audit

import (
	"time"
)

// ScenarioType represents the type of encoder rewrite scenario.
// There are 6 main scenarios as defined in the encoder rewrite engine.
type ScenarioType string

const (
	// ScenarioEncoderUpgrade indicates an upgrade from software to hardware encoder.
	// Example: libx264 -> h264_nvenc
	ScenarioEncoderUpgrade ScenarioType = "encoder_upgrade"

	// ScenarioEncoderFallback indicates a fallback to software encoder.
	// Example: h264_nvenc -> libx264 (when hardware encoder unavailable)
	ScenarioEncoderFallback ScenarioType = "encoder_fallback"

	// ScenarioEncoderSubstitution indicates substitution between hardware encoders.
	// Example: h264_nvenc -> h264_qsv (when NVENC unavailable but QSV available)
	ScenarioEncoderSubstitution ScenarioType = "encoder_substitution"

	// ScenarioParameterTranslation indicates parameter translation between encoders.
	// Example: crf=23 -> cq=23 for NVENC
	ScenarioParameterTranslation ScenarioType = "parameter_translation"

	// ScenarioHardwareParamInjection indicates injection of hardware-specific parameters.
	// Example: Adding -gpu_id for QSV encoders
	ScenarioHardwareParamInjection ScenarioType = "hardware_param_injection"

	// ScenarioNoEncoderSpecified indicates no encoder was specified, auto-selecting.
	// Example: No -c:v specified -> auto-select h264_nvenc
	ScenarioNoEncoderSpecified ScenarioType = "no_encoder_specified"

	// ScenarioFormatNotAvailable indicates the requested format has no
	// available encoder at all — the rewrite engine could not satisfy the
	// request (error-level, TSI-2365).
	ScenarioFormatNotAvailable ScenarioType = "format_not_available"

	// ScenarioEncoderUnsupported indicates the encoder name is unrecognized —
	// error-level (TSI-2365).
	ScenarioEncoderUnsupported ScenarioType = "encoder_unsupported"
)

// String returns the string representation of the scenario type.
func (s ScenarioType) String() string {
	return string(s)
}

// NotifyLevel represents the notification severity level.
type NotifyLevel string

const (
	// InfoLevel indicates informational notifications.
	// Used for successful rewrites, upgrades, etc.
	InfoLevel NotifyLevel = "INFO"

	// WarnLevel indicates warning notifications.
	// Used for fallbacks, substitutions, potential issues.
	WarnLevel NotifyLevel = "WARN"

	// ErrorLevel indicates error notifications.
	// Used for failed rewrites, critical issues.
	ErrorLevel NotifyLevel = "ERROR"
)

// String returns the string representation of the notify level.
func (l NotifyLevel) String() string {
	return string(l)
}

// AuditOperation represents a single audit record for a rewrite operation.
// It captures all relevant information about what was changed and why,
// including the worker capabilities chain.
type AuditOperation struct {
	// RequestID is a unique identifier for the ffmpeg request.
	// Multiple operations within the same request share this ID.
	RequestID string `json:"request_id"`

	// Timestamp is when the operation was recorded.
	Timestamp time.Time `json:"timestamp"`

	// ScenarioType indicates the type of rewrite scenario.
	ScenarioType ScenarioType `json:"scenario_type"`

	// OriginalEncoder is the original encoder name (may be empty for no_encoder_specified).
	OriginalEncoder string `json:"original_encoder,omitempty"`

	// RewrittenEncoder is the final encoder name after rewrite.
	RewrittenEncoder string `json:"rewritten_encoder,omitempty"`

	// OriginalParams contains the original parameter key-value pairs.
	OriginalParams map[string]interface{} `json:"original_params,omitempty"`

	// RewrittenParams contains the rewritten parameter key-value pairs.
	RewrittenParams map[string]interface{} `json:"rewritten_params,omitempty"`

	// DecisionReason explains why this rewrite was applied.
	DecisionReason string `json:"decision_reason"`

	// CapabilitiesSummary provides a concise summary of the worker's hardware capabilities
	// at the time of this operation (e.g., "h264_nvenc,h264_qsv,libx264").
	CapabilitiesSummary string `json:"capabilities_summary,omitempty"`

	// Context contains additional contextual information.
	// Examples: GPU vendor, available encoders, hardware capabilities.
	Context map[string]interface{} `json:"context,omitempty"`
}

// AuditSummary provides a summary of audit operations for a request.
type AuditSummary struct {
	// RequestID is the unique identifier for the request.
	RequestID string `json:"request_id"`

	// StartTime is when the first operation was recorded.
	StartTime time.Time `json:"start_time"`

	// EndTime is when the last operation was recorded.
	EndTime time.Time `json:"end_time"`

	// TotalOperations is the count of operations recorded.
	TotalOperations int `json:"total_operations"`

	// Scenarios is a breakdown of operations by scenario type.
	Scenarios map[ScenarioType]int `json:"scenarios"`

	// HasWarnings indicates if any WARN level notifications were generated.
	HasWarnings bool `json:"has_warnings"`

	// HasErrors indicates if any ERROR level notifications were generated.
	HasErrors bool `json:"has_errors"`
}
