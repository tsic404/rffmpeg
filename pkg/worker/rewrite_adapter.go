// Package worker provides FFmpeg worker functionality including job execution,
// capability detection, and encoder parameter rewriting integration.
package worker

import (
	"context"
	"fmt"
	"log"
	"sync"

	"github.com/tsix404/rffmpeg/pkg/encoder"
	"github.com/tsix404/rffmpeg/pkg/encoder/rewrite"
	"github.com/tsix404/rffmpeg/pkg/protocol"
	"strings"
)

// RewriteAdapter integrates the encoder rewrite engine with the Worker executor.
// It provides automatic encoder parameter rewriting based on worker capabilities.
type RewriteAdapter struct {
	// engine is the rewrite engine coordinator
	engine rewrite.RewriteEngine

	// hwCaps are the cached hardware capabilities
	hwCaps *rewrite.HardwareCapabilities

	// config holds adapter configuration
	config *RewriteAdapterConfig

	// mu protects concurrent access
	mu sync.RWMutex
}

// RewriteAdapterConfig holds configuration for the rewrite adapter.
type RewriteAdapterConfig struct {
	// Enabled enables/disables the rewrite functionality
	Enabled bool

	// Silent disables rewrite notifications in stderr
	Silent bool

	// FallbackToSoftware enables fallback to software when hardware fails
	FallbackToSoftware bool
}

// DefaultRewriteAdapterConfig returns the default configuration.
func DefaultRewriteAdapterConfig() *RewriteAdapterConfig {
	return &RewriteAdapterConfig{
		Enabled:            true,
		Silent:             false,
		FallbackToSoftware: true,
	}
}

// NewRewriteAdapter creates a new rewrite adapter with default configuration.
func NewRewriteAdapter() *RewriteAdapter {
	return &RewriteAdapter{
		engine: rewrite.NewEngineCoordinator(),
		config: DefaultRewriteAdapterConfig(),
	}
}

// NewRewriteAdapterWithEngine creates a new rewrite adapter with a custom engine.
func NewRewriteAdapterWithEngine(engine rewrite.RewriteEngine, config *RewriteAdapterConfig) *RewriteAdapter {
	if config == nil {
		config = DefaultRewriteAdapterConfig()
	}
	return &RewriteAdapter{
		engine: engine,
		config: config,
	}
}

// SetHardwareCapabilities sets the hardware capabilities for the adapter.
func (a *RewriteAdapter) SetHardwareCapabilities(caps *protocol.WorkerCapabilities) {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.hwCaps = a.convertCapabilities(caps)
}

// convertCapabilities converts protocol.WorkerCapabilities to rewrite.HardwareCapabilities.
func (a *RewriteAdapter) convertCapabilities(caps *protocol.WorkerCapabilities) *rewrite.HardwareCapabilities {
	if caps == nil {
		return &rewrite.HardwareCapabilities{}
	}

	hwCaps := &rewrite.HardwareCapabilities{
		AvailableEncoders: make([]encoder.EncoderFamily, 0),
		HardwareEncoders:  make([]encoder.EncoderFamily, 0),
		SoftwareEncoders:  make([]encoder.EncoderFamily, 0),
		SupportedCodecs:   make([]encoder.CodecFormat, 0),
		GPUDevices:        make([]rewrite.GPUDevice, 0),
		EncoderPriority:   rewrite.DefaultEncoderPriority(),
		EncoderBlacklist:  make([]encoder.EncoderFamily, 0),
	}

	// Convert encoders
	for _, enc := range caps.VideoEncoders {
		encFamily := encoder.EncoderFamily(enc.Name)
		hwCaps.AvailableEncoders = append(hwCaps.AvailableEncoders, encFamily)

		if enc.IsHW {
			hwCaps.HardwareEncoders = append(hwCaps.HardwareEncoders, encFamily)
		} else {
			hwCaps.SoftwareEncoders = append(hwCaps.SoftwareEncoders, encFamily)
		}

		// Track supported codecs
		codec := encFamily.CodecFormat()
		if codec != "" {
			found := false
			for _, c := range hwCaps.SupportedCodecs {
				if c == codec {
					found = true
					break
				}
			}
			if !found {
				hwCaps.SupportedCodecs = append(hwCaps.SupportedCodecs, codec)
			}
		}
	}

	// Convert GPU devices
	for _, dev := range caps.GPUDevices {
		hwCaps.GPUDevices = append(hwCaps.GPUDevices, rewrite.GPUDevice{
			Type:          dev.Type,
			Path:          dev.Path,
			Name:          dev.Name,
			Vendor:        dev.Vendor,
			DriverVersion: dev.DriverVersion,
			Accessible:    dev.Accessible,
			QSVHealthy:    dev.QSVHealthy,
		})
	}

	// Apply encoder priority from caps
	if len(caps.EncoderPriority) > 0 {
		hwCaps.EncoderPriority = make([]rewrite.EncoderPriorityEntry, 0)
		for i, name := range caps.EncoderPriority {
			hwCaps.EncoderPriority = append(hwCaps.EncoderPriority, rewrite.EncoderPriorityEntry{
				Encoder:  encoder.EncoderFamily(name),
				Priority: len(caps.EncoderPriority) - i,
			})
		}
	}

	// Apply encoder blacklist from caps
	if len(caps.EncoderBlacklist) > 0 {
		for _, name := range caps.EncoderBlacklist {
			hwCaps.EncoderBlacklist = append(hwCaps.EncoderBlacklist, encoder.EncoderFamily(name))
		}
	}

	return hwCaps
}

// RewriteArgs rewrites FFmpeg arguments based on hardware capabilities.
// This is the main entry point for argument rewriting.
//
// autoHW is passed per call instead of being mutated on the shared adapter
// config: concurrent jobs each carry their own flag, so one job's --auto-hw
// decision can no longer bleed into another job running concurrently.
func (a *RewriteAdapter) RewriteArgs(ctx context.Context, originalArgs []string, autoHW bool) ([]string, *RewriteResult, error) {
	a.mu.RLock()
	enabled := a.config.Enabled
	hwCaps := a.hwCaps
	a.mu.RUnlock()

	// If rewriting is disabled, return original args
	if !enabled {
		return originalArgs, &RewriteResult{Performed: false}, nil
	}

	// Parse encoder from args
	specifiedEncoder := a.parseEncoderFromArgs(originalArgs)

	// If -vn (no video) flag is present, video encoding is disabled entirely.
	// Skip rewriting unconditionally — we must not inject a video encoder (-c:v),
	// attempt auto-HW upgrade, or modify any video codec params, as that would
	// conflict with -vn and cause audio extraction (or other non-video tasks) to fail.
	if a.hasNoVideoFlag(originalArgs) {
		return originalArgs, &RewriteResult{Performed: false}, nil
	}

	// If the specified encoder is a passthrough encoder (e.g., "copy"),
	// skip rewriting entirely — these should never be translated.
	if a.isPassthroughEncoder(specifiedEncoder) {
		return originalArgs, &RewriteResult{Performed: false}, nil
	}

	// Build the rewrite request
	req := &rewrite.EncoderRewriteRequest{
		OriginalArgs:         originalArgs,
		HardwareCapabilities: *hwCaps,
		AutoHW:               autoHW,
		EncoderParams:        a.parseEncoderParamsFromArgs(originalArgs),
	}

	// Set parsed encoder
	req.SpecifiedEncoder = specifiedEncoder

	// Perform rewrite
	response, err := a.engine.Rewrite(ctx, req)
	if err != nil {
		return nil, nil, fmt.Errorf("rewrite failed: %w", err)
	}

	// Build result
	// Performed is true when any change was made: encoder selected/changed
	// or parameters translated.
	performed := response.TranslationPerformed ||
		response.OriginalEncoder != response.TargetEncoder
	result := &RewriteResult{
		Performed:           performed,
		OriginalEncoder:     string(response.OriginalEncoder),
		TargetEncoder:       string(response.TargetEncoder),
		Scenario:            response.Scenario.String(),
		Notifications:       make([]string, 0),
		AuditRecords:        response.AuditRecords,
		CapabilitiesSummary: a.buildCapabilitiesSummary(hwCaps),
		DecisionReason:      a.buildDecisionReason(response),
	}

	// Collect notifications
	for _, n := range response.Notifications {
		result.Notifications = append(result.Notifications, n.Message)
	}

	// Check for errors
	if len(response.Errors) > 0 {
		result.Error = response.Errors[0].Message

		// Check if this is an unknown encoder error (ENCODER_UNSUPPORTED)
		// Unknown encoders should NOT fall back - they indicate a user error
		if response.Errors[0].Code == rewrite.ErrEncoderUnsupported {
			return originalArgs, result, fmt.Errorf("encoder '%s' is not recognized: %s", specifiedEncoder, result.Error)
		}

		if a.config.FallbackToSoftware {
			// Try to fallback to software
			fallbackArgs, fallbackErr := a.fallbackToSoftware(ctx, originalArgs, hwCaps)
			if fallbackErr == nil {
				result.FallbackUsed = true
				// Generate notification for the fallback
				fallbackEncoder := a.parseEncoderFromArgs(fallbackArgs)
				if fallbackEncoder != "" {
					notification := fmt.Sprintf("[rffmpeg] %s unavailable, fallback to %s", specifiedEncoder, fallbackEncoder)
					result.Notifications = append(result.Notifications, notification)
				}
				return fallbackArgs, result, nil
			}
		}
		return originalArgs, result, fmt.Errorf("%s", result.Error)
	}

	return response.RewrittenArgs, result, nil
}

// ResolveTargetEncoder predicts the encoder a rewrite would select for these
// args without rewriting anything. It mirrors RewriteArgs' skip conditions
// (disabled adapter, -vn, passthrough "copy") so callers can key caches on
// the same encoder the actual rewrite will use. Returns "" when no rewrite
// applies or no target can be determined.
func (a *RewriteAdapter) ResolveTargetEncoder(args []string, autoHW bool) string {
	a.mu.RLock()
	enabled := a.config.Enabled
	hwCaps := a.hwCaps
	a.mu.RUnlock()

	if !enabled {
		return ""
	}

	specifiedEncoder := a.parseEncoderFromArgs(args)
	if a.hasNoVideoFlag(args) || a.isPassthroughEncoder(specifiedEncoder) {
		return ""
	}

	req := &rewrite.EncoderRewriteRequest{
		OriginalArgs:         args,
		HardwareCapabilities: *hwCaps,
		AutoHW:               autoHW,
		EncoderParams:        a.parseEncoderParamsFromArgs(args),
		SpecifiedEncoder:     specifiedEncoder,
	}

	response, err := a.engine.Rewrite(context.Background(), req)
	if err != nil || response.TargetEncoder == "" {
		return ""
	}
	return string(response.TargetEncoder)
}

// RewriteResult holds the result of a rewrite operation.
type RewriteResult struct {
	// Performed indicates whether rewriting was performed
	Performed bool

	// OriginalEncoder is the original encoder (if specified)
	OriginalEncoder string

	// TargetEncoder is the encoder after rewriting
	TargetEncoder string

	// Scenario is the rewrite scenario that was applied
	Scenario string

	// Notifications contains human-readable notification messages
	Notifications []string

	// AuditRecords contains detailed audit information
	AuditRecords []rewrite.AuditRecord

	// CapabilitiesSummary is a concise summary of worker HW capabilities at time of rewrite
	CapabilitiesSummary string

	// DecisionReason is the human-readable reason for the rewrite decision
	DecisionReason string

	// Error contains any error message
	Error string

	// FallbackUsed indicates if software fallback was used
	FallbackUsed bool
}

// buildCapabilitiesSummary creates a concise string summary of worker hardware capabilities.
func (a *RewriteAdapter) buildCapabilitiesSummary(hwCaps *rewrite.HardwareCapabilities) string {
	if hwCaps == nil {
		return "unknown"
	}
	encoders := make([]string, 0, len(hwCaps.AvailableEncoders))
	for _, enc := range hwCaps.AvailableEncoders {
		marker := string(enc)
		if enc.IsHardware() {
			marker += "[HW]"
		}
		encoders = append(encoders, marker)
	}
	if len(encoders) == 0 {
		return "none"
	}
	return strings.Join(encoders, ",")
}

// buildDecisionReason constructs a human-readable decision reason from the rewrite response.
func (a *RewriteAdapter) buildDecisionReason(response *rewrite.EncoderRewriteResponse) string {
	if response == nil {
		return ""
	}
	scenarioInfo := response.Scenario.String()
	if response.OriginalEncoder != response.TargetEncoder {
		if response.TargetEncoder.IsHardware() && !response.OriginalEncoder.IsHardware() {
			return fmt.Sprintf("auto-upgrade to hardware encoder %s (%s)", response.TargetEncoder, scenarioInfo)
		}
		return fmt.Sprintf("%s -> %s (%s)", response.OriginalEncoder, response.TargetEncoder, scenarioInfo)
	}
	if response.TranslationPerformed {
		return fmt.Sprintf("parameters translated for %s (%s)", response.TargetEncoder, scenarioInfo)
	}
	return scenarioInfo
}

// parseEncoderFromArgs extracts the video encoder from FFmpeg arguments.
func (a *RewriteAdapter) parseEncoderFromArgs(args []string) encoder.EncoderFamily {
	for i, arg := range args {
		if arg == "-c:v" || arg == "-codec:v" || arg == "-vcodec" {
			if i+1 < len(args) {
				return encoder.EncoderFamily(args[i+1])
			}
		}
		// Handle -c:v=encoder syntax
		if len(arg) > 4 && arg[:4] == "-c:v" {
			if arg[4] == '=' {
				return encoder.EncoderFamily(arg[5:])
			}
		}
	}
	return ""
}

// isPassthroughEncoder returns true if the encoder is a passthrough encoder
// that should not be rewritten (e.g., "copy").
func (a *RewriteAdapter) isPassthroughEncoder(enc encoder.EncoderFamily) bool {
	// "copy" is a special FFmpeg option that copies the stream without re-encoding.
	// It should never be rewritten or translated.
	return enc == "copy"
}

// parseEncoderParamsFromArgs extracts encoder-specific parameters from FFmpeg arguments.
// This includes parameters like -crf, -preset, -b:v, -cq, etc.
func (a *RewriteAdapter) parseEncoderParamsFromArgs(args []string) map[string]string {
	params := make(map[string]string)

	// FFmpeg flags that take a value but are NOT encoder-specific params
	// These should be skipped when parsing for encoder params
	skipFlags := map[string]bool{
		"i":         true, // input file
		"o":         true, // output file (rare)
		"f":         true, // format
		"c":         true, // codec (general)
		"c:a":       true, // audio codec
		"c:s":       true, // subtitle codec
		"c:d":       true, // data codec
		"codec":     true,
		"codec:a":   true,
		"codec:s":   true,
		"codec:d":   true,
		"acodec":    true, // audio codec
		"scodec":    true, // subtitle codec
		"vcodec":    true, // video codec (handled separately)
		"bsf:v":     true, // bitstream filter
		"bsf:a":     true,
		"filter:v":  true, // video filter
		"filter:a":  true, // audio filter
		"vf":        true, // video filter shorthand
		"af":        true, // audio filter shorthand
		"map":       true, // stream mapping
		"metadata":  true,
		"t":         true, // duration
		"ss":        true, // start time
		"to":        true, // end time
		"fs":        true, // file size limit
		"timestamp": true,
		"rt":        true, // realtime
		"threads":   true, // thread count (not encoder param)
	}

	// FFmpeg boolean flags that do NOT take a value argument.
	// Without this set, the parser would treat the next positional arg
	// (typically the output file) as the flag's value and either lose it
	// from the arg list or corrupt the param map.
	booleanFFmpegFlags := map[string]bool{
		"shortest":      true,
		"y":             true,
		"n":             true,
		"vn":            true,
		"an":            true,
		"sn":            true,
		"dn":            true,
		"stats":         true,
		"hide_banner":   true,
		"report":        true,
		"benchmark":     true,
		"copyts":        true,
		"start_at_zero": true,
		"bitexact":      true,
		"re":            true,
	}

	for i := 0; i < len(args); i++ {
		arg := args[i]

		// Skip encoder specification
		if arg == "-c:v" || arg == "-codec:v" || arg == "-vcodec" {
			i++ // Skip value too
			continue
		}

		// Handle -c:v=encoder syntax
		if len(arg) > 4 && arg[:4] == "-c:v" && arg[4] == '=' {
			continue
		}

		// Handle -param value or -param=value
		if len(arg) > 0 && arg[0] == '-' {
			paramName := arg[1:]

			// Check for = syntax
			if idx := findEqual(paramName); idx >= 0 {
				name := paramName[:idx]
				if !skipFlags[name] {
					params[name] = paramName[idx+1:]
				}
				continue
			}

			// Check if this is a flag to skip
			if skipFlags[paramName] {
				// Skip the value too if next arg exists and is not a flag
				if i+1 < len(args) && (len(args[i+1]) == 0 || args[i+1][0] != '-') {
					i++
				}
				continue
			}

			// Boolean flags don't consume the next arg as a value
			if booleanFFmpegFlags[paramName] {
				continue
			}

			// Check if next arg is the value (not a flag)
			if i+1 < len(args) && (len(args[i+1]) == 0 || args[i+1][0] != '-') {
				params[paramName] = args[i+1]
				i++ // Skip value
			}
		}
	}

	return params
}

func findEqual(s string) int {
	for i := 0; i < len(s); i++ {
		if s[i] == '=' {
			return i
		}
	}
	return -1
}

// hasNoVideoFlag checks if the original args contain -vn (no video) flag.
// When -vn is present, video encoding is disabled entirely, so we must not
// inject any video encoder (-c:v) or attempt auto-HW upgrade.
func (a *RewriteAdapter) hasNoVideoFlag(args []string) bool {
	for _, arg := range args {
		if arg == "-vn" {
			return true
		}
	}
	return false
}

// fallbackToSoftware attempts to fallback to software encoding.
// It returns an error if the specified encoder is completely unknown
// (i.e., its codec format cannot be determined).
func (a *RewriteAdapter) fallbackToSoftware(ctx context.Context, originalArgs []string, hwCaps *rewrite.HardwareCapabilities) ([]string, error) {
	// If -vn flag is present, video encoding is disabled — don't inject
	// a video encoder during fallback either.
	if a.hasNoVideoFlag(originalArgs) {
		return originalArgs, nil
	}

	// Find software encoder for the codec
	codec := encoder.CodecH264 // Default to H.264

	// Try to detect codec from original args
	originalEncoder := a.parseEncoderFromArgs(originalArgs)
	if originalEncoder != "" {
		if detected := originalEncoder.CodecFormat(); detected != "" {
			codec = detected
		} else {
			// The encoder is completely unknown (CodecFormat returns "").
			// This indicates a user error (typo, non-existent encoder name).
			// We should NOT silently fall back to libx264 - return an error instead.
			return nil, fmt.Errorf("encoder '%s' is not recognized", originalEncoder)
		}
	}

	// Get software encoder for codec
	swEncoder := hwCaps.GetSoftwareEncoder(codec)
	if swEncoder == "" {
		swEncoder = rewrite.SoftwareEncoderForCodec(codec)
	}

	if swEncoder == "" {
		return nil, fmt.Errorf("no software encoder available for codec %s", codec)
	}

	// Replace encoder in args
	result := make([]string, 0, len(originalArgs))
	skipNext := false
	encoderReplaced := false

	for _, arg := range originalArgs {
		if skipNext {
			skipNext = false
			continue
		}

		if arg == "-c:v" || arg == "-codec:v" || arg == "-vcodec" {
			skipNext = true
			if !encoderReplaced {
				result = append(result, "-c:v", string(swEncoder))
				encoderReplaced = true
			}
			continue
		}

		// Handle -c:v=encoder syntax
		if len(arg) > 4 && arg[:4] == "-c:v" && arg[4] == '=' {
			if !encoderReplaced {
				result = append(result, fmt.Sprintf("-c:v=%s", swEncoder))
				encoderReplaced = true
			}
			continue
		}

		result = append(result, arg)
	}

	// Add encoder if not present
	if !encoderReplaced {
		result = append(result, "-c:v", string(swEncoder))
	}

	log.Printf("Fallback to software encoder: %s", swEncoder)
	return result, nil
}

// ShouldRewrite checks if rewriting should be performed for the given args.
func (a *RewriteAdapter) ShouldRewrite(args []string, autoHW bool) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()

	if !a.config.Enabled {
		return false
	}

	// If -vn (no video) flag is present, video encoding is disabled entirely.
	// We must NOT rewrite — no video encoder should be injected or modified.
	if a.hasNoVideoFlag(args) {
		return false
	}

	// Check if encoder is specified
	enc := a.parseEncoderFromArgs(args)

	// If no encoder specified, we should rewrite to add one
	if enc == "" {
		return true
	}

	// Passthrough encoders (like "copy") should never be rewritten
	if a.isPassthroughEncoder(enc) {
		return false
	}

	// If encoder is specified but not available locally, we should rewrite
	if a.hwCaps != nil && !a.hwCaps.HasEncoder(enc) {
		return true
	}

	// If auto-hw is enabled and we have hardware encoders, consider upgrade
	if autoHW && a.hwCaps != nil && a.hwCaps.HasHardwareEncoder() {
		// Check if current encoder is software
		if !enc.IsHardware() {
			return true
		}
	}

	return false
}

// GetRewriteEngine returns the underlying rewrite engine for advanced configuration.
func (a *RewriteAdapter) GetRewriteEngine() rewrite.RewriteEngine {
	return a.engine
}

// SetEnabled enables or disables the rewrite adapter.
func (a *RewriteAdapter) SetEnabled(enabled bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.config.Enabled = enabled
}

// SetSilent enables or disables notification output.
func (a *RewriteAdapter) SetSilent(silent bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.config.Silent = silent
}
