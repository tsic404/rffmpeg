package worker

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// FFmpegErrorType represents the type of ffmpeg error.
type FFmpegErrorType int

const (
	// ErrorTypeUnknown indicates an unknown error type.
	ErrorTypeUnknown FFmpegErrorType = iota

	// ErrorTypeInvalidArgument indicates an invalid argument error.
	// Common message: "Invalid argument", "Option not found"
	ErrorTypeInvalidArgument

	// ErrorTypeEncoderNotFound indicates the encoder does not exist.
	// Common message: "No such encoder", "Unknown encoder"
	ErrorTypeEncoderNotFound

	// ErrorTypeDeviceNotFound indicates a device was not found.
	// Common message: "Device not found", "Cannot open device"
	ErrorTypeDeviceNotFound

	// ErrorTypeUnsupportedCodec indicates an unsupported codec.
	// Common message: "Unsupported codec", "Codec not supported"
	ErrorTypeUnsupportedCodec

	// ErrorTypeMemoryAllocation indicates a memory allocation failure.
	// Common message: "Cannot allocate memory", "Out of memory"
	ErrorTypeMemoryAllocation

	// ErrorTypeHWAccelFailed indicates hardware acceleration failure.
	// Common message: "Hardware acceleration error", "hwaccel"
	ErrorTypeHWAccelFailed

	// ErrorTypeInputOutput indicates an I/O error.
	// Common message: "Input/output error", "No such file"
	ErrorTypeInputOutput

	// ErrorTypePermissionDenied indicates a permission error.
	// Common message: "Permission denied", "Access denied"
	ErrorTypePermissionDenied

	// ErrorTypeOutputEmpty indicates the output file is empty (0 bytes).
	// FFmpeg exited with code 0 but produced no valid output data.
	ErrorTypeOutputEmpty
)

// String returns the string representation of the error type.
func (e FFmpegErrorType) String() string {
	switch e {
	case ErrorTypeInvalidArgument:
		return "invalid_argument"
	case ErrorTypeEncoderNotFound:
		return "encoder_not_found"
	case ErrorTypeDeviceNotFound:
		return "device_not_found"
	case ErrorTypeUnsupportedCodec:
		return "unsupported_codec"
	case ErrorTypeMemoryAllocation:
		return "memory_allocation"
	case ErrorTypeHWAccelFailed:
		return "hwaccel_failed"
	case ErrorTypeInputOutput:
		return "input_output"
	case ErrorTypePermissionDenied:
		return "permission_denied"
	case ErrorTypeOutputEmpty:
		return "output_empty"
	default:
		return "unknown"
	}
}

// Description returns a human-readable description of the error type.
func (e FFmpegErrorType) Description() string {
	switch e {
	case ErrorTypeInvalidArgument:
		return "Invalid argument or option not found"
	case ErrorTypeEncoderNotFound:
		return "Encoder does not exist or is not available"
	case ErrorTypeDeviceNotFound:
		return "Device not found or inaccessible"
	case ErrorTypeUnsupportedCodec:
		return "Codec not supported"
	case ErrorTypeMemoryAllocation:
		return "Memory allocation failed"
	case ErrorTypeHWAccelFailed:
		return "Hardware acceleration failed"
	case ErrorTypeInputOutput:
		return "Input/output error"
	case ErrorTypePermissionDenied:
		return "Permission denied"
	case ErrorTypeOutputEmpty:
		return "Output file is empty"
	default:
		return "Unknown error"
	}
}

// ErrorPattern represents a pattern for matching ffmpeg errors.
type ErrorPattern struct {
	// Type is the error type this pattern matches.
	Type FFmpegErrorType

	// Patterns are the string patterns to match in stderr.
	Patterns []string

	// Description describes the error pattern.
	Description string
}

// DefaultErrorPatterns returns the default error patterns for ffmpeg.
func DefaultErrorPatterns() []ErrorPattern {
	return []ErrorPattern{
		{
			Type: ErrorTypeInvalidArgument,
			Patterns: []string{
				"Invalid argument",
				"Option not found",
				"Invalid option",
				"Unrecognized option",
				"error parsing option",
				"Invalid value",
				"Expected a value for option",
			},
			Description: "Invalid argument or option error",
		},
		{
			Type: ErrorTypeEncoderNotFound,
			Patterns: []string{
				"No such encoder",
				"Unknown encoder",
				"Encoder not found",
				"not found in encoder list",
				"Requested encoder",
				"Selected encoder not available",
			},
			Description: "Encoder not found error",
		},
		{
			Type: ErrorTypeDeviceNotFound,
			Patterns: []string{
				"Device not found",
				"Cannot open device",
				"device not found",
				"Failed to open device",
				"DRM device",
				"VA device",
				"CUDA device",
				"cannot open device",
			},
			Description: "Device not found error",
		},
		{
			Type: ErrorTypeUnsupportedCodec,
			Patterns: []string{
				"Unsupported codec",
				"Codec not supported",
				"Unsupported feature",
				"codec not supported",
				"not supported by this encoder",
				"does not support",
			},
			Description: "Unsupported codec error",
		},
		{
			Type: ErrorTypeMemoryAllocation,
			Patterns: []string{
				"Cannot allocate memory",
				"Out of memory",
				"memory allocation failed",
				"failed to allocate",
				"Allocation failed",
			},
			Description: "Memory allocation error",
		},
		{
			Type: ErrorTypeHWAccelFailed,
			Patterns: []string{
				"Hardware acceleration error",
				"hwaccel",
				"hardware device",
				"GPU",
				"CUDA error",
				"NVENC error",
				"VAAPI error",
				"QSV error",
				"[QSV",
				"Error creating a MFX session",
				"Device creation failed",
				"qsv_device",
				"vaapi_device",
				"AMF error",
				"VideoToolbox error",
				"failed to init hwdevice",
				"failed to init hwaccel",
				"MFX session",
				"Impossible to convert between the formats",
				"Failed to process frame",
				"Encode failed",
			},
			Description: "Hardware acceleration error",
		},
		{
			Type: ErrorTypeInputOutput,
			Patterns: []string{
				"Input/output error",
				"No such file",
				"No such device",
				"I/O error",
				"read error",
				"write error",
				"end of file",
			},
			Description: "Input/output error",
		},
		{
			Type: ErrorTypePermissionDenied,
			Patterns: []string{
				"Permission denied",
				"Access denied",
				"Access is denied",
				"permission denied",
			},
			Description: "Permission denied error",
		},
		{
			Type: ErrorTypeOutputEmpty,
			Patterns: []string{
				"Output file is empty",
				"output file is empty",
				"output file not found",
				"Output file not found",
				"Empty output",
			},
			Description: "Output file is empty or not found",
		},
	}
}

// FFmpegError represents a parsed ffmpeg error.
type FFmpegError struct {
	// Type is the classified error type.
	Type FFmpegErrorType `json:"type"`

	// Message is the original error message from stderr.
	Message string `json:"message"`

	// ExitCode is the ffmpeg exit code.
	ExitCode int `json:"exit_code"`

	// Stderr is the full stderr output.
	Stderr string `json:"stderr,omitempty"`

	// MatchedPatterns contains the patterns that matched.
	MatchedPatterns []string `json:"matched_patterns,omitempty"`

	// Timestamp is when the error occurred.
	Timestamp time.Time `json:"timestamp"`

	// ContextLines are relevant lines from stderr around the error.
	ContextLines []string `json:"context_lines,omitempty"`
}

// Error implements the error interface.
func (e *FFmpegError) Error() string {
	return fmt.Sprintf("ffmpeg error (type=%s, exit_code=%d): %s", e.Type, e.ExitCode, e.Message)
}

// IsRetryable returns true if the error might be resolved by retrying with different parameters.
func (e *FFmpegError) IsRetryable() bool {
	switch e.Type {
	case ErrorTypeInvalidArgument, ErrorTypeEncoderNotFound, ErrorTypeDeviceNotFound,
		ErrorTypeUnsupportedCodec, ErrorTypeHWAccelFailed, ErrorTypeOutputEmpty:
		return true
	case ErrorTypeMemoryAllocation, ErrorTypeInputOutput, ErrorTypePermissionDenied:
		return false
	default:
		return false
	}
}

// InterceptedResult wraps ExecResult with error analysis.
type InterceptedResult struct {
	// ExecResult is the original execution result.
	ExecResult

	// FFmpegError is the parsed ffmpeg error, if any.
	FFmpegError *FFmpegError `json:"ffmpeg_error,omitempty"`

	// IsSuccess indicates whether the command succeeded.
	IsSuccess bool `json:"is_success"`

	// PruneRecommendations contains parameter pruning recommendations.
	PruneRecommendations []string `json:"prune_recommendations,omitempty"`
}

// ErrorInterceptor intercepts and analyzes ffmpeg execution results.
type ErrorInterceptor struct {
	// patterns are the error patterns to match.
	patterns []ErrorPattern

	// analyzer is the error analyzer.
	analyzer *ErrorAnalyzer

	// pruner is the parameter pruner.
	pruner *ParamPruner
}

// NewErrorInterceptor creates a new error interceptor.
func NewErrorInterceptor() *ErrorInterceptor {
	return &ErrorInterceptor{
		patterns: DefaultErrorPatterns(),
		analyzer: NewErrorAnalyzer(),
		pruner:   NewParamPruner(),
	}
}

// NewErrorInterceptorWithPatterns creates a new error interceptor with custom patterns.
func NewErrorInterceptorWithPatterns(patterns []ErrorPattern) *ErrorInterceptor {
	return &ErrorInterceptor{
		patterns: patterns,
		analyzer: NewErrorAnalyzerWithPatterns(patterns),
		pruner:   NewParamPruner(),
	}
}

// Intercept intercepts an execution result and analyzes errors.
func (i *ErrorInterceptor) Intercept(ctx context.Context, result ExecResult) *InterceptedResult {
	return i.InterceptWithEncoder(ctx, result, "")
}

// InterceptWithEncoder intercepts an execution result, analyzes errors, and detects
// silent hardware→software encoder fallback by comparing the requested encoder with
// what stderr reveals about the encoder that actually ran.
func (i *ErrorInterceptor) InterceptWithEncoder(ctx context.Context, result ExecResult, requestedEncoder string) *InterceptedResult {
	intercepted := &InterceptedResult{
		ExecResult: result,
		IsSuccess:  result.ExitCode == 0 && result.Error == nil,
	}

	// Always analyze stderr for error patterns, even when exit code is 0.
	// FFmpeg sometimes exits 0 even for critical failures (e.g., unknown codec
	// falls back to a64multi which produces 0-byte output that cannot be muxed).
	ffmpegErr := i.analyzer.Analyze(result.Stderr, result.ExitCode)
	if ffmpegErr != nil {
		intercepted.FFmpegError = ffmpegErr
		intercepted.IsSuccess = false
		intercepted.PruneRecommendations = i.pruner.GetPruneRecommendations(ffmpegErr.Type, result.Stderr)
	}

	// Detect silent hardware→software encoder fallback.
	// FFmpeg may exit 0 even when a requested hardware encoder (h264_qsv, h264_vaapi, etc.)
	// was unavailable and it silently fell back to a software encoder (libx264, etc.).
	// We detect this by checking if stderr reveals a software encoder was actually used
	// when the caller expected a hardware encoder.
	if intercepted.IsSuccess && requestedEncoder != "" && isHardwareEncoderByName(requestedEncoder) {
		if swEnc := detectSoftwareEncoderInStderr(result.Stderr); swEnc != "" && swEnc != requestedEncoder {
			intercepted.IsSuccess = false
			intercepted.FFmpegError = &FFmpegError{
				Type:      ErrorTypeHWAccelFailed,
				Message:   fmt.Sprintf("Hardware encoder %s silently fell back to software encoder %s", requestedEncoder, swEnc),
				ExitCode:  result.ExitCode,
				Stderr:    result.Stderr,
				Timestamp: time.Now(),
			}
		}
	}

	return intercepted
}

// isHardwareEncoderByName checks whether an encoder name indicates a hardware encoder.
// This is a standalone function so it can be used without an EncoderFallback instance.
func isHardwareEncoderByName(encoder string) bool {
	hwSuffixes := []string{
		"_nvenc",
		"_qsv",
		"_vaapi",
		"_amf",
		"_videotoolbox",
		"_cuvid",
		"_vdpau",
		"_nvdec",
	}
	for _, suffix := range hwSuffixes {
		if strings.HasSuffix(encoder, suffix) {
			return true
		}
	}
	return false
}

// detectSoftwareEncoderInStderr scans FFmpeg stderr output for evidence of a software encoder
// being used. Returns the encoder name found, or empty string if none detected.
// FFmpeg outputs encoder context lines like "[libx264 @ 0x...] using cpu capabilities: ..."
// which reveal the encoder that actually ran, even when a different encoder was requested
// and FFmpeg silently fell back.
func detectSoftwareEncoderInStderr(stderr string) string {
	softwareEncoders := []string{
		"libx264",
		"libx265",
		"libvpx-vp9",
		"libvpx",
		"libaom-av1",
		"libsvtav1",
		"mpeg2video",
		"mjpeg",
	}
	for _, enc := range softwareEncoders {
		// Check for the FFmpeg context pattern: [encoder_name @ ...]
		if strings.Contains(stderr, fmt.Sprintf("[%s @", enc)) {
			return enc
		}
	}
	return ""
}

// InterceptAndAnalyze intercepts, analyzes, and returns detailed error information.
func (i *ErrorInterceptor) InterceptAndAnalyze(ctx context.Context, result ExecResult) (*InterceptedResult, error) {
	intercepted := i.Intercept(ctx, result)

	if intercepted.FFmpegError != nil {
		return intercepted, intercepted.FFmpegError
	}

	if !intercepted.IsSuccess {
		return intercepted, fmt.Errorf("ffmpeg failed with exit code %d: %s", result.ExitCode, result.Stderr)
	}

	return intercepted, nil
}

// GetPatterns returns the error patterns used by the interceptor.
func (i *ErrorInterceptor) GetPatterns() []ErrorPattern {
	return i.patterns
}

// AddPattern adds a custom error pattern.
func (i *ErrorInterceptor) AddPattern(pattern ErrorPattern) {
	i.patterns = append(i.patterns, pattern)
	i.analyzer.AddPattern(pattern)
}

// ExecuteWithInterception executes an ffmpeg command and intercepts the result.
func (e *Executor) ExecuteWithInterception(ctx context.Context, args []string) (*InterceptedResult, error) {
	interceptor := NewErrorInterceptor()
	result := e.Execute(ctx, args)
	return interceptor.InterceptAndAnalyze(ctx, result)
}

// ExecuteWithInterceptionAndHandler executes with interception and a stderr handler.
func (e *Executor) ExecuteWithInterceptionAndHandler(ctx context.Context, args []string, stderrHandler StderrHandler) (*InterceptedResult, error) {
	interceptor := NewErrorInterceptor()
	result := e.ExecuteWithStderrHandler(ctx, args, stderrHandler)
	return interceptor.InterceptAndAnalyze(ctx, result)
}
