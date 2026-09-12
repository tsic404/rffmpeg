package worker

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/tsic404/rffmpeg/pkg/pathutil"
	"github.com/tsic404/rffmpeg/pkg/protocol"
	"github.com/tsic404/rffmpeg/pkg/worker/gpu"
)

// apiSuffix is the canonical API path appended to a normalized root URL.
const apiSuffix = "/api/v1"

// ErrJobConflict is returned when the server rejects a terminal status update
// with HTTP 409 Conflict: the job has already reached a terminal state (or the
// reporting worker no longer owns it). A late worker report racing a CLI
// cancel or a concurrent completion is a benign lost race — the job's final
// state is already correct, so callers should log at info level, not error.
var ErrJobConflict = errors.New("job already in terminal state or not owned by this worker")

// IsConflict reports whether err is a 409 Conflict from a terminal-status
// update. Callers use it to suppress benign log noise from lost races.
func IsConflict(err error) bool { return errors.Is(err, ErrJobConflict) }

// MaxRemoteInputBytes caps the size of files downloaded from remote URLs via
// DownloadInput. Server-side file downloads are already bounded by the
// server's own upload limits; remote URLs have no such bound.
const MaxRemoteInputBytes int64 = 20 * 1024 * 1024 * 1024 // 20GB

// Client is the HTTP client for communicating with the server
type Client struct {
	baseURL    string
	httpClient *http.Client // control plane: register/heartbeat/pull/status — short timeout
	dataClient *http.Client // data plane: file download/upload — long timeout
	workerID   string
	workerMu   sync.RWMutex
	token      string
}

// NewClient creates a new client for server communication.
// The baseURL should be the server address (e.g., "http://localhost:8080");
// a user-supplied trailing /api/v1 (with or without trailing slashes) is
// stripped so each request builder can re-append it as the canonical prefix.
func NewClient(baseURL, workerID, token string) *Client {
	normalizedURL := normalizeBaseURL(baseURL)

	return &Client{
		baseURL:  normalizedURL,
		workerID: workerID,
		token:    token,
		httpClient: &http.Client{
			Timeout: 15 * time.Second, // control-plane requests must fail fast; a hung heartbeat gets the worker marked offline
		},
		dataClient: &http.Client{
			Timeout: 30 * time.Minute, // large media transfers need a generous ceiling
		},
	}
}

// normalizeBaseURL strips a trailing /api/v1 (with or without trailing
// slashes) from a server root URL. Each request builder appends its own
// /api/v1-relative path, so a user-supplied "http://host/api/v1" would
// otherwise produce "http://host/api/v1/api/v1/..." and fail with 404.
// This mirrors the CLI's normalizeServerURL so both sides accept the same
// URLs.
func normalizeBaseURL(baseURL string) string {
	baseURL = strings.TrimSuffix(baseURL, "/")
	baseURL = strings.TrimSuffix(baseURL, apiSuffix)
	return strings.TrimSuffix(baseURL, "/")
}

// setAuthHeader sets the Authorization header if a token is configured
func (c *Client) setAuthHeader(req *http.Request) {
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
}

// SetWorkerID updates the worker identity used in API paths and payloads.
// It exists because re-registration (after a server restart) swaps the
// worker ID while other goroutines may concurrently send heartbeats or pull
// jobs; the swap must not race with those reads.
func (c *Client) SetWorkerID(workerID string) {
	c.workerMu.Lock()
	defer c.workerMu.Unlock()
	c.workerID = workerID
}

// getWorkerID returns the current worker identity.
func (c *Client) getWorkerID() string {
	c.workerMu.Lock()
	defer c.workerMu.Unlock()
	return c.workerID
}

// Register registers the worker with the server
func (c *Client) Register(name string, caps protocol.WorkerCapabilities) (string, error) {
	req := protocol.WorkerRegisterRequest{
		WorkerID:     c.getWorkerID(),
		Name:         name,
		Capabilities: caps,
	}

	resp, err := c.doRequest("POST", "/workers/register", req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var result protocol.WorkerRegisterResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("failed to decode response: %w", err)
	}

	return result.WorkerID, nil
}

// Heartbeat sends a heartbeat to the server and returns any cancelled job IDs.
// gpuMetrics carries GPU utilization samples. When Metrics.Valid is false the
// GPU fields are sent as 0, which receivers treat as "no sample available";
// a valid sample with UtilPct == 0 is a real 0% reading and must survive the
// wire (TSI-2365). Valid samples may come from nvidia-smi, intel_gpu_top, or
// amdgpu sysfs (TSI-2466).
func (c *Client) Heartbeat(status protocol.WorkerStatus, activeJobs []string, throughputFPS float64, completedJobs int, gpuMetrics gpu.Metrics) ([]string, error) {
	req := protocol.WorkerHeartbeatRequest{
		WorkerID:        c.getWorkerID(),
		Status:          status,
		ActiveJobs:      activeJobs,
		ThroughputFPS:   throughputFPS,
		CompletedJobs:   completedJobs,
		GPUUtilPct:      gpuMetrics.UtilPct,
		GPUMemUsedMB:    gpuMetrics.MemUsedMB,
		GPUMetricsValid: gpuMetrics.Valid,
	}

	resp, err := c.doRequest("POST", "/workers/heartbeat", req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result protocol.WorkerHeartbeatResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return result.CancelledJobs, nil
}

// PullJobs pulls pending jobs assigned to this worker
func (c *Client) PullJobs() ([]protocol.JobInfo, error) {
	url := fmt.Sprintf("%s%s/workers/%s/jobs", c.baseURL, apiSuffix, c.getWorkerID())

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	c.setAuthHeader(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to pull jobs: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("pull jobs failed with status: %d", resp.StatusCode)
	}

	var result protocol.WorkerJobPullResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return result.Jobs, nil
}

// DownloadInput downloads an input file from the server or directly from a remote URL.
// If fileID is a remote URL per pathutil.IsRemoteURL (scheme-prefixed, excluding the local
// "file" scheme), it is downloaded directly. Otherwise, it is treated as a
// server-side file ID and fetched from the server.
func (c *Client) DownloadInput(fileID, destPath string) error {
	var downloadURL string
	var req *http.Request
	var err error

	if pathutil.IsRemoteURL(fileID) {
		// Remote URL — download directly
		downloadURL = fileID
		req, err = http.NewRequest("GET", downloadURL, nil)
		if err != nil {
			return fmt.Errorf("failed to create request for URL %s: %w", fileID, err)
		}
		// No auth header for external URLs
	} else {
		// Server file ID — fetch from server
		downloadURL = fmt.Sprintf("%s%s/files/%s", c.baseURL, apiSuffix, fileID)
		req, err = http.NewRequest("GET", downloadURL, nil)
		if err != nil {
			return fmt.Errorf("failed to create request: %w", err)
		}
		c.setAuthHeader(req)
	}
	resp, err := c.dataClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to download file: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download failed with status: %d", resp.StatusCode)
	}

	// Create destination directory if needed
	if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
		return fmt.Errorf("failed to create directory: %w", err)
	}

	// Create the file
	file, err := os.Create(destPath)
	if err != nil {
		return fmt.Errorf("failed to create file: %w", err)
	}
	defer file.Close()

	// Copy the content with a hard size cap: a hostile remote URL must not
	// be able to fill the disk unbounded via DownloadInput.
	limited := io.LimitReader(resp.Body, MaxRemoteInputBytes)
	written, err := io.Copy(file, limited)
	if err != nil {
		return fmt.Errorf("failed to write file: %w", err)
	}
	if written >= MaxRemoteInputBytes {
		return fmt.Errorf("remote input exceeds maximum size of %d bytes", MaxRemoteInputBytes)
	}

	return nil
}

// UpdateJob updates the job status on the server.
func (c *Client) UpdateJob(jobID string, status protocol.JobStatus, exitCode int, errMsg string, cached bool) error {
	return c.UpdateJobWithFailure(jobID, status, exitCode, errMsg, cached, "", "")
}

// UpdateJobWithFailure updates the job status with optional failure classification.
func (c *Client) UpdateJobWithFailure(jobID string, status protocol.JobStatus, exitCode int, errMsg string, cached bool, failureType, failureDetails string) error {
	req := protocol.JobUpdateRequest{
		Status:         status,
		ExitCode:       exitCode,
		Error:          errMsg,
		Cached:         cached,
		FailureType:    failureType,
		FailureDetails: failureDetails,
		WorkerID:       c.workerID, // ownership guard: server rejects stale reports from a worker that lost the job
	}

	url := fmt.Sprintf("%s%s/jobs/%s", c.baseURL, apiSuffix, jobID)
	jsonBody, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("failed to marshal request: %w", err)
	}

	httpReq, err := http.NewRequest("PATCH", url, bytes.NewReader(jsonBody))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	c.setAuthHeader(httpReq)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("failed to update job: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// 409 Conflict: the job already reached a terminal state (e.g. CLI
		// cancel raced the worker's timeout/completion report) or the
		// reporting worker no longer owns it. The final state is already
		// correct, so surface a typed error callers can suppress instead of
		// a generic status string that reads as a server fault.
		if resp.StatusCode == http.StatusConflict {
			return fmt.Errorf("%w: update job failed with status: %d", ErrJobConflict, resp.StatusCode)
		}
		return fmt.Errorf("update job failed with status: %d", resp.StatusCode)
	}

	return nil
}

// SendStderrChunk sends a stderr chunk to the server for real-time log streaming
// without changing the job status. This prevents race conditions where a late
// stderr flush could overwrite a completed status.
func (c *Client) SendStderrChunk(jobID string, chunk string) error {
	req := protocol.JobUpdateRequest{
		StderrChunk: chunk,
		WorkerID:    c.workerID,
	}

	url := fmt.Sprintf("%s%s/jobs/%s", c.baseURL, apiSuffix, jobID)
	jsonBody, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("failed to marshal request: %w", err)
	}

	httpReq, err := http.NewRequest("PATCH", url, bytes.NewReader(jsonBody))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	c.setAuthHeader(httpReq)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("failed to send stderr chunk: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("send stderr chunk failed with status: %d", resp.StatusCode)
	}

	return nil
}

// SendStdoutChunk sends a stdout chunk to the server for real-time output streaming.
// This is used in streaming output mode where ffmpeg writes to stdout instead of a file.
// The chunk is arbitrary binary data, so it is base64-encoded before JSON transport:
// encoding/json would otherwise replace invalid UTF-8 bytes with U+FFFD.
func (c *Client) SendStdoutChunk(jobID string, chunk []byte) error {
	req := protocol.JobUpdateRequest{
		StdoutChunk: protocol.EncodeStdoutChunk(chunk),
		WorkerID:    c.workerID,
	}

	url := fmt.Sprintf("%s%s/jobs/%s", c.baseURL, apiSuffix, jobID)
	jsonBody, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("failed to marshal request: %w", err)
	}

	httpReq, err := http.NewRequest("PATCH", url, bytes.NewReader(jsonBody))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	c.setAuthHeader(httpReq)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("failed to send stdout chunk: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("send stdout chunk failed with status: %d", resp.StatusCode)
	}
	return nil
}

// UploadOutput uploads an output file to the server. The body is streamed
// through a pipe with an exact Content-Length so the whole file is never
// buffered in memory (TSI-2365).
func (c *Client) UploadOutput(jobID, filePath string) error {
	file, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("failed to open file: %w", err)
	}
	defer file.Close()

	pr, pw := io.Pipe()
	writer := multipart.NewWriter(pw)

	// Feed the multipart body from a goroutine: io.Pipe is synchronous, so
	// the writer must run concurrently with the HTTP client's reads.
	errChan := make(chan error, 1)
	go func() {
		defer close(errChan)
		part, perr := writer.CreateFormFile("file", filepath.Base(filePath))
		if perr != nil {
			pw.CloseWithError(fmt.Errorf("failed to create form file: %w", perr))
			return
		}
		if _, cerr := io.Copy(part, file); cerr != nil {
			pw.CloseWithError(fmt.Errorf("failed to copy file content: %w", cerr))
			return
		}
		pw.CloseWithError(writer.Close())
	}()

	url := fmt.Sprintf("%s%s/jobs/%s/output", c.baseURL, apiSuffix, jobID)
	httpReq, err := http.NewRequest("POST", url, pr)
	if err != nil {
		pr.Close()
		return fmt.Errorf("failed to create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", writer.FormDataContentType())
	c.setAuthHeader(httpReq)
	resp, err := c.dataClient.Do(httpReq)
	if err != nil {
		pr.Close()
		return fmt.Errorf("failed to upload output: %w", err)
	}
	defer resp.Body.Close()

	if copyErr := <-errChan; copyErr != nil {
		return copyErr
	}

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		msg := strings.TrimSpace(string(body))
		if msg != "" {
			return fmt.Errorf("upload output failed with status: %d: %s", resp.StatusCode, msg)
		}
		return fmt.Errorf("upload output failed with status: %d", resp.StatusCode)
	}

	return nil
}

// doRequest is a helper for making JSON requests
func (c *Client) doRequest(method, path string, body interface{}) (*http.Response, error) {
	url := c.baseURL + apiSuffix + path

	var reqBody io.Reader
	if body != nil {
		jsonBody, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal request: %w", err)
		}
		reqBody = bytes.NewReader(jsonBody)
	}

	req, err := http.NewRequest(method, url, reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	c.setAuthHeader(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}

	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		var errResp protocol.ErrorResponse
		if err := json.NewDecoder(resp.Body).Decode(&errResp); err == nil {
			return nil, fmt.Errorf("%s: %s", errResp.Code, errResp.Message)
		}
		return nil, fmt.Errorf("request failed with status: %d", resp.StatusCode)
	}

	return resp, nil
}
