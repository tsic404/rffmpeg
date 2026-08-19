package protocol

import "time"

// ChunkUploadInitRequest initiates a new chunked upload session
type ChunkUploadInitRequest struct {
	Filename    string `json:"filename"`
	FileSize    int64  `json:"file_size"`
	ChunkSize   int64  `json:"chunk_size,omitempty"`   // Optional, defaults to server config
	TotalChunks int    `json:"total_chunks,omitempty"` // Optional, calculated from file_size/chunk_size
	Checksum    string `json:"checksum,omitempty"`     // Optional SHA256 of entire file for final verification
}

// ChunkUploadInitResponse returns the upload session details
type ChunkUploadInitResponse struct {
	UploadID    string `json:"upload_id"`
	ChunkSize   int64  `json:"chunk_size"`
	TotalChunks int    `json:"total_chunks"`
	Message     string `json:"message,omitempty"`
}

// ChunkUploadRequest represents a single chunk upload
type ChunkUploadRequest struct {
	UploadID    string `json:"upload_id"`
	ChunkIndex  int    `json:"chunk_index"` // 0-based index
	ChunkSize   int64  `json:"chunk_size"`
	Checksum    string `json:"checksum"` // SHA256 of this chunk
	TotalChunks int    `json:"total_chunks,omitempty"`
}

// ChunkUploadResponse returns the result of a chunk upload
type ChunkUploadResponse struct {
	UploadID   string `json:"upload_id"`
	ChunkIndex int    `json:"chunk_index"`
	Message    string `json:"message,omitempty"`
}

// UploadProgressResponse returns the current upload progress
type UploadProgressResponse struct {
	UploadID        string    `json:"upload_id"`
	Filename        string    `json:"filename"`
	FileSize        int64     `json:"file_size"`
	ChunkSize       int64     `json:"chunk_size"`
	TotalChunks     int       `json:"total_chunks"`
	UploadedChunks  int       `json:"uploaded_chunks"`
	UploadedSize    int64     `json:"uploaded_size"`
	CompletedChunks []int     `json:"completed_chunks"` // List of completed chunk indices
	Status          string    `json:"status"`           // "in_progress", "completed", "expired"
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// ChunkUploadCompleteRequest finalizes the upload
type ChunkUploadCompleteRequest struct {
	UploadID string `json:"upload_id"`
	Checksum string `json:"checksum,omitempty"` // Optional SHA256 for final verification
}

// ChunkUploadCompleteResponse returns the final file info
type ChunkUploadCompleteResponse struct {
	FileID  string `json:"file_id"`
	Message string `json:"message,omitempty"`
}

// UploadSessionStatus represents the status of an upload session
type UploadSessionStatus string

const (
	UploadSessionStatusInProgress UploadSessionStatus = "in_progress"
	UploadSessionStatusCompleted  UploadSessionStatus = "completed"
	UploadSessionStatusExpired    UploadSessionStatus = "expired"
	UploadSessionStatusCancelled  UploadSessionStatus = "cancelled"
)
