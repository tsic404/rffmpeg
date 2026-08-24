package migration

import (
	"time"
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
	ID           string    `json:"id"`
	Timestamp    time.Time `json:"timestamp"`
	WorkerID     string    `json:"worker_id"`
	WorkerName   string    `json:"worker_name,omitempty"`
	Reason       Reason    `json:"reason"`
	RetryCount   int       `json:"retry_count"`
	JobIDs       []string  `json:"job_ids"`
	JobsMigrated int       `json:"jobs_migrated"`
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
