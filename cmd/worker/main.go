package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/tsix404/rffmpeg/pkg/protocol"
	"github.com/tsix404/rffmpeg/pkg/worker"
	"github.com/tsix404/rffmpeg/pkg/worker/capabilities"
	"github.com/tsix404/rffmpeg/pkg/worker/workerconfig"
)

const (
	defaultConfigPath = ""
)

func main() {
	configPath := flag.String("config", getEnv("RFFMPEG_CONFIG", defaultConfigPath), "Path to worker config file (JSON)")
	token := flag.String("token", "", "Worker authentication token (overrides config file and RFFMPEG_TOKEN env)")
	serverURL := flag.String("server-url", "", "Server URL (overrides config file and RFFMPEG_SERVER_URL env)")
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
	if *serverURL != "" {
		cfg.ServerURL = *serverURL
	}

	// Create worker
	workerCfg := worker.Config{
		ServerURL:         cfg.ServerURL,
		WorkerID:          cfg.WorkerID,
		Name:              cfg.Name,
		Token:             cfg.Token,
		TempDir:           cfg.TempDir,
		FFmpegPath:        cfg.FFmpegPath,
		Timeout:           cfg.Timeout.ToDuration(),
		HeartbeatInterval: cfg.HeartbeatInterval.ToDuration(),
		PollInterval:      cfg.PollInterval.ToDuration(),
		CacheConfig: worker.CacheConfig{
			Enabled:          cfg.CacheEnabled,
			Dir:              cfg.CacheDir,
			TTL:              cfg.CacheTTL.ToDuration(),
			MaxSizeBytes:     cfg.CacheMaxSizeMB * 1024 * 1024,
			TTLScanInterval:  10 * time.Minute,
			LRUCheckInterval: 5 * time.Minute,
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
