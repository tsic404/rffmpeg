package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/tsic404/rffmpeg/pkg/protocol"
	"github.com/tsic404/rffmpeg/pkg/worker"
	"github.com/tsic404/rffmpeg/pkg/worker/capabilities"
	"github.com/tsic404/rffmpeg/pkg/worker/workerconfig"
)

const (
	defaultConfigPath = ""
)

func main() {
	configPath := flag.String("config", getEnv("RFFMPEG_CONFIG", defaultConfigPath), "Path to worker config file (JSON)")
	token, inputAuthHeader, serverURL := registerConnectionFlags(flag.CommandLine)
	strictConfig := flag.Bool("strict-config", false, "Config file is authoritative: ignore RFFMPEG_* environment overrides")
	cacheEnabled, cacheTTL, cacheMaxSizeMB := registerCacheFlags(flag.CommandLine)
	flag.Parse()

	cfg, load, err := resolveConfig(*configPath, *strictConfig)
	if err != nil {
		log.Fatalf("Worker configuration error: %v", err)
	}

	// CLI flags take highest precedence
	appliedFlags := applyConnectionFlagOverrides(token, inputAuthHeader, serverURL, cfg)
	for name := range applyCacheFlagOverrides(flag.CommandLine, cacheEnabled, cacheTTL, cacheMaxSizeMB, cfg) {
		appliedFlags[name] = true
	}
	// The precedence notice is rendered last: only now is it known which sources
	// the flag overrides left standing.
	if notice := load.notice(appliedFlags); notice != "" {
		log.Print(notice)
	}

	// Validate configuration before building the worker.
	if err := cfg.Validate(); err != nil {
		log.Fatalf("Invalid configuration: %v", err)
	}

	// Create worker
	workerCfg := worker.Config{
		SharedFSAllowedPrefix: cfg.SharedFSAllowedPrefix,
		ServerURL:             cfg.ServerURL,
		WorkerID:              cfg.WorkerID,
		Name:                  cfg.Name,
		Token:                 cfg.Token,
		InputAuthHeader:       cfg.InputAuthHeader,
		TempDir:               cfg.TempDir,
		FFmpegPath:            cfg.FFmpegPath,
		Timeout:               cfg.Timeout.ToDuration(),
		IdleTimeout:           cfg.IdleTimeout.ToDuration(),
		HeartbeatInterval:     cfg.HeartbeatInterval.ToDuration(),
		PollInterval:          cfg.PollInterval.ToDuration(),
		CacheConfig: worker.CacheConfig{
			Enabled:          cfg.CacheEnabled,
			Dir:              cfg.CacheDir,
			TTL:              cfg.CacheTTL.ToDuration(),
			MaxSizeBytes:     cfg.CacheMaxSizeMB * 1024 * 1024,
			TTLScanInterval:  10 * time.Minute,
			LRUCheckInterval: 5 * time.Minute,
			URLTTL:           1 * time.Hour,
		},
		RetryConfig: &worker.RetryConfig{
			MaxRetries:             cfg.RetryMaxRetries,
			InitialInterval:        cfg.RetryInitialInterval.ToDuration(),
			UseExponentialBackoff:  cfg.RetryUseExponentialBackoff,
			MaxInterval:            cfg.RetryMaxInterval.ToDuration(),
			EnableSoftwareFallback: cfg.RetryEnableSoftwareFallback,
		},
	}

	w, err := worker.New(workerCfg)
	if err != nil {
		log.Fatalf("Failed to create worker: %v", err)
	}

	// Detect capabilities
	ctx := context.Background()
	caps, err := detectCapabilities(ctx, cfg)
	if err != nil {
		log.Printf("Warning: Capability detection failed: %v", err)
		// Continue with empty capabilities
		caps = getDefaultCapabilities(cfg)
	}

	logCapabilities(caps)

	// Register with server
	if err := w.Register(*caps); err != nil {
		log.Fatalf("Failed to register worker: %v", err)
	}

	// Setup signal handling
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		sig := <-sigCh
		log.Printf("Received signal: %v", sig)
		cancel()
	}()

	// Start worker
	log.Printf("Worker %s started, polling %s", w.ID(), cfg.ServerURL)
	w.Start(ctx)

	log.Println("Worker stopped")
}

// envFlagNames maps the environment overrides that have a same-name CLI flag to
// that flag. Flags outrank the environment, so a startup notice must not blame
// a variable whose field a flag actually replaced.
var envFlagNames = map[string]string{
	"RFFMPEG_SERVER_URL":        "server-url",
	"RFFMPEG_TOKEN":             "token",
	"RFFMPEG_INPUT_AUTH_HEADER": "input-auth-header",
	"RFFMPEG_CACHE_ENABLED":     "cache-enabled",
	"RFFMPEG_CACHE_TTL":         "cache-ttl",
	"RFFMPEG_CACHE_MAX_SIZE_MB": "cache-max-size-mb",
}

// configLoad records how the effective configuration was chosen, so the startup
// notice can name the sources that decided it.
type configLoad struct {
	strict     bool
	filePath   string   // config file used; empty when running on env/defaults
	applied    []string // env variables that replaced a config-file value
	fileFailed error    // config file could not be loaded
}

// notice renders the startup precedence report, or "" when there is nothing to
// report. appliedFlags names the CLI flags that actually wrote a value, so a
// variable a flag replaced is not named as the one that decided the
// configuration.
func (l configLoad) notice(appliedFlags map[string]bool) string {
	if l.fileFailed != nil {
		return fmt.Sprintf("Warning: Failed to load config file: %v, using defaults merged with environment", l.fileFailed)
	}

	names := l.effective(appliedFlags)
	if l.strict {
		notice := fmt.Sprintf("Strict config mode: config file %s is authoritative; RFFMPEG_* overrides ignored", l.filePath)
		if len(names) > 0 {
			notice += fmt.Sprintf(" (%s)", strings.Join(names, ", "))
		}
		return notice
	}
	if len(names) == 0 {
		return ""
	}
	return fmt.Sprintf("Warning: %s override config file %s; pass -strict-config to keep the config file authoritative",
		strings.Join(names, ", "), l.filePath)
}

// effective keeps the applied overrides that no effective flag replaced — the
// variables the notice may name.
func (l configLoad) effective(appliedFlags map[string]bool) []string {
	names := make([]string, 0, len(l.applied))
	for _, env := range l.applied {
		if flagName, ok := envFlagNames[env]; ok && appliedFlags[flagName] {
			continue
		}
		names = append(names, env)
	}
	return names
}

// resolveConfig loads the worker configuration and reports the precedence
// steps that decided it. Priority is command-line flags > RFFMPEG_* environment
// variables > config file > defaults; the caller applies flag overrides on top,
// then asks configLoad.notice what to report.
//
// strict makes the config file authoritative: the environment step is skipped,
// and a missing or unreadable file is an error instead of a fall-back to
// environment-only config.
func resolveConfig(configPath string, strict bool) (*workerconfig.Config, configLoad, error) {
	envCfg := workerconfig.LoadFromEnv()

	if configPath == "" {
		if strict {
			return nil, configLoad{}, errors.New("-strict-config requires a config file: pass -config or set RFFMPEG_CONFIG")
		}
		return envCfg, configLoad{}, nil
	}

	fileCfg, err := workerconfig.LoadFromFile(configPath)
	if err != nil {
		if strict {
			return nil, configLoad{}, fmt.Errorf("-strict-config: %w", err)
		}
		// A failed load must not discard RFFMPEG_TOKEN/RFFMPEG_SERVER_URL: they
		// are the only remaining route to a token-protected server.
		return workerconfig.Merge(workerconfig.DefaultConfig(), envCfg), configLoad{fileFailed: err}, nil
	}

	merged, applied := workerconfig.MergeAndReport(fileCfg, envCfg)
	if strict {
		return fileCfg, configLoad{strict: true, filePath: configPath, applied: applied}, nil
	}
	return merged, configLoad{filePath: configPath, applied: applied}, nil
}

// registerConnectionFlags registers the connection CLI flags on fs and returns
// pointers to their values.
func registerConnectionFlags(fs *flag.FlagSet) (token, inputAuthHeader, serverURL *string) {
	token = fs.String("token", "", "Worker authentication token (overrides config file and RFFMPEG_TOKEN env)")
	inputAuthHeader = fs.String("input-auth-header", "", "Authorization header for remote input URLs (overrides config file and RFFMPEG_INPUT_AUTH_HEADER env)")
	serverURL = fs.String("server-url", "", "Server URL (overrides config file and RFFMPEG_SERVER_URL env)")
	return token, inputAuthHeader, serverURL
}

// applyConnectionFlagOverrides maps the connection CLI flags onto cfg and
// reports which of them took effect. An empty value means the operator supplied
// nothing, so it neither writes a field nor counts as replacing it: with
// `-server-url ""` the merged value — and the variable that chose it — stays in
// charge and must remain in the precedence notice.
func applyConnectionFlagOverrides(token, inputAuthHeader, serverURL *string, cfg *workerconfig.Config) map[string]bool {
	applied := make(map[string]bool)
	if *token != "" {
		cfg.Token = *token
		applied["token"] = true
	}
	if *inputAuthHeader != "" {
		cfg.InputAuthHeader = *inputAuthHeader
		applied["input-auth-header"] = true
	}
	if *serverURL != "" {
		cfg.ServerURL = *serverURL
		applied["server-url"] = true
	}
	return applied
}

// cacheEnabledValue adapts a *bool to flag.Value so -cache-enabled accepts the
// same boolean aliases as RFFMPEG_CACHE_ENABLED (workerconfig.ParseBool).
type cacheEnabledValue struct {
	target *bool
}

// String returns the flag's value, used by flag for --help defaults.
func (v cacheEnabledValue) String() string {
	if v.target == nil {
		return "false"
	}
	return strconv.FormatBool(*v.target)
}

// Set parses s via workerconfig.ParseBool so the CLI and env alias sets match.
func (v cacheEnabledValue) Set(s string) error {
	parsed, ok := workerconfig.ParseBool(s)
	if !ok {
		return errors.New("expected 1/true/yes/on or 0/false/no/off")
	}
	*v.target = parsed
	return nil
}

// IsBoolFlag makes a bare -cache-enabled mean -cache-enabled=true.
func (v cacheEnabledValue) IsBoolFlag() bool { return true }

// registerCacheFlags registers the cache-related CLI flags on fs and returns
// pointers to their values. Defaults derive from workerconfig.DefaultConfig so
// --help cannot drift from the effective config defaults.
func registerCacheFlags(fs *flag.FlagSet) (cacheEnabled *bool, cacheTTL *time.Duration, cacheMaxSizeMB *int64) {
	defaults := workerconfig.DefaultConfig()
	cacheEnabled = new(bool)
	*cacheEnabled = defaults.CacheEnabled
	fs.Var(cacheEnabledValue{cacheEnabled}, "cache-enabled", "Enable job output cache")
	cacheTTL = fs.Duration("cache-ttl", defaults.CacheTTL.ToDuration(), "Cache entry TTL")
	cacheMaxSizeMB = fs.Int64("cache-max-size-mb", defaults.CacheMaxSizeMB, "Cache max size in MiB")
	return cacheEnabled, cacheTTL, cacheMaxSizeMB
}

// applyCacheFlagOverrides maps the cache CLI flags onto cfg for every flag the
// operator explicitly set and reports which of them took effect. Every cache
// flag is assigned unconditionally — flag parsing rejects an empty value — so
// fs.Visit alone decides it. fs.Visit reports only set flags, not defaults, so
// -cache-enabled=false stays expressible while an absent flag never clobbers a
// value loaded from the config file or environment.
func applyCacheFlagOverrides(fs *flag.FlagSet, cacheEnabled *bool, cacheTTL *time.Duration, cacheMaxSizeMB *int64, cfg *workerconfig.Config) map[string]bool {
	applied := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "cache-enabled":
			cfg.CacheEnabled = *cacheEnabled
		case "cache-ttl":
			cfg.CacheTTL = workerconfig.Duration(*cacheTTL)
		case "cache-max-size-mb":
			cfg.CacheMaxSizeMB = *cacheMaxSizeMB
		default:
			return
		}
		applied[f.Name] = true
	})
	return applied
}

// detectCapabilities performs capability detection based on configuration.
func detectCapabilities(ctx context.Context, cfg *workerconfig.Config) (*protocol.WorkerCapabilities, error) {
	// Create FFmpeg probe
	probe := worker.NewFFmpegProbe(cfg.FFmpegPath)

	// Create capability detector
	detector := capabilities.NewDetector(&ffmpegProbeAdapter{probe})

	// Convert config
	capsCfg := &capabilities.Config{
		AutoDetectCodecs:    cfg.AutoDetectCodecs,
		AutoDetectGPU:       cfg.AutoDetectGPU,
		MaxConcurrent:       cfg.MaxConcurrent,
		EncoderPriority:     cfg.EncoderPriority,
		EncoderBlacklist:    cfg.EncoderBlacklist,
		ManualEncoders:      cfg.ManualEncoders,
		ManualDecoders:      cfg.ManualDecoders,
		ManualGPUModel:      cfg.ManualGPUModel,
		ManualFFmpegVersion: cfg.ManualFFmpegVersion,
	}

	return detector.DetectWithConfig(ctx, capsCfg)
}

// getDefaultCapabilities returns minimal capabilities when detection fails.
func getDefaultCapabilities(cfg *workerconfig.Config) *protocol.WorkerCapabilities {
	caps := &protocol.WorkerCapabilities{
		MaxConcurrent:    cfg.MaxConcurrent,
		EncoderPriority:  cfg.EncoderPriority,
		EncoderBlacklist: cfg.EncoderBlacklist,
	}

	if !cfg.AutoDetectCodecs {
		caps.Encoders = cfg.ManualEncoders
		caps.Decoders = cfg.ManualDecoders
		caps.FFmpegVersion = cfg.ManualFFmpegVersion
	}
	if !cfg.AutoDetectGPU {
		caps.GPUModel = cfg.ManualGPUModel
	}

	return caps
}

// logCapabilities logs detected capabilities.
func logCapabilities(caps *protocol.WorkerCapabilities) {
	log.Printf("Worker capabilities:")
	log.Printf("  FFmpeg version: %s", caps.FFmpegVersion)
	log.Printf("  Max concurrent: %d", caps.MaxConcurrent)
	log.Printf("  Video encoders: %d (%d HW)", len(caps.VideoEncoders), len(caps.HWEncoders))
	for _, enc := range caps.VideoEncoders {
		hwMark := ""
		if enc.IsHW {
			hwMark = " [HW]"
		}
		log.Printf("    - %s%s", enc.Name, hwMark)
	}
	log.Printf("  Video decoders: %d (%d HW)", len(caps.VideoDecoders), len(caps.HWDecoders))
	log.Printf("  GPU devices: %d", len(caps.GPUDevices))
	for _, dev := range caps.GPUDevices {
		accessible := ""
		if !dev.Accessible {
			accessible = " (not accessible)"
		}
		log.Printf("    - %s: %s%s", dev.Type, dev.Name, accessible)
	}
	if len(caps.EncoderPriority) > 0 {
		log.Printf("  Encoder priority: %v", caps.EncoderPriority)
	}
	if len(caps.EncoderBlacklist) > 0 {
		log.Printf("  Encoder blacklist: %v", caps.EncoderBlacklist)
	}
}

// ffmpegProbeAdapter adapts FFmpegProbe to capabilities.FFmpegProber interface.
type ffmpegProbeAdapter struct {
	probe *worker.FFmpegProbe
}

func (a *ffmpegProbeAdapter) Probe(ctx context.Context) (*capabilities.FFmpegInfo, error) {
	info, err := a.probe.Probe(ctx)
	if err != nil {
		return nil, err
	}

	result := &capabilities.FFmpegInfo{
		Version:  info.Version,
		Encoders: make([]capabilities.CodecInfo, len(info.Encoders)),
		Decoders: make([]capabilities.CodecInfo, len(info.Decoders)),
		Hwaccels: info.Hwaccels,
		Codecs:   info.Codecs,
		Filters:  info.Filters,
		PixFmts:  info.PixFmts,
		Formats:  info.Formats,
	}

	for i, enc := range info.Encoders {
		result.Encoders[i] = capabilities.CodecInfo{
			Name:        enc.Name,
			Description: enc.Description,
			Type:        string(enc.Type),
			IsHW:        enc.IsHW,
		}
	}

	for i, dec := range info.Decoders {
		result.Decoders[i] = capabilities.CodecInfo{
			Name:        dec.Name,
			Description: dec.Description,
			Type:        string(dec.Type),
			IsHW:        dec.IsHW,
		}
	}

	return result, nil
}

// getEnv gets an environment variable or returns a default value.
func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}
