package audit

import (
	"bytes"
	"strings"
	"testing"
)

func TestStderrNotifier_Notify(t *testing.T) {
	var buf bytes.Buffer
	notifier := NewNotifierWithOutput(&buf)

	err := notifier.Notify(InfoLevel, "test message")
	if err != nil {
		t.Fatalf("Notify failed: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, "[rffmpeg]") {
		t.Errorf("Expected [rffmpeg] prefix in output, got: %s", output)
	}
	if !strings.Contains(output, "INFO") {
		t.Errorf("Expected level in output, got: %s", output)
	}
	if !strings.Contains(output, "test message") {
		t.Errorf("Expected message in output, got: %s", output)
	}
}

func TestStderrNotifier_Notifyf(t *testing.T) {
	var buf bytes.Buffer
	notifier := NewNotifierWithOutput(&buf)

	err := notifier.Notifyf(WarnLevel, "encoder %s -> %s", "libx264", "h264_nvenc")
	if err != nil {
		t.Fatalf("Notifyf failed: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, "libx264 -> h264_nvenc") {
		t.Errorf("Expected formatted message in output, got: %s", output)
	}
}

func TestStderrNotifier_Silent(t *testing.T) {
	var buf bytes.Buffer
	notifier := NewNotifierWithOutput(&buf)

	// Enable silent mode
	notifier.SetSilent(true)

	err := notifier.Notify(InfoLevel, "test message")
	if err != nil {
		t.Fatalf("Notify failed: %v", err)
	}

	if buf.Len() > 0 {
		t.Errorf("Expected no output in silent mode, got: %s", buf.String())
	}

	if !notifier.IsSilent() {
		t.Error("Expected IsSilent to return true")
	}

	// Disable silent mode
	notifier.SetSilent(false)
	notifier.Notify(InfoLevel, "test")

	if buf.Len() == 0 {
		t.Error("Expected output when not silent")
	}
}

func TestStderrNotifier_NotifyOperation(t *testing.T) {
	var buf bytes.Buffer
	notifier := NewNotifierWithOutput(&buf)

	tests := []struct {
		name     string
		op       AuditOperation
		contains string
	}{
		{
			name: "encoder upgrade",
			op: AuditOperation{
				ScenarioType:     ScenarioEncoderUpgrade,
				OriginalEncoder:  "libx264",
				RewrittenEncoder: "h264_nvenc",
				DecisionReason:   "GPU available",
			},
			contains: "编码器升级",
		},
		{
			name: "encoder fallback",
			op: AuditOperation{
				ScenarioType:     ScenarioEncoderFallback,
				OriginalEncoder:  "h264_nvenc",
				RewrittenEncoder: "libx264",
				DecisionReason:   "NVENC unavailable",
			},
			contains: "编码器降级",
		},
		{
			name: "no encoder specified",
			op: AuditOperation{
				ScenarioType:     ScenarioNoEncoderSpecified,
				RewrittenEncoder: "h264_nvenc",
				DecisionReason:   "Auto-detected NVIDIA GPU",
			},
			contains: "未指定编码器",
		},
		{
			name: "parameter translation",
			op: AuditOperation{
				ScenarioType:   ScenarioParameterTranslation,
				DecisionReason: "crf=23 -> cq=23",
			},
			contains: "参数转换",
		},
		{
			name: "hardware param injection",
			op: AuditOperation{
				ScenarioType:   ScenarioHardwareParamInjection,
				DecisionReason: "-gpu_id 0",
			},
			contains: "注入硬件参数",
		},
		{
			name: "encoder substitution",
			op: AuditOperation{
				ScenarioType:     ScenarioEncoderSubstitution,
				OriginalEncoder:  "h264_nvenc",
				RewrittenEncoder: "h264_qsv",
				DecisionReason:   "NVENC unavailable, QSV available",
			},
			contains: "编码器替换",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			buf.Reset()
			err := notifier.NotifyOperation(tt.op)
			if err != nil {
				t.Fatalf("NotifyOperation failed: %v", err)
			}

			output := buf.String()
			if !strings.Contains(output, "[rffmpeg]") {
				t.Errorf("Expected [rffmpeg] prefix in output, got: %s", output)
			}
			if !strings.Contains(output, tt.contains) {
				t.Errorf("Expected %q in output, got: %s", tt.contains, output)
			}
		})
	}
}

func TestStderrNotifier_LevelMapping(t *testing.T) {
	tests := []struct {
		scenario    ScenarioType
		expectLevel NotifyLevel
	}{
		{ScenarioEncoderUpgrade, InfoLevel},
		{ScenarioHardwareParamInjection, InfoLevel},
		{ScenarioNoEncoderSpecified, InfoLevel},
		{ScenarioEncoderFallback, WarnLevel},
		{ScenarioEncoderSubstitution, WarnLevel},
		{ScenarioParameterTranslation, WarnLevel},
	}

	for _, tt := range tests {
		t.Run(string(tt.scenario), func(t *testing.T) {
			level := scenarioToLevel(tt.scenario)
			if level != tt.expectLevel {
				t.Errorf("Expected level %s for scenario %s, got %s", tt.expectLevel, tt.scenario, level)
			}
		})
	}
}

func TestStderrNotifier_OutputFormat(t *testing.T) {
	var buf bytes.Buffer
	notifier := NewNotifierWithOutput(&buf)

	err := notifier.Notify(InfoLevel, "test message")
	if err != nil {
		t.Fatalf("Notify failed: %v", err)
	}

	output := buf.String()
	expected := "[rffmpeg] INFO: test message\n"
	if output != expected {
		t.Errorf("Expected %q, got %q", expected, output)
	}
}

func TestStderrNotifier_NotifyRewriteChain(t *testing.T) {
	var buf bytes.Buffer
	notifier := NewNotifierWithOutput(&buf)

	err := notifier.NotifyRewriteChain(
		"h264_nvenc,h264_qsv,libx264",
		"libx264",
		"h264_nvenc",
		"GPU available, auto-upgrade to hardware encoder",
		InfoLevel,
	)
	if err != nil {
		t.Fatalf("NotifyRewriteChain failed: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, "[rffmpeg]") {
		t.Errorf("Expected [rffmpeg] prefix, got: %s", output)
	}
	if !strings.Contains(output, "Worker capabilities: h264_nvenc,h264_qsv,libx264") {
		t.Errorf("Expected Worker capabilities in output, got: %s", output)
	}
	if !strings.Contains(output, "Requested: libx264") {
		t.Errorf("Expected Requested in output, got: %s", output)
	}
	if !strings.Contains(output, "Rewritten: h264_nvenc") {
		t.Errorf("Expected Rewritten in output, got: %s", output)
	}
	if !strings.Contains(output, "Reason: GPU available, auto-upgrade to hardware encoder") {
		t.Errorf("Expected Reason in output, got: %s", output)
	}
}

func TestStderrNotifier_NotifyRewriteChain_EmptyFields(t *testing.T) {
	var buf bytes.Buffer
	notifier := NewNotifierWithOutput(&buf)

	// Empty capabilities and reason
	err := notifier.NotifyRewriteChain(
		"",
		"libx264",
		"h264_nvenc",
		"",
		InfoLevel,
	)
	if err != nil {
		t.Fatalf("NotifyRewriteChain failed: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, "Requested: libx264") {
		t.Errorf("Expected Requested in output, got: %s", output)
	}
	if !strings.Contains(output, "Rewritten: h264_nvenc") {
		t.Errorf("Expected Rewritten in output, got: %s", output)
	}
	if strings.Contains(output, "Worker capabilities:") {
		t.Errorf("Expected NO Worker capabilities in output when empty, got: %s", output)
	}
	if strings.Contains(output, "Reason:") {
		t.Errorf("Expected NO Reason in output when empty, got: %s", output)
	}
}

func TestSilentNotifier(t *testing.T) {
	notifier := NewSilentNotifier()

	// All methods should do nothing and return nil
	if err := notifier.Notify(InfoLevel, "test"); err != nil {
		t.Errorf("Notify should return nil, got: %v", err)
	}

	if err := notifier.Notifyf(WarnLevel, "test %s", "msg"); err != nil {
		t.Errorf("Notifyf should return nil, got: %v", err)
	}

	if err := notifier.NotifyOperation(AuditOperation{}); err != nil {
		t.Errorf("NotifyOperation should return nil, got: %v", err)
	}

	if err := notifier.NotifyRewriteChain("", "", "", "", InfoLevel); err != nil {
		t.Errorf("NotifyRewriteChain should return nil, got: %v", err)
	}

	if !notifier.IsSilent() {
		t.Error("SilentNotifier should always be silent")
	}

	// These should be no-ops
	notifier.SetSilent(false)
	notifier.SetOutput(nil)

	if !notifier.IsSilent() {
		t.Error("SilentNotifier should still be silent after SetSilent(false)")
	}
}

func TestNotifier_Concurrency(t *testing.T) {
	var buf bytes.Buffer
	n := NewNotifierWithOutput(&buf)

	done := make(chan bool)

	// Concurrent writes
	for i := 0; i < 10; i++ {
		go func(id int) {
			for j := 0; j < 10; j++ {
				n.Notify(InfoLevel, "test message")
			}
			done <- true
		}(i)
	}

	// Concurrent silent toggles
	go func() {
		for i := 0; i < 10; i++ {
			n.SetSilent(i%2 == 0)
		}
		done <- true
	}()

	// Wait for all goroutines
	for i := 0; i < 11; i++ {
		<-done
	}
}

func TestFormatOperationMessage_Chinese(t *testing.T) {
	// Test Chinese message formatting
	tests := []struct {
		name     string
		op       AuditOperation
		contains string
	}{
		{
			name: "upgrade with empty original",
			op: AuditOperation{
				ScenarioType:     ScenarioEncoderUpgrade,
				OriginalEncoder:  "",
				RewrittenEncoder: "h264_nvenc",
			},
			contains: "未指定编码器",
		},
		{
			name: "fallback message",
			op: AuditOperation{
				ScenarioType:     ScenarioEncoderFallback,
				OriginalEncoder:  "h264_nvenc",
				RewrittenEncoder: "libx264",
				DecisionReason:   "NVENC不可用",
			},
			contains: "NVENC不可用",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg := formatOperationMessage(tt.op)
			if !strings.Contains(msg, tt.contains) {
				t.Errorf("Expected %q in message, got: %s", tt.contains, msg)
			}
		})
	}
}

// Benchmark tests
func BenchmarkStderrNotifier_Notify(b *testing.B) {
	var buf bytes.Buffer
	notifier := NewNotifierWithOutput(&buf)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		buf.Reset()
		notifier.Notify(InfoLevel, "test message")
	}
}

func BenchmarkStderrNotifier_NotifyOperation(b *testing.B) {
	var buf bytes.Buffer
	notifier := NewNotifierWithOutput(&buf)

	op := AuditOperation{
		ScenarioType:     ScenarioEncoderUpgrade,
		OriginalEncoder:  "libx264",
		RewrittenEncoder: "h264_nvenc",
		DecisionReason:   "GPU available",
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		buf.Reset()
		notifier.NotifyOperation(op)
	}
}

func BenchmarkSilentNotifier_Notify(b *testing.B) {
	notifier := NewSilentNotifier()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		notifier.Notify(InfoLevel, "test message")
	}
}
