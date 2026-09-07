package rewrite

import (
	"context"

	"github.com/google/uuid"
	"github.com/tsic404/rffmpeg/pkg/encoder"
)

// TranslatorAdapter wraps encoder.ParameterTranslatorImpl to implement
// the rewrite.ParameterTranslator interface.
// This enables parameter translation (e.g., crf -> qp for VAAPI) during
// auto-hw encoder upgrades.
type TranslatorAdapter struct {
	impl *encoder.ParameterTranslatorImpl
}

// NewTranslatorAdapter creates a new translator adapter with the default
// encoder parameter translator.
func NewTranslatorAdapter() *TranslatorAdapter {
	return &TranslatorAdapter{
		impl: encoder.NewDefaultParameterTranslator(),
	}
}

// Translate translates parameters from a source encoder to a target encoder.
// It implements the rewrite.ParameterTranslator interface.
func (a *TranslatorAdapter) Translate(
	ctx context.Context,
	sourceEncoder, targetEncoder encoder.EncoderFamily,
	params map[string]string,
) (*TranslationResult, error) {
	// Call the underlying translator (ignoring context since the underlying
	// implementation doesn't use it)
	result, err := a.impl.Translate(sourceEncoder, targetEncoder, params)
	if err != nil {
		return nil, err
	}

	// Convert encoder.TranslationResult to rewrite.TranslationResult
	rewriteResult := &TranslationResult{
		TargetEncoder:    result.TargetEncoder,
		TranslatedParams: result.TranslatedParams,
		HardwareParams:   result.HardwareParams,
		Errors:           result.Errors,
		Warnings:         result.Warnings,
		AuditRecords:     make([]AuditRecord, len(result.AuditRecords)),
	}

	// Convert audit records
	for i, record := range result.AuditRecords {
		success := record.Status == encoder.TranslationStatusSuccess ||
			record.Status == encoder.TranslationStatusDefault ||
			record.Status == encoder.TranslationStatusSkipped
		rewriteResult.AuditRecords[i] = AuditRecord{
			ID:            uuid.New().String(),
			Timestamp:     record.Timestamp,
			Operation:     AuditOpParamTranslate,
			SourceEncoder: record.SourceEncoder,
			TargetEncoder: record.TargetEncoder,
			SourceParam:   record.SourceParam,
			TargetParam:   record.TargetParam,
			SourceValue:   record.SourceValue,
			TargetValue:   record.TargetValue,
			Reason:        record.ConverterUsed,
			Success:       success,
			ErrorMessage:  record.ErrorMessage,
		}
	}

	return rewriteResult, nil
}

// GetSupportedTranslations returns encoders that can be translated to the target.
// It implements the rewrite.ParameterTranslator interface.
func (a *TranslatorAdapter) GetSupportedTranslations(targetEncoder encoder.EncoderFamily) []encoder.EncoderFamily {
	return a.impl.GetSupportedTranslations(targetEncoder)
}

// CanTranslate checks if translation is possible between two encoders.
// It implements the rewrite.ParameterTranslator interface.
func (a *TranslatorAdapter) CanTranslate(sourceEncoder, targetEncoder encoder.EncoderFamily) bool {
	// Check if the underlying implementation has direct translation rules
	mapping := a.impl.GetMapping()
	if mapping == nil {
		return false
	}
	return mapping.CanTranslate(sourceEncoder, targetEncoder)
}
