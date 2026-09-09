package client

import (
	"context"
	"errors"
	"net/url"
	"testing"
)

// TestIsRetryableError pins the chunk-upload retry classification. The retry
// path exists to absorb transient network/timeout failures, but the previous
// lowercase "timeout" substring check missed http.Client.Timeout's
// "context deadline exceeded (Client.Timeout ...)" form (capitalized
// "Client.Timeout"), so one slow chunk under CI contention failed the whole
// upload instead of retrying (TSI-2919).
func TestIsRetryableError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{
			"client timeout (context deadline exceeded)",
			&url.Error{Op: "Post", URL: "http://127.0.0.1/api/v1/upload/chunk/x/6", Err: context.DeadlineExceeded},
			true,
		},
		{"bare context deadline exceeded", context.DeadlineExceeded, true},
		{"connection reset", errors.New("read tcp: connection reset by peer"), true},
		{"temporary failure", errors.New("temporary network error"), true},
		{"service unavailable", errors.New("server 503 Service Unavailable"), true},
		{"auth rejected", errors.New("chunk upload failed: authentication rejected (HTTP 401)"), false},
		{"generic error", errors.New("some other failure"), false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isRetryableError(tc.err); got != tc.want {
				t.Errorf("isRetryableError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
