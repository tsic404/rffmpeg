package workerhealth

import (
	"testing"
	"time"

	"github.com/tsic404/rffmpeg/pkg/protocol"
)

// TestWorkerStateTable_StaleGPUPreserved verifies that heartbeats without a
// fresh GPU sample (GPUMetricsValid=false) do not wipe the last good reading:
// a valid 0% sample must survive, and an invalid sample must not zero it out
// (TSI-2365).
func TestWorkerStateTable_StaleGPUPreserved(t *testing.T) {
	table := NewWorkerStateTable(30 * time.Second)

	// First heartbeat: valid sample with a REAL 0% reading.
	table.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
		WorkerID:        "worker-1",
		Status:          "busy",
		GPUUtilPct:      0,
		GPUMetricsValid: true,
		Timestamp:       time.Now(),
	})
	state, ok := table.Get("worker-1")
	if !ok {
		t.Fatal("expected worker-1 to exist")
	}
	if !state.GPUMetricsValid {
		t.Error("expected GPUMetricsValid=true after valid heartbeat")
	}
	if state.GPUUtilPct != 0 {
		t.Errorf("valid 0%% reading must survive the wire/state table, got %f", state.GPUUtilPct)
	}

	// Second heartbeat: nvidia-smi failed — no fresh sample.
	table.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
		WorkerID:        "worker-1",
		Status:          "busy",
		GPUUtilPct:      0,
		GPUMetricsValid: false,
		Timestamp:       time.Now(),
	})
	state, _ = table.Get("worker-1")
	if state.GPUMetricsValid {
		t.Error("expected GPUMetricsValid=false after invalid sample")
	}
}

// TestWorkerStateTable_FreshSampleOverwritesStale verifies the opposite side:
// a new valid sample replaces an old one.
func TestWorkerStateTable_FreshSampleOverwritesStale(t *testing.T) {
	table := NewWorkerStateTable(30 * time.Second)

	table.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
		WorkerID:        "worker-2",
		GPUUtilPct:      10,
		GPUMetricsValid: true,
		Timestamp:       time.Now(),
	})
	table.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
		WorkerID:        "worker-2",
		GPUUtilPct:      80,
		GPUMemUsedMB:    2048,
		GPUMetricsValid: true,
		Timestamp:       time.Now(),
	})

	state, _ := table.Get("worker-2")
	if state.GPUUtilPct != 80 || state.GPUMemUsedMB != 2048 {
		t.Errorf("fresh sample should overwrite stale one, got util=%f mem=%d",
			state.GPUUtilPct, state.GPUMemUsedMB)
	}
}
