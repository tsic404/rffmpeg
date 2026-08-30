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
		return "/var/cache/rffmpeg"
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

// DefaultTempDir returns the default worker temp directory. Non-root users get
// a per-user XDG directory (~/.cache/rffmpeg-worker/<workerID>); when that is
// unavailable, or for root, the fallback is $TMPDIR/rffmpeg-worker-<uid>/<workerID>.
func DefaultTempDir(workerID string) string {
	return defaultTempDirFor(os.Geteuid(), os.Getuid(), os.UserCacheDir, os.TempDir(), workerID)
}

// defaultTempDirFor resolves the default temp directory given process identity
// and environment accessors. Injectable for tests.
func defaultTempDirFor(euid, uid int, userCacheDirFn func() (string, error), tempDir, workerID string) string {
	if euid == 0 {
		return FallbackTempDir(tempDir, uid, workerID)
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

// LoadFromEnv loads configuration from environment variables.
func LoadFromEnv() *Config {
	config := DefaultConfig()

	if url := os.Getenv("RFFMPEG_SERVER_URL"); url != "" {
		config.ServerURL = url
	}
	if id := os.Getenv("RFFMPEG_WORKER_ID"); id != "" {
		config.WorkerID = id
	}
	if name := os.Getenv("RFFMPEG_WORKER_NAME"); name != "" {
		config.Name = name
	}
	if token := os.Getenv("RFFMPEG_TOKEN"); token != "" {
		config.Token = token
	}
	if tempDir := os.Getenv("RFFMPEG_TEMP_DIR"); tempDir != "" {
		config.TempDir = tempDir
	}
	if ffmpeg := os.Getenv("RFFMPEG_FFMPEG_PATH"); ffmpeg != "" {
		config.FFmpegPath = ffmpeg
	}
	if timeout := os.Getenv("RFFMPEG_TIMEOUT"); timeout != "" {
		if d, err := time.ParseDuration(timeout); err == nil {
			config.Timeout = Duration(d)
		}
	}
	if maxConcurrent := os.Getenv("RFFMPEG_MAX_CONCURRENT"); maxConcurrent != "" {
		if n, err := parseInt(maxConcurrent); err == nil && n > 0 {
			config.MaxConcurrent = n
		}
	}
	if v := os.Getenv("RFFMPEG_AUTO_DETECT_GPU"); v != "" {
		if parsed, ok := parseBoolEnv(v); ok {
			config.AutoDetectGPU = parsed
		}
	}
	if v := os.Getenv("RFFMPEG_AUTO_DETECT_CODECS"); v != "" {
		if parsed, ok := parseBoolEnv(v); ok {
			config.AutoDetectCodecs = parsed
		}
	}

	// Cache environment variables
	if cacheEnabled := os.Getenv("RFFMPEG_CACHE_ENABLED"); cacheEnabled == "false" || cacheEnabled == "0" {
		config.CacheEnabled = false
	}
	if cacheDir := os.Getenv("RFFMPEG_CACHE_DIR"); cacheDir != "" {
		config.CacheDir = cacheDir
	}
	if cacheTTL := os.Getenv("RFFMPEG_CACHE_TTL"); cacheTTL != "" {
		if d, err := time.ParseDuration(cacheTTL); err == nil {
			config.CacheTTL = Duration(d)
		}
	}
	if cacheMaxSize := os.Getenv("RFFMPEG_CACHE_MAX_SIZE_MB"); cacheMaxSize != "" {
		if n, err := parseInt(cacheMaxSize); err == nil && n > 0 {
			config.CacheMaxSizeMB = int64(n)
		}
	}

	// Retry environment variables
	if retryMaxRetries := os.Getenv("RFFMPEG_RETRY_MAX_RETRIES"); retryMaxRetries != "" {
		if n, err := parseInt(retryMaxRetries); err == nil && n > 0 {
			config.RetryMaxRetries = n
		}
	}
	if retryInitialInterval := os.Getenv("RFFMPEG_RETRY_INITIAL_INTERVAL"); retryInitialInterval != "" {
		if d, err := time.ParseDuration(retryInitialInterval); err == nil {
			config.RetryInitialInterval = Duration(d)
		}
	}
	if v := os.Getenv("RFFMPEG_RETRY_EXPONENTIAL_BACKOFF"); v == "true" || v == "1" {
		config.RetryUseExponentialBackoff = true
	}
	if retryMaxInterval := os.Getenv("RFFMPEG_RETRY_MAX_INTERVAL"); retryMaxInterval != "" {
		if d, err := time.ParseDuration(retryMaxInterval); err == nil {
			config.RetryMaxInterval = Duration(d)
		}
	}
	if v := os.Getenv("RFFMPEG_RETRY_ENABLE_SOFTWARE_FALLBACK"); v == "false" || v == "0" {
		config.RetryEnableSoftwareFallback = false
	}

	return config
}

// Merge merges file config with environment config (env takes precedence).
func Merge(fileConfig, envConfig *Config) *Config {
	result := &Config{}

	// Start with file config
	if fileConfig != nil {
		*result = *fileConfig
	}

	// Override with env config values if set
	if envConfig != nil {
		if envConfig.ServerURL != "" && envConfig.ServerURL != DefaultConfig().ServerURL {
			result.ServerURL = envConfig.ServerURL
		}
		if envConfig.WorkerID != "" {
			result.WorkerID = envConfig.WorkerID
		}
		if envConfig.Name != "" {
			result.Name = envConfig.Name
		}
		if envConfig.Token != "" {
			result.Token = envConfig.Token
		}
		if envConfig.TempDir != "" {
			result.TempDir = envConfig.TempDir
		}
		if envConfig.FFmpegPath != "" && envConfig.FFmpegPath != DefaultConfig().FFmpegPath {
			result.FFmpegPath = envConfig.FFmpegPath
		}
		if envConfig.Timeout != DefaultConfig().Timeout {
			result.Timeout = envConfig.Timeout
		}
		if envConfig.MaxConcurrent != DefaultConfig().MaxConcurrent {
			result.MaxConcurrent = envConfig.MaxConcurrent
		}
		// Auto-detect booleans only override when the env var is explicitly
		// set to a recognized boolean. Copying envConfig unconditionally would
		// clobber a file's false with LoadFromEnv's default true (TSI-2640);
		// a non-empty but invalid value (e.g. "banana") must not count as an
		// explicit override either (TSI-2662).
		if v := os.Getenv("RFFMPEG_AUTO_DETECT_GPU"); v != "" {
			if parsed, ok := parseBoolEnv(v); ok {
				result.AutoDetectGPU = parsed
			}
		}
		if v := os.Getenv("RFFMPEG_AUTO_DETECT_CODECS"); v != "" {
			if parsed, ok := parseBoolEnv(v); ok {
				result.AutoDetectCodecs = parsed
			}
		}

		// Cache settings
		if envConfig.CacheDir != "" && envConfig.CacheDir != DefaultConfig().CacheDir {
			result.CacheDir = envConfig.CacheDir
		}
		if envConfig.CacheEnabled != result.CacheEnabled {
			// Only override if explicitly changed in env
			if os.Getenv("RFFMPEG_CACHE_ENABLED") != "" {
				result.CacheEnabled = envConfig.CacheEnabled
			}
		}
		if envConfig.CacheTTL != DefaultConfig().CacheTTL {
			result.CacheTTL = envConfig.CacheTTL
		}
		if envConfig.CacheMaxSizeMB != DefaultConfig().CacheMaxSizeMB {
			result.CacheMaxSizeMB = envConfig.CacheMaxSizeMB
		}

		// Retry settings
		if envConfig.RetryMaxRetries != 0 && envConfig.RetryMaxRetries != DefaultConfig().RetryMaxRetries {
			result.RetryMaxRetries = envConfig.RetryMaxRetries
		}
		if envConfig.RetryInitialInterval != 0 && envConfig.RetryInitialInterval != DefaultConfig().RetryInitialInterval {
			result.RetryInitialInterval = envConfig.RetryInitialInterval
		}
		if os.Getenv("RFFMPEG_RETRY_EXPONENTIAL_BACKOFF") != "" {
			result.RetryUseExponentialBackoff = envConfig.RetryUseExponentialBackoff
		}
		if envConfig.RetryMaxInterval != 0 && envConfig.RetryMaxInterval != DefaultConfig().RetryMaxInterval {
			result.RetryMaxInterval = envConfig.RetryMaxInterval
		}
		if os.Getenv("RFFMPEG_RETRY_ENABLE_SOFTWARE_FALLBACK") != "" {
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
