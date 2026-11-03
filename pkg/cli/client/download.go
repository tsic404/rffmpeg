package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// downloadIdleTimeout is the effective download inactivity bound; a var so
// tests can shrink the stall window instead of waiting out a real minute.
var downloadIdleTimeout = DownloadIdleTimeout

// downloadTempSuffix names the sibling staging file a download is written to
// before it is renamed onto the target; a pre-existing entry at that name moves
// the download to a pid-suffixed variant.
const downloadTempSuffix = ".part"

// maxDownloadSymlinkDepth bounds link chains when resolving the output path; a
// cycle keeps yielding dangling hops, so the depth has to be capped as well.
const maxDownloadSymlinkDepth = 40

// DownloadOutput downloads an output file.
//
// The transfer is single-shot (a failed download is retried as a whole, never
// resumed), so it is bounded by inactivity rather than by total duration: a
// large output that keeps arriving is never cut off by a wall-clock cap, while
// a stream that stops moving fails within DownloadIdleTimeout. The bytes go to
// a sibling temp file that is renamed into place only after the copy and the
// final flush succeed, so an interrupted download neither truncates an
// existing output nor leaves a partial file under the target name.
func (c *Client) DownloadOutput(fileID, outputPath string) error {
	url := fmt.Sprintf(c.serverURL+OutputEndpoint, fileID)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	c.setAuthHeader(req)

	client := &http.Client{Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: DefaultTimeout, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   DefaultTimeout,
		ResponseHeaderTimeout: DefaultTimeout,
		ForceAttemptHTTP2:     true,
	}}
	defer client.CloseIdleConnections()

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("download request failed: %w", err)
	}

	if resp.StatusCode == http.StatusNotFound {
		DrainAndClose(resp.Body)
		return fmt.Errorf("output file not found: %s", fileID)
	}

	if resp.StatusCode != http.StatusOK {
		DrainAndClose(resp.Body)
		return fmt.Errorf("download failed with status %d", resp.StatusCode)
	}

	target, err := downloadTarget(outputPath)
	if err != nil {
		DrainAndClose(resp.Body)
		return err
	}

	dir := filepath.Dir(target)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0755); err != nil {
			DrainAndClose(resp.Body)
			return fmt.Errorf("failed to create output directory: %w", err)
		}
	}

	body := newIdleWatchdogBody(resp.Body, downloadIdleTimeout, cancel)
	defer body.Close()

	tmpPath, tmp, err := createDownloadStaging(target)
	if err != nil {
		return fmt.Errorf("failed to create output file: %w", err)
	}
	placed := false
	defer func() {
		if placed {
			return
		}
		tmp.Close()
		os.Remove(tmpPath)
	}()

	if mode, ok, err := existingFileMode(target); err != nil {
		return err
	} else if ok {
		if err := tmp.Chmod(mode); err != nil {
			return fmt.Errorf("failed to preserve output file mode: %w", err)
		}
	}

	if _, err := io.Copy(tmp, body); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("download stalled: no data received for %s: %w", downloadIdleTimeout, err)
		}
		return fmt.Errorf("failed to write output file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("failed to write output file: %w", err)
	}
	if err := os.Rename(tmpPath, target); err != nil {
		return fmt.Errorf("failed to move downloaded file into place: %w", err)
	}
	placed = true

	return nil
}

// downloadTarget returns the path the download must write. A symlink is
// followed the way os.Create follows it, so the link itself survives and the
// file it names is what gets replaced. A dangling link resolves to the path it
// names — the download creates that file and leaves the link in place; a
// missing parent directory, a link loop or a permission problem is reported
// instead of falling back to replacing the link entry.
func downloadTarget(outputPath string) (string, error) {
	path := outputPath
	for depth := 0; depth < maxDownloadSymlinkDepth; depth++ {
		info, err := os.Lstat(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return path, nil
			}
			return "", fmt.Errorf("failed to inspect output file: %w", err)
		}
		if info.Mode()&os.ModeSymlink == 0 {
			return path, nil
		}
		resolved, err := filepath.EvalSymlinks(path)
		if err == nil {
			return resolved, nil
		}
		if !errors.Is(err, os.ErrNotExist) {

			return "", fmt.Errorf("failed to resolve output file: %w", err)
		}

		dest, err := os.Readlink(path)
		if err != nil {
			return "", fmt.Errorf("failed to resolve output file: %w", err)
		}
		if !filepath.IsAbs(dest) {
			dest = filepath.Join(filepath.Dir(path), dest)
		}

		if _, err := os.Stat(filepath.Dir(dest)); err != nil {
			return "", fmt.Errorf("failed to resolve output file: %w", err)
		}
		path = dest
	}
	return "", fmt.Errorf("failed to resolve output file %s: too many levels of symbolic links", outputPath)
}

// existingFileMode reports the mode a rename would replace. A missing target
// has no mode to carry, and a symlink's perm bits say nothing about the file
// it names, so both read as "no mode". Any other stat failure is returned: a
// permission problem must not silently drop the target's mode.
func existingFileMode(target string) (os.FileMode, bool, error) {
	info, err := os.Lstat(target)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("failed to inspect output file: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return 0, false, nil
	}
	return info.Mode().Perm(), true, nil
}

// createDownloadStaging opens the staging file beside target. The name is
// predictable, so the open is exclusive: an entry that already exists there —
// a symlink planted in a shared output directory included — is never followed
// or truncated, it only moves the download to the pid-suffixed name.
func createDownloadStaging(target string) (string, *os.File, error) {
	names := []string{
		target + downloadTempSuffix,
		fmt.Sprintf("%s%s.%d", target, downloadTempSuffix, os.Getpid()),
	}
	var lastErr error
	for _, name := range names {
		f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o666)
		if err == nil {
			return name, f, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return "", nil, err
		}
		lastErr = err
	}
	return "", nil, lastErr
}

// downloadIdleWatchdogBody cancels the transfer once the body delivers no
// bytes for idle. It records the time of the last received chunk and arms a
// timer for the remainder of the window, so the stall verdict lands at idle
// itself rather than at whatever a polling tick happens to be. Close joins the
// watchdog, so none outlives the download.
type downloadIdleWatchdogBody struct {
	body         io.ReadCloser
	idle         time.Duration
	cancel       context.CancelFunc
	lastProgress atomic.Int64 // unix nanoseconds of the latest received chunk
	done         chan struct{}
	wg           sync.WaitGroup
}

func newIdleWatchdogBody(body io.ReadCloser, idle time.Duration, cancel context.CancelFunc) *downloadIdleWatchdogBody {
	b := &downloadIdleWatchdogBody{
		body:   body,
		idle:   idle,
		cancel: cancel,
		done:   make(chan struct{}),
	}
	b.lastProgress.Store(time.Now().UnixNano())
	if idle > 0 {
		b.wg.Add(1)
		go b.watch()
	}
	return b
}

func (b *downloadIdleWatchdogBody) watch() {
	defer b.wg.Done()
	for {
		elapsed := time.Since(time.Unix(0, b.lastProgress.Load()))
		if elapsed >= b.idle {
			b.cancel()
			return
		}
		timer := time.NewTimer(b.idle - elapsed)
		select {
		case <-b.done:
			timer.Stop()
			return
		case <-timer.C:

		}
	}
}

func (b *downloadIdleWatchdogBody) Read(p []byte) (int, error) {
	n, err := b.body.Read(p)
	if n > 0 {
		b.lastProgress.Store(time.Now().UnixNano())
	}
	return n, err
}

func (b *downloadIdleWatchdogBody) Close() error {
	close(b.done)
	b.wg.Wait()
	return b.body.Close()
}
