package cortex_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/Fail-Safe/Noema/internal/cortex"
	"github.com/Fail-Safe/Noema/internal/trace"
)

// PK collisions on traces.id are the failure mode an end-user actually
// hits when they retry a create on the same day with the same title (or
// when an agent recreates a title held by a trashed/archived trace).
// The raw modernc.org/sqlite error string ("UNIQUE constraint failed:
// traces.id (1555)") is incomprehensible to a human and routinely
// misdiagnosed by LLM agents (the (1555) is the SQLITE_CONSTRAINT_PRIMARYKEY
// extended code, not a rowid). These tests pin the typed wrapper that
// Add returns instead.

func TestAdd_CollidesActive_ReturnsTypedError(t *testing.T) {
	cx := setup(t)
	first := trace.New("collision-target", "note", "", nil, "first body")
	if err := cx.Add(first); err != nil {
		t.Fatalf("seed Add: %v", err)
	}
	dup := trace.New("collision-target", "note", "", nil, "second body")
	err := cx.Add(dup)
	var collision *cortex.ErrTraceIDExists
	if !errors.As(err, &collision) {
		t.Fatalf("err = %v, want *ErrTraceIDExists", err)
	}
	if collision.ID != first.ID {
		t.Errorf("ID = %q, want %q", collision.ID, first.ID)
	}
	if collision.State != "active" {
		t.Errorf("State = %q, want active", collision.State)
	}
	if !strings.Contains(collision.Error(), "already exists (currently active)") {
		t.Errorf("Error() should mention active state, got: %s", collision.Error())
	}
}

func TestAdd_CollidesTrashed_StateReportedAsTrashed(t *testing.T) {
	cx := setup(t)
	first := trace.New("ghost", "note", "", nil, "body")
	if err := cx.Add(first); err != nil {
		t.Fatalf("seed Add: %v", err)
	}
	if err := cx.Trash(first.ID); err != nil {
		t.Fatalf("Trash: %v", err)
	}
	dup := trace.New("ghost", "note", "", nil, "different body")
	err := cx.Add(dup)
	var collision *cortex.ErrTraceIDExists
	if !errors.As(err, &collision) {
		t.Fatalf("err = %v, want *ErrTraceIDExists", err)
	}
	if collision.State != "trashed" {
		t.Errorf("State = %q, want trashed", collision.State)
	}
	if collision.TrashedAt == "" {
		t.Errorf("TrashedAt should be populated, got empty")
	}
	// Recovery hint should be appropriate to a trashed row.
	if !strings.Contains(collision.Error(), "noema recover") {
		t.Errorf("Error() should suggest recover for trashed state: %s", collision.Error())
	}
}

func TestAdd_CollidesArchived_StateReportedAsArchived(t *testing.T) {
	cx := setup(t)
	first := trace.New("dust", "note", "", nil, "body")
	if err := cx.Add(first); err != nil {
		t.Fatalf("seed Add: %v", err)
	}
	if err := cx.Archive(first.ID); err != nil {
		t.Fatalf("Archive: %v", err)
	}
	dup := trace.New("dust", "note", "", nil, "different body")
	err := cx.Add(dup)
	var collision *cortex.ErrTraceIDExists
	if !errors.As(err, &collision) {
		t.Fatalf("err = %v, want *ErrTraceIDExists", err)
	}
	if collision.State != "archived" {
		t.Errorf("State = %q, want archived", collision.State)
	}
	if !strings.Contains(collision.Error(), "noema unarchive") {
		t.Errorf("Error() should suggest unarchive for archived state: %s", collision.Error())
	}
}

func TestAdd_CollidesPurgedTombstone_StateReportedAsPurged(t *testing.T) {
	cx := setup(t)
	first := trace.New("ash", "note", "", nil, "body")
	if err := cx.Add(first); err != nil {
		t.Fatalf("seed Add: %v", err)
	}
	if err := cx.AdminPurge(first.ID, "test purge", trace.TierShort, false, cortex.ActorHuman); err != nil {
		t.Fatalf("AdminPurge soft: %v", err)
	}
	dup := trace.New("ash", "note", "", nil, "different body")
	err := cx.Add(dup)
	var collision *cortex.ErrTraceIDExists
	if !errors.As(err, &collision) {
		t.Fatalf("err = %v, want *ErrTraceIDExists", err)
	}
	if collision.State != "purged" {
		t.Errorf("State = %q, want purged", collision.State)
	}
	if !strings.Contains(collision.Error(), "--hard") {
		t.Errorf("Error() should explain that only --hard purge frees the slot: %s", collision.Error())
	}
}

func TestAdd_HardPurgeFreesIDSlot(t *testing.T) {
	// Counterpart to the soft-purge test above: a --hard purge actually
	// removes the row, so a follow-up Add with the same title should
	// succeed cleanly with no ErrTraceIDExists.
	cx := setup(t)
	first := trace.New("phoenix", "note", "", nil, "body")
	if err := cx.Add(first); err != nil {
		t.Fatalf("seed Add: %v", err)
	}
	if err := cx.AdminPurge(first.ID, "test hard purge", trace.TierShort, true, cortex.ActorHuman); err != nil {
		t.Fatalf("AdminPurge hard: %v", err)
	}
	dup := trace.New("phoenix", "note", "", nil, "rebirth body")
	if err := cx.Add(dup); err != nil {
		t.Fatalf("Add after hard purge should succeed, got: %v", err)
	}
}

func TestIsTraceIDExists_RecognisesWrappedError(t *testing.T) {
	// errors.As should reach the typed error even if a caller wraps it.
	wrapped := &cortex.ErrTraceIDExists{ID: "x", State: "active"}
	if !cortex.IsTraceIDExists(wrapped) {
		t.Errorf("IsTraceIDExists should report true for direct error")
	}
	if cortex.IsTraceIDExists(errors.New("unrelated")) {
		t.Errorf("IsTraceIDExists should report false for unrelated error")
	}
}
