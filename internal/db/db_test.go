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
	for _, table := range []string{"traces", "trace_tags", "events", "trace_lineage", "federation_state"} {
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

func TestMigration018_RepairsConsolidatedTraceTiers(t *testing.T) {
	dir := t.TempDir()
	conn, err := db.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	for _, row := range []struct {
		id    string
		title string
	}{
		{"20260725-distilled", "distilled"},
		{"20260725-ordinary", "ordinary"},
	} {
		if _, err := conn.Exec(
			`INSERT INTO traces (id, title, type, tier, created_at, updated_at) VALUES (?, ?, 'note', 'short', ?, ?)`,
			row.id, row.title, "2026-07-25T00:00:00Z", "2026-07-25T00:00:00Z",
		); err != nil {
			t.Fatalf("seed trace %s: %v", row.id, err)
		}
	}
	if _, err := conn.Exec(
		`INSERT INTO events (id, action, trace_id, timestamp, data) VALUES (?, 'consolidate', ?, ?, ?)`,
		"01KTESTCONSOLIDATE000000000", "20260725-distilled", "2026-07-25T00:00:01Z",
		`{"distilled_id":"20260725-distilled"}`,
	); err != nil {
		t.Fatalf("seed consolidate event: %v", err)
	}
	if _, err := conn.Exec(`DELETE FROM schema_migrations WHERE version = 18`); err != nil {
		t.Fatalf("reset migration 018: %v", err)
	}
	if err := conn.Close(); err != nil {
		t.Fatalf("close before migration replay: %v", err)
	}

	conn, err = db.Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer conn.Close()

	var distilledTier, ordinaryTier string
	if err := conn.QueryRow(`SELECT tier FROM traces WHERE id = '20260725-distilled'`).Scan(&distilledTier); err != nil {
		t.Fatalf("read distilled tier: %v", err)
	}
	if err := conn.QueryRow(`SELECT tier FROM traces WHERE id = '20260725-ordinary'`).Scan(&ordinaryTier); err != nil {
		t.Fatalf("read ordinary tier: %v", err)
	}
	if distilledTier != "mid" {
		t.Errorf("distilled tier = %q, want mid", distilledTier)
	}
	if ordinaryTier != "short" {
		t.Errorf("ordinary tier = %q, want short", ordinaryTier)
	}
}

func TestMigration019_RefoldsPendingTierHistory(t *testing.T) {
	dir := t.TempDir()
	conn, err := db.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	for _, row := range []struct {
		id   string
		tier string
	}{
		{"20260725-late-consolidate", "short"},
		{"20260725-late-promotions", "mid"},
		{"20260725-late-demotion", "mid"},
	} {
		if _, err := conn.Exec(
			`INSERT INTO traces (id, title, type, tier, created_at, updated_at) VALUES (?, ?, 'note', ?, ?, ?)`,
			row.id, row.id, row.tier, "2026-07-25T00:00:00Z", "2026-07-25T00:00:00Z",
		); err != nil {
			t.Fatalf("seed trace %s: %v", row.id, err)
		}
	}
	events := []struct {
		id      string
		action  string
		traceID string
		data    string
	}{
		{"01KTEST0190000000000000001", "consolidate", "20260725-late-consolidate", `{"distilled_id":"20260725-late-consolidate"}`},
		{"01KTEST0190000000000000002", "promote", "20260725-late-promotions", `{"from":"short","to":"mid"}`},
		{"01KTEST0190000000000000003", "promote", "20260725-late-promotions", `{"from":"mid","to":"long"}`},
		{"01KTEST0190000000000000004", "demote", "20260725-late-demotion", `{"from":"mid","to":"short"}`},
	}
	for _, e := range events {
		if _, err := conn.Exec(
			`INSERT INTO events (id, action, trace_id, timestamp, data) VALUES (?, ?, ?, ?, ?)`,
			e.id, e.action, e.traceID, "2026-07-25T00:00:01Z", e.data,
		); err != nil {
			t.Fatalf("seed event %s: %v", e.id, err)
		}
	}
	if _, err := conn.Exec(`DELETE FROM schema_migrations WHERE version = 19`); err != nil {
		t.Fatalf("reset migration 019: %v", err)
	}
	if err := conn.Close(); err != nil {
		t.Fatalf("close before migration replay: %v", err)
	}

	conn, err = db.Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer conn.Close()

	for id, want := range map[string]string{
		"20260725-late-consolidate": "mid",
		"20260725-late-promotions":  "long",
		"20260725-late-demotion":    "short",
	} {
		var got string
		if err := conn.QueryRow(`SELECT tier FROM traces WHERE id = ?`, id).Scan(&got); err != nil {
			t.Fatalf("read %s tier: %v", id, err)
		}
		if got != want {
			t.Errorf("%s tier = %q, want %q", id, got, want)
		}
	}
}

// ---- Migration 008: memory tiering ----

func TestMigration008_TierColumnExistsAndDefaults(t *testing.T) {
	conn, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer conn.Close()

	rows, err := conn.Query(`PRAGMA table_info(traces)`)
	if err != nil {
		t.Fatalf("PRAGMA table_info: %v", err)
	}
	defer rows.Close()
	cols := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt *string
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			t.Fatalf("Scan: %v", err)
		}
		cols[name] = true
	}
	for _, col := range []string{"tier", "read_count", "modify_count", "last_read_at", "tier_votes"} {
		if !cols[col] {
			t.Errorf("traces.%s missing after migration 008", col)
		}
	}
}

func TestMigration008_CheckConstraintRejectsBadTier(t *testing.T) {
	conn, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer conn.Close()

	_, err = conn.Exec(
		`INSERT INTO traces (id, title, type, tier, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)`,
		"20260419-bad", "bad tier", "note", "archived", "2026-04-19T00:00:00Z", "2026-04-19T00:00:00Z",
	)
	if err == nil {
		t.Fatal("CHECK constraint let 'archived' through")
	}
}

func TestMigration008_LongTermImmutableOnUpdate(t *testing.T) {
	conn, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer conn.Close()

	if _, err := conn.Exec(
		`INSERT INTO traces (id, title, type, tier, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)`,
		"20260419-stone", "immutable", "fact", "long", "2026-04-19T00:00:00Z", "2026-04-19T00:00:00Z",
	); err != nil {
		t.Fatalf("seed long-term row: %v", err)
	}

	_, err = conn.Exec(`UPDATE traces SET title = ? WHERE id = ?`, "edited", "20260419-stone")
	if err == nil {
		t.Fatal("trigger did not block UPDATE on tier='long' row")
	}

	// Demotion escape hatch: UPDATE that changes NEW.tier away from 'long'
	// must be allowed so admin recovery paths can move a trace out of
	// long-term without first dropping the trigger.
	if _, err := conn.Exec(`UPDATE traces SET tier = ? WHERE id = ?`, "short", "20260419-stone"); err != nil {
		t.Errorf("demotion from long to short blocked: %v", err)
	}
}

func TestMigration008_LongTermImmutableOnDelete(t *testing.T) {
	conn, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer conn.Close()

	if _, err := conn.Exec(
		`INSERT INTO traces (id, title, type, tier, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)`,
		"20260419-deep", "no-delete", "fact", "long", "2026-04-19T00:00:00Z", "2026-04-19T00:00:00Z",
	); err != nil {
		t.Fatalf("seed long-term row: %v", err)
	}

	_, err = conn.Exec(`DELETE FROM traces WHERE id = ?`, "20260419-deep")
	if err == nil {
		t.Fatal("trigger did not block DELETE on tier='long' row")
	}
}

func TestMigration008_ShortTermUnaffected(t *testing.T) {
	conn, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer conn.Close()

	if _, err := conn.Exec(
		`INSERT INTO traces (id, title, type, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
		"20260419-flex", "routine", "note", "2026-04-19T00:00:00Z", "2026-04-19T00:00:00Z",
	); err != nil {
		t.Fatalf("seed short row: %v", err)
	}
	if _, err := conn.Exec(`UPDATE traces SET title = ? WHERE id = ?`, "renamed", "20260419-flex"); err != nil {
		t.Errorf("UPDATE on default-tier row blocked: %v", err)
	}
	if _, err := conn.Exec(`DELETE FROM traces WHERE id = ?`, "20260419-flex"); err != nil {
		t.Errorf("DELETE on default-tier row blocked: %v", err)
	}
}
