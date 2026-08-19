package audit

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// TestIntegration demonstrates the audit and notification system working together.
// This test verifies the "rewrite is not silent" principle.
func TestIntegration(t *testing.T) {
	// Setup recorder and notifier
	recorder := NewRingBufferRecorder(100)
	var buf bytes.Buffer
	notifier := NewNotifierWithOutput(&buf)

	// Simulate an encoder upgrade scenario
	op := AuditOperation{
		RequestID:        "job-001",
		ScenarioType:     ScenarioEncoderUpgrade,
		OriginalEncoder:  "libx264",
		RewrittenEncoder: "h264_nvenc",
		OriginalParams: map[string]interface{}{
			"crf":    "23",
			"preset": "medium",
		},
		RewrittenParams: map[string]interface{}{
			"cq":     "23",
			"preset": "p4",
		},
		DecisionReason:      "检测到 NVIDIA GPU，自动升级到 NVENC 硬件编码器",
		CapabilitiesSummary: "h264_nvenc,h264_qsv,libx264",
		Context: map[string]interface{}{
			"gpu_vendor": "nvidia",
			"gpu_model":  "RTX 3080",
		},
	}

	// Record the operation
	err := recorder.Record(op)
	if err != nil {
		t.Fatalf("Record failed: %v", err)
	}

	// Notify the user
	err = notifier.NotifyOperation(op)
	if err != nil {
		t.Fatalf("NotifyOperation failed: %v", err)
	}

	// Verify notification was output
	output := buf.String()
	if !strings.Contains(output, "[rffmpeg]") {
		t.Error("Expected [rffmpeg] notification prefix")
	}
	if !strings.Contains(output, "INFO") {
		t.Error("Expected INFO level for upgrade scenario")
	}

	// Verify audit record can be queried
	ops, err := recorder.Query("job-001")
	if err != nil {
		t.Fatalf("Query failed: %v", err)
	}
	if len(ops) != 1 {
		t.Errorf("Expected 1 operation, got %d", len(ops))
	}

	// Verify all information was captured
	recorded := ops[0]
	if recorded.OriginalEncoder != "libx264" {
		t.Errorf("Expected original encoder libx264, got %s", recorded.OriginalEncoder)
	}
	if recorded.RewrittenEncoder != "h264_nvenc" {
		t.Errorf("Expected rewritten encoder h264_nvenc, got %s", recorded.RewrittenEncoder)
	}
	if recorded.Context["gpu_vendor"] != "nvidia" {
		t.Error("Expected GPU vendor context")
	}
	if recorded.CapabilitiesSummary != "h264_nvenc,h264_qsv,libx264" {
		t.Errorf("Expected CapabilitiesSummary, got %s", recorded.CapabilitiesSummary)
	}
}

// TestIntegrationMultipleOperations tests recording multiple operations for one request.
func TestIntegrationMultipleOperations(t *testing.T) {
	recorder := NewRingBufferRecorder(100)
	var buf bytes.Buffer
	notifier := NewNotifierWithOutput(&buf)

	requestID := "job-002"

	// Simulate multiple operations for one job
	operations := []AuditOperation{
		{
			RequestID:            requestID,
			ScenarioType:         ScenarioNoEncoderSpecified,
			RewrittenEncoder:     "h264_nvenc",
			DecisionReason:       "未指定编码器，自动选择 NVENC",
			CapabilitiesSummary:  "h264_nvenc,libx264",
		},
		{
			RequestID:       requestID,
			ScenarioType:    ScenarioParameterTranslation,
			DecisionReason:  "crf=23 -> cq=23",
			OriginalParams:  map[string]interface{}{"crf": "23"},
			RewrittenParams: map[string]interface{}{"cq": "23"},
		},
		{
			RequestID:      requestID,
			ScenarioType:   ScenarioHardwareParamInjection,
			DecisionReason: "-gpu_id 0",
		},
	}

	// Record and notify all operations
	for _, op := range operations {
		recorder.Record(op)
		notifier.NotifyOperation(op)
	}

	// Get summary
	summary, err := recorder.GetSummary(requestID)
	if err != nil {
		t.Fatalf("GetSummary failed: %v", err)
	}

	// Verify summary
	if summary.TotalOperations != 3 {
		t.Errorf("Expected 3 operations, got %d", summary.TotalOperations)
	}
	if summary.Scenarios[ScenarioNoEncoderSpecified] != 1 {
		t.Error("Expected 1 no_encoder_specified scenario")
	}
	if summary.Scenarios[ScenarioParameterTranslation] != 1 {
		t.Error("Expected 1 parameter_translation scenario")
	}
	if summary.Scenarios[ScenarioHardwareParamInjection] != 1 {
		t.Error("Expected 1 hardware_param_injection scenario")
	}

	// Verify output contains all notifications
	output := buf.String()
	if strings.Count(output, "[rffmpeg]") != 3 {
		t.Errorf("Expected 3 notifications, got %d", strings.Count(output, "[rffmpeg]"))
	}
}

// TestIntegrationFallback tests fallback scenario with warning.
func TestIntegrationFallback(t *testing.T) {
	recorder := NewRingBufferRecorder(100)
	var buf bytes.Buffer
	notifier := NewNotifierWithOutput(&buf)

	// Simulate fallback scenario
	op := AuditOperation{
		RequestID:           "job-003",
		ScenarioType:        ScenarioEncoderFallback,
		OriginalEncoder:     "h264_nvenc",
		RewrittenEncoder:    "libx264",
		DecisionReason:      "NVENC 不可用，降级到软件编码器",
		CapabilitiesSummary: "libx264",
	}

	recorder.Record(op)
	notifier.NotifyOperation(op)

	// Verify warning level
	output := buf.String()
	if !strings.Contains(output, "WARN") {
		t.Error("Expected WARN level for fallback scenario")
	}
	if !strings.Contains(output, "降级") {
		t.Error("Expected fallback message in output")
	}

	// Verify summary shows warnings
	summary, err := recorder.GetSummary("job-003")
	if err != nil {
		t.Fatalf("GetSummary failed: %v", err)
	}
	if !summary.HasWarnings {
		t.Error("Expected HasWarnings to be true for fallback scenario")
	}
}

// TestIntegrationRewriteChain tests the detailed rewrite chain notification.
func TestIntegrationRewriteChain(t *testing.T) {
	var buf bytes.Buffer
	notifier := NewNotifierWithOutput(&buf)

	err := notifier.NotifyRewriteChain(
		"h264_nvenc,h264_qsv,libx264",
		"libx264",
		"h264_nvenc",
		"NVIDIA GPU detected, auto-upgrade to hardware encoder",
		InfoLevel,
	)
	if err != nil {
		t.Fatalf("NotifyRewriteChain failed: %v", err)
	}

	output := buf.String()
	expectedPrefix := "[rffmpeg] INFO: Worker capabilities: h264_nvenc,h264_qsv,libx264 | Requested: libx264 | Rewritten: h264_nvenc | Reason: NVIDIA GPU detected, auto-upgrade to hardware encoder\n"
	if output != expectedPrefix {
		t.Errorf("Expected %q, got %q", expectedPrefix, output)
	}
}

// TestIntegrationSilentMode tests that silent mode suppresses output.
func TestIntegrationSilentMode(t *testing.T) {
	recorder := NewRingBufferRecorder(100)
	var buf bytes.Buffer
	notifier := NewNotifierWithOutput(&buf)

	// Enable silent mode
	notifier.SetSilent(true)

	// Record and notify
	op := AuditOperation{
		RequestID:        "job-004",
		ScenarioType:     ScenarioEncoderUpgrade,
		OriginalEncoder:  "libx264",
		RewrittenEncoder: "h264_nvenc",
		DecisionReason:   "GPU available",
	}

	recorder.Record(op)
	notifier.NotifyOperation(op)

	// Verify no output
	if buf.Len() > 0 {
		t.Errorf("Expected no output in silent mode, got: %s", buf.String())
	}

	// Verify record was still captured
	ops, err := recorder.Query("job-004")
	if err != nil {
		t.Fatalf("Query failed: %v", err)
	}
	if len(ops) != 1 {
		t.Error("Record should still be captured in silent mode")
	}
}

// TestIntegrationConcurrent tests concurrent recording and notification.
func TestIntegrationConcurrent(t *testing.T) {
	recorder := NewRingBufferRecorder(1000)
	notifier := NewSilentNotifier() // Use silent notifier to avoid race on buffer

	done := make(chan bool)

	// Simulate concurrent job processing
	for i := 0; i < 10; i++ {
		go func(jobID int) {
			for j := 0; j < 5; j++ {
				op := AuditOperation{
					RequestID:        "job-" + string(rune('0'+jobID)),
					ScenarioType:     ScenarioEncoderUpgrade,
					OriginalEncoder:  "libx264",
					RewrittenEncoder: "h264_nvenc",
					DecisionReason:   "test",
				}
				recorder.Record(op)
				notifier.NotifyOperation(op)
			}
			done <- true
		}(i)
	}

	// Wait for all goroutines
	for i := 0; i < 10; i++ {
		<-done
	}

	// Verify all records were captured (thread-safe recorder)
	if recorder.Size() != 50 {
		t.Errorf("Expected 50 records, got %d", recorder.Size())
	}
}

// TestIntegrationRingBufferOverflow tests behavior when buffer overflows.
func TestIntegrationRingBufferOverflow(t *testing.T) {
	// Small buffer to test overflow
	recorder := NewRingBufferRecorder(5)

	// Record more than capacity
	for i := 0; i < 10; i++ {
		op := AuditOperation{
			RequestID:      "job-" + string(rune('0'+i)),
			ScenarioType:   ScenarioEncoderUpgrade,
			DecisionReason: "test",
		}
		recorder.Record(op)
	}

	// Should only have last 5 records
	if recorder.Size() != 5 {
		t.Errorf("Expected size 5, got %d", recorder.Size())
	}

	// First 5 should be overwritten
	for i := 0; i < 5; i++ {
		_, err := recorder.Query("job-" + string(rune('0'+i)))
		if err != ErrNotFound {
			t.Errorf("job-%d should be overwritten", i)
		}
	}

	// Last 5 should exist
	for i := 5; i < 10; i++ {
		_, err := recorder.Query("job-" + string(rune('0'+i)))
		if err != nil {
			t.Errorf("job-%d should exist: %v", i, err)
		}
	}
}

// TestIntegrationTimeRangeQuery tests querying by time range.
func TestIntegrationTimeRangeQuery(t *testing.T) {
	recorder := NewRingBufferRecorder(100)

	// Record operations at specific times
	baseTime := time.Now()
	for i := 0; i < 5; i++ {
		op := AuditOperation{
			RequestID:      "job-001",
			Timestamp:      baseTime.Add(time.Duration(i) * time.Hour),
			ScenarioType:   ScenarioEncoderUpgrade,
			DecisionReason: "test",
		}
		recorder.Record(op)
	}

	// Query middle range
	start := baseTime.Add(30 * time.Minute)
	end := baseTime.Add(3*time.Hour + 30*time.Minute)

	ops, err := recorder.QueryByTimeRange(start, end)
	if err != nil {
		t.Fatalf("QueryByTimeRange failed: %v", err)
	}
	if len(ops) != 3 {
		t.Errorf("Expected 3 operations in range, got %d", len(ops))
	}
}
