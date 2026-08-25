package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/tsix404/rffmpeg/pkg/audit"
	"github.com/tsix404/rffmpeg/pkg/protocol"
	"github.com/tsix404/rffmpeg/pkg/worker/gpu"
)

// cacheI is the subset of the disk cache the worker job loop uses. It exists
// so tests can inject a fake (the concrete *Cache satisfies it).
type cacheI interface {
	Check(key string) (string, bool)
	Put(key string, sourcePath string, ttlOverride ...time.Duration) error
	ConfirmHit(key string)
	Cfg() CacheConfig
	Stats() CacheStats
	Start(ctx context.Context)
	Stop()
}

// Worker is the main worker struct that handles job processing
type Worker struct {
	id                 string
	name               string
	caps               protocol.WorkerCapabilities // Stored for re-registration after server restart
	client             *Client
	executor           *Executor
	retryExecutor      *RetryExecutor
	rewriteAdapter     *RewriteAdapter
	cache              cacheI
	tempDir            string
	mu                 sync.Mutex
	activeJobs         map[string]context.CancelFunc
	heartbeatInterval  time.Duration
	pollInterval       time.Duration
	lastHeartbeatTime  time.Time
	jobsCompleted      int
	totalJobsCompleted int
	ffprobeExecutor    *FFprobeExecutor
	pixelFormatChecker *PixelFormatChecker
	auditRecorder      audit.AuditRecorder
	auditNotifier      audit.Notifier
	gpuDetector        *gpu.Detector

	// regMu guards id/client.workerID updates and serializes re-registration:
	// poll failures and heartbeat failures can both trigger reregister()
	// concurrently; without serialization the ID swap races with readers and
	// an in-flight job can end up reported under two identities.
	regMu        sync.Mutex
	reregisterMu sync.Mutex // held while a re-registration is in flight (singleflight)
	registering  bool       // guarded by reregisterMu

	// stopCh is closed by Stop() to break the Start loop; jobsWG tracks
	// in-flight jobs so Stop can wait for their upload/cleanup to finish.
	stopCh chan struct{}
	jobsWG sync.WaitGroup
}

// Config holds worker configuration
type Config struct {
	ServerURL         string
	WorkerID          string
	Name              string
	Token             string // Auth token for server communication
	TempDir           string
	FFmpegPath        string
	Timeout           time.Duration
	HeartbeatInterval time.Duration // Interval between heartbeats
	PollInterval      time.Duration // Interval for polling jobs
	CacheConfig       CacheConfig   // Cache configuration
	RetryConfig       *RetryConfig  // Retry configuration
}

// New creates a new worker
func New(cfg Config) (*Worker, error) {
	if cfg.WorkerID == "" {
		cfg.WorkerID = uuid.New().String()
	}
	if cfg.TempDir == "" {
		cfg.TempDir = filepath.Join(os.TempDir(), "rffmpeg-worker", cfg.WorkerID)
	}
	if cfg.Name == "" {
		cfg.Name = fmt.Sprintf("worker-%s", cfg.WorkerID[:8])
	}
	if cfg.HeartbeatInterval == 0 {
		cfg.HeartbeatInterval = 30 * time.Second // Default heartbeat interval
	}
	if cfg.PollInterval == 0 {
		cfg.PollInterval = 5 * time.Second // Default poll interval
	}

	// Create temp directory
	if err := os.MkdirAll(cfg.TempDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create temp directory: %w", err)
	}

	client := NewClient(cfg.ServerURL, cfg.WorkerID, cfg.Token)
	executor := NewExecutor(cfg.FFmpegPath, cfg.Timeout)
	rewriteAdapter := NewRewriteAdapter()

	// Initialize retry executor
	if cfg.RetryConfig == nil {
		cfg.RetryConfig = DefaultRetryConfig()
	}
	retryExecutor := NewRetryExecutor(executor, cfg.RetryConfig)

	// Initialize cache
	cache, err := NewCache(cfg.CacheConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize cache: %w", err)
	}

	// Initialize audit recorder and notifier for rewrite operation tracking
	auditNotifier := audit.NewStderrNotifier()
	auditRecorder := audit.NewRingBufferRecorder(500)

	// Initialize the pixel format checker for VAAPI pre-flight compatibility
	// checks (restored: a refactor accidentally dropped this and silently
	// disabled yuv444p-style fallback for VAAPI encoders).
	ffprobeExecutor := NewFFprobeExecutor("")
	pixelFormatChecker := NewPixelFormatChecker(ffprobeExecutor)

	return &Worker{
		id:                 cfg.WorkerID,
		name:               cfg.Name,
		client:             client,
		executor:           executor,
		retryExecutor:      retryExecutor,
		rewriteAdapter:     rewriteAdapter,
		cache:              cache,
		tempDir:            cfg.TempDir,
		activeJobs:         make(map[string]context.CancelFunc),
		heartbeatInterval:  cfg.HeartbeatInterval,
		pollInterval:       cfg.PollInterval,
		lastHeartbeatTime:  time.Now(),
		ffprobeExecutor:    ffprobeExecutor,
		pixelFormatChecker: pixelFormatChecker,
		auditRecorder:      auditRecorder,
		auditNotifier:      auditNotifier,
		gpuDetector:        gpu.NewDetector(),
		stopCh:             make(chan struct{}),
	}, nil
}

// ID returns the worker ID
func (w *Worker) ID() string {
	w.regMu.Lock()
	defer w.regMu.Unlock()
	return w.id
}

// Register registers the worker with the server.
// Performs initial registration and stores capabilities for later re-registration
// after server restart (TSI-1737 recovery).
func (w *Worker) Register(caps protocol.WorkerCapabilities) error {
	return w.register(caps, w.totalJobsCompleted == 0 && len(w.activeJobs) == 0)
}

// register performs the registration. firstRegistration controls whether the
// per-registration counters are reset: a fresh process must reset them so the
// server-side warmup check (CompletedJobs < MinJobsForEviction) applies to
// this registration, but an automatic re-register after a transient server
// restart must NOT zero them — resetting dropped the worker out of the
// eviction median sample pool and re-opened the cold-start window.
func (w *Worker) register(caps protocol.WorkerCapabilities, firstRegistration bool) error {
	// Store capabilities for re-registration
	w.caps = caps

	w.regMu.Lock()
	defer w.regMu.Unlock()

	workerID, err := w.client.Register(w.name, caps)
	if err != nil {
		return fmt.Errorf("failed to register: %w", err)
	}
	w.id = workerID
	w.client.SetWorkerID(workerID)

	w.mu.Lock()
	if firstRegistration {
		// TSI-2365: reset the lifetime count only on FIRST registration.
		// On server-restart re-registrations totalJobsCompleted must survive:
		// it feeds the server-side warmup/median sample pool — zeroing it on
		// every re-registration would drop the worker out of slow-node
		// detection forever in restart-heavy fleets. (Master's firstRegistration
		// guard already encodes this distinction; keep it.)
		w.jobsCompleted = 0
		w.totalJobsCompleted = 0
	} else {
		w.jobsCompleted = 0
	}
	// lastHeartbeatTime is always reset, so the next heartbeat reports
	// throughput over a full interval instead of the pre-(re)start window.
	w.lastHeartbeatTime = time.Now()
	w.mu.Unlock()

	log.Printf("Worker registered with ID: %s", workerID)

	// Set hardware capabilities on the rewrite adapter
	w.rewriteAdapter.SetHardwareCapabilities(&caps)

	return nil
}

// reregister attempts to re-register with the server after detecting that the
// worker record was lost (e.g., after server restart cleared all workers).
// Returns true if re-registration succeeded.
//
// Singleflight: poll and heartbeat failures can both trigger re-registration
// concurrently. The first caller performs it; later callers arriving while a
// registration is already in flight wait for its outcome instead of issuing
// duplicate registrations.
func (w *Worker) reregister() bool {
	w.reregisterMu.Lock()
	// Reuse an in-flight registration if one is running.
	if w.registering {
		w.reregisterMu.Unlock()
		// Wait for the in-flight registration to finish, then report
		// success based on whether it changed our identity successfully.
		w.regMu.Lock()
		registered := w.id != ""
		w.regMu.Unlock()
		return registered
	}
	w.registering = true
	w.reregisterMu.Unlock()

	defer func() {
		w.reregisterMu.Lock()
		w.registering = false
		w.reregisterMu.Unlock()
	}()

	log.Printf("Attempting re-registration with server...")
	if err := w.register(w.caps, false); err != nil {
		log.Printf("Re-registration failed: %v", err)
		return false
	}
	w.regMu.Lock()
	currentID := w.id
	w.regMu.Unlock()
	log.Printf("Re-registration successful, worker ID: %s", currentID)
	return true
}

// Start starts the worker loop
func (w *Worker) Start(ctx context.Context) {
	// Start cache background eviction
	w.cache.Start(ctx)

	ticker := time.NewTicker(w.pollInterval)
	defer ticker.Stop()

	heartbeatTicker := time.NewTicker(w.heartbeatInterval)
	defer heartbeatTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Println("Worker shutting down (context cancelled)...")
			return
		case <-w.stopCh:
			log.Println("Worker shutting down (Stop called)...")
			return
		case <-ticker.C:
			// Do not accept new jobs after Stop was requested.
			select {
			case <-w.stopCh:
				return
			default:
			}
			w.pollAndProcess(ctx)
		case <-heartbeatTicker.C:
			w.sendHeartbeat()
		}
	}
}

// Stop stops the worker: it breaks the Start loop, cancels all in-flight jobs,
// and waits for them to finish their upload/cleanup before returning.
func (w *Worker) Stop() {
	select {
	case <-w.stopCh:
		// Already stopped
	default:
		close(w.stopCh)
	}

	w.mu.Lock()
	for jobID, cancel := range w.activeJobs {
		log.Printf("Cancelling job %s", jobID)
		cancel()
	}
	w.mu.Unlock()

	// Wait for in-flight jobs to complete reporting/upload/cleanup.
	w.jobsWG.Wait()

	// Stop cache background eviction
	w.cache.Stop()
}

// pollAndProcess polls for jobs and processes them
func (w *Worker) pollAndProcess(ctx context.Context) {
	jobs, err := w.client.PullJobs()
	if err != nil {
		log.Printf("Failed to pull jobs: %v", err)
		// Attempt re-registration if server may have restarted
		w.reregister()
		return
	}

	for _, job := range jobs {
		// Do not start new work once Stop was requested.
		select {
		case <-w.stopCh:
			return
		default:
		}

		// Start processing in a goroutine with atomic check-and-add to prevent race condition
		jobCtx, cancel := context.WithCancel(ctx)

		w.mu.Lock()
		if _, exists := w.activeJobs[job.ID]; exists {
			w.mu.Unlock()
			cancel() // Clean up the unused context
			continue // Already processing this job
		}
		w.activeJobs[job.ID] = cancel
		w.mu.Unlock()

		w.jobsWG.Add(1)
		go func(job protocol.JobInfo, jobCtx context.Context, cancel context.CancelFunc) {
			w.processJob(jobCtx, job, cancel, true)
		}(job, jobCtx, cancel)
	}
}

// batcherCreateHook, when non-nil, receives every StderrBatcher created on
// the cache-hit path of processJob. Nil in production; tests use it to
// observe batcher lifecycle (TSI-2415 leak regression).
var batcherCreateHook func(*StderrBatcher)

// processJob processes a single job
func (w *Worker) processJob(ctx context.Context, job protocol.JobInfo, cancel context.CancelFunc, wgTracked bool) {
	jobFailed := false
	defer func() {
		// Recover from panics so a single bad job cannot take down the whole
		// worker process; the job is reported as WORKER_CRASH and the worker
		// keeps serving other in-flight jobs.
		if r := recover(); r != nil {
			log.Printf("Job %s: panic recovered: %v\n%s", job.ID, r, debug.Stack())
			w.reportFailureWithType(job.ID, -1,
				fmt.Sprintf("worker panic during job processing: %v", r),
				string(protocol.FailureWorkerCrash),
				fmt.Sprintf("%v", r))
		}

		w.mu.Lock()
		delete(w.activeJobs, job.ID)
		// Only successful jobs advance the completion counters. Counting
		// failures/cancels/probes inflated completed_jobs and let workers
		// pass the MinJobsForEviction warmup gate without real work.
		if !jobFailed {
			w.jobsCompleted++
			w.totalJobsCompleted++
		}
		becameIdle := len(w.activeJobs) == 0
		w.mu.Unlock()

		// Immediately notify server when worker becomes idle so the scheduler
		// can assign new jobs without waiting for the next heartbeat cycle.
		if becameIdle {
			w.sendHeartbeat()
		}
		// Signal Stop (if waiting) that this job has fully finished. Only
		// balance the counter when pollAndProcess incremented it (direct
		// callers of processJob in tests pass wgTracked=false).
		if wgTracked {
			w.jobsWG.Done()
		}
	}()

	// Detect probe jobs: args[0] == "__rffmpeg_probe__". Probes are health
	// checks, not transcode work — they must not advance the completion
	// counters, or pure probe traffic would push a worker past the server-side
	// MinJobsForEviction warmup gate without doing real work.
	if len(job.Args) > 0 && job.Args[0] == "__rffmpeg_probe__" {
		jobFailed = true // suppress counter increment in the deferred hook
		// Probe jobs are not transcodes: counting them into completed_jobs
		// floods the warmup window and skews the median sample pool (TSI-2365).
		w.processProbeJob(ctx, job)
		return
	}

	log.Printf("Processing job %s (auto_hw=%v, streaming_output=%v)", job.ID, job.AutoHW, job.StreamingOutput)

	// Apply per-job timeout override if specified
	jobCtx := ctx
	var jobCancel context.CancelFunc
	if job.Timeout != nil && !job.Timeout.IsZero() {
		timeoutDur := time.Until(*job.Timeout)
		if timeoutDur > 0 {
			jobCtx, jobCancel = context.WithTimeout(ctx, timeoutDur)
			defer jobCancel()
			log.Printf("Job %s: using per-job timeout %v", job.ID, timeoutDur)
		}
	}

	// Check for direct paths (shared FS mode)
	directMode := len(job.DirectPaths) > 0
	if directMode {
		// Validate direct paths: reject path traversal
		for _, path := range job.DirectPaths {
			if containsPathTraversal(path) {
				w.reportFailureWithType(job.ID, 1,
					fmt.Sprintf("direct path contains '..' traversal: %s", path),
					string(protocol.FailureInputUnreachable),
					"path traversal rejected")
				return
			}
			if _, err := os.Stat(path); err != nil {
				w.reportFailureWithType(job.ID, 1,
					fmt.Sprintf("input path unreachable: %s: %v", path, err),
					string(protocol.FailureInputUnreachable),
					err.Error())
				return
			}
		}
		log.Printf("Job %s: direct paths mode, %d input(s)", job.ID, len(job.DirectPaths))
	}

	// Compute cache key from input files + original args (before rewrite).
	// auto_hw MUST be part of the key: a --auto-hw run may upgrade the encoder
	// (e.g. libx264 → h264_qsv), and that output must never be served to
	// requests without --auto-hw (TSI-2352).
	cacheKey := GenerateCacheKey(job.InputFiles, job.Args, job.AutoHW, filepath.Ext(job.OutputFilename), "")
	cached := false

	// Check if output is a network URL (rtmp://, udp://, etc.)
	// Cache is not applicable for streaming/network outputs
	isNetOutput := isRemoteURL(job.OutputFilename)

	// Check cache before any work (skip for direct mode, network outputs, and
	// streaming outputs — stdout has no local file to cache)
	if !directMode && !isNetOutput && !job.StreamingOutput {
		if cachePath, ok := w.cache.Check(cacheKey); ok {
			log.Printf("Job %s: cache HIT (key=%s)", job.ID, cacheKey[:16])

			// Stream the cache-hit notice through the job's stderr so CLI
			// clients observe it, not only this worker process' own log
			// (TSI-2349 precedent). Deferred Close guarantees the flush
			// timer is stopped on every exit path — including the early
			// return below when MkdirAll fails — otherwise timedFlush
			// re-arms itself forever and leaks the timer/goroutine.
			batcher := NewStderrBatcher(job.ID, w.client, DefaultStderrBatcherConfig())
			if batcherCreateHook != nil {
				batcherCreateHook(batcher)
			}
			defer batcher.Close()
			batcher.Add(fmt.Sprintf("[rffmpeg] Job %s: cache HIT (key=%s)\n", job.ID, cacheKey[:16]))

			// Create job-specific temp directory
			jobDir := filepath.Join(w.tempDir, job.ID)
			if err := os.MkdirAll(jobDir, 0755); err != nil {
				w.reportInfraFailure(job.ID, 1, fmt.Sprintf("Failed to create job directory: %v", err))
				return
			}
			defer w.cleanupJobDir(jobDir)

			// Copy cached file to output path
			outputFilename := job.OutputFilename
			if outputFilename == "" {
				outputFilename = "output"
			}
			var outputPath string
			if filepath.IsAbs(outputFilename) {
				outputPath = outputFilename
			} else {
				outputPath = filepath.Join(jobDir, outputFilename)
			}

			if err := copyFile(cachePath, outputPath); err != nil {
				log.Printf("Job %s: failed to copy cached file: %v", job.ID, err)
				batcher.Add(fmt.Sprintf("[rffmpeg] Job %s: cached output copy failed (%v), re-transcoding\n", job.ID, err))
				// Fall through to normal processing
			} else if _, err := os.Stat(outputPath); err != nil {
				// Copy claimed success but the file is not there — degrade to
				// a cache miss instead of reporting Completed without output.
				log.Printf("Job %s: cached file missing after copy (%v), falling through to normal processing", job.ID, err)
				batcher.Add(fmt.Sprintf("[rffmpeg] Job %s: cached file missing after copy (%v), re-transcoding\n", job.ID, err))
			} else if err := w.client.UploadOutput(job.ID, outputPath); err != nil {
				// Upload failure is recoverable by re-transcoding — fall
				// through to normal processing instead of failing the job.
				log.Printf("Job %s: failed to upload cached output (%v), falling through to normal processing", job.ID, err)
				batcher.Add(fmt.Sprintf("[rffmpeg] Job %s: cached output upload failed (%v), re-transcoding\n", job.ID, err))
				cached = false
			} else {
				// Confirm the cache hit (stats + LRU touch)
				w.cache.ConfirmHit(cacheKey)
				log.Printf("Uploaded cached output for job %s", job.ID)
				cached = true
				// Deliver the notice before reporting completion so CLI
				// clients always see it ahead of the terminal status.
				batcher.Close()

				// Report success with cached flag
				if err := w.client.UpdateJob(job.ID, protocol.JobStatusCompleted, 0, "", cached); err != nil {
					log.Printf("Failed to update job status to completed: %v", err)
				} else {
					log.Printf("Job %s completed from cache", job.ID)
				}
				return
			}
		}
	} else {
		log.Printf("Job %s: cache bypassed (direct paths mode)", job.ID)
	}

	// Cache miss — proceed with normal processing
	if !directMode {
		log.Printf("Job %s: cache MISS (key=%s)", job.ID, cacheKey[:16])
	}

	// Create job-specific temp directory
	jobDir := filepath.Join(w.tempDir, job.ID)
	if err := os.MkdirAll(jobDir, 0755); err != nil {
		w.reportInfraFailure(job.ID, 1, fmt.Sprintf("Failed to create job directory: %v", err))
		return
	}
	defer w.cleanupJobDir(jobDir)

	// Update job status to running
	if err := w.client.UpdateJob(job.ID, protocol.JobStatusRunning, 0, "", false); err != nil {
		log.Printf("Failed to update job status to running: %v", err)
		w.reportInfraFailure(job.ID, 1, fmt.Sprintf("Failed to update job status to running: %v", err))
		return
	}

	// Download input files (skip in direct mode)
	inputPaths := make([]string, 0, len(job.InputFiles))
	if directMode {
		inputPaths = job.DirectPaths
	} else {
		for _, fileID := range job.InputFiles {
			// Generate a safe filename for the downloaded file.
			// For remote URLs (http://, https://, etc.), extract the base filename
			// from the URL path. For server file IDs, use the fileID directly.
			var inputPath string
			if isRemoteURL(fileID) {
				// Remote URL: sanitize the last path segment — hostile URLs
				// must not inject path separators or oversized names.
				baseName := sanitizeInputBaseName(filepath.Base(fileID))
				inputPath = filepath.Join(jobDir, "input-"+baseName)
			} else {
				inputPath = filepath.Join(jobDir, "input-"+fileID)
			}
			if err := w.client.DownloadInput(fileID, inputPath); err != nil {
				w.reportInputDownloadFailure(job.ID, fileID, fmt.Sprintf("Failed to download input file %s: %v", fileID, err))
				return
			}
			inputPaths = append(inputPaths, inputPath)
			log.Printf("Downloaded input file %s to %s", fileID, inputPath)
		}
	}
	// Prepare output path.
	// Streaming jobs must write to stdout ("-") so ffmpeg's transcoded data
	// reaches the stdoutBatcher; defaulting an empty filename to a regular
	// file makes ffmpeg produce a local file and the CLI receive nothing.
	outputFilename := job.OutputFilename
	if job.StreamingOutput {
		if outputFilename == "" || isRemoteURL(outputFilename) {
			outputFilename = "-"
		}
	} else if outputFilename == "" {
		outputFilename = "output"
	}
	var outputPath string
	if outputFilename == "-" || isRemoteURL(outputFilename) {
		// ffmpeg stdout ("-") or network URL (rtmp://, udp://, etc.) — pass directly
		outputPath = outputFilename
	} else if filepath.IsAbs(outputFilename) {
		outputPath = outputFilename
	} else {
		outputPath = filepath.Join(jobDir, outputFilename)
	}

	// Build initial args
	args := BuildArgs(job.Args, inputPaths, outputPath)

	// Apply encoder rewrite based on job.AutoHW flag. The flag is passed per
	// RewriteArgs call — the shared adapter config must not be mutated per job
	// because concurrent jobs would clobber each other's auto-hw decision.

	// Create stderr batcher early to reduce HTTP requests.
	// Must be created before rewrite so notifications can be sent even when
	// rewrite returns an error (e.g., nonexistent codec).
	batcher := NewStderrBatcher(job.ID, w.client, DefaultStderrBatcherConfig())
	defer batcher.Close()

	// Create progress router to parse stderr for progress/ETA and send periodic updates
	progressRouter := NewProgressRouter(w.client, job.ID, batcher.StderrHandler())
	defer progressRouter.Reset()

	// Rewrite args based on hardware capabilities and auto_hw setting
	rewrittenArgs, rewriteResult, err := w.rewriteAdapter.RewriteArgs(jobCtx, args, job.AutoHW)

	// Send rewrite/fallback notifications to stderr so they appear in job output.
	// This must happen BEFORE ffmpeg execution AND before error reporting,
	// ensuring the rewrite log line is consistently visible to users
	// and not skipped due to codec lookup failure.
	if rewriteResult != nil {
		stderrHandler := progressRouter.Handler()
		for _, notification := range rewriteResult.Notifications {
			log.Printf("[Rewrite] %s", notification)
			stderrHandler(notification + "\n")
		}

		// Emit the detailed audit chain line through the job's stderr
		// batcher so it streams to CLI clients over the WebSocket stderr
		// channel (TSI-2349). The shared audit notifier only writes to the
		// worker process stderr, which CLI users never see, so the same
		// formatted line is duplicated here onto the per-job stream.
		// Format: [rffmpeg] INFO: Worker capabilities: <caps> | Requested: <orig> | Rewritten: <target> | Reason: <reason>
		if rewriteResult.Performed {
			requested := rewriteResult.OriginalEncoder
			if requested == "" {
				requested = "(none)"
			}
			rewritten := rewriteResult.TargetEncoder
			reason := rewriteResult.DecisionReason
			if reason == "" {
				reason = rewriteResult.Scenario
			}

			// Record audit operation — a failed Record is logged (audit trail
			// gaps must be visible), never silently discarded (TSI-2365).
			if w.auditRecorder != nil {
				if err := w.auditRecorder.Record(audit.AuditOperation{
					RequestID:           job.ID,
					OriginalEncoder:     rewriteResult.OriginalEncoder,
					RewrittenEncoder:    rewriteResult.TargetEncoder,
					DecisionReason:      rewriteResult.DecisionReason,
					CapabilitiesSummary: rewriteResult.CapabilitiesSummary,
				}); err != nil {
					log.Printf("Job %s: audit record failed: %v", job.ID, err)
				}
			}

			// Notify the detailed chain on both sinks:
			// - auditNotifier for operator visibility in worker logs
			// - stderrHandler for streaming to connected CLI clients
			if w.auditNotifier != nil {
				_ = w.auditNotifier.NotifyRewriteChain(
					rewriteResult.CapabilitiesSummary,
					requested,
					rewritten,
					reason,
					audit.InfoLevel,
				)
			}
			stderrHandler(audit.FormatRewriteChainLine(
				rewriteResult.CapabilitiesSummary,
				requested,
				rewritten,
				reason,
				audit.InfoLevel,
			))
		}
	}

	if err != nil {
		log.Printf("Rewrite error for job %s: %v", job.ID, err)
		// Fail the job if rewrite returns an error (e.g., format not available)
		jobFailed = true
		w.reportFailure(job.ID, 1, fmt.Sprintf("Encoder rewrite failed: %v", err), false)
		return
	}
	args = rewrittenArgs
	// Streaming (stdout) output cannot be seeked by muxers like mp4/mov.
	// Inject fragmented-output flags so the requested container still works
	// instead of failing inside ffmpeg with "muxer does not support non
	// seekable output" (TSI-2409). Runs after rewrite so the final args are
	// what gets adjusted.
	if job.StreamingOutput {
		var note string
		args, note = applyStreamableFormat(args, outputPath)
		if note != "" {
			log.Printf("Job %s: %s", job.ID, note)
			progressRouter.Handler()(fmt.Sprintf("[rffmpeg] %s\n", note))
		}
	}

	// Recompute cache key with the actual encoder from the rewrite result
	// This ensures encoder-specific outputs are keyed correctly.
	if rewriteResult != nil && rewriteResult.Performed && rewriteResult.TargetEncoder != "" {
		cacheKey = GenerateCacheKey(job.InputFiles, job.Args, job.AutoHW, filepath.Ext(job.OutputFilename), rewriteResult.TargetEncoder)
	}

	// Pre-flight pixel format check for VAAPI encoders
	// VAAPI has limited support for yuv444p and other high-chroma pixel formats
	// Probe input to detect incompatible pixel format and proactively fallback
	encoder := extractEncoderFromArgs(args)
	if w.pixelFormatChecker != nil && isVAAPIEncoder(encoder) {
		pixelFormatResult := w.pixelFormatChecker.CheckInputsForPixelFormatIncompatibility(jobCtx, inputPaths, encoder)
		if pixelFormatResult != nil && pixelFormatResult.NeedsFallback {
			log.Printf("[PixelFormat] Input has pixel format %s incompatible with %s, falling back to %s",
				pixelFormatResult.PixelFormat, encoder, pixelFormatResult.RecommendedEncoder)
			stderrHandler := progressRouter.Handler()
			stderrHandler(fmt.Sprintf("[rffmpeg] %s\n", pixelFormatResult.Reason))

			// Apply software encoder fallback
			fallback := NewEncoderFallback()
			fallbackArgs := fallback.PrepareFallbackArgsWithSource(args, outputPath, true) // true = user-selected
			if fallbackArgs != nil {
				args = fallbackArgs
				encoder = extractEncoderFromArgs(args)
				// Recompute cache key with the new encoder from pixel format fallback
				cacheKey = GenerateCacheKey(job.InputFiles, job.Args, job.AutoHW, filepath.Ext(job.OutputFilename), encoder)
				log.Printf("[PixelFormat] Using software encoder: %s", encoder)
				stderrHandler(fmt.Sprintf("[rffmpeg] fallback to software encoder %s\n", encoder))
			}
		}
	}

	log.Printf("Executing ffmpeg with args: %v", args)

	// Execute with appropriate handlers
	var result ExecResult
	var stdoutHandler StdoutHandler
	if job.StreamingOutput {
		// Streaming output mode: stdout goes to WebSocket. The batcher handler
		// is also passed to ExecuteWithRetry so retried attempts keep streaming.
		stdoutBatcher := NewStdoutBatcher(job.ID, w.client, DefaultStdoutBatcherConfig())
		defer stdoutBatcher.Close()

		stdoutHandler = stdoutBatcher.StdoutHandler()
		result = w.executor.ExecuteWithHandlers(jobCtx, args, stdoutHandler, progressRouter.Handler())
	} else {
		// Normal mode: stdout goes to file
		result = w.executor.ExecuteWithStderrHandler(jobCtx, args, progressRouter.Handler())
	}

	// Check for context cancellation
	if jobCtx.Err() == context.Canceled {
		log.Printf("Job %s was cancelled", job.ID)
		// Use -1 for cancelled jobs as the exit code may not be meaningful
		w.client.UpdateJob(job.ID, protocol.JobStatusCancelled, -1, "Job cancelled", false)
		return
	}

	// Check for timeout
	if result.IsTimeout {
		log.Printf("Job %s timed out", job.ID)
		errMsg := ""
		if result.Error != nil {
			errMsg = result.Error.Error()
		}
		failureType, details := ClassifyFailure(result.ExitCode, result.Stderr, errMsg, true, false)
		if err := w.client.UpdateJobWithFailure(job.ID, protocol.JobStatusTimeout, result.ExitCode, errMsg, false, string(failureType), details); err != nil {
			log.Printf("Failed to report job timeout: %v", err)
		}
		return
	}

	// Determine if output is a network URL (RTMP, RTSP, UDP, etc.).
	// Network outputs do not produce local files, so file existence/size
	// validation must be skipped to avoid false output_empty classification.
	networkOutput := isNetworkOutput(outputFilename, args)

	// Validate output file for non-streaming jobs: FFmpeg may exit 0 but
	// produce a 0-byte output (e.g., VAAPI hw encoder falls back to a64multi
	// codec which cannot be muxed, resulting in silent failure).
	// Skip for network outputs (RTMP, RTSP, UDP, etc.) — no local file is produced.
	if !networkOutput && !job.StreamingOutput && result.Error == nil && result.ExitCode == 0 {
		if info, err := os.Stat(outputPath); err != nil {
			log.Printf("Job %s: output file not found after exit code 0: %s: %v", job.ID, outputPath, err)
			result.Error = fmt.Errorf("output file not found: %s", outputPath)
			result.Stderr = "Output file not found: " + outputPath + "\n" + result.Stderr
		} else if info.Size() == 0 {
			log.Printf("Job %s: output file is 0 bytes after exit code 0: %s", job.ID, outputPath)
			result.Error = fmt.Errorf("output file is empty (0 bytes): %s", outputPath)
			result.Stderr = "Output file is empty (0 bytes): " + outputPath + "\n" + result.Stderr
		}
	}

	// Validate ffmpeg stderr for empty-output reports even when output file
	// exists and has non-zero size (e.g., when -ss exceeds file duration,
	// ffmpeg exits 0 but the output contains only container headers).
	// Skip for network outputs — ffmpeg stderr may report "empty output" for streams.
	if !networkOutput && !job.StreamingOutput && result.Error == nil && result.ExitCode == 0 {
		if ffmpegStderrIndicatesEmptyOutput(result.Stderr) {
			log.Printf("Job %s: ffmpeg reported empty output (exit code 0): %s", job.ID, outputPath)
			result.Error = fmt.Errorf("ffmpeg produced empty output: %s", outputPath)
			result.ExitCode = 1
		}
	}

	// Validate ffmpeg stderr for critical errors that indicate failed transcoding
	// even when ffmpeg exits with code 0 (e.g., corrupted input files).
	if result.Error == nil && result.ExitCode == 0 {
		if ffmpegStderrIndicatesCriticalError(result.Stderr) {
			log.Printf("Job %s: ffmpeg reported critical error in stderr (exit code 0)", job.ID)
			result.Error = fmt.Errorf("ffmpeg reported critical error: corrupted or invalid input data")
			result.ExitCode = 1
		}
	}

	// Handle execution result
	if result.Error != nil {
		log.Printf("Job %s failed: %v", job.ID, result.Error)

		// Check if this is a retryable error that warrants multi-stage retry
		interceptor := NewErrorInterceptor()
		intercepted := interceptor.Intercept(jobCtx, result)

		if intercepted.FFmpegError != nil && intercepted.FFmpegError.IsRetryable() {
			// Skip retry for output_empty errors on network outputs.
			// Network outputs (RTMP, RTSP, UDP, etc.) do not produce local files,
			// so output_empty is a false classification — retrying won't help.
			if networkOutput && intercepted.FFmpegError.Type == ErrorTypeOutputEmpty {
				log.Printf("Job %s: network output detected, skipping output_empty retry", job.ID)
				jobFailed = true
				w.reportFailure(job.ID, result.ExitCode, result.Stderr, false)
				return
			}

			// Notify about retry attempt
			notification := fmt.Sprintf("[RETRY] Initial attempt failed (%s), starting multi-stage retry", intercepted.FFmpegError.Type)
			log.Printf("%s", notification)
			progressRouter.Handler()(notification + "\n")

			// Use RetryExecutor for multi-stage retry:
			retryResult := w.retryExecutor.ExecuteWithRetry(jobCtx, args, outputPath, networkOutput, stdoutHandler)

			if retryResult.Success {
				// Retry succeeded — use the final result
				result = retryResult.FinalResult

				// Log audit trail
				for _, entry := range retryResult.AuditTrail {
					if entry.Success {
						log.Printf("Job %s: Retry stage %s (attempt %d) succeeded with encoder %s",
							job.ID, entry.Stage, entry.AttemptNumber, entry.Encoder)
						progressRouter.Handler()(fmt.Sprintf("[RETRY] Stage %s succeeded with encoder %s\n",
							entry.Stage, entry.Encoder))
					}
				}

				if retryResult.UsedSoftwareEncoder {
					log.Printf("Job %s: Fallback to software encoder %s succeeded", job.ID, retryResult.FinalEncoder)
					progressRouter.Handler()(fmt.Sprintf("[rffmpeg] %s unavailable, fallback to %s\n",
						retryResult.OriginalEncoder, retryResult.FinalEncoder))
				}
				goto uploadOutput
			}
			// All retries exhausted — report failure with audit trail
			log.Printf("Job %s: All retry attempts exhausted (stages: %s)", job.ID, retryResult.FinalStage)
			errMsg := fmt.Sprintf("All retry attempts exhausted (original encoder: %s, final stage: %s): %s",
				retryResult.OriginalEncoder, retryResult.FinalStage, retryResult.FinalResult.Stderr)
			jobFailed = true
			w.reportFailure(job.ID, retryResult.FinalResult.ExitCode, errMsg, false)
			return
		}

		jobFailed = true
		w.reportFailure(job.ID, result.ExitCode, result.Stderr, false)
		return
	}

uploadOutput:

	// Upload output file (skip for streaming output, direct mode jobs, and network URL outputs)
	isNetOutput = isRemoteURL(outputPath)
	if !job.StreamingOutput && !directMode && !isNetOutput {
		if _, err := os.Stat(outputPath); err == nil {
			if err := w.client.UploadOutput(job.ID, outputPath); err != nil {
				jobFailed = true
				w.reportInfraFailure(job.ID, result.ExitCode, fmt.Sprintf("Failed to upload output: %v", err))
				return
			}
			log.Printf("Uploaded output file for job %s", job.ID)
		}
	} else if isNetOutput {
		log.Printf("Job %s: network output URL, upload skipped", job.ID)
	}
	if directMode {
		log.Printf("Job %s: direct mode — output written to local path, upload skipped", job.ID)
	}

	// Cache the output for future requests (skip for direct mode and network URL outputs)
	if !cached && !directMode && !isNetOutput {
		// Use shorter TTL for URL-based inputs (content may change without URL changing)
		cacheTTL := time.Duration(0)
		for _, input := range job.InputFiles {
			if isRemoteURL(input) {
				cacheTTL = w.cache.Cfg().URLTTL
				break
			}
		}
		if err := w.cache.Put(cacheKey, outputPath, cacheTTL); err != nil {
			log.Printf("Job %s: failed to cache output: %v", job.ID, err)
			// Non-fatal — job still succeeded
		} else {
			log.Printf("Job %s: cached output (key=%s)", job.ID, cacheKey[:16])
		}
	}

	// Report success
	if err := w.client.UpdateJob(job.ID, protocol.JobStatusCompleted, result.ExitCode, "", cached); err != nil {
		log.Printf("Failed to update job status to completed: %v", err)
	} else {
		log.Printf("Job %s completed successfully", job.ID)
	}
}

// reportFailure classifies an ffmpeg execution failure and reports it to the
// server. errMsg must be ffmpeg stderr or a Go error from running ffmpeg —
// never generic infrastructure text (see reportInfraFailure).
func (w *Worker) reportFailure(jobID string, exitCode int, errMsg string, cached bool) {
	failureType, failureDetails := ClassifyFailure(exitCode, errMsg, errMsg, false, false)
	if err := w.client.UpdateJobWithFailure(jobID, protocol.JobStatusFailed, exitCode, errMsg, cached, string(failureType), failureDetails); err != nil {
		log.Printf("Failed to report job failure: %v", err)
	}
}

// reportInfraFailure reports a worker-side infrastructure failure that happened
// outside ffmpeg execution (job directory creation, server communication,
// output upload, probe plumbing). Pattern-matching this text would misclassify
// it as INPUT_UNREACHABLE etc., so it uses the dedicated INFRA bucket —
// FFMPEG_ERROR is reserved for actual ffmpeg execution failures (TSI-2365).
func (w *Worker) reportInfraFailure(jobID string, exitCode int, errMsg string) {
	if err := w.client.UpdateJobWithFailure(jobID, protocol.JobStatusFailed, exitCode, errMsg, false, string(protocol.FailureInfra), errMsg); err != nil {
		log.Printf("Failed to report job failure: %v", err)
	}
}

// reportInputDownloadFailure reports a failure to fetch a job input file.
// Classification depends on the input kind: a remote URL that cannot be
// fetched is INPUT_UNREACHABLE; a server file ID failing over the
// worker↔server channel is infrastructure → FFMPEG_ERROR.
func (w *Worker) reportInputDownloadFailure(jobID string, fileID, errMsg string) {
	failureType := ClassifyInputDownloadFailure(fileID)
	if err := w.client.UpdateJobWithFailure(jobID, protocol.JobStatusFailed, 1, errMsg, false, string(failureType), errMsg); err != nil {
		log.Printf("Failed to report job failure: %v", err)
	}
}

// reportFailureWithType reports a job failure with failure type classification.
func (w *Worker) reportFailureWithType(jobID string, exitCode int, errMsg, failureType, failureDetails string) {
	if err := w.client.UpdateJobWithFailure(jobID, protocol.JobStatusFailed, exitCode, errMsg, false, failureType, failureDetails); err != nil {
		log.Printf("Failed to report job failure: %v", err)
	}
}

// processProbeJob handles a probe job (args[0] == "__rffmpeg_probe__").
// It downloads the input file, runs ffprobe, uploads the JSON result as output.
func (w *Worker) processProbeJob(ctx context.Context, job protocol.JobInfo) {
	log.Printf("Processing probe job %s", job.ID)

	// Create job-specific temp directory
	jobDir := filepath.Join(w.tempDir, job.ID)
	if err := os.MkdirAll(jobDir, 0755); err != nil {
		w.reportInfraFailure(job.ID, 1, fmt.Sprintf("Failed to create job directory: %v", err))
		return
	}
	defer w.cleanupJobDir(jobDir)

	// Update job status to running
	if err := w.client.UpdateJob(job.ID, protocol.JobStatusRunning, 0, "", false); err != nil {
		log.Printf("Failed to update probe job status to running: %v", err)
		return
	}

	// Download input files
	if len(job.InputFiles) == 0 {
		w.reportInfraFailure(job.ID, 1, "no input files for probe job")
		return
	}

	// Generate a safe filename for the downloaded probe input
	fileID := job.InputFiles[0]
	var inputPath string
	if isRemoteURL(fileID) {
		// Remote URL: sanitize the last path segment — hostile URLs
		// must not inject path separators or oversized names.
		baseName := sanitizeInputBaseName(filepath.Base(fileID))
		inputPath = filepath.Join(jobDir, "input-"+baseName)
	} else {
		inputPath = filepath.Join(jobDir, "input-"+fileID)
	}
	if err := w.client.DownloadInput(fileID, inputPath); err != nil {
		w.reportInputDownloadFailure(job.ID, fileID, fmt.Sprintf("Failed to download input file %s: %v", fileID, err))
		return
	}
	log.Printf("Downloaded probe input file %s to %s", fileID, inputPath)

	// Run ffprobe
	prober := NewFFprobeExecutor("")
	result, err := prober.Probe(ctx, inputPath)
	if err != nil {
		w.reportInfraFailure(job.ID, 1, fmt.Sprintf("ffprobe failed: %v", err))
		return
	}

	// Write probe result as JSON output
	outputPath := filepath.Join(jobDir, "probe_result.json")
	outputBytes, err := json.Marshal(result)
	if err != nil {
		w.reportInfraFailure(job.ID, 1, fmt.Sprintf("Failed to marshal probe result: %v", err))
		return
	}
	if err := os.WriteFile(outputPath, outputBytes, 0644); err != nil {
		w.reportInfraFailure(job.ID, 1, fmt.Sprintf("Failed to write probe result: %v", err))
		return
	}

	// Upload output file
	if err := w.client.UploadOutput(job.ID, outputPath); err != nil {
		w.reportInfraFailure(job.ID, 1, fmt.Sprintf("Failed to upload probe output: %v", err))
		return
	}
	log.Printf("Uploaded probe output for job %s", job.ID)

	// Report success
	if err := w.client.UpdateJob(job.ID, protocol.JobStatusCompleted, 0, "", false); err != nil {
		log.Printf("Failed to update probe job status to completed: %v", err)
	} else {
		log.Printf("Probe job %s completed successfully", job.ID)
	}
}

// sendHeartbeat sends a heartbeat to the server and handles cancelled jobs
func (w *Worker) sendHeartbeat() {
	w.mu.Lock()
	activeJobIDs := make([]string, 0, len(w.activeJobs))
	for jobID := range w.activeJobs {
		activeJobIDs = append(activeJobIDs, jobID)
	}
	status := protocol.WorkerStatusIdle
	if len(activeJobIDs) > 0 {
		status = protocol.WorkerStatusBusy
	}

	// Calculate throughput (jobs completed per second since last heartbeat)
	now := time.Now()
	elapsed := now.Sub(w.lastHeartbeatTime).Seconds()
	var throughputFPS float64
	if elapsed > 0 && w.jobsCompleted > 0 {
		throughputFPS = float64(w.jobsCompleted) / elapsed
	}
	completedJobs := w.totalJobsCompleted
	w.jobsCompleted = 0
	w.lastHeartbeatTime = now
	w.mu.Unlock()

	gpuMetrics := w.gpuDetector.SampleMetrics()
	cancelledJobs, err := w.client.Heartbeat(status, activeJobIDs, throughputFPS, completedJobs, gpuMetrics)
	if err != nil {
		log.Printf("Failed to send heartbeat: %v", err)
		// Attempt re-registration if server may have restarted
		w.reregister()
		return
	}

	// Cancel any jobs that were cancelled on the server
	if len(cancelledJobs) > 0 {
		w.mu.Lock()
		for _, jobID := range cancelledJobs {
			if cancel, exists := w.activeJobs[jobID]; exists {
				log.Printf("Cancelling job %s (cancelled on server)", jobID)
				cancel()
			}
		}
		w.mu.Unlock()
	}
}

// cleanupJobDir cleans up the job's temporary directory
func (w *Worker) cleanupJobDir(dir string) {
	if err := os.RemoveAll(dir); err != nil {
		log.Printf("Failed to cleanup job directory %s: %v", dir, err)
	}
}

// containsPathTraversal checks whether any component of a path is exactly
// ".." (path traversal attempt). Unlike a raw substring search, legitimate
// filenames such as "my..video.mp4" or "a..b/c.mp4" are not false-positived
// (TSI-2365).
func containsPathTraversal(path string) bool {
	for _, part := range strings.FieldsFunc(path, func(r rune) bool {
		return r == '/' || r == '\\'
	}) {
		if part == ".." {
			return true
		}
	}
	return false
}

// ffmpegStderrIndicatesEmptyOutput checks ffmpeg stderr for indicators that the
// output is empty/meaningless despite ffmpeg reporting exit code 0. This catches
// cases like -ss exceeding the input file duration where ffmpeg produces an
// output file with container headers but no actual media content.
func ffmpegStderrIndicatesEmptyOutput(stderr string) bool {
	// Case-insensitive check for common ffmpeg empty-output messages.
	stderrLower := strings.ToLower(stderr)
	emptyIndicators := []string{
		"output file is empty",
		"nothing was encoded",
		"does not contain any stream",
	}
	for _, indicator := range emptyIndicators {
		if strings.Contains(stderrLower, indicator) {
			return true
		}
	}
	return false
}

// ffmpegStderrIndicatesCriticalError checks ffmpeg stderr for critical errors
// that indicate a failed transcoding even when ffmpeg exits with code 0.
// This catches cases like corrupted input files where ffmpeg may exit 0
// but still report errors like "Invalid data" in stderr.
func ffmpegStderrIndicatesCriticalError(stderr string) bool {
	// Case-insensitive check for critical ffmpeg error messages.
	stderrLower := strings.ToLower(stderr)
	criticalErrorIndicators := []string{
		"invalid data found when reading input",
		"invalid data found",
		"invalid nal unit size",
		"header missing",
		"corrupted file",
		"corrupted input",
		"error while decoding",
		"decode_slice_header error",
		"concealing errors",
		"error while processing",
		"unable to decode",
		"decode failed",
		"error decoding",
		// Encoder-related errors that may occur with exit code 0
		"unknown encoder",
		"requested encoder",
		"no such encoder",
		"encoder not found",
	}
	for _, indicator := range criticalErrorIndicators {
		if strings.Contains(stderrLower, indicator) {
			return true
		}
	}
	return false
}
