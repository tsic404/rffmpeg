package worker

import (
	"context"
	"testing"
	"time"

	"github.com/tsic404/rffmpeg/pkg/protocol"
)

// TestProcessJobReleasesJobContext pins the lifecycle boundary: the worker
// keeps the job's cancel func in activeJobs only for the duration of the job,
// so the deferred cleanup must cancel it. Without that, every completed job
// leaves a live context behind for the life of the worker process.
func TestProcessJobReleasesJobContext(t *testing.T) {
	w, _ := setupTestWorker(t, 24*time.Hour)

	jobCtx, cancel := context.WithCancel(context.Background())

	job := protocol.JobInfo{
		ID:         "job-context-release",
		InputFiles: []string{testUploadID("input-001")},
		Args:       []string{"-i", "<INPUT_FILE>", "-c:v", "libx264"},
	}

	w.processJob(jobCtx, job, cancel, false)

	if err := jobCtx.Err(); err != context.Canceled {
		t.Fatalf("job context err after completion = %v, want %v", err, context.Canceled)
	}
}
