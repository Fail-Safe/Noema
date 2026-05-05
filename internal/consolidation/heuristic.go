package consolidation

import (
	"context"
	"fmt"
	"time"

	"github.com/Fail-Safe/Noema/internal/cortex"
	"github.com/Fail-Safe/Noema/internal/trace"
)

// PassConfig controls the heuristic pass's scoring function and
// window. Zero values use the defaults documented below.
type PassConfig struct {
	// Window bounds the candidate pool to traces created within the
	// last N. Zero defaults to 24h.
	Window time.Duration

	// PromotionThreshold is the minimum blended score a candidate
	// needs to promote from short to mid. Zero defaults to 5 (tuned
	// so: 5 agent reads qualifies, 1 explicit user vote qualifies,
	// 2 derived-from references qualifies).
	PromotionThreshold int

	// Per-signal weights. Zero uses the defaults:
	//   reads    = 1  (weakest; easy to inflate)
	//   modifies = 2  (stronger; agent actively edited)
	//   lineage  = 3  (stronger still; others reference this)
	//   votes    = 5  (strongest; explicit user/agent intent)
	WeightReads    int
	WeightModifies int
	WeightLineage  int
	WeightVotes    int
}

func (c PassConfig) resolved() PassConfig {
	if c.Window == 0 {
		c.Window = 24 * time.Hour
	}
	if c.PromotionThreshold == 0 {
		c.PromotionThreshold = 5
	}
	if c.WeightReads == 0 {
		c.WeightReads = 1
	}
	if c.WeightModifies == 0 {
		c.WeightModifies = 2
	}
	if c.WeightLineage == 0 {
		c.WeightLineage = 3
	}
	if c.WeightVotes == 0 {
		c.WeightVotes = 5
	}
	return c
}

// HeuristicProvider is the subset of Cortex the heuristic pass needs.
// Separate from the narrower scheduler interface in agent.go so the
// agent can still run with a no-op pass for tests that exercise
// cadence without needing promotion plumbing.
type HeuristicProvider interface {
	PromotionCandidates(tier string, window time.Duration) ([]cortex.PromotionCandidate, error)
	Promote(id, newTier string) error
}

// PassResult summarises a single heuristic pass for logging /
// future telemetry. Exported fields because later phases may want
// to surface this in `noema memory stats` or event payloads.
type PassResult struct {
	Trigger    string
	Considered int
	Promoted   int
	Skipped    int
}

// HeuristicPass returns a PassFn that scores every in-window
// short-term candidate and 1:1-promotes those whose score meets the
// threshold. Idempotent across runs because promoted traces leave
// the short-term pool (tier column moves them to mid) and are not
// surfaced by subsequent PromotionCandidates calls.
//
// This is the Phase 8 implementation — LLM-free, no clustering, no
// many-to-one distillation. Phase 9 will add a second pass that
// distills clusters into new mid-term traces atop this baseline.
func HeuristicPass(cx HeuristicProvider, cfg PassConfig, log func(format string, args ...any)) PassFn {
	if log == nil {
		log = func(string, ...any) {}
	}
	cfg = cfg.resolved()
	return func(ctx context.Context, trigger string) error {
		candidates, err := cx.PromotionCandidates(trace.TierShort, cfg.Window)
		if err != nil {
			return fmt.Errorf("selecting short-term candidates: %w", err)
		}
		result := PassResult{Trigger: trigger, Considered: len(candidates)}
		for _, pc := range candidates {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			score := scoreCandidate(pc, cfg)
			if score < cfg.PromotionThreshold {
				result.Skipped++
				continue
			}
			if err := cx.Promote(pc.ID, trace.TierMid); err != nil {
				log("[consolidation] promote failed id=%s: %v", pc.ID, err)
				result.Skipped++
				continue
			}
			result.Promoted++
		}
		log("[consolidation] heuristic pass complete trigger=%s considered=%d promoted=%d skipped=%d",
			result.Trigger, result.Considered, result.Promoted, result.Skipped)
		return nil
	}
}

// MinLineageSourcesForCredit is the minimum derived_from count that
// earns the lineage weight in the heuristic score. A single derived_from
// is provenance metadata (this trace was extracted from / supersedes /
// annotates one other trace), not consolidation, and shouldn't tilt the
// promotion threshold on its own. Two-or-more is the same bar
// CreateDistilledTrace enforces (cortex.ErrDistillSourcesInsufficient at
// internal/cortex/distill.go:76), so the heuristic agrees with the
// distillation entry point on what counts as a real synthesis.
const MinLineageSourcesForCredit = 2

// scoreCandidate computes the blended signal for a single trace.
// Exported for tests; package-private callers don't need it because
// HeuristicPass is the single consumer.
//
// search_hit_count is folded into the reads bucket at the same weight
// because both signals describe passive/weak consumption — the read
// either came from a deliberate get_trace or from being one of the
// top-N hits an agent's search returned. Auto-injection providers like
// Hermes only ever generate search hits, and without this fold-in
// short-tier traces in those cortexes would never accumulate enough
// signal to promote.
//
// Lineage credit is gated by MinLineageSourcesForCredit: a trace with
// only one derived_from gets zero lineage points, regardless of the
// configured WeightLineage. This stops 1-source provenance links (e.g.
// the Hermes session-summary pattern, which sets derived_from to the
// single session trace it was extracted from) from gliding past the
// promotion threshold purely on the lineage head start. Multi-source
// traces still earn the full weight per source.
func scoreCandidate(pc cortex.PromotionCandidate, cfg PassConfig) int {
	lineageCredit := 0
	if pc.DerivedFromCount >= MinLineageSourcesForCredit {
		lineageCredit = pc.DerivedFromCount * cfg.WeightLineage
	}
	return (pc.ReadCount+pc.SearchHitCount)*cfg.WeightReads +
		pc.ModifyCount*cfg.WeightModifies +
		lineageCredit +
		pc.TierVotes*cfg.WeightVotes
}
