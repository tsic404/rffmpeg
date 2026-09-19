package handlers

import (
	"errors"
	"net/http"
	"os"
	"syscall"

	"github.com/tsic404/rffmpeg/pkg/protocol"
)

// freeSpaceBytes reports the bytes available to an unprivileged writer on the
// filesystem holding dir. A lookup failure is returned so callers can skip the
// pre-flight check rather than reject a valid upload whose free space cannot be
// queried.
func freeSpaceBytes(dir string) (uint64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(dir, &stat); err != nil {
		return 0, err
	}
	return stat.Bavail * uint64(stat.Bsize), nil
}

// ensureMultipartSpace pre-flights a multipart upload against the free space of
// the spill directory. The body is spooled once during parsing and read again
// into the output store, so peak usage is about twice the request length; a
// body that cannot be accommodated fails with 507 before any bytes are
// consumed. An unknown length (<= 0) skips the check — the parse-time ENOSPC
// classification still reports a genuinely full disk.
func (h *Handler) ensureMultipartSpace(w http.ResponseWriter, contentLength int64) bool {
	if contentLength <= 0 {
		return true
	}
	dir := h.multipartTmpDir
	if dir == "" {
		dir = os.TempDir()
	}
	free, err := freeSpaceBytes(dir)
	if err != nil {
		return true
	}
	if uint64(contentLength) >= free/2 {
		writeError(w, http.StatusInsufficientStorage, protocol.NewProtocolError(
			protocol.ErrCodeInsufficientStorage, "Insufficient storage: not enough disk space for upload", nil,
		))
		return false
	}
	return true
}

// multipartParseErrorStatus classifies a ParseMultipartForm failure for the
// HTTP response. A full disk (ENOSPC) is a capacity failure: 507 with a
// distinct error code so the worker can tell it apart from a malformed body.
func multipartParseErrorStatus(err error) (int, protocol.ErrorCode, string) {
	if errors.Is(err, syscall.ENOSPC) {
		return http.StatusInsufficientStorage, protocol.ErrCodeInsufficientStorage, "Insufficient storage: no space left on device"
	}
	return http.StatusBadRequest, protocol.ErrCodeInvalidRequest, "Failed to parse multipart form"
}

// writeMultipartParseError writes the response for a failed ParseMultipartForm.
func writeMultipartParseError(w http.ResponseWriter, err error) {
	status, code, msg := multipartParseErrorStatus(err)
	writeError(w, status, protocol.NewProtocolError(code, msg, err))
}
