package client

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/tsic404/rffmpeg/pkg/protocol"
)

// WorkerHealth carries live runtime metrics for a worker.
// The gpu_* fields are always present in the server response; gpu_metrics_valid
// distinguishes a real 0% reading from "no sample taken".
type WorkerHealth struct {
	Status         string   `json:"status"`
	GPUUtilPct     float64  `json:"gpu_util_percent"`
	GPUMemUsedMB   int      `json:"gpu_mem_used_mb"`
	ActiveJobs     []string `json:"active_jobs,omitempty"`
	JobsPerSec     float64  `json:"jobs_per_sec"`
	EWMAJobsPerSec float64  `json:"ewma_jobs_per_sec,omitempty"`
	CompletedJobs  int      `json:"completed_jobs"`
	LastSeen       string   `json:"last_seen"`
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
		if errResp, ok := parseErrorResponse(resp.Body); ok {
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
		if errResp, ok := parseErrorResponse(resp.Body); ok {
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
		if errResp, ok := parseErrorResponse(resp.Body); ok {
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
		if errResp, ok := parseErrorResponse(resp.Body); ok {
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
		if errResp, ok := parseErrorResponse(resp.Body); ok {
			return nil, fmt.Errorf("list hwaccels failed: %s", errResp.Message)
		}
		return nil, fmt.Errorf("list hwaccels failed with status %d", resp.StatusCode)
	}

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
		if errResp, ok := parseErrorResponse(resp.Body); ok {
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
		if errResp, ok := parseErrorResponse(resp.Body); ok {
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
		if errResp, ok := parseErrorResponse(resp.Body); ok {
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
		if errResp, ok := parseErrorResponse(resp.Body); ok {
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
