// Package encoder provides VP9 common parameter mapping structures and methods.
// It defines the standard libvpx-vp9 parameter set and provides unified translation
// rules for hardware-specific encoder implementations.
package encoder

import (
	"fmt"
	"strconv"
	"strings"
)

// VP9CommonMapping defines the VP9 encoder family's common parameter mapping relationships.
// It provides unified parameter translation rules and value conversion logic
// for all VP9 encoder implementations (libvpx-vp9, vp9_nvenc, vp9_qsv, vp9_vaapi).
type VP9CommonMapping struct {
	// StandardParams defines the libvpx-vp9 standard parameter set.
	StandardParams map[string]ParameterSpec `json:"standard_params"`

	// NameTranslations maps libvpx-vp9 parameter names to target encoder parameter names.
	// Key: target encoder family, Value: map of source param -> target param.
	NameTranslations map[EncoderFamily]map[string]string `json:"name_translations"`

	// ValueConversionRules maps parameter types to their conversion rules.
	// Key: parameter type (crf, quality, profile, level), Value: conversion rules.
	ValueConversionRules map[string]ValueConversionRule `json:"value_conversion_rules"`

	// RangeMappings defines valid value ranges for parameters.
	RangeMappings map[string]ValueRange `json:"range_mappings"`
}

// NewVP9CommonMapping creates a new VP9CommonMapping with all standard mappings initialized.
func NewVP9CommonMapping() *VP9CommonMapping {
	m := &VP9CommonMapping{
		StandardParams:       make(map[string]ParameterSpec),
		NameTranslations:     make(map[EncoderFamily]map[string]string),
		ValueConversionRules: make(map[string]ValueConversionRule),
		RangeMappings:        make(map[string]ValueRange),
	}

	// Initialize standard libvpx-vp9 parameters
	initVP9StandardParams(m)

	// Initialize name translations for each VP9 encoder
	initVP9NameTranslations(m)

	// Initialize value conversion rules
	initVP9ValueConversionRules(m)

	// Initialize range mappings
	initVP9RangeMappings(m)

	return m
}

// initVP9StandardParams initializes the libvpx-vp9 standard parameter set.
func initVP9StandardParams(m *VP9CommonMapping) {
	// Quality parameters (VP9 uses CRF differently)
	m.StandardParams["crf"] = ParameterSpec{
		Name:         "crf",
		Type:         ParamTypeInt,
		DefaultValue: "31",
		Description:  "Constant Rate Factor - quality level (0-63, lower is better quality)",
	}
	m.StandardParams["qp"] = ParameterSpec{
		Name:         "qp",
		Type:         ParamTypeInt,
		DefaultValue: "0",
		Description:  "Constant Quantization Parameter (0-63)",
	}

	// Quality/speed tradeoff
	m.StandardParams["cpu-used"] = ParameterSpec{
		Name:         "cpu-used",
		Type:         ParamTypeInt,
		DefaultValue: "1",
		Description:  "CPU usage/speed tradeoff (0-5, higher is faster but lower quality)",
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

	// Profile parameter (VP9 profiles)
	m.StandardParams["profile"] = ParameterSpec{
		Name:         "profile",
		Type:         ParamTypeString,
		DefaultValue: "0",
		Description:  "VP9 profile (0, 1, 2, 3)",
	}

	// Row multi-threading
	m.StandardParams["row-mt"] = ParameterSpec{
		Name:         "row-mt",
		Type:         ParamTypeInt,
		DefaultValue: "0",
		Description:  "Enable row-based multi-threading (0 or 1)",
	}

	// Tiles
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

// initVP9NameTranslations initializes parameter name translations for each encoder.
func initVP9NameTranslations(m *VP9CommonMapping) {
	// libvpx-vp9 -> vp9_nvenc name translations
	m.NameTranslations[EncoderVP9NVENC] = map[string]string{
		"crf":     "cq",
		"qp":      "qp",
		"b:v":     "b:v",
		"maxrate": "maxrate",
		"bufsize": "bufsize",
		"g":       "g",
		"profile": "profile",
	}

	// libvpx-vp9 -> vp9_qsv name translations
	m.NameTranslations[EncoderVP9QSV] = map[string]string{
		"crf":     "global_quality",
		"qp":      "qp",
		"b:v":     "b:v",
		"maxrate": "maxrate",
		"bufsize": "bufsize",
		"g":       "g",
		"profile": "profile",
	}

	// libvpx-vp9 -> vp9_vaapi name translations
	m.NameTranslations[EncoderVP9VAAPI] = map[string]string{
		"crf":     "quality",
		"qp":      "qp",
		"b:v":     "b:v",
		"maxrate": "maxrate",
		"bufsize": "bufsize",
		"g":       "g",
		"profile": "profile",
	}
}

// initVP9ValueConversionRules initializes value conversion rules for different parameter types.
func initVP9ValueConversionRules(m *VP9CommonMapping) {
	// CRF to QP conversion (direct pass-through)
	m.ValueConversionRules["crf_to_qp"] = ValueConversionRule{
		SourceParam:     "crf",
		TargetParamType: "qp",
		RangeMappingKey: "crf_vp9",
	}

	// CRF to NVENC CQ conversion (direct pass-through)
	m.ValueConversionRules["crf_to_cq"] = ValueConversionRule{
		SourceParam:     "crf",
		TargetParamType: "cq",
		RangeMappingKey: "crf_vp9",
	}

	// CRF to QSV global_quality conversion (direct pass-through)
	m.ValueConversionRules["crf_to_global_quality"] = ValueConversionRule{
		SourceParam:     "crf",
		TargetParamType: "global_quality",
		RangeMappingKey: "crf_vp9",
	}

	// CRF to VAAPI quality conversion (scale: 0-63 -> 0-100)
	m.ValueConversionRules["crf_to_quality_vp9"] = ValueConversionRule{
		SourceParam:     "crf",
		TargetParamType: "quality",
		ScaleFunc:       vp9CRFToVAAPIQuality,
		RangeMappingKey: "vaapi_quality_vp9",
	}

	// Profile mapping for VP9
	m.ValueConversionRules["profile_mapping_vp9"] = ValueConversionRule{
		SourceParam:     "profile",
		TargetParamType: "profile",
		ValueMap: map[string]string{
			"0": "0",
			"1": "1",
			"2": "2",
			"3": "3",
		},
		RangeMappingKey: "profile_vp9",
	}
}

// initVP9RangeMappings initializes valid value ranges for parameters.
func initVP9RangeMappings(m *VP9CommonMapping) {
	// CRF range for VP9 (0-63)
	m.RangeMappings["crf_vp9"] = ValueRange{
		Min:         0,
		Max:         63,
		Description: "VP9 CRF quality range: 0 (lossless) to 63 (worst quality)",
	}

	// QP range for VP9 (0-63)
	m.RangeMappings["qp"] = ValueRange{
		Min:         0,
		Max:         63,
		Description: "VP9 Quantization Parameter range: 0 (lossless) to 63",
	}

	// VAAPI quality range for VP9 (0-100)
	m.RangeMappings["vaapi_quality_vp9"] = ValueRange{
		Min:         0,
		Max:         100,
		Description: "VAAPI VP9 quality range: higher is better",
	}

	// CPU-used range
	m.RangeMappings["cpu-used"] = ValueRange{
		Min:         0,
		Max:         5,
		Description: "CPU usage level: 0 (slowest/best) to 5 (fastest)",
	}

	// Profile valid values for VP9
	m.RangeMappings["profile_vp9"] = ValueRange{
		ValidValues: []string{"0", "1", "2", "3"},
		Description: "VP9 profile values",
	}

	// Row-MT valid values
	m.RangeMappings["row-mt"] = ValueRange{
		Min:         0,
		Max:         1,
		Description: "Row multi-threading: 0 (disabled) or 1 (enabled)",
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
		Max:         2,
		Description: "Number of tile rows (log2 value)",
	}
}

// vp9CRFToVAAPIQuality converts VP9 CRF value (0-63) to VAAPI quality (0-100).
// Higher CRF = lower quality, higher VAAPI quality = better quality.
// So we invert: CRF 0 -> quality 100, CRF 63 -> quality 0.
func vp9CRFToVAAPIQuality(crf int) int {
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
// when translating from libvpx-vp9 to the specified target encoder.
// Returns an empty string if no translation exists.
func (m *VP9CommonMapping) GetCommonParameterTranslation(targetEncoder EncoderFamily, paramName string) string {
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
func (m *VP9CommonMapping) GetCommonValueConversion(sourceValue string, targetParamType string) (string, error) {
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
func (m *VP9CommonMapping) ValidateParameterRange(paramName string, value string) error {
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

// GetStandardParams returns all standard libvpx-vp9 parameters.
func (m *VP9CommonMapping) GetStandardParams() map[string]ParameterSpec {
	return m.StandardParams
}

// GetNameTranslations returns the parameter name translations for a specific target encoder.
func (m *VP9CommonMapping) GetNameTranslations(targetEncoder EncoderFamily) map[string]string {
	return m.NameTranslations[targetEncoder]
}

// GetValueRange returns the valid value range for a parameter.
func (m *VP9CommonMapping) GetValueRange(paramName string) (*ValueRange, bool) {
	rangeSpec, exists := m.RangeMappings[paramName]
	if !exists {
		return nil, false
	}
	return &rangeSpec, true
}

// ConvertParameter performs a complete parameter conversion for a target encoder.
// It translates the parameter name and converts the value according to the rules.
// Returns the target parameter name, converted value, and any error.
func (m *VP9CommonMapping) ConvertParameter(targetEncoder EncoderFamily, paramName string, value string) (string, string, error) {
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
func (m *VP9CommonMapping) getConversionKey(paramName string, targetEncoder EncoderFamily) string {
	switch paramName {
	case "crf":
		switch targetEncoder {
		case EncoderVP9NVENC:
			return "crf_to_cq"
		case EncoderVP9QSV:
			return "crf_to_global_quality"
		case EncoderVP9VAAPI:
			return "crf_to_quality_vp9"
		default:
			return "crf_to_cq"
		}
	case "profile":
		return "profile_mapping_vp9"
	default:
		return paramName
	}
}

// GetSupportedEncoders returns a list of VP9 encoder families supported by this mapping.
func (m *VP9CommonMapping) GetSupportedEncoders() []EncoderFamily {
	encoders := make([]EncoderFamily, 0, len(m.NameTranslations))
	for encoder := range m.NameTranslations {
		encoders = append(encoders, encoder)
	}
	return encoders
}

// IsParameterSupported checks if a parameter is supported for translation to a target encoder.
func (m *VP9CommonMapping) IsParameterSupported(targetEncoder EncoderFamily, paramName string) bool {
	translations, exists := m.NameTranslations[targetEncoder]
	if !exists {
		return false
	}
	targetParam, exists := translations[paramName]
	return exists && targetParam != ""
}
