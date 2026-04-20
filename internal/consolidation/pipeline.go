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

	// ClusterResults is the per-cluster breakdown. Populated for every
	// cluster the pipeline considered — distilled, rejected, skipped,
	// or fallback-promoted. Used by --emit-json for prompt-tuning
	// evaluation (a frontier model can score each distillation against
	// its sources to turn vibes into numbers).
	ClusterResults []ClusterResult `json:"cluster_results,omitempty"`
}

// ClusterResult is the per-cluster record for one pass. Marshals to
// JSON for --emit-json consumption.
type ClusterResult struct {
	IDs         []string      `json:"ids"`
	Bucket      string        `json:"bucket"` // groupKey() output, e.g. "note|2026-04-13"
	Profile     string        `json:"profile"`
	Outcome     string        `json:"outcome"` // distilled | rejected | skipped | fallback | error
	Title       string        `json:"title,omitempty"`
	Tags        []string      `json:"tags,omitempty"`
	Body        string        `json:"body,omitempty"`
	Confidence  float64       `json:"confidence,omitempty"`
	Reason      string        `json:"reason,omitempty"`
	Sources     []SourceTrace `json:"sources,omitempty"` // only populated on --emit-json
}

// SourceTrace mirrors TraceInput but is the JSON-serialization shape.
// Kept separate so the internal pipeline type can evolve without
// changing the emit-json schema.
type SourceTrace struct {
	ID    string   `json:"id"`
	Title string   `json:"title"`
	Tags  []string `json:"tags,omitempty"`
	Body  string   `json:"body,omitempty"`
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

	groups := groupCandidates(candidates)
	for gi, g := range groups {
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		// Split oversized groups into cluster-sized chunks so the
		// model-tier ceiling isn't violated. Heuristic: preserve
		// lineage groupings even when chunked; the order is
		// stable so the same input produces the same chunks.
		bucket := ""
		if len(g) > 0 {
			bucket = groupKey(g[0])
		}
		for i := 0; i < len(g); i += maxCluster {
			end := i + maxCluster
			if end > len(g) {
				end = len(g)
			}
			chunk := g[i:end]
			cr := ClusterResult{
				IDs:     idSlice(chunk),
				Bucket:  bucket,
				Profile: profile.Name(),
			}
			if len(chunk) < 2 {
				result.Skipped++
				cr.Outcome = "skipped"
				cr.Reason = "singleton chunk after bucketing"
				result.ClusterResults = append(result.ClusterResults, cr)
				continue
			}
			result.ClustersAttempted++

			cluster, err := buildCluster(cx, chunk)
			if err != nil {
				log("[consolidate] build cluster failed: %v", err)
				result.Skipped++
				cr.Outcome = "error"
				cr.Reason = fmt.Sprintf("build cluster: %v", err)
				result.ClusterResults = append(result.ClusterResults, cr)
				continue
			}
			cr.Sources = sourceSnapshot(cluster)

			d, runErr := runWithRetry(ctx, llm, profile, cfg.ModelName, cluster, cfg.MaxRetries)
			switch {
			case runErr != nil:
				if cfg.DryRun {
					// In dry-run mode the fallback path must not
					// write to the cortex — the whole point of
					// --dry-run is zero side effects, and a user
					// Ctrl-Cing a dry-run shouldn't silently mutate
					// tier state as a consolation prize.
					log("[consolidate] cluster failed after retries: %v; dry-run suppressed fallback promotion", runErr)
					result.Skipped++
					cr.Outcome = "skipped"
					cr.Reason = fmt.Sprintf("llm error (dry-run fallback suppressed): %v", runErr)
				} else {
					log("[consolidate] cluster failed after retries: %v; falling back to heuristic promotion", runErr)
					n := heuristicFallback(cx, chunk, log)
					result.FallbackPromotions += n
					cr.Outcome = "fallback"
					cr.Reason = fmt.Sprintf("llm error, heuristic-promoted %d: %v", n, runErr)
				}
			case !d.Cohesive:
				log("[consolidate] cluster rejected as incohesive (ids=%s)", idsOf(chunk))
				result.Rejected++
				cr.Outcome = "rejected"
				cr.Reason = "cohesion gate returned no"
			default:
				if err := submit(cx, cfg, d, chunk, profile.Name(), log); err != nil {
					log("[consolidate] submit failed (ids=%s): %v", idsOf(chunk), err)
					result.Skipped++
					cr.Outcome = "error"
					cr.Reason = fmt.Sprintf("submit: %v", err)
				} else {
					result.DistillationsCreated++
					cr.Outcome = "distilled"
					cr.Title = d.Title
					cr.Tags = d.Tags
					cr.Body = d.Body
					cr.Confidence = d.Confidence
				}
			}
			result.ClusterResults = append(result.ClusterResults, cr)
		}
		_ = gi
	}

	log("[consolidate] pass complete: considered=%d attempted=%d distilled=%d rejected=%d fallback-promoted=%d skipped=%d",
		result.CandidatesConsidered, result.ClustersAttempted, result.DistillationsCreated,
		result.Rejected, result.FallbackPromotions, result.Skipped)
	return result, nil
}

// groupCandidates buckets candidates by (type, day-of-creation) so a
// cluster handed to the LLM is more likely to be actually cohesive.
// The prior implementation returned a single orphan pool and chunked
// by cluster-size — that produced arbitrary groupings that a 9B
// reasoning model quite correctly rejected as incohesive. Grouping
// by type handles the common case where a cortex has families of
// similar traces (all hermes-session, all session-summary, all
// weekly-news, etc.) that want to be consolidated within their
// family. Grouping by day adds a temporal cohesion signal —
// "yesterday's observations" is a reasonable unit.
//
// Groups are returned in deterministic order (sorted by key) so
// test reproducibility is trivial. Within a group, original order
// (created_at DESC from the candidate query) is preserved.
//
// Skips candidates with empty IDs as a defensive measure against
// any stray row in the traces table — the pipeline's buildCluster
// would fail on them anyway, but filtering here means the count
// summary at the end of the pass is accurate rather than inflated.
func groupCandidates(candidates []cortex.PromotionCandidate) [][]cortex.PromotionCandidate {
	if len(candidates) == 0 {
		return nil
	}
	buckets := make(map[string][]cortex.PromotionCandidate)
	var keys []string
	for _, pc := range candidates {
		if pc.ID == "" {
			continue
		}
		key := groupKey(pc)
		if _, seen := buckets[key]; !seen {
			keys = append(keys, key)
		}
		buckets[key] = append(buckets[key], pc)
	}
	// Deterministic order: sort keys alphabetically so two runs on
	// the same input produce the same group ordering (useful for
	// tests and for operator sanity when comparing consecutive
	// pass outputs).
	sortStrings(keys)
	out := make([][]cortex.PromotionCandidate, 0, len(keys))
	for _, k := range keys {
		out = append(out, buckets[k])
	}
	return out
}

// groupKey returns the bucket identifier for a candidate. Day is
// extracted from the ISO-8601 created_at prefix (YYYY-MM-DD). Empty
// type defaults to "none" so the bucket still forms a valid key.
func groupKey(pc cortex.PromotionCandidate) string {
	day := pc.CreatedAt
	if len(day) >= 10 {
		day = day[:10]
	}
	typ := pc.Type
	if typ == "" {
		typ = "none"
	}
	return typ + "|" + day
}

// sortStrings is a tiny helper that avoids pulling in the sort
// package at the top of this file — all we need is an in-place
// lexical sort of a string slice, and this stays allocation-free
// even with dozens of buckets.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
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
	// Normalize tags defensively — local models emit human-readable
	// phrases ("MCP Server", "AI SME", "career goals") and the cortex
	// expects kebab-case. Prompt tuning alone is not a guarantee;
	// belt-and-braces here prevents garbage tags from landing in the DB.
	d.Tags = normalizeTags(d.Tags)
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

// idSlice returns just the IDs from a candidate chunk — used to
// populate ClusterResult.IDs for JSON emission.
func idSlice(chunk []cortex.PromotionCandidate) []string {
	out := make([]string, len(chunk))
	for i, pc := range chunk {
		out[i] = pc.ID
	}
	return out
}

// sourceSnapshot captures the TraceInput data as SourceTrace records
// for JSON emission. A judge needs the source bodies to assess
// whether a distillation faithfully represents them; without this
// the JSON is empty scaffolding.
func sourceSnapshot(c ClusterInput) []SourceTrace {
	out := make([]SourceTrace, len(c.Traces))
	for i, t := range c.Traces {
		out[i] = SourceTrace{ID: t.ID, Title: t.Title, Tags: t.Tags, Body: t.Body}
	}
	return out
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
