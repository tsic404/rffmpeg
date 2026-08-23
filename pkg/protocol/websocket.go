package protocol

import "time"

type WSMessageType string

const (
	WSMsgStderr    WSMessageType = "stderr"
	WSMsgStdout    WSMessageType = "stdout"
	WSMsgStatus    WSMessageType = "status"
	WSMsgProgress  WSMessageType = "progress"
	WSMsgComplete  WSMessageType = "complete"
	WSMsgError     WSMessageType = "error"
	WSMsgHeartbeat WSMessageType = "heartbeat"
)

type WSMessage struct {
	Type      WSMessageType `json:"type"`
	Timestamp time.Time     `json:"timestamp"`
	JobID     string        `json:"job_id,omitempty"`
	Payload   string        `json:"payload,omitempty"`
	Data      interface{}   `json:"data,omitempty"`
}

type WSStderrPayload struct {
	Chunk string `json:"chunk"`
}

type WSStatusPayload struct {
	Status   JobStatus `json:"status"`
	ExitCode int       `json:"exit_code,omitempty"`
	Error    string    `json:"error,omitempty"`
}

type WSProgressPayload struct {
	Percent     float64 `json:"percent"`
	Current     int64   `json:"current,omitempty"`
	Total       int64   `json:"total,omitempty"`
	Speed       float64 `json:"speed,omitempty"`
	TimeUs      int64   `json:"time_us,omitempty"`
	DurationUs  int64   `json:"duration_us,omitempty"`
	EtaSeconds  int     `json:"eta_seconds,omitempty"`
	TimeElapsed string  `json:"time_elapsed,omitempty"`
}

// WorkerHeartbeatPayload carries detailed worker metrics for heartbeat messages (TSI-756).
type WorkerHeartbeatPayload struct {
	WorkerID      string    `json:"worker_id"`
	Status        string    `json:"status"`
	GPUUtilPct    float64   `json:"gpu_util_percent,omitempty"`
	GPUMemUsedMB  int       `json:"gpu_mem_used_mb,omitempty"`
	ActiveJobs    []string  `json:"active_jobs,omitempty"`
	ThroughputFPS float64   `json:"throughput_fps,omitempty"`
	QueueDepth    int       `json:"queue_depth,omitempty"`
	CompletedJobs int       `json:"completed_jobs,omitempty"`
	Timestamp     time.Time `json:"timestamp"`
}

func NewWSMessage(msgType WSMessageType, jobID string) WSMessage {
	return WSMessage{
		Type:      msgType,
		Timestamp: time.Now(),
		JobID:     jobID,
	}
}

func NewStderrMessage(jobID, chunk string) WSMessage {
	return WSMessage{
		Type:      WSMsgStderr,
		Timestamp: time.Now(),
		JobID:     jobID,
		Payload:   chunk,
	}
}

// NewStdoutMessage creates a stdout chunk message for streaming output data
func NewStdoutMessage(jobID, chunk string) WSMessage {
	return WSMessage{
		Type:      WSMsgStdout,
		Timestamp: time.Now(),
		JobID:     jobID,
		Payload:   chunk,
	}
}

func NewStatusMessage(jobID string, status JobStatus, exitCode int, err string) WSMessage {
	return WSMessage{
		Type:      WSMsgStatus,
		Timestamp: time.Now(),
		JobID:     jobID,
		Data: WSStatusPayload{
			Status:   status,
			ExitCode: exitCode,
			Error:    err,
		},
	}
}

func NewProgressMessage(jobID string, percent float64, timeUs, durationUs int64, speed float64, etaSeconds int) WSMessage {
	return WSMessage{
		Type:      WSMsgProgress,
		Timestamp: time.Now(),
		JobID:     jobID,
		Data: WSProgressPayload{
			Percent:    percent,
			TimeUs:     timeUs,
			DurationUs: durationUs,
			Speed:      speed,
			EtaSeconds: etaSeconds,
		},
	}
}

func NewCompleteMessage(jobID string, exitCode int) WSMessage {
	return WSMessage{
		Type:      WSMsgComplete,
		Timestamp: time.Now(),
		JobID:     jobID,
		Data: WSStatusPayload{
			Status:   JobStatusCompleted,
			ExitCode: exitCode,
		},
	}
}

func NewErrorMessage(jobID, errMsg string) WSMessage {
	return WSMessage{
		Type:      WSMsgError,
		Timestamp: time.Now(),
		JobID:     jobID,
		Payload:   errMsg,
	}
}

func NewHeartbeatMessage() WSMessage {
	return WSMessage{
		Type:      WSMsgHeartbeat,
		Timestamp: time.Now(),
	}
}
