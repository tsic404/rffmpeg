package rewrite

import "github.com/tsic404/rffmpeg/pkg/encoder"

// Encoder priority constants define the default hardware encoder preference order.
// Higher values indicate higher priority: NVENC > QSV > VAAPI > AMF > VideoToolbox > software.
const (
	// PriorityNVENC is the priority for NVIDIA NVENC encoders.
	PriorityNVENC = 60

	// PriorityQSV is the priority for Intel Quick Sync Video encoders.
	PriorityQSV = 50

	// PriorityVAAPI is the priority for VAAPI encoders.
	PriorityVAAPI = 40

	// PriorityAMF is the priority for AMD AMF encoders.
	PriorityAMF = 30

	// PriorityVideoToolbox is the priority for Apple VideoToolbox encoders.
	PriorityVideoToolbox = 20

	// PrioritySoftware is the priority for software encoders.
	PrioritySoftware = 10
)

// DefaultEncoderPriority returns the default encoder priority list ordered from
// highest to lowest priority: NVENC > QSV > VAAPI > AMF > VideoToolbox > software.
func DefaultEncoderPriority() []EncoderPriorityEntry {
	return []EncoderPriorityEntry{
		// H.264 encoders by priority
		{Encoder: encoder.EncoderH264NVENC, Priority: PriorityNVENC},
		{Encoder: encoder.EncoderH264QSV, Priority: PriorityQSV},
		{Encoder: encoder.EncoderH264VAAPI, Priority: PriorityVAAPI},
		{Encoder: encoder.EncoderH264AMF, Priority: PriorityAMF},
		{Encoder: encoder.EncoderH264VT, Priority: PriorityVideoToolbox},
		{Encoder: encoder.EncoderLibX264, Priority: PrioritySoftware},

		// HEVC encoders by priority
		{Encoder: encoder.EncoderHEVCNVENC, Priority: PriorityNVENC},
		{Encoder: encoder.EncoderHEVCQSV, Priority: PriorityQSV},
		{Encoder: encoder.EncoderHEVCVAAPI, Priority: PriorityVAAPI},
		{Encoder: encoder.EncoderHEVCAMF, Priority: PriorityAMF},
		{Encoder: encoder.EncoderHEVCVT, Priority: PriorityVideoToolbox},
		{Encoder: encoder.EncoderLibX265, Priority: PrioritySoftware},

		// VP9 encoders by priority
		{Encoder: encoder.EncoderVP9NVENC, Priority: PriorityNVENC},
		{Encoder: encoder.EncoderVP9QSV, Priority: PriorityQSV},
		{Encoder: encoder.EncoderVP9VAAPI, Priority: PriorityVAAPI},
		{Encoder: encoder.EncoderLibVPX, Priority: PrioritySoftware},

		// AV1 encoders by priority
		{Encoder: encoder.EncoderAV1NVENC, Priority: PriorityNVENC},
		{Encoder: encoder.EncoderAV1QSV, Priority: PriorityQSV},
		{Encoder: encoder.EncoderAV1VAAPI, Priority: PriorityVAAPI},
		{Encoder: encoder.EncoderLibSVTAV1, Priority: PrioritySoftware},
		{Encoder: encoder.EncoderLibAOM, Priority: PrioritySoftware},
	}
}

// GetPriorityForEncoder returns the priority value for a given encoder family
// based on its GPU vendor.
func GetPriorityForEncoder(enc encoder.EncoderFamily) int {
	if !enc.IsHardware() {
		return PrioritySoftware
	}

	switch enc.GPUVendor() {
	case encoder.GPUVendorNVIDIA:
		return PriorityNVENC
	case encoder.GPUVendorIntel:
		return PriorityQSV
	case encoder.GPUVendorAMD:
		// Distinguish between VAAPI and AMF
		switch enc {
		case encoder.EncoderH264AMF, encoder.EncoderHEVCAMF:
			return PriorityAMF
		default:
			return PriorityVAAPI
		}
	case encoder.GPUVendorApple:
		return PriorityVideoToolbox
	default:
		return PrioritySoftware
	}
}

// SoftwareEncoderForCodec returns the default software encoder for a given codec format.
func SoftwareEncoderForCodec(codec encoder.CodecFormat) encoder.EncoderFamily {
	switch codec {
	case encoder.CodecH264:
		return encoder.EncoderLibX264
	case encoder.CodecHEVC:
		return encoder.EncoderLibX265
	case encoder.CodecVP9:
		return encoder.EncoderLibVPX
	case encoder.CodecAV1:
		return encoder.EncoderLibSVTAV1
	default:
		return ""
	}
}
