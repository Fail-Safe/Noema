package cortex_test

import (
	"testing"
	"time"

	"github.com/Fail-Safe/Noema/internal/cortex"
	"github.com/Fail-Safe/Noema/internal/federation"
	"github.com/Fail-Safe/Noema/internal/trace"
)

// TestLocalUsageSince_FiltersToLocalPeerOnly pins the publish
// contract: a peer only exposes its own rows via sync_read_signal.
// Remote rows we've synced in are never re-broadcast.
func TestLocalUsageSince_FiltersToLocalPeerOnly(t *testing.T) {
	cx := setup(t)
	tr := trace.New("owned", "note", "", nil, "body")
	if err := cx.Add(tr); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Local read bumps the local peer's row.
	if _, err := cx.GetAs(tr.ID, cortex.ActorAgent); err != nil {
		t.Fatalf("GetAs: %v", err)
	}

	// Inject a row attributed to a remote peer — this must NOT appear
	// in LocalUsageSince output.
	if _, err := cx.DB.Exec(`
		INSERT INTO trace_usage (trace_id, peer_cortex_id, read_count, modify_count, last_read_at, updated_at)
		VALUES (?, ?, 99, 99, '2099-01-01T00:00:00Z', '2099-01-01T00:00:00Z')`,
		tr.ID, "01REMOTEPEER000000000000",
	); err != nil {
		t.Fatalf("seed remote: %v", err)
	}

	rows, err := cx.LocalUsageSince("", 100)
	if err != nil {
		t.Fatalf("LocalUsageSince: %v", err)
	}
	for _, r := range rows {
		if r.PeerCortexID != cx.ID {
			t.Errorf("remote peer row leaked into LocalUsageSince output: %+v", r)
		}
	}
	if len(rows) != 1 {
		t.Errorf("expected 1 local row, got %d", len(rows))
	}
}

// TestLocalUsageSince_CursorAdvancesMonotonically pins the sync
// contract: rows are returned in UpdatedAt ASC order so callers can
// use the tail row's UpdatedAt as the next cursor.
func TestLocalUsageSince_CursorAdvancesMonotonically(t *testing.T) {
	cx := setup(t)
	for range 3 {
		tr := trace.New("t"+time.Now().Format("150405.000000"), "note", "", nil, "body")
		if err := cx.Add(tr); err != nil {
			t.Fatalf("Add: %v", err)
		}
		if _, err := cx.GetAs(tr.ID, cortex.ActorAgent); err != nil {
			t.Fatalf("GetAs: %v", err)
		}
		time.Sleep(time.Second + 10*time.Millisecond) // RFC3339 granularity is 1s
	}

	rows, err := cx.LocalUsageSince("", 100)
	if err != nil {
		t.Fatalf("LocalUsageSince: %v", err)
	}
	if len(rows) < 3 {
		t.Fatalf("expected at least 3 rows, got %d", len(rows))
	}
	for i := 1; i < len(rows); i++ {
		if rows[i-1].UpdatedAt > rows[i].UpdatedAt {
			t.Errorf("rows not in ascending UpdatedAt order: %s then %s", rows[i-1].UpdatedAt, rows[i].UpdatedAt)
		}
	}

	// Resume using the middle cursor — should get only the tail.
	mid := rows[len(rows)/2].UpdatedAt
	after, err := cx.LocalUsageSince(mid, 100)
	if err != nil {
		t.Fatalf("LocalUsageSince(mid): %v", err)
	}
	for _, r := range after {
		if r.UpdatedAt <= mid {
			t.Errorf("cursor leaked a row with UpdatedAt=%s (should be > %s)", r.UpdatedAt, mid)
		}
	}
}

// TestMergeRemoteUsage_RefusesLocalIDAttribution pins the safety
// invariant: even if a malicious or buggy peer ships us rows under
// our own cortex_id, we refuse to apply them. Otherwise a remote
// could rewrite our local counters.
func TestMergeRemoteUsage_RefusesLocalIDAttribution(t *testing.T) {
	cx := setup(t)
	tr := trace.New("selfguard", "note", "", nil, "body")
	if err := cx.Add(tr); err != nil {
		t.Fatalf("Add: %v", err)
	}
	// Establish a local row via a legit read.
	if _, err := cx.GetAs(tr.ID, cortex.ActorAgent); err != nil {
		t.Fatalf("GetAs: %v", err)
	}

	// Attempt to overwrite the local row via MergeRemoteUsage.
	if err := cx.MergeRemoteUsage([]federation.TraceUsage{{
		TraceID:      tr.ID,
		PeerCortexID: cx.ID, // local ID — should be refused
		ReadCount:    9999,
		ModifyCount:  9999,
		LastReadAt:   "2099-01-01T00:00:00Z",
		UpdatedAt:    "2099-01-01T00:00:00Z",
	}}); err != nil {
		t.Fatalf("MergeRemoteUsage: %v", err)
	}

	// Local row must still have read_count = 1 (unchanged).
	var reads int
	if err := cx.DB.QueryRow(
		`SELECT read_count FROM trace_usage WHERE trace_id = ? AND peer_cortex_id = ?`,
		tr.ID, cx.ID,
	).Scan(&reads); err != nil {
		t.Fatalf("read local row: %v", err)
	}
	if reads != 1 {
		t.Errorf("local row was overwritten by MergeRemoteUsage: read_count=%d, want 1", reads)
	}
}

// TestMergeRemoteUsage_MaxMergeOnRepeat pins the CRDT contract:
// re-applying an older row after a newer one is a no-op.
func TestMergeRemoteUsage_MaxMergeOnRepeat(t *testing.T) {
	cx := setup(t)
	tr := trace.New("maxmerge", "note", "", nil, "body")
	if err := cx.Add(tr); err != nil {
		t.Fatalf("Add: %v", err)
	}

	remote := "01REMOTEPEER000000000000"
	rows := []federation.TraceUsage{
		{TraceID: tr.ID, PeerCortexID: remote, ReadCount: 10, ModifyCount: 3, LastReadAt: "2099-02-01T00:00:00Z", UpdatedAt: "2099-02-01T00:00:00Z"},
	}
	if err := cx.MergeRemoteUsage(rows); err != nil {
		t.Fatalf("first merge: %v", err)
	}

	// Regression delivery: older values arrive. MAX-merge must
	// preserve the newer values we already have.
	regression := []federation.TraceUsage{
		{TraceID: tr.ID, PeerCortexID: remote, ReadCount: 4, ModifyCount: 1, LastReadAt: "2099-01-01T00:00:00Z", UpdatedAt: "2099-02-15T00:00:00Z"},
	}
	if err := cx.MergeRemoteUsage(regression); err != nil {
		t.Fatalf("regression merge: %v", err)
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
		t.Errorf("read_count after regression = %d, want 10 (MAX merge should resist downgrade)", reads)
	}
	if mods != 3 {
		t.Errorf("modify_count after regression = %d, want 3", mods)
	}
	if last != "2099-02-01T00:00:00Z" {
		t.Errorf("last_read_at after regression = %q, want 2099-02-01T00:00:00Z", last)
	}
}
