package handlers

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"syscall"
	"testing"

	"github.com/tsic404/rffmpeg/pkg/protocol"
)

func TestMultipartParseErrorStatus(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   protocol.ErrorCode
		wantMsg    string
	}{
		{
			name:       "disk full",
			err:        fmt.Errorf("parse: %w", syscall.ENOSPC),
			wantStatus: http.StatusInsufficientStorage,
			wantCode:   protocol.ErrCodeInsufficientStorage,
			wantMsg:    "Insufficient storage: no space left on device",
		},
		{
			name:       "deeply wrapped disk full",
			err:        fmt.Errorf("parse: %w", fmt.Errorf("create temp: %w", syscall.ENOSPC)),
			wantStatus: http.StatusInsufficientStorage,
			wantCode:   protocol.ErrCodeInsufficientStorage,
			wantMsg:    "Insufficient storage: no space left on device",
		},
		{
			name:       "malformed body",
			err:        errors.New("multipart: NextPart: EOF"),
			wantStatus: http.StatusBadRequest,
			wantCode:   protocol.ErrCodeInvalidRequest,
			wantMsg:    "Failed to parse multipart form",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, code, msg := multipartParseErrorStatus(tt.err)
			if status != tt.wantStatus {
				t.Errorf("status = %d, want %d", status, tt.wantStatus)
			}
			if code != tt.wantCode {
				t.Errorf("code = %q, want %q", code, tt.wantCode)
			}
			if msg != tt.wantMsg {
				t.Errorf("message = %q, want %q", msg, tt.wantMsg)
			}
		})
	}
}

func TestEnsureMultipartSpace(t *testing.T) {
	h := &Handler{} // multipartTmpDir empty → os.TempDir()

	// A body no filesystem can spool must be rejected with 507.
	w := httptest.NewRecorder()
	if h.ensureMultipartSpace(w, math.MaxInt64) {
		t.Fatal("expected a body too large for any disk to be rejected")
	}
	if w.Code != http.StatusInsufficientStorage {
		t.Errorf("status = %d, want %d", w.Code, http.StatusInsufficientStorage)
	}
	var resp protocol.ErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.Code != protocol.ErrCodeInsufficientStorage {
		t.Errorf("code = %q, want %q", resp.Code, protocol.ErrCodeInsufficientStorage)
	}

	// Unknown lengths skip the check.
	for _, n := range []int64{0, -1} {
		if !h.ensureMultipartSpace(httptest.NewRecorder(), n) {
			t.Errorf("expected length %d to skip the pre-check", n)
		}
	}

	// A tiny body always fits.
	if !h.ensureMultipartSpace(httptest.NewRecorder(), 1) {
		t.Error("expected a 1-byte body to pass the pre-check")
	}
}
