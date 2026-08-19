package rewrite

import (
	"context"

	"github.com/tsix404/rffmpeg/pkg/encoder"
)

// ScenarioClassifier defines the interface for classifying rewrite scenarios.
// It analyzes the input parameters and hardware capabilities to determine
// which of the 6 rewrite scenarios applies.
type ScenarioClassifier interface {
	// Classify analyzes the request and determines the applicable scenario.
	Classify(ctx context.Context, req *EncoderRewriteRequest) (ScenarioType, error)

	// GetScenarioInfo returns detailed information about a scenario.
	GetScenarioInfo(scenario ScenarioType) ScenarioInfo

	// SelectTargetEncoder selects the target encoder for a given scenario.
	SelectTargetEncoder(scenario ScenarioType, req *EncoderRewriteRequest) encoder.EncoderFamily
}

// ScenarioInfo contains detailed information about a rewrite scenario.
type ScenarioInfo struct {
	// Type is the scenario type.
	Type ScenarioType `json:"type"`

	// Description is a human-readable description.
	Description string `json:"description"`

	// RequiresTranslation indicates whether parameter translation is needed.
	RequiresTranslation bool `json:"requires_translation"`

	// RequiresHWInjection indicates whether hardware parameter injection is needed.
	RequiresHWInjection bool `json:"requires_hw_injection"`

	// IsError indicates whether this scenario results in an error.
	IsError bool `json:"is_error"`

	// RecommendedAction describes the recommended action for this scenario.
	RecommendedAction string `json:"recommended_action"`
}

// ParameterTranslator defines the interface for translating encoder parameters.
// It handles parameter name mapping and value conversion between encoder families.
type ParameterTranslator interface {
	// Translate translates parameters from a source encoder to a target encoder.
	Translate(ctx context.Context, sourceEncoder, targetEncoder encoder.EncoderFamily, params map[string]string) (*TranslationResult, error)

	// GetSupportedTranslations returns encoders that can be translated to the target.
	GetSupportedTranslations(targetEncoder encoder.EncoderFamily) []encoder.EncoderFamily

	// CanTranslate checks if translation is possible between two encoders.
	CanTranslate(sourceEncoder, targetEncoder encoder.EncoderFamily) bool
}

// TranslationResult represents the result of a parameter translation.
type TranslationResult struct {
	// TargetEncoder is the encoder the parameters were translated to.
	TargetEncoder encoder.EncoderFamily `json:"target_encoder"`

	// TranslatedParams contains the translated parameters.
	TranslatedParams map[string]string `json:"translated_params"`

	// HardwareParams contains hardware-specific parameters that were injected.
	HardwareParams map[string]string `json:"hardware_params,omitempty"`

	// AuditRecords contains detailed audit information for each parameter.
	AuditRecords []AuditRecord `json:"audit_records"`

	// Errors contains any errors that occurred during translation.
	Errors []string `json:"errors,omitempty"`

	// Warnings contains any warnings generated during translation.
	Warnings []string `json:"warnings,omitempty"`
}

// HardwareInjector defines the interface for injecting hardware-specific parameters.
// It automatically adds necessary hardware parameters based on the target encoder
// and available GPU devices.
type HardwareInjector interface {
	// Inject injects hardware-specific parameters for the given encoder.
	Inject(ctx context.Context, enc encoder.EncoderFamily, params map[string]string, hwCaps *HardwareCapabilities) (*InjectionResult, error)

	// GetRequiredParams returns the required hardware parameters for an encoder.
	GetRequiredParams(enc encoder.EncoderFamily) []string

	// SupportsEncoder checks if hardware injection is supported for an encoder.
	SupportsEncoder(enc encoder.EncoderFamily) bool
}

// InjectionResult represents the result of hardware parameter injection.
type InjectionResult struct {
	// InjectedParams contains the parameters that were injected.
	InjectedParams map[string]string `json:"injected_params"`

	// AllParams contains all parameters after injection.
	AllParams map[string]string `json:"all_params"`

	// DeviceUsed is the GPU device that was selected.
	DeviceUsed *GPUDevice `json:"device_used,omitempty"`

	// AuditRecords contains audit records for the injection.
	AuditRecords []AuditRecord `json:"audit_records"`

	// Warnings contains any warnings generated during injection.
	Warnings []string `json:"warnings,omitempty"`
}

// AuditRecorder defines the interface for recording audit information.
// It maintains a complete audit trail of all rewrite operations.
type AuditRecorder interface {
	// Record records a single audit entry.
	Record(ctx context.Context, record *AuditRecord) error

	// RecordBatch records multiple audit entries in a single operation.
	RecordBatch(ctx context.Context, records []*AuditRecord) error

	// GetRecords retrieves audit records for a request.
	GetRecords(ctx context.Context, requestID string) ([]AuditRecord, error)

	// GetSummary returns a summary of audit records for a request.
	GetSummary(ctx context.Context, requestID string) (*AuditSummary, error)
}

// AuditSummary contains a summary of audit records.
type AuditSummary struct {
	// RequestID is the request identifier.
	RequestID string `json:"request_id"`

	// TotalOperations is the total number of operations performed.
	TotalOperations int `json:"total_operations"`

	// SuccessfulOperations is the number of successful operations.
	SuccessfulOperations int `json:"successful_operations"`

	// FailedOperations is the number of failed operations.
	FailedOperations int `json:"failed_operations"`

	// OperationsByType counts operations by type.
	OperationsByType map[AuditOperation]int `json:"operations_by_type"`

	// OriginalEncoder is the starting encoder.
	OriginalEncoder encoder.EncoderFamily `json:"original_encoder"`

	// FinalEncoder is the final encoder after all operations.
	FinalEncoder encoder.EncoderFamily `json:"final_encoder"`

	// Scenario is the rewrite scenario that was applied.
	Scenario ScenarioType `json:"scenario"`

	// Timestamp is when the summary was generated.
	Timestamp string `json:"timestamp"`
}

// Notifier defines the interface for real-time notifications.
// It outputs rewrite notifications to stderr during FFmpeg execution.
type Notifier interface {
	// Notify sends a notification message.
	Notify(ctx context.Context, notification *Notification) error

	// NotifyBatch sends multiple notifications.
	NotifyBatch(ctx context.Context, notifications []*Notification) error

	// FormatNotification formats a notification for stderr output.
	FormatNotification(notification *Notification) string

	// SetOutput sets the output destination for notifications.
	SetOutput(output NotifierOutput)
}

// NotifierOutput defines the output destination for notifications.
type NotifierOutput interface {
	// Write writes the notification output.
	Write(data []byte) (int, error)
}

// RewriteEngine defines the main interface for the encoder rewrite engine.
// It orchestrates all components to perform complete parameter rewriting.
type RewriteEngine interface {
	// Rewrite performs a complete rewrite of the encoder parameters.
	Rewrite(ctx context.Context, req *EncoderRewriteRequest) (*EncoderRewriteResponse, error)

	// SetClassifier sets the scenario classifier implementation.
	SetClassifier(classifier ScenarioClassifier)

	// SetTranslator sets the parameter translator implementation.
	SetTranslator(translator ParameterTranslator)

	// SetHardwareInjector sets the hardware injector implementation.
	SetHardwareInjector(injector HardwareInjector)

	// SetAuditRecorder sets the audit recorder implementation.
	SetAuditRecorder(recorder AuditRecorder)

	// SetNotifier sets the notifier implementation.
	SetNotifier(notifier Notifier)
}
