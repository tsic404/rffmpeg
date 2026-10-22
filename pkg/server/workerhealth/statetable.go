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
	state.JobsPerSec = payload.JobsPerSec
	state.CompletedJobs = payload.CompletedJobs
	state.QueueDepth = payload.QueueDepth
	state.LastSeen = payload.Timestamp

	// Track the run of zero-throughput heartbeats. A single zero sample is not
	// evidence that a worker stopped: a worker that completes a job and then
	// waits out its poll interval reports a burst sample and a zero sample on
	// alternating heartbeats. Median sample-pool membership needs the run
	// length to tell that cadence apart from genuine idleness.
	if payload.JobsPerSec > 0 {
		state.ZeroSampleStreak = 0
	} else {
		state.ZeroSampleStreak++
	}

	// Compute EWMA-smoothed throughput
	if exists {
		if payload.JobsPerSec > 0 {
			// Track when the worker last produced a real throughput sample:
			// the busy-with-zero-EWMA eviction exemption is time-boxed by this,
			// so a hung ffmpeg (jobs active, throughput dead) cannot hide behind
			// "measurement absence" forever.
			state.LastThroughputAt = payload.Timestamp
		}
		// Apply EWMA smoothing: EWMA = α * current + (1-α) * previous
		alpha := t.ewmaAlpha
		state.EWMAJobsPerSec = alpha*payload.JobsPerSec + (1-alpha)*state.EWMAJobsPerSec
	} else {
		// First data point: initialize EWMA directly
		state.EWMAJobsPerSec = payload.JobsPerSec
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

// MinZeroSamplesForIdle is how many consecutive zero-throughput heartbeats a
// worker without active jobs must report before it leaves the median sample
// pool. One zero sample cannot be distinguished from the gap between two job
// bursts, and EWMA decay keeps such a worker within the eviction threshold for
// this many steps anyway (0.6^2 > 1/3), so the pool keeps a producing worker
// through a sampling gap without softening the eviction rule.
const MinZeroSamplesForIdle = 2

// SlowNodeDetectionResult holds the result of slow node detection.
type SlowNodeDetectionResult struct {
	NewlyEvicted []string
	Recovered    []string
	Median       float64
}

// workerInfo is the per-worker view DetectSlowWorkers evaluates.
type workerInfo struct {
	id             string
	ewmaJobsPerSec float64
	// jobsPerSec is the raw throughput of the latest heartbeat. It is what
	// separates "no work this interval" from "slow work": the EWMA of a worker
	// that stopped producing decays toward zero without ever reaching it, so an
	// exact-zero test on the smoothed value cannot identify an idle worker.
	jobsPerSec float64
	hasJobs    bool
	// zeroStreak is how many consecutive heartbeats reported zero throughput.
	// One zero sample is a gap between job bursts, not idleness; the run length
	// is what tells the two apart.
	zeroStreak int
}

// eligibleSlowNodeWorkersLocked collects the workers eligible for slow-node
// evaluation: non-offline and past the warmup threshold. Caller must hold the
// lock (read or write).
func (t *WorkerStateTable) eligibleSlowNodeWorkersLocked() []workerInfo {
	var workers []workerInfo
	for id, state := range t.states {
		// Skip offline workers
		if state.Status == string(protocol.WorkerStatusOffline) {
			continue
		}
		// Skip new workers still in their warmup phase
		if state.CompletedJobs < MinJobsForEviction {
			continue
		}
		workers = append(workers, workerInfo{
			id:             id,
			ewmaJobsPerSec: state.EWMAJobsPerSec,
			jobsPerSec:     state.JobsPerSec,
			hasJobs:        len(state.ActiveJobs) > 0,
			zeroStreak:     state.ZeroSampleStreak,
		})
	}
	return workers
}

// medianSample reduces the eligible worker pool to the median sample pool. A
// worker without active jobs leaves it after MinZeroSamplesForIdle consecutive
// zero-throughput heartbeats, or as soon as it reports zero throughput while
// its EWMA is still zero — a worker that never produced a non-zero sample
// would only drag the median down. One zero sample is not idleness: throughput
// counts jobs completed since the previous heartbeat, so a worker that bursts
// and then waits out its poll interval alternates non-zero and zero, and
// excluding it on the zero collapses the pool and disables detection.
func medianSample(workers []workerInfo) ([]workerInfo, []float64) {
	sampled := make([]workerInfo, 0, len(workers))
	throughputs := make([]float64, 0, len(workers))
	for _, w := range workers {
		if !w.hasJobs && w.jobsPerSec == 0 {
			// No throughput this interval and no job running: either a gap
			// between job bursts or genuine idleness. A single zero sample is
			// the former — the EWMA remains a usable rate estimate — so the
			// worker only leaves the pool after MinZeroSamplesForIdle
			// consecutive zeros. A zero EWMA means no positive sample was ever
			// recorded, and its 0 would drag the median down instead of
			// describing the cluster.
			if w.zeroStreak >= MinZeroSamplesForIdle || w.ewmaJobsPerSec == 0 {
				continue
			}
		}
		sampled = append(sampled, w)
		throughputs = append(throughputs, w.ewmaJobsPerSec)
	}
	return sampled, throughputs
}

// DetectSlowWorkers scans all worker states, computes the cluster median of EWMA-smoothed
// throughput, and marks workers as evicted if their throughput is below median/SlowNodeThreshold.
// Previously evicted workers are recovered if their throughput exceeds median/RecoveryThreshold.
// A worker with no active jobs and zero throughput in its latest heartbeat is never marked slow:
// its EWMA is a decay tail of measurement absence, not a current rate. Pool membership is a
// separate decision made by medianSample — one zero sample stays in the pool, consecutive zeros
// leave it. Workers that completed fewer than MinJobsForEviction jobs are excluded from both the
// evaluation and the pool, so cold-start throughput cannot trigger a false eviction.
func (t *WorkerStateTable) DetectSlowWorkers() SlowNodeDetectionResult {
	t.mu.Lock()
	defer t.mu.Unlock()

	activeWorkers := t.eligibleSlowNodeWorkersLocked()
	eligibleWorkers, throughputs := medianSample(activeWorkers)

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
			if median > 0 && w.ewmaJobsPerSec >= median/RecoveryThreshold {
				state.Evicted = false
				recovered = append(recovered, w.id)
			}
		} else if w.jobsPerSec == 0 && !w.hasJobs {
			// Idle: the EWMA is a decay tail of measurement absence, not a
			// current rate, so it is never evidence of slowness. Pool
			// membership is medianSample's call: one zero sample stays in
			// the pool, consecutive zeros leave it; neither is judged here.
			continue
		} else if w.jobsPerSec == 0 && t.busyExempt(w) {
			// Busy worker with no throughput sample this interval (job just
			// started): EWMA decay is measurement absence, not slowness —
			// exempt from eviction until real samples arrive, but only for a
			// bounded window (see busyExpiry): a hung ffmpeg with active jobs
			// must eventually face the slow-node safety net instead of hiding
			// in "measurement absence" forever. Only busy workers reach this
			// branch: the idle branch above claims workers with no jobs.
			continue
		} else {
			// Slow node detection: if throughput is below median/SlowNodeThreshold
			if median > 0 && w.ewmaJobsPerSec < median/SlowNodeThreshold {
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

// ClusterMedian returns the current cluster median of EWMA throughput, computed
// from the same sample pool DetectSlowWorkers evaluates: offline and warmup
// workers are excluded, as are idle workers — no active jobs and at least
// MinZeroSamplesForIdle consecutive zero-throughput heartbeats, including
// workers that never reported a non-zero sample. Read-only; returns 0 when
// fewer than two eligible samples exist. Exposed so the workers API can report
// the raw median that E2E assertions compare worker EWMA against.
func (t *WorkerStateTable) ClusterMedian() float64 {
	t.mu.RLock()
	defer t.mu.RUnlock()

	_, throughputs := medianSample(t.eligibleSlowNodeWorkersLocked())
	if len(throughputs) < 2 {
		return 0
	}
	sort.Float64s(throughputs)
	return computeMedian(throughputs)
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
