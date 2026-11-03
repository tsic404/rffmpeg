package rewrite

import (
	"context"
	"testing"

	"github.com/tsic404/rffmpeg/pkg/encoder"
)

// mockTranslator is a mock implementation of ParameterTranslator for testing.
type mockTranslator struct {
	translateFunc func(ctx context.Context, sourceEncoder, targetEncoder encoder.EncoderFamily, params map[string]string) (*TranslationResult, error)
}

func (m *mockTranslator) Translate(ctx context.Context, sourceEncoder, targetEncoder encoder.EncoderFamily, params map[string]string) (*TranslationResult, error) {
	if m.translateFunc != nil {
		return m.translateFunc(ctx, sourceEncoder, targetEncoder, params)
	}
	return &TranslationResult{
		TargetEncoder:    targetEncoder,
		TranslatedParams: params,
		AuditRecords:     []AuditRecord{},
	}, nil
}

func (m *mockTranslator) GetSupportedTranslations(targetEncoder encoder.EncoderFamily) []encoder.EncoderFamily {
	return nil
}

func (m *mockTranslator) CanTranslate(sourceEncoder, targetEncoder encoder.EncoderFamily) bool {
	return true
}

// mockAuditRecorder is a mock implementation of AuditRecorder for testing.
type mockAuditRecorder struct {
	records []*AuditRecord
}

func (m *mockAuditRecorder) Record(ctx context.Context, record *AuditRecord) error {
	m.records = append(m.records, record)
	return nil
}

func (m *mockAuditRecorder) RecordBatch(ctx context.Context, records []*AuditRecord) error {
	m.records = append(m.records, records...)
	return nil
}

func (m *mockAuditRecorder) GetRecords(ctx context.Context, requestID string) ([]AuditRecord, error) {
	return nil, nil
}

func (m *mockAuditRecorder) GetSummary(ctx context.Context, requestID string) (*AuditSummary, error) {
	return nil, nil
}

// mockNotifier is a mock implementation of Notifier for testing.
type mockNotifier struct {
	notifications []*Notification
}

func (m *mockNotifier) Notify(ctx context.Context, notification *Notification) error {
	m.notifications = append(m.notifications, notification)
	return nil
}

func (m *mockNotifier) NotifyBatch(ctx context.Context, notifications []*Notification) error {
	m.notifications = append(m.notifications, notifications...)
	return nil
}

func (m *mockNotifier) FormatNotification(notification *Notification) string {
	return notification.Message
}

func (m *mockNotifier) SetOutput(output NotifierOutput) {}

func TestEngineCoordinator_Rewrite(t *testing.T) {
	tests := []struct {
		name             string
		request          *EncoderRewriteRequest
		expectError      bool
		expectScenario   ScenarioType
		expectEncoder    encoder.EncoderFamily
		validateResponse func(t *testing.T, resp *EncoderRewriteResponse)
	}{
		{
			name: "scenario 1 - unspecified encoder with HW",
			request: &EncoderRewriteRequest{
				OriginalArgs:     []string{"-i", "input.mp4", "output.mp4"},
				SpecifiedEncoder: "",
				HardwareCapabilities: HardwareCapabilities{
					AvailableEncoders: []encoder.EncoderFamily{encoder.EncoderH264NVENC, encoder.EncoderLibX264},
					HardwareEncoders:  []encoder.EncoderFamily{encoder.EncoderH264NVENC},
					EncoderPriority:   DefaultEncoderPriority(),
				},
			},
			expectError:    false,
			expectScenario: ScenarioUnspecifiedEncoderWithHW,
			expectEncoder:  encoder.EncoderH264NVENC,
		},
		{
			name: "scenario 2 - unspecified encoder no HW",
			request: &EncoderRewriteRequest{
				OriginalArgs:     []string{"-i", "input.mp4", "output.mp4"},
				SpecifiedEncoder: "",
				HardwareCapabilities: HardwareCapabilities{
					AvailableEncoders: []encoder.EncoderFamily{encoder.EncoderLibX264},
					SoftwareEncoders:  []encoder.EncoderFamily{encoder.EncoderLibX264},
				},
			},
			expectError:    false,
			expectScenario: ScenarioUnspecifiedEncoderNoHW,
			expectEncoder:  encoder.EncoderLibX264,
		},
		{
			name: "scenario 3 - specified encoder supported",
			request: &EncoderRewriteRequest{
				OriginalArgs:     []string{"-i", "input.mp4", "-c:v", "h264_nvenc", "output.mp4"},
				SpecifiedEncoder: encoder.EncoderH264NVENC,
				HardwareCapabilities: HardwareCapabilities{
					AvailableEncoders: []encoder.EncoderFamily{encoder.EncoderH264NVENC, encoder.EncoderLibX264},
					HardwareEncoders:  []encoder.EncoderFamily{encoder.EncoderH264NVENC},
				},
			},
			expectError:    false,
			expectScenario: ScenarioSpecifiedEncoderSupported,
			expectEncoder:  encoder.EncoderH264NVENC,
		},
		{
			name: "scenario 4 - unsupported with alternative",
			request: &EncoderRewriteRequest{
				OriginalArgs:     []string{"-i", "input.mp4", "-c:v", "h264_qsv", "output.mp4"},
				SpecifiedEncoder: encoder.EncoderH264QSV,
				HardwareCapabilities: HardwareCapabilities{
					AvailableEncoders: []encoder.EncoderFamily{encoder.EncoderH264NVENC, encoder.EncoderLibX264},
					HardwareEncoders:  []encoder.EncoderFamily{encoder.EncoderH264NVENC},
					SupportedCodecs:   []encoder.CodecFormat{encoder.CodecH264},
					EncoderPriority:   DefaultEncoderPriority(),
				},
			},
			expectError:    false,
			expectScenario: ScenarioSpecifiedEncoderUnsupportedWithAlternative,
			expectEncoder:  encoder.EncoderH264NVENC,
		},
		{
			name: "scenario 5 - unsupported fallback software",
			request: &EncoderRewriteRequest{
				OriginalArgs:     []string{"-i", "input.mp4", "-c:v", "h264_nvenc", "output.mp4"},
				SpecifiedEncoder: encoder.EncoderH264NVENC,
				HardwareCapabilities: HardwareCapabilities{
					AvailableEncoders: []encoder.EncoderFamily{encoder.EncoderLibX264},
					SoftwareEncoders:  []encoder.EncoderFamily{encoder.EncoderLibX264},
					SupportedCodecs:   []encoder.CodecFormat{encoder.CodecH264},
				},
			},
			expectError:    false,
			expectScenario: ScenarioSpecifiedEncoderUnsupportedFallbackSoftware,
			expectEncoder:  encoder.EncoderLibX264,
		},
		{
			name: "scenario 6 - format not available",
			request: &EncoderRewriteRequest{
				OriginalArgs:     []string{"-i", "input.mp4", "-c:v", "h264_nvenc", "output.mp4"},
				SpecifiedEncoder: encoder.EncoderH264NVENC,
				HardwareCapabilities: HardwareCapabilities{
					AvailableEncoders: []encoder.EncoderFamily{encoder.EncoderLibX265},
					SupportedCodecs:   []encoder.CodecFormat{encoder.CodecHEVC},
				},
			},
			expectError:    false,
			expectScenario: ScenarioFormatNotAvailable,
			expectEncoder:  "",
			validateResponse: func(t *testing.T, resp *EncoderRewriteResponse) {
				if len(resp.Errors) == 0 {
					t.Error("expected error for format not available")
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			engine := NewEngineCoordinator()
			engine.SetAuditRecorder(&mockAuditRecorder{})
			engine.SetNotifier(&mockNotifier{})
			engine.SetTranslator(&mockTranslator{})

			response, err := engine.Rewrite(context.Background(), tt.request)

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

			if response.Scenario != tt.expectScenario {
				t.Errorf("expected scenario %v, got %v", tt.expectScenario, response.Scenario)
			}

			if response.TargetEncoder != tt.expectEncoder {
				t.Errorf("expected encoder %s, got %s", tt.expectEncoder, response.TargetEncoder)
			}

			if tt.validateResponse != nil {
				tt.validateResponse(t, response)
			}

			// Verify audit records were generated
			if len(response.AuditRecords) == 0 {
				t.Error("expected audit records to be generated")
			}

			// Verify notifications were generated in all scenarios
			if len(response.Notifications) == 0 {
				t.Error("expected notifications to be generated")
			}
		})
	}
}

func TestEngineCoordinator_RewriteSync(t *testing.T) {
	engine := NewEngineCoordinator()

	hwCaps := &HardwareCapabilities{
		AvailableEncoders: []encoder.EncoderFamily{encoder.EncoderH264NVENC, encoder.EncoderLibX264},
		HardwareEncoders:  []encoder.EncoderFamily{encoder.EncoderH264NVENC},
		EncoderPriority:   DefaultEncoderPriority(),
	}

	// Test simple rewrite
	args, err := engine.RewriteSync(context.Background(), []string{"-i", "input.mp4", "output.mp4"}, hwCaps)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
		return
	}

	// Verify encoder was added
	foundEncoder := false
	for _, arg := range args {
		if arg == string(encoder.EncoderH264NVENC) {
			foundEncoder = true
			break
		}
	}
	if !foundEncoder {
		t.Error("expected h264_nvenc to be in rewritten args")
	}
}

// TestEngineCoordinator_ParseInlineEncoderArgs verifies the "=" joined encoder
// forms are not selections (ffmpeg rejects that syntax) and that the codec key
// does not leak into the extracted params.
func TestEngineCoordinator_ParseInlineEncoderArgs(t *testing.T) {
	engine := NewEngineCoordinator()

	tests := []struct {
		name          string
		args          []string
		expectEncoder encoder.EncoderFamily
		expectParams  map[string]string
	}{
		{
			name:          "inline -codec:v= is not a selection",
			args:          []string{"-codec:v=h264_nvenc", "-crf", "23", "output.mp4"},
			expectEncoder: "",
			expectParams:  map[string]string{"crf": "23"},
		},
		{
			name:          "inline -vcodec= is not a selection",
			args:          []string{"-vcodec=libx264", "output.mp4"},
			expectEncoder: "",
			expectParams:  map[string]string{},
		},
		{
			name:          "inline -c:v= is not a selection",
			args:          []string{"-c:v=libx264", "output.mp4"},
			expectEncoder: "",
			expectParams:  map[string]string{},
		},
		{
			name:          "separate -codec:v still detected",
			args:          []string{"-codec:v", "h264_nvenc", "output.mp4"},
			expectEncoder: encoder.EncoderH264NVENC,
			expectParams:  map[string]string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := engine.parseEncoderFromArgs(tt.args, nil); got != tt.expectEncoder {
				t.Errorf("parseEncoderFromArgs = %q, want %q", got, tt.expectEncoder)
			}

			got := engine.parseEncoderParamsFromArgs(tt.args, nil)
			if len(got) != len(tt.expectParams) {
				t.Errorf("parseEncoderParamsFromArgs = %v, want %v", got, tt.expectParams)
				return
			}
			for k, v := range tt.expectParams {
				if got[k] != v {
					t.Errorf("parseEncoderParamsFromArgs[%q] = %q, want %q", k, got[k], v)
				}
			}
		})
	}
}

func TestEngineCoordinator_BuildRewrittenArgs(t *testing.T) {
	engine := NewEngineCoordinator()

	tests := []struct {
		name              string
		originalArgs      []string
		targetEncoder     encoder.EncoderFamily
		params            map[string]string
		expectContains    []string
		expectNotContains []string
	}{
		{
			name:           "add encoder to args without encoder",
			originalArgs:   []string{"-i", "input.mp4", "-crf", "23", "output.mp4"},
			targetEncoder:  encoder.EncoderH264NVENC,
			params:         map[string]string{"cq": "23"},
			expectContains: []string{"-c:v", "h264_nvenc", "-cq", "23"},
		},
		{
			name:              "replace existing encoder",
			originalArgs:      []string{"-i", "input.mp4", "-c:v", "libx264", "-crf", "23", "output.mp4"},
			targetEncoder:     encoder.EncoderH264NVENC,
			params:            map[string]string{"cq": "23"},
			expectContains:    []string{"-c:v", "h264_nvenc", "-cq"},
			expectNotContains: []string{"libx264"},
		},
		{
			name:           "handle -codec:v syntax",
			originalArgs:   []string{"-i", "input.mp4", "-codec:v", "libx264", "output.mp4"},
			targetEncoder:  encoder.EncoderH264QSV,
			params:         map[string]string{},
			expectContains: []string{"-codec:v", "h264_qsv"},
		},
		{
			name:              "inline -codec:v= passes through unchanged",
			originalArgs:      []string{"-i", "input.mp4", "-codec:v=h264_nvenc", "output.mp4"},
			targetEncoder:     encoder.EncoderH264QSV,
			params:            map[string]string{},
			expectContains:    []string{"-codec:v=h264_nvenc"},
			expectNotContains: []string{"-codec:v=h264_qsv", "h264_qsv"},
		},
		{
			name:              "inline -vcodec= passes through unchanged",
			originalArgs:      []string{"-i", "input.mp4", "-vcodec=libx264", "output.mp4"},
			targetEncoder:     encoder.EncoderH264NVENC,
			params:            map[string]string{},
			expectContains:    []string{"-vcodec=libx264"},
			expectNotContains: []string{"-vcodec=h264_nvenc"},
		},
		{
			name:              "inline -c:v= passes through unchanged",
			originalArgs:      []string{"-i", "input.mp4", "-c:v=libx264", "output.mp4"},
			targetEncoder:     encoder.EncoderH264NVENC,
			params:            map[string]string{},
			expectContains:    []string{"-c:v=libx264"},
			expectNotContains: []string{"-c:v=h264_nvenc"},
		},
		{
			name:              "inline form ignores leaked codec param",
			originalArgs:      []string{"-i", "input.mp4", "-codec:v=h264_nvenc", "output.mp4"},
			targetEncoder:     encoder.EncoderH264QSV,
			params:            map[string]string{"codec:v": "h264_nvenc"},
			expectContains:    []string{"-codec:v=h264_nvenc"},
			expectNotContains: []string{"-codec:v=h264_qsv", "h264_qsv"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := engine.buildRewrittenArgs(tt.originalArgs, tt.targetEncoder, tt.params, nil, nil, nil)

			for _, expected := range tt.expectContains {
				found := false
				for _, arg := range result {
					if arg == expected {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("expected result to contain %s, got %v", expected, result)
				}
			}

			for _, notExpected := range tt.expectNotContains {
				for _, arg := range result {
					if arg == notExpected {
						t.Errorf("expected result to NOT contain %s, got %v", notExpected, result)
					}
				}
			}
		})
	}
}

func TestIsGlobalInitParam(t *testing.T) {
	tests := []struct {
		name     string
		param    string
		expected bool
	}{
		{"init_hw_device is global", "init_hw_device", true},
		{"hwaccel is global", "hwaccel", true},
		{"hwaccel_output_format is global", "hwaccel_output_format", true},
		{"crf is NOT global", "crf", false},
		{"cq is NOT global", "cq", false},
		{"gpu is NOT global", "gpu", false},
		{"empty string is NOT global", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isGlobalInitParam(tt.param)
			if result != tt.expected {
				t.Errorf("isGlobalInitParam(%q) = %v, want %v", tt.param, result, tt.expected)
			}
		})
	}
}

func TestGlobalInitParamNames(t *testing.T) {
	expected := []string{"init_hw_device", "hwaccel", "hwaccel_output_format"}

	if len(globalInitParamNames) != len(expected) {
		t.Errorf("globalInitParamNames length = %d, want %d", len(globalInitParamNames), len(expected))
	}

	for _, name := range expected {
		found := false
		for _, gp := range globalInitParamNames {
			if gp == name {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("globalInitParamNames missing expected entry: %q", name)
		}
	}
}

func TestHasGlobalParam(t *testing.T) {
	tests := []struct {
		name      string
		args      []string
		paramName string
		expected  bool
	}{
		{
			name:      "finds -init_hw_device in args",
			args:      []string{"-init_hw_device", "qsv=hw", "-i", "input.mp4", "output.mp4"},
			paramName: "init_hw_device",
			expected:  true,
		},
		{
			name:      "finds -hwaccel in args",
			args:      []string{"-hwaccel", "qsv", "-i", "input.mp4", "output.mp4"},
			paramName: "hwaccel",
			expected:  true,
		},
		{
			name:      "not found when param absent",
			args:      []string{"-i", "input.mp4", "-c:v", "h264_nvenc", "output.mp4"},
			paramName: "init_hw_device",
			expected:  false,
		},
		{
			name:      "not found with empty args",
			args:      []string{},
			paramName: "init_hw_device",
			expected:  false,
		},
		{
			name:      "finds param at end of args",
			args:      []string{"-i", "input.mp4", "-init_hw_device", "qsv=hw"},
			paramName: "init_hw_device",
			expected:  true,
		},
		{
			name:      "does not partial-match",
			args:      []string{"-i", "input.mp4", "-init_hw_device_type", "qsv"},
			paramName: "init_hw_device",
			expected:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := hasGlobalParam(tt.args, tt.paramName)
			if result != tt.expected {
				t.Errorf("hasGlobalParam(%v, %q) = %v, want %v", tt.args, tt.paramName, result, tt.expected)
			}
		})
	}
}

func TestBuildRewrittenArgs_GlobalInitParams(t *testing.T) {
	engine := NewEngineCoordinator()

	tests := []struct {
		name           string
		originalArgs   []string
		targetEncoder  encoder.EncoderFamily
		params         map[string]string
		expectContains []string
		expectOrder    func(t *testing.T, result []string)
	}{
		{
			name:          "init_hw_device is prepended before -i",
			originalArgs:  []string{"-i", "input.mp4", "-c:v", "libx264", "output.mp4"},
			targetEncoder: encoder.EncoderH264QSV,
			params: map[string]string{
				"init_hw_device": "qsv=hw,child_device=/dev/dri/renderD128",
				"qsv_device":     "/dev/dri/renderD128",
				"async_depth":    "1",
			},
			expectContains: []string{"-init_hw_device", "qsv=hw,child_device=/dev/dri/renderD128", "-c:v", "h264_qsv", "-qsv_device", "/dev/dri/renderD128", "-async_depth", "1"},
			expectOrder: func(t *testing.T, result []string) {
				initIdx := -1
				inputIdx := -1
				for i, arg := range result {
					if arg == "-init_hw_device" {
						initIdx = i
					}
					if arg == "-i" && inputIdx == -1 {
						inputIdx = i
					}
				}
				if initIdx < 0 {
					t.Error("expected -init_hw_device to be in result")
					return
				}
				if inputIdx < 0 {
					t.Error("expected -i to be in result")
					return
				}
				if initIdx >= inputIdx {
					t.Errorf("-init_hw_device (index %d) must appear before first -i (index %d)", initIdx, inputIdx)
				}
			},
		},
		{
			name:          "global init param already present in original args",
			originalArgs:  []string{"-init_hw_device", "qsv=hw", "-i", "input.mp4", "output.mp4"},
			targetEncoder: encoder.EncoderH264QSV,
			params: map[string]string{
				"init_hw_device": "qsv=hw,child_device=/dev/dri/renderD128",
				"qsv_device":     "/dev/dri/renderD128",
			},
			expectContains: []string{"-c:v", "h264_qsv"},
			expectOrder: func(t *testing.T, result []string) {
				// When init_hw_device exists in both original args AND params,
				// the original is stripped (it's in the params map) but
				// the new value is NOT prepended (hasGlobalParam prevents
				// duplication). The remaining encoder params are still appended.
				// Verify no orphaned init_hw_device value remains.
				for i, arg := range result {
					if arg == "qsv=hw" && (i == 0 || result[i-1] != "-init_hw_device") {
						t.Errorf("orphaned value %q without -init_hw_device flag at index %d", arg, i)
					}
				}
			},
		},
		{
			name:          "hwaccel is prepended as global init param",
			originalArgs:  []string{"-i", "input.mp4", "output.mp4"},
			targetEncoder: encoder.EncoderH264QSV,
			params: map[string]string{
				"hwaccel":               "qsv",
				"hwaccel_output_format": "qsv",
			},
			expectContains: []string{"-hwaccel", "qsv", "-hwaccel_output_format", "qsv", "-c:v", "h264_qsv"},
			expectOrder: func(t *testing.T, result []string) {
				hwaccelIdx := -1
				inputIdx := -1
				for i, arg := range result {
					if arg == "-hwaccel" && hwaccelIdx == -1 {
						hwaccelIdx = i
					}
					if arg == "-i" && inputIdx == -1 {
						inputIdx = i
					}
				}
				if hwaccelIdx >= 0 && inputIdx >= 0 && hwaccelIdx >= inputIdx {
					t.Errorf("-hwaccel (index %d) must appear before first -i (index %d)", hwaccelIdx, inputIdx)
				}
			},
		},
		{
			name:          "no global init params injected when none in params",
			originalArgs:  []string{"-i", "input.mp4", "output.mp4"},
			targetEncoder: encoder.EncoderH264NVENC,
			params: map[string]string{
				"cq": "23",
			},
			expectContains: []string{"-c:v", "h264_nvenc", "-cq", "23"},
			expectOrder: func(t *testing.T, result []string) {
				if len(result) > 0 && result[0] != "-i" {
					t.Logf("first arg is %q (no global params expected)", result[0])
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := engine.buildRewrittenArgs(tt.originalArgs, tt.targetEncoder, tt.params, nil, nil, nil)

			for _, expected := range tt.expectContains {
				found := false
				for _, arg := range result {
					if arg == expected {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("expected result to contain %q, got %v", expected, result)
				}
			}

			if tt.expectOrder != nil {
				tt.expectOrder(t, result)
			}
		})
	}
}

// TestBuildRewrittenArgs_EncoderPlacement verifies that when no -c:v is
// specified in the original args, the encoder is inserted BEFORE the output
// path (not after).  FFmpeg options apply to the NEXT file, so -c:v after
// the output path has no file to apply to and is ignored.
func TestBuildRewrittenArgs_EncoderPlacement(t *testing.T) {
	engine := NewEngineCoordinator()

	tests := []struct {
		name         string
		originalArgs []string
		targetEnc    encoder.EncoderFamily
	}{
		{
			name:         "no encoder, single output at end",
			originalArgs: []string{"-i", "input.mp4", "output.mp4"},
			targetEnc:    encoder.EncoderH264QSV,
		},
		{
			name:         "no encoder, output after flags",
			originalArgs: []string{"-i", "input.mp4", "-crf", "23", "output.mp4"},
			targetEnc:    encoder.EncoderH264NVENC,
		},
		{
			name:         "no encoder, multiple non-flag items (filter value then output)",
			originalArgs: []string{"-i", "input.mp4", "-filter_complex", "scale=1280:720", "output.mp4"},
			targetEnc:    encoder.EncoderH264QSV,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := engine.buildRewrittenArgs(tt.originalArgs, tt.targetEnc, map[string]string{}, nil, nil, nil)

			encIdx := -1
			outputIdx := -1
			for i, arg := range result {
				if arg == "-c:v" {
					encIdx = i
				}
				if arg == "output.mp4" {
					outputIdx = i
				}
			}
			if encIdx < 0 {
				t.Fatal("expected -c:v in result")
			}
			if outputIdx < 0 {
				t.Fatal("expected output.mp4 in result")
			}
			if encIdx >= outputIdx {
				t.Errorf("-c:v (index %d) must come BEFORE output.mp4 (index %d), got %v",
					encIdx, outputIdx, result)
			}
		})
	}
}

func TestBuildRewrittenArgs_ParamsToFilter(t *testing.T) {
	engine := NewEngineCoordinator()

	tests := []struct {
		name              string
		originalArgs      []string
		targetEncoder     encoder.EncoderFamily
		params            map[string]string
		paramsToFilter    map[string]string
		expectContains    []string
		expectNotContains []string
	}{
		{
			name:              "translated param (crf->quality) is filtered from original args",
			originalArgs:      []string{"-i", "input.mp4", "-c:v", "libx264", "-crf", "23", "-preset", "medium", "output.mp4"},
			targetEncoder:     encoder.EncoderH264VAAPI,
			params:            map[string]string{"quality": "55"},
			paramsToFilter:    map[string]string{"crf": "23"},
			expectContains:    []string{"-c:v", "h264_vaapi", "-quality", "55", "-preset", "medium"},
			expectNotContains: []string{"-crf", "23", "libx264"},
		},
		{
			name:              "non-translated param (preset) is preserved when not in paramsToFilter",
			originalArgs:      []string{"-i", "input.mp4", "-c:v", "libx264", "-crf", "23", "-preset", "medium", "output.mp4"},
			targetEncoder:     encoder.EncoderH264VAAPI,
			params:            map[string]string{"quality": "55"},
			paramsToFilter:    map[string]string{"crf": "23"}, // Only crf is filtered; preset is preserved
			expectContains:    []string{"-quality", "55", "-preset", "medium"},
			expectNotContains: []string{"-crf"},
		},
		{
			name:              "nil paramsToFilter - original crf passes through when not in params",
			originalArgs:      []string{"-i", "input.mp4", "-c:v", "libx264", "-crf", "23", "output.mp4"},
			targetEncoder:     encoder.EncoderH264NVENC,
			params:            map[string]string{"cq": "23"},
			paramsToFilter:    nil, // No additional filtering; -crf passes through since not in params
			expectContains:    []string{"-cq", "23"},
			expectNotContains: []string{"libx264"},
		},
		{
			name:           "empty paramsToFilter - no filtering",
			originalArgs:   []string{"-i", "input.mp4", "-c:v", "libx264", "-crf", "23", "output.mp4"},
			targetEncoder:  encoder.EncoderH264VAAPI,
			params:         map[string]string{"quality": "55"},
			paramsToFilter: map[string]string{}, // Empty filter
			expectContains: []string{"-quality", "55"},
		},
		{
			name:              "multiple translated params all filtered",
			originalArgs:      []string{"-i", "input.mp4", "-c:v", "libx265", "-crf", "28", "-preset", "slow", "output.mp4"},
			targetEncoder:     encoder.EncoderHEVCVAAPI,
			params:            map[string]string{"quality": "46"},
			paramsToFilter:    map[string]string{"crf": "28", "preset": "slow"}, // Both crf and preset filtered
			expectContains:    []string{"-c:v", "hevc_vaapi", "-quality", "46"},
			expectNotContains: []string{"-crf", "28", "-preset", "slow", "libx265"},
		},
		{
			name:              "param with equals syntax (-crf=23) is filtered",
			originalArgs:      []string{"-i", "input.mp4", "-c:v", "libx264", "-crf=23", "output.mp4"},
			targetEncoder:     encoder.EncoderH264NVENC,
			params:            map[string]string{"cq": "23"},
			paramsToFilter:    map[string]string{"crf": "23"},
			expectContains:    []string{"-c:v", "h264_nvenc", "-cq", "23"},
			expectNotContains: []string{"-crf=23", "-crf"},
		},
		{
			// Blocking regression (round 2): a boolean flag adjacent to the
			// output path must not be mis-paired as flag+value; the inserted
			// encoder/params must land BEFORE the output path.
			name:              "boolean flag adjacent to output path does not break insertion",
			originalArgs:      []string{"-y", "-i", "in.mkv", "-shortest", "out.mp4"},
			targetEncoder:     encoder.EncoderH264VAAPI,
			params:            map[string]string{"quality": "55"},
			paramsToFilter:    map[string]string{},
			expectContains:    []string{"-shortest", "-c:v", "h264_vaapi", "-quality", "55"},
			expectNotContains: []string{},
		},
		{
			// Non-blocking regression (round 2 #2): full-name keys like "b:v"
			// must match before base-name stripping; the original occurrence
			// is marked seen so backfill does not duplicate it.
			name:              "stream-specifier full name matches table key without duplication",
			originalArgs:      []string{"-i", "input.mp4", "-c:v", "libx264", "-b:v", "5M", "output.mp4"},
			targetEncoder:     encoder.EncoderH264NVENC,
			params:            map[string]string{"b:v": "5M"},
			paramsToFilter:    map[string]string{},
			expectContains:    []string{"output.mp4"},
			expectNotContains: []string{},
		},
		{
			// Regression: translated params must NOT be inserted between a
			// trailing flag and its value (e.g., between "-t" and "60").
			name:              "translated params inserted before output, not inside trailing flag/value pair",
			originalArgs:      []string{"-i", "input.mp4", "-t", "60", "output.mp4"},
			targetEncoder:     encoder.EncoderH264VAAPI,
			params:            map[string]string{"quality": "55"},
			paramsToFilter:    map[string]string{},
			expectContains:    []string{"-quality"},
			expectNotContains: []string{},
		},
		{
			// Regression: inline filter form must drop only the flag itself,
			// never the output path that follows it.
			name:              "inline filter form does not swallow following output path",
			originalArgs:      []string{"-i", "input.mp4", "-c:v", "libx264", "-crf=23", "output.mp4"},
			targetEncoder:     encoder.EncoderH264NVENC,
			params:            map[string]string{"cq": "23"},
			paramsToFilter:    map[string]string{"crf": "23"},
			expectContains:    []string{"output.mp4"},
			expectNotContains: []string{"-crf=23"},
		},
		{
			name:              "value not starting with dash is kept as flag value, not dropped",
			originalArgs:      []string{"-i", "input.mp4", "-c:v", "libx264", "-preset", "fast", "-movflags", "+faststart", "output.mp4"},
			targetEncoder:     encoder.EncoderH264VAAPI,
			params:            map[string]string{"quality": "55"},
			paramsToFilter:    map[string]string{},
			expectContains:    []string{"-movflags", "+faststart", "-preset", "fast"},
			expectNotContains: []string{"-crf"},
		},
		{
			name:              "stream-specifier suffix param (-crf:v) is filtered by base name",
			originalArgs:      []string{"-i", "input.mp4", "-c:v", "libx264", "-crf:v", "23", "output.mp4"},
			targetEncoder:     encoder.EncoderH264NVENC,
			params:            map[string]string{"cq": "23"},
			paramsToFilter:    map[string]string{"crf": "23"},
			expectContains:    []string{"-c:v", "h264_nvenc", "-cq", "23"},
			expectNotContains: []string{"-crf:v", "-crf"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := engine.buildRewrittenArgs(tt.originalArgs, tt.targetEncoder, tt.params, tt.paramsToFilter, nil, nil)

			for _, expected := range tt.expectContains {
				found := false
				for _, arg := range result {
					if arg == expected {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("expected result to contain %q, got %v", expected, result)
				}
			}

			for _, notExpected := range tt.expectNotContains {
				for _, arg := range result {
					if arg == notExpected {
						t.Errorf("expected result to NOT contain %q, got %v", notExpected, result)
					}
				}
			}
		})
	}
}

// TestBuildRewrittenArgs_InsertBeforeOutputWithDashValue pins the fix:
// a value-taking option whose value starts with '-' ("-map -0:a") must not
// make the following token look like a flag; the rewritten encoder and its
// parameters have to land before the output file, or ffmpeg silently ignores
// them (per-file options apply to the next output).
func TestBuildRewrittenArgs_InsertBeforeOutputWithDashValue(t *testing.T) {
	engine := NewEngineCoordinator()
	originalArgs := []string{
		"-i", "in.mp4", "-c:v", "libx264", "-crf", "23",
		"-map", "0:v", "-map", "-0:a", "out.mp4",
	}
	result := engine.buildRewrittenArgs(
		originalArgs,
		encoder.EncoderH264NVENC,
		map[string]string{"cq": "23"},
		map[string]string{"crf": "23"},
		nil,
		nil,
	)

	pos := func(token string) int {
		for i, arg := range result {
			if arg == token {
				return i
			}
		}
		return -1
	}
	outputPos := pos("out.mp4")
	if outputPos != len(result)-1 {
		t.Fatalf("output file is not last: %v", result)
	}
	if cqPos := pos("-cq"); cqPos < 0 || cqPos > outputPos {
		t.Errorf("-cq must be inserted before the output file, got %v", result)
	}
	if cPos := pos("-c:v"); cPos < 0 || cPos > outputPos {
		t.Errorf("-c:v must be inserted before the output file, got %v", result)
	}
}

// TestTranslatorAdapter_CanTranslate_CrossFormatRejected verifies that a
// cross-format translation is refused: rewriting AV1 parameters to any H.264
// encoder would silently change the requested output codec, so the mapping
// must not advertise AV1 -> H.264 translation rules for any target in the
// family. The same-family AV1 case is asserted as the positive control,
// proving the false result is the family boundary rather than a translator
// that always reports false.
func TestTranslatorAdapter_CanTranslate_CrossFormatRejected(t *testing.T) {
	adapter := NewTranslatorAdapter()

	h264Targets := []encoder.EncoderFamily{
		encoder.EncoderLibX264,
		encoder.EncoderH264NVENC,
		encoder.EncoderH264QSV,
		encoder.EncoderH264VAAPI,
		encoder.EncoderH264AMF,
		encoder.EncoderH264VT,
	}
	for _, target := range h264Targets {
		if adapter.CanTranslate(encoder.EncoderLibSVTAV1, target) {
			t.Errorf("CanTranslate(AV1, %s) should be false: cross-format translation is rejected", target)
		}
	}

	if !adapter.CanTranslate(encoder.EncoderLibSVTAV1, encoder.EncoderAV1NVENC) {
		t.Error("CanTranslate(AV1, AV1) should be true: same-family translation rules exist")
	}
}

// TestFormatParamFateReport pins the report against records that describe no
// applied rewrite: a failed translation (its converter may have returned a
// value, but the original parameter is what survives) and a hardware
// injection (not a user parameter) must not be rendered as a mapping.
func TestFormatParamFateReport(t *testing.T) {
	records := []AuditRecord{
		{
			SourceParam: "crf", SourceValue: "23",
			TargetParam: "global_quality", TargetValue: "23",
			ErrorMessage: "value conversion failed", Success: false,
		},
		{
			SourceParam: "async_depth", TargetParam: "async_depth", TargetValue: "4",
			Reason: encoder.ConverterUsedHardwareInjection, Success: true,
		},
	}
	translated := map[string]string{"crf": "23", "async_depth": "4"}

	if got := formatParamFateReport(records, translated, encoder.EncoderH264QSV); got != "" {
		t.Errorf("formatParamFateReport() = %q, want empty report", got)
	}
}

// TestBuildRewrittenArgs_SameNameConversion pins where a converted same-name
// value is applied: the separate-value form carries the converted value, while
// the inline "-preset=<value>" form keeps the user's own string — ffmpeg
// rejects encoder options in that form, so rewriting it would only put a value
// the user never typed into ffmpeg's error message.
func TestBuildRewrittenArgs_SameNameConversion(t *testing.T) {
	engine := NewEngineCoordinator()
	params := map[string]string{"preset": "6"}
	converted := map[string]string{"preset": "6"}

	has := func(args []string, want string) bool {
		for _, arg := range args {
			if arg == want {
				return true
			}
		}
		return false
	}
	hasPair := func(args []string, flag, value string) bool {
		for i := 0; i+1 < len(args); i++ {
			if args[i] == flag && args[i+1] == value {
				return true
			}
		}
		return false
	}

	t.Run("separate-value form carries the converted value", func(t *testing.T) {
		result := engine.buildRewrittenArgs(
			[]string{"-i", "input.mp4", "-c:v", "libx264", "-preset", "fast", "output.mp4"},
			encoder.EncoderH264QSV, params, nil, converted, nil,
		)
		if !hasPair(result, "-preset", "6") {
			t.Errorf("expected -preset 6 in rewritten args, got %v", result)
		}
		if has(result, "fast") {
			t.Errorf("original preset value must be replaced, got %v", result)
		}
	})

	t.Run("inline form keeps the user's value", func(t *testing.T) {
		result := engine.buildRewrittenArgs(
			[]string{"-i", "input.mp4", "-c:v", "libx264", "-preset=fast", "output.mp4"},
			encoder.EncoderH264QSV, params, nil, converted, nil,
		)
		if !has(result, "-preset=fast") {
			t.Errorf("expected the user's -preset=fast to survive, got %v", result)
		}
		if has(result, "-preset=6") {
			t.Errorf("inline preset must not be rewritten, got %v", result)
		}
	})
}

// fixedInjector always reports the given injected parameters, ignoring whether
// the user already supplied them. It isolates the engine's merge guard from
// the hardware injector's own duplicate check.
type fixedInjector struct {
	params map[string]string
}

func (f *fixedInjector) Inject(ctx context.Context, enc encoder.EncoderFamily, params map[string]string, hwCaps *HardwareCapabilities) (*InjectionResult, error) {
	return &InjectionResult{
		InjectedParams: f.params,
		AllParams:      params,
		AuditRecords:   []AuditRecord{},
	}, nil
}

func (f *fixedInjector) GetRequiredParams(enc encoder.EncoderFamily) []string { return nil }

func (f *fixedInjector) SupportsEncoder(enc encoder.EncoderFamily) bool { return true }

// TestRewrite_InjectionMergeKeepsUserParam pins the second C-73 guard: even
// when the injector returns a parameter the user already supplied, the engine
// merge must keep the user's value.
func TestRewrite_InjectionMergeKeepsUserParam(t *testing.T) {
	engine := NewEngineCoordinatorWithComponents(
		NewScenarioClassifier(),
		&mockTranslator{
			translateFunc: func(ctx context.Context, source, target encoder.EncoderFamily, params map[string]string) (*TranslationResult, error) {
				return &TranslationResult{
					TargetEncoder:    target,
					TranslatedParams: map[string]string{"gpu": "3"},
					AuditRecords:     []AuditRecord{},
				}, nil
			},
		},
		&fixedInjector{params: map[string]string{"gpu": "0"}},
		nil, nil, nil,
	)

	response, err := engine.Rewrite(context.Background(), &EncoderRewriteRequest{
		OriginalArgs:     []string{"-i", "in.mp4", "-c:v", "libx264", "-gpu", "3", "out.mp4"},
		SpecifiedEncoder: encoder.EncoderLibX264,
		EncoderParams:    map[string]string{"gpu": "3"},
		AutoHW:           true,
		HardwareCapabilities: HardwareCapabilities{
			AvailableEncoders: []encoder.EncoderFamily{encoder.EncoderH264NVENC, encoder.EncoderLibX264},
			HardwareEncoders:  []encoder.EncoderFamily{encoder.EncoderH264NVENC},
			SoftwareEncoders:  []encoder.EncoderFamily{encoder.EncoderLibX264},
			SupportedCodecs:   []encoder.CodecFormat{encoder.CodecH264},
			EncoderPriority:   DefaultEncoderPriority(),
		},
	})
	if err != nil {
		t.Fatalf("rewrite failed: %v", err)
	}

	hasPair := func(args []string, flag, value string) bool {
		for i := 0; i+1 < len(args); i++ {
			if args[i] == flag && args[i+1] == value {
				return true
			}
		}
		return false
	}
	if !hasPair(response.RewrittenArgs, "-gpu", "3") {
		t.Errorf("user's -gpu 3 was lost: %v", response.RewrittenArgs)
	}
	if hasPair(response.RewrittenArgs, "-gpu", "0") {
		t.Errorf("injected -gpu 0 overrode the user's value: %v", response.RewrittenArgs)
	}
}
