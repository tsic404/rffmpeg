package db

// Internal test for the foreign-key assertion of verifyIntegrity, which the
// public sqliteDSN always sets on (via _fk=1) and therefore cannot be reached
// through New. Opening a raw connection without _fk makes PRAGMA foreign_keys
// default to OFF.

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

func TestVerifyIntegrityDetectsForeignKeysOff(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")

	// No _fk pragma in the DSN -> PRAGMA foreign_keys defaults to OFF.
	raw, err := sql.Open("sqlite3", "file:"+path)
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	defer raw.Close()

	d := &Database{db: raw, notifier: newJobNotifier()}
	if err := d.initTables(); err != nil {
		t.Fatalf("initTables: %v", err)
	}

	err = d.verifyIntegrity()
	if err == nil {
		t.Fatalf("verifyIntegrity should fail with foreign keys off")
	}
	if !strings.Contains(err.Error(), "foreign_keys") {
		t.Fatalf("error should name foreign_keys, got: %v", err)
	}
}
