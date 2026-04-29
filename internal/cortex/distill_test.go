package cortex_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Fail-Safe/Noema/internal/cortex"
	"github.com/Fail-Safe/Noema/internal/event"
	"github.com/Fail-Safe/Noema/internal/trace"
)

// Phase 9 of memory tiering: CreateDistilledTrace. See
// docs/plans/consolidation-plan.md §7 and §9 in the Noema-design repo.

func TestCreateDistilledTrace_HappyPath(t *testing.T) {
	cx := setup(t)
	src1 := trace.New("source one", "observation", "", nil, "raw body 1")
	src2 := trace.New("source two", "observation", "", nil, "raw body 2")
	if err := cx.Add(src1); err != nil {
		t.Fatalf("Add src1: %v", err)
	}
	if err := cx.Add(src2); err != nil {
		t.Fatalf("Add src2: %v", err)
	}

	id, err := cx.CreateDistilledTrace(cortex.DistilledTraceSpec{
		Title:              "Consolidated",
		Body:               "Distilled body combining src1 and src2",
		Tags:               []string{"auth", "session"},
		SourceIDs:          []string{src1.ID, src2.ID},
		ModelName:          "claude-opus-4-7",
		ModelTierProfile:   "frontier",
		CohesionConfidence: 0.85,
	})
	if err != nil {
		t.Fatalf("CreateDistilledTrace: %v", err)
	}
	if id == "" {
		t.Fatal("returned empty ID")
	}

	// New trace lands in mid tier with correct lineage.
	row, err := cx.Get(id)
	if err != nil {
		t.Fatalf("Get distilled: %v", err)
	}
	if row.Tier != trace.TierMid {
		t.Errorf("tier = %q, want %q", row.Tier, trace.TierMid)
	}
	if len(row.DerivedFrom) != 2 {
		t.Errorf("DerivedFrom = %v, want 2 entries", row.DerivedFrom)
	}

	// ActionConsolidate event carries the model/confidence payload.
	var actions []string
	rows, err := cx.DB.Query(`SELECT action, data FROM events WHERE trace_id = ? ORDER BY id ASC`, id)
	if err != nil {
		t.Fatalf("events query: %v", err)
	}
	defer rows.Close()
	var consolidatePayload map[string]any
	for rows.Next() {
		var action, data string
		if err := rows.Scan(&action, &data); err != nil {
			t.Fatalf("scan: %v", err)
		}
		actions = append(actions, action)
		if event.Action(action) == event.ActionConsolidate {
			if err := json.Unmarshal([]byte(data), &consolidatePayload); err != nil {
				t.Fatalf("unmarshal consolidate payload: %v", err)
			}
		}
	}
	// Expect ActionCreate then ActionConsolidate.
	if len(actions) < 2 || actions[0] != string(event.ActionCreate) || actions[1] != string(event.ActionConsolidate) {
		t.Errorf("events = %v, want [create, consolidate, ...]", actions)
	}
	if consolidatePayload["model_name"] != "claude-opus-4-7" {
		t.Errorf("event model_name = %v, want claude-opus-4-7", consolidatePayload["model_name"])
	}
	if consolidatePayload["cohesion_confidence"].(float64) != 0.85 {
		t.Errorf("event confidence = %v, want 0.85", consolidatePayload["cohesion_confidence"])
	}
	sources, _ := consolidatePayload["source_ids"].([]any)
	if len(sources) != 2 {
		t.Errorf("event source_ids = %v, want 2 entries", sources)
	}
}

func TestCreateDistilledTrace_RejectsSingleSource(t *testing.T) {
	cx := setup(t)
	solo := trace.New("solo", "note", "", nil, "body")
	if err := cx.Add(solo); err != nil {
		t.Fatalf("Add: %v", err)
	}
	_, err := cx.CreateDistilledTrace(cortex.DistilledTraceSpec{
		Title:     "Too thin",
		Body:      "not a real consolidation",
		SourceIDs: []string{solo.ID},
	})
	if !errors.Is(err, cortex.ErrDistillSourcesInsufficient) {
		t.Errorf("err = %v, want ErrDistillSourcesInsufficient", err)
	}
}

func TestCreateDistilledTrace_RejectsMissingSource(t *testing.T) {
	cx := setup(t)
	src := trace.New("exists", "note", "", nil, "body")
	if err := cx.Add(src); err != nil {
		t.Fatalf("Add: %v", err)
	}
	_, err := cx.CreateDistilledTrace(cortex.DistilledTraceSpec{
		Title:     "Pointing at ghost",
		Body:      "lineage check",
		SourceIDs: []string{src.ID, "20260101-ghost-trace"},
	})
	if !errors.Is(err, cortex.ErrDistillSourceMissing) {
		t.Errorf("err = %v, want ErrDistillSourceMissing", err)
	}
}

func TestCreateDistilledTrace_RejectsEmptyTitleOrBody(t *testing.T) {
	cx := setup(t)
	a := trace.New("a", "note", "", nil, "body")
	b := trace.New("b", "note", "", nil, "body")
	if err := cx.Add(a); err != nil {
		t.Fatalf("Add a: %v", err)
	}
	if err := cx.Add(b); err != nil {
		t.Fatalf("Add b: %v", err)
	}

	_, err := cx.CreateDistilledTrace(cortex.DistilledTraceSpec{
		Body:      "no title",
		SourceIDs: []string{a.ID, b.ID},
	})
	if err == nil || !strings.Contains(err.Error(), "title") {
		t.Errorf("missing-title err = %v", err)
	}
	_, err = cx.CreateDistilledTrace(cortex.DistilledTraceSpec{
		Title:     "no body",
		SourceIDs: []string{a.ID, b.ID},
	})
	if err == nil || !strings.Contains(err.Error(), "body") {
		t.Errorf("missing-body err = %v", err)
	}
}

func TestCreateDistilledTrace_PromotesSourcesToMid(t *testing.T) {
	// v1 policy: once a distillation lands, every short-tier source
	// is promoted to mid. This keeps both representations available
	// (distillation as the summary, sources as full-detail backing
	// store reachable via derived_from) and takes the sources out of
	// the PromotionCandidates pool so the next pass doesn't
	// re-consolidate them into a duplicate distillation.
	//
	// v2+ will likely replace this with archival-after-grace-period
	// (see the doc-comment on CreateDistilledTrace).
	cx := setup(t)
	src1 := trace.New("s1", "note", "", nil, "b1")
	src2 := trace.New("s2", "note", "", nil, "b2")
	if err := cx.Add(src1); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := cx.Add(src2); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, err := cx.CreateDistilledTrace(cortex.DistilledTraceSpec{
		Title: "dist", Body: "body", SourceIDs: []string{src1.ID, src2.ID},
	}); err != nil {
		t.Fatalf("CreateDistilledTrace: %v", err)
	}
	for _, id := range []string{src1.ID, src2.ID} {
		row, err := cx.Get(id)
		if err != nil {
			t.Fatalf("Get source: %v", err)
		}
		if row.Tier != trace.TierMid {
			t.Errorf("source %s tier = %q, want mid (sources promoted on distillation)", id, row.Tier)
		}
	}
}
