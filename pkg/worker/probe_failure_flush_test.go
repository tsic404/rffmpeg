package worker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/tsix404/rffmpeg/pkg/protocol"
)

// TestProcessProbeJobDirectPathFailuresReportWithoutFlush locks the TSI-2665
// convention: the two direct-path failure branches in processProbeJob pass an
// explicit nil flush placeholder to reportFailureWithType, matching the
// flushStderr call sites in processJob. A nil flush must be a safe no-op —
// the terminal failed PATCH still goes out with the correct classification,
// and the nil-func path must not panic.
func TestProcessProbeJobDirectPathFailuresReportWithoutFlush(t *testing.T) {
	cases := []struct {
		name            string
		directPath      string
		wantDetails     string
		wantErrContains string
	}{
		{
			name:        "traversal rejected",
			directPath:  "safe/../etc/passwd",
			wantDetails: "path traversal rejected",
		},
		{
			name:            "stat unreachable",
			directPath:      filepath.Join(t.TempDir(), "does-not-exist.mkv"),
			wantErrContains: "input path unreachable",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var terminal protocol.JobUpdateRequest
			terminalSeen := false

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPatch {
					w.WriteHeader(http.StatusOK)
					return
				}
				var update protocol.JobUpdateRequest
				if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
					t.Errorf("decode request body: %v", err)
					w.WriteHeader(http.StatusOK)
					return
				}
				if protocol.IsTerminalStatus(update.Status) {
					mu.Lock()
					terminal = update
					terminalSeen = true
					mu.Unlock()
				}
				w.WriteHeader(http.StatusOK)
				w.Write([]byte(`{"message":"ok"}`))
			}))
			defer srv.Close()

			client := NewClient(srv.URL, "worker-1", "")
			w := &Worker{client: client, tempDir: t.TempDir()}

			job := protocol.JobInfo{
				ID:          "probe-fail-001",
				InputFiles:  []string{tc.directPath},
				Args:        []string{"__rffmpeg_probe__", tc.directPath},
				DirectPaths: []string{tc.directPath},
			}
			w.processProbeJob(context.Background(), job)

			mu.Lock()
			seen, got := terminalSeen, terminal
			mu.Unlock()

			if !seen {
				t.Fatal("expected a terminal failed PATCH, got none")
			}
			if got.Status != protocol.JobStatusFailed {
				t.Errorf("Status = %q, want %q", got.Status, protocol.JobStatusFailed)
			}
			if got.FailureType != string(protocol.FailureInputUnreachable) {
				t.Errorf("FailureType = %q, want %q", got.FailureType, protocol.FailureInputUnreachable)
			}
			if tc.wantDetails != "" && got.FailureDetails != tc.wantDetails {
				t.Errorf("FailureDetails = %q, want %q", got.FailureDetails, tc.wantDetails)
			}
			if tc.wantErrContains != "" && !strings.Contains(got.Error, tc.wantErrContains) {
				t.Errorf("Error = %q, want it to contain %q", got.Error, tc.wantErrContains)
			}
		})
	}
}
