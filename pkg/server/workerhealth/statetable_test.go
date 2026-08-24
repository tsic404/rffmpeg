package workerhealth

import (
	"math"
	"testing"
	"time"

	"github.com/tsix404/rffmpeg/pkg/protocol"
)

func TestWorkerStateTable_UpdateAndGet(t *testing.T) {
	table := NewWorkerStateTable(30 * time.Second)

	payload := protocol.WorkerHeartbeatPayload{
		WorkerID:      "worker-1",
		Status:        "online",
		GPUUtilPct:    75.5,
		GPUMemUsedMB:  4096,
		ActiveJobs:    []string{"job-1", "job-2"},
		ThroughputFPS: 120.0,
		QueueDepth:    3,
		Timestamp:     time.Now(),
	}

	table.UpdateFromHeartbeat(payload)

	state, ok := table.Get("worker-1")
	if !ok {
		t.Fatal("expected worker-1 to exist")
	}

	if state.Status != "online" {
		t.Errorf("expected status online, got %s", state.Status)
	}
	if state.GPUUtilPct != 75.5 {
		t.Errorf("expected GPUUtilPct 75.5, got %f", state.GPUUtilPct)
	}
	if len(state.ActiveJobs) != 2 {
		t.Errorf("expected 2 active jobs, got %d", len(state.ActiveJobs))
	}
	if state.ThroughputFPS != 120.0 {
		t.Errorf("expected ThroughputFPS 120.0, got %f", state.ThroughputFPS)
	}
	// First heartbeat should initialize EWMA to the raw value
	if state.EWMAThroughput != 120.0 {
		t.Errorf("expected EWMAThroughput 120.0 on first heartbeat, got %f", state.EWMAThroughput)
	}
}

func TestWorkerStateTable_EWMAConvergence(t *testing.T) {
	table := NewWorkerStateTable(30 * time.Second)

	// Simulate a series of heartbeats with varying throughput
	// First heartbeat: raw=100, EWMA=100
	table.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
		WorkerID: "w1", Status: "online", ThroughputFPS: 100, Timestamp: time.Now(),
	})

	state, _ := table.Get("w1")
	if state.EWMAThroughput != 100.0 {
		t.Fatalf("expected initial EWMA 100, got %f", state.EWMAThroughput)
	}

	// Second heartbeat: raw=50, EWMA = 0.4*50 + 0.6*100 = 20 + 60 = 80
	table.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
		WorkerID: "w1", Status: "online", ThroughputFPS: 50, Timestamp: time.Now(),
	})

	state, _ = table.Get("w1")
	expected := 0.4*50 + 0.6*100 // = 80
	if math.Abs(state.EWMAThroughput-expected) > 0.001 {
		t.Errorf("expected EWMA %f, got %f", expected, state.EWMAThroughput)
	}

	// Third heartbeat: raw=50, EWMA = 0.4*50 + 0.6*80 = 20 + 48 = 68
	table.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
		WorkerID: "w1", Status: "online", ThroughputFPS: 50, Timestamp: time.Now(),
	})

	state, _ = table.Get("w1")
	expected2 := 0.4*50 + 0.6*80 // = 68
	if math.Abs(state.EWMAThroughput-expected2) > 0.001 {
		t.Errorf("expected EWMA %f, got %f", expected2, state.EWMAThroughput)
	}

	// Verify raw ThroughputFPS is not smoothed
	if state.ThroughputFPS != 50.0 {
		t.Errorf("expected raw ThroughputFPS 50, got %f", state.ThroughputFPS)
	}
}

func TestWorkerStateTable_EWMAZeroToNonZero(t *testing.T) {
	table := NewWorkerStateTable(30 * time.Second)

	// First heartbeat with zero throughput
	table.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
		WorkerID: "w1", Status: "online", ThroughputFPS: 0, Timestamp: time.Now(),
	})

	state, _ := table.Get("w1")
	if state.EWMAThroughput != 0.0 {
		t.Errorf("expected EWMA 0, got %f", state.EWMAThroughput)
	}

	// Second heartbeat with non-zero throughput
	table.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
		WorkerID: "w1", Status: "online", ThroughputFPS: 100, Timestamp: time.Now(),
	})

	state, _ = table.Get("w1")
	// Since initial EWMA was 0, 0.4*100 + 0.6*0 = 40
	expected := 0.4 * 100.0 // 40
	if math.Abs(state.EWMAThroughput-expected) > 0.001 {
		t.Errorf("expected EWMA %f, got %f", expected, state.EWMAThroughput)
	}
}

func TestWorkerStateTable_GetAll(t *testing.T) {
	table := NewWorkerStateTable(30 * time.Second)

	table.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
		WorkerID: "worker-1", Status: "online", Timestamp: time.Now(),
	})
	table.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
		WorkerID: "worker-2", Status: "busy", Timestamp: time.Now(),
	})

	all := table.GetAll()
	if len(all) != 2 {
		t.Errorf("expected 2 workers, got %d", len(all))
	}
}

func TestWorkerStateTable_ScanOffline(t *testing.T) {
	table := NewWorkerStateTable(100 * time.Millisecond)

	table.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
		WorkerID: "worker-1", Status: "online", Timestamp: time.Now().Add(-1 * time.Second),
	})

	offlineIDs := table.ScanOffline()
	if len(offlineIDs) != 1 {
		t.Errorf("expected 1 offline worker, got %d", len(offlineIDs))
	}
	if offlineIDs[0] != "worker-1" {
		t.Errorf("expected worker-1 offline, got %s", offlineIDs[0])
	}
}

func TestWorkerStateTable_RemoveWorker(t *testing.T) {
	table := NewWorkerStateTable(30 * time.Second)

	table.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
		WorkerID: "worker-1", Status: "online", Timestamp: time.Now(),
	})

	table.RemoveWorker("worker-1")

	_, ok := table.Get("worker-1")
	if ok {
		t.Error("expected worker-1 to be removed")
	}
}

// --- Slow Node Detection Tests ---

func TestDetectSlowWorkers_NormalCluster(t *testing.T) {
	table := NewWorkerStateTable(30 * time.Second)

	// 3 workers with similar throughput
	table.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
		WorkerID: "w1", Status: "online", ThroughputFPS: 100, Timestamp: time.Now(), CompletedJobs: MinJobsForEviction,
	})
	table.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
		WorkerID: "w2", Status: "online", ThroughputFPS: 110, Timestamp: time.Now(), CompletedJobs: MinJobsForEviction,
	})
	table.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
		WorkerID: "w3", Status: "online", ThroughputFPS: 90, Timestamp: time.Now(), CompletedJobs: MinJobsForEviction,
	})

	result := table.DetectSlowWorkers()
	if len(result.NewlyEvicted) != 0 {
		t.Errorf("expected 0 evicted workers, got %d: %v", len(result.NewlyEvicted), result.NewlyEvicted)
	}

	// Verify none are evicted
	for _, id := range []string{"w1", "w2", "w3"} {
		state, _ := table.Get(id)
		if state.Evicted {
			t.Errorf("worker %s should not be evicted", id)
		}
	}
}

func TestDetectSlowWorkers_SlowNodeDetected(t *testing.T) {
	table := NewWorkerStateTable(30 * time.Second)

	// 3 workers: w3 is significantly slower (throughput 10 vs median 100)
	table.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
		WorkerID: "w1", Status: "online", ThroughputFPS: 100, Timestamp: time.Now(), CompletedJobs: MinJobsForEviction,
	})
	table.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
		WorkerID: "w2", Status: "online", ThroughputFPS: 110, Timestamp: time.Now(), CompletedJobs: MinJobsForEviction,
	})
	table.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
		WorkerID: "w3", Status: "online", ThroughputFPS: 10, Timestamp: time.Now(), CompletedJobs: MinJobsForEviction,
	})

	result := table.DetectSlowWorkers()
	if len(result.NewlyEvicted) != 1 {
		t.Fatalf("expected 1 evicted worker, got %d: %v", len(result.NewlyEvicted), result.NewlyEvicted)
	}
	if result.NewlyEvicted[0] != "w3" {
		t.Errorf("expected w3 to be evicted, got %s", result.NewlyEvicted[0])
	}

	// Verify states
	state3, _ := table.Get("w3")
	if !state3.Evicted {
		t.Error("w3 should be evicted")
	}
	state1, _ := table.Get("w1")
	if state1.Evicted {
		t.Error("w1 should not be evicted")
	}
}

func TestDetectSlowWorkers_IdleWorkersSkipped(t *testing.T) {
	table := NewWorkerStateTable(30 * time.Second)

	// w1: active with normal throughput
	table.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
		WorkerID: "w1", Status: "online", ThroughputFPS: 100, ActiveJobs: []string{"job-1"}, Timestamp: time.Now(), CompletedJobs: MinJobsForEviction,
	})
	// w2: idle - zero throughput and no active jobs
	table.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
		WorkerID: "w2", Status: "online", ThroughputFPS: 0, ActiveJobs: nil, Timestamp: time.Now(), CompletedJobs: MinJobsForEviction,
	})
	// w3: active with normal throughput
	table.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
		WorkerID: "w3", Status: "online", ThroughputFPS: 110, ActiveJobs: []string{"job-2"}, Timestamp: time.Now(), CompletedJobs: MinJobsForEviction,
	})

	result := table.DetectSlowWorkers()
	if len(result.NewlyEvicted) != 0 {
		t.Errorf("expected 0 evicted, w2 should be skipped (idle), got %d: %v", len(result.NewlyEvicted), result.NewlyEvicted)
	}

	// w2 should NOT be evicted (idle workers are skipped, not marked)
	state2, _ := table.Get("w2")
	if state2.Evicted {
		t.Error("idle worker w2 should not be evicted")
	}
}

func TestDetectSlowWorkers_IdleWorkerWithActiveJobs(t *testing.T) {
	table := NewWorkerStateTable(30 * time.Second)

	// w1: active with normal throughput
	table.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
		WorkerID: "w1", Status: "online", ThroughputFPS: 100, ActiveJobs: []string{"job-1"}, Timestamp: time.Now(), CompletedJobs: MinJobsForEviction,
	})
	// w2: busy worker whose job just started — throughput sample is still 0.
	// EWMA decay here is measurement absence, not slowness, so w2 is exempt
	// from eviction evaluation until real throughput samples arrive.
	table.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
		WorkerID: "w2", Status: "online", ThroughputFPS: 0, ActiveJobs: []string{"job-2"}, Timestamp: time.Now(), CompletedJobs: MinJobsForEviction,
	})
	// w3: active with normal throughput
	table.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
		WorkerID: "w3", Status: "online", ThroughputFPS: 110, ActiveJobs: []string{"job-3"}, Timestamp: time.Now(), CompletedJobs: MinJobsForEviction,
	})

	// Median of [100, 110] → median=105. w2 (busy, zero throughput) is
	// exempt from eviction evaluation — a job that just started has no
	// throughput samples yet, and EWMA decay is measurement absence.
	result := table.DetectSlowWorkers()

	if len(result.NewlyEvicted) != 0 {
		t.Fatalf("expected 0 evicted workers, got %d: %v", len(result.NewlyEvicted), result.NewlyEvicted)
	}
	state2, _ := table.Get("w2")
	if state2.Evicted {
		t.Error("busy worker w2 with no throughput samples should be exempt from eviction")
	}
}

func TestDetectSlowWorkers_NewWorkerWarmup(t *testing.T) {
	table := NewWorkerStateTable(30 * time.Second)

	// Two established workers with healthy throughput.
	table.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
		WorkerID: "w1", Status: "online", ThroughputFPS: 100, CompletedJobs: MinJobsForEviction, Timestamp: time.Now(),
	})
	table.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
		WorkerID: "w2", Status: "online", ThroughputFPS: 110, CompletedJobs: MinJobsForEviction, Timestamp: time.Now(),
	})
	// New worker still in its warmup phase (below MinJobsForEviction) with very low throughput.
	table.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
		WorkerID: "w3", Status: "online", ThroughputFPS: 5, ActiveJobs: []string{"job-1"}, CompletedJobs: MinJobsForEviction - 1, Timestamp: time.Now(),
	})

	result := table.DetectSlowWorkers()
	if len(result.NewlyEvicted) != 0 {
		t.Fatalf("expected 0 evicted (w3 still warming up), got %d: %v", len(result.NewlyEvicted), result.NewlyEvicted)
	}
	state3, _ := table.Get("w3")
	if state3.Evicted {
		t.Error("new worker in warmup phase should not be evicted")
	}

	// Once w3 crosses the warmup threshold, it becomes eligible for eviction.
	table.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
		WorkerID: "w3", Status: "online", ThroughputFPS: 5, ActiveJobs: []string{"job-2"}, CompletedJobs: MinJobsForEviction, Timestamp: time.Now(),
	})

	result = table.DetectSlowWorkers()
	if len(result.NewlyEvicted) != 1 || result.NewlyEvicted[0] != "w3" {
		t.Fatalf("expected w3 to be evicted once warmed up, got %v", result.NewlyEvicted)
	}
}

func TestDetectSlowWorkers_SingleWorker(t *testing.T) {
	table := NewWorkerStateTable(30 * time.Second)

	table.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
		WorkerID: "w1", Status: "online", ThroughputFPS: 100, Timestamp: time.Now(), CompletedJobs: MinJobsForEviction,
	})

	result := table.DetectSlowWorkers()
	if len(result.NewlyEvicted) != 0 {
		t.Errorf("expected 0 evicted (single worker), got %d", len(result.NewlyEvicted))
	}

	state, _ := table.Get("w1")
	if state.Evicted {
		t.Error("single worker should not be evicted")
	}
}

func TestDetectSlowWorkers_SingleWorkerClearsEviction(t *testing.T) {
	table := NewWorkerStateTable(30 * time.Second)

	// Two workers: w2 is slow and gets evicted.
	table.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
		WorkerID: "w1", Status: "online", ThroughputFPS: 100, Timestamp: time.Now(), CompletedJobs: MinJobsForEviction,
	})
	table.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
		WorkerID: "w2", Status: "online", ThroughputFPS: 10, Timestamp: time.Now(), CompletedJobs: MinJobsForEviction,
	})

	result := table.DetectSlowWorkers()
	if len(result.NewlyEvicted) != 1 || result.NewlyEvicted[0] != "w2" {
		t.Fatalf("expected w2 to be evicted, got %v", result.NewlyEvicted)
	}

	// Cluster shrinks to a single worker (w1 leaves).
	table.RemoveWorker("w1")

	// The single remaining worker must have its eviction cleared and be
	// reported as recovered so the monitor can sync the DB.
	result = table.DetectSlowWorkers()
	if len(result.Recovered) != 1 || result.Recovered[0] != "w2" {
		t.Fatalf("expected w2 to be reported recovered, got %v", result.Recovered)
	}

	state, _ := table.Get("w2")
	if state.Evicted {
		t.Error("w2 should no longer be evicted")
	}
}

func TestDetectSlowWorkers_Recovery(t *testing.T) {
	table := NewWorkerStateTable(30 * time.Second)

	// Mark w3 as slow, then improve its throughput to trigger recovery
	table.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
		WorkerID: "w1", Status: "online", ThroughputFPS: 100, Timestamp: time.Now(), CompletedJobs: MinJobsForEviction,
	})
	table.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
		WorkerID: "w2", Status: "online", ThroughputFPS: 110, Timestamp: time.Now(), CompletedJobs: MinJobsForEviction,
	})
	table.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
		WorkerID: "w3", Status: "online", ThroughputFPS: 10, Timestamp: time.Now(), CompletedJobs: MinJobsForEviction,
	})

	// First detection: w3 should be evicted
	result := table.DetectSlowWorkers()
	if len(result.NewlyEvicted) != 1 || result.NewlyEvicted[0] != "w3" {
		t.Fatalf("expected w3 to be evicted initially, got %v", result.NewlyEvicted)
	}

	// Now simulate w3 recovering: throughput improves to 80
	table.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
		WorkerID: "w3", Status: "online", ThroughputFPS: 80, Timestamp: time.Now(), CompletedJobs: MinJobsForEviction,
	})

	// After EWMA smoothing: 0.4*80 + 0.6*10 = 32 + 6 = 38 (not enough for recovery yet)
	// Run detection again
	result2 := table.DetectSlowWorkers()
	// EWMA=38, median of [100, 110, 38] = 100, 38 < 100/3=33.3? No, 38 > 33.3 → w3 might recover
	// Actually wait: 38 < 100/1.5=66.7? Yes. And 38 > 100/3=33.3? Yes.
	// So w3 is still evicted but hasn't fully recovered
	_ = result2

	// Simulate several more heartbeats at 80 to bring EWMA up
	for i := 0; i < 10; i++ {
		table.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
			WorkerID: "w3", Status: "online", ThroughputFPS: 80, Timestamp: time.Now(), CompletedJobs: MinJobsForEviction,
		})
	}

	// Now EWMA should be close to 80
	result3 := table.DetectSlowWorkers()
	if len(result3.NewlyEvicted) != 0 {
		t.Errorf("expected w3 to recover, got evicted: %v", result3.NewlyEvicted)
	}
	if len(result3.Recovered) != 1 || result3.Recovered[0] != "w3" {
		t.Errorf("expected w3 to be in recovered list, got %v", result3.Recovered)
	}

	state3, _ := table.Get("w3")
	if state3.Evicted {
		t.Error("w3 should have recovered (no longer evicted)")
	}
}

func TestDetectSlowWorkers_OfflineWorkersSkipped(t *testing.T) {
	table := NewWorkerStateTable(30 * time.Second)

	// w1: online, normal
	table.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
		WorkerID: "w1", Status: "online", ThroughputFPS: 100, Timestamp: time.Now(), CompletedJobs: MinJobsForEviction,
	})
	// w2: offline, very slow
	table.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
		WorkerID: "w2", Status: string(protocol.WorkerStatusOffline), ThroughputFPS: 5, Timestamp: time.Now(), CompletedJobs: MinJobsForEviction,
	})
	// w3: online, normal
	table.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
		WorkerID: "w3", Status: "online", ThroughputFPS: 110, Timestamp: time.Now(), CompletedJobs: MinJobsForEviction,
	})

	result := table.DetectSlowWorkers()
	if len(result.NewlyEvicted) != 0 {
		t.Errorf("expected 0 evicted (offline w2 excluded), got %d: %v", len(result.NewlyEvicted), result.NewlyEvicted)
	}

	// w2 should not be evicted since it's offline
	state2, _ := table.Get("w2")
	if state2.Evicted {
		t.Error("offline worker should not be evicted")
	}
}

func TestDetectSlowWorkers_MedianWithEvenCount(t *testing.T) {
	table := NewWorkerStateTable(30 * time.Second)

	// 4 workers: median should be average of 2nd and 3rd
	table.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
		WorkerID: "w1", Status: "online", ThroughputFPS: 100, Timestamp: time.Now(), CompletedJobs: MinJobsForEviction,
	})
	table.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
		WorkerID: "w2", Status: "online", ThroughputFPS: 200, Timestamp: time.Now(), CompletedJobs: MinJobsForEviction,
	})
	table.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
		WorkerID: "w3", Status: "online", ThroughputFPS: 300, Timestamp: time.Now(), CompletedJobs: MinJobsForEviction,
	})
	table.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
		WorkerID: "w4", Status: "online", ThroughputFPS: 400, Timestamp: time.Now(), CompletedJobs: MinJobsForEviction,
	})

	// Sorted: [100, 200, 300, 400] → median = (200+300)/2 = 250
	// Threshold: 250/3 ≈ 83.3
	// w1=100 > 83.3 → not slow
	result := table.DetectSlowWorkers()
	if len(result.NewlyEvicted) != 0 {
		t.Errorf("expected 0 evicted, got %d: %v", len(result.NewlyEvicted), result.NewlyEvicted)
	}

	// Now make w1 very slow
	table.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
		WorkerID: "w1", Status: "online", ThroughputFPS: 10, Timestamp: time.Now(), CompletedJobs: MinJobsForEviction,
	})
	// EWMA after one update: 0.4*10 + 0.6*100 = 4 + 60 = 64
	// Median of [64, 200, 300, 400] = 250
	// 64 < 250/3=83.3 → slow
	evicted2 := table.DetectSlowWorkers()
	if len(evicted2.NewlyEvicted) != 1 || evicted2.NewlyEvicted[0] != "w1" {
		t.Errorf("expected w1 to be evicted, got %v", evicted2.NewlyEvicted)
	}
}

func TestComputeMedian(t *testing.T) {
	tests := []struct {
		name     string
		input    []float64
		expected float64
	}{
		{"empty", []float64{}, 0},
		{"single", []float64{5.0}, 5.0},
		{"odd count", []float64{1.0, 3.0, 5.0}, 3.0},
		{"even count", []float64{1.0, 2.0, 3.0, 4.0}, 2.5},
		{"two elements", []float64{10.0, 20.0}, 15.0},
		{"unsorted odd", []float64{5.0, 1.0, 3.0}, 3.0},
		{"unsorted even", []float64{4.0, 1.0, 3.0, 2.0}, 2.5},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// computeMedian expects a sorted slice; sort first
			sorted := make([]float64, len(tt.input))
			copy(sorted, tt.input)
			// Manual sort for the test to call computeMedian with sorted input
			for i := 0; i < len(sorted); i++ {
				for j := i + 1; j < len(sorted); j++ {
					if sorted[i] > sorted[j] {
						sorted[i], sorted[j] = sorted[j], sorted[i]
					}
				}
			}
			result := computeMedian(sorted)
			if math.Abs(result-tt.expected) > 0.0001 {
				t.Errorf("computeMedian(%v) = %f, want %f", tt.input, result, tt.expected)
			}
		})
	}
}

func TestDetectSlowWorkers_AllIdle(t *testing.T) {
	table := NewWorkerStateTable(30 * time.Second)

	// All workers are idle (zero throughput, no jobs)
	table.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
		WorkerID: "w1", Status: "online", ThroughputFPS: 0, Timestamp: time.Now(), CompletedJobs: MinJobsForEviction,
	})
	table.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
		WorkerID: "w2", Status: "online", ThroughputFPS: 0, Timestamp: time.Now(), CompletedJobs: MinJobsForEviction,
	})
	table.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
		WorkerID: "w3", Status: "online", ThroughputFPS: 0, Timestamp: time.Now(), CompletedJobs: MinJobsForEviction,
	})

	result := table.DetectSlowWorkers()
	if len(result.NewlyEvicted) != 0 {
		t.Errorf("expected 0 evicted (all idle), got %d: %v", len(result.NewlyEvicted), result.NewlyEvicted)
	}
}

func TestDetectSlowWorkers_ZeroMedian(t *testing.T) {
	table := NewWorkerStateTable(30 * time.Second)

	// All workers have zero throughput but have active jobs (not idle)
	table.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
		WorkerID: "w1", Status: "online", ThroughputFPS: 0, ActiveJobs: []string{"job-1"}, Timestamp: time.Now(), CompletedJobs: MinJobsForEviction,
	})
	table.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
		WorkerID: "w2", Status: "online", ThroughputFPS: 0, ActiveJobs: []string{"job-2"}, Timestamp: time.Now(), CompletedJobs: MinJobsForEviction,
	})

	// Median is 0 → no eviction (median > 0 guard)
	result := table.DetectSlowWorkers()
	if len(result.NewlyEvicted) != 0 {
		t.Errorf("expected 0 evicted (median=0), got %d: %v", len(result.NewlyEvicted), result.NewlyEvicted)
	}
}
