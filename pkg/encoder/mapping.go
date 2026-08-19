package encoder

import (
	"fmt"
)

// ValueConverter is a function that converts a parameter value from one encoder to another.
// It receives the source value and returns the converted value for the target encoder.
type ValueConverter func(sourceValue string) (targetValue string, err error)

// ParameterRule defines a rule for translating a parameter name and converting its value
// between different encoder implementations.
type ParameterRule struct {
	// SourceParam is the parameter name in the source encoder.
	SourceParam string `json:"source_param"`

	// TargetParam is the parameter name in the target encoder.
	TargetParam string `json:"target_param"`

	// Converter is an optional function to convert the parameter value.
	// If nil, the value is passed through unchanged.
	Converter ValueConverter `json:"-"`

	// DefaultValue is the default value to use if the source parameter is not provided.
	// This is only used when Required is true.
	DefaultValue string `json:"default_value,omitempty"`

	// Required indicates whether this parameter must be present for the target encoder.
	Required bool `json:"required,omitempty"`

	// Description provides documentation for the parameter rule.
	Description string `json:"description,omitempty"`
}

// Convert applies the parameter rule to convert a source value to the target encoder format.
// If the rule has a converter function, it is applied; otherwise, the value is passed through.
func (r *ParameterRule) Convert(sourceValue string) (string, error) {
	if r.Converter == nil {
		return sourceValue, nil
	}
	return r.Converter(sourceValue)
}

// HardwareParamRule defines a hardware-specific parameter that should be injected
// when using a particular encoder family with a specific GPU vendor.
type HardwareParamRule struct {
	// Param is the parameter name to inject.
	Param string `json:"param"`

	// Value is the parameter value to inject.
	Value string `json:"value"`

	// Condition is an optional condition for when to apply this rule.
	// If empty, the rule is always applied.
	Condition string `json:"condition,omitempty"`

	// Description provides documentation for the hardware parameter rule.
	Description string `json:"description,omitempty"`
}

// EncoderMapping is the main structure that holds all encoder mapping relationships.
// It provides methods to translate parameters between different encoder implementations
// and to retrieve hardware-specific parameters.
type EncoderMapping struct {
	// FamilyMappings maps encoder families to their codec formats.
	// Key: encoder family name, Value: codec format.
	FamilyMappings map[EncoderFamily]CodecFormat `json:"family_mappings"`

	// ParameterTranslations maps parameter translation rules between encoder pairs.
	// Key: "sourceEncoder:targetEncoder", Value: slice of parameter rules.
	ParameterTranslations map[string][]ParameterRule `json:"parameter_translations"`

	// ValueConverters maps value conversion functions by name.
	// This allows predefined converters to be referenced by name in configuration.
	ValueConverters map[string]ValueConverter `json:"-"`

	// HardwareParams maps hardware-specific parameter injection rules.
	// Key: "encoderFamily:gpuVendor", Value: slice of hardware parameter rules.
	HardwareParams map[string][]HardwareParamRule `json:"hardware_params"`
}

// NewEncoderMapping creates a new EncoderMapping with initialized maps.
func NewEncoderMapping() *EncoderMapping {
	return &EncoderMapping{
		FamilyMappings:        make(map[EncoderFamily]CodecFormat),
		ParameterTranslations: make(map[string][]ParameterRule),
		ValueConverters:       make(map[string]ValueConverter),
		HardwareParams:        make(map[string][]HardwareParamRule),
	}
}

// translationKey generates a key for the parameter translations map.
func translationKey(source, target EncoderFamily) string {
	return fmt.Sprintf("%s:%s", source, target)
}

// hardwareParamsKey generates a key for the hardware params map.
func hardwareParamsKey(encoder EncoderFamily, vendor GPUVendor) string {
	return fmt.Sprintf("%s:%s", encoder, vendor)
}

// GetParameterTranslation retrieves the parameter translation rule for a specific parameter
// when translating from sourceEncoder to targetEncoder.
// Returns nil if no translation rule exists for the parameter.
func (m *EncoderMapping) GetParameterTranslation(sourceEncoder, targetEncoder EncoderFamily, paramName string) *ParameterRule {
	key := translationKey(sourceEncoder, targetEncoder)
	rules, exists := m.ParameterTranslations[key]
	if !exists {
		return nil
	}

	for i := range rules {
		if rules[i].SourceParam == paramName {
			return &rules[i]
		}
	}

	return nil
}

// GetAllParameterTranslations retrieves all parameter translation rules
// for translating from sourceEncoder to targetEncoder.
func (m *EncoderMapping) GetAllParameterTranslations(sourceEncoder, targetEncoder EncoderFamily) []ParameterRule {
	key := translationKey(sourceEncoder, targetEncoder)
	rules, exists := m.ParameterTranslations[key]
	if !exists {
		return nil
	}
	return rules
}

// GetValueConversion retrieves a named value converter function.
// Returns nil if no converter exists with the given name.
func (m *EncoderMapping) GetValueConversion(converterName string) ValueConverter {
	return m.ValueConverters[converterName]
}

// RegisterValueConverter registers a value converter function with a given name.
func (m *EncoderMapping) RegisterValueConverter(name string, converter ValueConverter) {
	m.ValueConverters[name] = converter
}

// GetHardwareParams retrieves hardware-specific parameter injection rules
// for a specific encoder family and GPU vendor.
func (m *EncoderMapping) GetHardwareParams(encoderFamily EncoderFamily, vendor GPUVendor) []HardwareParamRule {
	key := hardwareParamsKey(encoderFamily, vendor)
	params, exists := m.HardwareParams[key]
	if !exists {
		return nil
	}
	return params
}

// AddHardwareParams adds hardware-specific parameter rules for an encoder and GPU vendor.
func (m *EncoderMapping) AddHardwareParams(encoderFamily EncoderFamily, vendor GPUVendor, params []HardwareParamRule) {
	key := hardwareParamsKey(encoderFamily, vendor)
	m.HardwareParams[key] = append(m.HardwareParams[key], params...)
}

// AddParameterTranslations adds parameter translation rules for a source-target encoder pair.
func (m *EncoderMapping) AddParameterTranslations(source, target EncoderFamily, rules []ParameterRule) {
	key := translationKey(source, target)
	m.ParameterTranslations[key] = append(m.ParameterTranslations[key], rules...)
}

// GetCodecFormat retrieves the codec format for a given encoder family.
func (m *EncoderMapping) GetCodecFormat(encoder EncoderFamily) (CodecFormat, bool) {
	format, exists := m.FamilyMappings[encoder]
	return format, exists
}

// RegisterEncoder registers an encoder family with its codec format.
func (m *EncoderMapping) RegisterEncoder(encoder EncoderFamily, format CodecFormat) {
	m.FamilyMappings[encoder] = format
}

// GetEncodersForCodec returns all registered encoder families for a given codec format.
func (m *EncoderMapping) GetEncodersForCodec(format CodecFormat) []EncoderFamily {
	var encoders []EncoderFamily
	for encoder, codec := range m.FamilyMappings {
		if codec == format {
			encoders = append(encoders, encoder)
		}
	}
	return encoders
}

// CanTranslate checks if translation rules exist between two encoders.
func (m *EncoderMapping) CanTranslate(source, target EncoderFamily) bool {
	key := translationKey(source, target)
	_, exists := m.ParameterTranslations[key]
	return exists
}
