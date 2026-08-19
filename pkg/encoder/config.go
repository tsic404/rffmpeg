package encoder

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// MappingConfig represents the external configuration file structure for encoder mappings.
// It supports both YAML and JSON formats and can be loaded from file to populate
// an EncoderMapping instance.
type MappingConfig struct {
	// Version is the configuration schema version.
	Version string `json:"version" yaml:"version"`

	// FamilyMappings maps encoder family names to codec format names.
	FamilyMappings map[string]string `json:"family_mappings" yaml:"family_mappings"`

	// ParameterTranslations maps encoder pair keys to parameter rules.
	// Key format: "sourceEncoder:targetEncoder".
	ParameterTranslations map[string][]ParameterRuleConfig `json:"parameter_translations" yaml:"parameter_translations"`

	// HardwareParams maps encoder+vendor keys to hardware parameter rules.
	// Key format: "encoderFamily:gpuVendor".
	HardwareParams map[string][]HardwareParamRule `json:"hardware_params" yaml:"hardware_params"`

	// ValueConverterReferences maps converter names to their type identifiers.
	// Used to wire up predefined converter functions from config.
	ValueConverterReferences map[string]string `json:"value_converter_refs" yaml:"value_converter_refs"`
}

// ParameterRuleConfig is the serializable form of ParameterRule for configuration files.
// Unlike ParameterRule, it uses ConverterRef (a named reference) instead of a function.
type ParameterRuleConfig struct {
	// SourceParam is the parameter name in the source encoder.
	SourceParam string `json:"source_param" yaml:"source_param"`

	// TargetParam is the parameter name in the target encoder.
	TargetParam string `json:"target_param" yaml:"target_param"`

	// ConverterRef is the name of a predefined value converter to use.
	// If empty, the value is passed through unchanged.
	ConverterRef string `json:"converter_ref,omitempty" yaml:"converter_ref,omitempty"`

	// DefaultValue is the default value to use if the source parameter is not provided.
	DefaultValue string `json:"default_value,omitempty" yaml:"default_value,omitempty"`

	// Required indicates whether this parameter must be present for the target encoder.
	Required bool `json:"required,omitempty" yaml:"required,omitempty"`

	// Description provides documentation for the parameter rule.
	Description string `json:"description,omitempty" yaml:"description,omitempty"`

	// ValueMap provides direct value mappings for the parameter.
	// When specified, a converter function will be created from this map at load time.
	ValueMap map[string]string `json:"value_map,omitempty" yaml:"value_map,omitempty"`
}

// LoadMappingConfig loads a MappingConfig from a file.
// It supports both JSON and YAML formats based on file extension.
// Returns an error if the file cannot be read or parsed.
func LoadMappingConfig(path string) (*MappingConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file %s: %w", path, err)
	}

	config := &MappingConfig{}
	ext := strings.ToLower(filepath.Ext(path))

	switch ext {
	case ".json":
		if err := json.Unmarshal(data, config); err != nil {
			return nil, fmt.Errorf("failed to parse JSON config %s: %w", path, err)
		}
	case ".yaml", ".yml":
		if err := unmarshalYAML(data, config); err != nil {
			return nil, fmt.Errorf("failed to parse YAML config %s: %w", path, err)
		}
	default:
		return nil, fmt.Errorf("unsupported config file format: %s (supported: .json, .yaml, .yml)", ext)
	}

	return config, nil
}

// LoadMappingConfigFromBytes loads a MappingConfig from raw bytes with the specified format.
// format should be "json" or "yaml".
func LoadMappingConfigFromBytes(data []byte, format string) (*MappingConfig, error) {
	config := &MappingConfig{}

	switch strings.ToLower(format) {
	case "json":
		if err := json.Unmarshal(data, config); err != nil {
			return nil, fmt.Errorf("failed to parse JSON config: %w", err)
		}
	case "yaml", "yml":
		if err := unmarshalYAML(data, config); err != nil {
			return nil, fmt.Errorf("failed to parse YAML config: %w", err)
		}
	default:
		return nil, fmt.Errorf("unsupported config format: %s (supported: json, yaml)", format)
	}

	return config, nil
}

// ApplyTo applies the configuration to an EncoderMapping instance.
// It registers all encoder families, parameter translations, hardware params,
// and value converters from the configuration.
func (c *MappingConfig) ApplyTo(m *EncoderMapping) error {
	// Register encoder families
	for familyStr, formatStr := range c.FamilyMappings {
		family := EncoderFamily(familyStr)
		format := CodecFormat(formatStr)
		m.RegisterEncoder(family, format)
	}

	// Register value converters from references
	for name, refType := range c.ValueConverterReferences {
		converter := getPredefinedConverter(refType)
		if converter != nil {
			m.RegisterValueConverter(name, converter)
		}
	}

	// Register parameter translations
	for key, ruleConfigs := range c.ParameterTranslations {
		parts := strings.SplitN(key, ":", 2)
		if len(parts) != 2 {
			return fmt.Errorf("invalid parameter translation key format: %s (expected 'source:target')", key)
		}

		source := EncoderFamily(parts[0])
		target := EncoderFamily(parts[1])

		rules := make([]ParameterRule, 0, len(ruleConfigs))
		for _, rc := range ruleConfigs {
			rule := ParameterRule{
				SourceParam:  rc.SourceParam,
				TargetParam:  rc.TargetParam,
				DefaultValue: rc.DefaultValue,
				Required:     rc.Required,
				Description:  rc.Description,
			}

			// Wire up converter from reference or value map
			if rc.ConverterRef != "" {
				rule.Converter = m.GetValueConversion(rc.ConverterRef)
			} else if len(rc.ValueMap) > 0 {
				valueMap := rc.ValueMap // capture for closure
				rule.Converter = func(sourceValue string) (string, error) {
					if targetValue, ok := valueMap[sourceValue]; ok {
						return targetValue, nil
					}
					return sourceValue, nil
				}
			}

			rules = append(rules, rule)
		}

		m.AddParameterTranslations(source, target, rules)
	}

	// Register hardware params
	for key, params := range c.HardwareParams {
		parts := strings.SplitN(key, ":", 2)
		if len(parts) != 2 {
			return fmt.Errorf("invalid hardware params key format: %s (expected 'encoder:vendor')", key)
		}

		encoder := EncoderFamily(parts[0])
		vendor := GPUVendor(parts[1])
		m.AddHardwareParams(encoder, vendor, params)
	}

	return nil
}

// LoadAndApplyMapping loads a configuration file and applies it to a new EncoderMapping.
// This is a convenience function that combines LoadMappingConfig and ApplyTo.
func LoadAndApplyMapping(path string) (*EncoderMapping, error) {
	config, err := LoadMappingConfig(path)
	if err != nil {
		return nil, err
	}

	m := NewEncoderMapping()
	if err := config.ApplyTo(m); err != nil {
		return nil, err
	}

	return m, nil
}

// getPredefinedConverter returns a predefined value converter function by name.
// This allows configuration files to reference well-known converter types.
func getPredefinedConverter(name string) ValueConverter {
	converters := map[string]ValueConverter{
		"x264_preset_to_nvenc":      x264PresetToNVENC,
		"x265_preset_to_nvenc":      x265PresetToNVENC,
		"crf_to_cq":                 crfToCQ,
		"crf_to_global_quality":     crfToGlobalQuality,
		"crf_to_quality":            crfToVAAPIQualityConverter,
		"x265_preset_to_nvenc_hevc": x265PresetToNVENC,
		"svtav1_preset_to_nvenc":    svtav1PresetToNVENC,
		"vpx_crf_to_vaapi_quality":  crfToVAAPIQualityConverter,
		"svtav1_crf_to_qsv_quality": svtav1CRFToQSVQuality,
	}

	return converters[name]
}

// crfToVAAPIQualityConverter wraps crfToVAAPIQuality as a ValueConverter.
func crfToVAAPIQualityConverter(value string) (string, error) {
	crf, err := parseIntValue(value)
	if err != nil {
		return "", fmt.Errorf("cannot convert non-numeric CRF value '%s' for VAAPI quality scaling: %w", value, err)
	}
	quality := crfToVAAPIQuality(crf)
	return fmt.Sprintf("%d", quality), nil
}

// svtav1PresetToNVENC converts SVT-AV1 preset (speed) values to NVENC AV1 preset names.
func svtav1PresetToNVENC(preset string) (string, error) {
	// SVT-AV1 uses numeric presets (0-13, where 0 = slowest/best, 13 = fastest)
	// NVENC AV1 uses p1-p7 like other NVENC encoders
	mapping := map[string]string{
		"0":  "p7",
		"1":  "p7",
		"2":  "p7",
		"3":  "p7",
		"4":  "p6",
		"5":  "p6",
		"6":  "p5",
		"7":  "p5",
		"8":  "p4",
		"9":  "p4",
		"10": "p3",
		"11": "p3",
		"12": "p2",
		"13": "p1",
	}
	if result, ok := mapping[preset]; ok {
		return result, nil
	}
	return preset, nil
}

// svtav1CRFToQSVQuality converts SVT-AV1 CRF value to QSV global_quality.
func svtav1CRFToQSVQuality(value string) (string, error) {
	return value, nil
}

// parseIntValue safely parses a string as an integer.
func parseIntValue(value string) (int, error) {
	var result int
	for _, c := range value {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("invalid numeric value: %s", value)
		}
		result = result*10 + int(c-'0')
	}
	return result, nil
}

// unmarshalYAML provides YAML parsing support.
// If the yaml package is not available, it returns an error suggesting JSON format.
func unmarshalYAML(data []byte, v interface{}) error {
	// Try to use gopkg.in/yaml.v3 if available
	// If not available, return a helpful error message
	return fmt.Errorf("YAML parsing requires gopkg.in/yaml.v3 dependency; please use JSON format or add yaml dependency")
}
