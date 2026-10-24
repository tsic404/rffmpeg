package workerhealth

import (
	"math"
	"testing"
	"time"

	"github.com/tsic404/rffmpeg/pkg/protocol"
)

func TestWorkerStateTable_UpdateAndGet(t *testing.T) {
	table := NewWorkerStateTable(30 * time.Second)

	payload := protocol.WorkerHeartbeatPayload{
		WorkerID:        "worker-1",
		Status:          "online",
		GPUUtilPct:      75.5,
		GPUMemUsedMB:    4096,
		GPUMetricsValid: true,
		ActiveJobs:      []string{"job-1", "job-2"},
		JobsPerSec:      120.0,
		QueueDepth:      3,
		Timestamp:       time.Now(),
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
	if state.JobsPerSec != 120.0 {
		t.Errorf("expected JobsPerSec 120.0, got %f", state.JobsPerSec)
	}
	// First heartbeat should initialize EWMA to the raw value
	if state.EWMAJobsPerSec != 120.0 {
		t.Errorf("expected EWMAJobsPerSec 120.0 on first heartbeat, got %f", state.EWMAJobsPerSec)
	}
}

func TestWorkerStateTable_EWMAConvergence(t *testing.T) {
	table := NewWorkerStateTable(30 * time.Second)

	// Simulate a series of heartbeats with varying throughput
	// First heartbeat: raw=100, EWMA=100
	table.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
		WorkerID: "w1", Status: "online", JobsPerSec: 100, Timestamp: time.Now(),
	})

	state, _ := table.Get("w1")
	if state.EWMAJobsPerSec != 100.0 {
		t.Fatalf("expected initial EWMA 100, got %f", state.EWMAJobsPerSec)
	}

	// Second heartbeat: raw=50, EWMA = 0.4*50 + 0.6*100 = 20 + 60 = 80
	table.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
		WorkerID: "w1", Status: "online", JobsPerSec: 50, Timestamp: time.Now(),
	})

	state, _ = table.Get("w1")
	expected := 0.4*50 + 0.6*100 // = 80
	if math.Abs(state.EWMAJobsPerSec-expected) > 0.001 {
		t.Errorf("expected EWMA %f, got %f", expected, state.EWMAJobsPerSec)
	}

	// Third heartbeat: raw=50, EWMA = 0.4*50 + 0.6*80 = 20 + 48 = 68
	table.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
		WorkerID: "w1", Status: "online", JobsPerSec: 50, Timestamp: time.Now(),
	})

	state, _ = table.Get("w1")
	expected2 := 0.4*50 + 0.6*80 // = 68
	if math.Abs(state.EWMAJobsPerSec-expected2) > 0.001 {
		t.Errorf("expected EWMA %f, got %f", expected2, state.EWMAJobsPerSec)
	}

	// Verify raw JobsPerSec is not smoothed
	if state.JobsPerSec != 50.0 {
		t.Errorf("expected raw JobsPerSec 50, got %f", state.JobsPerSec)
	}
}

func TestWorkerStateTable_EWMAZeroToNonZero(t *testing.T) {
	table := NewWorkerStateTable(30 * time.Second)

	// First heartbeat with zero throughput
	table.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
		WorkerID: "w1", Status: "online", JobsPerSec: 0, Timestamp: time.Now(),
	})

	state, _ := table.Get("w1")
	if state.EWMAJobsPerSec != 0.0 {
		t.Errorf("expected EWMA 0, got %f", state.EWMAJobsPerSec)
	}

	// Second heartbeat with non-zero throughput
	table.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
		WorkerID: "w1", Status: "online", JobsPerSec: 100, Timestamp: time.Now(),
	})

	state, _ = table.Get("w1")
	// Since initial EWMA was 0, 0.4*100 + 0.6*0 = 40
	expected := 0.4 * 100.0 // 40
	if math.Abs(state.EWMAJobsPerSec-expected) > 0.001 {
		t.Errorf("expected EWMA %f, got %f", expected, state.EWMAJobsPerSec)
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
		WorkerID: "w1", Status: "online", JobsPerSec: 100, Timestamp: time.Now(), CompletedJobs: MinJobsForEviction,
	})
	table.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
		WorkerID: "w2", Status: "online", JobsPerSec: 110, Timestamp: time.Now(), CompletedJobs: MinJobsForEviction,
	})
	table.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
		WorkerID: "w3", Status: "online", JobsPerSec: 90, Timestamp: time.Now(), CompletedJobs: MinJobsForEviction,
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
		WorkerID: "w1", Status: "online", JobsPerSec: 100, Timestamp: time.Now(), CompletedJobs: MinJobsForEviction,
	})
	table.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
		WorkerID: "w2", Status: "online", JobsPerSec: 110, Timestamp: time.Now(), CompletedJobs: MinJobsForEviction,
	})
	table.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
		WorkerID: "w3", Status: "online", JobsPerSec: 10, Timestamp: time.Now(), CompletedJobs: MinJobsForEviction,
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

// TestDetectSlowWorkers_BurstyWorkerStaysInMedianPool pins the sample-pool rule
// for a worker whose reported throughput alternates between a burst and a zero:
// throughput is "jobs completed since the previous heartbeat", so a fast worker
// that finishes a job and then waits out its poll interval reports exactly that
// pattern. Excluding it on the zero sample leaves the median computed from one
// worker only, which zeroes the median and silently disables slow-node
// detection instead of merely missing that worker for one interval.
func TestDetectSlowWorkers_BurstyWorkerStaysInMedianPool(t *testing.T) {
	table := NewWorkerStateTable(30 * time.Second)

	// ws reports its scaled throughput every interval and holds a long job.
	// wf alternates burst/zero with no active job on the zero samples.
	for range 3 {
		table.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
			WorkerID: "ws", Status: "busy", JobsPerSec: 0.05,
			ActiveJobs: []string{"long-job"}, CompletedJobs: MinJobsForEviction, Timestamp: time.Now(),
		})
		table.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
			WorkerID: "wf", Status: "busy", JobsPerSec: 12,
			CompletedJobs: MinJobsForEviction, Timestamp: time.Now(),
		})
		table.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
			WorkerID: "wf", Status: "idle", JobsPerSec: 0,
			CompletedJobs: MinJobsForEviction, Timestamp: time.Now(),
		})
	}

	result := table.DetectSlowWorkers()
	if result.Median <= 0 {
		t.Fatalf("median = %f: the bursty worker's zero sample collapsed the pool", result.Median)
	}
	if len(result.NewlyEvicted) != 1 || result.NewlyEvicted[0] != "ws" {
		t.Fatalf("newly evicted = %v, want [ws] (median %f)", result.NewlyEvicted, result.Median)
	}
	if state, _ := table.Get("wf"); state.Evicted {
		t.Error("the bursty worker must not be evicted on its between-bursts zero sample")
	}
}

// TestDetectSlowWorkers_SustainedIdleLeavesMedianPool pins the other side of the
// same rule: a worker that keeps reporting zero with no active jobs is idle, so
// it leaves the median pool and is never judged against a live peer's median.
func TestDetectSlowWorkers_SustainedIdleLeavesMedianPool(t *testing.T) {
	table := NewWorkerStateTable(30 * time.Second)

	table.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
		WorkerID: "w-busy", Status: "busy", JobsPerSec: 10,
		ActiveJobs: []string{"job-1"}, CompletedJobs: MinJobsForEviction, Timestamp: time.Now(),
	})
	// A long-idle worker keeps decaying but never exactly reaches zero.
	for range 20 {
		table.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
			WorkerID: "w-idle", Status: "idle", JobsPerSec: 0,
			CompletedJobs: MinJobsForEviction, Timestamp: time.Now(),
		})
	}

	state, _ := table.Get("w-idle")
	if state.ZeroSampleStreak < MinZeroSamplesForIdle {
		t.Fatalf("precondition: idle worker zero-sample streak = %d, want >= %d",
			state.ZeroSampleStreak, MinZeroSamplesForIdle)
	}

	result := table.DetectSlowWorkers()
	if len(result.NewlyEvicted) != 0 {
		t.Errorf("idle worker must not be evicted on its decayed EWMA, got %v", result.NewlyEvicted)
	}
	if got := table.ClusterMedian(); got != 0 {
		t.Errorf("cluster median = %f, want 0: only one sampled worker remains", got)
	}
}

// TestDetectSlowWorkers_MedianPoolZeroSampleBoundary pins the exact point a
// worker without active jobs leaves the median sample pool: it is still
// sampled on its MinZeroSamplesForIdle-1'th consecutive zero heartbeat and
// drops out on the next one. An off-by-one there shortens the window in which
// an idle worker is exempt from judgement to a single sample, while every
// coarser test still passes because a genuinely idle worker accumulates far
// more zeros than the threshold.
func TestDetectSlowWorkers_MedianPoolZeroSampleBoundary(t *testing.T) {
	if MinZeroSamplesForIdle != 2 {
		t.Fatalf("MinZeroSamplesForIdle = %d, want 2: the boundary asserted below is defined by this value",
			MinZeroSamplesForIdle)
	}

	table := NewWorkerStateTable(30 * time.Second)

	// Two steady producers whose EWMA settles at 10 and 12.
	for range 3 {
		table.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
			WorkerID: "w-low", Status: "online", JobsPerSec: 10,
			CompletedJobs: MinJobsForEviction, Timestamp: time.Now(),
		})
		table.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
			WorkerID: "w-high", Status: "online", JobsPerSec: 12,
			CompletedJobs: MinJobsForEviction, Timestamp: time.Now(),
		})
	}

	// w-edge produces at 20, then stops while holding no job: each zero sample
	// decays its EWMA (20 -> 12 -> 7.2), so its value moves the median only
	// through pool membership.
	for range 2 {
		table.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
			WorkerID: "w-edge", Status: "online", JobsPerSec: 20,
			CompletedJobs: MinJobsForEviction, Timestamp: time.Now(),
		})
	}
	table.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
		WorkerID: "w-edge", Status: "idle", JobsPerSec: 0,
		CompletedJobs: MinJobsForEviction, Timestamp: time.Now(),
	})

	state, _ := table.Get("w-edge")
	if state.ZeroSampleStreak != MinZeroSamplesForIdle-1 {
		t.Fatalf("precondition: zero-sample streak = %d, want %d",
			state.ZeroSampleStreak, MinZeroSamplesForIdle-1)
	}
	// {10, 12, 12} -> 12; dropping w-edge would leave {10, 12} -> 11.
	if got := table.ClusterMedian(); math.Abs(got-12) > 1e-9 {
		t.Errorf("median with a %d-zero-sample worker = %f, want 12: the worker must still be in the pool",
			state.ZeroSampleStreak, got)
	}

	table.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
		WorkerID: "w-edge", Status: "idle", JobsPerSec: 0,
		CompletedJobs: MinJobsForEviction, Timestamp: time.Now(),
	})

	state, _ = table.Get("w-edge")
	if state.ZeroSampleStreak != MinZeroSamplesForIdle {
		t.Fatalf("precondition: zero-sample streak = %d, want %d",
			state.ZeroSampleStreak, MinZeroSamplesForIdle)
	}
	// {10, 12} -> 11; keeping w-edge in with its decayed 7.2 would give 10.
	if got := table.ClusterMedian(); math.Abs(got-11) > 1e-9 {
		t.Errorf("median with a %d-zero-sample worker = %f, want 11: the worker must have left the pool",
			state.ZeroSampleStreak, got)
	}
}

func TestDetectSlowWorkers_IdleWorkersSkipped(t *testing.T) {
	table := NewWorkerStateTable(30 * time.Second)

	// w1: active with normal throughput
	table.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
		WorkerID: "w1", Status: "online", JobsPerSec: 100, ActiveJobs: []string{"job-1"}, Timestamp: time.Now(), CompletedJobs: MinJobsForEviction,
	})
	// w2: idle - zero throughput and no active jobs
	table.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
		WorkerID: "w2", Status: "online", JobsPerSec: 0, ActiveJobs: nil, Timestamp: time.Now(), CompletedJobs: MinJobsForEviction,
	})
	// w3: active with normal throughput
	table.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
		WorkerID: "w3", Status: "online", JobsPerSec: 110, ActiveJobs: []string{"job-2"}, Timestamp: time.Now(), CompletedJobs: MinJobsForEviction,
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
		WorkerID: "w1", Status: "online", JobsPerSec: 100, ActiveJobs: []string{"job-1"}, Timestamp: time.Now(), CompletedJobs: MinJobsForEviction,
	})
	// w2: busy worker whose job just started — throughput sample is still 0.
	// EWMA decay here is measurement absence, not slowness, so w2 is exempt
	// from eviction evaluation until real throughput samples arrive.
	table.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
		WorkerID: "w2", Status: "online", JobsPerSec: 0, ActiveJobs: []string{"job-2"}, Timestamp: time.Now(), CompletedJobs: MinJobsForEviction,
	})
	// w3: active with normal throughput
	table.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
		WorkerID: "w3", Status: "online", JobsPerSec: 110, ActiveJobs: []string{"job-3"}, Timestamp: time.Now(), CompletedJobs: MinJobsForEviction,
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

// TestDetectSlowWorkers_BusyWorkerWithoutSample pins the busy-worker exemption
// to the last real sample: a worker holding a job that reports no throughput
// this interval is not judged on its decayed EWMA while that sample is fresh,
// and is judged once busyExpiry lapses — so a hung ffmpeg still reaches the
// eviction safety net instead of hiding behind measurement absence forever.
func TestDetectSlowWorkers_BusyWorkerWithoutSample(t *testing.T) {
	table := NewWorkerStateTable(30 * time.Second)

	// Healthy peers keep the median high; both are busy and report throughput.
	for _, id := range []string{"w1", "w2"} {
		table.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
			WorkerID: id, Status: "online", JobsPerSec: 100,
			ActiveJobs: []string{"job-" + id}, CompletedJobs: MinJobsForEviction, Timestamp: time.Now(),
		})
	}

	// w-fresh sampled 1 job/sec just now; its next job has produced no sample yet.
	for range 2 {
		table.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
			WorkerID: "w-fresh", Status: "online", JobsPerSec: 1,
			ActiveJobs: []string{"job-fresh"}, CompletedJobs: MinJobsForEviction, Timestamp: time.Now(),
		})
	}
	table.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
		WorkerID: "w-fresh", Status: "online", JobsPerSec: 0,
		ActiveJobs: []string{"job-fresh"}, CompletedJobs: MinJobsForEviction, Timestamp: time.Now(),
	})

	// w-hung's last real sample predates busyExpiry; its job reports nothing since.
	table.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
		WorkerID: "w-hung", Status: "online", JobsPerSec: 1,
		ActiveJobs: []string{"job-hung"}, CompletedJobs: MinJobsForEviction,
		Timestamp: time.Now().Add(-busyExpiry - time.Minute),
	})
	table.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
		WorkerID: "w-hung", Status: "online", JobsPerSec: 0,
		ActiveJobs: []string{"job-hung"}, CompletedJobs: MinJobsForEviction, Timestamp: time.Now(),
	})

	result := table.DetectSlowWorkers()

	if len(result.NewlyEvicted) != 1 || result.NewlyEvicted[0] != "w-hung" {
		t.Fatalf("expected only w-hung evicted once the exemption lapsed, got %v", result.NewlyEvicted)
	}
	if state, _ := table.Get("w-fresh"); state.Evicted {
		t.Error("busy worker within busyExpiry of its last sample should not be evicted")
	}
}

// TestDetectSlowWorkers_SingleZeroSampleAfterSilence pins the idle branch to
// the zero-sample rule rather than to the busy exemption: a worker holding no
// job whose last real sample is older than busyExpiry, and which then reports
// a single zero, is idle — the expired exemption must not hand it to the
// eviction threshold in place of the one-zero-sample exemption.
func TestDetectSlowWorkers_SingleZeroSampleAfterSilence(t *testing.T) {
	table := NewWorkerStateTable(30 * time.Second)

	for _, id := range []string{"w1", "w2"} {
		table.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
			WorkerID: id, Status: "busy", JobsPerSec: 100,
			ActiveJobs: []string{"job-" + id}, CompletedJobs: MinJobsForEviction, Timestamp: time.Now(),
		})
	}

	table.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
		WorkerID: "w-quiet", Status: "idle", JobsPerSec: 2,
		CompletedJobs: MinJobsForEviction, Timestamp: time.Now().Add(-busyExpiry - time.Minute),
	})
	table.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
		WorkerID: "w-quiet", Status: "idle", JobsPerSec: 0,
		CompletedJobs: MinJobsForEviction, Timestamp: time.Now(),
	})

	state, _ := table.Get("w-quiet")
	if state.ZeroSampleStreak != MinZeroSamplesForIdle-1 {
		t.Fatalf("precondition: zero-sample streak = %d, want %d",
			state.ZeroSampleStreak, MinZeroSamplesForIdle-1)
	}

	result := table.DetectSlowWorkers()
	if len(result.NewlyEvicted) != 0 {
		t.Errorf("newly evicted = %v, want none: one zero sample is idleness, not a hung worker", result.NewlyEvicted)
	}
	if state.Evicted {
		t.Error("idle worker with a single zero sample must not be evicted once its last real sample expired")
	}
}

func TestDetectSlowWorkers_NewWorkerWarmup(t *testing.T) {
	table := NewWorkerStateTable(30 * time.Second)

	// Two established workers with healthy throughput.
	table.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
		WorkerID: "w1", Status: "online", JobsPerSec: 100, CompletedJobs: MinJobsForEviction, Timestamp: time.Now(),
	})
	table.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
		WorkerID: "w2", Status: "online", JobsPerSec: 110, CompletedJobs: MinJobsForEviction, Timestamp: time.Now(),
	})
	// New worker still in its warmup phase (below MinJobsForEviction) with very low throughput.
	table.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
		WorkerID: "w3", Status: "online", JobsPerSec: 5, ActiveJobs: []string{"job-1"}, CompletedJobs: MinJobsForEviction - 1, Timestamp: time.Now(),
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
		WorkerID: "w3", Status: "online", JobsPerSec: 5, ActiveJobs: []string{"job-2"}, CompletedJobs: MinJobsForEviction, Timestamp: time.Now(),
	})

	result = table.DetectSlowWorkers()
	if len(result.NewlyEvicted) != 1 || result.NewlyEvicted[0] != "w3" {
		t.Fatalf("expected w3 to be evicted once warmed up, got %v", result.NewlyEvicted)
	}
}

func TestDetectSlowWorkers_SingleWorker(t *testing.T) {
	table := NewWorkerStateTable(30 * time.Second)

	table.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
		WorkerID: "w1", Status: "online", JobsPerSec: 100, Timestamp: time.Now(), CompletedJobs: MinJobsForEviction,
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

func TestDetectSlowWorkers_PoolCollapseKeepsEviction(t *testing.T) {
	table := NewWorkerStateTable(30 * time.Second)

	// Two workers: w2 is slow and gets evicted.
	table.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
		WorkerID: "w1", Status: "online", JobsPerSec: 100, Timestamp: time.Now(), CompletedJobs: MinJobsForEviction,
	})
	table.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
		WorkerID: "w2", Status: "online", JobsPerSec: 10, Timestamp: time.Now(), CompletedJobs: MinJobsForEviction,
	})

	result := table.DetectSlowWorkers()
	if len(result.NewlyEvicted) != 1 || result.NewlyEvicted[0] != "w2" {
		t.Fatalf("expected w2 to be evicted, got %v", result.NewlyEvicted)
	}

	// The median sample pool collapses to a single worker (w1 leaves). Too few
	// samples to judge means no decision at all: the mark must survive, since
	// only throughput recovery may clear it.
	table.RemoveWorker("w1")

	result = table.DetectSlowWorkers()
	if len(result.Recovered) != 0 {
		t.Fatalf("pool collapse must not report recovery, got %v", result.Recovered)
	}
	if result.Median != 0 {
		t.Errorf("expected no median with fewer than two samples, got %f", result.Median)
	}
	state, _ := table.Get("w2")
	if !state.Evicted {
		t.Error("w2 eviction mark must persist while the sample pool is too small to judge")
	}
}

func TestDetectSlowWorkers_Recovery(t *testing.T) {
	table := NewWorkerStateTable(30 * time.Second)

	// Mark w3 as slow, then improve its throughput to trigger recovery
	table.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
		WorkerID: "w1", Status: "online", JobsPerSec: 100, Timestamp: time.Now(), CompletedJobs: MinJobsForEviction,
	})
	table.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
		WorkerID: "w2", Status: "online", JobsPerSec: 110, Timestamp: time.Now(), CompletedJobs: MinJobsForEviction,
	})
	table.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
		WorkerID: "w3", Status: "online", JobsPerSec: 10, Timestamp: time.Now(), CompletedJobs: MinJobsForEviction,
	})

	// First detection: w3 should be evicted
	result := table.DetectSlowWorkers()
	if len(result.NewlyEvicted) != 1 || result.NewlyEvicted[0] != "w3" {
		t.Fatalf("expected w3 to be evicted initially, got %v", result.NewlyEvicted)
	}

	// Now simulate w3 recovering: throughput improves to 80
	table.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
		WorkerID: "w3", Status: "online", JobsPerSec: 80, Timestamp: time.Now(), CompletedJobs: MinJobsForEviction,
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
			WorkerID: "w3", Status: "online", JobsPerSec: 80, Timestamp: time.Now(), CompletedJobs: MinJobsForEviction,
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
		WorkerID: "w1", Status: "online", JobsPerSec: 100, Timestamp: time.Now(), CompletedJobs: MinJobsForEviction,
	})
	// w2: offline, very slow
	table.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
		WorkerID: "w2", Status: string(protocol.WorkerStatusOffline), JobsPerSec: 5, Timestamp: time.Now(), CompletedJobs: MinJobsForEviction,
	})
	// w3: online, normal
	table.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
		WorkerID: "w3", Status: "online", JobsPerSec: 110, Timestamp: time.Now(), CompletedJobs: MinJobsForEviction,
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
		WorkerID: "w1", Status: "online", JobsPerSec: 100, Timestamp: time.Now(), CompletedJobs: MinJobsForEviction,
	})
	table.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
		WorkerID: "w2", Status: "online", JobsPerSec: 200, Timestamp: time.Now(), CompletedJobs: MinJobsForEviction,
	})
	table.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
		WorkerID: "w3", Status: "online", JobsPerSec: 300, Timestamp: time.Now(), CompletedJobs: MinJobsForEviction,
	})
	table.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
		WorkerID: "w4", Status: "online", JobsPerSec: 400, Timestamp: time.Now(), CompletedJobs: MinJobsForEviction,
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
		WorkerID: "w1", Status: "online", JobsPerSec: 10, Timestamp: time.Now(), CompletedJobs: MinJobsForEviction,
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
		WorkerID: "w1", Status: "online", JobsPerSec: 0, Timestamp: time.Now(), CompletedJobs: MinJobsForEviction,
	})
	table.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
		WorkerID: "w2", Status: "online", JobsPerSec: 0, Timestamp: time.Now(), CompletedJobs: MinJobsForEviction,
	})
	table.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
		WorkerID: "w3", Status: "online", JobsPerSec: 0, Timestamp: time.Now(), CompletedJobs: MinJobsForEviction,
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
		WorkerID: "w1", Status: "online", JobsPerSec: 0, ActiveJobs: []string{"job-1"}, Timestamp: time.Now(), CompletedJobs: MinJobsForEviction,
	})
	table.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
		WorkerID: "w2", Status: "online", JobsPerSec: 0, ActiveJobs: []string{"job-2"}, Timestamp: time.Now(), CompletedJobs: MinJobsForEviction,
	})

	// Median is 0 → no eviction (median > 0 guard)
	result := table.DetectSlowWorkers()
	if len(result.NewlyEvicted) != 0 {
		t.Errorf("expected 0 evicted (median=0), got %d: %v", len(result.NewlyEvicted), result.NewlyEvicted)
	}
}

func TestWorkerStateTableClusterMedian(t *testing.T) {
	table := NewWorkerStateTable(30 * time.Second)

	// Fewer than two eligible samples → 0.
	if got := table.ClusterMedian(); got != 0 {
		t.Fatalf("empty table median = %f, want 0", got)
	}

	// Two past-warmup, non-idle workers: median of EWMA throughput.
	table.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
		WorkerID: "w-fast", Status: "online", JobsPerSec: 3, Timestamp: time.Now(), CompletedJobs: MinJobsForEviction,
	})
	table.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
		WorkerID: "w-slow", Status: "online", JobsPerSec: 1, Timestamp: time.Now(), CompletedJobs: MinJobsForEviction,
	})
	if got := table.ClusterMedian(); got != 2 {
		t.Errorf("median of [1 3] = %f, want 2", got)
	}

	// Warmup worker (CompletedJobs < MinJobsForEviction) is excluded from the
	// sample pool, so its extreme value must not move the median.
	table.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
		WorkerID: "w-warmup", Status: "online", JobsPerSec: 1000, Timestamp: time.Now(), CompletedJobs: MinJobsForEviction - 1,
	})
	if got := table.ClusterMedian(); got != 2 {
		t.Errorf("median with warmup worker = %f, want 2", got)
	}

	// Idle worker (zero throughput, no active jobs) is excluded too.
	table.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
		WorkerID: "w-idle", Status: "online", JobsPerSec: 0, Timestamp: time.Now(), CompletedJobs: MinJobsForEviction,
	})
	if got := table.ClusterMedian(); got != 2 {
		t.Errorf("median with idle worker = %f, want 2", got)
	}
}
