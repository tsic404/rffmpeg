package scheduler

import (
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/tsix404/rffmpeg/pkg/protocol"
	"github.com/tsix404/rffmpeg/pkg/server/db"
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
}

// DefaultConfig returns the default scheduler configuration
func DefaultConfig() Config {
	return Config{
		JobTimeout:           30 * time.Minute,
		ScheduleInterval:     5 * time.Second,
		TimeoutCheckInterval: 30 * time.Second,
		MaxJobsPerWorker:     1, // One job at a time per worker by default
		NoWorkerJobTimeout:   2 * time.Minute,
	}
}

// Scheduler manages job scheduling and timeout detection
type Scheduler struct {
	db      *db.Database
	config  Config
	stop    chan struct{}
	done    chan struct{}
	mu      sync.Mutex
	resched chan struct{} // Channel to trigger immediate rescheduling

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
	go s.run()
}

// Stop stops the scheduler
func (s *Scheduler) Stop() {
	close(s.stop)
	<-s.done
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
		// Try to schedule this job
		scheduled := s.scheduleJob(job)
		if !scheduled {
			// No worker available for this job, leave it pending
			break
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

	// Fall back to regular scheduling if no capability-matched worker found
	if bestWorker == nil {
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

	// Assign job to the best worker
	err := s.db.AssignJobToWorker(job.ID, bestWorker.ID)
	if err != nil {
		log.Printf("Scheduler: Failed to assign job %s to worker %s: %v", job.ID, bestWorker.ID, err)
		return false
	}

	// Update job status to queued
	err = s.db.UpdateJobStatusWithFailure(job.ID, protocol.JobStatusQueued, nil, nil, nil, nil)
	if err != nil {
		log.Printf("Scheduler: Failed to update job %s status: %v", job.ID, err)
		return false
	}

	// Update worker status to busy if this was the first job
	if bestActiveCount == 0 {
		err = s.db.UpdateWorkerStatus(bestWorker.ID, protocol.WorkerStatusBusy)
		if err != nil {
			log.Printf("Scheduler: Failed to update worker %s status: %v", bestWorker.ID, err)
		}
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
		log.Printf("Scheduler: Job %s timed out (started at %v), rescheduling", job.ID, job.StartedAt.Time)

		// Reset job for rescheduling
		err := s.db.RescheduleJob(job.ID)
		if err != nil {
			log.Printf("Scheduler: Failed to reschedule job %s: %v", job.ID, err)
			continue
		}

		// Update worker's active job count and status
		if job.WorkerID.Valid {
			activeCount, err := s.db.GetWorkerActiveJobCount(job.WorkerID.String)
			if err == nil && activeCount == 0 {
				// No more active jobs, set worker to idle
				s.db.UpdateWorkerStatus(job.WorkerID.String, protocol.WorkerStatusIdle)
			}
		}

		log.Printf("Scheduler: Job %s rescheduled successfully", job.ID)
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
	failed, err := s.db.FailStarvedPendingJobs(cutoff, errMsg, string(protocol.FailureNoWorkerAvailable))
	if err != nil {
		log.Printf("Scheduler: Failed to fail starved pending jobs: %v", err)
		return
	}
	if failed > 0 {
		log.Printf("Scheduler: Failed %d starved job(s) after waiting %s with no schedulable worker",
			failed, s.config.NoWorkerJobTimeout)
	}
}
