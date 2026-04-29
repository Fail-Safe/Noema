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
	// All pragmas go in the DSN so they apply to every connection in the pool.
	dsn := dbPath + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	conn, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening database: %w", err)
	}
	d := &DB{conn}
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

// CheckpointWAL runs PRAGMA wal_checkpoint(TRUNCATE) on the given cortex's
// SQLite database, forcing all pending WAL pages into the main file and
// truncating the WAL to zero bytes. Used by `noema cortex backup` so the
// tarball captures a consistent snapshot without a separate -wal sidecar.
//
// This bypasses db.Open's migration runner on purpose: backup is a read-
// mostly operation and must not upgrade the on-disk schema as a side
// effect (a v1 cortex should stay at v1 after being backed up). If the
// database file does not exist yet, the function is a no-op — there is
// nothing to checkpoint on a cortex that has never been opened.
func CheckpointWAL(cortexDir string) error {
	dbPath := filepath.Join(cortexDir, "db", "noema.db")
	if _, err := os.Stat(dbPath); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	dsn := dbPath + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
	conn, err := sql.Open("sqlite", dsn)
	if err != nil {
		return fmt.Errorf("opening database for checkpoint: %w", err)
	}
	defer conn.Close()
	if _, err := conn.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		return fmt.Errorf("wal_checkpoint: %w", err)
	}
	return nil
}

// splitSQL splits a SQL script into individual statements on semicolons,
// skipping comment and blank lines. Semicolons inside BEGIN...END blocks
// (trigger bodies) are preserved so the trigger stays intact as one
// statement — required for migration 008's long-term immutability triggers.
// Convention: BEGIN and END; each appear on their own line in migration
// files so the depth tracking can rely on line-level matching.
func splitSQL(script string) []string {
	var stmts []string
	var cur strings.Builder
	depth := 0
	for _, line := range strings.Split(script, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "--") || trimmed == "" {
			continue
		}
		cur.WriteString(line)
		cur.WriteByte('\n')
		upper := strings.ToUpper(trimmed)
		switch {
		case upper == "BEGIN":
			depth++
		case upper == "END;" && depth > 0:
			depth--
		}
		if depth == 0 && strings.HasSuffix(trimmed, ";") {
			stmts = append(stmts, strings.TrimSpace(cur.String()))
			cur.Reset()
		}
	}
	return stmts
}
