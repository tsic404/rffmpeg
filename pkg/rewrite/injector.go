package rewrite

import (
	"fmt"
	"runtime"
	"strings"
	"time"

	"github.com/tsix404/rffmpeg/pkg/encoder"
)

// HardwareInjector defines the interface for injecting hardware-specific parameters
// into FFmpeg command arguments.
type HardwareInjector interface {
	// Inject injects hardware-specific parameters for the target encoder.
	// It takes the existing parameters and returns the injection result with audit records.
	Inject(targetEncoder encoder.EncoderFamily, existingParams map[string]string, caps *HardwareCapabilities) (*InjectionResult, error)

	// GetHWAccelArgs returns the hardware acceleration arguments for an encoder.
	// These are arguments like -hwaccel and -hwaccel_device that should be placed
	// before the input file in FFmpeg command.
	GetHWAccelArgs(targetEncoder encoder.EncoderFamily, caps *HardwareCapabilities) ([]string, error)

	// GetDevicePath returns the appropriate device path for an encoder type.
	// It considers platform-specific device naming and multi-GPU selection.
	GetDevicePath(encoderType string, caps *HardwareCapabilities, deviceIndex int) (string, error)

	// ValidateEncoderSupport checks if the encoder is supported by the available hardware.
	ValidateEncoderSupport(targetEncoder encoder.EncoderFamily, caps *HardwareCapabilities) (bool, string)
}

// HardwareInjectorImpl implements the HardwareInjector interface.
type HardwareInjectorImpl struct {
	// platform is the current operating system.
	platform string

	// injectHWAccelArgs determines if -hwaccel args should be injected.
	injectHWAccelArgs bool

	// preferUserParams when true, user-specified parameters take precedence.
	preferUserParams bool

	// defaultDeviceIndex is the default GPU device index to use.
	defaultDeviceIndex int
}

// InjectorOption is a function that configures the HardwareInjectorImpl.
type InjectorOption func(*HardwareInjectorImpl)

// WithInjectHWAccelArgs configures whether to inject -hwaccel arguments.
func WithInjectHWAccelArgs(inject bool) InjectorOption {
	return func(i *HardwareInjectorImpl) {
		i.injectHWAccelArgs = inject
	}
}

// WithPreferUserParams configures whether user parameters take precedence.
func WithPreferUserParams(prefer bool) InjectorOption {
	return func(i *HardwareInjectorImpl) {
		i.preferUserParams = prefer
	}
}

// WithDefaultDeviceIndex configures the default GPU device index.
func WithDefaultDeviceIndex(index int) InjectorOption {
	return func(i *HardwareInjectorImpl) {
		i.defaultDeviceIndex = index
	}
}

// NewHardwareInjector creates a new HardwareInjector with default settings.
func NewHardwareInjector(opts ...InjectorOption) *HardwareInjectorImpl {
	i := &HardwareInjectorImpl{
		platform:           runtime.GOOS,
		injectHWAccelArgs:  true,
		preferUserParams:   true,
		defaultDeviceIndex: 0,
	}

	for _, opt := range opts {
		opt(i)
	}

	return i
}

// Inject injects hardware-specific parameters for the target encoder.
func (i *HardwareInjectorImpl) Inject(
	targetEncoder encoder.EncoderFamily,
	existingParams map[string]string,
	caps *HardwareCapabilities,
) (*InjectionResult, error) {
	result := &InjectionResult{
		InjectedParams: make(map[string]string),
		ExistingParams: make(map[string]string),
		AuditRecords:   make([]InjectionAuditRecord, 0),
		Errors:         make([]string, 0),
		Warnings:       make([]string, 0),
	}

	// Check if encoder is hardware-accelerated
	if !targetEncoder.IsHardware() {
		result.Warnings = append(result.Warnings,
			fmt.Sprintf("encoder %s is not hardware-accelerated, no injection needed", targetEncoder))
		return result, nil
	}

	// Validate encoder support
	supported, reason := i.ValidateEncoderSupport(targetEncoder, caps)
	if !supported {
		result.Warnings = append(result.Warnings,
			fmt.Sprintf("encoder %s not supported: %s", targetEncoder, reason))
		return result, nil
	}

	// Get hardware acceleration type
	hwType := i.getHWType(targetEncoder)

	// Inject hardware acceleration parameters based on encoder type
	switch hwType {
	case "nvenc":
		i.injectNVENCParams(targetEncoder, existingParams, caps, result)
	case "qsv":
		i.injectQSVParams(targetEncoder, existingParams, caps, result)
	case "vaapi":
		i.injectVAAPIParams(targetEncoder, existingParams, caps, result)
	case "amf":
		i.injectAMFParams(targetEncoder, existingParams, caps, result)
	case "videotoolbox":
		i.injectVideoToolboxParams(targetEncoder, existingParams, caps, result)
	default:
		result.Warnings = append(result.Warnings,
			fmt.Sprintf("unknown hardware type for encoder %s", targetEncoder))
	}

	return result, nil
}

// GetHWAccelArgs returns the hardware acceleration arguments for an encoder.
func (i *HardwareInjectorImpl) GetHWAccelArgs(
	targetEncoder encoder.EncoderFamily,
	caps *HardwareCapabilities,
) ([]string, error) {
	if !targetEncoder.IsHardware() {
		return nil, nil
	}

	hwType := i.getHWType(targetEncoder)
	var args []string

	switch hwType {
	case "nvenc":
		args = i.getNVENCHWAccelArgs(caps)
	case "qsv":
		args = i.getQSVHWAccelArgs(caps)
	case "vaapi":
		args = i.getVAAPIHWAccelArgs(caps)
	case "amf":
		args = i.getAMFHWAccelArgs(caps)
	case "videotoolbox":
		args = i.getVideoToolboxHWAccelArgs(caps)
	}

	return args, nil
}

// GetDevicePath returns the appropriate device path for an encoder type.
func (i *HardwareInjectorImpl) GetDevicePath(
	encoderType string,
	caps *HardwareCapabilities,
	deviceIndex int,
) (string, error) {
	// Validate deviceIndex is non-negative
	if deviceIndex < 0 {
		return "", fmt.Errorf("invalid device index %d: must be non-negative", deviceIndex)
	}

	// Find matching GPU device
	for _, dev := range caps.GPUDevices {
		if dev.Type == encoderType && dev.Index == deviceIndex && dev.Accessible {
			return dev.Path, nil
		}
	}

	// Fallback based on encoder type and platform
	switch encoderType {
	case "nvenc":
		// NVIDIA uses device index
		return fmt.Sprintf("%d", deviceIndex), nil

	case "qsv":
		// QSV uses DRI render device
		return i.findDRIPath("Intel", deviceIndex, caps)

	case "vaapi":
		// VAAPI uses DRI render device
		return i.findDRIPath("AMD", deviceIndex, caps)

	case "amf":
		// AMF on Windows uses d3d11, on Linux uses VAAPI path
		if i.platform == "windows" {
			return "d3d11", nil
		}
		return i.findDRIPath("AMD", deviceIndex, caps)

	case "videotoolbox":
		// VideoToolbox doesn't need device path
		return "", nil
	}

	return "", fmt.Errorf("no device path found for encoder type %s", encoderType)
}

// ValidateEncoderSupport checks if the encoder is supported by the available hardware.
func (i *HardwareInjectorImpl) ValidateEncoderSupport(
	targetEncoder encoder.EncoderFamily,
	caps *HardwareCapabilities,
) (bool, string) {
	hwType := i.getHWType(targetEncoder)

	switch hwType {
	case "nvenc":
		if !caps.HasNVENC {
			return false, "NVENC not available on this system"
		}
		return i.hasEncoderDevice("nvenc", caps), ""

	case "qsv":
		if !caps.HasQSV {
			return false, "QSV not available on this system"
		}
		return i.hasEncoderDevice("qsv", caps), ""

	case "vaapi":
		if !caps.HasVAAPI {
			return false, "VAAPI not available on this system"
		}
		return i.hasEncoderDevice("vaapi", caps), ""

	case "amf":
		if !caps.HasAMF {
			return false, "AMF not available on this system"
		}
		return i.hasEncoderDevice("amf", caps), ""

	case "videotoolbox":
		if !caps.HasVideoToolbox {
			return false, "VideoToolbox not available on this system"
		}
		return true, ""

	default:
		return false, fmt.Sprintf("unknown hardware type: %s", hwType)
	}
}

// getHWType returns the hardware acceleration type for an encoder.
func (i *HardwareInjectorImpl) getHWType(enc encoder.EncoderFamily) string {
	vendor := enc.GPUVendor()
	switch vendor {
	case encoder.GPUVendorNVIDIA:
		return "nvenc"
	case encoder.GPUVendorIntel:
		return "qsv"
	case encoder.GPUVendorAMD:
		// Check if it's AMF or VAAPI based on encoder name
		if strings.Contains(string(enc), "_amf") {
			return "amf"
		}
		return "vaapi"
	case encoder.GPUVendorApple:
		return "videotoolbox"
	default:
		return ""
	}
}

// hasEncoderDevice checks if a device exists for the encoder type.
func (i *HardwareInjectorImpl) hasEncoderDevice(encType string, caps *HardwareCapabilities) bool {
	for _, dev := range caps.GPUDevices {
		if dev.Type == encType && dev.Accessible {
			// For QSV devices, also require the MFX runtime to be functional
			if encType == "qsv" && !dev.QSVHealthy {
				continue
			}
			return true
		}
	}
	return false
}

// findDRIPath finds the DRI render device path for a vendor.
func (i *HardwareInjectorImpl) findDRIPath(vendor string, index int, caps *HardwareCapabilities) (string, error) {
	// Validate index is non-negative
	if index < 0 {
		return "", fmt.Errorf("invalid device index %d: must be non-negative", index)
	}

	var candidates []GPUDevice

	for _, dev := range caps.GPUDevices {
		if (dev.Vendor == vendor || vendor == "") && dev.Accessible {
			candidates = append(candidates, dev)
		}
	}

	if len(candidates) == 0 {
		// Default fallback paths
		switch vendor {
		case "Intel":
			return "/dev/dri/renderD128", nil
		case "AMD":
			return "/dev/dri/renderD129", nil
		}
		return "", fmt.Errorf("no accessible DRI device found for vendor %s", vendor)
	}

	// Select device by index (ensuring index is within bounds)
	if index < len(candidates) {
		return candidates[index].Path, nil
	}

	// Fallback to first available device
	return candidates[0].Path, nil
}

// injectParams injects parameters with proper conflict handling and audit logging.
// This is a helper function to reduce code duplication across encoder-specific injection methods.
func (i *HardwareInjectorImpl) injectParams(
	targetEncoder encoder.EncoderFamily,
	params map[string]string,
	existingParams map[string]string,
	devicePath string,
	result *InjectionResult,
) {
	for param, value := range params {
		if i.preferUserParams {
			if val, exists := existingParams[param]; exists {
				result.ExistingParams[param] = val
				result.AuditRecords = append(result.AuditRecords, InjectionAuditRecord{
					Timestamp:          time.Now(),
					TargetEncoder:      string(targetEncoder),
					Parameter:          param,
					Value:              val,
					Source:             InjectionSourceConflict,
					ConflictResolution: "user_param_preserved",
					Platform:           i.platform,
				})
			} else {
				result.InjectedParams[param] = value
				result.AuditRecords = append(result.AuditRecords, InjectionAuditRecord{
					Timestamp:     time.Now(),
					TargetEncoder: string(targetEncoder),
					Parameter:     param,
					Value:         value,
					Source:        InjectionSourceAuto,
					DevicePath:    devicePath,
					Platform:      i.platform,
				})
			}
		} else {
			result.InjectedParams[param] = value
			result.AuditRecords = append(result.AuditRecords, InjectionAuditRecord{
				Timestamp:     time.Now(),
				TargetEncoder: string(targetEncoder),
				Parameter:     param,
				Value:         value,
				Source:        InjectionSourceAuto,
				DevicePath:    devicePath,
				Platform:      i.platform,
			})
		}
	}
}

// injectNVENCParams injects NVIDIA NVENC-specific parameters.
func (i *HardwareInjectorImpl) injectNVENCParams(
	targetEncoder encoder.EncoderFamily,
	existingParams map[string]string,
	caps *HardwareCapabilities,
	result *InjectionResult,
) {
	// Get device path
	devicePath, err := i.GetDevicePath("nvenc", caps, i.defaultDeviceIndex)
	fallbackPath := "0"
	if err != nil {
		result.Warnings = append(result.Warnings, fmt.Sprintf("failed to get device path: %v, using fallback: %s", err, fallbackPath))
		devicePath = fallbackPath
		// Record fallback usage in audit
		result.AuditRecords = append(result.AuditRecords, InjectionAuditRecord{
			Timestamp:     time.Now(),
			TargetEncoder: string(targetEncoder),
			Parameter:     "hwaccel_device",
			Value:         devicePath,
			Source:        InjectionSourceAuto,
			DevicePath:    devicePath,
			Platform:      i.platform,
			Notes:         fmt.Sprintf("fallback used due to error: %v", err),
		})
	}

	// Check for existing hwaccel_device
	if i.preferUserParams {
		if val, exists := existingParams["hwaccel_device"]; exists {
			result.ExistingParams["hwaccel_device"] = val
			result.AuditRecords = append(result.AuditRecords, InjectionAuditRecord{
				Timestamp:          time.Now(),
				TargetEncoder:      string(targetEncoder),
				Parameter:          "hwaccel_device",
				Value:              val,
				Source:             InjectionSourceConflict,
				ConflictResolution: "user_param_preserved",
				Platform:           i.platform,
			})
		} else {
			result.InjectedParams["hwaccel_device"] = devicePath
			result.AuditRecords = append(result.AuditRecords, InjectionAuditRecord{
				Timestamp:     time.Now(),
				TargetEncoder: string(targetEncoder),
				Parameter:     "hwaccel_device",
				Value:         devicePath,
				Source:        InjectionSourceAuto,
				DevicePath:    devicePath,
				Platform:      i.platform,
			})
		}
	} else {
		result.InjectedParams["hwaccel_device"] = devicePath
		result.AuditRecords = append(result.AuditRecords, InjectionAuditRecord{
			Timestamp:     time.Now(),
			TargetEncoder: string(targetEncoder),
			Parameter:     "hwaccel_device",
			Value:         devicePath,
			Source:        InjectionSourceAuto,
			DevicePath:    devicePath,
			Platform:      i.platform,
		})
	}
}

// injectQSVParams injects Intel QSV-specific parameters.
func (i *HardwareInjectorImpl) injectQSVParams(
	targetEncoder encoder.EncoderFamily,
	existingParams map[string]string,
	caps *HardwareCapabilities,
	result *InjectionResult,
) {
	devicePath, err := i.GetDevicePath("qsv", caps, i.defaultDeviceIndex)
	fallbackPath := "/dev/dri/renderD128"
	if err != nil {
		result.Warnings = append(result.Warnings, fmt.Sprintf("failed to get device path: %v, using fallback: %s", err, fallbackPath))
		devicePath = fallbackPath
	}

	// QSV parameters
	params := map[string]string{
		"hwaccel":        "qsv",
		"hwaccel_device": devicePath,
	}

	i.injectParams(targetEncoder, params, existingParams, devicePath, result)
}

// injectVAAPIParams injects VAAPI-specific parameters.
func (i *HardwareInjectorImpl) injectVAAPIParams(
	targetEncoder encoder.EncoderFamily,
	existingParams map[string]string,
	caps *HardwareCapabilities,
	result *InjectionResult,
) {
	devicePath, err := i.GetDevicePath("vaapi", caps, i.defaultDeviceIndex)
	fallbackPath := "/dev/dri/renderD128"
	if err != nil {
		result.Warnings = append(result.Warnings, fmt.Sprintf("failed to get device path: %v, using fallback: %s", err, fallbackPath))
		devicePath = fallbackPath
	}

	// VAAPI parameters
	params := map[string]string{
		"hwaccel":        "vaapi",
		"hwaccel_device": devicePath,
	}

	i.injectParams(targetEncoder, params, existingParams, devicePath, result)
}

// injectAMFParams injects AMD AMF-specific parameters.
func (i *HardwareInjectorImpl) injectAMFParams(
	targetEncoder encoder.EncoderFamily,
	existingParams map[string]string,
	caps *HardwareCapabilities,
	result *InjectionResult,
) {
	// AMF on Windows doesn't need hwaccel args
	// On Linux, AMF uses VAAPI paths
	if i.platform == "windows" {
		// Windows AMF uses D3D11 directly, no special injection needed
		result.AuditRecords = append(result.AuditRecords, InjectionAuditRecord{
			Timestamp:     time.Now(),
			TargetEncoder: string(targetEncoder),
			Parameter:     "amf",
			Value:         "d3d11",
			Source:        InjectionSourceAuto,
			Platform:      i.platform,
		})
		return
	}

	// Linux: Use VAAPI-style device path
	devicePath, err := i.GetDevicePath("amf", caps, i.defaultDeviceIndex)
	fallbackPath := "/dev/dri/renderD129"
	if err != nil {
		result.Warnings = append(result.Warnings, fmt.Sprintf("failed to get device path: %v, using fallback: %s", err, fallbackPath))
		devicePath = fallbackPath
	}

	params := map[string]string{
		"hwaccel":        "vaapi",
		"hwaccel_device": devicePath,
	}

	i.injectParams(targetEncoder, params, existingParams, devicePath, result)
}

// injectVideoToolboxParams injects Apple VideoToolbox-specific parameters.
func (i *HardwareInjectorImpl) injectVideoToolboxParams(
	targetEncoder encoder.EncoderFamily,
	existingParams map[string]string,
	caps *HardwareCapabilities,
	result *InjectionResult,
) {
	// VideoToolbox doesn't need device paths on macOS
	// Just record that we're using VideoToolbox
	result.AuditRecords = append(result.AuditRecords, InjectionAuditRecord{
		Timestamp:     time.Now(),
		TargetEncoder: string(targetEncoder),
		Parameter:     "videotoolbox",
		Value:         "auto",
		Source:        InjectionSourceAuto,
		Platform:      i.platform,
	})
}

// getNVENCHWAccelArgs returns NVENC hardware acceleration arguments.
func (i *HardwareInjectorImpl) getNVENCHWAccelArgs(caps *HardwareCapabilities) []string {
	devicePath, _ := i.GetDevicePath("nvenc", caps, i.defaultDeviceIndex)
	return []string{
		"-hwaccel", "cuda",
		"-hwaccel_device", devicePath,
	}
}

// getQSVHWAccelArgs returns QSV hardware acceleration arguments.
func (i *HardwareInjectorImpl) getQSVHWAccelArgs(caps *HardwareCapabilities) []string {
	devicePath, _ := i.GetDevicePath("qsv", caps, i.defaultDeviceIndex)
	return []string{
		"-hwaccel", "qsv",
		"-hwaccel_device", devicePath,
	}
}

// getVAAPIHWAccelArgs returns VAAPI hardware acceleration arguments.
func (i *HardwareInjectorImpl) getVAAPIHWAccelArgs(caps *HardwareCapabilities) []string {
	devicePath, _ := i.GetDevicePath("vaapi", caps, i.defaultDeviceIndex)
	return []string{
		"-hwaccel", "vaapi",
		"-hwaccel_device", devicePath,
	}
}

// getAMFHWAccelArgs returns AMF hardware acceleration arguments.
func (i *HardwareInjectorImpl) getAMFHWAccelArgs(caps *HardwareCapabilities) []string {
	if i.platform == "windows" {
		return []string{"-hwaccel", "d3d11"}
	}
	devicePath, _ := i.GetDevicePath("amf", caps, i.defaultDeviceIndex)
	return []string{
		"-hwaccel", "vaapi",
		"-hwaccel_device", devicePath,
	}
}

// getVideoToolboxHWAccelArgs returns VideoToolbox hardware acceleration arguments.
func (i *HardwareInjectorImpl) getVideoToolboxHWAccelArgs(caps *HardwareCapabilities) []string {
	return []string{"-hwaccel", "videotoolbox"}
}
