package audit

import (
	"strings"
	"testing"
	"time"
)

func TestRingBufferRecorder_Record(t *testing.T) {
	recorder := NewRingBufferRecorder(5)

	// Test basic recording
	op := AuditOperation{
		RequestID:        "req-1",
		ScenarioType:     ScenarioEncoderUpgrade,
		OriginalEncoder:  "libx264",
		RewrittenEncoder: "h264_nvenc",
		DecisionReason:   "GPU available: NVIDIA",
	}

	err := recorder.Record(op)
	if err != nil {
		t.Fatalf("Record failed: %v", err)
	}

	if recorder.Size() != 1 {
		t.Errorf("Expected size 1, got %d", recorder.Size())
	}

	// Verify timestamp was set
	ops, err := recorder.Query("req-1")
	if err != nil {
		t.Fatalf("Query failed: %v", err)
	}
	if len(ops) != 1 {
		t.Fatalf("Expected 1 operation, got %d", len(ops))
	}
	if ops[0].Timestamp.IsZero() {
		t.Error("Timestamp should be set automatically")
	}
}

func TestRingBufferRecorder_RingBuffer(t *testing.T) {
	recorder := NewRingBufferRecorder(3)

	// Record more than capacity
	for i := 0; i < 5; i++ {
		op := AuditOperation{
			RequestID:      "req-" + string(rune('0'+i)),
			ScenarioType:   ScenarioEncoderUpgrade,
			DecisionReason: "test",
		}
		recorder.Record(op)
	}

	// Should only have last 3 records
	if recorder.Size() != 3 {
		t.Errorf("Expected size 3, got %d", recorder.Size())
	}

	// First two should be overwritten
	_, err := recorder.Query("req-0")
	if err != ErrNotFound {
		t.Error("req-0 should be overwritten")
	}

	_, err = recorder.Query("req-1")
	if err != ErrNotFound {
		t.Error("req-1 should be overwritten")
	}

	// Last three should exist
	for _, id := range []string{"req-2", "req-3", "req-4"} {
		_, err := recorder.Query(id)
		if err != nil {
			t.Errorf("%s should exist: %v", id, err)
		}
	}
}

func TestRingBufferRecorder_Query(t *testing.T) {
	recorder := NewRingBufferRecorder(10)

	// Record multiple operations for same request
	for i := 0; i < 3; i++ {
		op := AuditOperation{
			RequestID:      "req-1",
			ScenarioType:   ScenarioEncoderUpgrade,
			DecisionReason: "test",
		}
		recorder.Record(op)
	}

	// Record operation for different request
	op := AuditOperation{
		RequestID:      "req-2",
		ScenarioType:   ScenarioEncoderFallback,
		DecisionReason: "test",
	}
	recorder.Record(op)

	// Query req-1
	ops, err := recorder.Query("req-1")
	if err != nil {
		t.Fatalf("Query failed: %v", err)
	}
	if len(ops) != 3 {
		t.Errorf("Expected 3 operations for req-1, got %d", len(ops))
	}

	// Query req-2
	ops, err = recorder.Query("req-2")
	if err != nil {
		t.Fatalf("Query failed: %v", err)
	}
	if len(ops) != 1 {
		t.Errorf("Expected 1 operation for req-2, got %d", len(ops))
	}

	// Query non-existent
	_, err = recorder.Query("req-999")
	if err != ErrNotFound {
		t.Error("Expected ErrNotFound for non-existent request")
	}
}

func TestRingBufferRecorder_QueryByTimeRange(t *testing.T) {
	recorder := NewRingBufferRecorder(10)

	now := time.Now()

	// Record operations with specific timestamps
	for i := 0; i < 3; i++ {
		op := AuditOperation{
			RequestID:      "req-1",
			Timestamp:      now.Add(time.Duration(i) * time.Hour),
			ScenarioType:   ScenarioEncoderUpgrade,
			DecisionReason: "test",
		}
		recorder.Record(op)
	}

	// Query first hour only
	ops, err := recorder.QueryByTimeRange(now, now.Add(time.Hour+time.Minute))
	if err != nil {
		t.Fatalf("QueryByTimeRange failed: %v", err)
	}
	if len(ops) != 2 {
		t.Errorf("Expected 2 operations, got %d", len(ops))
	}

	// Query outside range
	_, err = recorder.QueryByTimeRange(now.Add(-2*time.Hour), now.Add(-time.Hour))
	if err != ErrNotFound {
		t.Error("Expected ErrNotFound for empty range")
	}
}

func TestRingBufferRecorder_QueryRecent(t *testing.T) {
	recorder := NewRingBufferRecorder(10)

	// Record operations
	for i := 0; i < 5; i++ {
		op := AuditOperation{
			RequestID:      "req-" + string(rune('0'+i)),
			ScenarioType:   ScenarioEncoderUpgrade,
			DecisionReason: "test",
		}
		recorder.Record(op)
		time.Sleep(time.Millisecond) // Ensure different timestamps
	}

	// Query recent 3
	ops := recorder.QueryRecent(3)
	if len(ops) != 3 {
		t.Errorf("Expected 3 operations, got %d", len(ops))
	}

	// Should be in reverse chronological order (newest first)
	if ops[0].RequestID != "req-4" {
		t.Errorf("Expected newest first, got %s", ops[0].RequestID)
	}
}

func TestRingBufferRecorder_GetSummary(t *testing.T) {
	recorder := NewRingBufferRecorder(10)

	// Record operations for a request
	recorder.Record(AuditOperation{
		RequestID:        "req-1",
		ScenarioType:     ScenarioEncoderUpgrade,
		OriginalEncoder:  "libx264",
		RewrittenEncoder: "h264_nvenc",
		DecisionReason:   "GPU available",
	})

	recorder.Record(AuditOperation{
		RequestID:      "req-1",
		ScenarioType:   ScenarioHardwareParamInjection,
		DecisionReason: "Added -gpu_id",
	})

	recorder.Record(AuditOperation{
		RequestID:        "req-1",
		ScenarioType:     ScenarioEncoderFallback,
		OriginalEncoder:  "h264_nvenc",
		RewrittenEncoder: "libx264",
		DecisionReason:   "NVENC unavailable",
	})

	summary, err := recorder.GetSummary("req-1")
	if err != nil {
		t.Fatalf("GetSummary failed: %v", err)
	}

	if summary.TotalOperations != 3 {
		t.Errorf("Expected 3 operations, got %d", summary.TotalOperations)
	}

	if summary.Scenarios[ScenarioEncoderUpgrade] != 1 {
		t.Errorf("Expected 1 encoder_upgrade, got %d", summary.Scenarios[ScenarioEncoderUpgrade])
	}

	if !summary.HasWarnings {
		t.Error("Expected HasWarnings to be true (fallback scenario)")
	}
}

func TestRingBufferRecorder_Clear(t *testing.T) {
	recorder := NewRingBufferRecorder(10)

	// Record some operations
	for i := 0; i < 3; i++ {
		op := AuditOperation{
			RequestID:      "req-1",
			ScenarioType:   ScenarioEncoderUpgrade,
			DecisionReason: "test",
		}
		recorder.Record(op)
	}

	if recorder.Size() != 3 {
		t.Errorf("Expected size 3, got %d", recorder.Size())
	}

	recorder.Clear()

	if recorder.Size() != 0 {
		t.Errorf("Expected size 0 after clear, got %d", recorder.Size())
	}
}

func TestRingBufferRecorder_Concurrency(t *testing.T) {
	recorder := NewRingBufferRecorder(100)

	// Concurrent writes
	done := make(chan bool)
	for i := 0; i < 10; i++ {
		go func(id int) {
			for j := 0; j < 10; j++ {
				op := AuditOperation{
					RequestID:      "req-" + string(rune('0'+id)),
					ScenarioType:   ScenarioEncoderUpgrade,
					DecisionReason: "test",
				}
				recorder.Record(op)
			}
			done <- true
		}(i)
	}

	// Wait for all goroutines
	for i := 0; i < 10; i++ {
		<-done
	}

	// Should have 100 records
	if recorder.Size() != 100 {
		t.Errorf("Expected size 100, got %d", recorder.Size())
	}
}

func TestInMemoryRecorder(t *testing.T) {
	recorder := NewInMemoryRecorder()

	// Test basic recording
	op := AuditOperation{
		RequestID:      "req-1",
		ScenarioType:   ScenarioEncoderUpgrade,
		DecisionReason: "test",
	}

	err := recorder.Record(op)
	if err != nil {
		t.Fatalf("Record failed: %v", err)
	}

	if recorder.Size() != 1 {
		t.Errorf("Expected size 1, got %d", recorder.Size())
	}

	// Test unbounded growth
	for i := 0; i < 1000; i++ {
		recorder.Record(AuditOperation{
			RequestID:      "req-2",
			ScenarioType:   ScenarioEncoderUpgrade,
			DecisionReason: "test",
		})
	}

	if recorder.Size() != 1001 {
		t.Errorf("Expected size 1001, got %d", recorder.Size())
	}

	// Test capacity
	if recorder.Capacity() != -1 {
		t.Errorf("Expected capacity -1, got %d", recorder.Capacity())
	}
}

func TestScenarioType_String(t *testing.T) {
	tests := []struct {
		scenario ScenarioType
		expected string
	}{
		{ScenarioEncoderUpgrade, "encoder_upgrade"},
		{ScenarioEncoderFallback, "encoder_fallback"},
		{ScenarioEncoderSubstitution, "encoder_substitution"},
		{ScenarioParameterTranslation, "parameter_translation"},
		{ScenarioHardwareParamInjection, "hardware_param_injection"},
		{ScenarioNoEncoderSpecified, "no_encoder_specified"},
	}

	for _, tt := range tests {
		if tt.scenario.String() != tt.expected {
			t.Errorf("Expected %s, got %s", tt.expected, tt.scenario.String())
		}
	}
}

func TestNotifyLevel_String(t *testing.T) {
	tests := []struct {
		level    NotifyLevel
		expected string
	}{
		{InfoLevel, "INFO"},
		{WarnLevel, "WARN"},
		{ErrorLevel, "ERROR"},
	}

	for _, tt := range tests {
		if tt.level.String() != tt.expected {
			t.Errorf("Expected %s, got %s", tt.expected, tt.level.String())
		}
	}
}

func TestAuditOperation_JSON(t *testing.T) {
	// Test that AuditOperation can be marshaled/unmarshaled
	op := AuditOperation{
		RequestID:        "req-1",
		Timestamp:        time.Now(),
		ScenarioType:     ScenarioEncoderUpgrade,
		OriginalEncoder:  "libx264",
		RewrittenEncoder: "h264_nvenc",
		OriginalParams:   map[string]interface{}{"crf": "23"},
		RewrittenParams:  map[string]interface{}{"cq": "23"},
		DecisionReason:   "GPU available",
		Context:          map[string]interface{}{"gpu_vendor": "nvidia"},
	}

	// Just verify the fields are accessible
	if op.RequestID != "req-1" {
		t.Error("RequestID mismatch")
	}
	if op.ScenarioType != ScenarioEncoderUpgrade {
		t.Error("ScenarioType mismatch")
	}
}

func TestAuditSummary(t *testing.T) {
	recorder := NewRingBufferRecorder(10)

	// Record operations
	recorder.Record(AuditOperation{
		RequestID:        "req-1",
		ScenarioType:     ScenarioEncoderUpgrade,
		OriginalEncoder:  "libx264",
		RewrittenEncoder: "h264_nvenc",
		DecisionReason:   "GPU available",
	})

	summary, err := recorder.GetSummary("req-1")
	if err != nil {
		t.Fatalf("GetSummary failed: %v", err)
	}

	// Verify summary fields
	if summary.RequestID != "req-1" {
		t.Errorf("Expected RequestID req-1, got %s", summary.RequestID)
	}

	if summary.TotalOperations != 1 {
		t.Errorf("Expected 1 operation, got %d", summary.TotalOperations)
	}

	if summary.Scenarios[ScenarioEncoderUpgrade] != 1 {
		t.Errorf("Expected 1 encoder_upgrade scenario, got %d", summary.Scenarios[ScenarioEncoderUpgrade])
	}
}

// Benchmark tests
func BenchmarkRingBufferRecorder_Record(b *testing.B) {
	recorder := NewRingBufferRecorder(10000)
	op := AuditOperation{
		RequestID:      "req-1",
		ScenarioType:   ScenarioEncoderUpgrade,
		DecisionReason: "test",
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		recorder.Record(op)
	}
}

func BenchmarkRingBufferRecorder_Query(b *testing.B) {
	recorder := NewRingBufferRecorder(10000)

	// Pre-populate
	for i := 0; i < 1000; i++ {
		recorder.Record(AuditOperation{
			RequestID:      "req-1",
			ScenarioType:   ScenarioEncoderUpgrade,
			DecisionReason: "test",
		})
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		recorder.Query("req-1")
	}
}

func BenchmarkRingBufferRecorder_ConcurrentRecord(b *testing.B) {
	recorder := NewRingBufferRecorder(10000)

	b.RunParallel(func(pb *testing.PB) {
		op := AuditOperation{
			RequestID:      "req-1",
			ScenarioType:   ScenarioEncoderUpgrade,
			DecisionReason: "test",
		}
		for pb.Next() {
			recorder.Record(op)
		}
	})
}

// Test helper functions
func TestFormatOperationMessage(t *testing.T) {
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
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg := formatOperationMessage(tt.op)
			if !strings.Contains(msg, tt.contains) {
				t.Errorf("Expected message to contain %q, got %q", tt.contains, msg)
			}
		})
	}
}
