// Package encoder provides HEVC common parameter mapping structures and methods.
// It defines the standard libx265 parameter set and provides unified translation
// rules for hardware-specific encoder implementations.
package encoder

import (
	"fmt"
	"strconv"
	"strings"
)

// HEVCCommonMapping defines the HEVC encoder family's common parameter mapping relationships.
// It provides unified parameter translation rules and value conversion logic
// for all HEVC encoder implementations (libx265, hevc_nvenc, hevc_qsv, etc.).
type HEVCCommonMapping struct {
	// StandardParams defines the libx265 standard parameter set.
	StandardParams map[string]ParameterSpec `json:"standard_params"`

	// NameTranslations maps libx265 parameter names to target encoder parameter names.
	// Key: target encoder family, Value: map of source param -> target param.
	NameTranslations map[EncoderFamily]map[string]string `json:"name_translations"`

	// ValueConversionRules maps parameter types to their conversion rules.
	// Key: parameter type (crf, preset, tune, profile, level), Value: conversion rules.
	ValueConversionRules map[string]ValueConversionRule `json:"value_conversion_rules"`

	// RangeMappings defines valid value ranges for parameters.
	RangeMappings map[string]ValueRange `json:"range_mappings"`
}

// NewHEVCCommonMapping creates a new HEVCCommonMapping with all standard mappings initialized.
func NewHEVCCommonMapping() *HEVCCommonMapping {
	m := &HEVCCommonMapping{
		StandardParams:       make(map[string]ParameterSpec),
		NameTranslations:     make(map[EncoderFamily]map[string]string),
		ValueConversionRules: make(map[string]ValueConversionRule),
		RangeMappings:        make(map[string]ValueRange),
	}

	// Initialize standard libx265 parameters
	initHEVCStandardParams(m)

	// Initialize name translations for each HEVC encoder
	initHEVCNameTranslations(m)

	// Initialize value conversion rules
	initHEVCValueConversionRules(m)

	// Initialize range mappings
	initHEVCRangeMappings(m)

	return m
}

// initHEVCStandardParams initializes the libx265 standard parameter set.
func initHEVCStandardParams(m *HEVCCommonMapping) {
	// Quality parameters
	m.StandardParams["crf"] = ParameterSpec{
		Name:         "crf",
		Type:         ParamTypeInt,
		DefaultValue: "28",
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
		DefaultValue: "main",
		Description:  "HEVC profile (main, main10, mainstillpicture)",
	}

	// Level parameter
	m.StandardParams["level"] = ParameterSpec{
		Name:         "level",
		Type:         ParamTypeString,
		DefaultValue: "",
		Description:  "HEVC level (e.g., 3.0, 3.1, 4.0, 4.1, 5.0, 5.1, 5.2)",
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
		DefaultValue: "4",
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

// initHEVCNameTranslations initializes parameter name translations for each encoder.
func initHEVCNameTranslations(m *HEVCCommonMapping) {
	// libx265 -> hevc_nvenc name translations
	m.NameTranslations[EncoderHEVCNVENC] = map[string]string{
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

	// libx265 -> hevc_qsv name translations
	m.NameTranslations[EncoderHEVCQSV] = map[string]string{
		"crf":        "global_quality",
		"qp":         "qp",
		"preset":     "preset",
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

	// libx265 -> hevc_vaapi name translations
	m.NameTranslations[EncoderHEVCVAAPI] = map[string]string{
		"crf":     "quality",
		"qp":      "qp",
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

	// libx265 -> hevc_amf name translations
	m.NameTranslations[EncoderHEVCAMF] = map[string]string{
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

	// libx265 -> hevc_videotoolbox name translations
	m.NameTranslations[EncoderHEVCVT] = map[string]string{
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

// initHEVCValueConversionRules initializes value conversion rules for different parameter types.
func initHEVCValueConversionRules(m *HEVCCommonMapping) {
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

	// Preset conversion to NVENC (same as x264 since x265 uses same preset names)
	m.ValueConversionRules["preset_to_nvenc_hevc"] = ValueConversionRule{
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
	m.ValueConversionRules["preset_to_qsv_hevc"] = ValueConversionRule{
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
	m.ValueConversionRules["preset_to_amf_hevc"] = ValueConversionRule{
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
	m.ValueConversionRules["tune_to_nvenc_hevc"] = ValueConversionRule{
		SourceParam:     "tune",
		TargetParamType: "tune",
		ValueMap: map[string]string{
			"psnr":        "",
			"ssim":        "",
			"grain":       "grain",
			"zerolatency": "ull",
			"fastdecode":  "fastdecode",
			"animation":   "animation",
		},
		RangeMappingKey: "tune_hevc",
	}

	// Profile mapping for HEVC
	m.ValueConversionRules["profile_mapping_hevc"] = ValueConversionRule{
		SourceParam:     "profile",
		TargetParamType: "profile",
		ValueMap: map[string]string{
			"main":             "main",
			"main10":           "main10",
			"mainstillpicture": "mainstillpicture",
			"rext":             "rext",
		},
		RangeMappingKey: "profile_hevc",
	}

	// Level mapping for HEVC
	m.ValueConversionRules["level_mapping_hevc"] = ValueConversionRule{
		SourceParam:     "level",
		TargetParamType: "level",
		ValueMap: map[string]string{
			"1":   "1",
			"2":   "2",
			"2.1": "2.1",
			"3":   "3",
			"3.0": "3",
			"3.1": "3.1",
			"4":   "4",
			"4.0": "4",
			"4.1": "4.1",
			"5":   "5",
			"5.0": "5",
			"5.1": "5.1",
			"5.2": "5.2",
			"6":   "6",
			"6.0": "6",
			"6.1": "6.1",
			"6.2": "6.2",
		},
		RangeMappingKey: "level_hevc",
	}
}

// initHEVCRangeMappings initializes valid value ranges for parameters.
func initHEVCRangeMappings(m *HEVCCommonMapping) {
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

	// Preset valid values (libx265 uses same as libx264)
	m.RangeMappings["preset"] = ValueRange{
		ValidValues: []string{
			"ultrafast", "superfast", "veryfast", "faster", "fast",
			"medium", "slow", "slower", "veryslow", "placebo",
		},
		Description: "libx265 preset values in order of speed (fastest to slowest)",
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

	// Tune valid values for HEVC
	m.RangeMappings["tune_hevc"] = ValueRange{
		ValidValues: []string{
			"psnr", "ssim", "grain", "zerolatency", "fastdecode", "animation",
		},
		Description: "libx265 tune values for specific content types",
	}

	// Profile valid values for HEVC
	m.RangeMappings["profile_hevc"] = ValueRange{
		ValidValues: []string{
			"main", "main10", "mainstillpicture", "rext",
		},
		Description: "HEVC profile levels",
	}

	// Level valid values for HEVC
	m.RangeMappings["level_hevc"] = ValueRange{
		ValidValues: []string{
			"1", "2", "2.1",
			"3", "3.0", "3.1",
			"4", "4.0", "4.1",
			"5", "5.0", "5.1", "5.2",
			"6", "6.0", "6.1", "6.2",
		},
		Description: "HEVC level values",
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

// GetCommonParameterTranslation retrieves the target parameter name for a given source parameter
// when translating from libx265 to the specified target encoder.
// Returns an empty string if no translation exists.
func (m *HEVCCommonMapping) GetCommonParameterTranslation(targetEncoder EncoderFamily, paramName string) string {
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
func (m *HEVCCommonMapping) GetCommonValueConversion(sourceValue string, targetParamType string) (string, error) {
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
func (m *HEVCCommonMapping) ValidateParameterRange(paramName string, value string) error {
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

// GetStandardParams returns all standard libx265 parameters.
func (m *HEVCCommonMapping) GetStandardParams() map[string]ParameterSpec {
	return m.StandardParams
}

// GetNameTranslations returns the parameter name translations for a specific target encoder.
func (m *HEVCCommonMapping) GetNameTranslations(targetEncoder EncoderFamily) map[string]string {
	return m.NameTranslations[targetEncoder]
}

// GetValueRange returns the valid value range for a parameter.
func (m *HEVCCommonMapping) GetValueRange(paramName string) (*ValueRange, bool) {
	rangeSpec, exists := m.RangeMappings[paramName]
	if !exists {
		return nil, false
	}
	return &rangeSpec, true
}

// ConvertParameter performs a complete parameter conversion for a target encoder.
// It translates the parameter name and converts the value according to the rules.
// Returns the target parameter name, converted value, and any error.
func (m *HEVCCommonMapping) ConvertParameter(targetEncoder EncoderFamily, paramName string, value string) (string, string, error) {
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
func (m *HEVCCommonMapping) getConversionKey(paramName string, targetEncoder EncoderFamily) string {
	switch paramName {
	case "crf":
		switch targetEncoder {
		case EncoderHEVCNVENC:
			return "crf_to_cq"
		case EncoderHEVCQSV:
			return "crf_to_global_quality"
		case EncoderHEVCVAAPI:
			return "crf_to_quality"
		case EncoderHEVCAMF:
			return "crf_to_qp"
		default:
			return "crf_to_cq"
		}
	case "preset":
		switch targetEncoder {
		case EncoderHEVCNVENC:
			return "preset_to_nvenc_hevc"
		case EncoderHEVCQSV:
			return "preset_to_qsv_hevc"
		case EncoderHEVCAMF:
			return "preset_to_amf_hevc"
		default:
			return "preset_to_nvenc_hevc"
		}
	case "tune":
		return "tune_to_nvenc_hevc"
	case "profile":
		return "profile_mapping_hevc"
	case "level":
		return "level_mapping_hevc"
	default:
		return paramName
	}
}

// GetSupportedEncoders returns a list of HEVC encoder families supported by this mapping.
func (m *HEVCCommonMapping) GetSupportedEncoders() []EncoderFamily {
	encoders := make([]EncoderFamily, 0, len(m.NameTranslations))
	for encoder := range m.NameTranslations {
		encoders = append(encoders, encoder)
	}
	return encoders
}

// IsParameterSupported checks if a parameter is supported for translation to a target encoder.
func (m *HEVCCommonMapping) IsParameterSupported(targetEncoder EncoderFamily, paramName string) bool {
	translations, exists := m.NameTranslations[targetEncoder]
	if !exists {
		return false
	}
	targetParam, exists := translations[paramName]
	return exists && targetParam != ""
}
