package cortex_test

import (
	"testing"

	"github.com/Fail-Safe/Noema/internal/cortex"
	"github.com/Fail-Safe/Noema/internal/trace"
)

// Phase 1 of memory tiering: schema foundation + tier field plumbing. See
// docs/plans/consolidation-plan.md (in Noema-design) §1-3. These tests pin
// the contract that new traces default to tier='short', Get/List surface
// the tier, and Update preserves it end-to-end.

func TestAdd_DefaultsTierToShort(t *testing.T) {
	cx := setup(t)
	tr := trace.New("Defaults short", "note", "agent-1", nil, "body")
	if err := cx.Add(tr); err != nil {
		t.Fatalf("Add: %v", err)
	}

	row, err := cx.Get(tr.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if row.Tier != trace.TierShort {
		t.Errorf("Tier = %q, want %q", row.Tier, trace.TierShort)
	}

	// trace.Write is called during Add; confirm the in-memory Trace was
	// stamped with the default so callers inspecting it after Add see a
	// coherent tier value.
	if tr.Tier != trace.TierShort {
		t.Errorf("Add did not stamp Trace.Tier; got %q", tr.Tier)
	}
}

func TestAdd_RespectsExplicitTier(t *testing.T) {
	// An inbound trace created externally (e.g. via the watcher reading a
	// file with tier: mid in frontmatter) should land in the requested
	// tier rather than being forced back to short. Promotion guardrails
	// layer on in later phases; Phase 1 is permissive at ingest.
	cx := setup(t)
	tr := trace.New("Explicit mid", "observation", "agent-1", nil, "body")
	tr.Tier = trace.TierMid
	if err := cx.Add(tr); err != nil {
		t.Fatalf("Add: %v", err)
	}
	row, err := cx.Get(tr.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if row.Tier != trace.TierMid {
		t.Errorf("Tier = %q, want %q", row.Tier, trace.TierMid)
	}
}

func TestList_ReturnsTier(t *testing.T) {
	cx := setup(t)
	tr := trace.New("List tier", "note", "", nil, "body")
	if err := cx.Add(tr); err != nil {
		t.Fatalf("Add: %v", err)
	}
	rows, err := cx.List(cortex.ListOptions{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) == 0 {
		t.Fatal("List returned no rows")
	}
	for _, r := range rows {
		if r.Tier == "" {
			t.Errorf("Row %s has empty Tier from List", r.ID)
		}
	}
}

func TestUpdate_PreservesDBTierAgainstFileDrift(t *testing.T) {
	// If the on-disk frontmatter drifts (manual edit), the DB is
	// authoritative and Update stamps the DB tier back onto the file.
	// Verifies the safety rail that prevents external edits from silently
	// promoting traces into long-term.
	cx := setup(t)
	tr := trace.New("DB-authoritative tier", "note", "", nil, "body")
	if err := cx.Add(tr); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Rewrite the on-disk file with tier: long and a new body.
	path := cx.TraceFile(tr.ID, false)
	drifted, err := trace.ParseFile(path)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	drifted.Tier = trace.TierLong
	drifted.Body = "drifted body"
	if err := drifted.Write(path); err != nil {
		t.Fatalf("Write: %v", err)
	}

	if err := cx.Update(tr.ID); err != nil {
		t.Fatalf("Update: %v", err)
	}

	row, err := cx.Get(tr.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if row.Tier != trace.TierShort {
		t.Errorf("DB tier after drifted update = %q, want %q", row.Tier, trace.TierShort)
	}

	// File on disk must also have been stamped back to the DB value.
	after, err := trace.ParseFile(path)
	if err != nil {
		t.Fatalf("ParseFile after Update: %v", err)
	}
	if after.Tier != trace.TierShort {
		t.Errorf("file tier after drifted update = %q, want %q", after.Tier, trace.TierShort)
	}
}

// TestTierVotes_ReflectsVoteHistory pins the read path for the TUI's
// detail-pane vote counter: TierVotes returns the accumulated
// +1/-1 deltas applied via Vote(). Missing traces surface the
// underlying sql.ErrNoRows so callers can distinguish "0 votes" from
// "id doesn't exist."
func TestTierVotes_ReflectsVoteHistory(t *testing.T) {
	cx := setup(t)
	tr := trace.New("vote fixture", "note", "", nil, "body")
	if err := cx.Add(tr); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Fresh trace starts at zero.
	v, err := cx.TierVotes(tr.ID)
	if err != nil {
		t.Fatalf("TierVotes (fresh): %v", err)
	}
	if v != 0 {
		t.Errorf("fresh trace votes = %d, want 0", v)
	}

	// Two upvotes, one downvote — net +1.
	for _, d := range []int{1, 1, -1} {
		if err := cx.Vote(tr.ID, d, cortex.ActorHuman); err != nil {
			t.Fatalf("Vote(%d): %v", d, err)
		}
	}
	v, err = cx.TierVotes(tr.ID)
	if err != nil {
		t.Fatalf("TierVotes (after votes): %v", err)
	}
	if v != 1 {
		t.Errorf("votes after +1+1-1 = %d, want 1", v)
	}

	// Missing trace returns a non-nil error — TUI callers currently
	// treat any error as "0" (the detail pane blanks in that case),
	// but the contract is important for future callers.
	if _, err := cx.TierVotes("does-not-exist"); err == nil {
		t.Error("TierVotes on missing id should return an error, got nil")
	}
}
