package consolidation_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Fail-Safe/Noema/internal/consolidation"
	"github.com/Fail-Safe/Noema/internal/cortex"
)

// fakeGraduationProvider lets the graduation-pass tests drive
// arbitrary candidate sets without standing up a real cortex +
// clock-manipulating the created_at column.
type fakeGraduationProvider struct {
	candidates []cortex.PromotionCandidate
	promoted   []string
	promoteErr error
}

func (f *fakeGraduationProvider) GraduationCandidates(time.Duration) ([]cortex.PromotionCandidate, error) {
	return f.candidates, nil
}

func (f *fakeGraduationProvider) Promote(id, newTier string) error {
	if f.promoteErr != nil {
		return f.promoteErr
	}
	f.promoted = append(f.promoted, id)
	return nil
}

func TestGraduatePass_AllCriteriaMet(t *testing.T) {
	provider := &fakeGraduationProvider{
		candidates: []cortex.PromotionCandidate{
			{ID: "a", Tier: "mid", ReadCount: 5, ModifyCount: 0, TierVotes: 0},
		},
	}
	pass := consolidation.GraduatePass(provider, consolidation.GraduationConfig{
		MinAge: 14 * 24 * time.Hour, MinReadCount: 3, AllowModified: false,
	}, nil)

	if err := pass(context.Background(), "cron"); err != nil {
		t.Fatalf("pass: %v", err)
	}
	if len(provider.promoted) != 1 || provider.promoted[0] != "a" {
		t.Errorf("promoted = %v, want [a]", provider.promoted)
	}
}

func TestGraduatePass_InsufficientReads(t *testing.T) {
	provider := &fakeGraduationProvider{
		candidates: []cortex.PromotionCandidate{
			{ID: "a", Tier: "mid", ReadCount: 1, ModifyCount: 0, TierVotes: 0},
		},
	}
	pass := consolidation.GraduatePass(provider, consolidation.GraduationConfig{
		MinReadCount: 3, AllowModified: false,
	}, nil)
	_ = pass(context.Background(), "cron")
	if len(provider.promoted) != 0 {
		t.Errorf("promoted = %v, want empty (read_count below threshold)", provider.promoted)
	}
}

func TestGraduatePass_ModifiedTraceBlocked(t *testing.T) {
	// RequireUnmodified=true: any edit since creation blocks
	// graduation, even with strong usage signals.
	provider := &fakeGraduationProvider{
		candidates: []cortex.PromotionCandidate{
			{ID: "a", Tier: "mid", ReadCount: 99, ModifyCount: 1, TierVotes: 5},
		},
	}
	pass := consolidation.GraduatePass(provider, consolidation.GraduationConfig{
		MinReadCount: 3, AllowModified: false,
	}, nil)
	_ = pass(context.Background(), "cron")
	if len(provider.promoted) != 0 {
		t.Errorf("promoted = %v, want empty (modify_count > 0)", provider.promoted)
	}
}

func TestGraduatePass_ModifiedTraceAllowedWhenNotRequired(t *testing.T) {
	// Inverse: when AllowModified is true, modify_count doesn't gate.
	provider := &fakeGraduationProvider{
		candidates: []cortex.PromotionCandidate{
			{ID: "a", Tier: "mid", ReadCount: 5, ModifyCount: 3, TierVotes: 0},
		},
	}
	pass := consolidation.GraduatePass(provider, consolidation.GraduationConfig{
		MinReadCount: 3, AllowModified: true,
	}, nil)
	_ = pass(context.Background(), "cron")
	if len(provider.promoted) != 1 {
		t.Errorf("promoted = %v, want [a] (RequireUnmodified=false)", provider.promoted)
	}
}

func TestGraduatePass_NegativeVotesBlock(t *testing.T) {
	// Any active downvote (tier_votes < 0) is a veto: the trace was
	// flagged as not-durable by a user or agent.
	provider := &fakeGraduationProvider{
		candidates: []cortex.PromotionCandidate{
			{ID: "a", Tier: "mid", ReadCount: 99, ModifyCount: 0, TierVotes: -1},
		},
	}
	pass := consolidation.GraduatePass(provider, consolidation.GraduationConfig{
		MinReadCount: 3, AllowModified: false,
	}, nil)
	_ = pass(context.Background(), "cron")
	if len(provider.promoted) != 0 {
		t.Errorf("promoted = %v, want empty (tier_votes < 0)", provider.promoted)
	}
}

func TestGraduatePass_PromoteErrorCountsAsSkip(t *testing.T) {
	// A Promote error (e.g. trigger refused, trace purged between
	// query and promote) must not abort the pass — the next candidate
	// still gets its chance.
	provider := &fakeGraduationProvider{
		candidates: []cortex.PromotionCandidate{
			{ID: "a", Tier: "mid", ReadCount: 5, ModifyCount: 0, TierVotes: 0},
			{ID: "b", Tier: "mid", ReadCount: 5, ModifyCount: 0, TierVotes: 0},
		},
		promoteErr: errors.New("simulated failure"),
	}
	pass := consolidation.GraduatePass(provider, consolidation.GraduationConfig{
		MinReadCount: 3, AllowModified: false,
	}, nil)
	if err := pass(context.Background(), "cron"); err != nil {
		t.Fatalf("pass error leaked despite per-candidate swallowing: %v", err)
	}
	// Neither was recorded as promoted (provider.promoted only appends
	// on success, and promoteErr short-circuits that path).
	if len(provider.promoted) != 0 {
		t.Errorf("promoted = %v, want empty (both should have failed)", provider.promoted)
	}
}

func TestChainPasses_BothRun(t *testing.T) {
	// ChainPasses must invoke both even if the first errors.
	calls := []string{}
	first := func(context.Context, string) error {
		calls = append(calls, "first")
		return errors.New("first error")
	}
	second := func(context.Context, string) error {
		calls = append(calls, "second")
		return nil
	}
	chained := consolidation.ChainPasses(first, second)
	err := chained(context.Background(), "cron")
	if err == nil || err.Error() != "first error" {
		t.Errorf("err = %v, want first error", err)
	}
	if len(calls) != 2 || calls[0] != "first" || calls[1] != "second" {
		t.Errorf("calls = %v, want [first second]", calls)
	}
}
