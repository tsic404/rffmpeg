package workerhealth

import (
	"log"
	"sync"
	"time"

	"github.com/tsic404/rffmpeg/pkg/protocol"
	"github.com/tsic404/rffmpeg/pkg/server/db"
	"github.com/tsic404/rffmpeg/pkg/server/migration"
)

// Config holds the configuration for the worker health monitor
type Config struct {
	HeartbeatTimeout    time.Duration
	OfflineThreshold    time.Duration
	HealthCheckInterval time.Duration
	MaxRetryCount       int // Maximum retry count for job migration
}

// DefaultConfig returns the default configuration. HeartbeatTimeout must
// match ServerConfig.WorkerHeartbeatTimeout (pkg/config): two different
// defaults for the same knob caused drift between the monitor and the rest
// of the server (TSI-2365).
func DefaultConfig() Config {
	return Config{
		HeartbeatTimeout:    90 * time.Second,
		OfflineThreshold:    10 * time.Minute,
		HealthCheckInterval: 30 * time.Second,
		MaxRetryCount:       3,
	}
}

// Monitor periodically checks worker health and manages offline workers
type Monitor struct {
	db         *db.Database
	config     Config
	stop       chan struct{}
	done       chan struct{}
	mu         sync.Mutex
	started    bool      // Set by Start; guards the Stop-before-Start path
	stopOnce   sync.Once // Guarantees Stop is idempotent (TSI-2365)
	sched      SchedulerInterface
	stateTable *WorkerStateTable // Optional: enables slow node detection
}

// SchedulerInterface defines the interface for the scheduler
// This allows the monitor to trigger job rescheduling after migration
type SchedulerInterface interface {
	TriggerReschedule()
}

// New creates a new worker health monitor
func New(database *db.Database, config Config) *Monitor {
	return &Monitor{
		db:     database,
		config: config,
		stop:   make(chan struct{}),
		done:   make(chan struct{}),
	}
}

// SetScheduler sets the scheduler for triggering rescheduling
func (m *Monitor) SetScheduler(scheduler SchedulerInterface) {
	m.sched = scheduler
}

// SetStateTable sets the worker state table for slow node detection.
// When set, the monitor will periodically detect and mark slow workers.
func (m *Monitor) SetStateTable(st *WorkerStateTable) {
	m.stateTable = st
}

// Start begins the health monitoring loop
func (m *Monitor) Start() {
	m.mu.Lock()
	m.started = true
	m.mu.Unlock()
	go m.run()
}

// Stop stops the health monitor. Idempotent, and safe to call before Start:
// when the loop goroutine never launched, done is closed here so Stop neither
// double-closes nor blocks forever (TSI-2365).
func (m *Monitor) Stop() {
	m.stopOnce.Do(func() {
		m.mu.Lock()
		started := m.started
		if !started {
			close(m.done) // no loop will ever close it
		}
		m.mu.Unlock()

		close(m.stop)
		if started {
			<-m.done
		}
	})
}

// run is the main monitoring loop
func (m *Monitor) run() {
	defer close(m.done)

	ticker := time.NewTicker(m.config.HealthCheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-m.stop:
			log.Println("Worker health monitor stopped")
			return
		case <-ticker.C:
			m.checkWorkers()
		}
	}
}

// checkWorkers checks all workers and manages offline status
func (m *Monitor) checkWorkers() {
	// Mark workers as offline if heartbeat timeout exceeded and get their IDs
	offlineWorkerIDs, err := m.db.MarkOfflineWorkersWithMigration(m.config.HeartbeatTimeout)
	if err != nil {
		log.Printf("Failed to mark offline workers: %v", err)
		return
	}

	if len(offlineWorkerIDs) > 0 {
		log.Printf("Marked %d worker(s) as offline due to heartbeat timeout", len(offlineWorkerIDs))

		// Migrate jobs from each offline worker
		for _, workerID := range offlineWorkerIDs {
			m.migrateJobsFromWorker(workerID)
		}
	}

	// Remove workers that have been offline longer than threshold and evict
	// them from the in-memory state table: stale table entries previously
	// kept dead nodes in the slow-node median sample pool forever.
	removedIDs, err := m.db.RemoveOfflineWorkers(m.config.OfflineThreshold)
	if err != nil {
		log.Printf("Failed to remove offline workers: %v", err)
	} else if len(removedIDs) > 0 {
		if m.stateTable != nil {
			for _, workerID := range removedIDs {
				m.stateTable.RemoveWorker(workerID)
			}
		}
		log.Printf("Removed %d offline worker(s) exceeding offline threshold", len(removedIDs))
	}

	// Slow node detection (TSI-760)
	m.detectSlowWorkers()
}

// detectSlowWorkers runs slow node detection using the WorkerStateTable.
// Syncs eviction state to DB and records audit events for eviction/recovery.
func (m *Monitor) detectSlowWorkers() {
	if m.stateTable == nil {
		return
	}

	result := m.stateTable.DetectSlowWorkers()

	// Sync eviction state to DB for newly evicted workers
	for _, workerID := range result.NewlyEvicted {
		if err := m.db.MarkWorkerEvicted(workerID); err != nil {
			log.Printf("Failed to mark worker %s as evicted in DB: %v", workerID, err)
		}
		// Get worker state for throughput info
		state, ok := m.stateTable.Get(workerID)
		if !ok {
			continue
		}
		// Record audit event
		reason := "worker EWMA throughput below cluster median threshold"
		if _, err := m.db.CreateEvictionEvent(
			workerID,
			db.EvictionEventEvicted,
			state.EWMAThroughput,
			result.Median,
			reason,
		); err != nil {
			log.Printf("Failed to create eviction audit event for worker %s: %v", workerID, err)
		}
		log.Printf("Worker %s evicted: EWMA=%.2f, median=%.2f", workerID, state.EWMAThroughput, result.Median)
	}

	// Sync recovery to DB and record audit events
	for _, workerID := range result.Recovered {
		if err := m.db.ClearWorkerEviction(workerID); err != nil {
			log.Printf("Failed to clear eviction for worker %s in DB: %v", workerID, err)
		}
		state, ok := m.stateTable.Get(workerID)
		if !ok {
			continue
		}
		reason := "worker EWMA throughput recovered within cluster median threshold"
		if _, err := m.db.CreateEvictionEvent(
			workerID,
			db.EvictionEventRecovered,
			state.EWMAThroughput,
			result.Median,
			reason,
		); err != nil {
			log.Printf("Failed to create recovery audit event for worker %s: %v", workerID, err)
		}
		log.Printf("Worker %s recovered: EWMA=%.2f, median=%.2f", workerID, state.EWMAThroughput, result.Median)
	}

	if len(result.NewlyEvicted) > 0 || len(result.Recovered) > 0 {
		log.Printf("Slow node check: %d evicted, %d recovered, median=%.2f",
			len(result.NewlyEvicted), len(result.Recovered), result.Median)
		// Trigger scheduler to reschedule after eviction changes
		if m.sched != nil {
			m.sched.TriggerReschedule()
		}
	}
}

// migrateJobsFromWorker migrates in-progress jobs from a worker and records the migration event.
// Jobs that have exceeded MaxRetryCount (previous retries >= MaxRetryCount) are marked as failed
// instead of being migrated.
func (m *Monitor) migrateJobsFromWorker(workerID string) {
	// Get worker info before migration
	worker, err := m.db.GetWorker(workerID)
	if err != nil {
		log.Printf("Failed to get worker %s info for migration: %v", workerID, err)
		return
	}

	// Get jobs that will be migrated
	jobs, err := m.db.GetRunningJobsByWorker(workerID)
	if err != nil {
		log.Printf("Failed to get running jobs for worker %s: %v", workerID, err)
		return
	}

	if len(jobs) == 0 {
		log.Printf("Worker %s has no running jobs to migrate", workerID)
		return
	}

	workerName := ""
	if worker.Name != "" {
		workerName = worker.Name
	}

	// Phase 1: classify jobs (read-only) into migrated vs failed, tracking the
	// max retry count for the audit event. No job is made schedulable here.
	var migratedJobIDs []string
	var failedJobIDs []string
	retryCount := 0

	for _, job := range jobs {
		count, err := m.db.GetJobRetryCount(job.ID)
		if err != nil {
			log.Printf("Failed to get retry count for job %s: %v", job.ID, err)
			continue
		}

		if count > retryCount {
			retryCount = count
		}

		if count >= m.config.MaxRetryCount {
			failedJobIDs = append(failedJobIDs, job.ID)
		} else {
			migratedJobIDs = append(migratedJobIDs, job.ID)
		}
	}

	// Phase 2: record the migration event and the per-job redistribution
	// placeholders BEFORE any job becomes schedulable. ResetJobToPending (phase
	// 3) is what makes a job claimable, and a job claimed in the window between
	// reset and placeholder insert would never have its target written back —
	// a permanent loss of that hop's target (TSI-2929 review).
	if len(migratedJobIDs) > 0 {
		event, err := m.db.CreateMigrationEvent(
			workerID,
			workerName,
			string(migration.ReasonHeartbeatTimeout),
			retryCount,
			migratedJobIDs,
			len(migratedJobIDs),
		)
		if err != nil {
			log.Printf("Failed to create migration event for worker %s: %v", workerID, err)
		} else if err := m.db.CreateJobRedistributions(event.ID, migratedJobIDs); err != nil {
			// The migration event is authoritative; a redistribution placeholder
			// failure only degrades target observability.
			log.Printf("Failed to create job redistributions for worker %s: %v", workerID, err)
		}
	}

	// Phase 3: apply the decisions. ResetJobToPending is conditional on the job
	// still being active, so a job that raced to a terminal state is left alone.
	for _, jobID := range migratedJobIDs {
		if err := m.db.ResetJobToPending(jobID); err != nil {
			log.Printf("Failed to reset job %s to pending: %v", jobID, err)
		}
	}
	for _, jobID := range failedJobIDs {
		errMsg := "max retry count exceeded after repeated worker failures"
		if err := m.db.FailJob(jobID, errMsg, string(protocol.FailureWorkerCrash)); err != nil {
			log.Printf("Failed to mark job %s as failed: %v", jobID, err)
		}
	}

	if len(migratedJobIDs) > 0 {
		log.Printf("Migrated %d job(s) from offline worker %s (retry count: %d)",
			len(migratedJobIDs), workerID, retryCount)
	}

	if len(failedJobIDs) > 0 {
		log.Printf("Failed %d job(s) from offline worker %s due to exceeding max retry count (%d)",
			len(failedJobIDs), workerID, m.config.MaxRetryCount)
	}

	// Trigger scheduler to reschedule migrated jobs
	if len(migratedJobIDs) > 0 && m.sched != nil {
		m.sched.TriggerReschedule()
	}
}

// GetMigrationEvents retrieves migration events with pagination
func (m *Monitor) GetMigrationEvents(limit int, offset int) ([]migration.EventInfo, error) {
	events, err := m.db.GetMigrationEvents(limit, offset)
	if err != nil {
		return nil, err
	}

	result := make([]migration.EventInfo, len(events))
	for i, event := range events {
		result[i] = migration.FromDBEvent(event)
	}

	return result, nil
}

// GetMigrationEventsByWorker retrieves migration events for a specific worker
func (m *Monitor) GetMigrationEventsByWorker(workerID string, limit int) ([]migration.EventInfo, error) {
	events, err := m.db.GetMigrationEventsByWorker(workerID, limit)
	if err != nil {
		return nil, err
	}

	result := make([]migration.EventInfo, len(events))
	for i, event := range events {
		result[i] = migration.FromDBEvent(event)
	}

	return result, nil
}
