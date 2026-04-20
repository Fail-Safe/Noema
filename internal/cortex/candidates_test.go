package cortex_test

import (
	"testing"
	"time"

	"github.com/Fail-Safe/Noema/internal/cortex"
	"github.com/Fail-Safe/Noema/internal/trace"
)

// Phase 8 of memory tiering: PromotionCandidates query. See
// docs/plans/consolidation-plan.md §5 in the Noema-design repo.

func TestPromotionCandidates_FiltersByTier(t *testing.T) {
	cx := setup(t)
	seed(t, cx, "a short", trace.TierShort)
	seed(t, cx, "a mid", trace.TierMid)
	seed(t, cx, "a long", trace.TierLong)

	got, err := cx.PromotionCandidates(trace.TierShort, 24*time.Hour)
	if err != nil {
		t.Fatalf("PromotionCandidates: %v", err)
	}
	if len(got) != 1 || got[0].Tier != trace.TierShort {
		t.Errorf("got %v, want 1 short-tier candidate", got)
	}
}

func TestPromotionCandidates_ExcludesArchivedTrashedPurged(t *testing.T) {
	cx := setup(t)
	active := seed(t, cx, "still active", trace.TierShort)
	archived := seed(t, cx, "archived", trace.TierShort)
	trashed := seed(t, cx, "trashed", trace.TierShort)
	purged := seed(t, cx, "purged", trace.TierShort)

	if err := cx.Archive(archived); err != nil {
		t.Fatalf("Archive: %v", err)
	}
	if err := cx.Trash(trashed); err != nil {
		t.Fatalf("Trash: %v", err)
	}
	if err := cx.AdminPurge(purged, "exclusion test", trace.TierShort, false, cortex.ActorHuman); err != nil {
		t.Fatalf("AdminPurge: %v", err)
	}

	got, err := cx.PromotionCandidates(trace.TierShort, 24*time.Hour)
	if err != nil {
		t.Fatalf("PromotionCandidates: %v", err)
	}
	if len(got) != 1 || got[0].ID != active {
		t.Errorf("got %v, want only active trace %q", got, active)
	}
}

func TestPromotionCandidates_SignalsSurfaced(t *testing.T) {
	// Populate reads, modifies, votes, and lineage; confirm the query
	// returns all four signals via the blended join.
	cx := setup(t)
	parent := trace.New("parent", "fact", "", nil, "parent body")
	if err := cx.Add(parent); err != nil {
		t.Fatalf("Add parent: %v", err)
	}
	child := trace.New("child", "observation", "", nil, "child body")
	child.DerivedFrom = []string{parent.ID}
	if err := cx.Add(child); err != nil {
		t.Fatalf("Add child: %v", err)
	}
	// Bump parent's signals: 3 agent reads, 1 modify, 1 vote.
	for i := 0; i < 3; i++ {
		if _, err := cx.GetAs(parent.ID, cortex.ActorAgent); err != nil {
			t.Fatalf("GetAs: %v", err)
		}
	}
	path := cx.TraceFile(parent.ID, false)
	parsed, err := trace.ParseFile(path)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	parsed.Body = "edited"
	if err := parsed.Write(path); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := cx.UpdateAs(parent.ID, cortex.ActorAgent); err != nil {
		t.Fatalf("UpdateAs: %v", err)
	}
	if err := cx.Vote(parent.ID, 1, cortex.ActorHuman); err != nil {
		t.Fatalf("Vote: %v", err)
	}

	got, err := cx.PromotionCandidates(trace.TierShort, 24*time.Hour)
	if err != nil {
		t.Fatalf("PromotionCandidates: %v", err)
	}
	var parentCandidate *cortex.PromotionCandidate
	for i := range got {
		if got[i].ID == parent.ID {
			parentCandidate = &got[i]
		}
	}
	if parentCandidate == nil {
		t.Fatalf("parent not in candidates: %v", got)
	}
	if parentCandidate.ReadCount != 3 {
		t.Errorf("ReadCount = %d, want 3", parentCandidate.ReadCount)
	}
	if parentCandidate.ModifyCount != 1 {
		t.Errorf("ModifyCount = %d, want 1", parentCandidate.ModifyCount)
	}
	if parentCandidate.TierVotes != 1 {
		t.Errorf("TierVotes = %d, want 1", parentCandidate.TierVotes)
	}
	if parentCandidate.DerivedFromCount != 1 {
		t.Errorf("DerivedFromCount = %d, want 1 (child references parent)", parentCandidate.DerivedFromCount)
	}
}

func TestListOptions_TiersFilter(t *testing.T) {
	cx := setup(t)
	seed(t, cx, "short one", trace.TierShort)
	seed(t, cx, "mid one", trace.TierMid)
	seed(t, cx, "long one", trace.TierLong)

	// Single tier: only short returned.
	rows, err := cx.List(cortex.ListOptions{Tiers: []string{trace.TierShort}})
	if err != nil {
		t.Fatalf("List short: %v", err)
	}
	if len(rows) != 1 || rows[0].Tier != trace.TierShort {
		t.Errorf("filter to short failed: got %+v", rows)
	}

	// Multi-tier: short + mid, no long.
	rows, err = cx.List(cortex.ListOptions{Tiers: []string{trace.TierShort, trace.TierMid}})
	if err != nil {
		t.Fatalf("List short+mid: %v", err)
	}
	if len(rows) != 2 {
		t.Errorf("filter to short+mid returned %d rows, want 2", len(rows))
	}
	for _, r := range rows {
		if r.Tier == trace.TierLong {
			t.Errorf("long row leaked through filter: %s", r.ID)
		}
	}

	// Empty slice means no filter — all three return.
	rows, err = cx.List(cortex.ListOptions{})
	if err != nil {
		t.Fatalf("List all: %v", err)
	}
	if len(rows) != 3 {
		t.Errorf("empty Tiers filter should return all, got %d", len(rows))
	}

	// Search path also honors the filter so FTS queries respect
	// the current TUI tier visibility.
	rows, err = cx.Search("one", cortex.ListOptions{Tiers: []string{trace.TierLong}})
	if err != nil {
		t.Fatalf("Search with tier filter: %v", err)
	}
	if len(rows) != 1 || rows[0].Tier != trace.TierLong {
		t.Errorf("search filter to long failed: got %+v", rows)
	}
}

func TestPromotionCandidates_WindowBound(t *testing.T) {
	// A trace created before the rolling window's cutoff must not
	// appear as a candidate. RFC3339 only has second precision so
	// a nanosecond window alone can't prove filtering — backdate
	// the created_at via direct UPDATE (bypasses event emission,
	// fine for this test's purposes) and confirm a 1h window
	// excludes a trace created 25h ago.
	cx := setup(t)
	id := seed(t, cx, "old one", trace.TierShort)
	backdated := time.Now().UTC().Add(-25 * time.Hour).Format(time.RFC3339)
	if _, err := cx.DB.Exec(`UPDATE traces SET created_at = ? WHERE id = ?`, backdated, id); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	got, err := cx.PromotionCandidates(trace.TierShort, 1*time.Hour)
	if err != nil {
		t.Fatalf("PromotionCandidates: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected zero candidates outside window, got %d (%+v)", len(got), got)
	}
}
