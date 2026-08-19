package encoder

import (
	"testing"
)

func TestCodecFormatString(t *testing.T) {
	tests := []struct {
		format   CodecFormat
		expected string
	}{
		{CodecH264, "h264"},
		{CodecHEVC, "hevc"},
		{CodecVP9, "vp9"},
		{CodecAV1, "av1"},
	}

	for _, tt := range tests {
		t.Run(string(tt.format), func(t *testing.T) {
			if got := tt.format.String(); got != tt.expected {
				t.Errorf("CodecFormat.String() = %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestGPUVendorString(t *testing.T) {
	tests := []struct {
		vendor   GPUVendor
		expected string
	}{
		{GPUVendorNVIDIA, "nvidia"},
		{GPUVendorIntel, "intel"},
		{GPUVendorAMD, "amd"},
		{GPUVendorApple, "apple"},
		{GPUVendorNone, "none"},
	}

	for _, tt := range tests {
		t.Run(string(tt.vendor), func(t *testing.T) {
			if got := tt.vendor.String(); got != tt.expected {
				t.Errorf("GPUVendor.String() = %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestEncoderFamilyString(t *testing.T) {
	tests := []struct {
		encoder  EncoderFamily
		expected string
	}{
		{EncoderLibX264, "libx264"},
		{EncoderH264NVENC, "h264_nvenc"},
		{EncoderLibX265, "libx265"},
		{EncoderHEVCNVENC, "hevc_nvenc"},
	}

	for _, tt := range tests {
		t.Run(string(tt.encoder), func(t *testing.T) {
			if got := tt.encoder.String(); got != tt.expected {
				t.Errorf("EncoderFamily.String() = %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestEncoderFamilyCodecFormat(t *testing.T) {
	tests := []struct {
		encoder  EncoderFamily
		expected CodecFormat
	}{
		{EncoderLibX264, CodecH264},
		{EncoderH264NVENC, CodecH264},
		{EncoderH264QSV, CodecH264},
		{EncoderLibX265, CodecHEVC},
		{EncoderHEVCNVENC, CodecHEVC},
		{EncoderLibVPX, CodecVP9},
		{EncoderVP9VAAPI, CodecVP9},
		{EncoderLibSVTAV1, CodecAV1},
		{EncoderAV1NVENC, CodecAV1},
	}

	for _, tt := range tests {
		t.Run(string(tt.encoder), func(t *testing.T) {
			if got := tt.encoder.CodecFormat(); got != tt.expected {
				t.Errorf("EncoderFamily.CodecFormat() = %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestEncoderFamilyIsHardware(t *testing.T) {
	tests := []struct {
		encoder  EncoderFamily
		hardware bool
	}{
		{EncoderLibX264, false},
		{EncoderH264NVENC, true},
		{EncoderH264QSV, true},
		{EncoderH264VAAPI, true},
		{EncoderLibX265, false},
		{EncoderHEVCNVENC, true},
		{EncoderLibVPX, false},
		{EncoderVP9NVENC, true},
		{EncoderLibSVTAV1, false},
		{EncoderAV1NVENC, true},
	}

	for _, tt := range tests {
		t.Run(string(tt.encoder), func(t *testing.T) {
			if got := tt.encoder.IsHardware(); got != tt.hardware {
				t.Errorf("EncoderFamily.IsHardware() = %v, want %v", got, tt.hardware)
			}
		})
	}
}

func TestEncoderFamilyGPUVendor(t *testing.T) {
	tests := []struct {
		encoder  EncoderFamily
		expected GPUVendor
	}{
		{EncoderLibX264, GPUVendorNone},
		{EncoderH264NVENC, GPUVendorNVIDIA},
		{EncoderH264QSV, GPUVendorIntel},
		{EncoderH264VAAPI, GPUVendorAMD},
		{EncoderH264VT, GPUVendorApple},
		{EncoderH264AMF, GPUVendorAMD},
		{EncoderHEVCNVENC, GPUVendorNVIDIA},
		{EncoderHEVCQSV, GPUVendorIntel},
	}

	for _, tt := range tests {
		t.Run(string(tt.encoder), func(t *testing.T) {
			if got := tt.encoder.GPUVendor(); got != tt.expected {
				t.Errorf("EncoderFamily.GPUVendor() = %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestParameterRuleConvert(t *testing.T) {
	// Test with no converter
	rule := ParameterRule{
		SourceParam: "crf",
		TargetParam: "cq",
	}
	result, err := rule.Convert("23")
	if err != nil {
		t.Errorf("ParameterRule.Convert() error = %v", err)
	}
	if result != "23" {
		t.Errorf("ParameterRule.Convert() = %v, want %v", result, "23")
	}

	// Test with converter
	ruleWithConverter := ParameterRule{
		SourceParam: "preset",
		TargetParam: "preset",
		Converter:   x264PresetToNVENC,
	}
	result, err = ruleWithConverter.Convert("slow")
	if err != nil {
		t.Errorf("ParameterRule.Convert() error = %v", err)
	}
	if result != "p7" {
		t.Errorf("ParameterRule.Convert() = %v, want %v", result, "p7")
	}
}

func TestNewEncoderMapping(t *testing.T) {
	m := NewEncoderMapping()
	if m == nil {
		t.Fatal("NewEncoderMapping() returned nil")
	}
	if m.FamilyMappings == nil {
		t.Error("FamilyMappings not initialized")
	}
	if m.ParameterTranslations == nil {
		t.Error("ParameterTranslations not initialized")
	}
	if m.ValueConverters == nil {
		t.Error("ValueConverters not initialized")
	}
	if m.HardwareParams == nil {
		t.Error("HardwareParams not initialized")
	}
}

func TestEncoderMapping_RegisterAndGetCodecFormat(t *testing.T) {
	m := NewEncoderMapping()
	m.RegisterEncoder(EncoderLibX264, CodecH264)

	format, exists := m.GetCodecFormat(EncoderLibX264)
	if !exists {
		t.Error("GetCodecFormat() encoder not found")
	}
	if format != CodecH264 {
		t.Errorf("GetCodecFormat() = %v, want %v", format, CodecH264)
	}

	_, exists = m.GetCodecFormat(EncoderH264NVENC)
	if exists {
		t.Error("GetCodecFormat() should return false for unregistered encoder")
	}
}

func TestEncoderMapping_ParameterTranslations(t *testing.T) {
	m := NewEncoderMapping()

	rules := []ParameterRule{
		{SourceParam: "crf", TargetParam: "cq"},
		{SourceParam: "preset", TargetParam: "preset"},
	}
	m.AddParameterTranslations(EncoderLibX264, EncoderH264NVENC, rules)

	// Test GetParameterTranslation
	rule := m.GetParameterTranslation(EncoderLibX264, EncoderH264NVENC, "crf")
	if rule == nil {
		t.Error("GetParameterTranslation() returned nil")
	}
	if rule.TargetParam != "cq" {
		t.Errorf("GetParameterTranslation().TargetParam = %v, want %v", rule.TargetParam, "cq")
	}

	// Test non-existent parameter
	rule = m.GetParameterTranslation(EncoderLibX264, EncoderH264NVENC, "nonexistent")
	if rule != nil {
		t.Error("GetParameterTranslation() should return nil for non-existent parameter")
	}

	// Test GetAllParameterTranslations
	allRules := m.GetAllParameterTranslations(EncoderLibX264, EncoderH264NVENC)
	if len(allRules) != 2 {
		t.Errorf("GetAllParameterTranslations() returned %d rules, want 2", len(allRules))
	}
}

func TestEncoderMapping_HardwareParams(t *testing.T) {
	m := NewEncoderMapping()

	params := []HardwareParamRule{
		{Param: "rc", Value: "constqp"},
		{Param: "async_depth", Value: "4"},
	}
	m.AddHardwareParams(EncoderH264NVENC, GPUVendorNVIDIA, params)

	result := m.GetHardwareParams(EncoderH264NVENC, GPUVendorNVIDIA)
	if len(result) != 2 {
		t.Errorf("GetHardwareParams() returned %d params, want 2", len(result))
	}

	// Test non-existent hardware params
	result = m.GetHardwareParams(EncoderLibX264, GPUVendorNVIDIA)
	if result != nil {
		t.Error("GetHardwareParams() should return nil for non-existent params")
	}
}

func TestEncoderMapping_ValueConverters(t *testing.T) {
	m := NewEncoderMapping()

	converter := func(v string) (string, error) { return v + "_converted", nil }
	m.RegisterValueConverter("test_converter", converter)

	result := m.GetValueConversion("test_converter")
	if result == nil {
		t.Error("GetValueConversion() returned nil")
	}

	converted, err := result("test")
	if err != nil {
		t.Errorf("Converter error = %v", err)
	}
	if converted != "test_converted" {
		t.Errorf("Converter result = %v, want %v", converted, "test_converted")
	}
}

func TestEncoderMapping_CanTranslate(t *testing.T) {
	m := NewEncoderMapping()

	rules := []ParameterRule{
		{SourceParam: "crf", TargetParam: "cq"},
	}
	m.AddParameterTranslations(EncoderLibX264, EncoderH264NVENC, rules)

	if !m.CanTranslate(EncoderLibX264, EncoderH264NVENC) {
		t.Error("CanTranslate() should return true for registered translation")
	}

	if m.CanTranslate(EncoderLibX264, EncoderH264QSV) {
		t.Error("CanTranslate() should return false for unregistered translation")
	}
}

func TestEncoderMapping_GetEncodersForCodec(t *testing.T) {
	m := NewEncoderMapping()
	m.RegisterEncoder(EncoderLibX264, CodecH264)
	m.RegisterEncoder(EncoderH264NVENC, CodecH264)
	m.RegisterEncoder(EncoderLibX265, CodecHEVC)

	encoders := m.GetEncodersForCodec(CodecH264)
	if len(encoders) != 2 {
		t.Errorf("GetEncodersForCodec() returned %d encoders, want 2", len(encoders))
	}
}

func TestDefaultMapping(t *testing.T) {
	m := DefaultMapping()

	// Test that all encoder families are registered
	expectedEncoders := []EncoderFamily{
		EncoderLibX264, EncoderH264NVENC, EncoderH264QSV, EncoderH264VAAPI, EncoderH264AMF, EncoderH264VT,
		EncoderLibX265, EncoderHEVCNVENC, EncoderHEVCQSV, EncoderHEVCVAAPI, EncoderHEVCAMF, EncoderHEVCVT,
		EncoderLibVPX, EncoderVP9NVENC, EncoderVP9QSV, EncoderVP9VAAPI,
		EncoderLibSVTAV1, EncoderLibAOM, EncoderAV1NVENC, EncoderAV1QSV, EncoderAV1VAAPI,
	}

	for _, enc := range expectedEncoders {
		format, exists := m.GetCodecFormat(enc)
		if !exists {
			t.Errorf("Encoder %s not registered in default mapping", enc)
		}
		if format == "" {
			t.Errorf("Encoder %s has empty codec format", enc)
		}
	}

	// Test that parameter translations exist
	if !m.CanTranslate(EncoderLibX264, EncoderH264NVENC) {
		t.Error("Default mapping should have libx264 -> h264_nvenc translation")
	}

	// Test that hardware params exist
	hwParams := m.GetHardwareParams(EncoderH264NVENC, GPUVendorNVIDIA)
	if len(hwParams) == 0 {
		t.Error("Default mapping should have hardware params for h264_nvenc + nvidia")
	}

	// Test that value converters exist
	if m.GetValueConversion("x264_preset_to_nvenc") == nil {
		t.Error("Default mapping should have x264_preset_to_nvenc converter")
	}
}

func TestX264PresetToNVENC(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"ultrafast", "p1"},
		{"superfast", "p2"},
		{"veryfast", "p3"},
		{"faster", "p4"},
		{"fast", "p5"},
		{"medium", "p6"},
		{"slow", "p7"},
		{"slower", "p7"},
		{"veryslow", "p7"},
		{"unknown", "unknown"}, // pass-through for unknown
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result, err := x264PresetToNVENC(tt.input)
			if err != nil {
				t.Errorf("x264PresetToNVENC() error = %v", err)
			}
			if result != tt.expected {
				t.Errorf("x264PresetToNVENC() = %v, want %v", result, tt.expected)
			}
		})
	}
}
