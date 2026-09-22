package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"os"
	"os/signal"
	"strconv"
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
	token := flag.String("token", "", "Worker authentication token (overrides config file and RFFMPEG_TOKEN env)")
	inputAuthHeader := flag.String("input-auth-header", "", "Authorization header for remote input URLs (overrides config file and RFFMPEG_INPUT_AUTH_HEADER env)")
	serverURL := flag.String("server-url", "", "Server URL (overrides config file and RFFMPEG_SERVER_URL env)")
	cacheEnabled, cacheTTL, cacheMaxSizeMB := registerCacheFlags(flag.CommandLine)
	flag.Parse()

	// Load configuration. On file-load failure, fall back to defaults but
	// still merge environment variables — silently discarding
	// RFFMPEG_TOKEN/RFFMPEG_SERVER_URL would leave the worker unable to
	// authenticate against a token-required server.
	var cfg *workerconfig.Config
	if *configPath != "" {
		fileCfg, err := workerconfig.LoadFromFile(*configPath)
		if err != nil {
			log.Printf("Warning: Failed to load config file: %v, using defaults merged with environment", err)
			cfg = workerconfig.Merge(workerconfig.DefaultConfig(), workerconfig.LoadFromEnv())
		} else {
			// Merge with environment (env takes precedence)
			cfg = workerconfig.Merge(fileCfg, workerconfig.LoadFromEnv())
		}
	} else {
		// Load from environment only
		cfg = workerconfig.LoadFromEnv()
	}

	// CLI flags take highest precedence
	if *token != "" {
		cfg.Token = *token
	}
	if *inputAuthHeader != "" {
		cfg.InputAuthHeader = *inputAuthHeader
	}
	if *serverURL != "" {
		cfg.ServerURL = *serverURL
	}
	applyCacheFlagOverrides(flag.CommandLine, cacheEnabled, cacheTTL, cacheMaxSizeMB, cfg)
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
// operator explicitly set. fs.Visit reports only set flags — not defaults — so
// -cache-enabled=false stays expressible while an absent flag never clobbers a
// value loaded from the config file or environment.
func applyCacheFlagOverrides(fs *flag.FlagSet, cacheEnabled *bool, cacheTTL *time.Duration, cacheMaxSizeMB *int64, cfg *workerconfig.Config) {
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "cache-enabled":
			cfg.CacheEnabled = *cacheEnabled
		case "cache-ttl":
			cfg.CacheTTL = workerconfig.Duration(*cacheTTL)
		case "cache-max-size-mb":
			cfg.CacheMaxSizeMB = *cacheMaxSizeMB
		}
	})
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
