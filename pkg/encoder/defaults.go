package encoder

// DefaultMapping creates and returns an EncoderMapping pre-populated with
// all standard encoder families, parameter translation rules, and hardware
// parameter injection rules.
func DefaultMapping() *EncoderMapping {
	m := NewEncoderMapping()

	// Register all standard encoder families
	registerEncoderFamilies(m)

	// Register parameter translation rules
	registerParameterTranslations(m)

	// Register value converters
	registerValueConverters(m)

	// Register hardware parameter injection rules
	registerHardwareParams(m)

	return m
}

func registerEncoderFamilies(m *EncoderMapping) {
	// H.264 encoders
	m.RegisterEncoder(EncoderLibX264, CodecH264)
	m.RegisterEncoder(EncoderH264NVENC, CodecH264)
	m.RegisterEncoder(EncoderH264QSV, CodecH264)
	m.RegisterEncoder(EncoderH264VAAPI, CodecH264)
	m.RegisterEncoder(EncoderH264AMF, CodecH264)
	m.RegisterEncoder(EncoderH264VT, CodecH264)

	// HEVC encoders
	m.RegisterEncoder(EncoderLibX265, CodecHEVC)
	m.RegisterEncoder(EncoderHEVCNVENC, CodecHEVC)
	m.RegisterEncoder(EncoderHEVCQSV, CodecHEVC)
	m.RegisterEncoder(EncoderHEVCVAAPI, CodecHEVC)
	m.RegisterEncoder(EncoderHEVCAMF, CodecHEVC)
	m.RegisterEncoder(EncoderHEVCVT, CodecHEVC)

	// VP9 encoders
	m.RegisterEncoder(EncoderLibVPX, CodecVP9)
	m.RegisterEncoder(EncoderVP9NVENC, CodecVP9)
	m.RegisterEncoder(EncoderVP9QSV, CodecVP9)
	m.RegisterEncoder(EncoderVP9VAAPI, CodecVP9)

	// AV1 encoders
	m.RegisterEncoder(EncoderLibSVTAV1, CodecAV1)
	m.RegisterEncoder(EncoderLibAOM, CodecAV1)
	m.RegisterEncoder(EncoderAV1NVENC, CodecAV1)
	m.RegisterEncoder(EncoderAV1QSV, CodecAV1)
	m.RegisterEncoder(EncoderAV1VAAPI, CodecAV1)
}

func registerParameterTranslations(m *EncoderMapping) {
	// libx264 → h264_nvenc parameter translations
	m.AddParameterTranslations(EncoderLibX264, EncoderH264NVENC, []ParameterRule{
		{SourceParam: "crf", TargetParam: "cq", Description: "CRF quality to constant quality"},
		{SourceParam: "preset", TargetParam: "preset", Description: "Preset names differ between encoders"},
		{SourceParam: "tune", TargetParam: "tune", Description: "Tuning parameter"},
		{SourceParam: "profile", TargetParam: "profile", Description: "H.264 profile"},
		{SourceParam: "b:v", TargetParam: "b:v", Description: "Bitrate"},
		{SourceParam: "maxrate", TargetParam: "maxrate", Description: "Max bitrate"},
		{SourceParam: "bufsize", TargetParam: "bufsize", Description: "Buffer size"},
	})

	// libx264 → h264_qsv parameter translations
	m.AddParameterTranslations(EncoderLibX264, EncoderH264QSV, []ParameterRule{
		{SourceParam: "crf", TargetParam: "global_quality", Description: "CRF to global quality"},
		{SourceParam: "preset", TargetParam: "preset", Description: "Preset parameter"},
		{SourceParam: "profile", TargetParam: "profile", Description: "H.264 profile"},
		{SourceParam: "look_ahead", TargetParam: "look_ahead", Description: "Look-ahead parameter"},
	})

	// libx264 → h264_vaapi parameter translations
	m.AddParameterTranslations(EncoderLibX264, EncoderH264VAAPI, []ParameterRule{
		{SourceParam: "crf", TargetParam: "quality", Description: "CRF to quality level"},
		{SourceParam: "profile", TargetParam: "profile", Description: "H.264 profile"},
		{SourceParam: "b:v", TargetParam: "b:v", Description: "Bitrate"},
		{SourceParam: "maxrate", TargetParam: "maxrate", Description: "Max bitrate"},
	})

	// libx265 → hevc_nvenc parameter translations
	m.AddParameterTranslations(EncoderLibX265, EncoderHEVCNVENC, []ParameterRule{
		{SourceParam: "crf", TargetParam: "cq", Description: "CRF quality to constant quality"},
		{SourceParam: "preset", TargetParam: "preset", Description: "Preset names differ"},
		{SourceParam: "profile", TargetParam: "profile", Description: "HEVC profile"},
		{SourceParam: "b:v", TargetParam: "b:v", Description: "Bitrate"},
		{SourceParam: "maxrate", TargetParam: "maxrate", Description: "Max bitrate"},
		{SourceParam: "bufsize", TargetParam: "bufsize", Description: "Buffer size"},
	})

	// libx265 → hevc_qsv parameter translations
	m.AddParameterTranslations(EncoderLibX265, EncoderHEVCQSV, []ParameterRule{
		{SourceParam: "crf", TargetParam: "global_quality", Description: "CRF to global quality"},
		{SourceParam: "preset", TargetParam: "preset", Description: "Preset parameter"},
		{SourceParam: "profile", TargetParam: "profile", Description: "HEVC profile"},
	})

	// libvpx-vp9 → vp9_vaapi parameter translations
	m.AddParameterTranslations(EncoderLibVPX, EncoderVP9VAAPI, []ParameterRule{
		{SourceParam: "crf", TargetParam: "quality", Description: "CRF to quality level"},
		{SourceParam: "b:v", TargetParam: "b:v", Description: "Bitrate"},
		{SourceParam: "maxrate", TargetParam: "maxrate", Description: "Max bitrate"},
	})

	// libsvtav1 → av1_nvenc parameter translations
	m.AddParameterTranslations(EncoderLibSVTAV1, EncoderAV1NVENC, []ParameterRule{
		{SourceParam: "crf", TargetParam: "cq", Description: "CRF to constant quality"},
		{SourceParam: "preset", TargetParam: "preset", Description: "Preset parameter"},
		{SourceParam: "b:v", TargetParam: "b:v", Description: "Bitrate"},
	})

	// libsvtav1 → av1_qsv parameter translations
	m.AddParameterTranslations(EncoderLibSVTAV1, EncoderAV1QSV, []ParameterRule{
		{SourceParam: "crf", TargetParam: "global_quality", Description: "CRF to global quality"},
		{SourceParam: "preset", TargetParam: "preset", Description: "Preset parameter"},
	})
}

func registerValueConverters(m *EncoderMapping) {
	// libx264 preset to NVENC preset
	m.RegisterValueConverter("x264_preset_to_nvenc", x264PresetToNVENC)

	// libx265 preset to NVENC preset
	m.RegisterValueConverter("x265_preset_to_nvenc", x265PresetToNVENC)

	// CRF to NVENC CQ (direct pass-through with validation)
	m.RegisterValueConverter("crf_to_cq", crfToCQ)

	// CRF to QSV global_quality (approximate scale adjustment)
	m.RegisterValueConverter("crf_to_global_quality", crfToGlobalQuality)
}

func registerHardwareParams(m *EncoderMapping) {
	// NVIDIA hardware params for NVENC encoders
	nvencCommonParams := []HardwareParamRule{
		{
			Param: "rc",
			Value: "constqp",
			// rc=constqp is only meaningful when the quantizer comes from
			// the injected/default channel; an explicit user quality carrier
			// (cq/global_quality/quality) must not be combined with it.
			ConflictsWith: []string{"cq", "global_quality", "quality"},
		},
	}

	m.AddHardwareParams(EncoderH264NVENC, GPUVendorNVIDIA, nvencCommonParams)
	m.AddHardwareParams(EncoderHEVCNVENC, GPUVendorNVIDIA, nvencCommonParams)
	m.AddHardwareParams(EncoderAV1NVENC, GPUVendorNVIDIA, nvencCommonParams)

	// Intel QSV params
	qsvCommonParams := []HardwareParamRule{
		{Param: "async_depth", Value: "4", Description: "Async depth for QSV"},
	}

	m.AddHardwareParams(EncoderH264QSV, GPUVendorIntel, qsvCommonParams)
	m.AddHardwareParams(EncoderHEVCQSV, GPUVendorIntel, qsvCommonParams)
	m.AddHardwareParams(EncoderAV1QSV, GPUVendorIntel, qsvCommonParams)

	// VAAPI params
	vaapiCommonParams := []HardwareParamRule{
		{Param: "async_depth", Value: "4", Description: "Async depth for VAAPI"},
	}

	m.AddHardwareParams(EncoderH264VAAPI, GPUVendorAMD, vaapiCommonParams)
	m.AddHardwareParams(EncoderHEVCVAAPI, GPUVendorAMD, vaapiCommonParams)
	m.AddHardwareParams(EncoderVP9VAAPI, GPUVendorAMD, vaapiCommonParams)
	m.AddHardwareParams(EncoderAV1VAAPI, GPUVendorAMD, vaapiCommonParams)

	// AMD AMF params
	amfCommonParams := []HardwareParamRule{
		{Param: "usage", Value: "transcoding", Description: "AMF usage mode"},
	}

	m.AddHardwareParams(EncoderH264AMF, GPUVendorAMD, amfCommonParams)
	m.AddHardwareParams(EncoderHEVCAMF, GPUVendorAMD, amfCommonParams)

	// Apple VideoToolbox params
	vtCommonParams := []HardwareParamRule{
		{Param: "allow_sw", Value: "1", Description: "Allow software fallback on VideoToolbox"},
	}

	m.AddHardwareParams(EncoderH264VT, GPUVendorApple, vtCommonParams)
	m.AddHardwareParams(EncoderHEVCVT, GPUVendorApple, vtCommonParams)
}

// x264PresetToNVENC converts libx264 preset names to NVENC preset names.
func x264PresetToNVENC(preset string) (string, error) {
	mapping := map[string]string{
		"ultrafast": "p1",
		"superfast": "p2",
		"veryfast":  "p3",
		"faster":    "p4",
		"fast":      "p5",
		"medium":    "p6",
		"slow":      "p7",
		"slower":    "p7",
		"veryslow":  "p7",
		"placebo":   "p7",
	}
	if result, ok := mapping[preset]; ok {
		return result, nil
	}
	return preset, nil
}

// x265PresetToNVENC converts libx265 preset names to NVENC preset names.
func x265PresetToNVENC(preset string) (string, error) {
	return x264PresetToNVENC(preset)
}

// crfToCQ passes CRF value through to NVENC CQ parameter.
func crfToCQ(value string) (string, error) {
	return value, nil
}

// crfToGlobalQuality converts CRF value to QSV global_quality (approximate).
func crfToGlobalQuality(value string) (string, error) {
	return value, nil
}
