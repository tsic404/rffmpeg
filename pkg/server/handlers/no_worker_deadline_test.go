package handlers

import (
	"database/sql"
	"testing"
	"time"

	"github.com/tsic404/rffmpeg/pkg/protocol"
	"github.com/tsic404/rffmpeg/pkg/server/db"
)

// TestDBJobToJobInfo_NoWorkerDeadline pins the NoWorkerDeadline attachment
// semantics to the scheduler's starvation sweep filter: only unassigned
// pending jobs in a cluster with NO live schedulable worker can ever be
// failed with NO_WORKER_AVAILABLE, so only they carry the deadline.
//
// Queued jobs already own a worker_id and are never swept; a pending job
// behind a busy-but-live worker is never swept either (the sweep's live-worker
// guard short-circuits, TSI-2204), so neither carries a deadline. A stale
// heartbeat does not count as live (TSI-2419), and noWorkerJobTimeout<=0
// disables the deadline entirely.
func TestDBJobToJobInfo_NoWorkerDeadline(t *testing.T) {
	created := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	base := &db.Job{
		ID:          "job-1",
		InputFiles:  "[]",
		Args:        "[]",
		OutputFiles: "[]",
		DirectPaths: "[]",
		CreatedAt:   created,
		UpdatedAt:   created,
	}

	wantDeadline := created.Add(2*time.Minute + 30*time.Second)

	newHandler := func(database *db.Database, heartbeatTimeout time.Duration) *Handler {
		return &Handler{
			db:                   database,
			heartbeatTimeout:     heartbeatTimeout,
			noWorkerJobTimeout:   2 * time.Minute,
			timeoutCheckInterval: 30 * time.Second,
		}
	}

	newDB := func(t *testing.T) *db.Database {
		t.Helper()
		database, err := db.New(":memory:")
		if err != nil {
			t.Fatalf("Failed to create database: %v", err)
		}
		t.Cleanup(func() { database.Close() })
		return database
	}

	seedWorker := func(t *testing.T, database *db.Database, id string) {
		t.Helper()
		if _, err := database.CreateWorker(id, id, protocol.WorkerCapabilities{
			Encoders: []string{"libx264"}, FFmpegVersion: "5.0", MaxConcurrent: 1,
		}); err != nil {
			t.Fatalf("Failed to create worker %s: %v", id, err)
		}
	}

	backdateHeartbeat := func(t *testing.T, database *db.Database, id string, age time.Duration) {
		t.Helper()
		if _, err := database.GetDB().Exec(
			`UPDATE workers SET last_heartbeat = ? WHERE id = ?`,
			time.Now().Add(-age), id,
		); err != nil {
			t.Fatalf("Failed to backdate worker %s heartbeat: %v", id, err)
		}
	}

	t.Run("pending unassigned with no live worker carries deadline", func(t *testing.T) {
		database := newDB(t)
		h := newHandler(database, 90*time.Second)

		job := *base
		job.Status = protocol.JobStatusPending
		job.WorkerID = sql.NullString{} // unassigned

		info := h.dbJobToJobInfo(&job)
		if info.NoWorkerDeadline == nil {
			t.Fatal("pending unassigned job with no live worker: NoWorkerDeadline = nil, want non-nil")
		}
		if !info.NoWorkerDeadline.Equal(wantDeadline) {
			t.Errorf("pending unassigned job: NoWorkerDeadline = %v, want %v", info.NoWorkerDeadline, wantDeadline)
		}
	})

	t.Run("pending unassigned behind live worker carries no deadline", func(t *testing.T) {
		database := newDB(t)
		h := newHandler(database, 90*time.Second)
		seedWorker(t, database, "worker-live")

		job := *base
		job.Status = protocol.JobStatusPending
		job.WorkerID = sql.NullString{} // unassigned

		info := h.dbJobToJobInfo(&job)
		if info.NoWorkerDeadline != nil {
			t.Errorf("pending job behind live worker: NoWorkerDeadline = %v, want nil", info.NoWorkerDeadline)
		}
	})

	t.Run("pending unassigned with stale worker carries deadline", func(t *testing.T) {
		database := newDB(t)
		h := newHandler(database, 90*time.Second)
		seedWorker(t, database, "worker-stale")
		backdateHeartbeat(t, database, "worker-stale", 2*time.Minute)

		job := *base
		job.Status = protocol.JobStatusPending
		job.WorkerID = sql.NullString{}

		info := h.dbJobToJobInfo(&job)
		if info.NoWorkerDeadline == nil {
			t.Fatal("pending job with stale worker: NoWorkerDeadline = nil, want non-nil")
		}
		if !info.NoWorkerDeadline.Equal(wantDeadline) {
			t.Errorf("pending job with stale worker: NoWorkerDeadline = %v, want %v", info.NoWorkerDeadline, wantDeadline)
		}
	})

	t.Run("disabled freshness counts stale worker as live", func(t *testing.T) {
		database := newDB(t)
		h := newHandler(database, 0) // freshness check disabled
		seedWorker(t, database, "worker-stale")
		backdateHeartbeat(t, database, "worker-stale", 2*time.Minute)

		job := *base
		job.Status = protocol.JobStatusPending
		job.WorkerID = sql.NullString{}

		info := h.dbJobToJobInfo(&job)
		if info.NoWorkerDeadline != nil {
			t.Errorf("pending job with freshness disabled: NoWorkerDeadline = %v, want nil", info.NoWorkerDeadline)
		}
	})

	t.Run("queued carries no deadline", func(t *testing.T) {
		database := newDB(t)
		h := newHandler(database, 90*time.Second)

		job := *base
		job.Status = protocol.JobStatusQueued
		job.WorkerID = sql.NullString{String: "worker-1", Valid: true}

		info := h.dbJobToJobInfo(&job)
		if info.NoWorkerDeadline != nil {
			t.Errorf("queued job: NoWorkerDeadline = %v, want nil", info.NoWorkerDeadline)
		}
	})

	t.Run("disabled starvation carries no deadline", func(t *testing.T) {
		database := newDB(t)
		disabled := &Handler{
			db:                   database,
			heartbeatTimeout:     90 * time.Second,
			noWorkerJobTimeout:   0,
			timeoutCheckInterval: 30 * time.Second,
		}

		job := *base
		job.Status = protocol.JobStatusPending
		job.WorkerID = sql.NullString{}

		info := disabled.dbJobToJobInfo(&job)
		if info.NoWorkerDeadline != nil {
			t.Errorf("noWorkerJobTimeout=0: NoWorkerDeadline = %v, want nil", info.NoWorkerDeadline)
		}
	})
}
