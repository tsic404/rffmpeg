package config

import (
	"encoding/json"
	"fmt"
	"log"
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
	// MaxRetryCount bounds how many times the health monitor may migrate a
	// job from a failed worker before failing it. 0 disables migration.
	MaxRetryCount int `json:"max_retry_count" yaml:"max_retry_count"`

	// WebSocket settings
	AllowedOrigins []string `json:"allowed_origins" yaml:"allowed_origins"`

	// Scheduler settings
	JobTimeout           time.Duration `json:"job_timeout" yaml:"job_timeout"`
	ScheduleInterval     time.Duration `json:"schedule_interval" yaml:"schedule_interval"`
	TimeoutCheckInterval time.Duration `json:"timeout_check_interval" yaml:"timeout_check_interval"`
	MaxJobsPerWorker     int           `json:"max_jobs_per_worker" yaml:"max_jobs_per_worker"`
	// NoWorkerJobTimeout fails pending jobs waiting longer than this with no
	// schedulable worker (all offline/evicted) as NO_WORKER_AVAILABLE.
	// 0 disables the check.
	NoWorkerJobTimeout time.Duration `json:"no_worker_job_timeout" yaml:"no_worker_job_timeout"`
	// MaxTimeoutRetries bounds how many times the scheduler may requeue a
	// timed-out job before failing it as TIMEOUT. 0 fails on first timeout.
	MaxTimeoutRetries int `json:"max_timeout_retries" yaml:"max_timeout_retries"`

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
	Port                      string
	DataDir                   string
	Version                   string
	Config                    string
	AuthToken                 string
	WorkerHeartbeatTimeout    string
	WorkerOfflineThreshold    string
	WorkerHealthCheckInterval string
	JobTimeout                string
	ScheduleInterval          string
	TimeoutCheckInterval      string
	MaxJobsPerWorker          int
	NoWorkerJobTimeout        string
	// Retry budgets use pointers: nil means "flag not set", so 0 stays an
	// expressible value (disable retries/migration) unlike MaxJobsPerWorker,
	// where 0 already means unset.
	MaxTimeoutRetries          *int
	MaxRetryCount              *int
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
		NoWorkerJobTimeout:         2 * time.Minute,  // Fail jobs pending >2m with no schedulable worker
		MaxTimeoutRetries:          2,                // Requeue a timed-out job at most twice before failing
		MaxRetryCount:              3,                // Migrate a job at most 3 times before failing
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

// parseDurationOrLog parses a duration env var / flag value. On failure it
// logs and returns fallback instead of silently ignoring the setting (TSI-2365):
// a typo like "30 mintes" must be visible to the operator.
func parseDurationOrLog(name, value string, fallback time.Duration) time.Duration {
	d, err := time.ParseDuration(value)
	if err != nil {
		log.Printf("Config: invalid duration for %s=%q (%v); keeping default %s", name, value, err, fallback)
		return fallback
	}
	if d < 0 {
		log.Printf("Config: negative duration for %s=%q is not supported; keeping default %s", name, value, fallback)
		return fallback
	}
	return d
}

// parseBoolEnv reads a symmetric boolean env var: accepts 1/true/yes/on and
// 0/false/no/off (case-insensitive). Unrecognized values are logged and the
// current value kept.
func parseBoolEnv(name, value string, current bool) bool {
	switch strings.ToLower(value) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		log.Printf("Config: invalid boolean for %s=%q; keeping current value %v", name, value, current)
		return current
	}
}

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
	// Worker management durations — parse errors are logged, not swallowed.
	if v := os.Getenv("WORKER_HEARTBEAT_TIMEOUT"); v != "" {
		config.WorkerHeartbeatTimeout = parseDurationOrLog("WORKER_HEARTBEAT_TIMEOUT", v, config.WorkerHeartbeatTimeout)
	}
	if v := os.Getenv("WORKER_OFFLINE_THRESHOLD"); v != "" {
		config.WorkerOfflineThreshold = parseDurationOrLog("WORKER_OFFLINE_THRESHOLD", v, config.WorkerOfflineThreshold)
	}
	if v := os.Getenv("WORKER_HEALTH_CHECK_INTERVAL"); v != "" {
		config.WorkerHealthCheckInterval = parseDurationOrLog("WORKER_HEALTH_CHECK_INTERVAL", v, config.WorkerHealthCheckInterval)
	}
	if v := os.Getenv("JOB_TIMEOUT"); v != "" {
		config.JobTimeout = parseDurationOrLog("JOB_TIMEOUT", v, config.JobTimeout)
	}
	if v := os.Getenv("SCHEDULE_INTERVAL"); v != "" {
		config.ScheduleInterval = parseDurationOrLog("SCHEDULE_INTERVAL", v, config.ScheduleInterval)
	}
	if v := os.Getenv("TIMEOUT_CHECK_INTERVAL"); v != "" {
		config.TimeoutCheckInterval = parseDurationOrLog("TIMEOUT_CHECK_INTERVAL", v, config.TimeoutCheckInterval)
	}
	if v := os.Getenv("NO_WORKER_JOB_TIMEOUT"); v != "" {
		config.NoWorkerJobTimeout = parseDurationOrLog("NO_WORKER_JOB_TIMEOUT", v, config.NoWorkerJobTimeout)
	}
	if maxJobs := os.Getenv("MAX_JOBS_PER_WORKER"); maxJobs != "" {
		if n, err := strconv.Atoi(maxJobs); err == nil && n > 0 {
			config.MaxJobsPerWorker = n
		}
	}
	// Retry budgets — 0 is a valid value (disables the respective retry),
	// so these are parsed as non-negative ints, not >0 like the count above.
	if v := os.Getenv("MAX_TIMEOUT_RETRIES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			config.MaxTimeoutRetries = n
		}
	}
	if v := os.Getenv("MAX_RETRY_COUNT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			config.MaxRetryCount = n
		}
	}

	// Rate limit environment variables — symmetric boolean (TSI-2365)
	if v := os.Getenv("RATE_LIMIT_ENABLED"); v != "" {
		config.RateLimitEnabled = parseBoolEnv("RATE_LIMIT_ENABLED", v, config.RateLimitEnabled)
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
	// TLS environment variables — symmetric boolean (TSI-2365)
	if v := os.Getenv("TLS_ENABLED"); v != "" {
		config.TLS.Enabled = parseBoolEnv("TLS_ENABLED", v, config.TLS.Enabled)
	}

	// TLS environment variables (ENABLED handled above as a symmetric boolean)
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

	// Worker management flags — parse errors are logged, not swallowed (TSI-2365)
	if flags.WorkerHeartbeatTimeout != "" {
		c.WorkerHeartbeatTimeout = parseDurationOrLog("worker-heartbeat-timeout", flags.WorkerHeartbeatTimeout, c.WorkerHeartbeatTimeout)
	}
	if flags.WorkerOfflineThreshold != "" {
		c.WorkerOfflineThreshold = parseDurationOrLog("worker-offline-threshold", flags.WorkerOfflineThreshold, c.WorkerOfflineThreshold)
	}
	if flags.WorkerHealthCheckInterval != "" {
		c.WorkerHealthCheckInterval = parseDurationOrLog("worker-health-check-interval", flags.WorkerHealthCheckInterval, c.WorkerHealthCheckInterval)
	}

	// Scheduler flags
	if flags.JobTimeout != "" {
		c.JobTimeout = parseDurationOrLog("job-timeout", flags.JobTimeout, c.JobTimeout)
	}
	if flags.ScheduleInterval != "" {
		c.ScheduleInterval = parseDurationOrLog("schedule-interval", flags.ScheduleInterval, c.ScheduleInterval)
	}
	if flags.TimeoutCheckInterval != "" {
		c.TimeoutCheckInterval = parseDurationOrLog("timeout-check-interval", flags.TimeoutCheckInterval, c.TimeoutCheckInterval)
	}
	if flags.NoWorkerJobTimeout != "" {
		c.NoWorkerJobTimeout = parseDurationOrLog("no-worker-job-timeout", flags.NoWorkerJobTimeout, c.NoWorkerJobTimeout)
	}
	if flags.MaxJobsPerWorker > 0 {
		c.MaxJobsPerWorker = flags.MaxJobsPerWorker
	}

	// Retry budgets — nil means "flag not set", so 0 stays expressible.
	if flags.MaxTimeoutRetries != nil {
		c.MaxTimeoutRetries = *flags.MaxTimeoutRetries
	}
	if flags.MaxRetryCount != nil {
		c.MaxRetryCount = *flags.MaxRetryCount
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

// parseTLSVersion converts a string TLS version to uint16. Unrecognized
// values are logged — silently defaulting to TLS 1.2 hid operator typos.
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
		log.Printf("Config: unknown TLS min version %q; defaulting to TLS 1.2", version)
		return 0x0303
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
	return fmt.Sprintf("ServerConfig{port=%s, dataDir=%s, tls=%s, auth=%s, workerHeartbeatTimeout=%s, workerOfflineThreshold=%s, workerHealthCheckInterval=%s, jobTimeout=%s, scheduleInterval=%s, timeoutCheckInterval=%s, maxJobsPerWorker=%d, maxTimeoutRetries=%d, maxRetryCount=%d, rateLimitEnabled=%v, maxConcurrentJobsPerClient=%d}",
		c.Port, c.DataDir, tlsStatus, authStatus, c.WorkerHeartbeatTimeout, c.WorkerOfflineThreshold, c.WorkerHealthCheckInterval,
		c.JobTimeout, c.ScheduleInterval, c.TimeoutCheckInterval, c.MaxJobsPerWorker, c.MaxTimeoutRetries, c.MaxRetryCount, c.RateLimitEnabled, c.MaxConcurrentJobsPerClient)
}
