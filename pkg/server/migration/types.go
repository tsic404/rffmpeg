package migration

import (
	"encoding/json"
	"time"

	"github.com/tsic404/rffmpeg/pkg/server/db"
)

// Reason represents why a job migration occurred.
type Reason string

const (
	// ReasonHeartbeatTimeout indicates the worker was marked offline due to heartbeat timeout.
	ReasonHeartbeatTimeout Reason = "heartbeat_timeout"
	// ReasonWorkerOffline indicates the worker was explicitly marked offline.
	ReasonWorkerOffline Reason = "worker_offline"
	// ReasonServerRestart indicates migration due to server restart recovery.
	ReasonServerRestart Reason = "server_restart"
	// ReasonJobTimeout indicates the job was requeued after exceeding its
	// execution timeout (scheduler retry budget accounting).
	ReasonJobTimeout Reason = "job_timeout"
)

// Event represents a job migration event record.
type Event struct {
	// ID is the unique identifier for this migration event.
	ID string `json:"id"`
	// Timestamp is when the migration occurred.
	Timestamp time.Time `json:"timestamp"`
	// WorkerID is the ID of the worker that went offline.
	WorkerID string `json:"worker_id"`
	// WorkerName is the name of the worker that went offline.
	WorkerName string `json:"worker_name,omitempty"`
	// Reason is why the migration occurred.
	Reason Reason `json:"reason"`
	// RetryCount is the number of times these jobs have been retried.
	RetryCount int `json:"retry_count"`
	// JobIDs is the list of job IDs that were migrated.
	JobIDs []string `json:"job_ids"`
	// JobsMigrated is the count of jobs that were successfully migrated.
	JobsMigrated int `json:"jobs_migrated"`
	// CreatedAt is when this record was created.
	CreatedAt time.Time `json:"created_at"`
}

// EventInfo represents the public-facing migration event information.
type EventInfo struct {
	ID           string                    `json:"id"`
	Timestamp    time.Time                 `json:"timestamp"`
	WorkerID     string                    `json:"worker_id"`
	WorkerName   string                    `json:"worker_name,omitempty"`
	Reason       Reason                    `json:"reason"`
	RetryCount   int                       `json:"retry_count"`
	JobIDs       []string                  `json:"job_ids"`
	JobsMigrated int                       `json:"jobs_migrated"`
	Targets      []JobRedistributionTarget `json:"targets,omitempty"`
}

// FromDBEvent converts a db.MigrationEvent into the public EventInfo.
// The job_ids column stores a JSON array string; an unmarshal failure
// degrades to an empty list rather than failing the whole read.
func FromDBEvent(event *db.MigrationEvent) EventInfo {
	var jobIDs []string
	if err := json.Unmarshal([]byte(event.JobIDs), &jobIDs); err != nil {
		jobIDs = []string{}
	}

	workerName := ""
	if event.WorkerName.Valid {
		workerName = event.WorkerName.String
	}

	return EventInfo{
		ID:           event.ID,
		Timestamp:    event.Timestamp,
		WorkerID:     event.WorkerID,
		WorkerName:   workerName,
		Reason:       Reason(event.Reason),
		RetryCount:   event.RetryCount,
		JobIDs:       jobIDs,
		JobsMigrated: event.JobsMigrated,
	}
}

// JobRedistributionTarget is the per-job target side of a migration: which
// worker a single migrated job was reassigned to. A migration event's jobs can
// fan out to different workers, so the target is a per-job fact (TSI-2929).
type JobRedistributionTarget struct {
	JobID            string `json:"job_id"`
	TargetWorkerID   string `json:"target_worker_id,omitempty"`
	TargetWorkerName string `json:"target_worker_name,omitempty"`
}

// RedistributionsByEvent groups per-job redistribution records by their
// migration event ID, converting DB rows into the public target shape. Events
// with no redistribution record are absent from the map — callers treat that
// as "no reassignment recorded yet". A record with an unresolved target
// (target_worker_id NULL) carries an empty TargetWorkerID/Name.
func RedistributionsByEvent(rs []db.JobRedistribution) map[string][]JobRedistributionTarget {
	grouped := make(map[string][]JobRedistributionTarget)
	for _, r := range rs {
		t := JobRedistributionTarget{JobID: r.JobID}
		if r.TargetWorkerID.Valid {
			t.TargetWorkerID = r.TargetWorkerID.String
		}
		if r.TargetWorkerName.Valid {
			t.TargetWorkerName = r.TargetWorkerName.String
		}
		grouped[r.MigrationEventID] = append(grouped[r.MigrationEventID], t)
	}
	return grouped
}

// Config holds configuration for the migration manager.
type Config struct {
	// MaxRetryCount is the maximum number of times a job can be migrated before being marked as failed.
	MaxRetryCount int
}

// DefaultConfig returns the default migration configuration.
func DefaultConfig() Config {
	return Config{
		MaxRetryCount: 3,
	}
}
