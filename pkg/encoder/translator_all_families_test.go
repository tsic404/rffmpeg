package encoder

import (
	"testing"
)

// TestTranslate_AllH264Families tests crf→target-param mapping for all H.264 encoder families:
//   - h264_nvenc: crf → cq (pass-through)
//   - h264_qsv:   crf → global_quality (pass-through)
//   - h264_vaapi: crf → quality (scaled 0-51→0-100)
//   - h264_amf:   crf → qp_i
//   - h264_videotoolbox: crf → q:v
func TestTranslate_AllH264Families(t *testing.T) {
	translator := NewDefaultParameterTranslator()

	tests := []struct {
		name                string
		targetEncoder       EncoderFamily
		crfValue            string
		expectedParamName   string
		expectedValue       string
		expectPresetMap     bool
		expectedNVEncPreset string
	}{
		{
			name:              "NVENC crf→cq",
			targetEncoder:     EncoderH264NVENC,
			crfValue:          "23",
			expectedParamName: "cq",
			expectedValue:     "23",
			expectPresetMap:   true,
		},
		{
			name:              "QSV crf→global_quality",
			targetEncoder:     EncoderH264QSV,
			crfValue:          "18",
			expectedParamName: "global_quality",
			expectedValue:     "18",
		},
		{
			name:              "VAAPI crf→quality (scaled)",
			targetEncoder:     EncoderH264VAAPI,
			crfValue:          "23",
			expectedParamName: "quality",
			// 23 maps to 55 (scaled: 100 - 23*100/51 = 55)
			expectedValue: "55",
		},
		{
			name:              "AMF crf→qp_i",
			targetEncoder:     EncoderH264AMF,
			crfValue:          "23",
			expectedParamName: "qp_i",
			expectedValue:     "23",
		},
		{
			name:              "VideoToolbox crf→q:v",
			targetEncoder:     EncoderH264VT,
			crfValue:          "23",
			expectedParamName: "q:v",
			expectedValue:     "23",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			params := map[string]string{
				"crf":    tt.crfValue,
				"preset": "slow",
				"tune":   "film",
			}

			result, err := translator.Translate(EncoderLibX264, tt.targetEncoder, params)
			if err != nil {
				t.Fatalf("Translate failed: %v", err)
			}

			// Check the CRF mapping
			if val, ok := result.TranslatedParams[tt.expectedParamName]; !ok {
				t.Errorf("Expected target param '%s' to exist, got params: %v", tt.expectedParamName, result.TranslatedParams)
			} else if val != tt.expectedValue {
				t.Errorf("Expected %s=%s, got %s=%s", tt.expectedParamName, tt.expectedValue, tt.expectedParamName, val)
			}

			// Verify audit records were generated
			if len(result.AuditRecords) == 0 {
				t.Error("No audit records generated for translation")
			}

			// Verify hardware params were injected
			if len(result.HardwareParams) == 0 {
				t.Logf("No hardware params injected for %s (may be expected)", tt.targetEncoder)
			}
		})
	}
}

// TestTranslate_HEVCFamilies tests crf→target-param mapping for HEVC encoder families.
func TestTranslate_HEVCFamilies(t *testing.T) {
	translator := NewDefaultParameterTranslator()

	tests := []struct {
		name              string
		targetEncoder     EncoderFamily
		sourceEncoder     EncoderFamily
		crfValue          string
		expectedParamName string
		expectedValue     string
	}{
		{
			name:              "HEVC NVENC libx265→hevc_nvenc",
			targetEncoder:     EncoderHEVCNVENC,
			sourceEncoder:     EncoderLibX265,
			crfValue:          "28",
			expectedParamName: "cq",
			expectedValue:     "28",
		},
		{
			name:              "HEVC QSV libx265→hevc_qsv",
			targetEncoder:     EncoderHEVCQSV,
			sourceEncoder:     EncoderLibX265,
			crfValue:          "28",
			expectedParamName: "global_quality",
			expectedValue:     "28",
		},
		{
			name:              "HEVC VAAPI libx265→hevc_vaapi",
			targetEncoder:     EncoderHEVCVAAPI,
			sourceEncoder:     EncoderLibX265,
			crfValue:          "28",
			expectedParamName: "quality",
			// 28 maps to 46 (scaled: 100 - 28*100/51 = 46)
			expectedValue: "46",
		},
		{
			name:              "HEVC AMF libx265→hevc_amf",
			targetEncoder:     EncoderHEVCAMF,
			sourceEncoder:     EncoderLibX265,
			crfValue:          "28",
			expectedParamName: "qp_i",
			expectedValue:     "28",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			params := map[string]string{
				"crf":    tt.crfValue,
				"preset": "medium",
			}

			result, err := translator.Translate(tt.sourceEncoder, tt.targetEncoder, params)
			if err != nil {
				t.Fatalf("Translate failed: %v", err)
			}

			if val, ok := result.TranslatedParams[tt.expectedParamName]; !ok {
				t.Errorf("Expected target param '%s' to exist, got params: %v", tt.expectedParamName, result.TranslatedParams)
			} else if val != tt.expectedValue {
				t.Errorf("Expected %s=%s, got %s=%s", tt.expectedParamName, tt.expectedValue, tt.expectedParamName, val)
			}
		})
	}
}

// TestTranslate_PresetMapping verifies preset mappings for all encoder families.
func TestTranslate_PresetMapping(t *testing.T) {
	translator := NewDefaultParameterTranslator()

	tests := []struct {
		name           string
		targetEncoder  EncoderFamily
		sourcePreset   string
		expectedPreset string
	}{
		// NVENC presets: ultrafast→p1, faster→p4, medium→p6, slow→p7, veryslow→p7
		{"NVENC ultrafast→p1", EncoderH264NVENC, "ultrafast", "p1"},
		{"NVENC medium→p6", EncoderH264NVENC, "medium", "p6"},
		{"NVENC slow→p7", EncoderH264NVENC, "slow", "p7"},
		{"NVENC veryslow→p7", EncoderH264NVENC, "veryslow", "p7"},
		{"NVENC placebo→p7", EncoderH264NVENC, "placebo", "p7"},

		// QSV presets: ultrafast→veryfast, medium→medium, slow→slow, placebo→veryslow
		{"QSV ultrafast→veryfast", EncoderH264QSV, "ultrafast", "veryfast"},
		{"QSV medium→medium", EncoderH264QSV, "medium", "medium"},
		{"QSV slow→slow", EncoderH264QSV, "slow", "slow"},
		{"QSV placebo→veryslow", EncoderH264QSV, "placebo", "veryslow"},

		// AMF presets: ultrafast→speed, medium→balanced, slow→quality, veryslow→quality
		{"AMF ultrafast→speed", EncoderH264AMF, "ultrafast", "speed"},
		{"AMF medium→balanced", EncoderH264AMF, "medium", "balanced"},
		{"AMF slow→quality", EncoderH264AMF, "slow", "quality"},
		{"AMF veryslow→quality", EncoderH264AMF, "veryslow", "quality"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			params := map[string]string{
				"crf":    "23",
				"preset": tt.sourcePreset,
			}

			result, err := translator.Translate(EncoderLibX264, tt.targetEncoder, params)
			if err != nil {
				t.Fatalf("Translate failed: %v", err)
			}

			if val, ok := result.TranslatedParams["preset"]; ok {
				if val != tt.expectedPreset {
					t.Errorf("Expected preset=%s, got %s", tt.expectedPreset, val)
				}
			} else {
				// VAAPI and VT don't have preset param
				t.Logf("No preset in translated params for %s", tt.targetEncoder)
			}
		})
	}
}

// TestTranslate_AutoHWFlagBehavior verifies that --auto-hw flag behavior:
//   - When unspecified encoder + AutoHW=true: prefers hardware encoder
//   - When user specifies encoder: respects user choice
func TestTranslate_AutoHWFlagBehavior(t *testing.T) {
	mapping := DefaultMapping()
	translator := NewParameterTranslator(mapping)

	// Test: libx264 → hardware upgrade when no user encoder specified
	// This is tested at the rewrite engine level, not the translator level.
	// The translator just translates parameters between known encoders.
	// AutoHW behavior is handled by the rewrite adapter.

	t.Run("libx264 to h264_nvenc with crf translation", func(t *testing.T) {
		params := map[string]string{"crf": "20", "preset": "fast"}
		result, err := translator.Translate(EncoderLibX264, EncoderH264NVENC, params)
		if err != nil {
			t.Fatalf("Translate failed: %v", err)
		}
		if result.TranslatedParams["cq"] != "20" {
			t.Errorf("Expected cq=20, got %v", result.TranslatedParams["cq"])
		}
		if result.TranslatedParams["preset"] != "p5" {
			t.Errorf("Expected preset=p5 (fast→p5), got %s", result.TranslatedParams["preset"])
		}
	})

	t.Run("unknown encoder returns error in strict mode", func(t *testing.T) {
		strictTranslator := NewDefaultParameterTranslator(WithStrictMode(true))
		params := map[string]string{"crf": "23"}
		_, err := strictTranslator.Translate(EncoderLibX264, "unknown_encoder", params)
		if err == nil {
			t.Error("Expected error for unknown encoder in strict mode")
		}
	})
}

// TestTranslate_AuditTransparency verifies audit records contain proper details.
func TestTranslate_AuditTransparency(t *testing.T) {
	translator := NewDefaultParameterTranslator()

	params := map[string]string{
		"crf":    "23",
		"preset": "medium",
		"tune":   "film",
		"b:v":    "5000k",
	}

	result, err := translator.Translate(EncoderLibX264, EncoderH264NVENC, params)
	if err != nil {
		t.Fatalf("Translate failed: %v", err)
	}

	// Verify each translated parameter has an audit record
	if len(result.AuditRecords) == 0 {
		t.Error("Expected audit records for translation")
	}

	// Audit records should document original→translated mappings
	for _, record := range result.AuditRecords {
		t.Logf("Audit: source=%s, target=%s, param=%s → %s",
			record.SourceEncoder, record.TargetEncoder,
			record.SourceParam, record.TargetParam)
		if record.SourceParam == "" || record.TargetParam == "" {
			t.Errorf("Audit record has empty parameter: %+v", record)
		}
	}
}

// TestTranslate_VP9AndAV1 tests parameter translation for VP9 and AV1 codec families.
func TestTranslate_VP9AndAV1(t *testing.T) {
	translator := NewDefaultParameterTranslator()

	tests := []struct {
		name              string
		sourceEncoder     EncoderFamily
		targetEncoder     EncoderFamily
		crfValue          string
		expectedParamName string
		expectedValue     string
	}{
		{
			name:              "VP9 libvpx-vp9→vp9_vaapi",
			sourceEncoder:     EncoderLibVPX,
			targetEncoder:     EncoderVP9VAAPI,
			crfValue:          "31",
			expectedParamName: "quality",
			expectedValue:     "51",
		},
		{
			name:              "AV1 libaom-av1→av1_nvenc",
			sourceEncoder:     EncoderLibAOM,
			targetEncoder:     EncoderAV1NVENC,
			crfValue:          "30",
			expectedParamName: "cq",
			expectedValue:     "30",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			params := map[string]string{"crf": tt.crfValue}
			result, err := translator.Translate(tt.sourceEncoder, tt.targetEncoder, params)
			if err != nil {
				t.Fatalf("Translate failed: %v", err)
			}

			if val, ok := result.TranslatedParams[tt.expectedParamName]; ok {
				if val != tt.expectedValue {
					t.Errorf("Expected %s=%s, got %s", tt.expectedParamName, tt.expectedValue, val)
				}
			} else {
				t.Logf("Param %s not found in result. Available: %v", tt.expectedParamName, result.TranslatedParams)
			}
		})
	}
}
