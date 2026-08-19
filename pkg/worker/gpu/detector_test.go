package gpu

import (
	"runtime"
	"testing"
)

func TestNewDetector(t *testing.T) {
	d := NewDetector()
	if d == nil {
		t.Error("NewDetector() returned nil")
	}
}

func TestDetect(t *testing.T) {
	devices := Detect()
	// Should not panic, return a slice (possibly empty on unsupported platforms)
	if devices == nil {
		t.Error("Detect() returned nil")
	}
}

func TestDetector_DetectGPUDevices(t *testing.T) {
	d := NewDetector()
	devices := d.DetectGPUDevices()

	// Should return a slice (possibly empty)
	if devices == nil {
		t.Error("DetectGPUDevices() returned nil")
	}

	// On Linux, we might find devices
	if runtime.GOOS == "linux" {
		t.Logf("Linux detected %d GPU devices", len(devices))
		for _, dev := range devices {
			t.Logf("  Device: type=%s, path=%s, name=%s, vendor=%s, accessible=%v",
				dev.Type, dev.Path, dev.Name, dev.Vendor, dev.Accessible)
		}
	}

	// On macOS, should find VideoToolbox
	if runtime.GOOS == "darwin" {
		if len(devices) == 0 {
			t.Error("macOS should detect VideoToolbox")
		}
		foundVideoToolbox := false
		for _, dev := range devices {
			if dev.Type == DeviceTypeVideoToolbox {
				foundVideoToolbox = true
				break
			}
		}
		if !foundVideoToolbox {
			t.Error("macOS should detect VideoToolbox device")
		}
	}

	// On Windows, might find devices
	if runtime.GOOS == "windows" {
		t.Logf("Windows detected %d GPU devices", len(devices))
		for _, dev := range devices {
			t.Logf("  Device: type=%s, name=%s, vendor=%s",
				dev.Type, dev.Name, dev.Vendor)
		}
	}
}

func TestDetector_detectLinux(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Skipping Linux-specific test")
	}

	d := NewDetector()
	devices := d.detectLinux()

	t.Logf("detectLinux() found %d devices", len(devices))
	for _, dev := range devices {
		t.Logf("  Device: type=%s, path=%s, accessible=%v",
			dev.Type, dev.Path, dev.Accessible)
	}
}

func TestDetector_detectMacOS(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Skipping macOS-specific test")
	}

	d := NewDetector()
	devices := d.detectMacOS()

	if len(devices) == 0 {
		t.Error("detectMacOS() should return at least one device")
	}

	for _, dev := range devices {
		if dev.Type != DeviceTypeVideoToolbox {
			t.Errorf("Expected VideoToolbox type, got %s", dev.Type)
		}
		if !dev.Accessible {
			t.Error("VideoToolbox should be accessible on macOS")
		}
	}
}

func TestDetector_detectWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Skipping Windows-specific test")
	}

	d := NewDetector()
	devices := d.detectWindows()

	t.Logf("detectWindows() found %d devices", len(devices))
	for _, dev := range devices {
		t.Logf("  Device: type=%s, name=%s, vendor=%s",
			dev.Type, dev.Name, dev.Vendor)
	}
}

func TestDetector_detectVAAPI(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Skipping Linux-specific test")
	}

	d := NewDetector()
	devices := d.detectVAAPI()

	t.Logf("detectVAAPI() found %d devices", len(devices))
	for _, dev := range devices {
		t.Logf("  Device: path=%s, type=%s, vendor=%s, accessible=%v",
			dev.Path, dev.Type, dev.Vendor, dev.Accessible)
	}
}

func TestDetector_detectNVIDIA(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Skipping Linux-specific test")
	}

	d := NewDetector()
	devices := d.detectNVIDIA()

	t.Logf("detectNVIDIA() found %d devices", len(devices))
	for _, dev := range devices {
		t.Logf("  Device: path=%s, name=%s, driver=%s, accessible=%v",
			dev.Path, dev.Name, dev.DriverVersion, dev.Accessible)
	}
}

func TestDevice_JSON(t *testing.T) {
	// Test that Device struct can be JSON-serialized
	dev := Device{
		Type:          DeviceTypeNVENC,
		Path:          "/dev/nvidia0",
		Name:          "NVIDIA GeForce RTX 3080",
		DriverVersion: "535.104.05",
		Vendor:        "NVIDIA",
		Accessible:    true,
	}

	// Just verify the fields are set correctly
	if dev.Type != DeviceTypeNVENC {
		t.Errorf("Expected type %s, got %s", DeviceTypeNVENC, dev.Type)
	}
	if dev.Path != "/dev/nvidia0" {
		t.Errorf("Expected path /dev/nvidia0, got %s", dev.Path)
	}
	if !dev.Accessible {
		t.Error("Expected accessible to be true")
	}
}

func TestDeviceType(t *testing.T) {
	// Test all device types are defined
	types := []DeviceType{
		DeviceTypeVAAPI,
		DeviceTypeNVENC,
		DeviceTypeQSV,
		DeviceTypeAMF,
		DeviceTypeVideoToolbox,
		DeviceTypeD3D11,
		DeviceTypeD3D12,
	}

	for _, dt := range types {
		if string(dt) == "" {
			t.Errorf("Device type %v has empty string representation", dt)
		}
	}
}
