package rewrite

import (
	"context"
	"testing"
	"time"

	"github.com/tsic404/rffmpeg/pkg/encoder"
)

// TestEndToEnd_AllScenarios tests all 6 rewrite scenarios end-to-end.
func TestEndToEnd_AllScenarios(t *testing.T) {
	tests := []struct {
		name             string
		request          *EncoderRewriteRequest
		expectedScenario ScenarioType
		expectedEncoder  encoder.EncoderFamily
		expectError      bool
		validateResult   func(t *testing.T, response *EncoderRewriteResponse)
	}{
		{
			name: "Scenario 1: Unspecified encoder with hardware available",
			request: &EncoderRewriteRequest{
				OriginalArgs: []string{"-i", "input.mp4", "-crf", "23", "output.mp4"},
				HardwareCapabilities: HardwareCapabilities{
					AvailableEncoders: []encoder.EncoderFamily{
						encoder.EncoderH264NVENC,
						encoder.EncoderLibX264,
					},
					HardwareEncoders: []encoder.EncoderFamily{
						encoder.EncoderH264NVENC,
					},
					SupportedCodecs: []encoder.CodecFormat{encoder.CodecH264},
					GPUDevices: []GPUDevice{
						{Type: "nvenc", Vendor: "NVIDIA", Path: "0", Accessible: true},
					},
					EncoderPriority: DefaultEncoderPriority(),
				},
				EncoderParams: map[string]string{"crf": "23"},
				AutoHW:        true,
			},
			expectedScenario: ScenarioUnspecifiedEncoderWithHW,
			expectedEncoder:  encoder.EncoderH264NVENC,
			validateResult: func(t *testing.T, response *EncoderRewriteResponse) {
				// Should have hardware params injected
				if len(response.AuditRecords) == 0 {
					t.Error("expected audit records for hardware injection")
				}
			},
		},
		{
			name: "Scenario 2: Unspecified encoder without hardware",
			request: &EncoderRewriteRequest{
				OriginalArgs: []string{"-i", "input.mp4", "output.mp4"},
				HardwareCapabilities: HardwareCapabilities{
					AvailableEncoders: []encoder.EncoderFamily{
						encoder.EncoderLibX264,
					},
					SoftwareEncoders: []encoder.EncoderFamily{
						encoder.EncoderLibX264,
					},
					SupportedCodecs: []encoder.CodecFormat{encoder.CodecH264},
				},
			},
			expectedScenario: ScenarioUnspecifiedEncoderNoHW,
			expectedEncoder:  encoder.EncoderLibX264,
		},
		{
			name: "Scenario 3: Specified encoder supported",
			request: &EncoderRewriteRequest{
				OriginalArgs:     []string{"-i", "input.mp4", "-c:v", "h264_nvenc", "-cq", "23", "output.mp4"},
				SpecifiedEncoder: encoder.EncoderH264NVENC,
				HardwareCapabilities: HardwareCapabilities{
					AvailableEncoders: []encoder.EncoderFamily{
						encoder.EncoderH264NVENC,
						encoder.EncoderLibX264,
					},
					HardwareEncoders: []encoder.EncoderFamily{
						encoder.EncoderH264NVENC,
					},
					SupportedCodecs: []encoder.CodecFormat{encoder.CodecH264},
				},
				EncoderParams: map[string]string{"cq": "23"},
			},
			expectedScenario: ScenarioSpecifiedEncoderSupported,
			expectedEncoder:  encoder.EncoderH264NVENC,
		},
		{
			name: "Scenario 4: Specified encoder unsupported with hardware alternative",
			request: &EncoderRewriteRequest{
				OriginalArgs:     []string{"-i", "input.mp4", "-c:v", "h264_qsv", "-global_quality", "23", "output.mp4"},
				SpecifiedEncoder: encoder.EncoderH264QSV,
				HardwareCapabilities: HardwareCapabilities{
					AvailableEncoders: []encoder.EncoderFamily{
						encoder.EncoderH264NVENC,
						encoder.EncoderLibX264,
					},
					HardwareEncoders: []encoder.EncoderFamily{
						encoder.EncoderH264NVENC,
					},
					SupportedCodecs: []encoder.CodecFormat{encoder.CodecH264},
					GPUDevices: []GPUDevice{
						{Type: "nvenc", Vendor: "NVIDIA", Path: "0", Accessible: true},
					},
					EncoderPriority: DefaultEncoderPriority(),
				},
				EncoderParams: map[string]string{"global_quality": "23"},
			},
			expectedScenario: ScenarioSpecifiedEncoderUnsupportedWithAlternative,
			expectedEncoder:  encoder.EncoderH264NVENC,
		},
		{
			name: "Scenario 5: Specified encoder unsupported fallback to software",
			request: &EncoderRewriteRequest{
				OriginalArgs:     []string{"-i", "input.mp4", "-c:v", "h264_nvenc", "-cq", "23", "output.mp4"},
				SpecifiedEncoder: encoder.EncoderH264NVENC,
				HardwareCapabilities: HardwareCapabilities{
					AvailableEncoders: []encoder.EncoderFamily{
						encoder.EncoderLibX264,
					},
					SoftwareEncoders: []encoder.EncoderFamily{
						encoder.EncoderLibX264,
					},
					SupportedCodecs: []encoder.CodecFormat{encoder.CodecH264},
				},
				EncoderParams: map[string]string{"cq": "23"},
			},
			expectedScenario: ScenarioSpecifiedEncoderUnsupportedFallbackSoftware,
			expectedEncoder:  encoder.EncoderLibX264,
		},
		{
			name: "Scenario 6: Format not available",
			request: &EncoderRewriteRequest{
				OriginalArgs:     []string{"-i", "input.mp4", "-c:v", "h264_nvenc", "output.mp4"},
				SpecifiedEncoder: encoder.EncoderH264NVENC,
				HardwareCapabilities: HardwareCapabilities{
					AvailableEncoders: []encoder.EncoderFamily{
						encoder.EncoderLibX265,
					},
					SupportedCodecs: []encoder.CodecFormat{encoder.CodecHEVC},
				},
			},
			expectedScenario: ScenarioFormatNotAvailable,
			expectedEncoder:  "",
			expectError:      true,
			validateResult: func(t *testing.T, response *EncoderRewriteResponse) {
				if len(response.Errors) == 0 {
					t.Error("expected error for format not available scenario")
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create engine with all components
			engine := NewEngineCoordinator()

			// Set request metadata
			tt.request.RequestID = "test-" + tt.name
			tt.request.Timestamp = time.Now()

			// Perform rewrite
			response, err := engine.Rewrite(context.Background(), tt.request)

			if err != nil && !tt.expectError {
				t.Errorf("unexpected error: %v", err)
				return
			}

			if response == nil {
				t.Error("expected non-nil response")
				return
			}

			// Verify scenario
			if response.Scenario != tt.expectedScenario {
				t.Errorf("expected scenario %v, got %v", tt.expectedScenario, response.Scenario)
			}

			// Verify target encoder
			if response.TargetEncoder != tt.expectedEncoder {
				t.Errorf("expected encoder %s, got %s", tt.expectedEncoder, response.TargetEncoder)
			}

			// Run custom validation
			if tt.validateResult != nil {
				tt.validateResult(t, response)
			}

			// Verify audit records exist
			if len(response.AuditRecords) == 0 {
				t.Error("expected audit records to be generated")
			}

			// Verify request ID is preserved
			if response.RequestID != tt.request.RequestID {
				t.Errorf("expected request ID %s, got %s", tt.request.RequestID, response.RequestID)
			}
		})
	}
}

// TestEndToEnd_Performance verifies that single rewrite completes within 10ms.
func TestEndToEnd_Performance(t *testing.T) {
	engine := NewEngineCoordinator()

	request := &EncoderRewriteRequest{
		OriginalArgs: []string{"-i", "input.mp4", "-crf", "23", "output.mp4"},
		HardwareCapabilities: HardwareCapabilities{
			AvailableEncoders: []encoder.EncoderFamily{
				encoder.EncoderH264NVENC,
				encoder.EncoderLibX264,
			},
			HardwareEncoders: []encoder.EncoderFamily{
				encoder.EncoderH264NVENC,
			},
			SupportedCodecs: []encoder.CodecFormat{encoder.CodecH264},
			EncoderPriority: DefaultEncoderPriority(),
		},
		EncoderParams: map[string]string{"crf": "23"},
		AutoHW:        true,
	}

	// Measure performance over multiple iterations
	iterations := 100
	start := time.Now()

	for i := 0; i < iterations; i++ {
		_, err := engine.Rewrite(context.Background(), request)
		if err != nil {
			t.Errorf("rewrite failed: %v", err)
			return
		}
	}

	totalDuration := time.Since(start)
	avgDuration := totalDuration / time.Duration(iterations)

	t.Logf("Average rewrite time: %v", avgDuration)

	if avgDuration > 10*time.Millisecond {
		t.Errorf("average rewrite time %v exceeds 10ms requirement", avgDuration)
	}
}

// TestEndToEnd_ContextCancellation tests that context cancellation is handled properly.
func TestEndToEnd_ContextCancellation(t *testing.T) {
	engine := NewEngineCoordinator()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	request := &EncoderRewriteRequest{
		OriginalArgs: []string{"-i", "input.mp4", "output.mp4"},
		HardwareCapabilities: HardwareCapabilities{
			AvailableEncoders: []encoder.EncoderFamily{encoder.EncoderLibX264},
		},
	}

	_, err := engine.Rewrite(ctx, request)
	if err == nil {
		t.Error("expected error from cancelled context")
	}
}

// TestEndToEnd_ConcurrentRewrites tests concurrent rewrite operations.
func TestEndToEnd_ConcurrentRewrites(t *testing.T) {
	engine := NewEngineCoordinator()

	// Run multiple concurrent rewrites
	done := make(chan bool, 10)

	for i := 0; i < 10; i++ {
		go func(id int) {
			request := &EncoderRewriteRequest{
				OriginalArgs: []string{"-i", "input.mp4", "output.mp4"},
				HardwareCapabilities: HardwareCapabilities{
					AvailableEncoders: []encoder.EncoderFamily{
						encoder.EncoderH264NVENC,
						encoder.EncoderLibX264,
					},
					HardwareEncoders: []encoder.EncoderFamily{
						encoder.EncoderH264NVENC,
					},
					EncoderPriority: DefaultEncoderPriority(),
				},
				RequestID: "concurrent-" + string(rune('0'+id)),
			}

			response, err := engine.Rewrite(context.Background(), request)
			if err != nil {
				t.Errorf("concurrent rewrite %d failed: %v", id, err)
			}
			if response == nil {
				t.Errorf("concurrent rewrite %d returned nil response", id)
			}
			done <- true
		}(i)
	}

	// Wait for all goroutines to complete
	for i := 0; i < 10; i++ {
		<-done
	}
}

// TestEndToEnd_ComplexScenario tests a complex real-world scenario.
func TestEndToEnd_ComplexScenario(t *testing.T) {
	engine := NewEngineCoordinator()

	// Set up a mock translator that returns translated params
	mockTranslator := &mockTranslator{
		translateFunc: func(ctx context.Context, sourceEncoder, targetEncoder encoder.EncoderFamily, params map[string]string) (*TranslationResult, error) {
			// Translate QSV params to NVENC params
			translated := make(map[string]string)
			if v, ok := params["global_quality"]; ok {
				translated["cq"] = v
			}
			if v, ok := params["preset"]; ok {
				translated["preset"] = v // pass through
			}
			return &TranslationResult{
				TargetEncoder:    targetEncoder,
				TranslatedParams: translated,
				AuditRecords:     []AuditRecord{},
			}, nil
		},
	}
	engine.SetTranslator(mockTranslator)

	// Scenario: User requests QSV encoder but only NVENC is available
	// User also has encoder params that need translation
	request := &EncoderRewriteRequest{
		OriginalArgs: []string{
			"-i", "input.mp4",
			"-c:v", "h264_qsv",
			"-global_quality", "23",
			"-look_ahead", "1",
			"-preset", "medium",
			"output.mp4",
		},
		SpecifiedEncoder: encoder.EncoderH264QSV,
		HardwareCapabilities: HardwareCapabilities{
			AvailableEncoders: []encoder.EncoderFamily{
				encoder.EncoderH264NVENC,
				encoder.EncoderLibX264,
			},
			HardwareEncoders: []encoder.EncoderFamily{
				encoder.EncoderH264NVENC,
			},
			SupportedCodecs: []encoder.CodecFormat{encoder.CodecH264},
			GPUDevices: []GPUDevice{
				{Type: "nvenc", Vendor: "NVIDIA", Path: "0", Accessible: true},
			},
			EncoderPriority: DefaultEncoderPriority(),
		},
		EncoderParams: map[string]string{
			"global_quality": "23",
			"look_ahead":     "1",
			"preset":         "medium",
		},
	}

	response, err := engine.Rewrite(context.Background(), request)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
		return
	}

	// Should have switched to NVENC
	if response.TargetEncoder != encoder.EncoderH264NVENC {
		t.Errorf("expected NVENC, got %s", response.TargetEncoder)
	}

	// Should have performed translation
	if !response.TranslationPerformed {
		t.Error("expected translation to be performed")
	}

	// Should have notifications
	if len(response.Notifications) == 0 {
		t.Error("expected notifications about the rewrite")
	}

	// Rewritten args should have NVENC encoder
	foundNVENC := false
	for _, arg := range response.RewrittenArgs {
		if arg == "h264_nvenc" {
			foundNVENC = true
			break
		}
	}
	if !foundNVENC {
		t.Error("expected h264_nvenc in rewritten args")
	}
}

// TestEndToEnd_ParamsToFilter tests that the full Rewrite pipeline correctly
// filters translated params while preserving non-translated params.
// This uses the real TranslatorAdapter (via NewEngineCoordinator).
func TestEndToEnd_ParamsToFilter(t *testing.T) {
	engine := NewEngineCoordinator()

	// Scenario: libx264 -> h264_vaapi with --auto-hw
	// - crf should be translated to quality (filtered from original args)
	// - preset should be preserved (VAAPI doesn't support preset, so it should NOT be filtered)
	request := &EncoderRewriteRequest{
		OriginalArgs: []string{
			"-i", "input.mp4",
			"-c:v", "libx264",
			"-crf", "23",
			"-preset", "medium",
			"output.mp4",
		},
		SpecifiedEncoder: encoder.EncoderLibX264,
		HardwareCapabilities: HardwareCapabilities{
			AvailableEncoders: []encoder.EncoderFamily{
				encoder.EncoderH264VAAPI,
				encoder.EncoderLibX264,
			},
			HardwareEncoders: []encoder.EncoderFamily{
				encoder.EncoderH264VAAPI,
			},
			SoftwareEncoders: []encoder.EncoderFamily{
				encoder.EncoderLibX264,
			},
			SupportedCodecs: []encoder.CodecFormat{encoder.CodecH264},
			GPUDevices: []GPUDevice{
				{Type: "vaapi", Vendor: "AMD", Path: "/dev/dri/renderD128", Accessible: true},
			},
			EncoderPriority: DefaultEncoderPriority(),
		},
		EncoderParams: map[string]string{
			"crf":    "23",
			"preset": "medium",
		},
		AutoHW: true,
	}

	response, err := engine.Rewrite(context.Background(), request)
	if err != nil {
		t.Fatalf("rewrite failed: %v", err)
	}

	// Verify target encoder
	if response.TargetEncoder != encoder.EncoderH264VAAPI {
		t.Errorf("expected VAAPI target, got %s", response.TargetEncoder)
	}

	// Verify translation was performed
	if !response.TranslationPerformed {
		t.Error("expected translation to be performed")
	}

	// Verify -crf is NOT in rewritten args (should be translated to -quality)
	for _, arg := range response.RewrittenArgs {
		if arg == "-crf" || arg == "23" {
			// The value "23" could also appear as the quality value, so only check -crf flag
			if arg == "-crf" {
				t.Errorf("-crf should NOT be in rewritten args, got %v", response.RewrittenArgs)
			}
		}
	}

	// Verify -quality IS in rewritten args
	foundQuality := false
	for _, arg := range response.RewrittenArgs {
		if arg == "-quality" {
			foundQuality = true
			break
		}
	}
	if !foundQuality {
		t.Error("expected -quality in rewritten args")
	}

	// Verify -preset IS preserved (VAAPI doesn't translate preset, so it should remain)
	foundPreset := false
	for _, arg := range response.RewrittenArgs {
		if arg == "-preset" {
			foundPreset = true
			break
		}
	}
	if !foundPreset {
		t.Errorf("expected -preset to be preserved in rewritten args, got %v", response.RewrittenArgs)
	}
}

// TestEndToEnd_ParamsToFilter_NVENC tests the filter behavior for NVENC target.
// For NVENC, both crf->cq and preset mapping are supported, so all original
// params should be filtered.
func TestEndToEnd_ParamsToFilter_NVENC(t *testing.T) {
	engine := NewEngineCoordinator()

	request := &EncoderRewriteRequest{
		OriginalArgs: []string{
			"-i", "input.mp4",
			"-c:v", "libx264",
			"-crf", "23",
			"-preset", "slow",
			"output.mp4",
		},
		SpecifiedEncoder: encoder.EncoderLibX264,
		HardwareCapabilities: HardwareCapabilities{
			AvailableEncoders: []encoder.EncoderFamily{
				encoder.EncoderH264NVENC,
				encoder.EncoderLibX264,
			},
			HardwareEncoders: []encoder.EncoderFamily{
				encoder.EncoderH264NVENC,
			},
			SoftwareEncoders: []encoder.EncoderFamily{
				encoder.EncoderLibX264,
			},
			SupportedCodecs: []encoder.CodecFormat{encoder.CodecH264},
			GPUDevices: []GPUDevice{
				{Type: "nvenc", Vendor: "NVIDIA", Path: "0", Accessible: true},
			},
			EncoderPriority: DefaultEncoderPriority(),
		},
		EncoderParams: map[string]string{
			"crf":    "23",
			"preset": "slow",
		},
		AutoHW: true,
	}

	response, err := engine.Rewrite(context.Background(), request)
	if err != nil {
		t.Fatalf("rewrite failed: %v", err)
	}

	// Verify target encoder
	if response.TargetEncoder != encoder.EncoderH264NVENC {
		t.Errorf("expected NVENC target, got %s", response.TargetEncoder)
	}

	// Verify -crf is NOT in rewritten args (translated to -cq)
	for _, arg := range response.RewrittenArgs {
		if arg == "-crf" {
			t.Errorf("-crf should NOT be in rewritten args, got %v", response.RewrittenArgs)
		}
	}

	// Verify -cq IS present
	foundCQ := false
	for _, arg := range response.RewrittenArgs {
		if arg == "-cq" {
			foundCQ = true
			break
		}
	}
	if !foundCQ {
		t.Error("expected -cq in rewritten args")
	}

	// Verify libx264 is NOT in the args (replaced by h264_nvenc)
	for _, arg := range response.RewrittenArgs {
		if arg == "libx264" {
			t.Errorf("libx264 should NOT be in rewritten args, got %v", response.RewrittenArgs)
		}
	}

	// Verify h264_nvenc IS present
	foundNVENC := false
	for _, arg := range response.RewrittenArgs {
		if arg == "h264_nvenc" {
			foundNVENC = true
			break
		}
	}
	if !foundNVENC {
		t.Error("expected h264_nvenc in rewritten args")
	}
}
