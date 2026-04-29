package cortex_test

import (
	"testing"
	"time"

	"github.com/Fail-Safe/Noema/internal/cortex"
	"github.com/Fail-Safe/Noema/internal/trace"
)

// Phase 3 of memory tiering: read attribution. See docs/plans/consolidation-plan.md
// §3 in the Noema-design repo for the design rationale. These tests pin the
// contract that only ActorAgent bumps counters — the whole point of the
// attribution system is to keep the consolidation-scoring signal clean.

// readCountOf returns the aggregate (read_count, last_read_at) across
// all peers for the given trace — the same view the heuristic and
// scorer consume. Post-PR-A this reads from trace_usage (the new
// source of truth) rather than the legacy traces columns.
func readCountOf(t *testing.T, cx *cortex.Cortex, id string) (int, string) {
	t.Helper()
	var count int
	var lastRead *string
	err := cx.DB.QueryRow(
		`SELECT COALESCE(SUM(read_count), 0), MAX(last_read_at) FROM trace_usage WHERE trace_id = ?`, id,
	).Scan(&count, &lastRead)
	if err != nil {
		t.Fatalf("reading counters: %v", err)
	}
	last := ""
	if lastRead != nil {
		last = *lastRead
	}
	return count, last
}

func modifyCountOf(t *testing.T, cx *cortex.Cortex, id string) int {
	t.Helper()
	var count int
	if err := cx.DB.QueryRow(
		`SELECT COALESCE(SUM(modify_count), 0) FROM trace_usage WHERE trace_id = ?`, id,
	).Scan(&count); err != nil {
		t.Fatalf("reading modify_count: %v", err)
	}
	return count
}

func TestGetAs_AgentBumpsCounters(t *testing.T) {
	cx := setup(t)
	tr := trace.New("Agent read bumps", "note", "", nil, "body")
	if err := cx.Add(tr); err != nil {
		t.Fatalf("Add: %v", err)
	}
	before, _ := readCountOf(t, cx, tr.ID)
	if before != 0 {
		t.Fatalf("pre-read count = %d, want 0", before)
	}

	if _, err := cx.GetAs(tr.ID, cortex.ActorAgent); err != nil {
		t.Fatalf("GetAs: %v", err)
	}
	count, last := readCountOf(t, cx, tr.ID)
	if count != 1 {
		t.Errorf("read_count = %d, want 1", count)
	}
	if last == "" {
		t.Errorf("last_read_at empty after agent read")
	}
	if _, err := time.Parse(time.RFC3339, last); err != nil {
		t.Errorf("last_read_at %q not RFC3339: %v", last, err)
	}

	// Two more agent reads — count accumulates.
	if _, err := cx.GetAs(tr.ID, cortex.ActorAgent); err != nil {
		t.Fatalf("GetAs: %v", err)
	}
	if _, err := cx.GetAs(tr.ID, cortex.ActorAgent); err != nil {
		t.Fatalf("GetAs: %v", err)
	}
	if count, _ := readCountOf(t, cx, tr.ID); count != 3 {
		t.Errorf("read_count after 3 agent reads = %d, want 3", count)
	}
}

func TestGetAs_HumanAndSystemDoNotBump(t *testing.T) {
	cx := setup(t)
	tr := trace.New("Non-agent reads", "note", "", nil, "body")
	if err := cx.Add(tr); err != nil {
		t.Fatalf("Add: %v", err)
	}

	for _, actor := range []cortex.ReadActor{cortex.ActorHuman, cortex.ActorSystem} {
		if _, err := cx.GetAs(tr.ID, actor); err != nil {
			t.Fatalf("GetAs(%d): %v", actor, err)
		}
	}
	// Also the plain Get — that's how CLI and internal code reach rows.
	if _, err := cx.Get(tr.ID); err != nil {
		t.Fatalf("Get: %v", err)
	}
	count, last := readCountOf(t, cx, tr.ID)
	if count != 0 {
		t.Errorf("read_count after non-agent reads = %d, want 0", count)
	}
	if last != "" {
		t.Errorf("last_read_at = %q, want empty", last)
	}
}

func TestGetAs_LongTermTierSkipsBump(t *testing.T) {
	// Long-term is DB-immutable; bumping read_count on a tier='long' row
	// would trip the trigger and break the read. Document that expectation
	// at the Go layer so the skip isn't implicit.
	cx := setup(t)
	tr := trace.New("Long terminal", "fact", "", nil, "base truth")
	tr.Tier = trace.TierLong
	if err := cx.Add(tr); err != nil {
		t.Fatalf("Add: %v", err)
	}

	if _, err := cx.GetAs(tr.ID, cortex.ActorAgent); err != nil {
		t.Fatalf("GetAs agent on long-term: %v", err)
	}
	if count, _ := readCountOf(t, cx, tr.ID); count != 0 {
		t.Errorf("read_count bumped on long-term = %d, want 0", count)
	}
}

func TestUpdateAs_AgentBumpsModifyCount(t *testing.T) {
	cx := setup(t)
	tr := trace.New("Modify counter", "note", "", nil, "original body")
	if err := cx.Add(tr); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Agent update — modify_count should go up.
	path := cx.TraceFile(tr.ID, false)
	parsed, err := trace.ParseFile(path)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	parsed.Body = "edited body"
	if err := parsed.Write(path); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := cx.UpdateAs(tr.ID, cortex.ActorAgent); err != nil {
		t.Fatalf("UpdateAs: %v", err)
	}
	if got := modifyCountOf(t, cx, tr.ID); got != 1 {
		t.Errorf("modify_count after agent Update = %d, want 1", got)
	}
}

func TestUpdateAs_NonAgentDoesNotBump(t *testing.T) {
	cx := setup(t)
	tr := trace.New("System update", "note", "", nil, "original body")
	if err := cx.Add(tr); err != nil {
		t.Fatalf("Add: %v", err)
	}

	path := cx.TraceFile(tr.ID, false)
	parsed, err := trace.ParseFile(path)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	parsed.Body = "system-reconciled body"
	if err := parsed.Write(path); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := cx.UpdateAs(tr.ID, cortex.ActorSystem); err != nil {
		t.Fatalf("UpdateAs: %v", err)
	}
	if got := modifyCountOf(t, cx, tr.ID); got != 0 {
		t.Errorf("modify_count after system Update = %d, want 0", got)
	}

	// Plain Update (the path watcher / federation / CLI all use) must
	// also leave the counter untouched.
	parsed.Body = "watcher body"
	if err := parsed.Write(path); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := cx.Update(tr.ID); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got := modifyCountOf(t, cx, tr.ID); got != 0 {
		t.Errorf("modify_count after plain Update = %d, want 0", got)
	}
}
