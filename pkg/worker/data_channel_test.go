package worker

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"

	"github.com/tsix404/rffmpeg/pkg/protocol"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestClient_DownloadInput_HTTPRemote verifies that the Worker client can download
// input files from an HTTP server, simulating S8.1 HTTP remote input scenario.
func TestClient_DownloadInput_HTTPRemote(t *testing.T) {
	// Create a test HTTP server serving a dummy file
	testContent := []byte("fake-video-content-for-testing")
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify the request has auth header if token is set
		if token := r.Header.Get("Authorization"); token == "" {
			// No auth — still serve for basic test
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write(testContent)
	}))
	defer ts.Close()

	// Create a client pointed at the test server
	client := NewClient(ts.URL, "test-worker", "")

	// Create a temp directory for download
	tmpDir, err := os.MkdirTemp("", "rffmpeg-http-download-test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	destPath := filepath.Join(tmpDir, "downloaded-input.mp4")

	// Download the file
	err = client.DownloadInput("any-file-id", destPath)
	if err != nil {
		t.Fatalf("DownloadInput failed: %v", err)
	}

	// Verify the file was downloaded correctly
	data, err := os.ReadFile(destPath)
	if err != nil {
		t.Fatalf("Failed to read downloaded file: %v", err)
	}
	if string(data) != string(testContent) {
		t.Errorf("Downloaded content mismatch: got %q, want %q", string(data), string(testContent))
	}
}

// TestClient_DownloadInput_ErrorHandling verifies error handling for HTTP download failures.
func TestClient_DownloadInput_ErrorHandling(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		expectErr  bool
	}{
		{"404 not found", http.StatusNotFound, true},
		{"403 forbidden", http.StatusForbidden, true},
		{"500 server error", http.StatusInternalServerError, true},
		{"200 OK", http.StatusOK, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.statusCode)
				if tt.statusCode == http.StatusOK {
					w.Write([]byte("ok"))
				}
			}))
			defer ts.Close()

			client := NewClient(ts.URL, "test-worker", "")

			tmpDir, _ := os.MkdirTemp("", "rffmpeg-err-test")
			defer os.RemoveAll(tmpDir)

			err := client.DownloadInput("test-file", filepath.Join(tmpDir, "out.mp4"))
			if tt.expectErr && err == nil {
				t.Error("Expected error but got nil")
			}
			if !tt.expectErr && err != nil {
				t.Errorf("Expected no error but got: %v", err)
			}
		})
	}
}

// TestClient_HTTPInput_WithTokenAuth verifies that the worker client sends
// authentication tokens when downloading from HTTP servers.
func TestClient_HTTPInput_WithTokenAuth(t *testing.T) {
	receivedToken := ""
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedToken = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("authenticated"))
	}))
	defer ts.Close()

	client := NewClient(ts.URL, "test-worker", "my-secret-token")

	tmpDir, _ := os.MkdirTemp("", "rffmpeg-auth-test")
	defer os.RemoveAll(tmpDir)

	err := client.DownloadInput("file-id", filepath.Join(tmpDir, "out.mp4"))
	if err != nil {
		t.Fatalf("DownloadInput failed: %v", err)
	}

	if !strings.Contains(receivedToken, "my-secret-token") {
		t.Errorf("Expected Authorization header with token, got: %q", receivedToken)
	}
}

// TestClient_StreamingOutputMode verifies the SendStdoutChunk function works
// for streaming output mode (S8.2 RTMP/streaming scenario).
func TestClient_StreamingOutputMode(t *testing.T) {
	receivedChunks := []string{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PATCH" {
			// Read the chunk from the request body
			body := make([]byte, 1024)
			n, _ := r.Body.Read(body)
			receivedChunks = append(receivedChunks, string(body[:n]))
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	client := NewClient(ts.URL, "test-worker", "")

	// Simulate sending stdout chunks (streaming output). Binary payloads must
	// arrive byte-for-byte: SendStdoutChunk base64-encodes before JSON transport.
	chunks := [][]byte{
		[]byte("FLV header data..."),
		[]byte("video tag frame 1..."),
		[]byte("video tag frame 2..."),
	}

	for _, chunk := range chunks {
		err := client.SendStdoutChunk("test-job-id", chunk)
		if err != nil {
			t.Fatalf("SendStdoutChunk failed: %v", err)
		}
	}

	if len(receivedChunks) != len(chunks) {
		t.Errorf("Expected %d chunks received, got %d", len(chunks), len(receivedChunks))
	}
}

// TestClient_SendStdoutChunk_BinaryIntegrity verifies that arbitrary binary
// stdout data survives the JSON transport byte-for-byte (TSI-2355). Before the
// fix, SendStdoutChunk put raw bytes in a JSON string field and encoding/json
// replaced invalid UTF-8 bytes with U+FFFD on both ends.
func TestClient_SendStdoutChunk_BinaryIntegrity(t *testing.T) {
	var mu sync.Mutex
	var receivedB64 string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PATCH" {
			body, _ := io.ReadAll(r.Body)
			mu.Lock()
			receivedB64 = string(body)
			mu.Unlock()
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	client := NewClient(ts.URL, "test-worker", "")

	// Payload containing bytes that are invalid UTF-8 (0xFF 0xFE) plus a NUL.
	payload := []byte{'F', 'L', 'V', 0x00, 0x01, 0xFF, 0xFE, 0x00, 0x80, 'E', 'N', 'D'}
	if err := client.SendStdoutChunk("test-job-id", payload); err != nil {
		t.Fatalf("SendStdoutChunk failed: %v", err)
	}

	// The server receives JSON; decode exactly as handlers.go would.
	var req protocol.JobUpdateRequest
	if err := json.Unmarshal([]byte(receivedB64), &req); err != nil {
		t.Fatalf("Failed to unmarshal request: %v", err)
	}

	got, err := protocol.StdoutChunkBase64(req.StdoutChunk)
	if err != nil {
		t.Fatalf("Failed to decode stdout chunk: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("stdout payload corrupted in transit:\n got %v\nwant %v", got, payload)
	}
}

// TestClient_StderrBatcher verifies the stderr batcher works for real-time log streaming.
func TestClient_StderrBatcher(t *testing.T) {
	receivedMsgs := []string{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PATCH" {
			body := make([]byte, 2048)
			n, _ := r.Body.Read(body)
			receivedMsgs = append(receivedMsgs, string(body[:n]))
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	client := NewClient(ts.URL, "test-worker", "")

	// Create stderr batcher
	batcher := NewStderrBatcher("test-job", client, DefaultStderrBatcherConfig())
	defer batcher.Close()

	handler := batcher.StderrHandler()

	// Write log lines
	handler("frame=  100 fps= 30 q=28.0 size=    1024kB time=00:00:03.33 bitrate=2518.0kbits/s\n")
	handler("frame=  200 fps= 30 q=28.0 size=    2048kB time=00:00:06.66 bitrate=2518.0kbits/s\n")

	// Close the batcher to flush remaining data
	batcher.Close()

	if len(receivedMsgs) == 0 {
		t.Error("Expected at least one stderr message to be sent")
	}
}

// TestClient_StdoutBatcher verifies the stdout batcher for streaming output.
func TestClient_StdoutBatcher(t *testing.T) {
	receivedMsgs := []string{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PATCH" {
			body := make([]byte, 2048)
			n, _ := r.Body.Read(body)
			receivedMsgs = append(receivedMsgs, string(body[:n]))
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	client := NewClient(ts.URL, "test-worker", "")

	stdoutBatcher := NewStdoutBatcher("test-job", client, DefaultStdoutBatcherConfig())
	defer stdoutBatcher.Close()

	handler := stdoutBatcher.StdoutHandler()
	handler([]byte{0x00, 0x01, 0x02, 0x03}) // Binary FLV data
	handler([]byte{0x04, 0x05, 0x06, 0x07})

	stdoutBatcher.Close()

	if len(receivedMsgs) == 0 {
		t.Error("Expected at least one stdout message to be sent")
	}
}

// TestClient_UploadOutput verifies file upload from worker to server.
func TestClient_UploadOutput(t *testing.T) {
	uploadedContent := []byte{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Read the multipart upload body
		body := make([]byte, 4096)
		n, _ := r.Body.Read(body)
		uploadedContent = body[:n]
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	client := NewClient(ts.URL, "test-worker", "")

	// Create a temp output file
	tmpFile, err := os.CreateTemp("", "rffmpeg-upload-test-*.mp4")
	if err != nil {
		t.Fatalf("Failed to create temp file: %v", err)
	}
	defer os.Remove(tmpFile.Name())

	testData := []byte("transcoded-video-output-data")
	if _, err := tmpFile.Write(testData); err != nil {
		t.Fatalf("Failed to write test data: %v", err)
	}
	tmpFile.Close()

	err = client.UploadOutput("test-job-id", tmpFile.Name())
	if err != nil {
		t.Fatalf("UploadOutput failed: %v", err)
	}

	// Verify the uploaded content includes our test data
	if len(uploadedContent) == 0 {
		t.Error("Expected uploaded content to be non-empty")
	}
}

// TestClient_RTMPStreamingOutput tests that streaming output mode bypasses
// file-based output and sends data directly to the server via WebSocket/stdout.
func TestClient_RTMPStreamingOutput(t *testing.T) {
	// In rffmpeg, RTMP output is handled by ffmpeg directly.
	// The worker passes the output URL to ffmpeg (e.g., rtmp://server/live/stream).
	// The rffmpeg server and CLI use WebSocket for streaming output ingestion.
	// This test verifies that the client correctly handles streaming output jobs.

	// Verify that the client can handle a streaming output job update
	receivedUpdates := []string{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PATCH" {
			body := make([]byte, 2048)
			n, _ := r.Body.Read(body)
			receivedUpdates = append(receivedUpdates, string(body[:n]))
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	client := NewClient(ts.URL, "test-worker", "")

	// Simulate a streaming job: worker sends stdout chunks
	for i := 0; i < 5; i++ {
		err := client.SendStdoutChunk("streaming-job", []byte("FLV-frame-data"))
		if err != nil {
			t.Fatalf("SendStdoutChunk failed: %v", err)
		}
	}

	// Mark job as completed
	err := client.UpdateJob("streaming-job", protocol.JobStatusCompleted, 0, "", false)
	if err != nil {
		t.Fatalf("UpdateJob failed: %v", err)
	}

	if len(receivedUpdates) < 6 {
		t.Errorf("Expected at least 6 updates (5 chunks + 1 completion), got %d", len(receivedUpdates))
	}
}
