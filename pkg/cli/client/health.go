package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/tsic404/rffmpeg/pkg/protocol"
)

// healthProbeTimeout and healthProbeBackoff are the effective health-probe
// budget; vars (rather than const references) so tests can shrink the retry
// loop instead of waiting out real probe timeouts.
var (
	healthProbeTimeout = HealthProbeTimeout
	healthProbeBackoff = HealthProbeBackoff
)

// HealthCheck reports whether the server answers a health probe.
//
// The probe runs on its own client and attempt budget instead of the shared
// business client (DefaultTimeout): a readiness gate must not queue behind
// business traffic, and one stalled answer must not abort the command — a
// server busy enough to miss a probe (a concurrent-client burst) can still
// serve the job that follows. Only attempts with no answer at all are retried;
// a refused connection or a non-200 status is reported at once.
func (c *Client) HealthCheck() error {
	probe := &http.Client{Timeout: healthProbeTimeout}
	var lastErr error
	for attempt := 1; attempt <= HealthProbeAttempts; attempt++ {
		retryable, err := c.probeHealth(probe)
		if err == nil {
			return nil
		}
		if !retryable {
			return err
		}
		lastErr = err
		if attempt < HealthProbeAttempts {
			time.Sleep(healthProbeBackoff << (attempt - 1))
		}
	}
	return fmt.Errorf("%w (after %d attempts)", lastErr, HealthProbeAttempts)
}

// probeHealth performs one health probe attempt and reports whether a further
// attempt could succeed.
func (c *Client) probeHealth(probe *http.Client) (bool, error) {
	req, err := http.NewRequest("GET", c.serverURL+HealthEndpoint, nil)
	if err != nil {
		return false, fmt.Errorf("failed to create request: %w", err)
	}

	c.setAuthHeader(req)

	resp, err := probe.Do(req)
	if err != nil {
		return isProbeTimeout(err), fmt.Errorf("health check failed: %w", err)
	}
	defer DrainAndClose(resp.Body)

	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("server unhealthy: status %d", resp.StatusCode)
	}

	return false, nil
}

// isProbeTimeout reports whether err is a probe that got no answer within its
// time budget — the failure a retry can clear. A refused connection is not
// retried: it means nothing is listening, and no retry changes that.
func isProbeTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
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

	probeClient := &http.Client{Timeout: probeTimeout}
	resp, err := probeClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("probe request failed: %w", err)
	}
	defer DrainAndClose(resp.Body)

	if resp.StatusCode != http.StatusOK {
		// The probe endpoint shares the job-submission guard and rate limiter,
		// so its rejections (503 worker_unavailable, 429) come back as
		// ErrorResponse bodies. Decoding the error envelope — not
		// ProbeResponse, which has no code field — keeps the classification
		// code on the line, exactly like the job-submission path. A probe
		// failure body without a code still surfaces its message.
		var errResp protocol.ErrorResponse
		if err := json.NewDecoder(resp.Body).Decode(&errResp); err != nil {
			errResp = protocol.ErrorResponse{}
		}
		return nil, serverRejection("probe", resp.StatusCode, errResp.Code, errResp.Message)
	}

	var probeResp protocol.ProbeResponse
	if err := json.NewDecoder(resp.Body).Decode(&probeResp); err != nil {
		return nil, fmt.Errorf("failed to decode probe response: %w", err)
	}

	return &probeResp, nil
}
