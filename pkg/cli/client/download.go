package client

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
)

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
