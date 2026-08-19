package rewrite

import (
	"context"
	"testing"

	"github.com/tsix404/rffmpeg/pkg/encoder"
)

func TestScenarioClassifier_Classify(t *testing.T) {
	classifier := NewScenarioClassifier()

	tests := []struct {
		name           string
		request        *EncoderRewriteRequest
		expectedResult ScenarioType
		expectError    bool
	}{
		{
			name:           "nil request returns error",
			request:        nil,
			expectedResult: 0,
			expectError:    true,
		},
		{
			name: "no encoder specified with hardware available",
			request: &EncoderRewriteRequest{
				SpecifiedEncoder: "",
				HardwareCapabilities: HardwareCapabilities{
					HardwareEncoders:  []encoder.EncoderFamily{encoder.EncoderH264NVENC},
					AvailableEncoders: []encoder.EncoderFamily{encoder.EncoderH264NVENC, encoder.EncoderLibX264},
				},
			},
			expectedResult: ScenarioUnspecifiedEncoderWithHW,
			expectError:    false,
		},
		{
			name: "no encoder specified no hardware",
			request: &EncoderRewriteRequest{
				SpecifiedEncoder: "",
				HardwareCapabilities: HardwareCapabilities{
					SoftwareEncoders:  []encoder.EncoderFamily{encoder.EncoderLibX264},
					AvailableEncoders: []encoder.EncoderFamily{encoder.EncoderLibX264},
				},
			},
			expectedResult: ScenarioUnspecifiedEncoderNoHW,
			expectError:    false,
		},
		{
			name: "specified encoder is supported",
			request: &EncoderRewriteRequest{
				SpecifiedEncoder: encoder.EncoderH264NVENC,
				HardwareCapabilities: HardwareCapabilities{
					AvailableEncoders: []encoder.EncoderFamily{encoder.EncoderH264NVENC, encoder.EncoderLibX264},
					HardwareEncoders:  []encoder.EncoderFamily{encoder.EncoderH264NVENC},
				},
			},
			expectedResult: ScenarioSpecifiedEncoderSupported,
			expectError:    false,
		},
		{
			name: "specified encoder unsupported with hardware alternative",
			request: &EncoderRewriteRequest{
				SpecifiedEncoder: encoder.EncoderH264QSV,
				HardwareCapabilities: HardwareCapabilities{
					AvailableEncoders: []encoder.EncoderFamily{encoder.EncoderH264NVENC, encoder.EncoderLibX264},
					HardwareEncoders:  []encoder.EncoderFamily{encoder.EncoderH264NVENC},
					SupportedCodecs:   []encoder.CodecFormat{encoder.CodecH264},
					EncoderPriority:   DefaultEncoderPriority(),
				},
			},
			expectedResult: ScenarioSpecifiedEncoderUnsupportedWithAlternative,
			expectError:    false,
		},
		{
			name: "specified encoder unsupported fallback to software",
			request: &EncoderRewriteRequest{
				SpecifiedEncoder: encoder.EncoderH264NVENC,
				HardwareCapabilities: HardwareCapabilities{
					AvailableEncoders: []encoder.EncoderFamily{encoder.EncoderLibX264},
					SoftwareEncoders:  []encoder.EncoderFamily{encoder.EncoderLibX264},
					SupportedCodecs:   []encoder.CodecFormat{encoder.CodecH264},
				},
			},
			expectedResult: ScenarioSpecifiedEncoderUnsupportedFallbackSoftware,
			expectError:    false,
		},
		{
			name: "format not available",
			request: &EncoderRewriteRequest{
				SpecifiedEncoder: encoder.EncoderH264NVENC,
				HardwareCapabilities: HardwareCapabilities{
					AvailableEncoders: []encoder.EncoderFamily{encoder.EncoderLibX265},
					SupportedCodecs:   []encoder.CodecFormat{encoder.CodecHEVC},
				},
			},
			expectedResult: ScenarioFormatNotAvailable,
			expectError:    false,
		},
		{
			name: "blacklisted encoder with alternative",
			request: &EncoderRewriteRequest{
				SpecifiedEncoder: encoder.EncoderH264NVENC,
				HardwareCapabilities: HardwareCapabilities{
					AvailableEncoders: []encoder.EncoderFamily{encoder.EncoderH264QSV, encoder.EncoderLibX264},
					HardwareEncoders:  []encoder.EncoderFamily{encoder.EncoderH264QSV},
					SupportedCodecs:   []encoder.CodecFormat{encoder.CodecH264},
					EncoderBlacklist:  []encoder.EncoderFamily{encoder.EncoderH264NVENC},
					EncoderPriority:   DefaultEncoderPriority(),
				},
			},
			expectedResult: ScenarioSpecifiedEncoderUnsupportedWithAlternative,
			expectError:    false,
		},
		{
			name: "auto-hw upgrades software encoder to hardware (libx264 + AutoHW=true)",
			request: &EncoderRewriteRequest{
				SpecifiedEncoder: encoder.EncoderLibX264,
				AutoHW:           true,
				HardwareCapabilities: HardwareCapabilities{
					AvailableEncoders: []encoder.EncoderFamily{encoder.EncoderH264QSV, encoder.EncoderLibX264},
					HardwareEncoders:  []encoder.EncoderFamily{encoder.EncoderH264QSV},
					SoftwareEncoders:  []encoder.EncoderFamily{encoder.EncoderLibX264},
					SupportedCodecs:   []encoder.CodecFormat{encoder.CodecH264},
					EncoderPriority:   DefaultEncoderPriority(),
				},
			},
			expectedResult: ScenarioSpecifiedEncoderUnsupportedWithAlternative,
			expectError:    false,
		},
		{
			name: "auto-hw does not upgrade when disabled (libx264 + AutoHW=false)",
			request: &EncoderRewriteRequest{
				SpecifiedEncoder: encoder.EncoderLibX264,
				AutoHW:           false,
				HardwareCapabilities: HardwareCapabilities{
					AvailableEncoders: []encoder.EncoderFamily{encoder.EncoderH264QSV, encoder.EncoderLibX264},
					HardwareEncoders:  []encoder.EncoderFamily{encoder.EncoderH264QSV},
					SoftwareEncoders:  []encoder.EncoderFamily{encoder.EncoderLibX264},
					SupportedCodecs:   []encoder.CodecFormat{encoder.CodecH264},
					EncoderPriority:   DefaultEncoderPriority(),
				},
			},
			expectedResult: ScenarioSpecifiedEncoderSupported,
			expectError:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := classifier.Classify(context.Background(), tt.request)

			if tt.expectError {
				if err == nil {
					t.Error("expected error, got nil")
				}
				return
			}

			if err != nil {
				t.Errorf("unexpected error: %v", err)
				return
			}

			if result != tt.expectedResult {
				t.Errorf("expected scenario %v, got %v", tt.expectedResult, result)
			}
		})
	}
}

func TestScenarioClassifier_GetScenarioInfo(t *testing.T) {
	classifier := NewScenarioClassifier()

	scenarios := []ScenarioType{
		ScenarioUnspecifiedEncoderWithHW,
		ScenarioUnspecifiedEncoderNoHW,
		ScenarioSpecifiedEncoderSupported,
		ScenarioSpecifiedEncoderUnsupportedWithAlternative,
		ScenarioSpecifiedEncoderUnsupportedFallbackSoftware,
		ScenarioFormatNotAvailable,
	}

	for _, scenario := range scenarios {
		t.Run(scenario.String(), func(t *testing.T) {
			info := classifier.GetScenarioInfo(scenario)

			if info.Type != scenario {
				t.Errorf("expected type %v, got %v", scenario, info.Type)
			}

			if info.Description == "" {
				t.Error("description should not be empty")
			}

			if info.RecommendedAction == "" {
				t.Error("recommended action should not be empty")
			}

			// FormatNotAvailable should be an error scenario
			if scenario == ScenarioFormatNotAvailable && !info.IsError {
				t.Error("ScenarioFormatNotAvailable should be an error scenario")
			}
		})
	}
}

func TestScenarioClassifier_SelectTargetEncoder(t *testing.T) {
	classifier := NewScenarioClassifier()

	tests := []struct {
		name           string
		scenario       ScenarioType
		request        *EncoderRewriteRequest
		expectedResult encoder.EncoderFamily
	}{
		{
			name:     "unspecified with hw - select best hardware",
			scenario: ScenarioUnspecifiedEncoderWithHW,
			request: &EncoderRewriteRequest{
				HardwareCapabilities: HardwareCapabilities{
					AvailableEncoders: []encoder.EncoderFamily{encoder.EncoderH264NVENC, encoder.EncoderLibX264},
					HardwareEncoders:  []encoder.EncoderFamily{encoder.EncoderH264NVENC},
					EncoderPriority:   DefaultEncoderPriority(),
				},
			},
			expectedResult: encoder.EncoderH264NVENC,
		},
		{
			name:     "unspecified no hw - use libx264",
			scenario: ScenarioUnspecifiedEncoderNoHW,
			request: &EncoderRewriteRequest{
				HardwareCapabilities: HardwareCapabilities{},
			},
			expectedResult: encoder.EncoderLibX264,
		},
		{
			name:     "supported - use specified",
			scenario: ScenarioSpecifiedEncoderSupported,
			request: &EncoderRewriteRequest{
				SpecifiedEncoder: encoder.EncoderH264QSV,
			},
			expectedResult: encoder.EncoderH264QSV,
		},
		{
			name:     "unsupported with alternative - select hardware alternative",
			scenario: ScenarioSpecifiedEncoderUnsupportedWithAlternative,
			request: &EncoderRewriteRequest{
				SpecifiedEncoder: encoder.EncoderH264QSV,
				HardwareCapabilities: HardwareCapabilities{
					AvailableEncoders: []encoder.EncoderFamily{encoder.EncoderH264NVENC},
					HardwareEncoders:  []encoder.EncoderFamily{encoder.EncoderH264NVENC},
					EncoderPriority:   DefaultEncoderPriority(),
				},
			},
			expectedResult: encoder.EncoderH264NVENC,
		},
		{
			name:     "fallback software - select software encoder",
			scenario: ScenarioSpecifiedEncoderUnsupportedFallbackSoftware,
			request: &EncoderRewriteRequest{
				SpecifiedEncoder: encoder.EncoderH264NVENC,
				HardwareCapabilities: HardwareCapabilities{
					SoftwareEncoders: []encoder.EncoderFamily{encoder.EncoderLibX264},
				},
			},
			expectedResult: encoder.EncoderLibX264,
		},
		{
			name:     "format not available - empty result",
			scenario: ScenarioFormatNotAvailable,
			request: &EncoderRewriteRequest{
				SpecifiedEncoder: encoder.EncoderH264NVENC,
			},
			expectedResult: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := classifier.SelectTargetEncoder(tt.scenario, tt.request)

			if result != tt.expectedResult {
				t.Errorf("expected encoder %s, got %s", tt.expectedResult, result)
			}
		})
	}
}
