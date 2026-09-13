package db_test

// Tests for TSI-3122 (startup schema gate): New must fail, not silently
// recreate, when a pre-existing database is missing a required table or
// column, or is structurally corrupt. These tests construct the "existing
// database in a broken state" scenario directly (bypassing initTables) so they
// exercise the actual startup path rather than a state that cannot occur in it.

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/tsic404/rffmpeg/pkg/server/db"
)

// openRaw opens the database file without any schema initialization, so a test
// can construct an "existing" database in an arbitrary state.
func openRaw(t *testing.T, path string) *sql.DB {
	t.Helper()
	raw, err := sql.Open("sqlite3", "file:"+path+"?_journal_mode=WAL")
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	return raw
}

// TestNewFailsOnExistingDBMissingTable simulates the TSI-3122 restart failure:
// an existing database whose WAL recovery did not land is missing a required
// table. New must fail instead of silently recreating it empty.
func TestNewFailsOnExistingDBMissingTable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")

	raw := openRaw(t, path)
	// A partial schema: files exists, jobs does not — as a half-landed WAL
	// recovery would leave it.
	if _, err := raw.Exec(`CREATE TABLE files (
		id TEXT PRIMARY KEY, filename TEXT NOT NULL, path TEXT NOT NULL,
		size INTEGER NOT NULL, checksum TEXT, created_at DATETIME NOT NULL
	)`); err != nil {
		t.Fatalf("create partial schema: %v", err)
	}
	raw.Close()

	_, err := db.New(path)
	if err == nil {
		t.Fatalf("New should fail on an existing database missing the jobs table")
	}
	if !strings.Contains(err.Error(), "jobs") {
		t.Fatalf("error should name the missing jobs table, got: %v", err)
	}
}

// TestNewFailsOnExistingDBMissingColumn simulates a structurally damaged
// table: present under the right name but missing a column the server reads.
// A name-only check would pass this; the column check must catch it.
func TestNewFailsOnExistingDBMissingColumn(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")

	full, err := db.New(path)
	if err != nil {
		t.Fatalf("create full db: %v", err)
	}
	if err := full.Close(); err != nil {
		t.Fatalf("close full db: %v", err)
	}

	raw := openRaw(t, path)
	if _, err := raw.Exec(`ALTER TABLE jobs DROP COLUMN eta_seconds`); err != nil {
		t.Fatalf("drop column: %v", err)
	}
	raw.Close()

	_, err = db.New(path)
	if err == nil {
		t.Fatalf("New should fail on an existing database missing a required column")
	}
	if !strings.Contains(err.Error(), "eta_seconds") {
		t.Fatalf("error should name the missing column, got: %v", err)
	}
}

// TestNewFailsOnCorruptExistingDB corrupts a page and verifies integrity_check
// fails startup.
func TestNewFailsOnCorruptExistingDB(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")

	full, err := db.New(path)
	if err != nil {
		t.Fatalf("create full db: %v", err)
	}
	if err := full.Close(); err != nil {
		t.Fatalf("close full db: %v", err)
	}

	// Corrupt the type byte of page 2 (the first user-table root page) to an
	// invalid value. integrity_check detects the malformed page while the
	// schema page (page 1) stays readable, so New reaches verifyIntegrity.
	corruptPageHeader(t, path, 8192)

	_, err = db.New(path)
	if err == nil {
		t.Fatalf("New should fail on a corrupt database")
	}
	if !strings.Contains(err.Error(), "integrity") {
		t.Fatalf("error should report an integrity failure, got: %v", err)
	}
}

func corruptPageHeader(t *testing.T, path string, offset int) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read db file: %v", err)
	}
	if offset >= len(b) {
		t.Fatalf("offset %d beyond file size %d", offset, len(b))
	}
	b[offset] = 0x00
	if err := os.WriteFile(path, b, 0644); err != nil {
		t.Fatalf("write db file: %v", err)
	}
}
