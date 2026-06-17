package consolidation

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Fail-Safe/Noema/internal/cortex"
	"github.com/Fail-Safe/Noema/internal/trace"
)

// fakeHeuristicCortex is a minimal HeuristicProvider for driving the
// pass through scoring / promotion edge cases without a real DB. The
// candidates slice is what PromotionCandidates returns; promoted
// records every call to Promote so tests can assert which IDs moved.
type fakeHeuristicCortex struct {
	candidates []cortex.PromotionCandidate
	promoted   []string
	promoteErr error
}

func (f *fakeHeuristicCortex) PromotionCandidates(tier string, window time.Duration) ([]cortex.PromotionCandidate, error) {
	return f.candidates, nil
}

func (f *fakeHeuristicCortex) Promote(id, newTier string) error {
	if f.promoteErr != nil {
		return f.promoteErr
	}
	f.promoted = append(f.promoted, id)
	return nil
}

// ---- scoreCandidate ----

func TestScoreCandidate_BlendedFormula(t *testing.T) {
	// Defaults: reads=1, modifies=2, lineage=3, votes=5.
	cfg := PassConfig{}.resolved()

	tests := []struct {
		name string
		pc   cortex.PromotionCandidate
		want int
	}{
		{"all zero", cortex.PromotionCandidate{}, 0},
		{"5 reads", cortex.PromotionCandidate{ReadCount: 5}, 5},
		{"5 search hits", cortex.PromotionCandidate{SearchHitCount: 5}, 5},
		{"3 reads + 2 search hits sums into reads bucket", cortex.PromotionCandidate{ReadCount: 3, SearchHitCount: 2}, 5},
		{"1 modify", cortex.PromotionCandidate{ModifyCount: 1}, 2},
		// Lineage credit is gated at >= 2 sources. A single derived_from
		// is provenance, not consolidation — see MinLineageSourcesForCredit.
		{"1 lineage ref earns no credit", cortex.PromotionCandidate{DerivedFromCount: 1}, 0},
		{"1 lineage ref + search hits earns no passive credit", cortex.PromotionCandidate{SearchHitCount: 5, DerivedFromCount: 1}, 0},
		{"1 lineage ref + deliberate reads still counts", cortex.PromotionCandidate{ReadCount: 5, DerivedFromCount: 1}, 5},
		{"1 lineage ref + modify unlocks search-hit credit", cortex.PromotionCandidate{SearchHitCount: 3, ModifyCount: 1, DerivedFromCount: 1}, 5},
		{"1 lineage ref + vote unlocks search-hit credit", cortex.PromotionCandidate{SearchHitCount: 1, TierVotes: 1, DerivedFromCount: 1}, 6},
		{"2 lineage refs earn full credit", cortex.PromotionCandidate{DerivedFromCount: 2}, 6},
		{"3 lineage refs scale linearly", cortex.PromotionCandidate{DerivedFromCount: 3}, 9},
		{"1 vote", cortex.PromotionCandidate{TierVotes: 1}, 5},
		{"mix: 2 reads + 1 modify + 1 vote", cortex.PromotionCandidate{ReadCount: 2, ModifyCount: 1, TierVotes: 1}, 2 + 2 + 5},
		// 1-source lineage doesn't push a low-engagement trace over the
		// threshold — exactly the regression we want this gate to prevent
		// (Hermes session-summary mids glided past on lineage alone).
		{"1 lineage + 1 read stays sub-threshold", cortex.PromotionCandidate{ReadCount: 1, DerivedFromCount: 1}, 1},
		{"negative vote counts against", cortex.PromotionCandidate{TierVotes: -1}, -5},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := scoreCandidate(tc.pc, cfg); got != tc.want {
				t.Errorf("score = %d, want %d", got, tc.want)
			}
		})
	}
}

// ---- HeuristicPass ----

func TestHeuristicPass_PromotesAboveThreshold(t *testing.T) {
	cx := &fakeHeuristicCortex{
		candidates: []cortex.PromotionCandidate{
			{ID: "hot", Tier: trace.TierShort, ReadCount: 10},              // score 10 - promote
			{ID: "meh", Tier: trace.TierShort, ReadCount: 2},               // score 2  - skip
			{ID: "voted", Tier: trace.TierShort, TierVotes: 1},             // score 5  - promote (at threshold)
			{ID: "cold", Tier: trace.TierShort},                            // score 0  - skip
			{ID: "referenced", Tier: trace.TierShort, DerivedFromCount: 2}, // score 6  - promote (real consolidation)
			// Regression guard: a 1-source provenance link with low
			// engagement must not glide past the threshold. This is the
			// exact pattern (Hermes session-summary writes one
			// derived_from pointing at the session trace) that was
			// silently inflating mid-tier before MinLineageSourcesForCredit
			// landed. Score: 1*1 (read) + 0 (lineage gated) = 1.
			{ID: "session-summary-shaped", Tier: trace.TierShort, ReadCount: 1, DerivedFromCount: 1},
			// Regression guard for the 1-source mid leak detector: passive
			// search hits alone must not promote a Hermes session-summary
			// shaped trace.
			{ID: "session-summary-search-hit", Tier: trace.TierShort, SearchHitCount: 10, DerivedFromCount: 1},
		},
	}
	pass := HeuristicPass(cx, PassConfig{}, nil)
	if err := pass(context.Background(), "cron"); err != nil {
		t.Fatalf("pass: %v", err)
	}
	got := map[string]bool{}
	for _, id := range cx.promoted {
		got[id] = true
	}
	for _, want := range []string{"hot", "voted", "referenced"} {
		if !got[want] {
			t.Errorf("expected %q to be promoted, promoted=%v", want, cx.promoted)
		}
	}
	for _, shouldSkip := range []string{"meh", "cold", "session-summary-shaped", "session-summary-search-hit"} {
		if got[shouldSkip] {
			t.Errorf("unexpected promotion of %q", shouldSkip)
		}
	}
}

func TestHeuristicPass_Idempotent(t *testing.T) {
	// In production the promoted trace's tier changes to 'mid' so
	// PromotionCandidates(tier='short') wouldn't return it again. The
	// fake doesn't simulate that — drain the candidate list after
	// promotion to assert the pass's intent: one promotion per trace
	// per time the candidate is surfaced.
	cx := &fakeHeuristicCortex{
		candidates: []cortex.PromotionCandidate{
			{ID: "hot", Tier: trace.TierShort, ReadCount: 10},
		},
	}
	pass := HeuristicPass(cx, PassConfig{}, nil)
	_ = pass(context.Background(), "cron")
	// Simulate: promoted trace no longer in short-term pool.
	cx.candidates = nil
	_ = pass(context.Background(), "cron")

	if len(cx.promoted) != 1 {
		t.Errorf("double-promotion: promoted=%v", cx.promoted)
	}
}

func TestHeuristicPass_CustomThreshold(t *testing.T) {
	cx := &fakeHeuristicCortex{
		candidates: []cortex.PromotionCandidate{
			{ID: "below-default", Tier: trace.TierShort, ReadCount: 3}, // score 3
		},
	}
	pass := HeuristicPass(cx, PassConfig{PromotionThreshold: 2}, nil)
	_ = pass(context.Background(), "cron")
	if len(cx.promoted) != 1 || cx.promoted[0] != "below-default" {
		t.Errorf("custom threshold did not apply: promoted=%v", cx.promoted)
	}
}

func TestHeuristicPass_PromoteErrorDoesNotHaltPass(t *testing.T) {
	// A failed promotion on one candidate shouldn't prevent attempts
	// on the remaining candidates — otherwise a single locked row
	// could block the whole nightly pass.
	cx := &fakeHeuristicCortex{
		candidates: []cortex.PromotionCandidate{
			{ID: "fails", Tier: trace.TierShort, ReadCount: 10},
		},
		promoteErr: errors.New("locked"),
	}
	pass := HeuristicPass(cx, PassConfig{}, nil)
	if err := pass(context.Background(), "cron"); err != nil {
		t.Errorf("pass returned error on individual promote failure: %v", err)
	}
}

func TestHeuristicPass_EmptyCandidateList(t *testing.T) {
	cx := &fakeHeuristicCortex{}
	pass := HeuristicPass(cx, PassConfig{}, nil)
	if err := pass(context.Background(), "cron"); err != nil {
		t.Errorf("empty list should not error: %v", err)
	}
}

func TestHeuristicPass_RespectsContextCancellation(t *testing.T) {
	cx := &fakeHeuristicCortex{
		candidates: []cortex.PromotionCandidate{
			{ID: "a", ReadCount: 10},
			{ID: "b", ReadCount: 10},
		},
	}
	pass := HeuristicPass(cx, PassConfig{}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before pass even starts
	err := pass(ctx, "cron")
	if err == nil {
		t.Error("pass should return ctx.Err() when cancelled")
	}
	if len(cx.promoted) > 0 {
		t.Errorf("cancelled pass still promoted: %v", cx.promoted)
	}
}

// ---- Cortex method tests live alongside the DB-backed cortex suite ----
// see internal/cortex/candidates_test.go for PromotionCandidates coverage.
