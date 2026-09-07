package workerhealth

import (
	"sort"
	"sync"
	"time"

	"github.com/tsic404/rffmpeg/pkg/protocol"
)

// WorkerStateTable maintains an in-memory map of worker states
// populated by heartbeat messages. Thread-safe.
type WorkerStateTable struct {
	mu             sync.RWMutex
	states         map[string]*protocol.WorkerState
	offlineTimeout time.Duration
	ewmaAlpha      float64 // EWMA smoothing factor (0 < α ≤ 1)
}

// DefaultEWMAAlpha is the default smoothing factor for EWMA throughput calculation.
const DefaultEWMAAlpha = 0.4

// NewWorkerStateTable creates a new state table.
func NewWorkerStateTable(offlineTimeout time.Duration) *WorkerStateTable {
	if offlineTimeout <= 0 {
		offlineTimeout = 30 * time.Second
	}
	return &WorkerStateTable{
		states:         make(map[string]*protocol.WorkerState),
		offlineTimeout: offlineTimeout,
		ewmaAlpha:      DefaultEWMAAlpha,
	}
}

// UpdateFromHeartbeat updates or inserts a worker state from a heartbeat payload.
// Computes EWMA-smoothed throughput if the worker already has a previous EWMA value.
func (t *WorkerStateTable) UpdateFromHeartbeat(payload protocol.WorkerHeartbeatPayload) {
	t.mu.Lock()
	defer t.mu.Unlock()

	state, exists := t.states[payload.WorkerID]
	if !exists {
		state = &protocol.WorkerState{
			WorkerID:  payload.WorkerID,
			StartedAt: payload.Timestamp,
		}
		t.states[payload.WorkerID] = state
	}

	state.Status = payload.Status
	if payload.GPUMetricsValid {
		state.GPUUtilPct = payload.GPUUtilPct
		state.GPUMemUsedMB = payload.GPUMemUsedMB
		state.GPUMetricsValid = true
	} else {
		state.GPUMetricsValid = false
	}
	state.ActiveJobs = payload.ActiveJobs
	state.ThroughputFPS = payload.ThroughputFPS
	state.CompletedJobs = payload.CompletedJobs
	state.QueueDepth = payload.QueueDepth
	state.LastSeen = payload.Timestamp

	// Compute EWMA-smoothed throughput
	if exists {
		if payload.ThroughputFPS > 0 {
			// Track when the worker last produced a real throughput sample:
			// the busy-with-zero-EWMA eviction exemption is time-boxed by this,
			// so a hung ffmpeg (jobs active, throughput dead) cannot hide behind
			// "measurement absence" forever.
			state.LastThroughputAt = payload.Timestamp
		}
		// Apply EWMA smoothing: EWMA = α * current + (1-α) * previous
		alpha := t.ewmaAlpha
		state.EWMAThroughput = alpha*payload.ThroughputFPS + (1-alpha)*state.EWMAThroughput
	} else {
		// First data point: initialize EWMA directly
		state.EWMAThroughput = payload.ThroughputFPS
		state.LastThroughputAt = payload.Timestamp
	}
}

// Get returns the state for a worker.
func (t *WorkerStateTable) Get(workerID string) (*protocol.WorkerState, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	state, ok := t.states[workerID]
	return state, ok
}

// GetAll returns all worker states.
func (t *WorkerStateTable) GetAll() []protocol.WorkerState {
	t.mu.RLock()
	defer t.mu.RUnlock()

	result := make([]protocol.WorkerState, 0, len(t.states))
	for _, s := range t.states {
		result = append(result, *s)
	}
	return result
}

// ScanOffline marks workers as offline if they haven't been seen within the timeout.
// Returns the list of worker IDs that were marked offline.
func (t *WorkerStateTable) ScanOffline() []string {
	t.mu.Lock()
	defer t.mu.Unlock()

	now := time.Now()
	var offlineIDs []string
	for id, state := range t.states {
		if state.Status == string(protocol.WorkerStatusOffline) {
			continue
		}
		if now.Sub(state.LastSeen) > t.offlineTimeout {
			state.Status = string(protocol.WorkerStatusOffline)
			offlineIDs = append(offlineIDs, id)
		}
	}
	return offlineIDs
}

// RemoveWorker removes a worker from the table.
func (t *WorkerStateTable) RemoveWorker(workerID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.states, workerID)
}

// SlowNodeThreshold is the factor by which a worker's EWMA throughput must be below
// the cluster median to be marked as a slow node. A value of 3 means the worker's
// throughput is less than median/3.
const SlowNodeThreshold = 3.0

// RecoveryThreshold is the factor by which a previously evicted worker's EWMA throughput
// must be within the cluster median to be un-marked. A value of 1.5 means the worker's
// throughput is above median/1.5.
const RecoveryThreshold = 1.5

// MinJobsForEviction is the minimum number of jobs a worker must have completed
// before it becomes eligible for slow-node eviction. Workers still in their warmup
// phase (fewer than this many completed jobs) are excluded from the median
// calculation and are never marked as slow, preventing cold-start false positives.
const MinJobsForEviction = 5

// SlowNodeDetectionResult holds the result of slow node detection.
type SlowNodeDetectionResult struct {
	NewlyEvicted []string
	Recovered    []string
	Median       float64
}

// DetectSlowWorkers scans all worker states, computes the cluster median of EWMA-smoothed
// throughput, and marks workers as evicted if their throughput is below median/SlowNodeThreshold.
// Previously evicted workers are recovered if their throughput exceeds median/RecoveryThreshold.
// Workers with zero throughput and no active jobs (idle) are excluded from the median calculation
// and are not marked as slow — they are simply skipped. Newly registered workers that have
// completed fewer than MinJobsForEviction jobs are also excluded, so cold-start throughput
// (which is not yet representative) cannot trigger a false eviction.

// workerInfo is the per-worker view DetectSlowWorkers evaluates.
type workerInfo struct {
	id      string
	ewmaFPS float64
	hasJobs bool
}

func (t *WorkerStateTable) DetectSlowWorkers() SlowNodeDetectionResult {
	t.mu.Lock()
	defer t.mu.Unlock()

	// Collect EWMA throughput values from active, non-offline workers
	var activeWorkers []workerInfo

	for id, state := range t.states {
		// Skip offline workers
		if state.Status == string(protocol.WorkerStatusOffline) {
			continue
		}
		// Skip new workers still in their warmup phase
		if state.CompletedJobs < MinJobsForEviction {
			continue
		}
		hasJobs := len(state.ActiveJobs) > 0
		activeWorkers = append(activeWorkers, workerInfo{
			id:      id,
			ewmaFPS: state.EWMAThroughput,
			hasJobs: hasJobs,
		})
	}

	// Need at least 2 workers with throughput to compute a meaningful median
	var throughputs []float64
	var eligibleWorkers []workerInfo
	for _, w := range activeWorkers {
		// Skip idle workers: zero throughput AND no active jobs
		if w.ewmaFPS == 0 && !w.hasJobs {
			continue
		}
		throughputs = append(throughputs, w.ewmaFPS)
		eligibleWorkers = append(eligibleWorkers, w)
	}

	if len(throughputs) < 2 {
		// Not enough data to compute a meaningful median; clear eviction on all
		// active workers. Report previously evicted workers as recovered so the
		// monitor syncs the DB — otherwise a worker is stuck with its DB eviction
		// flag set (excluded from scheduling) while showing an idle status.
		var recovered []string
		for _, w := range activeWorkers {
			if st, ok := t.states[w.id]; ok && st.Evicted {
				st.Evicted = false
				recovered = append(recovered, w.id)
			}
		}
		return SlowNodeDetectionResult{Recovered: recovered, Median: 0}
	}

	// Compute median
	sort.Float64s(throughputs)
	median := computeMedian(throughputs)

	// Detect slow nodes and handle recovery
	var newlyEvicted []string
	var recovered []string
	for _, w := range eligibleWorkers {
		state := t.states[w.id]

		if state.Evicted {
			// Recovery check: if throughput is within RecoveryThreshold of median
			if median > 0 && w.ewmaFPS >= median/RecoveryThreshold {
				state.Evicted = false
				recovered = append(recovered, w.id)
			}
		} else if w.hasJobs && w.ewmaFPS == 0 && t.busyExempt(w) {
			// Busy worker with no throughput samples yet (job just started):
			// EWMA decay is measurement absence, not slowness — exempt from
			// eviction until real samples arrive, but only for a bounded
			// window (see busyExpiry): a hung ffmpeg with active jobs must
			// eventually face the slow-node safety net instead of hiding in
			// "measurement absence" forever.
			continue
		} else {
			// Slow node detection: if throughput is below median/SlowNodeThreshold
			if median > 0 && w.ewmaFPS < median/SlowNodeThreshold {
				state.Evicted = true
				newlyEvicted = append(newlyEvicted, w.id)
			}
		}
	}

	return SlowNodeDetectionResult{
		NewlyEvicted: newlyEvicted,
		Recovered:    recovered,
		Median:       median,
	}
}

// busyExpiry bounds how long a busy worker with zero EWMA throughput is
// exempt from slow-node eviction. Generous enough to cover a long-running
// job's startup ramp; short enough that a hung ffmpeg (jobs active, no
// samples) eventually reaches the eviction safety net.
const busyExpiry = 10 * time.Minute

// busyExempt reports whether a busy, zero-throughput worker still counts as
// "no samples yet". Once the worker has gone busyExpiry without a single
// non-zero throughput sample while holding active jobs, the exemption lapses
// and normal eviction evaluation applies.
func (t *WorkerStateTable) busyExempt(w workerInfo) bool {
	state, ok := t.states[w.id]
	if !ok {
		return true
	}
	if state.LastThroughputAt.IsZero() {
		// Never produced a sample: exempt only within the window since first sight.
		return time.Since(state.StartedAt) < busyExpiry
	}
	return time.Since(state.LastThroughputAt) < busyExpiry
}

// computeMedian returns the median value of a sorted slice of float64s.
func computeMedian(sorted []float64) float64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	if n%2 == 1 {
		return sorted[n/2]
	}
	// Even number of elements: average the two middle values
	return (sorted[n/2-1] + sorted[n/2]) / 2
}
