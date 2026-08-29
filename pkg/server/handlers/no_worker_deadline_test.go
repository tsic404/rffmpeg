package handlers

import (
	"database/sql"
	"testing"
	"time"

	"github.com/tsix404/rffmpeg/pkg/protocol"
	"github.com/tsix404/rffmpeg/pkg/server/db"
)

// TestDBJobToJobInfo_NoWorkerDeadline pins the NoWorkerDeadline attachment
// semantics to the scheduler's starvation sweep filter: only unassigned
// pending jobs (status=pending) can ever be failed with NO_WORKER_AVAILABLE,
// so only they carry the deadline. Queued jobs already own a worker_id and
// are never swept; and noWorkerJobTimeout<=0 disables the deadline entirely.
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

	h := &Handler{
		noWorkerJobTimeout:   2 * time.Minute,
		timeoutCheckInterval: 30 * time.Second,
	}

	wantDeadline := created.Add(2*time.Minute + 30*time.Second)

	t.Run("pending unassigned carries deadline", func(t *testing.T) {
		job := *base
		job.Status = protocol.JobStatusPending
		job.WorkerID = sql.NullString{} // unassigned

		info := h.dbJobToJobInfo(&job)
		if info.NoWorkerDeadline == nil {
			t.Fatal("pending unassigned job: NoWorkerDeadline = nil, want non-nil")
		}
		if !info.NoWorkerDeadline.Equal(wantDeadline) {
			t.Errorf("pending unassigned job: NoWorkerDeadline = %v, want %v", info.NoWorkerDeadline, wantDeadline)
		}
	})

	t.Run("queued carries no deadline", func(t *testing.T) {
		job := *base
		job.Status = protocol.JobStatusQueued
		job.WorkerID = sql.NullString{String: "worker-1", Valid: true}

		info := h.dbJobToJobInfo(&job)
		if info.NoWorkerDeadline != nil {
			t.Errorf("queued job: NoWorkerDeadline = %v, want nil", info.NoWorkerDeadline)
		}
	})

	t.Run("disabled starvation carries no deadline", func(t *testing.T) {
		job := *base
		job.Status = protocol.JobStatusPending
		job.WorkerID = sql.NullString{} // unassigned

		disabled := &Handler{
			noWorkerJobTimeout:   0,
			timeoutCheckInterval: 30 * time.Second,
		}
		info := disabled.dbJobToJobInfo(&job)
		if info.NoWorkerDeadline != nil {
			t.Errorf("noWorkerJobTimeout=0: NoWorkerDeadline = %v, want nil", info.NoWorkerDeadline)
		}
	})
}
