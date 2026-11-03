package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"sync"
	"time"
)

// FFprobeExecutor runs ffprobe commands for media file analysis. It is shared
// by concurrent jobs (and by the parallel duration probe), so its mutable
// state is guarded.
type FFprobeExecutor struct {
	mu          sync.Mutex
	ffprobePath string
	timeout     time.Duration
	validated   bool // set true after ValidatePath succeeds
}

// NewFFprobeExecutor creates a new ffprobe executor.
func NewFFprobeExecutor(ffprobePath string) *FFprobeExecutor {
	if ffprobePath == "" {
		ffprobePath = "ffprobe"
	}
	return &FFprobeExecutor{
		ffprobePath: ffprobePath,
		timeout:     30 * time.Second,
	}
}

// SetTimeout sets the execution timeout.
func (e *FFprobeExecutor) SetTimeout(d time.Duration) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.timeout = d
}

// ValidatePath checks if the ffprobe executable exists and is executable.
func (e *FFprobeExecutor) ValidatePath() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.validatePathLocked()
}

func (e *FFprobeExecutor) validatePathLocked() error {
	path, err := exec.LookPath(e.ffprobePath)
	if err != nil {
		return fmt.Errorf("ffprobe executable not found: %q: %w", e.ffprobePath, err)
	}
	e.ffprobePath = path
	e.validated = true
	return nil
}

// FFprobeResult holds the parsed ffprobe output.
type FFprobeResult struct {
	Format  map[string]interface{}   `json:"format,omitempty"`
	Streams []map[string]interface{} `json:"streams,omitempty"`
}

// Probe runs ffprobe against the given input and returns structured results.
func (e *FFprobeExecutor) Probe(ctx context.Context, input string) (*FFprobeResult, error) {
	e.mu.Lock()
	if !e.validated {
		if err := e.validatePathLocked(); err != nil {
			e.mu.Unlock()
			return nil, err
		}
	}
	ffprobePath := e.ffprobePath
	timeout := e.timeout
	e.mu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	args := []string{
		"-print_format", "json",
		"-show_format",
		"-show_streams",
		input,
	}

	cmd := exec.CommandContext(ctx, ffprobePath, args...)
	output, err := cmd.Output()
	if err != nil {
		// Try to capture stderr
		stderr := ""
		if exitErr, ok := err.(*exec.ExitError); ok {
			stderr = string(exitErr.Stderr)
		}
		if ctx.Err() == context.DeadlineExceeded {
			return nil, fmt.Errorf("ffprobe timed out after %v", timeout)
		}
		return nil, fmt.Errorf("ffprobe failed: %w (stderr: %s)", err, stderr)
	}

	var result FFprobeResult
	if err := json.Unmarshal(output, &result); err != nil {
		return nil, fmt.Errorf("failed to parse ffprobe JSON output: %w", err)
	}

	return &result, nil
}
