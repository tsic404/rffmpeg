package gpu

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveModelName(t *testing.T) {
	tests := []struct {
		name     string
		vendorID string
		deviceID string
		want     string
	}{
		{"intel igpu", "0x8086", "0x9bc5", "Intel UHD Graphics 630"},
		{"intel uppercase", "0X8086", "0X9BC5", "Intel UHD Graphics 630"},
		{"intel no prefix", "8086", "9bc5", "Intel UHD Graphics 630"},
		{"amd discrete", "0x1002", "0x73ff", "AMD Radeon RX 6600/6600 XT/6600M"},
		{"unknown device", "0x8086", "0xffff", ""},
		{"unknown vendor", "0x1234", "0x9bc5", ""},
		{"empty", "", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolveModelName(tt.vendorID, tt.deviceID); got != tt.want {
				t.Errorf("resolveModelName(%q, %q) = %q, want %q", tt.vendorID, tt.deviceID, got, tt.want)
			}
		})
	}
}

// writeDRISysfs builds a fake /sys tree with a single DRI device carrying the
// given PCI vendor/device ids and returns a Detector rooted at it.
func writeDRISysfs(t *testing.T, renderName, vendorID, deviceID string) *Detector {
	t.Helper()
	devDir := filepath.Join(t.TempDir(), "class", "drm", renderName, "device")
	if err := os.MkdirAll(devDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(devDir, "vendor"), []byte(vendorID+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(devDir, "device"), []byte(deviceID+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return &Detector{sysfsRoot: filepath.Join(filepath.Dir(devDir), "..", "..", "..")}
}

func TestIdentifyDRIDeviceMapsModelAndFallsBack(t *testing.T) {
	t.Run("known intel", func(t *testing.T) {
		d := writeDRISysfs(t, "renderD128", "0x8086", "0x9bc5")
		vendor, name, _ := d.identifyDRIDevice("/dev/dri/renderD128")
		if vendor != "Intel" {
			t.Errorf("vendor = %q, want %q", vendor, "Intel")
		}
		if name != "Intel UHD Graphics 630" {
			t.Errorf("name = %q, want %q", name, "Intel UHD Graphics 630")
		}
	})

	t.Run("unknown falls back to raw id", func(t *testing.T) {
		d := writeDRISysfs(t, "renderD128", "0x8086", "0xffff")
		_, name, _ := d.identifyDRIDevice("/dev/dri/renderD128")
		if name != "0xffff" {
			t.Errorf("name = %q, want raw device id %q", name, "0xffff")
		}
	})
}
