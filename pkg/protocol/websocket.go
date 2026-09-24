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

// WebSocket frame sizing contract, shared by the server's WritePump and the
// CLI's WSClient so the two sides cannot drift apart.
//
// The server packs queued messages into one newline-delimited text frame and
// the CLI caps what it accepts with SetReadLimit. gorilla discards an
// over-limit frame WHOLE — every message in it — and the CLI can only report
// that as a reconnected sequence gap: for streaming output, silent data loss.
// So WSFrameByteBudget caps the server's packing, and WSClientReadLimit, well
// above it, is what the client accepts.
const (
	// WSStdoutChunkBytes is the raw size of one executor stdout read and
	// WSStdoutBatchChunks how many of them the worker's StdoutBatcher
	// concatenates into a single WSMsgStdout. Their product bounds the payload
	// of the largest message any producer emits, which is what the frame
	// budget and the client read limit have to accommodate.
	WSStdoutChunkBytes  = 32 * 1024
	WSStdoutBatchChunks = 10

	// WSMaxStdoutMessageBytes bounds the on-wire size of that largest message:
	// base64 inflates the batched payload by 4/3 (base64.StdEncoding.EncodedLen
	// is not a constant expression, so the formula is inlined), and the JSON
	// envelope adds its own fields.
	WSMaxStdoutMessageBytes = (WSStdoutChunkBytes*WSStdoutBatchChunks+2)/3*4 + 1024

	// WSFrameByteBudget caps the message bytes the server packs into one
	// frame. A single message is never split across frames — the client parses
	// one JSON message per line — so a message larger than the budget is still
	// written whole, as its own frame.
	WSFrameByteBudget = 1 << 20

	// WSClientReadLimit is what the CLI passes to SetReadLimit. It exceeds
	// WSFrameByteBudget by a wide margin so that the one frame allowed to
	// overshoot the budget (a single over-budget message) is still accepted.
	WSClientReadLimit = 4 << 20
)

type WSMessage struct {
	Type      WSMessageType `json:"type"`
	Timestamp time.Time     `json:"timestamp"`
	JobID     string        `json:"job_id,omitempty"`
	Payload   string        `json:"payload,omitempty"`
	Data      interface{}   `json:"data,omitempty"`
	// Seq is a per-job monotonically increasing sequence number stamped by
	// the server hub on every data-bearing message. Clients use it to detect
	// gaps caused by reconnects — a silent hole in stderr/stdout produces
	// corrupted output that must be reported, not swallowed.
	Seq int64 `json:"seq,omitempty"`
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

// WorkerHeartbeatPayload carries detailed worker metrics for heartbeat messages.
type WorkerHeartbeatPayload struct {
	WorkerID        string    `json:"worker_id"`
	Status          string    `json:"status"`
	GPUUtilPct      float64   `json:"gpu_util_percent"` // Aggregated across all GPUs (0-100*N); 0 is a valid reading
	GPUMemUsedMB    int       `json:"gpu_mem_used_mb"`
	GPUMetricsValid bool      `json:"gpu_metrics_valid"` // True when GPUUtilPct/GPUMemUsedMB carry a fresh sample (any GPU source)
	ActiveJobs      []string  `json:"active_jobs,omitempty"`
	JobsPerSec      float64   `json:"jobs_per_sec"` // Jobs completed per second since the last heartbeat
	QueueDepth      int       `json:"queue_depth"`
	CompletedJobs   int       `json:"completed_jobs"`
	Timestamp       time.Time `json:"timestamp"`
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
