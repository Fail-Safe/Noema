package db

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func TestApplyMigrationRollsBackStatementsAndVersionOnFailure(t *testing.T) {
	conn, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "migration.db"))
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer conn.Close()

	d := &DB{conn}
	if _, err := d.Exec(`CREATE TABLE schema_migrations (version INTEGER NOT NULL PRIMARY KEY)`); err != nil {
		t.Fatalf("create migrations table: %v", err)
	}

	err = d.applyMigration("999_atomicity.sql", 999, []byte(`
CREATE TABLE partial_migration (id INTEGER PRIMARY KEY);
INSERT INTO table_that_does_not_exist (id) VALUES (1);
`))
	if err == nil {
		t.Fatal("applyMigration succeeded with an invalid statement")
	}

	var tableCount int
	if err := d.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'partial_migration'`,
	).Scan(&tableCount); err != nil {
		t.Fatalf("query partial table: %v", err)
	}
	if tableCount != 0 {
		t.Error("successful statement from failed migration was not rolled back")
	}

	var versionCount int
	if err := d.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version = 999`).Scan(&versionCount); err != nil {
		t.Fatalf("query migration version: %v", err)
	}
	if versionCount != 0 {
		t.Error("failed migration version was recorded")
	}
}
