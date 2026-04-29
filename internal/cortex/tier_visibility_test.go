package cortex_test

import (
	"testing"
	"time"

	"github.com/Fail-Safe/Noema/internal/cortex"
	"github.com/Fail-Safe/Noema/internal/trace"
)

// Phase 13 of memory tiering: archive/trash interactions with tier.
// See docs/plans/consolidation-plan.md §13 in the Noema-design repo.
// Migration 013 relaxes the long-term immutability trigger so
// archived_at and trashed_at can change on tier='long' rows without
// the admin-purge ceremony. Content/identity fields remain blocked —
// the tests below pin both halves of the new contract.

// ---- Long-term traces permit visibility-state changes ----

func TestArchive_OnLongTerm_PermittedByRefinedTrigger(t *testing.T) {
	cx := setup(t)
	id := seed(t, cx, "long-term to archive", trace.TierLong)
	if err := cx.Archive(id); err != nil {
		t.Fatalf("Archive long-term: %v", err)
	}
	row, err := cx.Get(id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if row.ArchivedAt == "" {
		t.Error("archived_at not set on long-term trace after Archive")
	}
	if row.Tier != trace.TierLong {
		t.Errorf("tier changed during archive: got %q, want long", row.Tier)
	}
}

func TestUnarchive_OnLongTerm_PermittedByRefinedTrigger(t *testing.T) {
	cx := setup(t)
	id := seed(t, cx, "long-term to unarchive", trace.TierLong)
	if err := cx.Archive(id); err != nil {
		t.Fatalf("Archive: %v", err)
	}
	if err := cx.Unarchive(id); err != nil {
		t.Fatalf("Unarchive long-term: %v", err)
	}
	row, err := cx.Get(id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if row.ArchivedAt != "" {
		t.Error("archived_at not cleared on long-term trace after Unarchive")
	}
	if row.Tier != trace.TierLong {
		t.Errorf("tier changed during unarchive: got %q, want long", row.Tier)
	}
}

func TestTrash_OnLongTerm_PermittedByRefinedTrigger(t *testing.T) {
	// Motivating case: a user soft-deletes an outdated long-term memory
	// for eventual auto-purge. The 30-day grace window plus admin
	// recovery path in the existing Trash/Recover machinery should work
	// on long-term rows too, without requiring the full admin-purge
	// ceremony.
	cx := setup(t)
	id := seed(t, cx, "long-term to trash", trace.TierLong)
	if err := cx.Trash(id); err != nil {
		t.Fatalf("Trash long-term: %v", err)
	}
	row, err := cx.Get(id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if row.TrashedAt == "" {
		t.Error("trashed_at not set on long-term trace after Trash")
	}
	if row.Tier != trace.TierLong {
		t.Errorf("tier changed during trash: got %q, want long", row.Tier)
	}
}

func TestRecover_OnLongTerm_PermittedByRefinedTrigger(t *testing.T) {
	cx := setup(t)
	id := seed(t, cx, "long-term to recover", trace.TierLong)
	if err := cx.Trash(id); err != nil {
		t.Fatalf("Trash: %v", err)
	}
	if err := cx.Recover(id); err != nil {
		t.Fatalf("Recover long-term: %v", err)
	}
	row, err := cx.Get(id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if row.TrashedAt != "" {
		t.Error("trashed_at not cleared on long-term trace after Recover")
	}
	if row.Tier != trace.TierLong {
		t.Errorf("tier changed during recover: got %q, want long", row.Tier)
	}
}

// ---- Content immutability still enforced on long-term ----

func TestLongTerm_TitleEditStillAborts_AfterVisibilityRelax(t *testing.T) {
	// The complement of the tests above: even with migration 013
	// permitting visibility changes, content/identity edits must still
	// abort. Without this assertion, the §13 relaxation could drift
	// into over-permissive territory in future trigger refactors.
	cx := setup(t)
	id := seed(t, cx, "immutable body", trace.TierLong)
	_, err := cx.DB.Exec(`UPDATE traces SET title = ? WHERE id = ?`, "rewrite", id)
	if err == nil {
		t.Error("refined trigger let title change slip through on long-term")
	}
}

func TestLongTerm_ContentHashEditStillAborts(t *testing.T) {
	// content_hash is the fingerprint of the body bytes — rewriting it
	// would decouple the DB's sense of identity from what's on disk,
	// which is exactly the kind of tampering the long-term immutability
	// trigger exists to block.
	cx := setup(t)
	id := seed(t, cx, "immutable hash", trace.TierLong)
	_, err := cx.DB.Exec(`UPDATE traces SET content_hash = ? WHERE id = ?`, "sha256:0000", id)
	if err == nil {
		t.Error("refined trigger let content_hash change slip through on long-term")
	}
}

// ---- Archived short-term traces are excluded from candidates ----

func TestPromotionCandidates_ExcludesArchivedShortTerm(t *testing.T) {
	// Archive acts as an implicit "don't promote this" signal. Counter
	// accumulation keeps happening when agents read it, but the
	// consolidation pass must not surface it as a candidate — surfacing
	// would resurrect the trace into mid-tier visibility against the
	// user's expressed intent.
	cx := setup(t)
	active := seed(t, cx, "active short", trace.TierShort)
	archived := seed(t, cx, "archived short", trace.TierShort)
	if err := cx.Archive(archived); err != nil {
		t.Fatalf("Archive: %v", err)
	}

	// Both traces have some signal; without the archive filter they'd
	// both qualify (once the promotion threshold is reached). The
	// filter should drop the archived one regardless of counter
	// values.
	bumpSignals(t, cx, active)
	bumpSignals(t, cx, archived)

	cands, err := cx.PromotionCandidates(trace.TierShort, 24*time.Hour)
	if err != nil {
		t.Fatalf("PromotionCandidates: %v", err)
	}
	for _, c := range cands {
		if c.ID == archived {
			t.Errorf("archived short-term trace %s appeared in candidates", archived)
		}
	}
	// Active trace should still be present.
	found := false
	for _, c := range cands {
		if c.ID == active {
			found = true
		}
	}
	if !found {
		t.Errorf("active short-term trace missing from candidates: %v", cands)
	}
}

func TestPromotionCandidates_UnarchiveReturnsToPool(t *testing.T) {
	// Signal preservation across archive/unarchive: after unarchive the
	// trace is a candidate again, and its counters haven't been reset.
	cx := setup(t)
	id := seed(t, cx, "round-trip", trace.TierShort)
	bumpSignals(t, cx, id)

	if err := cx.Archive(id); err != nil {
		t.Fatalf("Archive: %v", err)
	}
	if err := cx.Unarchive(id); err != nil {
		t.Fatalf("Unarchive: %v", err)
	}

	cands, err := cx.PromotionCandidates(trace.TierShort, 24*time.Hour)
	if err != nil {
		t.Fatalf("PromotionCandidates: %v", err)
	}
	var found *cortex.PromotionCandidate
	for i := range cands {
		if cands[i].ID == id {
			found = &cands[i]
		}
	}
	if found == nil {
		t.Fatalf("unarchived trace not in candidate pool: %v", cands)
	}
	if found.ReadCount == 0 {
		t.Errorf("counters reset during archive/unarchive round-trip: %+v", found)
	}
}

// bumpSignals gives a trace enough usage signal that it would normally
// qualify for promotion. Used by the candidate-exclusion tests to
// distinguish "filtered out by archive" from "never qualified".
func bumpSignals(t *testing.T, cx *cortex.Cortex, id string) {
	t.Helper()
	for i := 0; i < 3; i++ {
		if _, err := cx.GetAs(id, cortex.ActorAgent); err != nil {
			t.Fatalf("GetAs: %v", err)
		}
	}
	if err := cx.Vote(id, 1, cortex.ActorHuman); err != nil {
		t.Fatalf("Vote: %v", err)
	}
}
