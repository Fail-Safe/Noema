package cli

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Fail-Safe/Noema/internal/cortex"
	"github.com/Fail-Safe/Noema/internal/trace"
)

// newCortexForBackfill builds an on-disk cortex and seeds it with N
// sync-introduced traces (markdown files dropped on disk + cx.Sync) so the
// event log is empty for them. Returns the cortex and the IDs of the
// seeded traces in disk order so callers can assert against them.
func newCortexForBackfill(t *testing.T, name string, titles ...string) (*cortex.Cortex, []string) {
	t.Helper()
	parent := t.TempDir()
	if _, err := cortex.Create(name, parent); err != nil {
		t.Fatalf("cortex.Create: %v", err)
	}
	cx, err := cortex.Open(name, filepath.Join(parent, name))
	if err != nil {
		t.Fatalf("cortex.Open: %v", err)
	}
	t.Cleanup(func() { cx.Close() })

	ids := make([]string, 0, len(titles))
	for _, title := range titles {
		tr := trace.New(title, "note", "", nil, "body for "+title)
		if err := tr.Write(cx.TraceFile(tr.ID, false)); err != nil {
			t.Fatalf("Write %s: %v", tr.ID, err)
		}
		ids = append(ids, tr.ID)
	}
	if _, err := cx.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	return cx, ids
}

// TestRunEventsBackfill_PromptAcceptWritesEvents pins the headline path:
// the operator runs `noema events backfill`, sees the candidate list, says
// yes, and the events appear in the log. The output has to name the
// candidate IDs *before* the prompt so the operator confirms what they're
// committing — not just a count.
func TestRunEventsBackfill_PromptAcceptWritesEvents(t *testing.T) {
	cx, ids := newCortexForBackfill(t, "alpha", "first synced", "second synced")

	var out bytes.Buffer
	if err := runEventsBackfill(&out, strings.NewReader("y\n"), cx, false, false); err != nil {
		t.Fatalf("runEventsBackfill: %v\noutput:\n%s", err, out.String())
	}

	output := out.String()
	for _, id := range ids {
		if !strings.Contains(output, id) {
			t.Errorf("output missing trace id %s:\n%s", id, output)
		}
	}
	if !strings.Contains(output, "Backfilled 2 create event") {
		t.Errorf("output missing summary line:\n%s", output)
	}

	// Both traces must now have a single create event each.
	for _, id := range ids {
		events, err := cx.Events(id)
		if err != nil {
			t.Fatalf("Events %s: %v", id, err)
		}
		if len(events) != 1 {
			t.Errorf("trace %s: got %d events, want 1", id, len(events))
		}
	}
}

// TestRunEventsBackfill_AbortLeavesEventsEmpty pins the safety net: if the
// operator types n at the prompt, no events are written and the call
// returns an error so a script wrapping it can detect the abort. Without
// this test the prompt could quietly accept any input that isn't "y".
func TestRunEventsBackfill_AbortLeavesEventsEmpty(t *testing.T) {
	cx, ids := newCortexForBackfill(t, "alpha", "synced")

	var out bytes.Buffer
	err := runEventsBackfill(&out, strings.NewReader("n\n"), cx, false, false)
	if err == nil {
		t.Fatal("expected error on abort, got nil")
	}
	if !strings.Contains(err.Error(), "abort") {
		t.Errorf("error doesn't mention abort: %v", err)
	}

	for _, id := range ids {
		events, _ := cx.Events(id)
		if len(events) != 0 {
			t.Errorf("trace %s got %d events despite abort, want 0", id, len(events))
		}
	}
}

// TestRunEventsBackfill_DryRunWritesNothing pins that --dry-run prints the
// preview, never prompts, and never touches the event log. The "dry run"
// suffix in the output is what the operator looks for to confirm they're
// safe.
func TestRunEventsBackfill_DryRunWritesNothing(t *testing.T) {
	cx, ids := newCortexForBackfill(t, "alpha", "preview only")

	var out bytes.Buffer
	// Pass an empty reader so the test fails loudly if dry-run mistakenly
	// reaches the prompt — Fscanln on an empty reader would block forever
	// in production but here returns immediately, and the result would be
	// "abort" which is exactly the wrong behavior to silently accept.
	if err := runEventsBackfill(&out, strings.NewReader(""), cx, true, false); err != nil {
		t.Fatalf("runEventsBackfill dry-run: %v", err)
	}

	output := out.String()
	if !strings.Contains(output, "(dry run") {
		t.Errorf("output missing dry-run marker:\n%s", output)
	}
	if !strings.Contains(output, ids[0]) {
		t.Errorf("dry-run output missing trace id %s:\n%s", ids[0], output)
	}
	// And no events were written.
	events, _ := cx.Events(ids[0])
	if len(events) != 0 {
		t.Errorf("dry-run wrote %d events, want 0", len(events))
	}
}

// TestRunEventsBackfill_AssumeYesSkipsPrompt pins the --yes shortcut: it
// must skip the prompt entirely (so it works in scripts and from launchd
// units where there is no stdin) and proceed straight to writing events.
func TestRunEventsBackfill_AssumeYesSkipsPrompt(t *testing.T) {
	cx, ids := newCortexForBackfill(t, "alpha", "yes path")

	var out bytes.Buffer
	if err := runEventsBackfill(&out, strings.NewReader(""), cx, false, true); err != nil {
		t.Fatalf("runEventsBackfill --yes: %v", err)
	}

	if !strings.Contains(out.String(), "Backfilled 1 create event") {
		t.Errorf("output missing summary line:\n%s", out.String())
	}
	events, _ := cx.Events(ids[0])
	if len(events) != 1 {
		t.Errorf("trace %s got %d events, want 1", ids[0], len(events))
	}
}

// TestRunEventsBackfill_NothingToDo pins the empty-cortex case: no traces
// without create events means the command exits cleanly with a friendly
// message and no prompt. This is the expected behavior on a healthy
// cortex (and on every subsequent run after a successful backfill).
func TestRunEventsBackfill_NothingToDo(t *testing.T) {
	cx, _ := newCortexForBackfill(t, "alpha")
	// Add a trace via the normal path so it has a create event already.
	tr := trace.New("normal", "note", "", nil, "body")
	if err := cx.Add(tr); err != nil {
		t.Fatalf("Add: %v", err)
	}

	var out bytes.Buffer
	if err := runEventsBackfill(&out, strings.NewReader(""), cx, false, false); err != nil {
		t.Fatalf("runEventsBackfill: %v", err)
	}
	if !strings.Contains(out.String(), "Nothing to backfill") {
		t.Errorf("output missing nothing-to-do message:\n%s", out.String())
	}
}
