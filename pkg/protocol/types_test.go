package protocol

import (
	"encoding/json"
	"testing"

	"github.com/tsix404/rffmpeg/pkg/worker/gpu"
)

func TestEncoderInfo_JSON(t *testing.T) {
	enc := EncoderInfo{
		Name:        "h264_nvenc",
		Description: "NVIDIA NVENC H.264 encoder",
		Type:        "video",
		IsHW:        true,
		Priority:    10,
	}

	data, err := json.Marshal(enc)
	if err != nil {
		t.Fatalf("Failed to marshal EncoderInfo: %v", err)
	}

	var decoded EncoderInfo
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Failed to unmarshal EncoderInfo: %v", err)
	}

	if decoded.Name != enc.Name {
		t.Errorf("Name = %q, want %q", decoded.Name, enc.Name)
	}
	if !decoded.IsHW {
		t.Error("IsHW should be true")
	}
}

func TestDecoderInfo_JSON(t *testing.T) {
	dec := DecoderInfo{
		Name:        "h264_cuvid",
		Description: "NVIDIA CUVID H.264 decoder",
		Type:        "video",
		IsHW:        true,
	}

	data, err := json.Marshal(dec)
	if err != nil {
		t.Fatalf("Failed to marshal DecoderInfo: %v", err)
	}

	var decoded DecoderInfo
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Failed to unmarshal DecoderInfo: %v", err)
	}

	if decoded.Name != dec.Name {
		t.Errorf("Name = %q, want %q", decoded.Name, dec.Name)
	}
}

func TestGPUDeviceInfo_JSON(t *testing.T) {
	dev := GPUDeviceInfo{
		Type:          "nvenc",
		Path:          "/dev/nvidia0",
		Name:          "NVIDIA GeForce RTX 3080",
		DriverVersion: "535.104.05",
		Vendor:        "NVIDIA",
		Accessible:    true,
	}

	data, err := json.Marshal(dev)
	if err != nil {
		t.Fatalf("Failed to marshal GPUDeviceInfo: %v", err)
	}

	var decoded GPUDeviceInfo
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Failed to unmarshal GPUDeviceInfo: %v", err)
	}

	if decoded.Type != dev.Type {
		t.Errorf("Type = %q, want %q", decoded.Type, dev.Type)
	}
	if !decoded.Accessible {
		t.Error("Accessible should be true")
	}
}

func TestWorkerCapabilities_JSON(t *testing.T) {
	caps := WorkerCapabilities{
		GPUModel:      "NVIDIA RTX 3080",
		Encoders:      []string{"libx264", "h264_nvenc"},
		Decoders:      []string{"h264", "h264_cuvid"},
		FFmpegVersion: "ffmpeg version 5.0",
		MaxConcurrent: 4,
		VideoEncoders: []EncoderInfo{
			{Name: "libx264", Type: "video", IsHW: false},
			{Name: "h264_nvenc", Type: "video", IsHW: true},
		},
		VideoDecoders: []DecoderInfo{
			{Name: "h264", Type: "video", IsHW: false},
		},
		HWEncoders:       []string{"h264_nvenc"},
		HWDecoders:       []string{"h264_cuvid"},
		GPUDevices:       []GPUDeviceInfo{{Type: "nvenc", Name: "RTX 3080", Accessible: true}},
		EncoderPriority:  []string{"h264_nvenc", "libx264"},
		EncoderBlacklist: []string{"libx265"},
	}

	data, err := json.Marshal(caps)
	if err != nil {
		t.Fatalf("Failed to marshal WorkerCapabilities: %v", err)
	}

	var decoded WorkerCapabilities
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Failed to unmarshal WorkerCapabilities: %v", err)
	}

	if decoded.GPUModel != caps.GPUModel {
		t.Errorf("GPUModel = %q, want %q", decoded.GPUModel, caps.GPUModel)
	}
	if len(decoded.Encoders) != 2 {
		t.Errorf("Encoders length = %d, want 2", len(decoded.Encoders))
	}
	if len(decoded.VideoEncoders) != 2 {
		t.Errorf("VideoEncoders length = %d, want 2", len(decoded.VideoEncoders))
	}
	if len(decoded.HWEncoders) != 1 {
		t.Errorf("HWEncoders length = %d, want 1", len(decoded.HWEncoders))
	}
	if len(decoded.GPUDevices) != 1 {
		t.Errorf("GPUDevices length = %d, want 1", len(decoded.GPUDevices))
	}
	if decoded.MaxConcurrent != 4 {
		t.Errorf("MaxConcurrent = %d, want 4", decoded.MaxConcurrent)
	}
}

func TestNewWorkerCapabilities(t *testing.T) {
	videoEncoders := []EncoderInfo{
		{Name: "libx264", Type: "video", IsHW: false},
		{Name: "h264_nvenc", Type: "video", IsHW: true},
	}
	videoDecoders := []DecoderInfo{
		{Name: "h264", Type: "video", IsHW: false},
		{Name: "h264_cuvid", Type: "video", IsHW: true},
	}
	gpuDevices := []gpu.Device{
		{Type: gpu.DeviceTypeNVENC, Name: "RTX 3080", Accessible: true},
	}
	encoderPriority := []string{"h264_nvenc", "libx264"}
	encoderBlacklist := []string{"libx265"}

	caps := NewWorkerCapabilities(
		"ffmpeg version 5.0",
		videoEncoders,
		videoDecoders,
		gpuDevices,
		4,
		encoderPriority,
		encoderBlacklist,
	)

	// Check legacy fields are populated
	if len(caps.Encoders) != 2 {
		t.Errorf("Encoders length = %d, want 2", len(caps.Encoders))
	}
	if len(caps.Decoders) != 2 {
		t.Errorf("Decoders length = %d, want 2", len(caps.Decoders))
	}
	if len(caps.HWEncoders) != 1 {
		t.Errorf("HWEncoders length = %d, want 1", len(caps.HWEncoders))
	}
	if len(caps.HWDecoders) != 1 {
		t.Errorf("HWDecoders length = %d, want 1", len(caps.HWDecoders))
	}
	if caps.GPUModel != "RTX 3080" {
		t.Errorf("GPUModel = %q, want %q", caps.GPUModel, "RTX 3080")
	}
	if len(caps.GPUDevices) != 1 {
		t.Errorf("GPUDevices length = %d, want 1", len(caps.GPUDevices))
	}
	if caps.MaxConcurrent != 4 {
		t.Errorf("MaxConcurrent = %d, want 4", caps.MaxConcurrent)
	}
	if len(caps.EncoderPriority) != 2 {
		t.Errorf("EncoderPriority length = %d, want 2", len(caps.EncoderPriority))
	}
	if len(caps.EncoderBlacklist) != 1 {
		t.Errorf("EncoderBlacklist length = %d, want 1", len(caps.EncoderBlacklist))
	}
}

func TestNewWorkerCapabilities_Empty(t *testing.T) {
	caps := NewWorkerCapabilities(
		"ffmpeg version 5.0",
		nil,
		nil,
		nil,
		1,
		nil,
		nil,
	)

	if len(caps.Encoders) != 0 {
		t.Errorf("Encoders should be empty, got %d", len(caps.Encoders))
	}
	if len(caps.Decoders) != 0 {
		t.Errorf("Decoders should be empty, got %d", len(caps.Decoders))
	}
	if caps.GPUModel != "" {
		t.Errorf("GPUModel should be empty, got %q", caps.GPUModel)
	}
}
