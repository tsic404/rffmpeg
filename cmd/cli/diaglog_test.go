package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRedactSecrets pins C-9's token rule: a credential never reaches the
// diagnostic log, whether it arrived through the configuration plane or was
// typed inline on the command line. Both masks are asserted separately, and
// the inline spellings stay in the argument list — masking is log rendering,
// never argument parsing.
func TestRedactSecrets(t *testing.T) {
	orig := os.Args
	defer func() { os.Args = orig }()

	cases := []struct {
		name  string
		argv  []string
		token string
		want  string
	}{
		{"config token value absent", []string{"rffmpeg", "-i", "in.mp4", "out.mp4"}, "SECRET", "rffmpeg -i in.mp4 out.mp4"},
		{"config token value carried", []string{"rffmpeg", "-i", "in.mp4", "SECRET"}, "SECRET", "rffmpeg -i in.mp4 <redacted>"},
		{"no token configured", []string{"rffmpeg", "-i", "in.mp4", "out.mp4"}, "", "rffmpeg -i in.mp4 out.mp4"},
		{"inline --token= with a different value", []string{"rffmpeg", "-i", "in.mp4", "--token=OTHER", "out.mp4"}, "CONFIGURED", "rffmpeg -i in.mp4 --token=<redacted> out.mp4"},
		{"inline --token= with the configured value", []string{"rffmpeg", "-i", "in.mp4", "--token=CONFIGURED", "out.mp4"}, "CONFIGURED", "rffmpeg -i in.mp4 --token=<redacted> out.mp4"},
		{"inline -token= with no configured token", []string{"rffmpeg", "-i", "in.mp4", "-token=OTHER", "out.mp4"}, "", "rffmpeg -i in.mp4 -token=<redacted> out.mp4"},
		{"space separated --token", []string{"rffmpeg", "--token", "OTHER", "-i", "in.mp4", "out.mp4"}, "", "rffmpeg --token <redacted> -i in.mp4 out.mp4"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			os.Args = tc.argv
			if got := redactSecrets(tc.token); got != tc.want {
				t.Errorf("redactSecrets(%q) = %q, want %q", tc.token, got, tc.want)
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
	tee := startStderrTee("", "")
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

	tee := startStderrTee(logPath, "")
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

	orig := os.Stderr
	tee := startStderrTee(logPath, "")
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
	tee := startStderrTee("", "")
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

	tee := startStderrTee(logPath, "")
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
