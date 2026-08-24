// Package encoder provides AV1 common parameter mapping structures and methods.
// It defines the standard SVT-AV1 and libaom-av1 parameter sets and provides unified
// translation rules for hardware-specific encoder implementations.
package encoder

import (
	"fmt"
	"strconv"
	"strings"
)

// AV1CommonMapping defines the AV1 encoder family's common parameter mapping relationships.
// It provides unified parameter translation rules and value conversion logic
// for all AV1 encoder implementations (libsvtav1, libaom-av1, av1_nvenc, av1_qsv, av1_vaapi).
type AV1CommonMapping struct {
	// StandardParams defines the standard AV1 parameter set (SVT-AV1 based).
	StandardParams map[string]ParameterSpec `json:"standard_params"`

	// NameTranslations maps SVT-AV1 parameter names to target encoder parameter names.
	// Key: target encoder family, Value: map of source param -> target param.
	NameTranslations map[EncoderFamily]map[string]string `json:"name_translations"`

	// ValueConversionRules maps parameter types to their conversion rules.
	// Key: parameter type (crf, quality, profile, level), Value: conversion rules.
	ValueConversionRules map[string]ValueConversionRule `json:"value_conversion_rules"`

	// RangeMappings defines valid value ranges for parameters.
	RangeMappings map[string]ValueRange `json:"range_mappings"`
}

// NewAV1CommonMapping creates a new AV1CommonMapping with all standard mappings initialized.
func NewAV1CommonMapping() *AV1CommonMapping {
	m := &AV1CommonMapping{
		StandardParams:       make(map[string]ParameterSpec),
		NameTranslations:     make(map[EncoderFamily]map[string]string),
		ValueConversionRules: make(map[string]ValueConversionRule),
		RangeMappings:        make(map[string]ValueRange),
	}

	// Initialize standard SVT-AV1 parameters
	initAV1StandardParams(m)

	// Initialize name translations for each AV1 encoder
	initAV1NameTranslations(m)

	// Initialize value conversion rules
	initAV1ValueConversionRules(m)

	// Initialize range mappings
	initAV1RangeMappings(m)

	return m
}

// initAV1StandardParams initializes the SVT-AV1 standard parameter set.
func initAV1StandardParams(m *AV1CommonMapping) {
	// Quality parameters (SVT-AV1 uses CRF-style quality)
	m.StandardParams["crf"] = ParameterSpec{
		Name:         "crf",
		Type:         ParamTypeInt,
		DefaultValue: "30",
		Description:  "Constant Rate Factor - quality level (0-63, lower is better quality)",
	}
	m.StandardParams["qp"] = ParameterSpec{
		Name:         "qp",
		Type:         ParamTypeInt,
		DefaultValue: "0",
		Description:  "Constant Quantization Parameter",
	}

	// Speed/preset (SVT-AV1 uses speed parameter, 0-13 where 13 is fastest)
	m.StandardParams["preset"] = ParameterSpec{
		Name:         "preset",
		Type:         ParamTypeString,
		DefaultValue: "medium",
		Description:  "Encoding preset - tradeoff between speed and compression",
	}
	m.StandardParams["speed"] = ParameterSpec{
		Name:         "speed",
		Type:         ParamTypeInt,
		DefaultValue: "7",
		Description:  "SVT-AV1 speed preset (0-13, higher is faster but lower quality)",
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

	// Profile parameter (AV1 profiles)
	m.StandardParams["profile"] = ParameterSpec{
		Name:         "profile",
		Type:         ParamTypeString,
		DefaultValue: "main",
		Description:  "AV1 profile (main, high, professional)",
	}

	// Level parameter
	m.StandardParams["level"] = ParameterSpec{
		Name:         "level",
		Type:         ParamTypeString,
		DefaultValue: "",
		Description:  "AV1 level",
	}

	// Tile parameters
	m.StandardParams["tile-columns"] = ParameterSpec{
		Name:         "tile-columns",
		Type:         ParamTypeInt,
		DefaultValue: "0",
		Description:  "Number of tile columns (log2)",
	}
	m.StandardParams["tile-rows"] = ParameterSpec{
		Name:         "tile-rows",
		Type:         ParamTypeInt,
		DefaultValue: "0",
		Description:  "Number of tile rows (log2)",
	}
}

// initAV1NameTranslations initializes parameter name translations for each encoder.
func initAV1NameTranslations(m *AV1CommonMapping) {
	// SVT-AV1 -> av1_nvenc name translations
	m.NameTranslations[EncoderAV1NVENC] = map[string]string{
		"crf":     "cq",
		"qp":      "qp",
		"preset":  "preset",
		"speed":   "preset",
		"b:v":     "b:v",
		"maxrate": "maxrate",
		"bufsize": "bufsize",
		"g":       "g",
		"profile": "profile",
		"level":   "level",
	}

	// SVT-AV1 -> av1_qsv name translations
	m.NameTranslations[EncoderAV1QSV] = map[string]string{
		"crf":     "global_quality",
		"qp":      "qp",
		"preset":  "preset",
		"speed":   "preset",
		"b:v":     "b:v",
		"maxrate": "maxrate",
		"bufsize": "bufsize",
		"g":       "g",
		"profile": "profile",
		"level":   "level",
	}

	// SVT-AV1 -> av1_vaapi name translations
	m.NameTranslations[EncoderAV1VAAPI] = map[string]string{
		"crf":     "quality",
		"qp":      "qp",
		"speed":   "speed",
		"b:v":     "b:v",
		"maxrate": "maxrate",
		"bufsize": "bufsize",
		"g":       "g",
		"profile": "profile",
		"level":   "level",
	}

	// SVT-AV1 -> libaom-av1 name translations
	m.NameTranslations[EncoderLibAOM] = map[string]string{
		"crf":          "crf",
		"qp":           "qp",
		"speed":        "cpu-used",
		"b:v":          "b:v",
		"maxrate":      "maxrate",
		"bufsize":      "bufsize",
		"g":            "g",
		"profile":      "profile",
		"level":        "level",
		"tile-columns": "tile-columns",
		"tile-rows":    "tile-rows",
	}
}

// initAV1ValueConversionRules initializes value conversion rules for different parameter types.
func initAV1ValueConversionRules(m *AV1CommonMapping) {
	// CRF to QP conversion (direct pass-through)
	m.ValueConversionRules["crf_to_qp"] = ValueConversionRule{
		SourceParam:     "crf",
		TargetParamType: "qp",
		RangeMappingKey: "crf_av1",
	}

	// CRF to NVENC CQ conversion (direct pass-through)
	m.ValueConversionRules["crf_to_cq"] = ValueConversionRule{
		SourceParam:     "crf",
		TargetParamType: "cq",
		RangeMappingKey: "crf_av1",
	}

	// CRF to QSV global_quality conversion (direct pass-through)
	m.ValueConversionRules["crf_to_global_quality"] = ValueConversionRule{
		SourceParam:     "crf",
		TargetParamType: "global_quality",
		RangeMappingKey: "crf_av1",
	}

	// CRF to VAAPI quality conversion (scale: 0-63 -> 0-100)
	m.ValueConversionRules["crf_to_quality_av1"] = ValueConversionRule{
		SourceParam:     "crf",
		TargetParamType: "quality",
		ScaleFunc:       av1CRFToVAAPIQuality,
		RangeMappingKey: "vaapi_quality_av1",
	}

	// Preset conversion to NVENC (SVT-AV1 uses speed 0-13)
	m.ValueConversionRules["preset_to_nvenc_av1"] = ValueConversionRule{
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

	// Speed to NVENC preset conversion
	m.ValueConversionRules["speed_to_nvenc_av1"] = ValueConversionRule{
		SourceParam:     "speed",
		TargetParamType: "preset",
		ValueMap: map[string]string{
			"0":  "p7",
			"1":  "p7",
			"2":  "p7",
			"3":  "p7",
			"4":  "p6",
			"5":  "p6",
			"6":  "p5",
			"7":  "p5",
			"8":  "p4",
			"9":  "p4",
			"10": "p3",
			"11": "p3",
			"12": "p2",
			"13": "p1",
		},
		RangeMappingKey: "speed_av1",
	}

	// Profile mapping for AV1
	m.ValueConversionRules["profile_mapping_av1"] = ValueConversionRule{
		SourceParam:     "profile",
		TargetParamType: "profile",
		ValueMap: map[string]string{
			"main":         "main",
			"high":         "high",
			"professional": "professional",
		},
		RangeMappingKey: "profile_av1",
	}

	// Level mapping for AV1
	m.ValueConversionRules["level_mapping_av1"] = ValueConversionRule{
		SourceParam:     "level",
		TargetParamType: "level",
		ValueMap: map[string]string{
			"2.0": "2.0",
			"2.1": "2.1",
			"2.2": "2.2",
			"2.3": "2.3",
			"3.0": "3.0",
			"3.1": "3.1",
			"3.2": "3.2",
			"3.3": "3.3",
			"4.0": "4.0",
			"4.1": "4.1",
			"4.2": "4.2",
			"4.3": "4.3",
			"5.0": "5.0",
			"5.1": "5.1",
			"5.2": "5.2",
			"5.3": "5.3",
			"6.0": "6.0",
			"6.1": "6.1",
			"6.2": "6.2",
			"6.3": "6.3",
			"7.0": "7.0",
			"7.1": "7.1",
			"7.2": "7.2",
			"7.3": "7.3",
		},
		RangeMappingKey: "level_av1",
	}
}

// initAV1RangeMappings initializes valid value ranges for parameters.
func initAV1RangeMappings(m *AV1CommonMapping) {
	// CRF range for AV1 (0-63)
	m.RangeMappings["crf_av1"] = ValueRange{
		Min:         0,
		Max:         63,
		Description: "AV1 CRF quality range: 0 (lossless) to 63 (worst quality)",
	}

	// QP range for AV1
	m.RangeMappings["qp"] = ValueRange{
		Min:         0,
		Max:         255,
		Description: "AV1 Quantization Parameter range",
	}

	// VAAPI quality range for AV1 (0-100)
	m.RangeMappings["vaapi_quality_av1"] = ValueRange{
		Min:         0,
		Max:         100,
		Description: "VAAPI AV1 quality range: higher is better",
	}

	// Speed range for SVT-AV1 (0-13)
	m.RangeMappings["speed_av1"] = ValueRange{
		Min:         0,
		Max:         13,
		Description: "SVT-AV1 speed: 0 (slowest/best quality) to 13 (fastest)",
	}

	// Preset valid values
	m.RangeMappings["preset"] = ValueRange{
		ValidValues: []string{
			"ultrafast", "superfast", "veryfast", "faster", "fast",
			"medium", "slow", "slower", "veryslow", "placebo",
		},
		Description: "Preset values in order of speed (fastest to slowest)",
	}

	// NVENC preset valid values
	m.RangeMappings["nvenc_preset"] = ValueRange{
		ValidValues: []string{"p1", "p2", "p3", "p4", "p5", "p6", "p7"},
		Description: "NVENC preset values: p1 (fastest) to p7 (slowest/best quality)",
	}

	// Profile valid values for AV1
	m.RangeMappings["profile_av1"] = ValueRange{
		ValidValues: []string{"main", "high", "professional"},
		Description: "AV1 profile values",
	}

	// Level valid values for AV1
	m.RangeMappings["level_av1"] = ValueRange{
		ValidValues: []string{
			"2.0", "2.1", "2.2", "2.3",
			"3.0", "3.1", "3.2", "3.3",
			"4.0", "4.1", "4.2", "4.3",
			"5.0", "5.1", "5.2", "5.3",
			"6.0", "6.1", "6.2", "6.3",
			"7.0", "7.1", "7.2", "7.3",
		},
		Description: "AV1 level values",
	}

	// Tile columns range (log2)
	m.RangeMappings["tile-columns"] = ValueRange{
		Min:         0,
		Max:         6,
		Description: "Number of tile columns (log2 value)",
	}

	// Tile rows range (log2)
	m.RangeMappings["tile-rows"] = ValueRange{
		Min:         0,
		Max:         6,
		Description: "Number of tile rows (log2 value)",
	}
}

// av1CRFToVAAPIQuality converts AV1 CRF value (0-63) to VAAPI quality (0-100).
// Higher CRF = lower quality, higher VAAPI quality = better quality.
// So we invert: CRF 0 -> quality 100, CRF 63 -> quality 0.
func av1CRFToVAAPIQuality(crf int) int {
	// Linear scale: quality = 100 - (crf * 100 / 63)
	quality := 100 - (crf * 100 / 63)
	if quality < 0 {
		return 0
	}
	if quality > 100 {
		return 100
	}
	return quality
}

// GetCommonParameterTranslation retrieves the target parameter name for a given source parameter
// when translating from SVT-AV1 to the specified target encoder.
// Returns an empty string if no translation exists.
func (m *AV1CommonMapping) GetCommonParameterTranslation(targetEncoder EncoderFamily, paramName string) string {
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
func (m *AV1CommonMapping) GetCommonValueConversion(sourceValue string, targetParamType string) (string, error) {
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
func (m *AV1CommonMapping) ValidateParameterRange(paramName string, value string) error {
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

// GetStandardParams returns all standard SVT-AV1 parameters.
func (m *AV1CommonMapping) GetStandardParams() map[string]ParameterSpec {
	return m.StandardParams
}

// GetNameTranslations returns the parameter name translations for a specific target encoder.
func (m *AV1CommonMapping) GetNameTranslations(targetEncoder EncoderFamily) map[string]string {
	return m.NameTranslations[targetEncoder]
}

// GetValueRange returns the valid value range for a parameter.
func (m *AV1CommonMapping) GetValueRange(paramName string) (*ValueRange, bool) {
	rangeSpec, exists := m.RangeMappings[paramName]
	if !exists {
		return nil, false
	}
	return &rangeSpec, true
}

// ConvertParameter performs a complete parameter conversion for a target encoder.
// It translates the parameter name and converts the value according to the rules.
// Returns the target parameter name, converted value, and any error.
func (m *AV1CommonMapping) ConvertParameter(targetEncoder EncoderFamily, paramName string, value string) (string, string, error) {
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
func (m *AV1CommonMapping) getConversionKey(paramName string, targetEncoder EncoderFamily) string {
	switch paramName {
	case "crf":
		switch targetEncoder {
		case EncoderAV1NVENC:
			return "crf_to_cq"
		case EncoderAV1QSV:
			return "crf_to_global_quality"
		case EncoderAV1VAAPI:
			return "crf_to_quality_av1"
		default:
			return "crf_to_cq"
		}
	case "preset":
		switch targetEncoder {
		case EncoderAV1NVENC:
			return "preset_to_nvenc_av1"
		default:
			return "preset_to_nvenc_av1"
		}
	case "speed":
		switch targetEncoder {
		case EncoderAV1NVENC:
			return "speed_to_nvenc_av1"
		default:
			return "speed_to_nvenc_av1"
		}
	case "profile":
		return "profile_mapping_av1"
	case "level":
		return "level_mapping_av1"
	default:
		return paramName
	}
}

// GetSupportedEncoders returns a list of AV1 encoder families supported by this mapping.
func (m *AV1CommonMapping) GetSupportedEncoders() []EncoderFamily {
	encoders := make([]EncoderFamily, 0, len(m.NameTranslations))
	for encoder := range m.NameTranslations {
		encoders = append(encoders, encoder)
	}
	return encoders
}

// IsParameterSupported checks if a parameter is supported for translation to a target encoder.
func (m *AV1CommonMapping) IsParameterSupported(targetEncoder EncoderFamily, paramName string) bool {
	translations, exists := m.NameTranslations[targetEncoder]
	if !exists {
		return false
	}
	targetParam, exists := translations[paramName]
	return exists && targetParam != ""
}
