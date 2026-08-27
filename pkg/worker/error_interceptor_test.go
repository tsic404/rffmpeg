package worker

import (
	"context"
	"strings"
	"testing"
)

func TestFFmpegErrorType_String(t *testing.T) {
	tests := []struct {
		name     string
		e        FFmpegErrorType
		expected string
	}{
		{"unknown", ErrorTypeUnknown, "unknown"},
		{"invalid_argument", ErrorTypeInvalidArgument, "invalid_argument"},
		{"encoder_not_found", ErrorTypeEncoderNotFound, "encoder_not_found"},
		{"device_not_found", ErrorTypeDeviceNotFound, "device_not_found"},
		{"unsupported_codec", ErrorTypeUnsupportedCodec, "unsupported_codec"},
		{"memory_allocation", ErrorTypeMemoryAllocation, "memory_allocation"},
		{"hwaccel_failed", ErrorTypeHWAccelFailed, "hwaccel_failed"},
		{"input_output", ErrorTypeInputOutput, "input_output"},
		{"permission_denied", ErrorTypePermissionDenied, "permission_denied"},
		{"output_empty", ErrorTypeOutputEmpty, "output_empty"},
		{"process_crash", ErrorTypeProcessCrash, "process_crash"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.e.String(); got != tt.expected {
				t.Errorf("FFmpegErrorType.String() = %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestFFmpegErrorType_Description(t *testing.T) {
	tests := []struct {
		name string
		e    FFmpegErrorType
	}{
		{name: "unknown", e: ErrorTypeUnknown},
		{name: "invalid_argument", e: ErrorTypeInvalidArgument},
		{name: "encoder_not_found", e: ErrorTypeEncoderNotFound},
		{name: "device_not_found", e: ErrorTypeDeviceNotFound},
		{name: "unsupported_codec", e: ErrorTypeUnsupportedCodec},
		{name: "memory_allocation", e: ErrorTypeMemoryAllocation},
		{name: "hwaccel_failed", e: ErrorTypeHWAccelFailed},
		{name: "input_output", e: ErrorTypeInputOutput},
		{name: "output_empty", e: ErrorTypeOutputEmpty},
		{name: "permission_denied", e: ErrorTypePermissionDenied},
		{name: "process_crash", e: ErrorTypeProcessCrash},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.e.Description(); got == "" {
				t.Error("FFmpegErrorType.Description() returned empty string")
			}
		})
	}
}

func TestFFmpegError_Error(t *testing.T) {
	err := &FFmpegError{
		Type:     ErrorTypeInvalidArgument,
		Message:  "test error message",
		ExitCode: 1,
	}

	got := err.Error()
	if !strings.Contains(got, "invalid_argument") {
		t.Errorf("Error() should contain error type, got %v", got)
	}
	if !strings.Contains(got, "test error message") {
		t.Errorf("Error() should contain message, got %v", got)
	}
}

func TestFFmpegError_IsRetryable(t *testing.T) {
	tests := []struct {
		name     string
		e        FFmpegErrorType
		expected bool
	}{
		{"invalid_argument", ErrorTypeInvalidArgument, true},
		{"encoder_not_found", ErrorTypeEncoderNotFound, true},
		{"device_not_found", ErrorTypeDeviceNotFound, true},
		{"unsupported_codec", ErrorTypeUnsupportedCodec, true},
		{"hwaccel_failed", ErrorTypeHWAccelFailed, true},
		{"output_empty", ErrorTypeOutputEmpty, true},
		{"output_open", ErrorTypeOutputOpen, false},
		{"memory_allocation", ErrorTypeMemoryAllocation, false},
		{"input_output", ErrorTypeInputOutput, false},
		{"process_crash", ErrorTypeProcessCrash, true},
		{"permission_denied", ErrorTypePermissionDenied, false},
		{"unknown", ErrorTypeUnknown, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := &FFmpegError{Type: tt.e}
			if got := err.IsRetryable(); got != tt.expected {
				t.Errorf("IsRetryable() = %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestErrorAnalyzer_Analyze(t *testing.T) {
	tests := []struct {
		name         string
		stderr       string
		exitCode     int
		expectedType FFmpegErrorType
		shouldBeNil  bool
	}{
		{
			name:        "empty_success",
			stderr:      "",
			exitCode:    0,
			shouldBeNil: true,
		},
		{
			name:         "invalid_argument",
			stderr:       "[error] Invalid argument: -profile\n",
			exitCode:     1,
			expectedType: ErrorTypeInvalidArgument,
		},
		{
			name:         "encoder_not_found",
			stderr:       "[error] Unknown encoder 'h264_nvenc'\n",
			exitCode:     1,
			expectedType: ErrorTypeEncoderNotFound,
		},
		{
			name:         "device_not_found",
			stderr:       "[error] Device not found: /dev/dri/renderD128\n",
			exitCode:     1,
			expectedType: ErrorTypeDeviceNotFound,
		},
		{
			name:         "unsupported_codec",
			stderr:       "[error] Unsupported codec: hevc\n",
			exitCode:     1,
			expectedType: ErrorTypeUnsupportedCodec,
		},
		{
			name:         "memory_allocation",
			stderr:       "[error] Cannot allocate memory\n",
			exitCode:     1,
			expectedType: ErrorTypeMemoryAllocation,
		},
		{
			name:         "hwaccel_failed",
			stderr:       "[error] NVENC error: hardware encoder initialization failed\n",
			exitCode:     1,
			expectedType: ErrorTypeHWAccelFailed,
		},
		{
			name:         "io_error",
			stderr:       "[error] Input/output error\n",
			exitCode:     1,
			expectedType: ErrorTypeInputOutput,
		},
		{
			name:         "permission_denied",
			stderr:       "[error] Permission denied\n",
			exitCode:     1,
			expectedType: ErrorTypePermissionDenied,
		},
		{
			name:         "unknown_error",
			stderr:       "[error] Some unknown error\n",
			exitCode:     1,
			expectedType: ErrorTypeUnknown,
		},
		{
			name:         "case_insensitive",
			stderr:       "[ERROR] INVALID ARGUMENT\n",
			exitCode:     1,
			expectedType: ErrorTypeInvalidArgument,
		},
		{
			name:         "output_empty",
			stderr:       "Output file is empty (0 bytes): /tmp/output.mp4\n",
			exitCode:     0,
			expectedType: ErrorTypeOutputEmpty,
		},
		{
			name:         "sigabrt_exit_134",
			stderr:       "frame=  120 fps= 30 q=28.0\nAborted\n",
			exitCode:     134,
			expectedType: ErrorTypeProcessCrash,
		},
		{
			name:         "sigsegv_exit_139",
			stderr:       "frame=  120 fps= 30 q=28.0\n",
			exitCode:     139,
			expectedType: ErrorTypeProcessCrash,
		},
		{
			name:         "sigabrt_no_stderr",
			stderr:       "",
			exitCode:     134,
			expectedType: ErrorTypeProcessCrash,
		},
		{
			name:         "sigfpe_exit_136",
			stderr:       "",
			exitCode:     136,
			expectedType: ErrorTypeProcessCrash,
		},
		{
			name:         "output_open_failure_exit0",
			stderr:       "[out#0/mp3 @ 0x...] Error opening output file /nonexistent_dir/out.mp3.\nError opening output files.\n",
			exitCode:     0,
			expectedType: ErrorTypeOutputOpen,
		},
		{
			name:         "muxer_init_failure_exit0",
			stderr:       "[AVFormatContext @ 0x...] Unable to choose an output format for 'output.xyz'; use a standard extension for the filename or specify the format manually.\n[out#0 @ 0x...] Error initializing the muxer for output.xyz: Invalid argument\nError opening output file output.xyz.\n",
			exitCode:     0,
			expectedType: ErrorTypeOutputOpen,
		},
	}

	analyzer := NewErrorAnalyzer()

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := analyzer.Analyze(tt.stderr, tt.exitCode)

			if tt.shouldBeNil {
				if got != nil {
					t.Errorf("Analyze() should return nil, got %v", got)
				}
				return
			}

			if got == nil {
				t.Fatal("Analyze() returned nil, expected non-nil")
			}

			if got.Type != tt.expectedType {
				t.Errorf("Analyze() type = %v, want %v", got.Type, tt.expectedType)
			}

			if got.ExitCode != tt.exitCode {
				t.Errorf("Analyze() exitCode = %v, want %v", got.ExitCode, tt.exitCode)
			}

			if got.Message == "" {
				t.Error("Analyze() returned empty message")
			}
		})
	}
}

func TestErrorAnalyzer_AnalyzeWithArgs(t *testing.T) {
	analyzer := NewErrorAnalyzer()

	tests := []struct {
		name     string
		stderr   string
		exitCode int
		args     []string
	}{
		{
			name:     "encoder_not_found_with_args",
			stderr:   "[error] Unknown encoder 'h264_nvenc'\n",
			exitCode: 1,
			args:     []string{"-c:v", "h264_nvenc", "-i", "input.mp4", "output.mp4"},
		},
		{
			name:     "device_not_found_with_device",
			stderr:   "[error] Device not found: /dev/dri/renderD128\n",
			exitCode: 1,
			args:     []string{"-hwaccel_device", "/dev/dri/renderD128", "-i", "input.mp4", "output.mp4"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := analyzer.AnalyzeWithArgs(tt.stderr, tt.exitCode, tt.args)
			if got == nil {
				t.Fatal("AnalyzeWithArgs() returned nil")
			}
			if got.Message == "" {
				t.Error("AnalyzeWithArgs() returned empty message")
			}
		})
	}
}

func TestErrorInterceptor_Intercept(t *testing.T) {
	interceptor := NewErrorInterceptor()

	tests := []struct {
		name              string
		result            ExecResult
		expectSuccess     bool
		expectFFmpegError bool
	}{
		{
			name: "success",
			result: ExecResult{
				ExitCode: 0,
				Stdout:   "",
				Stderr:   "",
				Error:    nil,
			},
			expectSuccess:     true,
			expectFFmpegError: false,
		},
		{
			name: "invalid_argument_error",
			result: ExecResult{
				ExitCode: 1,
				Stdout:   "",
				Stderr:   "[error] Invalid argument\n",
				Error:    nil,
			},
			expectSuccess:     false,
			expectFFmpegError: true,
		},
		{
			name: "encoder_not_found_error",
			result: ExecResult{
				ExitCode: 1,
				Stdout:   "",
				Stderr:   "[error] Unknown encoder 'h264_nvenc'\n",
				Error:    nil,
			},
			expectSuccess:     false,
			expectFFmpegError: true,
		},
		{
			name: "unknown_encoder_with_exit_code_zero",
			result: ExecResult{
				ExitCode: 0,
				Stdout:   "",
				Stderr:   "Unknown encoder 'nonexistent_codec'\n",
				Error:    nil,
			},
			expectSuccess:     false,
			expectFFmpegError: true,
		},
		{
			name: "output_empty_with_exit_code_zero",
			result: ExecResult{
				ExitCode: 0,
				Stdout:   "",
				Stderr:   "Output file is empty (0 bytes): /tmp/output.mp4\n",
				Error:    nil,
			},
			expectSuccess:     false,
			expectFFmpegError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := interceptor.Intercept(context.Background(), tt.result)

			if got.IsSuccess != tt.expectSuccess {
				t.Errorf("Intercept() IsSuccess = %v, want %v", got.IsSuccess, tt.expectSuccess)
			}

			hasError := got.FFmpegError != nil
			if hasError != tt.expectFFmpegError {
				t.Errorf("Intercept() has FFmpegError = %v, want %v", hasError, tt.expectFFmpegError)
			}
		})
	}
}

func TestErrorInterceptor_InterceptAndAnalyze(t *testing.T) {
	interceptor := NewErrorInterceptor()

	// Test successful execution
	result := ExecResult{
		ExitCode: 0,
		Stdout:   "",
		Stderr:   "",
		Error:    nil,
	}

	intercepted, err := interceptor.InterceptAndAnalyze(context.Background(), result)
	if err != nil {
		t.Errorf("InterceptAndAnalyze() should not return error for success, got %v", err)
	}
	if !intercepted.IsSuccess {
		t.Error("InterceptAndAnalyze() should return IsSuccess=true")
	}

	// Test failed execution
	failedResult := ExecResult{
		ExitCode: 1,
		Stdout:   "",
		Stderr:   "[error] Invalid argument\n",
		Error:    nil,
	}

	intercepted, err = interceptor.InterceptAndAnalyze(context.Background(), failedResult)
	if err == nil {
		t.Error("InterceptAndAnalyze() should return error for failed execution")
	}
	if intercepted.IsSuccess {
		t.Error("InterceptAndAnalyze() should return IsSuccess=false for failed execution")
	}
}

func TestParamPruner_GetPruneRecommendations(t *testing.T) {
	pruner := NewParamPruner()

	tests := []struct {
		name         string
		errorType    FFmpegErrorType
		stderr       string
		expectParams []string
	}{
		{
			name:         "hwaccel_error",
			errorType:    ErrorTypeHWAccelFailed,
			stderr:       "[error] hwaccel error\n",
			expectParams: []string{"-hwaccel", "-hwaccel_device", "-hwaccel_output_format"},
		},
		{
			name:         "device_not_found",
			errorType:    ErrorTypeDeviceNotFound,
			stderr:       "[error] Device not found\n",
			expectParams: []string{"-hwaccel_device", "-vaapi_device", "-qsv_device", "-gpu"},
		},
		{
			name:         "memory_error",
			errorType:    ErrorTypeMemoryAllocation,
			stderr:       "[error] Cannot allocate memory\n",
			expectParams: []string{}, // Memory errors have no prune recommendations
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := pruner.GetPruneRecommendations(tt.errorType, tt.stderr)

			// Check that expected params are in the result
			for _, expected := range tt.expectParams {
				found := false
				for _, param := range got {
					if param == expected {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("GetPruneRecommendations() missing expected param %v, got %v", expected, got)
				}
			}
		})
	}
}

func TestParamPruner_PruneParams(t *testing.T) {
	pruner := NewParamPruner()

	tests := []struct {
		name     string
		args     []string
		toRemove []string
		expected []string
	}{
		{
			name:     "no_removal",
			args:     []string{"-i", "input.mp4", "-c:v", "h264", "output.mp4"},
			toRemove: []string{},
			expected: []string{"-i", "input.mp4", "-c:v", "h264", "output.mp4"},
		},
		{
			name:     "remove_hwaccel",
			args:     []string{"-hwaccel", "cuda", "-i", "input.mp4", "output.mp4"},
			toRemove: []string{"-hwaccel"},
			expected: []string{"-i", "input.mp4", "output.mp4"},
		},
		{
			name:     "remove_multiple",
			args:     []string{"-hwaccel", "cuda", "-hwaccel_device", "0", "-i", "input.mp4", "output.mp4"},
			toRemove: []string{"-hwaccel", "-hwaccel_device"},
			expected: []string{"-i", "input.mp4", "output.mp4"},
		},
		{
			name:     "remove_profile_with_colon",
			args:     []string{"-profile:v", "high", "-i", "input.mp4", "output.mp4"},
			toRemove: []string{"-profile:v"},
			expected: []string{"-i", "input.mp4", "output.mp4"},
		},
		{
			name:     "remove_with_equals",
			args:     []string{"-profile:v=high", "-i", "input.mp4", "output.mp4"},
			toRemove: []string{"-profile:v"},
			expected: []string{"-i", "input.mp4", "output.mp4"},
		},
		{
			name:     "preserve_other_params",
			args:     []string{"-i", "input.mp4", "-c:v", "h264", "-profile:v", "high", "-tune", "film", "output.mp4"},
			toRemove: []string{"-profile:v", "-tune"},
			expected: []string{"-i", "input.mp4", "-c:v", "h264", "output.mp4"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := pruner.PruneParams(tt.args, tt.toRemove)

			if len(got) != len(tt.expected) {
				t.Errorf("PruneParams() length = %v, want %v\ngot: %v\nexpected: %v", len(got), len(tt.expected), got, tt.expected)
				return
			}

			for i, v := range got {
				if v != tt.expected[i] {
					t.Errorf("PruneParams()[%d] = %v, want %v", i, v, tt.expected[i])
				}
			}
		})
	}
}

func TestParamPruner_PruneParamsFromError(t *testing.T) {
	pruner := NewParamPruner()

	args := []string{"-hwaccel", "cuda", "-hwaccel_device", "0", "-i", "input.mp4", "-c:v", "h264_nvenc", "output.mp4"}
	ffmpegErr := &FFmpegError{
		Type:     ErrorTypeDeviceNotFound,
		Stderr:   "[error] Device not found: /dev/dri/renderD128\n",
		ExitCode: 1,
	}

	got := pruner.PruneParamsFromError(args, ffmpegErr)

	// Device not found should remove device params
	for _, param := range []string{"-hwaccel_device", "-vaapi_device"} {
		for _, arg := range got {
			if arg == param {
				t.Errorf("PruneParamsFromError() should have removed %v", param)
			}
		}
	}
}

func TestParamPruner_PruneHardwareParams(t *testing.T) {
	pruner := NewParamPruner()

	args := []string{
		"-hwaccel", "cuda",
		"-hwaccel_device", "0",
		"-i", "input.mp4",
		"-c:v", "h264_nvenc",
		"-gpu", "0",
		"-preset", "fast",
		"output.mp4",
	}

	got := pruner.PruneHardwareParams(args)

	// Hardware params should be removed
	for _, param := range []string{"-hwaccel", "-hwaccel_device", "-gpu"} {
		for _, arg := range got {
			if arg == param {
				t.Errorf("PruneHardwareParams() should have removed %v", param)
			}
		}
	}

	// Non-hardware params should be preserved
	found := false
	for _, arg := range got {
		if arg == "-preset" {
			found = true
			break
		}
	}
	if !found {
		t.Error("PruneHardwareParams() should preserve -preset")
	}
}

func TestParamPruner_PruneProfileLevelParams(t *testing.T) {
	pruner := NewParamPruner()

	args := []string{
		"-i", "input.mp4",
		"-c:v", "h264",
		"-profile:v", "high",
		"-level:v", "4.0",
		"-preset", "fast",
		"output.mp4",
	}

	got := pruner.PruneProfileLevelParams(args)

	// Profile and level should be removed
	for _, param := range []string{"-profile:v", "-level:v"} {
		for _, arg := range got {
			if arg == param {
				t.Errorf("PruneProfileLevelParams() should have removed %v", param)
			}
		}
	}

	// Other params should be preserved
	for _, expected := range []string{"-i", "-c:v", "-preset", "output.mp4"} {
		found := false
		for _, arg := range got {
			if arg == expected {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("PruneProfileLevelParams() should preserve %v", expected)
		}
	}
}

func TestDefaultErrorPatterns(t *testing.T) {
	patterns := DefaultErrorPatterns()

	if len(patterns) == 0 {
		t.Fatal("DefaultErrorPatterns() returned empty list")
	}

	// Check that all error types have patterns
	seenTypes := make(map[FFmpegErrorType]bool)
	for _, p := range patterns {
		seenTypes[p.Type] = true
		if len(p.Patterns) == 0 {
			t.Errorf("ErrorPattern for %v has no patterns", p.Type)
		}
	}

	// Verify important error types are covered
	requiredTypes := []FFmpegErrorType{
		ErrorTypeInvalidArgument,
		ErrorTypeEncoderNotFound,
		ErrorTypeDeviceNotFound,
		ErrorTypeUnsupportedCodec,
		ErrorTypeMemoryAllocation,
		ErrorTypeHWAccelFailed,
		ErrorTypeProcessCrash,
	}

	for _, req := range requiredTypes {
		if !seenTypes[req] {
			t.Errorf("DefaultErrorPatterns() missing error type %v", req)
		}
	}
}

func TestDefaultPruneRules(t *testing.T) {
	rules := DefaultPruneRules()

	if len(rules) == 0 {
		t.Fatal("DefaultPruneRules() returned empty list")
	}

	// Check that rules are defined for common error types
	seenTypes := make(map[FFmpegErrorType]bool)
	for _, r := range rules {
		seenTypes[r.ErrorType] = true
	}

	for _, req := range []FFmpegErrorType{
		ErrorTypeInvalidArgument,
		ErrorTypeEncoderNotFound,
		ErrorTypeDeviceNotFound,
		ErrorTypeHWAccelFailed,
	} {
		if !seenTypes[req] {
			t.Errorf("DefaultPruneRules() missing rule for error type %v", req)
		}
	}
}

func TestInterceptedResult_APICompatibility(t *testing.T) {
	// Test that InterceptedResult preserves ExecResult fields
	result := ExecResult{
		ExitCode: 1,
		Stdout:   "test stdout",
		Stderr:   "[error] Invalid argument\n",
		Error:    nil,
	}

	intercepted := &InterceptedResult{
		ExecResult: result,
		IsSuccess:  false,
	}

	// Verify that ExecResult fields are accessible
	if intercepted.ExitCode != result.ExitCode {
		t.Error("InterceptedResult should preserve ExitCode")
	}
	if intercepted.Stdout != result.Stdout {
		t.Error("InterceptedResult should preserve Stdout")
	}
	if intercepted.Stderr != result.Stderr {
		t.Error("InterceptedResult should preserve Stderr")
	}
}

func TestClassifyErrorType(t *testing.T) {
	tests := []struct {
		name       string
		stderr     string
		expectType FFmpegErrorType
	}{
		{
			name:       "invalid_argument",
			stderr:     "[error] Invalid argument\n",
			expectType: ErrorTypeInvalidArgument,
		},
		{
			name:       "encoder_not_found",
			stderr:     "[error] Unknown encoder\n",
			expectType: ErrorTypeEncoderNotFound,
		},
		{
			name:       "unknown",
			stderr:     "[error] Something else\n",
			expectType: ErrorTypeUnknown,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ClassifyErrorType(tt.stderr)
			if got != tt.expectType {
				t.Errorf("ClassifyErrorType() = %v, want %v", got, tt.expectType)
			}
		})
	}
}

func TestIsHardwareEncoderByName(t *testing.T) {
	tests := []struct {
		name     string
		encoder  string
		expected bool
	}{
		{"nvenc", "h264_nvenc", true},
		{"qsv", "h264_qsv", true},
		{"vaapi", "h264_vaapi", true},
		{"amf", "hevc_amf", true},
		{"videotoolbox", "h264_videotoolbox", true},
		{"cuvid", "h264_cuvid", true},
		{"vdpau", "h264_vdpau", true},
		{"nvdec", "h264_nvdec", true},
		{"software_x264", "libx264", false},
		{"software_x265", "libx265", false},
		{"software_vp9", "libvpx-vp9", false},
		{"software_av1", "libaom-av1", false},
		{"default", "copy", false},
		{"empty", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isHardwareEncoderByName(tt.encoder); got != tt.expected {
				t.Errorf("isHardwareEncoderByName(%q) = %v, want %v", tt.encoder, got, tt.expected)
			}
		})
	}
}

func TestDetectSoftwareEncoderInStderr(t *testing.T) {
	tests := []struct {
		name     string
		stderr   string
		expected string
	}{
		{
			name:     "libx264_found",
			stderr:   "  Stream #0:0: Video: h264 (libx264), yuv420p\n[libx264 @ 0x55a1b0] using cpu capabilities: MMX2 SSE2Fast\n",
			expected: "libx264",
		},
		{
			name:     "libx265_found",
			stderr:   "[libx265 @ 0x7f8c00] using cpu capabilities: MMX2 SSE2Fast\n",
			expected: "libx265",
		},
		{
			name:     "libvpx-vp9_found",
			stderr:   "[libvpx-vp9 @ 0xabc] v1.10.0\n",
			expected: "libvpx-vp9",
		},
		{
			name:     "libaom-av1_found",
			stderr:   "[libaom-av1 @ 0xdef] 3.6.1\n",
			expected: "libaom-av1",
		},
		{
			name:     "libsvtav1_found",
			stderr:   "  Stream #0:0: Video: av1 (libsvtav1)\n[libsvtav1 @ 0x123] \n",
			expected: "libsvtav1",
		},
		{
			name:     "mpeg2video_found",
			stderr:   "[mpeg2video @ 0x456] \n",
			expected: "mpeg2video",
		},
		{
			name:     "mjpeg_found",
			stderr:   "[mjpeg @ 0x789] \n",
			expected: "mjpeg",
		},
		{
			name:     "libvpx_found",
			stderr:   "[libvpx @ 0xabc] v1.13.0\n",
			expected: "libvpx",
		},
		{
			name:     "no_software_encoder",
			stderr:   "[h264_qsv @ 0xabc] using QSV\n",
			expected: "",
		},
		{
			name:     "empty_stderr",
			stderr:   "",
			expected: "",
		},
		{
			name:     "only_hardware_encoder_in_stderr",
			stderr:   "[h264_nvenc @ 0xabc] using NVENC\n[h264_vaapi @ 0xdef] using VAAPI\n",
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := detectSoftwareEncoderInStderr(tt.stderr); got != tt.expected {
				t.Errorf("detectSoftwareEncoderInStderr() = %q, want %q", got, tt.expected)
			}
		})
	}
}

func TestErrorInterceptor_InterceptWithEncoder_SilentFallback(t *testing.T) {
	interceptor := NewErrorInterceptor()

	tests := []struct {
		name              string
		result            ExecResult
		requestedEncoder  string
		expectSuccess     bool
		expectFFmpegError bool
	}{
		{
			name: "hardware_qsv_silently_fell_back_to_libx264",
			result: ExecResult{
				ExitCode: 0,
				Stdout:   "",
				Stderr:   "  Stream #0:0: Video: h264 (libx264)\n[libx264 @ 0xabc] using cpu capabilities: MMX2\nframe=  100 fps=30 q=-1.0 Lsize=  500kB time=00:00:03.33\n",
				Error:    nil,
			},
			requestedEncoder:  "h264_qsv",
			expectSuccess:     false,
			expectFFmpegError: true,
		},
		{
			name: "hardware_vaapi_silently_fell_back_to_libx264",
			result: ExecResult{
				ExitCode: 0,
				Stdout:   "",
				Stderr:   "[libx264 @ 0xabc] using cpu capabilities: MMX2\n",
				Error:    nil,
			},
			requestedEncoder:  "h264_vaapi",
			expectSuccess:     false,
			expectFFmpegError: true,
		},
		{
			name: "hardware_nvenc_silently_fell_back_to_libx265",
			result: ExecResult{
				ExitCode: 0,
				Stdout:   "",
				Stderr:   "[libx265 @ 0xabc] using cpu capabilities\n",
				Error:    nil,
			},
			requestedEncoder:  "hevc_nvenc",
			expectSuccess:     false,
			expectFFmpegError: true,
		},
		{
			name: "hardware_encoder_succeeded_correctly",
			result: ExecResult{
				ExitCode: 0,
				Stdout:   "",
				Stderr:   "[h264_qsv @ 0xabc] using QSV\nframe=  100 fps=30\n",
				Error:    nil,
			},
			requestedEncoder:  "h264_qsv",
			expectSuccess:     true,
			expectFFmpegError: false,
		},
		{
			name: "software_encoder_no_fallback_detection_needed",
			result: ExecResult{
				ExitCode: 0,
				Stdout:   "",
				Stderr:   "[libx264 @ 0xabc] using cpu capabilities\n",
				Error:    nil,
			},
			requestedEncoder:  "libx264",
			expectSuccess:     true,
			expectFFmpegError: false,
		},
		{
			name: "empty_requested_encoder_no_fallback_check",
			result: ExecResult{
				ExitCode: 0,
				Stdout:   "",
				Stderr:   "[libx264 @ 0xabc] using cpu capabilities\n",
				Error:    nil,
			},
			requestedEncoder:  "",
			expectSuccess:     true,
			expectFFmpegError: false,
		},
		{
			name: "exit_code_nonzero_takes_priority",
			result: ExecResult{
				ExitCode: 1,
				Stdout:   "",
				Stderr:   "[QSV error] Failed to initialize\n",
				Error:    nil,
			},
			requestedEncoder:  "h264_qsv",
			expectSuccess:     false,
			expectFFmpegError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := interceptor.InterceptWithEncoder(context.Background(), tt.result, tt.requestedEncoder)

			if got.IsSuccess != tt.expectSuccess {
				t.Errorf("InterceptWithEncoder() IsSuccess = %v, want %v", got.IsSuccess, tt.expectSuccess)
			}

			hasError := got.FFmpegError != nil
			if hasError != tt.expectFFmpegError {
				t.Errorf("InterceptWithEncoder() has FFmpegError = %v, want %v", hasError, tt.expectFFmpegError)
			}

			if got.FFmpegError != nil && !tt.expectSuccess {
				if got.FFmpegError.Type != ErrorTypeHWAccelFailed && tt.result.ExitCode == 0 && tt.requestedEncoder != "" {
					// For silent fallback cases, the type should be HWAccelFailed
					if isHardwareEncoderByName(tt.requestedEncoder) {
						t.Errorf("InterceptWithEncoder() FFmpegError.Type = %v, want %v for silent fallback",
							got.FFmpegError.Type, ErrorTypeHWAccelFailed)
					}
				}
			}
		})
	}
}
