package db

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/mattn/go-sqlite3"
	"github.com/tsic404/rffmpeg/pkg/protocol"
)

// Database wraps the SQL database connection
type Database struct {
	db       *sql.DB
	notifier *JobNotifier

	// terminalHook runs for every job that reaches a terminal state, whichever
	// writer produced it. The rate limiter uses it to release the client's
	// quota slot: a terminal job that never releases its slot permanently
	// inflates the per-client counter and 429s that client's later submits.
	terminalHookMu sync.RWMutex
	terminalHook   func(jobID string)
}

// DefaultBusyTimeoutMS is how long a SQLite writer waits for a held write lock
// before failing with SQLITE_BUSY. It is long enough for a submission burst to
// queue behind the lock instead of surfacing as a retryable conflict.
const DefaultBusyTimeoutMS = 5000

// sqliteDSN builds a SQLite DSN with the pragmas required for safe concurrent
// use: WAL journaling so readers never block the writer, a busy
// timeout so concurrent writes queue instead of failing with SQLITE_BUSY,
// foreign-key enforcement so ON DELETE CASCADE fires (upload_chunks cleanup),
// and immediate transactions so lock acquisition happens at BEGIN rather than
// at first write (avoids deadlock-prone deferred-to-write upgrades).
// busyTimeoutMS is applied to every pooled connection, not just one.
func sqliteDSN(dbPath string, busyTimeoutMS int) string {
	params := fmt.Sprintf("_busy_timeout=%d&_journal_mode=WAL&_fk=1&_txlock=immediate", busyTimeoutMS)
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

// IsConcurrentWriteError reports whether err is a transient SQLite write
// conflict (SQLITE_BUSY / SQLITE_LOCKED family) — a database file/table lock
// that concurrent writers surface when the busy timeout is exhausted, not a
// hard fault. Callers classify this as a retryable conflict rather than an
// internal server error.
func IsConcurrentWriteError(err error) bool {
	if err == nil {
		return false
	}
	var sqliteErr sqlite3.Error
	if errors.As(err, &sqliteErr) {
		switch sqliteErr.Code {
		case sqlite3.ErrBusy, sqlite3.ErrLocked:
			return true
		}
	}
	return false
}

// New creates a new database connection and initializes tables, using the
// default busy timeout (DefaultBusyTimeoutMS).
func New(dbPath string) (*Database, error) {
	return NewWithBusyTimeout(dbPath, DefaultBusyTimeoutMS)
}

// NewWithBusyTimeout creates a new database connection and initializes tables
// with an explicit SQLite busy timeout in milliseconds — how long a writer
// waits for a held write lock before SQLITE_BUSY. 0 disables the wait, so any
// concurrent write fails immediately; that is what makes the API's retryable
// 429 (submit_conflict) path reachable from an E2E burst, since a burst of
// sub-millisecond INSERTs otherwise never outlasts even a 1ms wait. Negative
// values fall back to DefaultBusyTimeoutMS.
func NewWithBusyTimeout(dbPath string, busyTimeoutMS int) (*Database, error) {
	if dbPath == "" {

		return nil, fmt.Errorf("database path must not be empty (use \":memory:\" explicitly for a test database)")
	}
	if busyTimeoutMS < 0 {
		busyTimeoutMS = DefaultBusyTimeoutMS
	}

	db, err := sql.Open("sqlite3", sqliteDSN(dbPath, busyTimeoutMS))
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}
	d := &Database{db: db, notifier: newJobNotifier()}

	if dbPath == ":memory:" || dbPath == "file::memory:" {
		db.SetMaxOpenConns(1)
	}

	existing, err := d.hasUserTables()
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to inspect database schema: %w", err)
	}
	if existing {
		if err := d.verifyIntegrity(); err != nil {
			db.Close()
			return nil, fmt.Errorf("database integrity check failed: %w", err)
		}
		if err := d.verifyCoreTables(); err != nil {
			db.Close()
			return nil, fmt.Errorf("database schema check failed: %w", err)
		}
	}

	if err := d.initTables(); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to initialize tables: %w", err)
	}

	if err := d.verifyColumns(); err != nil {
		db.Close()
		return nil, fmt.Errorf("database column check failed: %w", err)
	}

	return d, nil
}

// tableSpec describes the minimum schema the server needs for one table: its
// name, the columns server code reads or writes, and the primary-key column
// that upserts (ON CONFLICT) depend on. core marks tables that have existed in
// every released schema since the initial commit: a pre-existing database
// missing one of them is corrupt (their absence can never be a legitimate
// upgrade gap), so startup must fail rather than recreate them empty. Non-core
// tables (ws_job_seq, job_redistributions) were added by later releases and
// are legitimately absent on an old database, so initTables may create them.
type tableSpec struct {
	name    string
	pk      string
	core    bool
	columns []string
}

var requiredTables = []tableSpec{
	{name: "jobs", pk: "id", core: true, columns: []string{
		"id", "status", "input_files", "args", "output_filename",
		"streaming_output", "output_files", "worker_id", "assigned_worker",
		"worker_name", "exit_code", "error", "failure_type", "failure_details",
		"retryable", "auto_hw", "cached", "timeout", "direct_paths",
		"progress_percent", "eta_seconds", "created_at", "updated_at",
		"claimed_at", "started_at", "finished_at",
	}},
	{name: "files", pk: "id", core: true, columns: []string{
		"id", "filename", "path", "size", "checksum", "created_at",
	}},
	{name: "workers", pk: "id", core: true, columns: []string{
		"id", "name", "status", "gpu_model", "encoders", "decoders",
		"video_encoders", "video_decoders", "gpu_devices", "ffmpeg_version", "max_concurrent",
		"evicted", "evicted_at", "hwaccels", "codecs", "filters", "pix_fmts",
		"formats", "last_heartbeat", "created_at",
	}},
	{name: "upload_sessions", pk: "id", core: true, columns: []string{
		"id", "filename", "file_size", "chunk_size", "total_chunks",
		"uploaded_chunks", "file_checksum", "file_id", "status", "created_at",
		"updated_at", "expires_at",
	}},
	{name: "upload_chunks", pk: "id", core: true, columns: []string{
		"id", "upload_id", "chunk_index", "chunk_size", "checksum", "path",
		"created_at",
	}},
	{name: "migration_events", pk: "id", core: true, columns: []string{
		"id", "timestamp", "worker_id", "worker_name", "reason",
		"retry_count", "job_ids", "jobs_migrated", "created_at",
	}},
	{name: "job_redistributions", pk: "id", core: false, columns: []string{
		"id", "migration_event_id", "job_id", "retry_count", "target_worker_id",
		"target_worker_name", "created_at",
	}},
	{name: "worker_eviction_events", pk: "id", core: true, columns: []string{
		"id", "timestamp", "worker_id", "event_type", "current_throughput",
		"cluster_median", "decision_reason", "created_at",
	}},
	{name: "ws_job_seq", pk: "job_id", core: false, columns: []string{
		"job_id", "last_seq", "updated_at",
	}},
}

// hasUserTables reports whether the database already contains any user table
// (anything in sqlite_master that is not a SQLite internal table). A fresh
// database has none; a pre-existing one has at least one.
func (d *Database) hasUserTables() (bool, error) {
	var n int
	err := d.db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`,
	).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("inspect sqlite_master: %w", err)
	}
	return n > 0, nil
}

// verifyIntegrity asserts the SQLite file is structurally sound and that
// foreign-key enforcement is on. integrity_check detects malformed pages and
// broken b-tree structure — the corruption a half-landed WAL recovery leaves
// behind — but not content-level bit flips in row data.
func (d *Database) verifyIntegrity() error {
	var result string
	if err := d.db.QueryRow(`PRAGMA integrity_check`).Scan(&result); err != nil {
		return fmt.Errorf("integrity_check failed: %w", err)
	}
	if result != "ok" {
		return fmt.Errorf("integrity_check reported corruption: %s", result)
	}

	var fk int
	if err := d.db.QueryRow(`PRAGMA foreign_keys`).Scan(&fk); err != nil {
		return fmt.Errorf("foreign_keys check failed: %w", err)
	}
	if fk != 1 {
		return fmt.Errorf("foreign_keys enforcement is off (got %d, want 1)", fk)
	}
	return nil
}

// verifyCoreTables fails if any core table is absent from sqlite_master. It
// must run before initTables: CREATE TABLE IF NOT EXISTS would otherwise
// recreate a missing core table as empty, silently discarding the rows that
// should have been there. Non-core tables added by later releases
// are excluded — their absence is a legitimate upgrade gap that initTables
// fills.
func (d *Database) verifyCoreTables() error {
	for _, t := range requiredTables {
		if !t.core {
			continue
		}
		var name string
		err := d.db.QueryRow(
			`SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`, t.name,
		).Scan(&name)
		if err == sql.ErrNoRows {
			return fmt.Errorf("required table %q is missing (no such table: %s)", t.name, t.name)
		}
		if err != nil {
			return fmt.Errorf("failed to verify table %q: %w", t.name, err)
		}
	}
	return nil
}

// verifyColumns fails if a required table is missing one of the columns the
// server depends on, or has lost its primary-key constraint. Table names are
// hard-coded identifiers, not user input, so they are safe to inline in the
// PRAGMA (SQLite does not bind identifiers).
func (d *Database) verifyColumns() error {
	for _, t := range requiredTables {
		rows, err := d.db.Query("PRAGMA table_info(" + t.name + ")")
		if err != nil {
			return fmt.Errorf("inspect columns of %q: %w", t.name, err)
		}
		type colInfo struct{ pk int }
		cols := make(map[string]colInfo)
		for rows.Next() {
			var (
				cid     int
				name    string
				ctype   string
				notNull int
				dflt    sql.NullString
				pk      int
			)
			if err := rows.Scan(&cid, &name, &ctype, &notNull, &dflt, &pk); err != nil {
				rows.Close()
				return fmt.Errorf("scan columns of %q: %w", t.name, err)
			}
			cols[name] = colInfo{pk: pk}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("iterate columns of %q: %w", t.name, err)
		}

		for _, c := range t.columns {
			if _, ok := cols[c]; !ok {
				return fmt.Errorf("required table %q is missing column %q", t.name, c)
			}
		}
		if t.pk != "" {
			if c, ok := cols[t.pk]; !ok || c.pk == 0 {
				return fmt.Errorf("required table %q is missing its primary key on %q", t.name, t.pk)
			}
		}
	}
	return nil
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

// SetTerminalHook installs a callback invoked for every job that reaches a
// terminal state, whichever writer produced it: worker report, cancel, the
// scheduler/monitor failure paths, or the starvation sweep. It covers the
// writers that bypass HTTP handlers, so per-job bookkeeping (quota release)
// cannot be forgotten on a new terminal path.
func (d *Database) SetTerminalHook(fn func(jobID string)) {
	d.terminalHookMu.Lock()
	d.terminalHook = fn
	d.terminalHookMu.Unlock()
}

// NotifyTerminal wakes in-process waiters after a successful terminal-status
// write and runs the terminal hook. It is nil-safe for databases constructed
// without a notifier or a hook.
func (d *Database) NotifyTerminal(jobID string) {
	d.terminalHookMu.RLock()
	hook := d.terminalHook
	d.terminalHookMu.RUnlock()
	if hook != nil {
		hook(jobID)
	}
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
			claimed_at DATETIME,
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
			gpu_devices TEXT DEFAULT '[]',
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
			file_id TEXT,
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

		-- a (upload_id, chunk_index) pair identifies one chunk.
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

		-- per-job redistribution targets. A migration event records
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
			retry_count INTEGER NOT NULL DEFAULT 0,
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

		-- per-job WebSocket sequence counters for gap detection.
		-- Transient by design: rows live only while a job is streaming (the
		-- hub deletes them on terminal broadcasts) and are safe to rebuild —
		-- dropping the table only resets restart-resume numbering to 1, it
		-- never loses job data.

		CREATE INDEX IF NOT EXISTS idx_eviction_events_timestamp ON worker_eviction_events(timestamp);
		CREATE INDEX IF NOT EXISTS idx_eviction_events_worker_id ON worker_eviction_events(worker_id);
	`)

	for _, stmt := range []string{
		`ALTER TABLE jobs ADD COLUMN failure_type TEXT DEFAULT ''`,
		`ALTER TABLE jobs ADD COLUMN failure_details TEXT DEFAULT ''`,
		`ALTER TABLE jobs ADD COLUMN retryable INTEGER DEFAULT 0`,
		`ALTER TABLE jobs ADD COLUMN timeout INTEGER`,
		`ALTER TABLE jobs ADD COLUMN claimed_at DATETIME`,
		`ALTER TABLE workers ADD COLUMN evicted INTEGER DEFAULT 0`,
		`ALTER TABLE workers ADD COLUMN evicted_at DATETIME`,
		`ALTER TABLE jobs ADD COLUMN direct_paths TEXT DEFAULT '[]'`,
		`ALTER TABLE jobs ADD COLUMN cached INTEGER DEFAULT 0`,
		`ALTER TABLE jobs ADD COLUMN assigned_worker TEXT`,
		`ALTER TABLE jobs ADD COLUMN worker_name TEXT`,
		`ALTER TABLE workers ADD COLUMN video_encoders TEXT DEFAULT '[]'`,
		`ALTER TABLE workers ADD COLUMN video_decoders TEXT DEFAULT '[]'`,
		`ALTER TABLE workers ADD COLUMN gpu_devices TEXT DEFAULT '[]'`,
		`ALTER TABLE workers ADD COLUMN hwaccels TEXT DEFAULT ''`,
		`ALTER TABLE workers ADD COLUMN codecs TEXT DEFAULT ''`,
		`ALTER TABLE workers ADD COLUMN filters TEXT DEFAULT ''`,
		`ALTER TABLE workers ADD COLUMN pix_fmts TEXT DEFAULT ''`,
		`ALTER TABLE workers ADD COLUMN formats TEXT DEFAULT ''`,
		`ALTER TABLE job_redistributions ADD COLUMN retry_count INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE upload_sessions ADD COLUMN file_id TEXT`,
	} {
		if _, err := d.db.Exec(stmt); err != nil {
			errStr := err.Error()

			if !strings.Contains(strings.ToLower(errStr), "duplicate column") {
				return err
			}
		}
	}

	if err := d.migrateEncoderUnavailableClassification(); err != nil {
		return err
	}

	if err := d.migrateRetryableColumn(); err != nil {
		return err
	}

	// jobs.timeout must hold a nanosecond duration (INTEGER). Fresh
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
// It runs inside a transaction so a crash cannot leave the column
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

// migrateEncoderUnavailableClassification re-labels historical submit-time
// rejections. Before ENCODER_UNAVAILABLE existed, the submit-time "no worker
// has this encoder" fast-fail persisted rows as ENCODER_UNSUPPORTED with
// worker_id NULL; once ENCODER_UNAVAILABLE landed, ENCODER_UNSUPPORTED became
// reserved for the worker's runtime classification (which always carries a
// worker_id). Without this relabel those legacy NULL-worker rows would be
// indistinguishable. The WHERE predicate is bounded and idempotent, so it is
// safe to run on every open with no schema-version bookkeeping.
func (d *Database) migrateEncoderUnavailableClassification() error {
	if _, err := d.db.Exec(`
		UPDATE jobs SET failure_type = ?
		WHERE failure_type = ? AND worker_id IS NULL
	`, string(protocol.FailureEncoderUnavailable), string(protocol.FailureEncoderUnsupported)); err != nil {
		return fmt.Errorf("migrate ENCODER_UNAVAILABLE classification: %w", err)
	}
	return nil
}

// migrateRetryableColumn backfills the retryable column for rows persisted
// before retryable was written alongside failure_type: those rows carry the
// schema DEFAULT 0, so the retryable classifications (TIMEOUT, WORKER_CRASH —
// the set protocol.FailureType.Retryable() returns true for) must be flipped
// to 1. The predicate is bounded and idempotent, so it is safe to run on every
// open with no schema-version bookkeeping.
func (d *Database) migrateRetryableColumn() error {
	if _, err := d.db.Exec(`
		UPDATE jobs SET retryable = 1
		WHERE failure_type IN (?, ?) AND retryable = 0
	`, string(protocol.FailureTimeout), string(protocol.FailureWorkerCrash)); err != nil {
		return fmt.Errorf("migrate retryable column: %w", err)
	}
	return nil
}

// jobExec is the statement executor shared by *sql.DB and *sql.Tx, so the
// requeue helpers below run identically inside and outside a transaction.
type jobExec interface {
	Exec(query string, args ...any) (sql.Result, error)
}

// Worker-availability predicates shared by the stuck-job scan and the sweep
// UPDATE, so the two statements cannot drift. A running job on an
// offline-but-still-present worker is deliberately NOT matched: that is
// exactly the failover migration's case (C-84 ②), and failing it here would
// race the requeue. The sweep only steps in when nobody can migrate it.
const (
	missingWorkerPredicate = `(j.worker_id IS NULL OR NOT EXISTS (
		SELECT 1 FROM workers w WHERE w.id = j.worker_id
	))`
	offlineWorkerPredicate = `EXISTS (
		SELECT 1 FROM workers w WHERE w.id = j.worker_id AND w.status = ?
	)`
	liveWorkerPredicate = `EXISTS (
		SELECT 1 FROM workers w WHERE w.id = j.worker_id AND w.status != ?
	)`
)
