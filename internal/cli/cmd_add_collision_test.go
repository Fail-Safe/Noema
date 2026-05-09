package cli

import (
	"bufio"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Fail-Safe/Noema/internal/cortex"
	"github.com/Fail-Safe/Noema/internal/trace"
)

func newCortexForCollision(t *testing.T) *cortex.Cortex {
	t.Helper()
	parent := t.TempDir()
	if _, err := cortex.Create("colltest", parent); err != nil {
		t.Fatalf("cortex.Create: %v", err)
	}
	cx, err := cortex.Open("colltest", filepath.Join(parent, "colltest"))
	if err != nil {
		t.Fatalf("cortex.Open: %v", err)
	}
	t.Cleanup(func() { cx.Close() })
	return cx
}

// scriptStdin replaces the package-level stdin reader with one backed
// by the supplied lines. The original is restored on test cleanup so
// parallel-safe.
func scriptStdin(t *testing.T, lines ...string) {
	t.Helper()
	previous := stdin
	stdin = bufio.NewReader(strings.NewReader(strings.Join(lines, "\n") + "\n"))
	t.Cleanup(func() { stdin = previous })
}

// TestResolveCollision_VaryTitle pins the V-vary path: a colliding
// trashed slot doesn't get touched, the user supplies a fresh title,
// and the new trace lands at the new id with the original body intact.
func TestResolveCollision_VaryTitle(t *testing.T) {
	cx := newCortexForCollision(t)
	original := trace.New("airlock", "note", "", nil, "first body")
	if err := cx.Add(original); err != nil {
		t.Fatalf("seed Add: %v", err)
	}
	if err := cx.Trash(original.ID); err != nil {
		t.Fatalf("Trash: %v", err)
	}

	dup := trace.New("airlock", "note", "", []string{"safety"}, "rebuilt body")
	collision, ok := cx.Add(dup).(*cortex.ErrTraceIDExists)
	if !ok {
		t.Fatalf("expected ErrTraceIDExists, got %v", collision)
	}

	scriptStdin(t, "v", "airlock-v2")
	if err := resolveCollisionInteractive(cx, dup, collision, "note", "", []string{"safety"}, "rebuilt body"); err != nil {
		t.Fatalf("resolveCollisionInteractive: %v", err)
	}
	// Original (trashed) row stays put.
	row, err := cx.Get(original.ID)
	if err != nil {
		t.Fatalf("Get original: %v", err)
	}
	if row.TrashedAt == "" {
		t.Errorf("trashed row should remain trashed, got TrashedAt empty")
	}
	// New row landed at the new id with the user's body intact.
	rows, err := cx.List(cortex.ListOptions{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var newID string
	for _, r := range rows {
		if r.ID != original.ID {
			newID = r.ID
		}
	}
	if newID == "" || !strings.Contains(newID, "airlock-v2") {
		t.Errorf("new row id = %q, want one containing airlock-v2", newID)
	}
}

// TestResolveCollision_RecoverTrashed pins the R path: user picks
// recover, the trashed row comes back, no new row is created.
func TestResolveCollision_RecoverTrashed(t *testing.T) {
	cx := newCortexForCollision(t)
	original := trace.New("relic", "note", "", nil, "valuable body")
	if err := cx.Add(original); err != nil {
		t.Fatalf("seed Add: %v", err)
	}
	if err := cx.Trash(original.ID); err != nil {
		t.Fatalf("Trash: %v", err)
	}

	dup := trace.New("relic", "note", "", nil, "throwaway body")
	collision := cx.Add(dup).(*cortex.ErrTraceIDExists)

	scriptStdin(t, "r")
	if err := resolveCollisionInteractive(cx, dup, collision, "note", "", nil, "throwaway body"); err != nil {
		t.Fatalf("resolveCollisionInteractive: %v", err)
	}
	row, err := cx.Get(original.ID)
	if err != nil {
		t.Fatalf("Get recovered: %v", err)
	}
	if row.TrashedAt != "" {
		t.Errorf("row should be recovered (TrashedAt cleared), got %q", row.TrashedAt)
	}
}

// TestResolveCollision_PurgeAndRetry pins the P path: hard-purge the
// colliding row to free the slot, then the new trace lands at the same
// id with the new body. Distinguishes P from R semantically — under R
// the user's new body is discarded; under P the new body wins.
func TestResolveCollision_PurgeAndRetry(t *testing.T) {
	cx := newCortexForCollision(t)
	original := trace.New("kiln", "note", "", nil, "discardable body")
	if err := cx.Add(original); err != nil {
		t.Fatalf("seed Add: %v", err)
	}
	if err := cx.Trash(original.ID); err != nil {
		t.Fatalf("Trash: %v", err)
	}

	dup := trace.New("kiln", "note", "", nil, "fresh body")
	collision := cx.Add(dup).(*cortex.ErrTraceIDExists)

	scriptStdin(t, "p")
	if err := resolveCollisionInteractive(cx, dup, collision, "note", "", nil, "fresh body"); err != nil {
		t.Fatalf("resolveCollisionInteractive: %v", err)
	}
	row, err := cx.Get(original.ID)
	if err != nil {
		t.Fatalf("Get after purge+retry: %v", err)
	}
	if row.TrashedAt != "" {
		t.Errorf("retried row should be active (TrashedAt empty), got %q", row.TrashedAt)
	}
}

// TestResolveCollision_EOFTreatedAsQuit pins a regression caught
// during local smoke: when stdin is empty (closed pipe, prior prompt
// consumed everything, scripted run with no input left), the resolver
// must NOT spin re-prompting against an empty buffer. EOF should
// cleanly fall through to the Q path and propagate the original
// collision error.
func TestResolveCollision_EOFTreatedAsQuit(t *testing.T) {
	cx := newCortexForCollision(t)
	original := trace.New("eofcase", "note", "", nil, "body")
	if err := cx.Add(original); err != nil {
		t.Fatalf("seed Add: %v", err)
	}

	dup := trace.New("eofcase", "note", "", nil, "body 2")
	collision := cx.Add(dup).(*cortex.ErrTraceIDExists)

	// Empty stdin — no choice supplied at all.
	scriptStdin(t)
	err := resolveCollisionInteractive(cx, dup, collision, "note", "", nil, "body 2")
	if !cortex.IsTraceIDExists(err) {
		t.Errorf("expected ErrTraceIDExists to propagate on EOF, got %v", err)
	}
}

// TestResolveCollision_QuitReturnsError pins the Q path: user backs
// out, original collision error propagates so the caller sees the
// failure.
func TestResolveCollision_QuitReturnsError(t *testing.T) {
	cx := newCortexForCollision(t)
	original := trace.New("vault", "note", "", nil, "body")
	if err := cx.Add(original); err != nil {
		t.Fatalf("seed Add: %v", err)
	}

	dup := trace.New("vault", "note", "", nil, "body 2")
	collision := cx.Add(dup).(*cortex.ErrTraceIDExists)

	scriptStdin(t, "q")
	err := resolveCollisionInteractive(cx, dup, collision, "note", "", nil, "body 2")
	if !cortex.IsTraceIDExists(err) {
		t.Errorf("expected ErrTraceIDExists to propagate on quit, got %v", err)
	}
}
