// Package workerconfig provides configuration management for the worker.
package workerconfig

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Environment variable names. LoadFromEnv records which of these were
// explicitly set (see Config.setKeys); Merge reads the same names so the two
// halves of the env-override contract cannot drift apart.
const (
	envServerURL               = "RFFMPEG_SERVER_URL"
	envWorkerID                = "RFFMPEG_WORKER_ID"
	envWorkerName              = "RFFMPEG_WORKER_NAME"
	envToken                   = "RFFMPEG_TOKEN"
	envTempDir                 = "RFFMPEG_TEMP_DIR"
	envFFmpegPath              = "RFFMPEG_FFMPEG_PATH"
	envTimeout                 = "RFFMPEG_TIMEOUT"
	envHeartbeatInterval       = "RFFMPEG_HEARTBEAT_INTERVAL"
	envPollInterval            = "RFFMPEG_POLL_INTERVAL"
	envMaxConcurrent           = "RFFMPEG_MAX_CONCURRENT"
	envAutoDetectGPU           = "RFFMPEG_AUTO_DETECT_GPU"
	envAutoDetectCodecs        = "RFFMPEG_AUTO_DETECT_CODECS"
	envSharedFSAllowedPrefix   = "RFFMPEG_SHARED_FS_ALLOWED_PREFIX"
	envCacheEnabled            = "RFFMPEG_CACHE_ENABLED"
	envCacheDir                = "RFFMPEG_CACHE_DIR"
	envCacheTTL                = "RFFMPEG_CACHE_TTL"
	envCacheMaxSizeMB          = "RFFMPEG_CACHE_MAX_SIZE_MB"
	envRetryMaxRetries         = "RFFMPEG_RETRY_MAX_RETRIES"
	envRetryInitialInterval    = "RFFMPEG_RETRY_INITIAL_INTERVAL"
	envRetryExponentialBackoff = "RFFMPEG_RETRY_EXPONENTIAL_BACKOFF"
	envRetryMaxInterval        = "RFFMPEG_RETRY_MAX_INTERVAL"
	envRetrySoftwareFallback   = "RFFMPEG_RETRY_ENABLE_SOFTWARE_FALLBACK"
)

// Duration is a custom type that can parse duration strings from JSON
type Duration time.Duration

// UnmarshalJSON implements json.Unmarshaler for Duration
func (d *Duration) UnmarshalJSON(b []byte) error {
	var v interface{}
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}

	switch value := v.(type) {
	case float64:
		*d = Duration(time.Duration(value))
		return nil
	case string:
		tmp, err := time.ParseDuration(value)
		if err != nil {
			return err
		}
		*d = Duration(tmp)
		return nil
	default:
		return fmt.Errorf("invalid duration type: %T", v)
	}
}

// MarshalJSON implements json.Marshaler for Duration
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// ToDuration converts Duration to time.Duration
func (d Duration) ToDuration() time.Duration {
	return time.Duration(d)
}

// Config holds worker configuration loaded from file or environment.
type Config struct {
	// Connection settings
	ServerURL string `json:"server_url" yaml:"server_url"`
	WorkerID  string `json:"worker_id" yaml:"worker_id"`
	Name      string `json:"name" yaml:"name"`
	Token     string `json:"token" yaml:"token"`

	// Paths
	TempDir    string `json:"temp_dir" yaml:"temp_dir"`
	FFmpegPath string `json:"ffmpeg_path" yaml:"ffmpeg_path"`

	// Timing
	Timeout           Duration `json:"timeout" yaml:"timeout"`
	HeartbeatInterval Duration `json:"heartbeat_interval" yaml:"heartbeat_interval"`
	PollInterval      Duration `json:"poll_interval" yaml:"poll_interval"`

	// Job handling
	MaxConcurrent int `json:"max_concurrent" yaml:"max_concurrent"`

	// Hardware detection
	AutoDetectGPU    bool `json:"auto_detect_gpu" yaml:"auto_detect_gpu"`
	AutoDetectCodecs bool `json:"auto_detect_codecs" yaml:"auto_detect_codecs"`

	// Encoder configuration
	EncoderPriority  []string `json:"encoder_priority" yaml:"encoder_priority"`
	EncoderBlacklist []string `json:"encoder_blacklist" yaml:"encoder_blacklist"`

	// Manual overrides (if auto-detection is disabled)
	ManualEncoders      []string `json:"manual_encoders" yaml:"manual_encoders"`
	ManualDecoders      []string `json:"manual_decoders" yaml:"manual_decoders"`
	ManualGPUModel      string   `json:"manual_gpu_model" yaml:"manual_gpu_model"`
	ManualFFmpegVersion string   `json:"manual_ffmpeg_version" yaml:"manual_ffmpeg_version"`

	// Cache settings
	CacheEnabled   bool     `json:"cache_enabled" yaml:"cache_enabled"`
	CacheDir       string   `json:"cache_dir" yaml:"cache_dir"`
	CacheTTL       Duration `json:"cache_ttl" yaml:"cache_ttl"`
	CacheMaxSizeMB int64    `json:"cache_max_size_mb" yaml:"cache_max_size_mb"` // Max cache size in MiB; 0 = use default (10 GiB)

	// Retry settings
	RetryMaxRetries             int      `json:"retry_max_retries" yaml:"retry_max_retries"`
	RetryInitialInterval        Duration `json:"retry_initial_interval" yaml:"retry_initial_interval"`
	RetryUseExponentialBackoff  bool     `json:"retry_use_exponential_backoff" yaml:"retry_use_exponential_backoff"`
	RetryMaxInterval            Duration `json:"retry_max_interval" yaml:"retry_max_interval"`
	RetryEnableSoftwareFallback bool     `json:"retry_enable_software_fallback" yaml:"retry_enable_software_fallback"`
	// Shared filesystem mode
	SharedFSAllowedPrefix string `json:"shared_fs_allowed_prefix" yaml:"shared_fs_allowed_prefix"` // Comma-separated path prefixes allowed in pass-through mode; empty = unlimited

	// setKeys records which environment variables LoadFromEnv observed as an
	// explicit override. Merge consults only this map — never os.Getenv — so
	// it stays a pure function of its arguments. Unexported: file configs and
	// hand-built configs simply have no overrides.
	setKeys map[string]bool
}

// FallbackCacheDir returns the per-user fallback cache directory under tempDir.
// The uid suffix prevents collisions between users on a shared host.
func FallbackCacheDir(tempDir string, uid int) string {
	return filepath.Join(tempDir, "rffmpeg-"+strconv.Itoa(uid))
}

// IsFallbackCacheDir reports whether dir is the per-user fallback cache
// directory for the current process, which must be created private (0700).
func IsFallbackCacheDir(dir string) bool {
	return dir == FallbackCacheDir(os.TempDir(), os.Getuid())
}

// FHS primary directories for root (euid == 0) deployments. CacheDir uses
// RootCacheDir; TempDir uses RootTempDirBase as the shared base under which
// each worker gets its own <workerID> subdirectory.
const (
	RootCacheDir    = "/var/cache/rffmpeg"
	RootTempDirBase = "/var/tmp/rffmpeg-worker"
)

// DefaultCacheDir returns the default cache directory for the worker.
// Root deployments (e.g. systemd) keep the FHS path /var/cache/rffmpeg;
// non-root users get an XDG-compliant per-user directory (~/.cache/rffmpeg)
func DefaultCacheDir() string {
	return defaultCacheDirFor(os.Geteuid(), os.Getuid(), os.UserCacheDir, os.Getenv("XDG_CACHE_HOME"), os.Getenv("HOME"), os.TempDir())
}

// defaultCacheDirFor resolves the default cache directory given process
// identity and environment accessors. Injectable for tests.
//
// A cache dir derived from $HOME is only trusted when HOME is owned by uid;
// $HOME pointing at another user's directory (e.g. /tmp or /) would otherwise
// still yield an OK cache dir, narrowing the 0700 fallback protection to the
// "no HOME at all" case. An explicit $XDG_CACHE_HOME override is trusted
// as-is, since it does not derive from HOME.
func defaultCacheDirFor(euid, uid int, userCacheDirFn func() (string, error), xdgCacheHome, homeDir, tempDir string) string {
	if euid == 0 {
		return RootCacheDir
	}
	dir, err := userCacheDirFn()
	if err != nil {
		return FallbackCacheDir(tempDir, uid)
	}
	if xdgCacheHome == "" && !homeOwnedBy(homeDir, uid) {
		return FallbackCacheDir(tempDir, uid)
	}
	return filepath.Join(dir, "rffmpeg")
}

// homeOwnedBy reports whether homeDir is owned by uid. A missing or
// foreign-owned homeDir is not owned.
func homeOwnedBy(homeDir string, uid int) bool {
	info, err := os.Stat(homeDir)
	if err != nil {
		return false
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	return st.Uid == uint32(uid)
}

// FallbackTempDir returns the per-user fallback worker temp directory under
// tempDir. The uid suffix prevents collisions between users on a shared host.
func FallbackTempDir(tempDir string, uid int, workerID string) string {
	return filepath.Join(tempDir, "rffmpeg-worker-"+strconv.Itoa(uid), workerID)
}

// DefaultTempDir returns the default worker temp directory. Root deployments
// (e.g. systemd) keep the FHS path /var/tmp/rffmpeg-worker/<workerID>; non-root
// users get a per-user XDG directory (~/.cache/rffmpeg-worker/<workerID>), or
// the fallback $TMPDIR/rffmpeg-worker-<uid>/<workerID> when XDG is unavailable.
func DefaultTempDir(workerID string) string {
	return defaultTempDirFor(os.Geteuid(), os.Getuid(), os.UserCacheDir, os.TempDir(), workerID)
}

// defaultTempDirFor resolves the default temp directory given process identity
// and environment accessors. Injectable for tests.
func defaultTempDirFor(euid, uid int, userCacheDirFn func() (string, error), tempDir, workerID string) string {
	if euid == 0 {
		return filepath.Join(RootTempDirBase, workerID)
	}
	if dir, err := userCacheDirFn(); err == nil {
		return filepath.Join(dir, "rffmpeg-worker", workerID)
	}
	return FallbackTempDir(tempDir, uid, workerID)
}

// DefaultConfig returns a Config with sensible defaults.
func DefaultConfig() *Config {
	return &Config{
		ServerURL:         "http://localhost:8080",
		FFmpegPath:        "ffmpeg",
		Timeout:           Duration(2 * time.Hour),
		HeartbeatInterval: Duration(30 * time.Second),
		PollInterval:      Duration(5 * time.Second),
		MaxConcurrent:     1,
		AutoDetectGPU:     true,
		AutoDetectCodecs:  true,
		CacheEnabled:      true,
		CacheDir:          DefaultCacheDir(),
		CacheTTL:          Duration(24 * time.Hour),
		CacheMaxSizeMB:    10240, // 10 GiB

		// Retry defaults
		RetryMaxRetries:             3,
		RetryInitialInterval:        Duration(1 * time.Second),
		RetryUseExponentialBackoff:  false,
		RetryMaxInterval:            Duration(30 * time.Second),
		RetryEnableSoftwareFallback: true,
	}
}

// Validate checks configuration invariants: the timing knobs must be positive
// (a zero or negative interval would break the worker loop tickers and a
// zero or negative timeout would cancel every job immediately), and disabling
// auto-detection requires explicit manual overrides (otherwise the worker
// would silently report the auto-detected capabilities it was told to skip).
func (c *Config) Validate() error {
	if c.HeartbeatInterval <= 0 {
		return fmt.Errorf("heartbeat_interval must be positive")
	}
	if c.Timeout <= 0 {
		return fmt.Errorf("timeout must be positive")
	}
	if c.PollInterval <= 0 {
		return fmt.Errorf("poll_interval must be positive")
	}
	if !c.AutoDetectCodecs && len(c.ManualEncoders) == 0 {
		return fmt.Errorf("manual_encoders must be non-empty when auto_detect_codecs is disabled")
	}
	if !c.AutoDetectCodecs && len(c.ManualDecoders) == 0 {
		return fmt.Errorf("manual_decoders must be non-empty when auto_detect_codecs is disabled")
	}
	if !c.AutoDetectGPU && c.ManualGPUModel == "" {
		return fmt.Errorf("manual_gpu_model must be non-empty when auto_detect_gpu is disabled")
	}
	return nil
}

// LoadFromFile loads configuration from a JSON file.
func LoadFromFile(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	config := DefaultConfig()
	if err := json.Unmarshal(data, config); err != nil {
		return nil, fmt.Errorf("failed to parse config file: %w", err)
	}

	return config, nil
}

// parseBoolEnv parses a boolean environment variable. It accepts 1/true/yes/on
// and 0/false/no/off (case-insensitive). The second return value reports
// whether the value was a recognized boolean.
func parseBoolEnv(value string) (bool, bool) {
	switch strings.ToLower(value) {
	case "1", "true", "yes", "on":
		return true, true
	case "0", "false", "no", "off":
		return false, true
	default:
		return false, false
	}
}

// LoadFromEnv loads configuration from environment variables. Config.setKeys
// records which variables were set to an applicable value; Merge consults only
// that map, so it never re-reads the process environment.
func LoadFromEnv() *Config {
	config := DefaultConfig()
	config.setKeys = make(map[string]bool)

	if url := os.Getenv(envServerURL); url != "" {
		config.ServerURL = url
		config.setKeys[envServerURL] = true
	}
	if id := os.Getenv(envWorkerID); id != "" {
		config.WorkerID = id
		config.setKeys[envWorkerID] = true
	}
	if name := os.Getenv(envWorkerName); name != "" {
		config.Name = name
		config.setKeys[envWorkerName] = true
	}
	if token := os.Getenv(envToken); token != "" {
		config.Token = token
		config.setKeys[envToken] = true
	}
	if tempDir := os.Getenv(envTempDir); tempDir != "" {
		config.TempDir = tempDir
		config.setKeys[envTempDir] = true
	}
	if ffmpeg := os.Getenv(envFFmpegPath); ffmpeg != "" {
		config.FFmpegPath = ffmpeg
		config.setKeys[envFFmpegPath] = true
	}
	if timeout := os.Getenv(envTimeout); timeout != "" {
		if d, err := time.ParseDuration(timeout); err == nil {
			config.Timeout = Duration(d)
			config.setKeys[envTimeout] = true
		}
	}
	if heartbeatInterval := os.Getenv(envHeartbeatInterval); heartbeatInterval != "" {
		if d, err := time.ParseDuration(heartbeatInterval); err == nil {
			config.HeartbeatInterval = Duration(d)
			config.setKeys[envHeartbeatInterval] = true
		}
	}
	if pollInterval := os.Getenv(envPollInterval); pollInterval != "" {
		if d, err := time.ParseDuration(pollInterval); err == nil {
			config.PollInterval = Duration(d)
			config.setKeys[envPollInterval] = true
		}
	}
	if maxConcurrent := os.Getenv(envMaxConcurrent); maxConcurrent != "" {
		if n, err := parseInt(maxConcurrent); err == nil && n > 0 {
			config.MaxConcurrent = n
			config.setKeys[envMaxConcurrent] = true
		}
	}
	if v := os.Getenv(envAutoDetectGPU); v != "" {
		if parsed, ok := parseBoolEnv(v); ok {
			config.AutoDetectGPU = parsed
			config.setKeys[envAutoDetectGPU] = true
		}
	}
	if v := os.Getenv(envAutoDetectCodecs); v != "" {
		if parsed, ok := parseBoolEnv(v); ok {
			config.AutoDetectCodecs = parsed
			config.setKeys[envAutoDetectCodecs] = true
		}
	}

	// Cache environment variables
	if v := os.Getenv(envCacheEnabled); v != "" {
		// Only a recognized boolean counts as an explicit set (TSI-2731):
		// "false"/"0"/"no"/"off" disable, "true"/"1"/"yes"/"on" keep the
		// default true, and any other value leaves the default untouched.
		if parsed, ok := parseBoolEnv(v); ok {
			config.CacheEnabled = parsed
			config.setKeys[envCacheEnabled] = true
		}
	}
	if cacheDir := os.Getenv(envCacheDir); cacheDir != "" {
		config.CacheDir = cacheDir
		config.setKeys[envCacheDir] = true
	}
	if cacheTTL := os.Getenv(envCacheTTL); cacheTTL != "" {
		if d, err := time.ParseDuration(cacheTTL); err == nil {
			config.CacheTTL = Duration(d)
			config.setKeys[envCacheTTL] = true
		}
	}
	if cacheMaxSize := os.Getenv(envCacheMaxSizeMB); cacheMaxSize != "" {
		if n, err := parseInt(cacheMaxSize); err == nil && n > 0 {
			config.CacheMaxSizeMB = int64(n)
			config.setKeys[envCacheMaxSizeMB] = true
		}
	}

	// Shared filesystem mode
	if prefix := os.Getenv(envSharedFSAllowedPrefix); prefix != "" {
		config.SharedFSAllowedPrefix = prefix
		config.setKeys[envSharedFSAllowedPrefix] = true
	}

	// Retry environment variables
	if retryMaxRetries := os.Getenv(envRetryMaxRetries); retryMaxRetries != "" {
		if n, err := parseInt(retryMaxRetries); err == nil && n > 0 {
			config.RetryMaxRetries = n
			config.setKeys[envRetryMaxRetries] = true
		}
	}
	if retryInitialInterval := os.Getenv(envRetryInitialInterval); retryInitialInterval != "" {
		if d, err := time.ParseDuration(retryInitialInterval); err == nil {
			config.RetryInitialInterval = Duration(d)
			config.setKeys[envRetryInitialInterval] = true
		}
	}
	if v := os.Getenv(envRetryExponentialBackoff); v != "" {
		if parsed, ok := parseBoolEnv(v); ok {
			config.RetryUseExponentialBackoff = parsed
			config.setKeys[envRetryExponentialBackoff] = true
		}
	}
	if retryMaxInterval := os.Getenv(envRetryMaxInterval); retryMaxInterval != "" {
		if d, err := time.ParseDuration(retryMaxInterval); err == nil {
			config.RetryMaxInterval = Duration(d)
			config.setKeys[envRetryMaxInterval] = true
		}
	}
	if v := os.Getenv(envRetrySoftwareFallback); v != "" {
		if parsed, ok := parseBoolEnv(v); ok {
			config.RetryEnableSoftwareFallback = parsed
			config.setKeys[envRetrySoftwareFallback] = true
		}
	}

	return config
}

// Merge merges file config with environment config (env takes precedence).
// A field is copied from envConfig only when envConfig.setKeys marks its
// environment variable as explicitly set. Merge never reads the process
// environment, so it is a pure function of its arguments and directly
// testable with hand-built Config values.
func Merge(fileConfig, envConfig *Config) *Config {
	result := &Config{}

	// Start with file config
	if fileConfig != nil {
		*result = *fileConfig
		// Env overrides are tracked only on envConfig; the merged result never
		// carries its own override map.
		result.setKeys = nil
	}

	// Override with env config values if explicitly set
	if envConfig != nil {
		if envConfig.setKeys[envServerURL] {
			result.ServerURL = envConfig.ServerURL
		}
		if envConfig.setKeys[envWorkerID] {
			result.WorkerID = envConfig.WorkerID
		}
		if envConfig.setKeys[envWorkerName] {
			result.Name = envConfig.Name
		}
		if envConfig.setKeys[envToken] {
			result.Token = envConfig.Token
		}
		if envConfig.setKeys[envTempDir] {
			result.TempDir = envConfig.TempDir
		}
		if envConfig.setKeys[envSharedFSAllowedPrefix] {
			result.SharedFSAllowedPrefix = envConfig.SharedFSAllowedPrefix
		}
		if envConfig.setKeys[envFFmpegPath] {
			result.FFmpegPath = envConfig.FFmpegPath
		}
		if envConfig.setKeys[envTimeout] {
			result.Timeout = envConfig.Timeout
		}
		if envConfig.setKeys[envHeartbeatInterval] {
			result.HeartbeatInterval = envConfig.HeartbeatInterval
		}
		if envConfig.setKeys[envPollInterval] {
			result.PollInterval = envConfig.PollInterval
		}
		if envConfig.setKeys[envMaxConcurrent] {
			result.MaxConcurrent = envConfig.MaxConcurrent
		}
		if envConfig.setKeys[envAutoDetectGPU] {
			result.AutoDetectGPU = envConfig.AutoDetectGPU
		}
		if envConfig.setKeys[envAutoDetectCodecs] {
			result.AutoDetectCodecs = envConfig.AutoDetectCodecs
		}

		// Cache settings
		if envConfig.setKeys[envCacheEnabled] {
			result.CacheEnabled = envConfig.CacheEnabled
		}
		if envConfig.setKeys[envCacheDir] {
			result.CacheDir = envConfig.CacheDir
		}
		if envConfig.setKeys[envCacheTTL] {
			result.CacheTTL = envConfig.CacheTTL
		}
		if envConfig.setKeys[envCacheMaxSizeMB] {
			result.CacheMaxSizeMB = envConfig.CacheMaxSizeMB
		}

		// Retry settings
		if envConfig.setKeys[envRetryMaxRetries] {
			result.RetryMaxRetries = envConfig.RetryMaxRetries
		}
		if envConfig.setKeys[envRetryInitialInterval] {
			result.RetryInitialInterval = envConfig.RetryInitialInterval
		}
		if envConfig.setKeys[envRetryExponentialBackoff] {
			result.RetryUseExponentialBackoff = envConfig.RetryUseExponentialBackoff
		}
		if envConfig.setKeys[envRetryMaxInterval] {
			result.RetryMaxInterval = envConfig.RetryMaxInterval
		}
		if envConfig.setKeys[envRetrySoftwareFallback] {
			result.RetryEnableSoftwareFallback = envConfig.RetryEnableSoftwareFallback
		}
	}

	return result
}

// ResolvePaths resolves relative paths to absolute paths based on a base directory.
func (c *Config) ResolvePaths(baseDir string) error {
	var err error

	if c.TempDir != "" {
		c.TempDir, err = resolvePath(c.TempDir, baseDir)
		if err != nil {
			return err
		}
	}

	if c.FFmpegPath != "" {
		c.FFmpegPath, err = resolvePath(c.FFmpegPath, baseDir)
		if err != nil {
			return err
		}
	}

	return nil
}

// resolvePath resolves a path relative to baseDir if it's not absolute.
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

// parseInt is a helper to parse integers from strings.
func parseInt(s string) (int, error) {
	var n int
	_, err := fmt.Sscanf(s, "%d", &n)
	return n, err
}
