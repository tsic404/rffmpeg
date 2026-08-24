// Package encoder provides parameter translation functionality for FFmpeg encoder parameters.
// It supports translation between different encoder families with parameter name mapping,
// value conversion, and audit trail generation.
package encoder

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// TranslationStatus represents the status of a parameter translation.
type TranslationStatus string

const (
	TranslationStatusSuccess TranslationStatus = "success"
	TranslationStatusSkipped TranslationStatus = "skipped"
	TranslationStatusFailed  TranslationStatus = "failed"
	TranslationStatusDefault TranslationStatus = "default_applied"
)

// ConverterUsedHardwareInjection marks audit records whose parameter was
// injected as a hardware default rather than translated from a user param.
const ConverterUsedHardwareInjection = "hardware_injection"

// TranslationAuditRecord records the details of a single parameter translation.
// It captures the source and target values, any conversions applied, and errors encountered.
type TranslationAuditRecord struct {
	// Timestamp is when the translation was performed.
	Timestamp time.Time `json:"timestamp"`

	// SourceEncoder is the encoder family the parameter originated from.
	SourceEncoder EncoderFamily `json:"source_encoder"`

	// TargetEncoder is the encoder family the parameter was translated to.
	TargetEncoder EncoderFamily `json:"target_encoder"`

	// SourceParam is the original parameter name.
	SourceParam string `json:"source_param"`

	// TargetParam is the translated parameter name.
	TargetParam string `json:"target_param"`

	// SourceValue is the original parameter value.
	SourceValue string `json:"source_value"`

	// TargetValue is the translated parameter value.
	TargetValue string `json:"target_value"`

	// Status indicates whether the translation succeeded, was skipped, or failed.
	Status TranslationStatus `json:"status"`

	// ConverterUsed is the name of the converter function applied, if any.
	ConverterUsed string `json:"converter_used,omitempty"`

	// ErrorMessage contains the error message if translation failed.
	ErrorMessage string `json:"error_message,omitempty"`

	// ChainPosition indicates the position in a chain translation (0-based).
	// For single translations, this is 0.
	ChainPosition int `json:"chain_position,omitempty"`

	// ChainTotal indicates the total number of translations in a chain.
	// For single translations, this is 1.
	ChainTotal int `json:"chain_total,omitempty"`
}

// TranslationResult represents the result of translating a set of parameters.
type TranslationResult struct {
	// TargetEncoder is the encoder family the parameters were translated to.
	TargetEncoder EncoderFamily `json:"target_encoder"`

	// TranslatedParams is the set of translated parameters in ffmpeg format.
	// Key: parameter name, Value: parameter value.
	TranslatedParams map[string]string `json:"translated_params"`

	// HardwareParams is the set of hardware-specific parameters injected.
	// Key: parameter name, Value: parameter value.
	HardwareParams map[string]string `json:"hardware_params,omitempty"`

	// AuditRecords contains detailed audit information for each translated parameter.
	AuditRecords []TranslationAuditRecord `json:"audit_records"`

	// Errors contains any errors that occurred during translation.
	Errors []string `json:"errors,omitempty"`

	// Warnings contains any warnings generated during translation.
	Warnings []string `json:"warnings,omitempty"`
}

// ParameterTranslator defines the interface for translating encoder parameters.
type ParameterTranslator interface {
	// Translate translates parameters from a source encoder to a target encoder.
	// It performs parameter name mapping, value conversion, and validation.
	// Returns a TranslationResult containing the translated parameters and audit records.
	Translate(sourceEncoder, targetEncoder EncoderFamily, params map[string]string) (*TranslationResult, error)

	// TranslateChain performs multi-level translation through a chain of encoders.
	// For example, libx264 -> h264_nvenc -> h264_qsv translates parameters
	// through each encoder in the chain.
	TranslateChain(encoderChain []EncoderFamily, params map[string]string) (*TranslationResult, error)

	// ValidateParameters validates that parameters are valid for a given encoder.
	ValidateParameters(encoder EncoderFamily, params map[string]string) ([]string, error)

	// NormalizeParameters normalizes parameter values to a canonical form.
	NormalizeParameters(encoder EncoderFamily, params map[string]string) (map[string]string, error)

	// GetSupportedTranslations returns a list of source encoders that can be translated to the target.
	GetSupportedTranslations(targetEncoder EncoderFamily) []EncoderFamily

	// GetAuditRecords returns all audit records from the last translation.
	GetAuditRecords() []TranslationAuditRecord
}

// ParameterTranslatorImpl implements the ParameterTranslator interface.
// It uses the EncoderMapping for parameter translation rules and value converters.
type ParameterTranslatorImpl struct {
	// mapping is the encoder mapping containing all translation rules.
	mapping *EncoderMapping

	// h264Mapping provides H.264 specific parameter mappings.
	h264Mapping *H264CommonMapping

	// hevcMapping provides HEVC specific parameter mappings.
	hevcMapping *HEVCCommonMapping

	// vp9Mapping provides VP9 specific parameter mappings.
	vp9Mapping *VP9CommonMapping

	// av1Mapping provides AV1 specific parameter mappings.
	av1Mapping *AV1CommonMapping

	// auditMu guards auditRecords: the translator is shared across concurrent
	// rewrites via TranslatorAdapter/EngineCoordinator, and Translate both
	// writes this field and is called concurrently.
	auditMu sync.Mutex

	// auditRecords stores audit records from the last translation.
	auditRecords []TranslationAuditRecord

	// strictMode when true causes translation to fail on any error.
	// When false, errors are recorded but translation continues.
	strictMode bool

	// injectHardwareParams when true automatically injects hardware-specific parameters.
	injectHardwareParams bool
}

// TranslatorOption is a function that configures the ParameterTranslatorImpl.
type TranslatorOption func(*ParameterTranslatorImpl)

// WithStrictMode configures the translator to fail on any translation error.
func WithStrictMode(strict bool) TranslatorOption {
	return func(t *ParameterTranslatorImpl) {
		t.strictMode = strict
	}
}

// WithHardwareParamInjection configures the translator to inject hardware-specific parameters.
func WithHardwareParamInjection(inject bool) TranslatorOption {
	return func(t *ParameterTranslatorImpl) {
		t.injectHardwareParams = inject
	}
}

// NewParameterTranslator creates a new ParameterTranslator with the given mapping.
// The mapping should contain all necessary encoder families and translation rules.
func NewParameterTranslator(mapping *EncoderMapping, opts ...TranslatorOption) *ParameterTranslatorImpl {
	t := &ParameterTranslatorImpl{
		mapping:              mapping,
		auditRecords:         make([]TranslationAuditRecord, 0),
		strictMode:           false,
		injectHardwareParams: true,
		h264Mapping:          NewH264CommonMapping(),
		hevcMapping:          NewHEVCCommonMapping(),
		vp9Mapping:           NewVP9CommonMapping(),
		av1Mapping:           NewAV1CommonMapping(),
	}

	for _, opt := range opts {
		opt(t)
	}

	return t
}

// NewDefaultParameterTranslator creates a new ParameterTranslator with default mappings.
func NewDefaultParameterTranslator(opts ...TranslatorOption) *ParameterTranslatorImpl {
	return NewParameterTranslator(DefaultMapping(), opts...)
}

// Translate translates parameters from a source encoder to a target encoder.
func (t *ParameterTranslatorImpl) Translate(sourceEncoder, targetEncoder EncoderFamily, params map[string]string) (*TranslationResult, error) {
	// Reset previous audit records (guarded — see auditMu).
	t.auditMu.Lock()
	t.auditRecords = make([]TranslationAuditRecord, 0)
	t.auditMu.Unlock()

	result := &TranslationResult{
		TargetEncoder:    targetEncoder,
		TranslatedParams: make(map[string]string),
		HardwareParams:   make(map[string]string),
		AuditRecords:     make([]TranslationAuditRecord, 0),
		Errors:           make([]string, 0),
		Warnings:         make([]string, 0),
	}

	// Validate that source and target encoders are registered
	if _, exists := t.mapping.GetCodecFormat(sourceEncoder); !exists {
		return nil, fmt.Errorf("source encoder '%s' is not registered", sourceEncoder)
	}

	targetFormat, exists := t.mapping.GetCodecFormat(targetEncoder)
	if !exists {
		return nil, fmt.Errorf("target encoder '%s' is not registered", targetEncoder)
	}

	// Verify codec format compatibility
	sourceFormat, _ := t.mapping.GetCodecFormat(sourceEncoder)
	if sourceFormat != targetFormat {
		result.Warnings = append(result.Warnings,
			fmt.Sprintf("cross-codec translation from %s to %s may not support all parameters",
				sourceFormat, targetFormat))
	}

	// Check if translation rules exist
	if !t.mapping.CanTranslate(sourceEncoder, targetEncoder) {
		result.Warnings = append(result.Warnings,
			fmt.Sprintf("no direct translation rules from %s to %s, using codec-specific fallback",
				sourceEncoder, targetEncoder))
	}

	// Translate each parameter in DETERMINISTIC order (sorted by name) so
	// that two source params mapping to the same target param (e.g. SVT-AV1
	// preset/speed both -> av1_nvenc "preset") resolve identically on every
	// run instead of by Go map iteration order.
	paramNames := make([]string, 0, len(params))
	for sourceParam := range params {
		paramNames = append(paramNames, sourceParam)
	}
	sort.Strings(paramNames)

	for _, sourceParam := range paramNames {
		sourceValue := params[sourceParam]
		record := t.translateSingleParam(sourceEncoder, targetEncoder, sourceParam, sourceValue, 0, 1)
		result.AuditRecords = append(result.AuditRecords, record)

		switch record.Status {
		case TranslationStatusSuccess, TranslationStatusDefault:
			if record.TargetParam != "" {
				result.TranslatedParams[record.TargetParam] = record.TargetValue
			}
		case TranslationStatusFailed:
			result.Errors = append(result.Errors, record.ErrorMessage)
			// An empty source value must never reach the output args: it
			// would emit a bare "-flag ''" that swallows the next token.
			// Warn (and error) without keeping the parameter.
			if sourceValue == "" {
				result.Warnings = append(result.Warnings, fmt.Sprintf(
					"parameter '%s' dropped: empty value and translation failed: %s",
					sourceParam, record.ErrorMessage))
				if t.strictMode {
					return result, fmt.Errorf("translation failed for parameter '%s': %s",
						sourceParam, record.ErrorMessage)
				}
				break
			}
			// Preserve the original parameter so it is never silently
			// dropped: the value may still be valid for the target encoder,
			// and losing it changes the output quality without notice.
			result.TranslatedParams[sourceParam] = sourceValue
			result.Warnings = append(result.Warnings, fmt.Sprintf(
				"translation failed for parameter '%s=%s', keeping original value: %s",
				sourceParam, sourceValue, record.ErrorMessage))
			if t.strictMode {
				return result, fmt.Errorf("translation failed for parameter '%s': %s",
					sourceParam, record.ErrorMessage)
			}
		case TranslationStatusSkipped:
			declaredUnsupported := strings.Contains(record.ErrorMessage,
				"is not supported by target encoder")

			// (a) The mapping explicitly declares the target encoder does
			// not support this parameter: DROP it. Passing it through emits
			// an option the encoder does not have and ffmpeg fails hard.
			if declaredUnsupported ||
				(t.isCodecScopedParam(sourceParam) &&
					t.codecFormatsDiffer(sourceEncoder, targetEncoder)) {
				result.Warnings = append(result.Warnings, fmt.Sprintf(
					"parameter '%s=%s' removed: not supported by %s",
					sourceParam, sourceValue, targetEncoder))
				break
			}

			// (b) No rule at all — an unknown user parameter. Keep the
			// original name/value pair and surface a warning; it may still
			// be valid for the target.
			result.TranslatedParams[sourceParam] = sourceValue
			result.Warnings = append(result.Warnings, fmt.Sprintf(
				"parameter '%s=%s' has no translation to %s, passing through unchanged",
				sourceParam, sourceValue, targetEncoder))
		}
	}

	// Inject hardware-specific parameters if enabled. This runs after the
	// user-parameter loop so an explicit user parameter always wins over a
	// default (map iteration order must not decide the outcome).
	if t.injectHardwareParams {
		t.injectHardwareParamsForEncoder(targetEncoder, result)
	}

	t.auditMu.Lock()
	t.auditRecords = result.AuditRecords
	t.auditMu.Unlock()
	return result, nil
}

// translateSingleParam translates a single parameter and returns an audit record.
func (t *ParameterTranslatorImpl) translateSingleParam(
	sourceEncoder, targetEncoder EncoderFamily,
	sourceParam, sourceValue string,
	chainPosition, chainTotal int,
) TranslationAuditRecord {
	record := TranslationAuditRecord{
		Timestamp:     time.Now(),
		SourceEncoder: sourceEncoder,
		TargetEncoder: targetEncoder,
		SourceParam:   sourceParam,
		SourceValue:   sourceValue,
		ChainPosition: chainPosition,
		ChainTotal:    chainTotal,
	}

	// Try to get translation rule from EncoderMapping
	rule := t.mapping.GetParameterTranslation(sourceEncoder, targetEncoder, sourceParam)

	// If no direct rule, try codec-specific mapping
	if rule == nil {
		rule = t.getCodecSpecificRule(sourceEncoder, targetEncoder, sourceParam)
	}

	if rule == nil {
		if t.shouldPassThrough(sourceEncoder, targetEncoder, sourceParam) {
			record.TargetParam = sourceParam
			record.TargetValue = sourceValue
			record.Status = TranslationStatusSuccess
		} else {
			record.Status = TranslationStatusSkipped
			record.ErrorMessage = fmt.Sprintf("no translation rule for parameter '%s'", sourceParam)
		}
		return record
	}

	// Apply the translation
	record.TargetParam = rule.TargetParam
	if record.TargetParam == "" {
		record.Status = TranslationStatusSkipped
		// The rule (direct or codec-specific) exists but declares the
		// parameter unsupported by the target encoder. This is an explicit
		// declaration, not a missing rule: the parameter must be DROPPED.
		record.ErrorMessage = fmt.Sprintf("parameter '%s' is not supported by target encoder", sourceParam)
		return record
	}

	// Get the appropriate value converter for this parameter
	// This ensures converters are applied even if the rule doesn't have one attached
	converter := rule.Converter
	if converter == nil {
		converter = t.getValueConverterForParam(sourceEncoder, targetEncoder, sourceParam)
	}

	// Convert the value
	var err error
	if converter != nil {
		record.TargetValue, err = converter(sourceValue)
	} else {
		record.TargetValue = sourceValue
	}
	if err != nil {
		record.Status = TranslationStatusFailed
		record.ErrorMessage = fmt.Sprintf("value conversion failed: %v", err)
		return record
	}

	// Check if default was applied
	if sourceValue == "" && rule.DefaultValue != "" {
		record.TargetValue = rule.DefaultValue
		record.Status = TranslationStatusDefault
	} else {
		record.Status = TranslationStatusSuccess
	}

	// Record converter used if applicable
	if converter != nil {
		record.ConverterUsed = fmt.Sprintf("%T", converter)
	}

	return record
}

// getValueConverterForParam gets the appropriate value converter for a parameter translation.
func (t *ParameterTranslatorImpl) getValueConverterForParam(sourceEncoder, targetEncoder EncoderFamily, sourceParam string) ValueConverter {
	format, exists := t.mapping.GetCodecFormat(sourceEncoder)
	if !exists {
		return nil
	}

	switch format {
	case CodecH264:
		return t.getH264ValueConverter(sourceParam, targetEncoder)
	case CodecHEVC:
		return t.getHEVCValueConverter(sourceParam, targetEncoder)
	case CodecVP9:
		return t.getVP9ValueConverter(sourceParam, targetEncoder)
	case CodecAV1:
		return t.getAV1ValueConverter(sourceParam, targetEncoder)
	}
	return nil
}

// getCodecSpecificRule attempts to get a rule from codec-specific mappings.
//
// Parameters whose value domain is codec-specific (profile, level, qp) are
// only resolved when source and target share the same codec format — the
// per-target NameTranslations tables describe same-codec hardware variants,
// not cross-codec semantics.
func (t *ParameterTranslatorImpl) getCodecSpecificRule(
	sourceEncoder, targetEncoder EncoderFamily,
	sourceParam string,
) *ParameterRule {
	switch sourceParam {
	case "profile", "level", "qp":
		sourceFormat, srcOK := t.mapping.GetCodecFormat(sourceEncoder)
		targetFormat, tgtOK := t.mapping.GetCodecFormat(targetEncoder)
		if !srcOK || !tgtOK || sourceFormat != targetFormat {
			return nil
		}
	}

	// Get the codec format for the source encoder
	format, exists := t.mapping.GetCodecFormat(sourceEncoder)
	if !exists {
		return nil
	}

	var (
		targetParam string
		converter   ValueConverter
		// declaredUnsupported records that the NameTranslations table has an
		// explicit "" entry for this parameter: the target encoder does not
		// support it and it must be dropped, never passed through.
		declaredUnsupported bool
	)

	check := func(m map[string]string) {
		if tgt, ok := m[sourceParam]; ok && tgt == "" {
			declaredUnsupported = true
		}
	}

	switch format {
	case CodecH264:
		targetParam = t.h264Mapping.GetCommonParameterTranslation(targetEncoder, sourceParam)
		if translations, ok := t.h264Mapping.NameTranslations[targetEncoder]; ok {
			check(translations)
		}
		if targetParam != "" {
			converter = t.getH264ValueConverter(sourceParam, targetEncoder)
		}
	case CodecHEVC:
		targetParam = t.hevcMapping.GetCommonParameterTranslation(targetEncoder, sourceParam)
		if translations, ok := t.hevcMapping.NameTranslations[targetEncoder]; ok {
			check(translations)
		}
		if targetParam != "" {
			converter = t.getHEVCValueConverter(sourceParam, targetEncoder)
		}
	case CodecVP9:
		targetParam = t.vp9Mapping.GetCommonParameterTranslation(targetEncoder, sourceParam)
		if translations, ok := t.vp9Mapping.NameTranslations[targetEncoder]; ok {
			check(translations)
		}
		if targetParam != "" {
			converter = t.getVP9ValueConverter(sourceParam, targetEncoder)
		}
	case CodecAV1:
		targetParam = t.av1Mapping.GetCommonParameterTranslation(targetEncoder, sourceParam)
		if translations, ok := t.av1Mapping.NameTranslations[targetEncoder]; ok {
			check(translations)
		}
		if targetParam != "" {
			converter = t.getAV1ValueConverter(sourceParam, targetEncoder)
		}
	}

	if targetParam == "" {
		if declaredUnsupported {
			return &ParameterRule{
				SourceParam: sourceParam,
				TargetParam: "",
				Description: "parameter is explicitly not supported by the target encoder",
			}
		}
		return nil
	}

	return &ParameterRule{
		SourceParam: sourceParam,
		TargetParam: targetParam,
		Converter:   converter,
	}
}

// getH264ValueConverter returns the appropriate value converter for H.264 parameters.
func (t *ParameterTranslatorImpl) getH264ValueConverter(param string, targetEncoder EncoderFamily) ValueConverter {
	switch param {
	case "crf":
		switch targetEncoder {
		case EncoderH264NVENC:
			return func(v string) (string, error) { return v, nil } // cq uses same value
		case EncoderH264QSV:
			return func(v string) (string, error) { return v, nil } // global_quality uses same value
		case EncoderH264VAAPI:
			return t.crfToVAAPIQualityConverter
		}
	case "preset":
		switch targetEncoder {
		case EncoderH264NVENC:
			return x264PresetToNVENC
		case EncoderH264QSV:
			return t.x264PresetToQSV
		case EncoderH264AMF:
			return t.x264PresetToAMF
		}
	}
	return nil
}

// getHEVCValueConverter returns the appropriate value converter for HEVC parameters.
func (t *ParameterTranslatorImpl) getHEVCValueConverter(param string, targetEncoder EncoderFamily) ValueConverter {
	switch param {
	case "crf":
		switch targetEncoder {
		case EncoderHEVCNVENC:
			return func(v string) (string, error) { return v, nil }
		case EncoderHEVCQSV:
			return func(v string) (string, error) { return v, nil }
		case EncoderHEVCVAAPI:
			return t.crfToVAAPIQualityConverter
		}
	case "preset":
		switch targetEncoder {
		case EncoderHEVCNVENC:
			return x265PresetToNVENC
		case EncoderHEVCQSV:
			return t.x264PresetToQSV
		case EncoderHEVCAMF:
			return t.x264PresetToAMF
		}
	}
	return nil
}

// getVP9ValueConverter returns the appropriate value converter for VP9 parameters.
func (t *ParameterTranslatorImpl) getVP9ValueConverter(param string, targetEncoder EncoderFamily) ValueConverter {
	switch param {
	case "crf":
		switch targetEncoder {
		case EncoderVP9NVENC:
			return func(v string) (string, error) { return v, nil }
		case EncoderVP9QSV:
			return func(v string) (string, error) { return v, nil }
		case EncoderVP9VAAPI:
			return t.vp9CRFToVAAPIQualityConverter
		}
	}
	return nil
}

// getAV1ValueConverter returns the appropriate value converter for AV1 parameters.
func (t *ParameterTranslatorImpl) getAV1ValueConverter(param string, targetEncoder EncoderFamily) ValueConverter {
	switch param {
	case "crf":
		switch targetEncoder {
		case EncoderAV1NVENC:
			return func(v string) (string, error) { return v, nil }
		case EncoderAV1QSV:
			return func(v string) (string, error) { return v, nil }
		case EncoderAV1VAAPI:
			return t.av1CRFToVAAPIQualityConverter
		}
	case "preset":
		// Convert preset names to NVENC presets
		if targetEncoder == EncoderAV1NVENC {
			return t.av1PresetToNVENC
		}
	case "speed":
		// Convert numeric speed values to NVENC presets
		if targetEncoder == EncoderAV1NVENC {
			return svtav1PresetToNVENC
		}
		if targetEncoder == EncoderLibAOM {
			return svtav1SpeedToCPUsed
		}
	}
	return nil
}

// av1PresetToNVENC converts AV1 preset names to NVENC preset values.
func (t *ParameterTranslatorImpl) av1PresetToNVENC(v string) (string, error) {
	// SVT-AV1 presets are numeric (0-13) and av1_nvenc only accepts p1-p7.
	// Route numeric values through the SVT-AV1 converter; named x264-style
	// presets keep the text mapping.
	if _, err := strconv.Atoi(v); err == nil {
		return svtav1PresetToNVENC(v)
	}
	return x264PresetToNVENC(v)
}

// Value converter wrappers
func (t *ParameterTranslatorImpl) crfToVAAPIQualityConverter(v string) (string, error) {
	return crfToVAAPIQualityConverter(v)
}

func (t *ParameterTranslatorImpl) vp9CRFToVAAPIQualityConverter(v string) (string, error) {
	numVal, err := parseIntValue(v)
	if err != nil {
		return "", err
	}
	quality := vp9CRFToVAAPIQuality(numVal)
	return fmt.Sprintf("%d", quality), nil
}

func (t *ParameterTranslatorImpl) av1CRFToVAAPIQualityConverter(v string) (string, error) {
	numVal, err := parseIntValue(v)
	if err != nil {
		return "", err
	}
	quality := av1CRFToVAAPIQuality(numVal)
	return fmt.Sprintf("%d", quality), nil
}

func (t *ParameterTranslatorImpl) x264PresetToQSV(v string) (string, error) {
	mapping := map[string]string{
		"ultrafast": "veryfast",
		"superfast": "veryfast",
		"veryfast":  "veryfast",
		"faster":    "faster",
		"fast":      "fast",
		"medium":    "medium",
		"slow":      "slow",
		"slower":    "slower",
		"veryslow":  "veryslow",
		"placebo":   "veryslow",
	}
	if result, ok := mapping[v]; ok {
		return result, nil
	}
	return v, nil
}

// isCodecScopedParam reports whether the parameter's valid value domain
// depends on the codec format.
func (t *ParameterTranslatorImpl) isCodecScopedParam(param string) bool {
	switch param {
	case "profile", "level", "qp":
		return true
	}
	return false
}

// codecFormatsDiffer reports whether source and target encoders belong to
// different codec formats (h264/hevc/vp9/av1).
func (t *ParameterTranslatorImpl) codecFormatsDiffer(sourceEncoder, targetEncoder EncoderFamily) bool {
	sourceFormat, srcOK := t.mapping.GetCodecFormat(sourceEncoder)
	targetFormat, tgtOK := t.mapping.GetCodecFormat(targetEncoder)
	return !srcOK || !tgtOK || sourceFormat != targetFormat
}

func (t *ParameterTranslatorImpl) x264PresetToAMF(v string) (string, error) {
	mapping := map[string]string{
		"ultrafast": "speed",
		"superfast": "speed",
		"veryfast":  "speed",
		"faster":    "balanced",
		"fast":      "balanced",
		"medium":    "balanced",
		"slow":      "quality",
		"slower":    "quality",
		"veryslow":  "quality",
		"placebo":   "quality",
	}
	if result, ok := mapping[v]; ok {
		return result, nil
	}
	return v, nil
}

// shouldPassThrough returns true if a parameter should be passed through
// unchanged when no translation rule exists.
//
// Parameters whose valid value domain depends on the codec (profile, level,
// qp) are only passed through between encoders of the SAME codec format —
// e.g. "-profile high" is legal for H.264 but meaningless/illegal for VP9.
func (t *ParameterTranslatorImpl) shouldPassThrough(
	sourceEncoder, targetEncoder EncoderFamily, param string,
) bool {
	// Common ffmpeg parameters that can be passed through
	passThroughParams := map[string]bool{
		"b:v":     true,
		"maxrate": true,
		"bufsize": true,
		"g":       true,
		"bf":      true,
		"refs":    true,
	}
	if passThroughParams[param] {
		return true
	}

	switch param {
	case "profile", "level", "qp":
		sourceFormat, srcOK := t.mapping.GetCodecFormat(sourceEncoder)
		targetFormat, tgtOK := t.mapping.GetCodecFormat(targetEncoder)
		return srcOK && tgtOK && sourceFormat == targetFormat
	}
	return false
}

// injectHardwareParamsForEncoder adds hardware-specific parameters for the target encoder.
func (t *ParameterTranslatorImpl) injectHardwareParamsForEncoder(targetEncoder EncoderFamily, result *TranslationResult) {
	vendor := targetEncoder.GPUVendor()
	if vendor == GPUVendorNone {
		return
	}

	hwParams := t.mapping.GetHardwareParams(targetEncoder, vendor)
	for _, param := range hwParams {
		// Don't override if already set
		if _, exists := result.TranslatedParams[param.Param]; !exists {
			// Respect mutual-exclusion metadata: if the user (or an earlier
			// translation) already supplied a conflicting parameter — e.g. an
			// explicit cq alongside an injected rc=constqp — skip the
			// injection instead of emitting a command ffmpeg will reject.
			conflict := false
			for _, c := range param.ConflictsWith {
				if _, exists := result.TranslatedParams[c]; exists {
					result.Warnings = append(result.Warnings, fmt.Sprintf(
						"hardware parameter '%s=%s' not injected: conflicts with existing parameter '%s'",
						param.Param, param.Value, c))
					conflict = true
					break
				}
			}
			if conflict {
				continue
			}

			result.HardwareParams[param.Param] = param.Value
			result.TranslatedParams[param.Param] = param.Value

			// Add audit record for hardware param injection
			record := TranslationAuditRecord{
				Timestamp:     time.Now(),
				SourceEncoder: "",
				TargetEncoder: targetEncoder,
				SourceParam:   param.Param,
				TargetParam:   param.Param,
				SourceValue:   "",
				TargetValue:   param.Value,
				Status:        TranslationStatusDefault,
				ConverterUsed: ConverterUsedHardwareInjection,
			}
			result.AuditRecords = append(result.AuditRecords, record)
		}
	}
}

// TranslateChain performs multi-level translation through a chain of encoders.
func (t *ParameterTranslatorImpl) TranslateChain(encoderChain []EncoderFamily, params map[string]string) (*TranslationResult, error) {
	if len(encoderChain) < 2 {
		return nil, fmt.Errorf("encoder chain must have at least 2 encoders (source and target)")
	}

	// Reset previous audit records (guarded — see auditMu).
	t.auditMu.Lock()
	t.auditRecords = make([]TranslationAuditRecord, 0)
	t.auditMu.Unlock()

	result := &TranslationResult{
		TargetEncoder:    encoderChain[len(encoderChain)-1],
		TranslatedParams: make(map[string]string),
		HardwareParams:   make(map[string]string),
		AuditRecords:     make([]TranslationAuditRecord, 0),
		Errors:           make([]string, 0),
		Warnings:         make([]string, 0),
	}

	// Start with the input parameters
	currentParams := make(map[string]string)
	for k, v := range params {
		currentParams[k] = v
	}

	// Translate through each encoder in the chain. Hardware injection is
	// disabled for intermediate stages (only the FINAL stage injects, after
	// this loop): a mid-chain nvenc's rc/async_depth must never leak into
	// later stages as if it were a user parameter.
	savedInject := t.injectHardwareParams
	if len(encoderChain) > 2 {
		t.injectHardwareParams = false
	}
	for i := 0; i < len(encoderChain)-1; i++ {
		sourceEncoder := encoderChain[i]
		targetEncoder := encoderChain[i+1]

		// Translate to the next encoder
		stageResult, err := t.Translate(sourceEncoder, targetEncoder, currentParams)
		if err != nil {
			if t.strictMode {
				t.injectHardwareParams = savedInject
				return result, fmt.Errorf("chain translation failed at step %d (%s -> %s): %w",
					i+1, sourceEncoder, targetEncoder, err)
			}
			result.Errors = append(result.Errors,
				fmt.Sprintf("chain step %d (%s -> %s) error: %v", i+1, sourceEncoder, targetEncoder, err))
			continue
		}

		// Update current params for next iteration. TranslatedParams now
		// carries Skipped/Failed originals too (preserved by Translate), so
		// parameters without a rule at this stage still reach later chain
		// stages instead of vanishing after the first hop.
		currentParams = make(map[string]string)
		for k, v := range stageResult.TranslatedParams {
			currentParams[k] = v
		}

		// Collect warnings and errors
		result.Warnings = append(result.Warnings, stageResult.Warnings...)
		result.Errors = append(result.Errors, stageResult.Errors...)

		// Update chain position in audit records
		for j := range stageResult.AuditRecords {
			stageResult.AuditRecords[j].ChainPosition = i
			stageResult.AuditRecords[j].ChainTotal = len(encoderChain) - 1
		}
		result.AuditRecords = append(result.AuditRecords, stageResult.AuditRecords...)
	}
	t.injectHardwareParams = savedInject

	// Set final translated params
	result.TranslatedParams = currentParams

	// Inject hardware params for the FINAL target encoder only, and stamp
	// the final-stage chain metadata onto the injected audit records so they
	// match records from the loop above.
	if t.injectHardwareParams {
		finalIdx := len(encoderChain) - 2
		before := len(result.AuditRecords)
		t.injectHardwareParamsForEncoder(encoderChain[len(encoderChain)-1], result)
		for j := before; j < len(result.AuditRecords); j++ {
			result.AuditRecords[j].ChainPosition = finalIdx
			result.AuditRecords[j].ChainTotal = len(encoderChain) - 1
		}
	}

	t.auditMu.Lock()
	t.auditRecords = result.AuditRecords
	t.auditMu.Unlock()
	return result, nil
}

// ValidateParameters validates that parameters are valid for a given encoder.
func (t *ParameterTranslatorImpl) ValidateParameters(encoder EncoderFamily, params map[string]string) ([]string, error) {
	var errors []string

	format, exists := t.mapping.GetCodecFormat(encoder)
	if !exists {
		return nil, fmt.Errorf("encoder '%s' is not registered", encoder)
	}

	// Get the appropriate codec-specific mapping for validation
	var rangeMappings map[string]ValueRange
	switch format {
	case CodecH264:
		rangeMappings = t.h264Mapping.RangeMappings
	case CodecHEVC:
		rangeMappings = t.hevcMapping.RangeMappings
	case CodecVP9:
		rangeMappings = t.vp9Mapping.RangeMappings
	case CodecAV1:
		rangeMappings = t.av1Mapping.RangeMappings
	}

	// Validate each parameter
	for param, value := range params {
		rangeSpec, exists := rangeMappings[param]
		if !exists {
			// No range spec for this parameter, skip validation
			continue
		}

		// Check valid discrete values
		if len(rangeSpec.ValidValues) > 0 {
			valid := false
			for _, validVal := range rangeSpec.ValidValues {
				if strings.EqualFold(value, validVal) {
					valid = true
					break
				}
			}
			if !valid {
				errors = append(errors, fmt.Sprintf("parameter '%s' value '%s' is not valid. Valid values: %v",
					param, value, rangeSpec.ValidValues))
			}
			continue
		}

		// Check numeric range
		numValue, err := parseIntValue(value)
		if err != nil {
			errors = append(errors, fmt.Sprintf("parameter '%s' expects numeric value, got '%s'",
				param, value))
			continue
		}

		if numValue < rangeSpec.Min || numValue > rangeSpec.Max {
			errors = append(errors, fmt.Sprintf("parameter '%s' value %d is out of range [%d, %d]",
				param, numValue, rangeSpec.Min, rangeSpec.Max))
		}
	}

	return errors, nil
}

// NormalizeParameters normalizes parameter values to a canonical form.
func (t *ParameterTranslatorImpl) NormalizeParameters(encoder EncoderFamily, params map[string]string) (map[string]string, error) {
	normalized := make(map[string]string)

	format, exists := t.mapping.GetCodecFormat(encoder)
	if !exists {
		return nil, fmt.Errorf("encoder '%s' is not registered", encoder)
	}

	// Get the appropriate codec-specific mapping for normalization
	var standardParams map[string]ParameterSpec
	switch format {
	case CodecH264:
		standardParams = t.h264Mapping.StandardParams
	case CodecHEVC:
		standardParams = t.hevcMapping.StandardParams
	case CodecVP9:
		standardParams = t.vp9Mapping.StandardParams
	case CodecAV1:
		standardParams = t.av1Mapping.StandardParams
	}

	// Normalize each parameter
	for param, value := range params {
		// Normalize parameter name to lowercase
		normalizedName := strings.ToLower(param)

		// If value is empty, try to use default
		if value == "" {
			if spec, exists := standardParams[normalizedName]; exists && spec.DefaultValue != "" {
				normalized[normalizedName] = spec.DefaultValue
			}
			continue
		}

		// Normalize value based on type
		if spec, exists := standardParams[normalizedName]; exists {
			switch spec.Type {
			case ParamTypeString:
				normalized[normalizedName] = strings.ToLower(value)
			case ParamTypeInt:
				// Parse and re-format to remove any leading zeros or spaces
				numVal, err := parseIntValue(value)
				if err != nil {
					normalized[normalizedName] = value // Keep original if can't parse
				} else {
					normalized[normalizedName] = fmt.Sprintf("%d", numVal)
				}
			case ParamTypeBool:
				// Normalize boolean values
				lowerVal := strings.ToLower(value)
				if lowerVal == "true" || lowerVal == "1" || lowerVal == "yes" {
					normalized[normalizedName] = "1"
				} else if lowerVal == "false" || lowerVal == "0" || lowerVal == "no" {
					normalized[normalizedName] = "0"
				} else {
					normalized[normalizedName] = value
				}
			default:
				normalized[normalizedName] = value
			}
		} else {
			// Unknown parameter, keep as-is
			normalized[normalizedName] = value
		}
	}

	return normalized, nil
}

// GetSupportedTranslations returns a list of source encoders that can be translated to the target.
func (t *ParameterTranslatorImpl) GetSupportedTranslations(targetEncoder EncoderFamily) []EncoderFamily {
	var sources []EncoderFamily

	for encoder := range t.mapping.FamilyMappings {
		if encoder == targetEncoder {
			continue
		}

		if t.mapping.CanTranslate(encoder, targetEncoder) {
			sources = append(sources, encoder)
			continue
		}

		// Check if codec-specific translation is available
		sourceFormat, _ := t.mapping.GetCodecFormat(encoder)
		targetFormat, _ := t.mapping.GetCodecFormat(targetEncoder)

		if sourceFormat == targetFormat {
			sources = append(sources, encoder)
		}
	}

	return sources
}

// GetAuditRecords returns all audit records from the last translation.
func (t *ParameterTranslatorImpl) GetAuditRecords() []TranslationAuditRecord {
	t.auditMu.Lock()
	defer t.auditMu.Unlock()
	return t.auditRecords
}

// GetMapping returns the underlying encoder mapping.
func (t *ParameterTranslatorImpl) GetMapping() *EncoderMapping {
	return t.mapping
}
