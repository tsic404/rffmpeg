package encoder

import (
	"testing"
)

func TestNewHEVCCommonMapping(t *testing.T) {
	m := NewHEVCCommonMapping()
	if m == nil {
		t.Fatal("NewHEVCCommonMapping() returned nil")
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

func TestHEVCCommonMapping_StandardParams(t *testing.T) {
	m := NewHEVCCommonMapping()

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

	// Verify specific defaults (HEVC uses 28 as default CRF)
	if m.StandardParams["crf"].DefaultValue != "28" {
		t.Errorf("CRF default = %v, want 28", m.StandardParams["crf"].DefaultValue)
	}
	if m.StandardParams["preset"].DefaultValue != "medium" {
		t.Errorf("Preset default = %v, want medium", m.StandardParams["preset"].DefaultValue)
	}
}

func TestHEVCCommonMapping_NameTranslations_NVENC(t *testing.T) {
	m := NewHEVCCommonMapping()

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
		result := m.GetCommonParameterTranslation(EncoderHEVCNVENC, source)
		if result != expected {
			t.Errorf("NVENC translation for '%s' = %v, want %v", source, result, expected)
		}
	}
}

func TestHEVCCommonMapping_NameTranslations_QSV(t *testing.T) {
	m := NewHEVCCommonMapping()

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
		result := m.GetCommonParameterTranslation(EncoderHEVCQSV, source)
		if result != expected {
			t.Errorf("QSV translation for '%s' = %v, want %v", source, result, expected)
		}
	}
}

func TestHEVCCommonMapping_NameTranslations_VAAPI(t *testing.T) {
	m := NewHEVCCommonMapping()

	expectedTranslations := map[string]string{
		"crf":     "quality",
		"qp":      "qp",
		"profile": "profile",
		"level":   "level",
		"b:v":     "b:v",
		"maxrate": "maxrate",
	}

	for source, expected := range expectedTranslations {
		result := m.GetCommonParameterTranslation(EncoderHEVCVAAPI, source)
		if result != expected {
			t.Errorf("VAAPI translation for '%s' = %v, want %v", source, result, expected)
		}
	}
}

func TestHEVCCommonMapping_NameTranslations_AMF(t *testing.T) {
	m := NewHEVCCommonMapping()

	expectedTranslations := map[string]string{
		"crf":     "qp_i",
		"qp":      "qp",
		"preset":  "quality",
		"profile": "profile",
		"level":   "level",
		"b:v":     "b:v",
	}

	for source, expected := range expectedTranslations {
		result := m.GetCommonParameterTranslation(EncoderHEVCAMF, source)
		if result != expected {
			t.Errorf("AMF translation for '%s' = %v, want %v", source, result, expected)
		}
	}
}

func TestHEVCCommonMapping_NameTranslations_VideoToolbox(t *testing.T) {
	m := NewHEVCCommonMapping()

	expectedTranslations := map[string]string{
		"crf":     "q:v",
		"qp":      "q:v",
		"profile": "profile",
		"level":   "level",
		"b:v":     "b:v",
	}

	for source, expected := range expectedTranslations {
		result := m.GetCommonParameterTranslation(EncoderHEVCVT, source)
		if result != expected {
			t.Errorf("VideoToolbox translation for '%s' = %v, want %v", source, result, expected)
		}
	}
}

func TestHEVCCommonMapping_ValueConversion_PresetToNVENC(t *testing.T) {
	m := NewHEVCCommonMapping()

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
			result, err := m.GetCommonValueConversion(tt.input, "preset_to_nvenc_hevc")
			if err != nil {
				t.Errorf("GetCommonValueConversion() error = %v", err)
			}
			if result != tt.expected {
				t.Errorf("GetCommonValueConversion() = %v, want %v", result, tt.expected)
			}
		})
	}
}

func TestHEVCCommonMapping_ValueConversion_PresetToQSV(t *testing.T) {
	m := NewHEVCCommonMapping()

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
			result, err := m.GetCommonValueConversion(tt.input, "preset_to_qsv_hevc")
			if err != nil {
				t.Errorf("GetCommonValueConversion() error = %v", err)
			}
			if result != tt.expected {
				t.Errorf("GetCommonValueConversion() = %v, want %v", result, tt.expected)
			}
		})
	}
}

func TestHEVCCommonMapping_ValueConversion_PresetToAMF(t *testing.T) {
	m := NewHEVCCommonMapping()

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
			result, err := m.GetCommonValueConversion(tt.input, "preset_to_amf_hevc")
			if err != nil {
				t.Errorf("GetCommonValueConversion() error = %v", err)
			}
			if result != tt.expected {
				t.Errorf("GetCommonValueConversion() = %v, want %v", result, tt.expected)
			}
		})
	}
}

func TestHEVCCommonMapping_ValueConversion_ProfileMapping(t *testing.T) {
	m := NewHEVCCommonMapping()

	tests := []struct {
		input    string
		expected string
	}{
		{"main", "main"},
		{"main10", "main10"},
		{"mainstillpicture", "mainstillpicture"},
		{"rext", "rext"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result, err := m.GetCommonValueConversion(tt.input, "profile_mapping_hevc")
			if err != nil {
				t.Errorf("GetCommonValueConversion() error = %v", err)
			}
			if result != tt.expected {
				t.Errorf("GetCommonValueConversion() = %v, want %v", result, tt.expected)
			}
		})
	}
}

func TestHEVCCommonMapping_ValueConversion_LevelMapping(t *testing.T) {
	m := NewHEVCCommonMapping()

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
		{"6.0", "6"},
		{"6.1", "6.1"},
		{"6.2", "6.2"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result, err := m.GetCommonValueConversion(tt.input, "level_mapping_hevc")
			if err != nil {
				t.Errorf("GetCommonValueConversion() error = %v", err)
			}
			if result != tt.expected {
				t.Errorf("GetCommonValueConversion() = %v, want %v", result, tt.expected)
			}
		})
	}
}

func TestHEVCCommonMapping_ValidateParameterRange_CRF(t *testing.T) {
	m := NewHEVCCommonMapping()

	// Valid CRF values
	validValues := []string{"0", "1", "28", "51"}
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

func TestHEVCCommonMapping_ValidateParameterRange_Preset(t *testing.T) {
	m := NewHEVCCommonMapping()

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

func TestHEVCCommonMapping_ConvertParameter(t *testing.T) {
	m := NewHEVCCommonMapping()

	// Test CRF conversion to NVENC
	targetParam, convertedValue, err := m.ConvertParameter(EncoderHEVCNVENC, "crf", "28")
	if err != nil {
		t.Fatalf("ConvertParameter() error = %v", err)
	}
	if targetParam != "cq" {
		t.Errorf("Target param = %v, want cq", targetParam)
	}
	if convertedValue != "28" {
		t.Errorf("Converted value = %v, want 28", convertedValue)
	}

	// Test preset conversion to NVENC
	targetParam, convertedValue, err = m.ConvertParameter(EncoderHEVCNVENC, "preset", "slow")
	if err != nil {
		t.Fatalf("ConvertParameter() error = %v", err)
	}
	if targetParam != "preset" {
		t.Errorf("Target param = %v, want preset", targetParam)
	}
	if convertedValue != "p7" {
		t.Errorf("Converted value = %v, want p7", convertedValue)
	}

	// Test unsupported parameter
	_, _, err = m.ConvertParameter(EncoderHEVCNVENC, "nonexistent", "value")
	if err == nil {
		t.Error("ConvertParameter() should return error for unsupported parameter")
	}
}

func TestHEVCCommonMapping_GetSupportedEncoders(t *testing.T) {
	m := NewHEVCCommonMapping()

	encoders := m.GetSupportedEncoders()
	if len(encoders) != 5 {
		t.Errorf("GetSupportedEncoders() returned %d encoders, want 5", len(encoders))
	}

	// Verify expected encoders are present
	expectedEncoders := []EncoderFamily{
		EncoderHEVCNVENC, EncoderHEVCQSV, EncoderHEVCVAAPI, EncoderHEVCAMF, EncoderHEVCVT,
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
			t.Errorf("Encoder %s not found in supported encoders", expected)
		}
	}
}

func TestHEVCCommonMapping_IsParameterSupported(t *testing.T) {
	m := NewHEVCCommonMapping()

	// Supported parameters
	if !m.IsParameterSupported(EncoderHEVCNVENC, "crf") {
		t.Error("crf should be supported for HEVC NVENC")
	}
	if !m.IsParameterSupported(EncoderHEVCQSV, "preset") {
		t.Error("preset should be supported for HEVC QSV")
	}

	// Unsupported parameters
	if m.IsParameterSupported(EncoderHEVCVAAPI, "preset") {
		t.Error("preset should not be supported for HEVC VAAPI (no translation)")
	}
	if m.IsParameterSupported(EncoderHEVCNVENC, "nonexistent") {
		t.Error("nonexistent should not be supported")
	}
}
