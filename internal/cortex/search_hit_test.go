package cortex_test

import (
	"testing"

	"github.com/Fail-Safe/Noema/internal/cortex"
	"github.com/Fail-Safe/Noema/internal/federation"
	"github.com/Fail-Safe/Noema/internal/trace"
)

// searchHitCountOf returns the aggregate search_hit_count for a trace
// across all peer rows — the same view PromotionCandidates surfaces
// to the consolidation scorer.
func searchHitCountOf(t *testing.T, cx *cortex.Cortex, id string) int {
	t.Helper()
	var n int
	if err := cx.DB.QueryRow(
		`SELECT COALESCE(SUM(search_hit_count), 0) FROM trace_usage WHERE trace_id = ?`, id,
	).Scan(&n); err != nil {
		t.Fatalf("reading search_hit_count: %v", err)
	}
	return n
}

// addNamed seeds a trace with predictable Go-themed body text so it
// shows up in any "go" search, and returns the ID.
func addNamed(t *testing.T, cx *cortex.Cortex, title string) string {
	t.Helper()
	tr := trace.New(title, "note", "", []string{"go"},
		"Go tooling, modules, goroutines, and SQLite via modernc together make a clean stack.")
	if err := cx.Add(tr); err != nil {
		t.Fatalf("Add %s: %v", title, err)
	}
	return tr.ID
}

func TestSearchAs_AgentBumpsTopN(t *testing.T) {
	cx := setup(t)
	// Seed five matchable traces. Top-N defaults to 3, so only the
	// first three by FTS rank should bump.
	ids := []string{
		addNamed(t, cx, "Go alpha"),
		addNamed(t, cx, "Go bravo"),
		addNamed(t, cx, "Go charlie"),
		addNamed(t, cx, "Go delta"),
		addNamed(t, cx, "Go echo"),
	}

	rows, err := cx.SearchAs("Go", cortex.ListOptions{}, cortex.ActorAgent, cortex.DefaultSearchHitTopN)
	if err != nil {
		t.Fatalf("SearchAs: %v", err)
	}
	if len(rows) < 3 {
		t.Fatalf("expected at least 3 search results, got %d", len(rows))
	}

	bumped := 0
	for _, id := range ids {
		if searchHitCountOf(t, cx, id) > 0 {
			bumped++
		}
	}
	if bumped != cortex.DefaultSearchHitTopN {
		t.Errorf("bumped %d traces, want exactly %d (top-N)", bumped, cortex.DefaultSearchHitTopN)
	}

	// Each bumped trace must be at +1 exactly (not higher).
	for _, r := range rows[:cortex.DefaultSearchHitTopN] {
		if got := searchHitCountOf(t, cx, r.ID); got != 1 {
			t.Errorf("top-N hit %s: search_hit_count = %d, want 1", r.ID, got)
		}
	}
}

func TestSearchAs_NonAgentDoesNotBump(t *testing.T) {
	cx := setup(t)
	id := addNamed(t, cx, "Go quiet")

	for _, actor := range []cortex.ReadActor{cortex.ActorHuman, cortex.ActorSystem} {
		if _, err := cx.SearchAs("Go", cortex.ListOptions{}, actor, cortex.DefaultSearchHitTopN); err != nil {
			t.Fatalf("SearchAs actor=%d: %v", actor, err)
		}
	}
	if got := searchHitCountOf(t, cx, id); got != 0 {
		t.Errorf("non-agent searches bumped search_hit_count = %d, want 0", got)
	}
}

func TestSearchAs_RepeatedAgentSearchAccumulates(t *testing.T) {
	cx := setup(t)
	id := addNamed(t, cx, "Go solo")

	for range 4 {
		if _, err := cx.SearchAs("Go", cortex.ListOptions{}, cortex.ActorAgent, cortex.DefaultSearchHitTopN); err != nil {
			t.Fatalf("SearchAs: %v", err)
		}
	}
	if got := searchHitCountOf(t, cx, id); got != 4 {
		t.Errorf("after 4 agent searches, search_hit_count = %d, want 4", got)
	}
}

func TestSearchAs_LongTierResultsAreSkipped(t *testing.T) {
	// Long-tier traces are immutable; bumping their counters is a
	// no-op signal anyway since they can't graduate further. The
	// guard lives in bumpSearchHitsForIDs, mirror the GetAs rule.
	cx := setup(t)
	tr := trace.New("Go everlasting", "fact", "", []string{"go"},
		"Go and goroutines and modules and modernc — durable knowledge.")
	tr.Tier = trace.TierLong
	if err := cx.Add(tr); err != nil {
		t.Fatalf("Add: %v", err)
	}

	if _, err := cx.SearchAs("Go", cortex.ListOptions{}, cortex.ActorAgent, cortex.DefaultSearchHitTopN); err != nil {
		t.Fatalf("SearchAs: %v", err)
	}
	if got := searchHitCountOf(t, cx, tr.ID); got != 0 {
		t.Errorf("long-tier hit got search_hit_count = %d, want 0 (must be skipped)", got)
	}
}

func TestFindSimilarAs_AgentBumpsTopN(t *testing.T) {
	cx := setup(t)
	src := trace.New("Go decision", "decision", "", []string{"go"},
		"We chose Go for tooling, modernc SQLite, goroutines, and static binaries.")
	if err := cx.Add(src); err != nil {
		t.Fatalf("Add src: %v", err)
	}
	for _, title := range []string{"Go A", "Go B", "Go C", "Go D"} {
		addNamed(t, cx, title)
	}

	matches, err := cx.FindSimilarAs(src.ID, cortex.SimilarOpts{Limit: 4}, cortex.ActorAgent, cortex.DefaultSearchHitTopN)
	if err != nil {
		t.Fatalf("FindSimilarAs: %v", err)
	}
	if len(matches) < 3 {
		t.Fatalf("expected ≥3 similar matches, got %d", len(matches))
	}

	// Source itself never appears, and its counter never moves.
	if got := searchHitCountOf(t, cx, src.ID); got != 0 {
		t.Errorf("source trace got search_hit_count = %d, want 0", got)
	}

	for _, m := range matches[:cortex.DefaultSearchHitTopN] {
		if got := searchHitCountOf(t, cx, m.ID); got != 1 {
			t.Errorf("top-N similar hit %s: search_hit_count = %d, want 1", m.ID, got)
		}
	}
}

// TestSearchHit_Federation_MaxMerge pins the CRDT contract for the new
// column: a regression delivery (older row arriving after a newer one)
// must not stomp the larger value.
func TestSearchHit_Federation_MaxMerge(t *testing.T) {
	cx := setup(t)
	tr := trace.New("federated hit", "note", "", nil, "body")
	if err := cx.Add(tr); err != nil {
		t.Fatalf("Add: %v", err)
	}

	remote := "01REMOTEPEER000000000000"
	if err := cx.MergeRemoteUsage([]federation.TraceUsage{{
		TraceID:        tr.ID,
		PeerCortexID:   remote,
		ReadCount:      0,
		ModifyCount:    0,
		SearchHitCount: 12,
		UpdatedAt:      "2099-02-01T00:00:00Z",
	}}); err != nil {
		t.Fatalf("first merge: %v", err)
	}

	// Older row arriving with a smaller search_hit_count.
	if err := cx.MergeRemoteUsage([]federation.TraceUsage{{
		TraceID:        tr.ID,
		PeerCortexID:   remote,
		ReadCount:      0,
		ModifyCount:    0,
		SearchHitCount: 3,
		UpdatedAt:      "2099-02-15T00:00:00Z",
	}}); err != nil {
		t.Fatalf("regression merge: %v", err)
	}

	var hits int
	if err := cx.DB.QueryRow(
		`SELECT search_hit_count FROM trace_usage WHERE trace_id = ? AND peer_cortex_id = ?`,
		tr.ID, remote,
	).Scan(&hits); err != nil {
		t.Fatalf("read merged row: %v", err)
	}
	if hits != 12 {
		t.Errorf("search_hit_count after regression = %d, want 12 (MAX-merge resists downgrade)", hits)
	}
}

// TestSearchHit_Federation_LocalUsageRoundtrip pins that
// LocalUsageSince emits the new column so peers actually receive it.
func TestSearchHit_Federation_LocalUsageRoundtrip(t *testing.T) {
	cx := setup(t)
	id := addNamed(t, cx, "Go roundtrip")
	if _, err := cx.SearchAs("Go", cortex.ListOptions{}, cortex.ActorAgent, cortex.DefaultSearchHitTopN); err != nil {
		t.Fatalf("SearchAs: %v", err)
	}

	rows, err := cx.LocalUsageSince("", 100)
	if err != nil {
		t.Fatalf("LocalUsageSince: %v", err)
	}
	var found *federation.TraceUsage
	for i := range rows {
		if rows[i].TraceID == id {
			found = &rows[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("trace %s missing from LocalUsageSince output", id)
	}
	if found.SearchHitCount != 1 {
		t.Errorf("LocalUsageSince SearchHitCount = %d, want 1", found.SearchHitCount)
	}
}

// TestPromotionCandidates_IncludesSearchHitCount pins that the
// candidate query surfaces the new column to the scorer.
func TestPromotionCandidates_IncludesSearchHitCount(t *testing.T) {
	cx := setup(t)
	id := addNamed(t, cx, "Go candidate")
	if _, err := cx.SearchAs("Go", cortex.ListOptions{}, cortex.ActorAgent, cortex.DefaultSearchHitTopN); err != nil {
		t.Fatalf("SearchAs: %v", err)
	}

	cands, err := cx.PromotionCandidates(trace.TierShort, 24*60*60*1_000_000_000)
	if err != nil {
		t.Fatalf("PromotionCandidates: %v", err)
	}
	var got *cortex.PromotionCandidate
	for i := range cands {
		if cands[i].ID == id {
			got = &cands[i]
			break
		}
	}
	if got == nil {
		t.Fatalf("trace %s missing from candidates", id)
	}
	if got.SearchHitCount != 1 {
		t.Errorf("PromotionCandidate.SearchHitCount = %d, want 1", got.SearchHitCount)
	}
}
