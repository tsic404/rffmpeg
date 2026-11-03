package client

import (
	"bytes"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// downloadServer serves the output endpoint with handler, wiring the request's
// cancellation into the handler so a stalled handler unblocks when the client
// gives up on the transfer.
func downloadServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/output/file-1" {
			http.NotFound(w, r)
			return
		}
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// assertNoStagedFile fails when a download staging file is left in dir.
func assertNoStagedFile(t *testing.T, dir string) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, "*"+downloadTempSuffix+"*"))
	if err != nil {
		t.Fatalf("glob staged files: %v", err)
	}
	if len(matches) > 0 {
		t.Fatalf("staged files left behind: %v", matches)
	}
}

// TestDownloadOutput_SlowProgressIsNotAStall pins the time bound's shape: a
// transfer slower than the whole download budget but with bytes arriving
// inside every idle window must complete. The old single wall-clock timeout on
// the request aborted exactly this case.
func TestDownloadOutput_SlowProgressIsNotAStall(t *testing.T) {
	oldIdle := downloadIdleTimeout
	downloadIdleTimeout = 150 * time.Millisecond
	defer func() { downloadIdleTimeout = oldIdle }()

	chunks := bytes.Repeat([]byte("0123456789abcdef"), 2)
	srv := downloadServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("response writer does not support flushing")
			return
		}
		for _, b := range chunks {
			if _, err := w.Write([]byte{b}); err != nil {
				return
			}
			flusher.Flush()
			// 40ms per byte for 32 bytes: ~1.3s of transfer, many times the
			// 150ms idle window in total but never idle within it.
			select {
			case <-time.After(40 * time.Millisecond):
			case <-r.Context().Done():
				return
			}
		}
	})
	dir := t.TempDir()
	out := filepath.Join(dir, "out.bin")

	c := New(srv.URL, "")
	if err := c.DownloadOutput("file-1", out); err != nil {
		t.Fatalf("DownloadOutput failed on a slow but live transfer: %v", err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	if !bytes.Equal(got, chunks) {
		t.Fatalf("output = %d bytes, want %d identical bytes", len(got), len(chunks))
	}
	assertNoStagedFile(t, dir)
}

// TestDownloadOutput_StalledTransferFailsWithoutTouchingExistingOutput pins
// both halves of the failure contract: inactivity is reported as a stall, and
// the pre-existing file at the target path is neither truncated nor replaced
// by the partial transfer.
func TestDownloadOutput_StalledTransferFailsWithoutTouchingExistingOutput(t *testing.T) {
	oldIdle := downloadIdleTimeout
	downloadIdleTimeout = 120 * time.Millisecond
	defer func() { downloadIdleTimeout = oldIdle }()

	srv := downloadServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		flusher, _ := w.(http.Flusher)
		if _, err := w.Write([]byte("partial")); err != nil {
			return
		}
		if flusher != nil {
			flusher.Flush()
		}
		// Stop delivering bytes until the client abandons the transfer.
		<-r.Context().Done()
	})
	dir := t.TempDir()
	out := filepath.Join(dir, "out.bin")
	if err := os.WriteFile(out, []byte("existing-output"), 0o644); err != nil {
		t.Fatalf("seed existing output: %v", err)
	}

	c := New(srv.URL, "")
	start := time.Now()
	err := c.DownloadOutput("file-1", out)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("DownloadOutput succeeded on a stalled transfer, want a stall error")
	}
	if !strings.Contains(err.Error(), "stalled") {
		t.Fatalf("error = %v, want it reported as a stalled download", err)
	}
	// The stall verdict is owed at idle, not at the next polling tick: the
	// two-tick detector reported ~2*idle, which is what this bound rejects.
	if elapsed > 7*downloadIdleTimeout/4 {
		t.Fatalf("stall detected after %v, want it within %v of the last byte", elapsed, downloadIdleTimeout)
	}

	got, readErr := os.ReadFile(out)
	if readErr != nil {
		t.Fatalf("read output after failed download: %v", readErr)
	}
	if string(got) != "existing-output" {
		t.Fatalf("existing output = %q, want it left untouched", got)
	}
	assertNoStagedFile(t, dir)
}

// TestDownloadOutput_AbortedTransferLeavesNoPartialTarget pins the other
// failure shape: a connection that dies mid-body must not leave the target
// path holding a half-written file.
func TestDownloadOutput_AbortedTransferLeavesNoPartialTarget(t *testing.T) {
	srv := downloadServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", "1024")
		flusher, _ := w.(http.Flusher)
		if _, err := w.Write([]byte("half")); err != nil {
			return
		}
		if flusher != nil {
			flusher.Flush()
		}
		// Abort the connection instead of finishing the declared body.
		if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
			if tcp, ok := conn.(*net.TCPConn); ok {
				_ = tcp.SetLinger(0)
			}
			_ = conn.Close()
			return
		}
		panic(http.ErrAbortHandler)
	})
	dir := t.TempDir()
	out := filepath.Join(dir, "out.bin")

	c := New(srv.URL, "")
	if err := c.DownloadOutput("file-1", out); err == nil {
		t.Fatal("DownloadOutput succeeded on an aborted transfer, want an error")
	}
	if _, statErr := os.Stat(out); !os.IsNotExist(statErr) {
		t.Fatalf("target exists after an aborted download (stat err = %v), want no partial file", statErr)
	}
	assertNoStagedFile(t, dir)
}

// TestDownloadOutput_ReplacesExistingOutputOnSuccess pins the success half of
// the staged rename: the new bytes land at the target path.
func TestDownloadOutput_ReplacesExistingOutputOnSuccess(t *testing.T) {
	srv := downloadServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write([]byte("fresh-output"))
	})
	dir := t.TempDir()
	out := filepath.Join(dir, "out.bin")
	if err := os.WriteFile(out, []byte("existing-output"), 0o644); err != nil {
		t.Fatalf("seed existing output: %v", err)
	}

	c := New(srv.URL, "")
	if err := c.DownloadOutput("file-1", out); err != nil {
		t.Fatalf("DownloadOutput: %v", err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	if string(got) != "fresh-output" {
		t.Fatalf("output = %q, want the downloaded bytes", got)
	}
	assertNoStagedFile(t, dir)
}

// TestDownloadOutput_DoesNotFollowPlantedStagingSymlink pins the staging open:
// a path that already exists at the predictable staging name must never be
// followed or truncated, and the download must still land on the target.
func TestDownloadOutput_DoesNotFollowPlantedStagingSymlink(t *testing.T) {
	srv := downloadServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write([]byte("downloaded"))
	})
	dir := t.TempDir()
	victim := filepath.Join(dir, "victim.txt")
	if err := os.WriteFile(victim, []byte("must-not-change"), 0o644); err != nil {
		t.Fatalf("seed victim: %v", err)
	}
	out := filepath.Join(dir, "out.bin")
	if err := os.Symlink(victim, out+downloadTempSuffix); err != nil {
		t.Fatalf("plant staging symlink: %v", err)
	}

	c := New(srv.URL, "")
	if err := c.DownloadOutput("file-1", out); err != nil {
		t.Fatalf("DownloadOutput: %v", err)
	}

	got, err := os.ReadFile(victim)
	if err != nil {
		t.Fatalf("read victim: %v", err)
	}
	if string(got) != "must-not-change" {
		t.Fatalf("victim = %q, want the download not to write through the planted symlink", got)
	}
	downloaded, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	if string(downloaded) != "downloaded" {
		t.Fatalf("output = %q, want the downloaded bytes", downloaded)
	}
	// The planted entry is not ours to delete, the pid-suffixed staging is.
	if _, err := os.Lstat(out + downloadTempSuffix + "." + strconv.Itoa(os.Getpid())); !os.IsNotExist(err) {
		t.Fatalf("pid-suffixed staging file left behind (stat err = %v)", err)
	}
}

// TestDownloadOutput_PreservesExistingTargetMode pins the overwrite metadata:
// replacing the directory entry must not reset the mode the target carried.
func TestDownloadOutput_PreservesExistingTargetMode(t *testing.T) {
	srv := downloadServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write([]byte("fresh"))
	})
	dir := t.TempDir()
	out := filepath.Join(dir, "out.bin")
	if err := os.WriteFile(out, []byte("old"), 0o600); err != nil {
		t.Fatalf("seed output: %v", err)
	}

	c := New(srv.URL, "")
	if err := c.DownloadOutput("file-1", out); err != nil {
		t.Fatalf("DownloadOutput: %v", err)
	}
	info, err := os.Stat(out)
	if err != nil {
		t.Fatalf("stat output: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("output mode = %#o, want the pre-existing 0600 preserved", got)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	if string(got) != "fresh" {
		t.Fatalf("output = %q, want the downloaded bytes", got)
	}
}

// TestDownloadOutput_WritesThroughTargetSymlink pins link semantics: a target
// that is a symlink keeps being a symlink, and the file it names is what gets
// replaced.
func TestDownloadOutput_WritesThroughTargetSymlink(t *testing.T) {
	srv := downloadServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write([]byte("fresh"))
	})
	dir := t.TempDir()
	real := filepath.Join(dir, "real.bin")
	if err := os.WriteFile(real, []byte("old"), 0o640); err != nil {
		t.Fatalf("seed real file: %v", err)
	}
	out := filepath.Join(dir, "out.bin")
	if err := os.Symlink(real, out); err != nil {
		t.Fatalf("create output symlink: %v", err)
	}

	c := New(srv.URL, "")
	if err := c.DownloadOutput("file-1", out); err != nil {
		t.Fatalf("DownloadOutput: %v", err)
	}
	info, err := os.Lstat(out)
	if err != nil {
		t.Fatalf("lstat output: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("output symlink was replaced by a regular file")
	}
	got, err := os.ReadFile(real)
	if err != nil {
		t.Fatalf("read resolved file: %v", err)
	}
	if string(got) != "fresh" {
		t.Fatalf("resolved file = %q, want the downloaded bytes", got)
	}
	realInfo, err := os.Stat(real)
	if err != nil {
		t.Fatalf("stat resolved file: %v", err)
	}
	if got := realInfo.Mode().Perm(); got != 0o640 {
		t.Fatalf("resolved file mode = %#o, want the pre-existing 0640 preserved", got)
	}
}

// TestDownloadOutput_CreatesDanglingSymlinkTarget pins the base semantics for a
// dangling link: os.Create follows it, so the download creates the file the
// link names and leaves the link in place. The created file must keep the
// umask-derived mode, never the link's meaningless 0777.
func TestDownloadOutput_CreatesDanglingSymlinkTarget(t *testing.T) {
	srv := downloadServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write([]byte("fresh"))
	})
	dir := t.TempDir()
	real := filepath.Join(dir, "real.bin")
	out := filepath.Join(dir, "out.bin")
	if err := os.Symlink("real.bin", out); err != nil {
		t.Fatalf("create dangling output symlink: %v", err)
	}

	c := New(srv.URL, "")
	if err := c.DownloadOutput("file-1", out); err != nil {
		t.Fatalf("DownloadOutput failed on a dangling output symlink: %v", err)
	}

	linkInfo, err := os.Lstat(out)
	if err != nil {
		t.Fatalf("lstat output: %v", err)
	}
	if linkInfo.Mode()&os.ModeSymlink == 0 {
		t.Fatal("output symlink was replaced by a regular file")
	}
	got, err := os.ReadFile(real)
	if err != nil {
		t.Fatalf("read resolved file: %v", err)
	}
	if string(got) != "fresh" {
		t.Fatalf("resolved file = %q, want the downloaded bytes", got)
	}
	info, err := os.Stat(real)
	if err != nil {
		t.Fatalf("stat resolved file: %v", err)
	}
	if perm := info.Mode().Perm(); perm&0o002 != 0 || perm&0o400 == 0 {
		t.Fatalf("created file mode = %#o, want a umask-derived owner-writable mode", perm)
	}
	assertNoStagedFile(t, dir)
}

// TestDownloadOutput_DanglingSymlinkMissingParentFails pins the other half:
// a link whose parent directory does not exist cannot be created, so the
// download reports ENOENT instead of replacing the link entry.
func TestDownloadOutput_DanglingSymlinkMissingParentFails(t *testing.T) {
	srv := downloadServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write([]byte("fresh"))
	})
	dir := t.TempDir()
	out := filepath.Join(dir, "out.bin")
	if err := os.Symlink(filepath.Join("sub", "real.bin"), out); err != nil {
		t.Fatalf("create dangling output symlink: %v", err)
	}

	c := New(srv.URL, "")
	err := c.DownloadOutput("file-1", out)
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("error = %v, want ENOENT for the missing link parent", err)
	}
	info, statErr := os.Lstat(out)
	if statErr != nil {
		t.Fatalf("lstat output: %v", statErr)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("output symlink was replaced by a regular file")
	}
	assertNoStagedFile(t, dir)
}

// TestDownloadOutput_SymlinkLoopFails pins that a link cycle is reported, not
// silently turned into a replacement of the link entry.
func TestDownloadOutput_SymlinkLoopFails(t *testing.T) {
	srv := downloadServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write([]byte("fresh"))
	})
	dir := t.TempDir()
	a := filepath.Join(dir, "a.bin")
	b := filepath.Join(dir, "b.bin")
	if err := os.Symlink(b, a); err != nil {
		t.Fatalf("create link a: %v", err)
	}
	if err := os.Symlink(a, b); err != nil {
		t.Fatalf("create link b: %v", err)
	}

	c := New(srv.URL, "")
	if err := c.DownloadOutput("file-1", a); err == nil {
		t.Fatal("DownloadOutput succeeded on a symlink loop, want an error")
	}
	info, err := os.Lstat(a)
	if err != nil {
		t.Fatalf("lstat output: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("output symlink was replaced by a regular file")
	}
	assertNoStagedFile(t, dir)
}

// TestDownloadOutput_StagingCarriesTargetModeDuringTransfer pins the window
// finding: the restrictive mode of an existing target must be on the staging
// file before any byte lands, not applied after the copy. The handler holds
// the response open after the first chunk so the test can inspect the staging
// file mid-transfer.
func TestDownloadOutput_StagingCarriesTargetModeDuringTransfer(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseDownload := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseDownload()

	srv := downloadServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		if _, err := w.Write([]byte("first")); err != nil {
			return
		}
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		close(started)
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		_, _ = w.Write([]byte("second"))
	})
	dir := t.TempDir()
	out := filepath.Join(dir, "out.bin")
	if err := os.WriteFile(out, []byte("old"), 0o600); err != nil {
		t.Fatalf("seed output: %v", err)
	}

	c := New(srv.URL, "")
	done := make(chan error, 1)
	go func() { done <- c.DownloadOutput("file-1", out) }()

	<-started
	staging, ok := waitForStagingFile(dir, 2*time.Second)
	if !ok {
		releaseDownload()
		t.Fatal("staging file never appeared during the transfer")
	}
	info, err := os.Stat(staging)
	if err != nil {
		releaseDownload()
		t.Fatalf("stat staging file: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		releaseDownload()
		t.Fatalf("staging mode = %#o during the transfer, want the target's 0600", got)
	}

	releaseDownload()
	if err := <-done; err != nil {
		t.Fatalf("DownloadOutput: %v", err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	if string(got) != "firstsecond" {
		t.Fatalf("output = %q, want the full download", got)
	}
}

// waitForStagingFile polls dir until a download staging file shows up.
func waitForStagingFile(dir string, timeout time.Duration) (string, bool) {
	deadline := time.Now().Add(timeout)
	for {
		matches, err := filepath.Glob(filepath.Join(dir, "*"+downloadTempSuffix+"*"))
		if err == nil && len(matches) > 0 {
			return matches[0], true
		}
		if time.Now().After(deadline) {
			return "", false
		}
		time.Sleep(2 * time.Millisecond)
	}
}
