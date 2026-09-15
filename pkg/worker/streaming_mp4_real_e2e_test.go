package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tsic404/rffmpeg/pkg/protocol"
	"github.com/tsic404/rffmpeg/pkg/worker/gpu"
)

// streamingCaptureServer serves a real input file and reassembles the
// streamed stdout chunks (base64) into a single byte buffer, so a test can
// verify the byte-level output of a streaming transcode.
type streamingCaptureServer struct {
	*httptest.Server

	mu     sync.Mutex
	stdout []byte
	input  []byte
}

func newStreamingCaptureServer(input []byte) *streamingCaptureServer {
	s := &streamingCaptureServer{input: input}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/files/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write(s.input)
	})
	mux.HandleFunc("/api/v1/jobs/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch {
			body, _ := io.ReadAll(r.Body)
			var u protocol.JobUpdateRequest
			if json.Unmarshal(body, &u) == nil && u.StdoutChunk != "" {
				if chunk, err := protocol.StdoutChunkBase64(u.StdoutChunk); err == nil {
					s.mu.Lock()
					s.stdout = append(s.stdout, chunk...)
					s.mu.Unlock()
				}
			}
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"message":"ok"}`))
	})
	s.Server = httptest.NewServer(mux)
	return s
}

func (s *streamingCaptureServer) Output() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.stdout...)
}

// mp4AtomOffsets walks the top-level MP4 box layout and returns the byte
// offset of each box type. It returns false if the buffer is not a well-formed
// MP4 (truncated box, zero-length box before the end).
func mp4AtomOffsets(data []byte) (map[string][]int, bool) {
	offsets := map[string][]int{}
	i := 0
	for i+8 <= len(data) {
		size := int(uint32(data[i])<<24 | uint32(data[i+1])<<16 | uint32(data[i+2])<<8 | uint32(data[i+3]))
		typ := string(data[i+4 : i+8])
		offsets[typ] = append(offsets[typ], i)
		if size == 0 {
			// Box extends to end of file.
			i = len(data)
			break
		}
		if size == 1 {
			if i+16 > len(data) {
				return offsets, false
			}
			ext := uint64(0)
			for _, b := range data[i+8 : i+16] {
				ext = ext<<8 | uint64(b)
			}
			if ext < 16 || int(ext) > len(data)-i {
				return offsets, false
			}
			i += int(ext)
		} else {
			if size < 8 || size > len(data)-i {
				return offsets, false
			}
			i += size
		}
	}
	if i != len(data) {
		return offsets, false
	}
	return offsets, true
}

// TestProcessJob_StreamingMp4RealFFmpeg is a behavior-level regression test
// for streaming `-f mp4 -`: it runs a real ffmpeg transcode through processJob
// and asserts the streamed bytes are a fragmented MP4 whose moov box sits
// directly after ftyp (so the file is seekable), and whose duration ffprobe
// can report. It also covers the worker's normalization of the "pipe:1" stdout
// alias to "-". It skips when ffmpeg/ffprobe are not installed.
func TestProcessJob_StreamingMp4RealFFmpeg(t *testing.T) {
	ffmpegPath, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not available")
	}
	ffprobePath, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe not available")
	}

	tmpDir := t.TempDir()

	// Generate a small real input so the transcode exercises the full pipeline.
	inputPath := filepath.Join(tmpDir, "input.mp4")
	gen := exec.Command(ffmpegPath, "-y",
		"-f", "lavfi", "-i", "testsrc=duration=1:size=320x240:rate=25",
		"-c:v", "libx264", "-pix_fmt", "yuv420p", inputPath)
	if out, err := gen.CombinedOutput(); err != nil {
		t.Fatalf("generate input: %v\n%s", err, out)
	}
	inputBytes, err := os.ReadFile(inputPath)
	if err != nil {
		t.Fatalf("read input: %v", err)
	}

	for _, outputFilename := range []string{"", "pipe:1", "PIPE:1"} {
		name := outputFilename
		if name == "" {
			name = "empty"
		}
		t.Run("output_filename="+name, func(t *testing.T) {
			out := runStreamingMp4(t, ffmpegPath, ffprobePath, inputBytes, outputFilename, tmpDir)
			if len(out) == 0 {
				t.Fatal("streaming mp4 produced no stdout bytes")
			}

			// moov must be present and directly follow ftyp: a non-fragmented
			// mp4 written to a pipe has ftyp followed by mdat with no moov.
			offsets, ok := mp4AtomOffsets(out)
			if !ok {
				t.Fatalf("streamed output is not a well-formed MP4 (%d bytes)", len(out))
			}
			ftyp, hasFtyp := offsets["ftyp"]
			moov, hasMoov := offsets["moov"]
			if !hasFtyp || !hasMoov {
				t.Fatalf("streamed MP4 missing ftyp or moov; atom layout=%v", offsets)
			}
			if !(ftyp[0] < moov[0]) {
				t.Fatalf("moov (%d) does not follow ftyp (%d); box layout=%v", moov[0], ftyp[0], offsets)
			}

			// ffprobe must be able to report a duration (seekable / parseable).
			probe := exec.Command(ffprobePath, "-v", "error",
				"-show_entries", "format=duration", "-of", "default=noprint_wrappers=1", "-")
			probe.Stdin = bytes.NewReader(out)
			probeOut, probeErr := probe.Output()
			if probeErr != nil {
				t.Fatalf("ffprobe could not parse streamed MP4: %v", probeErr)
			}
			if !strings.Contains(string(probeOut), "duration=") {
				t.Fatalf("ffprobe reported no duration for streamed MP4: %q", probeOut)
			}
		})
	}
}

// runStreamingMp4 runs one streaming mp4 job through processJob with a real
// ffmpeg executor and returns the reassembled stdout bytes.
func runStreamingMp4(t *testing.T, ffmpegPath, ffprobePath string, inputBytes []byte, outputFilename, tmpDir string) []byte {
	t.Helper()

	srv := newStreamingCaptureServer(inputBytes)
	defer srv.Close()

	workerTempDir := filepath.Join(tmpDir, "worker-temp-"+outputFilename)
	if err := os.MkdirAll(workerTempDir, 0o755); err != nil {
		t.Fatalf("mkdir worker-temp: %v", err)
	}

	cache, err := NewCache(CacheConfig{Enabled: false})
	if err != nil {
		t.Fatalf("NewCache: %v", err)
	}

	executor := NewExecutor(ffmpegPath, 30*time.Second)
	client := NewClient(srv.URL, uuid.New().String(), "")

	rewriteAdapter := NewRewriteAdapter()
	rewriteAdapter.config.Enabled = false

	retryExecutor := NewRetryExecutor(executor, DefaultRetryConfig())

	w := &Worker{
		id:                 "test-worker-real",
		name:               "test-worker-real",
		client:             client,
		executor:           executor,
		retryExecutor:      retryExecutor,
		rewriteAdapter:     rewriteAdapter,
		cache:              cache,
		tempDir:            workerTempDir,
		activeJobs:         make(map[string]context.CancelFunc),
		heartbeatInterval:  30 * time.Second,
		pollInterval:       5 * time.Second,
		lastHeartbeatTime:  time.Now(),
		ffprobeExecutor:    NewFFprobeExecutor(ffprobePath),
		pixelFormatChecker: nil,
		gpuDetector:        gpu.NewDetector(),
	}

	ctx := context.Background()
	jobCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Args mirror the CLI's parser output for `-f mp4 -`: the output token is
	// excluded and StreamingOutput is set; the worker appends the resolved "-".
	job := protocol.JobInfo{
		ID:              "stream-mp4-real",
		InputFiles:      []string{"input-001"},
		Args:            []string{"-i", "<INPUT_FILE>", "-c:v", "libx264", "-pix_fmt", "yuv420p", "-f", "mp4"},
		OutputFilename:  outputFilename,
		StreamingOutput: true,
	}

	w.processJob(jobCtx, job, cancel, false)

	return srv.Output()
}
