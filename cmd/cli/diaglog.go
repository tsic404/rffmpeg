package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

// diagLogFileEnv names the file where the CLI appends a diagnostic record when
// it exits before submitting a job. Empty (the default) disables capture
// entirely: as a drop-in ffmpeg replacement the CLI must leave its stderr
// stream and the filesystem untouched unless explicitly asked to log.
const diagLogFileEnv = "RFFMPEG_LOG_FILE"

// maxDiagBytes bounds the stderr mirror so a long-running transcode that keeps
// streaming progress to stderr cannot grow memory without limit. Pre-submit
// failures only emit a few lines, so the trailing tail is more than enough.
const maxDiagBytes = 1 << 20 // 1 MiB

// tailBuffer retains only the trailing maxBytes of everything written to it.
// Writes come from the single drain goroutine in stderrTee; String must be
// called only after that goroutine has stopped (drain joins it), so no locking
// is needed.
type tailBuffer struct {
	buf      []byte
	maxBytes int
}

func newTailBuffer(maxBytes int) *tailBuffer {
	return &tailBuffer{maxBytes: maxBytes}
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.buf = append(b.buf, p...)
	if len(b.buf) > b.maxBytes {
		b.buf = b.buf[len(b.buf)-b.maxBytes:]
	}
	return len(p), nil
}

func (b *tailBuffer) String() string {
	return string(b.buf)
}

// teeWriter mirrors each write into the tail buffer first and then, best
// effort, into the original stderr. The buffer result is returned, so a closed
// or EPIPE-broken stderr (e.g. the caller is `2>&1 | head` and the head side
// died) can never prevent the buffer from capturing the diagnostic — that is
// exactly the caller-discards-stderr scenario this feature exists for.
type teeWriter struct {
	buf  *tailBuffer
	orig io.Writer
}

func (w teeWriter) Write(p []byte) (int, error) {
	n, _ := w.buf.Write(p)
	_, _ = w.orig.Write(p)
	return n, nil
}

// stderrTee mirrors os.Stderr into a tailBuffer while it is active. A disabled
// tee (no log file configured) has a nil orig and all its methods are no-ops.
//
// The mutex guards drain/record, which can run from both the main flow's
// deferred cleanup and the signal handler's force-quit path, so teardown is
// idempotent and a record is written at most once per run.
type stderrTee struct {
	mu       sync.Mutex
	logPath  string
	buf      *tailBuffer
	orig     *os.File
	w        *os.File
	done     chan struct{}
	stopped  bool
	drained  bool
	recorded bool
}

// startStderrTee begins mirroring os.Stderr when diagLogFileEnv is set. It
// always returns a tee; when the variable is unset or the pipe cannot be
// created, the returned tee is disabled and stderr is left untouched.
func startStderrTee() *stderrTee {
	logPath := os.Getenv(diagLogFileEnv)
	t := &stderrTee{logPath: logPath}
	if logPath == "" {
		return t
	}
	t.buf = newTailBuffer(maxDiagBytes)
	r, w, err := os.Pipe()
	if err != nil {
		return &stderrTee{} // disabled
	}
	t.orig = os.Stderr
	t.w = w
	t.done = make(chan struct{})
	os.Stderr = w
	go func() {
		defer close(t.done)
		_, _ = io.Copy(teeWriter{buf: t.buf, orig: t.orig}, r)
		_ = r.Close()
	}()
	return t
}

// stop restores os.Stderr and joins the drain goroutine. It restores the
// global exactly once — the first call in run()'s defer and the early call
// after a successful submit both race the WebSocket listener goroutines that
// write to os.Stderr below, so a second restore would re-introduce the data
// race this flag prevents.
func (t *stderrTee) stop() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.orig == nil || t.stopped {
		return
	}
	os.Stderr = t.orig
	t.stopped = true
	t.drain()
}

// realStderr returns the writer the CLI should use for user-facing messages
// that must outlive the tee's teardown — specifically the signal handler's
// interrupt/cancel notices, which fire after a successful submit has already
// stopped the tee. It is the original stderr when the tee is active and
// os.Stderr otherwise; t.orig is written once in startStderrTee before any
// goroutine spawns and never mutated, so this read is race-free.
func (t *stderrTee) realStderr() *os.File {
	if t.orig != nil {
		return t.orig
	}
	return os.Stderr
}

// record drains the mirror (if needed) and appends a diagnostic entry for a
// pre-submit failure. It does not restore os.Stderr: the deferred cleanup calls
// stop first, and the signal handler is about to os.Exit where restoring is
// pointless and would race the main flow. A no-op when disabled, and at most
// one record is written per run.
func (t *stderrTee) record(exitCode int, serverURL string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.logPath == "" || t.recorded {
		return
	}
	t.drain()
	t.recorded = true
	_ = appendDiagRecord(t.logPath, exitCode, serverURL, t.buf.String())
}

// drain closes the pipe write end and waits for the drain goroutine to finish.
// The caller must hold t.mu.
func (t *stderrTee) drain() {
	if t.drained || t.orig == nil {
		return
	}
	_ = t.w.Close()
	<-t.done
	t.drained = true
}

// appendDiagRecord writes one diagnostic entry to logPath (0600, append).
func appendDiagRecord(logPath string, exitCode int, serverURL, stderr string) error {
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	defer f.Close()

	var b bytes.Buffer
	fmt.Fprintf(&b, "%s rffmpeg[%d] exit=%d pre-submit-failure server=%s\n",
		time.Now().UTC().Format(time.RFC3339), os.Getpid(), exitCode, serverURL)
	fmt.Fprintf(&b, "argv: %s\n", redactSecrets(os.Args))
	if stderr != "" {
		fmt.Fprintf(&b, "--- captured stderr ---\n%s", stderr)
		if !strings.HasSuffix(stderr, "\n") {
			b.WriteByte('\n')
		}
	}
	_, err = f.Write(b.Bytes())
	return err
}

// redactSecrets renders argv for the diagnostic log, masking the value of any
// --token/-token flag so an auth token never lands on disk.
func redactSecrets(argv []string) string {
	out := make([]string, 0, len(argv))
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		switch {
		case a == "--token" || a == "-token":
			out = append(out, a, "<redacted>")
			if i+1 < len(argv) {
				i++
			}
		case strings.HasPrefix(a, "--token=") || strings.HasPrefix(a, "-token="):
			out = append(out, a[:strings.IndexByte(a, '=')]+"=<redacted>")
		default:
			out = append(out, a)
		}
	}
	return strings.Join(out, " ")
}
