package encoder

import (
	"testing"
)

func TestNewVP9CommonMapping(t *testing.T) {
	m := NewVP9CommonMapping()
	if m == nil {
		t.Fatal("NewVP9CommonMapping() returned nil")
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

func TestVP9CommonMapping_StandardParams(t *testing.T) {
	m := NewVP9CommonMapping()

	expectedParams := []string{"crf", "qp", "cpu-used", "b:v", "maxrate", "bufsize",
		"g", "profile", "row-mt", "tile-columns", "tile-rows"}

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

	// Verify specific defaults (VP9 uses 31 as default CRF)
	if m.StandardParams["crf"].DefaultValue != "31" {
		t.Errorf("CRF default = %v, want 31", m.StandardParams["crf"].DefaultValue)
	}
	if m.StandardParams["cpu-used"].DefaultValue != "1" {
		t.Errorf("cpu-used default = %v, want 1", m.StandardParams["cpu-used"].DefaultValue)
	}
}

func TestVP9CommonMapping_NameTranslations_NVENC(t *testing.T) {
	m := NewVP9CommonMapping()

	expectedTranslations := map[string]string{
		"crf":     "cq",
		"qp":      "qp",
		"b:v":     "b:v",
		"maxrate": "maxrate",
		"bufsize": "bufsize",
		"g":       "g",
		"profile": "profile",
	}

	for source, expected := range expectedTranslations {
		result := m.GetCommonParameterTranslation(EncoderVP9NVENC, source)
		if result != expected {
			t.Errorf("NVENC translation for '%s' = %v, want %v", source, result, expected)
		}
	}
}

func TestVP9CommonMapping_NameTranslations_QSV(t *testing.T) {
	m := NewVP9CommonMapping()

	expectedTranslations := map[string]string{
		"crf":     "global_quality",
		"qp":      "qp",
		"b:v":     "b:v",
		"maxrate": "maxrate",
		"g":       "g",
		"profile": "profile",
	}

	for source, expected := range expectedTranslations {
		result := m.GetCommonParameterTranslation(EncoderVP9QSV, source)
		if result != expected {
			t.Errorf("QSV translation for '%s' = %v, want %v", source, result, expected)
		}
	}
}

func TestVP9CommonMapping_NameTranslations_VAAPI(t *testing.T) {
	m := NewVP9CommonMapping()

	expectedTranslations := map[string]string{
		"crf":     "quality",
		"qp":      "qp",
		"b:v":     "b:v",
		"maxrate": "maxrate",
		"g":       "g",
		"profile": "profile",
	}

	for source, expected := range expectedTranslations {
		result := m.GetCommonParameterTranslation(EncoderVP9VAAPI, source)
		if result != expected {
			t.Errorf("VAAPI translation for '%s' = %v, want %v", source, result, expected)
		}
	}
}

func TestVP9CommonMapping_ValueConversion_CRFToVAAPIQuality(t *testing.T) {
	m := NewVP9CommonMapping()

	tests := []struct {
		input    string
		expected string
	}{
		{"0", "100"}, // CRF 0 = lossless -> quality 100
		{"31", "51"}, // CRF 31 (default) -> quality ~51
		{"63", "0"},  // CRF 63 = worst -> quality 0
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result, err := m.GetCommonValueConversion(tt.input, "crf_to_quality_vp9")
			if err != nil {
				t.Errorf("GetCommonValueConversion() error = %v", err)
			}
			if result != tt.expected {
				t.Errorf("GetCommonValueConversion() = %v, want %v", result, tt.expected)
			}
		})
	}
}

func TestVP9CommonMapping_ValueConversion_ProfileMapping(t *testing.T) {
	m := NewVP9CommonMapping()

	tests := []struct {
		input    string
		expected string
	}{
		{"0", "0"},
		{"1", "1"},
		{"2", "2"},
		{"3", "3"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result, err := m.GetCommonValueConversion(tt.input, "profile_mapping_vp9")
			if err != nil {
				t.Errorf("GetCommonValueConversion() error = %v", err)
			}
			if result != tt.expected {
				t.Errorf("GetCommonValueConversion() = %v, want %v", result, tt.expected)
			}
		})
	}
}

func TestVP9CommonMapping_ValidateParameterRange_CRF(t *testing.T) {
	m := NewVP9CommonMapping()

	// Valid CRF values (VP9 uses 0-63 range)
	validValues := []string{"0", "1", "31", "63"}
	for _, v := range validValues {
		err := m.ValidateParameterRange("crf_vp9", v)
		if err != nil {
			t.Errorf("ValidateParameterRange(crf_vp9, %s) returned error: %v", v, err)
		}
	}

	// Invalid CRF values
	invalidValues := []string{"-1", "64", "100"}
	for _, v := range invalidValues {
		err := m.ValidateParameterRange("crf_vp9", v)
		if err == nil {
			t.Errorf("ValidateParameterRange(crf_vp9, %s) should return error", v)
		}
	}
}

func TestVP9CommonMapping_ValidateParameterRange_CPUUsed(t *testing.T) {
	m := NewVP9CommonMapping()

	// Valid cpu-used values (0-5)
	validValues := []string{"0", "1", "2", "3", "4", "5"}
	for _, v := range validValues {
		err := m.ValidateParameterRange("cpu-used", v)
		if err != nil {
			t.Errorf("ValidateParameterRange(cpu-used, %s) returned error: %v", v, err)
		}
	}

	// Invalid cpu-used value
	err := m.ValidateParameterRange("cpu-used", "6")
	if err == nil {
		t.Error("ValidateParameterRange(cpu-used, 6) should return error")
	}
}

func TestVP9CommonMapping_ValidateParameterRange_Profile(t *testing.T) {
	m := NewVP9CommonMapping()

	// Valid profile values
	validValues := []string{"0", "1", "2", "3"}
	for _, v := range validValues {
		err := m.ValidateParameterRange("profile_vp9", v)
		if err != nil {
			t.Errorf("ValidateParameterRange(profile_vp9, %s) returned error: %v", v, err)
		}
	}

	// Invalid profile value
	err := m.ValidateParameterRange("profile_vp9", "4")
	if err == nil {
		t.Error("ValidateParameterRange(profile_vp9, 4) should return error")
	}
}

func TestVP9CommonMapping_ConvertParameter(t *testing.T) {
	m := NewVP9CommonMapping()

	// Test CRF conversion to VAAPI
	targetParam, convertedValue, err := m.ConvertParameter(EncoderVP9VAAPI, "crf", "31")
	if err != nil {
		t.Fatalf("ConvertParameter() error = %v", err)
	}
	if targetParam != "quality" {
		t.Errorf("Target param = %v, want quality", targetParam)
	}
	if convertedValue != "51" {
		t.Errorf("Converted value = %v, want 51", convertedValue)
	}

	// Test CRF conversion to NVENC (direct pass-through)
	targetParam, convertedValue, err = m.ConvertParameter(EncoderVP9NVENC, "crf", "40")
	if err != nil {
		t.Fatalf("ConvertParameter() error = %v", err)
	}
	if targetParam != "cq" {
		t.Errorf("Target param = %v, want cq", targetParam)
	}
	if convertedValue != "40" {
		t.Errorf("Converted value = %v, want 40", convertedValue)
	}

	// Test unsupported parameter
	_, _, err = m.ConvertParameter(EncoderVP9NVENC, "nonexistent", "value")
	if err == nil {
		t.Error("ConvertParameter() should return error for unsupported parameter")
	}
}

func TestVP9CommonMapping_GetSupportedEncoders(t *testing.T) {
	m := NewVP9CommonMapping()

	encoders := m.GetSupportedEncoders()
	if len(encoders) != 3 {
		t.Errorf("GetSupportedEncoders() returned %d encoders, want 3", len(encoders))
	}

	// Verify expected encoders are present
	expectedEncoders := []EncoderFamily{
		EncoderVP9NVENC, EncoderVP9QSV, EncoderVP9VAAPI,
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

func TestVP9CommonMapping_IsParameterSupported(t *testing.T) {
	m := NewVP9CommonMapping()

	// Supported parameters
	if !m.IsParameterSupported(EncoderVP9NVENC, "crf") {
		t.Error("crf should be supported for VP9 NVENC")
	}
	if !m.IsParameterSupported(EncoderVP9QSV, "crf") {
		t.Error("crf should be supported for VP9 QSV")
	}

	// Unsupported parameters
	if m.IsParameterSupported(EncoderVP9NVENC, "nonexistent") {
		t.Error("nonexistent should not be supported")
	}
}

func TestVP9CRFToVAAPIQuality(t *testing.T) {
	tests := []struct {
		crf      int
		expected int
	}{
		{0, 100},
		{31, 51},
		{63, 0},
	}

	for _, tt := range tests {
		result := vp9CRFToVAAPIQuality(tt.crf)
		if result != tt.expected {
			t.Errorf("vp9CRFToVAAPIQuality(%d) = %d, want %d", tt.crf, result, tt.expected)
		}
	}
}
