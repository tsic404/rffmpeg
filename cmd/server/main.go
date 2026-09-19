package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"gopkg.in/natefinch/lumberjack.v2"

	"github.com/tsic404/rffmpeg/pkg/config"
	"github.com/tsic404/rffmpeg/pkg/server/auth"
	"github.com/tsic404/rffmpeg/pkg/server/db"
	"github.com/tsic404/rffmpeg/pkg/server/handlers"
	"github.com/tsic404/rffmpeg/pkg/server/panicguard"
	"github.com/tsic404/rffmpeg/pkg/server/ratelimit"
	"github.com/tsic404/rffmpeg/pkg/server/scheduler"
	"github.com/tsic404/rffmpeg/pkg/server/storage"
	"github.com/tsic404/rffmpeg/pkg/server/websocket"
	"github.com/tsic404/rffmpeg/pkg/server/workerhealth"
	tlspkg "github.com/tsic404/rffmpeg/pkg/tls"
)

// staleWorkerFactor scales the heartbeat timeout into the startup cleanup
// cutoff: a worker whose last heartbeat is older than staleWorkerFactor × the
// heartbeat timeout is dead residue and is removed lazily at startup
// Fresh-heartbeat workers survive the restart and re-register.
const staleWorkerFactor = 1.5

func main() {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("FATAL: server panicked: %v\n%s", r, debug.Stack())
			os.Exit(1)
		}
	}()
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
	// Create the data directory if it does not exist: sqlite
	// refuses to open a database whose parent directory is missing, and the
	// resulting "unable to open database file" error gives no actionable
	// hint. MkdirAll keeps --data-dir runnable out of the box.
	if err := os.MkdirAll(cfg.DataDir, 0755); err != nil {
		log.Fatalf("Failed to create data directory %s: %v", cfg.DataDir, err)
	}

	// Multipart upload bodies that exceed the in-memory parse threshold spill
	// to temporary files. The OS temp dir (/tmp) is often a small tmpfs, so a
	// large worker output upload can exhaust it and drop an already produced
	// file. Default the spill dir to the data directory's filesystem unless the
	// operator overrides it with --multipart-tmp-dir.
	if cfg.MultipartTmpDir == "" {
		cfg.MultipartTmpDir = cfg.DataDir
	}
	if err := os.MkdirAll(cfg.MultipartTmpDir, 0755); err != nil {
		log.Fatalf("Failed to create multipart temp directory %s: %v", cfg.MultipartTmpDir, err)
	}
	// mime/multipart hardcodes os.CreateTemp("", "multipart-*"), which resolves
	// through os.TempDir() → $TMPDIR. Point TMPDIR at the spill dir before any
	// request is served so large bodies never touch the tmpfs.
	if err := os.Setenv("TMPDIR", cfg.MultipartTmpDir); err != nil {
		log.Fatalf("Failed to set TMPDIR: %v", err)
	}

	// Route logging to a file in the data directory in addition to stderr so
	// that a panic or fatal in any goroutine — whose stack trace the runtime
	// otherwise writes only to stderr — is captured on disk instead of
	// vanishing with the process. This is what turns the "silent exit" into a
	// diagnosable crash.
	logFile, err := setupLogFile(cfg.DataDir)
	if err != nil {
		log.Printf("Warning: failed to open log file: %v", err)
	} else {
		defer logFile.Close()
	}

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

	// Lazily remove worker records whose heartbeat is stale — older than
	// 1.5× the heartbeat timeout. After a restart no worker is
	// serving, so a row that has gone that long without a heartbeat is dead
	// residue; a fresh-heartbeat worker is a live process that survived the
	// restart and re-registers to come back idle. Deleting only genuinely
	// stale rows keeps /api/v1/workers stable across restarts instead of
	// transiently dropping every worker to zero.
	staleWorkerCutoff := time.Duration(float64(cfg.WorkerHeartbeatTimeout) * staleWorkerFactor)
	staleWorkersRemoved, err := database.RemoveStaleWorkers(staleWorkerCutoff)
	if err != nil {
		log.Printf("Warning: failed to remove stale workers: %v", err)
	}

	if jobsReset > 0 || workersMarkedOffline > 0 || staleWorkersRemoved > 0 {
		log.Printf("State recovery: reset %d job(s) to pending, marked %d worker(s) offline, removed %d stale worker record(s)",
			jobsReset, workersMarkedOffline, staleWorkersRemoved)
	}

	// Initialize storage
	store, err := storage.New(cfg.DataDir)
	if err != nil {
		log.Fatalf("Failed to initialize storage: %v", err)
	}

	// Initialize worker state table (shared between handler and monitor)
	stateTable := workerhealth.NewWorkerStateTable(cfg.WorkerHeartbeatTimeout)

	// Create handler
	h := handlers.New(database, store, cfg.Version, stateTable)
	h.SetMultipartTmpDir(cfg.MultipartTmpDir)

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

	// Start worker health monitor. DefaultConfig() carries the MaxRetryCount
	// migration budget; only the durations are overridden from ServerConfig.
	workerHealthConfig := workerhealth.DefaultConfig()
	workerHealthConfig.HeartbeatTimeout = cfg.WorkerHeartbeatTimeout
	workerHealthConfig.OfflineThreshold = cfg.WorkerOfflineThreshold
	workerHealthConfig.HealthCheckInterval = cfg.WorkerHealthCheckInterval
	workerHealthConfig.MaxRetryCount = cfg.MaxRetryCount
	workerMonitor := workerhealth.New(database, workerHealthConfig)
	workerMonitor.SetStateTable(stateTable)
	workerMonitor.Start()
	log.Printf("Worker health monitor started (heartbeat timeout: %s, offline threshold: %s, check interval: %s, max retry count: %d)",
		cfg.WorkerHeartbeatTimeout, cfg.WorkerOfflineThreshold, cfg.WorkerHealthCheckInterval, cfg.MaxRetryCount)

	// Start job scheduler. DefaultConfig() carries MaxTimeoutRetries=2; the
	// literal Config{} previously dropped it to 0 and failed jobs on the
	// first timeout. Override only the fields ServerConfig owns.
	schedulerConfig := scheduler.DefaultConfig()
	schedulerConfig.JobTimeout = cfg.JobTimeout
	schedulerConfig.ScheduleInterval = cfg.ScheduleInterval
	schedulerConfig.TimeoutCheckInterval = cfg.TimeoutCheckInterval
	schedulerConfig.MaxJobsPerWorker = cfg.MaxJobsPerWorker
	schedulerConfig.NoWorkerJobTimeout = cfg.NoWorkerJobTimeout
	schedulerConfig.HeartbeatFreshness = cfg.WorkerHeartbeatTimeout
	schedulerConfig.MaxTimeoutRetries = cfg.MaxTimeoutRetries
	jobScheduler := scheduler.New(database, schedulerConfig)

	// Connect the scheduler to the monitor so job migration triggers rescheduling
	workerMonitor.SetScheduler(jobScheduler)

	// Connect the scheduler to the handler for immediate job assignment
	h.SetScheduler(jobScheduler)

	// Submit-time fail-fast uses the same freshness window as the monitor:
	// workers with stale heartbeats are treated as unavailable at submission
	// instead of accepting jobs that would wait for the no-worker
	// job timeout.
	h.SetHeartbeatTimeout(cfg.WorkerHeartbeatTimeout)
	h.SetStarvationConfig(cfg.NoWorkerJobTimeout, cfg.TimeoutCheckInterval)
	h.SetMaxJobsPerWorker(cfg.MaxJobsPerWorker)

	jobScheduler.Start()
	log.Printf("Job scheduler started (job timeout: %s, schedule interval: %s, timeout check interval: %s, max jobs per worker: %d, no-worker job timeout: %s, max timeout retries: %d)",
		schedulerConfig.JobTimeout, schedulerConfig.ScheduleInterval, schedulerConfig.TimeoutCheckInterval, schedulerConfig.MaxJobsPerWorker, schedulerConfig.NoWorkerJobTimeout, schedulerConfig.MaxTimeoutRetries)

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

	// Scheduler releases rate-limit quota and broadcasts WS status for jobs
	// it fails out-of-band (starvation sweep) — the handler path that normally
	// does both is bypassed by the bulk DB update.
	jobScheduler.SetRateLimiter(h.GetRateLimiter())
	jobScheduler.SetJobNotifier(h.GetWSHub())

	// Setup router
	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(handlers.NormalizeMethodNotAllowed)

	// Global request-body cap for JSON endpoints. Upload handlers apply
	// their own larger MaxBytesReader before parsing multipart forms, so a
	// 10MB default never truncates legitimate uploads.
	r.Use(auth.MaxBodyBytesMiddleware)

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
		r.Get("/jobs", h.ListJobs)
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

		// Migration audit events
		r.Get("/migrations", h.ListMigrationEvents)
		r.Get("/migrations/{eventId}", h.GetMigrationEvent)

		// Probe (ffprobe sync endpoint) — behind the job-submission rate
		// limiter: a probe dispatches real work, so an unthrottled client can
		// starve the queue just like unbounded job submissions.
		r.Group(func(r chi.Router) {
			r.Use(ratelimit.JobSubmitMiddleware(h.GetRateLimiter(), rateLimitCfg))
			r.Post("/probe", h.Probe)
		})
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
	// Protocol-consistent JSON 404 for any unmatched path, so a trailing empty
	// segment (e.g. /api/v1/migrations/) returns the same {"code":"not_found"}
	// structure as resource-specific 404s (e.g. GetMigrationEvent).
	r.NotFound(handlers.NotFound)

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
	go panicguard.Guard("http server", func() {
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
	})

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

// setupLogFile redirects the standard logger to write to both stderr and a
// size/age-bounded log file in the data directory. lumberjack rotates the file
// at MaxSize and prunes backups beyond MaxBackups/MaxAge, so a long-running
// server's log cannot grow without bound. Panics recovered
// by the background-goroutine guards log through this logger, so the crash
// cause lands on disk even when the deployment only captures stdout. A non-nil
// error means the file could not be opened and logging falls back to stderr
// only.
func setupLogFile(dataDir string) (*lumberjack.Logger, error) {
	path := filepath.Join(dataDir, "rffmpeg-server.log")
	// Eager writability check: lumberjack opens lazily on first write, so a
	// misconfigured data-dir would otherwise fail silently at startup.
	probe, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return nil, err
	}
	if err := probe.Close(); err != nil {
		return nil, err
	}
	logFile := &lumberjack.Logger{
		Filename:   path,
		MaxSize:    100, // megabytes
		MaxBackups: 3,
		MaxAge:     28, // days
		Compress:   false,
	}
	log.SetOutput(io.MultiWriter(os.Stderr, logFile))
	return logFile, nil
}

// parseFlags parses command-line flags
func parseFlags() *config.Flags {
	flags := &config.Flags{}

	flag.StringVar(&flags.Port, "port", "", "Server port (default: 8080)")
	flag.StringVar(&flags.DataDir, "data-dir", "", "Data directory (default: ./data)")
	flag.StringVar(&flags.MultipartTmpDir, "multipart-tmp-dir", "", "Directory for multipart upload temp files (default: --data-dir)")
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
	// Retry budgets — flag.Func keeps 0 expressible (disable retries); the
	// zero value would otherwise read as "unset". nil pointer = flag unset.
	flag.Func("max-timeout-retries", "Maximum times a timed-out job is requeued before failing; 0 disables retries (default: 2)", func(s string) error {
		n, err := strconv.Atoi(s)
		if err != nil || n < 0 {
			return fmt.Errorf("invalid max-timeout-retries %q: must be a non-negative integer", s)
		}
		flags.MaxTimeoutRetries = &n
		return nil
	})
	flag.Func("max-retry-count", "Maximum times a job is migrated after worker failure before failing; 0 disables migration (default: 3)", func(s string) error {
		n, err := strconv.Atoi(s)
		if err != nil || n < 0 {
			return fmt.Errorf("invalid max-retry-count %q: must be a non-negative integer", s)
		}
		flags.MaxRetryCount = &n
		return nil
	})
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
