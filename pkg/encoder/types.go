// Package encoder provides encoder mapping data structures for FFmpeg encoder translation.
// It supports parameter translation between different encoder families and hardware-specific
// parameter injection for NVIDIA, Intel, AMD, and Apple GPU vendors.
package encoder

// CodecFormat represents the codec format type (H.264, HEVC, VP9, AV1).
type CodecFormat string

const (
	CodecH264 CodecFormat = "h264"
	CodecHEVC CodecFormat = "hevc"
	CodecVP9  CodecFormat = "vp9"
	CodecAV1  CodecFormat = "av1"
)

// String returns the string representation of the codec format.
func (c CodecFormat) String() string {
	return string(c)
}

// GPUVendor represents the GPU hardware vendor type.
type GPUVendor string

const (
	GPUVendorNVIDIA GPUVendor = "nvidia"
	GPUVendorIntel  GPUVendor = "intel"
	GPUVendorAMD    GPUVendor = "amd"
	GPUVendorApple  GPUVendor = "apple"
	GPUVendorNone   GPUVendor = "none"
)

// String returns the string representation of the GPU vendor.
func (v GPUVendor) String() string {
	return string(v)
}

// EncoderFamily represents a specific encoder implementation.
// Each encoder belongs to a codec format and may have hardware-specific requirements.
type EncoderFamily string

const (
	// H.264 encoders
	EncoderLibX264   EncoderFamily = "libx264"
	EncoderH264NVENC EncoderFamily = "h264_nvenc"
	EncoderH264QSV   EncoderFamily = "h264_qsv"
	EncoderH264VAAPI EncoderFamily = "h264_vaapi"
	EncoderH264AMF   EncoderFamily = "h264_amf"
	EncoderH264VT    EncoderFamily = "h264_videotoolbox"

	// HEVC encoders
	EncoderLibX265   EncoderFamily = "libx265"
	EncoderHEVCNVENC EncoderFamily = "hevc_nvenc"
	EncoderHEVCQSV   EncoderFamily = "hevc_qsv"
	EncoderHEVCVAAPI EncoderFamily = "hevc_vaapi"
	EncoderHEVCAMF   EncoderFamily = "hevc_amf"
	EncoderHEVCVT    EncoderFamily = "hevc_videotoolbox"

	// VP9 encoders
	EncoderLibVPX   EncoderFamily = "libvpx-vp9"
	EncoderVP9NVENC EncoderFamily = "vp9_nvenc"
	EncoderVP9QSV   EncoderFamily = "vp9_qsv"
	EncoderVP9VAAPI EncoderFamily = "vp9_vaapi"

	// AV1 encoders
	EncoderLibSVTAV1 EncoderFamily = "libsvtav1"
	EncoderLibAOM    EncoderFamily = "libaom-av1"
	EncoderAV1NVENC  EncoderFamily = "av1_nvenc"
	EncoderAV1QSV    EncoderFamily = "av1_qsv"
	EncoderAV1VAAPI  EncoderFamily = "av1_vaapi"
)

// String returns the string representation of the encoder family.
func (e EncoderFamily) String() string {
	return string(e)
}

// CodecFormat returns the codec format for the encoder family.
func (e EncoderFamily) CodecFormat() CodecFormat {
	switch e {
	case EncoderLibX264, EncoderH264NVENC, EncoderH264QSV, EncoderH264VAAPI, EncoderH264AMF, EncoderH264VT:
		return CodecH264
	case EncoderLibX265, EncoderHEVCNVENC, EncoderHEVCQSV, EncoderHEVCVAAPI, EncoderHEVCAMF, EncoderHEVCVT:
		return CodecHEVC
	case EncoderLibVPX, EncoderVP9NVENC, EncoderVP9QSV, EncoderVP9VAAPI:
		return CodecVP9
	case EncoderLibSVTAV1, EncoderLibAOM, EncoderAV1NVENC, EncoderAV1QSV, EncoderAV1VAAPI:
		return CodecAV1
	default:
		return ""
	}
}

// IsHardware returns true if the encoder is hardware-accelerated.
func (e EncoderFamily) IsHardware() bool {
	switch e {
	case EncoderH264NVENC, EncoderH264QSV, EncoderH264VAAPI, EncoderH264AMF, EncoderH264VT,
		EncoderHEVCNVENC, EncoderHEVCQSV, EncoderHEVCVAAPI, EncoderHEVCAMF, EncoderHEVCVT,
		EncoderVP9NVENC, EncoderVP9QSV, EncoderVP9VAAPI,
		EncoderAV1NVENC, EncoderAV1QSV, EncoderAV1VAAPI:
		return true
	default:
		return false
	}
}

// GPUVendor returns the GPU vendor for hardware-accelerated encoders.
func (e EncoderFamily) GPUVendor() GPUVendor {
	switch e {
	case EncoderH264NVENC, EncoderHEVCNVENC, EncoderVP9NVENC, EncoderAV1NVENC:
		return GPUVendorNVIDIA
	case EncoderH264QSV, EncoderHEVCQSV, EncoderVP9QSV, EncoderAV1QSV:
		return GPUVendorIntel
	case EncoderH264VAAPI, EncoderHEVCVAAPI, EncoderVP9VAAPI, EncoderAV1VAAPI:
		return GPUVendorAMD
	case EncoderH264VT, EncoderHEVCVT:
		return GPUVendorApple
	case EncoderH264AMF, EncoderHEVCAMF:
		return GPUVendorAMD
	default:
		return GPUVendorNone
	}
}
