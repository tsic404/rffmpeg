package workerhealth

import (
	"log"
	"sync"
	"time"

	"github.com/tsic404/rffmpeg/pkg/server/db"
	"github.com/tsic404/rffmpeg/pkg/server/migration"
	"github.com/tsic404/rffmpeg/pkg/server/panicguard"
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
// of the server.
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
	stopOnce   sync.Once // Guarantees Stop is idempotent
	sched      SchedulerInterface
	stateTable *WorkerStateTable // Optional: enables slow node detection

	// streamReleaser drops the WebSocket sequence bookkeeping of jobs this
	// monitor terminalizes out of band (migration retry budget exhausted).
	// Optional: nil skips the cleanup.
	streamReleaser JobStreamReleaser
}

// SchedulerInterface defines the interface for the scheduler
// This allows the monitor to trigger job rescheduling after migration
type SchedulerInterface interface {
	TriggerReschedule()
}

// JobStreamReleaser drops a terminal job's WebSocket sequence bookkeeping.
// The websocket.Hub satisfies it; an interface keeps the monitor decoupled
// from the hub package. Jobs the monitor fails on its own never emit the
// complete frame that normally releases the counter.
type JobStreamReleaser interface {
	ReleaseJobStream(jobID string)
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

// SetJobStreamReleaser wires the WebSocket stream cleanup used for jobs this
// monitor terminalizes without a broadcast (migration retry budget exhausted).
func (m *Monitor) SetJobStreamReleaser(r JobStreamReleaser) {
	m.streamReleaser = r
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
	go panicguard.Guard("worker health monitor loop", m.run)
}

// Stop stops the health monitor. Idempotent, and safe to call before Start:
// when the loop goroutine never launched, done is closed here so Stop neither
// double-closes nor blocks forever.
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
		// Migrate jobs from each offline worker. Log per-worker detection detail
		// (ID, name, heartbeat staleness) so the kill -> offline latency is
		// directly observable in server logs rather than only a count.
		for _, workerID := range offlineWorkerIDs {
			m.logOfflineDetection(workerID)
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

	// Mark state-table entries offline once their heartbeat goes stale, before
	// slow-node detection. RemoveOfflineWorkers above only deletes rows past
	// OfflineThreshold; within the heartbeat-timeout -> offline-threshold
	// window a dead node's stale EWMA must not pollute the median sample pool.
	if m.stateTable != nil {
		m.stateTable.ScanOffline()
	}

	// Slow node detection
	m.detectSlowWorkers()
}

// logOfflineDetection logs per-worker offline detection detail. The previous
// count-only message hid the per-worker heartbeat staleness that determines
// the kill -> offline latency (~86s observed with a 90s heartbeat timeout and
// 30s check interval, not a fixed 105s); surfacing ID, name, staleness and the
// last_heartbeat timestamp makes that window directly measurable in logs.
func (m *Monitor) logOfflineDetection(workerID string) {
	worker, err := m.db.GetWorker(workerID)
	if err != nil {
		log.Printf("Marked worker %q offline due to heartbeat timeout (failed to read worker info: %v)", workerID, err)
		return
	}
	stale := time.Since(worker.LastHeartbeat)
	log.Printf("Marked worker %q (%q) offline: heartbeat stale for %s (last_heartbeat=%s, timeout=%s)",
		workerID, worker.Name, stale.Round(time.Second), worker.LastHeartbeat.Format(time.RFC3339), m.config.HeartbeatTimeout)
}

// detectSlowWorkers runs slow node detection using the WorkerStateTable.
// Syncs eviction state to DB and records audit events for eviction/recovery.
//
// DetectSlowWorkers applies its decisions to the in-memory marks first, so the
// persistent write below is the authority: a mark whose write fails is reverted
// here and a failed clear keeps the memory mark, leaving memory and DB in
// agreement instead of letting the panel and the next judgement diverge.
func (m *Monitor) detectSlowWorkers() {
	if m.stateTable == nil {
		return
	}

	result := m.stateTable.DetectSlowWorkers()

	// Sync eviction state to DB for newly evicted workers
	for _, workerID := range result.NewlyEvicted {
		if err := m.db.MarkWorkerEvicted(workerID); err != nil {
			// The persistent mark was not written: drop the in-memory one so
			// it cannot outlive the DB's (absent) mark.
			m.stateTable.SetEviction(workerID, false)
			log.Printf("Failed to mark worker %s as evicted in DB: %v", workerID, err)
			continue
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
			state.EWMAJobsPerSec,
			result.Median,
			reason,
		); err != nil {
			log.Printf("Failed to create eviction audit event for worker %s: %v", workerID, err)
		}
		log.Printf("Worker %s evicted: EWMA=%.2f, median=%.2f", workerID, state.EWMAJobsPerSec, result.Median)
	}

	// Sync recovery to DB and record the audit event. Both clear paths pass
	// their own reason: this one is throughput recovery, a manual operator
	// clear records db.EvictionReasonManualClear (C-90 keeps them apart).
	for _, workerID := range result.Recovered {
		state, ok := m.stateTable.Get(workerID)
		if !ok {
			continue
		}
		if _, err := m.db.ClearWorkerEviction(
			workerID,
			state.EWMAJobsPerSec,
			result.Median,
			db.EvictionReasonThroughputRecovered,
		); err != nil {
			// The DB still carries the mark: put it back in memory so both
			// agree and the next judgement stays on the recovery branch.
			m.stateTable.SetEviction(workerID, true)
			log.Printf("Failed to clear eviction for worker %s in DB: %v", workerID, err)
			continue
		}
		log.Printf("Worker %s recovered: EWMA=%.2f, median=%.2f", workerID, state.EWMAJobsPerSec, result.Median)
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

// migrateJobsFromWorker migrates the in-progress jobs of a worker the health
// tick just flipped offline. The classification, audit trail (migration event
// + per-job redistribution placeholders) and requeues run in one database
// transaction (see Database.MigrateJobsFromWorker), so C-85 holds: no job is
// released into scheduling before its audit row, and an audit write failure
// rolls the requeues back instead of stranding jobs.
//
// A failed migration restores the worker's non-offline status so the next tick
// re-detects it and retries — leaving it offline would make it invisible to
// the offline scan forever and orphan its jobs.
func (m *Monitor) migrateJobsFromWorker(workerID string) {
	result, err := m.db.MigrateJobsFromWorker(workerID, string(migration.ReasonHeartbeatTimeout), m.config.MaxRetryCount)
	if err != nil {
		log.Printf("Failed to migrate jobs from offline worker %s: %v", workerID, err)
		if restoreErr := m.db.RestoreWorkerStatusAfterFailedMigration(workerID); restoreErr != nil {
			log.Printf("Failed to restore worker %s after failed migration: %v", workerID, restoreErr)
		}
		return
	}

	if len(result.Migrated) == 0 && len(result.Failed) == 0 {
		log.Printf("Worker %s: no jobs to migrate (none active, or the worker recovered)", workerID)
		return
	}

	if len(result.Migrated) > 0 {
		log.Printf("Migrated %d job(s) from offline worker %s", len(result.Migrated), workerID)
		// Trigger scheduler to reschedule migrated jobs
		if m.sched != nil {
			m.sched.TriggerReschedule()
		}
	}
	if len(result.Failed) > 0 {
		if m.streamReleaser != nil {
			for _, jobID := range result.Failed {
				m.streamReleaser.ReleaseJobStream(jobID)
			}
		}
		log.Printf("Failed %d job(s) from offline worker %s due to exceeding max retry count (%d)",
			len(result.Failed), workerID, m.config.MaxRetryCount)
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
