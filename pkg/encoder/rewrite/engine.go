package rewrite

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/tsix404/rffmpeg/pkg/encoder"
)

// EngineCoordinator implements the RewriteEngine interface.
// It orchestrates all rewrite components (classifier, translator, injector, auditor, notifier)
// to perform complete encoder parameter rewriting.
type EngineCoordinator struct {
	// classifier determines the rewrite scenario
	classifier ScenarioClassifier

	// translator handles parameter translation between encoders
	translator ParameterTranslator

	// hardwareInjector injects hardware-specific parameters
	hardwareInjector HardwareInjector

	// auditRecorder records audit information
	auditRecorder AuditRecorder

	// notifier outputs real-time notifications
	notifier Notifier

	// config holds engine configuration
	config *EngineConfig
}

// EngineConfig holds configuration for the engine coordinator.
type EngineConfig struct {
	// EnableAudit enables audit recording
	EnableAudit bool

	// EnableNotifications enables stderr notifications
	EnableNotifications bool

	// SilentMode disables all notifications
	SilentMode bool

	// DefaultCodec is the default codec when none is specified
	DefaultCodec encoder.CodecFormat
}

// DefaultEngineConfig returns the default engine configuration.
func DefaultEngineConfig() *EngineConfig {
	return &EngineConfig{
		EnableAudit:         true,
		EnableNotifications: true,
		SilentMode:          false,
		DefaultCodec:        encoder.CodecH264,
	}
}

// NewEngineCoordinator creates a new engine coordinator with default components.
func NewEngineCoordinator() *EngineCoordinator {
	return &EngineCoordinator{
		classifier:       NewScenarioClassifier(),
		translator:       NewTranslatorAdapter(),
		hardwareInjector: NewHardwareInjector(),
		config:           DefaultEngineConfig(),
	}
}

// NewEngineCoordinatorWithComponents creates a new engine coordinator with custom components.
func NewEngineCoordinatorWithComponents(
	classifier ScenarioClassifier,
	translator ParameterTranslator,
	injector HardwareInjector,
	recorder AuditRecorder,
	notifier Notifier,
	config *EngineConfig,
) *EngineCoordinator {
	e := &EngineCoordinator{
		classifier:       classifier,
		translator:       translator,
		hardwareInjector: injector,
		auditRecorder:    recorder,
		notifier:         notifier,
	}
	if config != nil {
		e.config = config
	} else {
		e.config = DefaultEngineConfig()
	}
	return e
}

// SetClassifier sets the scenario classifier implementation.
func (e *EngineCoordinator) SetClassifier(classifier ScenarioClassifier) {
	e.classifier = classifier
}

// SetTranslator sets the parameter translator implementation.
func (e *EngineCoordinator) SetTranslator(translator ParameterTranslator) {
	e.translator = translator
}

// SetHardwareInjector sets the hardware injector implementation.
func (e *EngineCoordinator) SetHardwareInjector(injector HardwareInjector) {
	e.hardwareInjector = injector
}

// SetAuditRecorder sets the audit recorder implementation.
func (e *EngineCoordinator) SetAuditRecorder(recorder AuditRecorder) {
	e.auditRecorder = recorder
}

// SetNotifier sets the notifier implementation.
func (e *EngineCoordinator) SetNotifier(notifier Notifier) {
	e.notifier = notifier
}

// Rewrite performs a complete rewrite of the encoder parameters.
// It follows the workflow:
// 1. Classify the scenario
// 2. Select target encoder
// 3. Translate parameters (if needed)
// 4. Inject hardware parameters (if needed)
// 5. Record audit information
// 6. Generate notifications
// 7. Return rewritten parameters
func (e *EngineCoordinator) Rewrite(ctx context.Context, req *EncoderRewriteRequest) (*EncoderRewriteResponse, error) {
	startTime := time.Now()

	// Generate request ID if not provided
	if req.RequestID == "" {
		req.RequestID = uuid.New().String()
	}
	if req.Timestamp.IsZero() {
		req.Timestamp = startTime
	}

	response := &EncoderRewriteResponse{
		RequestID:     req.RequestID,
		Timestamp:     startTime,
		AuditRecords:  make([]AuditRecord, 0),
		Notifications: make([]Notification, 0),
		Errors:        make([]RewriteError, 0),
		Warnings:      make([]string, 0),
	}

	// Step 1: Classify the scenario
	scenario, err := e.classifier.Classify(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("classification failed: %w", err)
	}
	response.Scenario = scenario

	// Record scenario classification audit
	e.recordAudit(&response.AuditRecords, AuditRecord{
		ID:        uuid.New().String(),
		Timestamp: time.Now(),
		Operation: AuditOpScenarioClassify,
		Reason:    fmt.Sprintf("Classified as scenario: %s", scenario.String()),
		Success:   true,
	})

	// Get scenario info
	scenarioInfo := e.classifier.GetScenarioInfo(scenario)

	// Check for error scenarios
	if scenarioInfo.IsError {
		// Determine the correct error code based on scenario type
		errorCode := ErrFormatNotMatch
		if scenario == ScenarioEncoderUnsupported {
			errorCode = ErrEncoderUnsupported
		}
		response.Errors = append(response.Errors, RewriteError{
			Code:    errorCode,
			Message: scenarioInfo.Description,
		})
		// Generate a consistent rewrite notification even in error scenarios,
		// so the "[rffmpeg]" line is always visible to users.
		e.notify(response, NotificationLevelError, fmt.Sprintf(
			"[rffmpeg] %s -> (unavailable) (%s)",
			e.formatEncoder(req.SpecifiedEncoder),
			scenarioInfo.Description,
		))
		return response, nil
	}

	// Step 2: Select target encoder
	targetEncoder := e.classifier.SelectTargetEncoder(scenario, req)
	if targetEncoder == "" {
		response.Errors = append(response.Errors, RewriteError{
			Code:    ErrNoSuitableEncoder,
			Message: "No suitable encoder found",
		})
		// Generate a consistent rewrite notification even when no target
		// encoder is found, so the "[rffmpeg]" line is not skipped
		// due to codec lookup failure.
		e.notify(response, NotificationLevelError, fmt.Sprintf(
			"[rffmpeg] %s -> (none) (No suitable encoder found)",
			e.formatEncoder(req.SpecifiedEncoder),
		))
		return response, nil
	}

	// Record encoder selection
	e.recordAudit(&response.AuditRecords, AuditRecord{
		ID:            uuid.New().String(),
		Timestamp:     time.Now(),
		Operation:     AuditOpEncoderSelect,
		SourceEncoder: req.SpecifiedEncoder,
		TargetEncoder: targetEncoder,
		Reason:        fmt.Sprintf("Selected encoder for scenario: %s", scenario.String()),
		Success:       true,
	})

	response.OriginalEncoder = req.SpecifiedEncoder
	response.TargetEncoder = targetEncoder

	// Step 3: Translate parameters (if needed)
	var translatedParams map[string]string
	var paramsToFilter map[string]string    // Original params that should be filtered from args
	var sameNameConverted map[string]string // Same-name params whose values were converted
	if scenarioInfo.RequiresTranslation && e.translator != nil {
		translationResult, err := e.translator.Translate(
			ctx,
			req.SpecifiedEncoder,
			targetEncoder,
			req.EncoderParams,
		)
		if err != nil {
			response.Warnings = append(response.Warnings, fmt.Sprintf("Translation warning: %v", err))
		}
		if translationResult != nil {
			translatedParams = translationResult.TranslatedParams
			if translatedParams == nil {
				translatedParams = make(map[string]string)
			}
			response.TranslationPerformed = true

			// Record translation audit
			e.recordAudit(&response.AuditRecords, AuditRecord{
				ID:            uuid.New().String(),
				Timestamp:     time.Now(),
				Operation:     AuditOpParamTranslate,
				SourceEncoder: req.SpecifiedEncoder,
				TargetEncoder: targetEncoder,
				Reason:        fmt.Sprintf("Translated %d parameters", len(translatedParams)),
				Success:       true,
			})

			// Merge hardware params from translator
			for k, v := range translationResult.HardwareParams {
				translatedParams[k] = v
			}

			// Collect warnings from translation
			response.Warnings = append(response.Warnings, translationResult.Warnings...)

			// When translation is performed, we need to filter out the original params
			// that were translated (e.g., "crf" -> "quality").
			// Only filter params whose translated name genuinely changed (source != target);
			// non-translated params (skipped with empty target, like preset→"" for VAAPI)
			// must be preserved in the output args.
			paramsToFilter = make(map[string]string)
			for _, record := range translationResult.AuditRecords {
				if record.SourceParam != "" && record.SourceParam != record.TargetParam && record.TargetParam != "" {
					paramsToFilter[record.SourceParam] = record.SourceValue
				}
			}

			// Same-name params whose VALUE was converted by a translation rule
			// (e.g., x264 preset "fast" -> NVENC "p5", audit Reason carries the
			// converter). These keep their flag name but must have their value
			// rewritten in place so inline (-preset=fast) and separate forms
			// behave identically. Hardware injections are not user params.
			sameNameConverted = make(map[string]string)
			for _, record := range translationResult.AuditRecords {
				if record.Success &&
					record.SourceParam != "" && record.SourceParam == record.TargetParam &&
					record.Reason != "" && record.Reason != encoder.ConverterUsedHardwareInjection {
					sameNameConverted[record.SourceParam] = record.TargetValue
				}
			}
		} else {
			// Translation returned nil result, use original params
			translatedParams = make(map[string]string)
			for k, v := range req.EncoderParams {
				translatedParams[k] = v
			}
		}
	} else {
		// No translation needed, use original params
		translatedParams = make(map[string]string)
		for k, v := range req.EncoderParams {
			translatedParams[k] = v
		}
	}

	// Step 4: Inject hardware parameters (if needed)
	if scenarioInfo.RequiresHWInjection && e.hardwareInjector != nil {
		injectionResult, err := e.hardwareInjector.Inject(ctx, targetEncoder, translatedParams, &req.HardwareCapabilities)
		if err != nil {
			response.Warnings = append(response.Warnings, fmt.Sprintf("Hardware injection warning: %v", err))
		}
		if injectionResult != nil {
			// Merge injected params
			for k, v := range injectionResult.InjectedParams {
				translatedParams[k] = v
			}

			// Add audit records from injection
			response.AuditRecords = append(response.AuditRecords, injectionResult.AuditRecords...)
		}
	}

	// Step 5: Build rewritten arguments
	response.RewrittenArgs = e.buildRewrittenArgs(req.OriginalArgs, targetEncoder, translatedParams, paramsToFilter, sameNameConverted)

	// Step 6: Generate notifications
	// Notification message formats:
	//   - Auto-hw upfront: [rffmpeg] auto-selected <encoder>
	//   - Auto-hw upgrade: [rffmpeg] upgraded <original> → <target>
	//   - True fallback:   [rffmpeg] <original> unavailable, fallback to <target>
	var notificationMsg string
	switch scenario {
	case ScenarioSpecifiedEncoderUnsupportedWithAlternative, ScenarioSpecifiedEncoderUnsupportedFallbackSoftware:
		// Check if this is an auto-hw upgrade (original encoder is available and software)
		// vs a true fallback (original encoder is not available)
		originalEncoderAvailable := req.HardwareCapabilities.HasEncoder(req.SpecifiedEncoder) &&
			!req.HardwareCapabilities.IsBlacklisted(req.SpecifiedEncoder)
		if req.AutoHW && originalEncoderAvailable && !req.SpecifiedEncoder.IsHardware() {
			// Auto-hw upgrade: software encoder upgraded to hardware
			notificationMsg = fmt.Sprintf("[rffmpeg] upgraded %s → %s",
				e.formatEncoder(req.SpecifiedEncoder),
				targetEncoder)
		} else {
			// True fallback: encoder not available, using alternative
			notificationMsg = fmt.Sprintf("[rffmpeg] %s unavailable, fallback to %s",
				e.formatEncoder(req.SpecifiedEncoder),
				targetEncoder)
		}
	case ScenarioUnspecifiedEncoderWithHW:
		// Auto-HW selection: no encoder specified, automatically selected best HW encoder
		notificationMsg = fmt.Sprintf("[rffmpeg] auto-selected %s", targetEncoder)

	default:
		notificationMsg = fmt.Sprintf(
			"[rffmpeg] %s -> %s (%s)",
			e.formatEncoder(req.SpecifiedEncoder),
			targetEncoder,
			scenarioInfo.Description,
		)
	}
	e.notify(response, NotificationLevelInfo, notificationMsg)

	// Record final audit summary
	if e.auditRecorder != nil {
		for _, record := range response.AuditRecords {
			recordCopy := record
			if err := e.auditRecorder.Record(ctx, &recordCopy); err != nil {
				response.Warnings = append(response.Warnings, fmt.Sprintf("Audit recording warning: %v", err))
			}
		}
	}

	return response, nil
}

// recordAudit appends an audit record to the slice.
func (e *EngineCoordinator) recordAudit(records *[]AuditRecord, record AuditRecord) {
	*records = append(*records, record)
}

// notify generates a notification.
func (e *EngineCoordinator) notify(response *EncoderRewriteResponse, level NotificationLevel, message string) {
	notification := Notification{
		Timestamp: time.Now(),
		Level:     level,
		Message:   message,
	}
	response.Notifications = append(response.Notifications, notification)

	// Send to notifier if enabled
	if e.notifier != nil && e.config.EnableNotifications && !e.config.SilentMode {
		_ = e.notifier.Notify(context.Background(), &notification)
	}
}

// formatEncoder returns a human-readable encoder name.
func (e *EngineCoordinator) formatEncoder(enc encoder.EncoderFamily) string {
	if enc == "" {
		return "(none)"
	}
	return string(enc)
}

// globalInitParamNames lists ffmpeg global initialization parameters that must
// appear before the first input file. Unlike encoder-specific parameters
// (which follow -c:v), these configure hardware device initialization and
// hardware-accelerated decoding and must be set early in the command line.
var globalInitParamNames = []string{
	"init_hw_device",
	"hwaccel",
	"hwaccel_output_format",
}

// isGlobalInitParam reports whether paramName is a global initialization param.
func isGlobalInitParam(paramName string) bool {
	for _, gp := range globalInitParamNames {
		if paramName == gp {
			return true
		}
	}
	return false
}

// buildRewrittenArgs constructs the rewritten FFmpeg arguments.
// paramsToFilter contains original encoder parameters that should be removed
// from the args (these were translated to different parameter names).
// sameNameConverted maps parameters that kept their name but had their value
// converted by a translation rule (e.g., x264 "preset fast" -> NVENC "p5").
func (e *EngineCoordinator) buildRewrittenArgs(originalArgs []string, targetEncoder encoder.EncoderFamily, params map[string]string, paramsToFilter map[string]string, sameNameConverted map[string]string) []string {
	result := make([]string, 0, len(originalArgs)+len(params)+2)

	// Collect global initialization args (init_hw_device, hwaccel, etc.)
	// that must be placed before the first input file.  Encoder-specific
	// params are appended later (after -c:v).  We also check the original
	// args so global init params already present are not duplicated.
	var globalInitArgs []string
	existingGlobalParams := make(map[string]bool)
	for _, paramName := range globalInitParamNames {
		if paramValue, ok := params[paramName]; ok {
			// Only prepend if not already in original args
			if !hasGlobalParam(originalArgs, paramName) {
				globalInitArgs = append(globalInitArgs, fmt.Sprintf("-%s", paramName), paramValue)
			}
		}
		// Track params that exist in original args
		if hasGlobalParam(originalArgs, paramName) {
			existingGlobalParams[paramName] = true
		}
	}

	// Prepend global init args at the very beginning (before everything)
	result = append(result, globalInitArgs...)

	// Track if we've added the encoder
	encoderAdded := false
	// Track encoder params we've seen (including global params we already handled)
	seenParams := make(map[string]bool)
	for paramName := range params {
		if isGlobalInitParam(paramName) {
			seenParams[paramName] = true // Global params already handled above
		}
	}
	// Mark any global param already present in original args
	for paramName := range existingGlobalParams {
		seenParams[paramName] = true
	}

	skipNext := false
	for i, arg := range originalArgs {
		if skipNext {
			skipNext = false
			continue
		}

		// Positional argument: either the value of the previous flag or
		// an input/output path. It is never a parameter flag.
		if !strings.HasPrefix(arg, "-") {
			result = append(result, arg)
			continue
		}

		// Handle encoder specification (-c:v, -codec:v, -vcodec)
		if arg == "-c:v" || arg == "-codec:v" || arg == "-vcodec" {
			// Skip both the flag and its value
			skipNext = true
			if !encoderAdded {
				result = append(result, "-c:v", string(targetEncoder))
				encoderAdded = true
			}
			continue
		}

		// Handle encoder params that we need to filter or translate
		if len(params) > 0 || len(paramsToFilter) > 0 || len(sameNameConverted) > 0 {
			paramName := strings.TrimPrefix(arg, "-")

			// Inline args carry their own value, so they never consume the
			// next argument.
			inlineForm := strings.IndexByte(paramName, '=') != -1

			// Match tables first by FULL name (keys may contain stream
			// specifiers, e.g. "b:v", "bsf:v"), then fall back to the base
			// name with any inline value and specifier stripped
			// (e.g. -crf=23 -> crf, -crf:v -> crf).
			fullName := paramName
			if idx := strings.IndexByte(fullName, '='); idx != -1 {
				fullName = fullName[:idx]
			}
			baseName := fullName
			if idx := strings.IndexByte(baseName, ':'); idx != -1 {
				baseName = baseName[:idx]
			}

			// Lookup helper: try full name first, then base name.
			lookup := func(m map[string]string) (string, bool) {
				if v, ok := m[fullName]; ok {
					return v, true
				}
				if v, ok := m[baseName]; ok {
					return v, true
				}
				return "", false
			}
			markSeen := func() {
				seenParams[fullName] = true
				seenParams[baseName] = true
			}

			if _, exists := lookup(paramsToFilter); exists {
				if !inlineForm {
					// Separate-value form: consume the value so it isn't
					// mistaken for an input/output path.
					if i+1 < len(originalArgs) && !strings.HasPrefix(originalArgs[i+1], "-") {
						skipNext = true
					}
				}
				// Translated to a different name: drop here; the translated
				// form is re-added below from the params map. Inline forms
				// ("-crf=23") carry their own value and need no skipNext —
				// skipping would swallow the output path.
				markSeen()
				continue
			}

			if convertedValue, found := lookup(sameNameConverted); found {
				// Same-name param whose value was converted by a translation
				// rule (e.g., x264 "preset fast" -> NVENC "p5"): rewrite the
				// VALUE so inline and separate forms behave identically. The
				// flag name itself stays unchanged.
				markSeen()
				if inlineForm {
					eqIdx := strings.IndexByte(arg, '=')
					result = append(result, arg[:eqIdx+1]+convertedValue)
				} else {
					result = append(result, arg)
					if i+1 < len(originalArgs) && !strings.HasPrefix(originalArgs[i+1], "-") {
						result = append(result, convertedValue)
						skipNext = true
					}
				}
				continue
			}

			if _, exists := lookup(params); exists {
				// No translation rule: keep the original occurrence as-is.
				markSeen()
			}
		}

		result = append(result, arg)
	}

	// Add encoder if not already added.
	// FFmpeg positional option semantics: per-file options apply to the NEXT
	// file.  If we append -c:v after the output path, there is no next file
	// and FFmpeg falls back to its default encoder (e.g. libx264).  We must
	// insert -c:v BEFORE the output path for it to take effect.
	if !encoderAdded {
		result = insertBeforeOutputPath(result, []string{"-c:v", string(targetEncoder)})
	}

	// Add translated params (skip global init params already prepended).
	// FFmpeg per-file options apply to the NEXT file, so they must be placed
	// BEFORE the output path — appending after it silently ignores them.
	var newParams []string
	for paramName, paramValue := range params {
		if !seenParams[paramName] {
			newParams = append(newParams, fmt.Sprintf("-%s", paramName), paramValue)
		}
	}
	if len(newParams) > 0 {
		result = insertBeforeOutputPath(result, newParams)
	}

	return result
}

// booleanFFmpegFlags lists FFmpeg options that never take a separate value.
// Used by findOutputFilePos so a boolean flag followed by the output path
// (e.g., "-shortest out.mp4") is not mis-paired as flag+value.
var booleanFFmpegFlags = map[string]bool{
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
	"benchmark_all": true,
	"debug_ts":      true,
	"copyts":        true,
	"start_at_zero": true,
	"bitexact":      true,
	"re":            true,
	"stdin":         true,
	"copyinkf":      true,
}

// isBooleanFlagArg reports whether arg ("-flag" form) is a boolean FFmpeg
// flag or uses the inline "-flag=value" form; neither consumes the next
// argument.
func isBooleanFlagArg(arg string) bool {
	if !strings.HasPrefix(arg, "-") {
		return false
	}
	name := strings.TrimPrefix(arg, "-")
	if strings.IndexByte(name, '=') != -1 {
		return true // inline value form carries its own value
	}
	return booleanFFmpegFlags[name]
}

// findOutputFilePos returns the index of the output file within args: the
// last positional (no leading '-') argument that is NOT the value of a
// preceding value-taking flag. Pairing skips boolean flags and inline
// "=value" forms, which never consume the next argument.
func findOutputFilePos(args []string) int {
	outputPos := -1
	expectValue := false
	for i, arg := range args {
		if expectValue {
			expectValue = false
			continue
		}
		if strings.HasPrefix(arg, "-") && arg != "-" && !isBooleanFlagArg(arg) {
			// A value-taking flag consumes the next arg only if that arg
			// does not start with '-'.
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				expectValue = true
			}
			continue
		}
		outputPos = i
	}
	return outputPos
}

// insertBeforeOutputPath inserts toInsert before the output file in args.
// If no output file is found, the items are appended at the end.
func insertBeforeOutputPath(args []string, toInsert []string) []string {
	insertPos := findOutputFilePos(args)
	if insertPos < 0 || insertPos >= len(args) {
		return append(args, toInsert...)
	}
	result := make([]string, 0, len(args)+len(toInsert))
	result = append(result, args[:insertPos]...)
	result = append(result, toInsert...)
	result = append(result, args[insertPos:]...)
	return result
}

// hasGlobalParam checks if a global initialization parameter exists in args.
func hasGlobalParam(args []string, paramName string) bool {
	for i, arg := range args {
		// Check -param value or -param=value format
		clean := strings.TrimPrefix(arg, "-")
		if eqIdx := strings.Index(clean, "="); eqIdx >= 0 {
			if clean[:eqIdx] == paramName {
				return true
			}
		} else if clean == paramName && i+1 < len(args) {
			return true
		}
	}
	return false
}

// RewriteSync performs a synchronous rewrite with simplified input.
// This is a convenience method for simple use cases.
func (e *EngineCoordinator) RewriteSync(ctx context.Context, originalArgs []string, hwCaps *HardwareCapabilities) ([]string, error) {
	req := &EncoderRewriteRequest{
		OriginalArgs:         originalArgs,
		HardwareCapabilities: *hwCaps,
		EncoderParams:        make(map[string]string),
	}

	// Parse encoder from args
	req.SpecifiedEncoder = e.parseEncoderFromArgs(originalArgs)

	// Parse encoder params from args
	req.EncoderParams = e.parseEncoderParamsFromArgs(originalArgs)

	response, err := e.Rewrite(ctx, req)
	if err != nil {
		return nil, err
	}

	if len(response.Errors) > 0 {
		return nil, fmt.Errorf("%s: %s", response.Errors[0].Code, response.Errors[0].Message)
	}

	return response.RewrittenArgs, nil
}

// parseEncoderFromArgs extracts the specified encoder from FFmpeg arguments.
func (e *EngineCoordinator) parseEncoderFromArgs(args []string) encoder.EncoderFamily {
	for i, arg := range args {
		if arg == "-c:v" || arg == "-codec:v" || arg == "-vcodec" {
			if i+1 < len(args) {
				return encoder.EncoderFamily(args[i+1])
			}
		}
	}
	return ""
}

// parseEncoderParamsFromArgs extracts encoder parameters from FFmpeg arguments.
func (e *EngineCoordinator) parseEncoderParamsFromArgs(args []string) map[string]string {
	params := make(map[string]string)

	for i := 0; i < len(args); i++ {
		arg := args[i]

		// Skip encoder specification
		if arg == "-c:v" || arg == "-codec:v" || arg == "-vcodec" {
			i++ // Skip value too
			continue
		}

		// Handle -param value or -param=value
		if strings.HasPrefix(arg, "-") {
			paramName := strings.TrimPrefix(arg, "-")

			// Check for = syntax
			if strings.Contains(paramName, "=") {
				parts := strings.SplitN(paramName, "=", 2)
				params[parts[0]] = parts[1]
				continue
			}

			// Check if next arg is the value
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				params[paramName] = args[i+1]
				i++ // Skip value
			}
		}
	}

	return params
}
