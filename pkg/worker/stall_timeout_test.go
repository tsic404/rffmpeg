package worker

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestFFmpegSuppressesProgressOutput(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want bool
	}{
		{name: "no stats flags", args: []string{"-i", "in.mp4", "-c:v", "libx264", "out.mp4"}, want: false},
		{name: "nostats", args: []string{"-nostats", "-i", "in.mp4", "out.mp4"}, want: true},
		{name: "loglevel error", args: []string{"-loglevel", "error", "-i", "in.mp4", "out.mp4"}, want: true},
		{name: "loglevel warning", args: []string{"-loglevel", "warning"}, want: true},
		{name: "loglevel info", args: []string{"-loglevel", "info"}, want: false},
		{name: "loglevel verbose", args: []string{"-loglevel", "verbose"}, want: false},
		{name: "v quiet", args: []string{"-v", "quiet"}, want: true},
		{name: "loglevel equals error", args: []string{"-loglevel=error"}, want: true},
		{name: "loglevel equals verbose", args: []string{"-loglevel=verbose"}, want: false},
		{name: "numeric warning", args: []string{"-loglevel", "24"}, want: true},
		{name: "numeric info", args: []string{"-loglevel", "32"}, want: false},
		// A non-quiet loglevel must not mask a later -nostats: the scan
		// accumulates over every flag instead of returning on the first one.
		{name: "loglevel info then nostats", args: []string{"-loglevel", "info", "-nostats"}, want: true},
		{name: "nostats then loglevel info", args: []string{"-nostats", "-loglevel", "info"}, want: true},
		{name: "v info then nostats", args: []string{"-v", "info", "-nostats"}, want: true},
		{name: "loglevel verbose then nostats", args: []string{"-loglevel", "verbose", "-i", "in.mp4", "-nostats"}, want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ffmpegSuppressesProgressOutput(tt.args); got != tt.want {
				t.Errorf("ffmpegSuppressesProgressOutput(%v) = %v, want %v", tt.args, got, tt.want)
			}
		})
	}
}

// newStallTestExecutor builds an executor whose "ffmpeg" is `sh -c`, so the
// idle watchdog can be driven with shell timing without a real transcode.
func newStallTestExecutor(t *testing.T, idleTimeout time.Duration) *Executor {
	t.Helper()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh not installed")
	}
	ex := NewExecutor(sh, 30*time.Second)
	ex.SetIdleTimeout(idleTimeout)
	return ex
}

// TestExecutor_StallWatchdogKillsSilentProcess verifies the idle watchdog kills
// a process that stops producing output, well before its total execution budget
// would. This is the safety net for a transcode that hangs without any output.
func TestExecutor_StallWatchdogKillsSilentProcess(t *testing.T) {
	ex := newStallTestExecutor(t, 400*time.Millisecond)

	start := time.Now()
	// Emit one line, then go silent for 30s. Without the watchdog this would
	// hit the 30s total timeout; with it the process is killed in ~1s.
	result := ex.Execute(context.Background(), []string{"-c", "echo starting >&2; sleep 30"})

	if !result.IsTimeout {
		t.Fatalf("IsTimeout = false, want true (error=%v, stderr=%q)", result.Error, result.Stderr)
	}
	if result.Error == nil || !strings.Contains(result.Error.Error(), "stalled") {
		t.Errorf("Error = %v, want a stall message", result.Error)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("stall not detected promptly: took %v (total-timeout fallback, not the watchdog)", elapsed)
	}
}

// TestExecutor_StallWatchdogResetsOnOutput verifies a healthy output stream
// resets the watchdog, so a live transcode that emits -stats lines is never
// killed even when each individual gap is shorter than the idle timeout.
func TestExecutor_StallWatchdogResetsOnOutput(t *testing.T) {
	ex := newStallTestExecutor(t, 600*time.Millisecond)

	script := `i=0; while [ $i -lt 5 ]; do echo "frame=$i" >&2; sleep 0.2; i=$((i+1)); done`
	result := ex.Execute(context.Background(), []string{"-c", script})

	if result.IsTimeout {
		t.Fatalf("healthy output stream was killed as stalled (error=%v)", result.Error)
	}
	if result.Error != nil {
		t.Fatalf("Execute() error = %v, want nil", result.Error)
	}
}

// TestExecutor_StallWatchdogSkippedForQuietJobs verifies the watchdog does not
// run for jobs that suppress ffmpeg's -stats output, whose legitimate silence
// would otherwise be mistaken for a stall. The loglevel-before-nostats case is
// the regression: a non-quiet loglevel must not stop the scan from finding a
// later -nostats.
func TestExecutor_StallWatchdogSkippedForQuietJobs(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "nostats", args: []string{"-c", "sleep 1.5", "-nostats"}},
		{name: "loglevel info then nostats", args: []string{"-c", "sleep 1.5", "-loglevel", "info", "-nostats"}},
		{name: "quiet loglevel", args: []string{"-c", "sleep 1.5", "-loglevel", "quiet"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ex := newStallTestExecutor(t, 300*time.Millisecond)

			// The stats flags land in args after the script ($0 onward for sh),
			// so the gate sees them while the shell itself is unaffected.
			result := ex.Execute(context.Background(), tt.args)

			if result.IsTimeout {
				t.Fatalf("quiet job was killed as stalled (error=%v)", result.Error)
			}
			if result.Error != nil {
				t.Fatalf("Execute() error = %v, want nil", result.Error)
			}
		})
	}
}
