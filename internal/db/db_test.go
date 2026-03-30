package db_test

import (
	"os"
	"testing"

	"github.com/Fail-Safe/Noema/internal/db"
)

func TestOpen_CreatesSchema(t *testing.T) {
	conn, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer conn.Close()

	// schema_migrations must exist and have at least one entry.
	var count int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&count); err != nil {
		t.Fatalf("querying schema_migrations: %v", err)
	}
	if count == 0 {
		t.Error("no migrations recorded after Open")
	}

	// Core tables must exist.
	for _, table := range []string{"traces", "trace_tags"} {
		var name string
		if err := conn.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&name); err != nil {
			t.Errorf("table %q not found: %v", table, err)
		}
	}

	// FTS5 virtual table must exist.
	var name string
	if err := conn.QueryRow(`SELECT name FROM sqlite_master WHERE name='traces_fts'`).Scan(&name); err != nil {
		t.Errorf("traces_fts virtual table not found: %v", err)
	}
}

func TestOpen_MigrationIdempotent(t *testing.T) {
	dir := t.TempDir()

	conn1, err := db.Open(dir)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	var countAfterFirst int
	if err := conn1.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&countAfterFirst); err != nil {
		t.Fatalf("querying schema_migrations (first open): %v", err)
	}
	conn1.Close()

	conn2, err := db.Open(dir)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	defer conn2.Close()

	var countAfterSecond int
	if err := conn2.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&countAfterSecond); err != nil {
		t.Fatalf("querying schema_migrations (second open): %v", err)
	}
	if countAfterSecond != countAfterFirst {
		t.Errorf("schema_migrations count grew from %d to %d on second Open (migrations re-applied)", countAfterFirst, countAfterSecond)
	}
}

func TestOpen_ForeignKeysEnabled(t *testing.T) {
	conn, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer conn.Close()

	var enabled int
	if err := conn.QueryRow(`PRAGMA foreign_keys`).Scan(&enabled); err != nil {
		t.Fatalf("PRAGMA foreign_keys: %v", err)
	}
	if enabled != 1 {
		t.Error("foreign_keys must be enabled")
	}
}

func TestOpen_CreatesDBDirectory(t *testing.T) {
	dir := t.TempDir()
	conn, err := db.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	conn.Close()

	// db/ subdirectory and noema.db file must exist.
	if _, err := os.Stat(dir + "/db/noema.db"); err != nil {
		t.Errorf("noema.db not created at expected path: %v", err)
	}
}
