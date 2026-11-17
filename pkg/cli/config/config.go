package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	// DefaultServerURL is the built-in server URL: the local host on port 8080.
	DefaultServerURL = "http://localhost:8080"

	// DefaultPollTimeout is the client-side cap on how long the CLI polls a
	// pending (waiting-for-worker) job for a terminal status before giving up.
	// It bounds the wait-for-worker phase only: a job stuck pending behind
	// busy-but-live workers never receives a server verdict deadline, so
	// without this cap the CLI would poll forever. 0 opts out of the cap.
	DefaultPollTimeout = 10 * time.Minute

	// ConfigFileName is the config file name searched in each file layer.
	ConfigFileName = "rffmpeg.json"

	// CurrentDirConfigPath is the current-directory config layer. It is the
	// only file layer whose contents depend on where the CLI runs, so it is
	// adopted only when its provenance checks out (see checkCurrentDirConfig).
	CurrentDirConfigPath = "./" + ConfigFileName

	// SystemConfigPath is the system config directory layer.
	SystemConfigPath = "/etc/" + ConfigFileName

	// InfoFormatText and InfoFormatJSON select the capability-query output
	// format. Text is the default.
	InfoFormatText = "text"
	InfoFormatJSON = "json"
)

// Environment variable names of the CLI configuration plane. The CLI holds no
// configuration of its own on the command line: every rffmpeg setting is read
// from one of these variables or from the layered config files.
const (
	EnvServerURL         = "RFFMPEG_SERVER_URL"
	EnvToken             = "RFFMPEG_TOKEN"
	EnvSharedFS          = "RFFMPEG_SHARED_FS"
	EnvQuiet             = "RFFMPEG_QUIET"
	EnvAutoHW            = "RFFMPEG_AUTO_HW"
	EnvRetry             = "RFFMPEG_RETRY"
	EnvJobTimeout        = "RFFMPEG_JOB_TIMEOUT"
	EnvPollTimeout       = "RFFMPEG_POLL_TIMEOUT"
	EnvMaxRetries        = "RFFMPEG_MAX_RETRIES"
	EnvServerLossTimeout = "RFFMPEG_SERVER_LOSS_TIMEOUT"
	EnvInfoFormat        = "RFFMPEG_INFO_FORMAT"
	EnvLocalFallback     = "RFFMPEG_LOCAL_FALLBACK"
	EnvLogFile           = "RFFMPEG_LOG_FILE"
	EnvJobID             = "RFFMPEG_JOB_ID"
)

// ServerURLSource identifies the layer that supplied the effective server URL.
// The non-quiet banner prints it next to the URL so operators can tell an
// environment override apart from a config file and from the built-in default.
type ServerURLSource string

const (
	ServerURLSourceDefault ServerURLSource = "built-in default"
	ServerURLSourceSystem  ServerURLSource = "system"
	ServerURLSourceHome    ServerURLSource = "user home"
	ServerURLSourceCWD     ServerURLSource = "current directory"
	ServerURLSourceEnv     ServerURLSource = "env"
)

// Reasons a current-directory config file is skipped, in the order the
// conditions are checked (regular file, then owner, then permissions). The
// reason word is part of the warning line.
const (
	reasonNotRegularFile = "not a regular file"
	reasonOwnerMismatch  = "owner mismatch"
	reasonWritable       = "writable by group or others"
)

// Duration is a time.Duration that unmarshals from either a JSON string
// ("30s", "5m", "2h") or a numeric nanosecond value. It lets rffmpeg.json set
// a budget as a human-readable duration; time.Duration alone cannot parse the
// string form from JSON.
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

// Config is the resolved CLI configuration.
type Config struct {
	ServerURL string `json:"server_url"`
	// ServerURLSource records which layer set ServerURL. The banner prints it.
	ServerURLSource ServerURLSource `json:"-"`

	// Token authenticates requests; it goes into the Authorization header only.
	Token string `json:"token"`

	// SharedFS skips upload/download and hands the worker local paths.
	SharedFS bool `json:"shared_fs"`

	// Quiet suppresses the banner, upload progress and submission lines.
	Quiet bool `json:"quiet"`

	// AutoHW enables the automatic software → hardware encoder upgrade.
	AutoHW bool `json:"auto_hw"`

	// Retry retries rate-limited (HTTP 429) submissions with backoff.
	Retry bool `json:"retry"`

	// JobTimeout is the worker-side ffmpeg execution budget. nil means "not
	// configured": the field is not sent and the server's job timeout applies.
	JobTimeout *Duration `json:"job_timeout,omitempty"`

	// PollTimeout caps the wait-for-worker (pending) phase. nil means "unset"
	// (DefaultPollTimeout); a non-nil 0 means "no cap".
	PollTimeout *Duration `json:"poll_timeout,omitempty"`

	// MaxRetries bounds the WS/HTTP retry loops. nil means "unset"
	// (client.DefaultMaxRetries); a non-nil 0 means "no retries".
	MaxRetries *int `json:"max_retries,omitempty"`

	// ServerLossTimeout caps how long the CLI keeps waiting for an
	// already-submitted job once contact with the server is lost. nil means
	// "unset" (client.DefaultServerLossTimeout); a non-nil 0 disables the cap.
	ServerLossTimeout *Duration `json:"server_loss_timeout,omitempty"`

	// InfoFormat is "text" or "json": the capability-query output format.
	InfoFormat string `json:"info_format"`

	// LocalFallback lets a server-backed capability query answer from the
	// local ffmpeg when the server cannot.
	LocalFallback bool `json:"local_fallback"`

	// LogFile is the diagnostic log path for pre-submit failures; empty
	// disables the file entirely.
	LogFile string `json:"log_file"`
}

// IsSharedFS returns true if shared filesystem mode is enabled.
func (c *Config) IsSharedFS() bool { return c.SharedFS }

// IsJSON reports whether capability queries should answer in JSON.
func (c *Config) IsJSON() bool { return c.InfoFormat == InfoFormatJSON }

// fileConfig is one config file's contribution. Pointer fields distinguish
// "key absent" from an explicit zero value, so a higher layer only overrides
// the keys it actually sets.
type fileConfig struct {
	ServerURL         *string   `json:"server_url"`
	Token             *string   `json:"token"`
	SharedFS          *bool     `json:"shared_fs"`
	Quiet             *bool     `json:"quiet"`
	AutoHW            *bool     `json:"auto_hw"`
	Retry             *bool     `json:"retry"`
	JobTimeout        *Duration `json:"job_timeout"`
	PollTimeout       *Duration `json:"poll_timeout"`
	MaxRetries        *int      `json:"max_retries"`
	ServerLossTimeout *Duration `json:"server_loss_timeout"`
	InfoFormat        *string   `json:"info_format"`
	LocalFallback     *bool     `json:"local_fallback"`
	LogFile           *string   `json:"log_file"`
}

// Load resolves the CLI configuration from the configuration plane in the
// documented layer order: environment variables, the current-directory config
// file, the user home config file, the system config directory, and finally
// the built-in defaults. A higher layer overrides only the keys it sets.
//
// The current-directory layer is adopted only when the file is a regular file
// owned by the effective user and not writable by group or others; a skipped
// layer emits a warning line on stderr, which quiet mode does not suppress.
func Load() (*Config, error) {
	return load(Paths{
		CurrentDir: CurrentDirConfigPath,
		Home:       homeConfigPath(),
		System:     SystemConfigPath,
	})
}

// Paths names the three config file layers. It exists so tests can redirect
// the layers without touching the real home and system files.
type Paths struct {
	CurrentDir string
	Home       string
	System     string
}

// load resolves the layers named by paths.
func load(paths Paths) (*Config, error) {
	cfg := &Config{
		ServerURL:       DefaultServerURL,
		ServerURLSource: ServerURLSourceDefault,
		InfoFormat:      InfoFormatText,
	}

	// Lowest priority first, so a higher layer's keys win.
	if err := applyFileLayer(cfg, paths.System, ServerURLSourceSystem, nil); err != nil {
		return nil, err
	}
	if err := applyFileLayer(cfg, paths.Home, ServerURLSourceHome, nil); err != nil {
		return nil, err
	}
	if err := applyFileLayer(cfg, paths.CurrentDir, ServerURLSourceCWD, checkCurrentDirConfig); err != nil {
		return nil, err
	}
	if err := applyEnv(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// homeConfigPath is the user-home config layer.
func homeConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		home = os.Getenv("HOME")
	}
	if home == "" {
		return ""
	}
	return filepath.Join(home, "."+ConfigFileName)
}

// applyFileLayer merges one config file over cfg. A missing file is an absent
// layer. check, when set, decides whether the layer may be adopted at all; its
// skip reason is reported as a warning that quiet mode never suppresses.
func applyFileLayer(cfg *Config, path string, source ServerURLSource, check func(string) (bool, string)) error {
	if path == "" {
		return nil
	}
	if check != nil {
		ok, reason := check(path)
		if !ok {
			if reason != "" {
				fmt.Fprintf(os.Stderr, "Warning: ignoring config file %s: %s\n", path, reason)
			}
			return nil
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("%s: %w", path, err)
	}
	var layer fileConfig
	if err := json.Unmarshal(data, &layer); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return layer.applyTo(cfg, source, path)
}

// applyTo merges the keys this file sets into cfg, rejecting invalid values
// before anything is submitted.
func (l *fileConfig) applyTo(cfg *Config, source ServerURLSource, path string) error {
	if l.ServerURL != nil {
		cfg.ServerURL = *l.ServerURL
		cfg.ServerURLSource = source
	}
	if l.Token != nil {
		cfg.Token = *l.Token
	}
	if l.SharedFS != nil {
		cfg.SharedFS = *l.SharedFS
	}
	if l.Quiet != nil {
		cfg.Quiet = *l.Quiet
	}
	if l.AutoHW != nil {
		cfg.AutoHW = *l.AutoHW
	}
	if l.Retry != nil {
		cfg.Retry = *l.Retry
	}
	if l.JobTimeout != nil {
		if *l.JobTimeout <= 0 {
			return fmt.Errorf("%s: job_timeout must be greater than 0", path)
		}
		cfg.JobTimeout = l.JobTimeout
	}
	if l.PollTimeout != nil {
		if *l.PollTimeout < 0 {
			return fmt.Errorf("%s: poll_timeout must not be negative", path)
		}
		cfg.PollTimeout = l.PollTimeout
	}
	if l.MaxRetries != nil {
		if *l.MaxRetries < 0 {
			return fmt.Errorf("%s: max_retries must not be negative", path)
		}
		cfg.MaxRetries = l.MaxRetries
	}
	if l.ServerLossTimeout != nil {
		if *l.ServerLossTimeout < 0 {
			return fmt.Errorf("%s: server_loss_timeout must not be negative", path)
		}
		cfg.ServerLossTimeout = l.ServerLossTimeout
	}
	if l.InfoFormat != nil {
		format, err := normalizeInfoFormat(*l.InfoFormat)
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		cfg.InfoFormat = format
	}
	if l.LocalFallback != nil {
		cfg.LocalFallback = *l.LocalFallback
	}
	if l.LogFile != nil {
		cfg.LogFile = *l.LogFile
	}
	return nil
}

// applyEnv applies the environment layer, the highest-priority one.
func applyEnv(cfg *Config) error {
	if v := os.Getenv(EnvServerURL); v != "" {
		cfg.ServerURL = v
		cfg.ServerURLSource = ServerURLSourceEnv
	}
	if v := os.Getenv(EnvToken); v != "" {
		cfg.Token = v
	}
	var err error
	if cfg.SharedFS, err = envBool(EnvSharedFS, cfg.SharedFS); err != nil {
		return err
	}
	if cfg.Quiet, err = envBool(EnvQuiet, cfg.Quiet); err != nil {
		return err
	}
	if cfg.AutoHW, err = envBool(EnvAutoHW, cfg.AutoHW); err != nil {
		return err
	}
	if cfg.Retry, err = envBool(EnvRetry, cfg.Retry); err != nil {
		return err
	}
	if cfg.LocalFallback, err = envBool(EnvLocalFallback, cfg.LocalFallback); err != nil {
		return err
	}
	if v := os.Getenv(EnvJobTimeout); v != "" {
		d, perr := time.ParseDuration(v)
		if perr != nil || d <= 0 {
			return fmt.Errorf("%s=%q must be a positive duration (e.g. 30m)", EnvJobTimeout, v)
		}
		jobTimeout := Duration(d)
		cfg.JobTimeout = &jobTimeout
	}
	if v := os.Getenv(EnvPollTimeout); v != "" {
		d, perr := time.ParseDuration(v)
		if perr != nil || d < 0 {
			return fmt.Errorf("%s=%q must be a non-negative duration (0 disables the cap)", EnvPollTimeout, v)
		}
		pollTimeout := Duration(d)
		cfg.PollTimeout = &pollTimeout
	}
	if v := os.Getenv(EnvMaxRetries); v != "" {
		n, perr := strconv.Atoi(v)
		if perr != nil || n < 0 {
			return fmt.Errorf("%s=%q must be a non-negative integer (0 = no retries)", EnvMaxRetries, v)
		}
		cfg.MaxRetries = &n
	}
	if v := os.Getenv(EnvServerLossTimeout); v != "" {
		d, perr := time.ParseDuration(v)
		if perr != nil || d < 0 {
			return fmt.Errorf("%s=%q must be a non-negative duration (0 disables the cap)", EnvServerLossTimeout, v)
		}
		serverLossTimeout := Duration(d)
		cfg.ServerLossTimeout = &serverLossTimeout
	}
	if v := os.Getenv(EnvInfoFormat); v != "" {
		format, perr := normalizeInfoFormat(v)
		if perr != nil {
			return fmt.Errorf("%s: %w", EnvInfoFormat, perr)
		}
		cfg.InfoFormat = format
	}
	if v := os.Getenv(EnvLogFile); v != "" {
		cfg.LogFile = v
	}
	return nil
}

// envBool reads a boolean environment variable, keeping the current value when
// the variable is unset.
func envBool(name string, current bool) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "":
		return current, nil
	case "1", "true":
		return true, nil
	case "0", "false":
		return false, nil
	default:
		return current, fmt.Errorf("%s=%q must be one of 1, true, 0, false", name, os.Getenv(name))
	}
}

// normalizeInfoFormat validates the capability-query output format.
func normalizeInfoFormat(v string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case InfoFormatText, "":
		return InfoFormatText, nil
	case InfoFormatJSON:
		return InfoFormatJSON, nil
	default:
		return "", fmt.Errorf("info_format=%q must be one of %s, %s", v, InfoFormatText, InfoFormatJSON)
	}
}

// currentEUID is a seam for the owner check: tests cannot create a file owned
// by another user without root, so they swap the running user instead.
var currentEUID = effectiveUID

// checkCurrentDirConfig decides whether the current-directory layer may be
// adopted, judging a symlink by its resolved target. The conditions are
// evaluated in order — regular file, owner, permissions — and the first
// failure is the reported reason. A missing file is an absent layer, not a
// skipped one, and emits no warning.
func checkCurrentDirConfig(path string) (bool, string) {
	if _, err := os.Lstat(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, ""
		}
		return false, reasonNotRegularFile
	}
	fi, err := os.Stat(path)
	if err != nil || !fi.Mode().IsRegular() {
		return false, reasonNotRegularFile
	}
	if uid, ok := fileOwnerUID(fi); ok && uid != currentEUID() {
		return false, reasonOwnerMismatch
	}
	if fi.Mode().Perm()&0o022 != 0 {
		return false, reasonWritable
	}
	return true, ""
}

// Save writes the configuration to path as JSON.
func (c *Config) Save(path string) error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}
