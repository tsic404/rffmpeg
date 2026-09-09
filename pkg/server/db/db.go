package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/google/uuid"
	_ "github.com/mattn/go-sqlite3"
	"github.com/tsic404/rffmpeg/pkg/protocol"
)

// Job represents a job record in the database
type Job struct {
	ID              string
	Status          protocol.JobStatus
	InputFiles      string // JSON array stored as string
	Args            string // JSON array stored as string
	OutputFilename  string // Original output filename with extension
	StreamingOutput bool   // Output to stdout via WebSocket
	OutputFiles     string // JSON array stored as string
	WorkerID        sql.NullString
	AssignedWorker  sql.NullString // Worker ID of the last/current executor (survives terminal state)
	WorkerName      sql.NullString // Human-readable name of the assigned worker
	ExitCode        sql.NullInt32
	Error           sql.NullString
	FailureType     string        // Classified failure type (TSI-757)
	FailureDetails  string        // Human-readable failure detail
	AutoHW          bool          // Enable automatic hardware encoder upgrade
	Cached          bool          // Result was served from the worker cache (TSI-2519)
	Timeout         sql.NullInt64 // Per-job ffmpeg execution budget in nanoseconds (nil = worker default)
	DirectPaths     string        // JSON array of direct output paths for pass-through mode (TSI-807)
	ProgressPercent float64       // Current progress percentage (0-100)
	EtaSeconds      int           // Estimated seconds remaining (0 if unknown)
	CreatedAt       time.Time
	UpdatedAt       time.Time
	StartedAt       sql.NullTime
	FinishedAt      sql.NullTime
}

// File represents a file record in the database
type File struct {
	ID        string
	Filename  string
	Path      string
	Size      int64
	Checksum  sql.NullString
	CreatedAt time.Time
}

// Worker represents a worker record in the database
type Worker struct {
	ID            string
	Name          string
	Status        protocol.WorkerStatus
	GPUModel      sql.NullString
	Encoders      string // JSON array
	Decoders      string // JSON array
	VideoEncoders string // JSON array of EncoderInfo
	VideoDecoders string // JSON array of DecoderInfo
	FFmpegVersion string
	MaxConcurrent int
	Evicted       bool
	EvictedAt     sql.NullTime
	LastHeartbeat time.Time
	CreatedAt     time.Time

	// P0/P1 raw text capability fields
	Hwaccels string // Raw text from ffmpeg -hwaccels
	Codecs   string // Raw text from ffmpeg -codecs
	Filters  string // Raw text from ffmpeg -filters
	PixFmts  string // Raw text from ffmpeg -pix_fmts
	Formats  string // Raw text from ffmpeg -formats
}

// Database wraps the SQL database connection
type Database struct {
	db       *sql.DB
	notifier *JobNotifier
}

// sqliteDSN builds a SQLite DSN with the pragmas required for safe concurrent
// use (TSI-2359): WAL journaling so readers never block the writer, a busy
// timeout so concurrent writes queue instead of failing with SQLITE_BUSY,
// foreign-key enforcement so ON DELETE CASCADE fires (upload_chunks cleanup),
// and immediate transactions so lock acquisition happens at BEGIN rather than
// at first write (avoids deadlock-prone deferred-to-write upgrades).
func sqliteDSN(dbPath string) string {
	const params = "_busy_timeout=5000&_journal_mode=WAL&_fk=1&_txlock=immediate"
	switch {
	case strings.HasPrefix(dbPath, "file:"):
		if strings.Contains(dbPath, "?") {
			return dbPath + "&" + params
		}
		return dbPath + "?" + params
	case dbPath == ":memory:" || dbPath == "file::memory:":
		return "file::memory:?" + params
	default:
		return "file:" + dbPath + "?" + params
	}
}

// New creates a new database connection and initializes tables.
func New(dbPath string) (*Database, error) {
	if dbPath == "" {
		// An empty path would silently become an in-memory database that
		// "works" but loses everything on restart — reject it loudly instead
		// (TSI-2359 review round 2).
		return nil, fmt.Errorf("database path must not be empty (use \":memory:\" explicitly for a test database)")
	}

	db, err := sql.Open("sqlite3", sqliteDSN(dbPath))
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}
	d := &Database{db: db, notifier: newJobNotifier()}

	// An in-memory database lives per-connection: every extra pooled
	// connection would see its own empty database. Pin the pool to one
	// connection so :memory: keeps working as a shared store.
	if dbPath == ":memory:" || dbPath == "file::memory:" {
		db.SetMaxOpenConns(1)
	}

	if err := d.initTables(); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to initialize tables: %w", err)
	}

	return d, nil
}

// Close closes the database connection
func (d *Database) Close() error {
	return d.db.Close()
}

// GetDB returns the underlying sql.DB connection (for testing purposes)
func (d *Database) GetDB() *sql.DB {
	return d.db
}

// JobNotifier returns the per-job terminal-status notifier backing Subscribe.
func (d *Database) JobNotifier() *JobNotifier {
	return d.notifier
}

// NotifyTerminal wakes in-process waiters after a successful terminal-status
// write. It is nil-safe for databases constructed without a notifier.
func (d *Database) NotifyTerminal(jobID string) {
	if d.notifier != nil {
		d.notifier.Notify(jobID)
	}
}

// notifyTerminal is the internal alias used by the DB's own terminal-status
// writers; exported NotifyTerminal covers out-of-band writers (scheduler).
func (d *Database) notifyTerminal(jobID string) {
	d.NotifyTerminal(jobID)
}

// initTables creates the necessary tables
func (d *Database) initTables() error {
	_, err := d.db.Exec(`
		CREATE TABLE IF NOT EXISTS jobs (
			id TEXT PRIMARY KEY,
			status TEXT NOT NULL,
			input_files TEXT NOT NULL,
			args TEXT NOT NULL,
			output_filename TEXT DEFAULT '',
			streaming_output INTEGER DEFAULT 0,
			output_files TEXT DEFAULT '[]',
			worker_id TEXT,
			assigned_worker TEXT,
			worker_name TEXT,
			exit_code INTEGER,
			error TEXT,
			failure_type TEXT DEFAULT '',
			failure_details TEXT DEFAULT '',
			retryable INTEGER DEFAULT 0,
			auto_hw INTEGER DEFAULT 0,
			cached INTEGER DEFAULT 0,
			timeout INTEGER,
			direct_paths TEXT DEFAULT '[]',
			progress_percent REAL DEFAULT 0,
			eta_seconds INTEGER DEFAULT 0,
			created_at DATETIME NOT NULL,
			updated_at DATETIME NOT NULL,
			started_at DATETIME,
			finished_at DATETIME
		);

		CREATE TABLE IF NOT EXISTS files (
			id TEXT PRIMARY KEY,
			filename TEXT NOT NULL,
			path TEXT NOT NULL,
			size INTEGER NOT NULL,
			checksum TEXT,
			created_at DATETIME NOT NULL
		);

		CREATE TABLE IF NOT EXISTS workers (
			id TEXT PRIMARY KEY,
			name TEXT,
			status TEXT NOT NULL,
			gpu_model TEXT,
			encoders TEXT DEFAULT '[]',
			decoders TEXT DEFAULT '[]',
			video_encoders TEXT DEFAULT '[]',
			video_decoders TEXT DEFAULT '[]',
			ffmpeg_version TEXT,
			max_concurrent INTEGER DEFAULT 1,
			evicted INTEGER DEFAULT 0,
			evicted_at DATETIME,
			hwaccels TEXT DEFAULT '',
			codecs TEXT DEFAULT '',
			filters TEXT DEFAULT '',
			pix_fmts TEXT DEFAULT '',
			formats TEXT DEFAULT '',
			last_heartbeat DATETIME NOT NULL,
			created_at DATETIME NOT NULL
		);

		CREATE TABLE IF NOT EXISTS upload_sessions (
			id TEXT PRIMARY KEY,
			filename TEXT NOT NULL,
			file_size INTEGER NOT NULL,
			chunk_size INTEGER NOT NULL,
			total_chunks INTEGER NOT NULL,
			uploaded_chunks TEXT DEFAULT '[]',
			file_checksum TEXT,
			status TEXT NOT NULL,
			created_at DATETIME NOT NULL,
			updated_at DATETIME NOT NULL,
			expires_at DATETIME
		);

		CREATE TABLE IF NOT EXISTS upload_chunks (
			id TEXT PRIMARY KEY,
			upload_id TEXT NOT NULL,
			chunk_index INTEGER NOT NULL,
			chunk_size INTEGER NOT NULL,
			checksum TEXT NOT NULL,
			path TEXT NOT NULL,
			created_at DATETIME NOT NULL,
			FOREIGN KEY (upload_id) REFERENCES upload_sessions(id) ON DELETE CASCADE
		);

		CREATE INDEX IF NOT EXISTS idx_jobs_status ON jobs(status);
		CREATE INDEX IF NOT EXISTS idx_jobs_worker_id ON jobs(worker_id);
		-- Backs the GET /api/v1/jobs list ordering (created_at DESC, id DESC)
		-- so paginated listings scan an index instead of a full table sort.
		CREATE INDEX IF NOT EXISTS idx_jobs_created_at_id ON jobs(created_at DESC, id DESC);
		CREATE INDEX IF NOT EXISTS idx_workers_status ON workers(status);
		CREATE INDEX IF NOT EXISTS idx_upload_sessions_status ON upload_sessions(status);
		CREATE INDEX IF NOT EXISTS idx_upload_sessions_expires_at ON upload_sessions(expires_at);
		CREATE INDEX IF NOT EXISTS idx_upload_chunks_upload_id ON upload_chunks(upload_id);

		-- TSI-2359: a (upload_id, chunk_index) pair identifies one chunk.
		-- Without this, a retried upload could insert duplicate rows; the
		-- constraint makes CreateUploadChunk idempotent via conflict handling.
		CREATE UNIQUE INDEX IF NOT EXISTS idx_upload_chunks_upload_chunk
			ON upload_chunks(upload_id, chunk_index);

		CREATE TABLE IF NOT EXISTS migration_events (
			id TEXT PRIMARY KEY,
			timestamp DATETIME NOT NULL,
			worker_id TEXT NOT NULL,
			worker_name TEXT,
			reason TEXT NOT NULL,
			retry_count INTEGER NOT NULL,
			job_ids TEXT NOT NULL,
			jobs_migrated INTEGER NOT NULL,
			created_at DATETIME NOT NULL
		);

		CREATE INDEX IF NOT EXISTS idx_migration_events_timestamp ON migration_events(timestamp);
		CREATE INDEX IF NOT EXISTS idx_migration_events_worker_id ON migration_events(worker_id);

		-- TSI-2929: per-job redistribution targets. A migration event records
		-- the source worker and the full migrated job_ids list, but those jobs
		-- are then reassigned independently and can fan out to different
		-- workers — so the target is a per-job fact, not a per-event one. Rows
		-- are inserted as unresolved placeholders (target_worker_id NULL) when
		-- a job is migrated, and filled by the scheduler/pull path on
		-- reassignment. The job_id index keeps the fill path a point lookup
		-- instead of a full-table json_each scan.
		CREATE TABLE IF NOT EXISTS job_redistributions (
			id TEXT PRIMARY KEY,
			migration_event_id TEXT NOT NULL,
			job_id TEXT NOT NULL,
			target_worker_id TEXT,
			target_worker_name TEXT,
			created_at DATETIME NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_job_redistributions_job_id ON job_redistributions(job_id);
		CREATE INDEX IF NOT EXISTS idx_job_redistributions_migration_event_id ON job_redistributions(migration_event_id);

		CREATE TABLE IF NOT EXISTS worker_eviction_events (
			id TEXT PRIMARY KEY,
			timestamp DATETIME NOT NULL,
			worker_id TEXT NOT NULL,
			event_type TEXT NOT NULL,
			current_throughput REAL NOT NULL,
			cluster_median REAL NOT NULL,
			decision_reason TEXT NOT NULL,
			created_at DATETIME NOT NULL
		);

		CREATE TABLE IF NOT EXISTS ws_job_seq (
			job_id TEXT PRIMARY KEY,
			last_seq INTEGER NOT NULL,
			updated_at DATETIME NOT NULL
		);

		-- TSI-2379: per-job WebSocket sequence counters for gap detection.
		-- Transient by design: rows live only while a job is streaming (the
		-- hub deletes them on terminal broadcasts) and are safe to rebuild —
		-- dropping the table only resets restart-resume numbering to 1, it
		-- never loses job data.

		CREATE INDEX IF NOT EXISTS idx_eviction_events_timestamp ON worker_eviction_events(timestamp);
		CREATE INDEX IF NOT EXISTS idx_eviction_events_worker_id ON worker_eviction_events(worker_id);
	`)

	// Migrate existing databases: add columns if they don't exist.
	// SQLite does not support IF NOT EXISTS for ALTER TABLE ADD COLUMN,
	// so we ignore "duplicate column" errors from columns that already exist.
	for _, stmt := range []string{
		`ALTER TABLE jobs ADD COLUMN failure_type TEXT DEFAULT ''`,
		`ALTER TABLE jobs ADD COLUMN failure_details TEXT DEFAULT ''`,
		`ALTER TABLE jobs ADD COLUMN retryable INTEGER DEFAULT 0`,
		`ALTER TABLE jobs ADD COLUMN timeout INTEGER`,
		`ALTER TABLE workers ADD COLUMN evicted INTEGER DEFAULT 0`,
		`ALTER TABLE workers ADD COLUMN evicted_at DATETIME`,
		`ALTER TABLE jobs ADD COLUMN direct_paths TEXT DEFAULT '[]'`,
		`ALTER TABLE jobs ADD COLUMN cached INTEGER DEFAULT 0`,
		`ALTER TABLE jobs ADD COLUMN assigned_worker TEXT`,
		`ALTER TABLE jobs ADD COLUMN worker_name TEXT`,
		`ALTER TABLE workers ADD COLUMN video_encoders TEXT DEFAULT '[]'`,
		`ALTER TABLE workers ADD COLUMN video_decoders TEXT DEFAULT '[]'`,
		`ALTER TABLE workers ADD COLUMN hwaccels TEXT DEFAULT ''`,
		`ALTER TABLE workers ADD COLUMN codecs TEXT DEFAULT ''`,
		`ALTER TABLE workers ADD COLUMN filters TEXT DEFAULT ''`,
		`ALTER TABLE workers ADD COLUMN pix_fmts TEXT DEFAULT ''`,
		`ALTER TABLE workers ADD COLUMN formats TEXT DEFAULT ''`,
	} {
		if _, err := d.db.Exec(stmt); err != nil {
			errStr := err.Error()
			// SQLite error: "duplicate column name: ..." — safe to ignore
			if !strings.Contains(strings.ToLower(errStr), "duplicate column") {
				return err
			}
		}
	}

	// TSI-2886: jobs.timeout must hold a nanosecond duration (INTEGER). Fresh
	// databases already create it INTEGER, but legacy databases declared it
	// DATETIME — which makes the sqlite driver read INTEGER values back as
	// time.Time (treating them as Unix seconds) and corrupt a nanosecond
	// budget. Re-declare the column on legacy databases (see
	// migrateTimeoutColumnType); fresh databases skip this entirely.
	var timeoutDecl string
	if err := d.db.QueryRow(`SELECT type FROM pragma_table_info('jobs') WHERE name = 'timeout'`).Scan(&timeoutDecl); err != nil {
		return fmt.Errorf("inspect jobs.timeout column: %w", err)
	}
	if !strings.EqualFold(timeoutDecl, "INTEGER") {
		if err := d.migrateTimeoutColumnType(); err != nil {
			return err
		}
	}

	return err
}

// migrateTimeoutColumnType re-declares jobs.timeout from DATETIME to INTEGER
// (TSI-2886). It runs inside a transaction so a crash cannot leave the column
// half-renamed. In-flight jobs carrying a future RFC3339 absolute deadline keep
// their remaining budget in nanoseconds; expired and terminal deadlines are
// dropped (their job is done or would fail anyway).
func (d *Database) migrateTimeoutColumnType() error {
	tx, err := d.db.Begin()
	if err != nil {
		return fmt.Errorf("begin timeout column migration: %w", err)
	}
	defer tx.Rollback()

	steps := []struct {
		stmt string
		args []interface{}
	}{
		{`ALTER TABLE jobs RENAME COLUMN timeout TO timeout_legacy`, nil},
		{`ALTER TABLE jobs ADD COLUMN timeout INTEGER`, nil},
		{
			`UPDATE jobs SET timeout = CAST((julianday(timeout_legacy) - julianday('now')) * 86400000000000 AS INTEGER)
			 WHERE typeof(timeout_legacy) = 'text'
			   AND status IN (?, ?, ?)
			   AND (julianday(timeout_legacy) - julianday('now')) > 0`,
			[]interface{}{protocol.JobStatusPending, protocol.JobStatusQueued, protocol.JobStatusRunning},
		},
		{`ALTER TABLE jobs DROP COLUMN timeout_legacy`, nil},
	}
	for _, s := range steps {
		if _, err := tx.Exec(s.stmt, s.args...); err != nil {
			return fmt.Errorf("timeout column migration: %w", err)
		}
	}
	return tx.Commit()
}

// CreateJob creates a new job record
func (d *Database) CreateJob(inputFiles, args, outputFilename string, autoHW bool) (*Job, error) {
	return d.CreateJobWithStreaming(inputFiles, args, outputFilename, autoHW, false, nil, "")
}

// CreateJobWithStreaming creates a new job record with streaming output option and optional direct paths
func (d *Database) CreateJobWithStreaming(inputFiles, args, outputFilename string, autoHW bool, streamingOutput bool, timeout *time.Duration, directPaths string) (*Job, error) {
	id := uuid.New().String()
	now := time.Now()

	var timeoutVal interface{}
	if timeout != nil {
		timeoutVal = int64(*timeout)
	}

	_, err := d.db.Exec(`
		INSERT INTO jobs (id, status, input_files, args, output_filename, streaming_output, output_files, auto_hw, timeout, direct_paths, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, '[]', ?, ?, ?, ?, ?)
	`, id, protocol.JobStatusPending, inputFiles, args, outputFilename, streamingOutput, autoHW, timeoutVal, directPaths, now, now)

	if err != nil {
		return nil, fmt.Errorf("failed to create job: %w", err)
	}

	return d.GetJob(id)
}

// CreateFailedJob persists a job that is terminal at submission — no worker
// can serve it (e.g. ENCODER_UNSUPPORTED). A single INSERT writes the job
// directly in the failed state, so a deterministic submit-time rejection is
// recorded atomically: there is no pending→failed window in which a crash
// would leave the job pending and later failed by the starvation sweep as
// NO_WORKER_AVAILABLE with a classification that contradicts the response
// (TSI-2846).
func (d *Database) CreateFailedJob(inputFiles, args, outputFilename string, autoHW bool, streamingOutput bool, timeout *time.Duration, directPaths, failureType, errMsg string) (*Job, error) {
	id := uuid.New().String()
	now := time.Now()

	var timeoutVal interface{}
	if timeout != nil {
		timeoutVal = int64(*timeout)
	}

	const exitCode = -1
	_, err := d.db.Exec(`
		INSERT INTO jobs (id, status, input_files, args, output_filename, streaming_output, output_files, exit_code, error, failure_type, auto_hw, timeout, direct_paths, created_at, updated_at, finished_at)
		VALUES (?, ?, ?, ?, ?, ?, '[]', ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, id, protocol.JobStatusFailed, inputFiles, args, outputFilename, streamingOutput, exitCode, errMsg, failureType, autoHW, timeoutVal, directPaths, now, now, now)

	if err != nil {
		return nil, fmt.Errorf("failed to create failed job: %w", err)
	}

	// A terminal-state write must notify in-process DB waiters (TSI-2562),
	// matching every other terminal writer (UpdateJob, CancelJob, FailJob,
	// starvation sweep).
	d.notifyTerminal(id)

	return d.GetJob(id)
}

// GetJob retrieves a job by ID
func (d *Database) GetJob(id string) (*Job, error) {
	job := &Job{}
	err := d.db.QueryRow(`
		SELECT id, status, input_files, args, output_filename, streaming_output, output_files, worker_id, exit_code, error,
	       failure_type, failure_details, auto_hw, cached, timeout, direct_paths, progress_percent, eta_seconds,
	       created_at, updated_at, started_at, finished_at, assigned_worker, worker_name
		FROM jobs WHERE id = ?
	`, id).Scan(
		&job.ID, &job.Status, &job.InputFiles, &job.Args, &job.OutputFilename, &job.StreamingOutput, &job.OutputFiles,
		&job.WorkerID, &job.ExitCode, &job.Error, &job.FailureType, &job.FailureDetails, &job.AutoHW, &job.Cached,
		&job.Timeout, &job.DirectPaths, &job.ProgressPercent, &job.EtaSeconds,
		&job.CreatedAt, &job.UpdatedAt, &job.StartedAt, &job.FinishedAt, &job.AssignedWorker, &job.WorkerName,
	)

	if err == sql.ErrNoRows {
		return nil, protocol.ErrJobNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get job: %w", err)
	}

	return job, nil
}

// JobExists checks if a job with the given ID exists
func (d *Database) JobExists(id string) (bool, error) {
	var exists bool
	err := d.db.QueryRow(`
		SELECT EXISTS(SELECT 1 FROM jobs WHERE id = ?)
	`, id).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("failed to check job existence: %w", err)
	}
	return exists, nil
}

// UpdateJobStatusWithFailure updates the job status along with failure
// classification fields. Non-failure fields are ignored when the pointers are nil.
//
// TSI-2359: the update is guarded against stale writes. A late running
// report (or a stale cancel racing a worker's completion) must never
// resurrect or overwrite a finished job. The one legal terminal→terminal
// transition is failed→completed/failed: a worker retries the job after a
// failure and reports success (handlers clear stale failure metadata on
// completed), so that path stays open. Everything else hitting a terminal
// job is dropped and reported as ErrJobTerminal.
func (d *Database) UpdateJobStatusWithFailure(id string, status protocol.JobStatus, exitCode *int, errMsg *string, failureType, failureDetails *string) error {
	return d.updateJobStatusWithFailure(id, status, exitCode, errMsg, failureType, failureDetails, false)
}

// UpdateJobStatusWithFailureAndCache is UpdateJobStatusWithFailure with the
// cached flag persisted on terminal completion (TSI-2519). cached is coerced
// to false for any status other than completed: the column records whether a
// completed result was served from the worker cache.
func (d *Database) UpdateJobStatusWithFailureAndCache(id string, status protocol.JobStatus, exitCode *int, errMsg *string, failureType, failureDetails *string, cached bool) error {
	return d.updateJobStatusWithFailure(id, status, exitCode, errMsg, failureType, failureDetails, cached)
}

func (d *Database) updateJobStatusWithFailure(id string, status protocol.JobStatus, exitCode *int, errMsg *string, failureType, failureDetails *string, cached bool) error {
	now := time.Now()

	// TSI-2519 review: cached describes a completed result served from the
	// worker cache. PATCH /api/v1/jobs/{id} is public, so a client can craft
	// {status:"failed", cached:true} — coerce the flag to only completed hits.
	if status != protocol.JobStatusCompleted {
		cached = false
	}

	var startedAt, finishedAt *time.Time
	if status == protocol.JobStatusRunning {
		startedAt = &now
	}
	if protocol.IsTerminalStatus(status) {
		finishedAt = &now
	}

	// TSI-2359: reject updates that would overwrite a terminal outcome.
	// failed→completed / failed→failed remains allowed for the worker
	// retry-after-failure flow; all other terminal targets are frozen.
	var result sql.Result
	var err error
	if status == protocol.JobStatusCompleted || status == protocol.JobStatusFailed {
		result, err = d.db.Exec(`
			UPDATE jobs SET status = ?, updated_at = ?, exit_code = ?, error = ?,
			                failure_type = COALESCE(?, failure_type),
			                failure_details = COALESCE(?, failure_details),
			                started_at = COALESCE(started_at, ?), finished_at = ?,
			                cached = ?,
			                assigned_worker = COALESCE(assigned_worker, worker_id),
			                worker_name = COALESCE(worker_name, (SELECT name FROM workers WHERE id = jobs.worker_id))
			WHERE id = ?
			  AND NOT EXISTS (
			      SELECT 1 FROM jobs WHERE id = ? AND status IN (?, ?, ?)
			  )
		`, status, now, exitCode, errMsg, failureType, failureDetails,
			startedAt, finishedAt, cached, id, id,
			protocol.JobStatusCompleted, protocol.JobStatusCancelled, protocol.JobStatusTimeout)
	} else {
		result, err = d.db.Exec(`
			UPDATE jobs SET status = ?, updated_at = ?, exit_code = ?, error = ?,
			                failure_type = COALESCE(?, failure_type),
			                failure_details = COALESCE(?, failure_details),
			                started_at = COALESCE(started_at, ?), finished_at = ?
			WHERE id = ?
			  AND NOT EXISTS (
			      SELECT 1 FROM jobs WHERE id = ? AND status IN (?, ?, ?, ?)
			  )
		`, status, now, exitCode, errMsg, failureType, failureDetails,
			startedAt, finishedAt, id, id,
			protocol.JobStatusCompleted, protocol.JobStatusFailed,
			protocol.JobStatusCancelled, protocol.JobStatusTimeout)
	}

	if err != nil {
		return fmt.Errorf("failed to update job status: %w", err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to check rows affected: %w", err)
	}

	if rows == 0 {
		// Distinguish "job missing" from "job already terminal" so callers
		// can treat a lost race (late report vs. concurrent cancel) as a
		// no-op instead of an error.
		if _, getErr := d.GetJob(id); getErr != nil {
			return protocol.ErrJobNotFound
		}
		return protocol.ErrJobTerminal
	}

	if protocol.IsTerminalStatus(status) {
		d.notifyTerminal(id)
	}

	return nil
}

// UpdateJobTerminalStatusWithOwner updates a job to a terminal state only if
// the job is still owned by the reporting worker and still active (running or
// queued). This closes the double-execution window: after failover the job's
// worker_id points at the new worker, so a stale terminal report from the old
// worker (network partition survivor) matches zero rows and returns
// protocol.ErrJobNotOwned instead of overwriting the new owner's result.
// A report for a job already in a terminal state is equally rejected.
func (d *Database) UpdateJobTerminalStatusWithOwner(jobID, workerID string, status protocol.JobStatus, exitCode *int, errMsg *string, failureType, failureDetails *string) error {
	return d.updateJobTerminalStatusWithOwner(jobID, workerID, status, exitCode, errMsg, failureType, failureDetails, false)
}

// UpdateJobTerminalStatusWithOwnerAndCache is UpdateJobTerminalStatusWithOwner
// with the cached flag persisted on the terminal transition (TSI-2519).
// cached is coerced to false unless the target status is completed.
func (d *Database) UpdateJobTerminalStatusWithOwnerAndCache(jobID, workerID string, status protocol.JobStatus, exitCode *int, errMsg *string, failureType, failureDetails *string, cached bool) error {
	return d.updateJobTerminalStatusWithOwner(jobID, workerID, status, exitCode, errMsg, failureType, failureDetails, cached)
}

func (d *Database) updateJobTerminalStatusWithOwner(jobID, workerID string, status protocol.JobStatus, exitCode *int, errMsg *string, failureType, failureDetails *string, cached bool) error {
	now := time.Now()

	// TSI-2519 review: cached means a completed result served from cache.
	if status != protocol.JobStatusCompleted {
		cached = false
	}
	finishedAt := now
	result, err := d.db.Exec(`
		UPDATE jobs SET status = ?, updated_at = ?, exit_code = ?,
		                error = ?,
		                failure_type = COALESCE(?, failure_type),
		                failure_details = COALESCE(?, failure_details),
		                finished_at = ?, cached = ?,
		                assigned_worker = COALESCE(assigned_worker, ?),
		                worker_name = COALESCE(worker_name, (SELECT name FROM workers WHERE id = ?))
		WHERE id = ? AND worker_id = ? AND status IN (?, ?)
	`, status, now, exitCode, errMsg, failureType, failureDetails, finishedAt, cached,
		workerID, workerID, jobID, workerID, protocol.JobStatusRunning, protocol.JobStatusQueued)
	if err != nil {
		return fmt.Errorf("failed to update job terminal status: %w", err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to check rows affected: %w", err)
	}
	if rows == 0 {
		return protocol.ErrJobNotOwned
	}
	d.notifyTerminal(jobID)
	return nil
}

// UpdateJobProgress updates the progress and ETA seconds for a job.
func (d *Database) UpdateJobProgress(id string, progress float64, etaSeconds int) error {
	now := time.Now()

	_, err := d.db.Exec(`
		UPDATE jobs SET progress_percent = ?, eta_seconds = ?, updated_at = ?
		WHERE id = ?
	`, progress, etaSeconds, now, id)

	if err != nil {
		return fmt.Errorf("failed to update job progress: %w", err)
	}

	return nil
}

// AssignJobToWorker atomically assigns a job to a worker: one statement sets
// worker_id AND flips the status to queued. The previous two-step version
// (worker_id write, then queued status) left a pending+worker_id orphan when
// it failed in between — such a job was invisible to both the scheduler
// (status=pending scan) and FailStarvedPendingJobs (WHERE worker_id IS NULL),
// hanging CLI clients forever. Returns ErrJobNotOwned if the job is no longer
// schedulable (already assigned, or not pending).
func (d *Database) AssignJobToWorker(jobID, workerID string) error {
	now := time.Now()
	result, err := d.db.Exec(`
		UPDATE jobs SET worker_id = ?, assigned_worker = ?, worker_name = (SELECT name FROM workers WHERE id = ?), status = ?, updated_at = ?
		WHERE id = ? AND status = ? AND worker_id IS NULL
	`, workerID, workerID, workerID, protocol.JobStatusQueued, now, jobID, protocol.JobStatusPending)
	if err != nil {
		return fmt.Errorf("failed to assign job to worker: %w", err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to check rows affected: %w", err)
	}
	if rows == 0 {
		return protocol.ErrJobNotOwned
	}
	return nil
}

// CancelJob cancels a job if it's in a cancellable state
// Jobs can be cancelled if they are pending, queued, or running.
//
// TSI-2359: the state check and the update are a single conditional UPDATE,
// so a worker completing the job between the check and the write can no longer
// have its terminal status overwritten by a stale cancel (TOCTOU fix).
func (d *Database) CancelJob(id string) error {
	now := time.Now()
	result, err := d.db.Exec(`
		UPDATE jobs SET status = ?, updated_at = ?, finished_at = ?
		WHERE id = ? AND status IN (?, ?, ?)
	`, protocol.JobStatusCancelled, now, now, id,
		protocol.JobStatusPending, protocol.JobStatusQueued, protocol.JobStatusRunning)
	if err != nil {
		return fmt.Errorf("failed to cancel job: %w", err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to check rows affected: %w", err)
	}
	if rows == 0 {
		job, getErr := d.GetJob(id)
		if getErr != nil {
			return protocol.ErrJobNotFound
		}
		return fmt.Errorf("cannot cancel job in status %s", job.Status)
	}

	d.notifyTerminal(id)
	return nil
}

// UpdateJobOutput updates the output files for a job
func (d *Database) UpdateJobOutput(id string, outputFiles string) error {
	now := time.Now()
	_, err := d.db.Exec(`
		UPDATE jobs SET output_files = ?, updated_at = ? WHERE id = ?
	`, outputFiles, now, id)

	if err != nil {
		return fmt.Errorf("failed to update job output: %w", err)
	}

	return nil
}

// GetPendingJobs retrieves all pending jobs
func (d *Database) GetPendingJobs(limit int) ([]*Job, error) {
	rows, err := d.db.Query(`
		SELECT id, status, input_files, args, output_filename, streaming_output, output_files, worker_id, exit_code, error, failure_type, failure_details, auto_hw, cached, timeout, direct_paths, progress_percent, eta_seconds,
		       created_at, updated_at, started_at, finished_at, assigned_worker, worker_name
		FROM jobs WHERE status = ?
		ORDER BY created_at ASC LIMIT ?
	`, protocol.JobStatusPending, limit)

	if err != nil {
		return nil, fmt.Errorf("failed to get pending jobs: %w", err)
	}
	defer rows.Close()

	return d.scanJobs(rows)
}

// CreateFile creates a new file record
func (d *Database) CreateFile(filename, path string, size int64, checksum *string) (*File, error) {
	id := uuid.New().String()
	now := time.Now()

	var checksumVal interface{}
	if checksum != nil {
		checksumVal = *checksum
	}

	_, err := d.db.Exec(`
		INSERT INTO files (id, filename, path, size, checksum, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
	`, id, filename, path, size, checksumVal, now)

	if err != nil {
		return nil, fmt.Errorf("failed to create file: %w", err)
	}

	return d.GetFile(id)
}

// GetFile retrieves a file by ID
func (d *Database) GetFile(id string) (*File, error) {
	file := &File{}
	err := d.db.QueryRow(`
		SELECT id, filename, path, size, checksum, created_at
		FROM files WHERE id = ?
	`, id).Scan(
		&file.ID, &file.Filename, &file.Path, &file.Size,
		&file.Checksum, &file.CreatedAt,
	)

	if err == sql.ErrNoRows {
		return nil, protocol.ErrInvalidFileID
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get file: %w", err)
	}

	return file, nil
}

// scanJobs scans multiple job rows
func (d *Database) scanJobs(rows *sql.Rows) ([]*Job, error) {
	var jobs []*Job
	for rows.Next() {
		job := &Job{}
		err := rows.Scan(
			&job.ID, &job.Status, &job.InputFiles, &job.Args, &job.OutputFilename, &job.StreamingOutput, &job.OutputFiles,
			&job.WorkerID, &job.ExitCode, &job.Error, &job.FailureType, &job.FailureDetails, &job.AutoHW, &job.Cached,
			&job.Timeout, &job.DirectPaths, &job.ProgressPercent, &job.EtaSeconds,
			&job.CreatedAt, &job.UpdatedAt, &job.StartedAt, &job.FinishedAt, &job.AssignedWorker, &job.WorkerName,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan job: %w", err)
		}
		jobs = append(jobs, job)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating jobs: %w", err)
	}

	return jobs, nil
}

// CreateWorker creates a new worker record
func (d *Database) CreateWorker(id, name string, caps protocol.WorkerCapabilities) (*Worker, error) {
	if id == "" {
		id = uuid.New().String()
	}
	now := time.Now()

	encodersJSON, err := json.Marshal(caps.Encoders)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal encoders: %w", err)
	}
	decodersJSON, err := json.Marshal(caps.Decoders)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal decoders: %w", err)
	}
	videoEncodersJSON, err := json.Marshal(caps.VideoEncoders)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal video encoders: %w", err)
	}
	videoDecodersJSON, err := json.Marshal(caps.VideoDecoders)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal video decoders: %w", err)
	}

	var gpuModel interface{}
	if caps.GPUModel != "" {
		gpuModel = caps.GPUModel
	}

	_, err = d.db.Exec(`
		INSERT INTO workers (id, name, status, gpu_model, encoders, decoders, video_encoders, video_decoders, ffmpeg_version, max_concurrent, evicted, evicted_at, hwaccels, codecs, filters, pix_fmts, formats, last_heartbeat, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, id, name, protocol.WorkerStatusIdle, gpuModel, string(encodersJSON), string(decodersJSON),
		string(videoEncodersJSON), string(videoDecodersJSON),
		caps.FFmpegVersion, caps.MaxConcurrent, false, nil,
		caps.Hwaccels, caps.Codecs, caps.Filters, caps.PixFmts, caps.Formats,
		now, now)

	if err != nil {
		return nil, fmt.Errorf("failed to create worker: %w", err)
	}

	return d.GetWorker(id)
}

// CreateOrUpdateWorker creates a new worker or updates an existing one on re-registration.
// If the worker already exists (matched by id), it updates capabilities, resets status to
// idle, and clears eviction flags (TSI-1737 server restart recovery). When the id is new
// but the name matches stale residue of the same logical worker — an offline row, or a
// live row whose heartbeat has expired (crashed before the health monitor swept it) — that
// stale row is deleted first so a worker process restart (fresh UUID, same name) overwrites
// the old entry instead of creating a duplicate (TSI-2473, TSI-2670). Live same-name rows
// with a fresh heartbeat belong to concurrently running workers and are never deleted.
//
// heartbeatTimeout is the worker freshness window; <=0 disables the heartbeat-staleness
// test and deletes only offline residue.
func (d *Database) CreateOrUpdateWorker(id, name string, caps protocol.WorkerCapabilities, heartbeatTimeout time.Duration) (*Worker, error) {
	// TSI-2346: normalize on the write path too, so the same logical UUID
	// reported in different formats (hyphenated vs. compact, case, whitespace)
	// converges on one row instead of forking into unreachable duplicates.
	id = NormalizeWorkerID(id)
	if id == "" {
		id = uuid.New().String()
	}
	now := time.Now()

	encodersJSON, err := json.Marshal(caps.Encoders)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal encoders: %w", err)
	}
	decodersJSON, err := json.Marshal(caps.Decoders)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal decoders: %w", err)
	}
	videoEncodersJSON, err := json.Marshal(caps.VideoEncoders)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal video encoders: %w", err)
	}
	videoDecodersJSON, err := json.Marshal(caps.VideoDecoders)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal video decoders: %w", err)
	}

	var gpuModel interface{}
	if caps.GPUModel != "" {
		gpuModel = caps.GPUModel
	}

	// Try UPDATE first (handles re-registration after server restart)
	result, err := d.db.Exec(`
		UPDATE workers SET name=?, status=?, gpu_model=?, encoders=?, decoders=?,
		video_encoders=?, video_decoders=?, ffmpeg_version=?, max_concurrent=?,
		evicted=0, evicted_at=NULL, hwaccels=?, codecs=?, filters=?, pix_fmts=?, formats=?,
		last_heartbeat=? WHERE id=?
	`, name, protocol.WorkerStatusIdle, gpuModel,
		string(encodersJSON), string(decodersJSON),
		string(videoEncodersJSON), string(videoDecodersJSON),
		caps.FFmpegVersion, caps.MaxConcurrent,
		caps.Hwaccels, caps.Codecs, caps.Filters, caps.PixFmts, caps.Formats,
		now, id)
	if err != nil {
		return nil, fmt.Errorf("failed to update worker: %w", err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("failed to check rows affected: %w", err)
	}

	if rows == 0 {
		// TSI-2473/TSI-2670: a new worker process generates a fresh UUID while
		// reusing the same name. The old row is stale residue of the same
		// logical worker and must be deleted before the INSERT so a restart
		// overwrites it instead of forking a duplicate. Stale means EITHER
		// offline (the health monitor already flagged it) OR live-but-dead
		// (last heartbeat older than the freshness window — a crash followed
		// by an immediate restart lands here before the monitor's next tick).
		// A live row with a fresh heartbeat is a concurrently running worker
		// and must survive. Wrap DELETE+INSERT in a transaction so a failed
		// INSERT rolls back the DELETE. Skip the DELETE for empty names: the
		// handler rejects empty names, but defending here too avoids deleting
		// unrelated empty-name rows.
		tx, err := d.db.Begin()
		if err != nil {
			return nil, fmt.Errorf("failed to begin worker upsert transaction: %w", err)
		}
		defer tx.Rollback() //nolint:errcheck

		if name != "" {
			if heartbeatTimeout > 0 {
				cutoff := now.Add(-heartbeatTimeout)
				if _, err := tx.Exec(`DELETE FROM workers WHERE name = ? AND id != ? AND (status = ? OR last_heartbeat < ?)`,
					name, id, protocol.WorkerStatusOffline, cutoff); err != nil {
					return nil, fmt.Errorf("failed to remove stale same-name worker: %w", err)
				}
			} else {
				if _, err := tx.Exec(`DELETE FROM workers WHERE name = ? AND id != ? AND status = ?`,
					name, id, protocol.WorkerStatusOffline); err != nil {
					return nil, fmt.Errorf("failed to remove stale same-name worker: %w", err)
				}
			}
		}

		// Fallback: INSERT new worker
		_, err = tx.Exec(`
			INSERT INTO workers (id, name, status, gpu_model, encoders, decoders,
			video_encoders, video_decoders, ffmpeg_version, max_concurrent, evicted,
			evicted_at, hwaccels, codecs, filters, pix_fmts, formats, last_heartbeat, created_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		`, id, name, protocol.WorkerStatusIdle, gpuModel,
			string(encodersJSON), string(decodersJSON),
			string(videoEncodersJSON), string(videoDecodersJSON),
			caps.FFmpegVersion, caps.MaxConcurrent, false, nil,
			caps.Hwaccels, caps.Codecs, caps.Filters, caps.PixFmts, caps.Formats,
			now, now)
		if err != nil {
			return nil, fmt.Errorf("failed to create worker: %w", err)
		}

		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("failed to commit worker upsert: %w", err)
		}
	}

	return d.GetWorker(id)
}

// NormalizeWorkerID canonicalizes a worker ID so that lookups and ownership
// comparisons tolerate UUID formatting differences (hyphenated vs. compact).
// Non-UUID IDs pass through unchanged, since registration accepts arbitrary
// identifiers (TSI-2346).
func NormalizeWorkerID(id string) string {
	if parsed, err := uuid.Parse(strings.TrimSpace(id)); err == nil {
		return parsed.String()
	}
	return strings.TrimSpace(id)
}

// GetWorker retrieves a worker by ID
func (d *Database) GetWorker(id string) (*Worker, error) {
	id = NormalizeWorkerID(id)
	worker := &Worker{}
	err := d.db.QueryRow(`
		SELECT id, name, status, gpu_model, encoders, decoders, video_encoders, video_decoders, ffmpeg_version, max_concurrent, evicted, evicted_at, hwaccels, codecs, filters, pix_fmts, formats, last_heartbeat, created_at
		FROM workers WHERE id = ?
	`, id).Scan(
		&worker.ID, &worker.Name, &worker.Status, &worker.GPUModel,
		&worker.Encoders, &worker.Decoders, &worker.VideoEncoders, &worker.VideoDecoders, &worker.FFmpegVersion,
		&worker.MaxConcurrent, &worker.Evicted, &worker.EvictedAt,
		&worker.Hwaccels, &worker.Codecs, &worker.Filters, &worker.PixFmts, &worker.Formats,
		&worker.LastHeartbeat, &worker.CreatedAt,
	)

	if err == sql.ErrNoRows {
		return nil, protocol.ErrWorkerNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get worker: %w", err)
	}

	return worker, nil
}

// UpdateWorkerHeartbeat refreshes a live worker's heartbeat timestamp without
// touching its status. Worker status is derived state (active job count +
// terminal-state hooks), not worker-authoritative: honoring the reported
// status here let a late idle heartbeat overwrite the busy state written by
// the pull path, resurrecting TSI-2347 through a race. Offline/evicted guards
// keep dead workers from refreshing liveness.
func (d *Database) UpdateWorkerHeartbeat(id string, status protocol.WorkerStatus) error {
	// Same canonicalization as registration/lookup (TSI-2346): a heartbeat
	// carrying a non-canonical UUID format (compact/hyphenated variant) must
	// hit the same row the worker registered under.
	id = NormalizeWorkerID(id)
	now := time.Now()
	result, err := d.db.Exec(`
		UPDATE workers SET last_heartbeat = ? WHERE id = ? AND status != ?
	`, now, NormalizeWorkerID(id), protocol.WorkerStatusOffline)

	if err != nil {
		return fmt.Errorf("failed to update worker heartbeat: %w", err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to check rows affected: %w", err)
	}
	if rows == 0 {
		return protocol.ErrWorkerNotFound
	}

	return nil
}

// SetWorkerIdleIfNoActiveJobs flips an online worker to idle only when it has
// no active jobs and is not offline. This is the terminal-state hook of the
// derived-status invariant: an offline worker's late completion report can no
// longer resurrect it into the schedulable pool (zombie revival), and a
// racing new assignment (which sets busy) cannot be overwritten to idle
// because the active-count predicate fails.
func (d *Database) SetWorkerIdleIfNoActiveJobs(workerID string) error {
	result, err := d.db.Exec(`
		UPDATE workers SET status = ?, last_heartbeat = ?
		WHERE id = ? AND status != ? AND status != ?
		  AND NOT EXISTS (
			SELECT 1 FROM jobs
			WHERE jobs.worker_id = workers.id AND jobs.status IN (?, ?)
		  )
	`, protocol.WorkerStatusIdle, time.Now(), workerID,
		protocol.WorkerStatusIdle, protocol.WorkerStatusOffline,
		protocol.JobStatusRunning, protocol.JobStatusQueued)
	if err != nil {
		return fmt.Errorf("failed to set worker idle: %w", err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to check rows affected: %w", err)
	}
	if rows == 0 {
		return protocol.ErrWorkerNotFound
	}
	return nil
}

// GetJobsForWorker retrieves jobs assigned to a specific worker with pending or queued status
func (d *Database) GetJobsForWorker(workerID string, limit int) ([]*Job, error) {
	workerID = NormalizeWorkerID(workerID)
	rows, err := d.db.Query(`
		SELECT id, status, input_files, args, output_filename, streaming_output, output_files, worker_id, exit_code, error, failure_type, failure_details, auto_hw, cached, timeout, direct_paths, progress_percent, eta_seconds,
		       created_at, updated_at, started_at, finished_at, assigned_worker, worker_name
		FROM jobs WHERE worker_id = ? AND status IN (?, ?)
		ORDER BY created_at ASC LIMIT ?
	`, workerID, protocol.JobStatusPending, protocol.JobStatusQueued, limit)

	if err != nil {
		return nil, fmt.Errorf("failed to get jobs for worker: %w", err)
	}
	defer rows.Close()

	return d.scanJobs(rows)
}

// AssignPendingJobsToWorker assigns pending jobs to a worker and returns jobs already assigned to it
func (d *Database) AssignPendingJobsToWorker(workerID string, maxJobs int) ([]*Job, error) {
	// Normalize so a compact/uppercase UUID pull reaches jobs stored under the
	// canonical ID; GetJobsForWorker normalizes its own copy too.
	workerID = NormalizeWorkerID(workerID)
	// First, get jobs already assigned to this worker (queued status)
	// This handles the race condition where the scheduler assigned jobs before the worker polled
	queuedJobs, err := d.GetJobsForWorker(workerID, maxJobs)
	if err != nil {
		return nil, fmt.Errorf("failed to get queued jobs for worker: %w", err)
	}

	// If we already have enough jobs, return them
	if len(queuedJobs) >= maxJobs {
		return queuedJobs[:maxJobs], nil
	}

	// Get pending jobs and assign them to the worker
	tx, err := d.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	// Calculate how many more jobs we can assign
	remainingSlots := maxJobs - len(queuedJobs)

	// Get pending jobs (status = pending AND worker_id IS NULL)
	rows, err := tx.Query(`
		SELECT id, status, input_files, args, output_filename, streaming_output, output_files, worker_id, exit_code, error, failure_type, failure_details, auto_hw, cached, timeout, direct_paths, progress_percent, eta_seconds,
		       created_at, updated_at, started_at, finished_at, assigned_worker, worker_name
		FROM jobs WHERE status = ? AND worker_id IS NULL
		ORDER BY created_at ASC LIMIT ?
	`, protocol.JobStatusPending, remainingSlots)

	if err != nil {
		return nil, fmt.Errorf("failed to get pending jobs: %w", err)
	}

	newJobs, err := d.scanJobs(rows)
	if err != nil {
		return nil, err
	}

	// Fetch the worker name once so the claimed Job objects carry the same
	// attribution the UPDATE persists via subquery — the pull response is built
	// from these in-memory objects, and a re-read would otherwise return null
	// attribution for freshly claimed jobs (TSI-2920 review).
	workerName := ""
	if w, err := d.GetWorker(workerID); err == nil {
		workerName = w.Name
	}

	now := time.Now()
	claimedJobs := make([]*Job, 0, len(newJobs))
	for _, job := range newJobs {
		// TSI-2359: guard every claim with the exact preconditions read
		// above. Two workers pulling concurrently run this inside separate
		// write transactions; SQLite serializes them, and without the guard
		// the second transaction would blindly re-claim the same row. A
		// guarded UPDATE that matches 0 rows means the other worker won the
		// race — skip the job instead of double-dispatching it.
		result, err := tx.Exec(`
			UPDATE jobs SET worker_id = ?, assigned_worker = ?, worker_name = (SELECT name FROM workers WHERE id = ?), status = ?, updated_at = ?
			WHERE id = ? AND status = ? AND worker_id IS NULL
		`, workerID, workerID, workerID, protocol.JobStatusQueued, now, job.ID, protocol.JobStatusPending)
		if err != nil {
			return nil, fmt.Errorf("failed to assign job %s: %w", job.ID, err)
		}
		claimed, err := result.RowsAffected()
		if err != nil {
			return nil, fmt.Errorf("failed to check rows affected for job %s: %w", job.ID, err)
		}
		if claimed == 0 {
			continue // lost the race to another worker's pull
		}
		job.WorkerID = sql.NullString{String: workerID, Valid: true}
		job.AssignedWorker = sql.NullString{String: workerID, Valid: true}
		job.WorkerName = sql.NullString{String: workerName, Valid: workerName != ""}
		job.Status = protocol.JobStatusQueued
		claimedJobs = append(claimedJobs, job)
	}

	// TSI-2347: mark the worker busy atomically with the assignment so
	// health.status reflects activity immediately instead of waiting for the
	// next heartbeat. The idle guard keeps an offline worker offline.
	// last_heartbeat is deliberately NOT refreshed here: assignment is not
	// proof of liveness (the pull may come from a partitioned zombie), and
	// refreshing it postponed offline re-detection and lengthened the
	// double-execution window.
	if len(claimedJobs) > 0 {
		if _, err := tx.Exec(`
			UPDATE workers SET status = ? WHERE id = ? AND status = ?
		`, protocol.WorkerStatusBusy, workerID, protocol.WorkerStatusIdle); err != nil {
			return nil, fmt.Errorf("failed to mark worker busy: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("failed to commit transaction: %w", err)
	}

	// TSI-2929: write back the migration target for any claimed job that was
	// previously migrated, so the chain is resolvable from migration_events
	// alone. Best-effort audit write after commit: assignment is authoritative,
	// and a failure here must not fail the pull.
	for _, job := range claimedJobs {
		if err := d.RecordMigrationTarget(job.ID, workerID, workerName); err != nil {
			log.Printf("Failed to record migration target for job %s: %v", job.ID, err)
		}
	}

	// Combine queued jobs (already assigned) with newly claimed jobs
	return append(queuedJobs, claimedJobs...), nil
}

// MarkOfflineWorkers marks workers as offline if their last heartbeat exceeds the timeout
func (d *Database) MarkOfflineWorkers(heartbeatTimeout time.Duration) (int64, error) {
	cutoff := time.Now().Add(-heartbeatTimeout)
	result, err := d.db.Exec(`
		UPDATE workers SET status = ?
		WHERE status != ? AND last_heartbeat < ?
	`, protocol.WorkerStatusOffline, protocol.WorkerStatusOffline, cutoff)
	if err != nil {
		return 0, fmt.Errorf("failed to mark offline workers: %w", err)
	}
	return result.RowsAffected()
}

// RemoveOfflineWorkers removes offline workers whose last_heartbeat predates
// the cutoff (now - offlineThreshold) and returns their IDs so callers can
// evict them from in-memory state (the WorkerStateTable) — stale table entries
// previously kept feeding dead nodes into slow-node median calculations.
// MarkOfflineWorkers does not reset last_heartbeat, so the threshold is
// measured from the last heartbeat, not from when the worker was marked
// offline.
func (d *Database) RemoveOfflineWorkers(offlineThreshold time.Duration) ([]string, error) {
	cutoff := time.Now().Add(-offlineThreshold)

	// Single transaction: the SELECT-then-DELETE version had a TOCTOU window
	// where a worker deleted by the DELETE missed the returned ID list (its
	// state-table entry then leaked) or a freshly re-registered row got
	// swept. Inside a transaction, the read set and delete set are identical.
	tx, err := d.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	rows, err := tx.Query(`
		SELECT id FROM workers
		WHERE status = ? AND last_heartbeat < ?
	`, protocol.WorkerStatusOffline, cutoff)
	if err != nil {
		return nil, fmt.Errorf("failed to query removed workers: %w", err)
	}

	var workerIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, fmt.Errorf("failed to scan worker id: %w", err)
		}
		workerIDs = append(workerIDs, id)
	}
	rows.Close()

	if len(workerIDs) == 0 {
		return nil, nil
	}

	result, err := tx.Exec(`
		DELETE FROM workers
		WHERE status = ? AND last_heartbeat < ?
	`, protocol.WorkerStatusOffline, cutoff)
	if err != nil {
		return nil, fmt.Errorf("failed to remove offline workers: %w", err)
	}

	if _, err := result.RowsAffected(); err != nil {
		return nil, fmt.Errorf("failed to check rows affected: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("failed to commit transaction: %w", err)
	}
	return workerIDs, nil
}

// GetActiveWorkers retrieves all workers that are not offline
func (d *Database) GetActiveWorkers() ([]*Worker, error) {
	rows, err := d.db.Query(`
		SELECT id, name, status, gpu_model, encoders, decoders, video_encoders, video_decoders, ffmpeg_version, max_concurrent, evicted, evicted_at, hwaccels, codecs, filters, pix_fmts, formats, last_heartbeat, created_at
		FROM workers WHERE status != ?
		ORDER BY created_at ASC
	`, protocol.WorkerStatusOffline)
	if err != nil {
		return nil, fmt.Errorf("failed to get active workers: %w", err)
	}
	defer rows.Close()

	return d.scanWorkers(rows)
}

// GetIdleWorkers retrieves all workers that are idle and can accept jobs.
// Evicted workers are excluded.
func (d *Database) GetIdleWorkers() ([]*Worker, error) {
	rows, err := d.db.Query(`
		SELECT id, name, status, gpu_model, encoders, decoders, video_encoders, video_decoders, ffmpeg_version, max_concurrent, evicted, evicted_at, hwaccels, codecs, filters, pix_fmts, formats, last_heartbeat, created_at
		FROM workers WHERE status = ? AND evicted = 0
		ORDER BY last_heartbeat DESC
	`, protocol.WorkerStatusIdle)
	if err != nil {
		return nil, fmt.Errorf("failed to get idle workers: %w", err)
	}
	defer rows.Close()

	return d.scanWorkers(rows)
}

// GetSchedulableWorkers retrieves all workers that are not offline and not evicted.
// Unlike GetIdleWorkers, busy workers are included so a submitted job can be queued
// while a worker is currently busy (TSI-2204).
//
// The result is NOT filtered by heartbeat freshness: a worker whose last
// heartbeat is stale but that has not yet been marked offline still counts as
// schedulable. Callers that need liveness (submit-time fail-fast, starvation
// sweeps) must use GetLiveSchedulableWorkers instead (TSI-2419).
func (d *Database) GetSchedulableWorkers() ([]*Worker, error) {
	rows, err := d.db.Query(`
		SELECT id, name, status, gpu_model, encoders, decoders, video_encoders, video_decoders, ffmpeg_version, max_concurrent, evicted, evicted_at, hwaccels, codecs, filters, pix_fmts, formats, last_heartbeat, created_at
		FROM workers WHERE status != ? AND evicted = 0
		ORDER BY last_heartbeat DESC
	`, protocol.WorkerStatusOffline)
	if err != nil {
		return nil, fmt.Errorf("failed to get schedulable workers: %w", err)
	}
	defer rows.Close()

	return d.scanWorkers(rows)
}

// GetLiveSchedulableWorkers returns schedulable workers with a fresh
// heartbeat: not offline, not evicted, and last_heartbeat within freshness.
// A worker whose heartbeat is older than the freshness window is dead in
// practice — the health monitor only flips it offline on its next tick — so
// callers must not treat it as available (TSI-2419). A non-positive
// freshness disables the check (behaves like GetSchedulableWorkers).
func (d *Database) GetLiveSchedulableWorkers(freshness time.Duration) ([]*Worker, error) {
	freshnessClause, freshnessArgs := heartbeatFreshnessSQL(freshness)
	rows, err := d.querySchedulable(`
		SELECT id, name, status, gpu_model, encoders, decoders, video_encoders, video_decoders, ffmpeg_version, max_concurrent, evicted, evicted_at, hwaccels, codecs, filters, pix_fmts, formats, last_heartbeat, created_at
		FROM workers WHERE status != ? AND evicted = 0`+freshnessClause+`
		ORDER BY last_heartbeat DESC
	`, append([]any{protocol.WorkerStatusOffline}, freshnessArgs...)...)
	if err != nil {
		return nil, fmt.Errorf("failed to get live schedulable workers: %w", err)
	}
	return rows, nil
}

// GetWorkerActiveJobCount returns the number of active jobs (running or queued) for a worker
func (d *Database) GetWorkerActiveJobCount(workerID string) (int, error) {
	var count int
	err := d.db.QueryRow(`
		SELECT COUNT(*) FROM jobs WHERE worker_id = ? AND status IN (?, ?)
	`, workerID, protocol.JobStatusRunning, protocol.JobStatusQueued).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("failed to get active job count: %w", err)
	}
	return count, nil
}

// GetWorkerCompletedJobCount returns the number of completed jobs for a worker.
// The scheduler uses it to break ties between idle workers with equal active
// counts so the worker that has done fewer jobs is preferred, preventing
// starvation of late-registered workers (TSI-2477).
func (d *Database) GetWorkerCompletedJobCount(workerID string) (int, error) {
	var count int
	err := d.db.QueryRow(`
		SELECT COUNT(*) FROM jobs WHERE worker_id = ? AND status = ?
	`, workerID, protocol.JobStatusCompleted).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("failed to get completed job count: %w", err)
	}
	return count, nil
}

func (d *Database) UpdateWorkerStatus(id string, status protocol.WorkerStatus) error {
	id = NormalizeWorkerID(id)
	now := time.Now()
	result, err := d.db.Exec(`
		UPDATE workers SET status = ?, last_heartbeat = ? WHERE id = ?
	`, status, now, id)
	if err != nil {
		return fmt.Errorf("failed to update worker status: %w", err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to check rows affected: %w", err)
	}
	if rows == 0 {
		return protocol.ErrWorkerNotFound
	}

	return nil
}

// GetTimedOutJobs retrieves jobs that have been running longer than the timeout
func (d *Database) GetTimedOutJobs(timeout time.Duration) ([]*Job, error) {
	cutoff := time.Now().Add(-timeout)
	rows, err := d.db.Query(`
		SELECT id, status, input_files, args, output_filename, streaming_output, output_files, worker_id, exit_code, error, failure_type, failure_details, auto_hw, cached, timeout, direct_paths, progress_percent, eta_seconds,
		       created_at, updated_at, started_at, finished_at, assigned_worker, worker_name
		FROM jobs WHERE status = ? AND started_at IS NOT NULL AND started_at < ?
	`, protocol.JobStatusRunning, cutoff)
	if err != nil {
		return nil, fmt.Errorf("failed to get timed out jobs: %w", err)
	}
	defer rows.Close()

	return d.scanJobs(rows)
}

// RescheduleJob resets a job for rescheduling after timeout.
//
// TSI-2359: conditional on the job still being running. A timeout sweep that
// races a worker's completion report must not drag a finished job back to
// pending; the guarded UPDATE simply matches 0 rows and the caller logs it.
func (d *Database) RescheduleJob(id string) error {
	now := time.Now()
	// TSI-2597: refresh created_at so a timeout requeue re-anchors the
	// starvation sweep and NoWorkerDeadline clocks exactly like a failover
	// reset does. Both "re-queue = re-submit" paths share jobs.created_at as
	// their time base; MaxTimeoutRetries caps the requeues, so the restart
	// cannot wait unboundedly.
	result, err := d.db.Exec(`
		UPDATE jobs SET status = ?, worker_id = NULL, started_at = NULL, updated_at = ?,
		                created_at = ?
		WHERE id = ? AND status = ?
	`, protocol.JobStatusPending, now, now, id, protocol.JobStatusRunning)
	if err != nil {
		return fmt.Errorf("failed to reschedule job: %w", err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to check rows affected: %w", err)
	}
	if rows == 0 {
		job, getErr := d.GetJob(id)
		if getErr != nil {
			return protocol.ErrJobNotFound
		}
		return fmt.Errorf("cannot reschedule job in status %s: %w", job.Status, protocol.ErrJobTerminal)
	}

	return nil
}

// FailJob marks a job as failed with the given error message.
// The worker assignment is cleared and timestamps are updated.
// failureType is the machine-readable classification (e.g.
// protocol.FailureWorkerCrash when repeated worker crashes exhausted
// retries); pass "" to keep any existing classification.
func (d *Database) FailJob(id, errMsg string, failureType string) error {
	now := time.Now()
	exitCode := -1
	// TSI-2359: only fail jobs still in an active state. Failing a completed
	// or cancelled job would overwrite a legitimate terminal outcome.
	result, err := d.db.Exec(`
		UPDATE jobs SET status = ?, worker_id = NULL, started_at = NULL,
		                exit_code = ?, error = ?,
		                failure_type = COALESCE(NULLIF(?, ''), failure_type),
		                finished_at = ?, updated_at = ?
		WHERE id = ? AND status IN (?, ?, ?)
	`, protocol.JobStatusFailed, exitCode, errMsg, failureType, now, now, id,
		protocol.JobStatusPending, protocol.JobStatusQueued, protocol.JobStatusRunning)
	if err != nil {
		return fmt.Errorf("failed to fail job: %w", err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to check rows affected: %w", err)
	}
	if rows == 0 {
		job, getErr := d.GetJob(id)
		if getErr != nil {
			return protocol.ErrJobNotFound
		}
		return fmt.Errorf("cannot fail job in status %s: %w", job.Status, protocol.ErrJobTerminal)
	}

	d.notifyTerminal(id)
	return nil
}

// FailStarvedPendingJobs fails all pending jobs created before cutoff in a
// single statement, returning the number of jobs failed. Used by the scheduler
// starvation guard (TSI-2334) so a backlog larger than any fetch limit
// converges within one tick.
func (d *Database) FailStarvedPendingJobs(cutoff time.Time, errMsg, failureType string) (int64, error) {
	now := time.Now()
	exitCode := -1
	result, err := d.db.Exec(`
		UPDATE jobs SET status = ?, worker_id = NULL,
		                exit_code = ?, error = ?,
		                failure_type = ?, finished_at = ?, updated_at = ?
		WHERE status = ? AND worker_id IS NULL AND created_at < ?
	`, protocol.JobStatusFailed, exitCode, errMsg, failureType, now, now,
		protocol.JobStatusPending, cutoff)
	if err != nil {
		return 0, fmt.Errorf("failed to fail starved pending jobs: %w", err)
	}
	return result.RowsAffected()
}

// GetStarvedPendingJobIDs lists the IDs of pending jobs created before the
// cutoff (the exact set FailStarvedPendingJobs fails). The scheduler uses it
// to release rate-limit quota and broadcast WS updates per failed job — the
// bulk UPDATE bypasses the handler that normally does both (TSI-2365).
func (d *Database) GetStarvedPendingJobIDs(cutoff time.Time) ([]string, error) {
	rows, err := d.db.Query(`
		SELECT id FROM jobs
		WHERE status = ? AND worker_id IS NULL AND created_at < ?
	`, protocol.JobStatusPending, cutoff)
	if err != nil {
		return nil, fmt.Errorf("failed to list starved pending job IDs: %w", err)
	}
	defer rows.Close()

	ids := make([]string, 0, 16)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("failed to scan starved job ID: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// ResetJobToPending resets a job to pending status, clearing worker assignment.
// This is used during worker failover to re-queue a job for another worker.
func (d *Database) ResetJobToPending(id string) error {
	now := time.Now()
	// TSI-2359: only reset jobs still in an active state. A failover sweep
	// racing a worker's completion must not drag a finished job back to
	// pending and re-run it.
	//
	// TSI-2597: refresh created_at so the starvation sweep and the
	// NoWorkerDeadline it feeds both start from the migration moment. They
	// share jobs.created_at as their time base, so re-anchoring it here keeps
	// the sweep verdict and the CLI's client-side wait window consistent
	// without touching either formula.
	result, err := d.db.Exec(`
		UPDATE jobs SET status = ?, worker_id = NULL, started_at = NULL, updated_at = ?,
		                created_at = ?,
		                failure_type = '', failure_details = '', exit_code = NULL, error = NULL
		WHERE id = ? AND status IN (?, ?, ?)
	`, protocol.JobStatusPending, now, now, id,
		protocol.JobStatusQueued, protocol.JobStatusRunning, protocol.JobStatusPending)
	if err != nil {
		return fmt.Errorf("failed to reset job to pending: %w", err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to check rows affected: %w", err)
	}
	if rows == 0 {
		job, getErr := d.GetJob(id)
		if getErr != nil {
			return protocol.ErrJobNotFound
		}
		return fmt.Errorf("cannot reset job in status %s: %w", job.Status, protocol.ErrJobTerminal)
	}

	return nil
}

// scanWorkers scans multiple worker rows
func (d *Database) scanWorkers(rows *sql.Rows) ([]*Worker, error) {
	var workers []*Worker
	for rows.Next() {
		worker := &Worker{}
		err := rows.Scan(
			&worker.ID, &worker.Name, &worker.Status, &worker.GPUModel,
			&worker.Encoders, &worker.Decoders, &worker.VideoEncoders, &worker.VideoDecoders, &worker.FFmpegVersion,
			&worker.MaxConcurrent, &worker.Evicted, &worker.EvictedAt,
			&worker.Hwaccels, &worker.Codecs, &worker.Filters, &worker.PixFmts, &worker.Formats,
			&worker.LastHeartbeat, &worker.CreatedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan worker: %w", err)
		}
		workers = append(workers, worker)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating workers: %w", err)
	}

	return workers, nil
}

// RecoverState performs recovery operations after server restart: it resets
// jobs that were in running/queued state back to pending and marks all workers
// as offline. Stale-worker cleanup is a separate pass (RemoveStaleWorkers)
// invoked from cmd/server/main.go after recovery.
func (d *Database) RecoverState() (jobsReset int64, workersMarkedOffline int64, err error) {
	tx, err := d.db.Begin()
	if err != nil {
		return 0, 0, fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	// Reset running jobs to pending. TSI-2597: refresh created_at so the
	// recovery requeue re-anchors the starvation sweep and NoWorkerDeadline
	// clocks like every other requeue path — a restart clears the schedulable
	// set, and the first starvation tick must not kill in-flight jobs on a
	// stale pre-restart created_at.
	now := time.Now()
	result, err := tx.Exec(`
		UPDATE jobs SET status = ?, updated_at = ?, created_at = ?, worker_id = NULL, started_at = NULL
		WHERE status = ? OR status = ?
	`, protocol.JobStatusPending, now, now, protocol.JobStatusRunning, protocol.JobStatusQueued)
	if err != nil {
		return 0, 0, fmt.Errorf("failed to reset running jobs: %w", err)
	}
	jobsReset, err = result.RowsAffected()
	if err != nil {
		return 0, 0, fmt.Errorf("failed to get rows affected: %w", err)
	}

	// Mark all workers as offline
	result, err = tx.Exec(`
		UPDATE workers SET status = ?
	`, protocol.WorkerStatusOffline)
	if err != nil {
		return 0, 0, fmt.Errorf("failed to mark workers offline: %w", err)
	}
	workersMarkedOffline, err = result.RowsAffected()
	if err != nil {
		return 0, 0, fmt.Errorf("failed to get rows affected: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return 0, 0, fmt.Errorf("failed to commit transaction: %w", err)
	}

	return jobsReset, workersMarkedOffline, nil
}

// RemoveStaleWorkers deletes worker records whose last heartbeat predates
// now - staleThreshold. Called from cmd/server/main.go during startup
// recovery, after RecoverState has marked every worker offline: a worker that
// has not heartbeated within the threshold is dead, so its row is
// stale residue from a previous run. Without this, records accumulate when the
// server crashes or shuts down before the health monitor's offline-threshold
// sweep (10m default) ever runs, leaving stale entries that /api/v1/workers
// would keep returning as offline nodes (TSI-2366, TSI-2844).
//
// Fresh-heartbeat workers — a live process that survived a server restart —
// are preserved: they re-register and are recreated as idle by
// CreateOrUpdateWorker. Deleting only genuinely stale rows (last_heartbeat
// older than 1.5× the heartbeat timeout, per TSI-2844) is what makes this
// cleanup lazy versus the previous delete-everything-offline sweep, which
// dropped the worker list to zero on every restart.
func (d *Database) RemoveStaleWorkers(staleThreshold time.Duration) (int64, error) {
	cutoff := time.Now().Add(-staleThreshold)
	result, err := d.db.Exec(`DELETE FROM workers WHERE last_heartbeat < ?`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("failed to remove stale workers: %w", err)
	}
	return result.RowsAffected()
}

// GetJobsByStatus retrieves all jobs with a specific status
func (d *Database) GetJobsByStatus(status protocol.JobStatus, limit int) ([]*Job, error) {
	rows, err := d.db.Query(`
		SELECT id, status, input_files, args, output_filename, streaming_output, output_files, worker_id, exit_code, error, failure_type, failure_details, auto_hw, cached, timeout, direct_paths, progress_percent, eta_seconds,
		       created_at, updated_at, started_at, finished_at, assigned_worker, worker_name
		FROM jobs WHERE status = ?
		ORDER BY created_at ASC LIMIT ?
	`, status, limit)

	if err != nil {
		return nil, fmt.Errorf("failed to get jobs by status: %w", err)
	}
	defer rows.Close()

	return d.scanJobs(rows)
}

// ListJobs retrieves all jobs with pagination, newest first. Used by the
// GET /api/v1/jobs list endpoint.
func (d *Database) ListJobs(limit int, offset int) ([]*Job, error) {
	rows, err := d.db.Query(`
		SELECT id, status, input_files, args, output_filename, streaming_output, output_files, worker_id, exit_code, error, failure_type, failure_details, auto_hw, cached, timeout, direct_paths, progress_percent, eta_seconds,
		       created_at, updated_at, started_at, finished_at, assigned_worker, worker_name
		FROM jobs
		ORDER BY created_at DESC, id DESC
		LIMIT ? OFFSET ?
	`, limit, offset)

	if err != nil {
		return nil, fmt.Errorf("failed to list jobs: %w", err)
	}
	defer rows.Close()

	return d.scanJobs(rows)
}

// GetAllWorkers retrieves all workers from the database
func (d *Database) GetAllWorkers() ([]*Worker, error) {
	rows, err := d.db.Query(`
		SELECT id, name, status, gpu_model, encoders, decoders, video_encoders, video_decoders, ffmpeg_version, max_concurrent, evicted, evicted_at, hwaccels, codecs, filters, pix_fmts, formats, last_heartbeat, created_at
		FROM workers
		ORDER BY created_at ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("failed to get all workers: %w", err)
	}
	defer rows.Close()

	return d.scanWorkers(rows)
}

// GetWorkersByEncoder retrieves workers that have a specific encoder capability.
// Only returns workers that are not offline.
func (d *Database) GetWorkersByEncoder(encoderName string) ([]*Worker, error) {
	// Query workers where the encoder is in their encoders JSON array
	// SQLite JSON functions: json_each extracts elements from JSON array
	rows, err := d.db.Query(`
		SELECT DISTINCT w.id, w.name, w.status, w.gpu_model, w.encoders, w.decoders, w.video_encoders, w.video_decoders, w.ffmpeg_version, w.max_concurrent, w.evicted, w.evicted_at, w.hwaccels, w.codecs, w.filters, w.pix_fmts, w.formats, w.last_heartbeat, w.created_at
		FROM workers w, json_each(w.encoders) AS enc
		WHERE w.status != ? AND enc.value = ?
		ORDER BY w.last_heartbeat DESC
	`, protocol.WorkerStatusOffline, encoderName)
	if err != nil {
		return nil, fmt.Errorf("failed to get workers by encoder: %w", err)
	}
	defer rows.Close()

	return d.scanWorkers(rows)
}

// GetIdleWorkersByEncoder retrieves idle workers that have a specific encoder capability.
// Evicted workers are excluded.
func (d *Database) GetIdleWorkersByEncoder(encoderName string) ([]*Worker, error) {
	rows, err := d.db.Query(`
		SELECT DISTINCT w.id, w.name, w.status, w.gpu_model, w.encoders, w.decoders, w.video_encoders, w.video_decoders, w.ffmpeg_version, w.max_concurrent, w.evicted, w.evicted_at, w.hwaccels, w.codecs, w.filters, w.pix_fmts, w.formats, w.last_heartbeat, w.created_at
		FROM workers w, json_each(w.encoders) AS enc
		WHERE w.status = ? AND w.evicted = 0 AND enc.value = ?
		ORDER BY w.last_heartbeat DESC
	`, protocol.WorkerStatusIdle, encoderName)
	if err != nil {
		return nil, fmt.Errorf("failed to get idle workers by encoder: %w", err)
	}
	defer rows.Close()

	return d.scanWorkers(rows)
}

// GetSchedulableWorkersByEncoder retrieves workers that are not offline and not evicted
// and that have a specific encoder capability. Busy workers are included so a submitted
// job can be queued while the worker is currently busy (TSI-2204).
//
// Like GetSchedulableWorkers, the result is NOT filtered by heartbeat
// freshness; use GetLiveSchedulableWorkersByEncoder for the submit-time
// fail-fast path (TSI-2419).
func (d *Database) GetSchedulableWorkersByEncoder(encoderName string) ([]*Worker, error) {
	rows, err := d.querySchedulableByEncoder(encoderName, 0)
	if err != nil {
		return nil, fmt.Errorf("failed to get schedulable workers by encoder: %w", err)
	}
	return rows, nil
}

// GetLiveSchedulableWorkersByEncoder is GetSchedulableWorkersByEncoder with a
// heartbeat freshness window: workers whose last_heartbeat is older than
// freshness are excluded (TSI-2419). A non-positive freshness disables the
// check.
func (d *Database) GetLiveSchedulableWorkersByEncoder(encoderName string, freshness time.Duration) ([]*Worker, error) {
	rows, err := d.querySchedulableByEncoder(encoderName, freshness)
	if err != nil {
		return nil, fmt.Errorf("failed to get live schedulable workers by encoder: %w", err)
	}
	return rows, nil
}

// heartbeatFreshnessSQL returns the SQL clause excluding workers whose
// last_heartbeat predates now-freshness, plus its bind argument. Empty
// clause and args for non-positive freshness.
func heartbeatFreshnessSQL(freshness time.Duration) (string, []any) {
	if freshness <= 0 {
		return "", nil
	}
	cutoff := time.Now().Add(-freshness)
	return " AND last_heartbeat > ?", []any{cutoff}
}

// querySchedulable runs a schedulable-worker query and scans the rows.
func (d *Database) querySchedulable(query string, args ...any) ([]*Worker, error) {
	rows, err := d.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return d.scanWorkers(rows)
}

// querySchedulableByEncoder runs the encoder-capability schedulable query,
// optionally applying a heartbeat freshness window.
func (d *Database) querySchedulableByEncoder(encoderName string, freshness time.Duration) ([]*Worker, error) {
	freshnessClause, freshnessArgs := heartbeatFreshnessSQL(freshness)
	return d.querySchedulable(`
		SELECT DISTINCT w.id, w.name, w.status, w.gpu_model, w.encoders, w.decoders, w.video_encoders, w.video_decoders, w.ffmpeg_version, w.max_concurrent, w.evicted, w.evicted_at, w.hwaccels, w.codecs, w.filters, w.pix_fmts, w.formats, w.last_heartbeat, w.created_at
		FROM workers w, json_each(w.encoders) AS enc
		WHERE w.status != ? AND w.evicted = 0 AND enc.value = ?`+freshnessClause+`
		ORDER BY w.last_heartbeat DESC
	`, append([]any{protocol.WorkerStatusOffline, encoderName}, freshnessArgs...)...)
}

// UpdateWorkerCapabilities updates the worker's capabilities (encoders, decoders, etc.)
func (d *Database) UpdateWorkerCapabilities(id string, caps protocol.WorkerCapabilities) error {
	id = NormalizeWorkerID(id)
	now := time.Now()

	encodersJSON, err := json.Marshal(caps.Encoders)
	if err != nil {
		return fmt.Errorf("failed to marshal encoders: %w", err)
	}
	decodersJSON, err := json.Marshal(caps.Decoders)
	if err != nil {
		return fmt.Errorf("failed to marshal decoders: %w", err)
	}
	videoEncodersJSON, err := json.Marshal(caps.VideoEncoders)
	if err != nil {
		return fmt.Errorf("failed to marshal video encoders: %w", err)
	}
	videoDecodersJSON, err := json.Marshal(caps.VideoDecoders)
	if err != nil {
		return fmt.Errorf("failed to marshal video decoders: %w", err)
	}

	var gpuModel interface{}
	if caps.GPUModel != "" {
		gpuModel = caps.GPUModel
	}

	result, err := d.db.Exec(`
		UPDATE workers SET 
			gpu_model = ?, 
			encoders = ?, 
			decoders = ?,
			video_encoders = ?,
			video_decoders = ?,
			ffmpeg_version = ?, 
			max_concurrent = ?,
			hwaccels = ?,
			codecs = ?,
			filters = ?,
			pix_fmts = ?,
			formats = ?,
			last_heartbeat = ?
		WHERE id = ?
	`, gpuModel, string(encodersJSON), string(decodersJSON),
		string(videoEncodersJSON), string(videoDecodersJSON),
		caps.FFmpegVersion, caps.MaxConcurrent,
		caps.Hwaccels, caps.Codecs, caps.Filters, caps.PixFmts, caps.Formats,
		now, id)

	if err != nil {
		return fmt.Errorf("failed to update worker capabilities: %w", err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to check rows affected: %w", err)
	}
	if rows == 0 {
		return protocol.ErrWorkerNotFound
	}

	return nil
}

// WorkerWithJobCount represents a worker with its active and completed job counts.
// CompletedJobs breaks ties when multiple idle workers share the same active
// count: the worker that has done fewer jobs wins, so a late-registered worker
// is not starved by an earlier one that always appears first (TSI-2477).
type WorkerWithJobCount struct {
	Worker        *Worker
	ActiveJobs    int
	CompletedJobs int
}

// GetIdleWorkersWithJobCount retrieves idle workers with their active and
// completed job counts. The active count drives scheduling; the completed
// count breaks ties so late-registered workers are not starved (TSI-2477).
func (d *Database) GetIdleWorkersWithJobCount() ([]WorkerWithJobCount, error) {
	workers, err := d.GetIdleWorkers()
	if err != nil {
		return nil, err
	}

	return d.fillWorkerJobCounts(workers)
}

// GetIdleWorkersByEncoderWithJobCount retrieves idle workers with a specific
// encoder and their active and completed job counts, sorted by active then
// completed job count ascending.
func (d *Database) GetIdleWorkersByEncoderWithJobCount(encoderName string) ([]WorkerWithJobCount, error) {
	workers, err := d.GetIdleWorkersByEncoder(encoderName)
	if err != nil {
		return nil, err
	}

	result, err := d.fillWorkerJobCounts(workers)
	if err != nil {
		return nil, err
	}

	sortWorkersByJobCount(result)

	return result, nil
}

// fillWorkerJobCounts builds WorkerWithJobCount entries with active and
// completed job counts for the given workers. It fetches both counts in a
// single GROUP BY query instead of one COUNT per worker, so the cost is
// constant rather than 2N (TSI-2477 review finding 1).
func (d *Database) fillWorkerJobCounts(workers []*Worker) ([]WorkerWithJobCount, error) {
	if len(workers) == 0 {
		return nil, nil
	}

	ids := make([]interface{}, len(workers))
	for i, w := range workers {
		ids[i] = w.ID
	}
	placeholders := strings.Repeat("?,", len(ids))
	placeholders = placeholders[:len(placeholders)-1]

	query := `SELECT worker_id, ` +
		`SUM(CASE WHEN status IN (?, ?) THEN 1 ELSE 0 END) AS active, ` +
		`SUM(CASE WHEN status = ? THEN 1 ELSE 0 END) AS completed ` +
		`FROM jobs WHERE worker_id IN (` + placeholders + `) GROUP BY worker_id`

	args := []interface{}{protocol.JobStatusRunning, protocol.JobStatusQueued, protocol.JobStatusCompleted}
	args = append(args, ids...)

	rows, err := d.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to query worker job counts: %w", err)
	}
	defer rows.Close()

	type counts struct {
		active, completed int
	}
	stats := make(map[string]counts, len(workers))
	for rows.Next() {
		var id string
		var active, completed int
		if err := rows.Scan(&id, &active, &completed); err != nil {
			return nil, fmt.Errorf("failed to scan worker job counts: %w", err)
		}
		stats[id] = counts{active: active, completed: completed}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed iterating worker job counts: %w", err)
	}

	result := make([]WorkerWithJobCount, len(workers))
	for i, worker := range workers {
		c := stats[worker.ID]
		result[i] = WorkerWithJobCount{
			Worker:        worker,
			ActiveJobs:    c.active,
			CompletedJobs: c.completed,
		}
	}
	return result, nil
}

// sortWorkersByJobCount sorts workers by active job count ascending, breaking
// ties on completed job count so the worker that has done fewer jobs wins
// (TSI-2477).
func sortWorkersByJobCount(workers []WorkerWithJobCount) {
	for i := 0; i < len(workers)-1; i++ {
		for j := i + 1; j < len(workers); j++ {
			if workers[j].ActiveJobs < workers[i].ActiveJobs ||
				(workers[j].ActiveJobs == workers[i].ActiveJobs &&
					workers[j].CompletedJobs < workers[i].CompletedJobs) {
				workers[i], workers[j] = workers[j], workers[i]
			}
		}
	}
}

// GetWorkerEncoders retrieves the list of encoders for a specific worker
func (d *Database) GetWorkerEncoders(workerID string) ([]string, error) {
	workerID = NormalizeWorkerID(workerID)
	var encodersJSON string
	err := d.db.QueryRow(`SELECT encoders FROM workers WHERE id = ?`, workerID).Scan(&encodersJSON)
	if err == sql.ErrNoRows {
		return nil, protocol.ErrWorkerNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get worker encoders: %w", err)
	}

	var encoders []string
	if err := json.Unmarshal([]byte(encodersJSON), &encoders); err != nil {
		return nil, fmt.Errorf("failed to unmarshal encoders: %w", err)
	}

	return encoders, nil
}

// GetAllEncoders retrieves a unique list of all encoders across all workers
func (d *Database) GetAllEncoders() ([]string, error) {
	rows, err := d.db.Query(`SELECT DISTINCT enc.value FROM workers, json_each(encoders) AS enc`)
	if err != nil {
		return nil, fmt.Errorf("failed to get all encoders: %w", err)
	}
	defer rows.Close()

	var encoders []string
	for rows.Next() {
		var encoder string
		if err := rows.Scan(&encoder); err != nil {
			return nil, fmt.Errorf("failed to scan encoder: %w", err)
		}
		encoders = append(encoders, encoder)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating encoders: %w", err)
	}

	return encoders, nil
}

// GetAllDecoders retrieves a unique list of all decoders across all workers
func (d *Database) GetAllDecoders() ([]string, error) {
	rows, err := d.db.Query(`SELECT DISTINCT dec.value FROM workers, json_each(decoders) AS dec`)
	if err != nil {
		return nil, fmt.Errorf("failed to get all decoders: %w", err)
	}
	defer rows.Close()

	var decoders []string
	for rows.Next() {
		var decoder string
		if err := rows.Scan(&decoder); err != nil {
			return nil, fmt.Errorf("failed to scan decoder: %w", err)
		}
		decoders = append(decoders, decoder)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating decoders: %w", err)
	}

	return decoders, nil
}

// GetAllHwaccels retrieves a unique list of all hwaccels across all workers.
// Parses raw text from ffmpeg -hwaccels output: header line followed by one method per line.
func (d *Database) GetAllHwaccels() ([]string, error) {
	rows, err := d.db.Query(`SELECT hwaccels FROM workers WHERE status != ? AND hwaccels != ''`, protocol.WorkerStatusOffline)
	if err != nil {
		return nil, fmt.Errorf("failed to get hwaccels: %w", err)
	}
	defer rows.Close()

	seen := make(map[string]bool)
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, fmt.Errorf("failed to scan hwaccels: %w", err)
		}
		for _, item := range parseInfoFlagLines(raw) {
			seen[item] = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating hwaccels: %w", err)
	}

	result := make([]string, 0, len(seen))
	for item := range seen {
		result = append(result, item)
	}
	return result, nil
}

// GetAllCodecs retrieves a unique list of all codecs across all workers.
// Parses raw text from ffmpeg -codecs output: header/legend lines followed by codec entries.
// Extracts the codec name (second token) from each data line.
func (d *Database) GetAllCodecs() ([]string, error) {
	rows, err := d.db.Query(`SELECT codecs FROM workers WHERE status != ? AND codecs != ''`, protocol.WorkerStatusOffline)
	if err != nil {
		return nil, fmt.Errorf("failed to get codecs: %w", err)
	}
	defer rows.Close()

	seen := make(map[string]bool)
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, fmt.Errorf("failed to scan codecs: %w", err)
		}
		for _, item := range parseInfoFlagTokens(raw) {
			seen[item] = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating codecs: %w", err)
	}

	result := make([]string, 0, len(seen))
	for item := range seen {
		result = append(result, item)
	}
	return result, nil
}

// GetAllFilters retrieves a unique list of all filters across all workers.
// Parses raw text from ffmpeg -filters output: header/legend lines followed by filter entries.
func (d *Database) GetAllFilters() ([]string, error) {
	rows, err := d.db.Query(`SELECT filters FROM workers WHERE status != ? AND filters != ''`, protocol.WorkerStatusOffline)
	if err != nil {
		return nil, fmt.Errorf("failed to get filters: %w", err)
	}
	defer rows.Close()

	seen := make(map[string]bool)
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, fmt.Errorf("failed to scan filters: %w", err)
		}
		for _, item := range parseInfoFlagTokens(raw) {
			seen[item] = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating filters: %w", err)
	}

	result := make([]string, 0, len(seen))
	for item := range seen {
		result = append(result, item)
	}
	return result, nil
}

// GetAllPixFmts retrieves a unique list of all pixel formats across all workers.
// Parses raw text from ffmpeg -pix_fmts output: header/legend lines followed by pix_fmt entries.
func (d *Database) GetAllPixFmts() ([]string, error) {
	rows, err := d.db.Query(`SELECT pix_fmts FROM workers WHERE status != ? AND pix_fmts != ''`, protocol.WorkerStatusOffline)
	if err != nil {
		return nil, fmt.Errorf("failed to get pix_fmts: %w", err)
	}
	defer rows.Close()

	seen := make(map[string]bool)
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, fmt.Errorf("failed to scan pix_fmts: %w", err)
		}
		for _, item := range parseInfoFlagTokens(raw) {
			seen[item] = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating pix_fmts: %w", err)
	}

	result := make([]string, 0, len(seen))
	for item := range seen {
		result = append(result, item)
	}
	return result, nil
}

// GetAllFormats retrieves a unique list of all formats across all workers.
// Parses raw text from ffmpeg -formats output: header/legend lines followed by format entries.
func (d *Database) GetAllFormats() ([]string, error) {
	rows, err := d.db.Query(`SELECT formats FROM workers WHERE status != ? AND formats != ''`, protocol.WorkerStatusOffline)
	if err != nil {
		return nil, fmt.Errorf("failed to get formats: %w", err)
	}
	defer rows.Close()

	seen := make(map[string]bool)
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, fmt.Errorf("failed to scan formats: %w", err)
		}
		for _, item := range parseInfoFlagTokens(raw) {
			seen[item] = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating formats: %w", err)
	}

	result := make([]string, 0, len(seen))
	for item := range seen {
		result = append(result, item)
	}
	return result, nil
}

// parseInfoFlagLines parses raw text output where each data line is a simple name
// (e.g., ffmpeg -hwaccels output: header line "Hardware acceleration methods:",
// followed by one method per line). Returns non-empty, non-header lines.
func parseInfoFlagLines(raw string) []string {
	var items []string
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Skip header/legend lines
		if strings.HasPrefix(line, "Hardware") || strings.HasPrefix(line, "Codecs") ||
			strings.HasPrefix(line, "Filters") || strings.HasPrefix(line, "Pixel") ||
			strings.HasPrefix(line, "File") || strings.HasPrefix(line, " D") ||
			strings.HasPrefix(line, " .") || strings.HasPrefix(line, " I") ||
			strings.HasPrefix(line, " .O") || strings.HasPrefix(line, " ..") ||
			strings.HasPrefix(line, "FLAGS") || strings.HasPrefix(line, "-----") ||
			strings.HasPrefix(line, " --") || strings.HasPrefix(line, " =") {
			continue
		}
		items = append(items, line)
	}
	return items
}

// parseInfoFlagTokens parses raw text output where each data line starts with flags
// followed by a token name (e.g., ffmpeg -codecs/-filters/-pix_fmts/-formats output).
// Extracts the first non-flag token (the codec/filter/format/pix_fmt name).
func parseInfoFlagTokens(raw string) []string {
	var items []string
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Data line format: "FLAGS NAME            description..."
		// Legend lines: "FLAG_LETTERS..... = Description"
		// Separator lines: "------" or "--"
		// Header lines: "X formats:", "X codecs:", etc.
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		// Skip legend lines (second field is "=")
		if fields[1] == "=" {
			continue
		}
		// Skip separator lines (all dashes)
		if isAllDashes(fields[0]) {
			continue
		}
		// Skip header lines (second field ends with ":" like "formats:", "codecs:", etc.)
		if strings.HasSuffix(fields[1], ":") {
			continue
		}
		// The first field is flags, the second is the name
		items = append(items, fields[1])
	}
	return items
}

// isAllDashes returns true if the string consists entirely of dash/minus characters.
func isAllDashes(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c != '-' {
			return false
		}
	}
	return true
}

// GetAllEncodersInfo retrieves a unique list of all encoders across all workers
// with rich metadata (name, description, type, is_hw, priority) from the
// video_encoders column. Workers are scanned in registration order (rowid), so
// an earlier-registered worker is encountered first; for encoders that multiple
// workers expose under the same name, the entry with the highest priority wins,
// with is_hw and then the longer description as tie-breakers. This keeps a
// later-registered hardware/priority encoder from being shadowed by an earlier
// low-priority entry (TSI-2554).
func (d *Database) GetAllEncodersInfo() ([]protocol.EncoderInfo, error) {
	rows, err := d.db.Query(`SELECT video_encoders FROM workers WHERE video_encoders != '[]' ORDER BY rowid`)
	if err != nil {
		return nil, fmt.Errorf("failed to get all encoder info: %w", err)
	}
	defer rows.Close()

	seen := make(map[string]protocol.EncoderInfo)
	for rows.Next() {
		var videoEncodersJSON string
		if err := rows.Scan(&videoEncodersJSON); err != nil {
			return nil, fmt.Errorf("failed to scan video encoders: %w", err)
		}
		var encoders []protocol.EncoderInfo
		if err := json.Unmarshal([]byte(videoEncodersJSON), &encoders); err != nil {
			log.Printf("warning: failed to unmarshal video_encoders JSON: %v", err)
			continue
		}
		for _, enc := range encoders {
			seen[enc.Name] = mergeEncoderInfo(seen[enc.Name], enc)
		}
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating video encoders: %w", err)
	}

	result := make([]protocol.EncoderInfo, 0, len(seen))
	for _, enc := range seen {
		result = append(result, enc)
	}
	return result, nil
}

// mergeEncoderInfo picks the encoder metadata that should represent a name
// when several workers expose the same encoder. Priority is authoritative
// (higher wins, matching the user-defined encoder_priority order); ties break
// on is_hw (hardware preferred) and then on the longer description, so the
// richest metadata survives instead of whichever worker registered first.
func mergeEncoderInfo(current, candidate protocol.EncoderInfo) protocol.EncoderInfo {
	if current.Name == "" {
		return candidate
	}
	if candidate.Priority != current.Priority {
		if candidate.Priority > current.Priority {
			return candidate
		}
		return current
	}
	if candidate.IsHW != current.IsHW {
		if candidate.IsHW {
			return candidate
		}
		return current
	}
	if len(candidate.Description) > len(current.Description) {
		return candidate
	}
	return current
}

// GetAllDecodersInfo retrieves a unique list of all decoders across all workers
// with rich metadata (name, description, type, is_hw) from the video_decoders
// column. Workers are scanned in registration order (rowid); for decoders that
// multiple workers expose under the same name, is_hw wins and then the longer
// description, so a later-registered hardware decoder is not shadowed by an
// earlier software entry (TSI-2554).
func (d *Database) GetAllDecodersInfo() ([]protocol.DecoderInfo, error) {
	rows, err := d.db.Query(`SELECT video_decoders FROM workers WHERE video_decoders != '[]' ORDER BY rowid`)
	if err != nil {
		return nil, fmt.Errorf("failed to get all decoder info: %w", err)
	}
	defer rows.Close()

	seen := make(map[string]protocol.DecoderInfo)
	for rows.Next() {
		var videoDecodersJSON string
		if err := rows.Scan(&videoDecodersJSON); err != nil {
			return nil, fmt.Errorf("failed to scan video decoders: %w", err)
		}
		var decoders []protocol.DecoderInfo
		if err := json.Unmarshal([]byte(videoDecodersJSON), &decoders); err != nil {
			log.Printf("warning: failed to unmarshal video_decoders JSON: %v", err)
			continue
		}
		for _, dec := range decoders {
			seen[dec.Name] = mergeDecoderInfo(seen[dec.Name], dec)
		}
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating video decoders: %w", err)
	}

	result := make([]protocol.DecoderInfo, 0, len(seen))
	for _, dec := range seen {
		result = append(result, dec)
	}
	return result, nil
}

// mergeDecoderInfo picks the decoder metadata that should represent a name
// when several workers expose the same decoder: hardware preferred, then the
// longer description, so the richest metadata survives instead of whichever
// worker registered first.
func mergeDecoderInfo(current, candidate protocol.DecoderInfo) protocol.DecoderInfo {
	if current.Name == "" {
		return candidate
	}
	if candidate.IsHW != current.IsHW {
		if candidate.IsHW {
			return candidate
		}
		return current
	}
	if len(candidate.Description) > len(current.Description) {
		return candidate
	}
	return current
}

// MigrationEvent represents a migration event record in the database.
type MigrationEvent struct {
	ID           string
	Timestamp    time.Time
	WorkerID     string
	WorkerName   sql.NullString
	Reason       string
	RetryCount   int
	JobIDs       string // JSON array stored as string
	JobsMigrated int
	CreatedAt    time.Time
}

// CreateMigrationEvent creates a new migration event record.
func (d *Database) CreateMigrationEvent(workerID, workerName, reason string, retryCount int, jobIDs []string, jobsMigrated int) (*MigrationEvent, error) {
	id := uuid.New().String()
	now := time.Now()

	jobIDsJSON, err := json.Marshal(jobIDs)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal job IDs: %w", err)
	}

	var workerNameVal interface{}
	if workerName != "" {
		workerNameVal = workerName
	}

	_, err = d.db.Exec(`
		INSERT INTO migration_events (id, timestamp, worker_id, worker_name, reason, retry_count, job_ids, jobs_migrated, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, id, now, workerID, workerNameVal, reason, retryCount, string(jobIDsJSON), jobsMigrated, now)

	if err != nil {
		return nil, fmt.Errorf("failed to create migration event: %w", err)
	}

	return d.GetMigrationEvent(id)
}

// GetMigrationEvent retrieves a migration event by ID.
func (d *Database) GetMigrationEvent(id string) (*MigrationEvent, error) {
	event := &MigrationEvent{}
	err := d.db.QueryRow(`
		SELECT id, timestamp, worker_id, worker_name, reason, retry_count, job_ids, jobs_migrated, created_at
		FROM migration_events WHERE id = ?
	`, id).Scan(
		&event.ID, &event.Timestamp, &event.WorkerID, &event.WorkerName,
		&event.Reason, &event.RetryCount, &event.JobIDs, &event.JobsMigrated, &event.CreatedAt,
	)

	if err == sql.ErrNoRows {
		return nil, protocol.ErrMigrationEventNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get migration event: %w", err)
	}

	return event, nil
}

// GetMigrationEvents retrieves migration events with pagination.
func (d *Database) GetMigrationEvents(limit int, offset int) ([]*MigrationEvent, error) {
	rows, err := d.db.Query(`
		SELECT id, timestamp, worker_id, worker_name, reason, retry_count, job_ids, jobs_migrated, created_at
		FROM migration_events
		ORDER BY timestamp DESC, id DESC
		LIMIT ? OFFSET ?
	`, limit, offset)

	if err != nil {
		return nil, fmt.Errorf("failed to get migration events: %w", err)
	}
	defer rows.Close()

	var events []*MigrationEvent
	for rows.Next() {
		event := &MigrationEvent{}
		err := rows.Scan(
			&event.ID, &event.Timestamp, &event.WorkerID, &event.WorkerName,
			&event.Reason, &event.RetryCount, &event.JobIDs, &event.JobsMigrated, &event.CreatedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan migration event: %w", err)
		}
		events = append(events, event)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating migration events: %w", err)
	}

	return events, nil
}

// GetMigrationEventsByWorker retrieves migration events for a specific worker.
func (d *Database) GetMigrationEventsByWorker(workerID string, limit int) ([]*MigrationEvent, error) {
	rows, err := d.db.Query(`
		SELECT id, timestamp, worker_id, worker_name, reason, retry_count, job_ids, jobs_migrated, created_at
		FROM migration_events
		WHERE worker_id = ?
		ORDER BY timestamp DESC, id DESC
		LIMIT ?
	`, workerID, limit)

	if err != nil {
		return nil, fmt.Errorf("failed to get migration events by worker: %w", err)
	}
	defer rows.Close()

	var events []*MigrationEvent
	for rows.Next() {
		event := &MigrationEvent{}
		err := rows.Scan(
			&event.ID, &event.Timestamp, &event.WorkerID, &event.WorkerName,
			&event.Reason, &event.RetryCount, &event.JobIDs, &event.JobsMigrated, &event.CreatedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan migration event: %w", err)
		}
		events = append(events, event)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating migration events: %w", err)
	}

	return events, nil
}

// JobRedistribution represents a per-job redistribution record: which worker a
// migrated job was reassigned to. Unlike a migration event (source worker +
// full job_ids list), the target is a per-job fact because the jobs of a single
// migration can fan out to different workers.
type JobRedistribution struct {
	ID               string
	MigrationEventID string
	JobID            string
	TargetWorkerID   sql.NullString
	TargetWorkerName sql.NullString
	CreatedAt        time.Time
}

// CreateJobRedistributions inserts an unresolved placeholder row per migrated
// job, linking it to the migration event that migrated it. The target is filled
// later by RecordMigrationTarget when the scheduler/pull path reassigns the
// job. This is a cold path (one call per worker-offline migration).
func (d *Database) CreateJobRedistributions(migrationEventID string, jobIDs []string) error {
	tx, err := d.db.Begin()
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	now := time.Now()
	for _, jobID := range jobIDs {
		id := uuid.New().String()
		if _, err := tx.Exec(`
			INSERT INTO job_redistributions (id, migration_event_id, job_id, target_worker_id, target_worker_name, created_at)
			VALUES (?, ?, ?, NULL, NULL, ?)
		`, id, migrationEventID, jobID, now); err != nil {
			return fmt.Errorf("failed to create job redistribution for job %s: %w", jobID, err)
		}
	}

	return tx.Commit()
}

// RecordMigrationTarget fills the unresolved redistribution placeholder for a
// migrated job with the worker it was reassigned to. The lookup is an indexed
// point query on job_id, so a fresh job (no placeholder) matches zero rows and
// the UPDATE is a cheap no-op rather than a full-table json_each scan.
// targetWorkerName may be empty (worker registered without a name).
func (d *Database) RecordMigrationTarget(jobID, targetWorkerID, targetWorkerName string) error {
	var targetWorkerNameVal interface{}
	if targetWorkerName != "" {
		targetWorkerNameVal = targetWorkerName
	}

	_, err := d.db.Exec(`
		UPDATE job_redistributions
		SET target_worker_id = ?, target_worker_name = ?
		WHERE id = (
			SELECT id FROM job_redistributions
			WHERE job_id = ? AND target_worker_id IS NULL
			ORDER BY created_at DESC, id DESC
			LIMIT 1
		)
	`, targetWorkerID, targetWorkerNameVal, jobID)
	if err != nil {
		return fmt.Errorf("failed to record migration target for job %s: %w", jobID, err)
	}
	return nil
}

// GetJobRedistributionsByEvent lists the per-job redistribution records for a
// migration event, used to reconstruct the target side of a migration chain.
func (d *Database) GetJobRedistributionsByEvent(migrationEventID string) ([]JobRedistribution, error) {
	return d.GetJobRedistributionsByEvents([]string{migrationEventID})
}

// GetJobRedistributionsByEvents returns the per-job redistribution records for
// the given migration events in a single query, so the migration events list
// endpoint can attach targets without an N+1 per-event read.
func (d *Database) GetJobRedistributionsByEvents(migrationEventIDs []string) ([]JobRedistribution, error) {
	if len(migrationEventIDs) == 0 {
		return nil, nil
	}

	placeholders := make([]string, len(migrationEventIDs))
	args := make([]interface{}, len(migrationEventIDs))
	for i, id := range migrationEventIDs {
		placeholders[i] = "?"
		args[i] = id
	}

	rows, err := d.db.Query(fmt.Sprintf(`
		SELECT id, migration_event_id, job_id, target_worker_id, target_worker_name, created_at
		FROM job_redistributions
		WHERE migration_event_id IN (%s)
		ORDER BY created_at ASC, id ASC
	`, strings.Join(placeholders, ",")), args...)
	if err != nil {
		return nil, fmt.Errorf("failed to get job redistributions: %w", err)
	}
	defer rows.Close()

	var redistributions []JobRedistribution
	for rows.Next() {
		r := JobRedistribution{}
		if err := rows.Scan(&r.ID, &r.MigrationEventID, &r.JobID, &r.TargetWorkerID, &r.TargetWorkerName, &r.CreatedAt); err != nil {
			return nil, fmt.Errorf("failed to scan job redistribution: %w", err)
		}
		redistributions = append(redistributions, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating job redistributions: %w", err)
	}
	return redistributions, nil
}

// GetRunningJobsByWorker retrieves all running/queued jobs for a specific worker.
func (d *Database) GetRunningJobsByWorker(workerID string) ([]*Job, error) {
	workerID = NormalizeWorkerID(workerID)
	rows, err := d.db.Query(`
		SELECT id, status, input_files, args, output_filename, streaming_output, output_files, worker_id, exit_code, error, failure_type, failure_details, auto_hw, cached, timeout, direct_paths, progress_percent, eta_seconds,
		       created_at, updated_at, started_at, finished_at, assigned_worker, worker_name
		FROM jobs WHERE worker_id = ? AND status IN (?, ?)
		ORDER BY created_at ASC
	`, workerID, protocol.JobStatusRunning, protocol.JobStatusQueued)

	if err != nil {
		return nil, fmt.Errorf("failed to get running jobs by worker: %w", err)
	}
	defer rows.Close()

	return d.scanJobs(rows)
}

// MigrateJobsFromWorker migrates all in-progress jobs from a worker back to
// pending state. Returns the list of job IDs that were migrated.
//
// TSI-2359: the whole migration runs in one transaction, and each reset is a
// conditional UPDATE on status IN (running, queued). This closes two holes:
// (1) a job that reaches a terminal state after the snapshot is no longer
// dragged back to pending and re-run, and (2) a partial failure mid-migration
// now rolls back instead of leaving jobs half-reset with orphaned outputs.
func (d *Database) MigrateJobsFromWorker(workerID string) ([]string, error) {
	tx, err := d.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	now := time.Now()
	// Conditional reset straight from the active-state set: rows that left
	// the running/queued set between the caller's decision and this statement
	// are simply not touched, and RETURNING gives exactly what we migrated.
	// TSI-2597: created_at refreshes too — this is the same re-queue =
	// re-submit semantic as ResetJobToPending/RescheduleJob/RecoverState.
	rows, err := tx.Query(`
		UPDATE jobs SET status = ?, worker_id = NULL, started_at = NULL, updated_at = ?,
		                created_at = ?
		WHERE worker_id = ? AND status IN (?, ?)
		RETURNING id
	`, protocol.JobStatusPending, now, now, workerID,
		protocol.JobStatusRunning, protocol.JobStatusQueued)
	if err != nil {
		return nil, fmt.Errorf("failed to migrate jobs for worker %s: %w", workerID, err)
	}

	var jobIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, fmt.Errorf("failed to scan migrated job id: %w", err)
		}
		jobIDs = append(jobIDs, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("failed to iterate migrated jobs: %w", err)
	}
	rows.Close()

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("failed to commit migration: %w", err)
	}

	return jobIDs, nil
}

// MarkOfflineWorkersWithMigration marks workers as offline and returns their IDs for job migration.
// This is similar to MarkOfflineWorkers but returns the worker IDs that were marked offline.
func (d *Database) MarkOfflineWorkersWithMigration(heartbeatTimeout time.Duration) ([]string, error) {
	cutoff := time.Now().Add(-heartbeatTimeout)

	// First, get the worker IDs that will be marked offline
	rows, err := d.db.Query(`
		SELECT id FROM workers
		WHERE status != ? AND last_heartbeat < ?
	`, protocol.WorkerStatusOffline, cutoff)
	if err != nil {
		return nil, fmt.Errorf("failed to query offline workers: %w", err)
	}

	var workerIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, fmt.Errorf("failed to scan worker id: %w", err)
		}
		workerIDs = append(workerIDs, id)
	}
	rows.Close()

	if len(workerIDs) == 0 {
		return nil, nil
	}

	// Now mark them as offline
	_, err = d.db.Exec(`
		UPDATE workers SET status = ?
		WHERE status != ? AND last_heartbeat < ?
	`, protocol.WorkerStatusOffline, protocol.WorkerStatusOffline, cutoff)
	if err != nil {
		return nil, fmt.Errorf("failed to mark offline workers: %w", err)
	}

	return workerIDs, nil
}

// GetJobRetryCount returns the number of times a job has been migrated due to
// worker failure (heartbeat timeout / worker offline / server restart). It
// excludes timeout-driven requeues: those are a separate budget consumed by
// GetJobTimeoutRetryCount, and a hung ffmpeg rescheduled by the scheduler
// must not burn the worker-failure migration budget.
func (d *Database) GetJobRetryCount(jobID string) (int, error) {
	// Count migration events where this job ID appears in the job_ids JSON array.
	// Use json_each to properly expand the JSON array and check for exact matches,
	// avoiding partial match issues that would occur with LIKE.
	var count int
	err := d.db.QueryRow(`
		SELECT COUNT(*) FROM migration_events
		WHERE reason != 'job_timeout'
		  AND id IN (
			SELECT me.id FROM migration_events me, json_each(me.job_ids)
			WHERE json_each.value = ?
		  )
	`, jobID).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("failed to get job retry count: %w", err)
	}
	return count, nil
}

// GetJobTimeoutRetryCount counts only timeout-driven requeues for a job. The
// scheduler's MaxTimeoutRetries budget must not be consumed by unrelated
// migrations: a job twice migrated for worker crashes would otherwise burn its
// entire timeout budget on the first hang.
func (d *Database) GetJobTimeoutRetryCount(jobID string) (int, error) {
	var count int
	err := d.db.QueryRow(`
		SELECT COUNT(*) FROM migration_events
		WHERE reason = 'job_timeout'
		  AND id IN (
			SELECT me.id FROM migration_events me, json_each(me.job_ids)
			WHERE json_each.value = ?
		  )
	`, jobID).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("failed to get job timeout retry count: %w", err)
	}
	return count, nil
}

// --- Worker Eviction Methods (TSI-761) ---

// MarkWorkerEvicted marks a worker as evicted (slow node) with the current timestamp.
func (d *Database) MarkWorkerEvicted(workerID string) error {
	workerID = NormalizeWorkerID(workerID)
	now := time.Now()
	result, err := d.db.Exec(`
		UPDATE workers SET evicted = 1, evicted_at = ? WHERE id = ?
	`, now, workerID)
	if err != nil {
		return fmt.Errorf("failed to mark worker evicted: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to check rows affected: %w", err)
	}
	if rows == 0 {
		return protocol.ErrWorkerNotFound
	}
	return nil
}

// ClearWorkerEviction clears the eviction flag on a worker (recovery).
func (d *Database) ClearWorkerEviction(workerID string) error {
	workerID = NormalizeWorkerID(workerID)
	result, err := d.db.Exec(`
		UPDATE workers SET evicted = 0, evicted_at = NULL WHERE id = ?
	`, workerID)
	if err != nil {
		return fmt.Errorf("failed to clear worker eviction: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to check rows affected: %w", err)
	}
	if rows == 0 {
		return protocol.ErrWorkerNotFound
	}
	return nil
}

// IsWorkerEvicted checks whether a worker is currently evicted.
func (d *Database) IsWorkerEvicted(workerID string) (bool, error) {
	workerID = NormalizeWorkerID(workerID)
	var evicted bool
	err := d.db.QueryRow(`SELECT evicted FROM workers WHERE id = ?`, workerID).Scan(&evicted)
	if err == sql.ErrNoRows {
		return false, protocol.ErrWorkerNotFound
	}
	if err != nil {
		return false, fmt.Errorf("failed to check worker eviction: %w", err)
	}
	return evicted, nil
}

// --- Worker Eviction Events (TSI-762) ---

// EvictionEventType represents the type of eviction event.
type EvictionEventType string

const (
	EvictionEventEvicted   EvictionEventType = "evicted"
	EvictionEventRecovered EvictionEventType = "recovered"
)

// EvictionEvent represents a recorded eviction or recovery event.
type EvictionEvent struct {
	ID                string            `json:"id"`
	Timestamp         time.Time         `json:"timestamp"`
	WorkerID          string            `json:"worker_id"`
	EventType         EvictionEventType `json:"event_type"`
	CurrentThroughput float64           `json:"current_throughput"`
	ClusterMedian     float64           `json:"cluster_median"`
	DecisionReason    string            `json:"decision_reason"`
	CreatedAt         time.Time         `json:"created_at"`
}

// CreateEvictionEvent records a worker eviction or recovery event.
func (d *Database) CreateEvictionEvent(workerID string, eventType EvictionEventType, currentThroughput, clusterMedian float64, reason string) (*EvictionEvent, error) {
	now := time.Now()
	id := uuid.New().String()

	_, err := d.db.Exec(`
		INSERT INTO worker_eviction_events (id, timestamp, worker_id, event_type, current_throughput, cluster_median, decision_reason, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, id, now, workerID, string(eventType), currentThroughput, clusterMedian, reason, now)
	if err != nil {
		return nil, fmt.Errorf("failed to create eviction event: %w", err)
	}

	return &EvictionEvent{
		ID:                id,
		Timestamp:         now,
		WorkerID:          workerID,
		EventType:         eventType,
		CurrentThroughput: currentThroughput,
		ClusterMedian:     clusterMedian,
		DecisionReason:    reason,
		CreatedAt:         now,
	}, nil
}

// GetEvictionEvents retrieves eviction events with pagination.
func (d *Database) GetEvictionEvents(limit, offset int) ([]EvictionEvent, error) {
	rows, err := d.db.Query(`
		SELECT id, timestamp, worker_id, event_type, current_throughput, cluster_median, decision_reason, created_at
		FROM worker_eviction_events
		ORDER BY timestamp DESC
		LIMIT ? OFFSET ?
	`, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("failed to get eviction events: %w", err)
	}
	defer rows.Close()

	var events []EvictionEvent
	for rows.Next() {
		var e EvictionEvent
		if err := rows.Scan(&e.ID, &e.Timestamp, &e.WorkerID, &e.EventType, &e.CurrentThroughput, &e.ClusterMedian, &e.DecisionReason, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("failed to scan eviction event: %w", err)
		}
		events = append(events, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating eviction events: %w", err)
	}
	return events, nil
}

// GetEvictionEventsByWorker retrieves eviction events for a specific worker.
func (d *Database) GetEvictionEventsByWorker(workerID string, limit int) ([]EvictionEvent, error) {
	rows, err := d.db.Query(`
		SELECT id, timestamp, worker_id, event_type, current_throughput, cluster_median, decision_reason, created_at
		FROM worker_eviction_events
		WHERE worker_id = ?
		ORDER BY timestamp DESC
		LIMIT ?
	`, workerID, limit)
	if err != nil {
		return nil, fmt.Errorf("failed to get eviction events by worker: %w", err)
	}
	defer rows.Close()

	var events []EvictionEvent
	for rows.Next() {
		var e EvictionEvent
		if err := rows.Scan(&e.ID, &e.Timestamp, &e.WorkerID, &e.EventType, &e.CurrentThroughput, &e.ClusterMedian, &e.DecisionReason, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("failed to scan eviction event: %w", err)
		}
		events = append(events, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating eviction events: %w", err)
	}
	return events, nil
}

// LoadWSJobSeq returns the persisted WebSocket sequence counter for a job.
// Returns 0 when no counter exists — a fresh job starts numbering at 1, so
// callers treat 0 as "resume from scratch" without special-casing.
func (d *Database) LoadWSJobSeq(jobID string) (int64, error) {
	var lastSeq int64
	err := d.db.QueryRow(`SELECT last_seq FROM ws_job_seq WHERE job_id = ?`, jobID).Scan(&lastSeq)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("failed to load websocket seq for job %s: %w", jobID, err)
	}
	return lastSeq, nil
}

// SaveWSJobSeq persists the WebSocket sequence counter for a job. Called on
// every sequenced broadcast so a server restart resumes numbering where it
// left off instead of restarting at 1 (which clients read as lost data).
// Rows are keyed by job_id and upserted; the delete happens on terminal
// broadcast via DeleteWSJobSeq.
func (d *Database) SaveWSJobSeq(jobID string, lastSeq int64) error {
	_, err := d.db.Exec(`
		INSERT INTO ws_job_seq (job_id, last_seq, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(job_id) DO UPDATE SET last_seq = excluded.last_seq, updated_at = excluded.updated_at
	`, jobID, lastSeq, time.Now())
	if err != nil {
		return fmt.Errorf("failed to save websocket seq for job %s: %w", jobID, err)
	}
	return nil
}

// DeleteWSJobSeq drops the persisted sequence counter once the job reached a
// terminal state: no further sequenced data will be produced for it.
func (d *Database) DeleteWSJobSeq(jobID string) error {
	_, err := d.db.Exec(`DELETE FROM ws_job_seq WHERE job_id = ?`, jobID)
	if err != nil {
		return fmt.Errorf("failed to delete websocket seq for job %s: %w", jobID, err)
	}
	return nil
}
