package protocol

import (
	"encoding/json"
	"time"
)

func MarshalJSON(v interface{}) ([]byte, error) {
	return json.Marshal(v)
}

func UnmarshalJSON(data []byte, v interface{}) error {
	return json.Unmarshal(data, v)
}

func ParseJobStatus(s string) (JobStatus, bool) {
	switch JobStatus(s) {
	case JobStatusPending, JobStatusQueued, JobStatusRunning,
		JobStatusCompleted, JobStatusFailed, JobStatusCancelled, JobStatusTimeout:
		return JobStatus(s), true
	default:
		return "", false
	}
}

func ParseWorkerStatus(s string) (WorkerStatus, bool) {
	switch WorkerStatus(s) {
	case WorkerStatusIdle, WorkerStatusBusy, WorkerStatusOffline:
		return WorkerStatus(s), true
	default:
		return "", false
	}
}

func IsTerminalStatus(status JobStatus) bool {
	return status == JobStatusCompleted || status == JobStatusFailed || status == JobStatusCancelled || status == JobStatusTimeout
}

func IsErrorStatus(status JobStatus) bool {
	return status == JobStatusFailed || status == JobStatusCancelled || status == JobStatusTimeout
}

func (j *JobInfo) Duration() time.Duration {
	if j.StartedAt == nil {
		return 0
	}
	end := time.Now()
	if j.FinishedAt != nil {
		end = *j.FinishedAt
	}
	return end.Sub(*j.StartedAt)
}

func (w *WorkerCapabilities) HasEncoder(encoder string) bool {
	for _, e := range w.Encoders {
		if e == encoder {
			return true
		}
	}
	return false
}

func (w *WorkerCapabilities) HasDecoder(decoder string) bool {
	for _, d := range w.Decoders {
		if d == decoder {
			return true
		}
	}
	return false
}
