package consolidation

import (
	"context"
	"fmt"
	"time"

	"github.com/Fail-Safe/Noema/internal/cortex"
	"github.com/Fail-Safe/Noema/internal/trace"
)

// LLMCortex is the Cortex surface the LLM-driven consolidation
// pipeline needs. Separate from the narrower scheduler interface in
// agent.go so tests of cadence don't have to stub promotion paths.
type LLMCortex interface {
	PromotionCandidates(tier string, window time.Duration) ([]cortex.PromotionCandidate, error)
	Promote(id, newTier string) error
	CreateDistilledTrace(spec cortex.DistilledTraceSpec) (string, error)
	TraceFile(id string, archived bool) string
	Get(id string) (*cortex.Row, error)
}

// PipelineConfig carries everything an LLM pass needs that isn't the
// Cortex or the LLM itself. Sourced from cortex.md's
// ConsolidationConfig plus CLI overrides.
type PipelineConfig struct {
	Window     time.Duration
	ModelTier  string
	ModelName  string
	MaxRetries int
	DryRun     bool
}

// PipelineResult summarises a single pass so the CLI can log what
// happened without re-deriving it from events.
type PipelineResult struct {
	CandidatesConsidered int
	ClustersAttempted    int
	DistillationsCreated int
	FallbackPromotions   int
	Rejected             int
	Skipped              int
}

// RunLLMPass reads candidates from the cortex, groups them into
// clusters bounded by the model-tier profile's max cluster size,
// runs each cluster through the profile (cohesion + template /
// single-shot JSON), validates the output, and either records the
// distilled trace or falls back to heuristic 1:1 promotion.
//
// Context cancellation halts the pass at the next cluster boundary.
// Individual cluster failures are logged and skipped — one bad
// cluster should not abort the whole run.
func RunLLMPass(ctx context.Context, cx LLMCortex, llm LLMClient, cfg PipelineConfig, log func(format string, args ...any)) (PipelineResult, error) {
	if log == nil {
		log = func(string, ...any) {}
	}
	var result PipelineResult

	candidates, err := cx.PromotionCandidates(trace.TierShort, cfg.Window)
	if err != nil {
		return result, fmt.Errorf("selecting candidates: %w", err)
	}
	result.CandidatesConsidered = len(candidates)
	if len(candidates) < 2 {
		log("[consolidate] %d short-term candidates in window; nothing to cluster", len(candidates))
		return result, nil
	}

	profile := GetProfile(cfg.ModelTier)
	maxCluster := profile.MaxClusterSize()

	groups := groupByLineage(candidates)
	for _, g := range groups {
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		// Split oversized groups into cluster-sized chunks so the
		// model-tier ceiling isn't violated. Heuristic: preserve
		// lineage groupings even when chunked; the order is
		// stable so the same input produces the same chunks.
		for i := 0; i < len(g); i += maxCluster {
			end := i + maxCluster
			if end > len(g) {
				end = len(g)
			}
			chunk := g[i:end]
			if len(chunk) < 2 {
				result.Skipped++
				continue
			}
			result.ClustersAttempted++

			cluster, err := buildCluster(cx, chunk)
			if err != nil {
				log("[consolidate] build cluster failed: %v", err)
				result.Skipped++
				continue
			}

			d, runErr := runWithRetry(ctx, llm, profile, cfg.ModelName, cluster, cfg.MaxRetries)
			switch {
			case runErr != nil:
				log("[consolidate] cluster failed after retries: %v; falling back to heuristic promotion", runErr)
				result.FallbackPromotions += heuristicFallback(cx, chunk, log)
			case !d.Cohesive:
				log("[consolidate] cluster rejected as incohesive (ids=%s)", idsOf(chunk))
				result.Rejected++
			default:
				if err := submit(cx, cfg, d, chunk, profile.Name(), log); err != nil {
					log("[consolidate] submit failed (ids=%s): %v", idsOf(chunk), err)
					result.Skipped++
					continue
				}
				result.DistillationsCreated++
			}
		}
	}

	log("[consolidate] pass complete: considered=%d attempted=%d distilled=%d rejected=%d fallback-promoted=%d skipped=%d",
		result.CandidatesConsidered, result.ClustersAttempted, result.DistillationsCreated,
		result.Rejected, result.FallbackPromotions, result.Skipped)
	return result, nil
}

// groupByLineage clusters candidates by their first derived_from
// ancestor (if any), else by orphan pool. Simple heuristic: the
// pipeline never calls this without candidates, and the stable
// ordering matters for test reproducibility.
func groupByLineage(candidates []cortex.PromotionCandidate) [][]cortex.PromotionCandidate {
	// Phase 8 already has a similar grouping elsewhere, but the LLM
	// pass needs its own ordering that respects the profile's
	// max-cluster ceiling. For now the heuristic is "orphans go into
	// one catch-all bucket in original order". Later phases can
	// refine with tag-overlap / FTS5-similarity secondary grouping.
	if len(candidates) == 0 {
		return nil
	}
	// Single orphan pool keeps the logic simple and testable. The
	// per-profile max-cluster chunker in RunLLMPass splits the pool
	// into right-sized slices.
	return [][]cortex.PromotionCandidate{candidates}
}

func buildCluster(cx LLMCortex, chunk []cortex.PromotionCandidate) (ClusterInput, error) {
	var c ClusterInput
	for _, pc := range chunk {
		row, err := cx.Get(pc.ID)
		if err != nil {
			return c, fmt.Errorf("loading row for %s: %w", pc.ID, err)
		}
		// Body lives in the markdown file, not the row. ParseFile
		// reads it; consolidation runs on the canonical active path
		// (archived/trashed rows would have been filtered earlier).
		path := cx.TraceFile(pc.ID, row.ArchivedAt != "")
		t, err := trace.ParseFile(path)
		if err != nil {
			return c, fmt.Errorf("parsing trace file for %s: %w", pc.ID, err)
		}
		c.Traces = append(c.Traces, TraceInput{
			ID:    pc.ID,
			Title: row.Title,
			Body:  t.Body,
			Tags:  row.Tags,
		})
	}
	return c, nil
}

func runWithRetry(ctx context.Context, llm LLMClient, profile Profile, model string, cluster ClusterInput, maxRetries int) (Distillation, error) {
	if maxRetries < 0 {
		maxRetries = 0
	}
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		d, err := profile.Run(ctx, llm, model, cluster)
		if err == nil {
			return d, nil
		}
		lastErr = err
	}
	return Distillation{}, lastErr
}

// heuristicFallback promotes every candidate in the chunk 1:1 so a
// pass that couldn't produce a distillation still moves qualifying
// short-term traces into mid-term. Individual Promote failures are
// logged but don't abort the fallback loop — the caller already
// degraded from LLM to heuristic; one further failure shouldn't
// cascade.
func heuristicFallback(cx LLMCortex, chunk []cortex.PromotionCandidate, log func(format string, args ...any)) int {
	promoted := 0
	for _, pc := range chunk {
		if err := cx.Promote(pc.ID, trace.TierMid); err != nil {
			log("[consolidate] fallback promote failed id=%s: %v", pc.ID, err)
			continue
		}
		promoted++
	}
	return promoted
}

func submit(cx LLMCortex, cfg PipelineConfig, d Distillation, chunk []cortex.PromotionCandidate, profileName string, log func(format string, args ...any)) error {
	sources := make([]string, 0, len(chunk))
	for _, pc := range chunk {
		sources = append(sources, pc.ID)
	}
	if cfg.DryRun {
		log("[consolidate] DRY-RUN would distill %d sources -> %q (%d tags, %d body chars, confidence=%.2f)",
			len(sources), d.Title, len(d.Tags), len(d.Body), d.Confidence)
		return nil
	}
	id, err := cx.CreateDistilledTrace(cortex.DistilledTraceSpec{
		Title:              d.Title,
		Body:               d.Body,
		Tags:               d.Tags,
		SourceIDs:          sources,
		ModelName:          cfg.ModelName,
		ModelTierProfile:   profileName,
		CohesionConfidence: d.Confidence,
	})
	if err != nil {
		return err
	}
	log("[consolidate] distilled %d sources -> %s (%q)", len(sources), id, d.Title)
	return nil
}

func idsOf(chunk []cortex.PromotionCandidate) string {
	ids := make([]string, len(chunk))
	for i, pc := range chunk {
		ids[i] = pc.ID
	}
	if len(ids) <= 3 {
		return fmt.Sprintf("%v", ids)
	}
	return fmt.Sprintf("%v...(%d total)", ids[:3], len(ids))
}
