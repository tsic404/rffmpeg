package handlers

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tsic404/rffmpeg/pkg/protocol"
)

// TestWriteClearEvictionFailure pins the manual-clear error contract: a worker
// row that vanished between the lookup and the clear is an unknown object
// (404 not_found), never a 500. The row can disappear under the request when
// the monitor's offline sweep removes it.
func TestWriteClearEvictionFailure(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		status int
		code   protocol.ErrorCode
	}{
		{"vanished worker", protocol.ErrWorkerNotFound, http.StatusNotFound, protocol.ErrCodeNotFound},
		{"wrapped not found", fmt.Errorf("clear worker: %w", protocol.ErrWorkerNotFound), http.StatusNotFound, protocol.ErrCodeNotFound},
		{"storage failure", errors.New("database unavailable"), http.StatusInternalServerError, protocol.ErrCodeInternalError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			writeClearEvictionFailure(rec, tc.err)
			if rec.Code != tc.status {
				t.Fatalf("Expected %d, got %d: %s", tc.status, rec.Code, rec.Body.String())
			}
			var resp protocol.ErrorResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("Failed to decode %q: %v", rec.Body.String(), err)
			}
			if resp.Code != tc.code {
				t.Errorf("Expected code %q, got %q", tc.code, resp.Code)
			}
		})
	}
}
