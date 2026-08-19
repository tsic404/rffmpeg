package protocol

const (
	APIVersion = "v1"
	APIPrefix  = "/api/" + APIVersion
)

type JobStatus string

const (
	JobStatusPending   JobStatus = "pending"
	JobStatusQueued    JobStatus = "queued"
	JobStatusRunning   JobStatus = "running"
	JobStatusCompleted JobStatus = "completed"
	JobStatusFailed    JobStatus = "failed"
	JobStatusCancelled JobStatus = "cancelled"
	JobStatusTimeout   JobStatus = "timeout"
)

type WorkerStatus string

const (
	WorkerStatusIdle    WorkerStatus = "idle"
	WorkerStatusBusy    WorkerStatus = "busy"
	WorkerStatusOffline WorkerStatus = "offline"
)

type ErrorCode string

const (
	ErrCodeInvalidRequest    ErrorCode = "invalid_request"
	ErrCodeUnauthorized      ErrorCode = "unauthorized"
	ErrCodeNotFound          ErrorCode = "not_found"
	ErrCodeConflict          ErrorCode = "conflict"
	ErrCodeInternalError     ErrorCode = "internal_error"
	ErrCodeWorkerUnavailable ErrorCode = "worker_unavailable"
	ErrCodeJobFailed         ErrorCode = "job_failed"
	ErrCodeUploadFailed      ErrorCode = "upload_failed"
	ErrCodeDownloadFailed    ErrorCode = "download_failed"
	ErrCodeRateLimitExceeded ErrorCode = "rate_limit_exceeded"
)
