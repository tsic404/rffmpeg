// Package encoder provides H.264 common parameter mapping structures and methods.
// It defines the standard libx264 parameter set and provides unified translation
// rules for hardware-specific encoder implementations.
package encoder

import (
	"fmt"
	"strconv"
	"strings"
)

// H264CommonMapping defines the H.264 encoder family's common parameter mapping relationships.
// It provides unified parameter translation rules and value conversion logic
// for all H.264 encoder implementations (libx264, h264_nvenc, h264_qsv, etc.).
type H264CommonMapping struct {
	// StandardParams defines the libx264 standard parameter set.
	StandardParams map[string]ParameterSpec `json:"standard_params"`

	// NameTranslations maps libx264 parameter names to target encoder parameter names.
	// Key: target encoder family, Value: map of source param -> target param.
	NameTranslations map[EncoderFamily]map[string]string `json:"name_translations"`

	// ValueConversionRules maps parameter types to their conversion rules.
	// Key: parameter type (crf, preset, tune, profile, level), Value: conversion rules.
	ValueConversionRules map[string]ValueConversionRule `json:"value_conversion_rules"`

	// RangeMappings defines valid value ranges for parameters.
	RangeMappings map[string]ValueRange `json:"range_mappings"`
}

// ParameterSpec defines the specification for a single parameter.
type ParameterSpec struct {
	// Name is the parameter name as used in libx264.
	Name string `json:"name"`

	// Type is the parameter value type (string, int, float, bool).
	Type ParamType `json:"type"`

	// DefaultValue is the default value for the parameter.
	DefaultValue string `json:"default_value,omitempty"`

	// Description provides documentation for the parameter.
	Description string `json:"description,omitempty"`

	// Required indicates whether this parameter must be present.
	Required bool `json:"required,omitempty"`
}

// ParamType represents the type of a parameter value.
type ParamType string

const (
	ParamTypeString ParamType = "string"
	ParamTypeInt    ParamType = "int"
	ParamTypeFloat  ParamType = "float"
	ParamTypeBool   ParamType = "bool"
)

// ValueConversionRule defines rules for converting values between encoders.
type ValueConversionRule struct {
	// SourceParam is the source parameter name.
	SourceParam string `json:"source_param"`

	// TargetParamType is the type of parameter being converted (crf, preset, tune, etc.).
	TargetParamType string `json:"target_param_type"`

	// ValueMap provides direct value mappings for discrete parameters.
	// Key: source value, Value: target value.
	ValueMap map[string]string `json:"value_map,omitempty"`

	// ScaleFunc is an optional function for scaling numeric values.
	// Used for parameters like CRF that may need scale adjustment.
	ScaleFunc ScaleFunction `json:"-"`

	// RangeMappingKey is the key for looking up range validation rules.
	RangeMappingKey string `json:"range_mapping_key,omitempty"`
}

// ScaleFunction is a function that scales a numeric value.
type ScaleFunction func(value int) int

// ValueRange defines the valid range for a parameter value.
type ValueRange struct {
	// Min is the minimum valid value (inclusive).
	Min int `json:"min"`

	// Max is the maximum valid value (inclusive).
	Max int `json:"max"`

	// ValidValues is a list of valid discrete values (for non-numeric parameters).
	ValidValues []string `json:"valid_values,omitempty"`

	// Description provides documentation for the range.
	Description string `json:"description,omitempty"`
}

// NewH264CommonMapping creates a new H264CommonMapping with all standard mappings initialized.
func NewH264CommonMapping() *H264CommonMapping {
	m := &H264CommonMapping{
		StandardParams:       make(map[string]ParameterSpec),
		NameTranslations:     make(map[EncoderFamily]map[string]string),
		ValueConversionRules: make(map[string]ValueConversionRule),
		RangeMappings:        make(map[string]ValueRange),
	}

	// Initialize standard libx264 parameters
	initStandardParams(m)

	// Initialize name translations for each H.264 encoder
	initNameTranslations(m)

	// Initialize value conversion rules
	initValueConversionRules(m)

	// Initialize range mappings
	initRangeMappings(m)

	return m
}

// initStandardParams initializes the libx264 standard parameter set.
func initStandardParams(m *H264CommonMapping) {
	// Quality parameters
	m.StandardParams["crf"] = ParameterSpec{
		Name:         "crf",
		Type:         ParamTypeInt,
		DefaultValue: "23",
		Description:  "Constant Rate Factor - quality level (0-51, lower is better quality)",
	}
	m.StandardParams["qp"] = ParameterSpec{
		Name:         "qp",
		Type:         ParamTypeInt,
		DefaultValue: "0",
		Description:  "Constant Quantization Parameter (0-51)",
	}

	// Preset parameter
	m.StandardParams["preset"] = ParameterSpec{
		Name:         "preset",
		Type:         ParamTypeString,
		DefaultValue: "medium",
		Description:  "Encoding preset - tradeoff between speed and compression",
	}

	// Tune parameter
	m.StandardParams["tune"] = ParameterSpec{
		Name:         "tune",
		Type:         ParamTypeString,
		DefaultValue: "",
		Description:  "Tune the encoder for specific content types",
	}

	// Profile parameter
	m.StandardParams["profile"] = ParameterSpec{
		Name:         "profile",
		Type:         ParamTypeString,
		DefaultValue: "high",
		Description:  "H.264 profile (baseline, main, high, high10, high422, high444)",
	}

	// Level parameter
	m.StandardParams["level"] = ParameterSpec{
		Name:         "level",
		Type:         ParamTypeString,
		DefaultValue: "",
		Description:  "H.264 level (e.g., 3.0, 3.1, 4.0, 4.1, 5.0, 5.1)",
	}

	// Bitrate parameters
	m.StandardParams["b:v"] = ParameterSpec{
		Name:         "b:v",
		Type:         ParamTypeString,
		DefaultValue: "",
		Description:  "Target bitrate",
	}
	m.StandardParams["maxrate"] = ParameterSpec{
		Name:         "maxrate",
		Type:         ParamTypeString,
		DefaultValue: "",
		Description:  "Maximum bitrate",
	}
	m.StandardParams["bufsize"] = ParameterSpec{
		Name:         "bufsize",
		Type:         ParamTypeString,
		DefaultValue: "",
		Description:  "Buffer size",
	}

	// GOP parameters
	m.StandardParams["g"] = ParameterSpec{
		Name:         "g",
		Type:         ParamTypeInt,
		DefaultValue: "",
		Description:  "GOP size (keyframe interval)",
	}
	m.StandardParams["keyint_min"] = ParameterSpec{
		Name:         "keyint_min",
		Type:         ParamTypeInt,
		DefaultValue: "",
		Description:  "Minimum keyframe interval",
	}

	// B-frame parameters
	m.StandardParams["bf"] = ParameterSpec{
		Name:         "bf",
		Type:         ParamTypeInt,
		DefaultValue: "3",
		Description:  "Maximum number of B-frames",
	}
	m.StandardParams["b_strategy"] = ParameterSpec{
		Name:         "b_strategy",
		Type:         ParamTypeInt,
		DefaultValue: "",
		Description:  "B-frame strategy",
	}

	// Reference frames
	m.StandardParams["refs"] = ParameterSpec{
		Name:         "refs",
		Type:         ParamTypeInt,
		DefaultValue: "3",
		Description:  "Number of reference frames",
	}
}

// initNameTranslations initializes parameter name translations for each encoder.
func initNameTranslations(m *H264CommonMapping) {
	// libx264 -> h264_nvenc name translations
	m.NameTranslations[EncoderH264NVENC] = map[string]string{
		"crf":     "cq",
		"qp":      "qp",
		"preset":  "preset",
		"tune":    "tune",
		"profile": "profile",
		"level":   "level",
		"b:v":     "b:v",
		"maxrate": "maxrate",
		"bufsize": "bufsize",
		"g":       "g",
		"bf":      "bf",
		"refs":    "refs",
	}

	// libx264 -> h264_qsv name translations
	m.NameTranslations[EncoderH264QSV] = map[string]string{
		"crf":        "global_quality",
		"qp":         "qp",
		"preset":     "preset",
		"cq":         "global_quality",
		"tune":       "",
		"profile":    "profile",
		"level":      "level",
		"b:v":        "b:v",
		"maxrate":    "maxrate",
		"bufsize":    "bufsize",
		"g":          "g",
		"bf":         "bf",
		"refs":       "refs",
		"look_ahead": "look_ahead",
	}

	// libx264 -> h264_vaapi name translations
	m.NameTranslations[EncoderH264VAAPI] = map[string]string{
		"crf":     "quality",
		"qp":      "qp",
		"preset":  "",
		"cq":      "quality",
		"tune":    "",
		"profile": "profile",
		"level":   "level",
		"b:v":     "b:v",
		"maxrate": "maxrate",
		"bufsize": "bufsize",
		"g":       "g",
		"bf":      "bf",
		"refs":    "refs",
	}

	// libx264 -> h264_amf name translations
	m.NameTranslations[EncoderH264AMF] = map[string]string{
		"crf":     "qp_i",
		"qp":      "qp",
		"preset":  "quality",
		"tune":    "",
		"profile": "profile",
		"level":   "level",
		"b:v":     "b:v",
		"maxrate": "maxrate",
		"bufsize": "bufsize",
		"g":       "g",
		"bf":      "bf",
		"refs":    "refs",
	}

	// libx264 -> h264_videotoolbox name translations
	m.NameTranslations[EncoderH264VT] = map[string]string{
		"crf":     "q:v",
		"qp":      "q:v",
		"preset":  "",
		"tune":    "",
		"profile": "profile",
		"level":   "level",
		"b:v":     "b:v",
		"maxrate": "maxrate",
		"bufsize": "bufsize",
		"g":       "g",
		"bf":      "bf",
		"refs":    "refs",
	}
}

// initValueConversionRules initializes value conversion rules for different parameter types.
func initValueConversionRules(m *H264CommonMapping) {
	// CRF to QP conversion (direct pass-through, same range)
	m.ValueConversionRules["crf_to_qp"] = ValueConversionRule{
		SourceParam:     "crf",
		TargetParamType: "qp",
		RangeMappingKey: "crf",
	}

	// CRF to NVENC CQ conversion (direct pass-through)
	m.ValueConversionRules["crf_to_cq"] = ValueConversionRule{
		SourceParam:     "crf",
		TargetParamType: "cq",
		RangeMappingKey: "crf",
	}

	// CRF to QSV global_quality conversion (direct pass-through)
	m.ValueConversionRules["crf_to_global_quality"] = ValueConversionRule{
		SourceParam:     "crf",
		TargetParamType: "global_quality",
		RangeMappingKey: "crf",
	}

	// CRF to VAAPI quality conversion (scale: 0-51 -> 0-100 approximately)
	m.ValueConversionRules["crf_to_quality"] = ValueConversionRule{
		SourceParam:     "crf",
		TargetParamType: "quality",
		ScaleFunc:       crfToVAAPIQuality,
		RangeMappingKey: "vaapi_quality",
	}

	// Preset conversion to NVENC
	m.ValueConversionRules["preset_to_nvenc"] = ValueConversionRule{
		SourceParam:     "preset",
		TargetParamType: "preset",
		ValueMap: map[string]string{
			"ultrafast": "p1",
			"superfast": "p2",
			"veryfast":  "p3",
			"faster":    "p4",
			"fast":      "p5",
			"medium":    "p6",
			"slow":      "p7",
			"slower":    "p7",
			"veryslow":  "p7",
			"placebo":   "p7",
		},
		RangeMappingKey: "preset",
	}

	// Preset conversion to QSV
	m.ValueConversionRules["preset_to_qsv"] = ValueConversionRule{
		SourceParam:     "preset",
		TargetParamType: "preset",
		ValueMap: map[string]string{
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
		},
		RangeMappingKey: "preset",
	}

	// Preset conversion to AMF
	m.ValueConversionRules["preset_to_amf"] = ValueConversionRule{
		SourceParam:     "preset",
		TargetParamType: "quality",
		ValueMap: map[string]string{
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
		},
		RangeMappingKey: "preset",
	}

	// Tune value translation
	m.ValueConversionRules["tune_to_nvenc"] = ValueConversionRule{
		SourceParam:     "tune",
		TargetParamType: "tune",
		ValueMap: map[string]string{
			"film":        "film",
			"animation":   "animation",
			"grain":       "grain",
			"stillimage":  "stillimage",
			"fastdecode":  "fastdecode",
			"zerolatency": "ull",
			"psnr":        "",
			"ssim":        "",
		},
		RangeMappingKey: "tune",
	}

	// Profile mapping
	m.ValueConversionRules["profile_mapping"] = ValueConversionRule{
		SourceParam:     "profile",
		TargetParamType: "profile",
		ValueMap: map[string]string{
			"baseline":    "baseline",
			"main":        "main",
			"high":        "high",
			"high10":      "high10",
			"high422":     "high422",
			"high444":     "high444",
			"constrained": "constrained_baseline",
		},
		RangeMappingKey: "profile",
	}

	// Level mapping
	m.ValueConversionRules["level_mapping"] = ValueConversionRule{
		SourceParam:     "level",
		TargetParamType: "level",
		ValueMap: map[string]string{
			"1":   "1",
			"1b":  "1b",
			"1.1": "1.1",
			"1.2": "1.2",
			"1.3": "1.3",
			"2":   "2",
			"2.1": "2.1",
			"2.2": "2.2",
			"3":   "3",
			"3.0": "3",
			"3.1": "3.1",
			"3.2": "3.2",
			"4":   "4",
			"4.0": "4",
			"4.1": "4.1",
			"4.2": "4.2",
			"5":   "5",
			"5.0": "5",
			"5.1": "5.1",
			"5.2": "5.2",
		},
		RangeMappingKey: "level",
	}
}

// initRangeMappings initializes valid value ranges for parameters.
func initRangeMappings(m *H264CommonMapping) {
	// CRF range (0-51)
	m.RangeMappings["crf"] = ValueRange{
		Min:         0,
		Max:         51,
		Description: "CRF quality range: 0 (lossless) to 51 (worst quality)",
	}

	// QP range (0-51)
	m.RangeMappings["qp"] = ValueRange{
		Min:         0,
		Max:         51,
		Description: "Quantization Parameter range: 0 (lossless) to 51",
	}

	// VAAPI quality range (0-100)
	m.RangeMappings["vaapi_quality"] = ValueRange{
		Min:         0,
		Max:         100,
		Description: "VAAPI quality range: higher is better",
	}

	// Preset valid values
	m.RangeMappings["preset"] = ValueRange{
		ValidValues: []string{
			"ultrafast", "superfast", "veryfast", "faster", "fast",
			"medium", "slow", "slower", "veryslow", "placebo",
		},
		Description: "libx264 preset values in order of speed (fastest to slowest)",
	}

	// NVENC preset valid values
	m.RangeMappings["nvenc_preset"] = ValueRange{
		ValidValues: []string{"p1", "p2", "p3", "p4", "p5", "p6", "p7"},
		Description: "NVENC preset values: p1 (fastest) to p7 (slowest/best quality)",
	}

	// QSV preset valid values
	m.RangeMappings["qsv_preset"] = ValueRange{
		ValidValues: []string{"veryfast", "faster", "fast", "medium", "slow", "slower", "veryslow"},
		Description: "QSV preset values",
	}

	// AMF quality valid values
	m.RangeMappings["amf_quality"] = ValueRange{
		ValidValues: []string{"speed", "balanced", "quality"},
		Description: "AMF quality preset values",
	}

	// Tune valid values
	m.RangeMappings["tune"] = ValueRange{
		ValidValues: []string{
			"film", "animation", "grain", "stillimage",
			"fastdecode", "zerolatency", "psnr", "ssim",
		},
		Description: "libx264 tune values for specific content types",
	}

	// Profile valid values
	m.RangeMappings["profile"] = ValueRange{
		ValidValues: []string{
			"baseline", "main", "high", "high10", "high422", "high444",
		},
		Description: "H.264 profile levels",
	}

	// Level valid values
	m.RangeMappings["level"] = ValueRange{
		ValidValues: []string{
			"1", "1b", "1.1", "1.2", "1.3",
			"2", "2.1", "2.2",
			"3", "3.0", "3.1", "3.2",
			"4", "4.0", "4.1", "4.2",
			"5", "5.0", "5.1", "5.2",
		},
		Description: "H.264 level values",
	}

	// B-frames range
	m.RangeMappings["bf"] = ValueRange{
		Min:         0,
		Max:         16,
		Description: "Maximum number of B-frames (0-16)",
	}

	// Reference frames range
	m.RangeMappings["refs"] = ValueRange{
		Min:         1,
		Max:         16,
		Description: "Number of reference frames (1-16)",
	}
}

// crfToVAAPIQuality converts CRF value (0-51) to VAAPI quality (0-100).
// Higher CRF = lower quality, higher VAAPI quality = better quality.
// So we invert: CRF 0 -> quality 100, CRF 51 -> quality 0.
func crfToVAAPIQuality(crf int) int {
	// Linear scale: quality = 100 - (crf * 100 / 51)
	quality := 100 - (crf * 100 / 51)
	if quality < 0 {
		return 0
	}
	if quality > 100 {
		return 100
	}
	return quality
}

// GetCommonParameterTranslation retrieves the target parameter name for a given source parameter
// when translating from libx264 to the specified target encoder.
// Returns an empty string if no translation exists.
func (m *H264CommonMapping) GetCommonParameterTranslation(targetEncoder EncoderFamily, paramName string) string {
	translations, exists := m.NameTranslations[targetEncoder]
	if !exists {
		return ""
	}

	targetParam, exists := translations[paramName]
	if !exists {
		return ""
	}

	return targetParam
}

// GetCommonValueConversion converts a source value to the target parameter type format.
// It uses the conversion rules defined in ValueConversionRules.
// Returns the converted value or an error if conversion fails.
func (m *H264CommonMapping) GetCommonValueConversion(sourceValue string, targetParamType string) (string, error) {
	rule, exists := m.ValueConversionRules[targetParamType]
	if !exists {
		// No conversion rule, return value as-is
		return sourceValue, nil
	}

	// Check if there's a direct value mapping
	if rule.ValueMap != nil {
		if targetValue, ok := rule.ValueMap[sourceValue]; ok {
			return targetValue, nil
		}
		// If not found in map, return original value
		return sourceValue, nil
	}

	// Check if there's a scale function for numeric values
	if rule.ScaleFunc != nil {
		numValue, err := strconv.Atoi(sourceValue)
		if err != nil {
			return "", fmt.Errorf("cannot convert non-numeric value '%s' for scaling", sourceValue)
		}
		scaledValue := rule.ScaleFunc(numValue)
		return strconv.Itoa(scaledValue), nil
	}

	// No conversion needed
	return sourceValue, nil
}

// ValidateParameterRange validates if a parameter value is within the valid range.
// Returns an error if the value is out of range.
func (m *H264CommonMapping) ValidateParameterRange(paramName string, value string) error {
	// Normalize parameter name
	paramName = strings.ToLower(paramName)

	rangeSpec, exists := m.RangeMappings[paramName]
	if !exists {
		// No range validation defined for this parameter
		return nil
	}

	// If there are valid discrete values, check against them
	if len(rangeSpec.ValidValues) > 0 {
		for _, valid := range rangeSpec.ValidValues {
			if strings.EqualFold(value, valid) {
				return nil
			}
		}
		return fmt.Errorf("parameter '%s' value '%s' is not valid. Valid values: %v",
			paramName, value, rangeSpec.ValidValues)
	}

	// For numeric ranges, parse and validate
	numValue, err := strconv.Atoi(value)
	if err != nil {
		return fmt.Errorf("parameter '%s' expects numeric value, got '%s'", paramName, value)
	}

	if numValue < rangeSpec.Min || numValue > rangeSpec.Max {
		return fmt.Errorf("parameter '%s' value %d is out of range [%d, %d]",
			paramName, numValue, rangeSpec.Min, rangeSpec.Max)
	}

	return nil
}

// GetStandardParams returns all standard libx264 parameters.
func (m *H264CommonMapping) GetStandardParams() map[string]ParameterSpec {
	return m.StandardParams
}

// GetNameTranslations returns the parameter name translations for a specific target encoder.
func (m *H264CommonMapping) GetNameTranslations(targetEncoder EncoderFamily) map[string]string {
	return m.NameTranslations[targetEncoder]
}

// GetValueRange returns the valid value range for a parameter.
func (m *H264CommonMapping) GetValueRange(paramName string) (*ValueRange, bool) {
	rangeSpec, exists := m.RangeMappings[paramName]
	if !exists {
		return nil, false
	}
	return &rangeSpec, true
}

// ConvertParameter performs a complete parameter conversion for a target encoder.
// It translates the parameter name and converts the value according to the rules.
// Returns the target parameter name, converted value, and any error.
func (m *H264CommonMapping) ConvertParameter(targetEncoder EncoderFamily, paramName string, value string) (string, string, error) {
	// First, translate the parameter name
	targetParam := m.GetCommonParameterTranslation(targetEncoder, paramName)
	if targetParam == "" {
		// Parameter not supported for this encoder
		return "", "", fmt.Errorf("parameter '%s' is not supported for encoder '%s'", paramName, targetEncoder)
	}

	// Determine the conversion rule key based on parameter name
	conversionKey := m.getConversionKey(paramName, targetEncoder)

	// Convert the value
	convertedValue, err := m.GetCommonValueConversion(value, conversionKey)
	if err != nil {
		return "", "", fmt.Errorf("value conversion failed: %w", err)
	}

	return targetParam, convertedValue, nil
}

// getConversionKey returns the appropriate conversion rule key for a parameter and encoder.
func (m *H264CommonMapping) getConversionKey(paramName string, targetEncoder EncoderFamily) string {
	switch paramName {
	case "crf":
		switch targetEncoder {
		case EncoderH264NVENC:
			return "crf_to_cq"
		case EncoderH264QSV:
			return "crf_to_global_quality"
		case EncoderH264VAAPI:
			return "crf_to_quality"
		case EncoderH264AMF:
			return "crf_to_qp"
		default:
			return "crf_to_cq"
		}
	case "preset":
		switch targetEncoder {
		case EncoderH264NVENC:
			return "preset_to_nvenc"
		case EncoderH264QSV:
			return "preset_to_qsv"
		case EncoderH264AMF:
			return "preset_to_amf"
		default:
			return "preset_to_nvenc"
		}
	case "tune":
		return "tune_to_nvenc"
	case "profile":
		return "profile_mapping"
	case "level":
		return "level_mapping"
	default:
		return paramName
	}
}

// GetSupportedEncoders returns a list of H.264 encoder families supported by this mapping.
func (m *H264CommonMapping) GetSupportedEncoders() []EncoderFamily {
	encoders := make([]EncoderFamily, 0, len(m.NameTranslations))
	for encoder := range m.NameTranslations {
		encoders = append(encoders, encoder)
	}
	return encoders
}

// IsParameterSupported checks if a parameter is supported for translation to a target encoder.
func (m *H264CommonMapping) IsParameterSupported(targetEncoder EncoderFamily, paramName string) bool {
	translations, exists := m.NameTranslations[targetEncoder]
	if !exists {
		return false
	}
	targetParam, exists := translations[paramName]
	return exists && targetParam != ""
}
