package config

import (
	"encoding/json"
	"os"
	"path/filepath"
)

const (
	DefaultServerURL = "http://localhost:8080"
)

// Config holds CLI configuration
type Config struct {
	ServerURL string `json:"server_url"`
	Token     string `json:"token,omitempty"`
	SharedFS  bool   `json:"shared_fs,omitempty"` // Shared filesystem mode: skip upload/download
}

// Load reads configuration from file
// Search order: ./rffmpeg.json, ~/.rffmpeg.json, /etc/rffmpeg.json
func Load() (*Config, error) {
	cfg := &Config{
		ServerURL: DefaultServerURL,
	}

	// Config file locations in priority order
	configPaths := []string{
		"./rffmpeg.json",
		filepath.Join(os.Getenv("HOME"), ".rffmpeg.json"),
		"/etc/rffmpeg.json",
	}

	for _, path := range configPaths {
		if data, err := os.ReadFile(path); err == nil {
			if err := json.Unmarshal(data, cfg); err != nil {
				return nil, err
			}
			break
		}
	}

	// Environment variables override config file
	if url := os.Getenv("RFFMPEG_SERVER_URL"); url != "" {
		cfg.ServerURL = url
	}
	if token := os.Getenv("RFFMPEG_TOKEN"); token != "" {
		cfg.Token = token
	}
	if sharedFS := os.Getenv("RFFMPEG_SHARED_FS"); sharedFS != "" {
		cfg.SharedFS = sharedFS == "1" || sharedFS == "true"
	}

	return cfg, nil
}

// IsSharedFS returns true if shared filesystem mode is enabled.
// This is a convenience method; callers may also check cfg.SharedFS directly.
func (c *Config) IsSharedFS() bool {
	return c.SharedFS
}

// Save writes configuration to file in current directory
func (c *Config) Save(path string) error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}
