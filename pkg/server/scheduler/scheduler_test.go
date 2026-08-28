package scheduler

import (
	"testing"
	"time"

	"github.com/tsix404/rffmpeg/pkg/protocol"
	"github.com/tsix404/rffmpeg/pkg/server/db"
)

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.JobTimeout != 30*time.Minute {
		t.Errorf("Expected JobTimeout 30m, got %v", cfg.JobTimeout)
	}
	if cfg.ScheduleInterval != 5*time.Second {
		t.Errorf("Expected ScheduleInterval 5s, got %v", cfg.ScheduleInterval)
	}
	if cfg.TimeoutCheckInterval != 30*time.Second {
		t.Errorf("Expected TimeoutCheckInterval 30s, got %v", cfg.TimeoutCheckInterval)
	}
	if cfg.MaxJobsPerWorker != 1 {
		t.Errorf("Expected MaxJobsPerWorker 1, got %d", cfg.MaxJobsPerWorker)
	}
}

func TestSchedulerIdleWorkerPrioritization(t *testing.T) {
	// Create in-memory database
	database, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("Failed to create database: %v", err)
	}
	defer database.Close()

	// Create idle worker
	worker, err := database.CreateWorker("worker-1", "test-worker", protocol.WorkerCapabilities{
		Encoders:      []string{"libx264"},
		FFmpegVersion: "5.0",
		MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatalf("Failed to create worker: %v", err)
	}

	// Create pending job
	job, err := database.CreateJob(`["file1.mp4"]`, `["-i", "input.mp4"]`, "output.mp4", false)
	if err != nil {
		t.Fatalf("Failed to create job: %v", err)
	}

	// Verify worker is idle
	idleWorkers, err := database.GetIdleWorkers()
	if err != nil {
		t.Fatalf("Failed to get idle workers: %v", err)
	}
	if len(idleWorkers) != 1 {
		t.Fatalf("Expected 1 idle worker, got %d", len(idleWorkers))
	}
	if idleWorkers[0].ID != worker.ID {
		t.Errorf("Expected worker ID %s, got %s", worker.ID, idleWorkers[0].ID)
	}

	// Verify job is pending
	pendingJobs, err := database.GetPendingJobs(10)
	if err != nil {
		t.Fatalf("Failed to get pending jobs: %v", err)
	}
	if len(pendingJobs) != 1 {
		t.Fatalf("Expected 1 pending job, got %d", len(pendingJobs))
	}
	if pendingJobs[0].ID != job.ID {
		t.Errorf("Expected job ID %s, got %s", job.ID, pendingJobs[0].ID)
	}
}

func TestSchedulerActiveJobCount(t *testing.T) {
	database, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("Failed to create database: %v", err)
	}
	defer database.Close()

	// Create worker
	worker, err := database.CreateWorker("worker-1", "test-worker", protocol.WorkerCapabilities{
		Encoders:      []string{"libx264"},
		FFmpegVersion: "5.0",
		MaxConcurrent: 2,
	})
	if err != nil {
		t.Fatalf("Failed to create worker: %v", err)
	}

	// Create and assign job
	job, err := database.CreateJob(`["file1.mp4"]`, `["-i", "input.mp4"]`, "output.mp4", false)
	if err != nil {
		t.Fatalf("Failed to create job: %v", err)
	}

	err = database.AssignJobToWorker(job.ID, worker.ID)
	if err != nil {
		t.Fatalf("Failed to assign job: %v", err)
	}

	err = database.UpdateJobStatusWithFailure(job.ID, protocol.JobStatusRunning, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("Failed to update job status: %v", err)
	}

	// Check active job count
	count, err := database.GetWorkerActiveJobCount(worker.ID)
	if err != nil {
		t.Fatalf("Failed to get active job count: %v", err)
	}
	if count != 1 {
		t.Errorf("Expected 1 active job, got %d", count)
	}
}

func TestSchedulerJobTimeout(t *testing.T) {
	database, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("Failed to create database: %v", err)
	}
	defer database.Close()

	// Create worker
	worker, err := database.CreateWorker("worker-1", "test-worker", protocol.WorkerCapabilities{
		Encoders:      []string{"libx264"},
		FFmpegVersion: "5.0",
		MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatalf("Failed to create worker: %v", err)
	}

	// Create and start job with old timestamp
	job, err := database.CreateJob(`["file1.mp4"]`, `["-i", "input.mp4"]`, "output.mp4", false)
	if err != nil {
		t.Fatalf("Failed to create job: %v", err)
	}

	err = database.AssignJobToWorker(job.ID, worker.ID)
	if err != nil {
		t.Fatalf("Failed to assign job: %v", err)
	}

	err = database.UpdateJobStatusWithFailure(job.ID, protocol.JobStatusRunning, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("Failed to update job status: %v", err)
	}

	// Manually set started_at to past (simulate timeout)
	_, err = database.GetDB().Exec(`
		UPDATE jobs SET started_at = ? WHERE id = ?
	`, time.Now().Add(-2*time.Hour), job.ID)
	if err != nil {
		t.Fatalf("Failed to set started_at: %v", err)
	}

	// Check for timed out jobs
	timedOutJobs, err := database.GetTimedOutJobs(1 * time.Hour)
	if err != nil {
		t.Fatalf("Failed to get timed out jobs: %v", err)
	}
	if len(timedOutJobs) != 1 {
		t.Fatalf("Expected 1 timed out job, got %d", len(timedOutJobs))
	}

	// Reschedule the job
	err = database.RescheduleJob(job.ID)
	if err != nil {
		t.Fatalf("Failed to reschedule job: %v", err)
	}

	// Verify job is back to pending
	updatedJob, err := database.GetJob(job.ID)
	if err != nil {
		t.Fatalf("Failed to get job: %v", err)
	}
	if updatedJob.Status != protocol.JobStatusPending {
		t.Errorf("Expected status pending, got %s", updatedJob.Status)
	}
	if updatedJob.WorkerID.Valid {
		t.Errorf("Expected worker_id to be NULL after reschedule")
	}
}

func TestSchedulerWorkerStatusUpdate(t *testing.T) {
	database, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("Failed to create database: %v", err)
	}
	defer database.Close()

	// Create worker
	worker, err := database.CreateWorker("worker-1", "test-worker", protocol.WorkerCapabilities{
		Encoders:      []string{"libx264"},
		FFmpegVersion: "5.0",
		MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatalf("Failed to create worker: %v", err)
	}

	// Worker should start as idle
	if worker.Status != protocol.WorkerStatusIdle {
		t.Errorf("Expected worker to be idle, got %s", worker.Status)
	}

	// Update to busy
	err = database.UpdateWorkerStatus(worker.ID, protocol.WorkerStatusBusy)
	if err != nil {
		t.Fatalf("Failed to update worker status: %v", err)
	}

	// Verify status
	updatedWorker, err := database.GetWorker(worker.ID)
	if err != nil {
		t.Fatalf("Failed to get worker: %v", err)
	}
	if updatedWorker.Status != protocol.WorkerStatusBusy {
		t.Errorf("Expected worker to be busy, got %s", updatedWorker.Status)
	}

	// Update back to idle
	err = database.UpdateWorkerStatus(worker.ID, protocol.WorkerStatusIdle)
	if err != nil {
		t.Fatalf("Failed to update worker status: %v", err)
	}

	// Verify status
	updatedWorker, err = database.GetWorker(worker.ID)
	if err != nil {
		t.Fatalf("Failed to get worker: %v", err)
	}
	if updatedWorker.Status != protocol.WorkerStatusIdle {
		t.Errorf("Expected worker to be idle, got %s", updatedWorker.Status)
	}
}

func TestSchedulerNoJobAssignmentWhenBusy(t *testing.T) {
	database, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("Failed to create database: %v", err)
	}
	defer database.Close()

	// Create worker with max concurrent 1
	worker, err := database.CreateWorker("worker-1", "test-worker", protocol.WorkerCapabilities{
		Encoders:      []string{"libx264"},
		FFmpegVersion: "5.0",
		MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatalf("Failed to create worker: %v", err)
	}

	// Create and assign job
	job, err := database.CreateJob(`["file1.mp4"]`, `["-i", "input.mp4"]`, "output.mp4", false)
	if err != nil {
		t.Fatalf("Failed to create job: %v", err)
	}

	err = database.AssignJobToWorker(job.ID, worker.ID)
	if err != nil {
		t.Fatalf("Failed to assign job: %v", err)
	}

	err = database.UpdateJobStatusWithFailure(job.ID, protocol.JobStatusRunning, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("Failed to update job status: %v", err)
	}

	// Create another job
	job2, err := database.CreateJob(`["file2.mp4"]`, `["-i", "input.mp4"]`, "output2.mp4", false)
	if err != nil {
		t.Fatalf("Failed to create second job: %v", err)
	}

	// Verify active job count is 1
	count, err := database.GetWorkerActiveJobCount(worker.ID)
	if err != nil {
		t.Fatalf("Failed to get active job count: %v", err)
	}
	if count != 1 {
		t.Errorf("Expected 1 active job, got %d", count)
	}

	// Second job should still be pending
	job2Check, err := database.GetJob(job2.ID)
	if err != nil {
		t.Fatalf("Failed to get job2: %v", err)
	}
	if job2Check.Status != protocol.JobStatusPending {
		t.Errorf("Expected job2 to be pending, got %s", job2Check.Status)
	}
	if job2Check.WorkerID.Valid {
		t.Errorf("Expected job2 to have no worker assigned")
	}
}

func TestExtractEncoderFromArgs(t *testing.T) {
	tests := []struct {
		name     string
		args     string
		expected string
	}{
		{
			name:     "c:v encoder",
			args:     `["-i", "input.mp4", "-c:v", "libx264", "output.mp4"]`,
			expected: "libx264",
		},
		{
			name:     "vcodec encoder",
			args:     `["-i", "input.mp4", "-vcodec", "h264_nvenc", "output.mp4"]`,
			expected: "h264_nvenc",
		},
		{
			name:     "codec:v encoder",
			args:     `["-i", "input.mp4", "-codec:v", "hevc_qsv", "output.mp4"]`,
			expected: "hevc_qsv",
		},
		{
			name:     "c:v:0 stream encoder",
			args:     `["-i", "input.mp4", "-c:v:0", "h264_vaapi", "output.mp4"]`,
			expected: "h264_vaapi",
		},
		{
			name:     "no encoder specified",
			args:     `["-i", "input.mp4", "-y", "output.mp4"]`,
			expected: "",
		},
		{
			name:     "audio codec ignored",
			args:     `["-i", "input.mp4", "-c:a", "aac", "output.mp4"]`,
			expected: "",
		},
		{
			name:     "c:v copy passthrough",
			args:     `["-i", "video.mp4", "-i", "audio.mp3", "-c:v", "copy", "-c:a", "aac", "-shortest", "merged.mp4"]`,
			expected: "",
		},
		{
			name:     "vcodec copy passthrough",
			args:     `["-i", "video.mp4", "-vcodec", "copy", "output.mp4"]`,
			expected: "",
		},
		{
			name:     "codec:v copy passthrough",
			args:     `["-i", "video.mp4", "-codec:v", "copy", "output.mp4"]`,
			expected: "",
		},
		{
			name:     "c:v:0 copy passthrough",
			args:     `["-i", "video.mp4", "-c:v:0", "copy", "output.mp4"]`,
			expected: "",
		},

		{
			name:     "invalid JSON",
			args:     `not valid json`,
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ExtractEncoderFromArgs(tt.args)
			if result != tt.expected {
				t.Errorf("ExtractEncoderFromArgs(%q) = %q, want %q", tt.args, result, tt.expected)
			}
		})
	}
}

func TestIsVideoEncoder(t *testing.T) {
	tests := []struct {
		name     string
		encoder  string
		expected bool
	}{
		{"libx264 software", "libx264", true},
		{"h264_nvenc hardware", "h264_nvenc", true},
		{"hevc_qsv hardware", "hevc_qsv", true},
		{"unknown encoder", "unknown_encoder", false},
		{"audio encoder aac", "aac", false},
		{"VP9 software", "libvpx-vp9", true},
		{"AV1 software", "libaom-av1", true},
		{"AMF hardware", "h264_amf", true},
		{"VAAPI hardware", "h264_vaapi", true},
		{"VideoToolbox", "h264_videotoolbox", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isVideoEncoder(tt.encoder)
			if result != tt.expected {
				t.Errorf("isVideoEncoder(%q) = %v, want %v", tt.encoder, result, tt.expected)
			}
		})
	}
}

func TestSchedulerCapabilityAwareScheduling(t *testing.T) {
	database, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("Failed to create database: %v", err)
	}
	defer database.Close()

	// Create worker with h264_nvenc capability
	worker1, err := database.CreateWorker("worker-1", "gpu-worker", protocol.WorkerCapabilities{
		Encoders:      []string{"libx264", "h264_nvenc"},
		FFmpegVersion: "5.0",
		MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatalf("Failed to create worker1: %v", err)
	}

	// Create worker without h264_nvenc capability
	_, err = database.CreateWorker("worker-2", "cpu-worker", protocol.WorkerCapabilities{
		Encoders:      []string{"libx264"},
		FFmpegVersion: "5.0",
		MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatalf("Failed to create worker2: %v", err)
	}

	// Create job requesting h264_nvenc encoder
	job, err := database.CreateJob(`["file1.mp4"]`, `["-i", "input.mp4", "-c:v", "h264_nvenc", "output.mp4"]`, "output.mp4", false)
	if err != nil {
		t.Fatalf("Failed to create job: %v", err)
	}

	// Verify that GetIdleWorkersByEncoder returns only worker1
	workers, err := database.GetIdleWorkersByEncoder("h264_nvenc")
	if err != nil {
		t.Fatalf("Failed to get workers by encoder: %v", err)
	}
	if len(workers) != 1 {
		t.Fatalf("Expected 1 worker with h264_nvenc, got %d", len(workers))
	}
	if workers[0].ID != worker1.ID {
		t.Errorf("Expected worker1 ID, got %s", workers[0].ID)
	}

	// Verify that GetIdleWorkers returns both workers
	allWorkers, err := database.GetIdleWorkers()
	if err != nil {
		t.Fatalf("Failed to get idle workers: %v", err)
	}
	if len(allWorkers) != 2 {
		t.Errorf("Expected 2 idle workers, got %d", len(allWorkers))
	}

	// Verify encoder extraction
	encoder := ExtractEncoderFromArgs(job.Args)
	if encoder != "h264_nvenc" {
		t.Errorf("Expected encoder 'h264_nvenc', got '%s'", encoder)
	}
}

func TestGetWorkersByEncoder(t *testing.T) {
	database, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("Failed to create database: %v", err)
	}
	defer database.Close()

	// Create multiple workers with different capabilities
	_, err = database.CreateWorker("worker-1", "gpu1", protocol.WorkerCapabilities{
		Encoders:      []string{"libx264", "h264_nvenc", "hevc_nvenc"},
		FFmpegVersion: "5.0",
	})
	if err != nil {
		t.Fatalf("Failed to create worker1: %v", err)
	}

	_, err = database.CreateWorker("worker-2", "gpu2", protocol.WorkerCapabilities{
		Encoders:      []string{"libx264", "hevc_nvenc"},
		FFmpegVersion: "5.0",
	})
	if err != nil {
		t.Fatalf("Failed to create worker2: %v", err)
	}

	_, err = database.CreateWorker("worker-3", "cpu", protocol.WorkerCapabilities{
		Encoders:      []string{"libx264", "libx265"},
		FFmpegVersion: "5.0",
	})
	if err != nil {
		t.Fatalf("Failed to create worker3: %v", err)
	}

	// Test GetWorkersByEncoder
	tests := []struct {
		encoder       string
		expectedCount int
	}{
		{"h264_nvenc", 1},
		{"hevc_nvenc", 2},
		{"libx264", 3},
		{"libx265", 1},
		{"nonexistent", 0},
	}

	for _, tt := range tests {
		t.Run(tt.encoder, func(t *testing.T) {
			workers, err := database.GetWorkersByEncoder(tt.encoder)
			if err != nil {
				t.Fatalf("Failed to get workers by encoder: %v", err)
			}
			if len(workers) != tt.expectedCount {
				t.Errorf("Expected %d workers with %s, got %d", tt.expectedCount, tt.encoder, len(workers))
			}
		})
	}
}

func TestGetAllEncoders(t *testing.T) {
	database, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("Failed to create database: %v", err)
	}
	defer database.Close()

	// Create workers with different encoders
	_, err = database.CreateWorker("worker-1", "", protocol.WorkerCapabilities{
		Encoders:      []string{"libx264", "h264_nvenc"},
		FFmpegVersion: "5.0",
	})
	if err != nil {
		t.Fatalf("Failed to create worker1: %v", err)
	}

	_, err = database.CreateWorker("worker-2", "", protocol.WorkerCapabilities{
		Encoders:      []string{"libx264", "hevc_qsv"},
		FFmpegVersion: "5.0",
	})
	if err != nil {
		t.Fatalf("Failed to create worker2: %v", err)
	}

	encoders, err := database.GetAllEncoders()
	if err != nil {
		t.Fatalf("Failed to get all encoders: %v", err)
	}

	// Should have 3 unique encoders: libx264, h264_nvenc, hevc_qsv
	if len(encoders) != 3 {
		t.Errorf("Expected 3 unique encoders, got %d: %v", len(encoders), encoders)
	}

	// Verify all expected encoders are present
	encoderSet := make(map[string]bool)
	for _, enc := range encoders {
		encoderSet[enc] = true
	}
	for _, expected := range []string{"libx264", "h264_nvenc", "hevc_qsv"} {
		if !encoderSet[expected] {
			t.Errorf("Expected encoder %s not found", expected)
		}
	}
}

func TestUpdateWorkerCapabilities(t *testing.T) {
	database, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("Failed to create database: %v", err)
	}
	defer database.Close()

	// Create worker with initial capabilities
	worker, err := database.CreateWorker("worker-1", "", protocol.WorkerCapabilities{
		Encoders:      []string{"libx264"},
		FFmpegVersion: "5.0",
		MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatalf("Failed to create worker: %v", err)
	}

	// Update capabilities
	err = database.UpdateWorkerCapabilities(worker.ID, protocol.WorkerCapabilities{
		Encoders:      []string{"libx264", "h264_nvenc", "hevc_nvenc"},
		Decoders:      []string{"h264_cuvid", "hevc_cuvid"},
		GPUModel:      "NVIDIA RTX 3080",
		FFmpegVersion: "6.0",
		MaxConcurrent: 4,
	})
	if err != nil {
		t.Fatalf("Failed to update worker capabilities: %v", err)
	}

	// Verify updated capabilities
	updatedWorker, err := database.GetWorker(worker.ID)
	if err != nil {
		t.Fatalf("Failed to get updated worker: %v", err)
	}

	if updatedWorker.FFmpegVersion != "6.0" {
		t.Errorf("Expected FFmpeg version 6.0, got %s", updatedWorker.FFmpegVersion)
	}
	if updatedWorker.MaxConcurrent != 4 {
		t.Errorf("Expected MaxConcurrent 4, got %d", updatedWorker.MaxConcurrent)
	}
	if !updatedWorker.GPUModel.Valid || updatedWorker.GPUModel.String != "NVIDIA RTX 3080" {
		t.Errorf("Expected GPU model 'NVIDIA RTX 3080', got %v", updatedWorker.GPUModel)
	}

	// Verify encoders
	encoders, err := database.GetWorkerEncoders(worker.ID)
	if err != nil {
		t.Fatalf("Failed to get worker encoders: %v", err)
	}
	if len(encoders) != 3 {
		t.Errorf("Expected 3 encoders, got %d", len(encoders))
	}
}

// TSI-1500: Tests for encoder fallback functionality

func TestSchedulerEncoderFallback(t *testing.T) {
	database, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("Failed to create database: %v", err)
	}
	defer database.Close()

	// Create worker with only software encoder (libx264)
	worker1, err := database.CreateWorker("worker-1", "cpu-worker", protocol.WorkerCapabilities{
		Encoders:      []string{"libx264"},
		FFmpegVersion: "5.0",
		MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatalf("Failed to create worker1: %v", err)
	}

	// Create a job requesting h264_nvenc (which worker1 doesn't have)
	job, err := database.CreateJob(
		`["file1.mp4"]`,
		`["-i", "input.mp4", "-c:v", "h264_nvenc", "output.mp4"]`,
		"output.mp4",
		false,
	)
	if err != nil {
		t.Fatalf("Failed to create job: %v", err)
	}

	// Create scheduler
	scheduler := New(database, DefaultConfig())

	// Schedule the job
	scheduled := scheduler.scheduleJob(job)
	if !scheduled {
		t.Fatalf("Expected job to be scheduled with encoder fallback")
	}

	// Verify the job was assigned to worker1 (which has libx264, a compatible H.264 encoder)
	updatedJob, err := database.GetJob(job.ID)
	if err != nil {
		t.Fatalf("Failed to get job: %v", err)
	}

	if !updatedJob.WorkerID.Valid {
		t.Fatalf("Expected job to be assigned to a worker")
	}

	if updatedJob.WorkerID.String != worker1.ID {
		t.Errorf("Expected job to be assigned to worker1 (%s), got %s", worker1.ID, updatedJob.WorkerID.String)
	}

	// Verify the job status is queued
	if updatedJob.Status != protocol.JobStatusQueued {
		t.Errorf("Expected job status to be queued, got %s", updatedJob.Status)
	}
}

// The job requests hevc_nvenc (HEVC family); the only worker has libx264
// (H.264 family). A job explicitly requesting an encoder no worker provides
// stays pending instead of being force-assigned to an incapable worker —
// that would guarantee failure at run time (TSI-2362).
func TestSchedulerEncoderFallbackDifferentCodecFamily(t *testing.T) {
	database, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("Failed to create database: %v", err)
	}
	defer database.Close()

	// Create worker with only H.264 encoder
	if _, err := database.CreateWorker("worker-1", "cpu-worker", protocol.WorkerCapabilities{
		Encoders:      []string{"libx264"},
		FFmpegVersion: "5.0",
		MaxConcurrent: 1,
	}); err != nil {
		t.Fatalf("Failed to create worker1: %v", err)
	}

	// Create a job requesting hevc_nvenc (different codec family - HEVC, not H.264).
	job, err := database.CreateJob(
		`["file1.mp4"]`,
		`["-i", "input.mp4", "-c:v", "hevc_nvenc", "output.mp4"]`,
		"output.mp4",
		false,
	)
	if err != nil {
		t.Fatalf("Failed to create job: %v", err)
	}

	scheduler := New(database, DefaultConfig())
	scheduled := scheduler.scheduleJob(job)
	if scheduled {
		t.Fatalf("Expected job requesting unavailable encoder to stay pending")
	}

	updatedJob, err := database.GetJob(job.ID)
	if err != nil {
		t.Fatalf("Failed to get job: %v", err)
	}
	if updatedJob.Status != protocol.JobStatusPending {
		t.Errorf("Expected job status to remain pending, got %s", updatedJob.Status)
	}
	if updatedJob.WorkerID.Valid {
		t.Errorf("Expected no worker assignment, got %s", updatedJob.WorkerID.String)
	}
}

func TestSchedulerEncoderFallbackHEVC(t *testing.T) {
	database, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("Failed to create database: %v", err)
	}
	defer database.Close()

	// Create worker with libx265 (HEVC software encoder)
	worker1, err := database.CreateWorker("worker-1", "cpu-worker", protocol.WorkerCapabilities{
		Encoders:      []string{"libx265"},
		FFmpegVersion: "5.0",
		MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatalf("Failed to create worker1: %v", err)
	}

	// Create a job requesting hevc_qsv (HEVC hardware encoder)
	job, err := database.CreateJob(
		`["file1.mp4"]`,
		`["-i", "input.mp4", "-c:v", "hevc_qsv", "output.mp4"]`,
		"output.mp4",
		false,
	)
	if err != nil {
		t.Fatalf("Failed to create job: %v", err)
	}

	// Create scheduler
	scheduler := New(database, DefaultConfig())

	// Schedule the job - should fallback to libx265 (same codec family)
	scheduled := scheduler.scheduleJob(job)
	if !scheduled {
		t.Fatalf("Expected job to be scheduled with encoder fallback to libx265")
	}

	// Verify the job was assigned to worker1
	updatedJob, err := database.GetJob(job.ID)
	if err != nil {
		t.Fatalf("Failed to get job: %v", err)
	}

	if !updatedJob.WorkerID.Valid {
		t.Fatalf("Expected job to be assigned to a worker")
	}

	if updatedJob.WorkerID.String != worker1.ID {
		t.Errorf("Expected job to be assigned to worker1 (%s), got %s", worker1.ID, updatedJob.WorkerID.String)
	}
}

func TestSchedulerEncoderFallbackPrioritizesHardware(t *testing.T) {
	database, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("Failed to create database: %v", err)
	}
	defer database.Close()

	// Create worker with hardware encoder (h264_nvenc)
	workerNVENC, err := database.CreateWorker("worker-nvenc", "gpu-worker", protocol.WorkerCapabilities{
		Encoders:      []string{"h264_nvenc", "libx264"},
		FFmpegVersion: "5.0",
		MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatalf("Failed to create workerNVENC: %v", err)
	}

	// Create worker with only software encoder (libx264)
	workerCPU, err := database.CreateWorker("worker-cpu", "cpu-worker", protocol.WorkerCapabilities{
		Encoders:      []string{"libx264"},
		FFmpegVersion: "5.0",
		MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatalf("Failed to create workerCPU: %v", err)
	}

	// Create a job requesting h264_qsv (which neither worker has directly)
	// But workerNVENC has h264_nvenc which is in the same codec family and is hardware
	job, err := database.CreateJob(
		`["file1.mp4"]`,
		`["-i", "input.mp4", "-c:v", "h264_qsv", "output.mp4"]`,
		"output.mp4",
		false,
	)
	if err != nil {
		t.Fatalf("Failed to create job: %v", err)
	}

	// Create scheduler
	scheduler := New(database, DefaultConfig())

	// Schedule the job
	scheduled := scheduler.scheduleJob(job)
	if !scheduled {
		t.Fatalf("Expected job to be scheduled with encoder fallback")
	}

	// Verify the job was assigned to workerNVENC (has hardware encoder, prioritized over software)
	updatedJob, err := database.GetJob(job.ID)
	if err != nil {
		t.Fatalf("Failed to get job: %v", err)
	}

	if !updatedJob.WorkerID.Valid {
		t.Fatalf("Expected job to be assigned to a worker")
	}

	if updatedJob.WorkerID.String != workerNVENC.ID {
		t.Errorf("Expected job to be assigned to workerNVENC (%s) for hardware encoder priority, got %s", workerNVENC.ID, updatedJob.WorkerID.String)
	}

	_ = workerCPU // Use workerCPU to avoid unused variable warning
}

func TestSchedulerEncoderFallbackNoWorkers(t *testing.T) {
	database, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("Failed to create database: %v", err)
	}
	defer database.Close()

	// No workers available

	// Create a job requesting h264_nvenc
	job, err := database.CreateJob(
		`["file1.mp4"]`,
		`["-i", "input.mp4", "-c:v", "h264_nvenc", "output.mp4"]`,
		"output.mp4",
		false,
	)
	if err != nil {
		t.Fatalf("Failed to create job: %v", err)
	}

	// Create scheduler
	scheduler := New(database, DefaultConfig())

	// Schedule the job - should NOT be scheduled (no workers)
	scheduled := scheduler.scheduleJob(job)
	if scheduled {
		t.Errorf("Expected job NOT to be scheduled (no workers)")
	}

	// Verify the job is still pending
	updatedJob, err := database.GetJob(job.ID)
	if err != nil {
		t.Fatalf("Failed to get job: %v", err)
	}

	if updatedJob.Status != protocol.JobStatusPending {
		t.Errorf("Expected job status to be pending, got %s", updatedJob.Status)
	}
}

func TestSchedulerEncoderFallbackExactMatchPreferred(t *testing.T) {
	database, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("Failed to create database: %v", err)
	}
	defer database.Close()

	// Create worker with h264_qsv (the exact encoder requested)
	workerQSV, err := database.CreateWorker("worker-qsv", "intel-worker", protocol.WorkerCapabilities{
		Encoders:      []string{"h264_qsv", "libx264"},
		FFmpegVersion: "5.0",
		MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatalf("Failed to create workerQSV: %v", err)
	}

	// Create worker with h264_nvenc (same codec family, different hardware)
	workerNVENC, err := database.CreateWorker("worker-nvenc", "nvidia-worker", protocol.WorkerCapabilities{
		Encoders:      []string{"h264_nvenc", "libx264"},
		FFmpegVersion: "5.0",
		MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatalf("Failed to create workerNVENC: %v", err)
	}

	// Create a job requesting h264_qsv
	job, err := database.CreateJob(
		`["file1.mp4"]`,
		`["-i", "input.mp4", "-c:v", "h264_qsv", "output.mp4"]`,
		"output.mp4",
		false,
	)
	if err != nil {
		t.Fatalf("Failed to create job: %v", err)
	}

	// Create scheduler
	scheduler := New(database, DefaultConfig())

	// Schedule the job
	scheduled := scheduler.scheduleJob(job)
	if !scheduled {
		t.Fatalf("Expected job to be scheduled")
	}

	// Verify the job was assigned to workerQSV (exact match, not fallback)
	updatedJob, err := database.GetJob(job.ID)
	if err != nil {
		t.Fatalf("Failed to get job: %v", err)
	}

	if !updatedJob.WorkerID.Valid {
		t.Fatalf("Expected job to be assigned to a worker")
	}

	if updatedJob.WorkerID.String != workerQSV.ID {
		t.Errorf("Expected job to be assigned to workerQSV (%s) for exact match, got %s", workerQSV.ID, updatedJob.WorkerID.String)
	}

	_ = workerNVENC // Use to avoid unused variable warning
}

// TSI-2334: a pending job whose only worker went offline must be failed with
// NO_WORKER_AVAILABLE after the grace period instead of waiting forever.
func TestSchedulerNoWorkerStarvation(t *testing.T) {
	database, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("Failed to create database: %v", err)
	}
	defer database.Close()

	s := New(database, Config{
		ScheduleInterval:     time.Hour,
		TimeoutCheckInterval: time.Hour,
		MaxJobsPerWorker:     1,
		NoWorkerJobTimeout:   1 * time.Minute,
	})

	// Create an idle worker, then take it offline (simulates death after submit)
	_, err = database.CreateWorker("worker-1", "test-worker", protocol.WorkerCapabilities{
		Encoders:      []string{"libx264"},
		FFmpegVersion: "5.0",
		MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatalf("Failed to create worker: %v", err)
	}
	if err := database.UpdateWorkerStatus("worker-1", protocol.WorkerStatusOffline); err != nil {
		t.Fatalf("Failed to mark worker offline: %v", err)
	}

	job, err := database.CreateJob(`["file1.mp4"]`, `["-i", "input.mp4"]`, "output.mp4", false)
	if err != nil {
		t.Fatalf("Failed to create job: %v", err)
	}

	// Backdate created_at beyond the grace period
	if _, err := database.GetDB().Exec(`
		UPDATE jobs SET created_at = ? WHERE id = ?
	`, time.Now().Add(-5*time.Minute), job.ID); err != nil {
		t.Fatalf("Failed to backdate created_at: %v", err)
	}

	s.checkNoWorkerStarvation()

	updatedJob, err := database.GetJob(job.ID)
	if err != nil {
		t.Fatalf("Failed to get job: %v", err)
	}
	if updatedJob.Status != protocol.JobStatusFailed {
		t.Errorf("Expected status failed, got %s", updatedJob.Status)
	}
	if updatedJob.FailureType != string(protocol.FailureNoWorkerAvailable) {
		t.Errorf("Expected failure_type %q, got %q",
			protocol.FailureNoWorkerAvailable, updatedJob.FailureType)
	}
	if updatedJob.Error.String == "" {
		t.Error("Expected non-empty error message")
	}
}

// TSI-2204 contract: pending jobs queued behind BUSY workers must NOT be
// failed — schedulable workers exist, so the starvation check is a no-op.
func TestSchedulerNoWorkerStarvation_SkipsWhenWorkerBusy(t *testing.T) {
	database, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("Failed to create database: %v", err)
	}
	defer database.Close()

	s := New(database, Config{
		ScheduleInterval:     time.Hour,
		TimeoutCheckInterval: time.Hour,
		MaxJobsPerWorker:     1,
		NoWorkerJobTimeout:   1 * time.Minute,
	})

	_, err = database.CreateWorker("worker-1", "test-worker", protocol.WorkerCapabilities{
		Encoders:      []string{"libx264"},
		FFmpegVersion: "5.0",
		MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatalf("Failed to create worker: %v", err)
	}
	if err := database.UpdateWorkerStatus("worker-1", protocol.WorkerStatusBusy); err != nil {
		t.Fatalf("Failed to mark worker busy: %v", err)
	}

	job, err := database.CreateJob(`["file1.mp4"]`, `["-i", "input.mp4"]`, "output.mp4", false)
	if err != nil {
		t.Fatalf("Failed to create job: %v", err)
	}
	if _, err := database.GetDB().Exec(`
		UPDATE jobs SET created_at = ? WHERE id = ?
	`, time.Now().Add(-5*time.Minute), job.ID); err != nil {
		t.Fatalf("Failed to backdate created_at: %v", err)
	}

	s.checkNoWorkerStarvation()

	updatedJob, err := database.GetJob(job.ID)
	if err != nil {
		t.Fatalf("Failed to get job: %v", err)
	}
	if updatedJob.Status != protocol.JobStatusPending {
		t.Errorf("Busy worker means job stays pending; got %s", updatedJob.Status)
	}
}

// Fresh jobs inside the grace period are left pending even with no workers.
func TestSchedulerNoWorkerStarvation_RespectsGracePeriod(t *testing.T) {
	database, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("Failed to create database: %v", err)
	}
	defer database.Close()

	s := New(database, Config{
		ScheduleInterval:     time.Hour,
		TimeoutCheckInterval: time.Hour,
		MaxJobsPerWorker:     1,
		NoWorkerJobTimeout:   1 * time.Minute,
	})

	job, err := database.CreateJob(`["file1.mp4"]`, `["-i", "input.mp4"]`, "output.mp4", false)
	if err != nil {
		t.Fatalf("Failed to create job: %v", err)
	}

	// No workers registered at all; but job was created just now
	s.checkNoWorkerStarvation()

	updatedJob, err := database.GetJob(job.ID)
	if err != nil {
		t.Fatalf("Failed to get job: %v", err)
	}
	if updatedJob.Status != protocol.JobStatusPending {
		t.Errorf("Job within grace period should stay pending; got %s", updatedJob.Status)
	}
}

// NoWorkerJobTimeout=0 disables the check entirely.
func TestSchedulerNoWorkerStarvation_DisabledByZero(t *testing.T) {
	database, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("Failed to create database: %v", err)
	}
	defer database.Close()

	s := New(database, Config{
		ScheduleInterval:     time.Hour,
		TimeoutCheckInterval: time.Hour,
		MaxJobsPerWorker:     1,
		NoWorkerJobTimeout:   0,
	})

	job, err := database.CreateJob(`["file1.mp4"]`, `["-i", "input.mp4"]`, "output.mp4", false)
	if err != nil {
		t.Fatalf("Failed to create job: %v", err)
	}
	if _, err := database.GetDB().Exec(`
		UPDATE jobs SET created_at = ? WHERE id = ?
	`, time.Now().Add(-5*time.Minute), job.ID); err != nil {
		t.Fatalf("Failed to backdate created_at: %v", err)
	}

	s.checkNoWorkerStarvation()

	updatedJob, err := database.GetJob(job.ID)
	if err != nil {
		t.Fatalf("Failed to get job: %v", err)
	}
	if updatedJob.Status != protocol.JobStatusPending {
		t.Errorf("Check disabled (timeout=0): job should stay pending; got %s", updatedJob.Status)
	}
}

// TSI-2334 review: a backlog of starved jobs larger than any fetch limit must
// converge within a single tick (bulk SQL failure, not per-job iteration).
func TestSchedulerNoWorkerStarvation_BulkBacklog(t *testing.T) {
	database, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("Failed to create database: %v", err)
	}
	defer database.Close()

	s := New(database, Config{
		ScheduleInterval:     time.Hour,
		TimeoutCheckInterval: time.Hour,
		MaxJobsPerWorker:     1,
		NoWorkerJobTimeout:   1 * time.Minute,
	})

	const backlog = 250
	for i := 0; i < backlog; i++ {
		if _, err := database.CreateJob(`["file1.mp4"]`, `["-i", "input.mp4"]`, "output.mp4", false); err != nil {
			t.Fatalf("Failed to create job %d: %v", i, err)
		}
	}
	old := time.Now().Add(-5 * time.Minute)
	if _, err := database.GetDB().Exec(`UPDATE jobs SET created_at = ?`, old); err != nil {
		t.Fatalf("Failed to backdate created_at: %v", err)
	}

	s.checkNoWorkerStarvation()

	pendingJobs, err := database.GetPendingJobs(1000)
	if err != nil {
		t.Fatalf("Failed to get pending jobs: %v", err)
	}
	if len(pendingJobs) != 0 {
		t.Errorf("Expected all %d starved jobs failed in one tick, %d still pending", backlog, len(pendingJobs))
	}
	failed, err := database.GetJobsByStatus(protocol.JobStatusFailed, 1000)
	if err != nil {
		t.Fatalf("Failed to get failed jobs: %v", err)
	}
	if len(failed) != backlog {
		t.Errorf("Expected %d failed jobs, got %d", backlog, len(failed))
	}
	for _, job := range failed {
		if job.FailureType != string(protocol.FailureNoWorkerAvailable) {
			t.Errorf("job %s: expected failure_type NO_WORKER_AVAILABLE, got %q",
				job.ID, job.FailureType)
			break
		}
	}
}

// TestSchedulePendingJobsHeadOfLineBlocking verifies that a job no worker can
// run (missing encoder capability, no capacity) does not block schedulable
// jobs behind it in the queue (TSI-2362 acceptance 6).
func TestSchedulePendingJobsHeadOfLineBlocking(t *testing.T) {
	database, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("Failed to create database: %v", err)
	}
	defer database.Close()

	// Worker has only libx264.
	if _, err := database.CreateWorker("worker-1", "worker-1", protocol.WorkerCapabilities{
		Encoders: []string{"libx264"}, FFmpegVersion: "5.0", MaxConcurrent: 1,
	}); err != nil {
		t.Fatalf("Failed to create worker: %v", err)
	}

	// Head job requires an encoder nobody has; second job is schedulable.
	headJob, err := database.CreateJob(`["f.mkv"]`, `["-c:v","madeupenc","o.mkv"]`, "o.mkv", false)
	if err != nil {
		t.Fatal(err)
	}
	okJob, err := database.CreateJob(`["f2.mkv"]`, `["-c:v","libx264","o2.mkv"]`, "o2.mkv", false)
	if err != nil {
		t.Fatal(err)
	}

	s := New(database, DefaultConfig())
	s.schedulePendingJobs()

	gotOK, err := database.GetJob(okJob.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotOK.Status != protocol.JobStatusQueued {
		t.Errorf("schedulable job blocked by head-of-line job: status=%s", gotOK.Status)
	}

	gotHead, _ := database.GetJob(headJob.ID)
	if gotHead.Status != protocol.JobStatusPending {
		t.Errorf("unschedulable head job should stay pending, got %s", gotHead.Status)
	}
}

// TestCheckTimeoutsRetryBudget verifies that a job failing repeatedly with
// timeout is failed as TIMEOUT once the retry budget is exhausted instead of
// being requeued forever (TSI-2362 acceptance 4).
func TestCheckTimeoutsRetryBudget(t *testing.T) {
	database, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("Failed to create database: %v", err)
	}
	defer database.Close()

	if _, err := database.CreateWorker("worker-1", "worker-1", protocol.WorkerCapabilities{
		Encoders: []string{"libx264"}, FFmpegVersion: "5.0", MaxConcurrent: 1,
	}); err != nil {
		t.Fatal(err)
	}

	job, err := database.CreateJob(`["f.mkv"]`, `[]`, "o.mkv", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.AssignJobToWorker(job.ID, "worker-1"); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateJobStatusWithFailure(job.ID, protocol.JobStatusRunning, nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	// Backdate started_at so GetTimedOutJobs picks it up.
	if _, err := database.GetDB().Exec(`UPDATE jobs SET started_at = ? WHERE id = ?`,
		time.Now().Add(-2*time.Hour), job.ID); err != nil {
		t.Fatal(err)
	}

	const maxRetries = 2
	cfg := DefaultConfig()
	cfg.MaxTimeoutRetries = maxRetries
	s := New(database, cfg)

	// First pass: retries available → back to pending.
	s.checkTimeouts()
	got, _ := database.GetJob(job.ID)
	if got.Status != protocol.JobStatusPending {
		t.Fatalf("expected pending after first timeout (retry %d/%d), got %s", 1, maxRetries, got.Status)
	}

	// Exhaust the remaining budget: requeue as running and time out again.
	for i := 1; i < maxRetries; i++ {
		if err := database.AssignJobToWorker(job.ID, "worker-1"); err != nil {
			t.Fatal(err)
		}
		if err := database.UpdateJobStatusWithFailure(job.ID, protocol.JobStatusRunning, nil, nil, nil, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := database.GetDB().Exec(`UPDATE jobs SET started_at = ? WHERE id = ?`,
			time.Now().Add(-2*time.Hour), job.ID); err != nil {
			t.Fatal(err)
		}
		s.checkTimeouts()
	}

	// Budget exhausted: one more running+timed-out cycle must FAIL the job.
	if err := database.AssignJobToWorker(job.ID, "worker-1"); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateJobStatusWithFailure(job.ID, protocol.JobStatusRunning, nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := database.GetDB().Exec(`UPDATE jobs SET started_at = ? WHERE id = ?`,
		time.Now().Add(-2*time.Hour), job.ID); err != nil {
		t.Fatal(err)
	}
	s.checkTimeouts()

	got, _ = database.GetJob(job.ID)
	if got.Status != protocol.JobStatusFailed {
		t.Fatalf("expected failed(TIMEOUT) after exhausting retry budget, got %s", got.Status)
	}
	if got.FailureType != string(protocol.FailureTimeout) {
		t.Fatalf("expected failure_type TIMEOUT, got %q", got.FailureType)
	}
}

// TestSchedulerLoadBalancingTieBreak verifies that when multiple idle workers
// share the same (lowest) active job count, the scheduler breaks the tie by
// least completed jobs. Without this, the first worker in the query result
// always won ties, starving late-registered workers (TSI-2477).
func TestSchedulerLoadBalancingTieBreak(t *testing.T) {
	database, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("Failed to create database: %v", err)
	}
	defer database.Close()

	// Two idle workers with the same encoder and capacity.
	worker1, err := database.CreateWorker("worker-1", "w1", protocol.WorkerCapabilities{
		Encoders: []string{"libx264"}, FFmpegVersion: "5.0", MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatalf("Failed to create worker1: %v", err)
	}
	worker2, err := database.CreateWorker("worker-2", "w2", protocol.WorkerCapabilities{
		Encoders: []string{"libx264"}, FFmpegVersion: "5.0", MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatalf("Failed to create worker2: %v", err)
	}

	// Give worker-1 one completed job so worker-2 has the lower completed
	// count and should win the tie.
	if _, err := database.GetDB().Exec(`
		INSERT INTO jobs (id, status, input_files, args, output_filename, streaming_output, output_files, worker_id, exit_code, error, failure_type, retryable, auto_hw, progress_percent, eta_seconds, created_at, updated_at, finished_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, "completed-1", protocol.JobStatusCompleted, `["f.mkv"]`, `[]`, "o.mkv", 0, "[]",
		worker1.ID, 0, "", "", 0, 0, 0, 0,
		time.Now().Add(-time.Hour), time.Now().Add(-time.Hour), time.Now().Add(-time.Hour)); err != nil {
		t.Fatalf("Failed to seed completed job: %v", err)
	}

	// Both workers are idle with 0 active jobs; worker-2 has fewer completed
	// jobs, so a single pending job must be assigned to worker-2.
	job, err := database.CreateJob(`["f.mkv"]`, `[]`, "o.mkv", false)
	if err != nil {
		t.Fatalf("Failed to create job: %v", err)
	}

	s := New(database, DefaultConfig())
	s.schedulePendingJobs()

	got, err := database.GetJob(job.ID)
	if err != nil {
		t.Fatalf("Failed to get job: %v", err)
	}
	if got.WorkerID.String != worker2.ID {
		t.Errorf("tie should favor worker-2 (fewer completed jobs); got worker %q", got.WorkerID.String)
	}
}

// TestSchedulerLoadBalancingRoundRobin verifies that consecutive jobs
// distribute across idle workers instead of piling onto the first one
// (TSI-2477).
func TestSchedulerLoadBalancingRoundRobin(t *testing.T) {
	database, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("Failed to create database: %v", err)
	}
	defer database.Close()

	worker1, err := database.CreateWorker("worker-1", "w1", protocol.WorkerCapabilities{
		Encoders: []string{"libx264"}, FFmpegVersion: "5.0", MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatalf("Failed to create worker1: %v", err)
	}
	worker2, err := database.CreateWorker("worker-2", "w2", protocol.WorkerCapabilities{
		Encoders: []string{"libx264"}, FFmpegVersion: "5.0", MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatalf("Failed to create worker2: %v", err)
	}

	s := New(database, DefaultConfig())

	// Submit two jobs one at a time. With MaxConcurrent=1 the first job fills
	// one worker; the second must go to the other. The order in which workers
	// are picked does not matter, only that BOTH workers are used.
	job1, err := database.CreateJob(`["f1.mkv"]`, `[]`, "o1.mkv", false)
	if err != nil {
		t.Fatalf("Failed to create job1: %v", err)
	}
	s.schedulePendingJobs()

	job2, err := database.CreateJob(`["f2.mkv"]`, `[]`, "o2.mkv", false)
	if err != nil {
		t.Fatalf("Failed to create job2: %v", err)
	}
	s.schedulePendingJobs()

	got1, _ := database.GetJob(job1.ID)
	got2, _ := database.GetJob(job2.ID)

	if got1.WorkerID.String == got2.WorkerID.String {
		t.Errorf("both jobs assigned to the same worker %q; expected distribution across both workers",
			got1.WorkerID.String)
	}
	assigned := map[string]bool{got1.WorkerID.String: true, got2.WorkerID.String: true}
	if !assigned[worker1.ID] || !assigned[worker2.ID] {
		t.Errorf("expected both workers to receive a job; got %v", assigned)
	}
}
