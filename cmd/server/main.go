package main

import (
	"context"
	"crypto/tls"
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
	"github.com/tsic404/rffmpeg/pkg/server/accesslog"
	"github.com/tsic404/rffmpeg/pkg/server/auth"
	"github.com/tsic404/rffmpeg/pkg/server/db"
	"github.com/tsic404/rffmpeg/pkg/server/handlers"
	"github.com/tsic404/rffmpeg/pkg/server/ratelimit"
	"github.com/tsic404/rffmpeg/pkg/server/scheduler"
	"github.com/tsic404/rffmpeg/pkg/server/storage"
	"github.com/tsic404/rffmpeg/pkg/server/transport"
	"github.com/tsic404/rffmpeg/pkg/server/websocket"
	"github.com/tsic404/rffmpeg/pkg/server/workerhealth"
	tlspkg "github.com/tsic404/rffmpeg/pkg/tls"
)

// newHTTPServer builds the listener configuration. ReadHeaderTimeout bounds
// how long a client may take to send request headers; ReadTimeout and
// WriteTimeout stay zero because transfers are bounded per read/write by
// pkg/server/transport rather than per request.
func newHTTPServer(addr string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
}

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
	warnInsecureTransport(cfg)

	// Bind before the expensive initialization so the port is in LISTEN within
	// milliseconds of process start; see bindListener for the probe contract.
	addr := fmt.Sprintf(":%s", cfg.Port)
	ln, err := bindListener(addr)
	if err != nil {
		log.Fatalf("Failed to listen on %s: %v", addr, err)
	}

	// Create the data directory if it does not exist: sqlite
	// refuses to open a database whose parent directory is missing, and the
	// resulting "unable to open database file" error gives no actionable
	// hint. MkdirAll keeps --data-dir runnable out of the box.
	if err := os.MkdirAll(cfg.DataDir, 0755); err != nil {
		log.Fatalf("Failed to create data directory %s: %v", cfg.DataDir, err)
	}

	// Warn at startup when the data directory's filesystem is nearly full so a
	// large upload does not later fail with a swallowed ENOSPC.
	warnLowDataDirSpace(cfg.DataDir)

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
	database, err := db.NewWithBusyTimeout(dbPath, cfg.BusyTimeoutMS)
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

	// Content-addressed input blobs accumulate without bound across a long
	// run; a background sweep removes blobs unused past the TTL while keeping
	// any file still referenced by a non-terminal job.
	inputCleaner := storage.NewInputFileCleaner(store, cfg.InputFileTTL, cfg.InputFileCleanupInterval, database.ActiveInputFileIDs)
	inputCleaner.Start(context.Background())
	defer inputCleaner.Stop()

	// Initialize worker state table (shared between handler and monitor)
	stateTable := workerhealth.NewWorkerStateTable(cfg.WorkerHeartbeatTimeout)

	// Create handler. SetMultipartTmpDir keeps ensureMultipartSpace's pre-flight
	// on the same filesystem as the TMPDIR spill dir set above — the two must
	// always agree.
	h := handlers.New(database, store, cfg.Version, stateTable)
	h.SetMultipartTmpDir(cfg.MultipartTmpDir)
	// C-107: the submission side judges pass-through inputs and targets against
	// the same space declaration the workers use. An empty declaration leaves
	// the mode with no legal path; it never falls back to any absolute path.
	h.SetSharedFSRoots(cfg.SharedFSRoots)

	// Fault injection for the slow-worker eviction path: heartbeats from the
	// listed workers report scaled throughput. A malformed value aborts startup
	// — a silently ignored switch would leave an E2E run waiting on an eviction
	// that can never happen.
	if cfg.ThroughputScale != "" {
		scaleEntries, err := workerhealth.ParseThroughputScale(cfg.ThroughputScale)
		if err != nil {
			log.Fatalf("Invalid RFFMPEG_THROUGHPUT_SCALE: %v", err)
		}
		h.SetThroughputScaler(workerhealth.NewThroughputScaler(scaleEntries))
		log.Printf("Throughput scale injection configured: %s", cfg.ThroughputScale)
	}

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

	// Scheduler broadcasts WS status for jobs it fails out-of-band (starvation
	// sweep); quota release for those jobs rides the database terminal hook.
	jobScheduler.SetJobNotifier(h.GetWSHub())

	// Jobs the monitor terminalizes itself (migration retry budget exhausted)
	// never emit the complete frame that normally releases a job's WS sequence
	// counter, so the monitor releases it explicitly.
	workerMonitor.SetJobStreamReleaser(h.GetWSHub())

	// Setup router. Access logging goes through a non-blocking sink: chi's
	// Logger writes the access line from inside the handler chain, before the
	// response is flushed, so a stalled stdout would stall every response it
	// logs — health probes and business endpoints alike (see pkg/server/accesslog).
	accessLog := accesslog.NewSink(os.Stdout, accesslog.DefaultQueueDepth)
	defer accessLog.Close()

	r := chi.NewRouter()
	r.Use(accesslog.Middleware(accessLog))
	r.Use(middleware.Recoverer)
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(handlers.NormalizeMethodNotAllowed)

	// No whole-request read or write deadline is configured; arm a fresh
	// per-read and per-write deadline instead, so a stalled body or a client
	// that stops reading a large response is dropped while a slow transfer
	// that keeps moving bytes completes.
	r.Use(transport.IdleBodyMiddleware(transport.IdleTimeout))
	r.Use(transport.IdleWriteMiddleware(transport.IdleTimeout))

	// Global request-body cap for JSON endpoints. Upload handlers apply
	// their own larger MaxBytesReader before parsing multipart forms, so a
	// 10MB default never truncates legitimate uploads.
	r.Use(handlers.MaxBodyBytesMiddleware)

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
	registerRoutes(r, h, chunkHandler, rateLimitCfg)

	// Create HTTP server. Headers must arrive within 15s (slowloris), but there
	// is no whole-request read or write deadline: the contract allows a 10 GiB
	// single upload and single-shot downloads of arbitrarily large outputs, so
	// a fixed cap truncates every transfer slower than that size divided by the
	// cap. Stalled transfers are cut by the idle deadline middleware instead
	// (see pkg/server/transport).
	srv := newHTTPServer(addr, r)

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
	if cfg.TLS.Enabled {
		log.Printf("Starting HTTPS server on %s (version %s, mTLS: %v)", addr, cfg.Version, cfg.TLS.MTLS)
	} else {
		log.Printf("Starting HTTP server on %s (version %s)", addr, cfg.Version)
	}
	serveListener(srv, ln)

	// Graceful shutdown
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Println("Shutting down server...")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := shutdownServer(ctx, srv, h.GetWSHub(), jobScheduler, workerMonitor); err != nil {
		// Keep going: the deferred closes below (WebSocket hub is already
		// closed by shutdownServer, access log, database, input cleaner) must
		// run. A fatal exit here would skip them and lose buffered writes.
		log.Printf("Server shutdown incomplete: %v", err)
	}

	log.Println("Server stopped")
}

// warnInsecureTransport makes the transport posture explicit at startup. TLS
// is off by default for local development, and a minimum version below 1.2 is
// still accepted for compatibility, but neither may pass silently: credentials
// and media traverse the network unprotected, and every operator who reads the
// startup log should see that.
func warnInsecureTransport(cfg *config.ServerConfig) {
	if cfg.TLS == nil || !cfg.TLS.Enabled {
		log.Printf("WARNING: TLS is disabled; the API is served over plaintext HTTP. Auth tokens and job payloads are not protected in transit.")
		return
	}
	if cfg.TLS.MinVersion != 0 && cfg.TLS.MinVersion < tls.VersionTLS12 {
		log.Printf("WARNING: TLS minimum version is %s; versions below TLS 1.2 are deprecated and should not be used on an untrusted network.", tls.VersionName(cfg.TLS.MinVersion))
	}
}

// stopper is the shutdown half of a background component.
type stopper interface {
	Stop()
}

// httpDrainer is the graceful-stop half of http.Server.
type httpDrainer interface {
	Shutdown(context.Context) error
}

// closer is the shutdown half of the WebSocket hub.
type closer interface {
	Close()
}

// shutdownServer drains the server in a deliberate order:
//
//  1. http.Server.Shutdown stops accepting requests and waits for in-flight
//     handlers, so no new job is submitted while the rest is torn down.
//  2. The WebSocket hub closes its connections — they are hijacked and
//     therefore invisible to http.Server.Shutdown — and stops accepting new
//     ones.
//  3. The scheduler stops dispatching.
//  4. The worker-health monitor stops, after which the deferred database and
//     access-log closes run.
//
// It returns the HTTP shutdown error instead of exiting, so a drain timeout
// still reaches the normal cleanup path.
func shutdownServer(ctx context.Context, srv httpDrainer, hub closer, scheduler, monitor stopper) error {
	var firstErr error
	if err := srv.Shutdown(ctx); err != nil {
		firstErr = fmt.Errorf("http shutdown: %w", err)
	}
	hub.Close()
	scheduler.Stop()
	monitor.Stop()
	return firstErr
}

// minDataDirFreeBytes is the free-space floor the data directory's filesystem
// should hold at startup. Below it, chunk assembly and input/output blob
// storage are one large upload away from ENOSPC, so the operator gets a
// prominent warning instead of a silent 500 later.
const minDataDirFreeBytes = 2 << 30 // 2 GiB

// warnLowDataDirSpace logs a prominent warning when the filesystem holding the
// data directory has less free space than minDataDirFreeBytes. It is a
// best-effort startup snapshot, not a hard gate: a statfs quirk or an unusual
// filesystem must not prevent startup, so a lookup failure only logs a warning.
func warnLowDataDirSpace(dataDir string) {
	free, err := handlers.FreeSpaceBytes(dataDir)
	if err != nil {
		log.Printf("Warning: cannot determine free space for data-dir %s: %v", dataDir, err)
		return
	}
	if free < minDataDirFreeBytes {
		log.Printf("WARNING: data-dir %s has only %.1f GiB free (below %.1f GiB); large uploads may fail with ENOSPC.",
			dataDir, float64(free)/(1<<30), float64(minDataDirFreeBytes)/(1<<30))
	}
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

// optionalBoolFlag is a boolean flag that records whether it was given:
// nil target means "not set", so Merge keeps the config-file or environment
// value. An explicit --rate-limit-enabled=false must disable the limiter even
// when the loaded configuration enabled it.
type optionalBoolFlag struct {
	target **bool
}

func (f optionalBoolFlag) String() string {
	if f.target == nil || *f.target == nil {
		return ""
	}
	return strconv.FormatBool(**f.target)
}

func (f optionalBoolFlag) Set(value string) error {
	v, err := strconv.ParseBool(value)
	if err != nil {
		return err
	}
	*f.target = &v
	return nil
}

// IsBoolFlag lets the flag package accept the bare form and the =value form.
func (optionalBoolFlag) IsBoolFlag() bool { return true }

// parseFlags parses command-line flags
func parseFlags() *config.Flags {
	flags := &config.Flags{}

	flag.StringVar(&flags.Port, "port", "", "Server port (default: 8080)")
	flag.StringVar(&flags.DataDir, "data-dir", "", "Data directory (default: ./data)")
	flag.StringVar(&flags.MultipartTmpDir, "multipart-tmp-dir", "", "Directory for multipart upload temp files (default: --data-dir)")
	// A pointer keeps 0 expressible (disable the busy wait entirely) — the
	// zero value would otherwise read as "unset" and keep the 5s default.
	flag.Func("busy-timeout-ms", "SQLite busy timeout in ms before a concurrent write fails with SQLITE_BUSY; 0 disables the wait (default: 5000)", func(s string) error {
		n, err := strconv.Atoi(s)
		if err != nil || n < 0 {
			return fmt.Errorf("invalid busy-timeout-ms %q: must be a non-negative integer", s)
		}
		flags.BusyTimeoutMS = &n
		return nil
	})
	flag.StringVar(&flags.Version, "version", "", "Server version")
	flag.StringVar(&flags.Config, "config", "", "Path to configuration file (JSON)")

	// Worker management flags
	flag.StringVar(&flags.WorkerHeartbeatTimeout, "worker-heartbeat-timeout", "", "Timeout before marking worker offline (default: 90s)")
	flag.StringVar(&flags.WorkerOfflineThreshold, "worker-offline-threshold", "", "Duration after which offline workers are removed (default: 10m)")
	flag.StringVar(&flags.WorkerHealthCheckInterval, "worker-health-check-interval", "", "Interval for checking worker health (default: 30s)")
	flag.StringVar(&flags.InputFileTTL, "input-file-ttl", "", "Evict uploaded input blobs unused for this long; 0 disables cleanup (default: 24h)")
	flag.StringVar(&flags.InputFileCleanupInterval, "input-file-cleanup-interval", "", "Interval for sweeping expired input blobs (default: 10m)")

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
	flag.Var(optionalBoolFlag{target: &flags.RateLimitEnabled}, "rate-limit-enabled", "Enable per-client rate limiting (default: true)")
	flag.IntVar(&flags.MaxConcurrentJobsPerClient, "max-concurrent-jobs-per-client", 0, "Maximum concurrent jobs per client (default: 10)")
	flag.StringVar(&flags.RedisAddr, "redis-addr", "", "Redis address for distributed rate limiting (optional)")
	flag.StringVar(&flags.SharedFSRoots, "shared-fs-roots", "", "Comma-separated roots of the shared filesystem path space; workers must declare the same value (default: none, pass-through mode has no legal path)")

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
