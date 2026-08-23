package worker

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tsix404/rffmpeg/pkg/protocol"
	"github.com/tsix404/rffmpeg/pkg/worker/gpu"
)

// apiPrefix is the API version prefix for all server endpoints
const apiPrefix = "/api/v1"

// Client is the HTTP client for communicating with the server
type Client struct {
	baseURL    string
	httpClient *http.Client
	workerID   string
	token      string
}

// NewClient creates a new client for server communication.
// The baseURL should be the server address (e.g., "http://localhost:8080").
// If baseURL does not already end with /api/v1, it will be appended automatically.
func NewClient(baseURL, workerID, token string) *Client {
	// Normalize baseURL: ensure it ends with /api/v1 for backward compatibility
	normalizedURL := normalizeBaseURL(baseURL)

	return &Client{
		baseURL:  normalizedURL,
		workerID: workerID,
		token:    token,
		httpClient: &http.Client{
			Timeout: 30 * time.Minute, // Long timeout for large file transfers
		},
	}
}

// normalizeBaseURL ensures the baseURL ends with /api/v1.
// This provides backward compatibility for users who provide the server URL
// without the /api/v1 prefix (e.g., "http://localhost:8080" instead of "http://localhost:8080/api/v1").
func normalizeBaseURL(baseURL string) string {
	baseURL = strings.TrimSuffix(baseURL, "/")
	if strings.HasSuffix(baseURL, apiPrefix) {
		return baseURL
	}
	return baseURL + apiPrefix
}

// setAuthHeader sets the Authorization header if a token is configured
func (c *Client) setAuthHeader(req *http.Request) {
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
}

// Register registers the worker with the server
func (c *Client) Register(name string, caps protocol.WorkerCapabilities) (string, error) {
	req := protocol.WorkerRegisterRequest{
		WorkerID:     c.workerID,
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
// gpuMetrics carries GPU utilization samples; zero values mean "not available"
// and are omitted from the wire payload.
func (c *Client) Heartbeat(status protocol.WorkerStatus, activeJobs []string, throughputFPS float64, completedJobs int, gpuMetrics gpu.Metrics) ([]string, error) {
	req := protocol.WorkerHeartbeatRequest{
		WorkerID:      c.workerID,
		Status:        status,
		ActiveJobs:    activeJobs,
		ThroughputFPS: throughputFPS,
		CompletedJobs: completedJobs,
		GPUUtilPct:    gpuMetrics.UtilPct,
		GPUMemUsedMB:  gpuMetrics.MemUsedMB,
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
	url := fmt.Sprintf("%s/workers/%s/jobs", c.baseURL, c.workerID)

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
// If fileID contains "://", it is treated as a remote URL and downloaded directly.
// Otherwise, it is treated as a server-side file ID and fetched from the server.
func (c *Client) DownloadInput(fileID, destPath string) error {
	var downloadURL string
	var req *http.Request
	var err error

	if isRemoteURL(fileID) {
		// Remote URL — download directly
		downloadURL = fileID
		req, err = http.NewRequest("GET", downloadURL, nil)
		if err != nil {
			return fmt.Errorf("failed to create request for URL %s: %w", fileID, err)
		}
		// No auth header for external URLs
	} else {
		// Server file ID — fetch from server
		downloadURL = fmt.Sprintf("%s/files/%s", c.baseURL, fileID)
		req, err = http.NewRequest("GET", downloadURL, nil)
		if err != nil {
			return fmt.Errorf("failed to create request: %w", err)
		}
		c.setAuthHeader(req)
	}

	resp, err := c.httpClient.Do(req)
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

	// Copy the content
	_, err = io.Copy(file, resp.Body)
	if err != nil {
		return fmt.Errorf("failed to write file: %w", err)
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
	}

	url := fmt.Sprintf("%s/jobs/%s", c.baseURL, jobID)
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
	}

	url := fmt.Sprintf("%s/jobs/%s", c.baseURL, jobID)
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
func (c *Client) SendStdoutChunk(jobID string, chunk string) error {
	req := protocol.JobUpdateRequest{
		StdoutChunk: chunk,
	}

	url := fmt.Sprintf("%s/jobs/%s", c.baseURL, jobID)
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

// UploadOutput uploads an output file to the server
func (c *Client) UploadOutput(jobID, filePath string) error {
	// Open the file
	file, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("failed to open file: %w", err)
	}
	defer file.Close()

	// Create multipart form
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)

	part, err := writer.CreateFormFile("file", filepath.Base(filePath))
	if err != nil {
		return fmt.Errorf("failed to create form file: %w", err)
	}

	_, err = io.Copy(part, file)
	if err != nil {
		return fmt.Errorf("failed to copy file content: %w", err)
	}

	writer.Close()

	// Create request
	url := fmt.Sprintf("%s/jobs/%s/output", c.baseURL, jobID)
	httpReq, err := http.NewRequest("POST", url, &buf)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", writer.FormDataContentType())
	c.setAuthHeader(httpReq)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("failed to upload output: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("upload output failed with status: %d", resp.StatusCode)
	}

	return nil
}

// doRequest is a helper for making JSON requests
func (c *Client) doRequest(method, path string, body interface{}) (*http.Response, error) {
	url := c.baseURL + path

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
