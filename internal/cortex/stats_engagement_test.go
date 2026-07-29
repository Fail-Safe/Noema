package cortex_test

import (
	"testing"
	"time"

	"github.com/Fail-Safe/Noema/internal/cortex"
	"github.com/Fail-Safe/Noema/internal/trace"
)

// TestEngagementStats_AggregatesAcrossActiveTraces pins that the
// summary sums every counter from every peer's row, restricted to
// active (not archived/trashed/purged) traces. Archived rows are the
// most likely regression — adding a trace, archiving it, and then
// asserting the read counter still surfaces is exactly what we don't
// want for an "actively in use" dashboard.
func TestEngagementStats_AggregatesAcrossActiveTraces(t *testing.T) {
	cx := setup(t)

	active := trace.New("active", "note", "", nil, "body")
	if err := cx.Add(active); err != nil {
		t.Fatalf("Add active: %v", err)
	}
	// Two reads + one search hit on the active trace.
	for range 2 {
		if _, err := cx.GetAs(active.ID, cortex.ActorAgent); err != nil {
			t.Fatalf("GetAs: %v", err)
		}
	}
	if _, err := cx.SearchAs("active", cortex.ListOptions{}, cortex.ActorAgent, cortex.DefaultSearchHitTopN); err != nil {
		t.Fatalf("SearchAs: %v", err)
	}

	// Archived trace whose counters must be excluded.
	archived := trace.New("archived", "note", "", nil, "body")
	if err := cx.Add(archived); err != nil {
		t.Fatalf("Add archived: %v", err)
	}
	if _, err := cx.GetAs(archived.ID, cortex.ActorAgent); err != nil {
		t.Fatalf("GetAs archived: %v", err)
	}
	if err := cx.Archive(archived.ID); err != nil {
		t.Fatalf("Archive: %v", err)
	}

	got, err := cx.EngagementStats()
	if err != nil {
		t.Fatalf("EngagementStats: %v", err)
	}
	if got.TotalReads != 2 {
		t.Errorf("TotalReads = %d, want 2 (archived trace's read should be excluded)", got.TotalReads)
	}
	if got.TotalSearchHits != 1 {
		t.Errorf("TotalSearchHits = %d, want 1", got.TotalSearchHits)
	}
}

func TestEngagementStats_EmptyCortex(t *testing.T) {
	cx := setup(t)
	got, err := cx.EngagementStats()
	if err != nil {
		t.Fatalf("EngagementStats: %v", err)
	}
	if got.TotalReads != 0 || got.TotalSearchHits != 0 || got.TotalModifies != 0 {
		t.Errorf("expected all zero, got %+v", got)
	}
}

// TestMidLineageBreakdown_BucketsBySourceCount pins that mids are
// classified by their derived_from count. The 1-source bucket is the
// signal we care about — a growing count there is a smell, since
// The one-source promotion gate means new traces shouldn't promote on
// lineage alone anymore.
func TestMidLineageBreakdown_BucketsBySourceCount(t *testing.T) {
	cx := setup(t)

	// Stand-alone mid (no derived_from).
	a := trace.New("solo", "note", "", nil, "body")
	a.Tier = trace.TierMid
	if err := cx.Add(a); err != nil {
		t.Fatalf("Add a: %v", err)
	}

	// Source for the linked mids.
	src1 := trace.New("source-1", "note", "", nil, "body")
	if err := cx.Add(src1); err != nil {
		t.Fatalf("Add src1: %v", err)
	}
	src2 := trace.New("source-2", "note", "", nil, "body")
	if err := cx.Add(src2); err != nil {
		t.Fatalf("Add src2: %v", err)
	}

	// 1-source mid (the Hermes session-summary shape).
	single := trace.New("session-summary-shaped", "observation", "", nil, "body")
	single.Tier = trace.TierMid
	single.DerivedFrom = []string{src1.ID}
	if err := cx.Add(single); err != nil {
		t.Fatalf("Add single: %v", err)
	}

	// 2-source mid (a real distillation shape).
	multi := trace.New("real-distillation", "observation", "", nil, "body")
	multi.Tier = trace.TierMid
	multi.DerivedFrom = []string{src1.ID, src2.ID}
	if err := cx.Add(multi); err != nil {
		t.Fatalf("Add multi: %v", err)
	}

	// Short trace — must not show up in the mid breakdown.
	noise := trace.New("short noise", "note", "", nil, "body")
	if err := cx.Add(noise); err != nil {
		t.Fatalf("Add noise: %v", err)
	}

	got, err := cx.MidLineageBreakdown()
	if err != nil {
		t.Fatalf("MidLineageBreakdown: %v", err)
	}
	if got.NoSources != 1 {
		t.Errorf("NoSources = %d, want 1", got.NoSources)
	}
	if got.SingleSource != 1 {
		t.Errorf("SingleSource = %d, want 1", got.SingleSource)
	}
	if got.MultiSource != 1 {
		t.Errorf("MultiSource = %d, want 1", got.MultiSource)
	}
}

// TestMidEngagementSnapshot_ZeroEngagementCount pins the count of
// mids with no usage signal at all. The "older" subset is filtered to
// traces whose created_at predates the cutoff — used to exclude
// transient new mids from the archival-candidate count.
func TestMidEngagementSnapshot_ZeroEngagementCount(t *testing.T) {
	cx := setup(t)

	// Two cold mids — neither read, hit, nor modified.
	for i := range 2 {
		tr := trace.New("cold-"+itoa(i), "note", "", nil, "body")
		tr.Tier = trace.TierMid
		if err := cx.Add(tr); err != nil {
			t.Fatalf("Add cold-%d: %v", i, err)
		}
	}

	// One engaged mid (a single read makes it ineligible for the
	// zero-engagement bucket).
	hot := trace.New("hot", "note", "", nil, "body")
	hot.Tier = trace.TierMid
	if err := cx.Add(hot); err != nil {
		t.Fatalf("Add hot: %v", err)
	}
	if _, err := cx.GetAs(hot.ID, cortex.ActorAgent); err != nil {
		t.Fatalf("GetAs hot: %v", err)
	}

	// olderThan=0 collapses the older subset to "everything", which
	// matches the expectation that all current cold mids count.
	got, err := cx.MidEngagementSnapshot(0)
	if err != nil {
		t.Fatalf("MidEngagementSnapshot: %v", err)
	}
	if got.ZeroEngagement != 2 {
		t.Errorf("ZeroEngagement = %d, want 2", got.ZeroEngagement)
	}
	if got.ZeroEngagementOlder != 2 {
		t.Errorf("ZeroEngagementOlder (cutoff=now) = %d, want 2", got.ZeroEngagementOlder)
	}
}

// TestMidEngagementSnapshot_OlderCutoffExcludesTransients pins that
// the "older than" filter actually narrows. Setting olderThan to a
// year filters every freshly-seeded trace out of the older bucket
// while leaving the total intact.
func TestMidEngagementSnapshot_OlderCutoffExcludesTransients(t *testing.T) {
	cx := setup(t)
	for i := range 3 {
		tr := trace.New("fresh-"+itoa(i), "note", "", nil, "body")
		tr.Tier = trace.TierMid
		if err := cx.Add(tr); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
	got, err := cx.MidEngagementSnapshot(365 * 24 * time.Hour)
	if err != nil {
		t.Fatalf("MidEngagementSnapshot: %v", err)
	}
	if got.ZeroEngagement != 3 {
		t.Errorf("ZeroEngagement = %d, want 3", got.ZeroEngagement)
	}
	if got.ZeroEngagementOlder != 0 {
		t.Errorf("ZeroEngagementOlder (cutoff=1y) = %d, want 0", got.ZeroEngagementOlder)
	}
}
