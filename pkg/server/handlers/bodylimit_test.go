package handlers

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A client-supplied Content-Type must not lift the JSON body cap: only the
// multipart upload routes may exceed it.
func TestMaxBodyBytesMiddlewareIgnoresContentType(t *testing.T) {
	oversized := bytes.Repeat([]byte("a"), int(maxJSONBodyBytes)+1)

	tests := []struct {
		name        string
		method      string
		path        string
		contentType string
		wantCapped  bool
	}{
		{
			name:        "job submit labelled multipart is still capped",
			method:      http.MethodPost,
			path:        "/api/v1/jobs",
			contentType: "multipart/form-data; boundary=x",
			wantCapped:  true,
		},
		{
			name:       "worker register is capped",
			method:     http.MethodPost,
			path:       "/api/v1/workers/register",
			wantCapped: true,
		},
		{
			name:       "single upload reads its own limit",
			method:     http.MethodPost,
			path:       "/api/v1/upload",
			wantCapped: false,
		},
		{
			name:       "chunk upload reads its own limit",
			method:     http.MethodPost,
			path:       "/api/v1/upload/chunk/upload-1/3",
			wantCapped: false,
		},
		{
			name:       "job output upload reads its own limit",
			method:     http.MethodPost,
			path:       "/api/v1/jobs/job-1/output",
			wantCapped: false,
		},
		{
			name:       "lookalike JSON route stays capped",
			method:     http.MethodPost,
			path:       "/api/v1/upload/init",
			wantCapped: true,
		},
		{
			name:       "job output lookalike stays capped",
			method:     http.MethodPost,
			path:       "/api/v1/jobs/job-1/output/extra",
			wantCapped: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var readErr error
			handler := MaxBodyBytesMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, readErr = io.Copy(io.Discard, r.Body)
			}))

			req := httptest.NewRequest(tt.method, tt.path, bytes.NewReader(oversized))
			if tt.contentType != "" {
				req.Header.Set("Content-Type", tt.contentType)
			}
			handler.ServeHTTP(httptest.NewRecorder(), req)

			var maxErr *http.MaxBytesError
			if tt.wantCapped && !errors.As(readErr, &maxErr) {
				t.Errorf("body read error = %v, want *http.MaxBytesError", readErr)
			}
			if !tt.wantCapped && readErr != nil {
				t.Errorf("body read error = %v, want nil", readErr)
			}
		})
	}
}

func TestHasOwnBodyLimitRequiresMethodAndExactShape(t *testing.T) {
	tests := []struct {
		method string
		path   string
		want   bool
	}{
		{http.MethodPost, "/api/v1/upload", true},
		{http.MethodPut, "/api/v1/upload", false},
		{http.MethodPost, "/api/v1/upload/chunk/u/0", true},
		{http.MethodPost, "/api/v1/upload/chunk/u", false},
		{http.MethodPost, "/api/v1/upload/chunk/u/0/extra", false},
		{http.MethodPost, "/api/v1/jobs/j/output", true},
		{http.MethodPost, "/api/v1/jobs//output", false},
		{http.MethodPost, "/api/v1/jobs/j/k/output", false},
		{http.MethodPost, "/api/v1/jobs/j/outputx", false},
	}

	for _, tt := range tests {
		req := httptest.NewRequest(tt.method, tt.path, nil)
		if got := hasOwnBodyLimit(req); got != tt.want {
			t.Errorf("%s %s: hasOwnBodyLimit = %v, want %v", tt.method, tt.path, got, tt.want)
		}
	}
}
