package cortex_test

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/Fail-Safe/Noema/internal/cortex"
	"github.com/Fail-Safe/Noema/internal/event"
	"github.com/Fail-Safe/Noema/internal/trace"
)

// Phase 6 of memory tiering: AdminPurge and TierStats. See
// docs/plans/consolidation-plan.md §11 in the Noema-design repo.

func TestAdminPurge_Tombstone_ShortTier(t *testing.T) {
	cx := setup(t)
	tr := trace.New("routine short", "note", "", []string{"a", "b"}, "secret body")
	if err := cx.Add(tr); err != nil {
		t.Fatalf("Add: %v", err)
	}
	path := cx.TraceFile(tr.ID, false)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("pre-check: file missing: %v", err)
	}
	originalHash := trace.ContentHash(tr.Body)

	if err := cx.AdminPurge(tr.ID, "test reason", trace.TierShort, false, cortex.ActorHuman); err != nil {
		t.Fatalf("AdminPurge: %v", err)
	}

	// File is deleted.
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("file still present after soft-purge: %v", err)
	}

	// Row stays with purged_at/purge_reason set, tags cleared.
	var purgedAt, purgeReason *string
	if err := cx.DB.QueryRow(
		`SELECT purged_at, purge_reason FROM traces WHERE id = ?`, tr.ID,
	).Scan(&purgedAt, &purgeReason); err != nil {
		t.Fatalf("row missing after tombstone: %v", err)
	}
	if purgedAt == nil || *purgedAt == "" {
		t.Errorf("purged_at not set")
	}
	if purgeReason == nil || *purgeReason != "test reason" {
		t.Errorf("purge_reason = %v, want %q", purgeReason, "test reason")
	}
	var tagCount int
	if err := cx.DB.QueryRow(`SELECT COUNT(*) FROM trace_tags WHERE trace_id = ?`, tr.ID).Scan(&tagCount); err != nil {
		t.Fatalf("tag count: %v", err)
	}
	if tagCount != 0 {
		t.Errorf("tags not cleared: %d remain", tagCount)
	}

	// FTS reflects tombstone: old body no longer searchable, tombstone marker is.
	var hits int
	if err := cx.DB.QueryRow(
		`SELECT COUNT(*) FROM traces_fts WHERE traces_fts MATCH ?`, "secret",
	).Scan(&hits); err != nil {
		t.Fatalf("fts query: %v", err)
	}
	if hits != 0 {
		t.Errorf("original body still searchable in FTS after purge")
	}
	if err := cx.DB.QueryRow(
		`SELECT COUNT(*) FROM traces_fts WHERE traces_fts MATCH ?`, "purged",
	).Scan(&hits); err != nil {
		t.Fatalf("fts query: %v", err)
	}
	if hits != 1 {
		t.Errorf("tombstone marker not searchable in FTS")
	}

	// Event emitted with original content_hash as proof of destruction.
	action, payload := lastEvent(t, cx, tr.ID)
	if action != event.ActionPurge {
		t.Errorf("action = %q, want %q", action, event.ActionPurge)
	}
	if payload["content_hash"] != originalHash {
		t.Errorf("content_hash = %v, want %v", payload["content_hash"], originalHash)
	}
	if payload["reason"] != "test reason" {
		t.Errorf("reason = %v", payload["reason"])
	}
	if payload["actor"] != "human" {
		t.Errorf("actor = %v", payload["actor"])
	}
}

func TestAdminPurge_Tombstone_LongTerm_SuspendsTrigger(t *testing.T) {
	cx := setup(t)
	id := seed(t, cx, "long term purge", trace.TierLong)

	if err := cx.AdminPurge(id, "gdpr request #42", trace.TierLong, false, cortex.ActorHuman); err != nil {
		t.Fatalf("AdminPurge long-term: %v", err)
	}
	action, _ := lastEvent(t, cx, id)
	if action != event.ActionPurgeLongTerm {
		t.Errorf("action = %q, want %q", action, event.ActionPurgeLongTerm)
	}

	// Trigger MUST be restored after the purge — confirm by attempting
	// a forbidden UPDATE on a newly-minted long-term row.
	newID := seed(t, cx, "re-check trigger", trace.TierLong)
	_, err := cx.DB.Exec(`UPDATE traces SET title = ? WHERE id = ?`, "edited", newID)
	if err == nil {
		t.Error("trigger not restored after purge — title change slipped through")
	}
}

func TestAdminPurge_Hard_RemovesRowAndLineage(t *testing.T) {
	cx := setup(t)
	parent := trace.New("parent", "fact", "", nil, "parent body")
	if err := cx.Add(parent); err != nil {
		t.Fatalf("Add parent: %v", err)
	}
	child := trace.New("child", "observation", "", nil, "child body")
	child.DerivedFrom = []string{parent.ID}
	if err := cx.Add(child); err != nil {
		t.Fatalf("Add child: %v", err)
	}

	if err := cx.AdminPurge(parent.ID, "hard-delete test", trace.TierShort, true, cortex.ActorHuman); err != nil {
		t.Fatalf("AdminPurge hard: %v", err)
	}

	// Parent row gone entirely.
	_, err := cx.Get(parent.ID)
	if err == nil {
		t.Errorf("parent row still exists after hard purge")
	}

	// Lineage references to parent are wiped.
	var refs int
	if err := cx.DB.QueryRow(
		`SELECT COUNT(*) FROM trace_lineage WHERE derived_from = ?`, parent.ID,
	).Scan(&refs); err != nil {
		t.Fatalf("lineage query: %v", err)
	}
	if refs != 0 {
		t.Errorf("lineage references to parent remain: %d", refs)
	}

	// Event recorded as ActionPurgeHard — federation peers need the
	// distinction to apply the same hard removal rather than a tombstone.
	var action string
	if err := cx.DB.QueryRow(
		`SELECT action FROM events WHERE trace_id = ? ORDER BY id DESC LIMIT 1`, parent.ID,
	).Scan(&action); err != nil {
		t.Fatalf("event query: %v", err)
	}
	if event.Action(action) != event.ActionPurgeHard {
		t.Errorf("action = %q, want %q", action, event.ActionPurgeHard)
	}
}

func TestAdminPurge_TierMismatchRefuses(t *testing.T) {
	cx := setup(t)
	id := seed(t, cx, "tier mismatch", trace.TierMid)
	err := cx.AdminPurge(id, "wrong tier asserted", trace.TierShort, false, cortex.ActorHuman)
	if !errors.Is(err, cortex.ErrTierMismatch) {
		t.Errorf("err = %v, want ErrTierMismatch", err)
	}
	// Row must be untouched.
	row, gerr := cx.Get(id)
	if gerr != nil {
		t.Fatalf("Get after refused purge: %v", gerr)
	}
	if row.Tier != trace.TierMid {
		t.Errorf("tier changed on refused purge: %q", row.Tier)
	}
}

func TestAdminPurge_EmptyReasonRefused(t *testing.T) {
	cx := setup(t)
	id := seed(t, cx, "needs reason", trace.TierShort)
	err := cx.AdminPurge(id, "", trace.TierShort, false, cortex.ActorHuman)
	if err == nil {
		t.Error("empty reason should have been refused")
	}
	if !strings.Contains(err.Error(), "reason") {
		t.Errorf("error should mention reason: %v", err)
	}
}

// ---- TierStats ----

func TestTierStats_CountsActiveTracesByTier(t *testing.T) {
	cx := setup(t)
	// Seed: 3 short, 2 mid, 1 long, 1 purged-short.
	for i := 0; i < 3; i++ {
		seed(t, cx, "short-"+itoa(i), trace.TierShort)
	}
	for i := 0; i < 2; i++ {
		seed(t, cx, "mid-"+itoa(i), trace.TierMid)
	}
	seed(t, cx, "long-0", trace.TierLong)
	purged := seed(t, cx, "to-purge", trace.TierShort)
	if err := cx.AdminPurge(purged, "stats test", trace.TierShort, false, cortex.ActorHuman); err != nil {
		t.Fatalf("AdminPurge: %v", err)
	}

	stats, err := cx.TierStats()
	if err != nil {
		t.Fatalf("TierStats: %v", err)
	}
	if stats.Short != 3 {
		t.Errorf("Short = %d, want 3", stats.Short)
	}
	if stats.Mid != 2 {
		t.Errorf("Mid = %d, want 2", stats.Mid)
	}
	if stats.Long != 1 {
		t.Errorf("Long = %d, want 1", stats.Long)
	}
	if stats.Purged != 1 {
		t.Errorf("Purged = %d, want 1", stats.Purged)
	}
}

