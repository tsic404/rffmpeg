package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

const (
	DefaultServerURL = "http://localhost:8080"

	// DefaultPollTimeout is the client-side cap on how long the CLI polls a
	// pending (waiting-for-worker) job for a terminal status before giving up.
	// It is distinct from --timeout, which is the worker-side ffmpeg execution
	// budget: a job stuck pending behind busy-but-live workers never receives a
	// server verdict deadline, so without this cap the CLI would poll forever.
	DefaultPollTimeout = 10 * time.Minute
)

// Duration is a time.Duration that unmarshals from either a JSON string
// ("30s", "5m", "2h") or a numeric nanosecond value. It lets rffmpeg.json set
// poll_timeout as a human-readable duration; time.Duration alone cannot parse
// the string form from JSON.
type Duration time.Duration

// UnmarshalJSON implements json.Unmarshaler.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var v interface{}
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	switch val := v.(type) {
	case float64:
		*d = Duration(time.Duration(val))
		return nil
	case string:
		tmp, err := time.ParseDuration(val)
		if err != nil {
			return err
		}
		*d = Duration(tmp)
		return nil
	default:
		return fmt.Errorf("invalid duration type: %T", v)
	}
}

// Config holds CLI configuration
type Config struct {
	ServerURL string `json:"server_url"`
	Token     string `json:"token,omitempty"`
	SharedFS  bool   `json:"shared_fs,omitempty"` // Shared filesystem mode: skip upload/download
	// MaxRetries bounds the CLI's WS/HTTP retry loops (RFFMPEG_MAX_RETRIES).
	// nil means "unset" (fall back to client.DefaultMaxRetries); a non-nil 0
	// means "no retries" — fail fast instead of silently falling back.
	MaxRetries *int `json:"max_retries,omitempty"`

	// PollTimeout caps how long the CLI polls a pending (waiting-for-worker)
	// job before giving up (--poll-timeout / RFFMPEG_POLL_TIMEOUT / rffmpeg.json
	// "poll_timeout"). nil means "unset" (fall back to DefaultPollTimeout); a
	// non-nil 0 means "no cap" — poll indefinitely, the operator opts out.
	PollTimeout *Duration `json:"poll_timeout,omitempty"`
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
	// A negative poll_timeout is invalid: treat it as unset (fall back to
	// DefaultPollTimeout). 0 is valid and means "no cap" (opt out).
	if cfg.PollTimeout != nil && *cfg.PollTimeout < 0 {
		cfg.PollTimeout = nil
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
	if pt := os.Getenv("RFFMPEG_POLL_TIMEOUT"); pt != "" {
		if d, err := time.ParseDuration(pt); err == nil && d >= 0 {
			v := Duration(d)
			cfg.PollTimeout = &v
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
