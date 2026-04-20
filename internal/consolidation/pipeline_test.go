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

	// Verify a mid-tier trace now exists with derived_from linking
	// to all three sources.
	rows, err := cx.List(cortex.ListOptions{Tiers: []string{trace.TierMid}})
	if err != nil {
		t.Fatalf("List mid: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("mid tier rows = %d, want 1", len(rows))
	}
	if rows[0].Title != "Auth strategy — distilled" {
		t.Errorf("mid title = %q", rows[0].Title)
	}
	// List doesn't populate DerivedFrom (that's a separate lineage
	// query); fetch via Get to verify the derived_from links.
	full, err := cx.Get(rows[0].ID)
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
// degradation rail from Phase 8: when the LLM step repeatedly fails,
// the pipeline drops to 1:1 heuristic promotion rather than leaving
// the candidates in short-term forever.
func TestRunLLMPass_LLMError_FallsBackToHeuristic(t *testing.T) {
	cx := setupCortex(t)
	seedTraces(t, cx, 3)

	// Empty response queue — every call errors with "exhausted".
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
	if result.FallbackPromotions != 3 {
		t.Errorf("FallbackPromotions = %d, want 3 (one per candidate)", result.FallbackPromotions)
	}

	// The three originals should now be mid-tier via heuristic
	// fallback rather than frozen at short-tier forever.
	rows, _ := cx.List(cortex.ListOptions{Tiers: []string{trace.TierMid}})
	if len(rows) != 3 {
		t.Errorf("mid tier rows after fallback = %d, want 3", len(rows))
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
