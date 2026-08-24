package encoder

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadMappingConfig_JSON(t *testing.T) {
	// Create a temporary JSON config file
	configContent := `{
		"version": "1.0",
		"family_mappings": {
			"libx264": "h264",
			"h264_nvenc": "h264"
		},
		"parameter_translations": {
			"libx264:h264_nvenc": [
				{
					"source_param": "crf",
					"target_param": "cq",
					"description": "CRF to constant quality"
				},
				{
					"source_param": "preset",
					"target_param": "preset",
					"converter_ref": "x264_preset_to_nvenc",
					"description": "Preset mapping"
				}
			]
		},
		"hardware_params": {
			"h264_nvenc:nvidia": [
				{"param": "rc", "value": "constqp", "description": "Rate control mode"}
			]
		},
		"value_converter_refs": {
			"x264_preset_to_nvenc": "x264_preset_to_nvenc"
		}
	}`

	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "test_config.json")
	if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
		t.Fatalf("Failed to write test config: %v", err)
	}

	config, err := LoadMappingConfig(configPath)
	if err != nil {
		t.Fatalf("LoadMappingConfig() error = %v", err)
	}

	// Verify version
	if config.Version != "1.0" {
		t.Errorf("Version = %v, want 1.0", config.Version)
	}

	// Verify family mappings
	if len(config.FamilyMappings) != 2 {
		t.Errorf("FamilyMappings count = %d, want 2", len(config.FamilyMappings))
	}
	if config.FamilyMappings["libx264"] != "h264" {
		t.Errorf("FamilyMappings[libx264] = %v, want h264", config.FamilyMappings["libx264"])
	}

	// Verify parameter translations
	if len(config.ParameterTranslations) != 1 {
		t.Errorf("ParameterTranslations count = %d, want 1", len(config.ParameterTranslations))
	}
	rules := config.ParameterTranslations["libx264:h264_nvenc"]
	if len(rules) != 2 {
		t.Errorf("Rules count = %d, want 2", len(rules))
	}

	// Verify hardware params
	if len(config.HardwareParams) != 1 {
		t.Errorf("HardwareParams count = %d, want 1", len(config.HardwareParams))
	}
	hwParams := config.HardwareParams["h264_nvenc:nvidia"]
	if len(hwParams) != 1 {
		t.Errorf("HardwareParams rules count = %d, want 1", len(hwParams))
	}
}

func TestLoadMappingConfig_InvalidExtension(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "test_config.txt")
	if err := os.WriteFile(configPath, []byte("test"), 0644); err != nil {
		t.Fatalf("Failed to write test config: %v", err)
	}

	_, err := LoadMappingConfig(configPath)
	if err == nil {
		t.Error("LoadMappingConfig() should return error for unsupported extension")
	}
}

func TestLoadMappingConfig_NonExistent(t *testing.T) {
	_, err := LoadMappingConfig("/nonexistent/path/config.json")
	if err == nil {
		t.Error("LoadMappingConfig() should return error for non-existent file")
	}
}

func TestLoadMappingConfig_InvalidJSON(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "invalid.json")
	if err := os.WriteFile(configPath, []byte("{invalid json}"), 0644); err != nil {
		t.Fatalf("Failed to write test config: %v", err)
	}

	_, err := LoadMappingConfig(configPath)
	if err == nil {
		t.Error("LoadMappingConfig() should return error for invalid JSON")
	}
}

func TestLoadMappingConfigFromBytes_JSON(t *testing.T) {
	configContent := `{
		"version": "1.0",
		"family_mappings": {
			"libx264": "h264"
		}
	}`

	config, err := LoadMappingConfigFromBytes([]byte(configContent), "json")
	if err != nil {
		t.Fatalf("LoadMappingConfigFromBytes() error = %v", err)
	}

	if config.Version != "1.0" {
		t.Errorf("Version = %v, want 1.0", config.Version)
	}
}

func TestLoadMappingConfigFromBytes_YAML(t *testing.T) {
	// YAML should return an error since we don't have the yaml dependency
	_, err := LoadMappingConfigFromBytes([]byte("version: \"1.0\""), "yaml")
	if err == nil {
		t.Error("LoadMappingConfigFromBytes() should return error for YAML without dependency")
	}
}

func TestLoadMappingConfigFromBytes_InvalidFormat(t *testing.T) {
	_, err := LoadMappingConfigFromBytes([]byte("test"), "xml")
	if err == nil {
		t.Error("LoadMappingConfigFromBytes() should return error for invalid format")
	}
}

func TestMappingConfig_ApplyTo(t *testing.T) {
	config := &MappingConfig{
		Version: "1.0",
		FamilyMappings: map[string]string{
			"libx264":    "h264",
			"h264_nvenc": "h264",
		},
		ParameterTranslations: map[string][]ParameterRuleConfig{
			"libx264:h264_nvenc": {
				{
					SourceParam: "crf",
					TargetParam: "cq",
				},
				{
					SourceParam: "preset",
					TargetParam: "preset",
					ValueMap: map[string]string{
						"slow": "p7",
						"fast": "p5",
					},
				},
			},
		},
		HardwareParams: map[string][]HardwareParamRule{
			"h264_nvenc:nvidia": {
				{Param: "rc", Value: "constqp"},
			},
		},
	}

	m := NewEncoderMapping()
	if err := config.ApplyTo(m); err != nil {
		t.Fatalf("ApplyTo() error = %v", err)
	}

	// Verify family mappings
	format, exists := m.GetCodecFormat(EncoderLibX264)
	if !exists || format != CodecH264 {
		t.Errorf("GetCodecFormat(libx264) = %v, exists=%v, want h264, true", format, exists)
	}

	// Verify parameter translations
	rule := m.GetParameterTranslation(EncoderLibX264, EncoderH264NVENC, "crf")
	if rule == nil || rule.TargetParam != "cq" {
		t.Errorf("GetParameterTranslation(crf) failed")
	}

	// Verify value map converter
	rule = m.GetParameterTranslation(EncoderLibX264, EncoderH264NVENC, "preset")
	if rule == nil {
		t.Fatal("GetParameterTranslation(preset) returned nil")
	}
	if rule.Converter == nil {
		t.Fatal("Preset rule should have converter from ValueMap")
	}
	result, err := rule.Convert("slow")
	if err != nil {
		t.Fatalf("Converter error = %v", err)
	}
	if result != "p7" {
		t.Errorf("Converter(slow) = %v, want p7", result)
	}

	// Verify hardware params
	hwParams := m.GetHardwareParams(EncoderH264NVENC, GPUVendorNVIDIA)
	if len(hwParams) != 1 {
		t.Errorf("GetHardwareParams count = %d, want 1", len(hwParams))
	}
}

func TestMappingConfig_ApplyTo_InvalidTranslationKey(t *testing.T) {
	config := &MappingConfig{
		ParameterTranslations: map[string][]ParameterRuleConfig{
			"invalid_key_without_colon": {},
		},
	}

	m := NewEncoderMapping()
	err := config.ApplyTo(m)
	if err == nil {
		t.Error("ApplyTo() should return error for invalid translation key")
	}
}

func TestMappingConfig_ApplyTo_InvalidHardwareKey(t *testing.T) {
	config := &MappingConfig{
		HardwareParams: map[string][]HardwareParamRule{
			"invalid_key_without_colon": {},
		},
	}

	m := NewEncoderMapping()
	err := config.ApplyTo(m)
	if err == nil {
		t.Error("ApplyTo() should return error for invalid hardware key")
	}
}

func TestLoadAndApplyMapping(t *testing.T) {
	configContent := `{
		"version": "1.0",
		"family_mappings": {
			"libx264": "h264",
			"h264_nvenc": "h264"
		},
		"parameter_translations": {
			"libx264:h264_nvenc": [
				{"source_param": "crf", "target_param": "cq"}
			]
		}
	}`

	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "test.json")
	if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
		t.Fatalf("Failed to write test config: %v", err)
	}

	m, err := LoadAndApplyMapping(configPath)
	if err != nil {
		t.Fatalf("LoadAndApplyMapping() error = %v", err)
	}

	if !m.CanTranslate(EncoderLibX264, EncoderH264NVENC) {
		t.Error("Mapping should be able to translate libx264 to h264_nvenc")
	}
}

func TestGetPredefinedConverter(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		value    string
		expected string
	}{
		{"x264_preset_to_nvenc", "x264_preset_to_nvenc", "slow", "p7"},
		{"x265_preset_to_nvenc", "x265_preset_to_nvenc", "medium", "p6"},
		{"crf_to_cq", "crf_to_cq", "23", "23"},
		{"crf_to_global_quality", "crf_to_global_quality", "28", "28"},
		{"nonexistent", "nonexistent_converter", "test", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			converter := getPredefinedConverter(tt.input)
			if tt.expected == "" {
				if converter != nil {
					t.Error("Expected nil converter for nonexistent name")
				}
				return
			}
			if converter == nil {
				t.Fatalf("getPredefinedConverter(%s) returned nil", tt.input)
			}
			result, err := converter(tt.value)
			if err != nil {
				t.Fatalf("Converter error = %v", err)
			}
			if result != tt.expected {
				t.Errorf("Converter result = %v, want %v", result, tt.expected)
			}
		})
	}
}

func TestCRFToVAAPIQualityConverter(t *testing.T) {
	tests := []struct {
		crf      string
		expected string
	}{
		{"0", "100"},
		{"23", "55"},
		{"51", "0"},
	}

	for _, tt := range tests {
		t.Run(tt.crf, func(t *testing.T) {
			result, err := crfToVAAPIQualityConverter(tt.crf)
			if err != nil {
				t.Fatalf("crfToVAAPIQualityConverter error = %v", err)
			}
			if result != tt.expected {
				t.Errorf("crfToVAAPIQualityConverter(%s) = %v, want %v", tt.crf, result, tt.expected)
			}
		})
	}
}

func TestCRFToVAAPIQualityConverter_InvalidInput(t *testing.T) {
	_, err := crfToVAAPIQualityConverter("not_a_number")
	if err == nil {
		t.Error("crfToVAAPIQualityConverter should return error for non-numeric input")
	}
}

func TestSvtav1PresetToNVENC(t *testing.T) {
	validTests := []struct {
		input    string
		expected string
	}{
		{"0", "p7"},
		{"4", "p6"},
		{"7", "p5"},
		{"10", "p3"},
		{"13", "p1"},
	}

	for _, tt := range validTests {
		t.Run(tt.input, func(t *testing.T) {
			result, err := svtav1PresetToNVENC(tt.input)
			if err != nil {
				t.Fatalf("svtav1PresetToNVENC error = %v", err)
			}
			if result != tt.expected {
				t.Errorf("svtav1PresetToNVENC(%s) = %v, want %v", tt.input, result, tt.expected)
			}
		})
	}

	// Out-of-range or non-numeric presets must be rejected, never passed
	// through as illegal av1_nvenc values.
	for _, input := range []string{"unknown", "14", "-1", "99", ""} {
		t.Run("invalid/"+input, func(t *testing.T) {
			if _, err := svtav1PresetToNVENC(input); err == nil {
				t.Errorf("svtav1PresetToNVENC(%s) expected error, got nil", input)
			}
		})
	}
}
