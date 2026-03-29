package db

import (
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

type DB struct {
	*sql.DB
}

func Open(cortexDir string) (*DB, error) {
	dbDir := filepath.Join(cortexDir, "db")
	if err := os.MkdirAll(dbDir, 0o750); err != nil {
		return nil, fmt.Errorf("creating db dir: %w", err)
	}
	dbPath := filepath.Join(dbDir, "noema.db")
	conn, err := sql.Open("sqlite", dbPath+"?_journal_mode=WAL")
	if err != nil {
		return nil, fmt.Errorf("opening database: %w", err)
	}
	d := &DB{conn}
	// DSN pragma params are unreliable across drivers; set explicitly after open.
	if _, err := conn.Exec(`PRAGMA foreign_keys = ON`); err != nil {
		conn.Close()
		return nil, fmt.Errorf("enabling foreign keys: %w", err)
	}
	if err := d.migrate(); err != nil {
		conn.Close()
		return nil, err
	}
	return d, nil
}

func (d *DB) migrate() error {
	if _, err := d.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER NOT NULL PRIMARY KEY)`); err != nil {
		return fmt.Errorf("creating migrations table: %w", err)
	}

	entries, err := fs.Glob(migrationsFS, "migrations/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(entries)

	for _, entry := range entries {
		base := filepath.Base(entry)
		var version int
		if _, err := fmt.Sscanf(base, "%d_", &version); err != nil {
			continue
		}

		var count int
		if err := d.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version = ?`, version).Scan(&count); err != nil {
			return err
		}
		if count > 0 {
			continue
		}

		content, err := fs.ReadFile(migrationsFS, entry)
		if err != nil {
			return fmt.Errorf("reading migration %s: %w", base, err)
		}
		for _, stmt := range splitSQL(string(content)) {
			if _, err := d.Exec(stmt); err != nil {
				return fmt.Errorf("migration %s: %w\n  statement: %s", base, err, stmt)
			}
		}

		if _, err := d.Exec(`INSERT INTO schema_migrations (version) VALUES (?)`, version); err != nil {
			return fmt.Errorf("recording migration %s: %w", base, err)
		}
	}
	return nil
}

// splitSQL splits a SQL script into individual statements on semicolons,
// skipping comment and blank lines.
func splitSQL(script string) []string {
	var stmts []string
	var cur strings.Builder
	for _, line := range strings.Split(script, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "--") || trimmed == "" {
			continue
		}
		cur.WriteString(line)
		cur.WriteByte('\n')
		if strings.HasSuffix(trimmed, ";") {
			stmts = append(stmts, strings.TrimSpace(cur.String()))
			cur.Reset()
		}
	}
	return stmts
}
