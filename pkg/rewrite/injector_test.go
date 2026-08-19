package rewrite

import (
	"testing"
	"time"

	"github.com/tsix404/rffmpeg/pkg/encoder"
)

func TestNewHardwareInjector(t *testing.T) {
	injector := NewHardwareInjector()
	if injector == nil {
		t.Fatal("NewHardwareInjector returned nil")
	}
	if !injector.injectHWAccelArgs {
		t.Error("injectHWAccelArgs should be true by default")
	}
	if !injector.preferUserParams {
		t.Error("preferUserParams should be true by default")
	}
}

func TestNewHardwareInjectorWithOptions(t *testing.T) {
	injector := NewHardwareInjector(
		WithInjectHWAccelArgs(false),
		WithPreferUserParams(false),
		WithDefaultDeviceIndex(1),
	)

	if injector.injectHWAccelArgs {
		t.Error("injectHWAccelArgs should be false")
	}
	if injector.preferUserParams {
		t.Error("preferUserParams should be false")
	}
	if injector.defaultDeviceIndex != 1 {
		t.Errorf("defaultDeviceIndex should be 1, got %d", injector.defaultDeviceIndex)
	}
}

func TestInject_SoftwareEncoder(t *testing.T) {
	injector := NewHardwareInjector()
	caps := &HardwareCapabilities{
		Platform: "linux",
	}

	result, err := injector.Inject(encoder.EncoderLibX264, nil, caps)
	if err != nil {
		t.Fatalf("Inject failed: %v", err)
	}

	// Software encoder should not have injected params
	if len(result.InjectedParams) > 0 {
		t.Errorf("Software encoder should not have injected params, got: %v", result.InjectedParams)
	}

	// Should have a warning
	if len(result.Warnings) == 0 {
		t.Error("Expected warning for software encoder")
	}
}

func TestInject_NVENC(t *testing.T) {
	injector := NewHardwareInjector()
	caps := &HardwareCapabilities{
		Platform: "linux",
		HasNVENC: true,
		GPUDevices: []GPUDevice{
			{Type: "nvenc", Path: "0", Vendor: "NVIDIA", Index: 0, Accessible: true},
		},
	}

	result, err := injector.Inject(encoder.EncoderH264NVENC, nil, caps)
	if err != nil {
		t.Fatalf("Inject failed: %v", err)
	}

	// Should have injected hwaccel_device
	if _, ok := result.InjectedParams["hwaccel_device"]; !ok {
		t.Error("Expected hwaccel_device to be injected")
	}

	// Should have audit records
	if len(result.AuditRecords) == 0 {
		t.Error("Expected audit records")
	}
}

func TestInject_QSV(t *testing.T) {
	injector := NewHardwareInjector()
	caps := &HardwareCapabilities{
		Platform: "linux",
		HasQSV:   true,
		GPUDevices: []GPUDevice{
			{Type: "qsv", Path: "/dev/dri/renderD128", Vendor: "Intel", Index: 0, Accessible: true, QSVHealthy: true},
		},
	}

	result, err := injector.Inject(encoder.EncoderH264QSV, nil, caps)
	if err != nil {
		t.Fatalf("Inject failed: %v", err)
	}

	// Should have injected hwaccel and hwaccel_device
	if result.InjectedParams["hwaccel"] != "qsv" {
		t.Errorf("Expected hwaccel=qsv, got %s", result.InjectedParams["hwaccel"])
	}
	if result.InjectedParams["hwaccel_device"] != "/dev/dri/renderD128" {
		t.Errorf("Expected hwaccel_device=/dev/dri/renderD128, got %s", result.InjectedParams["hwaccel_device"])
	}
}

func TestInject_VAAPI(t *testing.T) {
	injector := NewHardwareInjector()
	caps := &HardwareCapabilities{
		Platform: "linux",
		HasVAAPI: true,
		GPUDevices: []GPUDevice{
			{Type: "vaapi", Path: "/dev/dri/renderD129", Vendor: "AMD", Index: 0, Accessible: true},
		},
	}

	result, err := injector.Inject(encoder.EncoderH264VAAPI, nil, caps)
	if err != nil {
		t.Fatalf("Inject failed: %v", err)
	}

	// Should have injected hwaccel and hwaccel_device
	if result.InjectedParams["hwaccel"] != "vaapi" {
		t.Errorf("Expected hwaccel=vaapi, got %s", result.InjectedParams["hwaccel"])
	}
	if result.InjectedParams["hwaccel_device"] != "/dev/dri/renderD129" {
		t.Errorf("Expected hwaccel_device=/dev/dri/renderD129, got %s", result.InjectedParams["hwaccel_device"])
	}
}

func TestInject_VideoToolbox(t *testing.T) {
	injector := NewHardwareInjector()
	caps := &HardwareCapabilities{
		Platform:        "darwin",
		HasVideoToolbox: true,
		GPUDevices: []GPUDevice{
			{Type: "videotoolbox", Path: "VideoToolbox", Vendor: "Apple", Accessible: true},
		},
	}

	result, err := injector.Inject(encoder.EncoderH264VT, nil, caps)
	if err != nil {
		t.Fatalf("Inject failed: %v", err)
	}

	// VideoToolbox should have audit record
	if len(result.AuditRecords) == 0 {
		t.Error("Expected audit records for VideoToolbox")
	}
}

func TestInject_AMF_Windows(t *testing.T) {
	injector := NewHardwareInjector()
	injector.platform = "windows"

	caps := &HardwareCapabilities{
		Platform: "windows",
		HasAMF:   true,
		GPUDevices: []GPUDevice{
			{Type: "amf", Path: "d3d11", Vendor: "AMD", Accessible: true},
		},
	}

	result, err := injector.Inject(encoder.EncoderH264AMF, nil, caps)
	if err != nil {
		t.Fatalf("Inject failed: %v", err)
	}

	// Windows AMF should have audit record
	if len(result.AuditRecords) == 0 {
		t.Error("Expected audit records for AMF")
	}
}

func TestInject_ParameterConflict(t *testing.T) {
	injector := NewHardwareInjector()
	caps := &HardwareCapabilities{
		Platform: "linux",
		HasNVENC: true,
		GPUDevices: []GPUDevice{
			{Type: "nvenc", Path: "0", Vendor: "NVIDIA", Index: 0, Accessible: true},
		},
	}

	// User already specified hwaccel_device
	existingParams := map[string]string{
		"hwaccel_device": "1",
	}

	result, err := injector.Inject(encoder.EncoderH264NVENC, existingParams, caps)
	if err != nil {
		t.Fatalf("Inject failed: %v", err)
	}

	// User param should be preserved
	if result.ExistingParams["hwaccel_device"] != "1" {
		t.Errorf("User param should be preserved, got: %v", result.ExistingParams)
	}

	// Should have conflict resolution audit record
	found := false
	for _, record := range result.AuditRecords {
		if record.ConflictResolution == "user_param_preserved" {
			found = true
			break
		}
	}
	if !found {
		t.Error("Expected conflict resolution audit record")
	}
}

func TestInject_NoConflict_WhenPreferUserParamsFalse(t *testing.T) {
	injector := NewHardwareInjector(WithPreferUserParams(false))
	caps := &HardwareCapabilities{
		Platform: "linux",
		HasNVENC: true,
		GPUDevices: []GPUDevice{
			{Type: "nvenc", Path: "0", Vendor: "NVIDIA", Index: 0, Accessible: true},
		},
	}

	// User already specified hwaccel_device
	existingParams := map[string]string{
		"hwaccel_device": "1",
	}

	result, err := injector.Inject(encoder.EncoderH264NVENC, existingParams, caps)
	if err != nil {
		t.Fatalf("Inject failed: %v", err)
	}

	// When preferUserParams is false, injected param should override user param
	if result.InjectedParams["hwaccel_device"] != "0" {
		t.Errorf("Injected param should override user param, got: %v", result.InjectedParams)
	}
}

func TestGetHWAccelArgs(t *testing.T) {
	injector := NewHardwareInjector()
	caps := &HardwareCapabilities{
		Platform: "linux",
		HasNVENC: true,
		GPUDevices: []GPUDevice{
			{Type: "nvenc", Path: "0", Vendor: "NVIDIA", Index: 0, Accessible: true},
		},
	}

	args, err := injector.GetHWAccelArgs(encoder.EncoderH264NVENC, caps)
	if err != nil {
		t.Fatalf("GetHWAccelArgs failed: %v", err)
	}

	// Should return hwaccel args
	expected := []string{"-hwaccel", "cuda", "-hwaccel_device", "0"}
	if len(args) != len(expected) {
		t.Errorf("Expected %d args, got %d: %v", len(expected), len(args), args)
	}
}

func TestGetHWAccelArgs_SoftwareEncoder(t *testing.T) {
	injector := NewHardwareInjector()
	caps := &HardwareCapabilities{Platform: "linux"}

	args, err := injector.GetHWAccelArgs(encoder.EncoderLibX264, caps)
	if err != nil {
		t.Fatalf("GetHWAccelArgs failed: %v", err)
	}

	// Software encoder should return nil args
	if args != nil {
		t.Errorf("Software encoder should return nil args, got: %v", args)
	}
}

func TestGetDevicePath(t *testing.T) {
	injector := NewHardwareInjector()
	caps := &HardwareCapabilities{
		Platform: "linux",
		GPUDevices: []GPUDevice{
			{Type: "nvenc", Path: "0", Vendor: "NVIDIA", Index: 0, Accessible: true},
			{Type: "qsv", Path: "/dev/dri/renderD128", Vendor: "Intel", Index: 0, Accessible: true, QSVHealthy: true},
		},
	}

	tests := []struct {
		encType     string
		deviceIndex int
		expected    string
	}{
		{"nvenc", 0, "0"},
		{"qsv", 0, "/dev/dri/renderD128"},
	}

	for _, tt := range tests {
		path, err := injector.GetDevicePath(tt.encType, caps, tt.deviceIndex)
		if err != nil {
			t.Errorf("GetDevicePath(%s, %d) failed: %v", tt.encType, tt.deviceIndex, err)
			continue
		}
		if path != tt.expected {
			t.Errorf("GetDevicePath(%s, %d) = %s, expected %s", tt.encType, tt.deviceIndex, path, tt.expected)
		}
	}
}

func TestValidateEncoderSupport(t *testing.T) {
	injector := NewHardwareInjector()

	tests := []struct {
		name        string
		encoder     encoder.EncoderFamily
		caps        *HardwareCapabilities
		wantSupport bool
	}{
		{
			name:    "NVENC with device",
			encoder: encoder.EncoderH264NVENC,
			caps: &HardwareCapabilities{
				Platform: "linux",
				HasNVENC: true,
				GPUDevices: []GPUDevice{
					{Type: "nvenc", Path: "0", Accessible: true},
				},
			},
			wantSupport: true,
		},
		{
			name:    "NVENC without device",
			encoder: encoder.EncoderH264NVENC,
			caps: &HardwareCapabilities{
				Platform: "linux",
				HasNVENC: true,
			},
			wantSupport: false,
		},
		{
			name:    "NVENC not available",
			encoder: encoder.EncoderH264NVENC,
			caps: &HardwareCapabilities{
				Platform: "linux",
				HasNVENC: false,
			},
			wantSupport: false,
		},
		{
			name:    "VideoToolbox on macOS",
			encoder: encoder.EncoderH264VT,
			caps: &HardwareCapabilities{
				Platform:        "darwin",
				HasVideoToolbox: true,
			},
			wantSupport: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			supported, reason := injector.ValidateEncoderSupport(tt.encoder, tt.caps)
			if supported != tt.wantSupport {
				t.Errorf("ValidateEncoderSupport() = %v, want %v, reason: %s", supported, tt.wantSupport, reason)
			}
		})
	}
}

func TestScenarioTypeString(t *testing.T) {
	scenarios := []ScenarioType{
		ScenarioUnspecifiedEncoderHWAvailable,
		ScenarioUnspecifiedEncoderHWUnavailable,
		ScenarioSpecifiedEncoderSupported,
		ScenarioSpecifiedEncoderHWFallback,
		ScenarioSpecifiedEncoderSWFallback,
		SpecifiedEncoderFormatUnavailable,
	}

	for _, s := range scenarios {
		if s.String() == "" {
			t.Errorf("ScenarioType %v should have non-empty string representation", s)
		}
	}
}

func TestInjectionAuditRecord_Timestamp(t *testing.T) {
	record := InjectionAuditRecord{
		Timestamp:     time.Now(),
		TargetEncoder: "h264_nvenc",
		Parameter:     "hwaccel_device",
		Value:         "0",
		Source:        InjectionSourceAuto,
		Platform:      "linux",
	}

	if record.Timestamp.IsZero() {
		t.Error("Timestamp should not be zero")
	}
}

func TestHEVCEncoders(t *testing.T) {
	injector := NewHardwareInjector()

	tests := []struct {
		encoder encoder.EncoderFamily
		caps    *HardwareCapabilities
	}{
		{
			encoder: encoder.EncoderHEVCNVENC,
			caps: &HardwareCapabilities{
				Platform: "linux",
				HasNVENC: true,
				GPUDevices: []GPUDevice{
					{Type: "nvenc", Path: "0", Vendor: "NVIDIA", Index: 0, Accessible: true},
				},
			},
		},
		{
			encoder: encoder.EncoderHEVCQSV,
			caps: &HardwareCapabilities{
				Platform: "linux",
				HasQSV:   true,
				GPUDevices: []GPUDevice{
					{Type: "qsv", Path: "/dev/dri/renderD128", Vendor: "Intel", Index: 0, Accessible: true, QSVHealthy: true},
				},
			},
		},
		{
			encoder: encoder.EncoderHEVCVAAPI,
			caps: &HardwareCapabilities{
				Platform: "linux",
				HasVAAPI: true,
				GPUDevices: []GPUDevice{
					{Type: "vaapi", Path: "/dev/dri/renderD129", Vendor: "AMD", Index: 0, Accessible: true},
				},
			},
		},
		{
			encoder: encoder.EncoderHEVCAMF,
			caps: &HardwareCapabilities{
				Platform: "windows",
				HasAMF:   true,
				GPUDevices: []GPUDevice{
					{Type: "amf", Path: "d3d11", Vendor: "AMD", Accessible: true},
				},
			},
		},
		{
			encoder: encoder.EncoderHEVCVT,
			caps: &HardwareCapabilities{
				Platform:        "darwin",
				HasVideoToolbox: true,
				GPUDevices: []GPUDevice{
					{Type: "videotoolbox", Path: "VideoToolbox", Vendor: "Apple", Accessible: true},
				},
			},
		},
	}

	for _, tt := range tests {
		result, err := injector.Inject(tt.encoder, nil, tt.caps)
		if err != nil {
			t.Errorf("Inject(%s) failed: %v", tt.encoder, err)
			continue
		}
		// Hardware encoders should have audit records
		if tt.encoder.IsHardware() && len(result.AuditRecords) == 0 {
			t.Errorf("Expected audit records for %s", tt.encoder)
		}
	}
}

func TestAV1Encoders(t *testing.T) {
	injector := NewHardwareInjector()

	tests := []struct {
		encoder encoder.EncoderFamily
		caps    *HardwareCapabilities
	}{
		{
			encoder: encoder.EncoderAV1NVENC,
			caps: &HardwareCapabilities{
				Platform: "linux",
				HasNVENC: true,
				GPUDevices: []GPUDevice{
					{Type: "nvenc", Path: "0", Vendor: "NVIDIA", Index: 0, Accessible: true},
				},
			},
		},
		{
			encoder: encoder.EncoderAV1QSV,
			caps: &HardwareCapabilities{
				Platform: "linux",
				HasQSV:   true,
				GPUDevices: []GPUDevice{
					{Type: "qsv", Path: "/dev/dri/renderD128", Vendor: "Intel", Index: 0, Accessible: true, QSVHealthy: true},
				},
			},
		},
		{
			encoder: encoder.EncoderAV1VAAPI,
			caps: &HardwareCapabilities{
				Platform: "linux",
				HasVAAPI: true,
				GPUDevices: []GPUDevice{
					{Type: "vaapi", Path: "/dev/dri/renderD129", Vendor: "AMD", Index: 0, Accessible: true},
				},
			},
		},
	}

	for _, tt := range tests {
		result, err := injector.Inject(tt.encoder, nil, tt.caps)
		if err != nil {
			t.Errorf("Inject(%s) failed: %v", tt.encoder, err)
			continue
		}
		// Hardware encoders should have audit records
		if tt.encoder.IsHardware() && len(result.AuditRecords) == 0 {
			t.Errorf("Expected audit records for %s", tt.encoder)
		}
	}
}

func TestVP9Encoders(t *testing.T) {
	injector := NewHardwareInjector()

	tests := []struct {
		encoder encoder.EncoderFamily
		caps    *HardwareCapabilities
	}{
		{
			encoder: encoder.EncoderVP9NVENC,
			caps: &HardwareCapabilities{
				Platform: "linux",
				HasNVENC: true,
				GPUDevices: []GPUDevice{
					{Type: "nvenc", Path: "0", Vendor: "NVIDIA", Index: 0, Accessible: true},
				},
			},
		},
		{
			encoder: encoder.EncoderVP9QSV,
			caps: &HardwareCapabilities{
				Platform: "linux",
				HasQSV:   true,
				GPUDevices: []GPUDevice{
					{Type: "qsv", Path: "/dev/dri/renderD128", Vendor: "Intel", Index: 0, Accessible: true, QSVHealthy: true},
				},
			},
		},
		{
			encoder: encoder.EncoderVP9VAAPI,
			caps: &HardwareCapabilities{
				Platform: "linux",
				HasVAAPI: true,
				GPUDevices: []GPUDevice{
					{Type: "vaapi", Path: "/dev/dri/renderD129", Vendor: "AMD", Index: 0, Accessible: true},
				},
			},
		},
	}

	for _, tt := range tests {
		result, err := injector.Inject(tt.encoder, nil, tt.caps)
		if err != nil {
			t.Errorf("Inject(%s) failed: %v", tt.encoder, err)
			continue
		}
		// Hardware encoders should have audit records
		if tt.encoder.IsHardware() && len(result.AuditRecords) == 0 {
			t.Errorf("Expected audit records for %s", tt.encoder)
		}
	}
}

func TestGetDevicePath_NegativeDeviceIndex(t *testing.T) {
	injector := NewHardwareInjector()
	caps := &HardwareCapabilities{
		Platform: "linux",
		GPUDevices: []GPUDevice{
			{Type: "nvenc", Path: "0", Vendor: "NVIDIA", Index: 0, Accessible: true},
		},
	}

	_, err := injector.GetDevicePath("nvenc", caps, -1)
	if err == nil {
		t.Error("Expected error for negative device index, got nil")
	}
}

func TestFindDRIPath_NegativeIndex(t *testing.T) {
	injector := NewHardwareInjector()
	caps := &HardwareCapabilities{
		Platform: "linux",
		GPUDevices: []GPUDevice{
			{Type: "qsv", Path: "/dev/dri/renderD128", Vendor: "Intel", Index: 0, Accessible: true, QSVHealthy: true},
		},
	}

	_, err := injector.findDRIPath("Intel", -1, caps)
	if err == nil {
		t.Error("Expected error for negative index in findDRIPath, got nil")
	}
}

func TestInject_FallbackWithAuditRecord(t *testing.T) {
	injector := NewHardwareInjector(WithDefaultDeviceIndex(0))
	caps := &HardwareCapabilities{
		Platform: "linux",
		HasNVENC: true,
		GPUDevices: []GPUDevice{
			// Device exists but doesn't match the requested index
			{Type: "nvenc", Path: "1", Vendor: "NVIDIA", Index: 1, Accessible: true},
		},
	}

	result, err := injector.Inject(encoder.EncoderH264NVENC, nil, caps)
	if err != nil {
		t.Fatalf("Inject failed: %v", err)
	}

	// Should have injected hwaccel_device with fallback (index 0 string)
	if result.InjectedParams["hwaccel_device"] != "0" {
		t.Errorf("Expected fallback hwaccel_device=0, got %s", result.InjectedParams["hwaccel_device"])
	}

	// Should have audit records
	if len(result.AuditRecords) == 0 {
		t.Error("Expected audit records for fallback injection")
	}
}

func TestGetDevicePath_OutOfBoundsFallback(t *testing.T) {
	injector := NewHardwareInjector()
	caps := &HardwareCapabilities{
		Platform: "linux",
		GPUDevices: []GPUDevice{
			{Type: "qsv", Path: "/dev/dri/renderD128", Vendor: "Intel", Index: 0, Accessible: true, QSVHealthy: true},
		},
	}

	// Index 5 is out of bounds, should fallback to first candidate
	path, err := injector.GetDevicePath("qsv", caps, 5)
	if err != nil {
		t.Fatalf("GetDevicePath failed: %v", err)
	}
	if path != "/dev/dri/renderD128" {
		t.Errorf("Expected fallback to first candidate, got %s", path)
	}
}
