package client

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tsic404/rffmpeg/pkg/protocol"
)

// UploadFile uploads a file to the server using streaming to avoid loading
// large files entirely into memory.
func (c *Client) UploadFile(filePath string) (string, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return "", fmt.Errorf("failed to open file: %w", err)
	}
	defer file.Close()

	pr, pw := io.Pipe()
	writer := multipart.NewWriter(pw)

	fileInfo, err := file.Stat()
	if err != nil {
		return "", fmt.Errorf("failed to get file info: %w", err)
	}

	contentLength := calculateMultipartSize(writer.Boundary(), fileInfo.Size(), filepath.Base(filePath))

	errChan := make(chan error, 1)
	go func() {
		defer pw.Close()
		defer writer.Close()

		part, err := writer.CreateFormFile("file", filepath.Base(filePath))
		if err != nil {
			errChan <- fmt.Errorf("failed to create form file: %w", err)
			return
		}

		if _, err := io.Copy(part, file); err != nil {
			errChan <- fmt.Errorf("failed to copy file: %w", err)
			return
		}
		errChan <- nil
	}()

	req, err := http.NewRequest("POST", c.serverURL+UploadEndpoint, pr)
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.ContentLength = contentLength
	c.setAuthHeader(req)

	client := &http.Client{
		Timeout: UploadTimeout,
		Transport: &http.Transport{

			IdleConnTimeout: 5 * time.Minute,
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("upload request failed: %w", err)
	}
	defer DrainAndClose(resp.Body)

	if resp.StatusCode != http.StatusOK {

		<-errChan

		if resp.StatusCode == http.StatusUnauthorized {
			msg := "missing or invalid token"
			if errResp, ok := parseErrorResponse(resp.Body); ok {
				msg = errResp.Message
			}
			return "", fmt.Errorf("upload failed: authentication rejected (HTTP %d): %s", resp.StatusCode, msg)
		}
		if errResp, ok := parseErrorResponse(resp.Body); ok {
			return "", fmt.Errorf("upload failed: %s", errResp.Message)
		}
		return "", fmt.Errorf("upload failed with status %d", resp.StatusCode)
	}

	if err := <-errChan; err != nil {
		return "", err
	}

	var uploadResp protocol.UploadResponse
	if err := json.NewDecoder(resp.Body).Decode(&uploadResp); err != nil {
		return "", fmt.Errorf("failed to decode response: %w", err)
	}

	return uploadResp.FileID, nil
}

// calculateMultipartSize calculates the exact size of a multipart form
// with the given boundary, file size, and filename.
func calculateMultipartSize(boundary string, fileSize int64, filename string) int64 {

	preamble := fmt.Sprintf("--%s\r\n", boundary)
	contentDisposition := fmt.Sprintf("Content-Disposition: form-data; name=\"file\"; filename=\"%s\"\r\n", escapeMultipartQuotes(filename))
	contentType := "Content-Type: application/octet-stream\r\n"
	headerEnd := "\r\n"
	epilogue := fmt.Sprintf("\r\n--%s--\r\n", boundary)

	return int64(len(preamble)+len(contentDisposition)+len(contentType)+len(headerEnd)) +
		fileSize +
		int64(len(epilogue))
}

// escapeMultipartQuotes mirrors mime/multipart's internal escapeQuotes: the
// stdlib escapes " and \ inside form-data parameter values. calculateMultipartSize
// must measure the escaped form or a filename containing a quote yields a
// Content-Length that disagrees with the real body and truncates the request.
func escapeMultipartQuotes(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "\"", "\\\"")
	return s
}

// Chunked upload constants
const (
	// DefaultChunkSize is the default chunk size for chunked uploads (10MB)
	DefaultChunkSize = 10 * 1024 * 1024
	// LargeFileThreshold is the threshold above which chunked upload is used (100MB)
	LargeFileThreshold = 100 * 1024 * 1024
	// MaxChunkRetries is the maximum number of retries for chunk upload
	MaxChunkRetries = 3
	// InitialRetryDelay is the initial delay for retry
	InitialRetryDelay = 1 * time.Second
	// MaxRetryDelay is the maximum delay for retry
	MaxRetryDelay = 30 * time.Second
)

// UploadFileChunked uploads a large file using the chunked upload API.
// This is more reliable for very large files (>100MB) as it uploads in smaller chunks.
func (c *Client) UploadFileChunked(filePath string, chunkSize int64) (string, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return "", fmt.Errorf("failed to open file: %w", err)
	}
	defer file.Close()

	fileInfo, err := file.Stat()
	if err != nil {
		return "", fmt.Errorf("failed to get file info: %w", err)
	}

	if chunkSize <= 0 {
		chunkSize = DefaultChunkSize
	}

	fileHash := sha256.New()
	if _, err := io.Copy(fileHash, file); err != nil {
		return "", fmt.Errorf("failed to calculate file checksum: %w", err)
	}
	fileChecksum := hex.EncodeToString(fileHash.Sum(nil))

	if _, err := file.Seek(0, 0); err != nil {
		return "", fmt.Errorf("failed to reset file position: %w", err)
	}

	initReq := protocol.ChunkUploadInitRequest{
		Filename:  filepath.Base(filePath),
		FileSize:  fileInfo.Size(),
		ChunkSize: chunkSize,
		Checksum:  fileChecksum,
	}

	initBody, err := json.Marshal(initReq)
	if err != nil {
		return "", fmt.Errorf("failed to marshal init request: %w", err)
	}

	req, err := http.NewRequest("POST", c.serverURL+"/api/v1/upload/init", bytes.NewReader(initBody))
	if err != nil {
		return "", fmt.Errorf("failed to create init request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	c.setAuthHeader(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("init request failed: %w", err)
	}
	defer DrainAndClose(resp.Body)

	if resp.StatusCode == http.StatusUnauthorized {
		msg := "missing or invalid token"
		if errResp, ok := parseErrorResponse(resp.Body); ok {
			msg = errResp.Message
		}
		return "", fmt.Errorf("init failed: authentication rejected (HTTP %d): %s", resp.StatusCode, msg)
	}

	if resp.StatusCode != http.StatusOK {
		if errResp, ok := parseErrorResponse(resp.Body); ok {
			return "", fmt.Errorf("init failed: %s", errResp.Message)
		}
		return "", fmt.Errorf("init failed with status %d", resp.StatusCode)
	}

	var initResp protocol.ChunkUploadInitResponse
	if err := json.NewDecoder(resp.Body).Decode(&initResp); err != nil {
		return "", fmt.Errorf("failed to decode init response: %w", err)
	}

	uploadID := initResp.UploadID
	totalChunks := initResp.TotalChunks
	actualChunkSize := initResp.ChunkSize

	for i := 0; i < totalChunks; i++ {

		if err := c.uploadChunkWithRetry(file, uploadID, i, actualChunkSize, fileInfo.Size(), totalChunks); err != nil {
			return "", fmt.Errorf("chunk %d upload failed after retries: %w", i, err)
		}
	}

	completeReq := protocol.ChunkUploadCompleteRequest{
		UploadID: uploadID,
		Checksum: fileChecksum,
	}
	completeBody, err := json.Marshal(completeReq)
	if err != nil {
		return "", fmt.Errorf("failed to marshal complete request: %w", err)
	}

	req, err = http.NewRequest("POST", c.serverURL+"/api/v1/upload/complete", bytes.NewReader(completeBody))
	if err != nil {
		return "", fmt.Errorf("failed to create complete request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	c.setAuthHeader(req)

	resp, err = c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("complete request failed: %w", err)
	}
	defer DrainAndClose(resp.Body)

	if resp.StatusCode != http.StatusOK {
		if errResp, ok := parseErrorResponse(resp.Body); ok {
			return "", fmt.Errorf("complete failed: %s", errResp.Message)
		}
		return "", fmt.Errorf("complete failed with status %d", resp.StatusCode)
	}

	var completeResp protocol.ChunkUploadCompleteResponse
	if err := json.NewDecoder(resp.Body).Decode(&completeResp); err != nil {
		return "", fmt.Errorf("failed to decode complete response: %w", err)
	}

	return completeResp.FileID, nil
}

// uploadChunkWithRetry uploads a single chunk with exponential backoff retry
func (c *Client) uploadChunkWithRetry(file *os.File, uploadID string, chunkIndex int, chunkSize int64, fileSize int64, totalChunks int) error {
	var lastErr error

	for attempt := 0; attempt < MaxChunkRetries; attempt++ {
		if attempt > 0 {

			delay := InitialRetryDelay * time.Duration(1<<(attempt-1))
			if delay > MaxRetryDelay {
				delay = MaxRetryDelay
			}
			time.Sleep(delay)
		}

		err := c.uploadSingleChunk(file, uploadID, chunkIndex, chunkSize, fileSize, totalChunks)
		if err == nil {
			return nil
		}
		lastErr = err

		if !isRetryableError(err) {
			return err
		}
	}

	return lastErr
}

// isRetryableError determines if an error is worth retrying.
func isRetryableError(err error) bool {
	if err == nil {
		return false
	}
	// Network timeouts are retryable. Detect them via the net.Error interface
	// rather than substring matching: http.Client.Timeout surfaces as
	// "context deadline exceeded (Client.Timeout exceeded ...)" — its
	// capitalized "Client.Timeout" evades a lowercase "timeout" check, so the
	// chunk retry path silently never retried the most common client timeout.
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}

	errStr := err.Error()
	return strings.Contains(errStr, "timeout") ||
		strings.Contains(errStr, "connection reset") ||
		strings.Contains(errStr, "temporary") ||
		strings.Contains(errStr, "unavailable") ||
		strings.Contains(errStr, "5xx") ||
		strings.Contains(errStr, "502") ||
		strings.Contains(errStr, "503") ||
		strings.Contains(errStr, "504")
}

// uploadSingleChunk uploads a single chunk without retry logic
func (c *Client) uploadSingleChunk(file *os.File, uploadID string, chunkIndex int, chunkSize int64, fileSize int64, totalChunks int) error {

	offset := int64(chunkIndex) * chunkSize
	if _, err := file.Seek(offset, 0); err != nil {
		return fmt.Errorf("failed to seek to chunk %d: %w", chunkIndex, err)
	}

	currentChunkSize := chunkSize
	if offset+chunkSize > fileSize {
		currentChunkSize = fileSize - offset
	}

	chunkHash := sha256.New()

	limitedReader := io.LimitReader(file, currentChunkSize)
	teeReader := io.TeeReader(limitedReader, chunkHash)

	chunkData, err := io.ReadAll(teeReader)
	if err != nil {
		return fmt.Errorf("failed to read chunk %d: %w", chunkIndex, err)
	}

	chunkChecksum := hex.EncodeToString(chunkHash.Sum(nil))

	pr, pw := io.Pipe()
	writer := multipart.NewWriter(pw)

	contentLength := calculateChunkMultipartSize(writer.Boundary(), currentChunkSize, chunkChecksum)

	errChan := make(chan error, 1)
	go func() {
		defer pw.Close()
		defer writer.Close()

		part, err := writer.CreateFormFile("chunk", "chunk")
		if err != nil {
			errChan <- err
			return
		}

		if _, err := part.Write(chunkData); err != nil {
			errChan <- err
			return
		}

		_ = writer.WriteField("checksum", chunkChecksum)

		errChan <- nil
	}()

	chunkURL := fmt.Sprintf("%s/api/v1/upload/chunk/%s/%d", c.serverURL, uploadID, chunkIndex)
	req, err := http.NewRequest("POST", chunkURL, pr)
	if err != nil {
		return fmt.Errorf("failed to create chunk request: %w", err)
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.ContentLength = contentLength
	c.setAuthHeader(req)

	timeout := calculateChunkTimeout(currentChunkSize)
	client := &http.Client{Timeout: timeout}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("chunk request failed: %w", err)
	}
	defer DrainAndClose(resp.Body)

	if resp.StatusCode != http.StatusOK {

		<-errChan

		if resp.StatusCode == http.StatusUnauthorized {
			msg := "missing or invalid token"
			if errResp, ok := parseErrorResponse(resp.Body); ok {
				msg = errResp.Message
			}
			return fmt.Errorf("chunk upload failed: authentication rejected (HTTP %d): %s", resp.StatusCode, msg)
		}
		if errResp, ok := parseErrorResponse(resp.Body); ok {
			return fmt.Errorf("chunk upload failed: %s", errResp.Message)
		}
		return fmt.Errorf("chunk upload failed with status %d", resp.StatusCode)
	}

	if err := <-errChan; err != nil {
		if errors.Is(err, io.ErrClosedPipe) {
			// A closed pipe only proves the writer stopped, not that the chunk
			// was stored. Suppress the error solely when the server explicitly
			// marks the idempotent already-uploaded path — the one 200 it
			// sends without draining the body. A bare 200 from a proxy or a
			// non-compliant server must still surface the write failure.
			var chunkResp protocol.ChunkUploadResponse
			if decodeErr := json.NewDecoder(resp.Body).Decode(&chunkResp); decodeErr == nil && chunkResp.Message == protocol.ChunkAlreadyUploadedMessage {
				return nil
			}
		}
		return fmt.Errorf("chunk write failed: %w", err)
	}

	return nil
}

// calculateChunkMultipartSize calculates the exact size of a chunk multipart form
func calculateChunkMultipartSize(boundary string, chunkSize int64, checksum string) int64 {

	preamble := fmt.Sprintf("--%s\r\n", boundary)
	contentDisposition := "Content-Disposition: form-data; name=\"chunk\"; filename=\"chunk\"\r\n"
	contentType := "Content-Type: application/octet-stream\r\n"
	headerEnd := "\r\n"

	checksumHeader := fmt.Sprintf("\r\n--%s\r\nContent-Disposition: form-data; name=\"checksum\"\r\n\r\n", boundary)
	checksumValue := checksum
	epilogue := fmt.Sprintf("\r\n--%s--\r\n", boundary)

	return int64(len(preamble)+len(contentDisposition)+len(contentType)+len(headerEnd)) +
		chunkSize +
		int64(len(checksumHeader)+len(checksumValue)+len(epilogue))
}

// calculateChunkTimeout calculates timeout based on chunk size
func calculateChunkTimeout(chunkSize int64) time.Duration {

	baseTimeout := 30 * time.Second
	mbPerSec := int64(3 * 1024 * 1024)

	timeout := baseTimeout + time.Duration(chunkSize/mbPerSec)*time.Second
	if timeout > UploadTimeout {
		timeout = UploadTimeout
	}
	return timeout
}

// UploadFileAuto automatically chooses between simple and chunked upload based on file size.
// Files larger than LargeFileThreshold (100MB) use chunked upload.
func (c *Client) UploadFileAuto(filePath string) (string, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return "", fmt.Errorf("failed to open file: %w", err)
	}
	defer file.Close()

	fileInfo, err := file.Stat()
	if err != nil {
		return "", fmt.Errorf("failed to get file info: %w", err)
	}

	if fileInfo.Size() > LargeFileThreshold {
		return c.UploadFileChunked(filePath, DefaultChunkSize)
	}

	return c.UploadFile(filePath)
}

// computeFileChecksum computes SHA256 checksum of a file
func computeFileChecksum(filePath string) (string, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return "", fmt.Errorf("failed to open file: %w", err)
	}
	defer file.Close()

	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", fmt.Errorf("failed to compute checksum: %w", err)
	}

	return hex.EncodeToString(hash.Sum(nil)), nil
}

// HashingReader wraps an io.Reader and computes a hash while reading
type HashingReader struct {
	reader io.Reader
	hash   hash.Hash
}

// NewHashingReader creates a new HashingReader
func NewHashingReader(reader io.Reader) *HashingReader {
	return &HashingReader{
		reader: reader,
		hash:   sha256.New(),
	}
}

// Read implements io.Reader
func (h *HashingReader) Read(p []byte) (n int, err error) {
	n, err = h.reader.Read(p)
	if n > 0 {
		h.hash.Write(p[:n])
	}
	return n, err
}

// Checksum returns the hex-encoded checksum
func (h *HashingReader) Checksum() string {
	return hex.EncodeToString(h.hash.Sum(nil))
}
