package handlers

import (
	"reflect"
	"testing"

	"github.com/tsic404/rffmpeg/pkg/protocol"
	"github.com/tsic404/rffmpeg/pkg/server/db"
)

// TestBuildWorkerSummariesAndOverrides_Homogeneous verifies that when every
// worker shares the executing worker's encoder list, no overrides are emitted
// (TSI-3048: a homogeneous cluster serializes a single shared list).
func TestBuildWorkerSummariesAndOverrides_Homogeneous(t *testing.T) {
	shared := []string{"libx264", "h264_nvenc"}
	allWorkers := []*db.Worker{
		{ID: "worker-1", Name: "gpu-1", Status: protocol.WorkerStatusIdle, Encoders: `["libx264","h264_nvenc"]`, FFmpegVersion: "5.1", MaxConcurrent: 2},
		{ID: "worker-2", Name: "gpu-2", Status: protocol.WorkerStatusBusy, Encoders: `["libx264","h264_nvenc"]`, FFmpegVersion: "5.1", MaxConcurrent: 4},
	}

	summaries, overrides := buildWorkerSummariesAndOverrides(allWorkers, shared)

	if len(summaries) != 2 {
		t.Fatalf("summaries length = %d, want 2", len(summaries))
	}
	if len(overrides) != 0 {
		t.Errorf("overrides length = %d, want 0 for a homogeneous cluster: %v", len(overrides), overrides)
	}
}

// TestBuildWorkerSummariesAndOverrides_Heterogeneous verifies that only workers
// whose encoder list differs from the shared list appear in the overrides map.
func TestBuildWorkerSummariesAndOverrides_Heterogeneous(t *testing.T) {
	shared := []string{"libx264", "h264_nvenc"}
	allWorkers := []*db.Worker{
		{ID: "worker-1", Name: "gpu-node", Status: protocol.WorkerStatusIdle, Encoders: `["libx264","h264_nvenc"]`, FFmpegVersion: "5.1", MaxConcurrent: 2},
		{ID: "worker-2", Name: "cpu-node", Status: protocol.WorkerStatusBusy, Encoders: `["libx265"]`, FFmpegVersion: "6.0", MaxConcurrent: 4},
	}

	summaries, overrides := buildWorkerSummariesAndOverrides(allWorkers, shared)

	if len(summaries) != 2 {
		t.Fatalf("summaries length = %d, want 2", len(summaries))
	}
	if len(overrides) != 1 {
		t.Fatalf("overrides length = %d, want 1 (only worker-2 differs): %v", len(overrides), overrides)
	}
	if _, has := overrides["worker-1"]; has {
		t.Errorf("overrides must not contain the shared worker-1: %v", overrides)
	}
	if got := overrides["worker-2"]; !reflect.DeepEqual(got, []string{"libx265"}) {
		t.Errorf("overrides[worker-2] = %v, want [libx265]", got)
	}
}

// TestBuildWorkerSummariesAndOverrides_InvalidJSON ensures a malformed encoder
// column degrades to an empty list rather than failing the whole response.
func TestBuildWorkerSummariesAndOverrides_InvalidJSON(t *testing.T) {
	shared := []string{"libx264"}
	allWorkers := []*db.Worker{
		{ID: "worker-1", Status: protocol.WorkerStatusIdle, Encoders: `not-json`},
	}

	summaries, overrides := buildWorkerSummariesAndOverrides(allWorkers, shared)
	if len(summaries) != 1 {
		t.Fatalf("summaries length = %d, want 1", len(summaries))
	}
	// Empty (nil-derived) list differs from the shared list, so it lands in
	// overrides as an empty slice, not a crash.
	if got := overrides["worker-1"]; len(got) != 0 {
		t.Errorf("overrides[worker-1] = %v, want empty for invalid JSON", got)
	}
}
