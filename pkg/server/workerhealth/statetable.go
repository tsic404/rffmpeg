package workerhealth

import (
	"sort"
	"sync"
	"time"

	"github.com/tsix404/rffmpeg/pkg/protocol"
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
	state.GPUUtilPct = payload.GPUUtilPct
	state.GPUMemUsedMB = payload.GPUMemUsedMB
	state.ActiveJobs = payload.ActiveJobs
	state.ThroughputFPS = payload.ThroughputFPS
	state.QueueDepth = payload.QueueDepth
	state.LastSeen = payload.Timestamp

	// Compute EWMA-smoothed throughput
	if exists {
		// Apply EWMA smoothing: EWMA = α * current + (1-α) * previous
		alpha := t.ewmaAlpha
		state.EWMAThroughput = alpha*payload.ThroughputFPS + (1-alpha)*state.EWMAThroughput
	} else {
		// First data point: initialize EWMA directly
		state.EWMAThroughput = payload.ThroughputFPS
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
// and are not marked as slow — they are simply skipped.
//
// Returns SlowNodeDetectionResult containing newly evicted IDs, recovered IDs, and the computed median.
func (t *WorkerStateTable) DetectSlowWorkers() SlowNodeDetectionResult {
	t.mu.Lock()
	defer t.mu.Unlock()

	// Collect EWMA throughput values from active, non-offline workers
	type workerInfo struct {
		id      string
		ewmaFPS float64
		hasJobs bool
	}
	var activeWorkers []workerInfo

	for id, state := range t.states {
		// Skip offline workers
		if state.Status == string(protocol.WorkerStatusOffline) {
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
		for _, w := range activeWorkers {
			if st, ok := t.states[w.id]; ok {
				st.Evicted = false
			}
		}
		return SlowNodeDetectionResult{Median: 0}
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
