package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
)

const (
	DefaultServerURL = "http://localhost:8080"
)

// Config holds CLI configuration
type Config struct {
	ServerURL string `json:"server_url"`
	Token     string `json:"token,omitempty"`
	SharedFS  bool   `json:"shared_fs,omitempty"` // Shared filesystem mode: skip upload/download
	// MaxRetries bounds the CLI's WS/HTTP retry loops (RFFMPEG_MAX_RETRIES).
	// nil means "unset" (fall back to client.DefaultMaxRetries); a non-nil 0
	// means "no retries" — fail fast instead of silently falling back.
	MaxRetries *int `json:"max_retries,omitempty"`
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

	// Negative max_retries in the config file is invalid: treat it as unset.
	if cfg.MaxRetries != nil && *cfg.MaxRetries < 0 {
		cfg.MaxRetries = nil
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
	if mr := os.Getenv("RFFMPEG_MAX_RETRIES"); mr != "" {
		if n, err := strconv.Atoi(mr); err == nil && n >= 0 {
			cfg.MaxRetries = &n
		}
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
