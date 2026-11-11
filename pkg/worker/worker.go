package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"os"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/tsic404/rffmpeg/pkg/audit"
	"github.com/tsic404/rffmpeg/pkg/ffmpegopts"
	"github.com/tsic404/rffmpeg/pkg/jobargs"
	"github.com/tsic404/rffmpeg/pkg/pathutil"
	"github.com/tsic404/rffmpeg/pkg/protocol"
	"github.com/tsic404/rffmpeg/pkg/worker/gpu"
	"github.com/tsic404/rffmpeg/pkg/worker/workerconfig"
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
	id                string
	name              string
	caps              protocol.WorkerCapabilities // Stored for re-registration after server restart
	client            *Client
	executor          *Executor
	retryExecutor     *RetryExecutor
	rewriteAdapter    *RewriteAdapter
	cache             cacheI
	tempDir           string
	mu                sync.Mutex
	activeJobs        map[string]context.CancelFunc
	heartbeatInterval time.Duration
	pollInterval      time.Duration
	// streamingCancelPollInterval is how often a running streaming job queries
	// its own status to discover a cancellation (the heartbeat channel is the
	// backstop and remains capped at heartbeatInterval).
	streamingCancelPollInterval time.Duration
	lastHeartbeatTime           time.Time
	// pullConflictCount counts consecutive pull conflicts that require the
	// eviction backoff (evicted slow nodes, or offline workers whose immediate
	// re-registration failed). It drives the exponential backoff and bounds
	// when re-registration is retried. Only the poll-loop goroutine reads or
	// writes it.
	pullConflictCount  int
	jobsCompleted      int
	totalJobsCompleted int
	ffprobeExecutor    *FFprobeExecutor
	pixelFormatChecker *PixelFormatChecker
	hwCodecChecker     *HardwareCodecChecker
	auditRecorder      audit.AuditRecorder
	auditNotifier      audit.Notifier
	gpuDetector        *gpu.Detector
	sharedFS           *pathutil.Space // Declared shared filesystem path space (C-108); none declared = no legal path

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
	InputAuthHeader   string // Raw Authorization header for remote input URL downloads
	TempDir           string
	FFmpegPath        string
	Timeout           time.Duration
	IdleTimeout       time.Duration // Kill ffmpeg after this long with no output; 0 disables
	HeartbeatInterval time.Duration // Interval between heartbeats
	PollInterval      time.Duration // Interval for polling jobs
	// StreamingCancelPollInterval is how often a running streaming job polls
	// its own status for cancellation; zero selects the 1s default.
	StreamingCancelPollInterval time.Duration
	CacheConfig                 CacheConfig  // Cache configuration
	RetryConfig                 *RetryConfig // Retry configuration
	SharedFSRoots               string       // Comma-separated roots of the shared filesystem path space (same declaration as the server)
}

// New creates a new worker
func New(cfg Config) (*Worker, error) {
	if cfg.WorkerID == "" {
		cfg.WorkerID = uuid.New().String()
	}
	tempDirDefaulted := false
	if cfg.TempDir == "" {
		cfg.TempDir = workerconfig.DefaultTempDir(cfg.WorkerID)
		tempDirDefaulted = true
	}
	if cfg.Name == "" {
		cfg.Name = fmt.Sprintf("worker-%s", cfg.WorkerID[:8])
	}
	if cfg.HeartbeatInterval == 0 {
		cfg.HeartbeatInterval = 30 * time.Second // Default heartbeat interval
	}
	if cfg.PollInterval == 0 {
		cfg.PollInterval = 1 * time.Second // Default poll interval
	}
	if cfg.StreamingCancelPollInterval <= 0 {
		cfg.StreamingCancelPollInterval = 1 * time.Second // Default streaming cancel poll interval
	}

	// Create temp directory. A defaulted path (root FHS path, per-user XDG
	// dir, or the per-user fallback under a shared $TMPDIR) must be private:
	// created 0700, then verified to catch a hostile pre-existing directory on
	// a shared host.
	// An explicitly configured path keeps 0755 for admin-controlled sharing.
	mode := os.FileMode(0o755)
	if tempDirDefaulted {
		mode = 0o700
	}
	if err := os.MkdirAll(cfg.TempDir, mode); err != nil {
		return nil, fmt.Errorf("failed to create temp directory: %w", err)
	}
	if tempDirDefaulted {
		if err := verifyPrivateDir(cfg.TempDir); err != nil {
			return nil, err
		}
		// The leaf's parent is the first directory below the trusted base
		// (/var/tmp, $TMPDIR, or the user cache dir). MkdirAll leaves a
		// pre-existing parent untouched, so a foreign-owned or loose-mode
		// parent under a sticky base could rename or replace the leaf after
		// the leaf check passes. Verify it too.
		if err := verifyPrivateDir(filepath.Dir(cfg.TempDir)); err != nil {
			return nil, err
		}
	}

	client := NewClient(cfg.ServerURL, cfg.WorkerID, cfg.Token)
	client.SetInputAuthHeader(cfg.InputAuthHeader)
	executor := NewExecutor(cfg.FFmpegPath, cfg.Timeout)
	executor.SetIdleTimeout(cfg.IdleTimeout)
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
	hwCodecChecker := NewHardwareCodecChecker(cfg.FFmpegPath)
	// Parse the shared filesystem space declaration once. Empty entries are
	// dropped so an env value like "/data/media, /mnt/nfs" behaves as the two
	// intended roots; an empty declaration leaves the mode with no legal path.
	sharedFS := pathutil.NewSpace(cfg.SharedFSRoots)

	return &Worker{
		id:                          cfg.WorkerID,
		name:                        cfg.Name,
		client:                      client,
		executor:                    executor,
		retryExecutor:               retryExecutor,
		rewriteAdapter:              rewriteAdapter,
		cache:                       cache,
		tempDir:                     cfg.TempDir,
		activeJobs:                  make(map[string]context.CancelFunc),
		heartbeatInterval:           cfg.HeartbeatInterval,
		sharedFS:                    sharedFS,
		pollInterval:                cfg.PollInterval,
		streamingCancelPollInterval: cfg.StreamingCancelPollInterval,
		lastHeartbeatTime:           time.Now(),
		ffprobeExecutor:             ffprobeExecutor,
		pixelFormatChecker:          pixelFormatChecker,
		hwCodecChecker:              hwCodecChecker,
		auditRecorder:               auditRecorder,
		auditNotifier:               auditNotifier,
		gpuDetector:                 gpu.NewDetector(),
		stopCh:                      make(chan struct{}),
	}, nil
}

// verifyPrivateDir verifies that dir is owned by the current effective user and
// grants no group or other permissions. It guards the defaulted worker temp
// directory and its parent on shared hosts: a pre-existing directory created by
// another user (or with looser permissions) under a shared $TMPDIR or /var/tmp
// must be rejected rather than silently used for job I/O.
func verifyPrivateDir(dir string) error {
	return verifyPrivateDirAs(dir, os.Geteuid())
}

// verifyPrivateDirAs is verifyPrivateDir with an injectable expected owner uid,
// so the owner-mismatch branch is testable without chowning to another user.
func verifyPrivateDirAs(dir string, euid int) error {
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("failed to stat temp directory: %w", err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("temp directory %q: unsupported stat type %T", dir, info.Sys())
	}
	if st.Uid != uint32(euid) {
		return fmt.Errorf("temp directory %q is owned by uid %d, want %d; refusing to use non-private directory", dir, st.Uid, euid)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		return fmt.Errorf("temp directory %q has mode %o, want 0700; refusing to use non-private directory", dir, got)
	}
	return nil
}

// ID returns the worker ID
func (w *Worker) ID() string {
	w.regMu.Lock()
	defer w.regMu.Unlock()
	return w.id
}

// Register registers the worker with the server.
// Performs initial registration and stores capabilities for later re-registration
// after server restart.
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
		// reset the lifetime count only on FIRST registration.
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

	// Set hardware capabilities on the rewrite adapter and the retry/fallback
	// parser so every path knows the same encoder set.
	w.rewriteAdapter.SetHardwareCapabilities(&caps)
	w.retryExecutor.SetHardwareCapabilities(w.rewriteAdapter.HardwareCapabilities())

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

// Start starts the worker loop. Heartbeat transmission runs on an independent
// goroutine so a long-running, high-CPU ffmpeg job can never starve it: the
// job-poll loop is the only path that blocks on job execution, and heartbeats
// must keep flowing at heartbeatInterval regardless of ffmpeg's CPU demand
// (a stalled heartbeat made the server mark the worker offline and
// migrate its still-running jobs).
func (w *Worker) Start(ctx context.Context) {
	// Start cache background eviction
	w.cache.Start(ctx)

	// heartbeatWG lets Start's loop wait for the heartbeat goroutine to exit
	// before returning.
	var heartbeatWG sync.WaitGroup
	heartbeatWG.Add(1)
	go func() {
		defer heartbeatWG.Done()
		heartbeatTicker := time.NewTicker(w.heartbeatInterval)
		defer heartbeatTicker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-w.stopCh:
				return
			case <-heartbeatTicker.C:
				w.sendHeartbeat()
			}
		}
	}()

	ticker := time.NewTicker(w.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Println("Worker shutting down (context cancelled)...")
			heartbeatWG.Wait()
			return
		case <-w.stopCh:
			log.Println("Worker shutting down (Stop called)...")
			heartbeatWG.Wait()
			return
		case <-ticker.C:
			// Do not accept new jobs after Stop was requested.
			select {
			case <-w.stopCh:
				heartbeatWG.Wait()
				return
			default:
			}
			w.pollAndProcess(ctx)
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
		// A 409 Conflict means the worker is offline (its jobs were already
		// migrated) or evicted from scheduling — a transient, recoverable
		// state, not a server fault. Offline is server-restart recovery: the
		// worker re-registers immediately so it re-enters the pool on the
		// next tick instead of waiting out the 30s heartbeat clock (that gap
		// left a transient worker_unavailable window for CLI submissions).
		// Evicted is slow-node detection; C-79 forbids a marked worker from
		// re-registering, so the exponential backoff stays.
		if IsPullConflict(err) {
			if IsWorkerEvicted(err) {
				w.backoffPullConflict(ctx, err)
				return
			}
			// Try one immediate re-registration. If it fails (server still
			// down or restarting), fall through to the eviction backoff so a
			// persistent failure doesn't hammer the register endpoint once per
			// poll tick; the backoff re-registers again once the conflict
			// persists past the threshold.
			if w.reregister() {
				w.pullConflictCount = 0
				return
			}
			w.backoffPullConflict(ctx, err)
			return
		}
		log.Printf("Failed to pull jobs: %v", err)
		// Attempt re-registration if server may have restarted
		w.reregister()
		return
	}
	// A successful pull resets the conflict backoff so a later conflict starts
	// over at the initial delay.
	w.pullConflictCount = 0

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

// Pull-conflict backoff schedule: the poll loop backs off on pull conflicts
// that must not re-register immediately — evicted slow nodes, or offline
// workers whose immediate re-registration failed. It retries with exponential
// backoff and only re-registers once the conflict persists past the threshold.
const (
	pullConflictInitialDelay = 1 * time.Second
	pullConflictMaxDelay     = 30 * time.Second

	// pullConflictReregisterThreshold is the number of consecutive 409 pull
	// conflicts after which the poll loop re-registers once. It is set past
	// every backoff tier (1s→2s→4s→8s→16s→30s cap) so the schedule below is
	// fully reachable and re-registration stays bounded (~61s of backoff).
	pullConflictReregisterThreshold = 7
)

// pullConflictSleep waits out a pull-conflict backoff delay, or returns early
// when the worker stops or the context is cancelled. A package var so tests
// can stub the wait and run the backoff instantly.
var pullConflictSleep = func(ctx context.Context, w *Worker, d time.Duration) {
	select {
	case <-time.After(d):
	case <-ctx.Done():
	case <-w.stopCh:
	}
}

// backoffPullConflict handles a pull conflict that must back off: an evicted
// (slow-node) worker, or an offline worker whose immediate re-registration
// failed. It backs off exponentially for the first (threshold-1) consecutive
// conflicts, then re-registers once on the threshold-th. A successful pull
// resets the count in pollAndProcess, so sparse conflicts never accumulate
// into a re-registration.
func (w *Worker) backoffPullConflict(ctx context.Context, err error) {
	w.pullConflictCount++
	if w.pullConflictCount >= pullConflictReregisterThreshold {
		w.pullConflictCount = 0
		log.Printf("Pull jobs conflict persisted (%d consecutive): %v; re-registering", pullConflictReregisterThreshold, err)
		w.reregister()
		return
	}
	delay := pullConflictBackoff(w.pullConflictCount)
	log.Printf("Pull jobs conflict: %v; retrying in %s", err, delay)
	pullConflictSleep(ctx, w, delay)
}

// pullConflictBackoff returns the wait before retrying after n consecutive 409
// pull conflicts: the initial delay doubled per conflict, capped at
// pullConflictMaxDelay. Production callers pass n in
// [1, pullConflictReregisterThreshold-1]; other values degrade to the nearest
// boundary (n<1 → initial delay, n≥threshold → max delay).
func pullConflictBackoff(consecutive int) time.Duration {
	delay := pullConflictInitialDelay
	for i := 1; i < consecutive; i++ {
		delay *= 2
		if delay >= pullConflictMaxDelay {
			return pullConflictMaxDelay
		}
	}
	return delay
}

// batcherCreateHook, when non-nil, receives every StderrBatcher created on
// the cache-hit path of processJob. Nil in production; tests use it to
// observe batcher lifecycle.
var batcherCreateHook func(*StderrBatcher)

// processJob processes a single job
func (w *Worker) processJob(ctx context.Context, job protocol.JobInfo, cancel context.CancelFunc, wgTracked bool) {
	jobFailed := false
	// flushStderr is assigned only after the main stderr batcher is created;
	// the pre-batcher direct-path failures report with a nil flush, which is
	// an idempotent no-op. The panic-recovery defer cannot reference the
	// batcher directly because it may not exist yet.
	var flushStderr func()
	defer func() {
		// Recover from panics so a single bad job cannot take down the whole
		// worker process; the job is reported as WORKER_CRASH and the worker
		// keeps serving other in-flight jobs.
		if r := recover(); r != nil {
			log.Printf("Job %s: panic recovered: %v\n%s", job.ID, r, debug.Stack())
			// A panic means the job did not complete: mark it failed so the
			// completion counters below are not advanced for a crashed job.
			jobFailed = true
			w.reportFailureWithType(job.ID, -1,
				fmt.Sprintf("worker panic during job processing: %v", r),
				string(protocol.FailureWorkerCrash),
				fmt.Sprintf("%v", r), flushStderr)
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
		// activeJobs only holds the cancel func, so deleting the entry does
		// not release the job context. Cancel it on every exit path — a
		// long-lived worker otherwise accumulates one live context per
		// completed job together with whatever it spawned.
		if cancel != nil {
			cancel()
		}
	}()

	// Detect probe jobs: args[0] == "__rffmpeg_probe__". Probes are health
	// checks, not transcode work — they must not advance the completion
	// counters, or pure probe traffic would push a worker past the server-side
	// MinJobsForEviction warmup gate without doing real work.
	if len(job.Args) > 0 && job.Args[0] == "__rffmpeg_probe__" {
		jobFailed = true // suppress counter increment in the deferred hook
		// Probe jobs are not transcodes: counting them into completed_jobs
		// floods the warmup window and skews the median sample pool.
		w.processProbeJob(ctx, job)
		return
	}

	log.Printf("Processing job %s (auto_hw=%v, streaming_output=%v)", job.ID, job.AutoHW, job.StreamingOutput)

	// Streaming output is attached to a live CLI client whose Ctrl+C must stop
	// ffmpeg promptly. The heartbeat response is the designed cancel channel,
	// but its default 30s cadence keeps a cancelled stream encoding long past
	// the client's bound. Poll this job's status instead; the heartbeat keeps
	// covering every non-streaming job.
	if job.StreamingOutput {
		stopCancelWatch := w.watchStreamingJobCancellation(ctx, job.ID, cancel)
		defer stopCancelWatch()
	}

	// The per-job timeout is a dedicated ffmpeg execution budget, not a
	// wall-clock bound on the pre-execution pipeline. Download, duration
	// probe, and classification must NOT be charged against it: a short
	// --timeout has to reach ffmpeg and surface as TIMEOUT only when ffmpeg
	// itself exceeds the budget. Pre-execution phases keep their own bounds
	// (data-plane client timeout, ffprobe executor timeout, in-memory work),
	// with the scheduler's job_timeout and CLI wait as the overall safety net
	// so a stuck pre-execution phase still terminates.
	jobCtx := ctx
	var execTimeout time.Duration
	if job.Timeout != nil && *job.Timeout > 0 {
		execTimeout = *job.Timeout
		log.Printf("Job %s: using per-job timeout %v", job.ID, execTimeout)
	}

	// Trust boundary (C-123 – C-127). Classification is the only basis on
	// which the execution side obtains inputs, so it runs before the workspace
	// exists: an identifier that matches no form must not leave a file behind.
	directMode := len(job.DirectPaths) > 0
	if directMode {
		// Pass-through inputs are read in place (C-92); each must be a
		// shared-filesystem path and reachable, otherwise the job is refused
		// before ffmpeg starts.
		for _, path := range job.DirectPaths {
			if v := w.validateDirectInputPath(path); v != nil {
				jobFailed = true
				// A path outside the declared space is a boundary violation
				// (INFRA); a missing input is the user's source being
				// unreachable.
				failureType := protocol.FailureInputUnreachable
				if v.boundary {
					failureType = protocol.FailureInfra
				}
				w.reportFailureWithType(job.ID, 1, v.msg, string(failureType), v.details, flushStderr)
				return
			}
		}
		log.Printf("Job %s: direct paths mode, %d input(s)", job.ID, len(job.DirectPaths))
	}
	plannedInputs, planFailure := w.planJobInputs(job, directMode)
	if planFailure != nil {
		jobFailed = true
		w.reportFailureWithType(job.ID, 1, planFailure.msg, string(protocol.FailureInfra), planFailure.details, flushStderr)
		return
	}

	plannedOutput, outputFailure := w.planJobOutput(ctx, job, directMode)
	if outputFailure != nil {
		jobFailed = true
		w.reportFailureWithType(job.ID, 1, outputFailure.msg, string(protocol.FailureInfra), outputFailure.details, flushStderr)
		return
	}

	// C-127: a job's arguments may only reach its manifest inputs and paths
	// inside the workspace (plus the pass-through target). The judgement runs
	// before any download so a rejected job leaves nothing behind.
	argPolicy := jobargs.Policy{
		InputCount: len(plannedInputs),
		Workspace:  filepath.Join(w.tempDir, job.ID),
	}
	if directMode {
		argPolicy.Target = plannedOutput.target
	}
	if violation := jobargs.Check(job.Args, argPolicy); violation != nil {
		jobFailed = true
		w.reportFailureWithType(job.ID, 1, violation.Error(),
			string(protocol.FailureInfra), "argument reaches outside the job boundary", flushStderr)
		return
	}

	// Compute cache key from input files + original args, keyed on the
	// encoder the rewrite WILL select (not the requested one). auto_hw MUST
	// be part of the key: a --auto-hw run may upgrade the encoder (e.g.
	// libx264 -> h264_qsv), and that output must never be served to requests
	// without --auto-hw. The Check key must use the same encoder
	// basis as Put below — otherwise an auto-hw job caches under the rewritten
	// encoder but checks under "" and can never hit.
	cacheKey := GenerateCacheKey(job.InputFiles, job.Args, job.AutoHW,
		filepath.Ext(job.OutputFilename), w.rewriteAdapter.ResolveTargetEncoder(job.Args, job.AutoHW))
	cached := false

	// A protocol address target writes to the network: no file lands in the
	// workspace, so cache lookup and store do not apply (C-96 / C-129).
	isNetOutput := plannedOutput.form == pathutil.OutputTargetProtocolAddress

	// Check cache before any work (skip for direct mode, network outputs, and
	// streaming outputs — stdout has no local file to cache)
	if !directMode && !isNetOutput && !job.StreamingOutput {
		if cachePath, ok := w.cache.Check(cacheKey); ok {
			log.Printf("Job %s: cache HIT (key=%s)", job.ID, cacheKey)

			// Stream the cache-hit notice through the job's stderr so CLI
			// clients observe it, not only this worker process' own log
			// Deferred Close guarantees the flush
			// timer is stopped on every exit path — including the early
			// return below when MkdirAll fails — otherwise timedFlush
			// re-arms itself forever and leaks the timer/goroutine.
			batcher := NewStderrBatcher(job.ID, w.client, DefaultStderrBatcherConfig())
			if batcherCreateHook != nil {
				batcherCreateHook(batcher)
			}
			defer batcher.Close()

			// Create job-specific temp directory
			jobDir := filepath.Join(w.tempDir, job.ID)
			if err := os.MkdirAll(jobDir, 0755); err != nil {
				jobFailed = true
				w.reportInfraFailure(job.ID, 1, fmt.Sprintf("Failed to create job directory: %v", err))
				return
			}
			defer w.cleanupJobDir(jobDir)

			// Copy cached file to output path. A cache hit is only reachable in
			// non pass-through mode, so the target is the workspace file name
			// judged by planJobOutput — never an absolute path from the job row.
			outputPath := filepath.Join(jobDir, plannedOutput.target)
			if !pathutil.Within(jobDir, outputPath) {
				w.reportInfraFailure(job.ID, 1,
					fmt.Sprintf("output path escapes the job workspace: %s", outputPath))
				return
			}

			// Overwrite-policy guard: a cache hit must never
			// silently truncate a pre-existing output file. When the target
			// already exists and the caller did not pass -y (or passed -n),
			// degrade to a cache miss so the normal ffmpeg path applies native
			// overwrite semantics and rejects with "Not overwriting - exiting".
			skipCacheCopy := false
			if ffmpegopts.OverwritePolicy(job.Args) != ffmpegopts.OverwriteForce {
				if _, err := os.Stat(outputPath); err == nil {
					skipCacheCopy = true
					log.Printf("Job %s: output %s exists and overwrite not allowed, degrading cache hit to miss", job.ID, outputPath)
					batcher.Add(fmt.Sprintf("[rffmpeg] Job %s: output exists and overwrite not allowed, re-transcoding\n", job.ID))
				}
			}

			if !skipCacheCopy {
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
					// Emit the cache-hit notice only once the cached output is
					// successfully delivered, so CLI clients never see it ahead
					// of a re-transcoding fallback.
					batcher.Add(fmt.Sprintf("[rffmpeg] Cache hit: %s\n", cacheKey))
					// Deliver the notice before reporting completion so CLI
					// clients always see it ahead of the terminal status.
					batcher.Close()

					// Report success with cached flag
					if err := w.client.UpdateJob(job.ID, protocol.JobStatusCompleted, 0, "", cached); err != nil {
						logTerminalReportError(job.ID, "report completed", err)
					} else {
						log.Printf("Job %s completed from cache", job.ID)
					}
					return
				}
			}
		}
	} else {
		reason := "direct paths mode"
		switch {
		case isNetOutput:
			reason = "protocol address target"
		case job.StreamingOutput:
			reason = "streaming output"
		}
		log.Printf("Job %s: cache bypassed (%s)", job.ID, reason)
	}

	// Cache miss — proceed with normal processing
	if !directMode && !isNetOutput && !job.StreamingOutput {
		log.Printf("Job %s: cache MISS (key=%s)", job.ID, cacheKey[:16])
	}

	// Create job-specific temp directory. A re-dispatched job (worker-failure
	// migration or server-restart recovery) re-enters here with a jobDir still
	// holding partial output from its interrupted previous attempt (deferred
	// cleanup only ran on a graceful exit). Remove it first so ffmpeg starts
	// clean instead of refusing to overwrite the leftover ("Not overwriting -
	// exiting"). The directory is job-private (tempDir/<jobID>/), so nothing
	// here is user data — direct/shared-FS output paths live outside jobDir.
	jobDir := filepath.Join(w.tempDir, job.ID)
	// jobDir is derived from the server-supplied job.ID. Guard the removal
	// scope before os.RemoveAll: an empty (or ".") ID collapses jobDir to the
	// temp root itself, and a ".." component walks outside it, so RemoveAll
	// would delete more than this job's own directory. job.ID is a
	// server-generated UUID, but the check is cheap and mirrors
	// pathutil.ContainsPathTraversal used by the direct-path validation.
	if job.ID == "" || pathutil.ContainsPathTraversal(job.ID) ||
		filepath.Clean(jobDir) == filepath.Clean(w.tempDir) {
		jobFailed = true
		w.reportInfraFailure(job.ID, 1, fmt.Sprintf("invalid job ID %q: refusing to clean job directory", job.ID))
		return
	}
	if err := os.RemoveAll(jobDir); err != nil {
		jobFailed = true
		w.reportInfraFailure(job.ID, 1, fmt.Sprintf("Failed to clean stale job directory: %v", err))
		return
	}
	if err := os.MkdirAll(jobDir, 0755); err != nil {
		jobFailed = true
		w.reportInfraFailure(job.ID, 1, fmt.Sprintf("Failed to create job directory: %v", err))
		return
	}
	defer w.cleanupJobDir(jobDir)
	// ffmpeg runs inside the workspace so a relative path in the argument list
	// resolves there rather than in the worker process' own directory (C-126).
	jobCtx = withWorkDir(jobCtx, jobDir)

	// Note: the job stays queued through the pre-execution pipeline below
	// (download, probe, classification). The running/started_at transition is
	// reported at the ffmpeg execution boundary so the server's started_at
	// matches where the --timeout budget actually begins.

	// Obtain the inputs. Only the upload and remote-URL forms are downloaded;
	// a shared filesystem path is read in place (C-92). The landing name comes
	// from the input's position, never from the identifier text (C-126).
	inputPaths := make([]string, 0, len(plannedInputs))
	for index, input := range plannedInputs {
		if input.form == pathutil.InputSharedPath {
			inputPaths = append(inputPaths, input.identifier)
			continue
		}
		inputPath, contained := inputLandingPath(jobDir, index)
		if !contained {
			jobFailed = true
			w.reportFailureWithType(job.ID, 1,
				fmt.Sprintf("input landing path escapes the job workspace: %s", inputPath),
				string(protocol.FailureInfra), "workspace boundary violation", flushStderr)
			return
		}
		if err := w.client.DownloadInput(ctx, input.identifier, inputPath); err != nil {
			jobFailed = true
			if ctx.Err() == context.Canceled {
				w.reportJobCancelled(job.ID)
				return
			}
			w.reportInputDownloadFailure(job.ID, input.identifier, err)
			return
		}
		inputPaths = append(inputPaths, inputPath)
		log.Printf("Downloaded input %s to %s", input.identifier, inputPath)
	}
	// Cancellation may have landed during the download: stop before paying for
	// the duration probe, encoder rewrite, and ffmpeg startup.
	if ctx.Err() == context.Canceled {
		jobFailed = true
		w.reportJobCancelled(job.ID)
		return
	}
	// The output target was judged by planJobOutput before the workspace
	// existed: a pass-through target is the validated absolute path, every
	// other job writes a single file name inside its workspace (C-125).
	outputFilename := plannedOutput.target
	outputPath := outputFilename
	if !directMode && outputFilename != "-" && plannedOutput.form == pathutil.OutputTargetFile {
		outputPath = filepath.Join(jobDir, outputFilename)
		if !pathutil.Within(jobDir, outputPath) {
			jobFailed = true
			w.reportFailureWithType(job.ID, 1,
				fmt.Sprintf("output path escapes the job workspace: %s", outputPath),
				string(protocol.FailureInfra), "workspace boundary violation", flushStderr)
			return
		}
	}

	// Re-dispatch cleanup: a job migrated off a failed worker (retry count > 0)
	// may point at a shared-FS output where the previous worker left a truncated
	// partial file, so ffmpeg refuses with "Not overwriting - exiting" and the
	// self-heal chain breaks (RetryExecutor's between-attempt idempotency never
	// runs for a migrated job whose first invocation fails on the stale file).
	// Remove it before the first execution; a fresh job (retry count 0) keeps
	// native overwrite semantics, and -n or a -y/-n conflict leaves the file for
	// ffmpeg to reject. Otherwise size/mtime are logged first.
	policy := ffmpegopts.OverwritePolicy(job.Args)
	preservesOutput := policy == ffmpegopts.OverwriteNever || policy == ffmpegopts.OverwriteConflict
	if job.RetryCount > 0 && outputPath != "" && outputPath != "-" && !isNetOutput && !preservesOutput {
		// C-126: a removal derived from the job may only touch the workspace or
		// the request's pass-through target, never another path.
		if !pathutil.Within(jobDir, outputPath) && !(directMode && outputPath == plannedOutput.target) {
			jobFailed = true
			w.reportFailureWithType(job.ID, 1,
				fmt.Sprintf("refusing to remove a path outside the job boundary: %s", outputPath),
				string(protocol.FailureInfra), "workspace boundary violation", flushStderr)
			return
		}
		if info, err := os.Stat(outputPath); err == nil {
			log.Printf("Job %s: removing output from previous worker (%d bytes, mtime %s): %s",
				job.ID, info.Size(), info.ModTime().Format(time.RFC3339), outputPath)
		}
		if err := os.Remove(outputPath); err != nil && !os.IsNotExist(err) {
			// A real removal failure (EACCES, read-only mount, held lock, dead
			// filesystem) leaves the stale file in place, so ffmpeg would still
			// refuse and the self-heal would break silently. Fail fast as an
			// infrastructure fault instead of swallowing the error.
			jobFailed = true
			w.reportInfraFailure(job.ID, 1, fmt.Sprintf("failed to remove stale output %s: %v", outputPath, err))
			return
		}
	}

	// Direct mode writes into a shared-FS target a client may read at any
	// moment. ffmpeg creates and grows the output while it encodes, so a
	// crash, timeout or cancel would leave a truncated file at the final path.
	// Run ffmpeg inside the job workspace and publish the result only on
	// success: the workspace is the only place a job may create files (C-126),
	// so the staging area lives there rather than beside the target. The staged
	// output keeps its original basename because ffmpeg derives sibling
	// artifacts from it: an HLS playlist references "out0.ts" next to
	// "out.m3u8", so publishing unchanged names keeps the layout a client sees.
	executionOutputPath := outputPath
	var stagingDir string
	if directMode && outputPath != "" && outputPath != "-" {
		// The final path is no longer visible to ffmpeg, so its own overwrite
		// refusal cannot fire. Reproduce it here for -n and for the default
		// (no -y): the existing output is the user's data and must not be
		// replaced by a run that only reaches the publish step on success.
		if policy != ffmpegopts.OverwriteForce && policy != ffmpegopts.OverwriteConflict {
			if _, err := os.Stat(outputPath); err == nil {
				jobFailed = true
				w.reportFailure(job.ID, 1,
					fmt.Sprintf("File %s already exists. Exiting.", outputPath), false, flushStderr)
				return
			}
		}
		stagingDir = filepath.Join(jobDir, "output-staging")
		if err := os.MkdirAll(stagingDir, 0o755); err != nil {
			jobFailed = true
			w.reportFailureWithType(job.ID, 1,
				fmt.Sprintf("failed to create output staging directory %s: %v", stagingDir, err),
				string(protocol.FailureFFmpegError), "output path not writable", flushStderr)
			return
		}
		defer func() {
			if stagingDir == "" {
				return
			}
			// Failure, timeout or cancel: the partial encode must never reach
			// the shared target and must not linger next to it either.
			if err := os.RemoveAll(stagingDir); err != nil {
				log.Printf("Job %s: failed to remove output staging directory %s: %v", job.ID, stagingDir, err)
			}
		}()
		executionOutputPath = filepath.Join(stagingDir, filepath.Base(outputPath))
		log.Printf("Job %s: direct output staged in %s", job.ID, stagingDir)
	}

	// Build initial args
	args := BuildArgs(job.Args, inputPaths, executionOutputPath)

	// Apply encoder rewrite based on job.AutoHW flag. The flag is passed per
	// RewriteArgs call — the shared adapter config must not be mutated per job
	// because concurrent jobs would clobber each other's auto-hw decision.

	// Create stderr batcher early to reduce HTTP requests.
	// Must be created before rewrite so notifications can be sent even when
	// rewrite returns an error (e.g., nonexistent codec).
	batcher := NewStderrBatcher(job.ID, w.client, DefaultStderrBatcherConfig())
	defer batcher.Close()
	flushStderr = batcher.FlushAndWait

	// Create progress router to parse stderr for progress/ETA and send periodic updates
	progressRouter := NewProgressRouter(w.client, job.ID, batcher.StderrHandler())
	defer progressRouter.Reset()

	// Seed the parser with the total media duration so percent/ETA are
	// meaningful from the very first stats line. ffmpeg's stderr
	// header only carries a Duration: line when the input is seekable — for
	// pipes and many network inputs it prints "Duration: N/A" and percent
	// would stay -1 forever. Best-effort: probe failures leave the
	// header-derived duration path in place.
	if durationUs := w.probeInputDurationUs(jobCtx, inputPaths); durationUs > 0 {
		progressRouter.SetDuration(durationUs)
		// For trimmed jobs (-ss/-t/-to on either side of -i) progress must be
		// anchored to the span ffmpeg writes, not the full input duration:
		// time= counts from zero within the output, so dividing by the input
		// duration never reaches 100% and ETA is off by orders of magnitude.
		progressRouter.SetSeekWindow(durationUs, ParseSeekWindow(args))
	}

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
		// channel. The shared audit notifier only writes to the
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
			// gaps must be visible), never silently discarded.
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
		w.reportFailure(job.ID, 1, fmt.Sprintf("Encoder rewrite failed: %v", err), false, batcher.FlushAndWait)
		return
	}
	args = rewrittenArgs
	// Streaming (stdout) output cannot be seeked by muxers like mp4/mov.
	// Inject fragmented-output flags so the requested container still works
	// instead of failing inside ffmpeg with "muxer does not support non
	// seekable output". Runs after rewrite so the final args are
	// what gets adjusted.
	if job.StreamingOutput {
		var note string
		args, note = applyStreamableFormat(args, outputPath)
		if note != "" {
			log.Printf("Job %s: %s", job.ID, note)
			progressRouter.Handler()(fmt.Sprintf("[rffmpeg] %s\n", note))
		}
	}

	// Pre-flight pixel format check for VAAPI encoders
	// VAAPI has limited support for yuv444p and other high-chroma pixel formats
	// Probe input to detect incompatible pixel format and proactively fallback
	encoder := extractEncoderFromArgsWithCaps(args, w.rewriteAdapter.HardwareCapabilities())
	if w.pixelFormatChecker != nil && isVAAPIEncoder(encoder) {
		pixelFormatResult := w.pixelFormatChecker.CheckInputsForPixelFormatIncompatibility(jobCtx, inputPaths, encoder)
		if pixelFormatResult != nil && pixelFormatResult.NeedsFallback {
			log.Printf("[PixelFormat] Input has pixel format %s incompatible with %s, falling back to %s",
				pixelFormatResult.PixelFormat, encoder, pixelFormatResult.RecommendedEncoder)
			stderrHandler := progressRouter.Handler()
			stderrHandler(fmt.Sprintf("[rffmpeg] %s\n", pixelFormatResult.Reason))

			// Apply software encoder fallback
			fallback := NewEncoderFallback()
			fallback.SetHardwareCapabilities(w.rewriteAdapter.HardwareCapabilities())
			fallbackArgs := fallback.PrepareFallbackArgsWithSource(args, outputPath, true) // true = user-selected
			if fallbackArgs != nil {
				args = fallbackArgs
				encoder = extractEncoderFromArgsWithCaps(args, w.rewriteAdapter.HardwareCapabilities())
				// Recompute cache key with the new encoder from pixel format fallback
				cacheKey = GenerateCacheKey(job.InputFiles, job.Args, job.AutoHW, filepath.Ext(job.OutputFilename), encoder)
				log.Printf("[PixelFormat] Using software encoder: %s", encoder)
				stderrHandler(fmt.Sprintf("[rffmpeg] fallback to software encoder %s\n", encoder))
			}
		}
	}

	// Pre-flight hardware codec capability check. A worker can advertise a
	// hardware encoder ffmpeg is compiled with (av1_qsv) whose codec the local
	// runtime cannot actually encode (AV1 on a Comet Lake iGPU). Fail fast with
	// ENCODER_UNSUPPORTED instead of degrading to a software fallback
	// (libaom-av1) so slow it looks like a silent hang.
	if w.hwCodecChecker != nil && isHardwareEncoderByName(encoder) {
		if unsupported, reason := w.hwCodecChecker.Check(jobCtx, encoder); unsupported {
			log.Printf("Job %s: hardware encoder %s unsupported on this worker: %s", job.ID, encoder, reason)
			progressRouter.Handler()(fmt.Sprintf("[rffmpeg] %s\n", reason))
			jobFailed = true
			w.reportFailureWithType(job.ID, 1, reason, string(protocol.FailureEncoderUnsupported), reason, batcher.FlushAndWait)
			return
		}
	}

	// Cancellation can land during the duration probe / encoder rewrite above.
	// Check once more at the execution boundary so a cancelled job never pays
	// for ffmpeg startup.
	if ctx.Err() == context.Canceled {
		jobFailed = true
		w.reportJobCancelled(job.ID)
		return
	}

	log.Printf("Executing ffmpeg with args: %v", args)

	// Mark the job running here, at the ffmpeg execution boundary, so the
	// server's started_at matches where the --timeout budget actually begins.
	// The pre-execution pipeline above keeps the job queued: a slow download
	// or probe must not consume the client's --timeout wait window.
	if err := w.client.UpdateJob(job.ID, protocol.JobStatusRunning, 0, "", false); err != nil {
		if ctx.Err() == context.Canceled {
			jobFailed = true
			w.reportJobCancelled(job.ID)
			return
		}
		log.Printf("Failed to update job status to running: %v", err)
		jobFailed = true
		w.reportInfraFailure(job.ID, 1, fmt.Sprintf("Failed to update job status to running: %v", err))
		return
	}

	// Reserve the per-job timeout budget for ffmpeg itself: the overlay is
	// created here, not at job start, so the pre-execution pipeline above
	// (download, probe, classification) can never consume it.
	execCtx := jobCtx
	var execCancel context.CancelFunc
	if execTimeout > 0 {
		execCtx, execCancel = context.WithTimeout(jobCtx, execTimeout)
		defer execCancel()
	}

	// This attempt launches a fresh ffmpeg process: re-base the ETA baseline
	// before any of its stderr reaches the parser. Retries report the same
	// boundary through ExecuteWithRetry.
	progressRouter.StartAttempt()

	// Execute with appropriate handlers
	var result ExecResult
	var stdoutHandler StdoutHandler
	var stdoutBatcher *StdoutBatcher
	if job.StreamingOutput {
		// Streaming output mode: stdout goes to WebSocket. The batcher handler
		// is also passed to ExecuteWithRetry so retried attempts keep streaming.
		stdoutBatcher = NewStdoutBatcher(job.ID, w.client, DefaultStdoutBatcherConfig())
		defer stdoutBatcher.Close()

		stdoutHandler = stdoutBatcher.StdoutHandler()
		result = w.executor.ExecuteWithHandlers(execCtx, args, stdoutHandler, progressRouter.Handler())
	} else {
		// Normal mode: stdout goes to file
		result = w.executor.ExecuteWithStderrHandler(execCtx, args, progressRouter.Handler())
	}

	// Check for context cancellation
	if jobCtx.Err() == context.Canceled {
		w.reportJobCancelled(job.ID)
		jobFailed = true
		return
	}

	// Check for timeout
	if result.IsTimeout {
		w.reportJobTimeout(job.ID, result, batcher.FlushAndWait)
		jobFailed = true
		return
	}

	// A protocol address target does not produce a local file, so the file
	// existence/size validation and the empty-output heuristics below do not
	// apply to it (C-125 / C-129).
	networkOutput := plannedOutput.form == pathutil.OutputTargetProtocolAddress

	// Validate output file for non-streaming jobs: FFmpeg may exit 0 but
	// produce a 0-byte output (e.g., VAAPI hw encoder falls back to a64multi
	// codec which cannot be muxed, resulting in silent failure).
	// Skip for network outputs (RTMP, RTSP, UDP, etc.) — no local file is produced.
	if !networkOutput && !job.StreamingOutput && result.Error == nil && result.ExitCode == 0 {
		if info, err := os.Stat(executionOutputPath); err != nil {
			log.Printf("Job %s: output file not found after exit code 0: %s: %v", job.ID, executionOutputPath, err)
			result.Error = fmt.Errorf("output file not found: %s", outputPath)
			result.ExitCode = 1
			result.Stderr = "Output file not found: " + outputPath + "\n" + result.Stderr
		} else if info.Size() == 0 {
			log.Printf("Job %s: output file is 0 bytes after exit code 0: %s", job.ID, executionOutputPath)
			result.Error = fmt.Errorf("output file is empty (0 bytes): %s", outputPath)
			result.ExitCode = 1
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

	// ffmpeg's own verdict wins: exit code 0 with warnings still produced the
	// requested output. The one exception is the muxer refusing to open or
	// initialize the output, which ffmpeg n9 reports with exit code 0.
	if result.Error == nil && result.ExitCode == 0 {
		if ffmpegStderrIndicatesUnwrittenOutput(result.Stderr) {
			log.Printf("Job %s: ffmpeg could not write the output (exit code 0)", job.ID)
			result.Error = fmt.Errorf("ffmpeg could not open or initialize the output file")
			result.ExitCode = 1
		}
	}

	// Handle execution result
	if result.Error != nil {
		log.Printf("Job %s failed: %v", job.ID, result.Error)

		// Check if this is a retryable error that warrants multi-stage retry
		interceptor := NewErrorInterceptor()
		intercepted := interceptor.Intercept(execCtx, result)

		if intercepted.FFmpegError != nil && intercepted.FFmpegError.IsRetryable() {
			// Skip retry for output_empty errors on network outputs.
			// Network outputs (RTMP, RTSP, UDP, etc.) do not produce local files,
			// so output_empty is a false classification — retrying won't help.
			if networkOutput && intercepted.FFmpegError.Type == ErrorTypeOutputEmpty {
				log.Printf("Job %s: network output detected, skipping output_empty retry", job.ID)
				jobFailed = true
				w.reportFailure(job.ID, result.ExitCode, result.Stderr, false, batcher.FlushAndWait)
				return
			}

			// Notify about retry attempt
			notification := fmt.Sprintf("[RETRY] Initial attempt failed (%s), starting multi-stage retry", intercepted.FFmpegError.Type)
			log.Printf("%s", notification)
			progressRouter.Handler()(notification + "\n")

			// Use RetryExecutor for multi-stage retry. It validates and clears
			// the same path the retry attempts write to — the staged path —
			// so on success the publish step below moves the final artifacts
			// into place exactly once.
			retryResult := w.retryExecutor.ExecuteWithRetry(execCtx, args, executionOutputPath, networkOutput, stdoutHandler, progressRouter.Handler(), progressRouter.StartAttempt)

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
			// Retry attempts share the same execCtx budget as the initial run.
			// The budget can expire inside an attempt (FinalResult.IsTimeout) or
			// during the retry backoff interval (execCtx DeadlineExceeded with a
			// non-timeout FinalResult — the loop breaks at the next ctx.Err()
			// check before running again). Both mean the ffmpeg execution budget
			// was exhausted and must be reported TIMEOUT, not FFMPEG_ERROR.
			if retryResult.FinalResult.IsTimeout || execCtx.Err() == context.DeadlineExceeded {
				timeoutResult := retryResult.FinalResult
				if !timeoutResult.IsTimeout {
					timeoutResult = ExecResult{
						ExitCode:  -1,
						IsTimeout: true,
						Error:     fmt.Errorf("ffmpeg command timed out after %v", execTimeout),
					}
				}
				w.reportJobTimeout(job.ID, timeoutResult, batcher.FlushAndWait)
				jobFailed = true
				return
			}
			// All retries exhausted — report failure with audit trail
			log.Printf("Job %s: All retry attempts exhausted (stages: %s)", job.ID, retryResult.FinalStage)
			errMsg := fmt.Sprintf("All retry attempts exhausted (original encoder: %s, final stage: %s): %s",
				retryResult.OriginalEncoder, retryResult.FinalStage, retryResult.FinalResult.Stderr)
			jobFailed = true
			w.reportFailure(job.ID, retryResult.FinalResult.ExitCode, errMsg, false, batcher.FlushAndWait)
			return
		}

		jobFailed = true
		w.reportFailure(job.ID, result.ExitCode, result.Stderr, false, batcher.FlushAndWait)
		return
	}

uploadOutput:

	// The staged direct-mode output becomes visible only now, after ffmpeg
	// exited 0 and the staged file passed the output checks.
	if stagingDir != "" {
		if err := publishStagedOutput(stagingDir, filepath.Dir(outputPath), filepath.Base(outputPath)); err != nil {
			jobFailed = true
			w.reportInfraFailure(job.ID, result.ExitCode,
				fmt.Sprintf("failed to publish staged output %s: %v", outputPath, err))
			return
		}
		// The deferred cleanup removes the now-empty staging directory.
		log.Printf("Job %s: staged output published to %s", job.ID, outputPath)
	}

	// Upload output file (skip for streaming output, direct mode jobs, and protocol address targets)
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
			if pathutil.IsRemoteURL(input) {
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

	// Flush and wait for all streamed stdout chunks to reach the server before
	// reporting completion. The stdout batcher's timer may not have fired for
	// the tail chunks yet; reporting "completed" first lets the CLI observe the
	// terminal status and close its WebSocket before the bytes arrive, dropping
	// the whole stream for short jobs. A send failure must abort the completion
	// instead — the CLI would otherwise see a truncated stream with rc=0
	// Mirrors StderrBatcher's FlushAndWait on the failure paths.
	if stdoutBatcher != nil {
		if err := stdoutBatcher.FlushAndWait(); err != nil {
			log.Printf("Job %s: failed to stream stdout to server: %v", job.ID, err)
			jobFailed = true
			w.reportInfraFailure(job.ID, 1, fmt.Sprintf("failed to stream stdout to server: %v", err))
			return
		}
	}

	// Send a terminal 100% progress update so CLI clients always see a
	// completed progress line even when ffmpeg's last -stats frame landed
	// short of 100% (common for fast/short encodes that finish between
	// stats ticks). Bypasses the router's throttle — the job is done.
	progressRouter.SendFinal()

	// Report success
	if err := w.client.UpdateJob(job.ID, protocol.JobStatusCompleted, result.ExitCode, "", cached); err != nil {
		logTerminalReportError(job.ID, "report completed", err)
	} else {
		log.Printf("Job %s completed successfully", job.ID)
	}
}

// reportJobCancelled reports the terminal cancelled status for a job whose
// context was cancelled (client cancel, failover, or worker shutdown).
func (w *Worker) reportJobCancelled(jobID string) {
	log.Printf("Job %s was cancelled", jobID)
	// Use -1 for cancelled jobs as the exit code may not be meaningful.
	if err := w.client.UpdateJob(jobID, protocol.JobStatusCancelled, -1, "Job cancelled", false); err != nil {
		logTerminalReportError(jobID, "report cancelled", err)
	}
}

// logTerminalReportError logs an error from a terminal-status update (completed,
// failed, timeout, cancelled). A 409 Conflict is a benign lost race — the job
// already reached a terminal state via a concurrent path (e.g. CLI cancel
// raced the worker's timeout report) — so it is logged at debug level instead
// of surfacing as a server fault. Every other error is a real failure to
// report and stays at error level.
func logTerminalReportError(jobID, action string, err error) {
	if IsConflict(err) {
		slog.Debug("Job terminal report skipped (already terminal)", "job_id", jobID, "action", action, "error", err)
		return
	}
	slog.Error("Failed to report terminal job status", "job_id", jobID, "action", action, "error", err)
}

// reportFailure classifies an ffmpeg execution failure and reports it to the
// server. errMsg must be ffmpeg stderr or a Go error from running ffmpeg —
// never generic infrastructure text (see reportInfraFailure). The reported
// Error is the concise classification summary, never the full stderr: the
// complete ffmpeg log already reaches the CLI once via the live stderr stream,
// so echoing it again in the terminal Error field duplicates it.
func (w *Worker) reportFailure(jobID string, exitCode int, errMsg string, cached bool, flush ...func()) {
	// Flush any pending stderr before reporting the terminal failure so the
	// tail stderr reaches the server before the terminal status PATCH. On the
	// fast-failure path the batcher timer may not have fired yet, and the
	// deferred batcher.Close() would flush it only after this PATCH — letting
	// CLI clients observe the terminal status before the tail stderr.
	for _, f := range flush {
		if f != nil {
			f()
		}
	}
	failureType, failureDetails := ClassifyFailure(exitCode, errMsg, errMsg, false, false)
	if err := w.client.UpdateJobWithFailure(jobID, protocol.JobStatusFailed, exitCode, failureDetails, cached, string(failureType), failureDetails); err != nil {
		logTerminalReportError(jobID, "report failure", err)
	}
}

// reportJobTimeout reports a timed-out job with the TIMEOUT classification
// Shared by the initial-execution path and the retry-exhausted
// path so a timeout landing inside ExecuteWithRetry is classified identically
// to one on the first attempt — otherwise the exhausted branch's isTimeout=false
// would misreport it as FFMPEG_ERROR. Flush callbacks run before the terminal
// PATCH so pending tail stderr reaches the server first (mirroring
// reportFailure); a nil callback is a safe no-op.
func (w *Worker) reportJobTimeout(jobID string, result ExecResult, flush ...func()) {
	log.Printf("Job %s timed out", jobID)
	for _, f := range flush {
		if f != nil {
			f()
		}
	}
	errMsg := ""
	if result.Error != nil {
		errMsg = result.Error.Error()
	}
	failureType, details := ClassifyFailure(result.ExitCode, result.Stderr, errMsg, true, false)
	if err := w.client.UpdateJobWithFailure(jobID, protocol.JobStatusTimeout, result.ExitCode, errMsg, false, string(failureType), details); err != nil {
		logTerminalReportError(jobID, "report timeout", err)
	}
}

// reportInfraFailure reports a worker-side infrastructure failure that happened
// outside ffmpeg execution (job directory creation, server communication,
// output upload, probe plumbing). Pattern-matching this text would misclassify
// it as INPUT_UNREACHABLE etc., so it uses the dedicated INFRA bucket —
// FFMPEG_ERROR is reserved for actual ffmpeg execution failures.
func (w *Worker) reportInfraFailure(jobID string, exitCode int, errMsg string) {
	if err := w.client.UpdateJobWithFailure(jobID, protocol.JobStatusFailed, exitCode, errMsg, false, string(protocol.FailureInfra), errMsg); err != nil {
		logTerminalReportError(jobID, "report failure", err)
	}
}

// reportInputDownloadFailure reports a failure to fetch a job input file.
// Classification depends on the input kind and cause: a worker disk-full while
// writing the input is DISK_FULL; a remote URL that cannot be fetched is
// INPUT_UNREACHABLE; a server file ID failing over the worker↔server channel
// is infrastructure → INFRA.
func (w *Worker) reportInputDownloadFailure(jobID string, fileID string, downloadErr error) {
	failureType := ClassifyInputDownloadFailure(fileID, downloadErr)
	errMsg := fmt.Sprintf("Failed to download input file %s: %v", fileID, downloadErr)
	// Only a transport-layer failure (dataClient.Do error) means the input URL
	// is genuinely unreachable. Its raw error ("dial tcp: lookup … : no such
	// host") is not user-friendly, so surface a clear message naming the input.
	// Size-limit, HTTP-status, and local disk errors carry their own actionable
	// messages and must pass through unchanged.
	userMsg := errMsg
	if failureType == protocol.FailureInputUnreachable {
		var transportErr *transportError
		if errors.As(downloadErr, &transportErr) {
			userMsg = fmt.Sprintf("Input file or stream cannot be reached: %s", fileID)
		}
	}
	if err := w.client.UpdateJobWithFailure(jobID, protocol.JobStatusFailed, 1, userMsg, false, string(failureType), errMsg); err != nil {
		logTerminalReportError(jobID, "report failure", err)
	}
}

// reportFailureWithType reports a job failure with an explicit failure type
// and details. The optional flush callbacks run before the terminal PATCH so
// any pending tail stderr reaches the server first; a nil callback
// is a safe no-op for paths where no stderr batcher exists yet.
func (w *Worker) reportFailureWithType(jobID string, exitCode int, errMsg, failureType, failureDetails string, flush ...func()) {
	for _, f := range flush {
		if f != nil {
			f()
		}
	}
	if err := w.client.UpdateJobWithFailure(jobID, protocol.JobStatusFailed, exitCode, errMsg, false, failureType, failureDetails); err != nil {
		logTerminalReportError(jobID, "report failure", err)
	}
}

// probeInputDurationUs returns the longest duration among the job's local
// inputs, in microseconds, or 0 when none can be determined (missing tool,
// unreadable, non-local). Used to seed the progress parser so percent/ETA work
// even when ffmpeg reports "Duration: N/A" (pipes, some network inputs).
// The longest input, not the first, is the denominator: a still-image cover
// input such as `-loop 1 -i cover.jpg -i audio.mp3` probes as a single frame,
// and measuring progress against it reports 100% at the first stats line.
// Inputs that fail to probe are skipped rather than aborting.
func (w *Worker) probeInputDurationUs(ctx context.Context, inputPaths []string) int64 {
	return longestProbedDurationUs(ctx, inputPaths, w.ffprobeExecutor.Probe)
}

// longestProbedDurationUs is the selection half of probeInputDurationUs: every
// probeable input is probed and the longest duration wins. probe is a seam so
// the selection can be tested without an ffprobe binary. Probes run
// concurrently: the result is a maximum, and probing a many-input job serially
// delays ffmpeg startup by one ffprobe per input.
func longestProbedDurationUs(ctx context.Context, inputPaths []string, probe func(context.Context, string) (*FFprobeResult, error)) int64 {
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		durs []int64
	)
	for _, p := range inputPaths {
		if p == "" || strings.HasPrefix(p, "-") {
			continue // skip placeholders and option-like args
		}
		if info, err := os.Stat(p); err != nil || !info.Mode().IsRegular() {
			continue
		}
		wg.Add(1)
		go func(p string) {
			defer wg.Done()
			result, err := probe(ctx, p)
			if err != nil {
				log.Printf("Progress duration probe failed for %s: %v", p, err)
				return
			}
			if result == nil || result.Format == nil {
				return
			}
			var us int64
			switch d := result.Format["duration"].(type) {
			case string:
				if secs, err := strconv.ParseFloat(d, 64); err == nil && secs > 0 {
					us = int64(secs * 1_000_000)
				}
			case float64:
				if d > 0 {
					us = int64(d * 1_000_000)
				}
			}
			if us > 0 {
				mu.Lock()
				durs = append(durs, us)
				mu.Unlock()
			}
		}(p)
	}
	wg.Wait()

	var longest int64
	for _, us := range durs {
		if us > longest {
			longest = us
		}
	}
	return longest
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

	// Resolve the probe input through the same whole-string classification the
	// submission side uses (C-123 / C-124). A shared filesystem path is read in
	// place; the download forms land as input-0, a name never taken from the
	// identifier text (C-126).
	directMode := len(job.DirectPaths) > 0
	if directMode {
		for _, path := range job.DirectPaths {
			if v := w.validateDirectInputPath(path); v != nil {
				failureType := protocol.FailureInputUnreachable
				if v.boundary {
					failureType = protocol.FailureInfra
				}
				w.reportFailureWithType(job.ID, 1, v.msg,
					string(failureType), v.details, nil)
				return
			}
		}
	}
	if len(job.InputFiles) == 0 {
		w.reportInfraFailure(job.ID, 1, "no input files for probe job")
		return
	}
	plannedInputs, planFailure := w.planJobInputs(job, directMode)
	if planFailure != nil {
		w.reportFailureWithType(job.ID, 1, planFailure.msg,
			string(protocol.FailureInfra), planFailure.details, nil)
		return
	}
	if len(plannedInputs) == 0 {
		w.reportInfraFailure(job.ID, 1, "no input files for probe job")
		return
	}
	inputPath := plannedInputs[0].identifier
	if plannedInputs[0].form != pathutil.InputSharedPath {
		landing, contained := inputLandingPath(jobDir, 0)
		if !contained {
			w.reportFailureWithType(job.ID, 1,
				fmt.Sprintf("input landing path escapes the job workspace: %s", landing),
				string(protocol.FailureInfra), "workspace boundary violation", nil)
			return
		}
		if err := w.client.DownloadInput(ctx, plannedInputs[0].identifier, landing); err != nil {
			if ctx.Err() == context.Canceled {
				w.reportJobCancelled(job.ID)
				return
			}
			w.reportInputDownloadFailure(job.ID, plannedInputs[0].identifier, err)
			return
		}
		log.Printf("Downloaded probe input %s to %s", plannedInputs[0].identifier, landing)
		inputPath = landing
	} else {
		log.Printf("Probe job %s: shared filesystem path, probing %s", job.ID, inputPath)
	}
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
		logTerminalReportError(job.ID, "report completed", err)
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
	var jobsPerSec float64
	if elapsed > 0 && w.jobsCompleted > 0 {
		jobsPerSec = float64(w.jobsCompleted) / elapsed
	}
	completedJobs := w.totalJobsCompleted
	w.jobsCompleted = 0
	w.lastHeartbeatTime = now
	w.mu.Unlock()

	gpuMetrics := w.gpuDetector.SampleMetrics()
	cancelledJobs, err := w.client.Heartbeat(status, activeJobIDs, jobsPerSec, completedJobs, gpuMetrics)
	if err != nil {
		log.Printf("Failed to send heartbeat: %v", err)
		// An evicted worker must not re-register (C-79); other failures
		// (server restart, network) trigger re-registration.
		if IsWorkerEvicted(err) {
			return
		}
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

// defaultStreamingCancelPollInterval bounds how long a cancelled streaming job
// keeps running when no interval is configured. The heartbeat response is the
// designed cancel backstop, but its default 30s cadence is far past the bound a
// client Ctrl+C must meet; the poll rate is bounded by the number of
// concurrently attached streams.
const defaultStreamingCancelPollInterval = 1 * time.Second

// streamingCancelRequestTimeoutFactor bounds a single cancellation-status
// query: the request is abandoned after this many poll intervals, so a slow
// status endpoint cannot swallow the next tick and push the cancel latency
// past the acceptance bound. The heartbeat remains the backstop meanwhile.
const streamingCancelRequestTimeoutFactor = 2

// streamingCancelFailureLogEvery limits how often consecutive poll failures
// are logged: the first failure is always logged (so a degraded fast path is
// visible), then one line per this many failures instead of one per interval.
const streamingCancelFailureLogEvery = 30

// cancelPollInterval returns the configured streaming cancel poll interval,
// falling back to the default for workers constructed without New (tests).
func (w *Worker) cancelPollInterval() time.Duration {
	if w.streamingCancelPollInterval > 0 {
		return w.streamingCancelPollInterval
	}
	return defaultStreamingCancelPollInterval
}

// watchStreamingJobCancellation polls jobID's server-side state and cancels
// the job context as soon as the job is no longer this worker's to execute.
// The cancel func is the job's own, so the cancellation tears down execCtx and
// cmd.Cancel kills the whole ffmpeg process group. The returned stop function
// must be called exactly once when the job finishes; it ends the watcher,
// aborts any in-flight status query, and returns once the watcher goroutine
// has exited.
//
// Poll failures never fail a job — the heartbeat response remains the cancel
// backstop — but they are counted and logged so a degraded fast path is
// observable.
func (w *Worker) watchStreamingJobCancellation(ctx context.Context, jobID string, cancel context.CancelFunc) func() {
	watchCtx, watchCancel := context.WithCancel(ctx)
	interval := w.cancelPollInterval()
	requestTimeout := streamingCancelRequestTimeoutFactor * interval
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		failures := 0
		for {
			select {
			case <-watchCtx.Done():
				return
			case <-done:
				return
			case <-ticker.C:
				reqCtx, reqCancel := context.WithTimeout(watchCtx, requestTimeout)
				job, err := w.client.GetJob(reqCtx, jobID)
				reqCancel()
				if err != nil {
					// A stop() or job-context teardown cancels the in-flight
					// query; that is normal shutdown, not a degraded fast path,
					// so it must not surface as a fallback warning.
					if watchCtx.Err() != nil {
						return
					}
					failures++
					if failures == 1 || failures%streamingCancelFailureLogEvery == 0 {
						log.Printf("Job %s: cancel status poll failed (%d consecutive): %v; heartbeat remains the backstop",
							jobID, failures, err)
					}
					continue
				}
				failures = 0
				if jobCancelledElsewhere(job, w.ID()) {
					log.Printf("Cancelling job %s (cancelled on server)", jobID)
					cancel()
					return
				}
			}
		}
	}()
	return func() {
		close(done)
		watchCancel()
		<-finished
	}
}

// jobCancelledElsewhere mirrors the Server's heartbeat cancel predicate: a job
// is no longer ours to run when it was cancelled, re-queued for another
// dispatch, or reassigned to a different worker after failover.
func jobCancelledElsewhere(job *protocol.JobInfo, workerID string) bool {
	if job.Status == protocol.JobStatusCancelled || job.Status == protocol.JobStatusPending {
		return true
	}
	return job.WorkerID != "" && job.WorkerID != workerID
}

// cleanupJobDir cleans up the job's temporary directory
func (w *Worker) cleanupJobDir(dir string) {
	if err := os.RemoveAll(dir); err != nil {
		log.Printf("Failed to cleanup job directory %s: %v", dir, err)
	}
}

// directPathViolation describes a rejected direct input/output path.
// msg is the user-visible error; details is the machine-readable reason.
type directPathViolation struct {
	msg     string
	details string
	// boundary marks a violation of the declared path space: it is an
	// infrastructure failure (C-124 / C-126), not an unreachable input.
	boundary bool
}

// validateDirectInputPath checks a pass-through input against the declared
// space and its own reachability. Space membership is judged on the resolved
// path (C-107), so a symlink inside a root that points outside every root is
// rejected; a missing input stays an input-unreachable error.
func (w *Worker) validateDirectInputPath(path string) *directPathViolation {
	if err := pathutil.ValidateSharedPath(w.pathSpace(), path); err != nil {
		return &directPathViolation{
			msg:      fmt.Sprintf("pass-through input is outside the declared shared filesystem space: %s", path),
			details:  err.Error(),
			boundary: true,
		}
	}
	if _, err := os.Stat(path); err != nil {
		return &directPathViolation{
			msg:     fmt.Sprintf("input path unreachable: %s: %v", path, err),
			details: err.Error(),
		}
	}
	return nil
}

// validateDirectOutputPath checks a pass-through target against the declared
// space. A target that only resolves outside the space on this side (a replaced
// symlink, a differently mounted root) fails the job here instead of writing
// anywhere (C-125).
func (w *Worker) validateDirectOutputPath(path string) *directPathViolation {
	space := w.pathSpace()
	// Shape first: an absolute, traversal-free path is the precondition for
	// every later judgement, and nothing on disk can make it true.
	switch {
	case path == "":
		return &directPathViolation{msg: "pass-through target is empty", details: "path is empty", boundary: true}
	case !filepath.IsAbs(path):
		return &directPathViolation{
			msg:      fmt.Sprintf("pass-through target must be an absolute path: %s", path),
			details:  "path must be absolute",
			boundary: true,
		}
	case pathutil.ContainsPathTraversal(path):
		return &directPathViolation{
			msg:      fmt.Sprintf("pass-through target must not contain a '..' component: %s", path),
			details:  "path contains a '..' component",
			boundary: true,
		}
	case !space.Declared():
		return &directPathViolation{
			msg:      fmt.Sprintf("pass-through target has no declared shared filesystem space: %s", path),
			details:  "no shared filesystem root is declared",
			boundary: true,
		}
	}
	// The target's own resolution runs before the membership judgement so a
	// broken link or a missing parent reports its actual cause instead of
	// being folded into an out-of-space verdict.
	if _, err := resolveOutputRealPath(path); err != nil {
		switch {
		case errors.Is(err, errOutputParentNotExist):
			return &directPathViolation{
				msg:     fmt.Sprintf("output path parent directory does not exist: %s", filepath.Dir(path)),
				details: "parent directory does not exist",
			}
		case errors.Is(err, errOutputBrokenSymlink):
			return &directPathViolation{
				msg:     fmt.Sprintf("output path is a broken symbolic link: %s", path),
				details: "output path is a broken symlink",
			}
		default:
			return &directPathViolation{
				msg:     fmt.Sprintf("output path cannot be resolved: %s: %v", path, err),
				details: err.Error(),
			}
		}
	}
	if !space.Contains(path) {
		return &directPathViolation{
			msg:      fmt.Sprintf("pass-through target is outside the declared shared filesystem space: %s", path),
			details:  "path is outside the declared shared filesystem space",
			boundary: true,
		}
	}
	return nil
}

// errOutputBrokenSymlink and errOutputParentNotExist distinguish the two
// NotExist outcomes of resolveOutputRealPath so validateDirectOutputPath can
// report the actual cause instead of lumping both under "parent missing".
var (
	errOutputBrokenSymlink  = errors.New("output path is a broken symbolic link")
	errOutputParentNotExist = errors.New("output path parent directory does not exist")
)

// resolveOutputRealPath returns the symlink-free real path for an output
// path: an existing final component is resolved in full, while a
// not-yet-existing file has its parent resolved and the base name re-joined
// so a symlinked parent is checked against the real target. The two NotExist
// outcomes are distinguished with sentinel errors so the caller reports the
// actual cause: errOutputBrokenSymlink means the path is a symlink to a
// missing target; errOutputParentNotExist means its parent is missing.
func resolveOutputRealPath(path string) (string, error) {
	if _, err := os.Lstat(path); err == nil {
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			if os.IsNotExist(err) {
				return "", errOutputBrokenSymlink
			}
			return "", err
		}
		return resolved, nil
	} else if !os.IsNotExist(err) {
		return "", err
	}
	realParent, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		if os.IsNotExist(err) {
			return "", errOutputParentNotExist
		}
		return "", err
	}
	return filepath.Join(realParent, filepath.Base(path)), nil
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

// publishStagedOutput moves every file ffmpeg produced from the workspace
// staging directory into the target directory, keeping the names ffmpeg chose
// so a playlist keeps referencing its segments. The main output moves last:
// until then a client sees either the previous complete result or no playlist
// at all, never one that points at files still staged.
func publishStagedOutput(stagingDir, targetDir, mainName string) error {
	entries, err := os.ReadDir(stagingDir)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			return fmt.Errorf("staged output contains unexpected directory %q", entry.Name())
		}
		names = append(names, entry.Name())
	}
	sort.Slice(names, func(i, j int) bool {
		return names[i] != mainName && names[j] == mainName
	})
	for _, name := range names {
		if err := publishFile(filepath.Join(stagingDir, name), filepath.Join(targetDir, name)); err != nil {
			return fmt.Errorf("move %s: %w", name, err)
		}
	}
	return nil
}

// publishFile places one staged file at its final path. A workspace and a
// shared target may sit on different filesystems, where rename cannot cross the
// boundary. The copy then writes the target path itself and never a sibling
// temporary name: C-126's pass-through exception covers the target path only, so
// a temp file beside it would be exactly the second outside path the exception
// forbids. A failed publish removes the target, leaving the path untouched.
func publishFile(stagingPath, targetPath string) error {
	err := os.Rename(stagingPath, targetPath)
	if err == nil {
		return nil
	}
	if !errors.Is(err, syscall.EXDEV) {
		return err
	}
	if err := copyFile(stagingPath, targetPath); err != nil {
		os.Remove(targetPath)
		return err
	}
	return nil
}

// ffmpegStderrIndicatesUnwrittenOutput reports whether ffmpeg failed to open or
// initialize the output even though it exited 0. ffmpeg n9 returns 0 for some
// output-open failures and for its own overwrite refusal, so the exit code
// alone cannot separate those from a completed transcode.
//
// Only the muxer's own error lines count: decoding warnings, stream info and
// metadata values (a title literally named "header missing", for example) are
// not failures. Anything broader turns a successful job into FFMPEG_ERROR.
func ffmpegStderrIndicatesUnwrittenOutput(stderr string) bool {
	for _, raw := range strings.Split(stderr, "\n") {
		line := strings.TrimSpace(raw)
		// Component messages carry an "[out#0/mp4 @ 0x...] " context tag.
		if strings.HasPrefix(line, "[") {
			if end := strings.Index(line, "] "); end != -1 {
				line = strings.TrimSpace(line[end+2:])
			}
		}
		switch {
		case strings.HasPrefix(line, "Error opening output"),
			strings.HasPrefix(line, "Error initializing the muxer"),
			strings.HasPrefix(line, "Unable to choose an output format"):
			return true
		case strings.HasPrefix(line, "File ") && strings.Contains(line, "already exists"):
			// -n refusal: "File 'out.mp4' already exists. Exiting." The
			// interactive variant embeds the same sentence in the overwrite
			// prompt, so the prompt's "Not overwriting" tail needs no separate
			// match — a metadata value carrying that phrase is not a refusal.
			return true
		}
	}
	return false
}
