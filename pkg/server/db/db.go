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
	"github.com/tsix404/rffmpeg/pkg/protocol"
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
	ExitCode        sql.NullInt32
	Error           sql.NullString
	FailureType     string       // Classified failure type (TSI-757)
	FailureDetails  string       // Human-readable failure detail
	Retryable       bool         // Whether the failure is retryable
	AutoHW          bool         // Enable automatic hardware encoder upgrade
	Timeout         sql.NullTime // Per-job timeout deadline (TSI-764)
	DirectPaths     string       // JSON array of direct output paths for pass-through mode (TSI-807)
	ProgressPercent float64      // Current progress percentage (0-100)
	EtaSeconds      int          // Estimated seconds remaining (0 if unknown)
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
	db *sql.DB
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

	d := &Database{db: db}

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
			exit_code INTEGER,
			error TEXT,
			failure_type TEXT DEFAULT '',
			failure_details TEXT DEFAULT '',
			retryable INTEGER DEFAULT 0,
			auto_hw INTEGER DEFAULT 0,
			timeout DATETIME,
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
		`ALTER TABLE jobs ADD COLUMN timeout DATETIME`,
		`ALTER TABLE workers ADD COLUMN evicted INTEGER DEFAULT 0`,
		`ALTER TABLE workers ADD COLUMN evicted_at DATETIME`,
		`ALTER TABLE jobs ADD COLUMN direct_paths TEXT DEFAULT '[]'`,
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

	return err
}

// CreateJob creates a new job record
func (d *Database) CreateJob(inputFiles, args, outputFilename string, autoHW bool) (*Job, error) {
	return d.CreateJobWithStreaming(inputFiles, args, outputFilename, autoHW, false, nil, "")
}

// CreateJobWithStreaming creates a new job record with streaming output option and optional direct paths
func (d *Database) CreateJobWithStreaming(inputFiles, args, outputFilename string, autoHW bool, streamingOutput bool, timeout *time.Time, directPaths string) (*Job, error) {
	id := uuid.New().String()
	now := time.Now()

	var timeoutVal interface{}
	if timeout != nil {
		timeoutVal = *timeout
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

// GetJob retrieves a job by ID
func (d *Database) GetJob(id string) (*Job, error) {
	job := &Job{}
	err := d.db.QueryRow(`
		SELECT id, status, input_files, args, output_filename, streaming_output, output_files, worker_id, exit_code, error,
	       failure_type, failure_details, retryable, auto_hw, timeout, direct_paths, progress_percent, eta_seconds,
	       created_at, updated_at, started_at, finished_at
		FROM jobs WHERE id = ?
	`, id).Scan(
		&job.ID, &job.Status, &job.InputFiles, &job.Args, &job.OutputFilename, &job.StreamingOutput, &job.OutputFiles,
		&job.WorkerID, &job.ExitCode, &job.Error, &job.FailureType, &job.FailureDetails, &job.Retryable, &job.AutoHW,
		&job.Timeout, &job.DirectPaths, &job.ProgressPercent, &job.EtaSeconds,
		&job.CreatedAt, &job.UpdatedAt, &job.StartedAt, &job.FinishedAt,
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
	now := time.Now()

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
			                started_at = COALESCE(started_at, ?), finished_at = ?
			WHERE id = ?
			  AND NOT EXISTS (
			      SELECT 1 FROM jobs WHERE id = ? AND status IN (?, ?, ?)
			  )
		`, status, now, exitCode, errMsg, failureType, failureDetails,
			startedAt, finishedAt, id, id,
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

	return nil
}

// AssignJobToWorker assigns a job to a worker
func (d *Database) AssignJobToWorker(jobID, workerID string) error {
	now := time.Now()
	_, err := d.db.Exec(`
		UPDATE jobs SET worker_id = ?, updated_at = ? WHERE id = ?
	`, workerID, now, jobID)

	if err != nil {
		return fmt.Errorf("failed to assign job to worker: %w", err)
	}

	return nil
}

// GetPendingJobs retrieves all pending jobs
func (d *Database) GetPendingJobs(limit int) ([]*Job, error) {
	rows, err := d.db.Query(`
		SELECT id, status, input_files, args, output_filename, streaming_output, output_files, worker_id, exit_code, error, failure_type, failure_details, retryable, auto_hw, timeout, direct_paths, progress_percent, eta_seconds,
		       created_at, updated_at, started_at, finished_at
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
			&job.WorkerID, &job.ExitCode, &job.Error, &job.FailureType, &job.FailureDetails, &job.Retryable, &job.AutoHW,
			&job.Timeout, &job.DirectPaths, &job.ProgressPercent, &job.EtaSeconds,
			&job.CreatedAt, &job.UpdatedAt, &job.StartedAt, &job.FinishedAt,
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
// If the worker already exists, it updates capabilities, resets status to idle,
// and clears eviction flags (TSI-1737 server restart recovery).
func (d *Database) CreateOrUpdateWorker(id, name string, caps protocol.WorkerCapabilities) (*Worker, error) {
	// TSI-2346: normalize on the write path too, so the same logical UUID
	// reported in different formats (hyphenated vs. compact, case, whitespace)
	// converges on one row instead of forking into unreachable duplicates.
	id = normalizeWorkerID(id)
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
		// Fallback: INSERT new worker
		_, err = d.db.Exec(`
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
	}

	return d.GetWorker(id)
}

// normalizeWorkerID canonicalizes a worker ID so lookups tolerate UUID
// formatting differences (hyphenated vs. compact). Non-UUID IDs pass through
// unchanged, since registration accepts arbitrary identifiers (TSI-2346).
func normalizeWorkerID(id string) string {
	if parsed, err := uuid.Parse(strings.TrimSpace(id)); err == nil {
		return parsed.String()
	}
	return strings.TrimSpace(id)
}

// GetWorker retrieves a worker by ID
func (d *Database) GetWorker(id string) (*Worker, error) {
	id = normalizeWorkerID(id)
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

// UpdateWorkerHeartbeat updates the worker's heartbeat timestamp and status
func (d *Database) UpdateWorkerHeartbeat(id string, status protocol.WorkerStatus) error {
	id = normalizeWorkerID(id)
	now := time.Now()
	result, err := d.db.Exec(`
		UPDATE workers SET status = ?, last_heartbeat = ? WHERE id = ?
	`, status, now, id)

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

// GetJobsForWorker retrieves jobs assigned to a specific worker with pending or queued status
func (d *Database) GetJobsForWorker(workerID string, limit int) ([]*Job, error) {
	workerID = normalizeWorkerID(workerID)
	rows, err := d.db.Query(`
		SELECT id, status, input_files, args, output_filename, streaming_output, output_files, worker_id, exit_code, error, failure_type, failure_details, retryable, auto_hw, timeout, direct_paths, progress_percent, eta_seconds,
		       created_at, updated_at, started_at, finished_at
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
	workerID = normalizeWorkerID(workerID)
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
		SELECT id, status, input_files, args, output_filename, streaming_output, output_files, worker_id, exit_code, error, failure_type, failure_details, retryable, auto_hw, timeout, direct_paths, progress_percent, eta_seconds,
		       created_at, updated_at, started_at, finished_at
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
			UPDATE jobs SET worker_id = ?, status = ?, updated_at = ?
			WHERE id = ? AND status = ? AND worker_id IS NULL
		`, workerID, protocol.JobStatusQueued, now, job.ID, protocol.JobStatusPending)
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
		job.Status = protocol.JobStatusQueued
		claimedJobs = append(claimedJobs, job)
	}

	// TSI-2347: mark the worker busy atomically with the assignment so
	// health.status reflects activity immediately instead of waiting for the
	// next heartbeat. The idle guard keeps an offline worker offline.
	//
	// Side effect: refreshing last_heartbeat here is deliberate. Assignment is
	// proof of liveness (the worker pulled this job), so bumping the timestamp
	// in the same transaction prevents the health monitor from racing us and
	// marking a just-assigned worker offline mid-transaction. It does not fake
	// a heartbeat: no state-table entry or GPU metrics are produced.

	if len(claimedJobs) > 0 {
		if _, err := tx.Exec(`
			UPDATE workers SET status = ?, last_heartbeat = ? WHERE id = ? AND status = ?
		`, protocol.WorkerStatusBusy, now, workerID, protocol.WorkerStatusIdle); err != nil {
			return nil, fmt.Errorf("failed to mark worker busy: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("failed to commit transaction: %w", err)
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

// RemoveOfflineWorkers removes workers that have been offline longer than the threshold
func (d *Database) RemoveOfflineWorkers(offlineThreshold time.Duration) (int64, error) {
	cutoff := time.Now().Add(-offlineThreshold)
	result, err := d.db.Exec(`
		DELETE FROM workers 
		WHERE status = ? AND last_heartbeat < ?
	`, protocol.WorkerStatusOffline, cutoff)
	if err != nil {
		return 0, fmt.Errorf("failed to remove offline workers: %w", err)
	}
	return result.RowsAffected()
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

func (d *Database) UpdateWorkerStatus(id string, status protocol.WorkerStatus) error {
	id = normalizeWorkerID(id)
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
		SELECT id, status, input_files, args, output_filename, streaming_output, output_files, worker_id, exit_code, error, failure_type, failure_details, retryable, auto_hw, timeout, direct_paths, progress_percent, eta_seconds,
		       created_at, updated_at, started_at, finished_at
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
	result, err := d.db.Exec(`
		UPDATE jobs SET status = ?, worker_id = NULL, started_at = NULL, updated_at = ?
		WHERE id = ? AND status = ?
	`, protocol.JobStatusPending, now, id, protocol.JobStatusRunning)
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

// ResetJobToPending resets a job to pending status, clearing worker assignment.
// This is used during worker failover to re-queue a job for another worker.
func (d *Database) ResetJobToPending(id string) error {
	now := time.Now()
	// TSI-2359: only reset jobs still in an active state. A failover sweep
	// racing a worker's completion must not drag a finished job back to
	// pending and re-run it.
	result, err := d.db.Exec(`
		UPDATE jobs SET status = ?, worker_id = NULL, started_at = NULL, updated_at = ?
		WHERE id = ? AND status IN (?, ?, ?)
	`, protocol.JobStatusPending, now, id,
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

// RecoverState performs recovery operations after server restart
// It resets jobs that were in running/queued state back to pending,
// removes offline worker records left over by previous runs (TSI-2366),
// and marks all workers as offline.
func (d *Database) RecoverState() (jobsReset int64, workersMarkedOffline int64, err error) {
	tx, err := d.db.Begin()
	if err != nil {
		return 0, 0, fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	// Reset running jobs to pending
	result, err := tx.Exec(`
		UPDATE jobs SET status = ?, updated_at = ?, worker_id = NULL, started_at = NULL
		WHERE status = ? OR status = ?
	`, protocol.JobStatusPending, time.Now(), protocol.JobStatusRunning, protocol.JobStatusQueued)
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

// RemoveStaleOfflineWorkers deletes every worker record currently marked
// offline. Called during startup recovery (RecoverState): after a server
// restart no worker can still be serving, so any offline row is residue from
// a previous run. Without this, records accumulate when the server crashes or
// shuts down before the health monitor's offline-threshold sweep ever runs,
// leaving stale duplicate entries for re-registering workers (TSI-2366).
// Live workers are unaffected: they re-register and are recreated with idle
// status by CreateOrUpdateWorker.
func (d *Database) RemoveStaleOfflineWorkers() (int64, error) {
	result, err := d.db.Exec(`
		DELETE FROM workers WHERE status = ?
	`, protocol.WorkerStatusOffline)
	if err != nil {
		return 0, fmt.Errorf("failed to remove stale offline workers: %w", err)
	}
	return result.RowsAffected()
}

// GetJobsByStatus retrieves all jobs with a specific status
func (d *Database) GetJobsByStatus(status protocol.JobStatus, limit int) ([]*Job, error) {
	rows, err := d.db.Query(`
		SELECT id, status, input_files, args, output_filename, streaming_output, output_files, worker_id, exit_code, error, failure_type, failure_details, retryable, auto_hw, timeout, direct_paths, progress_percent, eta_seconds,
		       created_at, updated_at, started_at, finished_at
		FROM jobs WHERE status = ?
		ORDER BY created_at ASC LIMIT ?
	`, status, limit)

	if err != nil {
		return nil, fmt.Errorf("failed to get jobs by status: %w", err)
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
func (d *Database) GetSchedulableWorkersByEncoder(encoderName string) ([]*Worker, error) {
	rows, err := d.db.Query(`
		SELECT DISTINCT w.id, w.name, w.status, w.gpu_model, w.encoders, w.decoders, w.video_encoders, w.video_decoders, w.ffmpeg_version, w.max_concurrent, w.evicted, w.evicted_at, w.hwaccels, w.codecs, w.filters, w.pix_fmts, w.formats, w.last_heartbeat, w.created_at
		FROM workers w, json_each(w.encoders) AS enc
		WHERE w.status != ? AND w.evicted = 0 AND enc.value = ?
		ORDER BY w.last_heartbeat DESC
	`, protocol.WorkerStatusOffline, encoderName)
	if err != nil {
		return nil, fmt.Errorf("failed to get schedulable workers by encoder: %w", err)
	}
	defer rows.Close()

	return d.scanWorkers(rows)
}

// UpdateWorkerCapabilities updates the worker's capabilities (encoders, decoders, etc.)
func (d *Database) UpdateWorkerCapabilities(id string, caps protocol.WorkerCapabilities) error {
	id = normalizeWorkerID(id)
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

// WorkerWithJobCount represents a worker with its active job count
type WorkerWithJobCount struct {
	Worker     *Worker
	ActiveJobs int
}

// GetIdleWorkersWithJobCount retrieves idle workers with their active job counts.
// This is useful for scheduling decisions.
func (d *Database) GetIdleWorkersWithJobCount() ([]WorkerWithJobCount, error) {
	workers, err := d.GetIdleWorkers()
	if err != nil {
		return nil, err
	}

	result := make([]WorkerWithJobCount, len(workers))
	for i, worker := range workers {
		count, err := d.GetWorkerActiveJobCount(worker.ID)
		if err != nil {
			return nil, fmt.Errorf("failed to get active job count for worker %s: %w", worker.ID, err)
		}
		result[i] = WorkerWithJobCount{
			Worker:     worker,
			ActiveJobs: count,
		}
	}

	return result, nil
}

// GetIdleWorkersByEncoderWithJobCount retrieves idle workers with a specific encoder
// and their active job counts, sorted by job count ascending.
func (d *Database) GetIdleWorkersByEncoderWithJobCount(encoderName string) ([]WorkerWithJobCount, error) {
	workers, err := d.GetIdleWorkersByEncoder(encoderName)
	if err != nil {
		return nil, err
	}

	result := make([]WorkerWithJobCount, len(workers))
	for i, worker := range workers {
		count, err := d.GetWorkerActiveJobCount(worker.ID)
		if err != nil {
			return nil, fmt.Errorf("failed to get active job count for worker %s: %w", worker.ID, err)
		}
		result[i] = WorkerWithJobCount{
			Worker:     worker,
			ActiveJobs: count,
		}
	}

	// Sort by active job count ascending
	sortWorkersByJobCount(result)

	return result, nil
}

// sortWorkersByJobCount sorts workers by active job count in ascending order
func sortWorkersByJobCount(workers []WorkerWithJobCount) {
	for i := 0; i < len(workers)-1; i++ {
		for j := i + 1; j < len(workers); j++ {
			if workers[j].ActiveJobs < workers[i].ActiveJobs {
				workers[i], workers[j] = workers[j], workers[i]
			}
		}
	}
}

// GetWorkerEncoders retrieves the list of encoders for a specific worker
func (d *Database) GetWorkerEncoders(workerID string) ([]string, error) {
	workerID = normalizeWorkerID(workerID)
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
// with rich metadata (name, description, type, is_hw) from the video_encoders column.
func (d *Database) GetAllEncodersInfo() ([]protocol.EncoderInfo, error) {
	rows, err := d.db.Query(`SELECT DISTINCT video_encoders FROM workers WHERE video_encoders != '[]'`)
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
			if _, exists := seen[enc.Name]; !exists {
				seen[enc.Name] = enc
			}
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

// GetAllDecodersInfo retrieves a unique list of all decoders across all workers
// with rich metadata (name, description, type, is_hw) from the video_decoders column.
func (d *Database) GetAllDecodersInfo() ([]protocol.DecoderInfo, error) {
	rows, err := d.db.Query(`SELECT DISTINCT video_decoders FROM workers WHERE video_decoders != '[]'`)
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
			if _, exists := seen[dec.Name]; !exists {
				seen[dec.Name] = dec
			}
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
		return nil, fmt.Errorf("migration event not found")
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
		ORDER BY timestamp DESC
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
		ORDER BY timestamp DESC
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

// GetRunningJobsByWorker retrieves all running/queued jobs for a specific worker.
func (d *Database) GetRunningJobsByWorker(workerID string) ([]*Job, error) {
	workerID = normalizeWorkerID(workerID)
	rows, err := d.db.Query(`
		SELECT id, status, input_files, args, output_filename, streaming_output, output_files, worker_id, exit_code, error, failure_type, failure_details, retryable, auto_hw, timeout, direct_paths, progress_percent, eta_seconds,
		       created_at, updated_at, started_at, finished_at
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
	rows, err := tx.Query(`
		UPDATE jobs SET status = ?, worker_id = NULL, started_at = NULL, updated_at = ?
		WHERE worker_id = ? AND status IN (?, ?)
		RETURNING id
	`, protocol.JobStatusPending, now, workerID,
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

// GetJobRetryCount returns the number of times a job has been migrated/retried.
// This is calculated by counting migration events that include this job ID.
func (d *Database) GetJobRetryCount(jobID string) (int, error) {
	// Count migration events where this job ID appears in the job_ids JSON array.
	// Use json_each to properly expand the JSON array and check for exact matches,
	// avoiding partial match issues that would occur with LIKE.
	var count int
	err := d.db.QueryRow(`
		SELECT COUNT(*) FROM migration_events
		WHERE id IN (
			SELECT me.id FROM migration_events me, json_each(me.job_ids)
			WHERE json_each.value = ?
		)
	`, jobID).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("failed to get job retry count: %w", err)
	}
	return count, nil
}

// --- Worker Eviction Methods (TSI-761) ---

// MarkWorkerEvicted marks a worker as evicted (slow node) with the current timestamp.
func (d *Database) MarkWorkerEvicted(workerID string) error {
	workerID = normalizeWorkerID(workerID)
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
	workerID = normalizeWorkerID(workerID)
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
	workerID = normalizeWorkerID(workerID)
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
