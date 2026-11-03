package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/tsic404/rffmpeg/pkg/protocol"
)

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
	GPUDevices    string // JSON array of GPUDeviceInfo
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
	gpuDevicesJSON, err := json.Marshal(caps.GPUDevices)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal gpu devices: %w", err)
	}

	var gpuModel interface{}
	if caps.GPUModel != "" {
		gpuModel = caps.GPUModel
	}

	_, err = d.db.Exec(`
		INSERT INTO workers (id, name, status, gpu_model, encoders, decoders, video_encoders, video_decoders, gpu_devices, ffmpeg_version, max_concurrent, evicted, evicted_at, hwaccels, codecs, filters, pix_fmts, formats, last_heartbeat, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, id, name, protocol.WorkerStatusIdle, gpuModel, string(encodersJSON), string(decodersJSON),
		string(videoEncodersJSON), string(videoDecodersJSON), string(gpuDevicesJSON),
		caps.FFmpegVersion, caps.MaxConcurrent, false, nil,
		caps.Hwaccels, caps.Codecs, caps.Filters, caps.PixFmts, caps.Formats,
		now, now)

	if err != nil {
		return nil, fmt.Errorf("failed to create worker: %w", err)
	}

	return d.GetWorker(id)
}

// CreateOrUpdateWorker creates a new worker or updates an existing one on
// re-registration: an existing id gets fresh capabilities and status idle, with
// any eviction flag preserved — releasing a mark belongs to throughput recovery
// or an operator's manual clear (C-79, C-118). A new id whose name matches
// stale residue of the same logical worker (offline row, or live row with an
// expired heartbeat) replaces that row instead of forking a duplicate; live
// same-name rows with a fresh heartbeat are never deleted. heartbeatTimeout is
// the freshness window; <=0 deletes only offline residue.
func (d *Database) CreateOrUpdateWorker(id, name string, caps protocol.WorkerCapabilities, heartbeatTimeout time.Duration) (*Worker, error) {

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
	gpuDevicesJSON, err := json.Marshal(caps.GPUDevices)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal gpu devices: %w", err)
	}

	var gpuModel interface{}
	if caps.GPUModel != "" {
		gpuModel = caps.GPUModel
	}

	result, err := d.db.Exec(`
		UPDATE workers SET name=?, status=?, gpu_model=?, encoders=?, decoders=?,
		video_encoders=?, video_decoders=?, gpu_devices=?, ffmpeg_version=?, max_concurrent=?,
		hwaccels=?, codecs=?, filters=?, pix_fmts=?, formats=?,
		last_heartbeat=? WHERE id=?
	`, name, protocol.WorkerStatusIdle, gpuModel,
		string(encodersJSON), string(decodersJSON),
		string(videoEncodersJSON), string(videoDecodersJSON), string(gpuDevicesJSON),
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

		tx, err := d.db.Begin()
		if err != nil {
			return nil, fmt.Errorf("failed to begin worker upsert transaction: %w", err)
		}
		defer tx.Rollback()

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

		_, err = tx.Exec(`
			INSERT INTO workers (id, name, status, gpu_model, encoders, decoders,
			video_encoders, video_decoders, gpu_devices, ffmpeg_version, max_concurrent, evicted,
			evicted_at, hwaccels, codecs, filters, pix_fmts, formats, last_heartbeat, created_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		`, id, name, protocol.WorkerStatusIdle, gpuModel,
			string(encodersJSON), string(decodersJSON),
			string(videoEncodersJSON), string(videoDecodersJSON), string(gpuDevicesJSON),
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
// identifiers.
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
		SELECT id, name, status, gpu_model, encoders, decoders, video_encoders, video_decoders, gpu_devices, ffmpeg_version, max_concurrent, evicted, evicted_at, hwaccels, codecs, filters, pix_fmts, formats, last_heartbeat, created_at
		FROM workers WHERE id = ?
	`, id).Scan(
		&worker.ID, &worker.Name, &worker.Status, &worker.GPUModel,
		&worker.Encoders, &worker.Decoders, &worker.VideoEncoders, &worker.VideoDecoders, &worker.GPUDevices, &worker.FFmpegVersion,
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
// the pull path, resurrecting a fixed bug through a race. Offline/evicted guards
// keep dead workers from refreshing liveness.
func (d *Database) UpdateWorkerHeartbeat(id string, status protocol.WorkerStatus) error {

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
// the cutoff and returns their IDs so callers can drop them from the
// in-memory state table (stale entries otherwise feed dead nodes into
// slow-node medians). MarkOfflineWorkers does not reset last_heartbeat.
// Evicted records are exempt: a marked worker cannot heartbeat its record
// fresh, so sweeping it here would drop the mark and let the process
// re-register as a brand-new worker. The row lives until an operator clears
// the mark or a new process registers under the same name.
func (d *Database) RemoveOfflineWorkers(offlineThreshold time.Duration) ([]string, error) {
	cutoff := time.Now().Add(-offlineThreshold)

	tx, err := d.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	rows, err := tx.Query(`
		SELECT id FROM workers
		WHERE status = ? AND last_heartbeat < ? AND evicted = 0
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
		WHERE status = ? AND last_heartbeat < ? AND evicted = 0
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
		SELECT id, name, status, gpu_model, encoders, decoders, video_encoders, video_decoders, gpu_devices, ffmpeg_version, max_concurrent, evicted, evicted_at, hwaccels, codecs, filters, pix_fmts, formats, last_heartbeat, created_at
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
		SELECT id, name, status, gpu_model, encoders, decoders, video_encoders, video_decoders, gpu_devices, ffmpeg_version, max_concurrent, evicted, evicted_at, hwaccels, codecs, filters, pix_fmts, formats, last_heartbeat, created_at
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
// while a worker is currently busy.
//
// The result is NOT filtered by heartbeat freshness: a worker whose last
// heartbeat is stale but that has not yet been marked offline still counts as
// schedulable. Callers that need liveness (submit-time fail-fast, starvation
// sweeps) must use GetLiveSchedulableWorkers instead.
func (d *Database) GetSchedulableWorkers() ([]*Worker, error) {
	rows, err := d.db.Query(`
		SELECT id, name, status, gpu_model, encoders, decoders, video_encoders, video_decoders, gpu_devices, ffmpeg_version, max_concurrent, evicted, evicted_at, hwaccels, codecs, filters, pix_fmts, formats, last_heartbeat, created_at
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
// callers must not treat it as available. A non-positive
// freshness disables the check (behaves like GetSchedulableWorkers).
func (d *Database) GetLiveSchedulableWorkers(freshness time.Duration) ([]*Worker, error) {
	freshnessClause, freshnessArgs := heartbeatFreshnessSQL(freshness)
	rows, err := d.querySchedulable(`
		SELECT id, name, status, gpu_model, encoders, decoders, video_encoders, video_decoders, gpu_devices, ffmpeg_version, max_concurrent, evicted, evicted_at, hwaccels, codecs, filters, pix_fmts, formats, last_heartbeat, created_at
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
	workerID = NormalizeWorkerID(workerID)
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
// starvation of late-registered workers.
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

// scanWorkers scans multiple worker rows
func (d *Database) scanWorkers(rows *sql.Rows) ([]*Worker, error) {
	var workers []*Worker
	for rows.Next() {
		worker := &Worker{}
		err := rows.Scan(
			&worker.ID, &worker.Name, &worker.Status, &worker.GPUModel,
			&worker.Encoders, &worker.Decoders, &worker.VideoEncoders, &worker.VideoDecoders, &worker.GPUDevices, &worker.FFmpegVersion,
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

// RemoveStaleWorkers deletes worker records whose last heartbeat predates
// now - staleThreshold. It runs during startup recovery after RecoverState has
// marked every worker offline; without it, records accumulate when the server
// crashes before the health monitor's offline sweep (10m default), leaving
// stale entries that /api/v1/workers keeps returning as offline nodes.
// Fresh-heartbeat workers (a live process that survived a restart) are
// preserved — they re-register and are recreated as idle. Deleting only
// genuinely stale rows is what makes this lazy versus delete-everything.
// Evicted records are exempt for the reason documented on RemoveOfflineWorkers.
func (d *Database) RemoveStaleWorkers(staleThreshold time.Duration) (int64, error) {
	cutoff := time.Now().Add(-staleThreshold)
	result, err := d.db.Exec(`DELETE FROM workers WHERE last_heartbeat < ? AND evicted = 0`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("failed to remove stale workers: %w", err)
	}
	return result.RowsAffected()
}

// GetAllWorkers retrieves all workers from the database
func (d *Database) GetAllWorkers() ([]*Worker, error) {
	rows, err := d.db.Query(`
		SELECT id, name, status, gpu_model, encoders, decoders, video_encoders, video_decoders, gpu_devices, ffmpeg_version, max_concurrent, evicted, evicted_at, hwaccels, codecs, filters, pix_fmts, formats, last_heartbeat, created_at
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

	rows, err := d.db.Query(`
		SELECT DISTINCT w.id, w.name, w.status, w.gpu_model, w.encoders, w.decoders, w.video_encoders, w.video_decoders, w.gpu_devices, w.ffmpeg_version, w.max_concurrent, w.evicted, w.evicted_at, w.hwaccels, w.codecs, w.filters, w.pix_fmts, w.formats, w.last_heartbeat, w.created_at
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
		SELECT DISTINCT w.id, w.name, w.status, w.gpu_model, w.encoders, w.decoders, w.video_encoders, w.video_decoders, w.gpu_devices, w.ffmpeg_version, w.max_concurrent, w.evicted, w.evicted_at, w.hwaccels, w.codecs, w.filters, w.pix_fmts, w.formats, w.last_heartbeat, w.created_at
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
// job can be queued while the worker is currently busy.
//
// Like GetSchedulableWorkers, the result is NOT filtered by heartbeat
// freshness; use GetLiveSchedulableWorkersByEncoder for the submit-time
// fail-fast path.
func (d *Database) GetSchedulableWorkersByEncoder(encoderName string) ([]*Worker, error) {
	rows, err := d.querySchedulableByEncoder(encoderName, 0)
	if err != nil {
		return nil, fmt.Errorf("failed to get schedulable workers by encoder: %w", err)
	}
	return rows, nil
}

// GetLiveSchedulableWorkersByEncoder is GetSchedulableWorkersByEncoder with a
// heartbeat freshness window: workers whose last_heartbeat is older than
// freshness are excluded. A non-positive freshness disables the
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
		SELECT DISTINCT w.id, w.name, w.status, w.gpu_model, w.encoders, w.decoders, w.video_encoders, w.video_decoders, w.gpu_devices, w.ffmpeg_version, w.max_concurrent, w.evicted, w.evicted_at, w.hwaccels, w.codecs, w.filters, w.pix_fmts, w.formats, w.last_heartbeat, w.created_at
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
	gpuDevicesJSON, err := json.Marshal(caps.GPUDevices)
	if err != nil {
		return fmt.Errorf("failed to marshal gpu devices: %w", err)
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
			gpu_devices = ?,
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
		string(videoEncodersJSON), string(videoDecodersJSON), string(gpuDevicesJSON),
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
// is not starved by an earlier one that always appears first.
type WorkerWithJobCount struct {
	Worker        *Worker
	ActiveJobs    int
	CompletedJobs int
}

// GetIdleWorkersWithJobCount retrieves idle workers with their active and
// completed job counts. The active count drives scheduling; the completed
// count breaks ties so late-registered workers are not starved.
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
// constant rather than 2N.
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
// ties on completed job count so the worker that has done fewer jobs wins.
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

// ClearWorkerEviction releases a worker's eviction mark and records the
// matching recovery event in the same transaction, so a cleared mark can never
// be unrecorded. reason identifies the clearing path in the audit trail
// (automatic throughput recovery vs. manual operator clear); currentThroughput
// and clusterMedian are the values at the moment of the decision.
//
// Only a set mark is a state change: clearing an already-clear worker returns
// false and writes no audit row. Returns protocol.ErrWorkerNotFound when the
// worker does not exist.
func (d *Database) ClearWorkerEviction(workerID string, currentThroughput, clusterMedian float64, reason string) (bool, error) {
	workerID = NormalizeWorkerID(workerID)
	tx, err := d.db.Begin()
	if err != nil {
		return false, fmt.Errorf("begin eviction clear: %w", err)
	}
	defer tx.Rollback()

	result, err := tx.Exec(`
		UPDATE workers SET evicted = 0, evicted_at = NULL WHERE id = ? AND evicted = 1
	`, workerID)
	if err != nil {
		return false, fmt.Errorf("failed to clear worker eviction: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("failed to check rows affected: %w", err)
	}
	if rows == 0 {
		var exists int
		err := tx.QueryRow(`SELECT COUNT(*) FROM workers WHERE id = ?`, workerID).Scan(&exists)
		if err != nil {
			return false, fmt.Errorf("failed to look up worker: %w", err)
		}
		if exists == 0 {
			return false, protocol.ErrWorkerNotFound
		}
		return false, nil
	}

	now := time.Now()
	if _, err := tx.Exec(evictionEventInsertSQL,
		uuid.New().String(), now, workerID, string(EvictionEventRecovered),
		currentThroughput, clusterMedian, reason, now,
	); err != nil {
		return false, fmt.Errorf("failed to record eviction recovery event: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit eviction clear: %w", err)
	}
	return true, nil
}

// GetWorkerByReference resolves a worker by identifier or registered name.
// An identifier match wins; names resolve to the most recently registered row
// when stale duplicates of the same name exist.
func (d *Database) GetWorkerByReference(ref string) (*Worker, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, protocol.ErrWorkerNotFound
	}
	worker, err := d.GetWorker(ref)
	if err == nil {
		return worker, nil
	}
	if !errors.Is(err, protocol.ErrWorkerNotFound) {
		return nil, err
	}

	worker = &Worker{}
	err = d.db.QueryRow(`
		SELECT id, name, status, gpu_model, encoders, decoders, video_encoders, video_decoders, gpu_devices, ffmpeg_version, max_concurrent, evicted, evicted_at, hwaccels, codecs, filters, pix_fmts, formats, last_heartbeat, created_at
		FROM workers WHERE name = ?
		ORDER BY created_at DESC, id DESC LIMIT 1
	`, ref).Scan(
		&worker.ID, &worker.Name, &worker.Status, &worker.GPUModel,
		&worker.Encoders, &worker.Decoders, &worker.VideoEncoders, &worker.VideoDecoders, &worker.GPUDevices, &worker.FFmpegVersion,
		&worker.MaxConcurrent, &worker.Evicted, &worker.EvictedAt,
		&worker.Hwaccels, &worker.Codecs, &worker.Filters, &worker.PixFmts, &worker.Formats,
		&worker.LastHeartbeat, &worker.CreatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, protocol.ErrWorkerNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get worker by reference: %w", err)
	}
	return worker, nil
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

// EvictionEventType represents the type of eviction event.
type EvictionEventType string

const (
	EvictionEventEvicted   EvictionEventType = "evicted"
	EvictionEventRecovered EvictionEventType = "recovered"
)

// Eviction recovery reason values recorded in worker_eviction_events.
// decision_reason. The two clear paths must stay distinguishable in the audit,
// so each path passes its own value instead of sharing a call-site literal.
const (
	EvictionReasonThroughputRecovered = "worker EWMA throughput recovered within cluster median threshold"
	EvictionReasonManualClear         = "manual eviction clear by operator"
)

// evictionEventInsertSQL is shared by the direct recorder and the transactional
// recovery path so both write the same columns.
const evictionEventInsertSQL = `
	INSERT INTO worker_eviction_events (id, timestamp, worker_id, event_type, current_throughput, cluster_median, decision_reason, created_at)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?)
`

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

	_, err := d.db.Exec(evictionEventInsertSQL,
		id, now, workerID, string(eventType), currentThroughput, clusterMedian, reason, now)
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
