package worker

import (
	"regexp"
	"strings"
)

// PruneRule defines a rule for pruning parameters based on error type.
type PruneRule struct {
	// ErrorType is the error type this rule applies to.
	ErrorType FFmpegErrorType `json:"error_type"`

	// ParamsToRemove is a list of parameters to remove.
	ParamsToRemove []string `json:"params_to_remove"`

	// PatternRules are rules based on stderr pattern matching.
	PatternRules []PatternPruneRule `json:"pattern_rules,omitempty"`

	// Description describes why these parameters should be removed.
	Description string `json:"description"`
}

// PatternPruneRule defines a rule for pruning based on stderr patterns.
type PatternPruneRule struct {
	// Pattern is the regex pattern to match in stderr.
	Pattern string `json:"pattern"`

	// ParamsToRemove is the list of parameters to remove if the pattern matches.
	ParamsToRemove []string `json:"params_to_remove"`

	// Description describes why these parameters should be removed.
	Description string `json:"description"`
}

// ParamPruner handles parameter pruning based on error analysis.
type ParamPruner struct {
	// rules are the pruning rules.
	rules []PruneRule

	// compiledPatternRules are compiled regex patterns.
	compiledPatternRules []compiledPatternPruneRule
}

// compiledPatternPruneRule is a compiled pattern prune rule.
type compiledPatternPruneRule struct {
	errorType      FFmpegErrorType
	pattern        *regexp.Regexp
	paramsToRemove []string
	description    string
}

// NewParamPruner creates a new parameter pruner.
func NewParamPruner() *ParamPruner {
	pruner := &ParamPruner{
		rules: DefaultPruneRules(),
	}

	// Compile pattern rules
	pruner.compiledPatternRules = make([]compiledPatternPruneRule, 0)
	for _, rule := range pruner.rules {
		for _, pr := range rule.PatternRules {
			re, err := regexp.Compile("(?i)" + pr.Pattern)
			if err == nil {
				pruner.compiledPatternRules = append(pruner.compiledPatternRules, compiledPatternPruneRule{
					errorType:      rule.ErrorType,
					pattern:        re,
					paramsToRemove: pr.ParamsToRemove,
					description:    pr.Description,
				})
			}
		}
	}

	return pruner
}

// NewParamPrunerWithRules creates a new parameter pruner with custom rules.
func NewParamPrunerWithRules(rules []PruneRule) *ParamPruner {
	pruner := &ParamPruner{
		rules: rules,
	}

	pruner.compiledPatternRules = make([]compiledPatternPruneRule, 0)
	for _, rule := range rules {
		for _, pr := range rule.PatternRules {
			re, err := regexp.Compile("(?i)" + pr.Pattern)
			if err == nil {
				pruner.compiledPatternRules = append(pruner.compiledPatternRules, compiledPatternPruneRule{
					errorType:      rule.ErrorType,
					pattern:        re,
					paramsToRemove: pr.ParamsToRemove,
					description:    pr.Description,
				})
			}
		}
	}

	return pruner
}

// DefaultPruneRules returns the default parameter pruning rules.
func DefaultPruneRules() []PruneRule {
	return []PruneRule{
		{
			ErrorType:      ErrorTypeInvalidArgument,
			Description:    "Remove potentially invalid parameters",
			ParamsToRemove: []string{
				// Common problematic parameters
			},
			PatternRules: []PatternPruneRule{
				{
					Pattern:        "hwaccel",
					ParamsToRemove: []string{"-hwaccel", "-hwaccel_device", "-hwaccel_output_format"},
					Description:    "Remove hardware acceleration parameters",
				},
				{
					Pattern:        "profile",
					ParamsToRemove: []string{"-profile", "-profile:v"},
					Description:    "Remove profile parameter",
				},
				{
					Pattern:        "level",
					ParamsToRemove: []string{"-level", "-level:v"},
					Description:    "Remove level parameter",
				},
				{
					Pattern:        "tune",
					ParamsToRemove: []string{"-tune"},
					Description:    "Remove tune parameter",
				},
				{
					Pattern:        "preset",
					ParamsToRemove: []string{"-preset"},
					Description:    "Remove preset parameter",
				},
				{
					Pattern:        "rc",
					ParamsToRemove: []string{"-rc", "-rc_lookahead"},
					Description:    "Remove rate control parameters",
				},
			},
		},
		{
			ErrorType:      ErrorTypeEncoderNotFound,
			Description:    "Remove encoder-specific parameters for fallback",
			ParamsToRemove: []string{
				// Will be determined by encoder type
			},
			PatternRules: []PatternPruneRule{
				{
					Pattern:        "nvenc",
					ParamsToRemove: []string{"-gpu", "-surfaces", "-delay", "-2pass", "-_multipass", "-rc", "-cq", "-b:v", "-maxrate", "-bufsize", "-profile:v", "-level:v", "-tier", "-preset", "-tune", "-rc-lookahead", "-spatial-aq", "-temporal-aq", "-aq-strength"},
					Description:    "Remove NVENC-specific parameters",
				},
				{
					Pattern:        "qsv",
					ParamsToRemove: []string{"-qsv_device", "-load_plugin", "-async_depth", "-max_frame_size", "-max_frame_size_i", "-max_frame_size_p", "-gpb", "-low_delay_brc", "-bb", "-gmc", "-idr_interval", "-pic_timing_sei", "-vcm", "-profile:v", "-level:v", "-tier", "-preset", "-tune"},
					Description:    "Remove QSV-specific parameters",
				},
				{
					Pattern:        "vaapi",
					ParamsToRemove: []string{"-vaapi_device", "-low_power", "-idr_interval", "-b_depth", "-async_depth", "-profile:v", "-level:v", "-tier", "-preset"},
					Description:    "Remove VAAPI-specific parameters",
				},
				{
					Pattern:        "amf",
					ParamsToRemove: []string{"-gpu", "-rc", "-enforce_hrd", "-filler_data", "-frame_skipping", "-max_au_size", "-header_spacing", "-b_ref_delta", "-intra_refresh_mb", "-cq", "-vbr", "-profile:v", "-level:v", "-tier", "-preset", "-tune"},
					Description:    "Remove AMF-specific parameters",
				},
				{
					Pattern:        "videotoolbox",
					ParamsToRemove: []string{"-profile:v", "-level:v", "-tier", "-preset", "-allow_sw", "-require_sw", "-realtime", "-frames_before", "-frames_after"},
					Description:    "Remove VideoToolbox-specific parameters",
				},
			},
		},
		{
			ErrorType:   ErrorTypeDeviceNotFound,
			Description: "Remove device-specific parameters",
			ParamsToRemove: []string{
				"-hwaccel_device", "-vaapi_device", "-qsv_device", "-gpu",
			},
			PatternRules: []PatternPruneRule{
				{
					Pattern:        "/dev/dri",
					ParamsToRemove: []string{"-vaapi_device", "-hwaccel_device"},
					Description:    "Remove DRI device parameters",
				},
				{
					Pattern:        "CUDA",
					ParamsToRemove: []string{"-gpu", "-hwaccel_device"},
					Description:    "Remove CUDA device parameters",
				},
				{
					Pattern:        "QSV",
					ParamsToRemove: []string{"-qsv_device", "-hwaccel_device"},
					Description:    "Remove QSV device parameters",
				},
			},
		},
		{
			ErrorType:   ErrorTypeUnsupportedCodec,
			Description: "Remove codec-specific parameters",
			ParamsToRemove: []string{
				"-profile", "-profile:v", "-level", "-level:v", "-tier",
			},
			PatternRules: []PatternPruneRule{
				{
					Pattern:        "profile",
					ParamsToRemove: []string{"-profile", "-profile:v"},
					Description:    "Remove profile parameter",
				},
				{
					Pattern:        "level",
					ParamsToRemove: []string{"-level", "-level:v"},
					Description:    "Remove level parameter",
				},
				{
					Pattern:        "tier",
					ParamsToRemove: []string{"-tier"},
					Description:    "Remove tier parameter",
				},
			},
		},
		{
			ErrorType:   ErrorTypeHWAccelFailed,
			Description: "Remove hardware acceleration parameters for fallback to software",
			ParamsToRemove: []string{
				"-hwaccel", "-hwaccel_device", "-hwaccel_output_format",
				"-init_hw_device",
			},
			PatternRules: []PatternPruneRule{
				{
					Pattern:        "hwaccel",
					ParamsToRemove: []string{"-hwaccel", "-hwaccel_device", "-hwaccel_output_format"},
					Description:    "Remove hardware acceleration parameters",
				},
				{
					Pattern:        "init_hw_device",
					ParamsToRemove: []string{"-init_hw_device"},
					Description:    "Remove init hardware device parameter",
				},
				{
					Pattern:        "nvenc",
					ParamsToRemove: []string{"-gpu", "-surfaces", "-delay", "-2pass", "-multipass", "-rc", "-cq", "-b:v", "-maxrate", "-bufsize", "-profile:v", "-level:v", "-tier", "-preset", "-tune", "-rc-lookahead", "-spatial-aq", "-temporal-aq", "-aq-strength"},
					Description:    "Remove NVENC-specific parameters for software fallback",
				},
				{
					Pattern:        "qsv",
					ParamsToRemove: []string{"-qsv_device", "-load_plugin", "-async_depth", "-max_frame_size", "-profile:v", "-level:v", "-tier", "-preset", "-tune"},
					Description:    "Remove QSV-specific parameters for software fallback",
				},
				{
					Pattern:        "vaapi",
					ParamsToRemove: []string{"-vaapi_device", "-low_power", "-idr_interval", "-b_depth", "-async_depth", "-profile:v", "-level:v", "-tier", "-preset"},
					Description:    "Remove VAAPI-specific parameters for software fallback",
				},
				{
					Pattern:        "amf",
					ParamsToRemove: []string{"-gpu", "-rc", "-enforce_hrd", "-filler_data", "-frame_skipping", "-profile:v", "-level:v", "-tier", "-preset", "-tune"},
					Description:    "Remove AMF-specific parameters for software fallback",
				},
				{
					Pattern:        "videotoolbox",
					ParamsToRemove: []string{"-profile:v", "-level:v", "-tier", "-preset", "-allow_sw", "-require_sw"},
					Description:    "Remove VideoToolbox-specific parameters for software fallback",
				},
			},
		},
		{
			ErrorType:      ErrorTypeMemoryAllocation,
			Description:    "Memory errors usually cannot be fixed by parameter pruning",
			ParamsToRemove: []string{},
			PatternRules:   []PatternPruneRule{},
		},
		{
			ErrorType:      ErrorTypeInputOutput,
			Description:    "I/O errors usually cannot be fixed by parameter pruning",
			ParamsToRemove: []string{},
			PatternRules:   []PatternPruneRule{},
		},
		{
			ErrorType:      ErrorTypePermissionDenied,
			Description:    "Permission errors usually cannot be fixed by parameter pruning",
			ParamsToRemove: []string{},
			PatternRules:   []PatternPruneRule{},
		},
	}
}

// GetPruneRecommendations returns a list of parameters to prune based on error type and stderr.
func (p *ParamPruner) GetPruneRecommendations(errorType FFmpegErrorType, stderr string) []string {
	recommendations := make(map[string]bool)

	// Find base rule for error type
	for _, rule := range p.rules {
		if rule.ErrorType == errorType {
			for _, param := range rule.ParamsToRemove {
				recommendations[param] = true
			}
		}
	}

	// Check pattern-based rules
	for _, compiled := range p.compiledPatternRules {
		if compiled.errorType == errorType || compiled.errorType == ErrorTypeUnknown {
			if compiled.pattern.MatchString(stderr) {
				for _, param := range compiled.paramsToRemove {
					recommendations[param] = true
				}
			}
		}
	}

	// Convert to slice
	result := make([]string, 0, len(recommendations))
	for param := range recommendations {
		result = append(result, param)
	}

	return result
}

// PruneParams removes specified parameters from the argument list.
// Returns the pruned argument list.
func (p *ParamPruner) PruneParams(args []string, paramsToRemove []string) []string {
	if len(paramsToRemove) == 0 {
		return args
	}

	// Create a set of params to remove
	removeSet := make(map[string]bool)
	for _, param := range paramsToRemove {
		removeSet[param] = true
	}

	result := make([]string, 0, len(args))
	skipNext := false

	for i, arg := range args {
		if skipNext {
			skipNext = false
			continue
		}

		// Check if this argument should be removed
		if removeSet[arg] {
			// Check if this flag takes a value (next arg is the value)
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				skipNext = true
			}
			continue
		}

		// Check for param=value format
		if strings.Contains(arg, "=") {
			parts := strings.SplitN(arg, "=", 2)
			if removeSet[parts[0]] {
				continue
			}
		}

		result = append(result, arg)
	}

	return result
}

// PruneParamsFromError prunes parameters based on error analysis.
func (p *ParamPruner) PruneParamsFromError(args []string, ffmpegErr *FFmpegError) []string {
	if ffmpegErr == nil {
		return args
	}

	recommendations := p.GetPruneRecommendations(ffmpegErr.Type, ffmpegErr.Stderr)
	return p.PruneParams(args, recommendations)
}

// PruneHardwareParams removes hardware-specific parameters for software fallback.
func (p *ParamPruner) PruneHardwareParams(args []string) []string {
	hwParams := []string{
		"-hwaccel", "-hwaccel_device", "-hwaccel_output_format",
		"-init_hw_device", "-vaapi_device", "-qsv_device", "-gpu",
		"-surfaces", "-delay", "-2pass", "-multipass", "-rc",
		"-rc_lookahead", "-rc-lookahead", "-spatial_aq", "-spatial-aq",
		"-temporal_aq", "-temporal-aq", "-aq_strength", "-aq-strength",
		"-low_power", "-low-power", "-async_depth", "-async-depth",
		"-load_plugin", "-load-plugin", "-enforce_hrd", "-enforce-hrd",
		"-filler_data", "-filler-data", "-frame_skipping", "-frame-skipping",
		"-allow_sw", "-allow-sw", "-require_sw", "-require-sw",
	}
	return p.PruneParams(args, hwParams)
}

// PruneProfileLevelParams removes profile and level parameters.
func (p *ParamPruner) PruneProfileLevelParams(args []string) []string {
	profileLevelParams := []string{
		"-profile", "-profile:v", "-level", "-level:v", "-tier",
	}
	return p.PruneParams(args, profileLevelParams)
}

// PruneAdvancedParams removes advanced encoding parameters.
func (p *ParamPruner) PruneAdvancedParams(args []string) []string {
	advancedParams := []string{
		"-preset", "-tune", "-x264-params", "-x265-params",
		"-crf", "-cq", "-qp", "-q:v",
		"-b:v", "-maxrate", "-bufsize", "-minrate",
		"-g", "-keyint_min", "-sc_threshold",
		"-bf", "-b_strategy", "-refs",
	}
	return p.PruneParams(args, advancedParams)
}
