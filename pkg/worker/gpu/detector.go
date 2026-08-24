// Package gpu provides cross-platform GPU device detection functionality.
package gpu

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// DeviceType represents the type of GPU acceleration.
type DeviceType string

const (
	DeviceTypeVAAPI        DeviceType = "vaapi"        // Video Acceleration API (Linux/Intel/AMD)
	DeviceTypeNVENC        DeviceType = "nvenc"        // NVIDIA Hardware Encoding
	DeviceTypeQSV          DeviceType = "qsv"          // Intel Quick Sync Video
	DeviceTypeAMF          DeviceType = "amf"          // AMD AMF (Advanced Media Framework)
	DeviceTypeVideoToolbox DeviceType = "videotoolbox" // Apple VideoToolbox (macOS)
	DeviceTypeD3D11        DeviceType = "d3d11"        // Direct3D 11 (Windows)
	DeviceTypeD3D12        DeviceType = "d3d12"        // Direct3D 12 (Windows)
)

// Device represents a detected GPU device.
type Device struct {
	Type          DeviceType `json:"type"`                     // Device type (vaapi, nvenc, etc.)
	Path          string     `json:"path"`                     // Device path (e.g., /dev/dri/renderD128)
	Name          string     `json:"name"`                     // Device name / model
	DriverVersion string     `json:"driver_version,omitempty"` // Driver version if available
	Vendor        string     `json:"vendor,omitempty"`         // Vendor name (NVIDIA, AMD, Intel, etc.)
	Accessible    bool       `json:"accessible"`               // Whether the device is accessible
	QSVHealthy    bool       `json:"qsv_healthy,omitempty"`    // Whether QSV MFX runtime is functional (Intel only)
	QSVError      string     `json:"qsv_error,omitempty"`      // Error message if QSV health check failed
	Error         string     `json:"error,omitempty"`          // Error message if detection failed
}

// Detector provides GPU device detection functionality.
type Detector struct{}

// NewDetector creates a new GPU detector.
func NewDetector() *Detector {
	return &Detector{}
}

// DetectGPUDevices detects available GPU devices on the current platform.
// Returns a list of detected devices. On unsupported platforms, returns an empty list.
func (d *Detector) DetectGPUDevices() []Device {
	switch runtime.GOOS {
	case "linux":
		return d.detectLinux()
	case "darwin":
		return d.detectMacOS()
	case "windows":
		return d.detectWindows()
	default:
		return []Device{}
	}
}

// detectLinux performs GPU detection on Linux systems.
func (d *Detector) detectLinux() []Device {
	devices := []Device{}

	// Detect VAAPI/QSV devices (Intel/AMD)
	vaapiDevs := d.detectVAAPI()
	devices = append(devices, vaapiDevs...)

	// Detect NVIDIA devices
	nvidiaDevs := d.detectNVIDIA()
	devices = append(devices, nvidiaDevs...)

	return devices
}

// detectVAAPI detects VAAPI/QSV devices (/dev/dri/renderD*).
func (d *Detector) detectVAAPI() []Device {
	devices := []Device{}

	// Check if /dev/dri directory exists
	driDir := "/dev/dri"
	if _, err := os.Stat(driDir); os.IsNotExist(err) {
		return devices
	}

	// Find renderD* devices
	renderPattern := filepath.Join(driDir, "renderD*")
	renderDevices, err := filepath.Glob(renderPattern)
	if err != nil || len(renderDevices) == 0 {
		return devices
	}

	for _, devPath := range renderDevices {
		device := Device{
			Type:       DeviceTypeVAAPI,
			Path:       devPath,
			Accessible: false,
		}

		// Check if device is accessible (read/write permissions)
		if file, err := os.OpenFile(devPath, os.O_RDONLY, 0); err == nil {
			file.Close()
			device.Accessible = true
		} else {
			device.Error = err.Error()
		}

		// Try to identify the vendor
		vendor, name, driver := d.identifyDRIDevice(devPath)
		device.Vendor = vendor
		device.Name = name
		device.DriverVersion = driver

		// Set device type based on vendor
		if vendor == "Intel" {
			device.Type = DeviceTypeQSV
			// Verify QSV MFX runtime is functional
			if healthy, errMsg := CheckQSVHealth(devPath); healthy {
				device.QSVHealthy = true
			} else {
				device.QSVHealthy = false
				device.QSVError = errMsg
			}
		} else if vendor == "AMD" {
			device.Type = DeviceTypeVAAPI
			// AMD also supports AMF
		}

		devices = append(devices, device)
	}

	return devices
}

// identifyDRIDevice identifies the vendor and name of a DRI device.
func (d *Detector) identifyDRIDevice(devPath string) (vendor, name, driver string) {
	// Try to read from sysfs to identify the device
	// /sys/class/drm/renderD128/device/vendor
	baseName := filepath.Base(devPath)
	sysPath := filepath.Join("/sys/class/drm", baseName, "device")

	// Read vendor ID
	vendorFile := filepath.Join(sysPath, "vendor")
	if data, err := os.ReadFile(vendorFile); err == nil {
		vendorID := strings.TrimSpace(string(data))
		switch vendorID {
		case "0x10de":
			vendor = "NVIDIA"
		case "0x8086":
			vendor = "Intel"
		case "0x1002":
			vendor = "AMD"
		}
	}

	// Read device name
	deviceFile := filepath.Join(sysPath, "device")
	if data, err := os.ReadFile(deviceFile); err == nil {
		name = strings.TrimSpace(string(data))
	}

	// Read driver version from sysfs
	driverFile := filepath.Join(sysPath, "driver/module/version")
	if data, err := os.ReadFile(driverFile); err == nil {
		driver = strings.TrimSpace(string(data))
	}

	// If driver version not found via sysfs, try drm module
	if driver == "" {
		driver = d.getDRMDriverVersion(devPath)
	}

	return vendor, name, driver
}

// getDRMDriverVersion attempts to get the DRM driver version.
func (d *Detector) getDRMDriverVersion(devPath string) string {
	// Try to read from /sys/kernel/debug/dri/*/name
	// This requires root or appropriate permissions
	debugPath := "/sys/kernel/debug/dri"
	if dirs, err := os.ReadDir(debugPath); err == nil {
		for _, dir := range dirs {
			nameFile := filepath.Join(debugPath, dir.Name(), "name")
			if data, err := os.ReadFile(nameFile); err == nil {
				content := string(data)
				// Format is usually: driver_name dev_id
				parts := strings.Fields(content)
				if len(parts) > 0 {
					return parts[0]
				}
			}
		}
	}
	return ""
}

// detectNVIDIA detects NVIDIA GPU devices.
func (d *Detector) detectNVIDIA() []Device {
	devices := []Device{}

	// First, check for /dev/nvidia* devices
	nvidiaPattern := "/dev/nvidia*"
	nvidiaDevices, err := filepath.Glob(nvidiaPattern)
	if err == nil && len(nvidiaDevices) > 0 {
		// Get NVIDIA device info via nvidia-smi
		name, driver := d.getNVIDIASMIInfo()

		for _, devPath := range nvidiaDevices {
			device := Device{
				Type:          DeviceTypeNVENC,
				Path:          devPath,
				Name:          name,
				DriverVersion: driver,
				Vendor:        "NVIDIA",
				Accessible:    false,
			}

			// Check if device is accessible
			if file, err := os.OpenFile(devPath, os.O_RDONLY, 0); err == nil {
				file.Close()
				device.Accessible = true
			} else {
				device.Error = err.Error()
			}

			devices = append(devices, device)
		}
	} else {
		// No /dev/nvidia* devices, but try nvidia-smi anyway
		// (might be in a container without device files)
		name, driver := d.getNVIDIASMIInfo()
		if name != "" || driver != "" {
			devices = append(devices, Device{
				Type:          DeviceTypeNVENC,
				Path:          "nvidia-smi",
				Name:          name,
				DriverVersion: driver,
				Vendor:        "NVIDIA",
				Accessible:    true,
			})
		}
	}

	return devices
}

// getNVIDIASMIInfo gets GPU info using nvidia-smi command.
func (d *Detector) getNVIDIASMIInfo() (name, driver string) {
	// Check if nvidia-smi exists
	if _, err := exec.LookPath("nvidia-smi"); err != nil {
		return "", ""
	}

	// Get GPU name
	cmd := exec.Command("nvidia-smi", "--query-gpu=name", "--format=csv,noheader,nounits")
	if output, err := cmd.Output(); err == nil {
		name = strings.TrimSpace(string(output))
		// Handle multiple GPUs - take first one
		if idx := strings.Index(name, "\n"); idx > 0 {
			name = name[:idx]
		}
	}

	// Get driver version
	cmd = exec.Command("nvidia-smi", "--query-gpu=driver_version", "--format=csv,noheader,nounits")
	if output, err := cmd.Output(); err == nil {
		driver = strings.TrimSpace(string(output))
		if idx := strings.Index(driver, "\n"); idx > 0 {
			driver = driver[:idx]
		}
	}

	return name, driver
}

// detectMacOS performs GPU detection on macOS.
func (d *Detector) detectMacOS() []Device {
	devices := []Device{}

	// Check if VideoToolbox is available by checking macOS version
	// VideoToolbox is available on OS X 10.8+ and all macOS versions
	// We can also check by trying to load the framework

	// Use system_profiler to get GPU info
	name, vendor := d.getMacOSGPUInfo()

	if name != "" {
		device := Device{
			Type:       DeviceTypeVideoToolbox,
			Path:       "VideoToolbox",
			Name:       name,
			Vendor:     vendor,
			Accessible: true, // VideoToolbox is always accessible on macOS
		}
		devices = append(devices, device)
	} else {
		// Fallback: assume VideoToolbox is available on macOS
		device := Device{
			Type:       DeviceTypeVideoToolbox,
			Path:       "VideoToolbox",
			Name:       "Apple GPU",
			Vendor:     "Apple",
			Accessible: true,
		}
		devices = append(devices, device)
	}

	return devices
}

// getMacOSGPUInfo gets GPU information using system_profiler.
func (d *Detector) getMacOSGPUInfo() (name, vendor string) {
	cmd := exec.Command("system_profiler", "SPDisplaysDataType")
	output, err := cmd.Output()
	if err != nil {
		return "", ""
	}

	// Parse the output to find GPU information
	scanner := bufio.NewScanner(strings.NewReader(string(output)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		// Look for "Chipset Model:" line
		if strings.HasPrefix(line, "Chipset Model:") {
			name = strings.TrimSpace(strings.TrimPrefix(line, "Chipset Model:"))
		}

		// Look for "Vendor:" line
		if strings.HasPrefix(line, "Vendor:") {
			vendor = strings.TrimSpace(strings.TrimPrefix(line, "Vendor:"))
		}
	}

	return name, vendor
}

// detectWindows performs GPU detection on Windows.
func (d *Detector) detectWindows() []Device {
	devices := []Device{}

	// Use PowerShell to query GPU information via WMI
	gpuInfos := d.getWindowsGPUInfo()

	for _, info := range gpuInfos {
		device := Device{
			Type:          DeviceTypeD3D11,
			Path:          info.Name,
			Name:          info.Name,
			Vendor:        info.Vendor,
			DriverVersion: info.DriverVersion,
			Accessible:    true, // Assume accessible if detected
		}

		// Check if D3D12 is also available
		// For simplicity, we report D3D11 for all GPUs
		devices = append(devices, device)
	}

	return devices
}

// windowsGPUInfo holds GPU information from Windows.
type windowsGPUInfo struct {
	Name          string
	Vendor        string
	DriverVersion string
}

// getWindowsGPUInfo gets GPU information using PowerShell/WMI.
func (d *Detector) getWindowsGPUInfo() []windowsGPUInfo {
	var infos []windowsGPUInfo

	// Use PowerShell to query Win32_VideoController
	psScript := `Get-WmiObject Win32_VideoController | Select-Object Name, VideoProcessor, DriverVersion | ConvertTo-Json -Compress`

	cmd := exec.Command("powershell", "-Command", psScript)
	output, err := cmd.Output()
	if err != nil {
		return infos
	}

	// Parse JSON output
	// PowerShell outputs either a single object or an array
	outputStr := strings.TrimSpace(string(output))
	if outputStr == "" {
		return infos
	}

	// Simple parsing - PowerShell JSON is well-formed
	// For a single object: {"Name":"...","VideoProcessor":"...","DriverVersion":"..."}
	// For multiple: [{"Name":"..."}, ...]

	// Handle both single object and array cases
	if strings.HasPrefix(outputStr, "[") {
		// Array of objects
		outputStr = strings.Trim(outputStr, "[]")
	}

	// Split by objects
	objects := strings.Split(outputStr, "},{")
	for _, obj := range objects {
		info := d.parseWindowsGPUObject(obj)
		if info.Name != "" {
			infos = append(infos, info)
		}
	}

	return infos
}

// parseWindowsGPUObject parses a PowerShell JSON object string.
func (d *Detector) parseWindowsGPUObject(obj string) windowsGPUInfo {
	info := windowsGPUInfo{}

	// Clean up the object string
	obj = strings.Trim(obj, "{}")

	// Parse key-value pairs
	parts := strings.Split(obj, ",")
	for _, part := range parts {
		kv := strings.SplitN(part, ":", 2)
		if len(kv) != 2 {
			continue
		}

		key := strings.TrimSpace(strings.Trim(kv[0], "\""))
		value := strings.TrimSpace(strings.Trim(kv[1], "\""))

		switch key {
		case "Name":
			info.Name = value
		case "VideoProcessor":
			if info.Name == "" {
				info.Name = value
			}
		case "DriverVersion":
			info.DriverVersion = value
		}
	}

	// Determine vendor from name
	info.Vendor = d.determineVendorFromName(info.Name)

	return info
}

// determineVendorFromName determines GPU vendor from the device name.
func (d *Detector) determineVendorFromName(name string) string {
	nameLower := strings.ToLower(name)

	if strings.Contains(nameLower, "nvidia") || strings.Contains(nameLower, "geforce") || strings.Contains(nameLower, "quadro") {
		return "NVIDIA"
	}
	if strings.Contains(nameLower, "amd") || strings.Contains(nameLower, "radeon") {
		return "AMD"
	}
	if strings.Contains(nameLower, "intel") {
		return "Intel"
	}
	if strings.Contains(nameLower, "apple") || strings.Contains(nameLower, "m1") || strings.Contains(nameLower, "m2") || strings.Contains(nameLower, "m3") {
		return "Apple"
	}
	if strings.Contains(nameLower, "microsoft") {
		return "Microsoft"
	}

	return "Unknown"
}

// Detect returns all detected GPU devices (convenience function).
func Detect() []Device {
	return NewDetector().DetectGPUDevices()
}
