package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/tsix404/rffmpeg/pkg/config"
	"github.com/tsix404/rffmpeg/pkg/server/auth"
	"github.com/tsix404/rffmpeg/pkg/server/db"
	"github.com/tsix404/rffmpeg/pkg/server/handlers"
	"github.com/tsix404/rffmpeg/pkg/server/ratelimit"
	"github.com/tsix404/rffmpeg/pkg/server/scheduler"
	"github.com/tsix404/rffmpeg/pkg/server/storage"
	"github.com/tsix404/rffmpeg/pkg/server/websocket"
	"github.com/tsix404/rffmpeg/pkg/server/workerhealth"
	tlspkg "github.com/tsix404/rffmpeg/pkg/tls"
)

func main() {
	// Parse command-line flags
	flags := parseFlags()

	// Load configuration
	cfg, err := loadConfig(flags)
	if err != nil {
		log.Fatalf("Failed to load configuration: %v", err)
	}

	// Validate configuration
	if err := cfg.Validate(); err != nil {
		log.Fatalf("Invalid configuration: %v", err)
	}

	// Resolve relative paths
	if err := cfg.ResolvePaths("."); err != nil {
		log.Fatalf("Failed to resolve paths: %v", err)
	}

	log.Printf("Configuration: %s", cfg)

	// Initialize database
	dbPath := fmt.Sprintf("%s/rffmpeg.db", cfg.DataDir)
	database, err := db.New(dbPath)
	if err != nil {
		log.Fatalf("Failed to initialize database: %v", err)
	}
	defer database.Close()

	// Recover state from previous run (if any)
	jobsReset, workersMarkedOffline, err := database.RecoverState()
	if err != nil {
		log.Fatalf("Failed to recover database state: %v", err)
	}

	// Remove worker records left offline by previous runs (TSI-2366). After a
	// restart no worker is serving, so any offline row is residue; live
	// workers re-register and come back as idle. This keeps crash loops from
	// accumulating stale duplicate records that the health monitor would only
	// clean up after the offline threshold — if it ever runs at all.
	staleWorkersRemoved, err := database.RemoveStaleOfflineWorkers()
	if err != nil {
		log.Printf("Warning: failed to remove stale offline workers: %v", err)
	}

	if jobsReset > 0 || workersMarkedOffline > 0 || staleWorkersRemoved > 0 {
		log.Printf("State recovery: reset %d job(s) to pending, marked %d worker(s) offline, removed %d stale offline worker record(s)",
			jobsReset, workersMarkedOffline, staleWorkersRemoved)
	}

	// Initialize storage
	store, err := storage.New(cfg.DataDir)
	if err != nil {
		log.Fatalf("Failed to initialize storage: %v", err)
	}

	// Initialize worker state table (shared between handler and monitor, TSI-759)
	stateTable := workerhealth.NewWorkerStateTable(cfg.WorkerHeartbeatTimeout)

	// Create handler
	h := handlers.New(database, store, cfg.Version, stateTable)

	// Create chunk upload handler
	chunkHandler := handlers.NewChunkUploadHandler(database, store, 0) // Uses default chunk size (10MB)
	chunkHandler.SetAuthToken(cfg.AuthToken)

	// Configure WebSocket allowed origins
	if len(cfg.AllowedOrigins) > 0 {
		websocket.SetAllowedOrigins(cfg.AllowedOrigins)
		log.Printf("WebSocket allowed origins: %v", cfg.AllowedOrigins)
	}

	// Start WebSocket hub
	h.StartWSHub()
	log.Printf("WebSocket hub started")

	// Start worker health monitor
	workerMonitor := workerhealth.New(database, workerhealth.Config{
		HeartbeatTimeout:    cfg.WorkerHeartbeatTimeout,
		OfflineThreshold:    cfg.WorkerOfflineThreshold,
		HealthCheckInterval: cfg.WorkerHealthCheckInterval,
		MaxRetryCount:       3,
	})
	workerMonitor.SetStateTable(stateTable)
	workerMonitor.Start()
	log.Printf("Worker health monitor started (heartbeat timeout: %s, offline threshold: %s, check interval: %s)",
		cfg.WorkerHeartbeatTimeout, cfg.WorkerOfflineThreshold, cfg.WorkerHealthCheckInterval)

	// Start job scheduler
	jobScheduler := scheduler.New(database, scheduler.Config{
		JobTimeout:           cfg.JobTimeout,
		ScheduleInterval:     cfg.ScheduleInterval,
		TimeoutCheckInterval: cfg.TimeoutCheckInterval,
		MaxJobsPerWorker:     cfg.MaxJobsPerWorker,
		NoWorkerJobTimeout:   cfg.NoWorkerJobTimeout,
	})

	// Connect the scheduler to the monitor so job migration triggers rescheduling
	workerMonitor.SetScheduler(jobScheduler)

	// Connect the scheduler to the handler for immediate job assignment (TSI-1501)
	h.SetScheduler(jobScheduler)

	jobScheduler.Start()
	log.Printf("Job scheduler started (job timeout: %s, schedule interval: %s, timeout check interval: %s, max jobs per worker: %d, no-worker job timeout: %s)",
		cfg.JobTimeout, cfg.ScheduleInterval, cfg.TimeoutCheckInterval, cfg.MaxJobsPerWorker, cfg.NoWorkerJobTimeout)

	// Initialize rate limiter runtime config
	rateLimitCfg := &ratelimit.RuntimeConfig{
		Enabled: cfg.RateLimitEnabled,
		Limit:   cfg.MaxConcurrentJobsPerClient,
	}
	if rateLimitCfg.Limit <= 0 {
		rateLimitCfg.Limit = 10 // sensible fallback
	}
	log.Printf("Rate limit: enabled=%v, maxConcurrentJobsPerClient=%d", rateLimitCfg.Enabled, rateLimitCfg.Limit)

	// Set rate limiter on handler (may be replaced with Redis counter if configured)
	if cfg.RedisAddr != "" {
		log.Printf("Distributed rate limiting: Redis at %s", cfg.RedisAddr)
		// Note: Redis counter requires go-redis dependency.
		// Falling back to in-memory counter. For distributed deployments,
		// install github.com/redis/go-redis/v9 and create a RedisCounter implementation.
	}

	// Setup router
	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)

	// Pass auth token to handler for health check reporting
	h.SetAuthToken(cfg.AuthToken)

	// Authentication middleware is always installed: with a configured token it
	// validates requests; without one it rejects everything (fail closed).
	r.Use(auth.Middleware(cfg.AuthToken))
	if cfg.AuthToken != "" {
		log.Printf("Authentication enabled")
	} else {
		log.Printf("WARNING: No auth token configured. All API requests will be rejected (401). Set --auth-token to enable access.")
	}

	// API routes
	r.Route("/api/v1", func(r chi.Router) {
		// File upload (simple)
		r.Post("/upload", h.Upload)

		// Chunked upload
		r.Post("/upload/init", chunkHandler.InitChunkUpload)
		r.Post("/upload/chunk/{uploadId}/{chunkIndex}", chunkHandler.UploadChunk)
		r.Get("/upload/progress/{uploadId}", chunkHandler.GetUploadProgress)
		r.Post("/upload/complete", chunkHandler.CompleteChunkUpload)
		r.Post("/upload/cancel/{uploadId}", chunkHandler.CancelChunkUpload)
		r.Get("/upload/resume/{uploadId}", chunkHandler.ResumeChunkUpload)

		// Jobs
		r.Group(func(r chi.Router) {
			r.Use(ratelimit.JobSubmitMiddleware(h.GetRateLimiter(), rateLimitCfg))
			r.Post("/jobs", h.SubmitJob)
		})
		r.Get("/jobs/{jobId}", h.GetJob)
		r.Delete("/jobs/{jobId}", h.CancelJob)
		r.Patch("/jobs/{jobId}", h.UpdateJob)
		r.Post("/jobs/{jobId}/output", h.UploadJobOutput)
		r.Get("/jobs/{jobId}/log", h.GetJobLogWS) // WebSocket endpoint for real-time logs

		// Output download
		r.Get("/output/{fileId}", h.DownloadOutput)

		// File download (for workers)
		r.Get("/files/{fileId}", h.DownloadFile)

		// Workers
		r.Post("/workers/register", h.RegisterWorker)
		r.Post("/workers/heartbeat", h.WorkerHeartbeat)
		r.Get("/workers/{workerId}/jobs", h.PullWorkerJobs)
		r.Get("/workers", h.ListWorkers)
		r.Get("/workers/{workerId}", h.GetWorker)

		// Encoders (capability query)
		r.Get("/encoders", h.ListAllEncoders)
		r.Get("/encoders/{encoder}/workers", h.ListWorkersByEncoder)

		// Decoders (capability query)
		r.Get("/decoders", h.ListAllDecoders)

		// Info flags (capability query — ffmpeg -codecs/-filters/-pix_fmts/-formats equivalent)
		r.Get("/codecs", h.ListAllCodecs)
		r.Get("/filters", h.ListAllFilters)
		r.Get("/pix_fmts", h.ListAllPixFmts)
		r.Get("/formats", h.ListAllFormats)

		// Hwaccels (P0 info flag aggregation)
		r.Get("/hwaccels", h.ListAllHwaccels)

		// Health check
		r.Get("/health", h.Health)

		// Probe (ffprobe sync endpoint)
		r.Post("/probe", h.Probe)
	})

	// Admin routes (outside /api/v1, separate prefix)
	r.Route("/admin", func(r chi.Router) {
		adminHandler := handlers.NewAdminHandler(h, rateLimitCfg)
		r.Get("/config", adminHandler.GetConfig)
		r.Patch("/config", adminHandler.UpdateConfig)
		r.Get("/metrics", adminHandler.GetMetrics)
	})

	// Root-level health check for monitoring tools (load balancers, orchestrators)
	r.Get("/health", h.Health)

	// Create HTTP server
	addr := fmt.Sprintf(":%s", cfg.Port)
	srv := &http.Server{
		Addr:         addr,
		Handler:      r,
		ReadTimeout:  5 * time.Minute, // Allow long uploads
		WriteTimeout: 5 * time.Minute, // Allow long downloads
		IdleTimeout:  120 * time.Second,
	}

	// Configure TLS if enabled
	var tlsConfig *tlspkg.Config
	if cfg.TLS.Enabled {
		tlsConfig = cfg.TLS

		// Perform initial certificate expiration check
		if err := tlspkg.RunExpirationCheck(tlsConfig); err != nil {
			log.Printf("WARNING: Certificate expiration check failed: %v", err)
		}

		// Create TLS config for the server
		tlsServerConfig, err := tlsConfig.ToTLSConfig()
		if err != nil {
			log.Fatalf("Failed to create TLS configuration: %v", err)
		}
		srv.TLSConfig = tlsServerConfig

		// Start certificate expiration monitor
		monitor := tlspkg.NewCertMonitor(tlsConfig, func(certInfo *tlspkg.CertificateInfo, message string) {
			log.Printf("[TLS ALERT] Certificate %s: %s", certInfo.FilePath, message)
		})
		monitor.Start()
		defer monitor.Stop()
	}

	// Start server in goroutine
	go func() {
		if cfg.TLS.Enabled {
			log.Printf("Starting HTTPS server on %s (version %s, mTLS: %v)", addr, cfg.Version, cfg.TLS.MTLS)
			if err := srv.ListenAndServeTLS("", ""); err != nil && err != http.ErrServerClosed {
				log.Fatalf("Server failed: %v", err)
			}
		} else {
			log.Printf("Starting HTTP server on %s (version %s)", addr, cfg.Version)
			if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Fatalf("Server failed: %v", err)
			}
		}
	}()

	// Graceful shutdown
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Println("Shutting down server...")
	workerMonitor.Stop()
	jobScheduler.Stop()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		log.Fatalf("Server shutdown failed: %v", err)
	}

	log.Println("Server stopped")
}

// parseFlags parses command-line flags
func parseFlags() *config.Flags {
	flags := &config.Flags{}

	flag.StringVar(&flags.Port, "port", "", "Server port (default: 8080)")
	flag.StringVar(&flags.DataDir, "data-dir", "", "Data directory (default: ./data)")
	flag.StringVar(&flags.Version, "version", "", "Server version")
	flag.StringVar(&flags.Config, "config", "", "Path to configuration file (JSON)")

	// Worker management flags
	flag.StringVar(&flags.WorkerHeartbeatTimeout, "worker-heartbeat-timeout", "", "Timeout before marking worker offline (default: 90s)")
	flag.StringVar(&flags.WorkerOfflineThreshold, "worker-offline-threshold", "", "Duration after which offline workers are removed (default: 10m)")
	flag.StringVar(&flags.WorkerHealthCheckInterval, "worker-health-check-interval", "", "Interval for checking worker health (default: 30s)")

	// Scheduler flags
	flag.StringVar(&flags.JobTimeout, "job-timeout", "", "Timeout for running jobs before rescheduling (default: 30m)")
	flag.StringVar(&flags.ScheduleInterval, "schedule-interval", "", "Interval for job scheduling (default: 5s)")
	flag.StringVar(&flags.TimeoutCheckInterval, "timeout-check-interval", "", "Interval for checking job timeouts (default: 30s)")
	flag.StringVar(&flags.NoWorkerJobTimeout, "no-worker-job-timeout", "", "Fail pending jobs waiting longer than this with no schedulable worker; 0 disables (default: 2m)")

	// TLS flags
	flag.BoolVar(&flags.TLSEnabled, "tls", false, "Enable TLS (HTTPS)")
	flag.StringVar(&flags.TLSCertFile, "tls-cert", "", "Path to TLS certificate file")
	flag.StringVar(&flags.TLSKeyFile, "tls-key", "", "Path to TLS private key file")
	flag.StringVar(&flags.TLSClientCAFile, "tls-client-ca", "", "Path to client CA certificate file (for mTLS)")
	flag.BoolVar(&flags.TLSMTLS, "mtls", false, "Enable mTLS (mutual TLS authentication)")
	flag.StringVar(&flags.TLSMinVersion, "tls-min-version", "", "Minimum TLS version (TLS1.0, TLS1.1, TLS1.2, TLS1.3)")
	flag.IntVar(&flags.TLSExpirationWarningDays, "tls-expiration-warning", 0, "Days before expiration to warn (default: 30)")

	// Authentication flags
	flag.StringVar(&flags.AuthToken, "auth-token", "", "Authentication token (PSK) for API requests")

	// Rate limit flags
	flag.BoolVar(&flags.RateLimitEnabled, "rate-limit-enabled", true, "Enable per-client rate limiting")
	flag.IntVar(&flags.MaxConcurrentJobsPerClient, "max-concurrent-jobs-per-client", 0, "Maximum concurrent jobs per client (default: 10)")
	flag.StringVar(&flags.RedisAddr, "redis-addr", "", "Redis address for distributed rate limiting (optional)")

	flag.Parse()

	return flags
}

// loadConfig loads configuration from file, environment, and flags
func loadConfig(flags *config.Flags) (*config.ServerConfig, error) {
	var cfg *config.ServerConfig

	// Load from config file if specified
	if flags.Config != "" {
		var err error
		cfg, err = config.LoadFromFile(flags.Config)
		if err != nil {
			return nil, fmt.Errorf("failed to load config file: %w", err)
		}
	} else {
		// Load from environment
		cfg = config.LoadFromEnv()
	}

	// Merge command-line flags (highest priority)
	cfg.Merge(flags)

	// Apply defaults for empty values
	if cfg.Port == "" {
		cfg.Port = "8080"
	}
	if cfg.DataDir == "" {
		cfg.DataDir = "./data"
	}
	if cfg.Version == "" {
		cfg.Version = "1.0.0"
	}

	return cfg, nil
}
