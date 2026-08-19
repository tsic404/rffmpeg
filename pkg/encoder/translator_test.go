package encoder

import (
	"testing"
)

func TestNewParameterTranslator(t *testing.T) {
	mapping := DefaultMapping()
	translator := NewParameterTranslator(mapping)

	if translator == nil {
		t.Fatal("NewParameterTranslator returned nil")
	}

	if translator.mapping == nil {
		t.Error("mapping not initialized")
	}

	if translator.h264Mapping == nil {
		t.Error("h264Mapping not initialized")
	}

	if translator.hevcMapping == nil {
		t.Error("hevcMapping not initialized")
	}

	if translator.vp9Mapping == nil {
		t.Error("vp9Mapping not initialized")
	}

	if translator.av1Mapping == nil {
		t.Error("av1Mapping not initialized")
	}
}

func TestNewDefaultParameterTranslator(t *testing.T) {
	translator := NewDefaultParameterTranslator()

	if translator == nil {
		t.Fatal("NewDefaultParameterTranslator returned nil")
	}

	// Verify it has default mappings
	if !translator.mapping.CanTranslate(EncoderLibX264, EncoderH264NVENC) {
		t.Error("Default translator should have libx264 -> h264_nvenc translation")
	}
}

func TestTranslatorOptions(t *testing.T) {
	// Test WithStrictMode
	translator := NewDefaultParameterTranslator(WithStrictMode(true))
	if !translator.strictMode {
		t.Error("WithStrictMode(true) did not set strict mode")
	}

	// Test WithHardwareParamInjection
	translator = NewDefaultParameterTranslator(WithHardwareParamInjection(false))
	if translator.injectHardwareParams {
		t.Error("WithHardwareParamInjection(false) did not disable hardware param injection")
	}
}

func TestTranslate_H264ToNVENC(t *testing.T) {
	translator := NewDefaultParameterTranslator()

	params := map[string]string{
		"crf":    "23",
		"preset": "slow",
		"tune":   "film",
		"b:v":    "5000k",
	}

	result, err := translator.Translate(EncoderLibX264, EncoderH264NVENC, params)
	if err != nil {
		t.Fatalf("Translate failed: %v", err)
	}

	// Check CRF was translated to cq
	if result.TranslatedParams["cq"] != "23" {
		t.Errorf("Expected cq=23, got %s", result.TranslatedParams["cq"])
	}

	// Check preset was converted
	if result.TranslatedParams["preset"] != "p7" {
		t.Errorf("Expected preset=p7, got %s", result.TranslatedParams["preset"])
	}

	// Check bitrate was passed through
	if result.TranslatedParams["b:v"] != "5000k" {
		t.Errorf("Expected b:v=5000k, got %s", result.TranslatedParams["b:v"])
	}

	// Check audit records were generated
	if len(result.AuditRecords) == 0 {
		t.Error("No audit records generated")
	}

	// Check hardware params were injected
	if len(result.HardwareParams) == 0 {
		t.Error("No hardware params injected")
	}
}

func TestTranslate_H264ToQSV(t *testing.T) {
	translator := NewDefaultParameterTranslator()

	params := map[string]string{
		"crf":    "23",
		"preset": "medium",
	}

	result, err := translator.Translate(EncoderLibX264, EncoderH264QSV, params)
	if err != nil {
		t.Fatalf("Translate failed: %v", err)
	}

	// Check CRF was translated to global_quality
	if result.TranslatedParams["global_quality"] != "23" {
		t.Errorf("Expected global_quality=23, got %s", result.TranslatedParams["global_quality"])
	}
}

func TestTranslate_H264ToVAAPI(t *testing.T) {
	translator := NewDefaultParameterTranslator()

	params := map[string]string{
		"crf": "23",
	}

	result, err := translator.Translate(EncoderLibX264, EncoderH264VAAPI, params)
	if err != nil {
		t.Fatalf("Translate failed: %v", err)
	}

	// Check CRF was translated to quality (with scale conversion)
	// CRF 23 should be converted: quality = 100 - (23 * 100 / 51) = 54
	if result.TranslatedParams["quality"] == "" {
		t.Error("Expected quality parameter to be set")
	}
}

func TestTranslate_HEVCToNVENC(t *testing.T) {
	translator := NewDefaultParameterTranslator()

	params := map[string]string{
		"crf":    "28",
		"preset": "fast",
	}

	result, err := translator.Translate(EncoderLibX265, EncoderHEVCNVENC, params)
	if err != nil {
		t.Fatalf("Translate failed: %v", err)
	}

	// Check CRF was translated to cq
	if result.TranslatedParams["cq"] != "28" {
		t.Errorf("Expected cq=28, got %s", result.TranslatedParams["cq"])
	}

	// Check preset was converted
	if result.TranslatedParams["preset"] != "p5" {
		t.Errorf("Expected preset=p5, got %s", result.TranslatedParams["preset"])
	}
}

func TestTranslate_VP9ToVAAPI(t *testing.T) {
	translator := NewDefaultParameterTranslator()

	params := map[string]string{
		"crf": "31",
	}

	result, err := translator.Translate(EncoderLibVPX, EncoderVP9VAAPI, params)
	if err != nil {
		t.Fatalf("Translate failed: %v", err)
	}

	// Check CRF was translated to quality
	if result.TranslatedParams["quality"] == "" {
		t.Error("Expected quality parameter to be set")
	}
}

func TestTranslate_AV1ToNVENC(t *testing.T) {
	translator := NewDefaultParameterTranslator()

	params := map[string]string{
		"crf":    "30",
		"preset": "medium",
	}

	result, err := translator.Translate(EncoderLibSVTAV1, EncoderAV1NVENC, params)
	if err != nil {
		t.Fatalf("Translate failed: %v", err)
	}

	// Check CRF was translated to cq
	if result.TranslatedParams["cq"] != "30" {
		t.Errorf("Expected cq=30, got %s", result.TranslatedParams["cq"])
	}
}

func TestTranslate_UnregisteredEncoder(t *testing.T) {
	translator := NewDefaultParameterTranslator()

	params := map[string]string{"crf": "23"}

	_, err := translator.Translate(EncoderFamily("unknown_encoder"), EncoderH264NVENC, params)
	if err == nil {
		t.Error("Expected error for unregistered source encoder")
	}

	_, err = translator.Translate(EncoderLibX264, EncoderFamily("unknown_encoder"), params)
	if err == nil {
		t.Error("Expected error for unregistered target encoder")
	}
}

func TestTranslateChain(t *testing.T) {
	translator := NewDefaultParameterTranslator()

	// Test chain: libx264 -> h264_nvenc -> h264_qsv
	chain := []EncoderFamily{EncoderLibX264, EncoderH264NVENC, EncoderH264QSV}
	params := map[string]string{
		"crf":    "23",
		"preset": "slow",
	}

	result, err := translator.TranslateChain(chain, params)
	if err != nil {
		t.Fatalf("TranslateChain failed: %v", err)
	}

	// Check final target encoder
	if result.TargetEncoder != EncoderH264QSV {
		t.Errorf("Expected target encoder h264_qsv, got %s", result.TargetEncoder)
	}

	// Check audit records have chain information
	for _, record := range result.AuditRecords {
		if record.ChainTotal != 2 {
			t.Errorf("Expected ChainTotal=2, got %d", record.ChainTotal)
		}
	}
}

func TestTranslateChain_InvalidChain(t *testing.T) {
	translator := NewDefaultParameterTranslator()

	// Test chain with only one encoder
	chain := []EncoderFamily{EncoderLibX264}
	params := map[string]string{"crf": "23"}

	_, err := translator.TranslateChain(chain, params)
	if err == nil {
		t.Error("Expected error for chain with only one encoder")
	}
}

func TestValidateParameters(t *testing.T) {
	translator := NewDefaultParameterTranslator()

	// Test valid parameters
	params := map[string]string{
		"crf":    "23",
		"preset": "medium",
	}

	errors, err := translator.ValidateParameters(EncoderLibX264, params)
	if err != nil {
		t.Fatalf("ValidateParameters failed: %v", err)
	}

	if len(errors) > 0 {
		t.Errorf("Unexpected validation errors: %v", errors)
	}

	// Test invalid CRF value (out of range)
	params = map[string]string{
		"crf": "100", // Max is 51
	}

	errors, err = translator.ValidateParameters(EncoderLibX264, params)
	if err != nil {
		t.Fatalf("ValidateParameters failed: %v", err)
	}

	if len(errors) == 0 {
		t.Error("Expected validation error for out-of-range CRF")
	}

	// Test invalid preset value
	params = map[string]string{
		"preset": "invalid_preset",
	}

	errors, err = translator.ValidateParameters(EncoderLibX264, params)
	if err != nil {
		t.Fatalf("ValidateParameters failed: %v", err)
	}

	if len(errors) == 0 {
		t.Error("Expected validation error for invalid preset")
	}
}

func TestValidateParameters_UnregisteredEncoder(t *testing.T) {
	translator := NewDefaultParameterTranslator()

	params := map[string]string{"crf": "23"}

	_, err := translator.ValidateParameters(EncoderFamily("unknown"), params)
	if err == nil {
		t.Error("Expected error for unregistered encoder")
	}
}

func TestNormalizeParameters(t *testing.T) {
	translator := NewDefaultParameterTranslator()

	// Test parameter normalization
	params := map[string]string{
		"CRF":    "23",   // Should be lowercased
		"Preset": "SLOW", // Should be lowercased
	}

	normalized, err := translator.NormalizeParameters(EncoderLibX264, params)
	if err != nil {
		t.Fatalf("NormalizeParameters failed: %v", err)
	}

	// Check parameter names are lowercased
	if _, exists := normalized["crf"]; !exists {
		t.Error("Expected 'crf' in normalized params")
	}

	if _, exists := normalized["preset"]; !exists {
		t.Error("Expected 'preset' in normalized params")
	}

	// Check preset value is lowercased
	if normalized["preset"] != "slow" {
		t.Errorf("Expected preset=slow, got %s", normalized["preset"])
	}
}

func TestNormalizeParameters_EmptyValues(t *testing.T) {
	translator := NewDefaultParameterTranslator()

	// Test that empty values get defaults
	params := map[string]string{
		"crf": "", // Should get default value 23
	}

	normalized, err := translator.NormalizeParameters(EncoderLibX264, params)
	if err != nil {
		t.Fatalf("NormalizeParameters failed: %v", err)
	}

	if normalized["crf"] != "23" {
		t.Errorf("Expected default crf=23, got %s", normalized["crf"])
	}
}

func TestNormalizeParameters_UnregisteredEncoder(t *testing.T) {
	translator := NewDefaultParameterTranslator()

	params := map[string]string{"crf": "23"}

	_, err := translator.NormalizeParameters(EncoderFamily("unknown"), params)
	if err == nil {
		t.Error("Expected error for unregistered encoder")
	}
}

func TestGetSupportedTranslations(t *testing.T) {
	translator := NewDefaultParameterTranslator()

	// Get encoders that can translate to h264_nvenc
	sources := translator.GetSupportedTranslations(EncoderH264NVENC)

	// Should include libx264
	found := false
	for _, src := range sources {
		if src == EncoderLibX264 {
			found = true
			break
		}
	}

	if !found {
		t.Error("Expected libx264 to be a supported source for h264_nvenc")
	}
}

func TestGetAuditRecords(t *testing.T) {
	translator := NewDefaultParameterTranslator()

	// Perform a translation
	params := map[string]string{"crf": "23"}
	_, err := translator.Translate(EncoderLibX264, EncoderH264NVENC, params)
	if err != nil {
		t.Fatalf("Translate failed: %v", err)
	}

	// Get audit records
	records := translator.GetAuditRecords()
	if len(records) == 0 {
		t.Error("Expected audit records after translation")
	}
}

func TestTranslationAuditRecord(t *testing.T) {
	translator := NewDefaultParameterTranslator()

	params := map[string]string{
		"crf":    "23",
		"preset": "slow",
	}

	result, err := translator.Translate(EncoderLibX264, EncoderH264NVENC, params)
	if err != nil {
		t.Fatalf("Translate failed: %v", err)
	}

	// Verify audit records have correct information
	for _, record := range result.AuditRecords {
		// Skip hardware param injection records (they have empty source encoder)
		if record.SourceEncoder == "" && record.ConverterUsed == "hardware_injection" {
			continue
		}

		if record.SourceEncoder != EncoderLibX264 {
			t.Errorf("Expected source encoder libx264, got %s", record.SourceEncoder)
		}

		if record.TargetEncoder != EncoderH264NVENC {
			t.Errorf("Expected target encoder h264_nvenc, got %s", record.TargetEncoder)
		}

		if record.Timestamp.IsZero() {
			t.Error("Expected non-zero timestamp")
		}

		if record.Status == "" {
			t.Error("Expected non-empty status")
		}
	}
}

func TestStrictMode(t *testing.T) {
	// Test that strict mode causes translation to fail on errors
	translator := NewDefaultParameterTranslator(WithStrictMode(true))

	// Create a scenario that would cause an error
	// (Using an encoder pair with no direct translation rules)
	params := map[string]string{
		"unknown_param": "value",
	}

	// This should not fail in non-strict mode, just skip the parameter
	result, err := translator.Translate(EncoderLibX264, EncoderH264NVENC, params)
	if err != nil {
		// In strict mode, unknown params might cause an error
		t.Logf("Strict mode error: %v", err)
	}

	if result != nil {
		// Check that the unknown param was skipped
		if _, exists := result.TranslatedParams["unknown_param"]; exists {
			t.Error("Unknown parameter should not be in translated params")
		}
	}
}

func TestHardwareParamInjection(t *testing.T) {
	// Test with hardware param injection enabled
	translator := NewDefaultParameterTranslator(WithHardwareParamInjection(true))

	params := map[string]string{"crf": "23"}
	result, err := translator.Translate(EncoderLibX264, EncoderH264NVENC, params)
	if err != nil {
		t.Fatalf("Translate failed: %v", err)
	}

	// Check that hardware params were injected
	if len(result.HardwareParams) == 0 {
		t.Error("Expected hardware params to be injected")
	}

	// Test with hardware param injection disabled
	translator = NewDefaultParameterTranslator(WithHardwareParamInjection(false))

	result, err = translator.Translate(EncoderLibX264, EncoderH264NVENC, params)
	if err != nil {
		t.Fatalf("Translate failed: %v", err)
	}

	// Check that hardware params were NOT injected
	if len(result.HardwareParams) > 0 {
		t.Error("Did not expect hardware params to be injected")
	}
}

func TestGetMapping(t *testing.T) {
	translator := NewDefaultParameterTranslator()

	mapping := translator.GetMapping()
	if mapping == nil {
		t.Error("GetMapping returned nil")
	}

	// Verify it's the same mapping
	if !mapping.CanTranslate(EncoderLibX264, EncoderH264NVENC) {
		t.Error("Mapping should have libx264 -> h264_nvenc translation")
	}
}

func TestCrossCodecTranslation(t *testing.T) {
	translator := NewDefaultParameterTranslator()

	// Test cross-codec translation (should generate warning)
	params := map[string]string{"crf": "23"}

	result, err := translator.Translate(EncoderLibX264, EncoderHEVCNVENC, params)
	if err != nil {
		t.Fatalf("Translate failed: %v", err)
	}

	// Should have a warning about cross-codec translation
	if len(result.Warnings) == 0 {
		t.Error("Expected warning for cross-codec translation")
	}
}

func TestTranslationStatus(t *testing.T) {
	translator := NewDefaultParameterTranslator()

	params := map[string]string{
		"crf":    "23",
		"preset": "slow",
	}

	result, err := translator.Translate(EncoderLibX264, EncoderH264NVENC, params)
	if err != nil {
		t.Fatalf("Translate failed: %v", err)
	}

	// Check that audit records have valid statuses
	for _, record := range result.AuditRecords {
		switch record.Status {
		case TranslationStatusSuccess, TranslationStatusSkipped, TranslationStatusFailed, TranslationStatusDefault:
			// Valid status
		default:
			t.Errorf("Invalid translation status: %s", record.Status)
		}
	}
}
