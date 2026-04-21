package consolidation

import (
	"context"
	"fmt"
	"time"

	"github.com/Fail-Safe/Noema/internal/cortex"
	"github.com/Fail-Safe/Noema/internal/trace"
)

// GraduationConfig controls the mid→long promotion heuristic. Every
// criterion below is an AND-gate: a trace graduates only when it
// clears every threshold simultaneously. Defaults derive from the
// consolidation-plan §15 design: 14 days + 3 reads + unmodified +
// no active downvotes.
type GraduationConfig struct {
	// MinAge is the minimum age a mid-tier trace must reach before it
	// can be considered for graduation. Zero defaults to 14 days.
	MinAge time.Duration

	// MinReadCount is the minimum read_count for graduation. Zero
	// defaults to 3.
	MinReadCount int

	// RequireUnmodified gates on modify_count == 0. True is the
	// default; set to false when edits are routine in the cortex and
	// shouldn't block graduation.
	RequireUnmodified bool
}

func (c GraduationConfig) resolved() GraduationConfig {
	if c.MinAge <= 0 {
		c.MinAge = 14 * 24 * time.Hour
	}
	if c.MinReadCount <= 0 {
		c.MinReadCount = 3
	}
	// RequireUnmodified is a bool; the zero-value is false. Callers
	// explicitly set it via cortex.GraduationConfig.EffectiveRequire-
	// Unmodified() which preserves the "default true" semantics.
	return c
}

// GraduationProvider is the narrow subset of Cortex the graduation
// pass needs. Separate from HeuristicProvider because the candidate
// query shape is different (mid-tier + age-gated, rather than
// short-tier + rolling-window).
type GraduationProvider interface {
	GraduationCandidates(minAge time.Duration) ([]cortex.PromotionCandidate, error)
	Promote(id, newTier string) error
}

// GraduatePass returns a PassFn that evaluates every mid-tier trace
// older than cfg.MinAge against the AND-gate criteria and promotes
// qualifying ones to long. Runs alongside HeuristicPass on the same
// trigger cadence — each scheduler fire invokes both passes in
// sequence, short→mid first then mid→long.
//
// Why a separate pass rather than extending HeuristicPass: the
// candidate set is disjoint (short vs mid), the thresholds are
// independent, and the graduation rule is intentionally strict and
// AND-gated rather than blended. Keeping the two concerns in separate
// functions means each can evolve without touching the other's tests.
func GraduatePass(cx GraduationProvider, cfg GraduationConfig, log func(format string, args ...any)) PassFn {
	if log == nil {
		log = func(string, ...any) {}
	}
	cfg = cfg.resolved()
	return func(ctx context.Context, trigger string) error {
		candidates, err := cx.GraduationCandidates(cfg.MinAge)
		if err != nil {
			return fmt.Errorf("selecting graduation candidates: %w", err)
		}
		result := PassResult{Trigger: trigger, Considered: len(candidates)}
		for _, pc := range candidates {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if !shouldGraduate(pc, cfg) {
				result.Skipped++
				continue
			}
			if err := cx.Promote(pc.ID, trace.TierLong); err != nil {
				log("[consolidation] graduate failed id=%s: %v", pc.ID, err)
				result.Skipped++
				continue
			}
			result.Promoted++
		}
		log("[consolidation] graduation pass complete trigger=%s considered=%d graduated=%d skipped=%d",
			result.Trigger, result.Considered, result.Promoted, result.Skipped)
		return nil
	}
}

// shouldGraduate applies the AND-gate criteria to a single candidate.
// Exported for tests only; GraduatePass is the sole production caller.
// The age check is implicit — GraduationCandidates already filters to
// created_at <= now-MinAge, so every passed-in pc has cleared the age
// bar.
func shouldGraduate(pc cortex.PromotionCandidate, cfg GraduationConfig) bool {
	if pc.ReadCount < cfg.MinReadCount {
		return false
	}
	if cfg.RequireUnmodified && pc.ModifyCount > 0 {
		return false
	}
	if pc.TierVotes < 0 {
		return false
	}
	return true
}

// ChainPasses composes two PassFns into one that runs them in order.
// Used by cmd_serve to wire heuristic short→mid and graduate mid→long
// onto a single scheduler trigger. If the first pass errors, the
// second still runs — graduation is independent of short-tier
// promotion and shouldn't be blocked by its failure.
func ChainPasses(first, second PassFn) PassFn {
	return func(ctx context.Context, trigger string) error {
		firstErr := first(ctx, trigger)
		secondErr := second(ctx, trigger)
		// Return the first error for visibility, but both passes ran.
		if firstErr != nil {
			return firstErr
		}
		return secondErr
	}
}
