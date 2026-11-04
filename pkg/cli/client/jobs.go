package client

import (
	"fmt"
	"net/http"
)

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
