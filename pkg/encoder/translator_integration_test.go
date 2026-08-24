package encoder

import (
	"strings"
	"testing"
)

// Integration tests for ParameterTranslator with mapping table

// TestIntegration_FullH264Translation tests a complete H.264 parameter translation scenario.
func TestIntegration_FullH264Translation(t *testing.T) {
	translator := NewDefaultParameterTranslator()

	// Simulate real-world H.264 encoding parameters
	params := map[string]string{
		"crf":     "23",
		"preset":  "slow",
		"tune":    "film",
		"profile": "high",
		"level":   "4.1",
		"b:v":     "5000k",
		"maxrate": "5500k",
		"bufsize": "11000k",
		"bf":      "3",
		"refs":    "3",
	}

	// Translate to NVENC
	result, err := translator.Translate(EncoderLibX264, EncoderH264NVENC, params)
	if err != nil {
		t.Fatalf("Translation failed: %v", err)
	}

	// Verify key translations
	verifyTranslation(t, result, "cq", "23", "CRF should be translated to cq")
	verifyTranslation(t, result, "preset", "p7", "slow preset should be p7")
	verifyTranslation(t, result, "profile", "high", "profile should be preserved")
	verifyTranslation(t, result, "b:v", "5000k", "bitrate should be preserved")

	// Verify audit records
	if len(result.AuditRecords) < len(params) {
		t.Errorf("Expected at least %d audit records, got %d", len(params), len(result.AuditRecords))
	}
	// rc=constqp is NOT injected here: the user-supplied crf→cq conflicts
	// with it (mutual-exclusion metadata). The command stays valid instead.
	if _, exists := result.HardwareParams["rc"]; exists {
		t.Error("rc=constqp should not be injected when user supplied cq")
	}
	if len(result.Warnings) == 0 {
		t.Error("Expected a warning about the suppressed rc injection")
	}

}

// TestIntegration_FullHEVCTranslation tests a complete HEVC parameter translation scenario.
func TestIntegration_FullHEVCTranslation(t *testing.T) {
	translator := NewDefaultParameterTranslator()

	params := map[string]string{
		"crf":     "28",
		"preset":  "medium",
		"profile": "main10",
		"level":   "5.1",
		"b:v":     "8000k",
	}

	result, err := translator.Translate(EncoderLibX265, EncoderHEVCNVENC, params)
	if err != nil {
		t.Fatalf("Translation failed: %v", err)
	}

	verifyTranslation(t, result, "cq", "28", "CRF should be translated to cq")
	verifyTranslation(t, result, "preset", "p6", "medium preset should be p6")
	verifyTranslation(t, result, "profile", "main10", "profile should be preserved")
}

// TestIntegration_VP9Translation tests VP9 parameter translation.
func TestIntegration_VP9Translation(t *testing.T) {
	translator := NewDefaultParameterTranslator()

	params := map[string]string{
		"crf":     "31",
		"b:v":     "4000k",
		"profile": "0",
	}

	result, err := translator.Translate(EncoderLibVPX, EncoderVP9VAAPI, params)
	if err != nil {
		t.Fatalf("Translation failed: %v", err)
	}

	// CRF should be converted to quality with scale adjustment
	if result.TranslatedParams["quality"] == "" {
		t.Error("Expected quality parameter to be set")
	}
}

// TestIntegration_AV1Translation tests AV1 parameter translation.
func TestIntegration_AV1Translation(t *testing.T) {
	translator := NewDefaultParameterTranslator()

	params := map[string]string{
		"crf":     "30",
		"preset":  "medium",
		"profile": "main",
		"b:v":     "6000k",
	}

	result, err := translator.Translate(EncoderLibSVTAV1, EncoderAV1NVENC, params)
	if err != nil {
		t.Fatalf("Translation failed: %v", err)
	}

	verifyTranslation(t, result, "cq", "30", "CRF should be translated to cq")
	verifyTranslation(t, result, "preset", "p6", "medium preset should be p6")
}

// TestIntegration_ChainedTranslation tests multi-level chained translation.
func TestIntegration_ChainedTranslation(t *testing.T) {
	translator := NewDefaultParameterTranslator()

	// Test chain: libx264 -> h264_nvenc -> h264_qsv
	chain := []EncoderFamily{EncoderLibX264, EncoderH264NVENC, EncoderH264QSV}
	params := map[string]string{
		"crf":    "23",
		"preset": "medium",
	}

	result, err := translator.TranslateChain(chain, params)
	if err != nil {
		t.Fatalf("Chain translation failed: %v", err)
	}

	// Final result should be for h264_qsv
	if result.TargetEncoder != EncoderH264QSV {
		t.Errorf("Expected final target encoder h264_qsv, got %s", result.TargetEncoder)
	}

	// Verify chain information in audit records
	for _, record := range result.AuditRecords {
		if record.ChainTotal != 2 {
			t.Errorf("Expected ChainTotal=2, got %d", record.ChainTotal)
		}
	}
}

// TestIntegration_ValidationAndNormalization tests parameter validation and normalization.
func TestIntegration_ValidationAndNormalization(t *testing.T) {
	translator := NewDefaultParameterTranslator()

	// Test with out-of-range values
	params := map[string]string{
		"crf": "100", // Out of range (0-51)
	}

	errors, err := translator.ValidateParameters(EncoderLibX264, params)
	if err != nil {
		t.Fatalf("ValidateParameters failed: %v", err)
	}

	if len(errors) == 0 {
		t.Error("Expected validation error for out-of-range CRF")
	}

	// Test normalization
	params = map[string]string{
		"CRF":    "23",   // Should be lowercased
		"Preset": "SLOW", // Should be lowercased
	}

	normalized, err := translator.NormalizeParameters(EncoderLibX264, params)
	if err != nil {
		t.Fatalf("NormalizeParameters failed: %v", err)
	}

	if _, exists := normalized["crf"]; !exists {
		t.Error("Expected 'crf' in normalized params")
	}

	if normalized["preset"] != "slow" {
		t.Errorf("Expected preset=slow, got %s", normalized["preset"])
	}
}

// TestIntegration_AllEncoderPairs tests translation for all registered encoder pairs.
func TestIntegration_AllEncoderPairs(t *testing.T) {
	translator := NewDefaultParameterTranslator()

	// Define test cases for different encoder pairs
	testCases := []struct {
		source   EncoderFamily
		target   EncoderFamily
		params   map[string]string
		expected map[string]string
	}{
		{
			source:   EncoderLibX264,
			target:   EncoderH264NVENC,
			params:   map[string]string{"crf": "23", "preset": "slow"},
			expected: map[string]string{"cq": "23", "preset": "p7"},
		},
		{
			source:   EncoderLibX264,
			target:   EncoderH264QSV,
			params:   map[string]string{"crf": "23", "preset": "medium"},
			expected: map[string]string{"global_quality": "23"},
		},
		{
			source:   EncoderLibX264,
			target:   EncoderH264VAAPI,
			params:   map[string]string{"crf": "23"},
			expected: map[string]string{}, // quality will be scaled
		},
		{
			source:   EncoderLibX265,
			target:   EncoderHEVCNVENC,
			params:   map[string]string{"crf": "28", "preset": "fast"},
			expected: map[string]string{"cq": "28", "preset": "p5"},
		},
		{
			source:   EncoderLibX265,
			target:   EncoderHEVCQSV,
			params:   map[string]string{"crf": "28"},
			expected: map[string]string{"global_quality": "28"},
		},
		{
			source:   EncoderLibVPX,
			target:   EncoderVP9VAAPI,
			params:   map[string]string{"crf": "31"},
			expected: map[string]string{}, // quality will be scaled
		},
		{
			source:   EncoderLibSVTAV1,
			target:   EncoderAV1NVENC,
			params:   map[string]string{"crf": "30", "preset": "medium"},
			expected: map[string]string{"cq": "30", "preset": "p6"},
		},
		{
			source:   EncoderLibSVTAV1,
			target:   EncoderAV1QSV,
			params:   map[string]string{"crf": "30"},
			expected: map[string]string{"global_quality": "30"},
		},
	}

	for _, tc := range testCases {
		t.Run(string(tc.source)+"_to_"+string(tc.target), func(t *testing.T) {
			result, err := translator.Translate(tc.source, tc.target, tc.params)
			if err != nil {
				t.Fatalf("Translation failed: %v", err)
			}

			for param, expectedValue := range tc.expected {
				if result.TranslatedParams[param] != expectedValue {
					t.Errorf("Expected %s=%s, got %s", param, expectedValue, result.TranslatedParams[param])
				}
			}
		})
	}
}

// TestIntegration_ErrorHandling tests error handling scenarios.
func TestIntegration_ErrorHandling(t *testing.T) {
	translator := NewDefaultParameterTranslator()

	// Test with unregistered encoder
	_, err := translator.Translate(EncoderFamily("unknown"), EncoderH264NVENC, map[string]string{"crf": "23"})
	if err == nil {
		t.Error("Expected error for unregistered source encoder")
	}

	// Test with invalid chain
	_, err = translator.TranslateChain([]EncoderFamily{EncoderLibX264}, map[string]string{"crf": "23"})
	if err == nil {
		t.Error("Expected error for invalid chain")
	}
}

// TestIntegration_AuditTrail tests that audit records are correctly generated.
func TestIntegration_AuditTrail(t *testing.T) {
	translator := NewDefaultParameterTranslator()

	params := map[string]string{
		"crf":    "23",
		"preset": "slow",
		"b:v":    "5000k",
	}

	result, err := translator.Translate(EncoderLibX264, EncoderH264NVENC, params)
	if err != nil {
		t.Fatalf("Translation failed: %v", err)
	}

	// Verify each parameter has an audit record
	paramAuditCount := make(map[string]int)
	for _, record := range result.AuditRecords {
		if record.SourceParam != "" {
			paramAuditCount[record.SourceParam]++
		}
	}

	for param := range params {
		if paramAuditCount[param] == 0 {
			t.Errorf("No audit record for parameter '%s'", param)
		}
	}

	// Verify audit records have timestamps
	for _, record := range result.AuditRecords {
		if record.Timestamp.IsZero() {
			t.Error("Audit record has zero timestamp")
		}
	}
}

// TestIntegration_HardwareParamInjection tests hardware-specific parameter injection.
func TestIntegration_HardwareParamInjection(t *testing.T) {
	translator := NewDefaultParameterTranslator(WithHardwareParamInjection(true))

	// Test NVENC hardware params: no user quality carrier, so rc=constqp
	// must be injected.
	result, err := translator.Translate(EncoderLibX264, EncoderH264NVENC, map[string]string{"preset": "fast"})
	if err != nil {
		t.Fatalf("Translation failed: %v", err)
	}

	if result.HardwareParams["rc"] != "constqp" {
		t.Error("Expected rc=constqp to be injected for NVENC")
	}

	// Test QSV hardware params
	result, err = translator.Translate(EncoderLibX264, EncoderH264QSV, map[string]string{"crf": "23"})
	if err != nil {
		t.Fatalf("Translation failed: %v", err)
	}

	if len(result.HardwareParams) == 0 {
		t.Error("Expected hardware params to be injected for QSV")
	}

	// Test without hardware param injection
	translator = NewDefaultParameterTranslator(WithHardwareParamInjection(false))
	result, err = translator.Translate(EncoderLibX264, EncoderH264NVENC, map[string]string{"crf": "23"})
	if err != nil {
		t.Fatalf("Translation failed: %v", err)
	}

	if len(result.HardwareParams) > 0 {
		t.Error("Did not expect hardware params when injection is disabled")
	}
}

// TestIntegration_SupportedTranslations tests GetSupportedTranslations.
func TestIntegration_SupportedTranslations(t *testing.T) {
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

	// Verify we have multiple sources
	if len(sources) < 1 {
		t.Error("Expected at least one supported source encoder")
	}
}

// TestIntegration_StrictMode tests strict mode behavior.
func TestIntegration_StrictMode(t *testing.T) {
	// Test with strict mode disabled (default)
	translator := NewDefaultParameterTranslator(WithStrictMode(false))

	// Translation should succeed even with unknown params
	result, err := translator.Translate(EncoderLibX264, EncoderH264NVENC, map[string]string{"unknown_param": "value"})
	if err != nil {
		t.Errorf("Translation should succeed in non-strict mode: %v", err)
	}

	// Unknown param should be passed through with a warning (never dropped)
	if v, exists := result.TranslatedParams["unknown_param"]; !exists || v != "value" {
		t.Errorf("Unknown parameter should be preserved, got value %q exists=%v", v, exists)
	}
	found := false
	for _, w := range result.Warnings {
		if strings.Contains(w, "unknown_param") {
			found = true
			break
		}
	}
	if !found {
		t.Error("Expected a warning mentioning unknown_param")
	}

	// Test with strict mode enabled
	translator = NewDefaultParameterTranslator(WithStrictMode(true))

	// Translation should still succeed (unknown params are skipped, not errors)
	result, err = translator.Translate(EncoderLibX264, EncoderH264NVENC, map[string]string{"crf": "23"})
	if err != nil {
		t.Errorf("Translation should succeed: %v", err)
	}

	if result.TranslatedParams["cq"] != "23" {
		t.Errorf("Expected cq=23, got %s", result.TranslatedParams["cq"])
	}
}

// Helper function to verify a translation result
func verifyTranslation(t *testing.T, result *TranslationResult, param, expected, msg string) {
	t.Helper()
	if result.TranslatedParams[param] != expected {
		t.Errorf("%s: expected %s=%s, got %s", msg, param, expected, result.TranslatedParams[param])
	}
}
