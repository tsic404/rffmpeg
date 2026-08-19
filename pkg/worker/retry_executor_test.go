package worker

import (
	"testing"
	"time"
)

func TestRetryStage_String(t *testing.T) {
	tests := []struct {
		name     string
		stage    RetryStage
		expected string
	}{
		{"initial", RetryStageInitial, "initial"},
		{"hardware_pruned", RetryStageHardwarePruned, "hardware_pruned"},
		{"advanced_pruned", RetryStageAdvancedPruned, "advanced_pruned"},
		{"software_fallback", RetryStageSoftwareFallback, "software_fallback"},
		{"exhausted", RetryStageExhausted, "exhausted"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.stage.String(); got != tt.expected {
				t.Errorf("RetryStage.String() = %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestRetryStage_Description(t *testing.T) {
	tests := []struct {
		name  string
		stage RetryStage
	}{
		{"initial", RetryStageInitial},
		{"hardware_pruned", RetryStageHardwarePruned},
		{"advanced_pruned", RetryStageAdvancedPruned},
		{"software_fallback", RetryStageSoftwareFallback},
		{"exhausted", RetryStageExhausted},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.stage.Description(); got == "" {
				t.Error("RetryStage.Description() returned empty string")
			}
		})
	}
}

func TestDefaultRetryConfig(t *testing.T) {
	config := DefaultRetryConfig()

	if config.MaxRetries != 3 {
		t.Errorf("DefaultRetryConfig().MaxRetries = %v, want 3", config.MaxRetries)
	}
	if config.InitialInterval != 1*time.Second {
		t.Errorf("DefaultRetryConfig().InitialInterval = %v, want 1s", config.InitialInterval)
	}
	if config.UseExponentialBackoff {
		t.Error("DefaultRetryConfig().UseExponentialBackoff should be false")
	}
	if !config.EnableSoftwareFallback {
		t.Error("DefaultRetryConfig().EnableSoftwareFallback should be true")
	}
}

func TestExtractEncoderFromArgs(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		expected string
	}{
		{
			name:     "c:v with separate value",
			args:     []string{"-i", "input.mp4", "-c:v", "h264_nvenc", "output.mp4"},
			expected: "h264_nvenc",
		},
		{
			name:     "c:v with equals value",
			args:     []string{"-i", "input.mp4", "-c:v=libx264", "output.mp4"},
			expected: "libx264",
		},
		{
			name:     "vcodec with separate value",
			args:     []string{"-i", "input.mp4", "-vcodec", "hevc_qsv", "output.mp4"},
			expected: "hevc_qsv",
		},
		{
			name:     "vcodec with equals value",
			args:     []string{"-i", "input.mp4", "-vcodec=libx265", "output.mp4"},
			expected: "libx265",
		},
		{
			name:     "no encoder",
			args:     []string{"-i", "input.mp4", "output.mp4"},
			expected: "",
		},
		{
			name:     "codec:v with separate value",
			args:     []string{"-i", "input.mp4", "-codec:v", "vp9_vaapi", "output.mp4"},
			expected: "vp9_vaapi",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := extractEncoderFromArgs(tt.args); got != tt.expected {
				t.Errorf("extractEncoderFromArgs() = %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestRetryResult_GetAuditTrailSummary(t *testing.T) {
	result := &RetryResult{
		TotalAttempts:       3,
		FinalStage:          RetryStageAdvancedPruned,
		Success:             true,
		UsedSoftwareEncoder: false,
		OriginalEncoder:     "h264_nvenc",
		FinalEncoder:        "h264_nvenc",
		AuditTrail: []RetryAuditEntry{
			{
				Timestamp:     time.Now(),
				Stage:         RetryStageInitial,
				AttemptNumber: 1,
				ExitCode:      1,
				Success:       false,
				ErrorType:     "hwaccel_failed",
			},
			{
				Timestamp:     time.Now(),
				Stage:         RetryStageHardwarePruned,
				AttemptNumber: 2,
				ExitCode:      0,
				Success:       true,
			},
		},
	}

	summary := result.GetAuditTrailSummary()

	if summary == "" {
		t.Error("GetAuditTrailSummary() returned empty string")
	}
}

// MockExecutor is a mock executor for testing.
type MockExecutor struct {
	results []ExecResult
	callIdx int
}

func (m *MockExecutor) Execute(ctx interface{}, args []string) ExecResult {
	if m.callIdx < len(m.results) {
		result := m.results[m.callIdx]
		m.callIdx++
		return result
	}
	return ExecResult{ExitCode: 0}
}

func TestEncoderFallback_IsHardwareEncoder(t *testing.T) {
	fallback := NewEncoderFallback()

	tests := []struct {
		name     string
		encoder  string
		expected bool
	}{
		{"nvenc", "h264_nvenc", true},
		{"qsv", "hevc_qsv", true},
		{"vaapi", "h264_vaapi", true},
		{"amf", "hevc_amf", true},
		{"videotoolbox", "h264_videotoolbox", true},
		{"software x264", "libx264", false},
		{"software x265", "libx265", false},
		{"software vp9", "libvpx-vp9", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := fallback.IsHardwareEncoder(tt.encoder); got != tt.expected {
				t.Errorf("IsHardwareEncoder(%v) = %v, want %v", tt.encoder, got, tt.expected)
			}
		})
	}
}

func TestEncoderFallback_GetSoftwareEncoder(t *testing.T) {
	fallback := NewEncoderFallback()

	tests := []struct {
		name     string
		encoder  string
		expected string
	}{
		{"h264_nvenc", "h264_nvenc", "libx264"},
		{"hevc_nvenc", "hevc_nvenc", "libx265"},
		{"h264_qsv", "h264_qsv", "libx264"},
		{"hevc_qsv", "hevc_qsv", "libx265"},
		{"h264_vaapi", "h264_vaapi", "libx264"},
		{"vp9_vaapi", "vp9_vaapi", "libvpx-vp9"},
		{"libx264", "libx264", "libx264"},
		{"libx265", "libx265", "libx265"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := fallback.GetSoftwareEncoder(tt.encoder); got != tt.expected {
				t.Errorf("GetSoftwareEncoder(%v) = %v, want %v", tt.encoder, got, tt.expected)
			}
		})
	}
}

func TestEncoderFallback_PrepareFallbackArgs(t *testing.T) {
	fallback := NewEncoderFallback()

	tests := []struct {
		name        string
		args        []string
		outputPath  string
		wantNil     bool
		wantEncoder string
	}{
		{
			name:        "nvenc fallback",
			args:        []string{"-i", "input.mp4", "-c:v", "h264_nvenc", "-preset", "fast", "output.mp4"},
			outputPath:  "output.mp4",
			wantNil:     false,
			wantEncoder: "libx264",
		},
		{
			name:       "already software encoder",
			args:       []string{"-i", "input.mp4", "-c:v", "libx264", "output.mp4"},
			outputPath: "output.mp4",
			wantNil:    true, // No fallback needed for software encoder
		},
		{
			name:       "no encoder specified",
			args:       []string{"-i", "input.mp4", "output.mp4"},
			outputPath: "output.mp4",
			wantNil:    true, // Cannot determine fallback
		},
		{
			name:        "qsv fallback with hardware params",
			args:        []string{"-i", "input.mp4", "-c:v", "hevc_qsv", "-qsv_device", "/dev/dri/renderD128", "-preset", "fast", "output.mp4"},
			outputPath:  "output.mp4",
			wantNil:     false,
			wantEncoder: "libx265",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := fallback.PrepareFallbackArgs(tt.args, tt.outputPath)

			if tt.wantNil {
				if got != nil {
					t.Errorf("PrepareFallbackArgs() = %v, want nil", got)
				}
				return
			}

			if got == nil {
				t.Error("PrepareFallbackArgs() returned nil, want non-nil")
				return
			}

			// Check that encoder was replaced
			encoder := extractEncoderFromArgs(got)
			if encoder != tt.wantEncoder {
				t.Errorf("PrepareFallbackArgs() encoder = %v, want %v", encoder, tt.wantEncoder)
			}
		})
	}
}

func TestEncoderFallback_GetEncoderFormat(t *testing.T) {
	fallback := NewEncoderFallback()

	tests := []struct {
		name     string
		encoder  string
		expected string
	}{
		{"h264_nvenc", "h264_nvenc", "h264"},
		{"hevc_nvenc", "hevc_nvenc", "hevc"},
		{"libx264", "libx264", "h264"},
		{"libx265", "libx265", "hevc"},
		{"vp9_vaapi", "vp9_vaapi", "vp9"},
		{"av1_nvenc", "av1_nvenc", "av1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := fallback.GetEncoderFormat(tt.encoder); got != tt.expected {
				t.Errorf("GetEncoderFormat(%v) = %v, want %v", tt.encoder, got, tt.expected)
			}
		})
	}
}

func TestEncoderFallback_GetAvailableSoftwareEncoders(t *testing.T) {
	fallback := NewEncoderFallback()

	tests := []struct {
		name          string
		format        string
		expectCount   int
		expectEncoder string
	}{
		{"h264", "h264", 2, "libx264"},
		{"hevc", "hevc", 1, "libx265"},
		{"vp9", "vp9", 1, "libvpx-vp9"},
		{"unknown", "unknown", 0, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := fallback.GetAvailableSoftwareEncoders(tt.format)

			if len(got) != tt.expectCount {
				t.Errorf("GetAvailableSoftwareEncoders(%v) count = %v, want %v", tt.format, len(got), tt.expectCount)
			}

			if tt.expectEncoder != "" && len(got) > 0 {
				found := false
				for _, e := range got {
					if e == tt.expectEncoder {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("GetAvailableSoftwareEncoders(%v) missing expected encoder %v", tt.format, tt.expectEncoder)
				}
			}
		})
	}
}

func TestEncoderFallback_AddEncoderMapping(t *testing.T) {
	fallback := NewEncoderFallback()

	// Add a new mapping
	fallback.AddEncoderMapping("custom_hw_encoder", "custom_sw_encoder")

	// Verify it was added
	got := fallback.GetSoftwareEncoder("custom_hw_encoder")
	if got != "custom_sw_encoder" {
		t.Errorf("GetSoftwareEncoder() = %v, want custom_sw_encoder", got)
	}
}

func TestEncoderFallback_ValidateEncoder(t *testing.T) {
	fallback := NewEncoderFallback()

	tests := []struct {
		name     string
		encoder  string
		expected bool
	}{
		{"h264_nvenc", "h264_nvenc", true},
		{"libx264", "libx264", true},
		{"unknown_encoder", "unknown_encoder", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := fallback.ValidateEncoder(tt.encoder); got != tt.expected {
				t.Errorf("ValidateEncoder(%v) = %v, want %v", tt.encoder, got, tt.expected)
			}
		})
	}
}

func TestEncoderFallback_isHardwareSpecificParam(t *testing.T) {
	fallback := NewEncoderFallback()

	tests := []struct {
		param    string
		expected bool
	}{
		{"-hwaccel", true},
		{"-hwaccel_device", true},
		{"-vaapi_device", true},
		{"-qsv_device", true},
		{"-gpu", true},
		{"-i", false},
		{"-c:v", false},
		{"-preset", false},
	}

	for _, tt := range tests {
		t.Run(tt.param, func(t *testing.T) {
			if got := fallback.isHardwareSpecificParam(tt.param); got != tt.expected {
				t.Errorf("isHardwareSpecificParam(%v) = %v, want %v", tt.param, got, tt.expected)
			}
		})
	}
}

func TestRetryAuditEntry(t *testing.T) {
	entry := RetryAuditEntry{
		Timestamp:     time.Now(),
		Stage:         RetryStageInitial,
		AttemptNumber: 1,
		Args:          []string{"-i", "input.mp4", "output.mp4"},
		ExitCode:      0,
		Success:       true,
	}

	if entry.Timestamp.IsZero() {
		t.Error("Timestamp should not be zero")
	}
	if entry.Stage != RetryStageInitial {
		t.Errorf("Stage = %v, want %v", entry.Stage, RetryStageInitial)
	}
}

func TestRetryResult(t *testing.T) {
	result := &RetryResult{
		FinalResult: ExecResult{
			ExitCode: 0,
			Stdout:   "",
			Stderr:   "",
		},
		TotalAttempts:       1,
		FinalStage:          RetryStageInitial,
		Success:             true,
		UsedSoftwareEncoder: false,
		OriginalEncoder:     "h264_nvenc",
		FinalEncoder:        "h264_nvenc",
		AuditTrail:          []RetryAuditEntry{},
	}

	if !result.Success {
		t.Error("Success should be true")
	}
	if result.TotalAttempts != 1 {
		t.Errorf("TotalAttempts = %v, want 1", result.TotalAttempts)
	}
}

func TestPruneHardwareParams_SwitchesEncoder(t *testing.T) {
	// Create a RetryExecutor with a mock executor and real fallback/pruner
	fallback := NewEncoderFallback()
	pruner := NewParamPruner()
	executor := NewExecutor("ffmpeg", 2*time.Hour)

	re := &RetryExecutor{
		executor:    executor,
		interceptor: NewErrorInterceptor(),
		pruner:      pruner,
		config:      DefaultRetryConfig(),
		fallback:    fallback,
	}

	// Input args with h264_qsv hardware encoder + hardware device params
	args := []string{"-i", "input.mp4", "-c:v", "h264_qsv", "-qsv_device", "/dev/dri/renderD128", "-preset", "fast", "output.mp4"}

	prunedArgs, prunedParams := re.pruneHardwareParams(args)

	// Verify encoder was switched to software
	encoder := extractEncoderFromArgs(prunedArgs)
	if encoder != "libx264" {
		t.Errorf("Expected encoder libx264, got %s", encoder)
	}

	// Verify hardware params were removed
	for _, arg := range prunedArgs {
		if arg == "h264_qsv" {
			t.Error("h264_qsv encoder should have been replaced")
		}
		if arg == "-qsv_device" || arg == "/dev/dri/renderD128" {
			t.Errorf("Hardware param %s should have been pruned", arg)
		}
	}

	// Verify pruned params list includes encoder switch
	foundEncoderSwitch := false
	for _, p := range prunedParams {
		if p == "encoder:h264_qsv->libx264" {
			foundEncoderSwitch = true
		}
	}
	if !foundEncoderSwitch {
		t.Errorf("Expected encoder switch in pruned params, got %v", prunedParams)
	}
}

func TestPruneHardwareParams_NoSwitchForSoftwareEncoder(t *testing.T) {
	fallback := NewEncoderFallback()
	pruner := NewParamPruner()
	executor := NewExecutor("ffmpeg", 2*time.Hour)

	re := &RetryExecutor{
		executor:    executor,
		interceptor: NewErrorInterceptor(),
		pruner:      pruner,
		config:      DefaultRetryConfig(),
		fallback:    fallback,
	}

	// Input args with software encoder (libx264) + no hardware params
	args := []string{"-i", "input.mp4", "-c:v", "libx264", "-preset", "fast", "output.mp4"}

	prunedArgs, _ := re.pruneHardwareParams(args)

	// Verify encoder was NOT switched (already software)
	encoder := extractEncoderFromArgs(prunedArgs)
	if encoder != "libx264" {
		t.Errorf("Expected encoder libx264 to remain, got %s", encoder)
	}
}
