package client

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tsix404/rffmpeg/pkg/protocol"
)

const (
	UploadEndpoint  = "/api/v1/upload"
	JobsEndpoint    = "/api/v1/jobs"
	JobEndpoint     = "/api/v1/jobs/%s"
	OutputEndpoint  = "/api/v1/output/%s"
	ProbeEndpoint   = "/api/v1/probe"
	HealthEndpoint  = "/api/v1/health"
	DefaultTimeout  = 30 * time.Second
	UploadTimeout   = 10 * time.Minute
	DownloadTimeout = 10 * time.Minute
	PollInterval    = 2 * time.Second
	MaxRetries      = 3
	RetryDelay      = 1 * time.Second
)

// Client is the HTTP client for rffmpeg server
type Client struct {
	serverURL string
	token     string
	http      *http.Client
}

// New creates a new client
func New(serverURL, token string) *Client {
	return &Client{
		serverURL: strings.TrimSuffix(serverURL, "/"),
		token:     token,
		http: &http.Client{
			Timeout: DefaultTimeout,
		},
	}
}

// DrainAndClose drains the response body and closes it, returning any error
func DrainAndClose(body io.ReadCloser) error {
	if body == nil {
		return nil
	}
	_, drainErr := io.Copy(io.Discard, body)
	closeErr := body.Close()
	if drainErr != nil {
		return drainErr
	}
	return closeErr
}

// UploadFile uploads a file to the server using streaming to avoid loading
// large files entirely into memory.
func (c *Client) UploadFile(filePath string) (string, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return "", fmt.Errorf("failed to open file: %w", err)
	}
	defer file.Close()

	// Use io.Pipe for streaming upload to avoid loading large files into memory
	pr, pw := io.Pipe()
	writer := multipart.NewWriter(pw)

	// Get file info
	fileInfo, err := file.Stat()
	if err != nil {
		return "", fmt.Errorf("failed to get file info: %w", err)
	}

	// Calculate accurate Content-Length
	contentLength := calculateMultipartSize(writer.Boundary(), fileInfo.Size(), filepath.Base(filePath))

	// Write multipart form in a goroutine
	errChan := make(chan error, 1)
	go func() {
		defer pw.Close()
		defer writer.Close()

		part, err := writer.CreateFormFile("file", filepath.Base(filePath))
		if err != nil {
			errChan <- fmt.Errorf("failed to create form file: %w", err)
			return
		}

		if _, err := io.Copy(part, file); err != nil {
			errChan <- fmt.Errorf("failed to copy file: %w", err)
			return
		}
		errChan <- nil
	}()

	req, err := http.NewRequest("POST", c.serverURL+UploadEndpoint, pr)
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.ContentLength = contentLength
	c.setAuthHeader(req)

	// Use longer timeout for large files
	client := &http.Client{
		Timeout: UploadTimeout,
		Transport: &http.Transport{
			// Allow longer idle time for large uploads
			IdleConnTimeout: 5 * time.Minute,
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("upload request failed: %w", err)
	}
	defer DrainAndClose(resp.Body)

	// Check for errors from the goroutine
	if err := <-errChan; err != nil {
		return "", err
	}

	if resp.StatusCode != http.StatusOK {
		var errResp protocol.ErrorResponse
		if err := json.NewDecoder(resp.Body).Decode(&errResp); err == nil {
			return "", fmt.Errorf("upload failed: %s", errResp.Message)
		}
		return "", fmt.Errorf("upload failed with status %d", resp.StatusCode)
	}

	var uploadResp protocol.UploadResponse
	if err := json.NewDecoder(resp.Body).Decode(&uploadResp); err != nil {
		return "", fmt.Errorf("failed to decode response: %w", err)
	}

	return uploadResp.FileID, nil
}

// calculateMultipartSize calculates the exact size of a multipart form
// with the given boundary, file size, and filename.
func calculateMultipartSize(boundary string, fileSize int64, filename string) int64 {
	// Calculate exact multipart overhead
	// --boundary\r\n
	// Content-Disposition: form-data; name="file"; filename="..."\r\n
	// Content-Type: application/octet-stream\r\n
	// \r\n
	// [file content]
	// \r\n--boundary--\r\n

	preamble := fmt.Sprintf("--%s\r\n", boundary)
	contentDisposition := fmt.Sprintf("Content-Disposition: form-data; name=\"file\"; filename=\"%s\"\r\n", filename)
	contentType := "Content-Type: application/octet-stream\r\n"
	headerEnd := "\r\n"
	epilogue := fmt.Sprintf("\r\n--%s--\r\n", boundary)

	return int64(len(preamble)+len(contentDisposition)+len(contentType)+len(headerEnd)) +
		fileSize +
		int64(len(epilogue))
}

// SubmitJob submits a transcoding job
func (c *Client) SubmitJob(inputFiles []string, args []string, outputFilename string, autoHW bool) (string, error) {
	return c.SubmitJobWithOptions(inputFiles, nil, args, outputFilename, autoHW, false, 0)
}

// SubmitJobWithOptions submits a transcoding job with additional options.
// timeout is the job timeout duration (0 means use server default).
func (c *Client) SubmitJobWithOptions(inputFiles []string, directPath []string, args []string, outputFilename string, autoHW bool, streamingOutput bool, timeout time.Duration) (string, error) {
	req := protocol.JobSubmitRequest{
		InputFiles:      inputFiles,
		DirectPath:      directPath,
		Args:            args,
		OutputFilename:  outputFilename,
		AutoHW:          autoHW,
		StreamingOutput: streamingOutput,
	}

	// Convert timeout duration to absolute timestamp
	if timeout > 0 {
		deadline := time.Now().Add(timeout)
		req.Timeout = &deadline
	}

	body, err := json.Marshal(req)
	if err != nil {
		return "", fmt.Errorf("failed to marshal request: %w", err)
	}

	httpReq, err := http.NewRequest("POST", c.serverURL+JobsEndpoint, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	c.setAuthHeader(httpReq)

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("submit job request failed: %w", err)
	}
	defer DrainAndClose(resp.Body)

	if resp.StatusCode != http.StatusOK {
		// Handle rate limit (429) with detailed info
		if resp.StatusCode == http.StatusTooManyRequests {
			bodyBytes, readErr := io.ReadAll(resp.Body)
			if readErr == nil {
				var rlResp protocol.RateLimitResponse
				if json.Unmarshal(bodyBytes, &rlResp) == nil {
					return "", fmt.Errorf(
						"rate limit exceeded: %d/%d concurrent jobs.\n        %s\n        Retry after %d seconds, or wait for existing jobs to complete.",
						rlResp.Current, rlResp.Limit, rlResp.Message, rlResp.RetryIn,
					)
				}
				// Fallback: show raw body
				return "", fmt.Errorf("rate limit exceeded (HTTP 429): %s", string(bodyBytes))
			}
			return "", fmt.Errorf("rate limit exceeded (HTTP 429)")
		}
		var errResp protocol.ErrorResponse
		if err := json.NewDecoder(resp.Body).Decode(&errResp); err == nil {
			return "", fmt.Errorf("job submission failed [%s]: %s", errResp.Code, errResp.Message)
		}
		return "", fmt.Errorf("job submission failed with status %d", resp.StatusCode)
	}

	var jobResp protocol.JobSubmitResponse
	if err := json.NewDecoder(resp.Body).Decode(&jobResp); err != nil {
		return "", fmt.Errorf("failed to decode response: %w", err)
	}

	return jobResp.JobID, nil
}

// GetJob gets job status
func (c *Client) GetJob(jobID string) (*protocol.JobInfo, error) {
	url := fmt.Sprintf(c.serverURL+JobEndpoint, jobID)

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	c.setAuthHeader(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("get job request failed: %w", err)
	}
	defer DrainAndClose(resp.Body)

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("job not found: %s", jobID)
	}

	if resp.StatusCode != http.StatusOK {
		var errResp protocol.ErrorResponse
		if err := json.NewDecoder(resp.Body).Decode(&errResp); err == nil {
			return nil, fmt.Errorf("get job failed: %s", errResp.Message)
		}
		return nil, fmt.Errorf("get job failed with status %d", resp.StatusCode)
	}

	var statusResp protocol.JobStatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&statusResp); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return &statusResp.Job, nil
}

// WaitForJob waits for job completion and returns exit code.
// It polls until the job reaches a terminal status or ctx is cancelled.
func (c *Client) WaitForJob(ctx context.Context, jobID string, showProgress bool) (*protocol.JobInfo, error) {
	ticker := time.NewTicker(PollInterval)
	defer ticker.Stop()

	for {
		job, err := c.GetJob(jobID)
		if err != nil {
			return nil, err
		}

		if showProgress {
			fmt.Fprintf(os.Stderr, "Job %s: %s\n", jobID, job.Status)
		}

		if protocol.IsTerminalStatus(job.Status) {
			return job, nil
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

// WaitForJobWithLogs waits for job completion with real-time log streaming via WebSocket
func (c *Client) WaitForJobWithLogs(ctx context.Context, jobID string, quiet bool) (*protocol.JobInfo, error) {
	// Start WebSocket connection for real-time logs
	wsClient := NewWSClient(c.serverURL, jobID, c.token,
		WithOnStderr(func(chunk string) {
			if !quiet {
				fmt.Fprint(os.Stderr, chunk)
			}
		}),
		WithOnStatus(func(status protocol.JobStatus, exitCode int, err string) {
			if !quiet && status != protocol.JobStatusRunning {
				fmt.Fprintf(os.Stderr, "Job status: %s", status)
				if err != "" {
					fmt.Fprintf(os.Stderr, " (error: %s)", err)
				}
				fmt.Fprintln(os.Stderr)
			}
		}),
		WithOnProgress(func(percent float64) {
			if !quiet {
				fmt.Fprintf(os.Stderr, "Progress: %.1f%%\n", percent)
			}
		}),
	)
	defer wsClient.Close()

	// Connect to WebSocket
	if err := wsClient.ConnectWithReconnect(ctx); err != nil {
		// If WebSocket fails, fall back to HTTP polling
		fmt.Fprintf(os.Stderr, "Warning: WebSocket connection failed, falling back to HTTP polling: %v\n", err)
		return c.WaitForJob(ctx, jobID, !quiet)
	}

	// Start listening in a goroutine
	listenDone := make(chan error, 1)
	go func() {
		listenDone <- wsClient.Listen(ctx)
	}()

	// Also poll for job completion as a backup
	pollDone := make(chan *protocol.JobInfo, 1)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(PollInterval):
				job, err := c.GetJob(jobID)
				if err != nil {
					continue
				}
				if protocol.IsTerminalStatus(job.Status) {
					pollDone <- job
					return
				}
			}
		}
	}()

	// Wait for completion, error, or context cancellation
	select {
	case job := <-pollDone:
		return job, nil
	case err := <-listenDone:
		if err != nil {
			// WebSocket failed, fall back to polling
			return c.WaitForJob(ctx, jobID, !quiet)
		}
		// WebSocket closed normally; poll until terminal status is reached.
		// A single GetJob call may return a non-terminal status if the
		// WebSocket closes before the server DB is updated.
		return c.WaitForJob(ctx, jobID, !quiet)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// WaitForJobWithStreamingOutput waits for job completion with streaming output to stdout.
// This is used when the output file is "-" to stream transcoded data directly to stdout.
func (c *Client) WaitForJobWithStreamingOutput(ctx context.Context, jobID string, quiet bool) (*protocol.JobInfo, error) {
	// Start WebSocket connection for real-time stdout streaming
	wsClient := NewWSClient(c.serverURL, jobID, c.token,
		WithOnStdout(func(chunk []byte) {
			// Write raw decoded stdout chunks directly to stdout
			os.Stdout.Write(chunk)
		}),
		WithOnStderr(func(chunk string) {
			// Write stderr to stderr (progress info, etc.)
			if !quiet {
				fmt.Fprint(os.Stderr, chunk)
			}
		}),
		WithOnStatus(func(status protocol.JobStatus, exitCode int, err string) {
			if !quiet && status != protocol.JobStatusRunning {
				fmt.Fprintf(os.Stderr, "Job status: %s", status)
				if err != "" {
					fmt.Fprintf(os.Stderr, " (error: %s)", err)
				}
				fmt.Fprintln(os.Stderr)
			}
		}),
		WithOnProgress(func(percent float64) {
			if !quiet {
				fmt.Fprintf(os.Stderr, "Progress: %.1f%%\n", percent)
			}
		}),
	)
	defer wsClient.Close()

	// Connect to WebSocket
	if err := wsClient.ConnectWithReconnect(ctx); err != nil {
		// If WebSocket fails, fall back to HTTP polling (without streaming output)
		fmt.Fprintf(os.Stderr, "Warning: WebSocket connection failed, falling back to HTTP polling: %v\n", err)
		return c.WaitForJob(ctx, jobID, !quiet)
	}

	// Start listening in a goroutine
	listenDone := make(chan error, 1)
	go func() {
		listenDone <- wsClient.Listen(ctx)
	}()

	// Poll for job completion
	pollDone := make(chan *protocol.JobInfo, 1)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(PollInterval):
				job, err := c.GetJob(jobID)
				if err != nil {
					continue
				}
				if protocol.IsTerminalStatus(job.Status) {
					pollDone <- job
					return
				}
			}
		}
	}()

	// Wait for completion, error, or context cancellation
	select {
	case job := <-pollDone:
		return job, nil
	case err := <-listenDone:
		if err != nil {
			// WebSocket failed, fall back to polling
			return c.WaitForJob(ctx, jobID, !quiet)
		}
		// WebSocket closed normally; poll until terminal status is reached.
		// A single GetJob call may return a non-terminal status if the
		// WebSocket closes before the server DB is updated.
		return c.WaitForJob(ctx, jobID, !quiet)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// DownloadOutput downloads an output file
func (c *Client) DownloadOutput(fileID, outputPath string) error {
	url := fmt.Sprintf(c.serverURL+OutputEndpoint, fileID)

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	c.setAuthHeader(req)

	client := &http.Client{Timeout: DownloadTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("download request failed: %w", err)
	}
	defer DrainAndClose(resp.Body)

	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("output file not found: %s", fileID)
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download failed with status %d", resp.StatusCode)
	}

	// Create output directory if needed
	dir := filepath.Dir(outputPath)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("failed to create output directory: %w", err)
		}
	}

	file, err := os.Create(outputPath)
	if err != nil {
		return fmt.Errorf("failed to create output file: %w", err)
	}
	defer file.Close()

	if _, err := io.Copy(file, resp.Body); err != nil {
		return fmt.Errorf("failed to write output file: %w", err)
	}

	return nil
}

// HealthCheck checks server health
func (c *Client) HealthCheck() error {
	req, err := http.NewRequest("GET", c.serverURL+HealthEndpoint, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	c.setAuthHeader(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("health check failed: %w", err)
	}
	defer DrainAndClose(resp.Body)

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("server unhealthy: status %d", resp.StatusCode)
	}

	return nil
}

// Probe sends a probe request to the server and returns media information.
func (c *Client) Probe(input string) (*protocol.ProbeResponse, error) {
	reqBody := protocol.ProbeRequest{Input: input}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal probe request: %w", err)
	}

	httpReq, err := http.NewRequest("POST", c.serverURL+ProbeEndpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create probe request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	c.setAuthHeader(httpReq)

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("probe request failed: %w", err)
	}
	defer DrainAndClose(resp.Body)

	if resp.StatusCode != http.StatusOK {
		var probeResp protocol.ProbeResponse
		if decodeErr := json.NewDecoder(resp.Body).Decode(&probeResp); decodeErr == nil && probeResp.Message != "" {
			return nil, fmt.Errorf("probe failed: %s", probeResp.Message)
		}
		return nil, fmt.Errorf("probe failed with status %d", resp.StatusCode)
	}

	var probeResp protocol.ProbeResponse
	if err := json.NewDecoder(resp.Body).Decode(&probeResp); err != nil {
		return nil, fmt.Errorf("failed to decode probe response: %w", err)
	}

	return &probeResp, nil
}

// CancelJob cancels a job
func (c *Client) CancelJob(jobID string) error {
	url := fmt.Sprintf(c.serverURL+JobEndpoint, jobID)

	req, err := http.NewRequest("DELETE", url, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	c.setAuthHeader(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("cancel request failed: %w", err)
	}
	defer DrainAndClose(resp.Body)

	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("job not found: %s", jobID)
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("cancel failed with status %d", resp.StatusCode)
	}

	return nil
}

// setAuthHeader sets the authorization header if token is present
func (c *Client) setAuthHeader(req *http.Request) {
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
}

// WorkerHealth carries live runtime metrics for a worker (TSI-2219).
type WorkerHealth struct {
	Status        string   `json:"status"`
	GPUUtilPct    float64  `json:"gpu_util_percent,omitempty"`
	GPUMemUsedMB  int      `json:"gpu_mem_used_mb,omitempty"`
	ActiveJobs    []string `json:"active_jobs,omitempty"`
	ThroughputFPS float64  `json:"throughput_fps,omitempty"`
	LastSeen      string   `json:"last_seen"`
}

// WorkerInfo represents worker information from the API
type WorkerInfo struct {
	ID            string        `json:"id"`
	Name          string        `json:"name,omitempty"`
	Status        string        `json:"status"`
	GPUModel      string        `json:"gpu_model,omitempty"`
	Encoders      []string      `json:"encoders"`
	Decoders      []string      `json:"decoders,omitempty"`
	FFmpegVersion string        `json:"ffmpeg_version"`
	MaxConcurrent int           `json:"max_concurrent"`
	LastHeartbeat string        `json:"last_heartbeat"`
	CreatedAt     string        `json:"created_at"`
	Hwaccels      string        `json:"hwaccels,omitempty"`
	Codecs        string        `json:"codecs,omitempty"`
	Filters       string        `json:"filters,omitempty"`
	PixFmts       string        `json:"pix_fmts,omitempty"`
	Formats       string        `json:"formats,omitempty"`
	Health        *WorkerHealth `json:"health"`
}

// ListWorkersResponse is the response for listing workers
type ListWorkersResponse struct {
	Workers []WorkerInfo `json:"workers"`
}

// ListEncodersResponse is the response for listing encoders
type ListEncodersResponse struct {
	Encoders []protocol.EncoderInfo `json:"encoders"`
}

// ListDecodersResponse is the response for listing decoders
type ListDecodersResponse struct {
	Decoders []protocol.DecoderInfo `json:"decoders"`
}

// ListHwaccelsResponse is the response for listing hwaccels
type ListHwaccelsResponse struct {
	Hwaccels []string `json:"hwaccels"`
}

// ListWorkersByEncoderResponse is the response for listing workers by encoder
type ListWorkersByEncoderResponse struct {
	Encoder string       `json:"encoder"`
	Workers []WorkerInfo `json:"workers"`
}

// ListWorkers retrieves all workers from the server
func (c *Client) ListWorkers() ([]WorkerInfo, error) {
	url := c.serverURL + "/api/v1/workers"

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	c.setAuthHeader(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("list workers request failed: %w", err)
	}
	defer DrainAndClose(resp.Body)

	if resp.StatusCode != http.StatusOK {
		var errResp protocol.ErrorResponse
		if err := json.NewDecoder(resp.Body).Decode(&errResp); err == nil {
			return nil, fmt.Errorf("list workers failed: %s", errResp.Message)
		}
		return nil, fmt.Errorf("list workers failed with status %d", resp.StatusCode)
	}

	var listResp ListWorkersResponse
	if err := json.NewDecoder(resp.Body).Decode(&listResp); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return listResp.Workers, nil
}

// GetWorker retrieves a single worker by ID
func (c *Client) GetWorker(workerID string) (*WorkerInfo, error) {
	url := c.serverURL + "/api/v1/workers/" + workerID

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	c.setAuthHeader(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("get worker request failed: %w", err)
	}
	defer DrainAndClose(resp.Body)

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("worker not found: %s", workerID)
	}

	if resp.StatusCode != http.StatusOK {
		var errResp protocol.ErrorResponse
		if err := json.NewDecoder(resp.Body).Decode(&errResp); err == nil {
			return nil, fmt.Errorf("get worker failed: %s", errResp.Message)
		}
		return nil, fmt.Errorf("get worker failed with status %d", resp.StatusCode)
	}

	var workerResp struct {
		Worker WorkerInfo `json:"worker"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&workerResp); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return &workerResp.Worker, nil
}

// ListAllEncoders retrieves all unique encoders across all workers
func (c *Client) ListAllEncoders() ([]protocol.EncoderInfo, error) {
	url := c.serverURL + "/api/v1/encoders"

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	c.setAuthHeader(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("list encoders request failed: %w", err)
	}
	defer DrainAndClose(resp.Body)

	if resp.StatusCode != http.StatusOK {
		var errResp protocol.ErrorResponse
		if err := json.NewDecoder(resp.Body).Decode(&errResp); err == nil {
			return nil, fmt.Errorf("list encoders failed: %s", errResp.Message)
		}
		return nil, fmt.Errorf("list encoders failed with status %d", resp.StatusCode)
	}

	var listResp ListEncodersResponse
	if err := json.NewDecoder(resp.Body).Decode(&listResp); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return listResp.Encoders, nil
}

// ListAllDecoders retrieves all unique decoders across all workers
func (c *Client) ListAllDecoders() ([]protocol.DecoderInfo, error) {
	url := c.serverURL + "/api/v1/decoders"

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	c.setAuthHeader(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("list decoders request failed: %w", err)
	}
	defer DrainAndClose(resp.Body)

	if resp.StatusCode != http.StatusOK {
		var errResp protocol.ErrorResponse
		if err := json.NewDecoder(resp.Body).Decode(&errResp); err == nil {
			return nil, fmt.Errorf("list decoders failed: %s", errResp.Message)
		}
		return nil, fmt.Errorf("list decoders failed with status %d", resp.StatusCode)
	}

	var listResp ListDecodersResponse
	if err := json.NewDecoder(resp.Body).Decode(&listResp); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return listResp.Decoders, nil
}

// ListAllHwaccels retrieves all unique hwaccels across all workers.
// The API returns text/plain by default; pass json=true for JSON.
func (c *Client) ListAllHwaccels() ([]string, error) {
	url := c.serverURL + "/api/v1/hwaccels"

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	c.setAuthHeader(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("list hwaccels request failed: %w", err)
	}
	defer DrainAndClose(resp.Body)

	if resp.StatusCode != http.StatusOK {
		var errResp protocol.ErrorResponse
		if err := json.NewDecoder(resp.Body).Decode(&errResp); err == nil {
			return nil, fmt.Errorf("list hwaccels failed: %s", errResp.Message)
		}
		return nil, fmt.Errorf("list hwaccels failed with status %d", resp.StatusCode)
	}

	// Read the response body (text/plain, one hwaccel per line)
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read hwaccels response: %w", err)
	}

	lines := strings.Split(strings.TrimSpace(string(body)), "\n")
	result := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" {
			result = append(result, line)
		}
	}
	return result, nil
}

// ListAllFilters retrieves all unique filters across all workers.
// The API returns text/plain by default; pass json=true for JSON.
func (c *Client) ListAllFilters(jsonOut bool) ([]string, error) {
	url := c.serverURL + "/api/v1/filters"
	if jsonOut {
		url += "?json=1"
	}

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	c.setAuthHeader(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("list filters request failed: %w", err)
	}
	defer DrainAndClose(resp.Body)

	if resp.StatusCode != http.StatusOK {
		var errResp protocol.ErrorResponse
		if err := json.NewDecoder(resp.Body).Decode(&errResp); err == nil {
			return nil, fmt.Errorf("list filters failed: %s", errResp.Message)
		}
		return nil, fmt.Errorf("list filters failed with status %d", resp.StatusCode)
	}

	if jsonOut {
		var listResp struct {
			Items []string `json:"items"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&listResp); err != nil {
			return nil, fmt.Errorf("failed to decode response: %w", err)
		}
		return listResp.Items, nil
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read filters response: %w", err)
	}

	lines := strings.Split(strings.TrimSpace(string(body)), "\n")
	result := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || line == "Filters:" {
			continue
		}
		result = append(result, line)
	}
	return result, nil
}

// ListAllPixFmts retrieves all unique pixel formats across all workers.
// The API returns text/plain by default; pass json=true for JSON.
func (c *Client) ListAllPixFmts(jsonOut bool) ([]string, error) {
	url := c.serverURL + "/api/v1/pix_fmts"
	if jsonOut {
		url += "?json=1"
	}

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	c.setAuthHeader(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("list pix_fmts request failed: %w", err)
	}
	defer DrainAndClose(resp.Body)

	if resp.StatusCode != http.StatusOK {
		var errResp protocol.ErrorResponse
		if err := json.NewDecoder(resp.Body).Decode(&errResp); err == nil {
			return nil, fmt.Errorf("list pix_fmts failed: %s", errResp.Message)
		}
		return nil, fmt.Errorf("list pix_fmts failed with status %d", resp.StatusCode)
	}

	if jsonOut {
		var listResp struct {
			Items []string `json:"items"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&listResp); err != nil {
			return nil, fmt.Errorf("failed to decode response: %w", err)
		}
		return listResp.Items, nil
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read pix_fmts response: %w", err)
	}

	lines := strings.Split(strings.TrimSpace(string(body)), "\n")
	result := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || line == "Pixel formats:" {
			continue
		}
		result = append(result, line)
	}
	return result, nil
}

// ListAllFormats retrieves all unique formats across all workers.
// The API returns text/plain by default; pass json=true for JSON.
func (c *Client) ListAllFormats(jsonOut bool) ([]string, error) {
	url := c.serverURL + "/api/v1/formats"
	if jsonOut {
		url += "?json=1"
	}

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	c.setAuthHeader(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("list formats request failed: %w", err)
	}
	defer DrainAndClose(resp.Body)

	if resp.StatusCode != http.StatusOK {
		var errResp protocol.ErrorResponse
		if err := json.NewDecoder(resp.Body).Decode(&errResp); err == nil {
			return nil, fmt.Errorf("list formats failed: %s", errResp.Message)
		}
		return nil, fmt.Errorf("list formats failed with status %d", resp.StatusCode)
	}

	if jsonOut {
		var listResp struct {
			Items []string `json:"items"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&listResp); err != nil {
			return nil, fmt.Errorf("failed to decode response: %w", err)
		}
		return listResp.Items, nil
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read formats response: %w", err)
	}

	lines := strings.Split(strings.TrimSpace(string(body)), "\n")
	result := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || line == "File formats:" {
			continue
		}
		result = append(result, line)
	}
	return result, nil
}

// ListWorkersByEncoder retrieves workers that have a specific encoder
func (c *Client) ListWorkersByEncoder(encoderName string) ([]WorkerInfo, error) {
	url := c.serverURL + "/api/v1/encoders/" + encoderName + "/workers"

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	c.setAuthHeader(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("list workers by encoder request failed: %w", err)
	}
	defer DrainAndClose(resp.Body)

	if resp.StatusCode != http.StatusOK {
		var errResp protocol.ErrorResponse
		if err := json.NewDecoder(resp.Body).Decode(&errResp); err == nil {
			return nil, fmt.Errorf("list workers by encoder failed: %s", errResp.Message)
		}
		return nil, fmt.Errorf("list workers by encoder failed with status %d", resp.StatusCode)
	}

	var listResp ListWorkersByEncoderResponse
	if err := json.NewDecoder(resp.Body).Decode(&listResp); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return listResp.Workers, nil
}

// Chunked upload constants
const (
	// DefaultChunkSize is the default chunk size for chunked uploads (10MB)
	DefaultChunkSize = 10 * 1024 * 1024
	// LargeFileThreshold is the threshold above which chunked upload is used (100MB)
	LargeFileThreshold = 100 * 1024 * 1024
	// MaxChunkRetries is the maximum number of retries for chunk upload
	MaxChunkRetries = 3
	// InitialRetryDelay is the initial delay for retry
	InitialRetryDelay = 1 * time.Second
	// MaxRetryDelay is the maximum delay for retry
	MaxRetryDelay = 30 * time.Second
)

// UploadFileChunked uploads a large file using the chunked upload API.
// This is more reliable for very large files (>100MB) as it uploads in smaller chunks.
func (c *Client) UploadFileChunked(filePath string, chunkSize int64) (string, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return "", fmt.Errorf("failed to open file: %w", err)
	}
	defer file.Close()

	fileInfo, err := file.Stat()
	if err != nil {
		return "", fmt.Errorf("failed to get file info: %w", err)
	}

	if chunkSize <= 0 {
		chunkSize = DefaultChunkSize
	}

	// Calculate file checksum for integrity verification
	fileHash := sha256.New()
	if _, err := io.Copy(fileHash, file); err != nil {
		return "", fmt.Errorf("failed to calculate file checksum: %w", err)
	}
	fileChecksum := hex.EncodeToString(fileHash.Sum(nil))

	// Reset file position after reading
	if _, err := file.Seek(0, 0); err != nil {
		return "", fmt.Errorf("failed to reset file position: %w", err)
	}

	// Initialize chunked upload session
	initReq := protocol.ChunkUploadInitRequest{
		Filename:  filepath.Base(filePath),
		FileSize:  fileInfo.Size(),
		ChunkSize: chunkSize,
		Checksum:  fileChecksum, // File integrity checksum
	}

	initBody, err := json.Marshal(initReq)
	if err != nil {
		return "", fmt.Errorf("failed to marshal init request: %w", err)
	}

	req, err := http.NewRequest("POST", c.serverURL+"/api/v1/upload/init", bytes.NewReader(initBody))
	if err != nil {
		return "", fmt.Errorf("failed to create init request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	c.setAuthHeader(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("init request failed: %w", err)
	}
	defer DrainAndClose(resp.Body)

	if resp.StatusCode != http.StatusOK {
		var errResp protocol.ErrorResponse
		if err := json.NewDecoder(resp.Body).Decode(&errResp); err == nil {
			return "", fmt.Errorf("init failed: %s", errResp.Message)
		}
		return "", fmt.Errorf("init failed with status %d", resp.StatusCode)
	}

	var initResp protocol.ChunkUploadInitResponse
	if err := json.NewDecoder(resp.Body).Decode(&initResp); err != nil {
		return "", fmt.Errorf("failed to decode init response: %w", err)
	}

	// Upload chunks
	uploadID := initResp.UploadID
	totalChunks := initResp.TotalChunks
	actualChunkSize := initResp.ChunkSize

	for i := 0; i < totalChunks; i++ {
		// Upload chunk with retry
		if err := c.uploadChunkWithRetry(file, uploadID, i, actualChunkSize, fileInfo.Size(), totalChunks); err != nil {
			return "", fmt.Errorf("chunk %d upload failed after retries: %w", i, err)
		}
	}

	// Complete upload
	completeReq := protocol.ChunkUploadCompleteRequest{
		UploadID: uploadID,
		Checksum: fileChecksum, // Final verification checksum
	}
	completeBody, err := json.Marshal(completeReq)
	if err != nil {
		return "", fmt.Errorf("failed to marshal complete request: %w", err)
	}

	req, err = http.NewRequest("POST", c.serverURL+"/api/v1/upload/complete", bytes.NewReader(completeBody))
	if err != nil {
		return "", fmt.Errorf("failed to create complete request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	c.setAuthHeader(req)

	resp, err = c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("complete request failed: %w", err)
	}
	defer DrainAndClose(resp.Body)

	if resp.StatusCode != http.StatusOK {
		var errResp protocol.ErrorResponse
		if err := json.NewDecoder(resp.Body).Decode(&errResp); err == nil {
			return "", fmt.Errorf("complete failed: %s", errResp.Message)
		}
		return "", fmt.Errorf("complete failed with status %d", resp.StatusCode)
	}

	var completeResp protocol.ChunkUploadCompleteResponse
	if err := json.NewDecoder(resp.Body).Decode(&completeResp); err != nil {
		return "", fmt.Errorf("failed to decode complete response: %w", err)
	}

	return completeResp.FileID, nil
}

// uploadChunkWithRetry uploads a single chunk with exponential backoff retry
func (c *Client) uploadChunkWithRetry(file *os.File, uploadID string, chunkIndex int, chunkSize int64, fileSize int64, totalChunks int) error {
	var lastErr error

	for attempt := 0; attempt < MaxChunkRetries; attempt++ {
		if attempt > 0 {
			// Exponential backoff
			delay := InitialRetryDelay * time.Duration(1<<(attempt-1))
			if delay > MaxRetryDelay {
				delay = MaxRetryDelay
			}
			time.Sleep(delay)
		}

		err := c.uploadSingleChunk(file, uploadID, chunkIndex, chunkSize, fileSize, totalChunks)
		if err == nil {
			return nil // Success
		}
		lastErr = err

		// Check if error is retryable
		if !isRetryableError(err) {
			return err
		}
	}

	return lastErr
}

// isRetryableError determines if an error is worth retrying
func isRetryableError(err error) bool {
	if err == nil {
		return false
	}
	// Network errors, timeouts, and 5xx errors are retryable
	errStr := err.Error()
	return strings.Contains(errStr, "timeout") ||
		strings.Contains(errStr, "connection reset") ||
		strings.Contains(errStr, "temporary") ||
		strings.Contains(errStr, "unavailable") ||
		strings.Contains(errStr, "5xx") ||
		strings.Contains(errStr, "502") ||
		strings.Contains(errStr, "503") ||
		strings.Contains(errStr, "504")
}

// uploadSingleChunk uploads a single chunk without retry logic
func (c *Client) uploadSingleChunk(file *os.File, uploadID string, chunkIndex int, chunkSize int64, fileSize int64, totalChunks int) error {
	// Seek to chunk position
	offset := int64(chunkIndex) * chunkSize
	if _, err := file.Seek(offset, 0); err != nil {
		return fmt.Errorf("failed to seek to chunk %d: %w", chunkIndex, err)
	}

	// Calculate chunk size for this chunk (last chunk may be smaller)
	currentChunkSize := chunkSize
	if offset+chunkSize > fileSize {
		currentChunkSize = fileSize - offset
	}

	// Create a hash for checksum calculation
	chunkHash := sha256.New()

	// Create a tee reader to read chunk data and compute checksum simultaneously
	limitedReader := io.LimitReader(file, currentChunkSize)
	teeReader := io.TeeReader(limitedReader, chunkHash)

	// Read chunk data into buffer for checksum and upload
	chunkData, err := io.ReadAll(teeReader)
	if err != nil {
		return fmt.Errorf("failed to read chunk %d: %w", chunkIndex, err)
	}

	// Calculate chunk checksum
	chunkChecksum := hex.EncodeToString(chunkHash.Sum(nil))

	// Upload chunk using streaming
	pr, pw := io.Pipe()
	writer := multipart.NewWriter(pw)

	// Calculate accurate Content-Length
	contentLength := calculateChunkMultipartSize(writer.Boundary(), currentChunkSize, chunkChecksum)

	errChan := make(chan error, 1)
	go func() {
		defer pw.Close()
		defer writer.Close()

		part, err := writer.CreateFormFile("chunk", "chunk")
		if err != nil {
			errChan <- err
			return
		}

		if _, err := part.Write(chunkData); err != nil {
			errChan <- err
			return
		}

		// Add checksum as form field
		_ = writer.WriteField("checksum", chunkChecksum)

		errChan <- nil
	}()

	chunkURL := fmt.Sprintf("%s/api/v1/upload/chunk/%s/%d", c.serverURL, uploadID, chunkIndex)
	req, err := http.NewRequest("POST", chunkURL, pr)
	if err != nil {
		return fmt.Errorf("failed to create chunk request: %w", err)
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.ContentLength = contentLength
	c.setAuthHeader(req)

	// Dynamic timeout based on chunk size
	timeout := calculateChunkTimeout(currentChunkSize)
	client := &http.Client{Timeout: timeout}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("chunk request failed: %w", err)
	}
	defer DrainAndClose(resp.Body)

	if err := <-errChan; err != nil {
		return fmt.Errorf("chunk write failed: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		var errResp protocol.ErrorResponse
		if err := json.NewDecoder(resp.Body).Decode(&errResp); err == nil {
			return fmt.Errorf("chunk upload failed: %s", errResp.Message)
		}
		return fmt.Errorf("chunk upload failed with status %d", resp.StatusCode)
	}

	return nil
}

// calculateChunkMultipartSize calculates the exact size of a chunk multipart form
func calculateChunkMultipartSize(boundary string, chunkSize int64, checksum string) int64 {
	// Calculate exact multipart overhead for chunk
	// --boundary\r\n
	// Content-Disposition: form-data; name="chunk"; filename="chunk"\r\n
	// Content-Type: application/octet-stream\r\n
	// \r\n
	// [chunk content]
	// \r\n--boundary\r\n
	// Content-Disposition: form-data; name="checksum"\r\n
	// \r\n
	// [checksum]
	// \r\n--boundary--\r\n

	preamble := fmt.Sprintf("--%s\r\n", boundary)
	contentDisposition := "Content-Disposition: form-data; name=\"chunk\"; filename=\"chunk\"\r\n"
	contentType := "Content-Type: application/octet-stream\r\n"
	headerEnd := "\r\n"

	checksumHeader := fmt.Sprintf("\r\n--%s\r\nContent-Disposition: form-data; name=\"checksum\"\r\n\r\n", boundary)
	checksumValue := checksum
	epilogue := fmt.Sprintf("\r\n--%s--\r\n", boundary)

	return int64(len(preamble)+len(contentDisposition)+len(contentType)+len(headerEnd)) +
		chunkSize +
		int64(len(checksumHeader)+len(checksumValue)+len(epilogue))
}

// calculateChunkTimeout calculates timeout based on chunk size
func calculateChunkTimeout(chunkSize int64) time.Duration {
	// Base timeout: 30 seconds for 10MB
	// Scale linearly: 3 seconds per MB
	baseTimeout := 30 * time.Second
	mbPerSec := int64(3 * 1024 * 1024) // 3MB/s

	timeout := baseTimeout + time.Duration(chunkSize/mbPerSec)*time.Second
	if timeout > UploadTimeout {
		timeout = UploadTimeout
	}
	return timeout
}

// UploadFileAuto automatically chooses between simple and chunked upload based on file size.
// Files larger than LargeFileThreshold (100MB) use chunked upload.
func (c *Client) UploadFileAuto(filePath string) (string, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return "", fmt.Errorf("failed to open file: %w", err)
	}
	defer file.Close()

	fileInfo, err := file.Stat()
	if err != nil {
		return "", fmt.Errorf("failed to get file info: %w", err)
	}

	// Use chunked upload for large files
	if fileInfo.Size() > LargeFileThreshold {
		return c.UploadFileChunked(filePath, DefaultChunkSize)
	}

	// Use simple streaming upload for smaller files
	return c.UploadFile(filePath)
}

// computeFileChecksum computes SHA256 checksum of a file
func computeFileChecksum(filePath string) (string, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return "", fmt.Errorf("failed to open file: %w", err)
	}
	defer file.Close()

	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", fmt.Errorf("failed to compute checksum: %w", err)
	}

	return hex.EncodeToString(hash.Sum(nil)), nil
}

// HashingReader wraps an io.Reader and computes a hash while reading
type HashingReader struct {
	reader io.Reader
	hash   hash.Hash
}

// NewHashingReader creates a new HashingReader
func NewHashingReader(reader io.Reader) *HashingReader {
	return &HashingReader{
		reader: reader,
		hash:   sha256.New(),
	}
}

// Read implements io.Reader
func (h *HashingReader) Read(p []byte) (n int, err error) {
	n, err = h.reader.Read(p)
	if n > 0 {
		h.hash.Write(p[:n])
	}
	return n, err
}

// Checksum returns the hex-encoded checksum
func (h *HashingReader) Checksum() string {
	return hex.EncodeToString(h.hash.Sum(nil))
}
