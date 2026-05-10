package consolidation_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/Fail-Safe/Noema/internal/consolidation"
	"github.com/Fail-Safe/Noema/internal/cortex"
	"github.com/Fail-Safe/Noema/internal/trace"
)

// The tests below are the acceptance tests for Phase 11: a full
// end-to-end LLM-driven consolidation pass against a real cortex
// (in a temp dir) with a stubbed LLM. If these go green we know the
// candidate selection, clustering, prompt construction, response
// parsing, and distilled-trace creation all compose correctly.

type scriptedLLM struct {
	responses []string
	calls     []consolidation.CompletionRequest
}

func (s *scriptedLLM) Complete(_ context.Context, req consolidation.CompletionRequest) (string, error) {
	s.calls = append(s.calls, req)
	if len(s.responses) == 0 {
		return "", &stubErr{msg: "scripted llm queue exhausted"}
	}
	r := s.responses[0]
	s.responses = s.responses[1:]
	return r, nil
}

type stubErr struct{ msg string }

func (e *stubErr) Error() string { return e.msg }

func setupCortex(t *testing.T) *cortex.Cortex {
	t.Helper()
	dir := t.TempDir()
	if _, err := cortex.Create("pipeline-test", dir); err != nil {
		t.Fatalf("Create: %v", err)
	}
	cx, err := cortex.Open("pipeline-test", filepath.Join(dir, "pipeline-test"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { cx.Close() })
	return cx
}

// seedTraces inserts n short-term traces with varying content so
// there's enough signal for the pipeline to pick them up.
func seedTraces(t *testing.T, cx *cortex.Cortex, n int) []string {
	t.Helper()
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		tr := trace.New(
			titleFor(i),
			"note",
			"agent-1",
			[]string{"auth"},
			bodyFor(i),
		)
		if err := cx.Add(tr); err != nil {
			t.Fatalf("Add %d: %v", i, err)
		}
		ids = append(ids, tr.ID)
	}
	return ids
}

func titleFor(i int) string {
	titles := []string{
		"OAuth kickoff",
		"Session TTL debate",
		"Token rotation plan",
	}
	return titles[i%len(titles)]
}

func bodyFor(i int) string {
	return "Content for trace " + []string{"alpha", "beta", "gamma"}[i%3] + "."
}

// TestRunLLMPass_HappyPath_Frontier exercises the single-shot JSON
// profile end-to-end: seed three short traces, stub one JSON
// response, run the pass, verify a mid-tier trace was created with
// derived_from pointing at all three sources.
func TestRunLLMPass_HappyPath_Frontier(t *testing.T) {
	cx := setupCortex(t)
	seedTraces(t, cx, 3)

	llm := &scriptedLLM{responses: []string{
		`{"cohesive": true, "title": "Auth strategy — distilled", "tags": ["auth", "consolidated"], "body": "All three auth notes consolidated into one memory.", "confidence": 0.85}`,
	}}

	result, err := consolidation.RunLLMPass(context.Background(), cx, llm, consolidation.PipelineConfig{
		Window:    24 * time.Hour,
		ModelTier: "frontier",
		ModelName: "claude-opus-4-7",
	}, nil)
	if err != nil {
		t.Fatalf("RunLLMPass: %v", err)
	}

	if result.DistillationsCreated != 1 {
		t.Errorf("DistillationsCreated = %d, want 1", result.DistillationsCreated)
	}
	if result.CandidatesConsidered != 3 {
		t.Errorf("CandidatesConsidered = %d, want 3", result.CandidatesConsidered)
	}

	// Verify a single mid-tier trace exists for the distilled summary;
	// sources stay at short under the current policy. derived_from on
	// the distilled row keeps the originals reachable, and LLMCandidates
	// will skip them on the next pass via the ActionConsolidate event.
	rows, err := cx.List(cortex.ListOptions{Tiers: []string{trace.TierMid}})
	if err != nil {
		t.Fatalf("List mid: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("mid tier rows = %d, want 1 (distilled only; sources stay at short)", len(rows))
	}
	distilledID := rows[0].ID
	if rows[0].Title != "Auth strategy — distilled" {
		t.Fatalf("mid-tier row title = %q, want %q", rows[0].Title, "Auth strategy — distilled")
	}
	shortRows, err := cx.List(cortex.ListOptions{Tiers: []string{trace.TierShort}})
	if err != nil {
		t.Fatalf("List short: %v", err)
	}
	if len(shortRows) != 3 {
		t.Errorf("short tier rows = %d, want 3 (sources untouched)", len(shortRows))
	}
	// List doesn't populate DerivedFrom (that's a separate lineage
	// query); fetch via Get to verify the derived_from links.
	full, err := cx.Get(distilledID)
	if err != nil {
		t.Fatalf("Get distilled: %v", err)
	}
	if len(full.DerivedFrom) != 3 {
		t.Errorf("derived_from = %v, want 3 entries", full.DerivedFrom)
	}
}

// TestRunLLMPass_NotCohesive_NoDistillation pins that when the
// cohesion gate returns no (small/large profiles) or cohesive:false
// (frontier), the pipeline doesn't create a distilled trace. The
// cluster counts as "attempted and rejected" in the result summary.
func TestRunLLMPass_NotCohesive_NoDistillation(t *testing.T) {
	cx := setupCortex(t)
	seedTraces(t, cx, 3)

	llm := &scriptedLLM{responses: []string{
		`{"cohesive": false}`,
	}}
	result, err := consolidation.RunLLMPass(context.Background(), cx, llm, consolidation.PipelineConfig{
		Window:    24 * time.Hour,
		ModelTier: "frontier",
		ModelName: "m",
	}, nil)
	if err != nil {
		t.Fatalf("RunLLMPass: %v", err)
	}
	if result.DistillationsCreated != 0 {
		t.Errorf("DistillationsCreated = %d, want 0", result.DistillationsCreated)
	}
	if result.Rejected != 1 {
		t.Errorf("Rejected = %d, want 1", result.Rejected)
	}

	rows, _ := cx.List(cortex.ListOptions{Tiers: []string{trace.TierMid}})
	if len(rows) != 0 {
		t.Errorf("mid tier rows = %d, want 0 (rejected cluster)", len(rows))
	}
}

// TestRunLLMPass_DryRun_SkipsWrite runs the full prompt + parse
// dance but never calls CreateDistilledTrace. Useful for operators
// to sanity-check their LLM responses before letting the pipeline
// write to the cortex.
func TestRunLLMPass_DryRun_SkipsWrite(t *testing.T) {
	cx := setupCortex(t)
	seedTraces(t, cx, 3)

	llm := &scriptedLLM{responses: []string{
		`{"cohesive": true, "title": "Dry title", "tags": ["t"], "body": "Dry body", "confidence": 0.5}`,
	}}
	result, err := consolidation.RunLLMPass(context.Background(), cx, llm, consolidation.PipelineConfig{
		Window:    24 * time.Hour,
		ModelTier: "frontier",
		ModelName: "m",
		DryRun:    true,
	}, nil)
	if err != nil {
		t.Fatalf("RunLLMPass dry-run: %v", err)
	}
	if result.DistillationsCreated != 1 {
		t.Errorf("DistillationsCreated counter should tick in dry-run for reporting, got %d", result.DistillationsCreated)
	}

	// But nothing actually landed in the cortex.
	rows, _ := cx.List(cortex.ListOptions{Tiers: []string{trace.TierMid}})
	if len(rows) != 0 {
		t.Errorf("dry-run wrote to the cortex anyway: %d mid-tier rows", len(rows))
	}
}

// TestRunLLMPass_LLMError_FallsBackToHeuristic pins the graceful-
// degradation rail when the LLM step repeatedly fails. The fallback
// runs the same heuristic score gate as the standalone HeuristicPass:
// candidates without enough signal stay at short rather than getting
// dragged forward unconditionally. Three zero-engagement seed traces
// score 0 — well under the threshold of 5 — so none should promote.
func TestRunLLMPass_LLMError_FallsBackToHeuristic_GatedByScore(t *testing.T) {
	cx := setupCortex(t)
	seedTraces(t, cx, 3)

	llm := &scriptedLLM{}

	result, err := consolidation.RunLLMPass(context.Background(), cx, llm, consolidation.PipelineConfig{
		Window:     24 * time.Hour,
		ModelTier:  "frontier",
		ModelName:  "m",
		MaxRetries: 1,
	}, nil)
	if err != nil {
		t.Fatalf("RunLLMPass: %v", err)
	}
	if result.DistillationsCreated != 0 {
		t.Errorf("DistillationsCreated = %d, want 0 (all failed)", result.DistillationsCreated)
	}
	if result.FallbackPromotions != 0 {
		t.Errorf("FallbackPromotions = %d, want 0 (zero-engagement seeds shouldn't pass the gate)", result.FallbackPromotions)
	}

	rows, _ := cx.List(cortex.ListOptions{Tiers: []string{trace.TierMid}})
	if len(rows) != 0 {
		t.Errorf("mid tier rows after gated fallback = %d, want 0", len(rows))
	}
}

// TestRunLLMPass_LLMError_FallbackPromotesQualifyingCandidates pairs
// with the gated test above: when a candidate has accumulated enough
// signal to clear the heuristic threshold, the fallback should still
// promote it. Three reads on the seeded trace gives score=3 with the
// default reads weight (1) and a derived_from of 2 adds the lineage
// credit (≥2 sources × WeightLineage 3 = 6) for a total of 9, well
// over the threshold of 5.
func TestRunLLMPass_LLMError_FallbackPromotesQualifyingCandidates(t *testing.T) {
	cx := setupCortex(t)
	ids := seedTraces(t, cx, 3)

	// Bump tier_votes on each seed so the heuristic score reaches the
	// threshold without needing to fabricate read/modify history.
	// One vote × WeightVotes (5) = 5, exactly at threshold.
	for _, id := range ids {
		if err := cx.Vote(id, +1, cortex.ActorAgent); err != nil {
			t.Fatalf("Vote %s: %v", id, err)
		}
	}

	llm := &scriptedLLM{}
	result, err := consolidation.RunLLMPass(context.Background(), cx, llm, consolidation.PipelineConfig{
		Window:     24 * time.Hour,
		ModelTier:  "frontier",
		ModelName:  "m",
		MaxRetries: 1,
	}, nil)
	if err != nil {
		t.Fatalf("RunLLMPass: %v", err)
	}
	if result.FallbackPromotions != 3 {
		t.Errorf("FallbackPromotions = %d, want 3 (one vote each clears threshold)", result.FallbackPromotions)
	}
	rows, _ := cx.List(cortex.ListOptions{Tiers: []string{trace.TierMid}})
	if len(rows) != 3 {
		t.Errorf("mid tier rows after qualifying fallback = %d, want 3", len(rows))
	}
}

// TestRunLLMPass_NoCandidates_EarlyReturn is a safety test: when
// there are fewer than 2 candidates (can't form a cluster of >=2),
// the pipeline returns cleanly without calling the LLM at all.
func TestRunLLMPass_NoCandidates_EarlyReturn(t *testing.T) {
	cx := setupCortex(t)
	// Seed just one trace.
	seedTraces(t, cx, 1)

	llm := &scriptedLLM{responses: []string{"should-not-be-called"}}
	result, err := consolidation.RunLLMPass(context.Background(), cx, llm, consolidation.PipelineConfig{
		Window:    24 * time.Hour,
		ModelTier: "frontier",
		ModelName: "m",
	}, nil)
	if err != nil {
		t.Fatalf("RunLLMPass: %v", err)
	}
	if len(llm.calls) != 0 {
		t.Errorf("llm called %d times with no cluster possible", len(llm.calls))
	}
	if result.ClustersAttempted != 0 {
		t.Errorf("ClustersAttempted = %d, want 0", result.ClustersAttempted)
	}
}
