package rewrite

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/tsic404/rffmpeg/pkg/encoder"
)

// --- ScenarioType tests ---

func TestScenarioTypeString(t *testing.T) {
	tests := []struct {
		scenario ScenarioType
		want     string
	}{
		{ScenarioUnspecifiedEncoderWithHW, "unspecified_encoder_with_hw"},
		{ScenarioUnspecifiedEncoderNoHW, "unspecified_encoder_no_hw"},
		{ScenarioSpecifiedEncoderSupported, "specified_encoder_supported"},
		{ScenarioSpecifiedEncoderUnsupportedWithAlternative, "specified_encoder_unsupported_with_alternative"},
		{ScenarioSpecifiedEncoderUnsupportedFallbackSoftware, "specified_encoder_unsupported_fallback_software"},
		{ScenarioFormatNotAvailable, "format_not_available"},
		{ScenarioType(0), "unknown"},
	}

	for _, tt := range tests {
		got := tt.scenario.String()
		if got != tt.want {
			t.Errorf("ScenarioType(%d).String() = %q, want %q", tt.scenario, got, tt.want)
		}
	}
}

func TestScenarioTypeDescription(t *testing.T) {
	tests := []struct {
		scenario ScenarioType
		want     string
	}{
		{ScenarioUnspecifiedEncoderWithHW, "Auto-upgrade to best hardware encoder"},
		{ScenarioUnspecifiedEncoderNoHW, "Use software baseline (libx264)"},
		{ScenarioSpecifiedEncoderSupported, "Pass through unchanged"},
		{ScenarioSpecifiedEncoderUnsupportedWithAlternative, "Translate to local hardware encoder"},
		{ScenarioSpecifiedEncoderUnsupportedFallbackSoftware, "Fallback to software encoder"},
		{ScenarioFormatNotAvailable, "Format not available (error)"},
	}

	for _, tt := range tests {
		got := tt.scenario.Description()
		if got != tt.want {
			t.Errorf("ScenarioType(%d).Description() = %q, want %q", tt.scenario, got, tt.want)
		}
	}
}

// --- HardwareCapabilities tests ---

func sampleHardwareCapabilities() HardwareCapabilities {
	return HardwareCapabilities{
		AvailableEncoders: []encoder.EncoderFamily{
			encoder.EncoderLibX264,
			encoder.EncoderLibX265,
			encoder.EncoderH264NVENC,
			encoder.EncoderHEVCNVENC,
			encoder.EncoderH264QSV,
		},
		HardwareEncoders: []encoder.EncoderFamily{
			encoder.EncoderH264NVENC,
			encoder.EncoderHEVCNVENC,
			encoder.EncoderH264QSV,
		},
		SoftwareEncoders: []encoder.EncoderFamily{
			encoder.EncoderLibX264,
			encoder.EncoderLibX265,
		},
		SupportedCodecs: []encoder.CodecFormat{
			encoder.CodecH264,
			encoder.CodecHEVC,
		},
		GPUDevices: []GPUDevice{
			{Type: "nvenc", Name: "NVIDIA RTX 3080", Vendor: "NVIDIA", Accessible: true},
		},
		EncoderPriority: DefaultEncoderPriority(),
		EncoderBlacklist: []encoder.EncoderFamily{
			encoder.EncoderH264AMF,
		},
	}
}

func TestHasEncoder(t *testing.T) {
	hc := sampleHardwareCapabilities()

	tests := []struct {
		enc  encoder.EncoderFamily
		want bool
	}{
		{encoder.EncoderH264NVENC, true},
		{encoder.EncoderLibX264, true},
		{encoder.EncoderH264VAAPI, false},
		{encoder.EncoderH264AMF, false},
	}

	for _, tt := range tests {
		got := hc.HasEncoder(tt.enc)
		if got != tt.want {
			t.Errorf("HasEncoder(%s) = %v, want %v", tt.enc, got, tt.want)
		}
	}
}

func TestHasHardwareEncoder(t *testing.T) {
	hc := sampleHardwareCapabilities()
	if !hc.HasHardwareEncoder() {
		t.Error("HasHardwareEncoder() = false, want true")
	}

	hc2 := HardwareCapabilities{
		AvailableEncoders: []encoder.EncoderFamily{encoder.EncoderLibX264},
		SoftwareEncoders:  []encoder.EncoderFamily{encoder.EncoderLibX264},
	}
	if hc2.HasHardwareEncoder() {
		t.Error("HasHardwareEncoder() = true for software-only, want false")
	}
}

func TestHasCodecSupport(t *testing.T) {
	hc := sampleHardwareCapabilities()

	tests := []struct {
		codec encoder.CodecFormat
		want  bool
	}{
		{encoder.CodecH264, true},
		{encoder.CodecHEVC, true},
		{encoder.CodecVP9, false},
		{encoder.CodecAV1, false},
	}

	for _, tt := range tests {
		got := hc.HasCodecSupport(tt.codec)
		if got != tt.want {
			t.Errorf("HasCodecSupport(%s) = %v, want %v", tt.codec, got, tt.want)
		}
	}
}

func TestGetBestHardwareEncoder(t *testing.T) {
	hc := sampleHardwareCapabilities()

	// H.264: NVENC has highest priority
	best := hc.GetBestHardwareEncoder(encoder.CodecH264)
	if best != encoder.EncoderH264NVENC {
		t.Errorf("GetBestHardwareEncoder(h264) = %s, want h264_nvenc", best)
	}

	// HEVC: NVENC has highest priority
	best = hc.GetBestHardwareEncoder(encoder.CodecHEVC)
	if best != encoder.EncoderHEVCNVENC {
		t.Errorf("GetBestHardwareEncoder(hevc) = %s, want hevc_nvenc", best)
	}

	// VP9: no hardware encoder available
	best = hc.GetBestHardwareEncoder(encoder.CodecVP9)
	if best != "" {
		t.Errorf("GetBestHardwareEncoder(vp9) = %s, want empty", best)
	}
}

func TestGetSoftwareEncoder(t *testing.T) {
	hc := sampleHardwareCapabilities()

	tests := []struct {
		codec encoder.CodecFormat
		want  encoder.EncoderFamily
	}{
		{encoder.CodecH264, encoder.EncoderLibX264},
		{encoder.CodecHEVC, encoder.EncoderLibX265},
		{encoder.CodecVP9, ""},
	}

	for _, tt := range tests {
		got := hc.GetSoftwareEncoder(tt.codec)
		if got != tt.want {
			t.Errorf("GetSoftwareEncoder(%s) = %s, want %s", tt.codec, got, tt.want)
		}
	}
}

func TestIsBlacklisted(t *testing.T) {
	hc := sampleHardwareCapabilities()

	if !hc.IsBlacklisted(encoder.EncoderH264AMF) {
		t.Error("IsBlacklisted(h264_amf) = false, want true")
	}
	if hc.IsBlacklisted(encoder.EncoderH264NVENC) {
		t.Error("IsBlacklisted(h264_nvenc) = true, want false")
	}
}

// --- EncoderRewriteRequest/Response serialization tests ---

func TestEncoderRewriteRequestJSON(t *testing.T) {
	req := EncoderRewriteRequest{
		OriginalArgs:     []string{"-i", "input.mp4", "-c:v", "libx264", "-crf", "23", "output.mp4"},
		SpecifiedEncoder: encoder.EncoderLibX264,
		TargetCodec:      encoder.CodecH264,
		HardwareCapabilities: HardwareCapabilities{
			AvailableEncoders: []encoder.EncoderFamily{encoder.EncoderH264NVENC},
			HardwareEncoders:  []encoder.EncoderFamily{encoder.EncoderH264NVENC},
			SupportedCodecs:   []encoder.CodecFormat{encoder.CodecH264},
		},
		EncoderParams: map[string]string{"crf": "23", "preset": "medium"},
		AutoHW:        true,
		RequestID:     "test-req-001",
		Timestamp:     time.Date(2026, 5, 19, 15, 0, 0, 0, time.UTC),
	}

	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("json.Marshal(req) error: %v", err)
	}

	var decoded EncoderRewriteRequest
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("json.Unmarshal error: %v", err)
	}

	if decoded.SpecifiedEncoder != encoder.EncoderLibX264 {
		t.Errorf("SpecifiedEncoder = %s, want libx264", decoded.SpecifiedEncoder)
	}
	if decoded.TargetCodec != encoder.CodecH264 {
		t.Errorf("TargetCodec = %s, want h264", decoded.TargetCodec)
	}
	if decoded.AutoHW != true {
		t.Error("AutoHW = false, want true")
	}
	if decoded.RequestID != "test-req-001" {
		t.Errorf("RequestID = %s, want test-req-001", decoded.RequestID)
	}
	if decoded.EncoderParams["crf"] != "23" {
		t.Errorf("EncoderParams[crf] = %s, want 23", decoded.EncoderParams["crf"])
	}
	if len(decoded.OriginalArgs) != 7 {
		t.Errorf("len(OriginalArgs) = %d, want 7", len(decoded.OriginalArgs))
	}
}

func TestEncoderRewriteResponseJSON(t *testing.T) {
	resp := EncoderRewriteResponse{
		RewrittenArgs:        []string{"-i", "input.mp4", "-c:v", "h264_nvenc", "-cq", "23", "output.mp4"},
		OriginalEncoder:      encoder.EncoderLibX264,
		TargetEncoder:        encoder.EncoderH264NVENC,
		Scenario:             ScenarioUnspecifiedEncoderWithHW,
		TranslationPerformed: true,
		AuditRecords: []AuditRecord{
			{
				ID:            "audit-001",
				Timestamp:     time.Date(2026, 5, 19, 15, 0, 1, 0, time.UTC),
				Operation:     AuditOpParamTranslate,
				SourceEncoder: encoder.EncoderLibX264,
				TargetEncoder: encoder.EncoderH264NVENC,
				SourceParam:   "crf",
				TargetParam:   "cq",
				SourceValue:   "23",
				TargetValue:   "23",
				Reason:        "Translating software to hardware encoder",
				Success:       true,
			},
		},
		Notifications: []Notification{
			{
				Timestamp: time.Date(2026, 5, 19, 15, 0, 1, 0, time.UTC),
				Level:     NotificationLevelInfo,
				Message:   "Encoder rewritten: libx264 -> h264_nvenc",
			},
		},
		RequestID: "test-req-001",
		Timestamp: time.Date(2026, 5, 19, 15, 0, 1, 0, time.UTC),
	}

	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("json.Marshal(resp) error: %v", err)
	}

	var decoded EncoderRewriteResponse
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("json.Unmarshal error: %v", err)
	}

	if decoded.OriginalEncoder != encoder.EncoderLibX264 {
		t.Errorf("OriginalEncoder = %s, want libx264", decoded.OriginalEncoder)
	}
	if decoded.TargetEncoder != encoder.EncoderH264NVENC {
		t.Errorf("TargetEncoder = %s, want h264_nvenc", decoded.TargetEncoder)
	}
	if decoded.Scenario != ScenarioUnspecifiedEncoderWithHW {
		t.Errorf("Scenario = %d, want %d", decoded.Scenario, ScenarioUnspecifiedEncoderWithHW)
	}
	if !decoded.TranslationPerformed {
		t.Error("TranslationPerformed = false, want true")
	}
	if len(decoded.AuditRecords) != 1 {
		t.Errorf("len(AuditRecords) = %d, want 1", len(decoded.AuditRecords))
	}
	if len(decoded.Notifications) != 1 {
		t.Errorf("len(Notifications) = %d, want 1", len(decoded.Notifications))
	}
	if decoded.AuditRecords[0].SourceParam != "crf" {
		t.Errorf("AuditRecords[0].SourceParam = %s, want crf", decoded.AuditRecords[0].SourceParam)
	}
}

// --- AuditRecord serialization test ---

func TestAuditRecordJSON(t *testing.T) {
	record := AuditRecord{
		ID:            "audit-002",
		Timestamp:     time.Date(2026, 5, 19, 15, 0, 2, 0, time.UTC),
		Operation:     AuditOpHWParamInject,
		SourceEncoder: encoder.EncoderH264NVENC,
		TargetEncoder: encoder.EncoderH264NVENC,
		TargetParam:   "-gpu",
		TargetValue:   "0",
		Reason:        "Injecting GPU device parameter for NVENC",
		Success:       true,
	}

	data, err := json.Marshal(record)
	if err != nil {
		t.Fatalf("json.Marshal error: %v", err)
	}

	var decoded AuditRecord
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("json.Unmarshal error: %v", err)
	}

	if decoded.Operation != AuditOpHWParamInject {
		t.Errorf("Operation = %s, want hw_param_inject", decoded.Operation)
	}
	if decoded.Success != true {
		t.Error("Success = false, want true")
	}
}

// --- Notification serialization test ---

func TestNotificationJSON(t *testing.T) {
	notif := Notification{
		Timestamp: time.Date(2026, 5, 19, 15, 0, 3, 0, time.UTC),
		Level:     NotificationLevelWarning,
		Message:   "Encoder h264_vaapi not available, falling back to libx264",
		Details:   map[string]string{"source": "h264_vaapi", "target": "libx264"},
	}

	data, err := json.Marshal(notif)
	if err != nil {
		t.Fatalf("json.Marshal error: %v", err)
	}

	var decoded Notification
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("json.Unmarshal error: %v", err)
	}

	if decoded.Level != NotificationLevelWarning {
		t.Errorf("Level = %s, want warning", decoded.Level)
	}
	if decoded.Details["source"] != "h264_vaapi" {
		t.Errorf("Details[source] = %s, want h264_vaapi", decoded.Details["source"])
	}
}

// --- RewriteError serialization test ---

func TestRewriteErrorJSON(t *testing.T) {
	rerr := RewriteError{
		Code:    ErrFormatNotMatch,
		Message: "VP9 format is not available on this worker",
		Detail:  "No VP9 encoders detected",
		Codec:   encoder.CodecVP9,
	}

	data, err := json.Marshal(rerr)
	if err != nil {
		t.Fatalf("json.Marshal error: %v", err)
	}

	var decoded RewriteError
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("json.Unmarshal error: %v", err)
	}

	if decoded.Code != ErrFormatNotMatch {
		t.Errorf("Code = %s, want FORMAT_NOT_MATCH", decoded.Code)
	}
	if decoded.Codec != encoder.CodecVP9 {
		t.Errorf("Codec = %s, want vp9", decoded.Codec)
	}
}

// --- HardwareCapabilities serialization test ---

func TestHardwareCapabilitiesJSON(t *testing.T) {
	hc := sampleHardwareCapabilities()

	data, err := json.Marshal(hc)
	if err != nil {
		t.Fatalf("json.Marshal error: %v", err)
	}

	var decoded HardwareCapabilities
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("json.Unmarshal error: %v", err)
	}

	if len(decoded.AvailableEncoders) != len(hc.AvailableEncoders) {
		t.Errorf("AvailableEncoders len = %d, want %d", len(decoded.AvailableEncoders), len(hc.AvailableEncoders))
	}
	if len(decoded.HardwareEncoders) != len(hc.HardwareEncoders) {
		t.Errorf("HardwareEncoders len = %d, want %d", len(decoded.HardwareEncoders), len(hc.HardwareEncoders))
	}
	if len(decoded.GPUDevices) != 1 {
		t.Errorf("GPUDevices len = %d, want 1", len(decoded.GPUDevices))
	}
	if decoded.GPUDevices[0].Vendor != "NVIDIA" {
		t.Errorf("GPUDevices[0].Vendor = %s, want NVIDIA", decoded.GPUDevices[0].Vendor)
	}
}

// --- Constants tests ---

func TestDefaultEncoderPriority(t *testing.T) {
	priority := DefaultEncoderPriority()

	if len(priority) == 0 {
		t.Fatal("DefaultEncoderPriority() returned empty list")
	}

	// Check NVENC has highest priority for H.264
	for _, entry := range priority {
		if entry.Encoder == encoder.EncoderH264NVENC {
			if entry.Priority != PriorityNVENC {
				t.Errorf("H264NVENC priority = %d, want %d", entry.Priority, PriorityNVENC)
			}
		}
		if entry.Encoder == encoder.EncoderLibX264 {
			if entry.Priority != PrioritySoftware {
				t.Errorf("libx264 priority = %d, want %d", entry.Priority, PrioritySoftware)
			}
		}
	}

	// Verify priority ordering: NVENC > QSV > VAAPI > AMF > VideoToolbox > Software
	if PriorityNVENC <= PriorityQSV {
		t.Errorf("PriorityNVENC (%d) should be > PriorityQSV (%d)", PriorityNVENC, PriorityQSV)
	}
	if PriorityQSV <= PriorityVAAPI {
		t.Errorf("PriorityQSV (%d) should be > PriorityVAAPI (%d)", PriorityQSV, PriorityVAAPI)
	}
	if PriorityVAAPI <= PriorityAMF {
		t.Errorf("PriorityVAAPI (%d) should be > PriorityAMF (%d)", PriorityVAAPI, PriorityAMF)
	}
	if PriorityAMF <= PriorityVideoToolbox {
		t.Errorf("PriorityAMF (%d) should be > PriorityVideoToolbox (%d)", PriorityAMF, PriorityVideoToolbox)
	}
	if PriorityVideoToolbox <= PrioritySoftware {
		t.Errorf("PriorityVideoToolbox (%d) should be > PrioritySoftware (%d)", PriorityVideoToolbox, PrioritySoftware)
	}
}

func TestGetPriorityForEncoder(t *testing.T) {
	tests := []struct {
		enc  encoder.EncoderFamily
		want int
	}{
		{encoder.EncoderH264NVENC, PriorityNVENC},
		{encoder.EncoderH264QSV, PriorityQSV},
		{encoder.EncoderH264VAAPI, PriorityVAAPI},
		{encoder.EncoderH264AMF, PriorityAMF},
		{encoder.EncoderH264VT, PriorityVideoToolbox},
		{encoder.EncoderLibX264, PrioritySoftware},
		{encoder.EncoderHEVCNVENC, PriorityNVENC},
		{encoder.EncoderLibX265, PrioritySoftware},
	}

	for _, tt := range tests {
		got := GetPriorityForEncoder(tt.enc)
		if got != tt.want {
			t.Errorf("GetPriorityForEncoder(%s) = %d, want %d", tt.enc, got, tt.want)
		}
	}
}

func TestSoftwareEncoderForCodec(t *testing.T) {
	tests := []struct {
		codec encoder.CodecFormat
		want  encoder.EncoderFamily
	}{
		{encoder.CodecH264, encoder.EncoderLibX264},
		{encoder.CodecHEVC, encoder.EncoderLibX265},
		{encoder.CodecVP9, encoder.EncoderLibVPX},
		{encoder.CodecAV1, encoder.EncoderLibSVTAV1},
	}

	for _, tt := range tests {
		got := SoftwareEncoderForCodec(tt.codec)
		if got != tt.want {
			t.Errorf("SoftwareEncoderForCodec(%s) = %s, want %s", tt.codec, got, tt.want)
		}
	}
}

// --- ErrorCode tests ---

func TestErrorCodeString(t *testing.T) {
	if ErrFormatNotMatch.String() != "FORMAT_NOT_MATCH" {
		t.Errorf("ErrFormatNotMatch.String() = %s, want FORMAT_NOT_MATCH", ErrFormatNotMatch.String())
	}
}

func TestErrorCodeDescription(t *testing.T) {
	desc := ErrFormatNotMatch.Description()
	if desc == "" {
		t.Error("ErrFormatNotMatch.Description() returned empty string")
	}
}

func TestErrorCodeIsTerminal(t *testing.T) {
	tests := []struct {
		code ErrorCode
		want bool
	}{
		{ErrFormatNotMatch, true},
		{ErrNoSuitableEncoder, true},
		{ErrBlacklistedEncoder, true},
		{ErrHardwareNotSupported, false},
		{ErrTranslationFailed, false},
		{ErrInvalidRequest, false},
		{ErrEncoderNotAvailable, false},
		{ErrHardwareInjectionFailed, false},
	}

	for _, tt := range tests {
		got := tt.code.IsTerminal()
		if got != tt.want {
			t.Errorf("ErrorCode(%s).IsTerminal() = %v, want %v", tt.code, got, tt.want)
		}
	}
}

// --- GPUDevice serialization test ---

func TestGPUDeviceJSON(t *testing.T) {
	dev := GPUDevice{
		Type:          "nvenc",
		Path:          "/dev/nvidia0",
		Name:          "NVIDIA RTX 3080",
		Vendor:        "NVIDIA",
		DriverVersion: "535.104.05",
		Accessible:    true,
	}

	data, err := json.Marshal(dev)
	if err != nil {
		t.Fatalf("json.Marshal error: %v", err)
	}

	var decoded GPUDevice
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("json.Unmarshal error: %v", err)
	}

	if decoded.Type != "nvenc" {
		t.Errorf("Type = %s, want nvenc", decoded.Type)
	}
	if decoded.Path != "/dev/nvidia0" {
		t.Errorf("Path = %s, want /dev/nvidia0", decoded.Path)
	}
	if !decoded.Accessible {
		t.Error("Accessible = false, want true")
	}
}

// --- EncoderPriorityEntry serialization test ---

func TestEncoderPriorityEntryJSON(t *testing.T) {
	entry := EncoderPriorityEntry{
		Encoder:  encoder.EncoderH264NVENC,
		Priority: PriorityNVENC,
	}

	data, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("json.Marshal error: %v", err)
	}

	var decoded EncoderPriorityEntry
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("json.Unmarshal error: %v", err)
	}

	if decoded.Encoder != encoder.EncoderH264NVENC {
		t.Errorf("Encoder = %s, want h264_nvenc", decoded.Encoder)
	}
	if decoded.Priority != PriorityNVENC {
		t.Errorf("Priority = %d, want %d", decoded.Priority, PriorityNVENC)
	}
}

// --- ScenarioInfo tests ---

func TestScenarioInfo(t *testing.T) {
	info := ScenarioInfo{
		Type:                ScenarioUnspecifiedEncoderWithHW,
		Description:         "Auto-upgrade to best hardware encoder",
		RequiresTranslation: true,
		RequiresHWInjection: true,
		IsError:             false,
		RecommendedAction:   "Select best available hardware encoder",
	}

	data, err := json.Marshal(info)
	if err != nil {
		t.Fatalf("json.Marshal error: %v", err)
	}

	var decoded ScenarioInfo
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("json.Unmarshal error: %v", err)
	}

	if decoded.Type != ScenarioUnspecifiedEncoderWithHW {
		t.Errorf("Type = %d, want %d", decoded.Type, ScenarioUnspecifiedEncoderWithHW)
	}
	if decoded.RequiresTranslation != true {
		t.Error("RequiresTranslation = false, want true")
	}
}

// --- Empty/omitempty tests ---

func TestEncoderRewriteResponseOmitEmpty(t *testing.T) {
	resp := EncoderRewriteResponse{
		RewrittenArgs:   []string{"-i", "input.mp4", "-c:v", "libx264", "output.mp4"},
		OriginalEncoder: encoder.EncoderLibX264,
		TargetEncoder:   encoder.EncoderLibX264,
		Scenario:        ScenarioSpecifiedEncoderSupported,
		AuditRecords:    []AuditRecord{},
		Notifications:   []Notification{},
		Timestamp:       time.Date(2026, 5, 19, 15, 0, 0, 0, time.UTC),
	}

	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("json.Marshal error: %v", err)
	}

	// Warnings and Errors should be omitted when empty
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("json.Unmarshal to raw map error: %v", err)
	}

	if _, exists := raw["warnings"]; exists {
		t.Error("warnings field should be omitted when empty")
	}
	if _, exists := raw["errors"]; exists {
		t.Error("errors field should be omitted when empty")
	}
}

func TestHardwareCapabilitiesEmptyBlacklist(t *testing.T) {
	hc := HardwareCapabilities{
		AvailableEncoders: []encoder.EncoderFamily{encoder.EncoderLibX264},
		SoftwareEncoders:  []encoder.EncoderFamily{encoder.EncoderLibX264},
		SupportedCodecs:   []encoder.CodecFormat{encoder.CodecH264},
	}

	data, err := json.Marshal(hc)
	if err != nil {
		t.Fatalf("json.Marshal error: %v", err)
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("json.Unmarshal to raw map error: %v", err)
	}

	if _, exists := raw["encoder_blacklist"]; exists {
		t.Error("encoder_blacklist field should be omitted when empty")
	}
	if _, exists := raw["gpu_devices"]; exists {
		t.Error("gpu_devices field should be omitted when empty")
	}
}
