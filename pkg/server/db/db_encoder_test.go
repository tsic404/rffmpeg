package db

import (
	"fmt"
	"os"
	"testing"

	"github.com/tsix404/rffmpeg/pkg/protocol"
)

func setupEncoderDBTest(t *testing.T) (*Database, func()) {
	t.Helper()
	tmpDir, err := os.MkdirTemp("", "rffmpeg-db-enc-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}

	database, err := New(fmt.Sprintf("%s/test.db", tmpDir))
	if err != nil {
		os.RemoveAll(tmpDir)
		t.Fatalf("Failed to create database: %v", err)
	}

	cleanup := func() {
		database.Close()
		os.RemoveAll(tmpDir)
	}

	return database, cleanup
}

// insertWorkerRaw inserts a worker with raw JSON strings for video_encoders/video_decoders.
// Used for testing edge cases with malformed JSON.
func insertWorkerRaw(d *Database, id, name string, videoEncodersJSON, videoDecodersJSON string) error {
	_, err := d.db.Exec(
		`INSERT INTO workers (id, name, status, encoders, decoders, video_encoders, video_decoders, ffmpeg_version, max_concurrent, evicted, evicted_at, last_heartbeat, created_at)
		 VALUES (?, ?, ?, '[]', '[]', ?, ?, 'test', 1, 0, NULL, datetime('now'), datetime('now'))`,
		id, name, protocol.WorkerStatusIdle, videoEncodersJSON, videoDecodersJSON,
	)
	return err
}

func TestGetAllEncodersInfoBasic(t *testing.T) {
	db, cleanup := setupEncoderDBTest(t)
	defer cleanup()

	// Create worker with encoders
	caps := protocol.WorkerCapabilities{
		VideoEncoders: []protocol.EncoderInfo{
			{Name: "h264_nvenc", Description: "NVIDIA NVENC H.264", Type: "video", IsHW: true},
			{Name: "libx264", Description: "libx264 H.264", Type: "video", IsHW: false},
		},
		VideoDecoders: []protocol.DecoderInfo{},
	}
	_, err := db.CreateWorker("", "worker-1", caps)
	if err != nil {
		t.Fatalf("Failed to create worker: %v", err)
	}

	encoders, err := db.GetAllEncodersInfo()
	if err != nil {
		t.Fatalf("GetAllEncodersInfo failed: %v", err)
	}

	if len(encoders) != 2 {
		t.Fatalf("Expected 2 encoders, got %d", len(encoders))
	}
}

func TestGetAllEncodersInfoDedupByName(t *testing.T) {
	db, cleanup := setupEncoderDBTest(t)
	defer cleanup()

	// Create two workers with overlapping encoder names
	caps1 := protocol.WorkerCapabilities{
		VideoEncoders: []protocol.EncoderInfo{
			{Name: "h264_nvenc", Description: "NVIDIA NVENC H.264", Type: "video", IsHW: true},
			{Name: "libx264", Description: "libx264 H.264", Type: "video", IsHW: false},
		},
	}
	caps2 := protocol.WorkerCapabilities{
		VideoEncoders: []protocol.EncoderInfo{
			{Name: "h264_nvenc", Description: "NVIDIA NVENC H.264", Type: "video", IsHW: true},
			{Name: "hevc_nvenc", Description: "NVIDIA NVENC HEVC", Type: "video", IsHW: true},
		},
	}

	_, err := db.CreateWorker("", "worker-1", caps1)
	if err != nil {
		t.Fatalf("Failed to create worker 1: %v", err)
	}
	_, err = db.CreateWorker("", "worker-2", caps2)
	if err != nil {
		t.Fatalf("Failed to create worker 2: %v", err)
	}

	encoders, err := db.GetAllEncodersInfo()
	if err != nil {
		t.Fatalf("GetAllEncodersInfo failed: %v", err)
	}

	// Should get 3 unique encoders (h264_nvenc deduped). Both workers register
	// identical h264_nvenc metadata, so the merged entry equals either.
	if len(encoders) != 3 {
		t.Fatalf("Expected 3 unique encoders, got %d: %v", len(encoders), encoders)
	}

	names := make(map[string]bool)
	for _, enc := range encoders {
		names[enc.Name] = true
	}
	if !names["h264_nvenc"] || !names["libx264"] || !names["hevc_nvenc"] {
		t.Errorf("Missing expected encoder names, got: %v", names)
	}
}

// TestGetAllEncodersInfoMultiWorkerPriorityMerge (TSI-2554) covers the
// multi-worker same-name aggregation: an earlier-registered low-priority
// encoder must not shadow a later-registered higher-priority entry.
func TestGetAllEncodersInfoMultiWorkerPriorityMerge(t *testing.T) {
	db, cleanup := setupEncoderDBTest(t)
	defer cleanup()

	// Worker 1 registers first with a low-priority software entry.
	caps1 := protocol.WorkerCapabilities{
		VideoEncoders: []protocol.EncoderInfo{
			{Name: "h264", Description: "H.264 (software)", Type: "video", IsHW: false, Priority: 1},
		},
	}
	// Worker 2 registers later with a higher-priority hardware entry.
	caps2 := protocol.WorkerCapabilities{
		VideoEncoders: []protocol.EncoderInfo{
			{Name: "h264", Description: "H.264 (hardware)", Type: "video", IsHW: true, Priority: 5},
		},
	}

	if _, err := db.CreateWorker("", "worker-1", caps1); err != nil {
		t.Fatalf("Failed to create worker 1: %v", err)
	}
	if _, err := db.CreateWorker("", "worker-2", caps2); err != nil {
		t.Fatalf("Failed to create worker 2: %v", err)
	}

	encoders, err := db.GetAllEncodersInfo()
	if err != nil {
		t.Fatalf("GetAllEncodersInfo failed: %v", err)
	}

	if len(encoders) != 1 {
		t.Fatalf("Expected 1 unique encoder, got %d: %v", len(encoders), encoders)
	}
	if !encoders[0].IsHW {
		t.Errorf("Expected hardware encoder to win, got %+v", encoders[0])
	}
	if encoders[0].Priority != 5 {
		t.Errorf("Expected priority 5 to win, got %+v", encoders[0])
	}
}

func TestGetAllEncodersInfoEmpty(t *testing.T) {
	db, cleanup := setupEncoderDBTest(t)
	defer cleanup()

	// Create worker with no video encoders
	caps := protocol.WorkerCapabilities{
		VideoEncoders: []protocol.EncoderInfo{},
		VideoDecoders: []protocol.DecoderInfo{},
	}
	_, err := db.CreateWorker("", "worker-1", caps)
	if err != nil {
		t.Fatalf("Failed to create worker: %v", err)
	}

	encoders, err := db.GetAllEncodersInfo()
	if err != nil {
		t.Fatalf("GetAllEncodersInfo failed: %v", err)
	}

	if len(encoders) != 0 {
		t.Fatalf("Expected 0 encoders, got %d", len(encoders))
	}
}

func TestGetAllEncodersInfoMalformedJSON(t *testing.T) {
	db, cleanup := setupEncoderDBTest(t)
	defer cleanup()

	// Insert a worker with malformed video_encoders JSON directly
	err := insertWorkerRaw(db, "test-id-1", "bad-worker", "{bad json", "[]")
	if err != nil {
		t.Fatalf("Failed to insert raw worker: %v", err)
	}

	// Insert a worker with valid encoders
	caps := protocol.WorkerCapabilities{
		VideoEncoders: []protocol.EncoderInfo{
			{Name: "libx264", Description: "libx264 H.264", Type: "video", IsHW: false},
		},
	}
	_, err = db.CreateWorker("", "good-worker", caps)
	if err != nil {
		t.Fatalf("Failed to create good worker: %v", err)
	}

	encoders, err := db.GetAllEncodersInfo()
	if err != nil {
		t.Fatalf("GetAllEncodersInfo failed: %v", err)
	}

	// Should return only the valid encoder (malformed JSON skipped with warning)
	if len(encoders) != 1 {
		t.Fatalf("Expected 1 encoder from valid worker, got %d", len(encoders))
	}
	if encoders[0].Name != "libx264" {
		t.Errorf("Expected encoder name 'libx264', got '%s'", encoders[0].Name)
	}
}

func TestGetAllDecodersInfoBasic(t *testing.T) {
	db, cleanup := setupEncoderDBTest(t)
	defer cleanup()

	caps := protocol.WorkerCapabilities{
		VideoEncoders: []protocol.EncoderInfo{},
		VideoDecoders: []protocol.DecoderInfo{
			{Name: "h264", Description: "H.264 decoder", Type: "video", IsHW: false},
			{Name: "hevc", Description: "HEVC decoder", Type: "video", IsHW: false},
		},
	}
	_, err := db.CreateWorker("", "worker-1", caps)
	if err != nil {
		t.Fatalf("Failed to create worker: %v", err)
	}

	decoders, err := db.GetAllDecodersInfo()
	if err != nil {
		t.Fatalf("GetAllDecodersInfo failed: %v", err)
	}

	if len(decoders) != 2 {
		t.Fatalf("Expected 2 decoders, got %d", len(decoders))
	}
}

func TestGetAllDecodersInfoDedupByName(t *testing.T) {
	db, cleanup := setupEncoderDBTest(t)
	defer cleanup()

	caps1 := protocol.WorkerCapabilities{
		VideoDecoders: []protocol.DecoderInfo{
			{Name: "h264", Description: "H.264 decoder", Type: "video", IsHW: false},
			{Name: "hevc", Description: "HEVC decoder", Type: "video", IsHW: false},
		},
	}
	caps2 := protocol.WorkerCapabilities{
		VideoDecoders: []protocol.DecoderInfo{
			{Name: "h264", Description: "H.264 decoder (HW)", Type: "video", IsHW: true},
			{Name: "av1", Description: "AV1 decoder", Type: "video", IsHW: false},
		},
	}

	_, err := db.CreateWorker("", "worker-1", caps1)
	if err != nil {
		t.Fatalf("Failed to create worker 1: %v", err)
	}
	_, err = db.CreateWorker("", "worker-2", caps2)
	if err != nil {
		t.Fatalf("Failed to create worker 2: %v", err)
	}

	decoders, err := db.GetAllDecodersInfo()
	if err != nil {
		t.Fatalf("GetAllDecodersInfo failed: %v", err)
	}

	// Should get 3 unique decoders (h264 deduped)
	if len(decoders) != 3 {
		t.Fatalf("Expected 3 unique decoders, got %d: %v", len(decoders), decoders)
	}

	names := make(map[string]bool)
	for _, dec := range decoders {
		names[dec.Name] = true
	}
	if !names["h264"] || !names["hevc"] || !names["av1"] {
		t.Errorf("Missing expected decoder names, got: %v", names)
	}
}

// TestGetAllDecodersInfoMultiWorkerMerge (TSI-2554) covers the multi-worker
// same-name aggregation: an earlier-registered software decoder must not
// shadow a later-registered hardware entry.
func TestGetAllDecodersInfoMultiWorkerMerge(t *testing.T) {
	db, cleanup := setupEncoderDBTest(t)
	defer cleanup()

	// Worker 1 registers first with a software decoder.
	caps1 := protocol.WorkerCapabilities{
		VideoDecoders: []protocol.DecoderInfo{
			{Name: "h264", Description: "H.264 decoder", Type: "video", IsHW: false},
		},
	}
	// Worker 2 registers later with a hardware decoder.
	caps2 := protocol.WorkerCapabilities{
		VideoDecoders: []protocol.DecoderInfo{
			{Name: "h264", Description: "H.264 decoder (HW)", Type: "video", IsHW: true},
		},
	}

	if _, err := db.CreateWorker("", "worker-1", caps1); err != nil {
		t.Fatalf("Failed to create worker 1: %v", err)
	}
	if _, err := db.CreateWorker("", "worker-2", caps2); err != nil {
		t.Fatalf("Failed to create worker 2: %v", err)
	}

	decoders, err := db.GetAllDecodersInfo()
	if err != nil {
		t.Fatalf("GetAllDecodersInfo failed: %v", err)
	}

	if len(decoders) != 1 {
		t.Fatalf("Expected 1 unique decoder, got %d: %v", len(decoders), decoders)
	}
	if !decoders[0].IsHW {
		t.Errorf("Expected hardware decoder to win, got %+v", decoders[0])
	}
	if decoders[0].Description != "H.264 decoder (HW)" {
		t.Errorf("Expected hardware description to win, got %+v", decoders[0])
	}
}

func TestGetAllDecodersInfoEmpty(t *testing.T) {
	db, cleanup := setupEncoderDBTest(t)
	defer cleanup()

	caps := protocol.WorkerCapabilities{
		VideoEncoders: []protocol.EncoderInfo{},
		VideoDecoders: []protocol.DecoderInfo{},
	}
	_, err := db.CreateWorker("", "worker-1", caps)
	if err != nil {
		t.Fatalf("Failed to create worker: %v", err)
	}

	decoders, err := db.GetAllDecodersInfo()
	if err != nil {
		t.Fatalf("GetAllDecodersInfo failed: %v", err)
	}

	if len(decoders) != 0 {
		t.Fatalf("Expected 0 decoders, got %d", len(decoders))
	}
}

func TestGetAllDecodersInfoMalformedJSON(t *testing.T) {
	db, cleanup := setupEncoderDBTest(t)
	defer cleanup()

	// Insert a worker with malformed video_decoders JSON directly
	err := insertWorkerRaw(db, "test-id-2", "bad-decoder-worker", "[]", "{bad json")
	if err != nil {
		t.Fatalf("Failed to insert raw worker: %v", err)
	}

	// Insert a worker with valid decoders
	caps := protocol.WorkerCapabilities{
		VideoDecoders: []protocol.DecoderInfo{
			{Name: "h264", Description: "H.264 decoder", Type: "video", IsHW: false},
		},
	}
	_, err = db.CreateWorker("", "good-decoder-worker", caps)
	if err != nil {
		t.Fatalf("Failed to create good worker: %v", err)
	}

	decoders, err := db.GetAllDecodersInfo()
	if err != nil {
		t.Fatalf("GetAllDecodersInfo failed: %v", err)
	}

	// Should return only the valid decoder
	if len(decoders) != 1 {
		t.Fatalf("Expected 1 decoder from valid worker, got %d", len(decoders))
	}
	if decoders[0].Name != "h264" {
		t.Errorf("Expected decoder name 'h264', got '%s'", decoders[0].Name)
	}
}
