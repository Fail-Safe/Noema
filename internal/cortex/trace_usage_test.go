package cortex_test

import (
	"path/filepath"
	"testing"

	"github.com/Fail-Safe/Noema/internal/cortex"
	"github.com/Fail-Safe/Noema/internal/trace"
)

// TestTraceUsage_BackfillPopulatesLocalRows pins the one-shot backfill
// behavior: when a cortex opens with schema 014 applied but an empty
// trace_usage table and pre-existing traces, the backfill credits the
// historical {read_count, modify_count, last_read_at} to the local
// peer's cortex ID. This keeps the heuristic operating on the same
// signal as before the refactor.
func TestTraceUsage_BackfillPopulatesLocalRows(t *testing.T) {
	dir := t.TempDir()
	m, err := cortex.Create("bk", dir)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	cx, err := cortex.Open("bk", filepath.Join(dir, "bk"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { cx.Close() })

	tr := trace.New("backfill me", "note", "", nil, "body")
	if err := cx.Add(tr); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Simulate a pre-PR-A cortex: zero out trace_usage (as if migration
	// 014 had just been applied for the first time), stamp legacy
	// counters into traces.{read_count,modify_count,last_read_at}.
	if _, err := cx.DB.Exec(`DELETE FROM trace_usage`); err != nil {
		t.Fatalf("clearing trace_usage: %v", err)
	}
	if _, err := cx.DB.Exec(
		`UPDATE traces SET read_count = 7, modify_count = 2, last_read_at = '2026-04-20T00:00:00Z' WHERE id = ?`,
		tr.ID,
	); err != nil {
		t.Fatalf("seeding legacy counters: %v", err)
	}

	// Close and reopen — backfill should fire on the reopen because
	// trace_usage is empty and traces has rows.
	cx.Close()
	cx2, err := cortex.Open("bk", filepath.Join(dir, "bk"))
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { cx2.Close() })

	var reads, mods int
	var last *string
	var peerID string
	err = cx2.DB.QueryRow(
		`SELECT read_count, modify_count, last_read_at, peer_cortex_id FROM trace_usage WHERE trace_id = ?`,
		tr.ID,
	).Scan(&reads, &mods, &last, &peerID)
	if err != nil {
		t.Fatalf("read trace_usage after backfill: %v", err)
	}
	if reads != 7 {
		t.Errorf("backfilled read_count = %d, want 7", reads)
	}
	if mods != 2 {
		t.Errorf("backfilled modify_count = %d, want 2", mods)
	}
	if last == nil || *last != "2026-04-20T00:00:00Z" {
		t.Errorf("backfilled last_read_at = %v, want 2026-04-20T00:00:00Z", last)
	}
	if peerID != m.ID {
		t.Errorf("backfill attributed to peer %q, want local cortex ID %q", peerID, m.ID)
	}
}

// TestTraceUsage_BackfillIsIdempotent pins the no-op guard: subsequent
// opens must not re-backfill and double the counters.
func TestTraceUsage_BackfillIsIdempotent(t *testing.T) {
	cx := setup(t)
	tr := trace.New("idempotent", "note", "", nil, "body")
	if err := cx.Add(tr); err != nil {
		t.Fatalf("Add: %v", err)
	}
	// Bump once via the normal path.
	if _, err := cx.GetAs(tr.ID, cortex.ActorAgent); err != nil {
		t.Fatalf("GetAs: %v", err)
	}

	// Close and reopen multiple times. If the backfill guard is broken,
	// each reopen would re-insert the initial-state row with read_count=1
	// on top of the existing one, or race the DELETE/INSERT somehow.
	dir := cx.Dir
	name := cx.Name
	cx.Close()
	for i := range 3 {
		cx2, err := cortex.Open(name, dir)
		if err != nil {
			t.Fatalf("reopen %d: %v", i, err)
		}
		cx2.Close()
	}

	cxFinal, err := cortex.Open(name, dir)
	if err != nil {
		t.Fatalf("final reopen: %v", err)
	}
	t.Cleanup(func() { cxFinal.Close() })

	var reads int
	if err := cxFinal.DB.QueryRow(
		`SELECT read_count FROM trace_usage WHERE trace_id = ?`, tr.ID,
	).Scan(&reads); err != nil {
		t.Fatalf("read trace_usage: %v", err)
	}
	if reads != 1 {
		t.Errorf("read_count after 4 reopens = %d, want 1 (backfill should be a no-op when trace_usage is non-empty)", reads)
	}
}

// TestTraceUsage_CRDTMergeViaUpsert pins the semantics the federation
// syncer will eventually rely on: an upsert of a remote peer's row
// applies MAX for monotonic counters and MAX for last_read_at. This
// is PR B territory functionally, but the SQL contract is testable
// now with a hand-rolled upsert against the table.
func TestTraceUsage_CRDTMergeViaUpsert(t *testing.T) {
	cx := setup(t)
	tr := trace.New("crdt target", "note", "", nil, "body")
	if err := cx.Add(tr); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Simulate a remote peer's delta arriving twice: the second arrival
	// carries smaller counter values and an older last_read_at. The
	// merge should preserve the higher-watermark values from the first
	// arrival — the CRDT contract PR B will enforce in the syncer.
	remote := "01REMOTE00000000000000PEER"
	upsert := `
		INSERT INTO trace_usage (trace_id, peer_cortex_id, read_count, modify_count, last_read_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(trace_id, peer_cortex_id) DO UPDATE SET
			read_count   = MAX(read_count,   excluded.read_count),
			modify_count = MAX(modify_count, excluded.modify_count),
			last_read_at = MAX(COALESCE(last_read_at, ''), COALESCE(excluded.last_read_at, '')),
			updated_at   = MAX(updated_at,   excluded.updated_at)
	`
	if _, err := cx.DB.Exec(upsert,
		tr.ID, remote, 10, 3, "2026-04-22T12:00:00Z", "2026-04-22T12:00:00Z",
	); err != nil {
		t.Fatalf("initial upsert: %v", err)
	}
	// Regression arrival: smaller values.
	if _, err := cx.DB.Exec(upsert,
		tr.ID, remote, 4, 1, "2026-04-21T12:00:00Z", "2026-04-23T00:00:00Z",
	); err != nil {
		t.Fatalf("regression upsert: %v", err)
	}

	var reads, mods int
	var last string
	if err := cx.DB.QueryRow(
		`SELECT read_count, modify_count, last_read_at FROM trace_usage WHERE trace_id = ? AND peer_cortex_id = ?`,
		tr.ID, remote,
	).Scan(&reads, &mods, &last); err != nil {
		t.Fatalf("read merged row: %v", err)
	}
	if reads != 10 {
		t.Errorf("read_count after regression upsert = %d, want 10 (MAX merge)", reads)
	}
	if mods != 3 {
		t.Errorf("modify_count after regression upsert = %d, want 3 (MAX merge)", mods)
	}
	if last != "2026-04-22T12:00:00Z" {
		t.Errorf("last_read_at after regression upsert = %q, want 2026-04-22T12:00:00Z (MAX merge)", last)
	}
}

// TestTraceUsage_AggregateSumsAcrossPeers pins the heuristic's view:
// SUM(read_count) and MAX(last_read_at) across every peer_cortex_id
// row for a trace is the aggregate the promotion/graduation queries
// consume. A trace with 3 local reads + 5 remote reads should score
// as if it had 8 reads — the whole point of federating the signal.
func TestTraceUsage_AggregateSumsAcrossPeers(t *testing.T) {
	cx := setup(t)
	tr := trace.New("aggregate target", "note", "", nil, "body")
	if err := cx.Add(tr); err != nil {
		t.Fatalf("Add: %v", err)
	}

	for range 3 {
		if _, err := cx.GetAs(tr.ID, cortex.ActorAgent); err != nil {
			t.Fatalf("local GetAs: %v", err)
		}
	}

	// Simulate a remote peer's deltas already synced in.
	remote := "01REMOTEFEDPEER0000000000"
	if _, err := cx.DB.Exec(`
		INSERT INTO trace_usage (trace_id, peer_cortex_id, read_count, modify_count, last_read_at, updated_at)
		VALUES (?, ?, 5, 0, '2026-04-23T19:00:00Z', '2026-04-23T19:00:00Z')`,
		tr.ID, remote,
	); err != nil {
		t.Fatalf("seed remote: %v", err)
	}

	var totalReads int
	var maxLast string
	err := cx.DB.QueryRow(
		`SELECT SUM(read_count), MAX(last_read_at) FROM trace_usage WHERE trace_id = ?`, tr.ID,
	).Scan(&totalReads, &maxLast)
	if err != nil {
		t.Fatalf("aggregate query: %v", err)
	}
	if totalReads != 8 {
		t.Errorf("aggregate read_count = %d, want 8 (3 local + 5 remote)", totalReads)
	}
	if maxLast != "2026-04-23T19:00:00Z" {
		t.Errorf("aggregate last_read_at = %q, want 2026-04-23T19:00:00Z (remote newer)", maxLast)
	}
}
