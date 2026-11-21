package worker_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tsic404/rffmpeg/pkg/protocol"
	"github.com/tsic404/rffmpeg/pkg/worker"
)

// TestUpdateJobWithFailureSendsPushConnections pins the worker half of the
// contract: a report that carries a list puts it in the request body, and a
// report that carries none omits the key so the server keeps the stored list.
func TestUpdateJobWithFailureSendsPushConnections(t *testing.T) {
	var captured []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("read body: %v", err)
			}
			captured = body
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"message":"ok"}`))
	}))
	defer srv.Close()

	client := worker.NewClient(srv.URL, "my-worker-id", "")
	conns := []protocol.PushConnection{
		{Family: "rtmp", Purpose: "session", Target: "10.0.0.1:1935", Origin: "request", Result: "ok"},
	}
	if err := client.UpdateJobWithFailure("job-1", protocol.JobStatusCompleted, 0, "", false, "", "", conns...); err != nil {
		t.Fatal(err)
	}

	var withList protocol.JobUpdateRequest
	if err := json.Unmarshal(captured, &withList); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	if len(withList.PushConnections) != 1 || withList.PushConnections[0] != conns[0] {
		t.Fatalf("push_connections = %+v, want %+v", withList.PushConnections, conns)
	}
	if withList.WorkerID != "my-worker-id" {
		t.Fatalf("worker_id = %q, want the ownership guard value", withList.WorkerID)
	}

	if err := client.UpdateJobWithFailure("job-1", protocol.JobStatusCompleted, 0, "", false, "", ""); err != nil {
		t.Fatal(err)
	}
	var withoutList map[string]json.RawMessage
	if err := json.Unmarshal(captured, &withoutList); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	if _, present := withoutList["push_connections"]; present {
		t.Fatalf("list-less report must omit push_connections, got %s", captured)
	}
}
