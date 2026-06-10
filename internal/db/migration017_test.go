package db_test

import (
	"testing"

	"github.com/Fail-Safe/Noema/internal/db"
)

// ---- Migration 017: event signature + pubkey ----

// TestMigration017_ColumnsAndSchema locks in the wire-format guarantee from
// 017_event_signature.sql: both columns exist on events, are NOT NULL, and
// default to the empty (= unsigned) sentinel. The defaults are load-bearing —
// scanEvents never special-cases NULL, and an empty signature is the agreed
// "produced before signing was configured" marker.
func TestMigration017_ColumnsAndSchema(t *testing.T) {
	conn, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer conn.Close()

	rows, err := conn.Query(`PRAGMA table_info(events)`)
	if err != nil {
		t.Fatalf("PRAGMA table_info: %v", err)
	}
	defer rows.Close()
	type colInfo struct {
		notnull int
		dflt    string
	}
	cols := map[string]colInfo{}
	for rows.Next() {
		var cid, notnull, pk int
		var name, ctype string
		var dflt *string
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			t.Fatalf("Scan: %v", err)
		}
		var d string
		if dflt != nil {
			d = *dflt
		}
		cols[name] = colInfo{notnull: notnull, dflt: d}
	}
	for _, c := range []string{"signature", "pubkey"} {
		info, ok := cols[c]
		if !ok {
			t.Errorf("events.%s missing after migration 017", c)
			continue
		}
		if info.notnull != 1 {
			t.Errorf("events.%s should be NOT NULL", c)
		}
		// SQLite renders a string default as the quoted literal ''.
		if info.dflt != "''" {
			t.Errorf("events.%s default = %q, want \"''\"", c, info.dflt)
		}
	}
}

// TestMigration017_BackfillsUnsignedSentinel is the backward-compat regression:
// a row inserted without the signing columns (the shape every pre-017 event
// has) must read back as the empty sentinel rather than NULL. This is exactly
// what the NOT NULL DEFAULT '' columns guarantee for rows that predate signing,
// and what lets a mixed-version ring treat old events as cleanly unsigned.
func TestMigration017_BackfillsUnsignedSentinel(t *testing.T) {
	conn, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer conn.Close()

	// Insert using only the pre-signing columns, omitting signature/pubkey.
	if _, err := conn.Exec(
		`INSERT INTO events (id, action, trace_id, cortex_id, origin, timestamp) VALUES (?, ?, ?, ?, ?, ?)`,
		"01EVENT017", "create", "20260610-t", "01CORTEX", "peer-a", "2026-06-10T00:00:00Z",
	); err != nil {
		t.Fatalf("insert pre-signing event: %v", err)
	}

	var sig, pub string
	if err := conn.QueryRow(
		`SELECT signature, pubkey FROM events WHERE id = ?`, "01EVENT017",
	).Scan(&sig, &pub); err != nil {
		t.Fatalf("read back event: %v", err)
	}
	if sig != "" {
		t.Errorf("signature = %q, want empty sentinel", sig)
	}
	if pub != "" {
		t.Errorf("pubkey = %q, want empty sentinel", pub)
	}
}
