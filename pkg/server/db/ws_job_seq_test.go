package db_test

import (
	"testing"
)

// TestWSJobSeqPersistence covers the TSI-2379 store contract: save upserts,
// load restores (0 when absent), delete clears.
func TestWSJobSeqPersistence(t *testing.T) {
	database, cleanup := setupDBTest(t)
	defer cleanup()

	// Absent job loads as 0 — fresh jobs start numbering at 1.
	got, err := database.LoadWSJobSeq("job-fresh")
	if err != nil {
		t.Fatalf("load absent: %v", err)
	}
	if got != 0 {
		t.Fatalf("absent seq must load as 0, got %d", got)
	}

	// Save then reload.
	if err := database.SaveWSJobSeq("job-a", 7); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err = database.LoadWSJobSeq("job-a")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got != 7 {
		t.Fatalf("expected persisted seq 7, got %d", got)
	}

	// Save again on the same job upserts instead of failing on the PK.
	if err := database.SaveWSJobSeq("job-a", 12); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, err = database.LoadWSJobSeq("job-a")
	if err != nil {
		t.Fatalf("load after upsert: %v", err)
	}
	if got != 12 {
		t.Fatalf("expected upserted seq 12, got %d", got)
	}

	// Delete clears the row; a subsequent load is back to 0.
	if err := database.DeleteWSJobSeq("job-a"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	got, err = database.LoadWSJobSeq("job-a")
	if err != nil {
		t.Fatalf("load after delete: %v", err)
	}
	if got != 0 {
		t.Fatalf("deleted seq must load as 0, got %d", got)
	}

	// Deleting an absent row is a no-op, not an error.
	if err := database.DeleteWSJobSeq("job-never-saved"); err != nil {
		t.Fatalf("delete absent: %v", err)
	}
}
