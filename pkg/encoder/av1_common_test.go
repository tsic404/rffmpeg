package encoder

import (
	"testing"
)

func TestNewAV1CommonMapping(t *testing.T) {
	m := NewAV1CommonMapping()
	if m == nil {
		t.Fatal("NewAV1CommonMapping() returned nil")
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

func TestAV1CommonMapping_StandardParams(t *testing.T) {
	m := NewAV1CommonMapping()

	expectedParams := []string{"crf", "qp", "preset", "speed", "b:v", "maxrate", "bufsize",
		"g", "profile", "level", "tile-columns", "tile-rows"}

	for _, paramName := range expectedParams {
		spec, exists := m.StandardParams[paramName]
		if !exists {
			t.Errorf("Standard param '%s' not found", paramName)
			continue
		}
		if spec.Name != paramName {
			t.Errorf("StandardParams[%s].Name = %v, want %v", paramName, spec.Name, paramName)
		}
	}

	// Verify specific defaults (AV1 uses 30 as default CRF)
	if m.StandardParams["crf"].DefaultValue != "30" {
		t.Errorf("CRF default = %v, want 30", m.StandardParams["crf"].DefaultValue)
	}
	if m.StandardParams["speed"].DefaultValue != "7" {
		t.Errorf("speed default = %v, want 7", m.StandardParams["speed"].DefaultValue)
	}
}

func TestAV1CommonMapping_NameTranslations_NVENC(t *testing.T) {
	m := NewAV1CommonMapping()

	expectedTranslations := map[string]string{
		"crf":     "cq",
		"qp":      "qp",
		"preset":  "preset",
		"b:v":     "b:v",
		"maxrate": "maxrate",
		"bufsize": "bufsize",
		"g":       "g",
		"profile": "profile",
		"level":   "level",
	}

	for source, expected := range expectedTranslations {
		result := m.GetCommonParameterTranslation(EncoderAV1NVENC, source)
		if result != expected {
			t.Errorf("NVENC translation for '%s' = %v, want %v", source, result, expected)
		}
	}
}

func TestAV1CommonMapping_NameTranslations_QSV(t *testing.T) {
	m := NewAV1CommonMapping()

	expectedTranslations := map[string]string{
		"crf":     "global_quality",
		"qp":      "qp",
		"preset":  "preset",
		"b:v":     "b:v",
		"maxrate": "maxrate",
		"g":       "g",
		"profile": "profile",
		"level":   "level",
	}

	for source, expected := range expectedTranslations {
		result := m.GetCommonParameterTranslation(EncoderAV1QSV, source)
		if result != expected {
			t.Errorf("QSV translation for '%s' = %v, want %v", source, result, expected)
		}
	}
}

func TestAV1CommonMapping_NameTranslations_VAAPI(t *testing.T) {
	m := NewAV1CommonMapping()

	expectedTranslations := map[string]string{
		"crf":     "quality",
		"qp":      "qp",
		"b:v":     "b:v",
		"maxrate": "maxrate",
		"g":       "g",
		"profile": "profile",
		"level":   "level",
	}

	for source, expected := range expectedTranslations {
		result := m.GetCommonParameterTranslation(EncoderAV1VAAPI, source)
		if result != expected {
			t.Errorf("VAAPI translation for '%s' = %v, want %v", source, result, expected)
		}
	}
}

func TestAV1CommonMapping_NameTranslations_LibAOM(t *testing.T) {
	m := NewAV1CommonMapping()

	expectedTranslations := map[string]string{
		"crf":          "crf",
		"qp":           "qp",
		"b:v":          "b:v",
		"maxrate":      "maxrate",
		"g":            "g",
		"profile":      "profile",
		"level":        "level",
		"tile-columns": "tile-columns",
		"tile-rows":    "tile-rows",
	}

	for source, expected := range expectedTranslations {
		result := m.GetCommonParameterTranslation(EncoderLibAOM, source)
		if result != expected {
			t.Errorf("LibAOM translation for '%s' = %v, want %v", source, result, expected)
		}
	}
}

func TestAV1CommonMapping_ValueConversion_CRFToVAAPIQuality(t *testing.T) {
	m := NewAV1CommonMapping()

	tests := []struct {
		input    string
		expected string
	}{
		{"0", "100"}, // CRF 0 = lossless -> quality 100
		{"30", "53"}, // CRF 30 (default) -> quality ~52 (rounded)
		{"63", "0"},  // CRF 63 = worst -> quality 0
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result, err := m.GetCommonValueConversion(tt.input, "crf_to_quality_av1")
			if err != nil {
				t.Errorf("GetCommonValueConversion() error = %v", err)
			}
			if result != tt.expected {
				t.Errorf("GetCommonValueConversion() = %v, want %v", result, tt.expected)
			}
		})
	}
}

func TestAV1CommonMapping_ValueConversion_PresetToNVENC(t *testing.T) {
	m := NewAV1CommonMapping()

	tests := []struct {
		input    string
		expected string
	}{
		{"ultrafast", "p1"},
		{"medium", "p6"},
		{"slow", "p7"},
		{"veryslow", "p7"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result, err := m.GetCommonValueConversion(tt.input, "preset_to_nvenc_av1")
			if err != nil {
				t.Errorf("GetCommonValueConversion() error = %v", err)
			}
			if result != tt.expected {
				t.Errorf("GetCommonValueConversion() = %v, want %v", result, tt.expected)
			}
		})
	}
}

func TestAV1CommonMapping_ValueConversion_SpeedToNVENC(t *testing.T) {
	m := NewAV1CommonMapping()

	tests := []struct {
		input    string
		expected string
	}{
		{"0", "p7"},  // slowest/best quality
		{"7", "p5"},  // medium
		{"13", "p1"}, // fastest
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result, err := m.GetCommonValueConversion(tt.input, "speed_to_nvenc_av1")
			if err != nil {
				t.Errorf("GetCommonValueConversion() error = %v", err)
			}
			if result != tt.expected {
				t.Errorf("GetCommonValueConversion() = %v, want %v", result, tt.expected)
			}
		})
	}
}

func TestAV1CommonMapping_ValueConversion_ProfileMapping(t *testing.T) {
	m := NewAV1CommonMapping()

	tests := []struct {
		input    string
		expected string
	}{
		{"main", "main"},
		{"high", "high"},
		{"professional", "professional"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result, err := m.GetCommonValueConversion(tt.input, "profile_mapping_av1")
			if err != nil {
				t.Errorf("GetCommonValueConversion() error = %v", err)
			}
			if result != tt.expected {
				t.Errorf("GetCommonValueConversion() = %v, want %v", result, tt.expected)
			}
		})
	}
}

func TestAV1CommonMapping_ValueConversion_LevelMapping(t *testing.T) {
	m := NewAV1CommonMapping()

	tests := []struct {
		input    string
		expected string
	}{
		{"2.0", "2.0"},
		{"3.0", "3.0"},
		{"4.0", "4.0"},
		{"5.0", "5.0"},
		{"5.1", "5.1"},
		{"6.0", "6.0"},
		{"7.0", "7.0"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result, err := m.GetCommonValueConversion(tt.input, "level_mapping_av1")
			if err != nil {
				t.Errorf("GetCommonValueConversion() error = %v", err)
			}
			if result != tt.expected {
				t.Errorf("GetCommonValueConversion() = %v, want %v", result, tt.expected)
			}
		})
	}
}

func TestAV1CommonMapping_ValidateParameterRange_CRF(t *testing.T) {
	m := NewAV1CommonMapping()

	// Valid CRF values (AV1 uses 0-63 range)
	validValues := []string{"0", "1", "30", "63"}
	for _, v := range validValues {
		err := m.ValidateParameterRange("crf_av1", v)
		if err != nil {
			t.Errorf("ValidateParameterRange(crf_av1, %s) returned error: %v", v, err)
		}
	}

	// Invalid CRF values
	invalidValues := []string{"-1", "64", "100"}
	for _, v := range invalidValues {
		err := m.ValidateParameterRange("crf_av1", v)
		if err == nil {
			t.Errorf("ValidateParameterRange(crf_av1, %s) should return error", v)
		}
	}
}

func TestAV1CommonMapping_ValidateParameterRange_Speed(t *testing.T) {
	m := NewAV1CommonMapping()

	// Valid speed values (0-13 for SVT-AV1)
	validValues := []string{"0", "1", "7", "13"}
	for _, v := range validValues {
		err := m.ValidateParameterRange("speed_av1", v)
		if err != nil {
			t.Errorf("ValidateParameterRange(speed_av1, %s) returned error: %v", v, err)
		}
	}

	// Invalid speed value
	err := m.ValidateParameterRange("speed_av1", "14")
	if err == nil {
		t.Error("ValidateParameterRange(speed_av1, 14) should return error")
	}
}

func TestAV1CommonMapping_ValidateParameterRange_Profile(t *testing.T) {
	m := NewAV1CommonMapping()

	// Valid profile values
	validValues := []string{"main", "high", "professional"}
	for _, v := range validValues {
		err := m.ValidateParameterRange("profile_av1", v)
		if err != nil {
			t.Errorf("ValidateParameterRange(profile_av1, %s) returned error: %v", v, err)
		}
	}

	// Invalid profile value
	err := m.ValidateParameterRange("profile_av1", "invalid")
	if err == nil {
		t.Error("ValidateParameterRange(profile_av1, invalid) should return error")
	}
}

func TestAV1CommonMapping_ConvertParameter(t *testing.T) {
	m := NewAV1CommonMapping()

	// Test CRF conversion to VAAPI
	targetParam, convertedValue, err := m.ConvertParameter(EncoderAV1VAAPI, "crf", "30")
	if err != nil {
		t.Fatalf("ConvertParameter() error = %v", err)
	}
	if targetParam != "quality" {
		t.Errorf("Target param = %v, want quality", targetParam)
	}
	if convertedValue != "53" {
		t.Errorf("Converted value = %v, want 53", convertedValue)
	}

	// Test CRF conversion to NVENC (direct pass-through)
	targetParam, convertedValue, err = m.ConvertParameter(EncoderAV1NVENC, "crf", "35")
	if err != nil {
		t.Fatalf("ConvertParameter() error = %v", err)
	}
	if targetParam != "cq" {
		t.Errorf("Target param = %v, want cq", targetParam)
	}
	if convertedValue != "35" {
		t.Errorf("Converted value = %v, want 35", convertedValue)
	}

	// Test preset conversion to NVENC
	targetParam, convertedValue, err = m.ConvertParameter(EncoderAV1NVENC, "preset", "slow")
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
	_, _, err = m.ConvertParameter(EncoderAV1NVENC, "nonexistent", "value")
	if err == nil {
		t.Error("ConvertParameter() should return error for unsupported parameter")
	}
}

func TestAV1CommonMapping_GetSupportedEncoders(t *testing.T) {
	m := NewAV1CommonMapping()

	encoders := m.GetSupportedEncoders()
	if len(encoders) != 4 {
		t.Errorf("GetSupportedEncoders() returned %d encoders, want 4", len(encoders))
	}

	// Verify expected encoders are present
	expectedEncoders := []EncoderFamily{
		EncoderAV1NVENC, EncoderAV1QSV, EncoderAV1VAAPI, EncoderLibAOM,
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

func TestAV1CommonMapping_IsParameterSupported(t *testing.T) {
	m := NewAV1CommonMapping()

	// Supported parameters
	if !m.IsParameterSupported(EncoderAV1NVENC, "crf") {
		t.Error("crf should be supported for AV1 NVENC")
	}
	if !m.IsParameterSupported(EncoderAV1QSV, "preset") {
		t.Error("preset should be supported for AV1 QSV")
	}
	if !m.IsParameterSupported(EncoderLibAOM, "tile-columns") {
		t.Error("tile-columns should be supported for libaom-av1")
	}

	// Unsupported parameters
	if m.IsParameterSupported(EncoderAV1NVENC, "nonexistent") {
		t.Error("nonexistent should not be supported")
	}
}

func TestAV1CRFToVAAPIQuality(t *testing.T) {
	tests := []struct {
		crf      int
		expected int
	}{
		{0, 100},
		{30, 53}, // rounded
		{63, 0},
	}

	for _, tt := range tests {
		result := av1CRFToVAAPIQuality(tt.crf)
		if result != tt.expected {
			t.Errorf("av1CRFToVAAPIQuality(%d) = %d, want %d", tt.crf, result, tt.expected)
		}
	}
}
