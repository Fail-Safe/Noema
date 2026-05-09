package mcp

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Fail-Safe/Noema/internal/trace"
)

// When create_trace's INSERT collides on the deterministic id slot, the
// raw SQLite error string ("UNIQUE constraint failed: traces.id (1555)")
// gets misread by LLM agents as a sequence-counter / rowid issue (the
// (1555) is actually SQLITE_CONSTRAINT_PRIMARYKEY, a constant). The MCP
// handler now returns a structured JSON envelope with the existing
// row's state so an agent can branch on `kind` and `existing_state`
// instead of having to parse English.
func TestCreateTrace_CollisionReturnsStructuredError(t *testing.T) {
	cx := newTestCortex(t)
	first := trace.New("collide-me", "note", "", nil, "first body")
	if err := cx.Add(first); err != nil {
		t.Fatalf("seed Add: %v", err)
	}
	if err := cx.Trash(first.ID); err != nil {
		t.Fatalf("Trash: %v", err)
	}

	s := NewServer(cx, "test", "")
	initServer(t, s)

	text, isErr := callTool(t, s, "create_trace", map[string]any{
		"title": "collide-me",
		"type":  "note",
		"body":  "second body",
	})
	if !isErr {
		t.Fatalf("expected error result, got success: %s", text)
	}

	var payload struct {
		Kind          string   `json:"kind"`
		ID            string   `json:"id"`
		ExistingState string   `json:"existing_state"`
		TrashedAt     string   `json:"trashed_at"`
		Fix           []string `json:"fix"`
		Summary       string   `json:"summary"`
	}
	if err := json.Unmarshal([]byte(text), &payload); err != nil {
		t.Fatalf("response is not JSON: %v\nraw=%s", err, text)
	}
	if payload.Kind != "trace_id_collision" {
		t.Errorf("kind = %q, want trace_id_collision", payload.Kind)
	}
	if payload.ID != first.ID {
		t.Errorf("id = %q, want %q", payload.ID, first.ID)
	}
	if payload.ExistingState != "trashed" {
		t.Errorf("existing_state = %q, want trashed", payload.ExistingState)
	}
	if payload.TrashedAt == "" {
		t.Errorf("trashed_at should be populated, got empty")
	}
	if len(payload.Fix) == 0 {
		t.Errorf("fix should list remediation options, got empty")
	}
	// At least one fix line should mention recover (since the row is
	// trashed, that's the soft path; purge is the hard path).
	var sawRecover bool
	for _, f := range payload.Fix {
		if strings.Contains(f, "recover") {
			sawRecover = true
		}
	}
	if !sawRecover {
		t.Errorf("fix list should include `noema recover` for trashed state, got: %v", payload.Fix)
	}
	if payload.Summary == "" {
		t.Errorf("summary should carry human-readable text for non-parsing readers")
	}
}
