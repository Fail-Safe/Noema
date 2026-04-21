package mcp

import (
	"strings"
	"testing"

	"github.com/Fail-Safe/Noema/internal/trace"
)

// Phase 5 of memory tiering: agent-facing vote_trace MCP tool. See
// docs/plans/consolidation-plan.md §12 in the Noema-design repo.

func TestVoteTrace_HappyPathUpAndDown(t *testing.T) {
	cx := newTestCortex(t)
	tr := trace.New("MCP vote target", "note", "", nil, "body")
	if err := cx.Add(tr); err != nil {
		t.Fatalf("Add: %v", err)
	}

	s := NewServer(cx, "test", "")
	initServer(t, s)

	for _, dir := range []string{"up", "up", "down"} {
		text, isErr := callTool(t, s, "vote_trace", map[string]any{"id": tr.ID, "direction": dir})
		if isErr {
			t.Fatalf("vote_trace %s returned error: %s", dir, text)
		}
		if !strings.Contains(text, "Vote recorded") {
			t.Errorf("response missing confirmation: %s", text)
		}
	}

	var votes int
	if err := cx.DB.QueryRow(`SELECT tier_votes FROM traces WHERE id = ?`, tr.ID).Scan(&votes); err != nil {
		t.Fatalf("reading tier_votes: %v", err)
	}
	if votes != 1 {
		t.Errorf("tier_votes = %d, want 1 (+1+1-1)", votes)
	}
}

func TestVoteTrace_MissingID(t *testing.T) {
	cx := newTestCortex(t)
	s := NewServer(cx, "test", "")
	initServer(t, s)

	text, isErr := callTool(t, s, "vote_trace", map[string]any{"direction": "up"})
	if !isErr {
		t.Errorf("missing id should have errored; got: %s", text)
	}
}

func TestVoteTrace_InvalidDirection(t *testing.T) {
	// Enum on the schema rejects anything other than up/down before the
	// handler runs. The MCP library surfaces that as an error result with
	// a descriptive message — pin the fact that bogus direction values
	// don't silently become no-ops or default to "up".
	cx := newTestCortex(t)
	tr := trace.New("bad dir", "note", "", nil, "body")
	if err := cx.Add(tr); err != nil {
		t.Fatalf("Add: %v", err)
	}
	s := NewServer(cx, "test", "")
	initServer(t, s)

	text, isErr := callTool(t, s, "vote_trace", map[string]any{"id": tr.ID, "direction": "sideways"})
	if !isErr {
		t.Errorf("invalid direction should have errored; got: %s", text)
	}

	// Confirm nothing got recorded.
	var votes int
	if err := cx.DB.QueryRow(`SELECT tier_votes FROM traces WHERE id = ?`, tr.ID).Scan(&votes); err != nil {
		t.Fatalf("reading tier_votes: %v", err)
	}
	if votes != 0 {
		t.Errorf("bogus direction call still tipped counter: tier_votes = %d", votes)
	}
}

func TestVoteTrace_NonexistentTrace(t *testing.T) {
	cx := newTestCortex(t)
	s := NewServer(cx, "test", "")
	initServer(t, s)

	text, isErr := callTool(t, s, "vote_trace", map[string]any{"id": "20260101-ghost", "direction": "up"})
	if !isErr {
		t.Errorf("voting on nonexistent trace should error; got: %s", text)
	}
}
