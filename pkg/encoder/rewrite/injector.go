package rewrite

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/tsic404/rffmpeg/pkg/encoder"
)

// HardwareInjectorImpl implements the HardwareInjector interface.
// It injects hardware-specific parameters based on the target encoder type
// and available GPU devices.
type HardwareInjectorImpl struct {
	// deviceSelector selects the best GPU device for an encoder
	deviceSelector DeviceSelector
}

// DeviceSelector defines the interface for selecting GPU devices.
type DeviceSelector interface {
	// SelectDevice selects the best GPU device for an encoder.
	SelectDevice(enc encoder.EncoderFamily, hwCaps *HardwareCapabilities) *GPUDevice
}

// DefaultDeviceSelector is the default device selector implementation.
type DefaultDeviceSelector struct{}

// NewHardwareInjector creates a new hardware injector with default settings.
func NewHardwareInjector() *HardwareInjectorImpl {
	return &HardwareInjectorImpl{
		deviceSelector: &DefaultDeviceSelector{},
	}
}

// NewHardwareInjectorWithSelector creates a new hardware injector with a custom device selector.
func NewHardwareInjectorWithSelector(selector DeviceSelector) *HardwareInjectorImpl {
	return &HardwareInjectorImpl{
		deviceSelector: selector,
	}
}

// Inject injects hardware-specific parameters for the given encoder.
func (h *HardwareInjectorImpl) Inject(ctx context.Context, enc encoder.EncoderFamily, params map[string]string, hwCaps *HardwareCapabilities) (*InjectionResult, error) {
	result := &InjectionResult{
		InjectedParams: make(map[string]string),
		AllParams:      make(map[string]string),
		AuditRecords:   make([]AuditRecord, 0),
		Warnings:       make([]string, 0),
	}

	// Copy existing params to AllParams
	for k, v := range params {
		result.AllParams[k] = v
	}

	// Check if encoder is hardware-accelerated
	if !enc.IsHardware() {
		// No hardware injection needed for software encoders
		return result, nil
	}

	// Check for context cancellation
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}

	// Select a GPU device
	device := h.deviceSelector.SelectDevice(enc, hwCaps)
	if device != nil {
		result.DeviceUsed = device
	}

	// Inject encoder-specific hardware parameters
	injectedParams := h.getHardwareParamsForEncoder(enc, device, hwCaps)
	for k, v := range injectedParams {
		// Only inject if not already specified by user
		if _, exists := params[k]; !exists {
			result.InjectedParams[k] = v
			result.AllParams[k] = v

			// Record audit
			result.AuditRecords = append(result.AuditRecords, AuditRecord{
				ID:          uuid.New().String(),
				Timestamp:   time.Now(),
				Operation:   AuditOpHWParamInject,
				TargetParam: k,
				TargetValue: v,
				Reason:      fmt.Sprintf("Injected hardware parameter for %s", enc),
				Success:     true,
			})
		}
	}

	return result, nil
}

// GetRequiredParams returns the required hardware parameters for an encoder.
func (h *HardwareInjectorImpl) GetRequiredParams(enc encoder.EncoderFamily) []string {
	switch enc.GPUVendor() {
	case encoder.GPUVendorNVIDIA:
		return []string{"gpu", "delay", "zerolatency"}
	case encoder.GPUVendorIntel:
		return []string{"qsv_device", "async_depth"}
	case encoder.GPUVendorAMD:
		if enc == encoder.EncoderH264VAAPI || enc == encoder.EncoderHEVCVAAPI ||
			enc == encoder.EncoderVP9VAAPI || enc == encoder.EncoderAV1VAAPI {
			return []string{"vaapi_device"}
		}
		return []string{"rc", "quality"}
	case encoder.GPUVendorApple:
		return []string{}
	default:
		return []string{}
	}
}

// SupportsEncoder checks if hardware injection is supported for an encoder.
func (h *HardwareInjectorImpl) SupportsEncoder(enc encoder.EncoderFamily) bool {
	return enc.IsHardware()
}

// getHardwareParamsForEncoder returns hardware-specific parameters for an encoder.
func (h *HardwareInjectorImpl) getHardwareParamsForEncoder(enc encoder.EncoderFamily, device *GPUDevice, hwCaps *HardwareCapabilities) map[string]string {
	params := make(map[string]string)

	vendor := enc.GPUVendor()

	switch vendor {
	case encoder.GPUVendorNVIDIA:
		// NVIDIA NVENC parameters
		if device != nil && device.Path != "" {
			// GPU index (e.g., "0", "1")
			params["gpu"] = device.Path
		}
		// Default NVENC parameters for real-time encoding
		params["delay"] = "0"
		params["zerolatency"] = "1"

	case encoder.GPUVendorIntel:
		// Intel QSV parameters
		if device != nil && device.Path != "" {
			// init_hw_device is required to initialize the QSV runtime (MFX session).
			// Without it, ffmpeg fails with "Error creating a MFX session: -9"
			// (MFX_ERR_DEVICE_FAILED) because the Intel Media SDK / oneVPL
			// runtime has not been initialized.
			params["init_hw_device"] = fmt.Sprintf("qsv=hw,child_device=%s", device.Path)
			params["qsv_device"] = device.Path
		}
		params["async_depth"] = "1"

	case encoder.GPUVendorAMD:
		// Check if VAAPI or AMF
		if enc == encoder.EncoderH264VAAPI || enc == encoder.EncoderHEVCVAAPI ||
			enc == encoder.EncoderVP9VAAPI || enc == encoder.EncoderAV1VAAPI {
			// VAAPI parameters
			if device != nil && device.Path != "" {
				params["vaapi_device"] = device.Path
			}
			// VAAPI requires vaapi pixel format for hardware surface compatibility.
			// Without this, filter chains fail with "Impossible to convert between
			// the formats supported by the filter" when mixing HW and SW paths.
			params["pix_fmt"] = "vaapi"
		} else {
			// AMF parameters
			params["rc"] = "cqp"
			params["quality"] = "balanced"
		}

	case encoder.GPUVendorApple:
		// Apple VideoToolbox - minimal parameters needed
		// VideoToolbox uses system default device
		break
	}

	return params
}

// SelectDevice selects the best GPU device for an encoder.
func (s *DefaultDeviceSelector) SelectDevice(enc encoder.EncoderFamily, hwCaps *HardwareCapabilities) *GPUDevice {
	if hwCaps == nil || len(hwCaps.GPUDevices) == 0 {
		return nil
	}

	vendor := enc.GPUVendor()

	// Find the first accessible device matching the encoder's vendor
	for i := range hwCaps.GPUDevices {
		device := &hwCaps.GPUDevices[i]
		if !device.Accessible {
			continue
		}

		// Match device type to encoder vendor
		switch vendor {
		case encoder.GPUVendorNVIDIA:
			if device.Type == "nvenc" || device.Vendor == "NVIDIA" {
				return device
			}
		case encoder.GPUVendorIntel:
			if device.Type == "qsv" || device.Vendor == "Intel" {
				// Only use device if QSV MFX runtime is confirmed healthy.
				// If QSVHealthy is false (e.g. intel-media-sdk not installed),
				// VA-API should be preferred via the capabilities filter.
				if device.QSVHealthy {
					return device
				}
			}
		case encoder.GPUVendorAMD:
			if device.Type == "vaapi" || device.Type == "amf" || device.Vendor == "AMD" {
				return device
			}
		case encoder.GPUVendorApple:
			if device.Type == "videotoolbox" || device.Vendor == "Apple" {
				return device
			}
		}
	}

	// If no vendor match found, return the first accessible device
	for i := range hwCaps.GPUDevices {
		if hwCaps.GPUDevices[i].Accessible {
			return &hwCaps.GPUDevices[i]
		}
	}

	return nil
}

// HardwareParamMapping represents a mapping of hardware parameters.
type HardwareParamMapping struct {
	// Encoder is the encoder family this mapping applies to.
	Encoder encoder.EncoderFamily `json:"encoder"`

	// RequiredParams is the list of required parameter names.
	RequiredParams []string `json:"required_params"`

	// OptionalParams is the list of optional parameter names.
	OptionalParams []string `json:"optional_params"`

	// DefaultValues maps parameter names to their default values.
	DefaultValues map[string]string `json:"default_values"`
}

// DefaultHardwareParamMappings returns the default hardware parameter mappings.
func DefaultHardwareParamMappings() map[encoder.EncoderFamily]*HardwareParamMapping {
	return map[encoder.EncoderFamily]*HardwareParamMapping{
		encoder.EncoderH264NVENC: {
			Encoder:        encoder.EncoderH264NVENC,
			RequiredParams: []string{},
			OptionalParams: []string{"gpu", "delay", "zerolatency", "preset", "rc", "cq"},
			DefaultValues: map[string]string{
				"delay":       "0",
				"zerolatency": "1",
			},
		},
		encoder.EncoderHEVCNVENC: {
			Encoder:        encoder.EncoderHEVCNVENC,
			RequiredParams: []string{},
			OptionalParams: []string{"gpu", "delay", "zerolatency", "preset", "rc", "cq"},
			DefaultValues: map[string]string{
				"delay":       "0",
				"zerolatency": "1",
			},
		},
		encoder.EncoderH264QSV: {
			Encoder:        encoder.EncoderH264QSV,
			RequiredParams: []string{},
			OptionalParams: []string{"qsv_device", "async_depth", "preset", "global_quality"},
			DefaultValues: map[string]string{
				"async_depth": "1",
			},
		},
		encoder.EncoderHEVCQSV: {
			Encoder:        encoder.EncoderHEVCQSV,
			RequiredParams: []string{},
			OptionalParams: []string{"qsv_device", "async_depth", "preset", "global_quality"},
			DefaultValues: map[string]string{
				"async_depth": "1",
			},
		},
		encoder.EncoderH264VAAPI: {
			Encoder:        encoder.EncoderH264VAAPI,
			RequiredParams: []string{"vaapi_device"},
			OptionalParams: []string{"compression_level", "quality", "pix_fmt"},
			DefaultValues:  map[string]string{},
		},
		encoder.EncoderHEVCVAAPI: {
			Encoder:        encoder.EncoderHEVCVAAPI,
			RequiredParams: []string{"vaapi_device"},
			OptionalParams: []string{"compression_level", "quality", "pix_fmt"},
			DefaultValues:  map[string]string{},
		},
		encoder.EncoderVP9VAAPI: {
			Encoder:        encoder.EncoderVP9VAAPI,
			RequiredParams: []string{"vaapi_device"},
			OptionalParams: []string{"compression_level", "quality", "pix_fmt"},
			DefaultValues:  map[string]string{},
		},
		encoder.EncoderAV1VAAPI: {
			Encoder:        encoder.EncoderAV1VAAPI,
			RequiredParams: []string{"vaapi_device"},
			OptionalParams: []string{"compression_level", "quality", "pix_fmt"},
			DefaultValues:  map[string]string{},
		},
		encoder.EncoderH264AMF: {
			Encoder:        encoder.EncoderH264AMF,
			RequiredParams: []string{},
			OptionalParams: []string{"rc", "quality", "usage"},
			DefaultValues: map[string]string{
				"rc":      "cqp",
				"quality": "balanced",
			},
		},
		encoder.EncoderHEVCAMF: {
			Encoder:        encoder.EncoderHEVCAMF,
			RequiredParams: []string{},
			OptionalParams: []string{"rc", "quality", "usage"},
			DefaultValues: map[string]string{
				"rc":      "cqp",
				"quality": "balanced",
			},
		},
		encoder.EncoderH264VT: {
			Encoder:        encoder.EncoderH264VT,
			RequiredParams: []string{},
			OptionalParams: []string{"allow_sw", "require_sw", "realtime"},
			DefaultValues:  map[string]string{},
		},
		encoder.EncoderHEVCVT: {
			Encoder:        encoder.EncoderHEVCVT,
			RequiredParams: []string{},
			OptionalParams: []string{"allow_sw", "require_sw", "realtime"},
			DefaultValues:  map[string]string{},
		},
		encoder.EncoderVP9NVENC: {
			Encoder:        encoder.EncoderVP9NVENC,
			RequiredParams: []string{},
			OptionalParams: []string{"gpu", "delay", "zerolatency", "preset", "rc", "cq"},
			DefaultValues: map[string]string{
				"delay":       "0",
				"zerolatency": "1",
			},
		},
		encoder.EncoderAV1NVENC: {
			Encoder:        encoder.EncoderAV1NVENC,
			RequiredParams: []string{},
			OptionalParams: []string{"gpu", "delay", "zerolatency", "preset", "rc", "cq"},
			DefaultValues: map[string]string{
				"delay":       "0",
				"zerolatency": "1",
			},
		},
		encoder.EncoderVP9QSV: {
			Encoder:        encoder.EncoderVP9QSV,
			RequiredParams: []string{},
			OptionalParams: []string{"qsv_device", "async_depth", "preset", "global_quality"},
			DefaultValues: map[string]string{
				"async_depth": "1",
			},
		},
		encoder.EncoderAV1QSV: {
			Encoder:        encoder.EncoderAV1QSV,
			RequiredParams: []string{},
			OptionalParams: []string{"qsv_device", "async_depth", "preset", "global_quality"},
			DefaultValues: map[string]string{
				"async_depth": "1",
			},
		},
	}
}
