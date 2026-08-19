package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	tlspkg "github.com/tsix404/rffmpeg/pkg/tls"
)

// ServerConfig holds the complete server configuration
type ServerConfig struct {
	// Server settings
	Port    string `json:"port" yaml:"port"`
	DataDir string `json:"data_dir" yaml:"data_dir"`
	Version string `json:"version" yaml:"version"`

	// Authentication settings
	AuthToken string `json:"auth_token" yaml:"auth_token"` // PSK token for authentication

	// Worker management settings
	WorkerHeartbeatTimeout    time.Duration `json:"worker_heartbeat_timeout" yaml:"worker_heartbeat_timeout"`
	WorkerOfflineThreshold    time.Duration `json:"worker_offline_threshold" yaml:"worker_offline_threshold"`
	WorkerHealthCheckInterval time.Duration `json:"worker_health_check_interval" yaml:"worker_health_check_interval"`

	// WebSocket settings
	AllowedOrigins []string `json:"allowed_origins" yaml:"allowed_origins"`

	// Scheduler settings
	JobTimeout           time.Duration `json:"job_timeout" yaml:"job_timeout"`
	ScheduleInterval     time.Duration `json:"schedule_interval" yaml:"schedule_interval"`
	TimeoutCheckInterval time.Duration `json:"timeout_check_interval" yaml:"timeout_check_interval"`
	MaxJobsPerWorker     int           `json:"max_jobs_per_worker" yaml:"max_jobs_per_worker"`

	// Rate limit settings (per-client)
	RateLimitEnabled           bool `json:"rate_limit_enabled" yaml:"rate_limit_enabled"`
	MaxConcurrentJobsPerClient int  `json:"max_concurrent_jobs_per_client" yaml:"max_concurrent_jobs_per_client"`

	// Redis settings for distributed rate limiting (optional)
	RedisAddr     string `json:"redis_addr" yaml:"redis_addr"`
	RedisPassword string `json:"redis_password" yaml:"redis_password"`
	RedisDB       int    `json:"redis_db" yaml:"redis_db"`

	// TLS configuration
	TLS *tlspkg.Config `json:"tls" yaml:"tls"`
}

// Flags holds command-line flag values for merging into ServerConfig
type Flags struct {
	Port                       string
	DataDir                    string
	Version                    string
	Config                     string
	AuthToken                  string
	WorkerHeartbeatTimeout     string
	WorkerOfflineThreshold     string
	WorkerHealthCheckInterval  string
	JobTimeout                 string
	ScheduleInterval           string
	TimeoutCheckInterval       string
	MaxJobsPerWorker           int
	TLSEnabled                 bool
	TLSCertFile                string
	TLSKeyFile                 string
	TLSClientCAFile            string
	TLSMTLS                    bool
	TLSMinVersion              string
	TLSExpirationWarningDays   int
	AllowedOrigins             []string
	RateLimitEnabled           bool
	MaxConcurrentJobsPerClient int
	RedisAddr                  string
}

// DefaultServerConfig returns a ServerConfig with sensible defaults
func DefaultServerConfig() *ServerConfig {
	return &ServerConfig{
		Port:                       "8080",
		DataDir:                    "./data",
		Version:                    "1.0.0",
		WorkerHeartbeatTimeout:     90 * time.Second, // Mark offline after 90s without heartbeat
		WorkerOfflineThreshold:     10 * time.Minute, // Remove from pool after 10 min offline
		WorkerHealthCheckInterval:  30 * time.Second, // Check worker health every 30s
		JobTimeout:                 30 * time.Minute, // Job timeout after 30 minutes
		ScheduleInterval:           5 * time.Second,  // Schedule jobs every 5 seconds
		TimeoutCheckInterval:       30 * time.Second, // Check for timeouts every 30 seconds
		MaxJobsPerWorker:           1,                // One job at a time per worker by default
		RateLimitEnabled:           true,             // Enable per-client rate limiting by default
		MaxConcurrentJobsPerClient: 10,               // Max 10 concurrent jobs per client
		TLS:                        tlspkg.DefaultConfig(),
	}
}

// LoadFromFile loads configuration from a JSON file
func LoadFromFile(path string) (*ServerConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	config := DefaultServerConfig()
	if err := json.Unmarshal(data, config); err != nil {
		return nil, fmt.Errorf("failed to parse config file: %w", err)
	}

	// Ensure TLS is never nil (handles "tls": null in config file)
	if config.TLS == nil {
		config.TLS = tlspkg.DefaultConfig()
	}

	return config, nil
}

// LoadFromEnv loads configuration from environment variables
func LoadFromEnv() *ServerConfig {
	config := DefaultServerConfig()

	if port := os.Getenv("PORT"); port != "" {
		config.Port = port
	}
	if dataDir := os.Getenv("DATA_DIR"); dataDir != "" {
		config.DataDir = dataDir
	}
	if version := os.Getenv("VERSION"); version != "" {
		config.Version = version
	}

	// Authentication token
	if authToken := os.Getenv("RFFMPEG_SERVER_TOKEN"); authToken != "" {
		config.AuthToken = authToken
	}

	// Worker management environment variables
	if timeout := os.Getenv("WORKER_HEARTBEAT_TIMEOUT"); timeout != "" {
		if d, err := time.ParseDuration(timeout); err == nil {
			config.WorkerHeartbeatTimeout = d
		}
	}
	if threshold := os.Getenv("WORKER_OFFLINE_THRESHOLD"); threshold != "" {
		if d, err := time.ParseDuration(threshold); err == nil {
			config.WorkerOfflineThreshold = d
		}
	}
	if interval := os.Getenv("WORKER_HEALTH_CHECK_INTERVAL"); interval != "" {
		if d, err := time.ParseDuration(interval); err == nil {
			config.WorkerHealthCheckInterval = d
		}
	}

	// Scheduler environment variables
	if jobTimeout := os.Getenv("JOB_TIMEOUT"); jobTimeout != "" {
		if d, err := time.ParseDuration(jobTimeout); err == nil {
			config.JobTimeout = d
		}
	}
	if scheduleInterval := os.Getenv("SCHEDULE_INTERVAL"); scheduleInterval != "" {
		if d, err := time.ParseDuration(scheduleInterval); err == nil {
			config.ScheduleInterval = d
		}
	}
	if timeoutCheckInterval := os.Getenv("TIMEOUT_CHECK_INTERVAL"); timeoutCheckInterval != "" {
		if d, err := time.ParseDuration(timeoutCheckInterval); err == nil {
			config.TimeoutCheckInterval = d
		}
	}
	if maxJobs := os.Getenv("MAX_JOBS_PER_WORKER"); maxJobs != "" {
		if n, err := strconv.Atoi(maxJobs); err == nil && n > 0 {
			config.MaxJobsPerWorker = n
		}
	}

	// Rate limit environment variables
	if rateLimitEnabled := os.Getenv("RATE_LIMIT_ENABLED"); rateLimitEnabled == "false" || rateLimitEnabled == "0" {
		config.RateLimitEnabled = false
	}
	if maxPerClient := os.Getenv("MAX_CONCURRENT_JOBS_PER_CLIENT"); maxPerClient != "" {
		if n, err := strconv.Atoi(maxPerClient); err == nil && n > 0 {
			config.MaxConcurrentJobsPerClient = n
		}
	}

	// Redis environment variables (optional, for distributed rate limiting)
	if redisAddr := os.Getenv("REDIS_ADDR"); redisAddr != "" {
		config.RedisAddr = redisAddr
	}
	if redisPassword := os.Getenv("REDIS_PASSWORD"); redisPassword != "" {
		config.RedisPassword = redisPassword
	}
	if redisDB := os.Getenv("REDIS_DB"); redisDB != "" {
		if n, err := strconv.Atoi(redisDB); err == nil {
			config.RedisDB = n
		}
	}

	// TLS environment variables
	if tlsEnabled := os.Getenv("TLS_ENABLED"); tlsEnabled == "true" || tlsEnabled == "1" {
		config.TLS.Enabled = true
	}
	if certFile := os.Getenv("TLS_CERT_FILE"); certFile != "" {
		config.TLS.CertFile = certFile
	}
	if keyFile := os.Getenv("TLS_KEY_FILE"); keyFile != "" {
		config.TLS.KeyFile = keyFile
	}
	if clientCAFile := os.Getenv("TLS_CLIENT_CA_FILE"); clientCAFile != "" {
		config.TLS.ClientCAFile = clientCAFile
	}
	if mtls := os.Getenv("TLS_MTLS"); mtls == "true" || mtls == "1" {
		config.TLS.MTLS = true
	}

	// WebSocket allowed origins (comma-separated)
	if origins := os.Getenv("ALLOWED_ORIGINS"); origins != "" {
		config.AllowedOrigins = strings.Split(origins, ",")
	}

	return config
}

// Merge merges command-line flags into the configuration
func (c *ServerConfig) Merge(flags *Flags) {
	if flags.Port != "" {
		c.Port = flags.Port
	}
	if flags.DataDir != "" {
		c.DataDir = flags.DataDir
	}
	if flags.Version != "" {
		c.Version = flags.Version
	}
	if flags.AuthToken != "" {
		c.AuthToken = flags.AuthToken
	}

	// Worker management flags
	if flags.WorkerHeartbeatTimeout != "" {
		if d, err := time.ParseDuration(flags.WorkerHeartbeatTimeout); err == nil {
			c.WorkerHeartbeatTimeout = d
		}
	}
	if flags.WorkerOfflineThreshold != "" {
		if d, err := time.ParseDuration(flags.WorkerOfflineThreshold); err == nil {
			c.WorkerOfflineThreshold = d
		}
	}
	if flags.WorkerHealthCheckInterval != "" {
		if d, err := time.ParseDuration(flags.WorkerHealthCheckInterval); err == nil {
			c.WorkerHealthCheckInterval = d
		}
	}

	// Scheduler flags
	if flags.JobTimeout != "" {
		if d, err := time.ParseDuration(flags.JobTimeout); err == nil {
			c.JobTimeout = d
		}
	}
	if flags.ScheduleInterval != "" {
		if d, err := time.ParseDuration(flags.ScheduleInterval); err == nil {
			c.ScheduleInterval = d
		}
	}
	if flags.TimeoutCheckInterval != "" {
		if d, err := time.ParseDuration(flags.TimeoutCheckInterval); err == nil {
			c.TimeoutCheckInterval = d
		}
	}
	if flags.MaxJobsPerWorker > 0 {
		c.MaxJobsPerWorker = flags.MaxJobsPerWorker
	}

	// Rate limit flags
	if flags.RateLimitEnabled {
		c.RateLimitEnabled = true
	}
	if flags.MaxConcurrentJobsPerClient > 0 {
		c.MaxConcurrentJobsPerClient = flags.MaxConcurrentJobsPerClient
	}
	if flags.RedisAddr != "" {
		c.RedisAddr = flags.RedisAddr
	}

	// TLS flags - ensure TLS config exists
	if c.TLS == nil {
		c.TLS = tlspkg.DefaultConfig()
	}

	if flags.TLSEnabled {
		c.TLS.Enabled = true
	}
	if flags.TLSCertFile != "" {
		c.TLS.CertFile = flags.TLSCertFile
	}
	if flags.TLSKeyFile != "" {
		c.TLS.KeyFile = flags.TLSKeyFile
	}
	if flags.TLSClientCAFile != "" {
		c.TLS.ClientCAFile = flags.TLSClientCAFile
	}
	if flags.TLSMTLS {
		c.TLS.MTLS = true
	}
	if flags.TLSMinVersion != "" {
		c.TLS.MinVersion = parseTLSVersion(flags.TLSMinVersion)
	}
	if flags.TLSExpirationWarningDays > 0 {
		c.TLS.ExpirationWarningDays = flags.TLSExpirationWarningDays
	}
}

// Validate validates the server configuration
func (c *ServerConfig) Validate() error {
	if c.Port == "" {
		return fmt.Errorf("port is required")
	}
	if c.DataDir == "" {
		return fmt.Errorf("data directory is required")
	}

	// Validate TLS config if enabled
	if c.TLS != nil {
		if err := c.TLS.Validate(); err != nil {
			return fmt.Errorf("TLS configuration error: %w", err)
		}
	}

	return nil
}

// ResolvePaths resolves relative paths to absolute paths based on a base directory
func (c *ServerConfig) ResolvePaths(baseDir string) error {
	var err error

	c.DataDir, err = resolvePath(c.DataDir, baseDir)
	if err != nil {
		return err
	}

	if c.TLS != nil {
		if c.TLS.CertFile != "" {
			c.TLS.CertFile, err = resolvePath(c.TLS.CertFile, baseDir)
			if err != nil {
				return err
			}
		}
		if c.TLS.KeyFile != "" {
			c.TLS.KeyFile, err = resolvePath(c.TLS.KeyFile, baseDir)
			if err != nil {
				return err
			}
		}
		if c.TLS.ClientCAFile != "" {
			c.TLS.ClientCAFile, err = resolvePath(c.TLS.ClientCAFile, baseDir)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

// resolvePath resolves a path relative to baseDir if it's not absolute
func resolvePath(path, baseDir string) (string, error) {
	if filepath.IsAbs(path) {
		return path, nil
	}

	absBase, err := filepath.Abs(baseDir)
	if err != nil {
		return "", err
	}

	return filepath.Join(absBase, path), nil
}

// parseTLSVersion converts a string TLS version to uint16
func parseTLSVersion(version string) uint16 {
	switch strings.ToUpper(version) {
	case "TLS1.0", "TLS10", "1.0":
		return 0x0301 // tls.VersionTLS10
	case "TLS1.1", "TLS11", "1.1":
		return 0x0302 // tls.VersionTLS11
	case "TLS1.2", "TLS12", "1.2":
		return 0x0303 // tls.VersionTLS12
	case "TLS1.3", "TLS13", "1.3":
		return 0x0304 // tls.VersionTLS13
	default:
		return 0x0303 // Default to TLS 1.2
	}
}

// String returns a string representation of the config (for logging)
func (c *ServerConfig) String() string {
	tlsStatus := "disabled"
	if c.TLS != nil && c.TLS.Enabled {
		tlsStatus = "enabled"
		if c.TLS.MTLS {
			tlsStatus = "mTLS enabled"
		}
	}
	authStatus := "disabled"
	if c.AuthToken != "" {
		authStatus = "enabled"
	}
	return fmt.Sprintf("ServerConfig{port=%s, dataDir=%s, tls=%s, auth=%s, workerHeartbeatTimeout=%s, workerOfflineThreshold=%s, workerHealthCheckInterval=%s, jobTimeout=%s, scheduleInterval=%s, timeoutCheckInterval=%s, maxJobsPerWorker=%d, rateLimitEnabled=%v, maxConcurrentJobsPerClient=%d}",
		c.Port, c.DataDir, tlsStatus, authStatus, c.WorkerHeartbeatTimeout, c.WorkerOfflineThreshold, c.WorkerHealthCheckInterval,
		c.JobTimeout, c.ScheduleInterval, c.TimeoutCheckInterval, c.MaxJobsPerWorker, c.RateLimitEnabled, c.MaxConcurrentJobsPerClient)
}
