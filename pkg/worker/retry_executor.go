package worker

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"time"
)

// RetryStage represents the current stage of retry execution.
type RetryStage int

const (
	// RetryStageInitial indicates the initial execution attempt.
	RetryStageInitial RetryStage = iota
	// RetryStageHardwarePruned indicates retry with hardware-specific params removed.
	RetryStageHardwarePruned
	// RetryStageAdvancedPruned indicates retry with advanced encoding params removed.
	RetryStageAdvancedPruned
	// RetryStageSoftwareFallback indicates fallback to software encoder.
	RetryStageSoftwareFallback
	// RetryStageExhausted indicates all retry attempts exhausted.
	RetryStageExhausted
)

// String returns the string representation of the retry stage.
func (s RetryStage) String() string {
	switch s {
	case RetryStageInitial:
		return "initial"
	case RetryStageHardwarePruned:
		return "hardware_pruned"
	case RetryStageAdvancedPruned:
		return "advanced_pruned"
	case RetryStageSoftwareFallback:
		return "software_fallback"
	case RetryStageExhausted:
		return "exhausted"
	default:
		return "unknown"
	}
}

// Description returns a human-readable description of the retry stage.
func (s RetryStage) Description() string {
	switch s {
	case RetryStageInitial:
		return "Initial execution attempt"
	case RetryStageHardwarePruned:
		return "Retry with hardware-specific parameters removed"
	case RetryStageAdvancedPruned:
		return "Retry with advanced encoding parameters removed"
	case RetryStageSoftwareFallback:
		return "Fallback to software encoder"
	case RetryStageExhausted:
		return "All retry attempts exhausted"
	default:
		return "Unknown stage"
	}
}

// RetryConfig holds configuration for the retry executor.
type RetryConfig struct {
	// MaxRetries is the maximum number of retry attempts (default: 3).
	MaxRetries int `json:"max_retries" yaml:"max_retries"`

	// InitialInterval is the initial retry interval (default: 1 second).
	InitialInterval time.Duration `json:"initial_interval" yaml:"initial_interval"`

	// UseExponentialBackoff enables exponential backoff for retry intervals.
	UseExponentialBackoff bool `json:"use_exponential_backoff" yaml:"use_exponential_backoff"`

	// MaxInterval is the maximum retry interval when using exponential backoff.
	MaxInterval time.Duration `json:"max_interval" yaml:"max_interval"`

	// EnableSoftwareFallback enables fallback to software encoder after all retries fail.
	EnableSoftwareFallback bool `json:"enable_software_fallback" yaml:"enable_software_fallback"`
}

// DefaultRetryConfig returns a RetryConfig with sensible defaults.
func DefaultRetryConfig() *RetryConfig {
	return &RetryConfig{
		MaxRetries:             3,
		InitialInterval:        1 * time.Second,
		UseExponentialBackoff:  false,
		MaxInterval:            30 * time.Second,
		EnableSoftwareFallback: true,
	}
}

// RetryAuditEntry represents a single entry in the retry audit trail.
type RetryAuditEntry struct {
	// Timestamp is when this retry attempt occurred.
	Timestamp time.Time `json:"timestamp"`

	// Stage is the retry stage.
	Stage RetryStage `json:"stage"`

	// AttemptNumber is the attempt number (1-based).
	AttemptNumber int `json:"attempt_number"`

	// Args are the ffmpeg arguments used for this attempt.
	Args []string `json:"args"`

	// ErrorType is the classified error type (if any).
	ErrorType string `json:"error_type,omitempty"`

	// ErrorMessage is the error message from stderr.
	ErrorMessage string `json:"error_message,omitempty"`

	// ExitCode is the ffmpeg exit code.
	ExitCode int `json:"exit_code"`

	// PrunedParams lists parameters that were pruned before this attempt.
	PrunedParams []string `json:"pruned_params,omitempty"`

	// Encoder is the encoder used for this attempt.
	Encoder string `json:"encoder,omitempty"`

	// Success indicates whether this attempt succeeded.
	Success bool `json:"success"`

	// ExecResult is the execution result for this attempt.
	ExecResult ExecResult `json:"-"`
}

// RetryResult holds the result of a retry execution.
type RetryResult struct {
	// FinalResult is the final execution result.
	FinalResult ExecResult `json:"final_result"`

	// InterceptedResult is the intercepted result with error analysis.
	InterceptedResult *InterceptedResult `json:"intercepted_result,omitempty"`

	// AuditTrail contains all retry attempts and outcomes.
	AuditTrail []RetryAuditEntry `json:"audit_trail"`

	// TotalAttempts is the total number of execution attempts.
	TotalAttempts int `json:"total_attempts"`

	// FinalStage is the stage at which execution completed.
	FinalStage RetryStage `json:"final_stage"`

	// Success indicates whether execution eventually succeeded.
	Success bool `json:"success"`

	// UsedSoftwareEncoder indicates whether software fallback was used.
	UsedSoftwareEncoder bool `json:"used_software_encoder"`

	// OriginalEncoder is the original encoder requested.
	OriginalEncoder string `json:"original_encoder,omitempty"`

	// FinalEncoder is the encoder used in the final attempt.
	FinalEncoder string `json:"final_encoder,omitempty"`
}

// Executor runs ffmpeg commands. It is extracted as an interface so the
// retry executor can be tested with a fake (the concrete *Executor satisfies
// it).
type executorI interface {
	Execute(ctx context.Context, args []string) ExecResult
	ExecuteWithHandlers(ctx context.Context, args []string, stdoutHandler StdoutHandler, stderrHandler StderrHandler) ExecResult
}

// RetryExecutor wraps an Executor with automatic retry and fallback logic.
type RetryExecutor struct {
	// executor is the underlying ffmpeg executor.
	executor executorI

	// interceptor is the error interceptor.
	interceptor *ErrorInterceptor

	// pruner is the parameter pruner.
	pruner *ParamPruner

	// config is the retry configuration.
	config *RetryConfig

	// fallback handles software encoder fallback.
	fallback *EncoderFallback
}

// NewRetryExecutor creates a new retry executor.
func NewRetryExecutor(executor *Executor, config *RetryConfig) *RetryExecutor {
	if config == nil {
		config = DefaultRetryConfig()
	}
	return &RetryExecutor{
		executor:    executor,
		interceptor: NewErrorInterceptor(),
		pruner:      NewParamPruner(),
		config:      config,
		fallback:    NewEncoderFallback(),
	}
}

// NewRetryExecutorWithFallback creates a new retry executor with custom encoder fallback.
func NewRetryExecutorWithFallback(executor *Executor, config *RetryConfig, fallback *EncoderFallback) *RetryExecutor {
	if config == nil {
		config = DefaultRetryConfig()
	}
	if fallback == nil {
		fallback = NewEncoderFallback()
	}
	return &RetryExecutor{
		executor:    executor,
		interceptor: NewErrorInterceptor(),
		pruner:      NewParamPruner(),
		config:      config,
		fallback:    fallback,
	}
}

// ExecuteWithRetry executes an ffmpeg command with automatic retry and fallback.
// The outputPath parameter is required to ensure output file path consistency across retries.
// networkOutput indicates the output is a network URL (RTMP, RTSP, etc.) — when true,
// or the output is "-" (ffmpeg stdout, streaming mode), file existence/size validation
// is skipped since no local file is produced.
// stdoutHandler, when non-nil (streaming jobs), receives ffmpeg stdout chunks on every
// retry attempt so streamed data reaches the client instead of being discarded.
func (e *RetryExecutor) ExecuteWithRetry(ctx context.Context, args []string, outputPath string, networkOutput bool, stdoutHandler StdoutHandler) *RetryResult {
	result := &RetryResult{
		AuditTrail:    make([]RetryAuditEntry, 0),
		FinalStage:    RetryStageInitial,
		TotalAttempts: 0,
		Success:       false,
	}

	// Track the original encoder
	originalEncoder := extractEncoderFromArgs(args)
	result.OriginalEncoder = originalEncoder

	// The user-selected encoder is tracked as a local variable instead of on
	// the shared fallback: ExecuteWithRetry runs per job, and a shared field
	// let one concurrent job's registration leak into another's fallback
	// decisions.

	// Track current arguments and stage
	currentArgs := make([]string, len(args))
	copy(currentArgs, args)
	stage := RetryStageInitial

	// Track the encoder being used and whether it was user-selected
	currentEncoder := originalEncoder
	isUserSelected := true // Initial encoder is always user-selected (or unspecified)

	for attempt := 1; attempt <= e.config.MaxRetries; attempt++ {
		// Check context cancellation
		if ctx.Err() != nil {
			log.Printf("Retry executor cancelled at attempt %d", attempt)
			break
		}

		// Record attempt start
		attemptStart := time.Now()
		stage = e.determineStage(attempt)

		// Idempotency: a failed attempt may have left a partial output file
		// behind. A later attempt that exits 0 without producing output would
		// otherwise be misjudged as successful by the os.Stat validation.
		if !networkOutput && outputPath != "" && outputPath != "-" {
			if err := os.Remove(outputPath); err == nil {
				log.Printf("Removed stale output from previous attempt: %s", outputPath)
			}
		}

		// Execute
		var execResult ExecResult
		if stdoutHandler != nil {
			execResult = e.executor.ExecuteWithHandlers(ctx, currentArgs, stdoutHandler, nil)
		} else {
			execResult = e.executor.Execute(ctx, currentArgs)
		}
		result.TotalAttempts = attempt

		// Intercept and analyze — also detect silent hardware→software fallback
		intercepted := e.interceptor.InterceptWithEncoder(ctx, execResult, currentEncoder)
		if intercepted.IsSuccess {
			// Validate output file: FFmpeg may exit 0 but produce 0-byte output
			// (e.g., unknown codec falls back to a64multi, MP4 mux fails silently).
			// Skip for network outputs and "-" (stdout) — no local file is produced.
			if !networkOutput && outputPath != "" && outputPath != "-" {
				if info, err := os.Stat(outputPath); err != nil {
					log.Printf("Output file not accessible after exit code 0: %s: %v", outputPath, err)
					intercepted.IsSuccess = false
					intercepted.FFmpegError = &FFmpegError{
						Type:      ErrorTypeOutputEmpty,
						Message:   fmt.Sprintf("output file not found: %s", outputPath),
						ExitCode:  execResult.ExitCode,
						Stderr:    execResult.Stderr,
						Timestamp: time.Now(),
					}
				} else if info.Size() == 0 {
					log.Printf("Output file is 0 bytes after exit code 0: %s", outputPath)
					intercepted.IsSuccess = false
					intercepted.FFmpegError = &FFmpegError{
						Type:      ErrorTypeOutputEmpty,
						Message:   fmt.Sprintf("output file is empty (0 bytes): %s", outputPath),
						ExitCode:  execResult.ExitCode,
						Stderr:    execResult.Stderr,
						Timestamp: time.Now(),
					}
				}
			}

			if intercepted.IsSuccess {
				// Success!
				result.FinalResult = execResult
				result.InterceptedResult = intercepted
				result.Success = true
				result.FinalStage = stage
				result.FinalEncoder = currentEncoder

				// Record successful attempt
				result.AuditTrail = append(result.AuditTrail, RetryAuditEntry{
					Timestamp:     attemptStart,
					Stage:         stage,
					AttemptNumber: attempt,
					Args:          currentArgs,
					ExitCode:      execResult.ExitCode,
					Encoder:       currentEncoder,
					Success:       true,
					ExecResult:    execResult,
				})

				log.Printf("FFmpeg execution succeeded at stage %s (attempt %d)", stage, attempt)
				return result
			}
		}

		// Record failed attempt
		var errorType string
		var errorMsg string
		var prunedParams []string

		if intercepted.FFmpegError != nil {
			errorType = intercepted.FFmpegError.Type.String()
			errorMsg = intercepted.FFmpegError.Message
		}

		result.AuditTrail = append(result.AuditTrail, RetryAuditEntry{
			Timestamp:     attemptStart,
			Stage:         stage,
			AttemptNumber: attempt,
			Args:          currentArgs,
			ErrorType:     errorType,
			ErrorMessage:  errorMsg,
			ExitCode:      execResult.ExitCode,
			PrunedParams:  prunedParams,
			Encoder:       currentEncoder,
			Success:       false,
			ExecResult:    execResult,
		})

		// Check if error is retryable
		if intercepted.FFmpegError != nil && !intercepted.FFmpegError.IsRetryable() {
			log.Printf("Non-retryable error (type=%s), stopping retries", errorType)
			break
		}

		// Apply retry strategy for next attempt
		if attempt < e.config.MaxRetries {
			nextArgs, nextStage, pruned := e.applyRetryStrategy(currentArgs, intercepted, attempt, outputPath, isUserSelected, networkOutput)

			// If the strategy returned an exhausted stage (no valid fallback), stop retrying
			if nextStage == RetryStageExhausted {
				log.Printf("No valid encoder fallback available for %s, stopping retries", currentEncoder)
				break
			}

			// Update pruned params in last audit entry
			if len(result.AuditTrail) > 0 {
				result.AuditTrail[len(result.AuditTrail)-1].PrunedParams = pruned
			}

			// Check if we need to switch to software encoder
			newEncoder := extractEncoderFromArgs(nextArgs)
			if newEncoder != currentEncoder {
				log.Printf("Switching encoder from %s to %s for retry stage %s", currentEncoder, newEncoder, nextStage)
				currentEncoder = newEncoder
				isUserSelected = false // Encoder was chosen by the system as a fallback
				if !e.fallback.IsHardwareEncoder(newEncoder) {
					result.UsedSoftwareEncoder = true
				}
			}

			currentArgs = nextArgs
			stage = nextStage

			// Apply retry interval
			e.applyRetryInterval(ctx, attempt)
		}
	}

	// All retries exhausted
	result.FinalStage = RetryStageExhausted
	result.FinalEncoder = currentEncoder

	// Execute one final attempt with software fallback if enabled.
	// A cancelled job must not start another full transcode round: check the
	// context before committing to the fallback attempt.
	if ctx.Err() != nil {
		log.Printf("Skipping final software fallback: context cancelled")
	} else if e.config.EnableSoftwareFallback && !result.UsedSoftwareEncoder {
		fallbackArgs := e.fallback.PrepareFallbackArgsWithSource(currentArgs, outputPath, isUserSelected)
		if fallbackArgs != nil {
			log.Printf("Attempting final software encoder fallback")

			attemptStart := time.Now()
			var execResult ExecResult
			if stdoutHandler != nil {
				execResult = e.executor.ExecuteWithHandlers(ctx, fallbackArgs, stdoutHandler, nil)
			} else {
				execResult = e.executor.Execute(ctx, fallbackArgs)
			}
			result.TotalAttempts++
			result.UsedSoftwareEncoder = true

			currentEncoder = extractEncoderFromArgs(fallbackArgs)
			result.FinalEncoder = currentEncoder

			intercepted := e.interceptor.InterceptWithEncoder(ctx, execResult, currentEncoder)
			if intercepted.IsSuccess {
				// Validate output file for fallback attempt too.
				// Skip for network outputs and "-" (stdout) — no local file is produced.
				if !networkOutput && outputPath != "" && outputPath != "-" {
					if info, err := os.Stat(outputPath); err != nil {
						log.Printf("Fallback output file not accessible after exit code 0: %s: %v", outputPath, err)
						intercepted.IsSuccess = false
						intercepted.FFmpegError = &FFmpegError{
							Type:      ErrorTypeOutputEmpty,
							Message:   fmt.Sprintf("output file not found: %s", outputPath),
							ExitCode:  execResult.ExitCode,
							Stderr:    execResult.Stderr,
							Timestamp: time.Now(),
						}
					} else if info.Size() == 0 {
						log.Printf("Fallback output file is 0 bytes after exit code 0: %s", outputPath)
						intercepted.IsSuccess = false
						intercepted.FFmpegError = &FFmpegError{
							Type:      ErrorTypeOutputEmpty,
							Message:   fmt.Sprintf("output file is empty (0 bytes): %s", outputPath),
							ExitCode:  execResult.ExitCode,
							Stderr:    execResult.Stderr,
							Timestamp: time.Now(),
						}
					}
				}

				if intercepted.IsSuccess {
					result.FinalResult = execResult
					result.InterceptedResult = intercepted
					result.Success = true
					result.FinalStage = RetryStageSoftwareFallback

					result.AuditTrail = append(result.AuditTrail, RetryAuditEntry{
						Timestamp:     attemptStart,
						Stage:         RetryStageSoftwareFallback,
						AttemptNumber: result.TotalAttempts,
						Args:          fallbackArgs,
						ExitCode:      execResult.ExitCode,
						Encoder:       currentEncoder,
						Success:       true,
						ExecResult:    execResult,
					})

					return result
				}
			}

			// Record final fallback failure
			var errorType string
			var errorMsg string
			if intercepted.FFmpegError != nil {
				errorType = intercepted.FFmpegError.Type.String()
				errorMsg = intercepted.FFmpegError.Message
			}

			result.AuditTrail = append(result.AuditTrail, RetryAuditEntry{
				Timestamp:     attemptStart,
				Stage:         RetryStageSoftwareFallback,
				AttemptNumber: result.TotalAttempts,
				Args:          fallbackArgs,
				ErrorType:     errorType,
				ErrorMessage:  errorMsg,
				ExitCode:      execResult.ExitCode,
				Encoder:       currentEncoder,
				Success:       false,
				ExecResult:    execResult,
			})
		}
	}

	// Return final failure result
	if len(result.AuditTrail) > 0 {
		result.FinalResult = result.AuditTrail[len(result.AuditTrail)-1].ExecResult
		result.InterceptedResult = e.interceptor.Intercept(ctx, result.FinalResult)
	}

	return result
}

// determineStage determines the retry stage based on attempt number.
func (e *RetryExecutor) determineStage(attempt int) RetryStage {
	switch attempt {
	case 1:
		return RetryStageInitial
	case 2:
		return RetryStageHardwarePruned
	case 3:
		return RetryStageAdvancedPruned
	default:
		return RetryStageSoftwareFallback
	}
}

// applyRetryStrategy applies the appropriate retry strategy for the given attempt.
// Returns the new arguments, the new stage, and a list of pruned parameters.
// isUserSelected indicates whether the current encoder was explicitly chosen by the user
// (true) or was itself chosen by the system as a fallback from a previous failure (false).
// networkOutput indicates the output is a network URL — when true, output_empty is never
// a valid reason to trigger encoder fallback since no local file is expected.
func (e *RetryExecutor) applyRetryStrategy(args []string, intercepted *InterceptedResult, attempt int, outputPath string, isUserSelected bool, networkOutput bool) ([]string, RetryStage, []string) {
	// Immediate software fallback for output-empty errors.
	// When FFmpeg exits 0 but produces 0-byte output, the encoder is effectively
	// broken (e.g., unknown codec falls back to a64multi which cannot be muxed).
	// Skip progressive pruning and switch directly to a valid software encoder.
	// Skip for network outputs — output_empty is expected for network URLs
	// since no local file is produced. Retrying won't change anything.
	if !networkOutput && intercepted != nil && intercepted.FFmpegError != nil &&
		intercepted.FFmpegError.Type == ErrorTypeOutputEmpty {
		fallbackArgs := e.fallback.PrepareFallbackArgsWithSource(args, outputPath, isUserSelected)
		if fallbackArgs != nil {
			swEncoder := extractEncoderFromArgs(fallbackArgs)
			currentEncoder := extractEncoderFromArgs(args)
			// Only switch if we actually get a different encoder
			if swEncoder != "" && swEncoder != currentEncoder {
				return fallbackArgs, RetryStageSoftwareFallback, []string{"fallback_to_software_output_empty"}
			}
		}
	}

	// For encoder-not-found errors, pruning hardware/advanced params won't help.
	// Immediately try software encoder fallback to find a valid encoder.
	// Use PrepareFallbackArgsWithSource to enable chained fallback when the
	// current encoder was itself a system-selected fallback.
	if intercepted != nil && intercepted.FFmpegError != nil && intercepted.FFmpegError.Type == ErrorTypeEncoderNotFound {
		fallbackArgs := e.fallback.PrepareFallbackArgsWithSource(args, outputPath, isUserSelected)
		if fallbackArgs != nil {
			return fallbackArgs, RetryStageSoftwareFallback, []string{"fallback_to_software"}
		}
		// No fallback available — return args unchanged to let the loop exit
		return args, RetryStageExhausted, nil
	}

	switch attempt {
	case 1:
		// First retry: remove hardware-specific parameters
		prunedArgs, prunedParams := e.pruneHardwareParams(args)
		return prunedArgs, RetryStageHardwarePruned, prunedParams

	case 2:
		// Second retry: remove advanced encoding parameters
		prunedArgs, prunedParams := e.pruneAdvancedParams(args)
		return prunedArgs, RetryStageAdvancedPruned, prunedParams

	default:
		// Third+ retry: fallback to software encoder
		fallbackArgs := e.fallback.PrepareFallbackArgsWithSource(args, outputPath, isUserSelected)
		if fallbackArgs != nil {
			return fallbackArgs, RetryStageSoftwareFallback, []string{"fallback_to_software"}
		}
		return args, RetryStageAdvancedPruned, nil
	}
}

// pruneHardwareParams removes hardware-specific parameters and switches to a software encoder.
// Returns the pruned arguments and a list of removed parameters.
func (e *RetryExecutor) pruneHardwareParams(args []string) ([]string, []string) {
	// Get recommendations based on hardware acceleration error
	recommendations := e.pruner.GetPruneRecommendations(ErrorTypeHWAccelFailed, "hwaccel")

	// Also add device-specific params
	deviceParams := []string{"-hwaccel_device", "-vaapi_device", "-qsv_device", "-gpu"}
	recommendations = append(recommendations, deviceParams...)

	// Deduplicate
	removeSet := make(map[string]bool)
	for _, p := range recommendations {
		removeSet[p] = true
	}
	prunedParams := make([]string, 0, len(removeSet))
	for p := range removeSet {
		prunedParams = append(prunedParams, p)
	}

	prunedArgs := e.pruner.PruneParams(args, prunedParams)

	// Also switch encoder from hardware to software.
	// If hardware params are being pruned, the hardware encoder won't work,
	// so we must replace it with its software equivalent.
	currentEncoder := extractEncoderFromArgs(prunedArgs)
	if currentEncoder != "" && e.fallback.IsHardwareEncoder(currentEncoder) {
		fallbackArgs := e.fallback.PrepareFallbackArgs(prunedArgs, "", true)
		if fallbackArgs != nil {
			swEncoder := extractEncoderFromArgs(fallbackArgs)
			if swEncoder != "" && swEncoder != currentEncoder {
				prunedArgs = fallbackArgs
				prunedParams = append(prunedParams, "encoder:"+currentEncoder+"->"+swEncoder)
			}
		}
	}

	return prunedArgs, prunedParams
}

// pruneAdvancedParams removes advanced encoding parameters.
// Returns the pruned arguments and a list of removed parameters.
func (e *RetryExecutor) pruneAdvancedParams(args []string) ([]string, []string) {
	// First prune hardware params, then add advanced params
	prunedArgs, hwPruned := e.pruneHardwareParams(args)

	// Additional advanced params to remove
	advancedParams := []string{
		"-profile", "-profile:v",
		"-level", "-level:v",
		"-tier",
		"-preset",
		"-tune",
	}

	// Deduplicate with already pruned
	prunedSet := make(map[string]bool)
	for _, p := range hwPruned {
		prunedSet[p] = true
	}

	newPruned := make([]string, 0)
	for _, p := range advancedParams {
		if !prunedSet[p] {
			newPruned = append(newPruned, p)
		}
	}

	prunedArgs = e.pruner.PruneParams(prunedArgs, newPruned)
	allPruned := append(hwPruned, newPruned...)

	return prunedArgs, allPruned
}

// applyRetryInterval applies the configured retry interval.
func (e *RetryExecutor) applyRetryInterval(ctx context.Context, attempt int) {
	interval := e.config.InitialInterval

	if e.config.UseExponentialBackoff {
		// Exponential backoff: interval * 2^(attempt-1)
		for i := 1; i < attempt; i++ {
			interval *= 2
			if interval > e.config.MaxInterval {
				interval = e.config.MaxInterval
				break
			}
		}
	}

	select {
	case <-ctx.Done():
	case <-time.After(interval):
	}
}

// extractEncoderFromArgs extracts the encoder name from ffmpeg arguments.
func extractEncoderFromArgs(args []string) string {
	for i, arg := range args {
		if (arg == "-c:v" || arg == "-vcodec" || arg == "-codec:v") && i+1 < len(args) {
			return args[i+1]
		}
		if strings.HasPrefix(arg, "-c:v=") {
			return strings.TrimPrefix(arg, "-c:v=")
		}
		if strings.HasPrefix(arg, "-vcodec=") {
			return strings.TrimPrefix(arg, "-vcodec=")
		}
	}
	return ""
}

// GetAuditTrailSummary returns a human-readable summary of the audit trail.
func (r *RetryResult) GetAuditTrailSummary() string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Total attempts: %d\n", r.TotalAttempts))
	sb.WriteString(fmt.Sprintf("Final stage: %s\n", r.FinalStage))
	sb.WriteString(fmt.Sprintf("Success: %v\n", r.Success))
	sb.WriteString(fmt.Sprintf("Original encoder: %s\n", r.OriginalEncoder))
	sb.WriteString(fmt.Sprintf("Final encoder: %s\n", r.FinalEncoder))
	sb.WriteString(fmt.Sprintf("Used software fallback: %v\n", r.UsedSoftwareEncoder))
	sb.WriteString("\nAttempt details:\n")

	for _, entry := range r.AuditTrail {
		sb.WriteString(fmt.Sprintf("  - Attempt %d (%s): ", entry.AttemptNumber, entry.Stage))
		if entry.Success {
			sb.WriteString("SUCCESS\n")
		} else {
			sb.WriteString(fmt.Sprintf("FAILED (exit_code=%d, error_type=%s)\n", entry.ExitCode, entry.ErrorType))
		}
		if len(entry.PrunedParams) > 0 {
			sb.WriteString(fmt.Sprintf("    Pruned params: %v\n", entry.PrunedParams))
		}
	}

	return sb.String()
}
