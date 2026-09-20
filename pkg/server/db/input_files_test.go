package db_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/tsic404/rffmpeg/pkg/protocol"
)

func inputFilesJSON(t *testing.T, ids []string) string {
	t.Helper()
	b, err := json.Marshal(ids)
	if err != nil {
		t.Fatalf("marshal input files: %v", err)
	}
	return string(b)
}

func TestActiveInputFileIDs(t *testing.T) {
	database, cleanup := setupDBTest(t)
	defer cleanup()

	// Pending job references two blobs.
	if _, err := database.CreateJob(inputFilesJSON(t, []string{"aaaa", "bbbb"}), `["-i","in"]`, "out.mp4", false); err != nil {
		t.Fatalf("create pending job: %v", err)
	}

	// Running job references "bbbb" (overlap) and "dddd".
	running, err := database.CreateJob(inputFilesJSON(t, []string{"bbbb", "dddd"}), `["-i","in"]`, "out3.mp4", false)
	if err != nil {
		t.Fatalf("create running job: %v", err)
	}
	if err := database.UpdateJobStatusWithFailure(running.ID, protocol.JobStatusRunning, nil, nil, nil, nil); err != nil {
		t.Fatalf("set running: %v", err)
	}

	// Completed job references "cccc" — terminal, must not be active.
	done, err := database.CreateJob(inputFilesJSON(t, []string{"cccc"}), `["-i","in"]`, "out2.mp4", false)
	if err != nil {
		t.Fatalf("create completed job: %v", err)
	}
	if err := database.UpdateJobStatusWithFailure(done.ID, protocol.JobStatusCompleted, nil, nil, nil, nil); err != nil {
		t.Fatalf("set completed: %v", err)
	}

	ids, err := database.ActiveInputFileIDs()
	if err != nil {
		t.Fatalf("ActiveInputFileIDs: %v", err)
	}

	for _, want := range []string{"aaaa", "bbbb", "dddd"} {
		if _, ok := ids[want]; !ok {
			t.Errorf("expected %q in active IDs, got %v", want, ids)
		}
	}
	if _, ok := ids["cccc"]; ok {
		t.Errorf("terminal job's input %q must not be active", "cccc")
	}
}

func TestActiveInputFileIDsMalformedRowFailsClosed(t *testing.T) {
	database, cleanup := setupDBTest(t)
	defer cleanup()

	now := time.Now()
	if _, err := database.GetDB().Exec(
		`INSERT INTO jobs (id, status, input_files, args, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)`,
		"bad-json", protocol.JobStatusPending, "{not-json", "[]", now, now,
	); err != nil {
		t.Fatalf("insert malformed job: %v", err)
	}

	if _, err := database.ActiveInputFileIDs(); err == nil {
		t.Fatal("malformed input_files row must fail the query (fail closed), not be skipped")
	}
}
