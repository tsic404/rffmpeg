package worker

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tsic404/rffmpeg/pkg/pathutil"
	"github.com/tsic404/rffmpeg/pkg/protocol"
	"github.com/tsic404/rffmpeg/pkg/worker/gpu"
)

// TestSpaceUndeclaredHasNoLegalPath locks the fail-closed default: an
// undeclared space does not degrade to "any absolute path", so the pass-through
// mode has no legal input or target at all (ADR-018 option A).
func TestSpaceUndeclaredHasNoLegalPath(t *testing.T) {
	w := &Worker{}
	inside := filepath.Join(t.TempDir(), "in.mp4")
	if v := w.validateDirectInputPath(inside); v == nil || !v.boundary {
		t.Fatalf("validateDirectInputPath(undeclared space) = %v, want a boundary violation", v)
	}
	if v := w.validateDirectOutputPath(inside); v == nil || !v.boundary {
		t.Fatalf("validateDirectOutputPath(undeclared space) = %v, want a boundary violation", v)
	}
}

// TestProcessProbeJob_DirectPathOutsideAllowedPrefixFails verifies the new
// declared path space is enforced on shared-FS probe inputs: a direct path
// outside every declared root must fail with INFRA before any ffprobe
// execution or HTTP input download.
// The allow-list is meant to restore the security boundary documented in the
// README, not merely to filter one job path — so rejection here proves the
// worker-side check, not just the parse helper.
func TestProcessProbeJob_DirectPathOutsideAllowedPrefixFails(t *testing.T) {
	inputPath := filepath.Join(t.TempDir(), "outside.wav")
	if err := writeTestWAV(inputPath); err != nil {
		t.Fatalf("failed to write test media file: %v", err)
	}

	mockSrv := newProbeDirectMockServer()
	defer mockSrv.Close()

	cacheDir := filepath.Join(t.TempDir(), "cache")
	cache, err := NewCache(CacheConfig{Enabled: true, Dir: cacheDir, TTL: 24 * time.Hour})
	if err != nil {
		t.Fatalf("NewCache failed: %v", err)
	}
	t.Cleanup(func() { cache.Stop() })

	workerTempDir := filepath.Join(t.TempDir(), "worker-temp")
	if err := os.MkdirAll(workerTempDir, 0755); err != nil {
		t.Fatalf("mkdir worker-temp: %v", err)
	}

	client := NewClient(mockSrv.URL, "test-worker-001", "")
	w := &Worker{
		id:                "test-worker-001",
		name:              "test-worker",
		client:            client,
		executor:          NewExecutor("ffmpeg", 30*time.Second),
		retryExecutor:     NewRetryExecutor(NewExecutor("ffmpeg", 30*time.Second), DefaultRetryConfig()),
		rewriteAdapter:    NewRewriteAdapter(),
		cache:             cache,
		tempDir:           workerTempDir,
		activeJobs:        make(map[string]context.CancelFunc),
		heartbeatInterval: 30 * time.Second,
		pollInterval:      5 * time.Second,
		lastHeartbeatTime: time.Now(),
		ffprobeExecutor:   NewFFprobeExecutor("/usr/bin/ffprobe"),
		gpuDetector:       gpu.NewDetector(),
		// Allow-list pins probes to /mnt/nfs; the input lives in a temp dir.
		sharedFS: pathutil.NewSpace("/mnt/nfs"),
	}

	job := protocol.JobInfo{
		ID:          "test-probe-direct-outside-001",
		InputFiles:  []string{inputPath},
		Args:        []string{"__rffmpeg_probe__", inputPath},
		DirectPaths: []string{inputPath},
	}

	w.processProbeJob(context.Background(), job)

	if atomic.LoadInt32(&mockSrv.downloadCount) != 0 {
		t.Errorf("rejected direct-path probe attempted %d HTTP input download(s), want 0",
			atomic.LoadInt32(&mockSrv.downloadCount))
	}
	if atomic.LoadInt32(&mockSrv.terminalCount) == 0 {
		t.Fatal("expected a terminal job update PATCH, got none")
	}

	mockSrv.mu.Lock()
	body := mockSrv.terminalBody
	mockSrv.mu.Unlock()

	var update protocol.JobUpdateRequest
	if err := json.Unmarshal(body, &update); err != nil {
		t.Fatalf("failed to parse terminal update body: %v\nbody: %s", err, body)
	}
	if update.Status != protocol.JobStatusFailed {
		t.Errorf("Status = %q, want %q (path outside allow-list must fail)", update.Status, protocol.JobStatusFailed)
	}
	if update.FailureType != string(protocol.FailureInfra) {
		t.Errorf("FailureType = %q, want %q", update.FailureType, string(protocol.FailureInfra))
	}
	if update.Error == "" {
		t.Error("Error = empty, want a prefix-rejection message")
	}
}

func TestValidateDirectInputPathRejectsSymlinkEscape(t *testing.T) {
	base := t.TempDir()
	inside := filepath.Join(base, "media")
	outside := filepath.Join(base, "secret")
	if err := os.MkdirAll(inside, 0755); err != nil {
		t.Fatalf("mkdir inside: %v", err)
	}
	if err := os.MkdirAll(outside, 0755); err != nil {
		t.Fatalf("mkdir outside: %v", err)
	}
	real := filepath.Join(outside, "movie.mp4")
	if err := os.WriteFile(real, []byte("x"), 0644); err != nil {
		t.Fatalf("write real file: %v", err)
	}
	link := filepath.Join(inside, "movie.mp4")
	if err := os.Symlink(real, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	w := &Worker{sharedFS: pathutil.NewSpace(inside)}
	v := w.validateDirectInputPath(link)
	if v == nil {
		t.Fatal("validateDirectInputPath allowed a symlink escape, want violation")
	}
	if !v.boundary {
		t.Errorf("boundary = false, want true (resolved path left the declared space)")
	}
	if !strings.Contains(v.details, "outside the declared shared filesystem space") {
		t.Errorf("details = %q, want an out-of-space judgement", v.details)
	}
}

func TestValidateDirectOutputPathRejectsSymlinkEscape(t *testing.T) {
	base := t.TempDir()
	inside := filepath.Join(base, "media")
	outside := filepath.Join(base, "secret")
	if err := os.MkdirAll(inside, 0755); err != nil {
		t.Fatalf("mkdir inside: %v", err)
	}
	if err := os.MkdirAll(outside, 0755); err != nil {
		t.Fatalf("mkdir outside: %v", err)
	}
	// Symlinked parent: output does not exist yet, but its parent resolves
	// outside the allowed prefix.
	linkParent := filepath.Join(inside, "linkdir")
	if err := os.Symlink(outside, linkParent); err != nil {
		t.Fatalf("symlink parent: %v", err)
	}
	output := filepath.Join(linkParent, "out.mp4")

	w := &Worker{sharedFS: pathutil.NewSpace(inside)}
	v := w.validateDirectOutputPath(output)
	if v == nil {
		t.Fatal("validateDirectOutputPath allowed a symlink-escaped parent, want violation")
	}
	if !v.boundary {
		t.Errorf("boundary = false, want true (resolved path left the declared space)")
	}
	if !strings.Contains(v.details, "outside the declared shared filesystem space") {
		t.Errorf("details = %q, want an out-of-space judgement", v.details)
	}
}

func TestValidateDirectOutputPathParentDirDoesNotExist(t *testing.T) {
	base := t.TempDir()
	allowed := filepath.Join(base, "allowed")
	if err := os.MkdirAll(allowed, 0755); err != nil {
		t.Fatalf("mkdir allowed: %v", err)
	}
	output := filepath.Join(allowed, "missing", "out.mp4")

	w := &Worker{sharedFS: pathutil.NewSpace(allowed)}
	v := w.validateDirectOutputPath(output)
	if v == nil {
		t.Fatal("validateDirectOutputPath allowed a path whose parent does not exist, want violation")
	}
	if v.details != "parent directory does not exist" {
		t.Errorf("details = %q, want %q", v.details, "parent directory does not exist")
	}
	wantParent := filepath.Join(allowed, "missing")
	if !strings.Contains(v.msg, wantParent) {
		t.Errorf("msg = %q, want mention of parent directory %q", v.msg, wantParent)
	}
}

func TestValidateDirectOutputPathRejectsBrokenSymlink(t *testing.T) {
	base := t.TempDir()
	allowed := filepath.Join(base, "allowed")
	if err := os.MkdirAll(allowed, 0755); err != nil {
		t.Fatalf("mkdir allowed: %v", err)
	}
	output := filepath.Join(allowed, "broken.mp4")
	if err := os.Symlink(filepath.Join(allowed, "missing-target.mp4"), output); err != nil {
		t.Fatalf("symlink broken: %v", err)
	}

	w := &Worker{sharedFS: pathutil.NewSpace(allowed)}
	v := w.validateDirectOutputPath(output)
	if v == nil {
		t.Fatal("validateDirectOutputPath allowed a broken-symlink output, want violation")
	}
	if v.details != "output path is a broken symlink" {
		t.Errorf("details = %q, want %q", v.details, "output path is a broken symlink")
	}
	if !strings.Contains(v.msg, output) {
		t.Errorf("msg = %q, want mention of output path %q", v.msg, output)
	}
}

func TestValidateDirectInputPathRejectsRelative(t *testing.T) {
	w := &Worker{sharedFS: pathutil.NewSpace("relative/prefix")}
	v := w.validateDirectInputPath("relative/prefix/file.mp4")
	if v == nil {
		t.Fatal("validateDirectInputPath allowed a relative path, want violation")
	}
	if !v.boundary {
		t.Error("a relative pass-through path is a path-space violation, want boundary=true")
	}
}

func TestValidateDirectOutputPathRejectsRelative(t *testing.T) {
	w := &Worker{sharedFS: pathutil.NewSpace("relative/prefix")}
	v := w.validateDirectOutputPath("relative/prefix/out.mp4")
	if v == nil {
		t.Fatal("validateDirectOutputPath allowed a relative path, want violation")
	}
	if !v.boundary {
		t.Error("a relative pass-through target is a path-space violation, want boundary=true")
	}
}

func TestProcessJob_DirectOutputOutsideAllowedPrefixFails(t *testing.T) {
	base := t.TempDir()
	allowed := filepath.Join(base, "allowed")
	if err := os.MkdirAll(allowed, 0755); err != nil {
		t.Fatalf("mkdir allowed: %v", err)
	}
	inputPath := filepath.Join(allowed, "input.mp4")
	if err := os.WriteFile(inputPath, []byte("x"), 0644); err != nil {
		t.Fatalf("write input: %v", err)
	}
	outside := filepath.Join(base, "outside.mp4")

	mockSrv := newProbeDirectMockServer()
	defer mockSrv.Close()

	cacheDir := filepath.Join(t.TempDir(), "cache")
	cache, err := NewCache(CacheConfig{Enabled: true, Dir: cacheDir, TTL: 24 * time.Hour})
	if err != nil {
		t.Fatalf("NewCache failed: %v", err)
	}
	t.Cleanup(func() { cache.Stop() })

	workerTempDir := filepath.Join(t.TempDir(), "worker-temp")
	if err := os.MkdirAll(workerTempDir, 0755); err != nil {
		t.Fatalf("mkdir worker-temp: %v", err)
	}

	client := NewClient(mockSrv.URL, "test-worker-001", "")
	w := &Worker{
		id:                "test-worker-001",
		name:              "test-worker",
		client:            client,
		executor:          NewExecutor("ffmpeg", 30*time.Second),
		retryExecutor:     NewRetryExecutor(NewExecutor("ffmpeg", 30*time.Second), DefaultRetryConfig()),
		rewriteAdapter:    NewRewriteAdapter(),
		cache:             cache,
		tempDir:           workerTempDir,
		activeJobs:        make(map[string]context.CancelFunc),
		heartbeatInterval: 30 * time.Second,
		pollInterval:      5 * time.Second,
		lastHeartbeatTime: time.Now(),
		ffprobeExecutor:   NewFFprobeExecutor("/usr/bin/ffprobe"),
		gpuDetector:       gpu.NewDetector(),
		sharedFS:          pathutil.NewSpace(allowed),
	}

	job := protocol.JobInfo{
		ID:             "test-job-direct-outside-output-001",
		InputFiles:     []string{inputPath},
		Args:           []string{"-i", "<INPUT_FILE>", "-c:v", "copy"},
		OutputFilename: outside,
		DirectPaths:    []string{inputPath},
	}

	w.processJob(context.Background(), job, func() {}, false)

	if atomic.LoadInt32(&mockSrv.terminalCount) == 0 {
		t.Fatal("expected a terminal job update PATCH, got none")
	}

	mockSrv.mu.Lock()
	body := mockSrv.terminalBody
	mockSrv.mu.Unlock()

	var update protocol.JobUpdateRequest
	if err := json.Unmarshal(body, &update); err != nil {
		t.Fatalf("failed to parse terminal update body: %v\nbody: %s", err, body)
	}
	if update.Status != protocol.JobStatusFailed {
		t.Errorf("Status = %q, want %q", update.Status, protocol.JobStatusFailed)
	}
	if update.FailureType != string(protocol.FailureInfra) {
		t.Errorf("FailureType = %q, want %q", update.FailureType, string(protocol.FailureInfra))
	}
	if !strings.Contains(update.Error, outside) {
		t.Errorf("Error = %q, want mention of output path %q", update.Error, outside)
	}
}

// TestProcessJob_DirectOutputFileURLRejected proves the output-side
// fix: a direct-mode "file://" output is a local ffmpeg protocol path, not a
// remote URL, so it must fall through to direct-output validation and be
// rejected as non-absolute instead of being passed to ffmpeg verbatim. Before
// the fix, pathutil.IsRemoteURL matched "file://" and the allow-list gate was skipped,
// letting ffmpeg's native file protocol write to /etc/passwd.
func TestProcessJob_DirectOutputFileURLRejected(t *testing.T) {
	base := t.TempDir()
	allowed := filepath.Join(base, "allowed")
	if err := os.MkdirAll(allowed, 0755); err != nil {
		t.Fatalf("mkdir allowed: %v", err)
	}
	inputPath := filepath.Join(allowed, "input.mp4")
	if err := os.WriteFile(inputPath, []byte("x"), 0644); err != nil {
		t.Fatalf("write input: %v", err)
	}
	outputPath := "file:///etc/passwd"

	mockSrv := newProbeDirectMockServer()
	defer mockSrv.Close()

	cacheDir := filepath.Join(t.TempDir(), "cache")
	cache, err := NewCache(CacheConfig{Enabled: true, Dir: cacheDir, TTL: 24 * time.Hour})
	if err != nil {
		t.Fatalf("NewCache failed: %v", err)
	}
	t.Cleanup(func() { cache.Stop() })

	workerTempDir := filepath.Join(t.TempDir(), "worker-temp")
	if err := os.MkdirAll(workerTempDir, 0755); err != nil {
		t.Fatalf("mkdir worker-temp: %v", err)
	}

	client := NewClient(mockSrv.URL, "test-worker-001", "")
	w := &Worker{
		id:                "test-worker-001",
		name:              "test-worker",
		client:            client,
		executor:          NewExecutor("ffmpeg", 30*time.Second),
		retryExecutor:     NewRetryExecutor(NewExecutor("ffmpeg", 30*time.Second), DefaultRetryConfig()),
		rewriteAdapter:    NewRewriteAdapter(),
		cache:             cache,
		tempDir:           workerTempDir,
		activeJobs:        make(map[string]context.CancelFunc),
		heartbeatInterval: 30 * time.Second,
		pollInterval:      5 * time.Second,
		lastHeartbeatTime: time.Now(),
		ffprobeExecutor:   NewFFprobeExecutor("/usr/bin/ffprobe"),
		gpuDetector:       gpu.NewDetector(),
		sharedFS:          pathutil.NewSpace(allowed),
	}

	job := protocol.JobInfo{
		ID:             "test-job-direct-file-url-output-001",
		InputFiles:     []string{inputPath},
		Args:           []string{"-i", "<INPUT_FILE>", "-c:v", "copy"},
		OutputFilename: outputPath,
		DirectPaths:    []string{inputPath},
	}

	w.processJob(context.Background(), job, func() {}, false)

	if atomic.LoadInt32(&mockSrv.terminalCount) == 0 {
		t.Fatal("expected a terminal job update PATCH, got none")
	}

	mockSrv.mu.Lock()
	body := mockSrv.terminalBody
	mockSrv.mu.Unlock()

	var update protocol.JobUpdateRequest
	if err := json.Unmarshal(body, &update); err != nil {
		t.Fatalf("failed to parse terminal update body: %v\nbody: %s", err, body)
	}
	if update.Status != protocol.JobStatusFailed {
		t.Errorf("Status = %q, want %q", update.Status, protocol.JobStatusFailed)
	}
	if update.FailureType != string(protocol.FailureInfra) {
		t.Errorf("FailureType = %q, want %q", update.FailureType, string(protocol.FailureInfra))
	}
	if !strings.Contains(update.Error, outputPath) {
		t.Errorf("Error = %q, want mention of output path %q", update.Error, outputPath)
	}
}

// TestProcessProbeJob_DirectInputInfixURLRejected proves the fix on
// the input side: an absolute shared-FS path containing "://" mid-string must
// be validated as a local path (and fail closed) instead of being mistaken for
// a remote URL and fetched over HTTP. downloadCount == 0 is the load-bearing
// assertion — the HTTP branch would have sent the absolute path to
// GET /api/v1/files/<path>.
func TestProcessProbeJob_DirectInputInfixURLRejected(t *testing.T) {
	allowed := filepath.Join(t.TempDir(), "allowed")
	if err := os.MkdirAll(allowed, 0755); err != nil {
		t.Fatalf("mkdir allowed: %v", err)
	}
	// Nonexistent on purpose: validation must reject before any ffprobe run.
	inputPath := filepath.Join(allowed, "x://evil")

	mockSrv := newProbeDirectMockServer()
	defer mockSrv.Close()

	cacheDir := filepath.Join(t.TempDir(), "cache")
	cache, err := NewCache(CacheConfig{Enabled: true, Dir: cacheDir, TTL: 24 * time.Hour})
	if err != nil {
		t.Fatalf("NewCache failed: %v", err)
	}
	t.Cleanup(func() { cache.Stop() })

	workerTempDir := filepath.Join(t.TempDir(), "worker-temp")
	if err := os.MkdirAll(workerTempDir, 0755); err != nil {
		t.Fatalf("mkdir worker-temp: %v", err)
	}

	client := NewClient(mockSrv.URL, "test-worker-001", "")
	w := &Worker{
		id:                "test-worker-001",
		name:              "test-worker",
		client:            client,
		executor:          NewExecutor("ffmpeg", 30*time.Second),
		retryExecutor:     NewRetryExecutor(NewExecutor("ffmpeg", 30*time.Second), DefaultRetryConfig()),
		rewriteAdapter:    NewRewriteAdapter(),
		cache:             cache,
		tempDir:           workerTempDir,
		activeJobs:        make(map[string]context.CancelFunc),
		heartbeatInterval: 30 * time.Second,
		pollInterval:      5 * time.Second,
		lastHeartbeatTime: time.Now(),
		ffprobeExecutor:   NewFFprobeExecutor("/usr/bin/ffprobe"),
		gpuDetector:       gpu.NewDetector(),
		sharedFS:          pathutil.NewSpace(allowed),
	}

	job := protocol.JobInfo{
		ID:          "test-probe-direct-infix-url-001",
		InputFiles:  []string{inputPath},
		Args:        []string{"__rffmpeg_probe__", inputPath},
		DirectPaths: []string{inputPath},
	}

	w.processProbeJob(context.Background(), job)

	if atomic.LoadInt32(&mockSrv.downloadCount) != 0 {
		t.Errorf("infix-URL direct-path probe attempted %d HTTP input download(s), want 0",
			atomic.LoadInt32(&mockSrv.downloadCount))
	}
	if atomic.LoadInt32(&mockSrv.terminalCount) == 0 {
		t.Fatal("expected a terminal job update PATCH, got none")
	}

	mockSrv.mu.Lock()
	body := mockSrv.terminalBody
	mockSrv.mu.Unlock()

	var update protocol.JobUpdateRequest
	if err := json.Unmarshal(body, &update); err != nil {
		t.Fatalf("failed to parse terminal update body: %v\nbody: %s", err, body)
	}
	if update.Status != protocol.JobStatusFailed {
		t.Errorf("Status = %q, want %q (infix URL path must fail closed)", update.Status, protocol.JobStatusFailed)
	}
	if update.FailureType != string(protocol.FailureInputUnreachable) {
		t.Errorf("FailureType = %q, want %q", update.FailureType, string(protocol.FailureInputUnreachable))
	}
}

// TestProcessJob_DirectOutputInfixURLRejected proves the fix on the
// output side: an absolute shared-FS output path containing "://" mid-string
// must be validated as a local path (and fail closed) instead of being passed
// straight through to ffmpeg as a remote URL. The error must mention the
// output's parent directory to prove it was rejected locally, not executed by ffmpeg.
func TestProcessJob_DirectOutputInfixURLRejected(t *testing.T) {
	base := t.TempDir()
	allowed := filepath.Join(base, "allowed")
	if err := os.MkdirAll(allowed, 0755); err != nil {
		t.Fatalf("mkdir allowed: %v", err)
	}
	inputPath := filepath.Join(allowed, "input.mp4")
	if err := os.WriteFile(inputPath, []byte("x"), 0644); err != nil {
		t.Fatalf("write input: %v", err)
	}
	outputPath := filepath.Join(allowed, "x://out.mp4")

	mockSrv := newProbeDirectMockServer()
	defer mockSrv.Close()

	cacheDir := filepath.Join(t.TempDir(), "cache")
	cache, err := NewCache(CacheConfig{Enabled: true, Dir: cacheDir, TTL: 24 * time.Hour})
	if err != nil {
		t.Fatalf("NewCache failed: %v", err)
	}
	t.Cleanup(func() { cache.Stop() })

	workerTempDir := filepath.Join(t.TempDir(), "worker-temp")
	if err := os.MkdirAll(workerTempDir, 0755); err != nil {
		t.Fatalf("mkdir worker-temp: %v", err)
	}

	client := NewClient(mockSrv.URL, "test-worker-001", "")
	w := &Worker{
		id:                "test-worker-001",
		name:              "test-worker",
		client:            client,
		executor:          NewExecutor("ffmpeg", 30*time.Second),
		retryExecutor:     NewRetryExecutor(NewExecutor("ffmpeg", 30*time.Second), DefaultRetryConfig()),
		rewriteAdapter:    NewRewriteAdapter(),
		cache:             cache,
		tempDir:           workerTempDir,
		activeJobs:        make(map[string]context.CancelFunc),
		heartbeatInterval: 30 * time.Second,
		pollInterval:      5 * time.Second,
		lastHeartbeatTime: time.Now(),
		ffprobeExecutor:   NewFFprobeExecutor("/usr/bin/ffprobe"),
		gpuDetector:       gpu.NewDetector(),
		sharedFS:          pathutil.NewSpace(allowed),
	}

	job := protocol.JobInfo{
		ID:             "test-job-direct-infix-url-output-001",
		InputFiles:     []string{inputPath},
		Args:           []string{"-i", "<INPUT_FILE>", "-c:v", "copy"},
		OutputFilename: outputPath,
		DirectPaths:    []string{inputPath},
	}

	w.processJob(context.Background(), job, func() {}, false)

	if atomic.LoadInt32(&mockSrv.terminalCount) == 0 {
		t.Fatal("expected a terminal job update PATCH, got none")
	}

	mockSrv.mu.Lock()
	body := mockSrv.terminalBody
	mockSrv.mu.Unlock()

	var update protocol.JobUpdateRequest
	if err := json.Unmarshal(body, &update); err != nil {
		t.Fatalf("failed to parse terminal update body: %v\nbody: %s", err, body)
	}
	if update.Status != protocol.JobStatusFailed {
		t.Errorf("Status = %q, want %q (infix URL output path must fail closed)", update.Status, protocol.JobStatusFailed)
	}
	if update.FailureType != string(protocol.FailureInfra) {
		t.Errorf("FailureType = %q, want %q", update.FailureType, string(protocol.FailureInfra))
	}
	if !strings.Contains(update.Error, filepath.Dir(outputPath)) {
		t.Errorf("Error = %q, want mention of output parent directory %q", update.Error, filepath.Dir(outputPath))
	}
}

// TestProcessJob_DirectOutputTraversalRejected proves the defense in
// depth: an output path containing a ".." component must be rejected before
// the allow-list check, so the check never depends on the path not existing.
func TestProcessJob_DirectOutputTraversalRejected(t *testing.T) {
	base := t.TempDir()
	allowed := filepath.Join(base, "allowed")
	if err := os.MkdirAll(allowed, 0755); err != nil {
		t.Fatalf("mkdir allowed: %v", err)
	}
	inputPath := filepath.Join(allowed, "input.mp4")
	if err := os.WriteFile(inputPath, []byte("x"), 0644); err != nil {
		t.Fatalf("write input: %v", err)
	}
	// Build the traversal path with string concatenation: filepath.Join
	// would clean the ".." components away before they reach validation.
	outputPath := allowed + string(os.PathSeparator) + ".." + string(os.PathSeparator) + "escape.mp4"

	mockSrv := newProbeDirectMockServer()
	defer mockSrv.Close()

	cacheDir := filepath.Join(t.TempDir(), "cache")
	cache, err := NewCache(CacheConfig{Enabled: true, Dir: cacheDir, TTL: 24 * time.Hour})
	if err != nil {
		t.Fatalf("NewCache failed: %v", err)
	}
	t.Cleanup(func() { cache.Stop() })

	workerTempDir := filepath.Join(t.TempDir(), "worker-temp")
	if err := os.MkdirAll(workerTempDir, 0755); err != nil {
		t.Fatalf("mkdir worker-temp: %v", err)
	}

	client := NewClient(mockSrv.URL, "test-worker-001", "")
	w := &Worker{
		id:                "test-worker-001",
		name:              "test-worker",
		client:            client,
		executor:          NewExecutor("ffmpeg", 30*time.Second),
		retryExecutor:     NewRetryExecutor(NewExecutor("ffmpeg", 30*time.Second), DefaultRetryConfig()),
		rewriteAdapter:    NewRewriteAdapter(),
		cache:             cache,
		tempDir:           workerTempDir,
		activeJobs:        make(map[string]context.CancelFunc),
		heartbeatInterval: 30 * time.Second,
		pollInterval:      5 * time.Second,
		lastHeartbeatTime: time.Now(),
		ffprobeExecutor:   NewFFprobeExecutor("/usr/bin/ffprobe"),
		gpuDetector:       gpu.NewDetector(),
		sharedFS:          pathutil.NewSpace(allowed),
	}

	job := protocol.JobInfo{
		ID:             "test-job-direct-traversal-output-001",
		InputFiles:     []string{inputPath},
		Args:           []string{"-i", "<INPUT_FILE>", "-c:v", "copy"},
		OutputFilename: outputPath,
		DirectPaths:    []string{inputPath},
	}

	w.processJob(context.Background(), job, func() {}, false)

	if atomic.LoadInt32(&mockSrv.terminalCount) == 0 {
		t.Fatal("expected a terminal job update PATCH, got none")
	}

	mockSrv.mu.Lock()
	body := mockSrv.terminalBody
	mockSrv.mu.Unlock()

	var update protocol.JobUpdateRequest
	if err := json.Unmarshal(body, &update); err != nil {
		t.Fatalf("failed to parse terminal update body: %v\nbody: %s", err, body)
	}
	if update.Status != protocol.JobStatusFailed {
		t.Errorf("Status = %q, want %q", update.Status, protocol.JobStatusFailed)
	}
	if update.FailureType != string(protocol.FailureInfra) {
		t.Errorf("FailureType = %q, want %q", update.FailureType, string(protocol.FailureInfra))
	}
	if !strings.Contains(update.Error, "'..' component") {
		t.Errorf("Error = %q, want an upward-segment rejection", update.Error)
	}
}
