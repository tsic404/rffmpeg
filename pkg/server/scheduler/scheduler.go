package scheduler

import (
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/tsix404/rffmpeg/pkg/protocol"
	"github.com/tsix404/rffmpeg/pkg/server/db"
	"github.com/tsix404/rffmpeg/pkg/server/migration"
	"github.com/tsix404/rffmpeg/pkg/server/ratelimit"
)

// Config holds the configuration for the scheduler
type Config struct {
	JobTimeout           time.Duration // Maximum time a job can run before being considered timed out
	ScheduleInterval     time.Duration // Interval for scheduling checks
	TimeoutCheckInterval time.Duration // Interval for timeout checks
	MaxJobsPerWorker     int           // Maximum jobs to assign to a worker at once
	// NoWorkerJobTimeout is how long a pending job may wait with no schedulable
	// worker (offline/evicted pool) before being failed as NO_WORKER_AVAILABLE.
	// 0 disables the check. Pending jobs queued behind busy workers are NOT
	// affected: schedulable workers exist, so the job keeps waiting (TSI-2204).
	NoWorkerJobTimeout time.Duration

	// MaxTimeoutRetries bounds how many times checkTimeouts may requeue the
	// same job before failing it as TIMEOUT (0 = fail on first timeout).
	MaxTimeoutRetries int
}

// DefaultConfig returns the default scheduler configuration
func DefaultConfig() Config {
	return Config{
		JobTimeout:           30 * time.Minute,
		ScheduleInterval:     5 * time.Second,
		TimeoutCheckInterval: 30 * time.Second,
		MaxJobsPerWorker:     1, // One job at a time per worker by default
		NoWorkerJobTimeout:   2 * time.Minute,

		// MaxTimeoutRetries bounds how many times checkTimeouts may requeue
		// the same job before failing it as TIMEOUT. Without a budget, a hung
		// ffmpeg with healthy heartbeats was rescheduled every JobTimeout
		// forever — each retry also double-executing against the stuck worker.
		MaxTimeoutRetries: 2,
	}
}

// JobBroadcaster pushes terminal-state WebSocket updates for jobs that reach
// a terminal status outside the HTTP handler path (starvation sweep). The
// websocket.Hub satisfies it; an interface keeps scheduler decoupled from
// the hub package (TSI-2365).
type JobBroadcaster interface {
	BroadcastStatus(jobID string, status protocol.JobStatus, exitCode int, err string) error
}

// SetRateLimiter wires the rate limiter so quota is released for jobs failed
// outside the HTTP handler path (TSI-2365).
func (s *Scheduler) SetRateLimiter(counter ratelimit.ClientJobCounter) {
	s.rateLimiter = counter
}

// SetJobNotifier wires the WS broadcaster for out-of-band job failures.
func (s *Scheduler) SetJobNotifier(n JobBroadcaster) {
	s.jobNotifier = n
}

// Scheduler manages job scheduling and timeout detection
type Scheduler struct {
	db       *db.Database
	config   Config
	stop     chan struct{}
	done     chan struct{}
	mu       sync.Mutex
	started  bool          // Set by Start; guards the Stop-before-Start path
	resched  chan struct{} // Channel to trigger immediate rescheduling
	stopOnce sync.Once     // Guarantees Stop is idempotent (TSI-2365)

	// rateLimiter releases the per-client quota of jobs failed outside the
	// HTTP handler path (e.g. starvation sweep). Optional: nil skips release
	// (TSI-2365).
	rateLimiter ratelimit.ClientJobCounter

	// jobNotifier broadcasts terminal-state WS messages for jobs failed
	// outside the HTTP handler path. Optional: nil skips broadcast.
	jobNotifier JobBroadcaster

	// noWorkerEnabled mirrors config.NoWorkerJobTimeout > 0; captured at
	// construction so the loop never sees a torn config read.
	noWorkerEnabled bool
}

// New creates a new scheduler
func New(database *db.Database, config Config) *Scheduler {
	return &Scheduler{
		db:              database,
		config:          config,
		stop:            make(chan struct{}),
		done:            make(chan struct{}),
		resched:         make(chan struct{}, 1), // Buffered to allow non-blocking trigger
		noWorkerEnabled: config.NoWorkerJobTimeout > 0,
	}
}

// Start begins the scheduler loop
func (s *Scheduler) Start() {
	s.mu.Lock()
	s.started = true
	s.mu.Unlock()
	go s.run()
}

// Stop stops the scheduler and waits for the loop to exit. Idempotent, and
// safe to call before Start: when the loop goroutine never launched, done is
// closed here so Stop neither double-closes nor blocks forever (TSI-2365).
func (s *Scheduler) Stop() {
	s.stopOnce.Do(func() {
		s.mu.Lock()
		started := s.started
		if !started {
			close(s.done) // no loop will ever close it
		}
		s.mu.Unlock()

		close(s.stop)
		if started {
			<-s.done
		}
	})
}

// run is the main scheduler loop
func (s *Scheduler) run() {
	defer close(s.done)

	scheduleTicker := time.NewTicker(s.config.ScheduleInterval)
	defer scheduleTicker.Stop()

	timeoutTicker := time.NewTicker(s.config.TimeoutCheckInterval)
	defer timeoutTicker.Stop()

	for {
		select {
		case <-s.stop:
			log.Println("Scheduler stopped")
			return
		case <-scheduleTicker.C:
			s.schedulePendingJobs()
		case <-s.resched:
			// Immediate rescheduling triggered (e.g., after worker offline migration)
			s.schedulePendingJobs()
		case <-timeoutTicker.C:
			s.checkTimeouts()
			s.checkNoWorkerStarvation()
		}
	}
}

// TriggerReschedule triggers an immediate scheduling pass.
// This is called by the worker health monitor after jobs have been migrated from an offline worker.
func (s *Scheduler) TriggerReschedule() {
	select {
	case s.resched <- struct{}{}:
		// Signal sent successfully
	default:
		// Channel already has a pending signal, no need to send another
	}
}

// schedulePendingJobs assigns pending jobs to idle workers using capability-aware scheduling
func (s *Scheduler) schedulePendingJobs() {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Get pending jobs
	pendingJobs, err := s.db.GetPendingJobs(100)
	if err != nil {
		log.Printf("Scheduler: Failed to get pending jobs: %v", err)
		return
	}

	if len(pendingJobs) == 0 {
		return
	}

	// Process each pending job
	for _, job := range pendingJobs {
		// Try to schedule this job; a job with no eligible worker (missing
		// encoder capability, no capacity) must not block schedulable jobs
		// behind it in the queue.
		scheduled := s.scheduleJob(job)
		if !scheduled {
			continue
		}
	}
}

// scheduleJob attempts to schedule a single job using capability-aware scheduling.
// Returns true if the job was successfully scheduled.
func (s *Scheduler) scheduleJob(job *db.Job) bool {
	// Extract the requested encoder from job args
	requestedEncoder := ExtractEncoderFromArgs(job.Args)

	var bestWorker *db.Worker
	var bestActiveCount int = -1
	var usedFallbackEncoder string // Tracks which fallback encoder was used, if any

	if requestedEncoder != "" {
		// Capability-aware scheduling: find workers with the requested encoder
		workers, err := s.db.GetIdleWorkersByEncoderWithJobCount(requestedEncoder)
		if err != nil {
			log.Printf("Scheduler: Failed to get workers by encoder %s: %v", requestedEncoder, err)
			// Fall back to regular scheduling
		} else if len(workers) > 0 {
			// Find the best worker (lowest active job count that can accept more)
			for _, w := range workers {
				maxJobs := w.Worker.MaxConcurrent
				if maxJobs <= 0 {
					maxJobs = s.config.MaxJobsPerWorker
				}

				if w.ActiveJobs < maxJobs {
					if bestActiveCount == -1 || w.ActiveJobs < bestActiveCount {
						bestWorker = w.Worker
						bestActiveCount = w.ActiveJobs
					}
				}
			}

			if bestWorker != nil {
				log.Printf("Scheduler: Job %s requests encoder %s, found worker %s with capability",
					job.ID, requestedEncoder, bestWorker.ID)
			} else {
				log.Printf("Scheduler: Job %s requests encoder %s, but no workers have capacity",
					job.ID, requestedEncoder)
			}
		} else {
			log.Printf("Scheduler: Job %s requests encoder %s, but no idle workers have this capability",
				job.ID, requestedEncoder)

			// TSI-1500: Try encoder fallback within the same codec family
			bestWorker, usedFallbackEncoder = s.tryEncoderFallback(job.ID, requestedEncoder)
			if bestWorker != nil {
				log.Printf("Scheduler: Job %s falling back from %s to encoder %s on worker %s",
					job.ID, requestedEncoder, usedFallbackEncoder, bestWorker.ID)
			}
		}
	}

	// Fall back to regular scheduling only when the job did NOT request a
	// specific encoder. A job explicitly requesting an encoder no worker has
	// must stay pending: assigning it to an incapable worker guarantees
	// failure, and the old behavior also made such jobs schedule-eligible,
	// masking capability gaps.
	if bestWorker == nil && requestedEncoder == "" {
		workers, err := s.db.GetIdleWorkersWithJobCount()
		if err != nil {
			log.Printf("Scheduler: Failed to get idle workers: %v", err)
			return false
		}

		for _, w := range workers {
			maxJobs := w.Worker.MaxConcurrent
			if maxJobs <= 0 {
				maxJobs = s.config.MaxJobsPerWorker
			}

			if w.ActiveJobs < maxJobs {
				if bestActiveCount == -1 || w.ActiveJobs < bestActiveCount {
					bestWorker = w.Worker
					bestActiveCount = w.ActiveJobs
				}
			}
		}
	}

	if bestWorker == nil {
		return false
	}
	// Assign the job atomically: one statement sets worker_id and flips the
	// status to queued, eliminating the two-step pending+worker_id orphan
	// window (a failure in between left the job invisible to both the
	// scheduler scan and the starvation fallback).
	err := s.db.AssignJobToWorker(job.ID, bestWorker.ID)
	if err != nil {
		if errors.Is(err, protocol.ErrJobNotOwned) {
			// Another path (pull) assigned it first; not an error.
			return true
		}
		log.Printf("Scheduler: Failed to assign job %s to worker %s: %v", job.ID, bestWorker.ID, err)
		return false
	}

	if requestedEncoder != "" {
		if usedFallbackEncoder != "" {
			log.Printf("Scheduler: Assigned job %s (fallback: %s -> %s) to worker %s", job.ID, requestedEncoder, usedFallbackEncoder, bestWorker.ID)
		} else {
			log.Printf("Scheduler: Assigned job %s (encoder: %s) to worker %s", job.ID, requestedEncoder, bestWorker.ID)
		}
	} else {
		log.Printf("Scheduler: Assigned job %s to worker %s", job.ID, bestWorker.ID)
	}

	return true
}

// tryEncoderFallback attempts to find a worker with a compatible encoder in the same codec family.
// Returns the worker and the fallback encoder name if found, or nil and empty string if not.
func (s *Scheduler) tryEncoderFallback(jobID, requestedEncoder string) (*db.Worker, string) {
	// Get the codec family for the requested encoder
	codecFamily := GetCodecFamily(requestedEncoder)
	if codecFamily == "" {
		log.Printf("Scheduler: Job %s encoder %s has unknown codec family, cannot fallback", jobID, requestedEncoder)
		return nil, ""
	}

	// Get all compatible encoders in priority order
	compatibleEncoders := GetCompatibleEncoders(requestedEncoder)
	if len(compatibleEncoders) == 0 {
		return nil, ""
	}

	log.Printf("Scheduler: Job %s trying fallback encoders for codec family %s: %v", jobID, codecFamily, compatibleEncoders)

	// Try each compatible encoder in priority order
	for _, fallbackEncoder := range compatibleEncoders {
		// Skip the originally requested encoder (we already checked it)
		if fallbackEncoder == requestedEncoder {
			continue
		}

		// Check if any idle worker has this encoder
		workers, err := s.db.GetIdleWorkersByEncoderWithJobCount(fallbackEncoder)
		if err != nil {
			log.Printf("Scheduler: Failed to get workers for fallback encoder %s: %v", fallbackEncoder, err)
			continue
		}

		if len(workers) == 0 {
			continue
		}

		// Find the best worker (lowest active job count)
		var bestWorker *db.Worker
		bestActiveCount := -1

		for _, w := range workers {
			maxJobs := w.Worker.MaxConcurrent
			if maxJobs <= 0 {
				maxJobs = s.config.MaxJobsPerWorker
			}

			if w.ActiveJobs < maxJobs {
				if bestActiveCount == -1 || w.ActiveJobs < bestActiveCount {
					bestWorker = w.Worker
					bestActiveCount = w.ActiveJobs
				}
			}
		}

		if bestWorker != nil {
			return bestWorker, fallbackEncoder
		}
	}

	log.Printf("Scheduler: Job %s no compatible encoder found for codec family %s", jobID, codecFamily)
	return nil, ""
}

// checkTimeouts detects and reschedules timed out jobs
func (s *Scheduler) checkTimeouts() {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Get running jobs that have exceeded timeout
	timedOutJobs, err := s.db.GetTimedOutJobs(s.config.JobTimeout)
	if err != nil {
		log.Printf("Scheduler: Failed to get timed out jobs: %v", err)
		return
	}

	for _, job := range timedOutJobs {
		log.Printf("Scheduler: Job %s timed out (started at %v)", job.ID, job.StartedAt.Time)

		// Retry budget: once the job has already been requeued MaxTimeoutRetries
		// times, stop rescheduling and fail it as TIMEOUT. Unbounded requeue
		retries, err := s.db.GetJobTimeoutRetryCount(job.ID)
		if err != nil {
			log.Printf("Scheduler: Failed to get retry count for job %s: %v", job.ID, err)
			continue
		}
		if retries >= s.config.MaxTimeoutRetries {
			errMsg := fmt.Sprintf("job exceeded maximum timeout retries (%d)", s.config.MaxTimeoutRetries)
			if err := s.db.FailJob(job.ID, errMsg, string(protocol.FailureTimeout)); err != nil {
				log.Printf("Scheduler: Failed to fail timed-out job %s: %v", job.ID, err)
				continue
			}
			log.Printf("Scheduler: Job %s failed after %d timeout retries", job.ID, retries)
		} else {
			// Reset job for rescheduling and record the requeue so the retry
			// budget is observable on the next pass (GetJobRetryCount reads
			// migration events).
			if err := s.db.RescheduleJob(job.ID); err != nil {
				log.Printf("Scheduler: Failed to reschedule job %s: %v", job.ID, err)
				continue
			}
			if _, err := s.db.CreateMigrationEvent(job.WorkerID.String, "",
				string(migration.ReasonJobTimeout), retries, []string{job.ID}, 1); err != nil {
				log.Printf("Scheduler: Failed to record timeout retry for job %s: %v", job.ID, err)
			}
			log.Printf("Scheduler: Job %s rescheduled (timeout retry %d/%d)", job.ID, retries+1, s.config.MaxTimeoutRetries)
		}
	}
}

// checkNoWorkerStarvation fails pending jobs that have been waiting longer
// than NoWorkerJobTimeout while the cluster has NO schedulable worker (all
// offline or evicted). Without this, a job submitted just before its only
// worker died would stay pending forever and CLI clients would hang (TSI-2334).
//
// Jobs queued behind busy workers are deliberately untouched: schedulable
// workers exist, so the job is expected to start once one frees up.
func (s *Scheduler) checkNoWorkerStarvation() {
	if !s.noWorkerEnabled {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	pendingJobs, err := s.db.GetPendingJobs(1)
	// GetPendingJobs(1) is a sentinel probe, not a work batch: we only need
	// to know whether ANY pending job exists. FailStarvedPendingJobs below
	// fails ALL qualifying starved jobs in one statement, so fetching more
	// rows here would be wasted work.
	if err != nil {
		log.Printf("Scheduler: Failed to get pending jobs: %v", err)
		return
	}
	if len(pendingJobs) == 0 {
		return
	}

	schedulable, err := s.db.GetSchedulableWorkers()
	if err != nil {
		log.Printf("Scheduler: Failed to get schedulable workers: %v", err)
		return
	}
	if len(schedulable) > 0 {
		// At least one worker can still take jobs; keep waiting (TSI-2204).
		return
	}

	cutoff := time.Now().Add(-s.config.NoWorkerJobTimeout)
	errMsg := fmt.Sprintf(
		"no worker available for over %s (all workers offline or evicted); submit again once a worker is online",
		s.config.NoWorkerJobTimeout)

	// The bulk UPDATE bypasses the HTTP handler that normally releases the
	// per-client quota and broadcasts the terminal status. Do both here, or
	// clients get stuck behind a permanently inflated counter and CLI
	// listeners never learn their job died (TSI-2365).
	starvedIDs, err := s.db.GetStarvedPendingJobIDs(cutoff)
	if err != nil {
		log.Printf("Scheduler: Failed to list starved pending jobs: %v", err)
		return
	}

	failed, err := s.db.FailStarvedPendingJobs(cutoff, errMsg, string(protocol.FailureNoWorkerAvailable))
	if err != nil {
		log.Printf("Scheduler: Failed to fail starved pending jobs: %v", err)
		return
	}
	if failed > 0 {
		log.Printf("Scheduler: Failed %d starved job(s) after waiting %s with no schedulable worker",
			failed, s.config.NoWorkerJobTimeout)

		for _, jobID := range starvedIDs {
			if s.rateLimiter != nil {
				s.rateLimiter.DecrementByJob(jobID)
			}
			if s.jobNotifier != nil {
				if err := s.jobNotifier.BroadcastStatus(jobID, protocol.JobStatusFailed, -1, errMsg); err != nil {
					log.Printf("Scheduler: Failed to broadcast starvation failure for job %s: %v", jobID, err)
				}
			}
		}
	}
}
