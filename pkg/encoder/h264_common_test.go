package encoder

import (
	"strconv"
	"testing"
)

func TestNewH264CommonMapping(t *testing.T) {
	m := NewH264CommonMapping()
	if m == nil {
		t.Fatal("NewH264CommonMapping() returned nil")
	}
	if m.StandardParams == nil {
		t.Error("StandardParams not initialized")
	}
	if m.NameTranslations == nil {
		t.Error("NameTranslations not initialized")
	}
	if m.ValueConversionRules == nil {
		t.Error("ValueConversionRules not initialized")
	}
	if m.RangeMappings == nil {
		t.Error("RangeMappings not initialized")
	}
}

func TestH264CommonMapping_StandardParams(t *testing.T) {
	m := NewH264CommonMapping()

	expectedParams := []string{"crf", "qp", "preset", "tune", "profile", "level",
		"b:v", "maxrate", "bufsize", "g", "keyint_min", "bf", "b_strategy", "refs"}

	for _, paramName := range expectedParams {
		spec, exists := m.StandardParams[paramName]
		if !exists {
			t.Errorf("Standard param '%s' not found", paramName)
			continue
		}
		if spec.Name != paramName {
			t.Errorf("StandardParams[%s].Name = %v, want %v", paramName, spec.Name, paramName)
		}
		if spec.Type == "" {
			t.Errorf("StandardParams[%s].Type is empty", paramName)
		}
	}

	// Verify specific defaults
	if m.StandardParams["crf"].DefaultValue != "23" {
		t.Errorf("CRF default = %v, want 23", m.StandardParams["crf"].DefaultValue)
	}
	if m.StandardParams["preset"].DefaultValue != "medium" {
		t.Errorf("Preset default = %v, want medium", m.StandardParams["preset"].DefaultValue)
	}
}

func TestH264CommonMapping_NameTranslations_NVENC(t *testing.T) {
	m := NewH264CommonMapping()

	expectedTranslations := map[string]string{
		"crf":     "cq",
		"qp":      "qp",
		"preset":  "preset",
		"tune":    "tune",
		"profile": "profile",
		"level":   "level",
		"b:v":     "b:v",
		"maxrate": "maxrate",
		"bufsize": "bufsize",
	}

	for source, expected := range expectedTranslations {
		result := m.GetCommonParameterTranslation(EncoderH264NVENC, source)
		if result != expected {
			t.Errorf("NVENC translation for '%s' = %v, want %v", source, result, expected)
		}
	}
}

func TestH264CommonMapping_NameTranslations_QSV(t *testing.T) {
	m := NewH264CommonMapping()

	expectedTranslations := map[string]string{
		"crf":     "global_quality",
		"qp":      "qp",
		"preset":  "preset",
		"profile": "profile",
		"level":   "level",
		"b:v":     "b:v",
		"maxrate": "maxrate",
	}

	for source, expected := range expectedTranslations {
		result := m.GetCommonParameterTranslation(EncoderH264QSV, source)
		if result != expected {
			t.Errorf("QSV translation for '%s' = %v, want %v", source, result, expected)
		}
	}
}

func TestH264CommonMapping_NameTranslations_VAAPI(t *testing.T) {
	m := NewH264CommonMapping()

	expectedTranslations := map[string]string{
		"crf":     "quality",
		"qp":      "qp",
		"profile": "profile",
		"level":   "level",
		"b:v":     "b:v",
		"maxrate": "maxrate",
	}

	for source, expected := range expectedTranslations {
		result := m.GetCommonParameterTranslation(EncoderH264VAAPI, source)
		if result != expected {
			t.Errorf("VAAPI translation for '%s' = %v, want %v", source, result, expected)
		}
	}
}

func TestH264CommonMapping_NameTranslations_AMF(t *testing.T) {
	m := NewH264CommonMapping()

	expectedTranslations := map[string]string{
		"crf":     "qp_i",
		"qp":      "qp",
		"preset":  "quality",
		"profile": "profile",
		"level":   "level",
		"b:v":     "b:v",
	}

	for source, expected := range expectedTranslations {
		result := m.GetCommonParameterTranslation(EncoderH264AMF, source)
		if result != expected {
			t.Errorf("AMF translation for '%s' = %v, want %v", source, result, expected)
		}
	}
}

func TestH264CommonMapping_NameTranslations_VideoToolbox(t *testing.T) {
	m := NewH264CommonMapping()

	expectedTranslations := map[string]string{
		"crf":     "q:v",
		"qp":      "q:v",
		"profile": "profile",
		"level":   "level",
		"b:v":     "b:v",
	}

	for source, expected := range expectedTranslations {
		result := m.GetCommonParameterTranslation(EncoderH264VT, source)
		if result != expected {
			t.Errorf("VideoToolbox translation for '%s' = %v, want %v", source, result, expected)
		}
	}
}

func TestH264CommonMapping_NameTranslations_UnsupportedEncoder(t *testing.T) {
	m := NewH264CommonMapping()

	result := m.GetCommonParameterTranslation(EncoderLibX265, "crf")
	if result != "" {
		t.Errorf("Expected empty string for unsupported encoder, got %v", result)
	}
}

func TestH264CommonMapping_NameTranslations_UnsupportedParam(t *testing.T) {
	m := NewH264CommonMapping()

	result := m.GetCommonParameterTranslation(EncoderH264NVENC, "nonexistent")
	if result != "" {
		t.Errorf("Expected empty string for unsupported parameter, got %v", result)
	}
}

func TestH264CommonMapping_ValueConversion_PresetToNVENC(t *testing.T) {
	m := NewH264CommonMapping()

	tests := []struct {
		input    string
		expected string
	}{
		{"ultrafast", "p1"},
		{"superfast", "p2"},
		{"veryfast", "p3"},
		{"faster", "p4"},
		{"fast", "p5"},
		{"medium", "p6"},
		{"slow", "p7"},
		{"slower", "p7"},
		{"veryslow", "p7"},
		{"placebo", "p7"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result, err := m.GetCommonValueConversion(tt.input, "preset_to_nvenc")
			if err != nil {
				t.Errorf("GetCommonValueConversion() error = %v", err)
			}
			if result != tt.expected {
				t.Errorf("GetCommonValueConversion() = %v, want %v", result, tt.expected)
			}
		})
	}
}

func TestH264CommonMapping_ValueConversion_PresetToQSV(t *testing.T) {
	m := NewH264CommonMapping()

	tests := []struct {
		input    string
		expected string
	}{
		{"ultrafast", "veryfast"},
		{"veryfast", "veryfast"},
		{"fast", "fast"},
		{"medium", "medium"},
		{"slow", "slow"},
		{"veryslow", "veryslow"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result, err := m.GetCommonValueConversion(tt.input, "preset_to_qsv")
			if err != nil {
				t.Errorf("GetCommonValueConversion() error = %v", err)
			}
			if result != tt.expected {
				t.Errorf("GetCommonValueConversion() = %v, want %v", result, tt.expected)
			}
		})
	}
}

func TestH264CommonMapping_ValueConversion_PresetToAMF(t *testing.T) {
	m := NewH264CommonMapping()

	tests := []struct {
		input    string
		expected string
	}{
		{"ultrafast", "speed"},
		{"veryfast", "speed"},
		{"medium", "balanced"},
		{"slow", "quality"},
		{"veryslow", "quality"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result, err := m.GetCommonValueConversion(tt.input, "preset_to_amf")
			if err != nil {
				t.Errorf("GetCommonValueConversion() error = %v", err)
			}
			if result != tt.expected {
				t.Errorf("GetCommonValueConversion() = %v, want %v", result, tt.expected)
			}
		})
	}
}

func TestH264CommonMapping_ValueConversion_TuneToNVENC(t *testing.T) {
	m := NewH264CommonMapping()

	tests := []struct {
		input    string
		expected string
	}{
		{"film", "film"},
		{"animation", "animation"},
		{"grain", "grain"},
		{"stillimage", "stillimage"},
		{"fastdecode", "fastdecode"},
		{"zerolatency", "ull"},
		// psnr and ssim are not supported, return empty string in map
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result, err := m.GetCommonValueConversion(tt.input, "tune_to_nvenc")
			if err != nil {
				t.Errorf("GetCommonValueConversion() error = %v", err)
			}
			if result != tt.expected {
				t.Errorf("GetCommonValueConversion() = %v, want %v", result, tt.expected)
			}
		})
	}
}

func TestH264CommonMapping_ValueConversion_ProfileMapping(t *testing.T) {
	m := NewH264CommonMapping()

	tests := []struct {
		input    string
		expected string
	}{
		{"baseline", "baseline"},
		{"main", "main"},
		{"high", "high"},
		{"high10", "high10"},
		{"high422", "high422"},
		{"high444", "high444"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result, err := m.GetCommonValueConversion(tt.input, "profile_mapping")
			if err != nil {
				t.Errorf("GetCommonValueConversion() error = %v", err)
			}
			if result != tt.expected {
				t.Errorf("GetCommonValueConversion() = %v, want %v", result, tt.expected)
			}
		})
	}
}

func TestH264CommonMapping_ValueConversion_LevelMapping(t *testing.T) {
	m := NewH264CommonMapping()

	tests := []struct {
		input    string
		expected string
	}{
		{"3.0", "3"},
		{"3.1", "3.1"},
		{"4.0", "4"},
		{"4.1", "4.1"},
		{"5.0", "5"},
		{"5.1", "5.1"},
		{"5.2", "5.2"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result, err := m.GetCommonValueConversion(tt.input, "level_mapping")
			if err != nil {
				t.Errorf("GetCommonValueConversion() error = %v", err)
			}
			if result != tt.expected {
				t.Errorf("GetCommonValueConversion() = %v, want %v", result, tt.expected)
			}
		})
	}
}

func TestH264CommonMapping_ValueConversion_CRFToVAAPIQuality(t *testing.T) {
	m := NewH264CommonMapping()

	tests := []struct {
		input    string
		expected string
	}{
		{"0", "100"}, // CRF 0 = lossless -> quality 100
		{"23", "55"}, // CRF 23 -> quality ~55
		{"51", "0"},  // CRF 51 = worst -> quality 0
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result, err := m.GetCommonValueConversion(tt.input, "crf_to_quality")
			if err != nil {
				t.Errorf("GetCommonValueConversion() error = %v", err)
			}
			if result != tt.expected {
				t.Errorf("GetCommonValueConversion() = %v, want %v", result, tt.expected)
			}
		})
	}
}

func TestH264CommonMapping_ValueConversion_CRFPassThrough(t *testing.T) {
	m := NewH264CommonMapping()

	// CRF to CQ should pass through directly
	result, err := m.GetCommonValueConversion("23", "crf_to_cq")
	if err != nil {
		t.Errorf("GetCommonValueConversion() error = %v", err)
	}
	if result != "23" {
		t.Errorf("GetCommonValueConversion(crf_to_cq) = %v, want 23", result)
	}

	// CRF to QP should pass through directly
	result, err = m.GetCommonValueConversion("28", "crf_to_qp")
	if err != nil {
		t.Errorf("GetCommonValueConversion() error = %v", err)
	}
	if result != "28" {
		t.Errorf("GetCommonValueConversion(crf_to_qp) = %v, want 28", result)
	}
}

func TestH264CommonMapping_ValueConversion_UnknownRule(t *testing.T) {
	m := NewH264CommonMapping()

	// Unknown conversion rule should return value as-is
	result, err := m.GetCommonValueConversion("test_value", "unknown_rule")
	if err != nil {
		t.Errorf("GetCommonValueConversion() error = %v", err)
	}
	if result != "test_value" {
		t.Errorf("GetCommonValueConversion(unknown_rule) = %v, want test_value", result)
	}
}

func TestH264CommonMapping_ValueConversion_NonNumericScale(t *testing.T) {
	m := NewH264CommonMapping()

	// Non-numeric value with a scale function should return error
	_, err := m.GetCommonValueConversion("not_a_number", "crf_to_quality")
	if err == nil {
		t.Error("Expected error for non-numeric value with scale function")
	}
}

func TestH264CommonMapping_ValidateParameterRange_CRF(t *testing.T) {
	m := NewH264CommonMapping()

	// Valid CRF values
	validValues := []string{"0", "1", "23", "28", "51"}
	for _, v := range validValues {
		err := m.ValidateParameterRange("crf", v)
		if err != nil {
			t.Errorf("ValidateParameterRange(crf, %s) returned error: %v", v, err)
		}
	}

	// Invalid CRF values
	invalidValues := []string{"-1", "52", "100"}
	for _, v := range invalidValues {
		err := m.ValidateParameterRange("crf", v)
		if err == nil {
			t.Errorf("ValidateParameterRange(crf, %s) should return error", v)
		}
	}
}

func TestH264CommonMapping_ValidateParameterRange_QP(t *testing.T) {
	m := NewH264CommonMapping()

	// Valid QP values
	validValues := []string{"0", "1", "25", "51"}
	for _, v := range validValues {
		err := m.ValidateParameterRange("qp", v)
		if err != nil {
			t.Errorf("ValidateParameterRange(qp, %s) returned error: %v", v, err)
		}
	}

	// Invalid QP values
	err := m.ValidateParameterRange("qp", "52")
	if err == nil {
		t.Error("ValidateParameterRange(qp, 52) should return error")
	}
}

func TestH264CommonMapping_ValidateParameterRange_Preset(t *testing.T) {
	m := NewH264CommonMapping()

	// Valid preset values
	validValues := []string{"ultrafast", "veryfast", "fast", "medium", "slow", "veryslow"}
	for _, v := range validValues {
		err := m.ValidateParameterRange("preset", v)
		if err != nil {
			t.Errorf("ValidateParameterRange(preset, %s) returned error: %v", v, err)
		}
	}

	// Invalid preset value
	err := m.ValidateParameterRange("preset", "invalid_preset")
	if err == nil {
		t.Error("ValidateParameterRange(preset, invalid_preset) should return error")
	}
}

func TestH264CommonMapping_ValidateParameterRange_Tune(t *testing.T) {
	m := NewH264CommonMapping()

	// Valid tune values
	validValues := []string{"film", "animation", "grain", "zerolatency", "psnr", "ssim"}
	for _, v := range validValues {
		err := m.ValidateParameterRange("tune", v)
		if err != nil {
			t.Errorf("ValidateParameterRange(tune, %s) returned error: %v", v, err)
		}
	}

	// Invalid tune value
	err := m.ValidateParameterRange("tune", "invalid_tune")
	if err == nil {
		t.Error("ValidateParameterRange(tune, invalid_tune) should return error")
	}
}

func TestH264CommonMapping_ValidateParameterRange_Profile(t *testing.T) {
	m := NewH264CommonMapping()

	// Valid profile values
	validValues := []string{"baseline", "main", "high", "high10"}
	for _, v := range validValues {
		err := m.ValidateParameterRange("profile", v)
		if err != nil {
			t.Errorf("ValidateParameterRange(profile, %s) returned error: %v", v, err)
		}
	}

	// Invalid profile value
	err := m.ValidateParameterRange("profile", "invalid_profile")
	if err == nil {
		t.Error("ValidateParameterRange(profile, invalid_profile) should return error")
	}
}

func TestH264CommonMapping_ValidateParameterRange_Level(t *testing.T) {
	m := NewH264CommonMapping()

	// Valid level values
	validValues := []string{"3.0", "3.1", "4.0", "4.1", "5.1"}
	for _, v := range validValues {
		err := m.ValidateParameterRange("level", v)
		if err != nil {
			t.Errorf("ValidateParameterRange(level, %s) returned error: %v", v, err)
		}
	}

	// Invalid level value
	err := m.ValidateParameterRange("level", "9.9")
	if err == nil {
		t.Error("ValidateParameterRange(level, 9.9) should return error")
	}
}

func TestH264CommonMapping_ValidateParameterRange_UnknownParam(t *testing.T) {
	m := NewH264CommonMapping()

	// Unknown parameter should return nil (no validation)
	err := m.ValidateParameterRange("unknown_param", "any_value")
	if err != nil {
		t.Errorf("ValidateParameterRange for unknown param should return nil, got: %v", err)
	}
}

func TestH264CommonMapping_ValidateParameterRange_NonNumericForRange(t *testing.T) {
	m := NewH264CommonMapping()

	// Non-numeric value for a numeric range parameter
	err := m.ValidateParameterRange("crf", "not_a_number")
	if err == nil {
		t.Error("ValidateParameterRange(crf, not_a_number) should return error")
	}
}

func TestH264CommonMapping_ConvertParameter_CRFToNVENC(t *testing.T) {
	m := NewH264CommonMapping()

	targetParam, convertedValue, err := m.ConvertParameter(EncoderH264NVENC, "crf", "23")
	if err != nil {
		t.Errorf("ConvertParameter() error = %v", err)
	}
	if targetParam != "cq" {
		t.Errorf("ConvertParameter() targetParam = %v, want cq", targetParam)
	}
	if convertedValue != "23" {
		t.Errorf("ConvertParameter() convertedValue = %v, want 23", convertedValue)
	}
}

func TestH264CommonMapping_ConvertParameter_PresetToNVENC(t *testing.T) {
	m := NewH264CommonMapping()

	targetParam, convertedValue, err := m.ConvertParameter(EncoderH264NVENC, "preset", "slow")
	if err != nil {
		t.Errorf("ConvertParameter() error = %v", err)
	}
	if targetParam != "preset" {
		t.Errorf("ConvertParameter() targetParam = %v, want preset", targetParam)
	}
	if convertedValue != "p7" {
		t.Errorf("ConvertParameter() convertedValue = %v, want p7", convertedValue)
	}
}

func TestH264CommonMapping_ConvertParameter_CRFToQSV(t *testing.T) {
	m := NewH264CommonMapping()

	targetParam, convertedValue, err := m.ConvertParameter(EncoderH264QSV, "crf", "23")
	if err != nil {
		t.Errorf("ConvertParameter() error = %v", err)
	}
	if targetParam != "global_quality" {
		t.Errorf("ConvertParameter() targetParam = %v, want global_quality", targetParam)
	}
	if convertedValue != "23" {
		t.Errorf("ConvertParameter() convertedValue = %v, want 23", convertedValue)
	}
}

func TestH264CommonMapping_ConvertParameter_CRFToVAAPI(t *testing.T) {
	m := NewH264CommonMapping()

	targetParam, convertedValue, err := m.ConvertParameter(EncoderH264VAAPI, "crf", "23")
	if err != nil {
		t.Errorf("ConvertParameter() error = %v", err)
	}
	if targetParam != "quality" {
		t.Errorf("ConvertParameter() targetParam = %v, want quality", targetParam)
	}
	// CRF 23 -> quality ~55
	if convertedValue != "55" {
		t.Errorf("ConvertParameter() convertedValue = %v, want 55", convertedValue)
	}
}

func TestH264CommonMapping_ConvertParameter_ProfileToNVENC(t *testing.T) {
	m := NewH264CommonMapping()

	targetParam, convertedValue, err := m.ConvertParameter(EncoderH264NVENC, "profile", "high")
	if err != nil {
		t.Errorf("ConvertParameter() error = %v", err)
	}
	if targetParam != "profile" {
		t.Errorf("ConvertParameter() targetParam = %v, want profile", targetParam)
	}
	if convertedValue != "high" {
		t.Errorf("ConvertParameter() convertedValue = %v, want high", convertedValue)
	}
}

func TestH264CommonMapping_ConvertParameter_PresetToAMF(t *testing.T) {
	m := NewH264CommonMapping()

	targetParam, convertedValue, err := m.ConvertParameter(EncoderH264AMF, "preset", "veryslow")
	if err != nil {
		t.Errorf("ConvertParameter() error = %v", err)
	}
	if targetParam != "quality" {
		t.Errorf("ConvertParameter() targetParam = %v, want quality", targetParam)
	}
	if convertedValue != "quality" {
		t.Errorf("ConvertParameter() convertedValue = %v, want quality", convertedValue)
	}
}

func TestH264CommonMapping_ConvertParameter_UnsupportedParam(t *testing.T) {
	m := NewH264CommonMapping()

	_, _, err := m.ConvertParameter(EncoderH264NVENC, "nonexistent", "value")
	if err == nil {
		t.Error("ConvertParameter() should return error for unsupported parameter")
	}
}

func TestH264CommonMapping_ConvertParameter_TuneToNVENC(t *testing.T) {
	m := NewH264CommonMapping()

	targetParam, convertedValue, err := m.ConvertParameter(EncoderH264NVENC, "tune", "zerolatency")
	if err != nil {
		t.Errorf("ConvertParameter() error = %v", err)
	}
	if targetParam != "tune" {
		t.Errorf("ConvertParameter() targetParam = %v, want tune", targetParam)
	}
	if convertedValue != "ull" {
		t.Errorf("ConvertParameter() convertedValue = %v, want ull", convertedValue)
	}
}

func TestH264CommonMapping_GetSupportedEncoders(t *testing.T) {
	m := NewH264CommonMapping()

	encoders := m.GetSupportedEncoders()
	expectedEncoders := []EncoderFamily{
		EncoderH264NVENC, EncoderH264QSV, EncoderH264VAAPI, EncoderH264AMF, EncoderH264VT,
	}

	if len(encoders) != len(expectedEncoders) {
		t.Errorf("GetSupportedEncoders() returned %d encoders, want %d", len(encoders), len(expectedEncoders))
	}

	for _, expected := range expectedEncoders {
		found := false
		for _, enc := range encoders {
			if enc == expected {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("GetSupportedEncoders() missing expected encoder %s", expected)
		}
	}
}

func TestH264CommonMapping_IsParameterSupported(t *testing.T) {
	m := NewH264CommonMapping()

	// Supported parameters
	if !m.IsParameterSupported(EncoderH264NVENC, "crf") {
		t.Error("crf should be supported for NVENC")
	}
	if !m.IsParameterSupported(EncoderH264NVENC, "preset") {
		t.Error("preset should be supported for NVENC")
	}

	// Unsupported parameters
	if m.IsParameterSupported(EncoderH264NVENC, "nonexistent") {
		t.Error("nonexistent should not be supported")
	}

	// Unsupported encoder
	if m.IsParameterSupported(EncoderLibX265, "crf") {
		t.Error("H.265 encoder should not be supported in H264 mapping")
	}

	// Tune is supported but VAAPI doesn't support it (empty translation)
	if m.IsParameterSupported(EncoderH264VAAPI, "tune") {
		t.Error("tune should not be supported for VAAPI (empty translation)")
	}
}

func TestH264CommonMapping_GetStandardParams(t *testing.T) {
	m := NewH264CommonMapping()

	params := m.GetStandardParams()
	if len(params) == 0 {
		t.Error("GetStandardParams() returned empty map")
	}

	// Verify all standard parameters are present
	expectedParams := []string{"crf", "qp", "preset", "tune", "profile", "level"}
	for _, p := range expectedParams {
		if _, exists := params[p]; !exists {
			t.Errorf("GetStandardParams() missing expected param %s", p)
		}
	}
}

func TestH264CommonMapping_GetNameTranslations(t *testing.T) {
	m := NewH264CommonMapping()

	translations := m.GetNameTranslations(EncoderH264NVENC)
	if len(translations) == 0 {
		t.Error("GetNameTranslations(NVENC) returned empty map")
	}

	// Verify CRF translation
	if translations["crf"] != "cq" {
		t.Errorf("NVENC crf translation = %v, want cq", translations["crf"])
	}
}

func TestH264CommonMapping_GetValueRange(t *testing.T) {
	m := NewH264CommonMapping()

	// Valid range
	rangeSpec, exists := m.GetValueRange("crf")
	if !exists {
		t.Error("GetValueRange(crf) should exist")
	}
	if rangeSpec.Min != 0 || rangeSpec.Max != 51 {
		t.Errorf("CRF range = [%d, %d], want [0, 51]", rangeSpec.Min, rangeSpec.Max)
	}

	// Non-existent range
	_, exists = m.GetValueRange("nonexistent")
	if exists {
		t.Error("GetValueRange(nonexistent) should not exist")
	}
}

func TestCRFToVAAPIQuality(t *testing.T) {
	tests := []struct {
		crf      int
		expected int
	}{
		{0, 100}, // lossless CRF -> max quality
		{25, 51}, // CRF 25 -> quality ~51
		{51, 0},  // worst CRF -> min quality
	}

	for _, tt := range tests {
		t.Run(strconv.Itoa(tt.crf), func(t *testing.T) {
			result := crfToVAAPIQuality(tt.crf)
			if result != tt.expected {
				t.Errorf("crfToVAAPIQuality(%d) = %d, want %d", tt.crf, result, tt.expected)
			}
		})
	}

	// Test boundary clamping
	if result := crfToVAAPIQuality(-1); result != 100 {
		t.Errorf("crfToVAAPIQuality(-1) = %d, want 100", result)
	}
	if result := crfToVAAPIQuality(100); result != 0 {
		t.Errorf("crfToVAAPIQuality(100) = %d, want 0", result)
	}
}

func TestH264CommonMapping_ValueConversion_LevelNormalization(t *testing.T) {
	m := NewH264CommonMapping()

	// Test that 3.0 maps to "3" (normalization)
	result, err := m.GetCommonValueConversion("3.0", "level_mapping")
	if err != nil {
		t.Errorf("GetCommonValueConversion() error = %v", err)
	}
	if result != "3" {
		t.Errorf("GetCommonValueConversion(level, 3.0) = %v, want 3", result)
	}

	// Test that 4.1 stays as "4.1"
	result, err = m.GetCommonValueConversion("4.1", "level_mapping")
	if err != nil {
		t.Errorf("GetCommonValueConversion() error = %v", err)
	}
	if result != "4.1" {
		t.Errorf("GetCommonValueConversion(level, 4.1) = %v, want 4.1", result)
	}
}

func TestH264CommonMapping_Integration_WithEncoderMapping(t *testing.T) {
	// Test that H264CommonMapping integrates with the base EncoderMapping
	baseMapping := DefaultMapping()
	h264Mapping := NewH264CommonMapping()

	// Verify that H.264 encoders are registered in the base mapping
	h264Encoders := baseMapping.GetEncodersForCodec(CodecH264)
	if len(h264Encoders) == 0 {
		t.Error("DefaultMapping should have H.264 encoders")
	}

	// Verify that H264CommonMapping covers the same encoders
	h264Supported := h264Mapping.GetSupportedEncoders()
	for _, enc := range h264Supported {
		format, exists := baseMapping.GetCodecFormat(enc)
		if !exists {
			t.Errorf("H264CommonMapping encoder %s not found in base mapping", enc)
		}
		if format != CodecH264 {
			t.Errorf("H264CommonMapping encoder %s has wrong codec format: %v", enc, format)
		}
	}
}

func TestH264CommonMapping_ValidateParameterRange_BFrames(t *testing.T) {
	m := NewH264CommonMapping()

	// Valid B-frame values
	validValues := []string{"0", "3", "16"}
	for _, v := range validValues {
		err := m.ValidateParameterRange("bf", v)
		if err != nil {
			t.Errorf("ValidateParameterRange(bf, %s) returned error: %v", v, err)
		}
	}

	// Invalid B-frame values
	err := m.ValidateParameterRange("bf", "17")
	if err == nil {
		t.Error("ValidateParameterRange(bf, 17) should return error")
	}
}

func TestH264CommonMapping_ValidateParameterRange_Refs(t *testing.T) {
	m := NewH264CommonMapping()

	// Valid refs values
	validValues := []string{"1", "3", "16"}
	for _, v := range validValues {
		err := m.ValidateParameterRange("refs", v)
		if err != nil {
			t.Errorf("ValidateParameterRange(refs, %s) returned error: %v", v, err)
		}
	}

	// Invalid refs values
	err := m.ValidateParameterRange("refs", "0")
	if err == nil {
		t.Error("ValidateParameterRange(refs, 0) should return error")
	}
	err = m.ValidateParameterRange("refs", "17")
	if err == nil {
		t.Error("ValidateParameterRange(refs, 17) should return error")
	}
}

func TestH264CommonMapping_ConvertParameter_LevelToNVENC(t *testing.T) {
	m := NewH264CommonMapping()

	targetParam, convertedValue, err := m.ConvertParameter(EncoderH264NVENC, "level", "4.1")
	if err != nil {
		t.Errorf("ConvertParameter() error = %v", err)
	}
	if targetParam != "level" {
		t.Errorf("ConvertParameter() targetParam = %v, want level", targetParam)
	}
	if convertedValue != "4.1" {
		t.Errorf("ConvertParameter() convertedValue = %v, want 4.1", convertedValue)
	}
}

func TestH264CommonMapping_ConvertParameter_PresetToQSV(t *testing.T) {
	m := NewH264CommonMapping()

	targetParam, convertedValue, err := m.ConvertParameter(EncoderH264QSV, "preset", "veryfast")
	if err != nil {
		t.Errorf("ConvertParameter() error = %v", err)
	}
	if targetParam != "preset" {
		t.Errorf("ConvertParameter() targetParam = %v, want preset", targetParam)
	}
	if convertedValue != "veryfast" {
		t.Errorf("ConvertParameter() convertedValue = %v, want veryfast", convertedValue)
	}
}
