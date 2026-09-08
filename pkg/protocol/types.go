package protocol

import (
	"encoding/base64"
	"time"

	"github.com/tsic404/rffmpeg/pkg/worker/gpu"
)

type UploadRequest struct {
	Filename string `json:"filename"`
	Size     int64  `json:"size"`
	Checksum string `json:"checksum,omitempty"`
}

type UploadResponse struct {
	FileID  string `json:"file_id"`
	Message string `json:"message,omitempty"`
}

type JobSubmitRequest struct {
	InputFiles      []string   `json:"input_files"`
	DirectPath      []string   `json:"direct_path,omitempty"` // Absolute paths when shared FS is enabled
	Args            []string   `json:"args"`
	OutputFilename  string     `json:"output_filename,omitempty"`
	StreamingOutput bool       `json:"streaming_output,omitempty"` // Output to stdout via WebSocket
	Priority        int        `json:"priority,omitempty"`
	Timeout         *time.Time `json:"timeout,omitempty"`
	AutoHW          bool       `json:"auto_hw,omitempty"` // Enable automatic hardware encoder upgrade
}

type JobSubmitResponse struct {
	JobID   string `json:"job_id"`
	Message string `json:"message,omitempty"`
}

type JobInfo struct {
	ID              string     `json:"id"`
	Status          JobStatus  `json:"status"`
	InputFiles      []string   `json:"input_files"`
	Args            []string   `json:"args"`
	OutputFilename  string     `json:"output_filename,omitempty"`
	StreamingOutput bool       `json:"streaming_output,omitempty"` // Output to stdout via WebSocket
	OutputFiles     []string   `json:"output_files,omitempty"`
	WorkerID        string     `json:"worker_id,omitempty"`
	ExitCode        int        `json:"exit_code,omitempty"`
	Error           string     `json:"error,omitempty"`
	AutoHW          bool       `json:"auto_hw,omitempty"` // Enable automatic hardware encoder upgrade
	Cached          bool       `json:"cached,omitempty"`  // Whether the result was served from the worker cache
	FailureType     string     `json:"failure_type,omitempty"`
	FailureDetails  string     `json:"failure_details,omitempty"`
	Timeout         *time.Time `json:"timeout,omitempty"` // Per-job timeout (nil = use default)
	// NoWorkerDeadline is the server-computed wall-clock time at which a
	// still-pending job will be failed as NO_WORKER_AVAILABLE by the
	// starvation sweep (created_at + no_worker_job_timeout +
	// timeout_check_interval). Nil for non-pending jobs, when the sweep is
	// disabled, or when at least one live schedulable worker exists (a busy
	// worker keeps the job waiting — the sweep's live-worker guard
	// short-circuits). The CLI uses it to size its own wait window so a
	// client-side timeout can never fire before the server verdict is
	// observable.
	NoWorkerDeadline *time.Time `json:"no_worker_deadline,omitempty"`
	DirectPaths      []string   `json:"direct_paths,omitempty"` // Direct output paths for pass-through mode (TSI-807)
	CreatedAt        time.Time  `json:"created_at"`
	UpdatedAt        time.Time  `json:"updated_at"`
	StartedAt        *time.Time `json:"started_at,omitempty"`
	FinishedAt       *time.Time `json:"finished_at,omitempty"`
	ProgressPercent  float64    `json:"progress_percent,omitempty"` // Current progress 0-100
	EtaSeconds       int        `json:"eta_seconds,omitempty"`      // Estimated time remaining in seconds
}

type JobStatusResponse struct {
	Job JobInfo `json:"job"`
}

// JobListResponse is the response for listing jobs (GET /api/v1/jobs).
type JobListResponse struct {
	Jobs []JobInfo `json:"jobs"`
}

type JobCancelResponse struct {
	Message string `json:"message"`
}

// EncoderInfo represents detailed encoder information
type EncoderInfo struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Type        string `json:"type"`               // "video", "audio", "subtitle"
	IsHW        bool   `json:"is_hw"`              // True if hardware-accelerated
	Priority    int    `json:"priority,omitempty"` // User-defined priority (higher = preferred)
}

// DecoderInfo represents detailed decoder information
type DecoderInfo struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Type        string `json:"type"`  // "video", "audio", "subtitle"
	IsHW        bool   `json:"is_hw"` // True if hardware-accelerated
}

// GPUDeviceInfo represents a GPU device for capability reporting
type GPUDeviceInfo struct {
	Type          string `json:"type"`           // "nvenc", "qsv", "vaapi", "amf", "videotoolbox", "d3d11"
	Path          string `json:"path,omitempty"` // Device path (e.g., /dev/dri/renderD128)
	Name          string `json:"name,omitempty"` // Device name/model
	DriverVersion string `json:"driver_version,omitempty"`
	Vendor        string `json:"vendor,omitempty"`
	Accessible    bool   `json:"accessible"`
	QSVHealthy    bool   `json:"qsv_healthy,omitempty"` // Whether QSV MFX runtime is functional (Intel only)
	QSVError      string `json:"qsv_error,omitempty"`   // Error message if QSV health check failed
}

// WorkerCapabilities holds the complete capabilities of a worker
type WorkerCapabilities struct {
	// Canonical capability fields. encoders is the normative flat list:
	// the only encoder field that participates in scheduling, registration
	// validation, and worker responses (video_encoders below is request-side
	// aggregation metadata only).
	GPUModel      string   `json:"gpu_model,omitempty"`
	Encoders      []string `json:"encoders"`
	Decoders      []string `json:"decoders,omitempty"`
	FFmpegVersion string   `json:"ffmpeg_version"`
	MaxConcurrent int      `json:"max_concurrent,omitempty"`

	// Optional request-side rich metadata. video_encoders/video_decoders are
	// sent by workers that probe ffmpeg and are consumed only by the
	// GET /api/v1/encoders and /api/v1/decoders aggregation endpoints. They
	// are NOT returned in any worker response. When a client registers only
	// the canonical flat lists, the server derives these from them.
	VideoEncoders    []EncoderInfo   `json:"video_encoders,omitempty"`    // Video encoders with HW markers
	VideoDecoders    []DecoderInfo   `json:"video_decoders,omitempty"`    // Video decoders with HW markers
	HWEncoders       []string        `json:"hw_encoders,omitempty"`       // List of HW-accelerated encoder names
	HWDecoders       []string        `json:"hw_decoders,omitempty"`       // List of HW-accelerated decoder names
	GPUDevices       []GPUDeviceInfo `json:"gpu_devices,omitempty"`       // Detected GPU devices
	EncoderPriority  []string        `json:"encoder_priority,omitempty"`  // User-defined encoder priority order
	EncoderBlacklist []string        `json:"encoder_blacklist,omitempty"` // Encoders to never use

	// P0/P1 raw text outputs for info flags
	Hwaccels string `json:"hwaccels,omitempty"` // Raw text from ffmpeg -hwaccels
	Codecs   string `json:"codecs,omitempty"`   // Raw text from ffmpeg -codecs
	Filters  string `json:"filters,omitempty"`  // Raw text from ffmpeg -filters
	PixFmts  string `json:"pix_fmts,omitempty"` // Raw text from ffmpeg -pix_fmts
	Formats  string `json:"formats,omitempty"`  // Raw text from ffmpeg -formats
}

// NewWorkerCapabilities creates a WorkerCapabilities from probe results and GPU detection
func NewWorkerCapabilities(
	ffmpegVersion string,
	videoEncoders []EncoderInfo,
	videoDecoders []DecoderInfo,
	gpuDevices []gpu.Device,
	maxConcurrent int,
	encoderPriority []string,
	encoderBlacklist []string,
) WorkerCapabilities {
	caps := WorkerCapabilities{
		FFmpegVersion:    ffmpegVersion,
		VideoEncoders:    videoEncoders,
		VideoDecoders:    videoDecoders,
		MaxConcurrent:    maxConcurrent,
		EncoderPriority:  encoderPriority,
		EncoderBlacklist: encoderBlacklist,
	}

	// Build legacy encoder list (names only)
	for _, enc := range videoEncoders {
		caps.Encoders = append(caps.Encoders, enc.Name)
		if enc.IsHW {
			caps.HWEncoders = append(caps.HWEncoders, enc.Name)
		}
	}

	// Build legacy decoder list (names only)
	for _, dec := range videoDecoders {
		caps.Decoders = append(caps.Decoders, dec.Name)
		if dec.IsHW {
			caps.HWDecoders = append(caps.HWDecoders, dec.Name)
		}
	}

	// Convert GPU devices
	for _, dev := range gpuDevices {
		caps.GPUDevices = append(caps.GPUDevices, GPUDeviceInfo{
			Type:          string(dev.Type),
			Path:          dev.Path,
			Name:          dev.Name,
			DriverVersion: dev.DriverVersion,
			Vendor:        dev.Vendor,
			Accessible:    dev.Accessible,
			QSVHealthy:    dev.QSVHealthy,
			QSVError:      dev.QSVError,
		})
		// Set legacy GPUModel to first GPU name
		if caps.GPUModel == "" && dev.Name != "" {
			caps.GPUModel = dev.Name
		}
	}

	return caps
}

type WorkerRegisterRequest struct {
	WorkerID     string             `json:"worker_id,omitempty"`
	Name         string             `json:"name,omitempty"`
	Capabilities WorkerCapabilities `json:"capabilities"`
}

type WorkerRegisterResponse struct {
	WorkerID string `json:"worker_id"`
	Message  string `json:"message,omitempty"`
}

type WorkerHeartbeatRequest struct {
	WorkerID        string       `json:"worker_id"`
	Status          WorkerStatus `json:"status"`
	ActiveJobs      []string     `json:"active_jobs,omitempty"`
	ThroughputFPS   float64      `json:"throughput_fps"` // Jobs completed per second since the last heartbeat (despite the legacy name)
	CompletedJobs   int          `json:"completed_jobs"`
	GPUUtilPct      float64      `json:"gpu_util_percent"` // Aggregated across all GPUs (0-100*N on multi-GPU hosts); 0 is a valid reading
	GPUMemUsedMB    int          `json:"gpu_mem_used_mb"`
	GPUMetricsValid bool         `json:"gpu_metrics_valid"` // True when GPUUtilPct/GPUMemUsedMB carry a fresh sample (nvidia-smi, intel_gpu_top, or amdgpu sysfs)
}
type WorkerHeartbeatResponse struct {
	Message       string   `json:"message"`
	CancelledJobs []string `json:"cancelled_jobs,omitempty"`
}

type WorkerJobPullResponse struct {
	Jobs []JobInfo `json:"jobs"`
}

// StdoutChunkBase64 decodes a base64-encoded stdout chunk (see EncodeStdoutChunk).
func StdoutChunkBase64(encoded string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(encoded)
}

// EncodeStdoutChunk encodes raw stdout bytes as standard base64.
//
// Stdout data is arbitrary binary and must not travel in a JSON string field:
// encoding/json replaces invalid UTF-8 bytes with U+FFFD on both encode and
// decode, silently corrupting the stream. Base64 is ASCII-safe, so it survives
// every JSON boundary between worker and CLI byte-for-byte.
func EncodeStdoutChunk(data []byte) string {
	return base64.StdEncoding.EncodeToString(data)
}

type JobUpdateRequest struct {
	Status         JobStatus `json:"status,omitempty"`
	ExitCode       int       `json:"exit_code,omitempty"`
	Error          string    `json:"error,omitempty"`
	StderrChunk    string    `json:"stderr_chunk,omitempty"`
	StdoutChunk    string    `json:"stdout_chunk,omitempty"` // base64-encoded raw stdout bytes (binary-safe)
	Progress       float64   `json:"progress,omitempty"`
	EtaSeconds     int       `json:"eta_seconds,omitempty"`     // Estimated time remaining in seconds
	TimeUs         int64     `json:"time_us,omitempty"`         // Current decoded time in microseconds
	DurationUs     int64     `json:"duration_us,omitempty"`     // Total duration in microseconds
	Speed          float64   `json:"speed,omitempty"`           // Current encoding speed multiplier
	Cached         bool      `json:"cached,omitempty"`          // Whether the result was served from cache
	FailureType    string    `json:"failure_type,omitempty"`    // Machine-readable failure category
	FailureDetails string    `json:"failure_details,omitempty"` // Human-readable failure details
	WorkerID       string    `json:"worker_id,omitempty"`       // Reporting worker; terminal updates are rejected unless this still owns the job
}

type JobUpdateResponse struct {
	Message string `json:"message"`
}

type JobOutputUploadRequest struct {
	Filename string `json:"filename"`
	Size     int64  `json:"size"`
	Checksum string `json:"checksum,omitempty"`
}

type JobOutputUploadResponse struct {
	Message string `json:"message"`
}

type HealthResponse struct {
	Status      string    `json:"status"`
	Timestamp   time.Time `json:"timestamp"`
	Version     string    `json:"version,omitempty"`
	AuthEnabled bool      `json:"auth_enabled"`
}

type ErrorResponse struct {
	Code    ErrorCode `json:"code"`
	Message string    `json:"message"`
	Detail  string    `json:"detail,omitempty"`
}

// RateLimitResponse is the response body for rate-limited requests (HTTP 429).
type RateLimitResponse struct {
	Code    ErrorCode `json:"code"`
	Message string    `json:"message"`
	Detail  string    `json:"detail,omitempty"`
	Current int       `json:"current"`  // Current active job count for this client
	Limit   int       `json:"limit"`    // Max allowed concurrent jobs per client
	RetryIn int       `json:"retry_in"` // Suggested retry delay in seconds
}

// --- Failure Types (TSI-757) ---

type FailureType string

const (
	FailureInputUnreachable   FailureType = "INPUT_UNREACHABLE"
	FailureEncoderUnsupported FailureType = "ENCODER_UNSUPPORTED"
	FailureDiskFull           FailureType = "DISK_FULL"
	FailureTimeout            FailureType = "TIMEOUT"
	FailureWorkerCrash        FailureType = "WORKER_CRASH"
	FailureFFmpegError        FailureType = "FFMPEG_ERROR"
	FailureNoWorkerAvailable  FailureType = "NO_WORKER_AVAILABLE"
	FailureInfra              FailureType = "INFRA"
)

func (f FailureType) IsValid() bool {
	switch f {
	case FailureInputUnreachable, FailureEncoderUnsupported, FailureDiskFull,
		FailureTimeout, FailureWorkerCrash, FailureFFmpegError,
		FailureNoWorkerAvailable, FailureInfra:
		return true
	default:
		return false
	}
}

func (f FailureType) Retryable() bool {
	switch f {
	case FailureTimeout, FailureWorkerCrash:
		return true
	default:
		return false
	}
}

// --- Probe Types (TSI-754) ---

type ProbeRequest struct {
	Input string `json:"input"`
}

type ProbeResponse struct {
	Format  interface{}  `json:"format,omitempty"`
	Streams interface{}  `json:"streams,omitempty"`
	Rffmpeg *RffmpegMeta `json:"_rffmpeg,omitempty"`
	Error   string       `json:"error,omitempty"`
	Message string       `json:"message,omitempty"`
}

type RffmpegMeta struct {
	WorkerEncoders []string           `json:"worker_encoders,omitempty"`
	Suggestion     *EncoderSuggestion `json:"suggestion,omitempty"`
	Workers        []WorkerSummary    `json:"workers,omitempty"`
}

// WorkerSummary is a lightweight worker representation for the probe _rffmpeg response.
type WorkerSummary struct {
	ID            string   `json:"id"`
	Name          string   `json:"name,omitempty"`
	Status        string   `json:"status"`
	GPUModel      string   `json:"gpu_model,omitempty"`
	Encoders      []string `json:"encoders"`
	FFmpegVersion string   `json:"ffmpeg_version"`
	MaxConcurrent int      `json:"max_concurrent"`
}

type EncoderSuggestion struct {
	RecommendedEncoder string `json:"recommended_encoder"`
	Reason             string `json:"reason"`
}

// --- Worker State (TSI-756) ---

type WorkerState struct {
	WorkerID        string    `json:"worker_id"`
	Status          string    `json:"status"` // online / offline / degraded / busy
	GPUMemUsedMB    int       `json:"gpu_mem_used_mb,omitempty"`
	GPUMetricsValid bool      `json:"gpu_metrics_valid"` // True when GPU metrics reflect a fresh sample from any source (TSI-2365/TSI-2466)
	GPUUtilPct      float64   `json:"gpu_util_percent,omitempty"`
	ActiveJobs      []string  `json:"active_jobs,omitempty"`
	ThroughputFPS   float64   `json:"throughput_fps,omitempty"`
	EWMAThroughput  float64   `json:"ewma_throughput,omitempty"` // EWMA-smoothed throughput
	Evicted         bool      `json:"evicted"`                   // Whether worker is a slow node (evicted from scheduling)
	QueueDepth      int       `json:"queue_depth,omitempty"`
	CompletedJobs   int       `json:"completed_jobs,omitempty"` // Cumulative jobs completed since the current registration
	LastSeen        time.Time `json:"last_seen"`
	StartedAt       time.Time `json:"started_at,omitempty"`
	// LastThroughputAt is the timestamp of the last heartbeat that reported a
	// non-zero throughput sample. Time-boxes the busy-worker eviction
	// exemption in slow-node detection.
	LastThroughputAt time.Time `json:"last_throughput_at,omitempty"`
}
