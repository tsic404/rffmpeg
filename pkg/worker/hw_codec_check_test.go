package worker

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/tsic404/rffmpeg/pkg/protocol"
)

func TestHardwareCodecUnsupportedReason(t *testing.T) {
	tests := []struct {
		name   string
		stderr string
		want   string
	}{
		{
			name:   "qsv runtime cannot encode codec",
			stderr: "[av1_qsv @ 0x55] This version of runtime doesn't support AV1 encoding",
			want:   "[av1_qsv @ 0x55] This version of runtime doesn't support AV1 encoding",
		},
		{
			name:   "expanded contraction form",
			stderr: "[av1_qsv @ 0x55] This version of runtime does not support AV1 encoding",
			want:   "[av1_qsv @ 0x55] This version of runtime does not support AV1 encoding",
		},
		{
			// vp9_qsv's default ratecontrol is a parameter problem the real
			// job may still satisfy — it must not be treated as a codec gap.
			name:   "parameter-only probe failure",
			stderr: "[vp9_qsv @ 0x55] Selected ratecontrol mode is unsupported\n[vp9_qsv @ 0x55] Current frame rate is unsupported",
			want:   "",
		},
		{
			name:   "device creation failure",
			stderr: "[AVHWDeviceContext @ 0x55] Device creation failed: -9.\nMFX session: -9",
			want:   "",
		},
		{
			name:   "empty stderr",
			stderr: "",
			want:   "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hardwareCodecUnsupportedReason(tt.stderr); got != tt.want {
				t.Errorf("hardwareCodecUnsupportedReason(%q) = %q, want %q", tt.stderr, got, tt.want)
			}
		})
	}
}

// writeMockProbeFFmpeg writes an executable standing in for ffmpeg that
// appends every invocation's arguments to logPath. Invocations carrying
// nullsrc (the capability probe) print stderrLines and exit probeExit; all
// other invocations (the real transcode) exit 0.
func writeMockProbeFFmpeg(t *testing.T, dir, logPath string, probeExit int, stderrLines ...string) string {
	t.Helper()

	var script strings.Builder
	script.WriteString("#!/bin/bash\n")
	fmt.Fprintf(&script, "printf '%%s\\n' \"$*\" >> %s\n", shellSingleQuote(logPath))
	script.WriteString("case \"$*\" in\n")
	script.WriteString("  *nullsrc*)\n")
	for _, line := range stderrLines {
		fmt.Fprintf(&script, "    printf '%%s\\n' %s >&2\n", shellSingleQuote(line))
	}
	fmt.Fprintf(&script, "    exit %d\n", probeExit)
	script.WriteString("    ;;\n")
	script.WriteString("esac\n")
	script.WriteString("exit 0\n")

	path := filepath.Join(dir, "mock-ffmpeg")
	if err := os.WriteFile(path, []byte(script.String()), 0755); err != nil {
		t.Fatalf("write mock ffmpeg: %v", err)
	}
	return path
}

// writeStatefulProbeFFmpeg writes an executable that behaves differently on
// each invocation (keyed by how many times it has run), so tests can model a
// transient probe failure followed by a real capability gap.
func writeStatefulProbeFFmpeg(t *testing.T, dir, logPath string, exits []int, stderrLines [][]string) string {
	t.Helper()
	if len(exits) != len(stderrLines) {
		t.Fatalf("writeStatefulProbeFFmpeg: exits/stderrLines length mismatch")
	}

	var script strings.Builder
	script.WriteString("#!/bin/bash\n")
	fmt.Fprintf(&script, "LOG=%s\n", shellSingleQuote(logPath))
	script.WriteString("n=0; [ -f \"$LOG\" ] && n=$(wc -l < \"$LOG\")\n")
	script.WriteString("printf '%s\\n' \"$*\" >> \"$LOG\"\n")
	script.WriteString("case $((n+1)) in\n")
	for i, exit := range exits {
		fmt.Fprintf(&script, "  %d)\n", i+1)
		for _, line := range stderrLines[i] {
			fmt.Fprintf(&script, "    printf '%%s\\n' %s >&2\n", shellSingleQuote(line))
		}
		fmt.Fprintf(&script, "    exit %d\n", exit)
		script.WriteString("    ;;\n")
	}
	fmt.Fprintf(&script, "  *) exit %d\n", exits[len(exits)-1])
	script.WriteString("esac\n")

	path := filepath.Join(dir, "mock-ffmpeg")
	if err := os.WriteFile(path, []byte(script.String()), 0755); err != nil {
		t.Fatalf("write mock ffmpeg: %v", err)
	}
	return path
}

// readInvocationLog returns the recorded ffmpeg invocations, one per line.
func readInvocationLog(t *testing.T, logPath string) []string {
	t.Helper()
	data, err := os.ReadFile(logPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read invocation log: %v", err)
	}
	var lines []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

func TestHardwareCodecCheckerCheck(t *testing.T) {
	t.Run("software encoder never probes", func(t *testing.T) {
		dir := t.TempDir()
		logPath := filepath.Join(dir, "invocations.log")
		ffmpegPath := writeMockProbeFFmpeg(t, dir, logPath, 171, "This version of runtime doesn't support AV1 encoding")

		c := NewHardwareCodecChecker(ffmpegPath)
		if unsupported, reason := c.Check(context.Background(), "libx264"); unsupported || reason != "" {
			t.Fatalf("Check(libx264) = (%v, %q), want (false, \"\")", unsupported, reason)
		}
		if got := readInvocationLog(t, logPath); len(got) != 0 {
			t.Fatalf("software encoder must not invoke ffmpeg, got %v", got)
		}
	})

	t.Run("unsupported codec is reported", func(t *testing.T) {
		dir := t.TempDir()
		logPath := filepath.Join(dir, "invocations.log")
		want := "[av1_qsv @ 0x55] This version of runtime doesn't support AV1 encoding"
		ffmpegPath := writeMockProbeFFmpeg(t, dir, logPath, 171, want)

		c := NewHardwareCodecChecker(ffmpegPath)
		unsupported, reason := c.Check(context.Background(), "av1_qsv")
		if !unsupported {
			t.Fatalf("Check(av1_qsv) unsupported = false, want true (reason=%q)", reason)
		}
		if reason != want {
			t.Errorf("Check(av1_qsv) reason = %q, want %q", reason, want)
		}
	})

	t.Run("parameter-only probe failure is inconclusive", func(t *testing.T) {
		dir := t.TempDir()
		logPath := filepath.Join(dir, "invocations.log")
		ffmpegPath := writeMockProbeFFmpeg(t, dir, logPath, 218,
			"[vp9_qsv @ 0x55] Selected ratecontrol mode is unsupported")

		c := NewHardwareCodecChecker(ffmpegPath)
		if unsupported, reason := c.Check(context.Background(), "vp9_qsv"); unsupported || reason != "" {
			t.Fatalf("Check(vp9_qsv) = (%v, %q), want (false, \"\")", unsupported, reason)
		}
	})

	t.Run("conclusive unsupported verdict is cached", func(t *testing.T) {
		dir := t.TempDir()
		logPath := filepath.Join(dir, "invocations.log")
		ffmpegPath := writeMockProbeFFmpeg(t, dir, logPath, 171,
			"This version of runtime doesn't support AV1 encoding")

		c := NewHardwareCodecChecker(ffmpegPath)
		for i := range 3 {
			if unsupported, _ := c.Check(context.Background(), "av1_qsv"); !unsupported {
				t.Fatalf("call %d: unsupported = false, want true", i+1)
			}
		}
		if got := readInvocationLog(t, logPath); len(got) != 1 {
			t.Fatalf("probe invoked %d times, want 1 (cached)", len(got))
		}
	})

	t.Run("successful probe is cached", func(t *testing.T) {
		dir := t.TempDir()
		logPath := filepath.Join(dir, "invocations.log")
		ffmpegPath := writeMockProbeFFmpeg(t, dir, logPath, 0)

		c := NewHardwareCodecChecker(ffmpegPath)
		for i := range 3 {
			if unsupported, _ := c.Check(context.Background(), "h264_qsv"); unsupported {
				t.Fatalf("call %d: unsupported = true, want false", i+1)
			}
		}
		if got := readInvocationLog(t, logPath); len(got) != 1 {
			t.Fatalf("probe invoked %d times, want 1 (supported verdict cached)", len(got))
		}
	})

	t.Run("inconclusive verdict is not cached", func(t *testing.T) {
		dir := t.TempDir()
		logPath := filepath.Join(dir, "invocations.log")
		ffmpegPath := writeMockProbeFFmpeg(t, dir, logPath, 1, "Device creation failed: -9.")

		c := NewHardwareCodecChecker(ffmpegPath)
		for i := range 2 {
			if unsupported, _ := c.Check(context.Background(), "av1_qsv"); unsupported {
				t.Fatalf("call %d: unsupported = true, want false for an inconclusive probe", i+1)
			}
		}
		if got := readInvocationLog(t, logPath); len(got) != 2 {
			t.Fatalf("probe invoked %d times, want 2 (inconclusive verdicts must be re-probed)", len(got))
		}
	})

	// The regression the review caught: a single transient probe failure must
	// not permanently disable the pre-check. The first probe fails for an
	// unrelated reason; the second reports the real codec gap and must be seen.
	t.Run("transient failure does not permanently disable the check", func(t *testing.T) {
		dir := t.TempDir()
		logPath := filepath.Join(dir, "invocations.log")
		ffmpegPath := writeStatefulProbeFFmpeg(t, dir, logPath,
			[]int{1, 171},
			[][]string{
				{"[AVHWDeviceContext @ 0x55] Device creation failed: -9."},
				{"[av1_qsv @ 0x55] This version of runtime doesn't support AV1 encoding"},
			})

		c := NewHardwareCodecChecker(ffmpegPath)
		if unsupported, _ := c.Check(context.Background(), "av1_qsv"); unsupported {
			t.Fatal("first probe failed transiently; must not report unsupported")
		}
		unsupported, reason := c.Check(context.Background(), "av1_qsv")
		if !unsupported {
			t.Fatalf("second probe reported the codec gap; unsupported = false (reason=%q)", reason)
		}
		if got := readInvocationLog(t, logPath); len(got) != 2 {
			t.Fatalf("probe invoked %d times, want 2 (transient failure must not be cached)", len(got))
		}
	})
}

// TestHardwareCodecCheckerConcurrentProbesDeduplicated verifies that concurrent
// jobs requesting the same encoder share one probe instead of each spawning
// their own.
func TestHardwareCodecCheckerConcurrentProbesDeduplicated(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "invocations.log")
	ffmpegPath := writeMockProbeFFmpeg(t, dir, logPath, 171,
		"This version of runtime doesn't support AV1 encoding")

	c := NewHardwareCodecChecker(ffmpegPath)

	const callers = 8
	start := make(chan struct{})
	var wg sync.WaitGroup
	results := make([]bool, callers)
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			unsupported, _ := c.Check(context.Background(), "av1_qsv")
			results[i] = unsupported
		}()
	}
	close(start)
	wg.Wait()

	for i, got := range results {
		if !got {
			t.Errorf("caller %d: unsupported = false, want true", i)
		}
	}
	if invocations := readInvocationLog(t, logPath); len(invocations) != 1 {
		t.Fatalf("probe invoked %d times, want 1 (concurrent callers must share one probe)", len(invocations))
	}
}

// TestProcessJob_HardwareCodecUnsupportedFailsFast is the regression test for
// a hardware encoder the runtime cannot encode (av1_qsv on a Comet Lake iGPU).
// The pre-flight check must fail the job with ENCODER_UNSUPPORTED after the
// probe alone: the real transcode must not run, so the job can no longer
// silently degrade to an unusably slow software fallback that never reaches a
// terminal state.
func TestProcessJob_HardwareCodecUnsupportedFailsFast(t *testing.T) {
	tmpDir := t.TempDir()
	logPath := filepath.Join(tmpDir, "invocations.log")
	ffmpegPath := writeMockProbeFFmpeg(t, tmpDir, logPath, 171,
		"[av1_qsv @ 0x55] This version of runtime doesn't support AV1 encoding")

	srv := newFailureMatrixServer(false, false)
	defer srv.Close()

	w := newFailureMatrixWorker(t, ffmpegPath, srv.URL, tmpDir)
	w.hwCodecChecker = NewHardwareCodecChecker(ffmpegPath)

	job := protocol.JobInfo{
		ID:             "hw-codec-precheck",
		InputFiles:     []string{"input-001"},
		Args:           []string{"-i", "<INPUT_FILE>", "-c:v", "av1_qsv"},
		OutputFilename: "out.mp4",
	}

	jobCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	w.processJob(jobCtx, job, cancel, false)

	update := srv.terminalUpdate(t)
	if update.Status != protocol.JobStatusFailed {
		t.Errorf("Status = %q, want %q", update.Status, protocol.JobStatusFailed)
	}
	if update.FailureType != string(protocol.FailureEncoderUnsupported) {
		t.Errorf("FailureType = %q, want %q (error=%q)", update.FailureType, protocol.FailureEncoderUnsupported, update.Error)
	}

	invocations := readInvocationLog(t, logPath)
	if len(invocations) != 1 {
		t.Fatalf("ffmpeg invoked %d times, want 1 (the capability probe only); invocations: %v", len(invocations), invocations)
	}
	if !strings.Contains(invocations[0], "nullsrc") {
		t.Errorf("the single ffmpeg invocation must be the capability probe, got %q", invocations[0])
	}
}
