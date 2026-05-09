package mcp

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Fail-Safe/Noema/internal/cortex"
	"github.com/Fail-Safe/Noema/internal/trace"
)

// Phase 9 of memory tiering: MCP consolidation tools. See
// docs/plans/consolidation-plan.md §7 in the Noema-design repo.

func TestListConsolidationCandidates_ReturnsStructuredJSON(t *testing.T) {
	cx := newTestCortex(t)

	// Two short traces to surface as candidates, one mid to verify
	// tier filter (only short should appear).
	for _, title := range []string{"short one", "short two"} {
		tr := trace.New(title, "note", "", nil, "body")
		if err := cx.Add(tr); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
	midTrace := trace.New("mid only", "observation", "", nil, "mid body")
	midTrace.Tier = trace.TierMid
	if err := cx.Add(midTrace); err != nil {
		t.Fatalf("Add mid: %v", err)
	}

	s := NewServer(cx, "test", "")
	initServer(t, s)

	text, isErr := callTool(t, s, "list_consolidation_candidates", nil)
	if isErr {
		t.Fatalf("tool errored: %s", text)
	}

	var payload struct {
		WindowHours int `json:"window_hours"`
		Candidates  []struct {
			ID   string `json:"ID"`
			Tier string `json:"Tier"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal([]byte(text), &payload); err != nil {
		t.Fatalf("unmarshal: %v\nraw=%s", err, text)
	}
	if payload.WindowHours <= 0 {
		t.Errorf("window_hours = %d, want > 0 (default 24h)", payload.WindowHours)
	}
	if len(payload.Candidates) != 2 {
		t.Errorf("candidates = %d, want 2", len(payload.Candidates))
	}
	for _, c := range payload.Candidates {
		if c.Tier != trace.TierShort {
			t.Errorf("candidate %s tier = %q, want short", c.ID, c.Tier)
		}
	}
}

func TestRecordConsolidationResult_CreatesMidTrace(t *testing.T) {
	cx := newTestCortex(t)
	src1 := trace.New("auth decision", "decision", "", nil, "we chose oauth")
	src2 := trace.New("auth observation", "observation", "", nil, "users find it intuitive")
	if err := cx.Add(src1); err != nil {
		t.Fatalf("Add src1: %v", err)
	}
	if err := cx.Add(src2); err != nil {
		t.Fatalf("Add src2: %v", err)
	}

	s := NewServer(cx, "test", "")
	initServer(t, s)

	text, isErr := callTool(t, s, "record_consolidation_result", map[string]any{
		"title":               "Auth strategy — distilled",
		"body":                "OAuth chosen and validated by user feedback.",
		"source_ids":          src1.ID + "," + src2.ID,
		"tags":                "auth,consolidated",
		"model_name":          "claude-opus-4-7",
		"model_tier_profile":  "frontier",
		"cohesion_confidence": 0.92,
	})
	if isErr {
		t.Fatalf("tool errored: %s", text)
	}
	if !strings.Contains(text, "Distilled trace created") {
		t.Errorf("response missing confirmation: %s", text)
	}

	// Sources stay at their original tier; only the distilled trace
	// lands in mid. derived_from on the distilled row keeps the
	// originals reachable, and LLMCandidates filters them out of
	// subsequent passes via the ActionConsolidate event log.
	rows, err := cx.List(cortex.ListOptions{Tiers: []string{trace.TierMid}})
	if err != nil {
		t.Fatalf("List mid: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("mid tier rows = %d, want 1 (distilled only): %+v", len(rows), rows)
	}
	if rows[0].Title != "Auth strategy — distilled" {
		t.Errorf("mid-tier row title = %q, want %q", rows[0].Title, "Auth strategy — distilled")
	}
	shortRows, err := cx.List(cortex.ListOptions{Tiers: []string{trace.TierShort}})
	if err != nil {
		t.Fatalf("List short: %v", err)
	}
	if len(shortRows) != 2 {
		t.Errorf("short tier rows = %d, want 2 (sources untouched): %+v", len(shortRows), shortRows)
	}
}

func TestRecordConsolidationResult_RejectsSingleSource(t *testing.T) {
	cx := newTestCortex(t)
	solo := trace.New("solo", "note", "", nil, "body")
	if err := cx.Add(solo); err != nil {
		t.Fatalf("Add: %v", err)
	}
	s := NewServer(cx, "test", "")
	initServer(t, s)

	text, isErr := callTool(t, s, "record_consolidation_result", map[string]any{
		"title":      "not a real distillation",
		"body":       "only one source",
		"source_ids": solo.ID,
	})
	if !isErr {
		t.Errorf("single-source submission should error, got: %s", text)
	}
}
