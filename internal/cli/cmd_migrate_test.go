package cli

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Fail-Safe/Noema/internal/config"
	"github.com/Fail-Safe/Noema/internal/cortex"
	"github.com/Fail-Safe/Noema/internal/db"
	"github.com/Fail-Safe/Noema/internal/trace"
)

// setupV1Cortex builds a fresh v2 cortex (the only kind cortex.Create knows
// how to write) and then degrades it back to v1 in place: clears the manifest
// id, drops the version to 1, blanks every cortex_id column, and replaces the
// vector clock keys with the cortex's display name. The result is a faithful
// stand-in for a cortex written by an older binary.
func setupV1Cortex(t *testing.T, name string) (dir string, traceID string) {
	t.Helper()
	parent := t.TempDir()
	if _, err := cortex.Create(name, parent); err != nil {
		t.Fatalf("Create: %v", err)
	}
	dir = filepath.Join(parent, name)

	// Add one trace through the live cortex so we have realistic rows in the
	// traces and events tables, then close it before we mutate state.
	cx, err := cortex.Open(name, dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	tr := trace.New("Migration Subject", "fact", "tester", []string{"migrate"}, "Body before migration.")
	if err := cx.Add(tr); err != nil {
		t.Fatalf("Add: %v", err)
	}
	traceID = tr.ID
	cx.Close()

	// Degrade the manifest back to v1: drop id, drop version, drop the
	// federation block. Read it raw, rewrite it, write it back.
	m, err := cortex.ReadManifest(dir)
	if err != nil {
		t.Fatalf("ReadManifest: %v", err)
	}
	originalID := m.ID
	if originalID == "" {
		t.Fatalf("Create produced manifest with no ID — test setup assumption broken")
	}
	m.ID = ""
	m.Version = 1
	if err := cortex.WriteManifest(dir, m); err != nil {
		t.Fatalf("WriteManifest: %v", err)
	}

	// Open the DB raw (bypassing cortex.Open's version check) and clear all
	// cortex_id columns plus rewrite the vclock to be name-keyed.
	dbPath := filepath.Join(dir, "db", "noema.db")
	conn, err := sql.Open("sqlite", dbPath+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer conn.Close()

	if _, err := conn.Exec(`UPDATE traces SET cortex_id = ''`); err != nil {
		t.Fatalf("clear traces.cortex_id: %v", err)
	}
	if _, err := conn.Exec(`UPDATE events SET cortex_id = ''`); err != nil {
		t.Fatalf("clear events.cortex_id: %v", err)
	}

	// Replace the existing vclock with a name-keyed one. The original ID had
	// at least one tick from the create event we added above.
	nameKeyedClock := map[string]uint64{name: 1}
	clockJSON, _ := json.Marshal(nameKeyedClock)
	if _, err := conn.Exec(
		`INSERT INTO federation_state (key, value) VALUES ('vclock', ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		string(clockJSON),
	); err != nil {
		t.Fatalf("rewrite vclock: %v", err)
	}
	return dir, traceID
}

func TestMigrateCortexID_HappyPath(t *testing.T) {
	const name = "migration-target"
	dir, traceID := setupV1Cortex(t, name)

	cfg := &config.Config{
		Default:  name,
		Cortexes: map[string]config.CortexEntry{name: {Path: dir}},
	}
	entry := cfg.Cortexes[name]

	var out bytes.Buffer
	in := strings.NewReader("y\n")
	if err := runCortexIDMigration(&out, in, cfg, name, entry, false, false); err != nil {
		t.Fatalf("runCortexIDMigration: %v\noutput:\n%s", err, out.String())
	}

	// Manifest should now be at v2 with a fresh 26-char ULID.
	m, err := cortex.ReadManifest(dir)
	if err != nil {
		t.Fatalf("ReadManifest: %v", err)
	}
	if m.Version != cortex.ManifestVersion {
		t.Errorf("manifest version = %d, want %d", m.Version, cortex.ManifestVersion)
	}
	if len(m.ID) != 26 {
		t.Errorf("manifest id = %q (len %d), want 26-char ULID", m.ID, len(m.ID))
	}
	newID := m.ID

	// Config should carry the new id.
	if cfg.Cortexes[name].ID != newID {
		t.Errorf("config entry id = %q, want %q", cfg.Cortexes[name].ID, newID)
	}

	// The trace and event rows should be re-keyed under the new ULID.
	conn, err := db.Open(dir)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer conn.Close()

	var tracesUnderNewID int
	if err := conn.QueryRow(
		`SELECT COUNT(*) FROM traces WHERE id = ? AND cortex_id = ?`, traceID, newID,
	).Scan(&tracesUnderNewID); err != nil {
		t.Fatalf("query traces: %v", err)
	}
	if tracesUnderNewID != 1 {
		t.Errorf("traces under new id = %d, want 1", tracesUnderNewID)
	}

	var eventsUnderNewID int
	if err := conn.QueryRow(
		`SELECT COUNT(*) FROM events WHERE trace_id = ? AND cortex_id = ?`, traceID, newID,
	).Scan(&eventsUnderNewID); err != nil {
		t.Fatalf("query events: %v", err)
	}
	if eventsUnderNewID < 1 {
		t.Errorf("events under new id = %d, want >= 1", eventsUnderNewID)
	}

	// Vector clock should now be keyed on the new ULID, not the cortex name.
	var clockJSON string
	if err := conn.QueryRow(
		`SELECT value FROM federation_state WHERE key = 'vclock'`,
	).Scan(&clockJSON); err != nil {
		t.Fatalf("read vclock: %v", err)
	}
	var vc map[string]uint64
	if err := json.Unmarshal([]byte(clockJSON), &vc); err != nil {
		t.Fatalf("parse vclock: %v", err)
	}
	if _, ok := vc[name]; ok {
		t.Errorf("vclock still has name-keyed bucket %q: %v", name, vc)
	}
	if vc[newID] == 0 {
		t.Errorf("vclock has no entry under new id %q: %v", newID, vc)
	}
}

func TestMigrateCortexID_Idempotent(t *testing.T) {
	const name = "already-migrated"
	parent := t.TempDir()
	if _, err := cortex.Create(name, parent); err != nil {
		t.Fatalf("Create: %v", err)
	}
	dir := filepath.Join(parent, name)

	m, _ := cortex.ReadManifest(dir)
	originalID := m.ID

	cfg := &config.Config{
		Default:  name,
		Cortexes: map[string]config.CortexEntry{name: {Path: dir, ID: originalID}},
	}
	var out bytes.Buffer
	if err := runCortexIDMigration(&out, strings.NewReader(""), cfg, name, cfg.Cortexes[name], false, true); err != nil {
		t.Fatalf("runCortexIDMigration: %v", err)
	}
	if !strings.Contains(out.String(), "already at manifest version") {
		t.Errorf("expected idempotent skip message, got:\n%s", out.String())
	}

	// Manifest must be untouched.
	m2, _ := cortex.ReadManifest(dir)
	if m2.ID != originalID {
		t.Errorf("idempotent run changed id: %q -> %q", originalID, m2.ID)
	}
}

// TestMigrateCortexID_ClearsStalePeerState pins the post-incident cleanup
// behavior described in cmd_migrate.go: a v1 cortex with federation peers
// configured may have accumulated name-keyed vclock buckets and pinned
// peer cortex_ids from before the cortex-id era. Both kinds of state
// must be wiped during migration so that:
//
//   - federation.Compare doesn't see ghost buckets that never advance
//     (false divergence detection once peers also migrate)
//   - peers re-pin against the new local id on next handshake instead of
//     refusing on a stale "peer identity mismatch"
func TestMigrateCortexID_ClearsStalePeerState(t *testing.T) {
	const name = "with-peers"
	dir, _ := setupV1Cortex(t, name)

	// Add federation peers to the v1 manifest so the migration can
	// recognize their names when scrubbing the vclock.
	m, err := cortex.ReadManifest(dir)
	if err != nil {
		t.Fatalf("ReadManifest: %v", err)
	}
	m.Federation = &cortex.FederationConfig{
		Peers: []cortex.PeerEntry{
			{Name: "ai-2", Endpoint: "https://ai-2.example:3000"},
			{Name: "ai-3", Endpoint: "https://ai-3.example:3000"},
		},
	}
	if err := cortex.WriteManifest(dir, m); err != nil {
		t.Fatalf("WriteManifest with peers: %v", err)
	}

	// Inject stale state directly into the DB:
	//   - vclock has the local name bucket (set up by setupV1Cortex) plus
	//     two name-keyed peer buckets that should be cleared
	//   - federation_state has two peer cortex_id pins that should be cleared
	dbPath := filepath.Join(dir, "db", "noema.db")
	conn, err := sql.Open("sqlite", dbPath+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer conn.Close()

	staleClock := map[string]uint64{name: 1, "ai-2": 5, "ai-3": 7}
	clockJSON, _ := json.Marshal(staleClock)
	if _, err := conn.Exec(
		`INSERT INTO federation_state (key, value) VALUES ('vclock', ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		string(clockJSON),
	); err != nil {
		t.Fatalf("inject vclock: %v", err)
	}
	for _, peer := range []struct {
		name string
		id   string
	}{
		{"ai-2", "01JOLD000000000000000AI200"},
		{"ai-3", "01JOLD000000000000000AI300"},
	} {
		if _, err := conn.Exec(
			`INSERT INTO federation_state (key, value) VALUES (?, ?)`,
			"peer:"+peer.name+":cortex_id", peer.id,
		); err != nil {
			t.Fatalf("inject peer pin %s: %v", peer.name, err)
		}
	}
	conn.Close()

	cfg := &config.Config{
		Default:  name,
		Cortexes: map[string]config.CortexEntry{name: {Path: dir}},
	}
	var out bytes.Buffer
	if err := runCortexIDMigration(&out, strings.NewReader("y\n"), cfg, name, cfg.Cortexes[name], false, false); err != nil {
		t.Fatalf("runCortexIDMigration: %v\noutput:\n%s", err, out.String())
	}
	output := out.String()
	if !strings.Contains(output, "stale peer buckets cleared: 2") {
		t.Errorf("expected 'stale peer buckets cleared: 2' in output, got:\n%s", output)
	}
	if !strings.Contains(output, "peer cortex_id pins cleared: 2") {
		t.Errorf("expected 'peer cortex_id pins cleared: 2' in output, got:\n%s", output)
	}

	// Verify the post-migration vclock has only the new ULID and no
	// trace of the cleared peer-name buckets.
	conn2, err := db.Open(dir)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer conn2.Close()
	newID := cfg.Cortexes[name].ID
	if newID == "" {
		t.Fatal("config entry missing new id after migration")
	}

	var clockRaw string
	if err := conn2.QueryRow(
		`SELECT value FROM federation_state WHERE key = 'vclock'`,
	).Scan(&clockRaw); err != nil {
		t.Fatalf("read vclock: %v", err)
	}
	var vc map[string]uint64
	if err := json.Unmarshal([]byte(clockRaw), &vc); err != nil {
		t.Fatalf("parse vclock: %v", err)
	}
	if _, lingering := vc["ai-2"]; lingering {
		t.Errorf("vclock still has ai-2 bucket: %v", vc)
	}
	if _, lingering := vc["ai-3"]; lingering {
		t.Errorf("vclock still has ai-3 bucket: %v", vc)
	}
	if vc[newID] == 0 {
		t.Errorf("vclock missing entry under new id %q: %v", newID, vc)
	}

	// Both peer cortex_id pins must be gone — they're rebuilt from
	// scratch on the next successful handshake.
	var pinCount int
	if err := conn2.QueryRow(
		`SELECT COUNT(*) FROM federation_state WHERE key LIKE 'peer:%:cortex_id'`,
	).Scan(&pinCount); err != nil {
		t.Fatalf("count peer pins: %v", err)
	}
	if pinCount != 0 {
		t.Errorf("expected 0 peer cortex_id pins after migration, got %d", pinCount)
	}
}

func TestMigrateCortexID_AbortsOnNo(t *testing.T) {
	const name = "abort-target"
	dir, _ := setupV1Cortex(t, name)
	cfg := &config.Config{
		Default:  name,
		Cortexes: map[string]config.CortexEntry{name: {Path: dir}},
	}

	var out bytes.Buffer
	err := runCortexIDMigration(&out, strings.NewReader("n\n"), cfg, name, cfg.Cortexes[name], false, false)
	if err == nil || !strings.Contains(err.Error(), "aborted") {
		t.Fatalf("expected abort error, got %v", err)
	}

	// Manifest must still be at v1.
	m, _ := cortex.ReadManifest(dir)
	if m.Version != 1 || m.ID != "" {
		t.Errorf("manifest mutated despite abort: version=%d id=%q", m.Version, m.ID)
	}
}

