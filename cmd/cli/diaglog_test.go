package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRedactSecrets(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want string
	}{
		{"space token", []string{"rffmpeg", "--token", "SECRET", "-i", "in.mp4"}, "rffmpeg --token <redacted> -i in.mp4"},
		{"equals token", []string{"rffmpeg", "--token=SECRET", "out.mp4"}, "rffmpeg --token=<redacted> out.mp4"},
		{"short space token", []string{"rffmpeg", "-token", "SECRET"}, "rffmpeg -token <redacted>"},
		{"short equals token", []string{"rffmpeg", "-token=SECRET"}, "rffmpeg -token=<redacted>"},
		{"no token", []string{"rffmpeg", "-i", "in.mp4", "out.mp4"}, "rffmpeg -i in.mp4 out.mp4"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := redactSecrets(tt.in); got != tt.want {
				t.Errorf("redactSecrets(%v) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestTailBufferRetainsTail(t *testing.T) {
	b := newTailBuffer(5)
	if _, err := b.Write([]byte("0123456789")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := b.String(); got != "56789" {
		t.Errorf("tailBuffer retained %q, want %q", got, "56789")
	}
}

func TestStderrTeeDisabledNoOp(t *testing.T) {
	t.Setenv(diagLogFileEnv, "")
	tee := startStderrTee()
	orig := os.Stderr

	tee.stop()
	if os.Stderr != orig {
		t.Errorf("disabled tee.stop() reassigned os.Stderr")
	}
	// record on a disabled tee must not panic or touch the filesystem.
	tee.record(ExitError, "http://example.test")
}

func TestStderrTeeCaptureAndRecord(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "diag.log")
	t.Setenv(diagLogFileEnv, logPath)

	tee := startStderrTee()
	defer tee.stop()

	fmt.Fprintf(os.Stderr, "boom: transient failure\n")
	tee.stop()
	tee.record(ExitError, "http://example.test")
	tee.record(ExitError, "http://example.test") // second is a no-op

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	got := string(data)
	if !strings.Contains(got, "exit=1 pre-submit-failure server=http://example.test") {
		t.Errorf("record missing header: %q", got)
	}
	if !strings.Contains(got, "boom: transient failure") {
		t.Errorf("record missing captured stderr: %q", got)
	}
	if c := strings.Count(got, "pre-submit-failure"); c != 1 {
		t.Errorf("record written %d times, want 1", c)
	}
	fi, err := os.Stat(logPath)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if fi.Mode().Perm() != 0600 {
		t.Errorf("log perms = %o, want 600", fi.Mode().Perm())
	}
}

// TestStderrTeeRealStderr covers the signal-handler regression: after the tee
// is stopped (post-submit), interrupt/cancel messages must still reach the
// original stderr rather than the tee's closed pipe.
func TestStderrTeeRealStderr(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "diag.log")
	t.Setenv(diagLogFileEnv, logPath)

	orig := os.Stderr
	tee := startStderrTee()
	if tee.realStderr() != orig {
		t.Errorf("active tee realStderr() = %v, want original os.Stderr %v", tee.realStderr(), orig)
	}

	// After stop the pipe is closed; realStderr must still be the original fd,
	// so post-submit interrupt notices are not silently dropped.
	tee.stop()
	if tee.realStderr() != orig {
		t.Errorf("stopped tee realStderr() = %v, want original os.Stderr %v", tee.realStderr(), orig)
	}
}

func TestStderrTeeRealStderrDisabled(t *testing.T) {
	t.Setenv(diagLogFileEnv, "")
	tee := startStderrTee()
	if tee.realStderr() != os.Stderr {
		t.Errorf("disabled tee realStderr() = %v, want os.Stderr %v", tee.realStderr(), os.Stderr)
	}
}

// TestStderrTeeRecordWithoutStop covers the signal-handler path: record is
// called before stop, so it must drain the pipe itself and still capture
// pending stderr without restoring os.Stderr.
func TestStderrTeeRecordWithoutStop(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "diag.log")
	t.Setenv(diagLogFileEnv, logPath)

	tee := startStderrTee()
	defer tee.stop()

	fmt.Fprintln(os.Stderr, "interrupt during upload")
	tee.record(ExitError, "http://example.test")

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.Contains(string(data), "interrupt during upload") {
		t.Errorf("record missing captured stderr: %q", string(data))
	}
}
