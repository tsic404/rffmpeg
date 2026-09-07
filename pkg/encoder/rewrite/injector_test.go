package rewrite

import (
	"context"
	"testing"

	"github.com/tsic404/rffmpeg/pkg/encoder"
)

func TestHardwareInjector_Inject(t *testing.T) {
	injector := NewHardwareInjector()

	tests := []struct {
		name            string
		encoder         encoder.EncoderFamily
		params          map[string]string
		hwCaps          *HardwareCapabilities
		expectInjection bool
		expectedParams  map[string]string
	}{
		{
			name:            "software encoder - no injection",
			encoder:         encoder.EncoderLibX264,
			params:          map[string]string{"crf": "23"},
			hwCaps:          &HardwareCapabilities{},
			expectInjection: false,
			expectedParams:  map[string]string{},
		},
		{
			name:    "NVENC with GPU device",
			encoder: encoder.EncoderH264NVENC,
			params:  map[string]string{"cq": "23"},
			hwCaps: &HardwareCapabilities{
				GPUDevices: []GPUDevice{
					{Type: "nvenc", Vendor: "NVIDIA", Path: "0", Accessible: true},
				},
			},
			expectInjection: true,
			expectedParams: map[string]string{
				"gpu": "0",
			},
		},
		{
			name:    "QSV with device",
			encoder: encoder.EncoderH264QSV,
			params:  map[string]string{"global_quality": "23"},
			hwCaps: &HardwareCapabilities{
				GPUDevices: []GPUDevice{
					{Type: "qsv", Vendor: "Intel", Path: "/dev/dri/renderD128", Accessible: true},
				},
			},
			expectInjection: true,
			expectedParams: map[string]string{
				"init_hw_device": "qsv=hw,child_device=/dev/dri/renderD128",
				"qsv_device":     "/dev/dri/renderD128",
				"async_depth":    "1",
			},
		},
		{
			name:            "QSV without device - no init_hw_device",
			encoder:         encoder.EncoderH264QSV,
			params:          map[string]string{"global_quality": "23"},
			hwCaps:          &HardwareCapabilities{},
			expectInjection: true,
			expectedParams: map[string]string{
				"async_depth": "1",
			},
		},
		{
			name:    "HEVC QSV with device - init_hw_device injected",
			encoder: encoder.EncoderHEVCQSV,
			params:  map[string]string{},
			hwCaps: &HardwareCapabilities{
				GPUDevices: []GPUDevice{
					{Type: "qsv", Vendor: "Intel", Path: "/dev/dri/renderD128", Accessible: true},
				},
			},
			expectInjection: true,
			expectedParams: map[string]string{
				"init_hw_device": "qsv=hw,child_device=/dev/dri/renderD128",
				"qsv_device":     "/dev/dri/renderD128",
				"async_depth":    "1",
			},
		},
		{
			name:    "VAAPI with device",
			encoder: encoder.EncoderH264VAAPI,
			params:  map[string]string{},
			hwCaps: &HardwareCapabilities{
				GPUDevices: []GPUDevice{
					{Type: "vaapi", Vendor: "AMD", Path: "/dev/dri/renderD128", Accessible: true},
				},
			},
			expectInjection: true,
			expectedParams: map[string]string{
				"vaapi_device": "/dev/dri/renderD128",
			},
		},
		{
			name:            "VideoToolbox - minimal params",
			encoder:         encoder.EncoderH264VT,
			params:          map[string]string{},
			hwCaps:          &HardwareCapabilities{},
			expectInjection: false,
			expectedParams:  map[string]string{},
		},
		{
			name:    "user param not overwritten",
			encoder: encoder.EncoderH264NVENC,
			params:  map[string]string{"gpu": "1", "cq": "23"},
			hwCaps: &HardwareCapabilities{
				GPUDevices: []GPUDevice{
					{Type: "nvenc", Vendor: "NVIDIA", Path: "0", Accessible: true},
				},
			},
			expectInjection: true,
			expectedParams:  map[string]string{
				// gpu should NOT be injected because user specified it
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := injector.Inject(context.Background(), tt.encoder, tt.params, tt.hwCaps)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if tt.expectInjection {
				if len(result.InjectedParams) == 0 {
					t.Error("expected parameter injection, got none")
					return
				}

				for k, v := range tt.expectedParams {
					if result.InjectedParams[k] != v {
						t.Errorf("expected injected param %s=%s, got %s", k, v, result.InjectedParams[k])
					}
				}
			} else {
				// Software encoders or VideoToolbox should have no or minimal injection
				if len(result.InjectedParams) > len(tt.expectedParams) {
					t.Errorf("expected no injection for software encoder, got %d params", len(result.InjectedParams))
				}
			}
		})
	}
}

func TestHardwareInjector_GetRequiredParams(t *testing.T) {
	injector := NewHardwareInjector()

	tests := []struct {
		encoder        encoder.EncoderFamily
		expectRequired bool
	}{
		{encoder.EncoderH264NVENC, true},
		{encoder.EncoderH264QSV, true},
		{encoder.EncoderH264VAAPI, true},
		{encoder.EncoderLibX264, false},
		{encoder.EncoderH264VT, false},
	}

	for _, tt := range tests {
		t.Run(string(tt.encoder), func(t *testing.T) {
			params := injector.GetRequiredParams(tt.encoder)
			if tt.expectRequired && len(params) == 0 {
				t.Error("expected required params, got none")
			}
		})
	}
}

func TestHardwareInjector_SupportsEncoder(t *testing.T) {
	injector := NewHardwareInjector()

	// Hardware encoders should be supported
	if !injector.SupportsEncoder(encoder.EncoderH264NVENC) {
		t.Error("expected NVENC to be supported")
	}

	// Software encoders should not be supported
	if injector.SupportsEncoder(encoder.EncoderLibX264) {
		t.Error("expected libx264 to not be supported")
	}
}

func TestDefaultDeviceSelector_SelectDevice(t *testing.T) {
	selector := &DefaultDeviceSelector{}

	tests := []struct {
		name           string
		encoder        encoder.EncoderFamily
		hwCaps         *HardwareCapabilities
		expectDevice   bool
		expectedVendor string
	}{
		{
			name:         "no devices",
			encoder:      encoder.EncoderH264NVENC,
			hwCaps:       &HardwareCapabilities{},
			expectDevice: false,
		},
		{
			name:    "NVIDIA encoder selects NVIDIA device",
			encoder: encoder.EncoderH264NVENC,
			hwCaps: &HardwareCapabilities{
				GPUDevices: []GPUDevice{
					{Type: "qsv", Vendor: "Intel", Accessible: true},
					{Type: "nvenc", Vendor: "NVIDIA", Path: "0", Accessible: true},
				},
			},
			expectDevice:   true,
			expectedVendor: "NVIDIA",
		},
		{
			name:    "Intel encoder selects Intel device",
			encoder: encoder.EncoderH264QSV,
			hwCaps: &HardwareCapabilities{
				GPUDevices: []GPUDevice{
					{Type: "nvenc", Vendor: "NVIDIA", Accessible: true},
					{Type: "qsv", Vendor: "Intel", Path: "/dev/dri/renderD128", Accessible: true, QSVHealthy: true},
				},
			},
			expectDevice:   true,
			expectedVendor: "Intel",
		},
		{
			name:    "skip inaccessible device",
			encoder: encoder.EncoderH264NVENC,
			hwCaps: &HardwareCapabilities{
				GPUDevices: []GPUDevice{
					{Type: "nvenc", Vendor: "NVIDIA", Accessible: false},
				},
			},
			expectDevice: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			device := selector.SelectDevice(tt.encoder, tt.hwCaps)

			if tt.expectDevice {
				if device == nil {
					t.Error("expected device, got nil")
					return
				}
				if device.Vendor != tt.expectedVendor {
					t.Errorf("expected vendor %s, got %s", tt.expectedVendor, device.Vendor)
				}
			} else {
				if device != nil {
					t.Errorf("expected no device, got %+v", device)
				}
			}
		})
	}
}
