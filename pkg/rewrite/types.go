// Package rewrite provides encoder parameter rewrite functionality for FFmpeg.
// It includes hardware parameter injection, device selection, and audit tracking.
package rewrite

import (
	"time"
)

// ScenarioType represents the type of encoder rewrite scenario.
type ScenarioType string

const (
	// ScenarioUnspecifiedEncoderHWAvailable: User didn't specify encoder, HW acceleration available
	ScenarioUnspecifiedEncoderHWAvailable ScenarioType = "unspecified_hw_available"
	// ScenarioUnspecifiedEncoderHWUnavailable: User didn't specify encoder, no HW acceleration
	ScenarioUnspecifiedEncoderHWUnavailable ScenarioType = "unspecified_hw_unavailable"
	// ScenarioSpecifiedEncoderSupported: User specified encoder that is locally supported
	ScenarioSpecifiedEncoderSupported ScenarioType = "specified_supported"
	// ScenarioSpecifiedEncoderHWFallback: User specified encoder not supported, but same-format HW exists
	ScenarioSpecifiedEncoderHWFallback ScenarioType = "specified_hw_fallback"
	// ScenarioSpecifiedEncoderSWFallback: User specified encoder not supported, fallback to SW
	ScenarioSpecifiedEncoderSWFallback ScenarioType = "specified_sw_fallback"
	// SpecifiedEncoderFormatUnavailable: User specified encoder format completely unavailable
	SpecifiedEncoderFormatUnavailable ScenarioType = "specified_format_unavailable"
)

// String returns the string representation of the scenario type.
func (s ScenarioType) String() string {
	return string(s)
}

// HardwareCapabilities represents the hardware acceleration capabilities detected on the system.
type HardwareCapabilities struct {
	// AvailableEncoders lists all available hardware-accelerated encoders.
	AvailableEncoders []string `json:"available_encoders"`

	// GPUDevices lists detected GPU devices with their paths and types.
	GPUDevices []GPUDevice `json:"gpu_devices"`

	// Platform is the current operating system (linux, darwin, windows).
	Platform string `json:"platform"`

	// HasNVENC indicates NVIDIA NVENC support.
	HasNVENC bool `json:"has_nvenc"`

	// HasQSV indicates Intel Quick Sync Video support.
	HasQSV bool `json:"has_qsv"`

	// HasVAAPI indicates VAAPI support (Linux/AMD/Intel).
	HasVAAPI bool `json:"has_vaapi"`

	// HasAMF indicates AMD AMF support.
	HasAMF bool `json:"has_amf"`

	// HasVideoToolbox indicates Apple VideoToolbox support (macOS).
	HasVideoToolbox bool `json:"has_videotoolbox"`
}

// GPUDevice represents a detected GPU device.
type GPUDevice struct {
	// Type is the GPU acceleration type (nvenc, qsv, vaapi, amf, videotoolbox).
	Type string `json:"type"`

	// Path is the device path (e.g., /dev/dri/renderD128, nvidia-smi, VideoToolbox).
	Path string `json:"path"`

	// Name is the device name/model.
	Name string `json:"name,omitempty"`

	// Vendor is the GPU vendor (NVIDIA, AMD, Intel, Apple).
	Vendor string `json:"vendor,omitempty"`

	// Index is the device index for multi-GPU systems.
	Index int `json:"index,omitempty"`

	// Accessible indicates if the device is accessible.
	Accessible bool `json:"accessible"`

	// QSVHealthy indicates whether the QSV MFX runtime is functional on Intel devices.
	QSVHealthy bool `json:"qsv_healthy,omitempty"`
}

// EncoderPriority defines the priority order for encoder selection.
// Higher values indicate higher priority.
type EncoderPriority string

const (
	PriorityNVENC        EncoderPriority = "nvenc"
	PriorityQSV          EncoderPriority = "qsv"
	PriorityVAAPI        EncoderPriority = "vaapi"
	PriorityAMF          EncoderPriority = "amf"
	PriorityVideoToolbox EncoderPriority = "videotoolbox"
	PrioritySoftware     EncoderPriority = "software"
)

// EncoderPriorityOrder defines the default priority order for encoders.
// NVENC > QSV > VAAPI > AMF > VideoToolbox > Software
var EncoderPriorityOrder = []EncoderPriority{
	PriorityNVENC,
	PriorityQSV,
	PriorityVAAPI,
	PriorityAMF,
	PriorityVideoToolbox,
	PrioritySoftware,
}

// InjectionAuditRecord records the details of a hardware parameter injection.
type InjectionAuditRecord struct {
	// Timestamp is when the injection was performed.
	Timestamp time.Time `json:"timestamp"`

	// TargetEncoder is the encoder family the parameter was injected for.
	TargetEncoder string `json:"target_encoder"`

	// Parameter is the name of the injected parameter.
	Parameter string `json:"parameter"`

	// Value is the value of the injected parameter.
	Value string `json:"value"`

	// Source is the source of the injection (auto, user_override, device_selection).
	Source InjectionSource `json:"source"`

	// DevicePath is the device path if applicable.
	DevicePath string `json:"device_path,omitempty"`

	// ConflictResolution indicates how a parameter conflict was resolved.
	ConflictResolution string `json:"conflict_resolution,omitempty"`

	// Platform is the platform the injection was performed for.
	Platform string `json:"platform"`

	// Notes contains additional information about the injection (e.g., fallback reason).
	Notes string `json:"notes,omitempty"`
}

// InjectionSource represents the source of an injected parameter.
type InjectionSource string

const (
	InjectionSourceAuto            InjectionSource = "auto"
	InjectionSourceDeviceSelection InjectionSource = "device_selection"
	InjectionSourceConflict        InjectionSource = "conflict_resolution"
)

// InjectionResult represents the result of hardware parameter injection.
type InjectionResult struct {
	// InjectedParams is the set of injected hardware parameters.
	// Key: parameter name, Value: parameter value.
	InjectedParams map[string]string `json:"injected_params"`

	// ExistingParams are parameters that were already present and not overridden.
	ExistingParams map[string]string `json:"existing_params"`

	// AuditRecords contains detailed audit information for each injection.
	AuditRecords []InjectionAuditRecord `json:"audit_records"`

	// Errors contains any errors that occurred during injection.
	Errors []string `json:"errors,omitempty"`

	// Warnings contains any warnings generated during injection.
	Warnings []string `json:"warnings,omitempty"`
}
