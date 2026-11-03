package db

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/tsic404/rffmpeg/pkg/protocol"
)

func TestCreateUploadSession(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "db_test_*.db")
	if err != nil {
		t.Fatalf("Failed to create temp file: %v", err)
	}
	tmpFile.Close()
	defer os.Remove(tmpFile.Name())

	db, err := New(tmpFile.Name())
	if err != nil {
		t.Fatalf("Failed to create database: %v", err)
	}
	defer db.Close()

	checksum := "abc123"
	session, err := db.CreateUploadSession("test.mp4", 1024*1024*100, 10*1024*1024, 10, &checksum)
	if err != nil {
		t.Fatalf("Failed to create upload session: %v", err)
	}

	if session.Filename != "test.mp4" {
		t.Errorf("Expected filename test.mp4, got %s", session.Filename)
	}
	if session.FileSize != 1024*1024*100 {
		t.Errorf("Expected file size %d, got %d", 1024*1024*100, session.FileSize)
	}
	if session.TotalChunks != 10 {
		t.Errorf("Expected 10 chunks, got %d", session.TotalChunks)
	}
	if session.Status != protocol.UploadSessionStatusInProgress {
		t.Errorf("Expected status in_progress, got %s", session.Status)
	}
}

func TestGetUploadSession(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "db_test_*.db")
	if err != nil {
		t.Fatalf("Failed to create temp file: %v", err)
	}
	tmpFile.Close()
	defer os.Remove(tmpFile.Name())

	db, err := New(tmpFile.Name())
	if err != nil {
		t.Fatalf("Failed to create database: %v", err)
	}
	defer db.Close()

	session, err := db.CreateUploadSession("test.mp4", 1024, 512, 2, nil)
	if err != nil {
		t.Fatalf("Failed to create upload session: %v", err)
	}

	retrieved, err := db.GetUploadSession(session.ID)
	if err != nil {
		t.Fatalf("Failed to get upload session: %v", err)
	}

	if retrieved.ID != session.ID {
		t.Errorf("Expected ID %s, got %s", session.ID, retrieved.ID)
	}
}

func TestUpdateUploadSessionChunks(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "db_test_*.db")
	if err != nil {
		t.Fatalf("Failed to create temp file: %v", err)
	}
	tmpFile.Close()
	defer os.Remove(tmpFile.Name())

	db, err := New(tmpFile.Name())
	if err != nil {
		t.Fatalf("Failed to create database: %v", err)
	}
	defer db.Close()

	session, err := db.CreateUploadSession("test.mp4", 1024, 512, 3, nil)
	if err != nil {
		t.Fatalf("Failed to create upload session: %v", err)
	}

	// Add chunks
	if err := db.UpdateUploadSessionChunks(session.ID, 0); err != nil {
		t.Fatalf("Failed to update chunks: %v", err)
	}
	if err := db.UpdateUploadSessionChunks(session.ID, 2); err != nil {
		t.Fatalf("Failed to update chunks: %v", err)
	}

	// Retrieve and verify
	retrieved, err := db.GetUploadSession(session.ID)
	if err != nil {
		t.Fatalf("Failed to get upload session: %v", err)
	}

	var uploadedChunks []int
	if err := json.Unmarshal([]byte(retrieved.UploadedChunks), &uploadedChunks); err != nil {
		t.Fatalf("Failed to parse uploaded chunks: %v", err)
	}

	if len(uploadedChunks) != 2 {
		t.Errorf("Expected 2 uploaded chunks, got %d", len(uploadedChunks))
	}

	// Verify idempotency - adding same chunk again should not duplicate
	if err := db.UpdateUploadSessionChunks(session.ID, 0); err != nil {
		t.Fatalf("Failed to update chunks (idempotent): %v", err)
	}

	retrieved, _ = db.GetUploadSession(session.ID)
	json.Unmarshal([]byte(retrieved.UploadedChunks), &uploadedChunks)
	if len(uploadedChunks) != 2 {
		t.Errorf("Expected 2 uploaded chunks after idempotent update, got %d", len(uploadedChunks))
	}
}

func TestCompleteUploadSession(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "db_test_*.db")
	if err != nil {
		t.Fatalf("Failed to create temp file: %v", err)
	}
	tmpFile.Close()
	defer os.Remove(tmpFile.Name())

	db, err := New(tmpFile.Name())
	if err != nil {
		t.Fatalf("Failed to create database: %v", err)
	}
	defer db.Close()

	session, err := db.CreateUploadSession("test.mp4", 1024, 512, 2, nil)
	if err != nil {
		t.Fatalf("Failed to create upload session: %v", err)
	}

	if claimed, err := db.ClaimUploadSessionAssembly(session.ID); err != nil || !claimed {
		t.Fatalf("ClaimUploadSessionAssembly = (%v, %v), want (true, nil)", claimed, err)
	}
	if err := db.CompleteUploadSession(session.ID, "abc123"); err != nil {
		t.Fatalf("Failed to complete upload session: %v", err)
	}

	retrieved, err := db.GetUploadSession(session.ID)
	if err != nil {
		t.Fatalf("Failed to get upload session: %v", err)
	}

	if retrieved.Status != protocol.UploadSessionStatusCompleted {
		t.Errorf("Expected status completed, got %s", retrieved.Status)
	}
	if !retrieved.FileID.Valid || retrieved.FileID.String != "abc123" {
		t.Errorf("Expected file id abc123 to be persisted, got %+v", retrieved.FileID)
	}
}

func TestClaimUploadSessionAssemblyIsSingleWinner(t *testing.T) {
	db, err := New(":memory:")
	if err != nil {
		t.Fatalf("Failed to create database: %v", err)
	}
	defer db.Close()

	session, err := db.CreateUploadSession("test.mp4", 1024, 512, 2, nil)
	if err != nil {
		t.Fatalf("Failed to create upload session: %v", err)
	}

	claimed, err := db.ClaimUploadSessionAssembly(session.ID)
	if err != nil || !claimed {
		t.Fatalf("first claim = (%v, %v), want (true, nil)", claimed, err)
	}
	session, err = db.GetUploadSession(session.ID)
	if err != nil {
		t.Fatalf("GetUploadSession: %v", err)
	}
	if session.Status != protocol.UploadSessionStatusAssembling {
		t.Fatalf("status after claim = %s, want %s", session.Status, protocol.UploadSessionStatusAssembling)
	}

	claimed, err = db.ClaimUploadSessionAssembly(session.ID)
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if claimed {
		t.Error("second claim won the CAS: a concurrent completion could assemble the same chunks twice")
	}

	// A failed assembly reopens the session, so a corrected retry can claim it.
	if err := db.ReopenUploadSession(session.ID); err != nil {
		t.Fatalf("ReopenUploadSession: %v", err)
	}
	session, err = db.GetUploadSession(session.ID)
	if err != nil {
		t.Fatalf("GetUploadSession: %v", err)
	}
	if session.Status != protocol.UploadSessionStatusInProgress {
		t.Fatalf("status after reopen = %s, want %s", session.Status, protocol.UploadSessionStatusInProgress)
	}
	claimed, err = db.ClaimUploadSessionAssembly(session.ID)
	if err != nil || !claimed {
		t.Fatalf("claim after reopen = (%v, %v), want (true, nil)", claimed, err)
	}
}

func TestCompleteUploadSessionRequiresAssemblyClaim(t *testing.T) {
	db, err := New(":memory:")
	if err != nil {
		t.Fatalf("Failed to create database: %v", err)
	}
	defer db.Close()

	session, err := db.CreateUploadSession("test.mp4", 1024, 512, 2, nil)
	if err != nil {
		t.Fatalf("Failed to create upload session: %v", err)
	}

	if err := db.CompleteUploadSession(session.ID, "abc123"); err == nil {
		t.Fatal("expected completion without the assembly claim to fail")
	}
	retrieved, err := db.GetUploadSession(session.ID)
	if err != nil {
		t.Fatalf("GetUploadSession: %v", err)
	}
	if retrieved.Status != protocol.UploadSessionStatusInProgress {
		t.Errorf("status = %s, want %s", retrieved.Status, protocol.UploadSessionStatusInProgress)
	}
	if retrieved.FileID.Valid {
		t.Errorf("file id must stay unset after a rejected completion, got %q", retrieved.FileID.String)
	}
}

func TestCancelUploadSession(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "db_test_*.db")
	if err != nil {
		t.Fatalf("Failed to create temp file: %v", err)
	}
	tmpFile.Close()
	defer os.Remove(tmpFile.Name())

	db, err := New(tmpFile.Name())
	if err != nil {
		t.Fatalf("Failed to create database: %v", err)
	}
	defer db.Close()

	session, err := db.CreateUploadSession("test.mp4", 1024, 512, 2, nil)
	if err != nil {
		t.Fatalf("Failed to create upload session: %v", err)
	}

	if err := db.CancelUploadSession(session.ID); err != nil {
		t.Fatalf("Failed to cancel upload session: %v", err)
	}

	retrieved, err := db.GetUploadSession(session.ID)
	if err != nil {
		t.Fatalf("Failed to get upload session: %v", err)
	}

	if retrieved.Status != protocol.UploadSessionStatusCancelled {
		t.Errorf("Expected status cancelled, got %s", retrieved.Status)
	}
}

func TestCreateUploadChunk(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "db_test_*.db")
	if err != nil {
		t.Fatalf("Failed to create temp file: %v", err)
	}
	tmpFile.Close()
	defer os.Remove(tmpFile.Name())

	db, err := New(tmpFile.Name())
	if err != nil {
		t.Fatalf("Failed to create database: %v", err)
	}
	defer db.Close()

	session, err := db.CreateUploadSession("test.mp4", 1024, 512, 2, nil)
	if err != nil {
		t.Fatalf("Failed to create upload session: %v", err)
	}

	chunk, err := db.CreateUploadChunk(session.ID, 0, 512, "checksum123", "/path/to/chunk")
	if err != nil {
		t.Fatalf("Failed to create upload chunk: %v", err)
	}

	if chunk.UploadID != session.ID {
		t.Errorf("Expected upload ID %s, got %s", session.ID, chunk.UploadID)
	}
	if chunk.ChunkIndex != 0 {
		t.Errorf("Expected chunk index 0, got %d", chunk.ChunkIndex)
	}
}

func TestGetUploadChunk(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "db_test_*.db")
	if err != nil {
		t.Fatalf("Failed to create temp file: %v", err)
	}
	tmpFile.Close()
	defer os.Remove(tmpFile.Name())

	db, err := New(tmpFile.Name())
	if err != nil {
		t.Fatalf("Failed to create database: %v", err)
	}
	defer db.Close()

	session, _ := db.CreateUploadSession("test.mp4", 1024, 512, 2, nil)
	db.CreateUploadChunk(session.ID, 0, 512, "checksum123", "/path/to/chunk")

	chunk, err := db.GetUploadChunk(session.ID, 0)
	if err != nil {
		t.Fatalf("Failed to get upload chunk: %v", err)
	}

	if chunk.Checksum != "checksum123" {
		t.Errorf("Expected checksum checksum123, got %s", chunk.Checksum)
	}
}

func TestGetUploadChunks(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "db_test_*.db")
	if err != nil {
		t.Fatalf("Failed to create temp file: %v", err)
	}
	tmpFile.Close()
	defer os.Remove(tmpFile.Name())

	db, err := New(tmpFile.Name())
	if err != nil {
		t.Fatalf("Failed to create database: %v", err)
	}
	defer db.Close()

	session, _ := db.CreateUploadSession("test.mp4", 1024, 512, 3, nil)
	db.CreateUploadChunk(session.ID, 0, 512, "checksum0", "/path/0")
	db.CreateUploadChunk(session.ID, 1, 512, "checksum1", "/path/1")
	db.CreateUploadChunk(session.ID, 2, 512, "checksum2", "/path/2")

	chunks, err := db.GetUploadChunks(session.ID)
	if err != nil {
		t.Fatalf("Failed to get upload chunks: %v", err)
	}

	if len(chunks) != 3 {
		t.Errorf("Expected 3 chunks, got %d", len(chunks))
	}

	// Verify ordering
	for i, chunk := range chunks {
		if chunk.ChunkIndex != i {
			t.Errorf("Expected chunk index %d, got %d", i, chunk.ChunkIndex)
		}
	}
}

func TestChunkExists(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "db_test_*.db")
	if err != nil {
		t.Fatalf("Failed to create temp file: %v", err)
	}
	tmpFile.Close()
	defer os.Remove(tmpFile.Name())

	db, err := New(tmpFile.Name())
	if err != nil {
		t.Fatalf("Failed to create database: %v", err)
	}
	defer db.Close()

	session, _ := db.CreateUploadSession("test.mp4", 1024, 512, 2, nil)
	db.CreateUploadChunk(session.ID, 0, 512, "checksum", "/path")

	exists, err := db.ChunkExists(session.ID, 0)
	if err != nil {
		t.Fatalf("Failed to check chunk existence: %v", err)
	}
	if !exists {
		t.Error("Chunk 0 should exist")
	}
	exists, err = db.ChunkExists(session.ID, 1)
	if err != nil {
		t.Fatalf("Failed to check chunk existence: %v", err)
	}
	if exists {
		t.Error("Chunk 1 should not exist")
	}
}

func TestDeleteUploadChunks(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "db_test_*.db")
	if err != nil {
		t.Fatalf("Failed to create temp file: %v", err)
	}
	tmpFile.Close()
	defer os.Remove(tmpFile.Name())

	db, err := New(tmpFile.Name())
	if err != nil {
		t.Fatalf("Failed to create database: %v", err)
	}
	defer db.Close()

	session, _ := db.CreateUploadSession("test.mp4", 1024, 512, 2, nil)
	db.CreateUploadChunk(session.ID, 0, 512, "checksum", "/path")
	db.CreateUploadChunk(session.ID, 1, 512, "checksum", "/path")

	if err := db.DeleteUploadChunks(session.ID); err != nil {
		t.Fatalf("Failed to delete upload chunks: %v", err)
	}

	chunks, _ := db.GetUploadChunks(session.ID)
	if len(chunks) != 0 {
		t.Errorf("Expected 0 chunks after deletion, got %d", len(chunks))
	}
}

// UploadSession tests
func TestUploadSessionFileChecksum(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "db_test_*.db")
	if err != nil {
		t.Fatalf("Failed to create temp file: %v", err)
	}
	tmpFile.Close()
	defer os.Remove(tmpFile.Name())

	db, err := New(tmpFile.Name())
	if err != nil {
		t.Fatalf("Failed to create database: %v", err)
	}
	defer db.Close()

	// Test with checksum
	checksum := "sha256:abc123"
	session, err := db.CreateUploadSession("test.mp4", 1024, 512, 2, &checksum)
	if err != nil {
		t.Fatalf("Failed to create upload session: %v", err)
	}

	retrieved, _ := db.GetUploadSession(session.ID)
	if !retrieved.FileChecksum.Valid {
		t.Error("FileChecksum should be valid")
	}
	if retrieved.FileChecksum.String != checksum {
		t.Errorf("Expected checksum %s, got %s", checksum, retrieved.FileChecksum.String)
	}

	// Test without checksum
	session2, err := db.CreateUploadSession("test2.mp4", 1024, 512, 2, nil)
	if err != nil {
		t.Fatalf("Failed to create upload session: %v", err)
	}

	retrieved2, _ := db.GetUploadSession(session2.ID)
	if retrieved2.FileChecksum.Valid {
		t.Error("FileChecksum should not be valid for session without checksum")
	}
}

func TestExpireUploadSessionsAndListExpiredIDs(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "db_test_*.db")
	if err != nil {
		t.Fatalf("Failed to create temp file: %v", err)
	}
	tmpFile.Close()
	defer os.Remove(tmpFile.Name())

	db, err := New(tmpFile.Name())
	if err != nil {
		t.Fatalf("Failed to create database: %v", err)
	}
	defer db.Close()

	// A live session (not yet expired), an expired in_progress session and an
	// expired session whose completion claim left it in assembling.
	live, err := db.CreateUploadSession("live.mp4", 1024, 512, 2, nil)
	if err != nil {
		t.Fatalf("CreateUploadSession live: %v", err)
	}
	expired, err := db.CreateUploadSession("expired.mp4", 1024, 512, 2, nil)
	if err != nil {
		t.Fatalf("CreateUploadSession expired: %v", err)
	}
	assembling, err := db.CreateUploadSession("assembling.mp4", 1024, 512, 2, nil)
	if err != nil {
		t.Fatalf("CreateUploadSession assembling: %v", err)
	}
	if claimed, err := db.ClaimUploadSessionAssembly(assembling.ID); err != nil || !claimed {
		t.Fatalf("ClaimUploadSessionAssembly = (%v, %v), want (true, nil)", claimed, err)
	}

	for _, id := range []string{expired.ID, assembling.ID} {
		if _, err := db.db.Exec(`UPDATE upload_sessions SET expires_at = ? WHERE id = ?`,
			time.Now().Add(-time.Hour), id); err != nil {
			t.Fatalf("backdate expires_at for %s: %v", id, err)
		}
	}

	affected, err := db.ExpireUploadSessions()
	if err != nil {
		t.Fatalf("ExpireUploadSessions: %v", err)
	}
	if affected != 2 {
		t.Errorf("ExpireUploadSessions affected %d rows, want 2 (in_progress + assembling)", affected)
	}

	ids, err := db.GetExpiredUploadSessionIDs()
	if err != nil {
		t.Fatalf("GetExpiredUploadSessionIDs: %v", err)
	}
	got := make(map[string]bool, len(ids))
	for _, id := range ids {
		got[id] = true
	}
	if len(ids) != 2 || !got[expired.ID] || !got[assembling.ID] {
		t.Errorf("expired IDs = %v, want both %s and %s", ids, expired.ID, assembling.ID)
	}

	// The live session must not be expired or listed.
	retrieved, err := db.GetUploadSession(live.ID)
	if err != nil {
		t.Fatalf("GetUploadSession live: %v", err)
	}
	if retrieved.Status != protocol.UploadSessionStatusInProgress {
		t.Errorf("live status = %s, want in_progress", retrieved.Status)
	}
}
