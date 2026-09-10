package worker

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestBuildArgsOverwriteSemantics verifies the TSI-2964 fix end-to-end against
// real ffmpeg: BuildArgs must not inject -y, so a pre-existing output file
// follows native ffmpeg overwrite semantics.
//
//   - explicit -y: overwrites the existing output (job succeeds)
//   - no -y / -n:  refuses to overwrite ("Not overwriting - exiting")
//   - explicit -n: refuses to overwrite ("already exists")
func TestBuildArgsOverwriteSemantics(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}

	dir := t.TempDir()
	inputPath := filepath.Join(dir, "in.mp4")
	outputPath := filepath.Join(dir, "out.mp4")

	// Generate a tiny valid input so ffmpeg has something real to transcode.
	gen := exec.Command("ffmpeg", "-y",
		"-f", "lavfi", "-i", "testsrc=duration=0.1:size=64x64:rate=10",
		"-c:v", "libx264", "-preset", "ultrafast", inputPath)
	if out, err := gen.CombinedOutput(); err != nil {
		t.Fatalf("failed to generate input: %v\n%s", err, out)
	}

	const staleContent = "existing content"
	writeStale := func(t *testing.T) {
		t.Helper()
		if err := os.WriteFile(outputPath, []byte(staleContent), 0o644); err != nil {
			t.Fatalf("failed to write stale output: %v", err)
		}
	}
	readOutput := func(t *testing.T) string {
		t.Helper()
		b, err := os.ReadFile(outputPath)
		if err != nil {
			t.Fatalf("failed to read output: %v", err)
		}
		return string(b)
	}

	executor := NewExecutor("ffmpeg", time.Minute)
	run := func(t *testing.T, jobArgs []string) ExecResult {
		t.Helper()
		args := BuildArgs(jobArgs, []string{inputPath}, outputPath)
		return executor.Execute(context.Background(), args)
	}

	t.Run("explicit -y overwrites", func(t *testing.T) {
		writeStale(t)
		res := run(t, []string{"-y", "-i", "<INPUT_FILE>", "-c:v", "libx264", "-preset", "ultrafast"})
		if res.ExitCode != 0 {
			t.Fatalf("expected overwrite to succeed, got exit=%d stderr=%q", res.ExitCode, res.Stderr)
		}
		if got := readOutput(t); got == staleContent {
			t.Fatalf("output was not overwritten: still %q", got)
		}
	})

	t.Run("no flag rejects", func(t *testing.T) {
		writeStale(t)
		res := run(t, []string{"-i", "<INPUT_FILE>", "-c:v", "libx264", "-preset", "ultrafast"})
		if !strings.Contains(res.Stderr, "Not overwriting") {
			t.Errorf("expected 'Not overwriting' in stderr, got %q", res.Stderr)
		}
		// ffmpeg 8.x/9.x exits 0 for "Not overwriting - exiting" (stderr-only
		// refusal), so the raw exit code is not a rejection signal. The worker
		// rejects the job via stderr classification instead — assert that
		// classifier fires, so the refusal is never reported as success.
		if !ffmpegStderrIndicatesCriticalError(res.Stderr) {
			t.Errorf("worker must classify 'Not overwriting' as a job failure, stderr=%q", res.Stderr)
		}
		if got := readOutput(t); got != staleContent {
			t.Errorf("existing output was modified: %q", got)
		}
	})

	t.Run("explicit -n rejects", func(t *testing.T) {
		writeStale(t)
		res := run(t, []string{"-n", "-i", "<INPUT_FILE>", "-c:v", "libx264", "-preset", "ultrafast"})
		if !strings.Contains(res.Stderr, "already exists") {
			t.Errorf("expected 'already exists' in stderr, got %q", res.Stderr)
		}
		if !ffmpegStderrIndicatesCriticalError(res.Stderr) {
			t.Errorf("worker must classify '-n already exists' as a job failure, stderr=%q", res.Stderr)
		}
		if got := readOutput(t); got != staleContent {
			t.Errorf("existing output was modified: %q", got)
		}
	})
}
